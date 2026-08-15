package api

import (
	"errors"
	"strings"
	"testing"

	"connectrpc.com/connect"

	kelsonv1alpha1 "github.com/dafrie/kelson/internal/api/gen/kelson/v1alpha1"
	"github.com/dafrie/kelson/internal/build"
	"github.com/dafrie/kelson/internal/controlstore"
)

// autoDeploy's trigger half (ADR-0036 decision 3, issue #248).
//
// What these assert is one behaviour and one design decision. The behaviour: a
// report with a ref and no PR moves exactly the components the stale set names,
// in exactly the environments that opted in, and says why it did not move the
// rest. The decision: what "moves" means here is a *spec write* — the
// environment's per-component image, spliced by internal/promote, stored — and
// not an artifact this process pushed, because the artifact belongs to
// kelson-controller (autodeploy.go's package comment argues it at length). So
// the assertions read the stored document back rather than a fake publisher.

// trackingEnvDoc is an environment that follows its components' sources. `web`
// and `worker` are both bound to the project's single source at `main`, so a
// push to it makes both stale.
func trackingEnvDoc(name string, extra string) []byte {
	return []byte(`apiVersion: kelson.dev/v1alpha1
kind: Environment
metadata: {name: ` + name + `}
spec:
  project: checkout
  autoDeploy: true
` + extra)
}

// trackingReport is what a `by: ci` pipeline sends after building a branch.
func trackingReport(images map[string]string) *kelsonv1alpha1.ReportBuildRequest {
	return &kelsonv1alpha1.ReportBuildRequest{
		Project: "checkout",
		Sha:     reportSHA,
		Ref:     "refs/heads/main",
		Images:  images,
	}
}

func webImage() string    { return "ghcr.io/acme/checkout-web@sha256:" + strings.Repeat("a", 64) }
func workerImage() string { return "ghcr.io/acme/checkout-worker@sha256:" + strings.Repeat("b", 64) }

// nextWebImage is what the *second* push to the tracked branch built. Tracking
// is only tracking if this one lands too.
func nextWebImage() string { return "ghcr.io/acme/checkout-web@sha256:" + strings.Repeat("c", 64) }

// storedEnvironment reads one environment document back out of the store, which
// is where the whole outcome of this trigger lives.
func storedEnvironment(t *testing.T, specs *fakeSpecStore, project, environment string) string {
	t.Helper()
	stored, err := specs.Get(t.Context(), project)
	if err != nil {
		t.Fatalf("reading the stored spec: %v", err)
	}
	doc, ok := stored.Documents.Environments[environment]
	if !ok {
		t.Fatalf("the store holds no document for environment %s", environment)
	}
	return string(doc)
}

// --- the ref contract --------------------------------------------------------

