package rollback

import (
	"fmt"
	"strings"

	"github.com/dafrie/kelson/internal/diff"
)

// Cause is the machine-readable reason an irreversible change was flagged. An
// agent branches on Cause the way it branches on diff.Risk — it must never
// parse Message prose.
type Cause string

const (
	// CauseImmutableField is a field the API server will not let an update
	// change (a Deployment/StatefulSet selector, a Service clusterIP, a
	// StatefulSet volumeClaimTemplate). Reverting to the old value cannot
	// succeed as an update; the write is rejected before it persists.
	CauseImmutableField Cause = "immutable-field"
	// CauseDeletedPVC is a PVC the rollback recreates or deletes. Either way the
	// volume's data is gone: a recreated PVC binds to a fresh, empty volume and
	// a deleted PVC destroys its PersistentVolume.
	CauseDeletedPVC Cause = "pvc-data-lost"
	// CauseStatefulData is state owned by a data operator (e.g. a CloudNativePG
	// Cluster) or otherwise stateful. Rolling the manifest back does not roll
	// the data back.
	CauseStatefulData Cause = "stateful-data-not-rolled-back"
	// CauseMigrations is a preview-wide caveat, not a resource finding: kelson
	// runs no database migrations (issue #104), so it cannot verify that a
	// rollback undoes migration side effects.
	CauseMigrations Cause = "migrations-not-covered"
)

// Finding is one identified irreversibility: which resource is affected, at
// which field, why it cannot be safely reverted, and whether the prior state
// is unrecoverable (Never). It exists because diff.ResourceDiff carries a risk
// but no reason — the escalation lives in the diff, the reason lives here.
type Finding struct {
	// Resource is apiVersion/kind/name of the affected resource. Empty for
	// preview-wide findings such as the migrations caveat.
	Resource string `json:"resource,omitempty"`
	Kind     string `json:"kind,omitempty"`
	// Path is the field path for an immutable-field finding.
	Path    string `json:"path,omitempty"`
	Cause   Cause  `json:"cause"`
	Message string `json:"message"`
	// Never is true when the change cannot restore prior state at all (data is
	// gone); it renders the finding a hard blocker rather than a warning.
	Never bool `json:"never,omitempty"`
}

// MaxRisk returns the most severe Cause in the findings, for a machine reader
// that wants one answer without inspecting each Finding.
func MaxRisk(f []Finding) Cause {
	worst := Cause("")
	rank := map[Cause]int{
		CauseDeletedPVC:     4,
		CauseStatefulData:   3,
		CauseImmutableField: 2,
		CauseMigrations:     1,
	}
	for _, x := range f {
		if rank[x.Cause] > rank[worst] {
			worst = x.Cause
		}
	}
	return worst
}

// Preview analyses a rollback comparison diff and marks every change that
// cannot be safely reverted. d must be the diff whose before side is current
// state and whose after side is the manifests recorded for the target revision
// (issue #55).
//
// Preview annotates d in place: each affected resource's Risk is escalated to
// diff.RiskDisruptive and the Summary is recomputed, so an agent branching on
// diff.MaxRisk (or Summary.Disruptive) trips before execution. The returned
// findings carry the reasons — diff.ResourceDiff has no message field, which is
// why a small dedicated type is warranted — plus a preview-wide note that
// database migrations are not covered (#104).
func Preview(d *diff.Diff) (*diff.Diff, []Finding) {
	if d == nil {
		return nil, nil
	}
	findings := analyze(d)
	d.Summary = summarize(d.Resources)
	return d, findings
}

// analyze derives the irreversibility findings for a diff and escalates each
// affected resource's risk. It is pure over the diff except for the in-place
// risk escalation, which Preview is documented to perform.
func analyze(d *diff.Diff) []Finding {
	var out []Finding
	for i := range d.Resources {
		r := &d.Resources[i]
		irreversible := false
		if f, ok := statefulFinding(r); ok {
			out = append(out, f)
			irreversible = true
		}
		for _, f := range immutableFindings(r, d.Level) {
			out = append(out, f)
			irreversible = true
		}
		if irreversible {
			r.Risk = diff.RiskDisruptive
		}
	}
	if d.Level == diff.LevelServer {
		out = append(out, l2ImmutableFindings(d)...)
	}
	out = append(out, migrationsCaveat())
	return out
}

// summarize recomputes the blast-radius roll-up after resource risks have been
// escalated. internal/diff.summarize is not exported, and duplicating this
// small roll-up is cheaper and safer than threading an annotation back through
// the shared contract package.
func summarize(resources []diff.ResourceDiff) diff.Summary {
	var s diff.Summary
	s.MaxRisk = diff.RiskCosmetic
	for _, r := range resources {
		switch r.Op {
		case diff.OpAdded:
			s.Added++
		case diff.OpModified:
			s.Modified++
		case diff.OpRemoved:
			s.Removed++
		}
		switch r.Risk {
		case diff.RiskRestart:
			s.Restarting = append(s.Restarting, r.Name)
		case diff.RiskDisruptive:
			s.Disruptive = append(s.Disruptive, r.Name)
		}
		if riskRank(r.Risk) > riskRank(s.MaxRisk) {
			s.MaxRisk = r.Risk
		}
	}
	return s
}

func riskRank(r diff.Risk) int {
	switch r {
	case diff.RiskCosmetic:
		return 0
	case diff.RiskAdditive:
		return 1
	case diff.RiskRestart:
		return 2
	case diff.RiskDisruptive:
		return 3
	default:
		return 0
	}
}

func ref(r diff.ResourceDiff) string {
	if r.APIVersion == "" {
		return fmt.Sprintf("%s/%s", r.Kind, r.Name)
	}
	return r.APIVersion + "/" + r.Kind + "/" + r.Name
}

// immutablePresent reports whether a field path changes one of the immutable
// fields enumerated for its kind. A path matches when it equals the field or
// descends beneath it (spec.selector and spec.selector.matchLabels...).
func immutablePresent(prefixes []string, path string) bool {
	for _, p := range prefixes {
		if path == p || strings.HasPrefix(path, p+".") {
			return true
		}
	}
	return false
}
