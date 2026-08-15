package controlstore

import (
	"context"
	"fmt"
	"sort"

	"gopkg.in/yaml.v3"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/dafrie/kelson/api/kelson/v1alpha1"
	"github.com/dafrie/kelson/internal/model"
)

// The spec store, backed by custom resources (ADR-0027 decisions 1 and 6).
//
// # One project is one Project CR plus its Environment CRs
//
// A project's documents are exactly the objects a user would `kubectl apply`:
// a `kelson.dev/v1alpha1` Project and one Environment per environment, bound to
// it by `spec.project` and living beside it in the same namespace, which is the
// binding the controller resolves (internal/controller). PutSpec is therefore a
// server-side apply of that object set, with field manager [FieldManager], and
// GetSpec is a read of it — `kubectl get projects` answers the same question
// the RPC does, which is the whole prize ADR-0027 was after.
//
// # The authored bytes do not round-trip, and that is decided rather than lost
//
// ADR-0013 §1's ConfigMap store returned the user's YAML verbatim: comments,
// key order, the lot. A custom resource is a decoded, re-serialized object, so
// [SpecStore.Get] returns an *equivalent* document rather than the same bytes
// (ADR-0027 decision 6, stated there as a regression and accepted). The
// document store this project recommends is the user's own repository, where
// byte fidelity is git's job and always was.
//
// # Optimistic concurrency is the Project CR's resourceVersion
//
// [Stored.Version] is the Project's resourceVersion, passed back opaquely and
// asserted on the next write — the same contract, the same `store/version-
// conflict` code and the same remediation text the ConfigMap store had. The
// Project is the version anchor because a project's documents are written as a
// set: every Put rewrites the Project, so its version moves whenever any part
// of the project does.

// FieldManager is the field manager every write in this package claims
// (ADR-0027 decision 6). It is the server's identity in `managedFields`, which
// is what lets the controller's own status writes and an operator's `kubectl
// apply` coexist with kelson-server's spec writes without any of the three
// clobbering the others' fields.
const FieldManager = "kelson-server"

// Documents are the YAML documents of one project.
//
// They are bytes rather than a parsed model because that is the shape every
// caller wants: PutSpec receives what a client typed, GetSpec hands a UI editor
// something to show, and the render pipeline decodes them with the same
// model.DecodeDocuments the CLI runs. The store parses them on the way in only
// to build the custom resources, and re-serializes them on the way out.
type Documents struct {
	// Project is the project document (a `kind: Project` YAML document).
	Project []byte
	// Environments maps an environment name to its document.
	Environments map[string][]byte
}

// Stored is one project's spec as the store holds it.
type Stored struct {
	// Project is the project name.
	Project string
	// Documents are the documents, re-serialized from the custom resources.
	Documents Documents
	// Version is the Project custom resource's resourceVersion, opaque to
	// callers and passed back on the next write as the optimistic-concurrency
	// token (ADR-0027 decision 6: what Kubernetes already guarantees, kelson
	// does not reimplement).
	Version string
	// Environments are the environment names present, sorted, so a caller can
	// list environments without decoding any YAML.
	Environments []string
}

// PutOptions carries the write-time controls every mutating RPC shares
// (ADR-0013 §2).
type PutOptions struct {
	// ExpectedVersion is the Version the caller read before editing. It is
	// required to overwrite an existing project and must be empty to create
	// one: a write with neither a version nor Force is a blind overwrite, and
	// blind overwrites are exactly what optimistic concurrency exists to
	// refuse.
	ExpectedVersion string
	// Force overrides the version check. It is the deliberate "I know, take my
	// version" escape hatch, never a default.
	Force bool
	// IdempotencyKey, when it matches the key recorded on the stored Project,
	// makes this call a replay: the stored state is returned as success and
	// nothing is written.
	IdempotencyKey string
	// Image replaces the Project's `spec.image` before the objects are built.
	//
	// It exists for one caller: a Deploy carrying `--image`. Under the spine
	// the render happens in the controller, from the custom resource, so an
	// image override that was applied only to the server's own render would be
	// silently dropped on the way to the cluster — the deploy would report one
	// image and run another. Writing it is what makes it real, and it is the
	// same substitution the pipeline does (rule P3: it stands in for
	// `spec.image`, so a component or environment pin still wins).
	//
	// Empty means "leave the authored image alone", which is every other
	// caller.
	Image string
}

