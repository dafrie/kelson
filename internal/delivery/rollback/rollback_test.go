package rollback

import (
	"context"
	"strings"
	"testing"

	sigsyaml "sigs.k8s.io/yaml"

	"github.com/dafrie/kelson/internal/delivery"
	"github.com/dafrie/kelson/internal/delivery/direct"
	"github.com/dafrie/kelson/internal/diff"
)

func fieldDiff(path string) diff.FieldDiff {
	return diff.FieldDiff{Path: path, Risk: diff.RiskAdditive, Origin: diff.OriginSpec}
}

func dep(name string) diff.ResourceDiff {
	return diff.ResourceDiff{
		APIVersion: "apps/v1", Kind: "Deployment", Name: name, Namespace: "shop",
		Op: diff.OpModified, Risk: diff.RiskRestart,
	}
}

// hasCause reports whether any finding carries the given cause.
func hasCause(f []Finding, c Cause) bool {
	for _, x := range f {
		if x.Cause == c {
			return true
		}
	}
	return false
}

func countCause(f []Finding, c Cause) int {
	n := 0
	for _, x := range f {
		if x.Cause == c {
			n++
		}
	}
	return n
}

// TestPreviewCleanRollback: a rollback whose only changes are config-level and
// additive is flagged nowhere — the only finding is the migrations caveat, risk
// is not escalated, and the summary reflects the plain diff (issue #55).
func TestPreviewCleanRollback(t *testing.T) {
	d := &diff.Diff{
		Level:       diff.LevelRendered,
		Project:     "shop",
		Environment: "production",
		Resources: []diff.ResourceDiff{
			withFields(dep("checkout"), fieldDiff("spec.template.spec.containers[0].image")),
			{APIVersion: "v1", Kind: "Service", Name: "checkout", Namespace: "shop", Op: diff.OpAdded, Risk: diff.RiskAdditive},
		},
	}
	got, findings := Preview(d)

	if countCause(findings, CauseMigrations) != 1 {
		t.Fatalf("migrations caveat should appear exactly once, got %d", countCause(findings, CauseMigrations))
	}
	for _, c := range []Cause{CauseImmutableField, CauseDeletedPVC, CauseStatefulData} {
		if hasCause(findings, c) {
			t.Fatalf("clean rollback flagged %s: %+v", c, findings)
		}
	}
	if got.Resources[0].Risk != diff.RiskRestart {
		t.Fatalf("clean rollback escalated risk to %q, want restart-required", got.Resources[0].Risk)
	}
	if got.Summary.MaxRisk != diff.RiskRestart {
		t.Fatalf("MaxRisk = %q, want restart-required", got.Summary.MaxRisk)
	}
}

// TestPreviewImmutableFieldL1 is the offline (no-cluster) path: a Deployment
// spec.selector change is identified before execution via the static list, its
// resource escalated to disruptive, and the summary rolls that up (issue #55
// acceptance: "changes that cannot be safely reverted are identified in the
// preview, before execution").
func TestPreviewImmutableFieldL1(t *testing.T) {
	d := &diff.Diff{
		Level: diff.LevelRendered,
		Resources: []diff.ResourceDiff{
			withFields(dep("checkout"), fieldDiff("spec.selector.matchLabels.app")),
		},
	}
	got, findings := Preview(d)

	f := findingsByCause(findings, CauseImmutableField)
	if len(f) != 1 {
		t.Fatalf("want 1 immutable-field finding, got %d", len(f))
	}
	if f[0].Path != "spec.selector.matchLabels.app" || !f[0].Never {
		t.Fatalf("finding = %+v", f[0])
	}
	if got.Resources[0].Risk != diff.RiskDisruptive {
		t.Fatalf("immutable-field resource risk = %q, want disruptive", got.Resources[0].Risk)
	}
	if got.Summary.MaxRisk != diff.RiskDisruptive {
		t.Fatalf("MaxRisk = %q, want disruptive", got.Summary.MaxRisk)
	}
}

// TestPreviewImmutableFieldL2 reuses the API server's own dry-run signal: a
// field-is-immutable enforcement violation on the server path is reported as an
// irreversible finding without consulting the static list.
func TestPreviewImmutableFieldL2(t *testing.T) {
	d := &diff.Diff{
		Level: diff.LevelServer,
		Resources: []diff.ResourceDiff{
			withFields(dep("checkout"), fieldDiff("spec.template.spec.containers[0].image")),
		},
		Violations: []diff.PolicyViolation{{
			Engine: "kubernetes", Policy: "field-is-immutable",
			Resource: "Deployment/checkout", Path: "spec.selector",
			Message: "field is immutable", Enforcement: diff.EnforcementEnforce,
		}},
	}
	_, findings := Preview(d)

	if len(findingsByCause(findings, CauseImmutableField)) != 1 {
		t.Fatalf("L2 immutable-field finding missing: %+v", findings)
	}
}

