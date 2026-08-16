package model

import (
	"slices"
	"testing"
)

// The authoring half of the release-command hook (issue #104): which kinds may
// carry one, what it must say, and what resolution fills in.
//
// The hook spent one release in the gate table, because ADR-0028 deleted the
// plane that waited for the Job. Issue #227 gave it a barrier that is Flux's own
// — two Kustomizations with a `dependsOn`, the first health-gated on the Job —
// so the refusal is gone and these documents are judged on their own terms
// again. The tests below are the ones that were written against the gate,
// turned back the right way up: what was "and the gate fires" is now "and
// nothing else is wrong with it".

// TestReleaseHookIsAccepted: a well-formed hook on a kind that may carry one
// validates, with no complaint of any kind. This is the assertion that used to
// require a schema/not-implemented refusal naming #227, and it is the one line
// of the model that landing #227 changed.
func TestReleaseHookIsAccepted(t *testing.T) {
	for _, entry := range []string{
		`{name: web, port: 8080, release: {command: ["./manage.py", "migrate"]}}`,
		`{name: worker, release: {command: ["rake", "db:migrate"], timeout: 30m}}`,
		`{name: triage, kind: agent, release: {command: ["./migrate"]}}`,
	} {
		t.Run(entry, func(t *testing.T) {
			errs := decodeProjectSpec(t, "  image: i:1\n  components:\n    - "+entry+"\n")
			if len(errs) != 0 {
				t.Fatalf("a well-formed release hook must validate, got:\n%v", errs)
			}
		})
	}
}

// TestReleaseHookShapeIsChecked: what the author wrote is still judged, and the
// judgements are the ones ADR-0019 decision 1 fixed — which kinds may carry a
// hook, that it names a command, and that its timeout is a duration a command
// could finish in.
func TestReleaseHookShapeIsChecked(t *testing.T) {
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
// The number this produces is read twice: it is the Job's
// activeDeadlineSeconds, and it is what the release Kustomization's own
// spec.timeout is derived from, so a migration is not reported failed by the
// reconciler while it is still running (internal/renderer's
// ReleaseTimeoutSeconds).
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
	if len(errs) > 0 {
		t.Fatalf("decode: %v", errs)
	}
	return docs[0].(*Project), docs[1].(*Environment)
}
