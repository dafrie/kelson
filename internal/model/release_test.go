package model

import (
	"slices"
	"strings"
	"testing"
)

// The authoring half of the release-command hook (issue #104): which kinds may
// carry one, what it must say, and what resolution fills in.
//
// Since ADR-0028 decision 8 the hook is *gated*: the plane that waited for the
// Job is deleted, so a component that carries one is refused by name with #227
// in the message. What is tested here is therefore two things at once — the
// refusal, and that the shape checks still run underneath it, because the day
// #227 lands the row goes and these documents have to be judged on their own
// terms again.

// TestReleaseHookIsGated: a well-formed hook on a kind that may carry one is
// refused, and the refusal names the field and the issue rather than leaving
// the author to discover that their migration never ran.
func TestReleaseHookIsGated(t *testing.T) {
	for _, entry := range []string{
		`{name: web, port: 8080, release: {command: ["./manage.py", "migrate"]}}`,
		`{name: worker, release: {command: ["rake", "db:migrate"], timeout: 30m}}`,
		`{name: triage, kind: agent, release: {command: ["./migrate"]}}`,
	} {
		t.Run(entry, func(t *testing.T) {
			errs := decodeProjectSpec(t, "  image: i:1\n  components:\n    - "+entry+"\n")
			var gate *Error
			for i := range errs {
				if errs[i].Code == ErrNotImplemented && errs[i].Field == "$.spec.components[0].release" {
					gate = &errs[i]
				}
			}
			if gate == nil {
				t.Fatalf("a release hook must be gated until #227, got:\n%v", errs)
			}
			if !strings.Contains(gate.Remediation, "#227") {
				t.Errorf("the gate must name where the work is tracked, got %q", gate.Remediation)
			}
			if len(errs) != 1 {
				t.Errorf("a well-formed hook must produce the gate and nothing else, got:\n%v", errs)
			}
		})
	}
}

// TestReleaseHookShapeIsStillChecked: the gate is about what kelson implements,
// not about what the author wrote.
func TestReleaseHookShapeIsStillChecked(t *testing.T) {
	for _, tc := range []struct {
		name     string
		entry    string
		wantCode Code
	}{
		{
			name:     "no command",
			entry:    `{name: web, port: 8080, release: {timeout: 5m}}`,
			wantCode: ErrMissingRequired,
		},
		{
			name:     "empty argument",
			entry:    `{name: web, port: 8080, release: {command: ["./migrate", ""]}}`,
			wantCode: ErrMissingRequired,
		},
		{
			name:     "timeout is not a duration",
			entry:    `{name: web, port: 8080, release: {command: ["./migrate"], timeout: soon}}`,
			wantCode: ErrInvalidFormat,
		},
		{
			name:     "timeout of zero",
			entry:    `{name: web, port: 8080, release: {command: ["./migrate"], timeout: 0s}}`,
			wantCode: ErrOutOfRange,
		},
		{
			// A cron component already is a command on a schedule; a release
			// hook on one would be a command inside a command.
			name:     "cron",
			entry:    `{name: nightly, schedule: "0 3 * * *", release: {command: ["./migrate"]}}`,
			wantCode: ErrMutuallyExclusive,
		},
		{
			name:     "data component",
			entry:    `{name: db, kind: postgres, preset: small, release: {command: ["./migrate"]}}`,
			wantCode: ErrMutuallyExclusive,
		},
		{
			name: "helm component",
			entry: `{name: ingress, kind: helm, chart: c, chartVersion: "1.0.0", ` +
				`source: {repository: "https://charts.example.com"}, release: {command: ["./migrate"]}}`,
			wantCode: ErrMutuallyExclusive,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			errs := decodeProjectSpec(t, "  image: i:1\n  components:\n    - "+tc.entry+"\n")
			if !slices.Contains(errs.Codes(), tc.wantCode) {
				t.Fatalf("%s produced %v, want a %s", tc.entry, errs.Codes(), tc.wantCode)
			}
		})
	}
}

