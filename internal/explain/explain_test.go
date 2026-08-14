package explain

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/dafrie/kelson/internal/delivery"
	"github.com/dafrie/kelson/internal/diff"
	"github.com/dafrie/kelson/internal/observation"
)

// The tests here drive the whole capability on fakes: staged verdicts (what the
// observation plane decided), staged recorded manifests (what two revisions
// actually deployed) and a staged log window. No cluster is involved and none
// can be — the package holds no client, which is the property ADR-0023 records.

const (
	liveRevision = "rev-00000043"
	pastRevision = "rev-00000042"
	namespace    = "hello-production"
)

// deploymentYAML renders a one-container Deployment with the given env entries
// and an optional probe, in the shape internal/renderer emits.
func deploymentYAML(name, image string, env []string, probe bool) []byte {
	var b strings.Builder
	fmt.Fprintf(&b, "apiVersion: apps/v1\nkind: Deployment\nmetadata:\n  name: %s\n  namespace: %s\n", name, namespace)
	b.WriteString("spec:\n  template:\n    spec:\n      containers:\n")
	fmt.Fprintf(&b, "        - name: %s\n          image: %s\n", name, image)
	if probe {
		b.WriteString("          readinessProbe:\n            httpGet:\n              path: /healthz\n              port: 8080\n")
	}
	if len(env) > 0 {
		b.WriteString("          env:\n")
		for _, e := range env {
			b.WriteString(e)
		}
	}
	return []byte(b.String())
}

func literalEnv(name, value string) string {
	return fmt.Sprintf("            - name: %s\n              value: %q\n", name, value)
}

func secretEnv(name, secret, key string) string {
	return fmt.Sprintf("            - name: %s\n              valueFrom:\n                secretKeyRef:\n                  name: %s\n                  key: %s\n",
		name, secret, key)
}

func manifest(name string, yaml []byte) delivery.Manifest {
	return delivery.Manifest{APIVersion: "apps/v1", Kind: "Deployment", Name: name, Namespace: namespace, YAML: yaml}
}

// revisions stages a ManifestFn over a map of revision -> manifests.
func revisions(sets map[string][]delivery.Manifest) ManifestFn {
	return func(_ context.Context, revision string) ([]delivery.Manifest, error) {
		set, ok := sets[revision]
		if !ok {
			return nil, fmt.Errorf("revision %q is not in the retained history", revision)
		}
		return set, nil
	}
}

func history() []delivery.Entry {
	return []delivery.Entry{
		{Revision: liveRevision, CommittedAt: "2026-08-13T09:20:00Z", Message: "deploy hello production", Author: "ada"},
		{Revision: pastRevision, CommittedAt: "2026-08-12T17:02:00Z", Message: "deploy hello production"},
	}
}

func crashVerdict(logs string) observation.Verdict {
	return observation.Verdict{
		Healthy:     false,
		Code:        observation.CodeCrashLoopBackOff,
		Reason:      "CrashLoopBackOff",
		Resource:    "Deployment/" + namespace + "/web",
		Remediation: "the container keeps crashing: read its logs, fix the command or startup error, then redeploy",
		Containers: []observation.Container{{
			Name: "web", Pod: "web-6b8f-2xq", Code: observation.CodeCrashLoopBackOff,
			Reason: "CrashLoopBackOff", Logs: logs,
		}},
	}
}

// baseInput is the missing-env-var scenario: the live revision dropped
// DATABASE_URL, its predecessor had it.
func baseInput(logs string) Input {
	return Input{
		Project:     "hello",
		Environment: "production",
		Namespace:   namespace,
		Status:      delivery.Status{Phase: delivery.PhaseDegraded, Revision: liveRevision},
		Verdicts:    []observation.Verdict{crashVerdict(logs)},
		History:     history(),
		Manifests: revisions(map[string][]delivery.Manifest{
			liveRevision: {manifest("web", deploymentYAML("web", "ghcr.io/acme/hello:1.4.3",
				[]string{literalEnv("LOG_LEVEL", "info")}, false))},
			pastRevision: {manifest("web", deploymentYAML("web", "ghcr.io/acme/hello:1.4.2",
				[]string{literalEnv("DATABASE_URL", "postgres://db/app"), literalEnv("LOG_LEVEL", "info")}, false))},
		}),
	}
}

