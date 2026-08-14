// Package promote is the porcelain over the per-environment image pin
// (ADR-0016 decision 2, docs/model.md "Promotion").
//
// # What a promotion is, and what this package owns
//
// Promoting staging to production is three operations kelson already had:
// read the digest staging deployed, write it as production's pin, deploy. The
// pin itself is one spec field — `Environment.spec.components[].image` — and
// the deploy is the ordinary deploy. What was missing, and what lives here, is
// the middle: deciding which image each component is promoted to, and writing
// that decision into the authored document without disturbing anything else.
//
// The package is deliberately I/O-free and cluster-free. [Deployed] reads
// rendered manifests it is handed, [Plan] decides from values, and [Pin] edits
// bytes. Where those bytes come from — the local -f file for `kelson promote`,
// the cluster-backed spec store for the Promote RPC — is the caller's problem,
// which is what lets the CLI and the server share one meaning of "promote".
//
// # The digest comes from the delivery history, not from the spec
//
// The source of a promotion is what the source environment's latest revision
// *runs*, extracted from the rendered manifests that revision recorded. It is
// deliberately not the source environment's spec: a spec says what should be
// deployed and a revision says what was, and promoting the first would move
// production to an image staging has not proven. A component whose image the
// recorded manifests do not carry is skipped with a reason and never guessed.
//
// # The write is a splice, not a re-serialization
//
// The spec store is byte-faithful (ADR-0013 §1) and so is a -f file on disk:
// the document is the user's, comments and key order included. A yaml.v3
// Node round-trip does not preserve it — blank lines vanish, flow mappings
// are re-spaced, trailing comments move column — so [Pin] uses the parsed
// node tree only to *locate* the edit and then splices bytes at that
// position. Everything outside the one line it writes is unchanged by
// construction rather than by hope, which is the same guarantee
// ui/src/spec/edit.ts buys with its byte-equality guard.
package promote

import (
	"fmt"
	"strings"

	"github.com/dafrie/kelson/internal/model"
)

// Code is a promotion error code. The wire error taxonomy passes codes through
// verbatim (ADR-0013 §2), so these strings are a compatibility promise and
// agents branch on them.
type Code string

const (
	// ErrNothingDeployed is a promotion whose source environment has no
	// recorded revision. There is no deployed truth to read, and re-rendering
	// the source spec instead would promote an intention rather than a fact.
	ErrNothingDeployed Code = "promote/nothing-deployed"

	// ErrNoWorkloads is a promotion that would pin nothing: the project
	// declares no workload components, or the component filter selected only
	// data components. A data component runs what its operator runs (ADR-0005),
	// so it has no image to promote.
	ErrNoWorkloads Code = "promote/no-workloads"

	// ErrComponentUnknown is a component filter naming something the project
	// does not declare.
	ErrComponentUnknown Code = "promote/component-unknown"

	// ErrNotInRevision is the per-component skip: the source environment's
	// latest revision carries no image for this component. It is a reason
	// attached to one component, not a failure of the promotion.
	ErrNotInRevision Code = "promote/not-in-revision"

	// ErrDocumentUnwritable is an Environment document whose shape the pin
	// splice will not edit — a flow-style components list, a components key
	// that is not a sequence. Refusing is the point: rewriting such a document
	// would mean re-serializing it and losing the formatting the store
	// promises to keep.
	ErrDocumentUnwritable Code = "promote/document-unwritable"
)

const promoteDocsBase = "https://kelson.dev/model/promotion"

// Error is one structured promotion failure. It shares the shape of
// delivery.Error and controlstore.Error so the API layer maps it onto the one
// wire Error without inventing a second taxonomy.
type Error struct {
	Code        Code   `json:"code"`
	Resource    string `json:"resource"`
	Field       string `json:"field,omitempty"`
	Message     string `json:"message"`
	Remediation string `json:"remediation"`
	DocsURL     string `json:"docsUrl"`
}

func (e Error) Error() string {
	loc := e.Resource
	if e.Field != "" {
		loc += "." + e.Field
	}
	return fmt.Sprintf("%s [%s] %s: %s (see %s)", loc, e.Code, e.Message, e.Remediation, e.DocsURL)
}

func newError(code Code, resource, field, msg, remediation string) Error {
	return Error{
		Code:        code,
		Resource:    resource,
		Field:       field,
		Message:     msg,
		Remediation: remediation,
		DocsURL:     promoteDocsBase,
	}
}

// Status is what a promotion decided for one component.
type Status string

const (
	// StatusPinned is a component whose pin the promotion writes.
	StatusPinned Status = "pinned"
	// StatusUnchanged is a component already pinned to the image the source
	// runs. Reporting it as a no-op rather than omitting it is deliberate: a
	// caller reading the plan must be able to see that the component was
	// considered.
	StatusUnchanged Status = "unchanged"
	// StatusSkipped is a component the promotion declined to pin, with a
	// reason. Skipping is never silent and never a guess.
	StatusSkipped Status = "skipped"
)

