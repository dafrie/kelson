package model

import (
	"slices"
	"testing"
)

// The authoring half of the release-command hook (issue #104): which kinds may
// carry one, what it must say, and what resolution fills in.

func TestReleaseHookValidates(t *testing.T) {
	for _, tc := range []struct {
		name     string
		entry    string
		wantCode Code
		wantErr  bool
	}{
		{
			name:  "service with a release command",
			entry: `{name: web, port: 8080, release: {command: ["./manage.py", "migrate"]}}`,
		},
		{
			name:  "worker with a release command and a timeout",
			entry: `{name: worker, release: {command: ["rake", "db:migrate"], timeout: 30m}}`,
		},
		{
			name:  "agent with a release command",
			entry: `{name: triage, kind: agent, release: {command: ["./migrate"]}}`,
		},
		{
			name:     "no command",
			entry:    `{name: web, port: 8080, release: {timeout: 5m}}`,
			wantErr:  true,
			wantCode: ErrMissingRequired,
		},
		{
			name:     "empty argument",
			entry:    `{name: web, port: 8080, release: {command: ["./migrate", ""]}}`,
			wantErr:  true,
			wantCode: ErrMissingRequired,
		},
		{
			name:     "timeout is not a duration",
			entry:    `{name: web, port: 8080, release: {command: ["./migrate"], timeout: soon}}`,
			wantErr:  true,
			wantCode: ErrInvalidFormat,
		},
		{
			name:     "timeout of zero",
			entry:    `{name: web, port: 8080, release: {command: ["./migrate"], timeout: 0s}}`,
			wantErr:  true,
			wantCode: ErrOutOfRange,
		},
		{
			// A cron component already is a command on a schedule; a release
			// hook on one would be a command inside a command.
			name:     "cron",
			entry:    `{name: nightly, schedule: "0 3 * * *", release: {command: ["./migrate"]}}`,
			wantErr:  true,
			wantCode: ErrMutuallyExclusive,
		},
		{
			name:     "data component",
			entry:    `{name: db, kind: postgres, preset: small, release: {command: ["./migrate"]}}`,
			wantErr:  true,
			wantCode: ErrMutuallyExclusive,
		},
		{
			name: "helm component",
			entry: `{name: ingress, kind: helm, chart: c, chartVersion: "1.0.0", ` +
				`source: {repository: "https://charts.example.com"}, release: {command: ["./migrate"]}}`,
			wantErr:  true,
			wantCode: ErrMutuallyExclusive,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			errs := decodeProjectSpec(t, "  image: i:1\n  components:\n    - "+tc.entry+"\n")
			if !tc.wantErr {
				if len(errs) > 0 {
					t.Fatalf("%s must validate, got:\n%v", tc.entry, errs)
				}
				return
			}
			if len(errs) == 0 {
				t.Fatalf("%s must be rejected", tc.entry)
			}
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
			resolved, errs := Resolve(p, e)
			if len(errs) > 0 {
				t.Fatalf("resolve: %v", errs)
			}
			got := resolved.Components[0].Release
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
