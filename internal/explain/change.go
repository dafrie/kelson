package explain

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/dafrie/kelson/internal/delivery"
)

// The provenance half of an explanation: what the most recent revision changed
// (issue #77, ADR-0023).
//
// The correlation is made from the *recorded rendered manifests* of two
// revisions, never from the current spec. A spec has moved on since the deploy
// that broke the environment; the recorded bytes are what was applied, which is
// the only thing a claim like "revision rev-00000043 removed DATABASE_URL" can
// honestly be built from (#38 defines the recorded set as the artifact).
//
// It degrades in exactly one direction. No history, no manifest seam, one
// revision only, a pruned predecessor: each of those is a Note and an
// Explanation without a change correlation — never a missing explanation, and
// never a correlation asserted without the bytes behind it.

// MaxChanges bounds how many env and image changes one Change carries. The
// list is evidence for a diagnosis, not a changelog: a revision that changed
// forty variables is one whose diff a reader should go and read.
const MaxChanges = 12

// recentChange resolves the live and previous revisions' workloads and the
// change between them. The workloads are returned alongside because every other
// cause reads them too — the probe paths, the images, the env-var correlation —
// and a second fetch of the same revision would be a second cluster round trip
// for bytes already in hand.
func (e *Explanation) recentChange(ctx context.Context, in Input) (live, previous []workload, change *Change) {
	if len(in.History) == 0 {
		e.note("no recorded revisions for %s/%s: nothing has been deployed through kelson here, so no change can be correlated",
			in.Project, in.Environment)
		return nil, nil, nil
	}
	head := revisionRef(in.History[0])
	change = &Change{Revision: head}

	if in.Manifests == nil {
		change.Summary = "the recorded manifests of a revision are not readable here, so this change is named but not described"
		e.note("recorded manifests are unavailable: causes cannot be correlated with a field-level change")
		return nil, nil, change
	}

	live, ok := e.manifests(ctx, in, head.Revision)
	if !ok {
		change.Summary = "the recorded manifests of " + head.Revision + " could not be read"
		return nil, nil, change
	}
	if len(in.History) < 2 {
		change.Summary = head.Revision + " is the first recorded revision of this environment: there is nothing to compare it against"
		return live, nil, change
	}

	prevRef := revisionRef(in.History[1])
	change.Previous = &prevRef
	previous, ok = e.manifests(ctx, in, prevRef.Revision)
	if !ok {
		change.Summary = fmt.Sprintf("%s is the live revision; %s was not retained, so what changed cannot be read",
			head.Revision, prevRef.Revision)
		return live, nil, change
	}

	change.Env = diffEnv(previous, live)
	change.Images = diffImages(previous, live)
	change.Summary = changeSummary(head, prevRef, change)
	return live, previous, change
}

// manifests reads one revision's recorded manifests, turning a failure into a
// Note. A pruned revision is the ordinary reason this fails and it is not an
// error: retention is a policy, and an explanation that dies because the
// history window moved would be useless exactly when history is deep.
func (e *Explanation) manifests(ctx context.Context, in Input, revision string) ([]workload, bool) {
	set, err := in.Manifests(ctx, revision)
	if err != nil {
		e.note("the recorded manifests of %s could not be read (%s), so no change correlation was made for it", revision, err)
		return nil, false
	}
	return readWorkloads(set), true
}

func revisionRef(entry delivery.Entry) RevisionRef {
	return RevisionRef{
		Revision:    entry.Revision,
		CommittedAt: entry.CommittedAt,
		Message:     entry.Message,
		Author:      entry.Author,
	}
}

