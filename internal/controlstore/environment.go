package controlstore

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/watch"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/dafrie/kelson/api/kelson/v1alpha1"
	"github.com/dafrie/kelson/internal/model"
)

// The Environment status reader: what the server's delivery verbs read instead
// of a journal (ADR-0027 decision 6, ADR-0028 decision 4).
//
// # Why this is a store and not a handler concern
//
// `Environment.status` is where the controller records what it delivered:
// the phase, the settled revision, the conditions and the bounded history
// mirror. Every one of the façade's delivery verbs is a projection of it —
// Deploy streams it, Status reports it, History lists its history, Rollback
// waits on it — and the api plane may not hold a Kubernetes client
// (.golangci.yml). So the read, the watch and the annotation patch live here,
// behind plain Go types, and the handlers project.
//
// # It reads, and it writes exactly one thing
//
// [EnvironmentStore.Annotate] is the only write, and it is a merge patch of
// `metadata.annotations` — never a server-side apply. An apply states a
// complete intent for the fields its manager owns, so applying an object that
// carries only annotations under [FieldManager] would delete the spec that
// manager wrote. The two rollback and promotion annotations of ADR-0028 are
// exactly what `kubectl annotate` writes, and this writes them the same way.

// AnnotationManager is the field manager annotation patches claim.
//
// It is deliberately not [FieldManager]. A later spec write is a server-side
// apply that mentions no annotations, and server-side apply removes the fields
// the *applier* owns and no longer specifies — so stamping provenance under the
// same manager would make `kelson.dev/promoted-from` disappear on the next
// PutSpec. Under its own manager the annotation is owned by nobody the spec
// apply speaks for, and it survives until something removes it deliberately.
const AnnotationManager = FieldManager + "-annotations"

// EnvironmentState is one Environment custom resource as the façade reads it:
// its identity, its generation bookkeeping and everything the controller
// recorded in `status`.
//
// The fields are plain Go types rather than the API machinery's, because the
// api plane may not import them. The one exception is deliberate:
// ValidationErrors are [model.Errors], the same taxonomy the CLI and the wire
// carry (ADR-0027 decision 5) — converting them to a third shape here would
// make the status a second dialect of the same vocabulary.
type EnvironmentState struct {
	// Project and Environment are the identity the caller asked for.
	Project     string
	Environment string

	// Namespace is `spec.namespace` as authored — empty means the resolver's
	// default applies. It is not the resolved namespace: resolving is the
	// model's job and this store does not run it.
	Namespace string

	// Generation is `.metadata.generation`: the revision number of the *spec*,
	// bumped by the API server on every spec change.
	Generation int64

	// ObservedGeneration is the generation the status describes. A status
	// trailing the generation has not caught up, and every consumer must read
	// it before believing the rest.
	ObservedGeneration int64

	// Phase is the delivery phase, one of the v1alpha1.Phase* strings. Empty
	// until something has been delivered.
	Phase string

	// Revision is the settled revision: the artifact tag the OCIRepository is
	// pinned to. Empty until something has been published.
	Revision string

	// RollbackRevision is the rollback target the controller has acted on, and
	// RollbackGeneration the generation it was applied at (ADR-0028
	// decision 5).
	RollbackRevision   string
	RollbackGeneration int64

	// Conditions are Ready and Progressing as the controller wrote them.
	Conditions []Condition

	// History is the bounded mirror of published revisions, newest first.
	History []Revision

	// ValidationErrors is what validate.go said about this Environment, with
	// the slash codes intact.
	ValidationErrors model.Errors

	// Annotations are the object's annotations, so a caller can see the
	// rollback pin it just wrote without a second read.
	Annotations map[string]string
}

// Condition is one status condition, flattened.
type Condition struct {
	Type    string
	Status  string // "True", "False" or "Unknown"
	Reason  string
	Message string
	// Since is the condition's last transition time.
	Since time.Time
	// ObservedGeneration is the generation the condition was written for.
	ObservedGeneration int64
}

