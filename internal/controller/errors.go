package controller

import (
	"time"

	"github.com/dafrie/kelson/api/kelson/v1alpha1"
)

// DeliveryError is everything steps 4 to 6 can refuse with: a reason from the
// closed set in api/kelson/v1alpha1, the sentence a human reads, what to do
// about it, and the underlying cause.
//
// # Why the taxonomy is closed, and why it decides the requeue
//
// A reconciler has exactly three ways to answer a failure, and getting the
// choice wrong is the difference between a controller that converges and one
// that burns a cluster's API budget:
//
//   - **return the error** — controller-runtime requeues with exponential
//     backoff. Right for a transient failure of something outside the cluster:
//     a registry that did not answer.
//   - **requeue after a fixed delay, returning nil** — right for a failure that
//     will be fixed by an operator doing something elsewhere: installing Flux,
//     granting RBAC, fixing a credential. Backoff is wrong here because the
//     wait is human-scale and unbounded, and an error return would log a stack
//     of failures for a situation nobody has looked at yet.
//   - **status only, no requeue** — right for a failure that cannot fix itself
//     and cannot be fixed by waiting: an invalid reference, a name conflict, a
//     rollback target that does not exist. Something has to change, and every
//     change that could fix it is a watch event.
//
// Which of the three applies is a property of the reason and not of the call
// site, so it is a table here rather than a judgement at each `return`. That is
// also what makes the set closed: a reason with no row does not compile into a
// behaviour, it panics in a test.
//
// FluxNotInstalled deserves its own sentence, because it is the reason the
// table exists. A cluster with no Flux is the *expected* state of a fresh
// install (ADR-0030), it is fixed by `kelson install`, and a controller that
// crash-looped on it would make "kelson is broken" the first impression of a
// product whose whole install story is "offers, never assumes". So it requeues
// on a five-minute timer and returns no error at all.
type DeliveryError struct {
	// Reason is one of the v1alpha1 delivery reasons. It is written verbatim
	// into the Ready condition, so it is what a `kubectl get -o jsonpath` or an
	// agent branches on.
	Reason string
	// Message says what happened, in the terms the reader is standing in.
	Message string
	// Retry is how long to wait before trying again, zero for "do not requeue".
	// It is filled from the table by [newDeliveryError] rather than by callers.
	Retry time.Duration
	// Backoff asks controller-runtime for its exponential backoff instead of a
	// fixed delay, by returning the error from Reconcile.
	Backoff bool
	// Err is the underlying cause, for errors.Is/As and for the log.
	Err error
}

func (e *DeliveryError) Error() string {
	if e.Err != nil {
		return e.Reason + ": " + e.Message + ": " + e.Err.Error()
	}
	return e.Reason + ": " + e.Message
}

func (e *DeliveryError) Unwrap() error { return e.Err }

// operatorRetry is how long a failure waits on a human. Five minutes is long
// enough that a controller watching a hundred environments through a missing
// Flux does not generate meaningful load, and short enough that an operator who
// has just fixed the problem sees it converge while they are still watching.
const operatorRetry = 5 * time.Minute

// deliveryPolicy is the table above, as code. Every reason in
// v1alpha1's delivery set has a row, and errors_test.go asserts that.
var deliveryPolicy = map[string]struct {
	retry   time.Duration
	backoff bool
}{
	// Waiting on an operator: a fixed timer, no error, no crash loop.
	v1alpha1.ReasonFluxNotInstalled:     {retry: operatorRetry},
	v1alpha1.ReasonPushDenied:           {retry: operatorRetry},
	v1alpha1.ReasonFluxApplyForbidden:   {retry: operatorRetry},
	v1alpha1.ReasonFieldManagerConflict: {retry: operatorRetry},

	// Nothing will change on its own, and everything that could change is a
	// watch event: say so in the status and stop.
	v1alpha1.ReasonRegistryNotConfigured: {},
	v1alpha1.ReasonArtifactRefInvalid:    {},
	v1alpha1.ReasonNameConflict:          {},
	v1alpha1.ReasonRollbackTargetUnknown: {},

	// Transient I/O against something outside the cluster: the one case where
	// exponential backoff is exactly right.
	v1alpha1.ReasonRegistryUnreachable: {backoff: true},
}

// newDeliveryError builds a refusal, taking its retry behaviour from the table
// so no call site has to decide it. An unknown reason is a programming error
// and is given the safest behaviour — status only — rather than being allowed
// to invent a fourth answer.
func newDeliveryError(reason, message string, err error) *DeliveryError {
	policy := deliveryPolicy[reason]
	return &DeliveryError{
		Reason:  reason,
		Message: message,
		Retry:   policy.retry,
		Backoff: policy.backoff,
		Err:     err,
	}
}

// asDeliveryError finds a *DeliveryError in a chain, filling in the retry
// policy for one that was constructed as a literal (ArtifactRef builds its own,
// because it is called from outside the reconcile and has no error to wrap).
func asDeliveryError(err error) (*DeliveryError, bool) {
	for err != nil {
		if de, ok := err.(*DeliveryError); ok { //nolint:errorlint // the chain is walked below
			policy := deliveryPolicy[de.Reason]
			if de.Retry == 0 && !de.Backoff {
				de.Retry, de.Backoff = policy.retry, policy.backoff
			}
			return de, true
		}
		u, ok := err.(interface{ Unwrap() error }) //nolint:errorlint // ditto
		if !ok {
			return nil, false
		}
		err = u.Unwrap()
	}
	return nil, false
}
