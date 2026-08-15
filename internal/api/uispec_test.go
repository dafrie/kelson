package api

import (
	"context"
	"testing"

	"connectrpc.com/connect"

	kelsonv1alpha1 "github.com/dafrie/kelson/internal/api/gen/kelson/v1alpha1"
	"github.com/dafrie/kelson/internal/renderer"
)

// The UI's create flow (#63) builds these two documents in the browser, from a
// name, an image and a port. Nothing in the TypeScript test suite can prove
// that the real model accepts them — the model is Go — so the same bytes are
// pinned on both sides and each side asserts what only it can.
//
// The TypeScript half is ui/src/spec/documents.test.ts, whose MINIMAL_PROJECT
// and MINIMAL_ENVIRONMENT constants are byte-identical to the two below; the
// builder that produces them is ui/src/spec/documents.ts. Editing either
// builder without editing the other fails here, which is the point: a UI that
// writes documents kelson rejects is worse than a UI that cannot write them.
const (
	uiProjectDoc = `apiVersion: kelson.dev/v1alpha1
kind: Project
metadata:
  name: hello

spec:
  image: ghcr.io/acme/hello:1.4.2

  components:
    - name: web
      port: 8080
`

	uiEnvironmentDoc = `apiVersion: kelson.dev/v1alpha1
kind: Environment
metadata:
  name: development

spec:
  project: hello
`
)

// The create flow's other source: a git repository kelson builds itself (#63).
// These bytes are SOURCE_PROJECT in ui/src/spec/documents.test.ts, under the
// same convention as the pair above.
//
// The strategy is written as `dockerfile` and is not a choice the form offers.
// That is not taste: BuildService refuses `auto` because detecting a strategy
// reads a source tree and a server has none (build.ReasonDetectionNeedsSource),
// so a form with one hard-coded value cannot write the one value the server
// cannot run. `buildpacks` now builds too (#49) and offering it is a UI change,
// not a build-plane one — until then the browser writes the strategy it knows
// works. TestUISourceSpecStoresAndBuilds asserts exactly that, which is what
// stops the form quietly offering an option the build plane would reject.
const uiSourceProjectDoc = `apiVersion: kelson.dev/v1alpha1
kind: Project
metadata:
  name: hello

spec:
  source:
    git: https://github.com/acme/hello
    ref: main
  build:
    strategy: dockerfile

  components:
    - name: web
      port: 8080
`

// The UI's edit flow (#65) rewrites a stored spec from a form: environment
// variables at both scopes, an image override, autoscaling bounds, a second
// component. The same fixture convention applies — these bytes are
// EDITED_PROJECT and EDITED_ENVIRONMENT in ui/src/spec/edit.test.ts, where the
// TypeScript half asserts that reading them into the form and writing them back
// out reproduces them exactly. Only Go can assert the other half: that this is
// a document kelson accepts.
//
// It declares no domains, like the create fixture and for the same reason: the
// zero profile has no Gateway API, so a declared domain is
// render/gateway-api-missing rather than a statement about the builder.
const (
	uiEditedProjectDoc = `apiVersion: kelson.dev/v1alpha1
kind: Project
metadata:
  name: hello

spec:
  image: ghcr.io/acme/hello:1.4.2

  env:
    LOG_LEVEL: info
    PORT: "3000"

  components:
    - name: web
      port: 8080
      health: /healthz
      replicas: { min: 2, max: 10 }
      env:
        ROLE: web

    - name: worker
      image: ghcr.io/acme/hello-worker:1.4.2
      replicas: { min: 1 }
`

	uiEditedEnvironmentDoc = `apiVersion: kelson.dev/v1alpha1
kind: Environment
metadata:
  name: development

spec:
  project: hello
  namespace: hello-sandbox
`
)

// The UI's previews form (ADR-0017 stage 3) writes this Environment: flux
// delivery, because previews render in no other mode, and the whole `previews:`
// block including both label lists in the flow styling the documentation shows.
//
// Same fixture convention as the pairs above — these bytes are
// PREVIEWS_ENVIRONMENT in ui/src/spec/edit.test.ts, where the TypeScript half
// asserts the form reads them and writes them back byte-identically. Only Go
// can assert the half that matters more: that a document the browser writes
// from a checkbox and eleven inputs is one the model validates and the renderer
// turns into a ResourceSetInputProvider and a ResourceSet.
const uiPreviewsEnvironmentDoc = `apiVersion: kelson.dev/v1alpha1
kind: Environment
metadata:
  name: staging

spec:
  project: hello
  namespace: hello-staging
  previews:
    provider: github
    repo: https://github.com/acme/hello
    secretRef: github-auth
    interval: 10m
    filter:
      labels: [deploy/preview]
      includeBranch: "^feat/.*"
      excludeBranch: "^wip/.*"
      limit: 5
    skip:
      labels: [deploy/preview-pause, "!ci/passed"]
    artifacts:
      repository: oci://ghcr.io/acme/hello-previews
      secretRef: ghcr-auth
`