// Change is the promotion's decision about one component.
type Change struct {
	// Component is the component's name in the Project.
	Component string
	// From is the image the target environment is pinned to today. Empty means
	// unpinned: the environment was following the component or project image.
	From string
	// To is the image the source environment's latest revision runs, and the
	// value the pin is written to. Empty when the component is skipped.
	To string
	// Status is what happens to this component.
	Status Status
	// Code and Reason explain a skip, in the taxonomy and in prose. Both are
	// empty for a pinned or unchanged component.
	Code   Code
	Reason string
}

// Plan decides what promoting into target pins, given what the source
// environment's latest revision runs.
//
// deployed maps a component name to the image that revision runs — the output
// of [Deployed]. only, when non-empty, restricts the promotion to the named
// components; a name the project does not declare is an error rather than a
// silent no-op, because a typo in a filter must not read as "nothing to
// promote".
//
// The returned changes follow the Project's component order, which is the
// order the author wrote and therefore the order every other kelson output
// uses.
func Plan(project *model.Project, target *model.Environment, deployed map[string]string, only []string) ([]Change, error) {
	if project == nil || target == nil {
		return nil, fmt.Errorf("promote: a project and a target environment are required")
	}
	filter, err := componentFilter(project, only)
	if err != nil {
		return nil, err
	}

	pins := map[string]string{}
	for _, ov := range target.Spec.Components {
		pins[ov.Name] = ov.Image
	}

	var changes []Change
	workloads := 0
	for _, c := range project.Spec.Components {
		if !c.EffectiveKind().IsWorkload() {
			continue
		}
		if filter != nil && !filter[c.Name] {
			continue
		}
		workloads++
		change := Change{Component: c.Name, From: pins[c.Name]}
		image, ok := deployed[c.Name]
		switch {
		case !ok:
			change.Status = StatusSkipped
			change.Code = ErrNotInRevision
			change.Reason = "the source environment's latest revision records no image for this component"
		case image == change.From:
			change.To = image
			change.Status = StatusUnchanged
		default:
			change.To = image
			change.Status = StatusPinned
		}
		changes = append(changes, change)
	}

	if workloads == 0 {
		return nil, newError(ErrNoWorkloads,
			"Project/"+project.Metadata.Name, "$.spec.components",
			"this promotion would pin nothing: no workload component is selected",
			"promote a component the project declares as a service, worker, cron or agent; a data component runs what its operator runs and has no image to promote")
	}
	return changes, nil
}

// componentFilter validates --component/components against the project and
// returns the selected set, or nil when everything is selected.
func componentFilter(project *model.Project, only []string) (map[string]bool, error) {
	if len(only) == 0 {
		return nil, nil
	}
	declared := map[string]bool{}
	names := make([]string, 0, len(project.Spec.Components))
	for _, c := range project.Spec.Components {
		declared[c.Name] = true
		names = append(names, c.Name)
	}
	filter := map[string]bool{}
	for _, name := range only {
		if !declared[name] {
			return nil, newError(ErrComponentUnknown,
				"Project/"+project.Metadata.Name, "$.spec.components",
				fmt.Sprintf("component %q is not declared by this project (declared: %s)", name, strings.Join(names, ", ")),
				"name a component the project declares, or omit the filter to promote every workload component")
		}
		filter[name] = true
	}
	return filter, nil
}

// NothingDeployed is the refusal a promotion makes when its source environment
// has no recorded revision.
func NothingDeployed(project, environment string) error {
	return newError(ErrNothingDeployed,
		fmt.Sprintf("Environment/%s", environment), "",
		fmt.Sprintf("nothing has been deployed to %s/%s, so there is no deployed image to promote", project, environment),
		fmt.Sprintf("deploy %s first (kelson deploy --env %s); a promotion reads what the source environment's latest revision runs, never what its spec intends", environment, environment))
}

// Pinned returns the changes that write a pin, in plan order. It is the set a
// caller edits documents for; unchanged and skipped components need no write.
func Pinned(changes []Change) []Change {
	var out []Change
	for _, c := range changes {
		if c.Status == StatusPinned {
			out = append(out, c)
		}
	}
	return out
}

// Summary is the one-line count every surface prints, so the CLI, the MCP tool
// and the UI cannot disagree about what a promotion did.
func Summary(changes []Change) string {
	var pinned, unchanged, skipped int
	for _, c := range changes {
		switch c.Status {
		case StatusPinned:
			pinned++
		case StatusUnchanged:
			unchanged++
		case StatusSkipped:
			skipped++
		}
	}
	return fmt.Sprintf("%d pinned, %d unchanged, %d skipped", pinned, unchanged, skipped)
}