// DeleteOptions carries the same controls for a delete.
type DeleteOptions struct {
	ExpectedVersion string
	Force           bool
	// IdempotencyKey makes a delete replayable. A deleted object takes its
	// annotations with it, so a replayed delete is indistinguishable from a
	// first delete of a project that never existed; with a key present the
	// absence is therefore reported as success — the recorded outcome of a
	// completed delete — and without one it is a store/not-found.
	IdempotencyKey string
}

// SpecStoreOptions configures a SpecStore.
type SpecStoreOptions struct {
	// Client reads and writes the kelson.dev custom resources. It must be built
	// against a scheme with v1alpha1.AddToScheme applied.
	Client client.Client
	// Namespace is where the Project and Environment resources live.
	//
	// One namespace for the whole server, not one per project. The controller
	// binds an Environment to the Project of the same name *in its own
	// namespace* (internal/controller), so a project and its environments must
	// be co-located; a single server namespace guarantees that by construction,
	// keeps the server's RBAC to one namespace (ADR-0013 §3) and keeps the wire
	// API free of a namespace field it never had (ADR-0027 decision 6: the
	// ConnectRPC surface is unchanged).
	Namespace string
}

// SpecStore is the custom-resource-backed store of project specs.
type SpecStore struct {
	client    client.Client
	namespace string
}

// NewSpecStore returns a SpecStore reading and writing kelson.dev custom
// resources in one namespace.
func NewSpecStore(opts SpecStoreOptions) (*SpecStore, error) {
	if opts.Client == nil {
		return nil, fmt.Errorf("controlstore: a Kubernetes client is required")
	}
	if opts.Namespace == "" {
		return nil, fmt.Errorf("controlstore: a namespace is required")
	}
	return &SpecStore{client: opts.Client, namespace: opts.Namespace}, nil
}

// Put stores a project's documents, honouring optimistic concurrency and the
// idempotency-key replay contract.
//
// The documents are decoded before anything is written, so a malformed or
// mis-named document fails the whole call rather than leaving half a project
// applied. What is *not* re-checked here is semantic validity: model.Validate
// is the handler's job (ADR-0027 decision 5) and the API server's CEL rules
// refuse the cheap structural mistakes at apply time.
func (s *SpecStore) Put(ctx context.Context, project string, docs Documents, opts PutOptions) (Stored, error) {
	if err := validSegment("project", project); err != nil {
		return Stored{}, err
	}
	desired, err := s.resources(project, docs, opts.Image)
	if err != nil {
		return Stored{}, err
	}

	ref := specRef(project)
	current, found, err := s.read(ctx, project)
	if err != nil {
		return Stored{}, err
	}

	switch {
	case !found:
		if opts.ExpectedVersion != "" && !opts.Force {
			return Stored{}, NotFound(ref,
				fmt.Sprintf("no spec is stored for project %q, so version %q cannot be updated", project, opts.ExpectedVersion),
				"put without a version to create the project")
		}
	default:
		// The replay check comes before the version check: a retried request
		// carries the version its first attempt was written against, which the
		// first attempt has already superseded.
		if opts.IdempotencyKey != "" && current.project.Annotations[annIdempotencyKey] == opts.IdempotencyKey {
			return storedFrom(current)
		}
		if !opts.Force {
			if err := checkVersion(ref, project, opts.ExpectedVersion, current.project.ResourceVersion); err != nil {
				return Stored{}, err
			}
		}
	}

	if err := s.checkEnvironmentNames(ctx, project, desired); err != nil {
		return Stored{}, err
	}

	// The Project carries the version precondition, because it is the version
	// this store hands out. The Environments do not: they are written as part
	// of the same logical document set, and asserting a second precondition
	// would let a write fail for a version no caller was ever given.
	desired.project.Annotations = idempotencyAnnotation(opts.IdempotencyKey)
	if found && !opts.Force {
		desired.project.ResourceVersion = current.project.ResourceVersion
	}
	if err := s.apply(ctx, desired.project); err != nil {
		if apierrors.IsConflict(err) {
			return Stored{}, withCause(VersionConflict(ref,
				fmt.Sprintf("project %q changed while this write was in flight", project),
				"re-read the spec with GetSpec and retry the write with the version it returns"), err)
		}
		return Stored{}, fmt.Errorf("controlstore: write spec for %s: %w", ref, err)
	}
	for _, env := range desired.environments {
		if err := s.apply(ctx, env); err != nil {
			return Stored{}, fmt.Errorf("controlstore: write environment %s/%s: %w", project, env.Name, err)
		}
	}

	// An environment the caller no longer sends is an environment they deleted.
	// The ConfigMap store got this for free — one object held every document,
	// so a rewrite dropped the key — and a set of objects has to do it on
	// purpose, or a removed environment would keep reconciling forever.
	for name, obj := range current.environments {
		if _, kept := desired.byName[name]; kept {
			continue
		}
		if err := s.client.Delete(ctx, obj); err != nil && !apierrors.IsNotFound(err) {
			return Stored{}, fmt.Errorf("controlstore: remove environment %s/%s: %w", project, name, err)
		}
	}

	written, found, err := s.read(ctx, project)
	if err != nil {
		return Stored{}, err
	}
	if !found {
		return Stored{}, fmt.Errorf("controlstore: project %q disappeared immediately after it was written", project)
	}
	return storedFrom(written)
}

