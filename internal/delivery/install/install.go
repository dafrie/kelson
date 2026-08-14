package install

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
	"k8s.io/client-go/dynamic"
	sigsyaml "sigs.k8s.io/yaml"

	"github.com/dafrie/kelson/internal/clusterprofile"
	"github.com/dafrie/kelson/internal/delivery"
)

// The provenance kelson stamps on everything it installs. See the package doc
// for why app.kubernetes.io/managed-by is deliberately not among them.
const (
	// LabelComponent is the selector handle: which component of the pins table
	// this object belongs to. It is the ONLY thing [Remover] selects on, and
	// kelson stamps it exclusively on objects it applies itself.
	LabelComponent = "kelson.dev/installed-component"
	// LabelVersion is the pinned upstream version kelson applied, so a cluster
	// can be asked what it is running without consulting this repository.
	LabelVersion = "kelson.dev/installed-version"
	// AnnOwnership records whether this apply is what brought the object into
	// existence. It is the sole licence to delete it later, exactly as
	// delivery.AnnNamespaceOwnership is for a Namespace.
	AnnOwnership = "kelson.dev/component-ownership"
)

const (
	// OwnershipCreated: the object did not exist and kelson's apply created it.
	// Uninstall may remove it.
	OwnershipCreated = "created"
	// OwnershipAdopted: the object was already there and kelson applied over
	// it. Uninstall must leave it — somebody else made it, for reasons kelson
	// does not know.
	OwnershipAdopted = "adopted"
)

// FieldManager is the field-manager identity every install apply claims. It is
// direct mode's manager on purpose: two kelson field managers writing to one
// cluster would report a conflict between kelson and kelson.
const FieldManager = "kelson"

// The two coordinates this package addresses without a mapper.
//
// crdGVR is polled to know when a CRD the manifest just applied is Established;
// fluxInstanceGVR is the CR `kelson install flux` creates. Both are written out
// rather than resolved through discovery because the RESTMapper a CLI builds at
// connect time predates the CRDs this install registers, and because the
// flux-operator API may only ever be reached unstructured (AGPL; package doc).
var (
	crdGVR          = schema.GroupVersionResource{Group: "apiextensions.k8s.io", Version: "v1", Resource: "customresourcedefinitions"}
	fluxInstanceGVR = schema.GroupVersionResource{Group: "fluxcd.controlplane.io", Version: "v1", Resource: "fluxinstances"}
)

// fluxInstanceCRD is the CRD an applied FluxInstance needs served. flux-operator
// validates that the resource is named "flux" (a CEL rule in the CRD itself), so
// the name below is not a preference.
const (
	fluxInstanceCRD  = "fluxinstances.fluxcd.controlplane.io"
	fluxInstanceName = "flux"
)

// Mapper resolves a GroupKind to the resource and scope a dynamic call needs.
// Same seam and same reason as direct.Mapper.
type Mapper interface {
	RESTMapping(gk schema.GroupKind, versions ...string) (*meta.RESTMapping, error)
}

// Ref is one object's identity, in the shape the preview prints it.
type Ref struct {
	APIVersion string
	Kind       string
	Name       string
	Namespace  string
}

func (r Ref) String() string {
	if r.Namespace == "" {
		return r.Kind + "/" + r.Name
	}
	return r.Kind + "/" + r.Namespace + "/" + r.Name
}

// Object is one thing an install will apply.
type Object struct {
	Ref Ref
	GVR schema.GroupVersionResource
	// Exists is what a read at plan time saw. It is advisory — the ownership
	// verdict is re-established immediately before the apply, because the plan
	// is a snapshot and the cluster is not — but it is what lets the preview say
	// "this one is already there and kelson will not claim it".
	Exists bool
	// Authored marks an object kelson composed rather than one that came out of
	// the pinned manifest: the FluxInstance, and nothing else today.
	Authored bool

	obj *unstructured.Unstructured
}

