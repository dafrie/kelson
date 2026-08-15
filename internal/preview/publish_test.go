package preview_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/dafrie/kelson/internal/build/registry"
	"github.com/dafrie/kelson/internal/model"
	"github.com/dafrie/kelson/internal/preview"
)

// [preview.Publisher] is the composition ADR-0017 decision 8 deferred and
// ADR-0034 decision 3 called in: render, package, resolve a credential, push.
// Every step it composes is tested where it lives, so what these tests hold it
// to is the composition — the order, what the pusher is handed, which
// credential it is built with, and the two properties a *server-side* caller
// depends on that a CLI one never had to: that a replay publishes the same
// bytes, and that a refusal keeps the vocabulary of the step that refused.

// fakePusher records what it was asked to upload and with what.
type fakePusher struct {
	mu       sync.Mutex
	pushed   []preview.Artifact
	creds    []registry.Credential
	insecure []bool
	err      error
}

// pusherFor is the [preview.PusherFor] seam over this fake: it captures the
// credential the publisher resolved, which is otherwise invisible from outside.
func (f *fakePusher) pusherFor(cred registry.Credential, insecure bool) preview.ArtifactPusher {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.creds = append(f.creds, cred)
	f.insecure = append(f.insecure, insecure)
	return f
}

func (f *fakePusher) Push(_ context.Context, a preview.Artifact) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return "", f.err
	}
	f.pushed = append(f.pushed, a)
	return a.Repository + "@" + a.Digest, nil
}

func (f *fakePusher) last(t *testing.T) preview.Artifact {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.pushed) == 0 {
		t.Fatal("nothing was pushed")
	}
	return f.pushed[len(f.pushed)-1]
}

func testPublisher(f *fakePusher) *preview.Publisher {
	// A docker config path that does not exist is the anonymous case, which is
	// what keeps this test off the filesystem as well as off the network.
	return &preview.Publisher{RegistryConfig: "/nonexistent/kelson-test/config.json", Pusher: f.pusherFor}
}

func mustPublish(t *testing.T, p *preview.Publisher, opts preview.Options) *preview.Published {
	t.Helper()
	out, err := p.Publish(t.Context(), opts)
	if err != nil {
		t.Fatalf("Publish: %v", err)
	}
	return out
}

// The publish lands where the rendered ResourceSet will look for it: the
// artifact repository the environment declares, under the head commit
// (ADR-0017 decisions 1 and 2).
func TestPublishPushesToTheDeclaredRepositoryAtTheHeadCommit(t *testing.T) {
	f := &fakePusher{}
	out := mustPublish(t, testPublisher(f), testOptions())

	a := f.last(t)
	if a.Repository != "ghcr.io/acme/checkout-previews" {
		t.Errorf("pushed to %q, want the environment's artifacts.repository with oci:// stripped", a.Repository)
	}
	if a.Tag != testSHA {
		t.Errorf("pushed under tag %q, want the head commit %s", a.Tag, testSHA)
	}
	if out.Reference != a.Repository+"@"+a.Digest {
		t.Errorf("Reference = %q, want the digest-pinned reference the registry serves", out.Reference)
	}
	if out.Set == nil || out.Set.Namespace != "checkout-staging-pr412" {
		t.Errorf("the publish did not report the preview it rendered: %+v", out.Set)
	}
}

// The reported images are what the preview runs. This is the whole point of the
// CI hand-off: CI says which image exists for this commit, and the preview that
// did not run it would be previewing the commit the environment is already on.
func TestPublishRunsTheReportedImages(t *testing.T) {
	f := &fakePusher{}
	opts := testOptions()
	opts.Project.Spec.Components = append(opts.Project.Spec.Components, model.Component{Name: "worker", Kind: model.ComponentWorker})
	reported := map[string]string{
		"web":    "ghcr.io/acme/checkout-web@sha256:" + strings.Repeat("b", 64),
		"worker": "ghcr.io/acme/checkout-worker@sha256:" + strings.Repeat("c", 64),
	}
	opts.Images = reported

	mustPublish(t, testPublisher(f), opts)
	body := publishedBody(t, f.last(t))
	for component, ref := range reported {
		if !strings.Contains(body, ref) {
			t.Errorf("the published set does not run %s for %s", ref, component)
		}
	}
}