// TestUIPreviewsSpecValidatesAndRenders is the previews form's preflight: the
// document the browser builds, through PutSpec at dry_run=RENDER.
//
// A clean answer means the block decodes, passes validation — the naming cap,
// the label grammar, the oci:// URL rule, the Secret-name-not-a-token rule —
// resolves with kelson's defaults, and renders. The rendered pair is asserted
// too, because "the spec is valid" and "previews were actually rendered" are
// different claims and the form promises the second.
func TestUIPreviewsSpecValidatesAndRenders(t *testing.T) {
	c := serve(t, Options{Specs: newFakeSpecStore()})

	res, err := c.spec.PutSpec(context.Background(), connect.NewRequest(&kelsonv1alpha1.PutSpecRequest{
		Documents: specDocuments(uiProjectDoc, map[string]string{"staging": uiPreviewsEnvironmentDoc}),
		DryRun:    kelsonv1alpha1.DryRun_DRY_RUN_RENDER,
	}))
	if err != nil {
		t.Fatalf("PutSpec: %v", err)
	}
	if errs := res.Msg.GetErrors(); len(errs) > 0 {
		for _, e := range errs {
			t.Errorf("the UI's previews spec was rejected: [%s] %s %s: %s", e.GetCode(), e.GetResource(), e.GetField(), e.GetMessage())
		}
		t.Fatalf("%d finding(s); the previews form and the model disagree", len(errs))
	}

	rendered, err := c.render.Render(context.Background(), connect.NewRequest(&kelsonv1alpha1.RenderRequest{
		Spec:        inlineSpec(uiProjectDoc, map[string]string{"staging": uiPreviewsEnvironmentDoc}),
		Environment: "staging",
	}))
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if errs := rendered.Msg.GetErrors(); len(errs) > 0 {
		t.Fatalf("Render reported errors: %v", errs)
	}
	kinds := map[string]string{}
	for _, m := range rendered.Msg.GetManifests() {
		kinds[m.GetKind()] = m.GetName()
	}
	if kinds["ResourceSetInputProvider"] != "hello-staging-previews" {
		t.Errorf("ResourceSetInputProvider = %q, want hello-staging-previews", kinds["ResourceSetInputProvider"])
	}
	if kinds["ResourceSet"] != "hello-staging-previews" {
		t.Errorf("ResourceSet = %q, want hello-staging-previews", kinds["ResourceSet"])
	}
}