// True reports whether the condition holds.
func (c Condition) True() bool { return c.Status == string(conditionTrue) }

// Revision is one entry of the history mirror (ADR-0028 decision 4).
type Revision struct {
	Revision  string
	Digest    string
	SpecHash  string
	Images    []string
	Outcome   string
	Timestamp time.Time
}

// conditionStatus mirrors metav1.ConditionStatus without exporting it: the api
// plane compares against [Condition.True] and never sees the constant.
type conditionStatus string

const conditionTrue conditionStatus = "True"

// Condition returns the named condition.
func (s EnvironmentState) Condition(name string) (Condition, bool) {
	for _, c := range s.Conditions {
		if c.Type == name {
			return c, true
		}
	}
	return Condition{}, false
}

// Ready is the summary condition both kinds carry.
func (s EnvironmentState) Ready() (Condition, bool) { return s.Condition(v1alpha1.ConditionReady) }

// Progressing says whether kelson is still working on this Environment. Its
// absence is treated as "in flight" by [EnvironmentState.Settled], because a
// status that has not been written yet has not finished anything.
func (s EnvironmentState) Progressing() (Condition, bool) {
	return s.Condition(v1alpha1.ConditionProgressing)
}

// Current reports whether the status describes the spec as it stands now.
func (s EnvironmentState) Current() bool {
	return s.Generation > 0 && s.ObservedGeneration >= s.Generation
}

// Settled reports whether nothing is in flight for the current generation.
//
// It is the controller's own verdict rather than a second opinion assembled
// from the phase: `Progressing=False` is written on exactly the paths where
// the controller has stopped — a terminal phase, a refusal it will not retry
// without a human, and a rollback pin — and reading the phase instead would
// mean re-deriving that table in a second place and letting the two disagree.
func (s EnvironmentState) Settled() bool {
	if !s.Current() {
		return false
	}
	progressing, ok := s.Progressing()
	return ok && !progressing.True()
}

// HeadRevision returns the newest history entry, which is the revision the
// environment most recently published.
func (s EnvironmentState) HeadRevision() (Revision, bool) {
	if len(s.History) == 0 {
		return Revision{}, false
	}
	return s.History[0], true
}

// FindRevision returns the history entry for one revision.
func (s EnvironmentState) FindRevision(revision string) (Revision, bool) {
	for _, r := range s.History {
		if r.Revision == revision {
			return r, true
		}
	}
	return Revision{}, false
}

// Running returns the entry for the revision the environment is serving: the
// settled revision when the status names one, the head of the history
// otherwise.
//
// The two differ exactly while a rollback is pinned, where `status.revision` is
// an older tag than the newest published one — and what a promotion reads is
// what the source environment *runs*, not the last thing it built (ADR-0016
// decision 2).
func (s EnvironmentState) Running() (Revision, bool) {
	if s.Revision != "" {
		if r, ok := s.FindRevision(s.Revision); ok {
			return r, true
		}
	}
	return s.HeadRevision()
}

// PreviousRevision returns the newest published revision that is not the one
// being served — the target of a rollback that names none.
func (s EnvironmentState) PreviousRevision() (Revision, bool) {
	current := s.Revision
	if current == "" {
		if head, ok := s.HeadRevision(); ok {
			current = head.Revision
		}
	}
	for _, r := range s.History {
		if r.Revision != current {
			return r, true
		}
	}
	return Revision{}, false
}

// EnvironmentStoreOptions configures an [EnvironmentStore].
type EnvironmentStoreOptions struct {
	// Client reads, watches and patches the kelson.dev custom resources. It is
	// a watching client because a deployment's progress is reported by the
	// controller writing status, and polling for it would either lag or hammer
	// the API server.
	Client client.WithWatch
	// Namespace is where the Project and Environment resources live — the same
	// single server namespace [SpecStoreOptions] documents.
	Namespace string
}

