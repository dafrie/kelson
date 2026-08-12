// Package rollback is the DELIVERY-plane home of issue #55's second half:
// identifying, in a preview and before execution, every change a rollback
// cannot safely revert.
//
// Issue #38 and #42-45 already shipped the machinery this package builds on:
// adapters can Rollback to any retained revision, and internal/diff produces
// the machine-readable *diff.Diff that a preview is expected to return. What
// was missing — and what this package provides — is the honesty the issue's
// design note demands: "if a rollback cannot fully restore prior state, say so
// before doing it."
//
// The irreversibility question is answered by Preview, which analyses a
// rollback comparison diff (before = current state, after = the recorded
// manifests of the target revision), escalates each un-revertable resource to
// diff.RiskDisruptive so an agent branching on Summary.MaxRisk trips before
// execution, and returns the findings that say why.
//
// # What Preview does not do
//
// Preview does not render, and does not contact the cluster. It consumes the
// comparison diff and, on the L2 path, the API server's own dry-run verdict —
// both of which arrive already computed (internal/diff for L1, internal/delivery/dryrun
// for the L2 signal). It never re-renders the target revision: the whole point
// of rollback (#38) is to replay recorded bytes, not to re-render.
//
// # What is and is not detected
//
//   - Immutable fields: precise on the L2 path — the API server's own rejection
//     (internal/delivery/dryrun, a field-is-immutable violation) is trusted over
//     any local knowledge. Offline (L1) it falls back to a static list of the
//     fields the API server will not let an update change. The static list is
//     inherently a heuristic: it can only know the immutable fields kelson has
//     enumerated, so L1 may both miss a field the server would reject and flag
//     one the server would accept.
//   - Deleted PVCs and data-operator-owned state: stateful, reported as such —
//     restoring the manifest never restores the data behind it.
//   - Database migrations: kelson does not run migrations yet (issue #104), so
//     Preview cannot detect them and says so rather than implying the rollback
//     is fully safe.
//
// # Source seam
//
// Source abstracts where the two manifest sides come from — the direct-mode
// rendered-history store or a Git deployment repository. DirectSource reads the
// direct store; the Git modes adapt their writer to Source. The comparison diff
// itself is produced upstream and handed to Preview; wiring a Source into
// diff.Between is the one piece left to the delivery layer (see the package
// report), because internal/diff's only L1 entry consumes []renderer.Manifest,
// which recorded history bytes cannot be turned into without a re-render.
package rollback
