package direct

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/dafrie/kelson/internal/delivery"
)

// The delivery half of the release-command hook (issue #104, ADR-0019).
//
// The property under test is not "a Job is applied" — the apply loop already
// applies everything. It is that the adapter STOPS at the Job: nothing after it
// reaches the cluster until the migration has succeeded, and a migration that
// fails leaves the previous revision serving.

const releaseJobName = "release-checkout-5be0c165"

// newReleaseAdapter builds an adapter with the release wait sped up, plus
// whatever else the case needs.
func newReleaseAdapter(t *testing.T, c *cluster, opts ...func(*Options)) *Adapter {
	t.Helper()
	store, err := OpenStore(StoreOptions{Dir: t.TempDir(), Keep: 5, Now: fixedClock()})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	o := Options{
		Client:      c.dyn,
		Mapper:      testMapper(),
		History:     store,
		Now:         fixedClock(),
		ReleasePoll: time.Millisecond,
	}
	for _, f := range opts {
		f(&o)
	}
	a, err := New(o)
	if err != nil {
		t.Fatalf("new adapter: %v", err)
	}
	return a
}

// releaseSet is a rendered set in the shape the renderer produces for a
// component with a release hook: the ServiceAccount and the Job first, the
// workload after.
func releaseSet(t *testing.T) delivery.ManifestSet {
	t.Helper()
	return set(
		manifest(t, "v1", "Namespace", testNS, ""),
		manifest(t, "v1", "ServiceAccount", "checkout", testNS),
		manifest(t, "batch/v1", "Job", releaseJobName, testNS,
			labelSet(map[string]string{labelReleaseHook: "true"}),
			spec(map[string]any{"backoffLimit": int64(2)})),
		manifest(t, "apps/v1", "Deployment", "checkout", testNS, spec(map[string]any{"replicas": int64(2)})),
	)
}

func jobKey() string    { return "Job/" + testNS + "/" + releaseJobName }
func deployKey() string { return "Deployment/" + testNS + "/checkout" }

