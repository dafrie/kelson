package api

import (
	"context"
	"fmt"
	"time"

	kelsonv1alpha1 "github.com/dafrie/kelson/internal/api/gen/kelson/v1alpha1"
	"github.com/dafrie/kelson/internal/controlstore"
	"github.com/dafrie/kelson/internal/delivery"
	"github.com/dafrie/kelson/internal/delivery/statemachine"
)

// The delivery verbs read one thing: `Environment.status` (ADR-0027 decision 6,
// ADR-0028 decision 1 step 6). This file is the seam that reaches it and the
// projection that turns it into the wire's vocabulary.
//
// # Why the projection goes through statemachine.State
//
// The wire's Transition message says in as many words that it "mirrors
// statemachine.State", and issue #37's rule is that the engine is the single
// owner of what a phase means. The controller writes a phase from that same
// vocabulary (api/kelson/v1alpha1/phase_test.go asserts the two lists are
// identical), so the honest projection is to rebuild the State and let it
// classify itself — Answer() decides waiting/progressing/live/rejected/degraded
// and Err() decides what a failure is called. Re-deriving either here would be
// a second opinion that could disagree with the CLI's.
//
// # What the spine cannot say, and is therefore not said
//
// Three wire fields have no source on this spine and are left empty rather than
// filled with a plausible value:
//
//   - RollbackResponse.Preview.diff_json — a rollback preview compared two
//     recorded manifest sets, and the sets now live in the registry as
//     immutable artifacts this server does not fetch (ADR-0028 decision 4).
//     The preview reports the target and a finding that says so.
//   - RollbackResponse.Committed.as_revision — a rollback publishes nothing and
//     records no new history entry (internal/controller/history.go: "a rollback
//     does not prepend"), so there is no revision it "was recorded as".
//   - HistoryEntry.author — the spine records who deployed nothing; the audit
//     trail (ADR-0026) is where that question is answered.

// EnvironmentStore is the Environment-status seam.
// *controlstore.EnvironmentStore implements it. A nil one makes every verb that
// needs delivery state answer CodeUnimplemented naming the missing seam, which
// is how a partially-wired server (a test, a build with no cluster) fails
// honestly instead of inventing a phase.
type EnvironmentStore interface {
	Get(ctx context.Context, project, environment string) (controlstore.EnvironmentState, error)
	Watch(ctx context.Context, project, environment string) (<-chan controlstore.EnvironmentState, error)
	Annotate(ctx context.Context, project, environment string, annotations map[string]string) (controlstore.EnvironmentState, error)
}

// RevisionLister is the durable record the status mirror is a mirror of: the
// registry's own tag list (ADR-0028 decision 4, issue #241).
//
// It is spelled exactly as internal/controller's lister of the same name, over
// the same two questions, because the two planes have to answer them
// identically: a rollback this server accepted and the controller then refused
// — or the reverse — would be one record read twice and believed once.
// controller.RegistryRevisions implements both, and both binaries build it from
// the same `--registry` and `--registry-config`.
//
// A nil one is a server that can see only the bounded mirror: History stops at
// the window and a rollback to anything older is refused with the window named.
// That is the pre-#241 posture exactly, and it stays correct for an instance
// that holds no registry credential — what it must never become is a server
// that reports a revision it never looked for as one that does not exist.
type RevisionLister interface {
	// Revisions lists every revision the registry holds for one environment,
	// newest first. An environment that has published nothing is an empty list
	// and no error.
	Revisions(ctx context.Context, project, environment string) ([]string, error)

	// Resolve reports whether one revision is in the registry and which bytes
	// it names. found=false with a nil error is "there is no such revision" —
	// an answer, and a different one from "kelson could not look".
	Resolve(ctx context.Context, project, environment, revision string) (digest string, found bool, err error)
}

// maxHistoryEntries is the bound on `Environment.status.history[]` (ADR-0028
// decision 4). It is spelled here for the reason the annotations below are —
// this plane holds no Kubernetes types — and asserted against the custom
// resource's own constant in environment_test.go.
//
// The server reads it for one decision: a mirror holding that many entries is
// one that has probably dropped something, which is what makes a failed
// registry query worth refusing over rather than degrading past (History).
const maxHistoryEntries = 20

// The annotations ADR-0028 defines on an Environment. They are spelled here
// rather than imported from api/kelson/v1alpha1 for the reason the phase
// constants are: this plane holds no Kubernetes types, and the constants are
// asserted against the custom resource's own in environment_test.go.
const (
	// annotationRollbackTo pins an Environment to a revision it has already
	// published (ADR-0028 decision 5).
	annotationRollbackTo = "kelson.dev/rollback-to"
	// annotationPromotedFrom records where a promotion's images came from
	// (ADR-0028 decision 6), as `<source-environment>@<revision>`.
	annotationPromotedFrom = "kelson.dev/promoted-from"
)

// reasonRollbackTargetUnknown is the controller's Ready reason for a rollback
// target it will not honour: the tag is not in `status.history`, so kelson
// cannot confirm it published it (internal/controller's verifyRollbackTarget).
//
// It is spelled here for the same reason the annotations above are, and it is
// asserted against the custom resource's own constant in environment_test.go.
// Rollback needs it because a refusal is the one controller answer that leaves
// `status.rollbackRevision` empty: the refusal happens before the bookkeeping,
// so a watcher that only looked at that field would wait out its whole budget
// for a rollback the controller has already declined (followRollback).
const reasonRollbackTargetUnknown = "RollbackTargetUnknown"

