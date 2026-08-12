package rollback

import (
	"strings"

	"github.com/dafrie/kelson/internal/diff"
)

// statefulFinding reports a change the rollback makes to state-holding
// resources that a manifest rollback cannot restore, and whether the finding is
// unrecoverable (Never).
//
// The precise cases are:
//
//   - A PersistentVolumeClaim the rollback recreates (present in the target
//     revision, absent now) binds to a fresh, empty volume — the data it used
//     to hold is gone. Equally, a PVC the rollback deletes (present now, absent
//     in the target) destroys the claim and its PersistentVolume. Both are
//     unrecoverable.
//   - A resource owned by a data operator (a CloudNativePG Postgres Cluster)
//     holds data outside the manifest. Reverting the manifest leaves the data
//     as it is: the rollback does not roll the database back. This is reported
//     as stateful-not-restored, and is a warning rather than a hard Never — for
//     a modified working resource it may be exactly what the operator wants —
//     but it must never be implied safe.
//
// Everything here is identified from the resource's Kind (and apiVersion for
// the operator-owned case), which is why the analyzer needs no cluster: it is
// classification by resource shape, not a live-state read.
func statefulFinding(r *diff.ResourceDiff) (Finding, bool) {
	switch {
	case r.Kind == "PersistentVolumeClaim" && r.Op == diff.OpAdded:
		return Finding{
			Resource: ref(*r),
			Kind:     r.Kind,
			Cause:    CauseDeletedPVC,
			Message:  "this rollback recreates a PVC that is gone; it binds to a fresh empty volume and the data it held is lost",
			Never:    true,
		}, true
	case r.Kind == "PersistentVolumeClaim" && r.Op == diff.OpRemoved:
		return Finding{
			Resource: ref(*r),
			Kind:     r.Kind,
			Cause:    CauseDeletedPVC,
			Message:  "this rollback deletes a PVC that exists now; the claim and its PersistentVolume data are destroyed",
			Never:    true,
		}, true
	case isDataOperator(r) && r.Op != diff.OpAdded:
		return Finding{
			Resource: ref(*r),
			Kind:     r.Kind,
			Cause:    CauseStatefulData,
			Message:  "the manifest rollback does not roll back the data held by this " + r.Kind + "; the operator's state is unchanged",
		}, true
	case isDataOperator(r):
		return Finding{
			Resource: ref(*r),
			Kind:     r.Kind,
			Cause:    CauseStatefulData,
			Message:  "this rollback (re)creates a " + r.Kind + "; it will provision new, empty state rather than restoring the data it replaced",
		}, true
	}
	return Finding{}, false
}

// isDataOperator reports whether a resource is owned by a data operator whose
// state lives outside the manifest. Detection is by apiVersion/kind shape and
// is deliberately conservative: only operators kelson has enumerated are
// recognised, so an unrecognised data CRD slips through unflagged. Recognising
// the schema of every operator is out of scope; naming the ones it knows is
// the honest version of the claim.
func isDataOperator(r *diff.ResourceDiff) bool {
	// CloudNativePG runs the Postgres data plane; its Cluster CRD is the
	// declared owner of the physical database.
	if r.Kind == "Cluster" && strings.Contains(r.APIVersion, "cnpg.io") {
		return true
	}
	// A StatefulSet is state-bearing but the operator-owned-data rule above is
	// the sharper test; PVCs are handled directly. KubeDB and other operators
	// are recognised by group name as they are enumerated.
	return false
}

// migrationsCaveat is the preview-wide honesty finding: kelson runs no
// database migrations (issue #104), so a rollback can undo application code but
// the preview cannot vouch for migration side effects. It is deliberately a
// caveat on every rollback, not a resource finding — implying a rollback is
// fully safe when migrations are not covered is exactly the dishonesty the
// issue's design note forbids.
func migrationsCaveat() Finding {
	return Finding{
		Cause:   CauseMigrations,
		Message: "database migrations are not covered: kelson does not run migrations (issue #104), so this rollback does not and cannot undo migration side effects",
	}
}