// TestReleaseRefusalNamesTheField: the error has to point at `release`, or an
// author has to guess which of a component's fields the complaint is about.
func TestReleaseRefusalNamesTheField(t *testing.T) {
	errs := decodeProjectSpec(t, `  image: i:1
  components:
    - name: db
      kind: postgres
      preset: small
      release: {command: ["./migrate"]}
`)
	found := false
	for _, e := range errs {
		if e.Field == "$.spec.components[0].release" {
			found = true
		}
	}
	if !found {
		t.Fatalf("no error names $.spec.components[0].release:\n%v", errs)
	}
}

// TestReleaseResolvesItsTimeout: the renderer may not import `time`, so the
// duration is seconds by the time it gets there — and an unset timeout is the
// default rather than "no deadline".
//
// Resolution keeps working under the gate on purpose (internal/model's gate
// table says so): landing #227 is deleting a row and a call site, not
// rebuilding the field.
func TestReleaseResolvesItsTimeout(t *testing.T) {
	for _, tc := range []struct {
		written string
		want    int
	}{
		{"", int(DefaultReleaseTimeout.Seconds())},
		{"timeout: 30m", 1800},
		{"timeout: 1h30m", 5400},
		{"timeout: 90s", 90},
	} {
		t.Run("timeout "+tc.written, func(t *testing.T) {
			p, e := releaseDocuments(t, tc.written)
			// resolve rather than Resolve: the exported entry point validates
			// first, and validation is where the gate lives. This is the same
			// split every other gated field's precedence test uses.
			r, errs := resolve(p, e, nil)
			if len(errs) > 0 {
				t.Fatalf("resolve: %v", errs)
			}
			got := r.Components[0].Release
			if got == nil {
				t.Fatal("the release hook did not survive resolution")
			}
			if got.TimeoutSeconds != tc.want {
				t.Errorf("timeout resolved to %ds, want %ds", got.TimeoutSeconds, tc.want)
			}
			if len(got.Command) != 2 {
				t.Errorf("command resolved to %v", got.Command)
			}
		})
	}
}

// TestComponentWithoutReleaseResolvesToNil keeps the spec-hash promise: a
// nil-able field must marshal to nothing, or adding it would have changed the
// provenance annotation of every workload that does not use it.
func TestComponentWithoutReleaseResolvesToNil(t *testing.T) {
	docs, errs := DecodeDocuments([]byte(`apiVersion: kelson.dev/v1alpha1
kind: Project
metadata: {name: p}
spec:
  image: i:1
  components: [{name: web, port: 8080}]
---
apiVersion: kelson.dev/v1alpha1
kind: Environment
metadata: {name: production}
spec: {project: p}
`))
	if len(errs) > 0 {
		t.Fatalf("decode: %v", errs)
	}
	resolved, rerrs := Resolve(docs[0].(*Project), docs[1].(*Environment))
	if len(rerrs) > 0 {
		t.Fatalf("resolve: %v", rerrs)
	}
	if resolved.Components[0].Release != nil {
		t.Fatalf("a component with no release hook resolved to %+v", resolved.Components[0].Release)
	}
}

func releaseDocuments(t *testing.T, timeout string) (*Project, *Environment) {
	t.Helper()
	release := `{command: ["./manage.py", "migrate"]`
	if timeout != "" {
		release += ", " + timeout
	}
	release += "}"
	docs, errs := DecodeDocuments([]byte(`apiVersion: kelson.dev/v1alpha1
kind: Project
metadata: {name: p}
spec:
  image: i:1
  components:
    - {name: web, port: 8080, release: ` + release + `}
---
apiVersion: kelson.dev/v1alpha1
kind: Environment
metadata: {name: production}
spec: {project: p}
`))
	// The gate is the point of TestReleaseHookIsGated and noise here: this
	// helper is about what resolution does with a hook, which is still
	// everything it did before the field was refused.
	for _, e := range errs {
		if e.Code != ErrNotImplemented {
			t.Fatalf("decode: %v", errs)
		}
	}
	return docs[0].(*Project), docs[1].(*Environment)
}