// TestPreviewImmutableFieldL2IgnoresAudit: an audit-mode (non-enforce)
// violation must not be reported as a blocker — enforcement matters.
func TestPreviewImmutableFieldL2IgnoresAudit(t *testing.T) {
	d := &diff.Diff{
		Level: diff.LevelServer,
		Resources: []diff.ResourceDiff{
			withFields(dep("checkout"), fieldDiff("spec.template.spec.containers[0].image")),
		},
		Violations: []diff.PolicyViolation{{
			Engine: "kubernetes", Policy: "field-is-immutable",
			Resource: "Deployment/checkout", Message: "audit only", Enforcement: diff.EnforcementAudit,
		}},
	}
	_, findings := Preview(d)
	if hasCause(findings, CauseImmutableField) {
		t.Fatalf("audit-mode immutable violation treated as a blocker: %+v", findings)
	}
}

// TestPreviewDeletedPVCRecreated: the acceptance case — a rollback that would
// recreate a PVC whose data is gone is flagged as never-restoring, before
// execution.
func TestPreviewDeletedPVCRecreated(t *testing.T) {
	d := &diff.Diff{
		Level: diff.LevelRendered,
		Resources: []diff.ResourceDiff{
			{APIVersion: "v1", Kind: "PersistentVolumeClaim", Name: "data", Namespace: "shop", Op: diff.OpAdded, Risk: diff.RiskAdditive},
		},
	}
	got, findings := Preview(d)

	f := findingsByCause(findings, CauseDeletedPVC)
	if len(f) != 1 || !f[0].Never {
		t.Fatalf("recreated-PVC finding = %+v, want a Never pvc-data-lost finding", f)
	}
	if got.Resources[0].Risk != diff.RiskDisruptive {
		t.Fatalf("recreated-PVC risk = %q, want disruptive", got.Resources[0].Risk)
	}
}

// TestPreviewDeletedPVCRemoved: deleting a PVC that exists now is equally
// destructive — the claim and its volume data are destroyed.
func TestPreviewDeletedPVCRemoved(t *testing.T) {
	d := &diff.Diff{
		Level: diff.LevelRendered,
		Resources: []diff.ResourceDiff{
			{APIVersion: "v1", Kind: "PersistentVolumeClaim", Name: "data", Namespace: "shop", Op: diff.OpRemoved, Risk: diff.RiskDisruptive},
		},
	}
	_, findings := Preview(d)
	if len(findingsByCause(findings, CauseDeletedPVC)) != 1 {
		t.Fatalf("removed-PVC finding missing: %+v", findings)
	}
}

// TestPreviewStatefulDataOperator: rolling back a CloudNativePG Cluster — an
// operator-owned resource whose data lives outside the manifest — is reported
// as stateful not rolled back, and the resource is escalated (issue #55).
func TestPreviewStatefulDataOperator(t *testing.T) {
	d := &diff.Diff{
		Level: diff.LevelRendered,
		Resources: []diff.ResourceDiff{
			{APIVersion: "postgresql.cnpg.io/v1", Kind: "Cluster", Name: "db", Namespace: "shop", Op: diff.OpModified, Risk: diff.RiskRestart},
		},
	}
	got, findings := Preview(d)
	if len(findingsByCause(findings, CauseStatefulData)) != 1 {
		t.Fatalf("data-operator finding missing: %+v", findings)
	}
	if got.Resources[0].Risk != diff.RiskDisruptive {
		t.Fatalf("data-operator risk = %q, want disruptive", got.Resources[0].Risk)
	}
}

// TestPreviewMigrationsCaveatAlwaysPresent: every rollback preview states that
// migrations are not covered rather than implying it is fully safe (issue #55).
func TestPreviewMigrationsCaveatAlwaysPresent(t *testing.T) {
	_, findings := Preview(&diff.Diff{Level: diff.LevelRendered})
	if !hasCause(findings, CauseMigrations) {
		t.Fatalf("migrations caveat missing on an otherwise-empty preview: %+v", findings)
	}
}

// TestPreviewMaxRiskEscalatesSummary: escalating resource risks is reflected in
// Summary.Disruptive and MaxRisk so an agent branching on the summary trips.
func TestPreviewMaxRiskEscalatesSummary(t *testing.T) {
	d := &diff.Diff{
		Level: diff.LevelRendered,
		Resources: []diff.ResourceDiff{
			withFields(dep("checkout"), fieldDiff("spec.selector")),
		},
	}
	got, _ := Preview(d)
	if got.Summary.MaxRisk != diff.RiskDisruptive {
		t.Fatalf("MaxRisk = %q, want disruptive", got.Summary.MaxRisk)
	}
	if len(got.Summary.Disruptive) != 1 || got.Summary.Disruptive[0] != "checkout" {
		t.Fatalf("Disruptive = %v, want [checkout]", got.Summary.Disruptive)
	}
}

