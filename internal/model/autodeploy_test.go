package model

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"
)

// autoDeploy (ADR-0036 decisions 1 and 2).
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

// TestStaleSetFollowsTheBinding is decision 2's routing half: a push moves what
// is bound to the repository that moved, and nothing else. There is no
// per-component ref field that could disagree with the binding (ADR-0035).
func TestStaleSetFollowsTheBinding(t *testing.T) {
	p, e := loadPairUnvalidated(t, trackingProject, trackingEnv("  autoDeploy: true\n"))
	r := resolved(t, p, e)

	if got := r.StaleComponents("https://github.com/acme/checkout", "main"); !slices.Equal(got, []string{"web", "worker"}) {
		t.Errorf("a push to the app repository = %v, want [web worker]", got)
	}
	if got := r.StaleComponents("https://github.com/acme/build-tools", "v2"); !slices.Equal(got, []string{"builder"}) {
		t.Errorf("a push to the tools repository = %v, want [builder]", got)
	}
	if got := r.StaleComponents("https://github.com/acme/unrelated", "main"); got != nil {
		t.Errorf("a push to a repository nothing binds to = %v, want nothing", got)
	}
	if got := r.StaleComponents("https://github.com/acme/checkout", "release-2"); got != nil {
		t.Errorf("a push to a ref no source reads = %v, want nothing", got)
	}
}

// TestStaleSetTracksOnlyWhatOptedIn: the binding says which components a push
// *could* move; the flag says which of them it does.
func TestStaleSetTracksOnlyWhatOptedIn(t *testing.T) {
	p, e := loadPairUnvalidated(t, trackingProject,
		trackingEnv("  components:\n    - {name: worker, autoDeploy: true}\n"))
	r := resolved(t, p, e)

	if got := r.StaleComponents("https://github.com/acme/checkout", "main"); !slices.Equal(got, []string{"worker"}) {
		t.Errorf("stale set = %v, want just the component that opted in", got)
	}

	p, e = loadPairUnvalidated(t, trackingProject, trackingEnv(""))
	if got := resolved(t, p, e).StaleComponents("https://github.com/acme/checkout", "main"); got != nil {
		t.Errorf("an environment that tracks nothing = %v, want nothing — silently", got)
	}
}

// TestStaleSetSkipsPinnedComponents is the P3 half of decision 2: a pinned
// component ignores everything, which is the promotion posture ADR-0016
// established. Both scopes that beat `--image` hold a component still, and
// neither switches the flag off — the component tracks and cannot move.
func TestStaleSetSkipsPinnedComponents(t *testing.T) {
	const pinnedProject = `
apiVersion: kelson.dev/v1alpha1
kind: Project
metadata: {name: checkout}
spec:
  sources:
    - {name: app, git: https://github.com/acme/checkout, ref: main}
  components:
    - {name: web, port: 8080, source: app}
    - {name: worker, source: app, image: ghcr.io/acme/worker:1.4.0}
`
	p, e := loadPairUnvalidated(t, pinnedProject, trackingEnv(
		"  autoDeploy: true\n  components:\n    - {name: web, image: 'ghcr.io/acme/checkout@sha256:9f6ad2c1'}\n"))
	r := resolved(t, p, e)

	if !r.AutoDeploys("web") || !r.AutoDeploys("worker") {
		t.Fatalf("both components track: %v", r.AutoDeploy)
	}
	if got := r.StaleComponents("https://github.com/acme/checkout", "main"); got != nil {
		t.Errorf("stale set = %v, want nothing: every component is pinned", got)
	}
}

// TestProjectImageIsNotAPin: `--image` stands in for the Project's image (rule
// P3), so a project-wide image is the slot a build fills rather than a pin, and
// a push moves what it names.
func TestProjectImageIsNotAPin(t *testing.T) {
	const projectImage = `
apiVersion: kelson.dev/v1alpha1
kind: Project
metadata: {name: checkout}
spec:
  image: ghcr.io/acme/checkout:1
  sources:
    - {name: app, git: https://github.com/acme/checkout, ref: main}
  components:
    - {name: web, port: 8080, source: app}
`
	p, e := loadPairUnvalidated(t, projectImage, trackingEnv("  autoDeploy: true\n"))
	r := resolved(t, p, e)

	if got := r.StaleComponents("https://github.com/acme/checkout", "main"); !slices.Equal(got, []string{"web"}) {
		t.Errorf("stale set = %v, want [web]", got)
	}
}

