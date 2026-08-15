package api

import (
	"errors"
	"strings"
	"testing"

	"connectrpc.com/connect"

	kelsonv1alpha1 "github.com/dafrie/kelson/internal/api/gen/kelson/v1alpha1"
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