func causeWith(t *testing.T, e Explanation, code Code) Cause {
	t.Helper()
	for _, c := range e.Causes {
		if c.Code == code {
			return c
		}
	}
	t.Fatalf("no %s cause in %d causes:\n%s", code, len(e.Causes), e.Text())
	return Cause{}
}

func mustNotHave(t *testing.T, e Explanation, code Code) {
	t.Helper()
	for _, c := range e.Causes {
		if c.Code == code {
			t.Fatalf("unexpected %s cause:\n%s", code, e.Text())
		}
	}
}

func contains(t *testing.T, got string, want ...string) {
	t.Helper()
	for _, w := range want {
		if !strings.Contains(got, w) {
			t.Errorf("missing %q in:\n%s", w, got)
		}
	}
}

// TestAcceptanceMissingEnvVarNamesVariableAndRevision is issue #77's acceptance
// criterion: a CrashLoopBackOff caused by a missing environment variable is
// explained with the specific variable and the revision that introduced it.
//
// Both signals fire — the recorded manifests show DATABASE_URL removed by
// rev-00000043, and the container's own output names it — so the confidence is
// high and the evidence carries one of each.
func TestAcceptanceMissingEnvVarNamesVariableAndRevision(t *testing.T) {
	e := Explain(context.Background(), baseInput(
		"booting hello 1.4.3\nTraceback (most recent call last):\n  File \"app.py\", line 9, in <module>\nKeyError: 'DATABASE_URL'\n"))

	c := causeWith(t, e, CodeMissingEnvVar)
	if c.Confidence != ConfidenceHigh {
		t.Errorf("confidence = %q, want high (both signals agree)", c.Confidence)
	}
	contains(t, c.Message, "DATABASE_URL", "removed", "output names it")
	if c.IntroducedBy == nil || c.IntroducedBy.Revision != liveRevision {
		t.Fatalf("introducedBy = %+v, want %s", c.IntroducedBy, liveRevision)
	}
	if c.IntroducedBy.CommittedAt != "2026-08-13T09:20:00Z" || c.IntroducedBy.Author != "ada" {
		t.Errorf("the revision reference must carry the history entry, got %+v", c.IntroducedBy)
	}

	var kinds []EvidenceKind
	for _, ev := range c.Evidence {
		kinds = append(kinds, ev.Kind)
	}
	if !hasKind(kinds, EvidenceLog) || !hasKind(kinds, EvidenceRevision) {
		t.Errorf("evidence kinds = %v, want both a log and a revision item", kinds)
	}
	contains(t, e.Text(), "DATABASE_URL", "KeyError", liveRevision, "explain/missing-env-var", "[high]")

	// The cause outranks the symptom it explains: both are present, and the
	// missing variable is read first.
	if e.Causes[0].Code != CodeMissingEnvVar {
		t.Errorf("first cause = %s, want the missing variable above the crash loop", e.Causes[0].Code)
	}
	causeWith(t, e, CodeCrashLoop)
	contains(t, e.Summary, "DATABASE_URL", "high confidence", liveRevision)
}

func hasKind(kinds []EvidenceKind, want EvidenceKind) bool {
	for _, k := range kinds {
		if k == want {
			return true
		}
	}
	return false
}

// TestMissingEnvVarCorrelationOnlyIsMedium: the revision took the variable away
// and the container says nothing about it. That is one signal, so it is a
// medium and the message says the word "correlation" rather than implying the
// container complained.
func TestMissingEnvVarCorrelationOnlyIsMedium(t *testing.T) {
	e := Explain(context.Background(), baseInput("panic: runtime error: invalid memory address\n"))

	c := causeWith(t, e, CodeMissingEnvVar)
	if c.Confidence != ConfidenceMedium {
		t.Errorf("confidence = %q, want medium (correlation only)", c.Confidence)
	}
	contains(t, c.Message, "DATABASE_URL", "correlation", "does not name")
	if c.IntroducedBy == nil || c.IntroducedBy.Revision != liveRevision {
		t.Errorf("a correlation still names the revision it correlates with, got %+v", c.IntroducedBy)
	}
}