// Get returns one project's documents, re-serialized from the stored resources.
func (s *SpecStore) Get(ctx context.Context, project string) (Stored, error) {
	if err := validSegment("project", project); err != nil {
		return Stored{}, err
	}
	current, found, err := s.read(ctx, project)
	if err != nil {
		return Stored{}, err
	}
	if !found {
		return Stored{}, notStored(project)
	}
	return storedFrom(current)
}

// List returns every stored project, ordered by name.
func (s *SpecStore) List(ctx context.Context) ([]Stored, error) {
	var projects v1alpha1.ProjectList
	if err := s.client.List(ctx, &projects, client.InNamespace(s.namespace)); err != nil {
		return nil, fmt.Errorf("controlstore: list projects in %s: %w", s.namespace, err)
	}
	var environments v1alpha1.EnvironmentList
	if err := s.client.List(ctx, &environments, client.InNamespace(s.namespace)); err != nil {
		return nil, fmt.Errorf("controlstore: list environments in %s: %w", s.namespace, err)
	}

	out := make([]Stored, 0, len(projects.Items))
	for i := range projects.Items {
		p := &projects.Items[i]
		set := resourceSet{project: p, environments: map[string]*v1alpha1.Environment{}}
		for j := range environments.Items {
			env := &environments.Items[j]
			if env.Spec.Project == p.Name {
				set.environments[env.Name] = env
			}
		}
		stored, err := storedFrom(set)
		if err != nil {
			return nil, err
		}
		out = append(out, stored)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Project < out[j].Project })
	return out, nil
}

// Delete removes a project and every environment bound to it.
//
// The Project goes last. A reader that catches the store mid-delete sees a
// project with fewer environments rather than orphaned Environments with no
// Project to validate against, and the controller already reports the latter as
// ReasonProjectNotFound — a state worth passing through quickly, not one worth
// leaving behind if the delete is interrupted.
func (s *SpecStore) Delete(ctx context.Context, project string, opts DeleteOptions) error {
	if err := validSegment("project", project); err != nil {
		return err
	}
	ref := specRef(project)
	current, found, err := s.read(ctx, project)
	if err != nil {
		return err
	}
	if !found {
		if opts.IdempotencyKey != "" {
			return nil
		}
		return notStored(project)
	}
	if !opts.Force {
		if err := checkVersion(ref, project, opts.ExpectedVersion, current.project.ResourceVersion); err != nil {
			return err
		}
	}

	for name, env := range current.environments {
		if err := s.client.Delete(ctx, env); err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("controlstore: delete environment %s/%s: %w", project, name, err)
		}
	}
	rv := current.project.ResourceVersion
	delOpts := []client.DeleteOption{}
	if rv != "" && !opts.Force {
		delOpts = append(delOpts, client.Preconditions{ResourceVersion: &rv})
	}
	err = s.client.Delete(ctx, current.project, delOpts...)
	switch {
	case apierrors.IsNotFound(err):
		return nil // Someone else deleted it; the requested end state holds.
	case apierrors.IsConflict(err):
		return withCause(VersionConflict(ref,
			fmt.Sprintf("project %q changed while the delete was in flight", project),
			"re-read the spec with GetSpec and retry the delete with the version it returns"), err)
	case err != nil:
		return fmt.Errorf("controlstore: delete spec for %s: %w", ref, err)
	}
	return nil
}