// EnvironmentStore reads and watches Environment status, and stamps the two
// annotations ADR-0028 defines.
type EnvironmentStore struct {
	client    client.WithWatch
	namespace string
}

// NewEnvironmentStore returns a store over one namespace.
func NewEnvironmentStore(opts EnvironmentStoreOptions) (*EnvironmentStore, error) {
	if opts.Client == nil {
		return nil, fmt.Errorf("controlstore: a watching Kubernetes client is required")
	}
	if opts.Namespace == "" {
		return nil, fmt.Errorf("controlstore: a namespace is required")
	}
	return &EnvironmentStore{client: opts.Client, namespace: opts.Namespace}, nil
}

// Get reads one environment's state.
//
// An Environment object whose `spec.project` names another project is reported
// as not found rather than returned: the object name is the environment name
// and the namespace is shared, so "production" exists at most once — and
// answering a question about shop/production with the state of billing's would
// be the worst possible way to learn that.
func (s *EnvironmentStore) Get(ctx context.Context, project, environment string) (EnvironmentState, error) {
	if err := validSegment("project", project); err != nil {
		return EnvironmentState{}, err
	}
	if err := validSegment("environment", environment); err != nil {
		return EnvironmentState{}, err
	}
	var env v1alpha1.Environment
	err := s.client.Get(ctx, client.ObjectKey{Namespace: s.namespace, Name: environment}, &env)
	if apierrors.IsNotFound(err) {
		return EnvironmentState{}, notDelivered(project, environment)
	}
	if err != nil {
		return EnvironmentState{}, fmt.Errorf("controlstore: read %s: %w", environmentRef(project, environment), err)
	}
	if env.Spec.Project != project {
		return EnvironmentState{}, NotFound(environmentRef(project, environment),
			fmt.Sprintf("environment %q in namespace %s belongs to project %q", environment, s.namespace, env.Spec.Project),
			"name the project that owns this environment, or rename the environment")
	}
	return environmentState(project, &env), nil
}

// Watch streams one environment's state: the current one first, then every
// change until the context is cancelled or the object is deleted.
//
// The channel is closed when the watch ends for any reason, so a caller ranges
// over it and then asks the context why it stopped. Nothing is buffered beyond
// one state — a consumer that stops reading blocks the sender, which is
// correct: the sender is a goroutine owned by the caller's own context and it
// goes away with it.
func (s *EnvironmentStore) Watch(ctx context.Context, project, environment string) (<-chan EnvironmentState, error) {
	current, err := s.Get(ctx, project, environment)
	if err != nil {
		return nil, err
	}
	// The watch is over the namespace rather than one object because a name
	// field selector is not served for custom resources by every API server
	// version, and filtering two names in this process costs nothing.
	w, err := s.client.Watch(ctx, &v1alpha1.EnvironmentList{}, client.InNamespace(s.namespace))
	if err != nil {
		return nil, fmt.Errorf("controlstore: watch %s: %w", environmentRef(project, environment), err)
	}

	out := make(chan EnvironmentState, 1)
	out <- current
	go func() {
		defer close(out)
		defer w.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case event, ok := <-w.ResultChan():
				if !ok {
					return
				}
				env, ok := event.Object.(*v1alpha1.Environment)
				if !ok || env.Name != environment || env.Spec.Project != project {
					continue
				}
				if event.Type == watch.Deleted {
					// The environment is gone. Ending the stream is the honest
					// answer: there is no state left to report, and the caller
					// re-reads to find out it is not found.
					return
				}
				select {
				case out <- environmentState(project, env):
				case <-ctx.Done():
					return
				}
			}
		}
	}()
	return out, nil
}