// TestMissingEnvVarFromLogsAloneNamesNoRevision: the container says a variable
// is missing and no revision touched it. Medium, and deliberately no
// introducedBy — a variable that was never declared has no revision to blame.
func TestMissingEnvVarFromLogsAloneNamesNoRevision(t *testing.T) {
	in := baseInput("fatal: STRIPE_API_KEY is not set\n")
	// Both revisions identical: nothing changed.
	same := []delivery.Manifest{manifest("web", deploymentYAML("web", "ghcr.io/acme/hello:1.4.3",
		[]string{literalEnv("LOG_LEVEL", "info")}, false))}
	in.Manifests = revisions(map[string][]delivery.Manifest{liveRevision: same, pastRevision: same})

	e := Explain(context.Background(), in)
	c := causeWith(t, e, CodeMissingEnvVar)
	if c.Confidence != ConfidenceMedium {
		t.Errorf("confidence = %q, want medium (the output alone)", c.Confidence)
	}
	contains(t, c.Message, "STRIPE_API_KEY", "No recorded revision changed it")
	if c.IntroducedBy != nil {
		t.Errorf("introducedBy = %+v, want none: nothing correlates this with a change", c.IntroducedBy)
	}
}

// TestMissingEnvVarNeedsASignal: a crash loop with neither an env change nor a
// variable named in the output produces no missing-variable cause at all. The
// crash loop itself is still reported — kelson says what it knows and nothing
// more.
func TestMissingEnvVarNeedsASignal(t *testing.T) {
	in := baseInput("segmentation fault\n")
	same := []delivery.Manifest{manifest("web", deploymentYAML("web", "ghcr.io/acme/hello:1.4.3", nil, false))}
	in.Manifests = revisions(map[string][]delivery.Manifest{liveRevision: same, pastRevision: same})

	e := Explain(context.Background(), in)
	mustNotHave(t, e, CodeMissingEnvVar)
	causeWith(t, e, CodeCrashLoop)
}

// TestSecretReferenceAddedIsACandidate: repointing a variable at a Secret key
// is the same failure as removing it when the key is not there, and the cause
// names the Secret and key so the reader knows what to write.
func TestSecretReferenceAddedIsACandidate(t *testing.T) {
	in := baseInput("error: cannot read DATABASE_URL from environment\n")
	in.Manifests = revisions(map[string][]delivery.Manifest{
		liveRevision: {manifest("web", deploymentYAML("web", "ghcr.io/acme/hello:1.4.3",
			[]string{secretEnv("DATABASE_URL", "checkout-db", "url")}, false))},
		pastRevision: {manifest("web", deploymentYAML("web", "ghcr.io/acme/hello:1.4.2",
			[]string{literalEnv("DATABASE_URL", "postgres://db/app")}, false))},
	})

	e := Explain(context.Background(), in)
	c := causeWith(t, e, CodeMissingEnvVar)
	contains(t, c.Message, "DATABASE_URL", "secret checkout-db/url")
	contains(t, c.Remediation, "checkout-db")
	// The field reference points at the live manifest, because the variable is
	// still declared there.
	var fields []string
	for _, ev := range c.Evidence {
		if ev.Kind == EvidenceField {
			fields = append(fields, ev.Detail)
		}
	}
	if len(fields) == 0 || !strings.Contains(fields[0], "env[DATABASE_URL]") {
		t.Errorf("field evidence = %v, want the rendered env path", fields)
	}
}

