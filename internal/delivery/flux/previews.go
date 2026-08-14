package flux

import (
	"context"
	"errors"
	"sort"
	"strconv"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/dafrie/kelson/internal/preview/naming"
)

// Reading PR previews back out of the cluster (ADR-0017 stage 3).
//
// # Why the per-pull-request pair is the source, and not the namespaces
//
// A preview is three things in a cluster: an OCIRepository and a Kustomization
// that flux-operator instantiated from the ResourceSet's template, both in the
// *environment's* namespace, and the preview's own namespace that the applied
// artifact carries. Only the first two exist for every preview flux-operator
// knows about.
//
// That asymmetry decides the source. The failure ADR-0017 names as the visible
// one — an environment whose CI never ran `kelson preview publish` — produces an
// OCIRepository reporting that the artifact does not exist, a Kustomization
// waiting on it, and *no namespace at all*. Enumerating preview namespaces
// would report that environment as having no previews, which is precisely the
// wrong answer: flux-operator found the pull requests, kelson's publisher never
// ran, and a reader needs to see the pull request and the reason. So this reads
// the pair, and the preview namespace is a name it derives rather than a thing
// it lists.
//
// # What it does not read
//
// The ResourceSetInputProvider's exported inputs. flux-operator publishes what
// it found on the forge, and reporting it would answer "which pull requests are
// labelled" — a different question from "which previews exist", and one this
// reader would have to spell an upstream status field to answer. The pair is
// the state kelson's own manifests created; the poller is reported by its Ready
// condition alone.
//
// LICENSE: fluxcd.controlplane.io is flux-operator's API, and flux-operator is
// AGPL-3.0 while kelson is MIT. As everywhere else in this package it is read
// as unstructured data through GVRs written by hand, never as an imported Go
// module (docs/architecture.md, "Living with flux-operator").

var (
	// resourceSetGVR and inputProviderGVR are the lifecycle pair the renderer
	// writes (internal/renderer/previews.go).
	resourceSetGVR   = schema.GroupVersionResource{Group: "fluxcd.controlplane.io", Version: "v1", Resource: "resourcesets"}
	inputProviderGVR = schema.GroupVersionResource{Group: "fluxcd.controlplane.io", Version: "v1", Resource: "resourcesetinputproviders"}

	// ociRepositoryGVR is pinned at v1beta2 because that is the version kelson
	// *writes* — for chart sources (internal/renderer/helm.go) and inside the
	// ResourceSet's resourcesTemplate. A reader on a version the writer does not
	// use would be a second opinion about the same objects.
	ociRepositoryGVR = schema.GroupVersionResource{Group: "source.toolkit.fluxcd.io", Version: "v1beta2", Resource: "ocirepositories"}

	// httpRouteGVR is where a preview's hostnames actually are. kelson renders
	// Gateway API and nothing else (#140), so an HTTPRoute in the preview's
	// namespace is the rendered answer to "what does this preview serve on".
	httpRouteGVR = schema.GroupVersionResource{Group: "gateway.networking.k8s.io", Version: "v1", Resource: "httproutes"}
)

// PreviewScope addresses one environment's previews: whose they are, and where
// the lifecycle pair that owns them runs.
type PreviewScope struct {
	Project     string
	Environment string
	// Namespace is the *parent* environment's namespace — where the
	// ResourceSet, the ResourceSetInputProvider and every per-pull-request
	// OCIRepository and Kustomization live (ADR-0017 decision 3).
	Namespace string
}

// PreviewPhase is what one preview is doing, decided from the two conditions
// the pair carries. It is computed here rather than in a client for the reason
// the delivery state machine exists (issue #37): one place decides what a state
// means, and the CLI, the API and the UI render the same word.
type PreviewPhase string

const (
	// PreviewReady is fetched and applied: the change request is running.
	PreviewReady PreviewPhase = "ready"
	// PreviewApplying is fetched, with the apply not yet settled.
	PreviewApplying PreviewPhase = "applying"
	// PreviewAwaitingArtifact is the OCIRepository reporting that it cannot
	// fetch. It is the state an environment whose CI does not run
	// `kelson preview publish` sits in permanently (ADR-0017 decision 8), which
	// is why it is named for the cause rather than folded into "failed".
	PreviewAwaitingArtifact PreviewPhase = "awaiting-artifact"
	// PreviewFailed is fetched but not applied.
	PreviewFailed PreviewPhase = "failed"
	// PreviewUnknown is a pair that has not reported yet, or half a pair.
	PreviewUnknown PreviewPhase = "unknown"
)

