package build

import (
	"fmt"
	"strings"

	"github.com/dafrie/kelson/internal/model"
)

// What a build clones, once ADR-0035 made that a per-component question.
//
// Until sources were named, "the repository" was a property of the Project:
// `spec.source.git`, one clone, one image, every component built from it.
// ADR-0035 decision 3 moves the statement onto the component — each buildable
// component binds to exactly one declared source — and decision 4 says the
// build plane follows: "a component's clone, ref-resolution and build run
// against *its* source".
//
// So the build path no longer reads `spec.source`. It reads the bindings the
// resolver produced ([model.Resolved.Sources]), which already answer the
// question for the singular spelling, the plural spelling and a name that
// resolved to one of the instance's GitSources — one function, three spellings,
// no second copy of the shadowing rule.
//
// This file lives beside plan.go and for the same reason: the CLI and the API
// server must not be able to disagree about which repository a spec builds
// from, or about what a refusal to build it is called.

// SourceBinding is one source a build clones, with the components whose code
// lives in it.
//
// Components are carried rather than counted because they are what the refusals
// below have to name — an operator told "this project binds to two sources"
// still has to open the spec to find out which component is where — and because
// they are what makes the sharing rule visible: two components bound to one
// source are one entry here, hence one clone, not two.
type SourceBinding struct {
	// Source is the binding as the resolver produced it: the name that won
	// (project-local shadowing a global one), the repository, the ref and the
	// connection the author named, if any.
	Source model.ResolvedSource

	// Components are the components bound to it, in spec order.
	Components []string
}

// Bindings groups a resolved spec's per-component bindings into the distinct
// sources a build must clone, in the order the components that bind to them
// appear.
//
// Grouping is by source *name*, which is the identity resolution assigns: a
// name resolves to exactly one source in one resolution, so two components
// naming `tools` are two components on one repository, at one ref, read with
// one credential. That is what makes the clone shared rather than repeated —
// the property ADR-0035 decision 4 asks for when it says a project may clone
// several repositories per revision, not several copies of one.
//
// Two *different* names pointing at the same URL stay two entries. They are two
// declarations, they may carry different refs or different connections, and
// collapsing them would make a build's inputs depend on a comparison of URL
// spellings rather than on what the spec said.
func Bindings(r *model.Resolved) []SourceBinding {
	if r == nil {
		return nil
	}
	var out []SourceBinding
	index := map[string]int{}
	for _, binding := range r.Sources {
		key := binding.Source.Name
		if at, seen := index[key]; seen {
			out[at].Components = append(out[at].Components, binding.Component)
			continue
		}
		index[key] = len(out)
		out = append(out, SourceBinding{Source: binding.Source, Components: []string{binding.Component}})
	}
	return out
}

// SourceToBuild picks the source one build request clones, or refuses.
//
// A build is one clone of one repository producing one image, on both callers
// and on the wire (BuildResponse.Finished carries one reference), so the
// question this answers is singular even though the model's is not. The three
// answers are:
//
//   - nothing bound — [ReasonNoSource]. This is the refusal that used to fire
//     for a project spelling its sources in the plural, which was never what it
//     meant: it now fires only for a project no buildable component of which is
//     bound to a source at all.
//   - exactly one — that one, whichever spelling declared it and whether it came
//     from the Project's list or the instance's GitSources.
//   - several — [ReasonSeveralSources], naming each and what to do instead.
//     kelson's own build plane pins one image per project (registry.Repository,
//     model rule P3), so a project whose components build from different
//     repositories needs per-component image production, which is what
//     `spec.build.by: ci` and ReportBuild already are (ADR-0034 decision 3).
func SourceToBuild(project string, r *model.Resolved) (SourceBinding, error) {
	bindings := Bindings(r)
	switch len(bindings) {
	case 0:
		return SourceBinding{}, Error{
			Reason: ReasonNoSource,
			Message: fmt.Sprintf("no buildable component of Project %s is bound to a source, "+
				"so there is nothing to build from", project),
			Remediation: "declare the repository under spec.sources (or the singular spec.source, which is " +
				"shorthand for one entry named `default`) and bind a component to it with `source: <name>` — a " +
				"component that names none uses the project's default. Or deploy a pre-built image instead, with " +
				"spec.image or an explicit image reference (ADR-0035 decision 3)",
		}
	case 1:
		return bindings[0], nil
	default:
		return SourceBinding{}, Error{
			Reason: ReasonSeveralSources,
			Message: fmt.Sprintf("Project %s builds its components from %d different sources (%s), and one build "+
				"produces one image", project, len(bindings), describeBindings(bindings)),
			Remediation: "kelson's own build plane clones one repository per build and pins one image per project, " +
				"so a project whose components build from different repositories cannot be built by it. Hand image " +
				"production to your pipeline instead — set spec.build.by: ci and let each repository's CI report the " +
				"components it built, with `kelson ci report-build --sha <commit> --image <component>=<reference>` " +
				"(ADR-0034 decision 3, ADR-0035 decision 4) — or split the project so each one builds from one " +
				"repository",
		}
	}
}

// describeBindings renders the sources a project binds to as
// `name (component, component)`, in binding order, for a refusal that has to be
// actionable without opening the spec.
func describeBindings(bindings []SourceBinding) string {
	parts := make([]string, 0, len(bindings))
	for _, b := range bindings {
		name := b.Source.Name
		if name == "" {
			name = b.Source.Git
		}
		parts = append(parts, fmt.Sprintf("%s → %s", name, strings.Join(b.Components, ", ")))
	}
	return strings.Join(parts, "; ")
}

// SourceRef is the ref this build resolves against the remote: the caller's
// override if it gave one, else the source's own `ref:`, else empty for the
// repository's default branch.
//
// The override is per request and the ref is per source (ADR-0035 decision 1),
// so this is where the two meet. It is a function rather than two lines at each
// call site because `kelson build --ref` and BuildRequest.ref are documented as
// the same override and must not drift into meaning two things.
func (b SourceBinding) SourceRef(override string) string {
	if override = strings.TrimSpace(override); override != "" {
		return override
	}
	return b.Source.Ref
}