// TestImagePullNamesTheImageAndTheRegistryError: the kubelet quotes the
// reference it tried, and that is what the cause names.
func TestImagePullNamesTheImageAndTheRegistryError(t *testing.T) {
	in := baseInput("")
	in.Verdicts = []observation.Verdict{{
		Healthy:  false,
		Code:     observation.CodeImagePullBackOff,
		Reason:   `Back-off pulling image "ghcr.io/acme/hello:1.4.3": failed to authorize: 401 Unauthorized`,
		Resource: "Deployment/" + namespace + "/web",
	}}

	e := Explain(context.Background(), in)
	c := causeWith(t, e, CodeImagePull)
	if c.Confidence != ConfidenceHigh {
		t.Errorf("confidence = %q, want high (the kubelet named it)", c.Confidence)
	}
	contains(t, c.Message, "ghcr.io/acme/hello:1.4.3")
	if len(c.Evidence) == 0 || !strings.Contains(c.Evidence[0].Detail, "401 Unauthorized") {
		t.Errorf("the registry's own error must be the evidence, got %+v", c.Evidence)
	}
}

// TestOOMKilledIsItsOwnCause: the kernel stopping a container and the container
// crashing on its own logic ask for different fixes, so they are different
// causes even though observation gives both the same code.
func TestOOMKilledIsItsOwnCause(t *testing.T) {
	in := baseInput("")
	in.Verdicts = []observation.Verdict{{
		Healthy:  false,
		Code:     observation.CodeCrashLoopBackOff,
		Reason:   "OOMKilled",
		Resource: "Deployment/" + namespace + "/web",
		Containers: []observation.Container{{
			Name: "web", Pod: "web-6b8f-2xq", Code: observation.CodeCrashLoopBackOff, Reason: "OOMKilled",
		}},
	}}

	e := Explain(context.Background(), in)
	c := causeWith(t, e, CodeOOMKilled)
	if c.Confidence != ConfidenceHigh {
		t.Errorf("confidence = %q, want high", c.Confidence)
	}
	contains(t, c.Message, "memory limit")
	contains(t, c.Remediation, "memory limit")
	mustNotHave(t, e, CodeCrashLoop)
}

// TestInferredCrashLoopIsMedium: observation also infers a loop from a
// terminated container with restarts, where the kubelet named no waiting
// reason. One signal short, so it is a medium and says which part is inferred.
func TestInferredCrashLoopIsMedium(t *testing.T) {
	in := baseInput("")
	in.Verdicts = []observation.Verdict{{
		Healthy:  false,
		Code:     observation.CodeCrashLoopBackOff,
		Reason:   "Error",
		Resource: "Deployment/" + namespace + "/web",
	}}

	e := Explain(context.Background(), in)
	c := causeWith(t, e, CodeCrashLoop)
	if c.Confidence != ConfidenceMedium {
		t.Errorf("confidence = %q, want medium (no kubelet CrashLoopBackOff)", c.Confidence)
	}
	contains(t, c.Message, "inferred")
}

// TestFailingProbeNamesPathAndPort: which probe, and where it points, read from
// the manifest kelson applied.
func TestFailingProbeNamesPathAndPort(t *testing.T) {
	in := baseInput("")
	in.Verdicts = []observation.Verdict{{
		Healthy:  false,
		Code:     observation.CodeFailingProbe,
		Reason:   "probe is failing",
		Resource: "Deployment/" + namespace + "/web",
	}}
	in.Manifests = revisions(map[string][]delivery.Manifest{
		liveRevision: {manifest("web", deploymentYAML("web", "ghcr.io/acme/hello:1.4.3", nil, true))},
		pastRevision: {manifest("web", deploymentYAML("web", "ghcr.io/acme/hello:1.4.2", nil, true))},
	})

	e := Explain(context.Background(), in)
	c := causeWith(t, e, CodeFailingProbe)
	if c.Confidence != ConfidenceMedium {
		t.Errorf("confidence = %q, want medium: observation calls this one a heuristic", c.Confidence)
	}
	contains(t, c.Message, "/healthz", "8080", "inferred")
	contains(t, e.Text(), "readinessProbe.httpGet")
}

