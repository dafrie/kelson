package promote

import (
	"errors"
	"testing"

	"github.com/dafrie/kelson/internal/model"
)

func project() *model.Project {
	p := &model.Project{}
	p.Metadata.Name = "checkout"
	p.Spec.Components = []model.Component{
		{Name: "web", Port: 8080},
		{Name: "digest", Schedule: "30 6 * * 1-5"},
		{Name: "db", Kind: model.ComponentPostgres},
	}
	return p
}

func environment(overrides ...model.ComponentOverride) *model.Environment {
	e := &model.Environment{}
	e.Metadata.Name = "production"
	e.Spec.Project = "checkout"
	e.Spec.Components = overrides
	return e
}

func changeFor(changes []Change, name string) Change {
	for _, c := range changes {
		if c.Component == name {
			return c
		}
	}
	return Change{}
}

// TestPlanPinsWhatTheSourceRuns is the happy path: an unpinned target moves to
// the image the source's latest revision runs, and the data component is not
// in the plan at all.
func TestPlanPinsWhatTheSourceRuns(t *testing.T) {
	deployed := map[string]string{"web": "img@sha256:aaaa", "digest": "img@sha256:bbbb"}
	changes, err := Plan(project(), environment(), deployed, nil)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if len(changes) != 2 {
		t.Fatalf("Plan returned %d changes, want 2 (the data component has no image to promote): %+v", len(changes), changes)
	}
	if changes[0].Component != "web" || changes[1].Component != "digest" {
		t.Errorf("Plan reordered the project's components: %+v", changes)
	}
	web := changeFor(changes, "web")
	if web.Status != StatusPinned || web.From != "" || web.To != "img@sha256:aaaa" {
		t.Errorf("web: %+v", web)
	}
}

// TestPlanReportsAnAlreadyPinnedComponentAsANoOp: reported, never omitted — a
// caller must be able to see that the component was considered.
func TestPlanReportsAnAlreadyPinnedComponentAsANoOp(t *testing.T) {
	deployed := map[string]string{"web": "img@sha256:aaaa", "digest": "img@sha256:bbbb"}
	target := environment(model.ComponentOverride{Name: "web", Image: "img@sha256:aaaa"})
	changes, err := Plan(project(), target, deployed, nil)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	web := changeFor(changes, "web")
	if web.Status != StatusUnchanged {
		t.Errorf("web: got %s, want unchanged: %+v", web.Status, web)
	}
	if web.From != "img@sha256:aaaa" || web.To != "img@sha256:aaaa" {
		t.Errorf("an unchanged component must still report both sides: %+v", web)
	}
	if len(Pinned(changes)) != 1 {
		t.Errorf("an unchanged component must not be written: %+v", Pinned(changes))
	}
}

// TestPlanSkipsAComponentTheRevisionDoesNotCarry, with a reason and a code.
func TestPlanSkipsAComponentTheRevisionDoesNotCarry(t *testing.T) {
	changes, err := Plan(project(), environment(), map[string]string{"web": "img@sha256:aaaa"}, nil)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	digest := changeFor(changes, "digest")
	if digest.Status != StatusSkipped || digest.Code != ErrNotInRevision {
		t.Errorf("digest: %+v", digest)
	}
	if digest.To != "" {
		t.Errorf("a skipped component must not carry an image it would have guessed: %+v", digest)
	}
	if digest.Reason == "" {
		t.Error("a skip with no reason is a silent skip")
	}
}

// TestPlanRefusesAnUnknownComponentFilter.
func TestPlanRefusesAnUnknownComponentFilter(t *testing.T) {
	_, err := Plan(project(), environment(), map[string]string{"web": "img@sha256:aaaa"}, []string{"webb"})
	var pe Error
	if !errors.As(err, &pe) || pe.Code != ErrComponentUnknown {
		t.Fatalf("want promote/component-unknown, got %v", err)
	}
}

// TestPlanRefusesAPromotionOfOnlyDataComponents: filtering down to a data
// component selects nothing promotable, and saying so beats an empty plan.
func TestPlanRefusesAPromotionOfOnlyDataComponents(t *testing.T) {
	_, err := Plan(project(), environment(), map[string]string{"web": "img@sha256:aaaa"}, []string{"db"})
	var pe Error
	if !errors.As(err, &pe) || pe.Code != ErrNoWorkloads {
		t.Fatalf("want promote/no-workloads, got %v", err)
	}
}

// TestPlanHonoursTheComponentFilter.
func TestPlanHonoursTheComponentFilter(t *testing.T) {
	deployed := map[string]string{"web": "img@sha256:aaaa", "digest": "img@sha256:bbbb"}
	changes, err := Plan(project(), environment(), deployed, []string{"digest"})
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if len(changes) != 1 || changes[0].Component != "digest" {
		t.Fatalf("the filter was not applied: %+v", changes)
	}
}

// TestNothingDeployedNamesTheEnvironmentAndTheFix.
func TestNothingDeployedNamesTheEnvironmentAndTheFix(t *testing.T) {
	var pe Error
	if !errors.As(NothingDeployed("checkout", "staging"), &pe) {
		t.Fatal("NothingDeployed is not a structured promotion error")
	}
	if pe.Code != ErrNothingDeployed {
		t.Errorf("code %q", pe.Code)
	}
	if pe.Remediation == "" {
		t.Error("a refusal with no remediation")
	}
}

// TestSummaryCountsEveryStatus.
func TestSummaryCountsEveryStatus(t *testing.T) {
	got := Summary([]Change{
		{Status: StatusPinned}, {Status: StatusPinned},
		{Status: StatusUnchanged},
		{Status: StatusSkipped},
	})
	if got != "2 pinned, 1 unchanged, 1 skipped" {
		t.Errorf("Summary = %q", got)
	}
}
