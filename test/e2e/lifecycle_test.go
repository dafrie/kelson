//go:build e2e

package e2e

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dafrie/kelson/internal/diff"
)

const (
	// baseImage is what testdata/minimal.yaml declares, and nextImage is what
	// the mutation step rewrites it to. Both are registry.k8s.io: no Docker Hub
	// anonymous rate limit, no authentication, and a few hundred kilobytes each.
	baseImage = "registry.k8s.io/pause:3.10"
	nextImage = "registry.k8s.io/pause:3.9"

	// rolloutTimeout covers an image pull on a cold node plus the rollout. It is
	// generous because the failure it guards against (a slow pull) is not the
	// failure the suite is looking for.
	rolloutTimeout = 4 * time.Minute
	// statusTimeout is the budget for the two planes status reads to agree once
	// the rollout has already settled. Short on purpose: past a minute this is
	// a disagreement, not a delay.
	statusTimeout = 90 * time.Second
)

// TestRenderApplyObserveLifecycle is what the delivery spine can prove end to
// end today, against a real API server.
//
// The suite used to run the whole loop through kelson: deploy, observe healthy,
// mutate, preview offline and against the live cluster, deploy the change, roll
// back, and verify the cluster itself reverted. ADR-0028 deleted the applier
// and the rollback, so what is left is the half that never needed them — render
// and both diff levels — plus the property that makes the deletion survivable:
// a kelson render is a flat set of standard manifests, so `kubectl apply` puts
// exactly it in the cluster and `kelson status` reads the workloads back.
//
// The deploy and rollback halves come back with issue #224, against the
// controller and the artifact; [TestDeletedVerbsRefuseHonestly] is what stands
// in for them until then.
func TestRenderApplyObserveLifecycle(t *testing.T) {
	h := newHarness(t, "kelson-e2e")
	const env = "e2e"

	// original.yaml stays as rendered from testdata; spec.yaml is the file the
	// mutation step rewrites. Keeping both is what makes `kelson diff --from`
	// meaningful.
	original := h.copyFixture("original.yaml")
	spec := h.copyFixture("spec.yaml")

	t.Log("== put the rendered set in the cluster ==")
	h.applyRendered(spec, env)
	h.waitForRollout("web", baseImage, rolloutTimeout)

	// The whole rendered set is live, not only the Deployment the poll watched.
	for _, want := range []string{"service/web", "serviceaccount/web"} {
		h.kubectlOK("-n", h.namespace, "get", want)
	}

	t.Log("== status reports the workload healthy, and says what it cannot report ==")
	// Status answers half of its question now (cmd/kelson/status.go): the
	// observation verdict is real, and the delivery phase is stated as missing
	// rather than guessed. Asserting both is the point — a gap that stopped
	// announcing itself would be the silent success this project refuses.
	h.waitForStatus(spec, env, statusTimeout,
		"Deployment/kelson-e2e/web healthy",
		"delivery phase: not reported",
	)

	t.Log("== mutate the spec ==")
	h.mutateImage(spec, baseImage, nextImage)

	t.Log("== diff (offline, L1) reports the image change ==")
	l1 := h.kelson("diff", "-f", spec, "--from", original, "--env", env, "--output", "json")
	assertDiffExitChanged(t, l1)
	assertImageChange(t, l1, baseImage, nextImage)

	t.Log("== diff (server-side dry run, L2) reports it against the live cluster ==")
	l2 := h.kelson("diff", "-f", spec, "--env", env, "--dry-run", "server", "--output", "json")
	assertDiffExitChanged(t, l2)
	assertImageChange(t, l2, baseImage, nextImage)

	t.Log("== the mutated set applies and the cluster rolls to it ==")
	h.applyRendered(spec, env)
	h.waitForRollout("web", nextImage, rolloutTimeout)

	t.Log("== status is healthy again on the new image ==")
	h.waitForStatus(spec, env, statusTimeout, "Deployment/kelson-e2e/web healthy")
}

// TestDeletedVerbsRefuseHonestly is what stands in for the deploy and rollback
// halves of the lifecycle until issue #224 lands.
//
// A gated verb is only acceptable if it refuses in a shape a caller can act on,
// and the one place that is worth proving against a real binary rather than a
// unit test is here: the exit code, the taxonomy code and the tracking issue,
// as a user's shell sees them.
func TestDeletedVerbsRefuseHonestly(t *testing.T) {
	h := newHarness(t, "kelson-e2e-gated")
	const env = "e2e"
	spec := h.copyFixture("spec.yaml")

	for _, args := range [][]string{
		{"deploy", "-f", spec, "--env", env, "--yes"},
		{"rollback", "-f", spec, "--env", env, "--yes"},
	} {
		res := h.kelson(args...)
		if res.code == 0 {
			t.Fatalf("`kelson %s` exited 0; the machinery behind it is deleted\n%s", strings.Join(args, " "), res.combined())
		}
		out := res.combined()
		for _, want := range []string{"delivery/not-implemented", "#224"} {
			if !strings.Contains(out, want) {
				t.Errorf("`kelson %s` did not name %q:\n%s", strings.Join(args, " "), want, out)
			}
		}
	}
}

