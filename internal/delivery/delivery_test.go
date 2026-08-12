package delivery

import (
	"context"
	"errors"
	"testing"
)

// stubAdapter exercises the interface contract without a cluster or git repo.
type stubAdapter struct {
	name         string
	capabilities Capabilities
}

func (s stubAdapter) Name() string { return s.name }

func (s stubAdapter) Capabilities() Capabilities { return s.capabilities }

func (s stubAdapter) Apply(context.Context, ManifestSet) (Result, error) {
	return Result{Revision: "stub", Applied: true}, nil
}

func (s stubAdapter) Status(context.Context, ManifestSet) (Status, error) {
	return Status{Phase: PhaseHealthy, Revision: "stub"}, nil
}

func (s stubAdapter) History(context.Context, ManifestSet) ([]Entry, error) {
	return nil, nil
}

func (s stubAdapter) Rollback(context.Context, ManifestSet, Entry) (Result, error) {
	return Result{Revision: "stub", Applied: true}, nil
}

func TestRegistrySelectAndNegotiate(t *testing.T) {
	r := NewRegistry()
	if err := r.Register(stubAdapter{name: "direct"}); err != nil {
		t.Fatal(err)
	}
	if err := r.Register(stubAdapter{name: "direct"}); err == nil {
		t.Fatal("expected duplicate-name registration to fail")
	}
	if err := r.Register(stubAdapter{name: "flux", capabilities: Capabilities{RequiresGit: true, SupportsPR: true}}); err != nil {
		t.Fatal(err)
	}

	a, err := r.Select("direct")
	if err != nil {
		t.Fatal(err)
	}
	if a.Name() != "direct" {
		t.Fatalf("got %q", a.Name())
	}

	flux, err := r.Select("flux")
	if err != nil {
		t.Fatal(err)
	}
	if !flux.Capabilities().RequiresGit || !flux.Capabilities().SupportsPR {
		t.Fatal("flux should require git and support PRs; direct should not")
	}

	if _, err := r.Select("nope"); err == nil {
		t.Fatal("selecting an unregistered adapter should fail")
	}
}

func TestConflictErrorIsLoud(t *testing.T) {
	err := Conflict("Project/checkout", "$.spec", "concurrent edit detected", "re-read the latest spec and reapply")
	var de Error
	if !errors.As(err, &de) {
		t.Fatalf("expected delivery.Error, got %T", err)
	}
	if de.Code != ErrConflicted {
		t.Fatalf("got code %q", de.Code)
	}
	if !AsConflict(err) {
		t.Fatal("AsConflict should report true")
	}
	if de.DocsURL == "" {
		t.Fatal("structured error must carry a docs URL")
	}
}