// Item is one component's whole install.
type Item struct {
	Component Component
	// Objects are in apply order, which is the pinned manifest's own document
	// order (upstream puts the Namespace first and the CRDs before the workloads
	// that need them) followed by anything kelson authors.
	Objects []Object
	// Digest is the verified digest of the fetched manifest, echoed into the
	// preview so what was checked is visible rather than merely claimed.
	Digest string
}

// Refusal is a component that was asked for and will not be installed.
type Refusal struct {
	Name string
	// Outcome is detection's answer: Yes (it is already here) or Unknown (the
	// probe could not tell). A refusal is never issued on a No.
	Outcome     clusterprofile.Outcome
	Reason      string
	Remediation string
}

// Plan is what an install would do, computed before anything is applied.
type Plan struct {
	Items    []Item
	Refusals []Refusal
}

// Empty reports whether there is nothing to install.
func (p *Plan) Empty() bool { return len(p.Items) == 0 }

// ObjectCount is how many objects the whole plan would apply.
func (p *Plan) ObjectCount() int {
	var n int
	for _, item := range p.Items {
		n += len(item.Objects)
	}
	return n
}

// Request addresses what to install.
type Request struct {
	// Components are the names asked for. Unknown names are an error naming the
	// table's contents, never a silent skip.
	Components []string
	// AllMissing selects every supported component detection reports absent.
	AllMissing bool
	// Profile is the detection result. It is an input rather than something this
	// package captures itself, so a plan is a pure function of (request,
	// profile, fetched bytes) and can be tested without a cluster.
	Profile clusterprofile.ClusterProfile
}

// Validate reports whether the request addresses something.
func (r Request) Validate() error {
	switch {
	case len(r.Components) == 0 && !r.AllMissing:
		return delivery.ApplyFailed("(request)", "components",
			"an install is addressed by component",
			"name one or more of "+strings.Join(Names(), ", ")+", or pass --all-missing")
	case len(r.Components) > 0 && r.AllMissing:
		return delivery.ApplyFailed("(request)", "components",
			"--all-missing and a named component both say what to install, and they disagree",
			"pass one of them")
	}
	return nil
}

// Options configures an Installer.
type Options struct {
	// Client applies and reads.
	Client dynamic.Interface
	// Mapper resolves the kinds in a pinned manifest. It never has to resolve a
	// CRD the same manifest registers: the only custom resource kelson applies
	// is the FluxInstance, whose coordinates are static.
	Mapper Mapper
	// Fetch retrieves pinned manifests.
	Fetch Fetcher
	// FieldManager overrides the field-manager identity; defaults to
	// FieldManager.
	FieldManager string
	// EstablishTimeout bounds the wait for a freshly applied CRD to be served.
	// Zero uses defaultEstablishTimeout; a negative value skips the wait.
	EstablishTimeout time.Duration
	// EstablishPoll is how often that wait re-reads the CRD. Zero uses
	// defaultEstablishPoll.
	EstablishPoll time.Duration
}

const (
	defaultEstablishTimeout = 2 * time.Minute
	defaultEstablishPoll    = 500 * time.Millisecond
)

// Installer plans and performs installs.
type Installer struct {
	client  dynamic.Interface
	mapper  Mapper
	fetch   Fetcher
	manager string
	timeout time.Duration
	poll    time.Duration
}

// New returns an Installer.
func New(opts Options) (*Installer, error) {
	switch {
	case opts.Client == nil:
		return nil, errors.New("install: a dynamic client is required")
	case opts.Mapper == nil:
		return nil, errors.New("install: a REST mapper is required to resolve the kinds in an install manifest")
	case opts.Fetch == nil:
		return nil, errors.New("install: a fetcher is required; kelson references upstream manifests and vendors none")
	}
	manager := opts.FieldManager
	if manager == "" {
		manager = FieldManager
	}
	timeout := opts.EstablishTimeout
	if timeout == 0 {
		timeout = defaultEstablishTimeout
	}
	poll := opts.EstablishPoll
	if poll == 0 {
		poll = defaultEstablishPoll
	}
	return &Installer{client: opts.Client, mapper: opts.Mapper, fetch: opts.Fetch,
		manager: manager, timeout: timeout, poll: poll}, nil
}