// Annotate merge-patches annotations onto one Environment and returns the state
// as the API server answered it.
//
// An empty value removes the annotation, which is JSON merge patch's own
// meaning for a null. A merge patch and not an apply: see the package comment
// above [AnnotationManager].
func (s *EnvironmentStore) Annotate(ctx context.Context, project, environment string, annotations map[string]string) (EnvironmentState, error) {
	if len(annotations) == 0 {
		return EnvironmentState{}, fmt.Errorf("controlstore: annotating %s with nothing", environmentRef(project, environment))
	}
	// The read is the existence and ownership check: patching an object that
	// belongs to another project would silently roll back somebody else's
	// environment.
	if _, err := s.Get(ctx, project, environment); err != nil {
		return EnvironmentState{}, err
	}

	values := make(map[string]any, len(annotations))
	for k, v := range annotations {
		if v == "" {
			values[k] = nil
			continue
		}
		values[k] = v
	}
	body, err := json.Marshal(map[string]any{"metadata": map[string]any{"annotations": values}})
	if err != nil {
		return EnvironmentState{}, fmt.Errorf("controlstore: encoding the annotation patch for %s: %w",
			environmentRef(project, environment), err)
	}

	env := &v1alpha1.Environment{}
	env.Name, env.Namespace = environment, s.namespace
	if err := s.client.Patch(ctx, env, client.RawPatch(types.MergePatchType, body),
		client.FieldOwner(AnnotationManager)); err != nil {
		if apierrors.IsNotFound(err) {
			return EnvironmentState{}, notDelivered(project, environment)
		}
		return EnvironmentState{}, fmt.Errorf("controlstore: annotate %s: %w", environmentRef(project, environment), err)
	}
	return environmentState(project, env), nil
}

// environmentState projects a custom resource onto the façade's view.
func environmentState(project string, env *v1alpha1.Environment) EnvironmentState {
	state := EnvironmentState{
		Project:            project,
		Environment:        env.Name,
		Namespace:          env.Spec.Namespace,
		Generation:         env.Generation,
		ObservedGeneration: env.Status.ObservedGeneration,
		Phase:              env.Status.Phase,
		Revision:           env.Status.Revision,
		RollbackRevision:   env.Status.RollbackRevision,
		RollbackGeneration: env.Status.RollbackGeneration,
		Annotations:        env.Annotations,
	}
	for _, c := range env.Status.Conditions {
		state.Conditions = append(state.Conditions, Condition{
			Type:               c.Type,
			Status:             string(c.Status),
			Reason:             c.Reason,
			Message:            c.Message,
			Since:              c.LastTransitionTime.Time,
			ObservedGeneration: c.ObservedGeneration,
		})
	}
	for _, h := range env.Status.History {
		state.History = append(state.History, Revision{
			Revision:  h.Revision,
			Digest:    h.Digest,
			SpecHash:  h.SpecHash,
			Images:    h.Images,
			Outcome:   h.Outcome,
			Timestamp: h.Timestamp.Time,
		})
	}
	for _, e := range env.Status.ValidationErrors {
		state.ValidationErrors = append(state.ValidationErrors, model.Error{
			Code:        model.Code(e.Code),
			Resource:    e.Resource,
			Field:       e.Field,
			Message:     e.Message,
			Remediation: e.Remediation,
			DocsURL:     e.DocsURL,
			Line:        e.Line,
			Column:      e.Column,
		})
	}
	return state
}

// notDelivered is the not-found for an environment that has no custom resource.
//
// The remediation names the write that creates one, because the usual cause is
// ordering rather than a typo: a spec that has never been stored has nothing to
// report a phase, a history or a rollback target for.
func notDelivered(project, environment string) Error {
	return NotFound(environmentRef(project, environment),
		fmt.Sprintf("no Environment resource exists for %s/%s", project, environment),
		"store the spec with PutSpec (or `kubectl apply` the Environment) — an environment kelson has never "+
			"been given has nothing to report")
}

// environmentRef is the resource identity carried on errors, matching
// specRef's shape.
func environmentRef(project, environment string) string {
	return "environment/" + project + "/" + environment
}