// waitForStatus polls `kelson status` until its output contains every want.
//
// It polls rather than asserting once because status samples two planes at an
// instant, and the instant just after a rollout settles is the one where they
// are most likely to disagree by a second. On expiry the failure carries the
// full status output and the expectations it never satisfied.
func (h *harness) waitForStatus(spec, env string, timeout time.Duration, wants ...string) {
	h.t.Helper()
	h.waitFor("kelson status to report "+strings.Join(wants, " + "), timeout, func() (bool, string) {
		res := h.kelson("status", "-f", spec, "--env", env)
		if res.code != 0 {
			return false, fmt.Sprintf("status exited %d\n%s", res.code, res.combined())
		}
		var missing []string
		for _, want := range wants {
			if !strings.Contains(res.stdout, want) {
				missing = append(missing, want)
			}
		}
		if len(missing) > 0 {
			return false, fmt.Sprintf("still missing %q in:\n%s", missing, strings.TrimSpace(res.stdout))
		}
		return true, "every expectation present"
	})
}

// assertDiffExitChanged holds `kelson diff` to its CI exit contract (issue #46):
// 2 means changes are present. 0 would mean the mutation did not reach the
// preview at all, and 3 would mean the preview found a blocker — on a stock
// kind cluster with no policy engine, that is a finding worth failing on.
func assertDiffExitChanged(t *testing.T, res result) {
	t.Helper()
	if res.code != 2 {
		t.Fatalf("kelson diff: want exit 2 (changes present), got %d\n%s", res.code, res.combined())
	}
}

// assertImageChange decodes the structured diff and requires that it names the
// container image moving from before to after. The JSON is decoded into the
// same type the CLI encodes (internal/diff), so a change to that contract shows
// up here as a compile error rather than a silently passing assertion.
func assertImageChange(t *testing.T, res result, before, after string) {
	t.Helper()
	var d diff.Diff
	if err := json.Unmarshal([]byte(res.stdout), &d); err != nil {
		t.Fatalf("parsing `kelson diff --output json`: %v\n%s", err, res.combined())
	}
	for _, r := range d.Resources {
		if r.Kind != "Deployment" || r.Name != "web" {
			continue
		}
		for _, f := range r.Fields {
			if !strings.HasSuffix(f.Path, "image") {
				continue
			}
			if f.Before == before && f.After == after {
				return
			}
			t.Fatalf("diff reports %s changing from %v to %v, want %q to %q\n%s",
				f.Path, f.Before, f.After, before, after, res.stdout)
		}
	}
	t.Fatalf("diff does not report an image change on Deployment/web (%d resources)\n%s", len(d.Resources), res.stdout)
}

// --- fixture handling -------------------------------------------------------

// copyFixture writes testdata/minimal.yaml into the test's own working
// directory under the given name. Tests mutate their copy; the checked-in
// fixture is never touched.
func (h *harness) copyFixture(name string) string {
	h.t.Helper()
	src := filepath.Join(repoRoot, "test", "e2e", "testdata", "minimal.yaml")
	body, err := os.ReadFile(src)
	if err != nil {
		h.t.Fatalf("reading the e2e fixture %s: %v", src, err)
	}
	if !strings.Contains(string(body), baseImage) {
		h.t.Fatalf("the fixture %s no longer declares %s, so the mutation step would be a no-op", src, baseImage)
	}
	dst := filepath.Join(h.work, name)
	if err := os.WriteFile(dst, body, 0o600); err != nil {
		h.t.Fatalf("writing %s: %v", dst, err)
	}
	return dst
}

// mutateImage rewrites the fixture copy in place. Editing the spec file is the
// closest thing the harness has to a user changing their mind, which is the
// event diff and rollback exist to serve.
func (h *harness) mutateImage(path, from, to string) {
	h.t.Helper()
	body, err := os.ReadFile(path) //nolint:gosec // path is this test's own temp file
	if err != nil {
		h.t.Fatalf("reading %s: %v", path, err)
	}
	updated := strings.ReplaceAll(string(body), from, to)
	if updated == string(body) {
		h.t.Fatalf("%s contains no %q to rewrite", path, from)
	}
	if err := os.WriteFile(path, []byte(updated), 0o600); err != nil {
		h.t.Fatalf("writing %s: %v", path, err)
	}
	h.t.Logf("rewrote the image in %s: %s -> %s", filepath.Base(path), from, to)
}
