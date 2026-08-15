//go:build e2e

// Package e2e drives the real kelson CLI against a real Kubernetes API server
// (issue #86). It is the suite that covers what golden files cannot: the
// delivery adapters, status correlation, and the cluster's own answer.
//
// # How it is gated
//
// Two gates, both deliberate. The `e2e` build tag keeps this package out of
// `go test ./...` entirely, and KELSON_E2E=1 is required at run time — so
// `go test -tags e2e ./...` on a laptop with no cluster skips rather than
// fails. Opting in with KELSON_E2E=1 is a promise that a cluster is reachable:
// from there an unreachable API server is a failure, never a skip, because a
// harness that silently skips is a harness that silently stops proving
// anything.
//
// # What it talks to
//
// The CLI as a subprocess, and kubectl as the independent witness. Nothing
// here imports a Kubernetes client: the command plane's lint allow-list
// forbids it (.golangci.yml, the `main` depguard rule), and more importantly a
// test that asserts through the same client the code under test uses is a
// weaker test than one that asks kubectl.
//
// # Debuggability from logs
//
// Nobody watches this run. Every command is echoed with its exit code, every
// poll prints what it last observed when its deadline expires, and any failed
// test dumps the namespace — resources, pod descriptions and events — before
// it returns. A CI log that does not explain its own failure is a bug in this
// file.
package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"
)

const (
	// enableEnv must be "1" for these tests to run. See the package doc.
	enableEnv = "KELSON_E2E"
	// binEnv names a prebuilt kelson binary. CI builds one and points at it;
	// an unset value makes the harness build its own.
	binEnv = "KELSON_E2E_BIN"
	// kubeconfigEnv overrides the ambient KUBECONFIG for this suite only.
	kubeconfigEnv = "KELSON_E2E_KUBECONFIG"

	// pollInterval is how often a wait re-reads the cluster. Two seconds is
	// slow enough not to hammer the API server and fast enough that a 2-minute
	// budget still prints ~60 observations into the log.
	pollInterval = 2 * time.Second
	// commandTimeout bounds any single subprocess. It is larger than the
	// deploy budget the tests pass to `kelson deploy` so that the CLI's own
	// timeout, which produces a diagnosis, always fires first.
	commandTimeout = 8 * time.Minute
)

var (
	// enabled records whether KELSON_E2E opted this run in.
	enabled bool
	// kelsonBin is the CLI under test.
	kelsonBin string
	// kubeconfig is the kubeconfig every subprocess inherits, empty to use the
	// ambient one.
	kubeconfig string
	// repoRoot is the checkout this package lives in.
	repoRoot string
)

func TestMain(m *testing.M) {
	if os.Getenv(enableEnv) == "1" {
		enabled = true
		if err := setup(); err != nil {
			fmt.Fprintf(os.Stderr, "e2e setup failed: %v\n", err)
			os.Exit(1)
		}
	}
	os.Exit(m.Run())
}

// setup resolves the binary and proves a cluster is reachable before any test
// runs. Failing here rather than inside the first test keeps a missing
// prerequisite from being reported as a broken deploy.
func setup() error {
	root, err := findRepoRoot()
	if err != nil {
		return err
	}
	repoRoot = root

	kubeconfig = os.Getenv(kubeconfigEnv)

	if bin := os.Getenv(binEnv); bin != "" {
		// A relative path resolves against the repository root, not the
		// process's working directory: `go test ./test/e2e/...` runs the binary
		// from the package directory, so "hack/bin/kelson" would otherwise be
		// looked for under test/e2e/.
		abs := bin
		if !filepath.IsAbs(abs) {
			abs = filepath.Join(root, abs)
		}
		if _, err := os.Stat(abs); err != nil {
			return fmt.Errorf("%s points at %s, which is not there: %w", binEnv, abs, err)
		}
		kelsonBin = abs
	} else {
		bin, err := buildCLI(root)
		if err != nil {
			return err
		}
		kelsonBin = bin
	}

	if _, err := exec.LookPath("kubectl"); err != nil {
		return fmt.Errorf("kubectl is not on PATH, and this suite uses it as the independent witness: %w", err)
	}
	return checkCluster()
}