// Plan works out what an install would do, without applying anything.
//
// The order is the whole design. Detection decides eligibility BEFORE a byte is
// fetched, so a cluster that already runs cert-manager is told so instead of
// downloading a manifest kelson will refuse to apply. Then every eligible
// component is fetched and digest-verified — all of them, before any of them is
// applied — because an install that fails halfway through a two-component run
// with one applied and one unfetchable is the half-installed state this package
// exists to avoid.
func (i *Installer) Plan(ctx context.Context, req Request) (*Plan, error) {
	if err := req.Validate(); err != nil {
		return nil, err
	}
	wanted, err := i.resolve(req)
	if err != nil {
		return nil, err
	}

	plan := &Plan{}
	for _, c := range wanted {
		if refusal, refused := refuse(c, req.Profile, req.AllMissing); refused {
			if refusal != nil {
				plan.Refusals = append(plan.Refusals, *refusal)
			}
			continue
		}
		item, err := i.load(ctx, c)
		if err != nil {
			return nil, err
		}
		plan.Items = append(plan.Items, *item)
	}
	return plan, nil
}

// resolve turns a request into pin rows, in table order.
func (i *Installer) resolve(req Request) ([]Component, error) {
	if req.AllMissing {
		return Supported(), nil
	}
	seen := map[string]bool{}
	var out []Component
	for _, name := range req.Components {
		c, ok := Lookup(name)
		if !ok {
			return nil, delivery.ApplyFailed("(request)", "components",
				fmt.Sprintf("%q is not a component kelson knows how to install", name),
				"kelson installs: "+strings.Join(Names(), ", ")+". Anything else is a workload you install "+
					"yourself, and kelson will detect and adopt it (ADR-0005)")
		}
		if seen[c.Name] {
			continue
		}
		seen[c.Name] = true
		out = append(out, c)
	}
	// Table order, not argument order: the pins table puts flux first because
	// installing it changes what the other rows' previews mean the least, and a
	// preview whose order depends on how the flags were typed is harder to diff
	// between two runs.
	sort.SliceStable(out, func(a, b int) bool { return indexOf(out[a].Name) < indexOf(out[b].Name) })
	return out, nil
}

func indexOf(name string) int {
	for i, c := range Components {
		if c.Name == name {
			return i
		}
	}
	return len(Components)
}

// refuse decides whether a component is eligible, and what to say when it is
// not.
//
// A --all-missing run drops an ineligible component silently only when it is
// already present: "install what is missing" said nothing about it, and listing
// every satisfied component as a refusal would bury the two that matter. An
// Unknown is always reported, because "we could not tell" is exactly what a
// sweep must not swallow. A deferred row is always reported too — the user
// asked for something kelson has an answer about.
func refuse(c Component, prof clusterprofile.ClusterProfile, allMissing bool) (*Refusal, bool) {
	outcome, detail := c.Presence(prof)
	switch outcome {
	case clusterprofile.OutcomeYes:
		if allMissing {
			return nil, true
		}
		return &Refusal{
			Name:    c.Name,
			Outcome: outcome,
			Reason:  detail,
			Remediation: "kelson never modifies or upgrades a component it did not install (ADR-0005). It will " +
				"render against this one as detection reports it; `kelson profile` shows what that is",
		}, true
	case clusterprofile.OutcomeUnknown:
		return &Refusal{
			Name:    c.Name,
			Outcome: outcome,
			Reason:  detail,
			Remediation: "kelson will not install over a component it could not look for. Grant the probe the " +
				"permission named above and re-run, or install the component yourself and let detection adopt it",
		}, true
	}
	if c.Status != StatusSupported {
		return &Refusal{
			Name:        c.Name,
			Outcome:     clusterprofile.OutcomeNo,
			Reason:      "detection reports it absent, and kelson does not install it: " + c.FollowUp,
			Remediation: "install it yourself from upstream; kelson detects and adopts it (ADR-0003)",
		}, true
	}
	return nil, false
}