// TestSchedulingAndSecretSyncRelayTheirVerdicts: the codes observation already
// owns are mapped, not re-derived, and the controller's own words are evidence.
func TestSchedulingAndSecretSyncRelayTheirVerdicts(t *testing.T) {
	in := baseInput("")
	in.Verdicts = []observation.Verdict{
		{
			Healthy: false, Code: observation.CodeSecretSyncFailed,
			Reason:   "SecretSyncedError: could not get secret data from provider",
			Resource: "external-secrets.io/ExternalSecret/" + namespace + "/checkout-db",
		},
		{
			Healthy: false, Code: observation.CodeInsufficientResources,
			Reason:   "0/3 nodes are available: 3 Insufficient memory",
			Resource: "Deployment/" + namespace + "/worker",
		},
		{
			Healthy: false, Code: observation.CodeMissing,
			Reason:   namespace + "/api is not in the cluster",
			Resource: "Deployment/" + namespace + "/api",
		},
	}

	e := Explain(context.Background(), in)
	sync := causeWith(t, e, CodeSecretSyncFailed)
	contains(t, sync.Evidence[0].Detail, "SecretSyncedError")
	res := causeWith(t, e, CodeInsufficientResources)
	contains(t, res.Evidence[0].Detail, "Insufficient memory")
	causeWith(t, e, CodeWorkloadMissing)

	// The Secret that never synced is above the workloads waiting on it.
	if e.Causes[0].Code != CodeSecretSyncFailed {
		t.Errorf("first cause = %s, want the sync failure above its symptoms", e.Causes[0].Code)
	}
}

// TestReconcilerFailureIsRelayedVerbatim: a Flux rejection is the answer, and
// the controller's own sentence is the evidence.
func TestReconcilerFailureIsRelayedVerbatim(t *testing.T) {
	in := baseInput("")
	in.Verdicts = nil
	in.Status = delivery.Status{
		Phase:    delivery.PhaseRejected,
		Revision: liveRevision,
		Cause:    "flux: Kustomization apps/hello rejected the change (BuildFailed): accumulating resources: file not found",
		Detail:   map[string]string{"kustomization": "apps/hello"},
	}

	e := Explain(context.Background(), in)
	c := causeWith(t, e, CodeReconcilerNotReady)
	if c.Confidence != ConfidenceHigh || c.Resource != "apps/hello" {
		t.Errorf("cause = %+v, want high confidence on apps/hello", c)
	}
	contains(t, c.Evidence[0].Detail, "BuildFailed", "accumulating resources")
}

// TestDecryptionFailureIsItsOwnCause: the SOPS failure kustomize-controller
// reports as an opaque build error gets a code of its own and the three-step
// fix ADR-0022 records.
func TestDecryptionFailureIsItsOwnCause(t *testing.T) {
	in := baseInput("")
	in.Verdicts = nil
	in.Status = delivery.Status{
		Phase:    delivery.PhaseRejected,
		Revision: liveRevision,
		Cause: "flux: Kustomization apps/hello rejected the change (BuildFailed): Error getting data key\n" +
			"  cause: this Kustomization could not decrypt the SOPS-encrypted manifests at ./apps/hello.",
	}

	e := Explain(context.Background(), in)
	c := causeWith(t, e, CodeDecryptionFailed)
	contains(t, c.Remediation, "spec.decryption", "age identity", "recipients")
	mustNotHave(t, e, CodeReconcilerNotReady)
}

// TestDirectModeDegradedIsNotAReconcilerCause: direct mode applies manifests
// itself, and its degraded status is the same fact the verdicts carry in more
// detail. Repeating it would spend the budget saying one thing twice.
func TestDirectModeDegradedIsNotAReconcilerCause(t *testing.T) {
	in := baseInput("")
	in.Status = delivery.Status{
		Phase:    delivery.PhaseDegraded,
		Revision: liveRevision,
		Cause:    "direct: Deployment/hello-production/web: 0/1 replicas ready",
	}
	e := Explain(context.Background(), in)
	mustNotHave(t, e, CodeReconcilerNotReady)
}