// A reported image outranks the environment's own pin, which is the one place
// a preview inverts the spec's ordinary precedence — and the reason is that a
// preview running production's pin previews nothing. See Options.Images.
func TestPublishedReportedImageOutranksTheEnvironmentPin(t *testing.T) {
	f := &fakePusher{}
	opts := testOptions()
	pinned := "ghcr.io/acme/checkout@sha256:" + strings.Repeat("d", 64)
	built := "ghcr.io/acme/checkout@sha256:" + strings.Repeat("e", 64)
	opts.Environment.Spec.Components = []model.ComponentOverride{{Name: "web", Image: pinned}}
	opts.Images = map[string]string{"web": built}

	mustPublish(t, testPublisher(f), opts)
	body := publishedBody(t, f.last(t))
	if strings.Contains(body, pinned) {
		t.Error("the preview runs the environment's pin, so it previews the commit the environment is already on")
	}
	if !strings.Contains(body, built) {
		t.Errorf("the preview does not run the reported image %s", built)
	}
}

// A component nobody reported keeps whatever the spec resolves for it: "a
// partial report is a partial pin rather than a broken render".
func TestPublishLeavesUnreportedComponentsAlone(t *testing.T) {
	f := &fakePusher{}
	opts := testOptions()
	opts.Project.Spec.Components = append(opts.Project.Spec.Components, model.Component{Name: "worker", Kind: model.ComponentWorker})
	opts.Images = map[string]string{"web": "ghcr.io/acme/checkout-web@sha256:" + strings.Repeat("b", 64)}

	mustPublish(t, testPublisher(f), opts)
	if body := publishedBody(t, f.last(t)); !strings.Contains(body, "ghcr.io/acme/checkout:1.4.2") {
		t.Error("the unreported component lost the project's image instead of keeping it")
	}
}

// The publisher must not edit the caller's documents: a server publishes one
// report into several environments out of one decoded spec, and the second
// publish would otherwise inherit the first one's pins.
func TestPublishDoesNotMutateTheAuthoredDocuments(t *testing.T) {
	f := &fakePusher{}
	opts := testOptions()
	opts.Environment.Spec.Components = []model.ComponentOverride{{Name: "web", Image: "ghcr.io/acme/checkout:pinned"}}
	opts.Images = map[string]string{"web": "ghcr.io/acme/checkout@sha256:" + strings.Repeat("e", 64)}

	mustPublish(t, testPublisher(f), opts)
	if got := opts.Environment.Spec.Components[0].Image; got != "ghcr.io/acme/checkout:pinned" {
		t.Errorf("the environment's own pin is now %q; the publish edited the caller's document", got)
	}
	if opts.Environment.Spec.Previews == nil {
		t.Error("the authored previews block was dropped from the caller's document")
	}
}

// Idempotency, and the shape of it that matters: a replayed report re-derives
// byte-identical blobs, so the registry is asked to store what it already has
// rather than a second artifact. ADR-0017 decision 10 is what makes this true —
// there is deliberately no dedupe cache here that could disagree with the
// registry about it.
func TestRepublishingTheSameReportIsTheSameArtifact(t *testing.T) {
	f := &fakePusher{}
	p := testPublisher(f)
	opts := testOptions()
	opts.Images = map[string]string{"web": "ghcr.io/acme/checkout-web@sha256:" + strings.Repeat("b", 64)}

	first := mustPublish(t, p, opts)
	second := mustPublish(t, p, testOptionsWith(opts.Images))
	if first.Reference != second.Reference {
		t.Fatalf("a replayed report published a different artifact:\n %s\n %s", first.Reference, second.Reference)
	}
	if a, b := f.pushed[0], f.pushed[1]; a.Digest != b.Digest || !bytesEqual(a.Layer, b.Layer) {
		t.Error("the two publishes differ in bytes, so a replay would upload a second artifact")
	}
}