// checkEnvironmentNames refuses a write that would take an Environment object
// another project already owns.
//
// An Environment's *object name* is its environment name — the controller reads
// it back as one (internal/controller's modelEnvironment) — so within one
// namespace `production` is a single object, and two projects cannot both have
// one. The store shares a namespace by design ([SpecStoreOptions.Namespace]),
// so this is reachable, and the failure mode if it were not checked is the
// worst kind: a server-side apply would silently rebind the other project's
// environment, and the losing project's spec would come back from GetSpec with
// an environment missing.
//
// It is reported as a conflict rather than fixed, because both spellings of a
// fix are decisions this store may not take on its own: renaming the object
// would break the round trip, and giving each project a namespace of its own
// would need namespace-create permission the server deliberately does not hold
// (ADR-0003's additive-install doctrine). Tracked as the per-project-namespace
// follow-up on ADR-0027.
func (s *SpecStore) checkEnvironmentNames(ctx context.Context, project string, desired resourceSet) error {
	if len(desired.environments) == 0 {
		return nil
	}
	var list v1alpha1.EnvironmentList
	if err := s.client.List(ctx, &list, client.InNamespace(s.namespace)); err != nil {
		return fmt.Errorf("controlstore: list environments in %s: %w", s.namespace, err)
	}
	for i := range list.Items {
		env := &list.Items[i]
		if env.Spec.Project == project {
			continue
		}
		if _, wanted := desired.environments[env.Name]; !wanted {
			continue
		}
		return VersionConflict(specRef(project),
			fmt.Sprintf("environment %q in namespace %s already belongs to project %q",
				env.Name, s.namespace, env.Spec.Project),
			fmt.Sprintf("an Environment's object name is its environment name, so %s holds one %q for all projects. "+
				"Rename this environment, or run the two projects against servers configured with different namespaces",
				s.namespace, env.Name))
	}
	return nil
}

// apply is the one write primitive: a server-side apply under [FieldManager],
// forcing ownership of the fields this server manages.
//
// Force is not a licence to overwrite a concurrent *spec* write — that is what
// the resourceVersion precondition above refuses — but a statement about
// managed-field ownership: a field kelson-server previously set and another
// manager has since claimed is still kelson-server's to set, and the
// alternative is a write that fails with a field-manager conflict no API caller
// could have anticipated or resolved.
func (s *SpecStore) apply(ctx context.Context, obj client.Object) error {
	// The object is converted to unstructured rather than applied typed,
	// because controller-runtime's typed apply takes a generated apply
	// configuration and this repository generates none: ADR-0027 decision 2
	// refuses the kubebuilder scaffolding that would produce them, and
	// controller-gen's applyconfiguration generator is part of it. Converting
	// here costs one reflection pass per write and keeps the generated-artifact
	// convention at the two entries ADR-0027 chose (deepcopy and the CRDs).
	raw, err := runtime.DefaultUnstructuredConverter.ToUnstructured(obj)
	if err != nil {
		return fmt.Errorf("controlstore: encoding %s %s: %w", obj.GetObjectKind().GroupVersionKind().Kind, obj.GetName(), err)
	}
	return s.client.Apply(ctx, client.ApplyConfigurationFromUnstructured(&unstructured.Unstructured{Object: raw}),
		client.FieldOwner(FieldManager), client.ForceOwnership)
}

// resourceSet is one project's objects as the cluster holds them.
type resourceSet struct {
	project      *v1alpha1.Project
	environments map[string]*v1alpha1.Environment
	// byName is the desired-state twin of environments, used by Put to decide
	// which stored environments a write removes.
	byName map[string]bool
}