// TestReleaseJobCauses: a hook that is running is a deployment in progress; one
// that failed is why the workloads of this revision were never applied.
func TestReleaseJobCauses(t *testing.T) {
	for _, tc := range []struct {
		state string
		code  Code
		want  string
	}{
		{"running", CodeReleaseJobPending, "still running"},
		{"failed", CodeReleaseJobFailed, "never applied"},
		{"timeout", CodeReleaseJobFailed, "ran out of time"},
	} {
		t.Run(tc.state, func(t *testing.T) {
			in := baseInput("")
			in.Verdicts = nil
			in.Status = delivery.Status{
				Phase:    delivery.PhaseReconciling,
				Revision: liveRevision,
				Cause:    "direct: release job hello-migrate is " + tc.state,
				Detail:   map[string]string{"releaseJob": "hello-migrate", "releaseState": tc.state},
			}
			e := Explain(context.Background(), in)
			c := causeWith(t, e, tc.code)
			contains(t, c.Message, "hello-migrate", tc.want)
			if c.Resource != "Job/hello-migrate" {
				t.Errorf("resource = %q, want Job/hello-migrate", c.Resource)
			}
		})
	}
}

// TestPolicyViolationsCarryTheirEnforcement: an enforced rejection is what
// stopped the change; an audit finding never stopped anything and must not read
// as if it did.
func TestPolicyViolationsCarryTheirEnforcement(t *testing.T) {
	in := baseInput("")
	in.Verdicts = nil
	in.Violations = []diff.PolicyViolation{
		{
			Code: diff.CodeWebhookDenied, Engine: "kyverno", Policy: "require-limits", Rule: "check-memory",
			Resource: "Deployment/web", Path: "spec.template.spec.containers[0].resources.limits",
			Message: "validation error: memory limit is required", Enforcement: diff.EnforcementEnforce,
			Remediation: "read policy require-limits",
		},
		{
			Code: diff.CodeAuditFinding, Engine: "kyverno", Policy: "disallow-latest",
			Resource: "Deployment/web", Message: "image tag latest is discouraged", Enforcement: diff.EnforcementAudit,
		},
	}

	e := Explain(context.Background(), in)
	var enforced, audit Cause
	for _, c := range e.Causes {
		if strings.Contains(c.Message, "require-limits") {
			enforced = c
		}
		if strings.Contains(c.Message, "disallow-latest") {
			audit = c
		}
	}
	if enforced.Confidence != ConfidenceHigh {
		t.Errorf("enforced violation confidence = %q, want high", enforced.Confidence)
	}
	contains(t, enforced.Message, "memory limit is required")
	if audit.Confidence != ConfidenceLow {
		t.Errorf("audit finding confidence = %q, want low", audit.Confidence)
	}
	contains(t, audit.Message, "audit mode", "Nothing was vetoed")
}

// TestHealthyExplainsNothing: no failing verdict, no cause. The summary says so
// rather than inventing a worry.
func TestHealthyExplainsNothing(t *testing.T) {
	in := baseInput("")
	in.Status = delivery.Status{Phase: delivery.PhaseHealthy, Revision: liveRevision}
	in.Verdicts = []observation.Verdict{{Healthy: true, Code: observation.CodeHealthy, Resource: "Deployment/" + namespace + "/web"}}

	e := Explain(context.Background(), in)
	if len(e.Causes) != 0 {
		t.Fatalf("healthy explanation has %d causes:\n%s", len(e.Causes), e.Text())
	}
	contains(t, e.Summary, "no cause found")
	contains(t, e.Text(), "CAUSES", "none:")
	// The change correlation is still made: "nothing is wrong and here is what
	// last changed" is a legitimate answer.
	if e.RecentChange == nil || e.RecentChange.Revision.Revision != liveRevision {
		t.Errorf("recent change = %+v, want the live revision", e.RecentChange)
	}
}

// TestMissingSeamsDegradeToNotes: every seam is optional, and an absent one
// costs a correlation and says so — never the whole answer.
func TestMissingSeamsDegradeToNotes(t *testing.T) {
	e := Explain(context.Background(), Input{
		Project: "hello", Environment: "production", Namespace: namespace,
		Status:   delivery.Status{Phase: delivery.PhaseDegraded, Revision: liveRevision},
		Verdicts: []observation.Verdict{crashVerdict("boom\n")},
	})
	causeWith(t, e, CodeCrashLoop)
	if e.RecentChange != nil {
		t.Errorf("recent change = %+v, want none without history", e.RecentChange)
	}
	if len(e.Notes) == 0 || !strings.Contains(e.Notes[0], "no recorded revisions") {
		t.Errorf("notes = %v, want the missing history stated", e.Notes)
	}
}