// TestDirectSourceReadsAnyRetainedRevision covers "rollback to a revision older
// than the immediately previous one": a Source must return the recorded
// manifests of any retained revision, not just the newest one.
func TestDirectSourceReadsAnyRetainedRevision(t *testing.T) {
	store := openStoreT(t, 20)
	ctx := context.Background()
	appendT(t, store, "rev-00000001", "apps/v1", "Deployment", "web")
	appendT(t, store, "rev-00000002", "apps/v1", "Deployment", "web")
	appendT(t, store, "rev-00000003", "apps/v1", "Deployment", "web")

	src := &DirectSource{Store: store, Project: "shop", Environment: "production"}
	oldest, err := src.Revision(ctx, "rev-00000001")
	if err != nil {
		t.Fatalf("Revision(rev-1): %v", err)
	}
	current, err := src.Current(ctx)
	if err != nil {
		t.Fatalf("Current: %v", err)
	}
	if len(oldest) != 1 || oldest[0].Name != "web" {
		t.Fatalf("oldest = %+v", oldest)
	}
	if len(current) != 1 || current[0].Name != "web" {
		t.Fatalf("current = %+v", current)
	}
	if string(current[0].YAML) == string(oldest[0].YAML) {
		t.Fatal("current and oldest should differ for this fixture")
	}
}

// TestDirectSourcePrunedRevision: a target revision dropped by retention is
// reported clearly, not silently replayed as empty history.
func TestDirectSourcePrunedRevision(t *testing.T) {
	store := openStoreT(t, 3) // keep only 3
	ctx := context.Background()
	appendT(t, store, "rev-00000001", "apps/v1", "Deployment", "web")
	appendT(t, store, "rev-00000002", "apps/v1", "Deployment", "web")
	appendT(t, store, "rev-00000003", "apps/v1", "Deployment", "web")
	appendT(t, store, "rev-00000004", "apps/v1", "Deployment", "web")

	src := &DirectSource{Store: store, Project: "shop", Environment: "production"}
	_, err := src.Revision(ctx, "rev-00000001")
	if err == nil {
		t.Fatal("pruned revision should error")
	}
	if !strings.Contains(err.Error(), "not in the retained history") {
		t.Fatalf("error = %q, want a clear retained-history message", err.Error())
	}
}

// TestMaxRiskRanks: MaxRisk orders causes so the hardest irreversibility wins.
func TestMaxRiskRanks(t *testing.T) {
	if got := MaxRisk([]Finding{{Cause: CauseMigrations}}); got != CauseMigrations {
		t.Fatalf("MaxRisk(migrations) = %q", got)
	}
	if got := MaxRisk([]Finding{{Cause: CauseMigrations}, {Cause: CauseDeletedPVC}}); got != CauseDeletedPVC {
		t.Fatalf("MaxRisk should rank pvc-data-lost above migrations, got %q", got)
	}
}

// --- fixture helpers ---------------------------------------------------------

// withFields annotates a resource diff with fields, for building fixtures.
func withFields(r diff.ResourceDiff, fs ...diff.FieldDiff) diff.ResourceDiff {
	r.Fields = append(r.Fields, fs...)
	return r
}

func findingsByCause(f []Finding, c Cause) []Finding {
	var out []Finding
	for _, x := range f {
		if x.Cause == c {
			out = append(out, x)
		}
	}
	return out
}

func openStoreT(t *testing.T, keep int) *direct.Store {
	t.Helper()
	s, err := direct.OpenStore(direct.StoreOptions{Dir: t.TempDir(), Keep: keep})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	return s
}

// appendT records one deployment revision into the store. The replicas field
// differs per revision so distinct revisions yield distinct recorded bytes.
func appendT(t *testing.T, s *direct.Store, revision, apiVersion, kind, name string) {
	t.Helper()
	obj := map[string]any{
		"apiVersion": apiVersion,
		"kind":       kind,
		"metadata":   map[string]any{"name": name, "namespace": "shop"},
		"spec":       map[string]any{"replicas": revision},
	}
	doc, err := sigsyaml.Marshal(obj)
	if err != nil {
		t.Fatalf("marshal %s: %v", revision, err)
	}
	m := delivery.Manifest{APIVersion: apiVersion, Kind: kind, Name: name, Namespace: "shop", YAML: doc}
	stream := []byte("---\n" + string(m.YAML) + "\n")
	if _, err := s.Append("shop", "production", direct.Record{Revision: revision}, stream); err != nil {
		t.Fatalf("append %s: %v", revision, err)
	}
}