// read loads a project and the environments bound to it.
func (s *SpecStore) read(ctx context.Context, project string) (resourceSet, bool, error) {
	var p v1alpha1.Project
	err := s.client.Get(ctx, client.ObjectKey{Namespace: s.namespace, Name: project}, &p)
	if apierrors.IsNotFound(err) {
		return resourceSet{}, false, nil
	}
	if err != nil {
		return resourceSet{}, false, fmt.Errorf("controlstore: read spec for %s: %w", specRef(project), err)
	}

	var list v1alpha1.EnvironmentList
	if err := s.client.List(ctx, &list, client.InNamespace(s.namespace)); err != nil {
		return resourceSet{}, false, fmt.Errorf("controlstore: list environments for %s: %w", specRef(project), err)
	}
	set := resourceSet{project: &p, environments: map[string]*v1alpha1.Environment{}}
	for i := range list.Items {
		env := &list.Items[i]
		if env.Spec.Project == project {
			set.environments[env.Name] = env
		}
	}
	return set, true, nil
}

// resources decodes the authored documents into the objects a Put applies.
func (s *SpecStore) resources(project string, docs Documents, image string) (resourceSet, error) {
	if len(docs.Project) == 0 {
		return resourceSet{}, fmt.Errorf("controlstore: project %q has no project document", project)
	}
	mp, err := decodeOne[*model.Project](docs.Project, "project")
	if err != nil {
		return resourceSet{}, err
	}
	if mp.Metadata.Name != project {
		return resourceSet{}, fmt.Errorf("controlstore: the project document names %q and the request names %q",
			mp.Metadata.Name, project)
	}
	if image != "" {
		mp.Spec.Image = image
	}

	out := resourceSet{
		project:      &v1alpha1.Project{Spec: mp.Spec},
		environments: map[string]*v1alpha1.Environment{},
		byName:       map[string]bool{},
	}
	out.project.APIVersion = model.APIVersion
	out.project.Kind = v1alpha1.KindProject
	out.project.Name = project
	out.project.Namespace = s.namespace
	out.project.Labels = specLabels(project)

	for name, doc := range docs.Environments {
		if err := validSegment("environment", name); err != nil {
			return resourceSet{}, err
		}
		if len(doc) == 0 {
			return resourceSet{}, fmt.Errorf("controlstore: environment %q has an empty document", name)
		}
		me, err := decodeOne[*model.Environment](doc, "environment "+name)
		if err != nil {
			return resourceSet{}, err
		}
		if me.Metadata.Name != name {
			return resourceSet{}, fmt.Errorf("controlstore: environment document %q names %q", name, me.Metadata.Name)
		}
		// spec.project is what binds the Environment to its Project in the
		// cluster, and it is what List and read filter on. A document that
		// names another project would be stored here and reconciled against a
		// different Project than the one it was filed under.
		if me.Spec.Project != project {
			return resourceSet{}, fmt.Errorf("controlstore: environment %q declares spec.project %q and is being stored under project %q",
				name, me.Spec.Project, project)
		}
		env := &v1alpha1.Environment{Spec: me.Spec}
		env.APIVersion = model.APIVersion
		env.Kind = v1alpha1.KindEnvironment
		env.Name = name
		env.Namespace = s.namespace
		env.Labels = envLabels(project, name)
		out.environments[name] = env
		out.byName[name] = true
	}
	return out, nil
}

// decodeOne decodes exactly one document of the expected kind. The taxonomy is
// model's own (model.Errors), so a client that sends a malformed spec reads the
// same `schema/...` codes it would have read from the CLI.
func decodeOne[T any](doc []byte, what string) (T, error) {
	var zero T
	parsed, errs := model.DecodeDocuments(doc)
	if len(errs) > 0 {
		return zero, errs
	}
	if len(parsed) != 1 {
		return zero, fmt.Errorf("controlstore: the %s document holds %d documents; it must hold exactly one", what, len(parsed))
	}
	typed, ok := parsed[0].(T)
	if !ok {
		return zero, fmt.Errorf("controlstore: the %s document is a %T", what, parsed[0])
	}
	return typed, nil
}