// TestStaleSetSkipsCommitPinnedSources: a source at a commit names one revision
// forever, so no push moves it — not even one whose ref is spelled as that
// commit.
func TestStaleSetSkipsCommitPinnedSources(t *testing.T) {
	const sha = "9f6ad2c1b3e4f5a67890123456789abcdef01234"
	const pinnedSource = `
apiVersion: kelson.dev/v1alpha1
kind: Project
metadata: {name: checkout}
spec:
  sources:
    - {name: app, git: https://github.com/acme/checkout, ref: ` + sha + `}
  components:
    - {name: web, port: 8080, source: app}
`
	p, e := loadPairUnvalidated(t, pinnedSource, trackingEnv("  autoDeploy: true\n"))
	r := resolved(t, p, e)

	if got := r.StaleComponents("https://github.com/acme/checkout", sha); got != nil {
		t.Errorf("stale set = %v, want nothing: a SHA-pinned source tracks nothing", got)
	}
	if got := r.StaleComponents("https://github.com/acme/checkout", "main"); got != nil {
		t.Errorf("stale set = %v, want nothing", got)
	}
}

// TestStaleSetNeedsBothHalvesOfThePush: an empty repository or an empty ref
// matches nothing rather than everything, because "I do not know what moved" is
// not a licence to redeploy.
func TestStaleSetNeedsBothHalvesOfThePush(t *testing.T) {
	p, e := loadPairUnvalidated(t, trackingProject, trackingEnv("  autoDeploy: true\n"))
	r := resolved(t, p, e)

	for _, tc := range []struct{ repo, ref string }{
		{"", "main"},
		{"https://github.com/acme/checkout", ""},
		{"", ""},
	} {
		if got := r.StaleComponents(tc.repo, tc.ref); got != nil {
			t.Errorf("StaleComponents(%q, %q) = %v, want nothing", tc.repo, tc.ref, got)
		}
	}
}

// TestStaleSetTakesShortRefs pins the contract at the seam: the caller strips
// the ref namespace, and this side compares short names exactly.
func TestStaleSetTakesShortRefs(t *testing.T) {
	p, e := loadPairUnvalidated(t, trackingProject, trackingEnv("  autoDeploy: true\n"))
	r := resolved(t, p, e)

	if got := r.StaleComponents("https://github.com/acme/checkout", "refs/heads/main"); got != nil {
		t.Errorf("a ref the caller did not normalize = %v; the contract is the short name", got)
	}
	if got := r.StaleComponents("https://github.com/acme/checkout", "Main"); got != nil {
		t.Errorf("git refs are case-sensitive, got %v", got)
	}
}

// TestSameRepositoryAcrossSpellings: a spec and a forge write the same
// repository differently, and neither is wrong.
func TestSameRepositoryAcrossSpellings(t *testing.T) {
	const spec = "https://github.com/acme/checkout"
	for _, same := range []string{
		"https://github.com/acme/checkout",
		"https://github.com/acme/checkout.git",
		"https://github.com/acme/checkout/",
		"https://GitHub.com/Acme/Checkout",
		"github.com/acme/checkout",
		"git@github.com:acme/checkout.git",
		"http://github.com/acme/checkout",
	} {
		if !SameRepository(spec, same) {
			t.Errorf("SameRepository(%q, %q) = false, want true", spec, same)
		}
	}
	for _, other := range []string{
		"https://gitlab.com/acme/checkout", // same path, different forge: a real mirror collision
		"https://github.com/acme/checkout-ui",
		"https://github.com/other/checkout",
		"https://github.com",
		"",
		"::not a url",
	} {
		if SameRepository(spec, other) {
			t.Errorf("SameRepository(%q, %q) = true, want false", spec, other)
		}
	}
}