// TestPrunedRevisionIsANoteNotAFailure: retention moving past the predecessor
// leaves the live revision named and the comparison unavailable, stated.
func TestPrunedRevisionIsANoteNotAFailure(t *testing.T) {
	in := baseInput("KeyError: 'DATABASE_URL'\n")
	in.Manifests = func(_ context.Context, revision string) ([]delivery.Manifest, error) {
		if revision == liveRevision {
			return []delivery.Manifest{manifest("web", deploymentYAML("web", "img", nil, false))}, nil
		}
		return nil, errors.New("revision is not in the retained history")
	}

	e := Explain(context.Background(), in)
	if e.RecentChange == nil || e.RecentChange.Revision.Revision != liveRevision {
		t.Fatalf("recent change = %+v, want the live revision named", e.RecentChange)
	}
	if len(e.RecentChange.Env) != 0 {
		t.Errorf("env changes = %v, want none: the predecessor could not be read", e.RecentChange.Env)
	}
	if len(e.Notes) == 0 {
		t.Errorf("a pruned predecessor must be stated in the notes")
	}
	// The log signal alone still names the variable, at medium.
	c := causeWith(t, e, CodeMissingEnvVar)
	if c.Confidence != ConfidenceMedium {
		t.Errorf("confidence = %q, want medium without the change signal", c.Confidence)
	}
}

// TestLogSeamIsUsedWhenTheVerdictCarriesNoLogs: a probe built without a log
// source produces verdicts with no container output, and the caller's own log
// window is the fallback.
func TestLogSeamIsUsedWhenTheVerdictCarriesNoLogs(t *testing.T) {
	in := baseInput("")
	in.Verdicts = []observation.Verdict{{
		Healthy: false, Code: observation.CodeCrashLoopBackOff, Reason: "CrashLoopBackOff",
		Resource: "Deployment/" + namespace + "/web",
	}}
	asked := 0
	in.Logs = func(_ context.Context, ns, component string, lines int) ([]observation.Line, error) {
		asked++
		if ns != namespace || component != "web" || lines != MaxLogLines {
			return nil, fmt.Errorf("unexpected query %s/%s/%d", ns, component, lines)
		}
		return []observation.Line{{Pod: "web-1", Message: "fatal: DATABASE_URL is not set"}}, nil
	}

	e := Explain(context.Background(), in)
	c := causeWith(t, e, CodeMissingEnvVar)
	contains(t, c.Message, "DATABASE_URL")
	if asked != 1 {
		t.Errorf("the log window was fetched %d times, want exactly one for one workload", asked)
	}
}

// TestLogSeamFailureIsANote: a log engine that is down costs the excerpt, not
// the explanation.
func TestLogSeamFailureIsANote(t *testing.T) {
	in := baseInput("")
	in.Verdicts = []observation.Verdict{{
		Healthy: false, Code: observation.CodeCrashLoopBackOff, Reason: "CrashLoopBackOff",
		Resource: "Deployment/" + namespace + "/web",
	}}
	in.Logs = func(context.Context, string, string, int) ([]observation.Line, error) {
		return nil, errors.New("no log engine configured")
	}

	e := Explain(context.Background(), in)
	causeWith(t, e, CodeCrashLoop)
	if len(e.Notes) == 0 || !strings.Contains(strings.Join(e.Notes, " "), "no log engine configured") {
		t.Errorf("notes = %v, want the log failure stated", e.Notes)
	}
}

// TestDeterministic: the same input twice produces byte-identical text. Map
// iteration inside the manifest reader and the diff must never reach the
// output, or an agent reads two identical calls as a change.
func TestDeterministic(t *testing.T) {
	in := baseInput("KeyError: 'DATABASE_URL'\n")
	first := Explain(context.Background(), in).Text()
	for i := 0; i < 20; i++ {
		if got := Explain(context.Background(), in).Text(); got != first {
			t.Fatalf("run %d differs:\n%s\n---\n%s", i, first, got)
		}
	}
}