// TestUIMinimalSpecValidatesAndRenders runs the create flow's own preflight —
// PutSpec at dry_run=RENDER — over the three-field documents the UI builds.
//
// It has to come back with no findings at all. RENDER is the strictest rung a
// spec store offers (internal/api/spec.go: validate, then render every
// environment against the zero profile), so a clean answer here means the
// browser-built document decodes, passes the full model taxonomy including the
// #141 not-implemented gates, resolves, and renders — which is exactly the
// promise the UI makes when it shows the user a preview and a Create button.
//
// The zero profile is what makes the three-field case the right fixture: it
// declares no domains, so it needs no Gateway API and renders against a
// cluster nothing is known about (contrast TestSpecPutDryRunRender, whose
// fixture does declare one and fails with render/gateway-api-missing).
func TestUIMinimalSpecValidatesAndRenders(t *testing.T) {
	store := newFakeSpecStore()
	c := serve(t, Options{Specs: store})
	ctx := context.Background()

	docs := specDocuments(uiProjectDoc, map[string]string{"development": uiEnvironmentDoc})
	res, err := c.spec.PutSpec(ctx, connect.NewRequest(&kelsonv1alpha1.PutSpecRequest{
		Documents: docs,
		DryRun:    kelsonv1alpha1.DryRun_DRY_RUN_RENDER,
	}))
	if err != nil {
		t.Fatalf("PutSpec: %v", err)
	}
	if errs := res.Msg.GetErrors(); len(errs) > 0 {
		for _, e := range errs {
			t.Errorf("the UI's minimal spec was rejected: [%s] %s %s: %s", e.GetCode(), e.GetResource(), e.GetField(), e.GetMessage())
		}
		t.Fatalf("%d finding(s); the UI builder and the model disagree", len(errs))
	}
	if got := res.Msg.GetSpec().GetProject(); got != "hello" {
		t.Errorf("project = %q, want hello (metadata.name is the store key)", got)
	}
	if envs := res.Msg.GetSpec().GetEnvironments(); len(envs) != 1 || envs[0] != "development" {
		t.Errorf("environments = %v, want [development]", envs)
	}
	if _, err := store.Get(ctx, "hello"); err == nil {
		t.Error("the preview stored the spec; dry_run=RENDER must store nothing")
	}

	// The second call is the flow's Create: the same bytes, no dry run, an
	// idempotency key. It is what makes the preview a promise rather than a
	// separate opinion about the document.
	created, err := c.spec.PutSpec(ctx, connect.NewRequest(&kelsonv1alpha1.PutSpecRequest{
		Documents:      docs,
		IdempotencyKey: "ui-create-1",
	}))
	if err != nil {
		t.Fatalf("PutSpec (create): %v", err)
	}
	if created.Msg.GetSpec().GetVersion() == "" {
		t.Error("a create returned no version")
	}

	// A second create of the same name is the duplicate the UI answers with
	// "a project with this name already exists": a blind write against an
	// existing project is store/version-conflict, not an overwrite.
	_, err = c.spec.PutSpec(ctx, connect.NewRequest(&kelsonv1alpha1.PutSpecRequest{Documents: docs}))
	if connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Fatalf("duplicate create: code = %v, want FailedPrecondition (err %v)", connect.CodeOf(err), err)
	}
}

// TestUIEditedSpecValidatesAndUpdates runs the edit flow (#65) over the
// documents its form builds: the preflight at dry_run=RENDER, then the real
// write carrying the version the create returned.
//
// The version is the point. An edit is not a create — the UI has read the spec,
// the user has changed it, and something else may have written in between — so
// the write echoes the version GetSpec gave it and the store refuses it if that
// is no longer current (ADR-0013 §1). Both halves are asserted here: the write
// with the right version succeeds, and the same write replayed against the now
// stale version is store/version-conflict, which is the distinct state the
// editor renders rather than an error panel.
func TestUIEditedSpecValidatesAndUpdates(t *testing.T) {
	store := newFakeSpecStore()
	c := serve(t, Options{Specs: store})
	ctx := context.Background()

	created, err := c.spec.PutSpec(ctx, connect.NewRequest(&kelsonv1alpha1.PutSpecRequest{
		Documents:      specDocuments(uiProjectDoc, map[string]string{"development": uiEnvironmentDoc}),
		IdempotencyKey: "ui-create-1",
	}))
	if err != nil {
		t.Fatalf("PutSpec (create): %v", err)
	}
	version := created.Msg.GetSpec().GetVersion()

	edited := specDocuments(uiEditedProjectDoc, map[string]string{"development": uiEditedEnvironmentDoc})
	res, err := c.spec.PutSpec(ctx, connect.NewRequest(&kelsonv1alpha1.PutSpecRequest{
		Documents: edited,
		DryRun:    kelsonv1alpha1.DryRun_DRY_RUN_RENDER,
	}))
	if err != nil {
		t.Fatalf("PutSpec (check): %v", err)
	}
	if errs := res.Msg.GetErrors(); len(errs) > 0 {
		for _, e := range errs {
			t.Errorf("the UI's edited spec was rejected: [%s] %s %s: %s", e.GetCode(), e.GetResource(), e.GetField(), e.GetMessage())
		}
		t.Fatalf("%d finding(s); the edit builder and the model disagree", len(errs))
	}

	updated, err := c.spec.PutSpec(ctx, connect.NewRequest(&kelsonv1alpha1.PutSpecRequest{
		Documents:      edited,
		Version:        version,
		IdempotencyKey: "ui-edit-1",
	}))
	if err != nil {
		t.Fatalf("PutSpec (save): %v", err)
	}
	if updated.Msg.GetSpec().GetVersion() == version {
		t.Error("the update returned the version it was written against")
	}

	// The store keeps what was authored, not a re-serialisation of it: the
	// editor shows those bytes back, and the round trip has to be exact.
	stored, err := store.Get(ctx, "hello")
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if got := string(stored.Documents.Project); got != uiEditedProjectDoc {
		t.Errorf("stored project document is not byte-faithful:\n%s", got)
	}
	if got := string(stored.Documents.Environments["development"]); got != uiEditedEnvironmentDoc {
		t.Errorf("stored environment document is not byte-faithful:\n%s", got)
	}

	// Someone else's write, from the editor's point of view: the version it
	// still holds is stale, and the store says so instead of overwriting.
	_, err = c.spec.PutSpec(ctx, connect.NewRequest(&kelsonv1alpha1.PutSpecRequest{
		Documents:      edited,
		Version:        version,
		IdempotencyKey: "ui-edit-2",
	}))
	if connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Fatalf("stale update: code = %v, want FailedPrecondition (err %v)", connect.CodeOf(err), err)
	}

	// …and force is the editor's labelled escape hatch, which must work.
	if _, err := c.spec.PutSpec(ctx, connect.NewRequest(&kelsonv1alpha1.PutSpecRequest{
		Documents:      edited,
		Version:        version,
		Force:          true,
		IdempotencyKey: "ui-edit-3",
	})); err != nil {
		t.Fatalf("forced update: %v", err)
	}
}