// buildCLI compiles the binary under test into a temporary directory. CI
// prefers KELSON_E2E_BIN (one build, reused), but a local `go test -tags e2e`
// should not require a separate build step first.
func buildCLI(root string) (string, error) {
	dir, err := os.MkdirTemp("", "kelson-e2e-bin")
	if err != nil {
		return "", err
	}
	bin := filepath.Join(dir, "kelson")
	cmd := exec.Command("go", "build", "-o", bin, "./cmd/kelson")
	cmd.Dir = root
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("building ./cmd/kelson: %w\n%s", err, out)
	}
	return bin, nil
}

// checkCluster is the "you promised me a cluster" gate. Its error names the
// two ways to get one so a failed CI run does not need a maintainer to
// remember them.
func checkCluster() error {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, "kubectl", "get", "--raw=/readyz")
	cmd.Env = childEnv()
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s=1 was set but no Kubernetes API server answered /readyz: %w\n"+
			"output: %s\n"+
			"Create one with `hack/e2e/up.sh` (kind, pinned version) and export the KUBECONFIG it prints,\n"+
			"or point %s at an existing kubeconfig.",
			enableEnv, err, strings.TrimSpace(string(out)), kubeconfigEnv)
	}
	return nil
}

// findRepoRoot walks up from this source file. The harness shells out to `go
// build` and reads testdata by absolute path, so it must not depend on the
// working directory the runner chose.
func findRepoRoot() (string, error) {
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		return "", fmt.Errorf("cannot locate the e2e package on disk")
	}
	dir := filepath.Dir(file)
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", fmt.Errorf("no go.mod above %s", filepath.Dir(file))
		}
		dir = parent
	}
}

func childEnv() []string {
	env := os.Environ()
	if kubeconfig != "" {
		env = append(env, "KUBECONFIG="+kubeconfig)
	}
	return env
}

// --- the per-test harness ---------------------------------------------------

// harness is one test's view of the world: where the binary is and which
// namespace it is working in.
//
// It used to carry a per-test rendered-history directory as well, because the
// suite deployed through the direct adapter and rolled back through its
// journal. ADR-0028 deleted both; what the suite applies with now is
// [harness.applyRendered], which is `kelson render | kubectl apply -f -` — the
// same manifests, put in the cluster by a tool that is not kelson, which is
// also the anti-lock-in property ADR-0028 decision 10 claims.
type harness struct {
	t         *testing.T
	namespace string
	work      string
}

// newHarness skips when the suite was not opted into, and otherwise returns a
// harness whose namespace is dumped into the log if the test fails.
func newHarness(t *testing.T, namespace string) *harness {
	t.Helper()
	if !enabled {
		t.Skipf("%s is not 1: set %s=1 with a reachable cluster to run the end-to-end suite", enableEnv, enableEnv)
	}
	h := &harness{
		t:         t,
		namespace: namespace,
		work:      t.TempDir(),
	}
	t.Cleanup(func() {
		if t.Failed() {
			h.dumpNamespace()
		}
	})
	return h
}

// result is one subprocess run. Nothing here fails the test on its own: the
// caller decides what a given exit code means, because `kelson diff` exits 2
// on a change by design (issue #46).
type result struct {
	stdout string
	stderr string
	code   int
}

// combined is what to print when a result is surprising: both streams, in one
// blob, because the interesting line is as often on stderr as on stdout.
func (r result) combined() string {
	var b strings.Builder
	if s := strings.TrimRight(r.stdout, "\n"); s != "" {
		b.WriteString("stdout:\n" + s + "\n")
	}
	if s := strings.TrimRight(r.stderr, "\n"); s != "" {
		b.WriteString("stderr:\n" + s + "\n")
	}
	if b.Len() == 0 {
		return "(no output)\n"
	}
	return b.String()
}

func (h *harness) run(name string, args ...string) result {
	h.t.Helper()
	res, elapsed := h.runQuiet(name, args...)
	h.t.Logf("$ %s %s\n  -> exit %d in %s\n%s",
		filepath.Base(name), strings.Join(args, " "), res.code, elapsed, res.combined())
	return res
}