// adapterName is what Committed.adapter carries. There is one reconciliation
// path now (ADR-0028 decision 1), so it is a constant rather than a selection —
// and it is still reported, because a client that renders "deployed via …"
// should not have to special-case an empty string.
const adapterName = "flux"

// environments returns the status seam or the refusal that names it.
func (s *Server) environmentStore() (EnvironmentStore, error) {
	if s.environments == nil {
		return nil, unimplemented("the Environment status reader")
	}
	return s.environments, nil
}

// deliveryState rebuilds the state-machine's view of one revision from the
// status the controller wrote.
//
// Stuck is not read from the status because the controller does not record it:
// it is this server's own verdict that a stream's budget expired with nothing
// terminal, so it is passed in.
func deliveryState(st controlstore.EnvironmentState, stuck bool) statemachine.State {
	state := statemachine.State{
		Target: statemachine.Target{
			Project:     st.Project,
			Environment: st.Environment,
			Revision:    st.Revision,
		},
		Phase:            delivery.Phase(st.Phase),
		Stuck:            stuck,
		ObservedRevision: st.Revision,
	}
	if ready, ok := st.Ready(); ok {
		state.Since = ready.Since
		state.UpdatedAt = ready.Since
		if !ready.True() {
			// A false Ready is the controller's own account of what went
			// wrong, in its own reason vocabulary (api/kelson/v1alpha1's
			// Reason* constants). Relaying it verbatim is what makes the
			// stream actionable — the alternative is translating a reason
			// nobody can look up into one this package invented.
			state.Cause = statemachine.Cause{
				Component: "kelson",
				Reason:    ready.Reason,
				Message:   ready.Message,
			}
		}
	}
	if state.Cause.IsZero() {
		if progressing, ok := st.Progressing(); ok && progressing.Message != "" {
			state.Cause = statemachine.Cause{
				Component: "kelson",
				Reason:    progressing.Reason,
				Message:   progressing.Message,
			}
		}
	}
	return state
}

// wireTransition projects a state onto the Transition message.
func wireTransition(state statemachine.State) *kelsonv1alpha1.DeployResponse_Transition {
	t := &kelsonv1alpha1.DeployResponse_Transition{
		Phase:            string(state.Phase),
		Answer:           string(state.Answer()),
		Stuck:            state.Stuck,
		ObservedRevision: state.ObservedRevision,
	}
	if !state.Cause.IsZero() {
		t.Cause = &kelsonv1alpha1.DeployResponse_Cause{
			Component: state.Cause.Component,
			Reason:    state.Cause.Reason,
			Message:   state.Cause.Message,
		}
	}
	if !state.Since.IsZero() {
		t.SinceUnixMs = state.Since.UnixMilli()
	}
	return t
}

// sameTransition reports whether two projections say the same thing, so a
// status write that changed nothing a client can see does not become an event.
// A watch delivers an event for every write to the object, including the ones
// that only bumped an observedGeneration.
func sameTransition(a, b *kelsonv1alpha1.DeployResponse_Transition) bool {
	if a == nil || b == nil {
		return a == b
	}
	return a.GetPhase() == b.GetPhase() &&
		a.GetAnswer() == b.GetAnswer() &&
		a.GetStuck() == b.GetStuck() &&
		a.GetObservedRevision() == b.GetObservedRevision() &&
		a.GetCause().GetReason() == b.GetCause().GetReason() &&
		a.GetCause().GetMessage() == b.GetCause().GetMessage()
}

// settledError is the structured error a settled deployment carries, or nil
// when it settled well.
//
// The order is the order of specificity. A spec the controller refused carries
// the model's own errors with their slash codes, and those are the answer — an
// author whose `$.spec.components[0].port` is wrong must read that and not
// "delivery/apply-failed". Below them the state machine classifies its own
// failures. Below that, a Ready=False the state machine has no phase for
// (`RegistryNotConfigured`, `FluxNotInstalled`, `ProjectNotFound`) is a
// delivery failure named by the controller's reason.
func settledError(st controlstore.EnvironmentState, state statemachine.State) error {
	if len(st.ValidationErrors) > 0 {
		return st.ValidationErrors
	}
	if err := state.Err(); err != nil {
		return err
	}
	ready, ok := st.Ready()
	if !ok || ready.True() {
		return nil
	}
	return delivery.Error{
		Code:        delivery.ErrApplyFailed,
		Resource:    st.Project + "/" + st.Environment,
		Message:     ready.Message,
		Remediation: "read the Environment's conditions for the whole account: `kubectl describe environment " + st.Environment + "`",
		DocsURL:     "https://kelson.dev/delivery/errors/" + string(delivery.ErrApplyFailed),
		Cause:       ready.Reason,
	}
}

// wireError projects one error onto the single structured Error a Settled event
// carries. A list of findings collapses to its first entry — the message says
// how many there were, because the wire has one slot and dropping the count
// would make a spec with nine errors look like a spec with one.
func wireError(err error) *kelsonv1alpha1.Error {
	errs := wireErrors(err)
	if len(errs) == 0 {
		if err == nil {
			return nil
		}
		return &kelsonv1alpha1.Error{Code: string(delivery.ErrApplyFailed), Message: err.Error()}
	}
	first := errs[0]
	if len(errs) > 1 {
		first.Message = fmt.Sprintf("%s (and %d more)", first.Message, len(errs)-1)
	}
	return first
}

// deployBudget is how long a stream follows an environment before calling it
// stuck: the request's timeout, or the server's default.
func (s *Server) deployBudget(seconds int64) time.Duration {
	if seconds > 0 {
		return time.Duration(seconds) * time.Second
	}
	return s.deployTimeout
}