// load fetches, verifies and decodes one component's manifest.
func (i *Installer) load(ctx context.Context, c Component) (*Item, error) {
	body, err := i.fetch.Fetch(ctx, c.ManifestURL)
	if err != nil {
		return nil, unreachable(c.ManifestURL, err.Error())
	}
	if err := verifyDigest(c, body); err != nil {
		return nil, err
	}
	objects, err := i.decode(c, body)
	if err != nil {
		return nil, err
	}
	if c.Name == "flux" {
		obj, err := i.fluxInstance(c)
		if err != nil {
			return nil, err
		}
		objects = append(objects, *obj)
	}
	if err := i.readExistence(ctx, objects); err != nil {
		return nil, err
	}
	return &Item{Component: c, Objects: objects, Digest: c.SHA256}, nil
}

// decode splits the manifest into objects, stamps provenance and resolves each
// one's resource. Document order is apply order and is preserved: upstream
// publishes these manifests in an order that already puts the Namespace first
// and the CRDs before what uses them, and re-sorting them would be kelson
// second-guessing the people who wrote the install.
func (i *Installer) decode(c Component, body []byte) ([]Object, error) {
	reader := utilyaml.NewYAMLReader(bufio.NewReader(bytes.NewReader(body)))
	var out []Object
	for n := 1; ; n++ {
		doc, err := reader.Read()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, malformed(c, fmt.Sprintf("document %d could not be read: %v", n, err))
		}
		if len(bytes.TrimSpace(stripSeparator(doc))) == 0 {
			continue
		}
		obj := &unstructured.Unstructured{}
		jsonBytes, err := sigsyaml.YAMLToJSON(doc)
		if err != nil {
			return nil, malformed(c, fmt.Sprintf("document %d is not valid YAML: %v", n, err))
		}
		if err := obj.UnmarshalJSON(jsonBytes); err != nil {
			return nil, malformed(c, fmt.Sprintf("document %d is not a Kubernetes object: %v", n, err))
		}
		gvk := obj.GroupVersionKind()
		if gvk.Kind == "" || obj.GetName() == "" {
			return nil, malformed(c, fmt.Sprintf("document %d has no kind or no metadata.name", n))
		}
		object, err := i.object(c, obj, false)
		if err != nil {
			return nil, err
		}
		out = append(out, *object)
	}
	if len(out) == 0 {
		return nil, malformed(c, "it contains no objects")
	}
	return out, nil
}

// stripSeparator removes the leading document separator utilyaml's reader keeps
// on the first document of a stream that starts with one.
func stripSeparator(doc []byte) []byte {
	return bytes.TrimPrefix(bytes.TrimSpace(doc), []byte("---"))
}

// object stamps provenance on one decoded document and resolves its resource.
//
// The provenance labels are added to the object's own label map rather than
// replacing it: upstream's labels are upstream's, and an install that dropped
// them would break every selector the component's own manifests rely on.
func (i *Installer) object(c Component, obj *unstructured.Unstructured, authored bool) (*Object, error) {
	gvk := obj.GroupVersionKind()
	lbls := obj.GetLabels()
	if lbls == nil {
		lbls = map[string]string{}
	}
	lbls[LabelComponent] = c.Name
	lbls[LabelVersion] = c.Version
	obj.SetLabels(lbls)

	ref := Ref{
		APIVersion: gvk.GroupVersion().String(),
		Kind:       gvk.Kind,
		Name:       obj.GetName(),
		Namespace:  obj.GetNamespace(),
	}
	gvr, namespaced, err := i.resourceFor(gvk)
	if err != nil {
		return nil, delivery.ApplyFailed(ref.String(), "",
			fmt.Sprintf("no API resource for %s in this cluster: %v", gvk, err),
			"the pinned manifest for "+c.Title+" needs a kind this cluster does not serve; check the Kubernetes "+
				"version against docs/reference/support-matrix.md")
	}
	if !namespaced {
		ref.Namespace = ""
		obj.SetNamespace("")
	} else if ref.Namespace == "" {
		// A namespaced object with no namespace in the manifest belongs in the
		// component's namespace. Defaulting it here rather than at apply time
		// keeps the preview honest about where it lands.
		ref.Namespace = c.Namespace
		obj.SetNamespace(c.Namespace)
	}
	return &Object{Ref: ref, GVR: gvr, Authored: authored, obj: obj}, nil
}

