package api

import (
	"context"
	"strings"
	"testing"

	"connectrpc.com/connect"

	kelsonv1alpha1 "github.com/dafrie/kelson/internal/api/gen/kelson/v1alpha1"
	"github.com/dafrie/kelson/internal/controlstore"
)

// TestSpecPutGetRoundTrip: the store keeps the user's document, byte-faithful
// (ADR-0013 §1). A spec that came back changed would mean the server owns a
// second version of a document whose home is the user's file.
func TestSpecPutGetRoundTrip(t *testing.T) {
	c := serve(t, Options{Specs: newFakeSpecStore()})
	ctx := context.Background()

	put, err := c.spec.PutSpec(ctx, connect.NewRequest(&kelsonv1alpha1.PutSpecRequest{
		Documents: specDocuments(projectDoc, map[string]string{"development": developmentDoc}),
	}))
	if err != nil {
		t.Fatalf("PutSpec: %v", err)
	}
	if errs := put.Msg.GetErrors(); len(errs) > 0 {
		t.Fatalf("a valid spec reported errors: %v", errs)
	}
	if put.Msg.GetSpec().GetProject() != "hello" {
		t.Errorf("project = %q, want hello", put.Msg.GetSpec().GetProject())
	}
	if put.Msg.GetSpec().GetVersion() == "" {
		t.Error("PutSpec returned no version to pass back on the next write")
	}

	got, err := c.spec.GetSpec(ctx, connect.NewRequest(&kelsonv1alpha1.GetSpecRequest{Project: "hello"}))
	if err != nil {
		t.Fatalf("GetSpec: %v", err)
	}
	docs := got.Msg.GetSpec().GetDocuments()
	if string(docs.GetProject()) != projectDoc {
		t.Errorf("project document did not round-trip:\n%s", docs.GetProject())
	}
	if string(docs.GetEnvironments()["development"]) != developmentDoc {
		t.Errorf("environment document did not round-trip:\n%s", docs.GetEnvironments()["development"])
	}
	if envs := got.Msg.GetSpec().GetEnvironments(); len(envs) != 1 || envs[0] != "development" {
		t.Errorf("environments = %v, want [development]", envs)
	}

	list, err := c.spec.ListSpecs(ctx, connect.NewRequest(&kelsonv1alpha1.ListSpecsRequest{}))
	if err != nil {
		t.Fatalf("ListSpecs: %v", err)
	}
	if len(list.Msg.GetSpecs()) != 1 {
		t.Fatalf("ListSpecs returned %d specs, want 1", len(list.Msg.GetSpecs()))
	}
	if list.Msg.GetSpecs()[0].GetDocuments() != nil {
		t.Error("ListSpecs returned documents; a listing is not every spec's full text")
	}
}

// TestSpecPutVersionConflict: optimistic concurrency is the whole point of the
// version token. A write with no version against an existing project, and a
// write with a stale one, are both store/version-conflict → FailedPrecondition.
func TestSpecPutVersionConflict(t *testing.T) {
	c := serve(t, Options{Specs: newFakeSpecStore()})
	ctx := context.Background()

	docs := specDocuments(projectDoc, map[string]string{"development": developmentDoc})
	first, err := c.spec.PutSpec(ctx, connect.NewRequest(&kelsonv1alpha1.PutSpecRequest{Documents: docs}))
	if err != nil {
		t.Fatalf("PutSpec: %v", err)
	}

	_, err = c.spec.PutSpec(ctx, connect.NewRequest(&kelsonv1alpha1.PutSpecRequest{Documents: docs}))
	if connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Fatalf("blind overwrite: code = %v, want FailedPrecondition (err %v)", connect.CodeOf(err), err)
	}
	if got := detailCode(t, err); got != string(controlstore.ErrVersionConflict) {
		t.Errorf("detail code = %q, want %q", got, controlstore.ErrVersionConflict)
	}

	_, err = c.spec.PutSpec(ctx, connect.NewRequest(&kelsonv1alpha1.PutSpecRequest{Documents: docs, Version: "stale"}))
	if connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Fatalf("stale version: code = %v, want FailedPrecondition", connect.CodeOf(err))
	}

	// The version the last read returned is the one that succeeds.
	second, err := c.spec.PutSpec(ctx, connect.NewRequest(&kelsonv1alpha1.PutSpecRequest{
		Documents: docs,
		Version:   first.Msg.GetSpec().GetVersion(),
	}))
	if err != nil {
		t.Fatalf("PutSpec with the current version: %v", err)
	}
	if second.Msg.GetSpec().GetVersion() == first.Msg.GetSpec().GetVersion() {
		t.Error("a successful write did not advance the version")
	}

	// force is the deliberate escape hatch, never a default.
	if _, err := c.spec.PutSpec(ctx, connect.NewRequest(&kelsonv1alpha1.PutSpecRequest{Documents: docs, Force: true})); err != nil {
		t.Fatalf("forced overwrite: %v", err)
	}
}