// TestReleaseJobIsAWaitAndNotAnOrdering is the acceptance test of issue #104:
// the workloads are applied only after the release command has succeeded. The
// event log interleaves applies and Job reads, which is the only way to see the
// difference between "applied in this order" and "waited".
func TestReleaseJobIsAWaitAndNotAnOrdering(t *testing.T) {
	c := newCluster()
	c.scriptJobStatus(
		jobRunning(),
		jobRunning(),
		jobCondition("Complete", "True", "", "migrations applied"),
	)
	a := newReleaseAdapter(t, c)

	if _, err := a.Apply(context.Background(), releaseSet(t)); err != nil {
		t.Fatalf("apply: %v", err)
	}

	want := []string{
		"apply Namespace//" + testNS,
		"apply ServiceAccount/" + testNS + "/checkout",
		"apply " + jobKey(),
		"read " + jobKey(),
		"read " + jobKey(),
		"read " + jobKey(),
		"apply " + deployKey(),
	}
	got := c.eventLog()
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("event log:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

// TestReleaseFailureStopsTheDeployBeforeTheWorkloads is the other half of the
// acceptance criteria: a failed migration blocks the rollout, names itself, and
// carries the command's own output.
func TestReleaseFailureStopsTheDeployBeforeTheWorkloads(t *testing.T) {
	c := newCluster(releasePod())
	c.scriptJobStatus(jobCondition("Failed", "True", "BackoffLimitExceeded", "Job has reached the specified backoff limit"))

	var states []delivery.Status
	a := newReleaseAdapter(t, c, func(o *Options) {
		o.Logs = logTailerFunc(func(_ context.Context, ns, pod, container string) (string, error) {
			if ns != testNS || pod != "release-pod" || container != releaseContainer {
				return "", errors.New("wrong container: " + ns + "/" + pod + "/" + container)
			}
			return "Running migration 0042_add_index\nERROR: relation \"orders\" does not exist\n", nil
		})
		o.Progress = func(st delivery.Status) { states = append(states, st) }
	})

	_, err := a.Apply(context.Background(), releaseSet(t))
	if err == nil {
		t.Fatal("a failed release command did not fail the deploy")
	}
	if !delivery.AsReleaseFailed(err) {
		t.Fatalf("error is %v, want a delivery/release-failed", err)
	}
	var de delivery.Error
	if !errors.As(err, &de) {
		t.Fatalf("error %v does not carry the delivery taxonomy", err)
	}
	if !strings.Contains(de.Resource, releaseJobName) {
		t.Errorf("the failure does not name the Job: %s", de.Resource)
	}
	if !strings.Contains(de.Message, "previous revision is still running") {
		t.Errorf("the failure does not say what is still live: %s", de.Message)
	}
	if !strings.Contains(de.Cause, "relation \"orders\" does not exist") {
		t.Errorf("the failure does not quote the command's output: %q", de.Cause)
	}

	// Nothing of this revision rolled.
	if c.get(t, "deployments", testNS, "checkout") != nil {
		t.Error("the Deployment was applied even though the migration failed")
	}
	entries, err := a.History(context.Background(), releaseSet(t))
	if err != nil {
		t.Fatalf("history: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("a failed release recorded %d history entries; the revision never went live", len(entries))
	}

	// The state machine sees the failure in its own vocabulary: Rejected is
	// "processed and refused, not live", which is exactly this (ADR-0019).
	if len(states) == 0 {
		t.Fatal("no progress observations were reported")
	}
	last := states[len(states)-1]
	if last.Phase != delivery.PhaseRejected {
		t.Errorf("final observation is %s, want %s", last.Phase, delivery.PhaseRejected)
	}
	if last.Detail["releaseJob"] != releaseJobName {
		t.Errorf("the observation does not name the Job: %v", last.Detail)
	}
}

// TestReleaseJobCompletedForThisRevisionIsNotRerun is the idempotency contract
// the deterministic name exists for: re-applying an unchanged revision finds
// the Job it already ran, and a completed migration is a fact about the
// revision rather than about the deploy attempt.
func TestReleaseJobCompletedForThisRevisionIsNotRerun(t *testing.T) {
	c := newCluster()
	c.scriptJobStatus(jobCondition("Complete", "True", "", "migrations applied"))
	a := newReleaseAdapter(t, c)

	if _, err := a.Apply(context.Background(), releaseSet(t)); err != nil {
		t.Fatalf("first apply: %v", err)
	}
	first := countApplies(c.applyLog(), jobKey())

	if _, err := a.Apply(context.Background(), releaseSet(t)); err != nil {
		t.Fatalf("second apply: %v", err)
	}
	if second := countApplies(c.applyLog(), jobKey()); second != first {
		t.Fatalf("re-deploying the same revision applied the release Job again (%d applies, was %d); "+
			"a completed migration must not run twice for one spec", second, first)
	}
	if c.get(t, "deployments", testNS, "checkout") == nil {
		t.Error("the workloads did not roll after an already-completed migration")
	}
}

// TestFailedReleaseJobIsRetriedOnRedeploy: a Job object cannot be edited, so a
// verdict that must be re-taken is a delete and a create. Without this, the
// first transient failure would be permanent short of kubectl.
func TestFailedReleaseJobIsRetriedOnRedeploy(t *testing.T) {
	c := newCluster()
	c.scriptJobStatus(
		// The attempt that fails, then the failed Job the retry finds waiting
		// for it, then the run that succeeds.
		jobCondition("Failed", "True", "BackoffLimitExceeded", "the database was down"),
		jobCondition("Failed", "True", "BackoffLimitExceeded", "the database was down"),
		jobCondition("Complete", "True", "", "migrations applied"),
	)
	a := newReleaseAdapter(t, c)

	// The failed Job of the previous attempt is already in the cluster.
	if _, err := a.Apply(context.Background(), releaseSet(t)); err == nil {
		t.Fatal("the first attempt should have failed")
	}
	if _, err := a.Apply(context.Background(), releaseSet(t)); err != nil {
		t.Fatalf("the retry did not succeed: %v", err)
	}
	if !contains(c.deleteLog(), "jobs/"+testNS+"/"+releaseJobName) {
		t.Errorf("the failed release Job was not deleted before the retry: %v", c.deleteLog())
	}
	if c.get(t, "deployments", testNS, "checkout") == nil {
		t.Error("the workloads did not roll after the retried migration succeeded")
	}
}

// TestReleaseWaitEndsWithTheContext: an interrupted wait is an unknown answer,
// and unknown must never read as success — the workloads stay unapplied.
func TestReleaseWaitEndsWithTheContext(t *testing.T) {
	c := newCluster()
	c.scriptJobStatus(jobRunning())
	a := newReleaseAdapter(t, c)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()

	_, err := a.Apply(ctx, releaseSet(t))
	if err == nil {
		t.Fatal("a release command that never finished reported success")
	}
	if !delivery.AsReleaseFailed(err) {
		t.Fatalf("error is %v, want a delivery/release-failed", err)
	}
	if !strings.Contains(err.Error(), "interrupted") {
		t.Errorf("the error does not say the wait was interrupted: %v", err)
	}
	if c.get(t, "deployments", testNS, "checkout") != nil {
		t.Error("the Deployment was applied even though the migration never finished")
	}
}

// TestRollbackDoesNotRerunTheReleaseCommand pins the interaction the issue asks
// to be stated plainly: rolling the application back does not roll a migration
// back, and re-running the old revision's release command would only repeat
// work the database has already done.
func TestRollbackDoesNotRerunTheReleaseCommand(t *testing.T) {
	c := newCluster()
	c.scriptJobStatus(jobCondition("Complete", "True", "", "migrations applied"))
	a := newReleaseAdapter(t, c)

	if _, err := a.Apply(context.Background(), releaseSet(t)); err != nil {
		t.Fatalf("apply: %v", err)
	}
	entries, err := a.History(context.Background(), releaseSet(t))
	if err != nil || len(entries) != 1 {
		t.Fatalf("history = %v, %v", entries, err)
	}
	applies := countApplies(c.applyLog(), jobKey())

	if _, err := a.Rollback(context.Background(), releaseSet(t), entries[0]); err != nil {
		t.Fatalf("rollback: %v", err)
	}
	if got := countApplies(c.applyLog(), jobKey()); got != applies {
		t.Errorf("the rollback re-applied the release Job (%d applies, was %d)", got, applies)
	}
	if c.get(t, "deployments", testNS, "checkout") == nil {
		t.Error("the rollback did not apply the workloads")
	}
}

// TestReleaseHealthReadback covers the Status() half: an interrupted deploy
// must report the migration that is still running rather than a rollout that
// never happened.
func TestReleaseHealthReadback(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status map[string]any
		want   healthState
		cause  string
	}{
		{"complete", jobCondition("Complete", "True", "", "done"), healthOK, ""},
		{"running", jobRunning(), healthProgressing, "has not finished"},
		{"failed", jobCondition("Failed", "True", "BackoffLimitExceeded", "boom"), healthDegraded, "failed"},
		{"deadline", jobCondition("Failed", "True", "DeadlineExceeded", "too slow"), healthDegraded, "timeout"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			obj := &unstructured.Unstructured{Object: map[string]any{
				"apiVersion": "batch/v1",
				"kind":       "Job",
				"metadata": map[string]any{
					"name":   releaseJobName,
					"labels": map[string]any{labelReleaseHook: "true"},
				},
				"status": tc.status,
			}}
			state, cause := health(obj)
			if state != tc.want {
				t.Fatalf("health = %v, want %v (cause %q)", state, tc.want, cause)
			}
			if tc.cause != "" && !strings.Contains(cause, tc.cause) {
				t.Errorf("cause %q does not mention %q", cause, tc.cause)
			}
		})
	}
}

// TestNonReleaseJobHasNoHealthOpinion: unknown health must never read as
// unhealthy, so a Job kelson did not stamp is left alone.
func TestNonReleaseJobHasNoHealthOpinion(t *testing.T) {
	obj := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "batch/v1",
		"kind":       "Job",
		"metadata":   map[string]any{"name": "someone-elses-job"},
		"status":     jobCondition("Failed", "True", "BackoffLimitExceeded", "boom"),
	}}
	if state, _ := health(obj); state != healthOK {
		t.Fatalf("health = %v for a Job kelson does not own, want healthOK", state)
	}
}

// --- helpers ----------------------------------------------------------------

type logTailerFunc func(ctx context.Context, namespace, pod, container string) (string, error)

func (f logTailerFunc) ContainerLogs(ctx context.Context, namespace, pod, container string) (string, error) {
	return f(ctx, namespace, pod, container)
}

// releasePod is the pod the Job controller would have produced, labelled the
// way Kubernetes labels it so the adapter finds it.
func releasePod() *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1",
		"kind":       "Pod",
		"metadata": map[string]any{
			"name":      "release-pod",
			"namespace": testNS,
			"labels":    map[string]any{"job-name": releaseJobName},
		},
	}}
}

func countApplies(log []string, key string) int {
	n := 0
	for _, e := range log {
		if e == key {
			n++
		}
	}
	return n
}

func contains(log []string, want string) bool {
	for _, e := range log {
		if e == want {
			return true
		}
	}
	return false
}