// Preview is one change request's preview as the cluster reports it.
type Preview struct {
	// ID is the change request number, recovered from the object name. It is
	// what a human types when they go looking (ADR-0017 decision 2).
	ID string
	// Namespace is <project>-<environment>-pr<id>, which is also the name of
	// the OCIRepository and the Kustomization this preview is read from.
	Namespace string
	// SHA is the tag the OCIRepository pins: the change request's head commit,
	// substituted by flux-operator into the template kelson wrote.
	SHA string

	Phase PreviewPhase
	// Reason and Message are the condition that decided Phase — the artifact's
	// when it is not ready, the apply's otherwise.
	Reason  string
	Message string

	// Artifact and Applied are the two Ready conditions, kept separate so a
	// caller can tell "nothing was published" from "what was published did not
	// apply" without re-deriving it from Phase.
	Artifact ConditionState
	Applied  ConditionState

	// Revision is the Kustomization's lastAppliedRevision: the artifact digest
	// actually running in this preview.
	Revision string
	// Suspended is the Kustomization's spec.suspend. A suspended preview is
	// frozen, not broken, and its phase would otherwise read as settled.
	Suspended bool

	// Hosts are the hostnames the preview's HTTPRoutes claim, sorted. Empty
	// means the preview serves nothing routable, or that Gateway API is not
	// served on this cluster — the two are not distinguished here, because a
	// preview with no routes is the commoner of the two and neither is a
	// failure.
	Hosts []string

	// CreatedAt is when flux-operator created the Kustomization, which is when
	// this preview appeared. Zero when the API server reported none.
	CreatedAt time.Time
}

// PreviewLifecycle is the ResourceSetInputProvider / ResourceSet pair: the
// poller that finds change requests and the template that instantiates them.
// It answers the question a list of previews cannot — an empty list because
// nothing is labelled looks identical to an empty list because the poller
// cannot reach the forge.
type PreviewLifecycle struct {
	// Name is <project>-<environment>-previews, the name both objects share.
	Name string
	// Served is false when the cluster does not serve fluxcd.controlplane.io,
	// i.e. flux-operator is not installed. Nothing else in this struct means
	// anything then.
	Served bool
	// Present is whether the pair was found. False with Served true is an
	// environment whose manifests have not been delivered yet.
	Present bool

	// Provider is the ResourceSetInputProvider's Ready condition — whether the
	// forge is being polled at all.
	Provider        ConditionState
	ProviderReason  string
	ProviderMessage string

	// Set is the ResourceSet's Ready condition — whether the per-pull-request
	// objects are being instantiated.
	Set        ConditionState
	SetReason  string
	SetMessage string
}

// PreviewSet is everything this reader knows about one environment's previews.
type PreviewSet struct {
	Lifecycle PreviewLifecycle
	// Previews are sorted by change request number, highest first: the newest
	// pull request is the one a reader came for.
	Previews []Preview
}

// PreviewReader reads an environment's previews back. It is an interface for
// the same reason StatusReader is: the api plane may not import a Kubernetes
// client (.golangci.yml), so the seam is what crosses the boundary.
type PreviewReader interface {
	Previews(ctx context.Context, scope PreviewScope) (PreviewSet, error)
}

var _ PreviewReader = DynamicStatusReader{}

// Previews implements [PreviewReader].
//
// Every read is best-effort in one specific direction: a kind the cluster does
// not serve produces absence, never an error, because "flux-operator is not
// installed" and "Gateway API is not installed" are findings a caller renders
// rather than failures that should collapse the whole answer. A read that fails
// for any other reason is returned — a reader that reported an RBAC denial as
// "no previews" would be worse than one that reported nothing at all.
func (r DynamicStatusReader) Previews(ctx context.Context, scope PreviewScope) (PreviewSet, error) {
	if r.Client == nil {
		return PreviewSet{}, readErr("previews", errors.New("no dynamic client is configured"))
	}

	lifecycle := PreviewLifecycle{
		Name:     naming.Lifecycle(scope.Project, scope.Environment),
		Served:   true,
		Provider: ConditionUnknown,
		Set:      ConditionUnknown,
	}

	providers, err := r.previewList(ctx, inputProviderGVR, scope.Namespace)
	switch {
	case unserved(err):
		return PreviewSet{Lifecycle: PreviewLifecycle{Name: lifecycle.Name}}, nil
	case err != nil:
		return PreviewSet{}, readErr("ResourceSetInputProviders", err)
	}
	if provider := providers[lifecycle.Name]; provider != nil {
		lifecycle.Present = true
		lifecycle.Provider, lifecycle.ProviderReason, lifecycle.ProviderMessage = readyOf(provider.Object)
	}

	sets, err := r.previewList(ctx, resourceSetGVR, scope.Namespace)
	switch {
	case unserved(err):
		return PreviewSet{Lifecycle: PreviewLifecycle{Name: lifecycle.Name}}, nil
	case err != nil:
		return PreviewSet{}, readErr("ResourceSets", err)
	}
	if set := sets[lifecycle.Name]; set != nil {
		lifecycle.Present = true
		lifecycle.Set, lifecycle.SetReason, lifecycle.SetMessage = readyOf(set.Object)
	}

	previews, err := r.previewChildren(ctx, scope)
	if err != nil {
		return PreviewSet{}, err
	}
	return PreviewSet{Lifecycle: lifecycle, Previews: previews}, nil
}