// TestSpecPutIdempotency: a replayed key returns the recorded outcome and
// writes nothing, which is what makes a retried request safe (ADR-0013 §2).
func TestSpecPutIdempotency(t *testing.T) {
	c := serve(t, Options{Specs: newFakeSpecStore()})
	ctx := context.Background()

	docs := specDocuments(projectDoc, map[string]string{"development": developmentDoc})
	req := &kelsonv1alpha1.PutSpecRequest{Documents: docs, IdempotencyKey: "req-1"}
	first, err := c.spec.PutSpec(ctx, connect.NewRequest(req))
	if err != nil {
		t.Fatalf("PutSpec: %v", err)
	}
	replay, err := c.spec.PutSpec(ctx, connect.NewRequest(req))
	if err != nil {
		t.Fatalf("replayed PutSpec should succeed, not conflict: %v", err)
	}
	if got, want := replay.Msg.GetSpec().GetVersion(), first.Msg.GetSpec().GetVersion(); got != want {
		t.Errorf("replay version = %q, want the recorded %q (the replay wrote)", got, want)
	}
}

// TestSpecPutInvalidStoresNothing: an invalid spec is an answer. The RPC
// succeeds, the findings come back, and the store is left untouched.
func TestSpecPutInvalidStoresNothing(t *testing.T) {
	store := newFakeSpecStore()
	c := serve(t, Options{Specs: store})
	ctx := context.Background()

	broken := strings.Replace(projectDoc, "      health: /healthz", "      healthz: /healthz", 1)
	res, err := c.spec.PutSpec(ctx, connect.NewRequest(&kelsonv1alpha1.PutSpecRequest{
		Documents: specDocuments(broken, map[string]string{"development": developmentDoc}),
	}))
	if err != nil {
		t.Fatalf("PutSpec should answer, not fail: %v", err)
	}
	if len(res.Msg.GetErrors()) == 0 {
		t.Fatal("an invalid spec reported no errors")
	}
	if _, err := store.Get(ctx, "hello"); err == nil {
		t.Error("an invalid spec was stored")
	}
}

// TestSpecPutDryRunRender renders every environment and stores nothing — the
// extra fidelity of the RENDER rung over plain validation.
func TestSpecPutDryRunRender(t *testing.T) {
	store := newFakeSpecStore()
	c := serve(t, Options{Specs: store})
	ctx := context.Background()

	res, err := c.spec.PutSpec(ctx, connect.NewRequest(&kelsonv1alpha1.PutSpecRequest{
		Documents: specDocuments(projectDoc, map[string]string{"development": developmentDoc}),
		DryRun:    kelsonv1alpha1.DryRun_DRY_RUN_RENDER,
	}))
	if err != nil {
		t.Fatalf("PutSpec: %v", err)
	}
	// The spec validates but cannot render against the zero profile: its
	// service declares a domain and there is no Gateway API to attach it to
	// (#140). That is exactly the answer a dry run is asked for.
	if len(res.Msg.GetErrors()) != 1 || res.Msg.GetErrors()[0].GetCode() != "render/gateway-api-missing" {
		t.Fatalf("errors = %v, want one render/gateway-api-missing", res.Msg.GetErrors())
	}
	if _, err := store.Get(ctx, "hello"); err == nil {
		t.Error("a dry run stored the spec")
	}
}

// TestSpecDelete covers the delete half of the same optimistic-concurrency
// contract, and the not-found mapping.
func TestSpecDelete(t *testing.T) {
	c := serve(t, Options{Specs: newFakeSpecStore()})
	ctx := context.Background()

	put, err := c.spec.PutSpec(ctx, connect.NewRequest(&kelsonv1alpha1.PutSpecRequest{
		Documents: specDocuments(projectDoc, map[string]string{"development": developmentDoc}),
	}))
	if err != nil {
		t.Fatalf("PutSpec: %v", err)
	}

	_, err = c.spec.DeleteSpec(ctx, connect.NewRequest(&kelsonv1alpha1.DeleteSpecRequest{Project: "hello", Version: "stale"}))
	if connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Fatalf("stale delete: code = %v, want FailedPrecondition", connect.CodeOf(err))
	}
	if _, err := c.spec.DeleteSpec(ctx, connect.NewRequest(&kelsonv1alpha1.DeleteSpecRequest{
		Project: "hello",
		Version: put.Msg.GetSpec().GetVersion(),
	})); err != nil {
		t.Fatalf("DeleteSpec: %v", err)
	}

	_, err = c.spec.GetSpec(ctx, connect.NewRequest(&kelsonv1alpha1.GetSpecRequest{Project: "hello"}))
	if connect.CodeOf(err) != connect.CodeNotFound {
		t.Fatalf("code = %v, want NotFound", connect.CodeOf(err))
	}
}

// TestSpecStoreUnwired: a server started without a store says so rather than
// panicking.
func TestSpecStoreUnwired(t *testing.T) {
	c := serve(t, Options{})
	_, err := c.spec.GetSpec(context.Background(), connect.NewRequest(&kelsonv1alpha1.GetSpecRequest{Project: "hello"}))
	if connect.CodeOf(err) != connect.CodeUnimplemented {
		t.Fatalf("code = %v, want Unimplemented (err %v)", connect.CodeOf(err), err)
	}
}
