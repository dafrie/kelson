// Package dryrun implements the L2 server-side dry-run preview (#43, #45): it
// submits every rendered resource with --dry-run=server and presents the API
// server's own verdict as a diff.Diff at Level=server.
//
// It shares the direct adapter's delivery-side machinery (a dynamic client, an
// unstructured coercion helper, kelson's field manager) rather than building a
// second client path, and it never influences rendering — it only ever
// consumes an already-rendered delivery.ManifestSet (#32).
package dryrun

import (
	"reflect"
	"strconv"
	"strings"

	"github.com/dafrie/kelson/internal/diff"
)

// fieldDiffs is the face of the whole package: given the live object, the
// object the API server says it would persist, and the object kelson submitted,
// return the field-level changes between live and would-be, each tagged with
// its origin (who authored the change) and its risk.

// diffLiveAndReturned computes the field diffs between `returned` (the object
// the server dry-run says it would persist) and `live` (current state). `sent`
// is the object kelson actually submitted, used to attribute each change to the
// user (OriginSpec) or to the API server on the way in (OriginAdmission /
// OriginDefaulting) — issue #43's whole readability story.
func diffLiveAndReturned(live, returned, sent map[string]any) []diff.FieldDiff {
	return diffs("", live, returned, sent)
}

// diffSetAndLive computes a rendered (L1-style) diff: desired `sent` versus
// current `live`, every change attributed to the spec. It is the content of the
// L1 fallback when L2 is unavailable (degraded preview, issue #43).
func diffSetAndLive(sent, live map[string]any) []diff.FieldDiff {
	return diffs("", live, sent, sent)
}

// diffs walks two value trees (omitting the first when it equals the second)
// and returns the leaf changes at dotted paths like
// spec.template.spec.containers[0].env[DATABASE_URL]. `s` is the corresponding
// subtree of the submitted object, used for origin attribution.
func diffs(p string, l, r, s any) []diff.FieldDiff {
	if equalAny(l, r) {
		return nil
	}
	lm, lok := l.(map[string]any)
	rm, rok := r.(map[string]any)
	ls, lsl := l.([]any)
	rs, rsl := r.([]any)

	switch {
	case lok && rok:
		sm, _ := s.(map[string]any)
		var out []diff.FieldDiff
		for _, k := range unionKeys(lm, rm) {
			out = append(out, diffs(path(p, k), lm[k], rm[k], sub(sm, k))...)
		}
		return out
	case lsl && rsl:
		ss, _ := s.([]any)
		return diffsSlices(p, ls, rs, ss)
	default:
		return []diff.FieldDiff{leaf(p, l, r, s)}
	}
}

// diffsSlices compares two lists by index. A member that exists on the right
// but not the left is a new member — the classic injected sidecar — and is
// reported as a single admission-origin change rather than a stream of fields.
func diffsSlices(p string, l, r, s []any) []diff.FieldDiff {
	var out []diff.FieldDiff
	n := len(l)
	if len(r) > n {
		n = len(r)
	}
	for i := 0; i < n; i++ {
		ip := p + "[" + strconv.Itoa(i) + "]"
		switch {
		case i < len(l) && i < len(r):
			out = append(out, diffs(ip, l[i], r[i], at(s, i))...)
		case i < len(r):
			// A brand-new member not in the live object: structurally added by
			// the API server or a mutating webhook (sidecar injection).
			out = append(out, diff.FieldDiff{
				Path:   ip,
				After:  r[i],
				Origin: diff.OriginAdmission,
				Risk:   riskField(ip),
			})
		default:
			// A member present live that the would-be object drops. In a
			// dry-run the server never removes by itself, so this is nearly
			// always a user edit; we still defensively attribute it.
			orig := diff.OriginSpec
			if at(s, i) == nil {
				orig = diff.OriginAdmission
			}
			out = append(out, diff.FieldDiff{Path: ip, Before: l[i], Origin: orig, Risk: riskField(ip)})
		}
	}
	return out
}

// leaf emits a single field-level change. Origin attribution (#43):
//
//   - The submitted object carries the same value that would persist → the user
//     authored the change: OriginSpec.
//   - The submitted object carries a value but the server normalized it on the
//     way in (e.g. imagePullPolicy coerced to Always) → OriginDefaulting.
//   - The submitted object never mentioned the field; the API server added or
//     changed it. A structural value (a whole map or list) is characteristic of
//     a mutating webhook (sidecar injection) → OriginAdmission; a scalar the
//     server fills in is defaulting → OriginDefaulting.
//
// The admission/defaulting split is heuristic: a real API server offers no
// per-field annotation saying which of several mutating webhooks (or the
// defaulting pass) produced a field. We use shape and the fact that defaults
// are overwhelmingly scalar normalizations, and we say so rather than pretend
// otherwise.
func leaf(p string, before, after, s any) diff.FieldDiff {
	return diff.FieldDiff{
		Path:   p,
		Before: before,
		After:  after,
		Origin: fieldOrigin(s, before, after),
		Risk:   riskField(p),
	}
}

