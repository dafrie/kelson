package uninstall

import (
	"context"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/dafrie/kelson/internal/delivery"
)

// Outcome is what happened to one target.
type Outcome string

const (
	// OutcomeDeleted: kelson deleted it.
	OutcomeDeleted Outcome = "deleted"
	// OutcomeGone: it was already absent. An uninstall run twice reports this
	// for everything and succeeds — "nothing to do" is a success.
	OutcomeGone Outcome = "gone"
	// OutcomeLeft: kelson refused to delete it. Its live labels no longer put
	// it in the scope, or it was replaced between the plan and the delete. It
	// is a bystander now, and bystanders are reported, never deleted.
	OutcomeLeft Outcome = "left"
	// OutcomeFailed: the API server refused the delete.
	OutcomeFailed Outcome = "failed"
)

// Result is one target's outcome.
type Result struct {
	Ref     Ref
	Outcome Outcome
	// Detail explains an outcome that is not a plain delete.
	Detail string
}

// Report is what an execution did, one line per target plus the counts.
type Report struct {
	Results []Result
	Deleted int
	Gone    int
	Left    int
	Failed  int
}

// Bystanders returns the resources the run deliberately left alone.
func (r *Report) Bystanders() []Result {
	var out []Result
	for _, res := range r.Results {
		if res.Outcome == OutcomeLeft {
			out = append(out, res)
		}
	}
	return out
}

// Execute deletes the plan's targets in order and reports what happened to each
// one.
//
// Three rules, and all three are the same rule seen from different angles:
//
//   - Every target is re-read immediately before its delete and re-checked
//     against its LIVE labels. A plan is a snapshot; the cluster is not. A
//     resource that stopped being kelson's between the two is left alone and
//     reported — the same refusal internal/secret makes when asked to delete a
//     Secret it did not write.
//   - Every delete carries a UID precondition, so a resource deleted and
//     re-created between the plan and the delete is refused rather than deleted
//     on the strength of a check against the object it replaced.
//   - A failed delete does not abandon the run. One kind the credentials may
//     not delete must not strand everything after it; the failures are
//     collected, reported per object, and returned as one structured error at
//     the end so the exit code is honest.
//
// The Namespace gets one check more than everything else, because it is the one
// delete that reaches resources this scope never selected: its live occupants
// are read back too, and another kelson deployment living there refuses the
// delete however the plan read it (issue #215).
func (u *Uninstaller) Execute(ctx context.Context, plan *Plan) (*Report, error) {
	if plan == nil {
		return nil, delivery.ApplyFailed("(plan)", "",
			"no plan to execute",
			"call Plan() first; an uninstall never deletes anything it did not preview")
	}
	report := &Report{}
	var failures []string

	for _, t := range plan.Targets {
		res := u.deleteTarget(ctx, plan.Scope, t)
		report.Results = append(report.Results, res)
		switch res.Outcome {
		case OutcomeDeleted:
			report.Deleted++
		case OutcomeGone:
			report.Gone++
		case OutcomeLeft:
			report.Left++
		case OutcomeFailed:
			report.Failed++
			failures = append(failures, t.Ref.String()+": "+res.Detail)
		}
	}

	if len(failures) > 0 {
		return report, delivery.ApplyFailed(strings.Join(refsOf(report), ", "), "",
			"the API server refused to delete: "+strings.Join(failures, "; "),
			"check RBAC for these kinds, remove them by hand, or re-run the uninstall — it is idempotent, "+
				"and what it already deleted reports as gone the second time")
	}
	return report, nil
}

// deleteTarget performs the read-check-delete for one target.
func (u *Uninstaller) deleteTarget(ctx context.Context, scope Scope, t Target) Result {
	client := u.client.Resource(t.GVR).Namespace(t.Ref.Namespace)

	live, err := client.Get(ctx, t.Ref.Name, metav1.GetOptions{})
	switch {
	case apierrors.IsNotFound(err):
		return Result{Ref: t.Ref, Outcome: OutcomeGone, Detail: "already absent"}
	case err != nil:
		return Result{Ref: t.Ref, Outcome: OutcomeFailed, Detail: "could not be read before deleting: " + err.Error()}
	}

	if !scope.owns(live.GetLabels()) {
		return Result{Ref: t.Ref, Outcome: OutcomeLeft,
			Detail: "it no longer carries " + scope.Selector() + ", so kelson does not claim it"}
	}
	if t.Tier == TierNamespace {
		if live.GetAnnotations()[delivery.AnnNamespaceOwnership] != delivery.NamespaceOwnershipCreated {
			return Result{Ref: t.Ref, Outcome: OutcomeLeft,
				Detail: "the namespace no longer records that kelson created it, and deleting one takes everything inside it"}
		}
		// Who created the namespace is a fact about the past; who is living in
		// it is a fact about now, and the plan's answer to it is as much a
		// snapshot as everything else here. The window between the preview and
		// this delete is exactly when another project's first apply lands, so
		// the one deletion that cascades gets the question asked twice (issue
		// #215).
		if tenants := u.namespaceTenants(ctx, scope, t.Ref.Name); !tenants.clear() {
			return Result{Ref: t.Ref, Outcome: OutcomeLeft, Detail: tenants.refusal()}
		}
	}

	opts := metav1.DeleteOptions{}
	if uid := live.GetUID(); uid != "" {
		opts.Preconditions = &metav1.Preconditions{UID: &uid}
	}
	switch err := client.Delete(ctx, t.Ref.Name, opts); {
	case err == nil:
		return Result{Ref: t.Ref, Outcome: OutcomeDeleted}
	case apierrors.IsNotFound(err):
		return Result{Ref: t.Ref, Outcome: OutcomeGone, Detail: "deleted by something else first"}
	case apierrors.IsConflict(err):
		return Result{Ref: t.Ref, Outcome: OutcomeLeft,
			Detail: "it was replaced between the preview and the delete, so this is a different object now"}
	default:
		return Result{Ref: t.Ref, Outcome: OutcomeFailed, Detail: err.Error()}
	}
}

// refsOf names the failed targets for the aggregate error's resource field.
func refsOf(r *Report) []string {
	var out []string
	for _, res := range r.Results {
		if res.Outcome == OutcomeFailed {
			out = append(out, res.Ref.String())
		}
	}
	return out
}
