package model

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"
)

// autoDeploy (ADR-0036 decision 1).
//
// Every document here sets a field issue #141 still gates, so the cases load
// through loadPairUnvalidated and call the unexported resolve — the same route
// the P4 precedence tests take, and for the same reason: resolution of a gated
// field is real behaviour long before anything acts on it, so landing the
// trigger paths is deleting two table rows rather than building precedence. The
// gate itself is proven in coverage_test.go.

const trackingProject = `
apiVersion: kelson.dev/v1alpha1
kind: Project
metadata: {name: checkout}
spec:
  sources:
    - name: app
      git: https://github.com/acme/checkout
      ref: main
    - name: tools
      git: https://github.com/acme/build-tools
      ref: v2
  components:
    - {name: web, port: 8080, source: app}
    - {name: worker, source: app}
    - {name: builder, source: tools}
    - {name: db, kind: postgres}
`

func trackingEnv(spec string) string {
	return `
apiVersion: kelson.dev/v1alpha1
kind: Environment
metadata: {name: staging}
spec:
  project: checkout
` + spec
}

// tracked resolves a pair and returns the effective tracking list, which is the
// answer both the trigger paths and the UI read.
func tracked(t *testing.T, env string) []string {
	t.Helper()
	p, e := loadPairUnvalidated(t, trackingProject, trackingEnv(env))
	return resolved(t, p, e).AutoDeploy
}