// storedFrom re-serializes a set of custom resources as authored documents.
func storedFrom(set resourceSet) (Stored, error) {
	stored := Stored{
		Project: set.project.Name,
		Version: set.project.ResourceVersion,
		Documents: Documents{
			Environments: map[string][]byte{},
		},
	}
	doc, err := encodeDocument(&model.Project{
		TypeMeta: typeMeta(v1alpha1.KindProject),
		Metadata: model.ObjectMeta{Name: set.project.Name},
		Spec:     set.project.Spec,
	})
	if err != nil {
		return Stored{}, fmt.Errorf("controlstore: serializing project %q: %w", set.project.Name, err)
	}
	stored.Documents.Project = doc

	for name, env := range set.environments {
		doc, err := encodeDocument(&model.Environment{
			TypeMeta: typeMeta(v1alpha1.KindEnvironment),
			Metadata: model.ObjectMeta{Name: name},
			Spec:     env.Spec,
		})
		if err != nil {
			return Stored{}, fmt.Errorf("controlstore: serializing environment %q: %w", name, err)
		}
		stored.Documents.Environments[name] = doc
		stored.Environments = append(stored.Environments, name)
	}
	sort.Strings(stored.Environments)
	return stored, nil
}

// encodeDocument renders one model document as the YAML a user would have
// written. gopkg.in/yaml.v3 is deliberate: the model's yaml tags are what
// model.DecodeDocuments reads, so encoding through them is what makes a
// GetSpec → PutSpec round trip land on the same object.
func encodeDocument(doc any) ([]byte, error) {
	return yaml.Marshal(doc)
}

func typeMeta(kind string) model.TypeMeta {
	return model.TypeMeta{APIVersion: model.APIVersion, Kind: kind}
}

func specLabels(project string) map[string]string {
	return map[string]string{
		labelManagedBy: managedByKelson,
		labelProject:   project,
		labelState:     stateSpec,
	}
}

func envLabels(project, environment string) map[string]string {
	l := specLabels(project)
	l[labelEnvironment] = environment
	return l
}

// idempotencyAnnotation describes the write that produced the current contents,
// so a write without a key clears a predecessor's rather than inheriting it and
// making an unrelated request look like a replay.
func idempotencyAnnotation(key string) map[string]string {
	if key == "" {
		return nil
	}
	return map[string]string{annIdempotencyKey: key}
}

// checkVersion is the optimistic-concurrency gate. An empty expected version
// against an existing object is refused rather than treated as "create or
// overwrite": the caller that has not read the spec cannot know what it is
// about to replace.
func checkVersion(ref, project, expected, actual string) error {
	if expected == "" {
		return VersionConflict(ref,
			fmt.Sprintf("project %q already exists and the write carried no version", project),
			"read the spec with GetSpec and put it back with the version it returned, or set force to overwrite deliberately")
	}
	if expected != actual {
		return VersionConflict(ref,
			fmt.Sprintf("project %q is at version %q, the write expected %q", project, actual, expected),
			"re-read the spec with GetSpec, re-apply the edit and retry with the version it returns")
	}
	return nil
}

func notStored(project string) Error {
	return NotFound(specRef(project),
		fmt.Sprintf("no spec is stored for project %q", project),
		"list the stored projects with ListSpecs, or create this one with PutSpec")
}

// specRef is the resource identity carried on errors: the project, not the
// custom resource, because which object holds a project is an implementation
// detail this store has already changed once (ADR-0027 supersedes ADR-0013 §1).
func specRef(project string) string { return "spec/" + project }

// NewClient builds the typed client the spec store needs, from the REST config
// a live connection already resolved (internal/delivery/kube's Cluster.Config).
//
// It exists so that no caller outside this package has to name a
// controller-runtime type. The API plane's depguard rule (.golangci.yml) allows
// kelson-server the transport and the generated code and nothing else, exactly
// so a handler cannot reach for a cluster call instead of going through a
// store; building the client here keeps that fence intact while still letting
// the server hand this package a connection.
//
// It is a *watching* client because [EnvironmentStore] follows an
// Environment's status while a deployment is in flight, and a client that could
// not watch would leave the façade polling for a change the API server is able
// to push. client.WithWatch is a client.Client, so [SpecStore] is unaffected.
func NewClient(cfg *rest.Config) (client.WithWatch, error) {
	s := runtime.NewScheme()
	if err := v1alpha1.AddToScheme(s); err != nil {
		return nil, fmt.Errorf("controlstore: registering kelson.dev/v1alpha1: %w", err)
	}
	c, err := client.NewWithWatch(cfg, client.Options{Scheme: s})
	if err != nil {
		return nil, fmt.Errorf("controlstore: building the custom-resource client: %w", err)
	}
	return c, nil
}