// TestUISourceSpecStoresAndBuilds is the create-from-git path's Go half.
//
// It asserts the three things only the Go side can:
//
//  1. the browser-built document validates, so PutSpec stores it — a source
//     spec is stored, not rendered, which is why a project with no image yet
//     can be created at all (internal/api/spec.go);
//  2. the preflight at dry_run=RENDER reports image/unresolved and nothing
//     else, which is the finding the UI shows as expected rather than as a
//     blocker (splitFindings in ui/src/spec/documents.ts). If the model ever
//     rejected these bytes for a second reason, the UI would be suppressing a
//     real problem, and this is what catches that;
//  3. BuildService accepts the spec and gets as far as a real build — the
//     strategy the form writes is one the server can actually run.
func TestUISourceSpecStoresAndBuilds(t *testing.T) {
	store := newFakeSpecStore()
	builder := &fakeBuilder{logs: "#1 [internal] load build definition\n"}
	c := serve(t, Options{
		Specs:         store,
		Build:         buildPlaneFor(builder, &fakeRevisions{}, nil),
		BuildDefaults: BuildDefaults{Registry: "ghcr.io/acme"},
	})
	ctx := context.Background()

	docs := specDocuments(uiSourceProjectDoc, map[string]string{"development": uiEnvironmentDoc})
	check, err := c.spec.PutSpec(ctx, connect.NewRequest(&kelsonv1alpha1.PutSpecRequest{
		Documents: docs,
		DryRun:    kelsonv1alpha1.DryRun_DRY_RUN_RENDER,
	}))
	if err != nil {
		t.Fatalf("PutSpec (check): %v", err)
	}
	errs := check.Msg.GetErrors()
	if len(errs) != 1 || errs[0].GetCode() != renderer.ErrImageUnresolved {
		for _, e := range errs {
			t.Errorf("finding: [%s] %s %s: %s", e.GetCode(), e.GetResource(), e.GetField(), e.GetMessage())
		}
		t.Fatalf("want exactly one %s finding, got %d; the UI would be hiding the rest",
			renderer.ErrImageUnresolved, len(errs))
	}

	if _, err := c.spec.PutSpec(ctx, connect.NewRequest(&kelsonv1alpha1.PutSpecRequest{
		Documents:      docs,
		IdempotencyKey: "ui-create-source-1",
	})); err != nil {
		t.Fatalf("a spec that cannot render yet must still store: %v", err)
	}

	// The stored spec is what the build screen names, by project — the browser
	// does not re-send the documents it just stored.
	got, err := collectBuild(t, c, &kelsonv1alpha1.BuildRequest{
		Spec:        &kelsonv1alpha1.SpecRef{Spec: &kelsonv1alpha1.SpecRef_Project{Project: "hello"}},
		Environment: "development",
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if got.started.GetStrategy() != "dockerfile" {
		t.Errorf("strategy = %q, want the one the form writes", got.started.GetStrategy())
	}
	if got.finished.GetReference() == "" {
		t.Error("the build produced no reference for the deploy handoff")
	}
	if req := builder.lastRequest(t); req.SourceGit != "https://github.com/acme/hello" {
		t.Errorf("the builder got source %q", req.SourceGit)
	}
}