// runQuiet is run without the echo, for the one caller that runs the same
// command a hundred times: a poll.
//
// A wait that re-reads `-o json` every two seconds for six minutes echoes the
// whole object about a hundred and eighty times, which is thousands of lines of
// duplicate YAML in a CI log — enough, in the delivery spine's first red run, to
// push the *earlier* tests' output out of the retrievable window entirely. The
// state a reader needs is not every observation; it is the one that changed
// (waitFor logs those) and the whole object at the end (dumpSpine prints it).
//
// It returns the elapsed time as well so run can report it without timing the
// command twice.
func (h *harness) runQuiet(name string, args ...string) (result, time.Duration) {
	h.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), commandTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = h.work
	cmd.Env = childEnv()
	var stdout, stderr strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	started := time.Now()
	err := cmd.Run()
	elapsed := time.Since(started).Round(time.Millisecond)
	res := result{stdout: stdout.String(), stderr: stderr.String()}
	switch {
	case err == nil:
		res.code = 0
	case cmd.ProcessState != nil:
		res.code = cmd.ProcessState.ExitCode()
	default:
		res.code = -1
		res.stderr += "\n" + err.Error()
	}

	if ctx.Err() != nil {
		// Echoed here rather than left to the caller: a command that ran out of
		// time is exactly the one whose partial output is worth seeing, and the
		// Fatalf below means the caller never gets to print it.
		h.t.Logf("$ %s %s\n  -> timed out after %s\n%s",
			filepath.Base(name), strings.Join(args, " "), elapsed, res.combined())
		h.t.Fatalf("%s %s did not finish within %s", name, strings.Join(args, " "), commandTimeout)
	}
	return res, elapsed
}

// kelson runs the CLI under test.
func (h *harness) kelson(args ...string) result {
	h.t.Helper()
	return h.run(kelsonBin, args...)
}

// kelsonOK runs the CLI and fails the test unless it exits 0.
func (h *harness) kelsonOK(args ...string) result {
	h.t.Helper()
	res := h.kelson(args...)
	if res.code != 0 {
		h.t.Fatalf("kelson %s: want exit 0, got %d\n%s", strings.Join(args, " "), res.code, res.combined())
	}
	return res
}

func (h *harness) kubectl(args ...string) result {
	h.t.Helper()
	return h.run("kubectl", args...)
}

// kubectlOK runs kubectl and fails the test unless it exits 0. kubectl is the
// witness, so a witness that cannot answer is a broken test, not a finding.
func (h *harness) kubectlOK(args ...string) result {
	h.t.Helper()
	res := h.kubectl(args...)
	if res.code != 0 {
		h.t.Fatalf("kubectl %s: want exit 0, got %d\n%s", strings.Join(args, " "), res.code, res.combined())
	}
	return res
}

// --- waiting ----------------------------------------------------------------

// waitFor polls check until it reports satisfied, then returns. On expiry it
// fails with the description and the last observation, so the log says what
// the cluster actually held instead of only what was expected.
//
// check returns (satisfied, observation). The observation is logged whenever
// it changes, which turns a timeout into a story rather than a single line.
func (h *harness) waitFor(desc string, timeout time.Duration, check func() (bool, string)) {
	h.t.Helper()
	deadline := time.Now().Add(timeout)
	last := ""
	for {
		ok, observed := check()
		if observed != last {
			h.t.Logf("waiting for %s: %s", desc, observed)
			last = observed
		}
		if ok {
			return
		}
		if time.Now().After(deadline) {
			h.t.Fatalf("timed out after %s waiting for %s\nlast observed: %s", timeout, desc, observed)
		}
		time.Sleep(pollInterval)
	}
}

// waitForRollout waits until the named Deployment reports the expected image on
// its pod template AND has that image live on a ready pod. Both halves matter:
// the template alone says the apply landed, the pod says the cluster acted on
// it.
func (h *harness) waitForRollout(deployment, wantImage string, timeout time.Duration) {
	h.t.Helper()
	desc := fmt.Sprintf("deployment/%s in %s to run %s", deployment, h.namespace, wantImage)
	h.waitFor(desc, timeout, func() (bool, string) {
		tmpl := h.kubectl("-n", h.namespace, "get", "deployment", deployment,
			"-o", "jsonpath={.spec.template.spec.containers[0].image} {.status.updatedReplicas}/{.status.replicas} available={.status.availableReplicas}")
		if tmpl.code != 0 {
			return false, "deployment not readable: " + strings.TrimSpace(tmpl.combined())
		}
		pods := h.kubectl("-n", h.namespace, "get", "pods",
			"-l", "kelson.dev/component="+deployment,
			"-o", "jsonpath={range .items[*]}{.metadata.name}={.status.phase}/{.spec.containers[0].image} {end}")
		observed := strings.TrimSpace(tmpl.stdout) + " | pods: " + strings.TrimSpace(pods.stdout)

		fields := strings.Fields(tmpl.stdout)
		if len(fields) < 3 || fields[0] != wantImage {
			return false, observed
		}
		if !strings.HasSuffix(fields[2], "=1") {
			return false, observed
		}
		if fields[1] != "1/1" {
			return false, observed
		}
		return true, observed
	})
}