// changeSummary is the one line a reader gets before the lists.
func changeSummary(head, previous RevisionRef, change *Change) string {
	if len(change.Env) == 0 && len(change.Images) == 0 {
		return fmt.Sprintf("%s → %s changed no container image and no environment variable", previous.Revision, head.Revision)
	}
	parts := make([]string, 0, 2)
	if n := len(change.Images); n > 0 {
		parts = append(parts, fmt.Sprintf("%d image %s", n, plural(n, "change", "changes")))
	}
	if n := len(change.Env); n > 0 {
		parts = append(parts, fmt.Sprintf("%d environment-variable %s", n, plural(n, "change", "changes")))
	}
	return fmt.Sprintf("%s → %s: %s", previous.Revision, head.Revision, strings.Join(parts, ", "))
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

// diffEnv compares the environment of every container that exists in either
// revision, per workload and per container.
//
// Matching is by name on both levels, which is what makes a *renamed* variable
// read as a removal plus an addition rather than as a modification. That is the
// honest reading: a container asking for DATABASE_URL does not care that
// DB_URL appeared, and the two entries side by side are what makes the rename
// legible to whoever is looking.
func diffEnv(previous, live []workload) []EnvChange {
	var out []EnvChange
	for _, pair := range matchContainers(previous, live) {
		before := envByName(pair.before.Env)
		after := envByName(pair.after.Env)
		for _, name := range unionKeys(before, after) {
			b, hadBefore := before[name]
			a, hasAfter := after[name]
			switch {
			case hadBefore && !hasAfter:
				out = append(out, EnvChange{
					Workload: pair.workload, Container: pair.container,
					Name: name, Kind: ChangeRemoved, Before: b.form(),
				})
			case !hadBefore && hasAfter:
				out = append(out, EnvChange{
					Workload: pair.workload, Container: pair.container,
					Name: name, Kind: ChangeAdded, After: a.form(),
				})
			case b.form() != a.form():
				out = append(out, EnvChange{
					Workload: pair.workload, Container: pair.container,
					Name: name, Kind: ChangeModified, Before: b.form(), After: a.form(),
				})
			}
		}
	}
	return out
}

// diffImages compares the image of every container that exists in both
// revisions. A container that only exists on one side is a structural change
// the env diff already reports variable by variable; reporting its image as a
// change too would say the same thing twice.
func diffImages(previous, live []workload) []ImageChange {
	var out []ImageChange
	for _, pair := range matchContainers(previous, live) {
		if pair.before.Name == "" || pair.after.Name == "" {
			continue
		}
		if pair.before.Image == pair.after.Image {
			continue
		}
		out = append(out, ImageChange{
			Workload: pair.workload, Container: pair.container,
			Before: pair.before.Image, After: pair.after.Image,
		})
	}
	return out
}

// containerPair is one container as it exists in either revision. A zero
// before or after is a container that only exists on the other side.
type containerPair struct {
	workload  string
	container string
	before    container
	after     container
}

// matchContainers pairs containers across two revisions by workload ref and
// container name, in a deterministic order.
func matchContainers(previous, live []workload) []containerPair {
	type key struct{ workload, container string }
	pairs := map[key]*containerPair{}
	var order []key

	add := func(w workload, c container, isLive bool) {
		k := key{w.ref(), c.Name}
		p, ok := pairs[k]
		if !ok {
			p = &containerPair{workload: k.workload, container: k.container}
			pairs[k] = p
			order = append(order, k)
		}
		if isLive {
			p.after = c
			return
		}
		p.before = c
	}
	for _, w := range previous {
		for _, c := range w.Containers {
			add(w, c, false)
		}
	}
	for _, w := range live {
		for _, c := range w.Containers {
			add(w, c, true)
		}
	}
	sort.SliceStable(order, func(i, j int) bool {
		if order[i].workload != order[j].workload {
			return order[i].workload < order[j].workload
		}
		return order[i].container < order[j].container
	})
	out := make([]containerPair, 0, len(order))
	for _, k := range order {
		out = append(out, *pairs[k])
	}
	return out
}

func envByName(vars []envVar) map[string]envVar {
	out := make(map[string]envVar, len(vars))
	for _, v := range vars {
		out[v.Name] = v
	}
	return out
}

// unionKeys returns every key of either map, sorted, so a diff's order never
// depends on map iteration.
func unionKeys(a, b map[string]envVar) []string {
	seen := make(map[string]bool, len(a)+len(b))
	out := make([]string, 0, len(a)+len(b))
	for _, m := range []map[string]envVar{a, b} {
		for k := range m {
			if seen[k] {
				continue
			}
			seen[k] = true
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}