// TestAutoDeployPrecedence is decision 1 whole: the component's own setting if
// it declared one, else the environment's, else false. No pair of levels is an
// error and both directions are legal.
func TestAutoDeployPrecedence(t *testing.T) {
	for _, tc := range []struct {
		name string
		spec string
		want []string
	}{
		{
			name: "unset everywhere is manual",
			spec: "",
		},
		{
			name: "the environment tracks everything that builds",
			spec: "  autoDeploy: true\n",
			want: []string{"web", "worker", "builder"},
		},
		{
			name: "an environment that says false is the same as one that says nothing",
			spec: "  autoDeploy: false\n",
		},
		{
			name: "a component opts out of a tracking environment",
			spec: "  autoDeploy: true\n  components:\n    - {name: worker, autoDeploy: false}\n",
			want: []string{"web", "builder"},
		},
		{
			name: "a component opts in under an environment that sets nothing",
			spec: "  components:\n    - {name: worker, autoDeploy: true}\n",
			want: []string{"worker"},
		},
		{
			name: "a component opts in under an environment that says false",
			spec: "  autoDeploy: false\n  components:\n    - {name: builder, autoDeploy: true}\n",
			want: []string{"builder"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := tracked(t, tc.spec); !slices.Equal(got, tc.want) {
				t.Errorf("AutoDeploy = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestAutoDeployAsksResolvedNotTheDocuments: the precedence rule has one
// implementation, and it is reachable as a question about a component rather
// than as a list a caller has to search.
func TestAutoDeployAsksResolvedNotTheDocuments(t *testing.T) {
	p, e := loadPairUnvalidated(t, trackingProject,
		trackingEnv("  autoDeploy: true\n  components:\n    - {name: worker, autoDeploy: false}\n"))
	r := resolved(t, p, e)

	if !r.AutoDeploys("web") {
		t.Error("web inherits the environment's tracking")
	}
	if r.AutoDeploys("worker") {
		t.Error("worker overrode the environment to false and must not track")
	}
	if r.AutoDeploys("nothing-of-the-sort") {
		t.Error("a component that does not exist tracks nothing")
	}
}

// TestAutoDeploySkipsWhatBuildsNothing: a data component has no source, so a
// tracking environment says nothing about it. The kinds that build nothing are
// outside the question entirely (ADR-0035 decision 3).
func TestAutoDeploySkipsWhatBuildsNothing(t *testing.T) {
	if got := tracked(t, "  autoDeploy: true\n"); slices.Contains(got, "db") {
		t.Errorf("a data component must not appear in the tracking set: %v", got)
	}
}

// TestAutoDeployOnAnUnbuildableKindIsRefused: the flag is held to the half of
// the model it belongs to, exactly as an image on a database is — a field that
// resolves into nothing is the silence issue #141 exists to prevent.
func TestAutoDeployOnAnUnbuildableKindIsRefused(t *testing.T) {
	docs, _ := DecodeDocuments([]byte(`
apiVersion: kelson.dev/v1alpha1
kind: Project
metadata: {name: shop}
spec:
  image: ghcr.io/acme/shop:2
  components:
    - {name: db, kind: postgres}
    - {name: web, port: 8080}
---
apiVersion: kelson.dev/v1alpha1
kind: Environment
metadata: {name: prod}
spec:
  project: shop
  components:
    - {name: db, autoDeploy: true}
`))
	errs := ValidateEnvironment(docs[1].(*Environment), docs[0].(*Project))
	var refused *Error
	for i := range errs {
		if errs[i].Code == ErrMutuallyExclusive && strings.HasSuffix(errs[i].Field, ".autoDeploy") {
			refused = &errs[i]
		}
	}
	if refused == nil {
		t.Fatalf("autoDeploy on a data component must be refused, got:\n%v", errs)
	}
	if !strings.Contains(refused.Remediation, "preset") {
		t.Errorf("the refusal must point at what a data component *can* be overridden with: %s", refused.Remediation)
	}
}

// TestPinsAreRecordedPerComponent: which scope named a component's image is
// what resolution merges away, and decision 2 needs it back — an environment
// pin and a component image both hold a component still, and a Project image
// does not, because `--image` stands in for it (rule P3).
func TestPinsAreRecordedPerComponent(t *testing.T) {
	const pinnedProject = `
apiVersion: kelson.dev/v1alpha1
kind: Project
metadata: {name: checkout}
spec:
  image: ghcr.io/acme/checkout:1
  sources:
    - {name: app, git: https://github.com/acme/checkout, ref: main}
  components:
    - {name: web, port: 8080, source: app}
    - {name: worker, source: app, image: ghcr.io/acme/worker:1.4.0}
    - {name: builder, source: app}
`
	p, e := loadPairUnvalidated(t, pinnedProject, trackingEnv(
		"  autoDeploy: true\n  components:\n    - {name: web, image: 'ghcr.io/acme/checkout@sha256:9f6ad2c1'}\n"))
	r := resolved(t, p, e)

	if !slices.Equal(r.ImagePins, []string{"web", "worker"}) {
		t.Errorf("ImagePins = %v, want [web worker]: a Project image is the slot a build fills, not a pin", r.ImagePins)
	}
	if !r.ImagePinned("web") || !r.ImagePinned("worker") || r.ImagePinned("builder") {
		t.Errorf("ImagePinned disagrees with ImagePins: %v", r.ImagePins)
	}
	if !r.AutoDeploys("web") {
		t.Error("a pin does not switch the flag off — it is the flag plus a reason the component cannot move")
	}
}

// TestTrackingStaysOffTheComponentHash is the churn guard the ADR-0035 slice
// wrote for source bindings, restated for this one: neither fact may reach a
// ResolvedComponent, because that struct is hashed into every resource's
// `kelson.dev/spec-hash` and neither changes a rendered byte.
func TestTrackingStaysOffTheComponentHash(t *testing.T) {
	p, e := loadPairUnvalidated(t, trackingProject, trackingEnv("  autoDeploy: true\n"))
	r := resolved(t, p, e)

	for _, c := range r.Components {
		blob, err := json.Marshal(c)
		if err != nil {
			t.Fatalf("marshalling %s: %v", c.Name, err)
		}
		for _, forbidden := range []string{"autoDeploy", "imagePins"} {
			if strings.Contains(string(blob), forbidden) {
				t.Errorf("ResolvedComponent %s carries %q: %s", c.Name, forbidden, blob)
			}
		}
	}
}

// TestUntrackedSpecsCarryNeitherKey: an environment that says nothing about
// tracking hashes exactly as it did before the fields existed, so adding them
// re-tagged nothing.
func TestUntrackedSpecsCarryNeitherKey(t *testing.T) {
	p, e := loadPairUnvalidated(t, trackingProject, trackingEnv(""))
	blob, err := json.Marshal(resolved(t, p, e))
	if err != nil {
		t.Fatalf("marshalling the resolved spec: %v", err)
	}
	for _, forbidden := range []string{"autoDeploy", "imagePins"} {
		if strings.Contains(string(blob), forbidden) {
			t.Errorf("a spec that tracks nothing must marshal no %q: %s", forbidden, blob)
		}
	}
}