// previewChildren pairs the per-pull-request OCIRepositories with their
// Kustomizations. Both are named for the preview, so the name is the join key
// and no ownership walk is needed.
func (r DynamicStatusReader) previewChildren(ctx context.Context, scope PreviewScope) ([]Preview, error) {
	prefix := naming.Preview(scope.Project, scope.Environment, "")

	allSources, err := r.previewList(ctx, ociRepositoryGVR, scope.Namespace)
	if err != nil && !unserved(err) {
		return nil, readErr("OCIRepositories", err)
	}
	allApplies, err := r.previewList(ctx, kustomizationGVR, scope.Namespace)
	if err != nil && !unserved(err) {
		return nil, readErr("Kustomizations", err)
	}
	sources, applies := children(allSources, prefix), children(allApplies, prefix)
	hosts, err := r.previewHosts(ctx, scope)
	if err != nil {
		return nil, err
	}

	names := make(map[string]struct{}, len(sources)+len(applies))
	for name := range sources {
		names[name] = struct{}{}
	}
	for name := range applies {
		names[name] = struct{}{}
	}

	out := make([]Preview, 0, len(names))
	for name := range names {
		p := Preview{
			ID:        strings.TrimPrefix(name, prefix),
			Namespace: name,
			Artifact:  ConditionUnknown,
			Applied:   ConditionUnknown,
			Hosts:     hosts[name],
		}
		var artifactReason, artifactMessage string
		if src := sources[name]; src != nil {
			p.SHA, _, _ = unstructured.NestedString(src.Object, "spec", "ref", "tag")
			p.Artifact, artifactReason, artifactMessage = readyOf(src.Object)
		}
		if apply := applies[name]; apply != nil {
			p.Applied, p.Reason, p.Message = readyOf(apply.Object)
			p.Revision, _, _ = unstructured.NestedString(apply.Object, "status", "lastAppliedRevision")
			p.Suspended, _, _ = unstructured.NestedBool(apply.Object, "spec", "suspend")
			p.CreatedAt = apply.GetCreationTimestamp().Time
		}
		p.Phase = previewPhase(p.Artifact, p.Applied)
		// The artifact's own words win when it is what is holding the preview
		// up: "no artifact found for tag <sha>" is the sentence that names the
		// missing publish, and the Kustomization's "dependency not ready" is
		// the consequence rather than the cause.
		if p.Phase == PreviewAwaitingArtifact {
			p.Reason, p.Message = artifactReason, artifactMessage
		}
		out = append(out, p)
	}

	// Highest change request number first. Sorting the number rather than the
	// string keeps pr9 below pr412, which a lexical sort would not.
	sort.Slice(out, func(i, j int) bool {
		a, b := previewOrder(out[i].ID), previewOrder(out[j].ID)
		if a != b {
			return a > b
		}
		return out[i].ID < out[j].ID
	})
	return out, nil
}

// previewOrder is the change request number as a number, or -1 for a name whose
// suffix is not one — which the naming scheme forbids, so it sorts last rather
// than being dropped: an object kelson did not expect is still an object in the
// environment's namespace, and hiding it would hide the surprise.
func previewOrder(id string) int {
	n, err := strconv.Atoi(id)
	if err != nil {
		return -1
	}
	return n
}

