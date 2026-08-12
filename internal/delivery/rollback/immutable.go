package rollback

import (
	"strings"

	"github.com/dafrie/kelson/internal/diff"
)

// immutableFields is the L1 (offline) static list of fields the Kubernetes API
// server will not let an update change. It mirrors the immutable-field
// knowledge already in internal/diff's field-level risk (issue #42) so the two
// agree on what "cannot be reverted as an update" means, extended with the
// identity fields an update can never touch.
//
// This list powers the no-cluster path only. When an L2 dry-run verdict is
// available the analyzer trusts the API server's own field-is-immutable
// rejection (#43) and does not consult this table, because the table is a
// heuristic that can both under- and over-approximate what a real cluster
// rejects: it knows only the fields kelson has enumerated.
var immutableFields = map[string][]string{
	"Deployment":  {"spec.selector"},
	"StatefulSet": {"spec.selector", "spec.volumeClaimTemplates"},
	"Service":     {"spec.clusterIP", "spec.type", "spec.selector"},
	"Job":         {"spec.selector", "spec.manualSelector"},
}

// immutableFindings reports the immutable-field changes on one resource.
//
// On the rendered (L1) path it walks the resource's field diffs against the
// static list. On the server (L2) path it returns nothing here — the API
// server's rejections are collected from Violations by l2ImmutableFindings,
// and the L1 list must not fire alongside the authoritative verdict (issue #43:
// "the API server's dry-run verdict" is the true signal).
func immutableFindings(r *diff.ResourceDiff, level diff.Level) []Finding {
	if level == diff.LevelServer {
		return nil
	}
	prefixes := immutableFields[r.Kind]
	if len(prefixes) == 0 {
		return nil
	}
	var out []Finding
	for _, f := range r.Fields {
		if !immutablePresent(prefixes, f.Path) {
			continue
		}
		out = append(out, Finding{
			Resource: ref(*r),
			Kind:     r.Kind,
			Path:     f.Path,
			Cause:    CauseImmutableField,
			Message:  "an update cannot change " + f.Path + " on a " + r.Kind + "; the API server would reject the rollback before it persists",
			Never:    true,
		})
	}
	return out
}

// l2ImmutableFindings maps the API server's own dry-run rejections onto
// findings. It reuses internal/delivery/dryrun's signal (a field-is-immutable
// or invalid-field enforcement violation produced when a dry-run apply is
// rejected) rather than reimplementing immutable-field knowledge (issue #55).
func l2ImmutableFindings(d *diff.Diff) []Finding {
	// De-duplicate by (policy,resource,path): a rejection may name several
	// causes for one resource.
	seen := map[string]bool{}
	var out []Finding
	for _, v := range d.Violations {
		if v.Enforcement != diff.EnforcementEnforce {
			continue
		}
		if v.Policy != "field-is-immutable" && v.Policy != "invalid-field" {
			continue
		}
		key := v.Policy + "|" + v.Resource + "|" + v.Path
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, Finding{
			Resource: v.Resource,
			Kind:     kindOf(v.Resource),
			Path:     v.Path,
			Cause:    CauseImmutableField,
			Message:  "the API server would reject the rollback: " + v.Message,
			Never:    true,
		})
	}
	return out
}

// kindOf extracts the Kind from the resource string dryrun emits — its
// ResourceRef.String() renders "Kind/name" — so the finding carries the kind
// without parsing (and never inventing) an apiVersion.
func kindOf(resource string) string {
	if i := strings.IndexByte(resource, '/'); i >= 0 {
		return resource[:i]
	}
	return resource
}