// …and the converse, stated as plainly: two reports of one commit with
// different images are not a replay. The second wins, and the digest says so.
func TestRepublishingWithDifferentImagesIsANewArtifact(t *testing.T) {
	f := &fakePusher{}
	p := testPublisher(f)

	first := mustPublish(t, p, testOptionsWith(map[string]string{"web": "ghcr.io/acme/checkout-web@sha256:" + strings.Repeat("b", 64)}))
	second := mustPublish(t, p, testOptionsWith(map[string]string{"web": "ghcr.io/acme/checkout-web@sha256:" + strings.Repeat("c", 64)}))
	if first.Reference == second.Reference {
		t.Fatal("a rebuilt image at the same commit published the same artifact, so the preview would not move")
	}
	if a, b := f.pushed[0], f.pushed[1]; a.Tag != b.Tag {
		t.Errorf("the two publishes used different tags (%s, %s); both are the same commit", a.Tag, b.Tag)
	}
}

// A refusal keeps the vocabulary of the step that refused. A caller branching on
// preview/requires-flux must read the same value here that it reads from
// `kelson preview publish`, and nothing may be pushed on the way to it.
func TestPublishRefusalsAreThePlanesOwn(t *testing.T) {
	f := &fakePusher{}
	opts := testOptions()
	opts.Environment.Spec.Delivery = &model.Delivery{Mode: model.DeliveryDirect}

	_, err := testPublisher(f).Publish(t.Context(), opts)
	var refusal preview.Error
	if !errors.As(err, &refusal) || refusal.Reason != preview.ReasonRequiresFlux {
		t.Fatalf("Publish err = %v, want %s", err, preview.ReasonRequiresFlux)
	}
	if len(f.pushed) != 0 {
		t.Error("a refused render still pushed something")
	}
}

// A push failure reaches the caller unwrapped, so the reason a registry gave is
// the reason a report's message carries.
func TestPublishReportsAPushFailure(t *testing.T) {
	denied := errors.New("403 Forbidden")
	f := &fakePusher{err: denied}
	if _, err := testPublisher(f).Publish(t.Context(), testOptions()); !errors.Is(err, denied) {
		t.Fatalf("Publish err = %v, want the registry's own refusal", err)
	}
}

// The insecure-registry list is operator configuration and reaches the pusher,
// which is what makes a kind cluster's localhost registry work without a flag
// on the report.
func TestPublishHonorsInsecureRegistries(t *testing.T) {
	f := &fakePusher{}
	opts := testOptions()
	opts.Environment.Spec.Previews.Artifacts.Repository = "oci://registry.internal:5000/acme/previews"
	p := testPublisher(f)
	p.InsecureRegistries = []string{"registry.internal:5000"}

	mustPublish(t, p, opts)
	if len(f.insecure) != 1 || !f.insecure[0] {
		t.Errorf("the pusher was built with insecure=%v, want true for a host on the list", f.insecure)
	}
}

// testOptionsWith is a fresh set of documents with reported images, so a second
// publish in one test starts from documents the first one never saw.
func testOptionsWith(images map[string]string) preview.Options {
	opts := testOptions()
	opts.Images = images
	return opts
}

// publishedBody is every manifest inside the pushed artifact, concatenated:
// what the preview actually runs, read out of the layer the registry received
// rather than out of the render it came from.
func publishedBody(t *testing.T, a preview.Artifact) string {
	t.Helper()
	var b strings.Builder
	for _, entry := range untar(t, a.Layer) {
		b.Write(entry.body)
	}
	return b.String()
}

func bytesEqual(a, b []byte) bool { return string(a) == string(b) }