// --- diagnostics ------------------------------------------------------------

// dumpNamespace prints everything a maintainer would ask for first. It runs
// only on failure, from t.Cleanup, and never fails the test itself — a dump
// that could fail would hide the failure it was called to explain.
func (h *harness) dumpNamespace() {
	h.t.Logf("--- diagnostics for namespace %s ---", h.namespace)
	for _, args := range [][]string{
		{"get", "all,configmaps,serviceaccounts", "-n", h.namespace, "-o", "wide", "--show-labels"},
		{"describe", "pods", "-n", h.namespace},
		{"get", "events", "-n", h.namespace, "--sort-by=.lastTimestamp"},
		{"get", "namespace", h.namespace, "-o", "yaml"},
	} {
		res := h.kubectl(args...)
		if res.code != 0 {
			h.t.Logf("diagnostic `kubectl %s` failed (exit %d) — continuing", strings.Join(args, " "), res.code)
		}
	}
}

// --- reading the cluster ----------------------------------------------------

// objectList is the shape of `kubectl get ... -o json` this suite reads. Only
// the identity of each object is needed: the tests compare sets of resources,
// never their contents.
type objectList struct {
	Items []struct {
		Kind     string `json:"kind"`
		Metadata struct {
			Name      string `json:"name"`
			Namespace string `json:"namespace"`
		} `json:"metadata"`
	} `json:"items"`
}

// listKeys returns "Kind/name" for every object of the given types matching the
// selector, sorted by the map's own iteration being avoided — callers compare
// sets, so the return is a set.
func (h *harness) listKeys(types, selector string) map[string]bool {
	h.t.Helper()
	args := []string{"-n", h.namespace, "get", types, "-o", "json"}
	if selector != "" {
		args = append(args, "-l", selector)
	}
	res := h.kubectlOK(args...)

	var list objectList
	if err := json.Unmarshal([]byte(res.stdout), &list); err != nil {
		h.t.Fatalf("parsing `kubectl get %s -o json`: %v\n%s", types, err, res.stdout)
	}
	keys := make(map[string]bool, len(list.Items))
	for _, item := range list.Items {
		keys[item.Kind+"/"+item.Metadata.Name] = true
	}
	return keys
}

// sortedKeys renders a set for a failure message.
func sortedKeys(set map[string]bool) string {
	if len(set) == 0 {
		return "(none)"
	}
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	slices.Sort(out)
	return strings.Join(out, ", ")
}

// applyRendered renders a spec offline and applies the result with kubectl.
//
// This is what the suite uses to put a set in the cluster now that the delivery
// verbs are gated (ADR-0028, issue #224). It is not a workaround: it is the
// property ADR-0028 decision 10 relies on — a kelson render is a flat set of
// standard manifests that a tool which is not kelson can apply — and it is the
// same set the controller will publish as an artifact for Flux to apply, so the
// sweeps these tests do afterwards are testing the objects that will really be
// there.
//
// What it deliberately does NOT reproduce is anything kelson's own applier did
// beyond apply: no wait for readiness, no prune, no revision. Tests that need
// those wait on the cluster themselves (waitForRollout).
func (h *harness) applyRendered(spec, env string) {
	h.t.Helper()
	rendered := h.kelsonOK("render", "-f", spec, "--env", env)

	ctx, cancel := context.WithTimeout(context.Background(), commandTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "kubectl", "apply", "-f", "-")
	cmd.Dir = h.work
	cmd.Env = childEnv()
	cmd.Stdin = strings.NewReader(rendered.stdout)
	var stdout, stderr strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		h.t.Fatalf("kubectl apply of the rendered set failed: %v\n%s%s", err, stdout.String(), stderr.String())
	}
	h.t.Logf("$ kelson render -f %s --env %s | kubectl apply -f -\n%s", spec, env, stdout.String())
}