// resourceFor resolves a kind, preferring the static coordinates for the two
// resources whose CRDs an install may itself be registering.
func (i *Installer) resourceFor(gvk schema.GroupVersionKind) (schema.GroupVersionResource, bool, error) {
	if gvk.GroupVersion() == fluxInstanceGVR.GroupVersion() && gvk.Kind == "FluxInstance" {
		return fluxInstanceGVR, true, nil
	}
	mapping, err := i.mapper.RESTMapping(gvk.GroupKind(), gvk.Version)
	if err != nil {
		return schema.GroupVersionResource{}, false, err
	}
	return mapping.Resource, mapping.Scope.Name() == meta.RESTScopeNameNamespace, nil
}

// fluxInstance composes the one custom resource kelson authors.
//
// It is built as an unstructured map, never as a typed object: flux-operator is
// AGPL-3.0 and kelson is MIT, so the integration is CR-only and no module of
// theirs is imported (package doc, docs/architecture.md).
//
// What it does NOT set is as deliberate as what it does. There is no spec.sync:
// pointing a fresh Flux at a Git repository is a delivery decision that belongs
// to `kelson deploy --mode flux` and to the user's repository layout, not to the
// act of installing Flux. An install that silently started reconciling a
// repository would be doing something nobody asked for.
func (i *Installer) fluxInstance(c Component) (*Object, error) {
	obj := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": fluxInstanceGVR.GroupVersion().String(),
		"kind":       "FluxInstance",
		"metadata": map[string]any{
			"name":      fluxInstanceName,
			"namespace": c.Namespace,
		},
		"spec": map[string]any{
			"distribution": map[string]any{
				"version":  FluxDistributionVersion,
				"registry": FluxDistributionRegistry,
			},
			"components": toAnySlice(FluxComponents),
			"cluster": map[string]any{
				"type": "kubernetes",
			},
		},
	}}
	return i.object(c, obj, true)
}

func toAnySlice(in []string) []any {
	out := make([]any, 0, len(in))
	for _, s := range in {
		out = append(out, s)
	}
	return out
}

// readExistence fills in Object.Exists for the preview.
//
// A read that fails for any reason other than NotFound leaves Exists false and
// is not an error: the preview is better with the answer and still correct
// without it, and the verdict that matters is re-established at apply time
// anyway. What must never happen is the opposite — treating an unreadable
// object as absent at APPLY time, which is why ownership is decided there by a
// read whose failure IS fatal (see stampOwnership).
func (i *Installer) readExistence(ctx context.Context, objects []Object) error {
	for idx := range objects {
		o := &objects[idx]
		_, err := i.client.Resource(o.GVR).Namespace(o.Ref.Namespace).Get(ctx, o.Ref.Name, metav1.GetOptions{})
		switch {
		case err == nil:
			o.Exists = true
		case apierrors.IsNotFound(err):
			o.Exists = false
		}
	}
	return nil
}

func malformed(c Component, detail string) error {
	return delivery.ApplyFailed(c.ManifestURL, "",
		"the pinned install manifest for "+c.Title+" "+c.Version+" could not be read: "+detail,
		"the digest matched, so these are the bytes upstream published — this is a kelson bug or an upstream "+
			"format change. Report it with the component name and version; nothing was applied")
}