// previewHosts reads the hostnames each preview's HTTPRoutes claim, keyed by
// the preview's namespace.
//
// The label selector is the provenance every kelson-rendered object carries
// (internal/renderer's provenance.labels): the preview's manifests are the
// parent environment's render, so they name the *parent* environment — which is
// exactly what makes one selector answer for every preview at once. The
// namespace prefix is what separates the previews from the parent itself.
func (r DynamicStatusReader) previewHosts(ctx context.Context, scope PreviewScope) (map[string][]string, error) {
	list, err := r.Client.Resource(httpRouteGVR).List(ctx, metav1.ListOptions{
		LabelSelector: "app.kubernetes.io/managed-by=kelson," +
			"kelson.dev/project=" + scope.Project + "," +
			"kelson.dev/environment=" + scope.Environment,
	})
	if unserved(err) {
		// No Gateway API on this cluster. A preview still exists and still has
		// a phase; it has no hostnames kelson can read, which is a smaller
		// claim than a failed listing.
		return nil, nil
	}
	if err != nil {
		return nil, readErr("HTTPRoutes", err)
	}

	prefix := naming.Preview(scope.Project, scope.Environment, "")
	out := map[string][]string{}
	for i := range list.Items {
		ns := list.Items[i].GetNamespace()
		// The namespace is the check that matters and it is made here rather
		// than trusted to the selector: the selector narrows what crosses the
		// network, but only the name tells a preview's route from the parent
		// environment's, which carries the identical labels.
		if len(ns) <= len(prefix) || !strings.HasPrefix(ns, prefix) {
			continue
		}
		if !managedByKelson(list.Items[i].GetLabels(), scope) {
			continue
		}
		hosts, _, _ := unstructured.NestedStringSlice(list.Items[i].Object, "spec", "hostnames")
		out[ns] = append(out[ns], hosts...)
	}
	for ns := range out {
		sort.Strings(out[ns])
		out[ns] = dedupe(out[ns])
	}
	return out, nil
}

// managedByKelson repeats the selector's test locally. A fake API server does
// not apply label selectors and a real one does, so a check that lives only in
// the query is a check no test can hold this reader to.
func managedByKelson(labels map[string]string, scope PreviewScope) bool {
	return labels["app.kubernetes.io/managed-by"] == "kelson" &&
		labels["kelson.dev/project"] == scope.Project &&
		labels["kelson.dev/environment"] == scope.Environment
}

func dedupe(in []string) []string {
	out := in[:0]
	for i, s := range in {
		if i == 0 || in[i-1] != s {
			out = append(out, s)
		}
	}
	return out
}

// previewList lists one kind in one namespace, keyed by name.
//
// Listing and filtering in Go rather than asking the API server for one object
// is what makes "the CRD is not installed" a clean signal: a list against a
// resource the cluster does not serve is a 404 for the whole path, while a list
// against an installed one with nothing in it is an empty list. A Get cannot
// tell those two 404s apart without reading the status details, and the rest of
// this package lists for the same reason (dynamic.go).
func (r DynamicStatusReader) previewList(ctx context.Context, gvr schema.GroupVersionResource, namespace string) (map[string]*unstructured.Unstructured, error) {
	list, err := r.Client.Resource(gvr).Namespace(namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}
	out := make(map[string]*unstructured.Unstructured, len(list.Items))
	for i := range list.Items {
		out[list.Items[i].GetName()] = &list.Items[i]
	}
	return out, nil
}

// children keeps the entries whose names are previews of this scope: the prefix
// and at least one character of change request number after it.
func children(objs map[string]*unstructured.Unstructured, prefix string) map[string]*unstructured.Unstructured {
	out := make(map[string]*unstructured.Unstructured, len(objs))
	for name, obj := range objs {
		if len(name) > len(prefix) && strings.HasPrefix(name, prefix) {
			out[name] = obj
		}
	}
	return out
}

// unserved reports whether an error means the cluster does not serve this kind
// at all. A missing CRD reaches a dynamic client two ways — the REST mapper
// having no match, and the API server answering 404 for a path that does not
// exist — and both mean the same thing to a caller.
func unserved(err error) bool {
	return err != nil && (meta.IsNoMatchError(err) || apierrors.IsNotFound(err))
}

// readyOf extracts the Ready condition, defaulting to Unknown for an object
// that has not reported one.
func readyOf(obj map[string]any) (ConditionState, string, string) {
	for _, c := range conditionsOf(obj) {
		if c.kind == "Ready" {
			return ConditionState(c.status), c.reason, c.message
		}
	}
	return ConditionUnknown, "", ""
}

// previewPhase maps the pair's two conditions onto one word.
//
// The artifact is asked first and its failure is its own phase, because the
// commonest reason a preview does not exist is that nothing published it, and
// "failed" would send a reader to look at the manifests instead of at CI.
func previewPhase(artifact, applied ConditionState) PreviewPhase {
	switch artifact {
	case ConditionFalse:
		return PreviewAwaitingArtifact
	case ConditionUnknown:
		return PreviewUnknown
	}
	switch applied {
	case ConditionTrue:
		return PreviewReady
	case ConditionFalse:
		return PreviewFailed
	default:
		return PreviewApplying
	}
}
