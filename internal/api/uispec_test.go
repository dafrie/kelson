package api

import (
	"context"
	"testing"

	"connectrpc.com/connect"

	kelsonv1alpha1 "github.com/dafrie/kelson/internal/api/gen/kelson/v1alpha1"
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

  applications:
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