// The model takes a short ref and says so: it parses no payloads, so whichever
// surface read the delivery strips the namespace. This is that surface's half,
// and the two namespaces that collapse are the two git has.
func TestShortRefStripsTheTwoNamespaces(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"refs/heads/main", "main"},
		{"refs/heads/release/2026-08", "release/2026-08"},
		{"refs/tags/v1.2.3", "v1.2.3"},
		{"main", "main"},
		{"v1.2.3", "v1.2.3"},
		{"  refs/heads/main  ", "main"},
		{"", ""},
		// Deliberately untouched: a pull request's head ref is not a branch, and
		// reducing it to "412" would make it match a source that follows a branch
		// of that name.
		{"refs/pull/412/head", "refs/pull/412/head"},
		{"refs/notes/commits", "refs/notes/commits"},
	} {
		if got := ShortRef(tc.in); got != tc.want {
			t.Errorf("ShortRef(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// --- the trigger --------------------------------------------------------------

// The whole tracking half in one test: a report at the ref two components follow
// writes both images into the environment that opted in, and leaves the
// environment that did not exactly as it was.
func TestReportBuildMovesTheStaleComponents(t *testing.T) {
	p := reportServerWith(t, reportProjectDoc, map[string][]byte{
		"staging":    trackingEnvDoc("staging", ""),
		"production": []byte(reportEnvironmentDoc),
	})
	res := report(t, p.clients, trackingReport(map[string]string{
		"web":    webImage(),
		"worker": workerImage(),
	}))

	if !res.GetAccepted() {
		t.Fatalf("accepted = false for a report kelson acted on: %s", res.GetMessage())
	}
	if got := res.GetTriggered(); len(got) != 1 || got[0] != "staging" {
		t.Fatalf("triggered = %v, want the one environment that tracks", got)
	}

	staging := storedEnvironment(t, p.specs, "checkout", "staging")
	for _, want := range []string{webImage(), workerImage()} {
		if !strings.Contains(staging, want) {
			t.Errorf("the stored staging document does not pin %s:\n%s", want, staging)
		}
	}
	// production declares no autoDeploy, so nothing about it moved. The
	// document is byte-identical to what was stored.
	if got := storedEnvironment(t, p.specs, "checkout", "production"); got != reportEnvironmentDoc {
		t.Errorf("the environment that does not track was rewritten:\n%s", got)
	}
	// And the message names what moved where, so a pipeline log answers "what
	// did my report do" without a second call.
	for _, want := range []string{"web", "worker", "staging", "main"} {
		if !strings.Contains(res.GetMessage(), want) {
			t.Errorf("the message does not name %q: %s", want, res.GetMessage())
		}
	}
}

// A tag report moves a component whose source follows that tag, which is the
// half of the ref contract a branch test cannot cover: `refs/tags/` and
// `refs/heads/` collapse to the same short name space, exactly as the authored
// `ref:` field does.
func TestReportBuildFollowsATag(t *testing.T) {
	project := strings.Replace(reportProjectDoc, "    ref: main\n", "    ref: v1.2.3\n", 1)
	p := reportServerWith(t, project, map[string][]byte{"staging": trackingEnvDoc("staging", "")})

	res := report(t, p.clients, &kelsonv1alpha1.ReportBuildRequest{
		Project: "checkout",
		Sha:     reportSHA,
		Ref:     "refs/tags/v1.2.3",
		Images:  map[string]string{"web": webImage()},
	})
	if got := res.GetTriggered(); len(got) != 1 || got[0] != "staging" {
		t.Fatalf("triggered = %v (message %s), want the environment following the tag", got, res.GetMessage())
	}
}

// --- the marker, and the second push (ADR-0036 decision 5) ---------------------

// TestASecondPushMovesWhatTheFirstPushMoved is the defect decision 5 exists to
// close, and it is the one test that could not pass before it.
//
// The trigger's only way to tell the controller anything is a spec write, and
// the field it writes is a pin — so before the marker, this environment moved to
// the first push's digest and then reported itself as pinned forever after. What
// makes the second push land is that the pin it is overwriting is one it wrote:
// marked, therefore in the stale set, therefore its own to move.
func TestASecondPushMovesWhatTheFirstPushMoved(t *testing.T) {
	p := reportServerWith(t, reportProjectDoc, map[string][]byte{"staging": trackingEnvDoc("staging", "")})

	first := report(t, p.clients, trackingReport(map[string]string{"web": webImage()}))
	if got := first.GetTriggered(); len(got) != 1 || got[0] != "staging" {
		t.Fatalf("the first push triggered %v (%s), want staging", got, first.GetMessage())
	}
	after := storedEnvironment(t, p.specs, "checkout", "staging")
	if !strings.Contains(after, webImage()) {
		t.Fatalf("the first push did not pin the reported image:\n%s", after)
	}
	// The pin carries its provenance, which is the whole mechanism: without it
	// the next stale set cannot tell this from a person's promotion.
	if !strings.Contains(after, "imageTracked: true") {
		t.Fatalf("the trigger's pin is unmarked, so tracking stops here:\n%s", after)
	}

	second := report(t, p.clients, trackingReport(map[string]string{"web": nextWebImage()}))
	if got := second.GetTriggered(); len(got) != 1 || got[0] != "staging" {
		t.Fatalf("the second push triggered %v, want staging again — this is the defect #248 hit: %s",
			got, second.GetMessage())
	}
	if strings.Contains(second.GetMessage(), "an image pin holds it") {
		t.Errorf("the trigger reported its own pin as somebody holding the component still: %s", second.GetMessage())
	}

	after = storedEnvironment(t, p.specs, "checkout", "staging")
	if !strings.Contains(after, nextWebImage()) {
		t.Errorf("the second push's image did not reach the document:\n%s", after)
	}
	if strings.Contains(after, webImage()) {
		t.Errorf("the first push's image survived the second:\n%s", after)
	}
	if strings.Count(after, "imageTracked") != 1 {
		t.Errorf("the marker was written more than once:\n%s", after)
	}
}

// An author may write the marker deliberately — image plus marker is "start
// here, and let tracking advance it" (decision 5's third bullet). A document
// that says so moves on the very first push, where the same document without
// the marker would not have moved at all.
func TestAnAuthorWrittenMarkerIsRespected(t *testing.T) {
	const authored = "ghcr.io/acme/checkout-worker:start-here"
	p := reportServerWith(t, reportProjectDoc, map[string][]byte{
		"staging": trackingEnvDoc("staging", `  components:
    - name: worker
      image: `+authored+`
      imageTracked: true
`),
	})
	res := report(t, p.clients, trackingReport(map[string]string{"worker": workerImage()}))

	if got := res.GetTriggered(); len(got) != 1 || got[0] != "staging" {
		t.Fatalf("triggered = %v (%s), want the environment whose author marked the pin", got, res.GetMessage())
	}
	staging := storedEnvironment(t, p.specs, "checkout", "staging")
	if !strings.Contains(staging, workerImage()) || strings.Contains(staging, authored) {
		t.Errorf("the marked pin was not advanced to what the push built:\n%s", staging)
	}
}

// The other half, and the one decision 2 protects: a pin nobody marked is a
// person holding the component still, and the trigger neither overwrites it nor
// argues with it. The document keeps the line byte for byte and the answer says
// why, in the words it has always used.
func TestAnUnmarkedPinIsNeverOverwritten(t *testing.T) {
	const held = "      image: ghcr.io/acme/checkout-worker:frozen-by-a-person\n"
	p := reportServerWith(t, reportProjectDoc, map[string][]byte{
		"staging": trackingEnvDoc("staging", "  components:\n    - name: worker\n"+held),
	})
	res := report(t, p.clients, trackingReport(map[string]string{
		"web":    webImage(),
		"worker": workerImage(),
	}))

	const reason = "environment staging did not move worker: an image pin holds it, and a pinned component " +
		"ignores everything (rule P3, ADR-0016)"
	if !strings.Contains(res.GetMessage(), reason) {
		t.Errorf("the pinned reason is not the one a person's pin has always got\n  want: %s\n  got:  %s",
			reason, res.GetMessage())
	}
	staging := storedEnvironment(t, p.specs, "checkout", "staging")
	if !strings.Contains(staging, held) {
		t.Errorf("the person's pin was rewritten:\n%s", staging)
	}
	if strings.Contains(staging, workerImage()) {
		t.Errorf("the trigger overwrote an unmarked pin:\n%s", staging)
	}
	// It never became the trigger's to move, so nothing marked it either.
	if strings.Contains(staging, "name: worker\n      imageTracked") {
		t.Errorf("the trigger marked a pin it was not allowed to move:\n%s", staging)
	}
	// web was stale, so the environment still moved — the refusal is per
	// component, not per environment.
	if got := res.GetTriggered(); len(got) != 1 {
		t.Fatalf("triggered = %v, want staging to still move for web", got)
	}
}

// --- why a reported component did not move ------------------------------------

// Every reason the stale set has, in the sentence a pipeline reads. This is
// ADR-0036 decision 3's "components reported but not stale are named in the
// response message, not silently deployed", and it is why the resolved spec
// keeps AutoDeploy and ImagePins beside the merged image: one string cannot say
// whether a component held still because nobody tracks it or because somebody
// pinned it, and those two have opposite fixes.
func TestReportBuildNamesWhatItDidNotMove(t *testing.T) {
	for _, tc := range []struct {
		name    string
		env     []byte
		project string
		want    string
	}{
		{
			name: "pinned",
			env: trackingEnvDoc("staging", `  components:
    - name: worker
      image: ghcr.io/acme/checkout-worker:frozen
`),
			want: "environment staging did not move worker: an image pin holds it",
		},
		{
			name: "not tracking",
			env: trackingEnvDoc("staging", `  components:
    - name: worker
      autoDeploy: false
`),
			want: "environment staging did not move worker: it does not track its source here",
		},
		{
			name:    "bound elsewhere",
			project: strings.Replace(reportProjectDoc, "    ref: main\n", "    ref: release\n", 1),
			env:     trackingEnvDoc("staging", ""),
			want:    "environment staging did not move worker: it follows release, and this push is about main",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			project := tc.project
			if project == "" {
				project = reportProjectDoc
			}
			p := reportServerWith(t, project, map[string][]byte{"staging": tc.env})
			res := report(t, p.clients, trackingReport(map[string]string{
				"web":    webImage(),
				"worker": workerImage(),
			}))
			if !strings.Contains(res.GetMessage(), tc.want) {
				t.Errorf("the message does not say why worker held still\n  want: %s\n  got:  %s",
					tc.want, res.GetMessage())
			}
			if strings.Contains(storedEnvironment(t, p.specs, "checkout", "staging"), workerImage()) {
				t.Error("a component that was not stale was deployed anyway")
			}
		})
	}
}

// A component the report does not name keeps whatever the spec resolves for it,
// and a stale component with no reported image is named rather than re-pinned to
// what it already runs.
func TestReportBuildNamesAStaleComponentWithNoImage(t *testing.T) {
	p := reportServerWith(t, reportProjectDoc, map[string][]byte{"staging": trackingEnvDoc("staging", "")})
	res := report(t, p.clients, trackingReport(map[string]string{"web": webImage()}))

	want := "environment staging did not move worker: it follows this push and the trigger carries no image for it"
	if !strings.Contains(res.GetMessage(), want) {
		t.Errorf("the message does not name the component it had no image for\n  want: %s\n  got:  %s",
			want, res.GetMessage())
	}
	if got := res.GetTriggered(); len(got) != 1 {
		t.Fatalf("triggered = %v, want the environment to still move for web", got)
	}
}

// --- statuses (ADR-0036 decision 4) -------------------------------------------

// An auto-deploy writes a commit status under its own context, and links the
// deepest route the UI actually serves for an environment.
func TestReportBuildWritesTheDeployStatus(t *testing.T) {
	p := reportServerWith(t, reportProjectDoc, map[string][]byte{"staging": trackingEnvDoc("staging", "")})
	report(t, p.clients, trackingReport(map[string]string{"web": webImage()}))

	statuses := p.outcomes.statuses()
	if len(statuses) != 1 {
		t.Fatalf("wrote %d statuses, want one per auto-deploy", len(statuses))
	}
	got := statuses[0]
	if got.Context != DeployStatusContext {
		t.Errorf("context = %q, want %q — a preview and a deploy must not overwrite each other's check",
			got.Context, DeployStatusContext)
	}
	if got.Context == PreviewStatusContext {
		t.Error("the deploy status shares the preview's context")
	}
	if got.State != "success" || got.SHA != reportSHA {
		t.Errorf("state=%q sha=%q, want a terminal success against the reported commit", got.State, got.SHA)
	}
	if want := "/projects/checkout/staging/history"; got.Path != want {
		t.Errorf("path = %q, want %q", got.Path, want)
	}
}

// A push no environment follows still writes a status, and it is green: a commit
// on a ref nothing tracks is not a broken pipeline, and a red check for it would
// train a team to ignore the green ones.
func TestTheDeployStatusIsGreenWhenNothingFollows(t *testing.T) {
	p := previewReportServer(t)
	report(t, p.clients, trackingReport(map[string]string{"web": webImage()}))

	statuses := p.outcomes.statuses()
	if len(statuses) != 1 || statuses[0].State != "success" {
		t.Fatalf("statuses = %+v, want one green status", statuses)
	}
	if !strings.Contains(statuses[0].Description, "main") {
		t.Errorf("description = %q, want it to name the ref nothing follows", statuses[0].Description)
	}
}

// Statuses degrade silently and never fail the trigger: ADR-0034 decision 5
// makes them "a courtesy of the integration, not a delivery dependency". A
// reporter that was *asked and failed* is different — something configured is
// broken — so it is named in the message and the deploy still stands.
func TestTheDeployStatusDegradesSilently(t *testing.T) {
	t.Run("no reporter wired", func(t *testing.T) {
		specs := newFakeSpecStore()
		if _, err := specs.Put(t.Context(), "checkout", controlstore.Documents{
			Project:      []byte(reportProjectDoc),
			Environments: map[string][]byte{"staging": trackingEnvDoc("staging", "")},
		}, controlstore.PutOptions{}); err != nil {
			t.Fatalf("storing the project: %v", err)
		}
		c := serve(t, Options{Specs: specs})
		res := report(t, c, trackingReport(map[string]string{"web": webImage()}))
		if got := res.GetTriggered(); len(got) != 1 {
			t.Fatalf("triggered = %v, want the deploy to happen without a reporter", got)
		}
		if strings.Contains(res.GetMessage(), "commit status") {
			t.Errorf("an absent reporter was reported: %s", res.GetMessage())
		}
	})

	t.Run("reporter fails", func(t *testing.T) {
		p := reportServerWith(t, reportProjectDoc, map[string][]byte{"staging": trackingEnvDoc("staging", "")})
		p.outcomes.err = errors.New("403 Forbidden writing a status to github.com/acme/checkout")
		res := report(t, p.clients, trackingReport(map[string]string{"web": webImage()}))
		if got := res.GetTriggered(); len(got) != 1 {
			t.Fatalf("triggered = %v, want a failed status not to undo the deploy", got)
		}
		if !strings.Contains(res.GetMessage(), "the commit status could not be written back") {
			t.Errorf("a broken reporter was swallowed: %s", res.GetMessage())
		}
	})
}

// --- the webhook path (ADR-0036 decision 3, second half) ----------------------

// kelsonProjectDoc is a project whose images come from kelson's own build plane
// — the `by: kelson` default for a project with a source — which is what a
// webhook push has to build before it can deploy anything.
const kelsonProjectDoc = `apiVersion: kelson.dev/v1alpha1
kind: Project
metadata: {name: checkout}
spec:
  components:
    - name: web
      kind: service
      port: 8080
  source:
    git: https://github.com/acme/checkout
    ref: main
  build:
    strategy: dockerfile
`

// twoSourceProjectDoc is the shape #252 refuses: two repositories, one build
// plane, one image per build.
const twoSourceProjectDoc = `apiVersion: kelson.dev/v1alpha1
kind: Project
metadata: {name: checkout}
spec:
  components:
    - name: web
      kind: service
      port: 8080
      source: app
    - name: worker
      kind: worker
      source: tools
  sources:
    - name: app
      git: https://github.com/acme/checkout
      ref: main
    - name: tools
      git: https://gitlab.com/acme/build-tools
      ref: main
  build:
    strategy: dockerfile
`

// trackingServer serves one stored project with the build plane wired, and
// returns the Server itself: the webhook path is not an RPC, so it is driven
// through the exported seam rather than through a client.
func trackingServer(t *testing.T, project string, envs map[string][]byte, opts ...func(*Options)) (*Server, *fakeSpecStore, *fakeBuilder) {
	t.Helper()
	specs := newFakeSpecStore()
	if _, err := specs.Put(t.Context(), "checkout", controlstore.Documents{
		Project: []byte(project), Environments: envs,
	}, controlstore.PutOptions{}); err != nil {
		t.Fatalf("storing the project: %v", err)
	}
	builder := &fakeBuilder{result: build.Result{
		Reference: webImage(),
		Digest:    "sha256:" + strings.Repeat("a", 64),
	}}
	o := Options{
		Specs: specs,
		Build: buildPlaneFor(builder, &fakeRevisions{}, nil),
		BuildDefaults: BuildDefaults{
			Registry:   "ghcr.io/acme",
			PushSecret: "ghcr-push",
		},
	}
	for _, adjust := range opts {
		adjust(&o)
	}
	return New(o), specs, builder
}

// pushOf is the trigger a verified delivery produces: the repository, the ref
// as the forge spelled it, and the pushed head.
func pushOf(ref string) PushTrigger {
	return PushTrigger{
		Project:    "checkout",
		Repo:       "https://github.com/acme/checkout",
		Ref:        ref,
		SHA:        reportSHA,
		Connection: "acme-github",
	}
}

// A push to a kelson-built project runs the build plane at the pushed head and
// then moves the environments that follow it — with the image that build
// produced, which is the whole difference between this path and a CI report.
func TestPushBuildsThenMoves(t *testing.T) {
	server, specs, builder := trackingServer(t, kelsonProjectDoc,
		map[string][]byte{"staging": trackingEnvDoc("staging", "")})

	out, err := server.AutoDeployPush(t.Context(), pushOf("refs/heads/main"))
	if err != nil {
		t.Fatalf("AutoDeployPush: %v", err)
	}
	if len(out.Triggered) != 1 || out.Triggered[0] != "staging" {
		t.Fatalf("triggered = %v (notes %v), want the tracking environment", out.Triggered, out.Notes)
	}
	req := builder.lastRequest(t)
	if req.SourceRef != reportSHA || req.Revision != reportSHA {
		t.Errorf("built %s, want the commit the push carried", req.SourceRef)
	}
	if req.SourceGit != "https://github.com/acme/checkout" {
		t.Errorf("cloned %s, want the bound source", req.SourceGit)
	}
	if got := storedEnvironment(t, specs, "checkout", "staging"); !strings.Contains(got, webImage()) {
		t.Errorf("the built image did not reach the environment document:\n%s", got)
	}
}

// A push to a ref nothing follows never reaches the build plane. Building on
// every push to every branch is the cost this ordering exists to avoid.
func TestPushAtAnUnfollowedRefBuildsNothing(t *testing.T) {
	server, _, builder := trackingServer(t, kelsonProjectDoc,
		map[string][]byte{"staging": trackingEnvDoc("staging", "")})

	out, err := server.AutoDeployPush(t.Context(), pushOf("refs/heads/spike"))
	if err != nil {
		t.Fatalf("AutoDeployPush: %v", err)
	}
	if len(out.Triggered) != 0 {
		t.Errorf("triggered = %v for a ref nothing follows", out.Triggered)
	}
	if builder.calls() != 0 {
		t.Errorf("the build plane ran %d times for a push nothing follows", builder.calls())
	}
}

// The multi-source refusal (#252): named in full, before anything is built, and
// carried out of both halves of the seam so the delivery's answer can say it.
func TestPushRefusesAMultiSourceKelsonProject(t *testing.T) {
	server, _, builder := trackingServer(t, twoSourceProjectDoc,
		map[string][]byte{"staging": trackingEnvDoc("staging", "")})

	plan, err := server.PlanPush(t.Context(), pushOf("refs/heads/main"))
	if err != nil {
		t.Fatalf("PlanPush: %v", err)
	}
	for _, want := range []string{"#252", "build/several-sources", "spec.build.by: ci"} {
		if !strings.Contains(plan.Refused, want) {
			t.Errorf("the refusal does not name %q: %s", want, plan.Refused)
		}
	}
	if plan.Moves() {
		t.Errorf("a refused project still planned %v", plan.Environments)
	}

	out, err := server.AutoDeployPush(t.Context(), pushOf("refs/heads/main"))
	if err != nil {
		t.Fatalf("AutoDeployPush: %v", err)
	}
	if out.Refused == "" || len(out.Triggered) != 0 {
		t.Errorf("outcome = %+v, want the refusal and nothing moved", out)
	}
	if builder.calls() != 0 {
		t.Error("a refused project was built anyway")
	}
}

// PlanPush is the half a forge waits on, so it must decide and write nothing:
// no build, no spec write.
func TestPlanPushWritesNothing(t *testing.T) {
	server, specs, builder := trackingServer(t, kelsonProjectDoc,
		map[string][]byte{"staging": trackingEnvDoc("staging", "")})
	before := storedEnvironment(t, specs, "checkout", "staging")

	plan, err := server.PlanPush(t.Context(), pushOf("refs/heads/main"))
	if err != nil {
		t.Fatalf("PlanPush: %v", err)
	}
	if got := plan.Environments["staging"]; len(got) != 1 || got[0] != "web" {
		t.Errorf("plan = %v, want the one component the push makes stale", plan.Environments)
	}
	if builder.calls() != 0 {
		t.Error("planning built something")
	}
	if after := storedEnvironment(t, specs, "checkout", "staging"); after != before {
		t.Error("planning wrote to the spec store")
	}
}

// --- audit --------------------------------------------------------------------

// A report is a mutation, so it leaves a record naming the principal that sent
// it — the agent identity a pipeline authenticates as (ADR-0034 decision 6,
// ADR-0036 decision 4's "the reporting agent identity for ReportBuild"). The
// change it records is the environments it set in motion, against the commit.
func TestAutoDeployRecordsTheReportingAgent(t *testing.T) {
	sink := newFakeAuditSink()
	specs := newFakeSpecStore()
	if _, err := specs.Put(t.Context(), "checkout", controlstore.Documents{
		Project:      []byte(reportProjectDoc),
		Environments: map[string][]byte{"staging": trackingEnvDoc("staging", "")},
	}, controlstore.PutOptions{}); err != nil {
		t.Fatalf("storing the project: %v", err)
	}
	g := newGatedServer(t, Options{Specs: specs, Audit: sink})
	token := g.mint(t, "ci", controlstore.Scope{Operations: []controlstore.Operation{controlstore.OpMutate}})

	if _, err := g.as(token).builds.ReportBuild(t.Context(),
		connect.NewRequest(trackingReport(map[string]string{"web": webImage()}))); err != nil {
		t.Fatalf("ReportBuild: %v", err)
	}

	rec := sink.find(t, "/kelson.v1alpha1.BuildService/ReportBuild")
	if rec.Principal.Type != string(PrincipalAgent) || rec.Principal.Name != "ci" {
		t.Errorf("principal = %s, want the reporting agent", rec.Principal.String())
	}
	if rec.Target.Project != "checkout" {
		t.Errorf("target = %s, want the project the report named", rec.Target.String())
	}
	if rec.Change == nil || rec.Change.From != reportSHA || rec.Change.Revision != "staging" {
		t.Errorf("change = %+v, want the environments moved and the commit they moved to", rec.Change)
	}
}

// A webhook push is the other principal shape, and it is the one ADR-0036
// decision 4 is careful about: the connection whose secret verified the
// delivery, as a system principal, never an invented human.
//
// It also proves the record exists at all. This path reaches no ConnectRPC
// interceptor — a delivery arrives at internal/forgehttp, outside the handlers
// and outside the credential gate — so without an explicit write the one
// mutation a push causes would leave no trail.
func TestAutoDeployRecordsTheConnectionAsASystemPrincipal(t *testing.T) {
	sink := newFakeAuditSink()
	server, _, _ := trackingServer(t, kelsonProjectDoc,
		map[string][]byte{"staging": trackingEnvDoc("staging", "")},
		func(o *Options) { o.Audit = sink })

	if _, err := server.AutoDeployPush(t.Context(), pushOf("refs/heads/main")); err != nil {
		t.Fatalf("AutoDeployPush: %v", err)
	}

	rec := sink.find(t, "forge/webhook/push")
	if rec.Principal.Type != string(PrincipalSystem) {
		t.Errorf("principal type = %q, want %q", rec.Principal.Type, PrincipalSystem)
	}
	if rec.Principal.Type == string(PrincipalHuman) || rec.Principal.Type == string(PrincipalAgent) {
		t.Error("a webhook push was recorded as somebody who was never there")
	}
	if rec.Principal.Name != "acme-github" {
		t.Errorf("principal name = %q, want the connection that verified the delivery", rec.Principal.Name)
	}
	if rec.Operation != controlstore.OpMutate || rec.Outcome != controlstore.AuditAllowed {
		t.Errorf("operation=%s outcome=%s, want an allowed mutation", rec.Operation, rec.Outcome)
	}
	if rec.Target.Project != "checkout" {
		t.Errorf("target = %s, want the project the push moved", rec.Target.String())
	}
	if rec.Change == nil || rec.Change.Revision != "staging" || rec.Change.From != reportSHA {
		t.Errorf("change = %+v, want what the push set in motion", rec.Change)
	}
	if rec.Scope != "" {
		t.Errorf("scope = %q, want none — a connection has no scope to record", rec.Scope)
	}
}

// A push that moved nothing writes no record, which is the rule every other
// path already keeps: an allowed read changed nothing, and a webhook that
// resolved an empty stale set is a read.
func TestAutoDeployRecordsNothingForAPushThatMovedNothing(t *testing.T) {
	sink := newFakeAuditSink()
	server, _, _ := trackingServer(t, kelsonProjectDoc,
		map[string][]byte{"staging": trackingEnvDoc("staging", "")},
		func(o *Options) { o.Audit = sink })

	if _, err := server.AutoDeployPush(t.Context(), pushOf("refs/heads/spike")); err != nil {
		t.Fatalf("AutoDeployPush: %v", err)
	}
	if got := sink.all(); len(got) != 0 {
		t.Errorf("a push that moved nothing wrote %d records", len(got))
	}
}

// And a refused push is recorded as refused, with the code the taxonomy uses —
// the one durable trace of a `build/several-sources` push, since the
// environment's conditions are the controller's alone.
func TestAutoDeployRecordsARefusedPush(t *testing.T) {
	sink := newFakeAuditSink()
	server, _, _ := trackingServer(t, twoSourceProjectDoc,
		map[string][]byte{"staging": trackingEnvDoc("staging", "")},
		func(o *Options) { o.Audit = sink })

	if _, err := server.AutoDeployPush(t.Context(), pushOf("refs/heads/main")); err != nil {
		t.Fatalf("AutoDeployPush: %v", err)
	}
	rec := sink.find(t, "forge/webhook/push")
	if rec.Outcome != controlstore.AuditRefused || rec.Code != "build/several-sources" {
		t.Errorf("outcome=%s code=%s, want the refusal recorded as one", rec.Outcome, rec.Code)
	}
}