func fieldOrigin(s, before, after any) diff.Origin {
	switch {
	case s != nil && equalAny(s, after):
		return diff.OriginSpec
	case s != nil:
		return diff.OriginDefaulting
	default:
		if isStructural(after) {
			return diff.OriginAdmission
		}
		return diff.OriginDefaulting
	}
}

// isStructural reports whether a value is a map or slice, the signature of a
// webhook adding structure rather than a scalar default.
func isStructural(v any) bool {
	_, m := v.(map[string]any)
	_, s := v.([]any)
	return m || s
}

// resourceRisk is the max risk across the resource's fields, lifted to the
// workload-restart level when appropriate. Disruptive is assigned separately by
// the caller when the API server rejected the resource (the write would fail).
func resourceRisk(op diff.Op, fields []diff.FieldDiff, kind string) diff.Risk {
	if op == diff.OpAdded {
		return diff.RiskAdditive
	}
	worst := diff.RiskCosmetic
	for _, f := range fields {
		if riskOrder(f.Risk) > riskOrder(worst) {
			worst = f.Risk
		}
	}
	if worst == diff.RiskCosmetic && fields != nil {
		// If anything at all changed and nothing was cosmetic, it is at least
		// additive (a behavior-relevant field).
		worst = diff.RiskAdditive
	}
	return worst
}

// riskField classifies a single field's risk. Pod-template changes on a
// workload are restart-required (pods roll); provenance/label metadata is
// cosmetic; everything else is additive.
func riskField(p string) diff.Risk {
	if metadataPath(p) {
		return diff.RiskCosmetic
	}
	if podTemplatePath(p) {
		return diff.RiskRestart
	}
	return diff.RiskAdditive
}

// metadataPath reports whether the field lives under metadata in a way that
// has no behavioral effect (annotations, labels, generation).
func metadataPath(p string) bool {
	return strings.HasPrefix(p, "metadata.")
}

// podTemplatePath reports whether a change to a workload's pod template will
// roll its pods. Deployments carry it under spec.template, CronJobs under
// spec.jobTemplate.
func podTemplatePath(p string) bool {
	return strings.HasPrefix(p, "spec.template.") ||
		strings.HasPrefix(p, "spec.jobTemplate.")
}

func riskOrder(r diff.Risk) int {
	switch r {
	case diff.RiskDisruptive:
		return 4
	case diff.RiskRestart:
		return 3
	case diff.RiskAdditive:
		return 2
	default:
		return 1
	}
}

// maxRisk returns the highest risk ordering two risks.
func maxRisk(a, b diff.Risk) diff.Risk {
	if riskOrder(a) >= riskOrder(b) {
		return a
	}
	return b
}

// --- helpers ----------------------------------------------------------------

func path(parent, key string) string {
	if parent == "" {
		return key
	}
	return parent + "." + key
}

func unionKeys(a, b map[string]any) []string {
	seen := map[string]bool{}
	var out []string
	for k := range a {
		seen[k] = true
		out = append(out, k)
	}
	for k := range b {
		if !seen[k] {
			out = append(out, k)
		}
	}
	return out
}

func sub(m map[string]any, k string) any {
	if m == nil {
		return nil
	}
	return m[k]
}

func at(s []any, i int) any {
	if s == nil || i >= len(s) {
		return nil
	}
	return s[i]
}

// equalAny compares two decoded values, treating integral numbers as equal even
// across Go numeric types (unstructured decoding yields int64, YAML decoding
// may yield int).
func equalAny(a, b any) bool {
	if a == nil || b == nil {
		return a == b
	}
	if reflect.DeepEqual(a, b) {
		return true
	}
	af, aok := toFloat(a)
	bf, bok := toFloat(b)
	if aok && bok && af == bf {
		return true
	}
	return false
}

func toFloat(v any) (float64, bool) {
	switch n := v.(type) {
	case int:
		return float64(n), true
	case int64:
		return float64(n), true
	case int32:
		return float64(n), true
	case float64:
		return n, true
	case float32:
		return float64(n), true
	default:
		return 0, false
	}
}
