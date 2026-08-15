//go:build e2e

// The R1 exit gate (ADR-0028, docs/roadmap.md): a spec deployed end to end on a
// kind cluster, as custom resources, through the controller.
//
// Everything else in this package drives the CLI and applies its output with
// kubectl. This file applies nothing but a Project and an Environment, and then
// asserts on what the *cluster* did about them: an OCI artifact appeared in a
// registry under a tag derived from the object's generation, an OCIRepository
// and a Kustomization appeared carrying kelson's provenance, workloads appeared
// in a third namespace, and `.status` said all of it out loud.
//
// # What it talks to, and what it refuses to talk to
//
// kubectl, as everywhere else in this suite: the independent witness. Nothing
// here holds a Kubernetes client — the command plane's depguard allow-list
// forbids one, and a test that reads through the same client the controller
// writes with proves less than one that asks the API server separately.
//
// The kelson packages imported here are imported for their *names* and never
// for their behaviour: the annotation keys the publisher writes
// (internal/artifact, internal/controller), the labels the delivery plane
// stamps (internal/delivery), the registry's own address
// (internal/delivery/install), the tag grammar (internal/model's ShortHash) and
// the status vocabulary (api/kelson/v1alpha1). A rename on either side is then
// a compile error here rather than a test that silently stops checking
// anything — the same argument lifecycle_test.go makes for decoding
// `kelson diff` into internal/diff's own types.
//
// # Prerequisites, and why a missing one fails rather than skips
//
// This scenario needs Flux, an in-cluster registry and kelson-controller, none
// of which a bare `hack/e2e/up.sh` cluster has. `hack/e2e/spine.sh` provisions
// all three and `make test-e2e` runs it first. A cluster without them is a
// harness that was set up wrong, so it is a loud failure naming the script —
// never a skip, because a skipped exit gate is an exit gate that stopped
// existing.
package e2e

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dafrie/kelson/api/kelson/v1alpha1"
	"github.com/dafrie/kelson/internal/artifact"
	"github.com/dafrie/kelson/internal/controller"
	"github.com/dafrie/kelson/internal/delivery"
	"github.com/dafrie/kelson/internal/delivery/install"
	"github.com/dafrie/kelson/internal/model"
)

const (
	// The three namespaces the scenario spans, kept distinct on purpose (see
	// testdata/spine.yaml): the custom resources live in one, the Flux pair
	// kelson writes lives in the Flux namespace, and the workloads land in a
	// third.
	spineProject   = "kelson-spine"
	spineEnvName   = "live"
	spineNamespace = "kelson-spine"
	spineAppNS     = "kelson-spine-app"
	spineComponent = "web"

	// spinePublishTimeout covers the first revision: a render, a push, Flux
	// pulling the artifact, a kustomize apply, and a cold image pull. Generous,
	// because the failure it would otherwise produce (a slow pull) is not the
	// failure this test is looking for.
	spinePublishTimeout = 6 * time.Minute
	// spineSettleTimeout covers every later transition, where the image is
	// already on the node.
	spineSettleTimeout = 4 * time.Minute
	// spineQuietWindow is how long "nothing happened" is observed for before it
	// is believed. It is the one assertion in this file that cannot converge —
	// it can only fail — so it is bounded by patience rather than by a
	// deadline.
	spineQuietWindow = 45 * time.Second

	// spineResumedReplicas is what the resuming spec edit asks for. Two rather
	// than one so the resumed revision is distinguishable from the rolled-back
	// one by a second, independent field as well as by its image.
	spineResumedReplicas = 2
)

// TestDeliverySpine is the R1 exit gate.
//
// One linear scenario, because every step depends on the state the previous one
// left: revision 2 cannot exist before revision 1, and there is nothing to roll
// back to until both do. Subtests would suggest an independence that is not
// there.
//
//	1  apply a Project and an Environment          → Ready, Healthy, revision 1
//	2  the registry holds revision 1               → tag, provenance annotations
//	3  the Flux pair exists and is kelson's        → labels, pin, prune, wait
//	4  the workload is live                        → the image the spec names
//	5  edit the spec                               → revision 2, both in history
//	6  annotate kelson.dev/rollback-to=revision 1  → the deployed bytes revert
//	7  nothing republishes while pinned            → the quiet window
//	8  edit the spec under the pin                 → revision 3 is published
//	9  remove the annotation                       → revision 3 goes live
func TestDeliverySpine(t *testing.T) {
	h := newHarness(t, spineNamespace)
	h.requireSpinePrerequisites()

	t.Cleanup(h.dumpSpine)
	t.Cleanup(func() {
		// A failed run leaves everything standing: the namespaces, the Flux
		// pair, the artifacts. That is what makes `kubectl -n kelson-spine
		// describe environment live` worth typing after a red CI job.
		if t.Failed() {
			t.Logf("the test failed: leaving %s, %s and the Flux pair in place for inspection",
				spineNamespace, spineAppNS)
			return
		}
		h.removeSpineFixture()
	})

	t.Log("== apply the Project and the Environment as custom resources ==")
	h.kubectlOK("apply", "-f", filepath.Join(repoRoot, "test", "e2e", "testdata", "spine.yaml"))

	env := h.awaitEnvironment("the first revision to be published and healthy", spinePublishTimeout,
		func(e *v1alpha1.Environment) (bool, string) { return h.settledHealthy(e) })
	gen1, rev1 := env.Generation, env.Status.Revision
	h.assertRevisionShape(env, rev1, gen1)
	t.Logf("revision %s is live for generation %d", rev1, gen1)

	if len(env.Status.History) != 1 {
		t.Fatalf("status.history holds %d entries after one publish, want 1: %s",
			len(env.Status.History), historyLine(env))
	}
	h.assertHistoryEntry(env.Status.History[0], rev1, baseImage)

	t.Log("== the registry holds the artifact, under the tag the generation names ==")
	repository := h.assertArtifactRepository(env)
	registry := h.registryProxy()
	h.assertTagsInclude(registry, repository, rev1)
	h.assertArtifactProvenance(registry, repository, rev1, gen1, env.Status.History[0])

	t.Log("== the Flux pair exists, is kelson's, and consumes that artifact ==")
	h.assertFluxPair(rev1, h.historyDigestFor(env, rev1))

	t.Log("== the workload the artifact carries is live ==")
	h.awaitWorkload("the first revision's workload", baseImage, 1, spineSettleTimeout)

	t.Log("== editing the spec publishes a second revision ==")
	h.patchEnvironment(fmt.Sprintf(
		`{"spec":{"components":[{"name":%q,"image":%q}]}}`, spineComponent, nextImage))

	env = h.awaitEnvironment("the second revision to be published and healthy", spineSettleTimeout,
		func(e *v1alpha1.Environment) (bool, string) {
			if e.Status.Revision == rev1 {
				return false, "still serving " + rev1
			}
			return h.settledHealthy(e)
		})
	gen2, rev2 := env.Generation, env.Status.Revision
	if gen2 <= gen1 {
		t.Fatalf("a spec edit did not bump .metadata.generation: %d then %d", gen1, gen2)
	}
	h.assertRevisionShape(env, rev2, gen2)
	if len(env.Status.History) != 2 {
		t.Fatalf("status.history holds %d entries after two publishes, want 2: %s",
			len(env.Status.History), historyLine(env))
	}
	// Newest first, and the older entry is untouched: the history is a record,
	// not a cache of the current state (ADR-0028 decision 4).
	h.assertHistoryEntry(env.Status.History[0], rev2, nextImage)
	h.assertHistoryEntry(env.Status.History[1], rev1, baseImage)

	h.assertTagsInclude(registry, repository, rev1, rev2)
	h.assertArtifactProvenance(registry, repository, rev2, gen2, env.Status.History[0])
	h.assertFluxPair(rev2, h.historyDigestFor(env, rev2))
	h.awaitWorkload("the second revision's workload", nextImage, 1, spineSettleTimeout)

	t.Log("== rolling back moves the pointer to bytes that already exist ==")
	h.kubectlOK("-n", spineNamespace, "annotate", "environment", spineEnvName,
		v1alpha1.AnnotationRollbackTo+"="+rev1, "--overwrite")

	env = h.awaitEnvironment("the rollback to be in force", spineSettleTimeout,
		func(e *v1alpha1.Environment) (bool, string) {
			ready := conditionOf(e, v1alpha1.ConditionReady)
			progressing := conditionOf(e, v1alpha1.ConditionProgressing)
			switch {
			case e.Status.Revision != rev1:
				return false, "still serving " + e.Status.Revision
			case ready.Reason != v1alpha1.ReasonRolledBack || ready.Status != "True":
				return false, "Ready is " + ready.String()
			case progressing.Reason != v1alpha1.ReasonRollbackPinned || progressing.Status != "False":
				return false, "Progressing is " + progressing.String()
			}
			return true, "pinned to " + rev1
		})

	if env.Status.RollbackRevision != rev1 {
		t.Errorf("status.rollbackRevision = %q, want %q — a rollback that is in force must say which one",
			env.Status.RollbackRevision, rev1)
	}
	if env.Status.RollbackGeneration != gen2 {
		t.Errorf("status.rollbackGeneration = %d, want %d (the generation the annotation arrived at); "+
			"it is what decides whether a later spec edit is new intent",
			env.Status.RollbackGeneration, gen2)
	}
	// A rollback publishes nothing and records nothing: the tag it pins is
	// already in the history, which is what verifyRollbackTarget checked.
	if len(env.Status.History) != 2 {
		t.Errorf("status.history holds %d entries after a rollback, want the same 2: %s",
			len(env.Status.History), historyLine(env))
	} else if head := env.Status.History[0].Revision; head != rev2 {
		t.Errorf("status.history head is %q after a rollback, want %q: a rollback republishes nothing, "+
			"so it must not prepend an entry", head, rev2)
	}
	if msg := conditionOf(env, v1alpha1.ConditionProgressing).Message; !strings.Contains(msg, v1alpha1.AnnotationRollbackTo) {
		t.Errorf("the Progressing message does not name %s, so an operator asking why their edit will not "+
			"deploy is not answered by `kubectl describe`: %q", v1alpha1.AnnotationRollbackTo, msg)
	}

	// The assertion the whole feature exists for: the pointer moved, so the
	// cluster is running the older bytes again — while the spec on the object
	// still says otherwise.
	h.assertFluxPair(rev1, h.historyDigestFor(env, rev1))
	h.awaitWorkload("the rolled-back workload", baseImage, 1, spineSettleTimeout)

	t.Log("== nothing is republished while the pin is in force ==")
	h.assertSpineStable("the pinned revision", spineQuietWindow, func(e *v1alpha1.Environment) error {
		if e.Status.Revision != rev1 {
			return fmt.Errorf("status.revision moved to %q while pinned to %q", e.Status.Revision, rev1)
		}
		if len(e.Status.History) != 2 {
			return fmt.Errorf("status.history grew to %d entries while pinned: %s", len(e.Status.History), historyLine(e))
		}
		return nil
	})
	h.assertTagsInclude(registry, repository, rev1, rev2)

	t.Log("== a spec edit under the pin is new intent, and publishes ==")
	// ADR-0028 decision 5 names two ways out of a rollback: remove the
	// annotation, or edit the spec. This is the second one: the edit
	// publishes, and the standing annotation goes *inert* — a stable state
	// whose bookkeeping (status.rollbackRevision at the generation the
	// rollback arrived) survives the reconcile so the annotation is not
	// re-read as a new rollback. (It once was: the inert reconcile cleared
	// the fields and the environment flapped back to the pinned tag one
	// reconcile after the edit. Caught while writing this stage; the
	// regression test lives in internal/controller/reconcile_test.go.)
	h.patchEnvironment(fmt.Sprintf(
		`{"spec":{"components":[{"name":%q,"image":%q,"replicas":{"min":%d}}]}}`,
		spineComponent, nextImage, spineResumedReplicas))

	env = h.awaitEnvironment("the third revision to be published", spineSettleTimeout,
		func(e *v1alpha1.Environment) (bool, string) {
			if len(e.Status.History) < 3 {
				return false, "history still holds " + historyLine(e)
			}
			return true, "history holds " + historyLine(e)
		})
	gen3, rev3 := env.Generation, env.Status.History[0].Revision
	if gen3 <= gen2 {
		t.Fatalf("the second spec edit did not bump .metadata.generation: %d then %d", gen2, gen3)
	}
	if !strings.HasPrefix(rev3, strconv.FormatInt(gen3, 10)+"-") {
		t.Fatalf("the newest history entry is %q, which is not a revision of generation %d", rev3, gen3)
	}
	h.assertTagsInclude(registry, repository, rev1, rev2, rev3)
	h.assertArtifactProvenance(registry, repository, rev3, gen3, env.Status.History[0])

	// The annotation is still on the object and now inert: the bookkeeping is
	// retained (that is what keeps it inert), the condition says so out loud,
	// and the pointer follows the *edit*, not the stale pin.
	env = h.awaitEnvironment("the standing annotation to go inert", spineSettleTimeout,
		func(e *v1alpha1.Environment) (bool, string) {
			progressing := conditionOf(e, v1alpha1.ConditionProgressing)
			if !strings.Contains(progressing.Message, "inert") {
				return false, "Progressing is " + progressing.String()
			}
			return true, "Progressing says the annotation is inert"
		})
	if env.Status.RollbackRevision != rev1 || env.Status.RollbackGeneration != gen2 {
		t.Errorf("the inert rollback lost its bookkeeping (%q at generation %d, want %q at %d); "+
			"without it the next reconcile re-reads the annotation as a new rollback and re-pins",
			env.Status.RollbackRevision, env.Status.RollbackGeneration, rev1, gen2)
	}
	h.assertFluxPair(rev3, h.historyDigestFor(env, rev3))

	t.Log("== removing the annotation resumes tracking the spec ==")
	h.kubectlOK("-n", spineNamespace, "annotate", "environment", spineEnvName,
		v1alpha1.AnnotationRollbackTo+"-")

	env = h.awaitEnvironment("the environment to track its spec again", spineSettleTimeout,
		func(e *v1alpha1.Environment) (bool, string) {
			if e.Status.Revision != rev3 {
				return false, "still serving " + e.Status.Revision
			}
			if e.Status.RollbackRevision != "" {
				return false, "status.rollbackRevision still says " + e.Status.RollbackRevision
			}
			return h.settledHealthy(e)
		})
	if reason := conditionOf(env, v1alpha1.ConditionReady).Reason; reason != v1alpha1.ReasonReady {
		t.Errorf("Ready reason is %q after the annotation was removed, want %q", reason, v1alpha1.ReasonReady)
	}
	if _, ok := env.Annotations[v1alpha1.AnnotationRollbackTo]; ok {
		t.Errorf("%s is still on the object after `kubectl annotate ...-`", v1alpha1.AnnotationRollbackTo)
	}
	h.assertFluxPair(rev3, h.historyDigestFor(env, rev3))
	h.awaitWorkload("the resumed workload", nextImage, spineResumedReplicas, spineSettleTimeout)
	t.Logf("the spine published %s, %s and %s, rolled back to %s and resumed on %s", rev1, rev2, rev3, rev1, rev3)
}

// --- prerequisites ----------------------------------------------------------

// requireSpinePrerequisites proves the cluster is the one hack/e2e/spine.sh
// builds before a single assertion runs.
//
// Each check names the script, because the failure it guards against is a
// cluster set up by hand or by an older harness — and "the Environment never
// went Ready" six minutes later is a much worse way to learn that Flux is
// absent than "there is no OCIRepository CRD in this cluster".
func (h *harness) requireSpinePrerequisites() {
	h.t.Helper()
	const fix = "\nProvision it with `hack/e2e/spine.sh` (or `make test-e2e`, which runs it first)."

	for _, crd := range []string{
		"environments.kelson.dev",
		"ocirepositories.source.toolkit.fluxcd.io",
		"kustomizations.kustomize.toolkit.fluxcd.io",
	} {
		if res := h.kubectl("get", "crd", crd); res.code != 0 {
			h.t.Fatalf("the %s CRD is not served by this cluster, so the delivery spine cannot run.%s\n%s",
				crd, fix, res.combined())
		}
	}

	// `kubectl wait` answers "is it there?" and "is it up?" in one call, and
	// tolerates the case this is most likely to meet: spine.sh restarted the
	// controller a moment ago and the new pod is still starting.
	if res := h.kubectl("-n", install.RegistryNamespace, "wait", "--for=condition=Available",
		"deploy/"+install.RegistryName, "--timeout=180s"); res.code != 0 {
		h.t.Fatalf("no available %s deployment in %s, so there is nowhere to publish an artifact.%s\n%s",
			install.RegistryName, install.RegistryNamespace, fix, res.combined())
	}
	if res := h.kubectl("-n", controller.DefaultFluxNamespace, "wait", "--for=condition=Available",
		"deploy", "-l", "app.kubernetes.io/component=controller", "--timeout=180s"); res.code != 0 {
		h.t.Fatalf("no available kelson-controller deployment in %s: nothing would reconcile a Project or "+
			"an Environment.%s\n%s", controller.DefaultFluxNamespace, fix, res.combined())
	}

	res := h.kubectlOK("-n", controller.DefaultFluxNamespace, "get", "deploy",
		"-l", "app.kubernetes.io/component=controller",
		"-o", "jsonpath={range .items[*]}{.metadata.name}={.spec.template.spec.containers[0].args}{end}")
	observed := strings.TrimSpace(res.stdout)
	// The controller must be pointed at the registry this test reads back from.
	// Asserting it here — rather than discovering an empty tag list later — is
	// what keeps hack/e2e/spine.sh's bash spelling of the endpoint honest
	// against install.RegistryEndpoint, which is the constant it copies.
	if !strings.Contains(observed, "--registry="+install.RegistryEndpoint) {
		h.t.Fatalf("kelson-controller was not started with --registry=%s, so it publishes somewhere this test "+
			"does not read: %s%s", install.RegistryEndpoint, observed, fix)
	}
	h.t.Logf("prerequisites: %s", observed)
}

// --- the Environment as the API server holds it -----------------------------

// readEnvironment returns the Environment decoded into the same types the
// controller writes, or nil and an observation saying why not.
//
// Quiet, unlike every other command in this suite, because it is a poll: the
// waits below re-read this object every [pollInterval] for up to
// [spinePublishTimeout], and echoing the whole JSON each time is thousands of
// duplicate lines that bury the rest of the run's log (see harness.runQuiet).
// Nothing is lost: waitFor logs each observation that *changes*, and dumpSpine
// prints the whole object on failure.
func (h *harness) readEnvironment() (*v1alpha1.Environment, string) {
	res, _ := h.runQuiet("kubectl", "-n", spineNamespace, "get", "environment", spineEnvName, "-o", "json")
	if res.code != 0 {
		return nil, "environment not readable: " + strings.TrimSpace(res.combined())
	}
	var env v1alpha1.Environment
	if err := json.Unmarshal([]byte(res.stdout), &env); err != nil {
		return nil, fmt.Sprintf("environment did not decode: %v", err)
	}
	return &env, ""
}

// awaitEnvironment polls the Environment until check is satisfied, and returns
// the object that satisfied it. Every observation carries the whole status
// summary, so a timeout in a CI log shows the sequence the controller went
// through rather than only where it stopped.
func (h *harness) awaitEnvironment(desc string, timeout time.Duration,
	check func(*v1alpha1.Environment) (bool, string)) *v1alpha1.Environment {
	h.t.Helper()
	var settled *v1alpha1.Environment
	h.waitFor(desc, timeout, func() (bool, string) {
		env, problem := h.readEnvironment()
		if env == nil {
			return false, problem
		}
		ok, detail := check(env)
		if ok {
			settled = env
		}
		return ok, environmentLine(env) + " — " + detail
	})
	return settled
}

// settledHealthy is the "this generation is done, and it went well" predicate:
// the status describes the current generation, the revision it names is *this*
// generation's, Flux finished with it, and Ready says so.
//
// The revision check is not redundant with the phase check, and leaving it out
// is how this predicate was passed by a status that had not moved. `phase` is a
// single field with no revision attached, so a phase left over from an earlier
// revision reads exactly like a fresh one — and the controller did leave one
// behind: the transition guard is revision-blind, `Healthy` has no legal
// successor but `Degraded`, so every phase observed after the first healthy
// deploy was dropped and the status said `Healthy` from the instant of the next
// publish onwards (internal/controller's phaseFor, and
// TestANewRevisionRestartsThePhase). Step 5 was then satisfied by a status that
// described the *previous* revision, which is the one thing an end-to-end test
// of a deploy must not accept.
//
// Tying the revision to `.metadata.generation` is what makes that impossible
// without the caller having to know the tag in advance: the tag's first half is
// the generation it was published for (ADR-0028 decision 2), so a revision that
// names this generation cannot be a leftover, and observedGeneration says the
// whole status — the phase included — was written by a reconcile of it.
func (h *harness) settledHealthy(e *v1alpha1.Environment) (bool, string) {
	ready := conditionOf(e, v1alpha1.ConditionReady)
	generation := strconv.FormatInt(e.Generation, 10)
	switch {
	case e.Status.ObservedGeneration != e.Generation:
		return false, "status describes generation " + strconv.FormatInt(e.Status.ObservedGeneration, 10)
	case len(e.Status.ValidationErrors) > 0:
		return false, "validation errors: " + validationLine(e)
	case e.Status.Revision == "":
		return false, "nothing published yet"
	case !strings.HasPrefix(e.Status.Revision, generation+"-"):
		return false, "still serving " + e.Status.Revision + ", which is not a revision of generation " + generation
	case e.Status.Phase != v1alpha1.PhaseHealthy:
		return false, "phase is " + defaultTo(e.Status.Phase, "(empty)")
	case ready.Status != "True" || ready.Reason != v1alpha1.ReasonReady:
		return false, "Ready is " + ready.String()
	}
	return true, "healthy on " + e.Status.Revision
}

// assertSpineStable fails on the first violation inside a window, rather than
// waiting for one. It is how "nothing happened" is asserted: a converging poll
// cannot express it, because there is no state to converge on.
func (h *harness) assertSpineStable(desc string, window time.Duration, check func(*v1alpha1.Environment) error) {
	h.t.Helper()
	deadline := time.Now().Add(window)
	for {
		env, problem := h.readEnvironment()
		if env == nil {
			h.t.Fatalf("while holding %s steady: %s", desc, problem)
		}
		if err := check(env); err != nil {
			h.t.Fatalf("%s did not hold: %v\n%s", desc, err, environmentLine(env))
		}
		if time.Now().After(deadline) {
			h.t.Logf("%s held for %s: %s", desc, window, environmentLine(env))
			return
		}
		time.Sleep(pollInterval)
	}
}

// patchEnvironment edits the spec, which is what bumps .metadata.generation and
// therefore what the artifact tag's first half is. A merge patch replaces the
// component list wholesale, which is what a user editing a document does.
func (h *harness) patchEnvironment(patch string) {
	h.t.Helper()
	h.kubectlOK("-n", spineNamespace, "patch", "environment", spineEnvName, "--type=merge", "-p", patch)
}

// assertRevisionShape holds the tag to ADR-0028 decision 2's grammar:
// <generation>-<spec-hash-short>. Both halves are checked against a source
// other than the tag itself — the object's own generation, and the spec hash
// the history entry records — so this cannot pass by comparing a value with
// itself.
func (h *harness) assertRevisionShape(e *v1alpha1.Environment, revision string, generation int64) {
	h.t.Helper()
	if len(e.Status.History) == 0 {
		h.t.Fatalf("status.revision is %q but status.history is empty, so there is nothing to check the tag against", revision)
	}
	specHash := e.Status.History[0].SpecHash
	want := fmt.Sprintf("%d-%s", generation, model.ShortHash(specHash))
	if revision != want {
		h.t.Fatalf("status.revision = %q, want %q — the tag is <generation>-<spec-hash-short> "+
			"(generation %d, spec hash %q)", revision, want, generation, specHash)
	}
}

// assertHistoryEntry checks one entry of the bounded mirror. The digest is what
// makes the entry a record of *what* was published rather than only of where.
func (h *harness) assertHistoryEntry(entry v1alpha1.HistoryEntry, revision, image string) {
	h.t.Helper()
	if entry.Revision != revision {
		h.t.Errorf("history entry is for revision %q, want %q", entry.Revision, revision)
		return
	}
	if !strings.HasPrefix(entry.Digest, "sha256:") {
		h.t.Errorf("history entry %s carries digest %q, want a sha256 digest of the artifact", revision, entry.Digest)
	}
	if len(model.ShortHash(entry.SpecHash)) != model.ShortHashLength {
		h.t.Errorf("history entry %s carries spec hash %q, which is too short to name a revision by",
			revision, entry.SpecHash)
	}
	if !containsString(entry.Images, image) {
		h.t.Errorf("history entry %s records images %v, which does not include %q — "+
			"`which build is in production` is what this field answers", revision, entry.Images, image)
	}
}

// historyDigestFor is the digest status.history recorded for revision. It is a
// lookup by revision rather than by index — even though every call site here
// could name one — because a lookup fails loudly if the revision it is asked
// for is not the one the caller just confirmed is there, instead of silently
// reading whichever entry happens to sit at that index.
func (h *harness) historyDigestFor(e *v1alpha1.Environment, revision string) string {
	h.t.Helper()
	for _, entry := range e.Status.History {
		if entry.Revision == revision {
			return entry.Digest
		}
	}
	h.t.Fatalf("status.history has no entry for %s: %s", revision, historyLine(e))
	return ""
}

// --- the Flux pair ----------------------------------------------------------

// fluxObject is as much of a Flux object as this suite reads: its identity, the
// labels that say whose it is, its spec as a tree, and — since F8
// (f4e516d) started pinning spec.ref.digest — enough of its status to check
// what Flux says it actually applied.
type fluxObject struct {
	Metadata struct {
		Name      string            `json:"name"`
		Namespace string            `json:"namespace"`
		Labels    map[string]string `json:"labels"`
	} `json:"metadata"`
	Spec   map[string]any `json:"spec"`
	Status map[string]any `json:"status"`
}

// assertFluxPair checks both objects kelson owns for this environment: that
// they exist, that they carry the provenance the controller stamps
// (internal/delivery/provenance.go), that they are wired to each other and to
// the artifact this revision published, and — when digest is known — that the
// OCIRepository is pinned to it and the Kustomization's own status names it.
//
// digest is empty for a history entry that predates digest recording (there is
// none in this suite; every revision here is fresh), in which case the pin and
// the Kustomization's status are tag-only checks, same as before F8.
func (h *harness) assertFluxPair(revision, digest string) {
	h.t.Helper()
	name := controller.ObjectName(spineProject, spineEnvName)
	ns := controller.DefaultFluxNamespace

	// The pointer moves a moment after the status does, so this is a poll and
	// not a read: a rollback writes the OCIRepository and the status in the same
	// reconcile, but the cache the next read is served from may be a beat
	// behind — and RolledBack (environment.go's setConditions) reports Ready
	// before Flux itself has necessarily caught up on the pinned revision, so
	// the Kustomization's own status is polled here too rather than read once.
	var oci, kus fluxObject
	h.waitFor(fmt.Sprintf("%s/%s to be pinned to %s", ns, name, revision), spineSettleTimeout,
		func() (bool, string) {
			obj, problem := h.fluxObject(controller.KindOCIRepository, name)
			if obj == nil {
				return false, problem
			}
			oci = *obj
			if got := nestedString(obj.Spec, "ref", "tag"); got != revision {
				return false, "spec.ref.tag is " + defaultTo(got, "(unset)")
			}
			if digest != "" {
				if got := nestedString(obj.Spec, "ref", "digest"); got != digest {
					return false, "spec.ref.digest is " + defaultTo(got, "(unset)")
				}
			}

			kobj, kproblem := h.fluxObject(controller.KindKustomization, name)
			if kobj == nil {
				return false, kproblem
			}
			kus = *kobj
			if digest == "" {
				return true, "pinned"
			}
			// With ref.digest pinned, source-controller reports the artifact
			// revision as bare "sha256:<digest>", not "<tag>@sha256:<digest>"
			// — the shape CI run 64 caught kelson's own observer (PhaseFor,
			// revisionMatches in internal/delivery/flux/status.go) failing to
			// recognise. Checking it here against a real cluster, rather than
			// only in the unit tests that can invent any shape they like, is
			// the point of this assertion.
			applied := nestedString(kobj.Status, "lastAppliedRevision")
			if applied == "" || !strings.Contains(applied, digest) {
				return false, "Kustomization status.lastAppliedRevision is " +
					defaultTo(applied, "(unset)") + ", want it to name digest " + digest
			}
			return true, "pinned, and the Kustomization's own status names the digest"
		})

	wantURL := "oci://" + h.artifactRepository()
	if got := nestedString(oci.Spec, "url"); got != wantURL {
		h.t.Errorf("OCIRepository spec.url = %q, want %q", got, wantURL)
	}
	// Plain HTTP is an operator's named exception, and the in-cluster registry
	// is one: without this, source-controller fails its TLS handshake against a
	// registry that never had a certificate.
	if got := nestedBool(oci.Spec, "insecure"); !got {
		h.t.Errorf("OCIRepository spec.insecure = false, but %s speaks plain HTTP and the controller was "+
			"told so with --insecure-registries", install.RegistryEndpoint)
	}
	h.assertProvenanceLabels(controller.KindOCIRepository, oci)

	h.assertProvenanceLabels(controller.KindKustomization, kus)
	for _, want := range []struct {
		path []string
		want any
		why  string
	}{
		{[]string{"path"}, "./", "the artifact is a flat directory of manifests at its root (ADR-0017 decision 10)"},
		{[]string{"prune"}, true, "a resource dropped from the spec has to leave the cluster"},
		{[]string{"wait"}, true, "wait: true is what makes Ready mean healthy rather than applied"},
		{[]string{"targetNamespace"}, spineAppNS, "the workloads land in the environment's namespace, not kelson's"},
		{[]string{"sourceRef", "kind"}, controller.KindOCIRepository, "the pair consumes the artifact, not a git repository"},
		{[]string{"sourceRef", "name"}, name, "one name for the pair is what makes it unmismatchable"},
	} {
		got := nested(kus.Spec, want.path...)
		if got != want.want {
			h.t.Errorf("Kustomization spec.%s = %v, want %v — %s",
				strings.Join(want.path, "."), got, want.want, want.why)
		}
	}
}

// assertProvenanceLabels checks the four labels every object of the pair
// carries. The last one is the reverse index the controller maps a Flux event
// back to an Environment with, and the guard against two environments of the
// same name taking each other's Kustomization.
func (h *harness) assertProvenanceLabels(kind string, obj fluxObject) {
	h.t.Helper()
	for key, want := range map[string]string{
		delivery.LabelManagedBy:            delivery.ManagedByKelson,
		delivery.LabelProject:              spineProject,
		delivery.LabelEnvironment:          spineEnvName,
		delivery.LabelEnvironmentNamespace: spineNamespace,
	} {
		if got := obj.Metadata.Labels[key]; got != want {
			h.t.Errorf("%s %s/%s label %s = %q, want %q", kind,
				obj.Metadata.Namespace, obj.Metadata.Name, key, got, want)
		}
	}
}

// fluxObject reads one of the pair kelson writes. Quiet for the same reason
// readEnvironment is: assertFluxPair polls it, and a Kustomization's JSON is
// long enough that echoing every observation drowns the log.
func (h *harness) fluxObject(kind, name string) (*fluxObject, string) {
	h.t.Helper()
	res, _ := h.runQuiet("kubectl", "-n", controller.DefaultFluxNamespace, "get", strings.ToLower(kind), name, "-o", "json")
	if res.code != 0 {
		return nil, "not readable: " + strings.TrimSpace(res.combined())
	}
	var obj fluxObject
	if err := json.Unmarshal([]byte(res.stdout), &obj); err != nil {
		return nil, fmt.Sprintf("did not decode: %v", err)
	}
	return &obj, ""
}

// --- the workload -----------------------------------------------------------

type deploymentSnapshot struct {
	Spec struct {
		Replicas *int `json:"replicas"`
		Template struct {
			Spec struct {
				Containers []struct {
					Name  string `json:"name"`
					Image string `json:"image"`
				} `json:"containers"`
			} `json:"spec"`
		} `json:"template"`
	} `json:"spec"`
	Status struct {
		Replicas          int `json:"replicas"`
		ReadyReplicas     int `json:"readyReplicas"`
		AvailableReplicas int `json:"availableReplicas"`
	} `json:"status"`
}

// awaitWorkload waits until the Deployment Flux applied runs the expected image
// on the expected number of ready pods.
//
// Both halves matter and neither is redundant: the pod template says the
// artifact Flux pulled carried what kelson rendered, and the ready count says
// the cluster acted on it. This is the assertion that makes the whole chain —
// custom resource, artifact, OCIRepository, Kustomization — mean something in
// the only place a user cares about.
func (h *harness) awaitWorkload(desc, image string, replicas int, timeout time.Duration) {
	h.t.Helper()
	h.waitFor(fmt.Sprintf("%s (%s × %d in %s)", desc, image, replicas, spineAppNS), timeout,
		func() (bool, string) {
			// Quiet: a Deployment's JSON is eighty lines and this poll runs for
			// minutes. See harness.runQuiet.
			res, _ := h.runQuiet("kubectl", "-n", spineAppNS, "get", "deployment", spineComponent, "-o", "json")
			if res.code != 0 {
				return false, "deployment not readable: " + strings.TrimSpace(res.combined())
			}
			var d deploymentSnapshot
			if err := json.Unmarshal([]byte(res.stdout), &d); err != nil {
				return false, fmt.Sprintf("deployment did not decode: %v", err)
			}
			if len(d.Spec.Template.Spec.Containers) == 0 {
				return false, "the pod template declares no containers"
			}
			live := d.Spec.Template.Spec.Containers[0].Image
			observed := fmt.Sprintf("image=%s replicas=%s ready=%d/%d available=%d",
				live, replicaCount(d.Spec.Replicas), d.Status.ReadyReplicas, d.Status.Replicas, d.Status.AvailableReplicas)
			return live == image && d.Status.ReadyReplicas == replicas && d.Status.AvailableReplicas == replicas, observed
		})
}

func replicaCount(r *int) string {
	if r == nil {
		return "(unset)"
	}
	return strconv.Itoa(*r)
}

// --- the registry -----------------------------------------------------------

// artifactRepository is where this environment's artifacts are published:
// <registry>/kelson/<project>-<environment> (ADR-0028 decision 2). It is
// derived from the same constants the controller derives it from, so the two
// cannot disagree without a compile error.
func (h *harness) artifactRepository() string {
	return fmt.Sprintf("%s/%s/%s", install.RegistryEndpoint, controller.ArtifactNamespace,
		controller.ObjectName(spineProject, spineEnvName))
}

// assertArtifactRepository checks kelson's own answer against this test's, and
// returns the repository path within the registry (everything after the host).
func (h *harness) assertArtifactRepository(e *v1alpha1.Environment) string {
	h.t.Helper()
	full := h.artifactRepository()
	path, ok := strings.CutPrefix(full, install.RegistryEndpoint+"/")
	if !ok {
		h.t.Fatalf("cannot derive a repository path from %q", full)
	}
	h.t.Logf("artifacts for %s/%s go to %s (revision %s)", spineProject, spineEnvName, full, e.Status.Revision)
	return path
}

// registryProxy is a kubectl port-forward to the in-cluster registry, plus an
// HTTP client that speaks the distribution API through it.
//
// It is a port-forward rather than `kubectl get --raw` through the API server's
// service proxy for one reason: reading an OCI manifest requires an Accept
// header naming the manifest's media type, and `--raw` sends kubectl's own.
type registryProxy struct {
	t    *testing.T
	env  []string
	mu   sync.Mutex
	cmd  *exec.Cmd
	base string
	logs *safeBuffer
}

// safeBuffer collects a subprocess's output from the goroutine draining it. A
// plain Builder would be a data race, and dropping the output would make a
// broken port-forward silent.
type safeBuffer struct {
	mu sync.Mutex
	b  strings.Builder
}

func (s *safeBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *safeBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

func (h *harness) registryProxy() *registryProxy {
	h.t.Helper()
	p := &registryProxy{t: h.t, env: childEnv(), logs: &safeBuffer{}}
	p.start()
	h.t.Cleanup(p.stop)
	return p
}

// start opens the forward and blocks until kubectl announces the local port it
// chose. Port 0 is asked for rather than a fixed one, because a fixed port is a
// collision with whatever else the runner happens to be doing.
func (p *registryProxy) start() {
	p.t.Helper()
	cmd := exec.Command("kubectl", "-n", install.RegistryNamespace, "port-forward",
		"svc/"+install.RegistryName, fmt.Sprintf(":%d", install.RegistryPort))
	cmd.Env = p.env
	cmd.Stderr = p.logs
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		p.t.Fatalf("opening a pipe to kubectl port-forward: %v", err)
	}
	if err := cmd.Start(); err != nil {
		p.t.Fatalf("starting kubectl port-forward to %s/%s: %v", install.RegistryNamespace, install.RegistryName, err)
	}

	// The forward prints a line per connection it handles, so its stdout has to
	// be drained for the whole life of the process or kubectl blocks on a full
	// pipe halfway through the suite.
	ports := make(chan string, 1)
	go func() {
		defer func() { _ = stdout.Close() }()
		scanner := bufio.NewScanner(stdout)
		for scanner.Scan() {
			line := scanner.Text()
			_, _ = p.logs.Write([]byte(line + "\n"))
			if rest, ok := strings.CutPrefix(line, "Forwarding from 127.0.0.1:"); ok {
				port, _, _ := strings.Cut(rest, " ")
				select {
				case ports <- port:
				default:
				}
			}
		}
	}()

	select {
	case port := <-ports:
		p.cmd = cmd
		p.base = "http://127.0.0.1:" + port
		p.t.Logf("port-forward to %s/%s established on %s", install.RegistryNamespace, install.RegistryName, p.base)
	case <-time.After(60 * time.Second):
		_ = cmd.Process.Kill()
		p.t.Fatalf("kubectl port-forward to %s/%s never announced a local port in 60s\n%s",
			install.RegistryNamespace, install.RegistryName, p.logs.String())
	}
}

func (p *registryProxy) stop() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.cmd == nil || p.cmd.Process == nil {
		return
	}
	_ = p.cmd.Process.Kill()
	_ = p.cmd.Wait()
	p.cmd = nil
}

// get performs one distribution-API request. A transport failure restarts the
// forward once and retries: a port-forward is a long-lived subprocess against a
// pod that may have been rescheduled, and a dropped connection is not a finding
// about the registry.
func (p *registryProxy) get(path, accept string) (int, http.Header, []byte) {
	p.t.Helper()
	for attempt := range 2 {
		status, header, body, err := p.request(path, accept)
		if err == nil {
			return status, header, body
		}
		if attempt == 1 {
			p.t.Fatalf("GET %s through the registry port-forward: %v\n%s", path, err, p.logs.String())
		}
		p.t.Logf("the registry port-forward failed (%v); restarting it and retrying %s", err, path)
		p.stop()
		p.start()
	}
	return 0, nil, nil
}

func (p *registryProxy) request(path, accept string) (int, http.Header, []byte, error) {
	p.mu.Lock()
	base := p.base
	p.mu.Unlock()

	req, err := http.NewRequest(http.MethodGet, base+path, nil)
	if err != nil {
		return 0, nil, nil, err
	}
	if accept != "" {
		req.Header.Set("Accept", accept)
	}
	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return 0, nil, nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return 0, nil, nil, err
	}
	return resp.StatusCode, resp.Header, body, nil
}

// assertTagsInclude checks the registry's own tag list. It is the record
// ADR-0028 decision 4 calls the history: `status.history` is a bounded mirror of
// this, and this is what a query past the window reads.
func (h *harness) assertTagsInclude(p *registryProxy, repository string, want ...string) {
	h.t.Helper()
	status, _, body := p.get("/v2/"+repository+"/tags/list", "application/json")
	if status != http.StatusOK {
		h.t.Fatalf("GET /v2/%s/tags/list returned %d, want 200: %s", repository, status, string(body))
	}
	var list struct {
		Name string   `json:"name"`
		Tags []string `json:"tags"`
	}
	if err := json.Unmarshal(body, &list); err != nil {
		h.t.Fatalf("decoding the tag list for %s: %v\n%s", repository, err, string(body))
	}
	for _, tag := range want {
		if !containsString(list.Tags, tag) {
			h.t.Fatalf("the registry holds tags %v for %s, which does not include %q",
				list.Tags, repository, tag)
		}
	}
	h.t.Logf("the registry holds %v for %s", list.Tags, repository)
}

// ociManifest is the artifact as the registry stored it.
type ociManifest struct {
	MediaType string `json:"mediaType"`
	Config    struct {
		MediaType string `json:"mediaType"`
		Digest    string `json:"digest"`
	} `json:"config"`
	Layers []struct {
		MediaType string `json:"mediaType"`
		Digest    string `json:"digest"`
	} `json:"layers"`
	Annotations map[string]string `json:"annotations"`
}

// assertArtifactProvenance is the assertion that an artifact in a registry
// answers the same "whose is this, and of what?" question a resource in a
// cluster does.
//
// It checks the media types Flux requires of anything an OCIRepository will
// hand to kustomize-controller, and the five annotations the spine writes: the
// OCI revision, kelson's project and environment, and the generation and spec
// hash the tag is derived from. The last two are checked *against the tag*, so
// the artifact's own metadata has to agree with its address.
func (h *harness) assertArtifactProvenance(p *registryProxy, repository, tag string,
	generation int64, entry v1alpha1.HistoryEntry) {
	h.t.Helper()
	accept := strings.Join([]string{artifact.ManifestMediaType, "application/json", "*/*"}, ", ")
	status, header, body := p.get("/v2/"+repository+"/manifests/"+tag, accept)
	if status != http.StatusOK {
		h.t.Fatalf("GET /v2/%s/manifests/%s returned %d, want 200: %s", repository, tag, status, string(body))
	}
	var m ociManifest
	if err := json.Unmarshal(body, &m); err != nil {
		h.t.Fatalf("decoding the manifest for %s:%s: %v\n%s", repository, tag, err, string(body))
	}

	if m.MediaType != artifact.ManifestMediaType {
		h.t.Errorf("artifact %s:%s has mediaType %q, want %q", repository, tag, m.MediaType, artifact.ManifestMediaType)
	}
	if m.Config.MediaType != artifact.ConfigMediaType {
		h.t.Errorf("artifact %s:%s has config mediaType %q, want %q — it is what tells a reader this is a "+
			"Flux artifact and not an image", repository, tag, m.Config.MediaType, artifact.ConfigMediaType)
	}
	if len(m.Layers) != 1 || m.Layers[0].MediaType != artifact.LayerMediaType {
		h.t.Errorf("artifact %s:%s carries %d layers (%v), want exactly one of type %q",
			repository, tag, len(m.Layers), m.Layers, artifact.LayerMediaType)
	}
	// The digest the registry reports is the one the status recorded: a tag says
	// where the artifact was put, a digest says what was put there.
	if got := header.Get("Docker-Content-Digest"); got != "" && entry.Digest != "" && got != entry.Digest {
		h.t.Errorf("the registry reports %s:%s as %s, and status.history records %s",
			repository, tag, got, entry.Digest)
	}

	for key, want := range map[string]string{
		artifact.AnnRevision:     tag,
		artifact.AnnProject:      spineProject,
		artifact.AnnEnvironment:  spineEnvName,
		controller.AnnGeneration: strconv.FormatInt(generation, 10),
		controller.AnnSpecHash:   entry.SpecHash,
	} {
		if got := m.Annotations[key]; got != want {
			h.t.Errorf("artifact %s:%s annotation %s = %q, want %q", repository, tag, key, got, want)
		}
	}
	// The tag is derived from the two annotations above, so they must compose it.
	if composed := fmt.Sprintf("%s-%s", m.Annotations[controller.AnnGeneration],
		model.ShortHash(m.Annotations[controller.AnnSpecHash])); composed != tag {
		h.t.Errorf("artifact %s:%s says it is generation %q of spec hash %q, which composes the tag %q",
			repository, tag, m.Annotations[controller.AnnGeneration], m.Annotations[controller.AnnSpecHash], composed)
	}
	h.t.Logf("artifact %s:%s carries %d provenance annotations and %d layer", repository, tag, len(m.Annotations), len(m.Layers))
}

// --- fixture teardown and diagnostics ---------------------------------------

// removeSpineFixture deletes what the scenario applied, on the success path
// only. Deleting the Environment is also the finalizer's own end-to-end
// exercise: it is what removes the Kustomization, which is what prunes the
// workloads (ADR-0028's 2026-08-14 amendment).
func (h *harness) removeSpineFixture() {
	for _, args := range [][]string{
		{"-n", spineNamespace, "delete", "environment", spineEnvName, "--ignore-not-found", "--timeout=180s"},
		{"-n", spineNamespace, "delete", "project", spineProject, "--ignore-not-found", "--timeout=60s"},
		{"delete", "namespace", spineNamespace, "--ignore-not-found", "--timeout=120s"},
		{"delete", "namespace", spineAppNS, "--ignore-not-found", "--timeout=120s"},
	} {
		if res := h.kubectl(args...); res.code != 0 {
			// Never a failure: the assertions are done, and a teardown that
			// failed the test would report a cleanup problem as a spine problem.
			h.t.Logf("cleanup `kubectl %s` did not succeed (exit %d) — continuing", strings.Join(args, " "), res.code)
		}
	}
}

// dumpSpine prints everything a maintainer would ask for, from the three
// namespaces this scenario spans plus the logs of the three controllers
// involved. It runs only on failure and never fails the test itself.
func (h *harness) dumpSpine() {
	if !h.t.Failed() {
		return
	}
	h.t.Log("--- delivery spine diagnostics ---")
	for _, args := range [][]string{
		{"-n", spineNamespace, "get", "projects,environments", "-o", "yaml"},
		{"-n", controller.DefaultFluxNamespace, "get", "ocirepositories,kustomizations", "-o", "yaml"},
		{"-n", spineAppNS, "get", "all", "-o", "wide", "--show-labels"},
		{"-n", spineAppNS, "describe", "pods"},
		{"-n", spineAppNS, "get", "events", "--sort-by=.lastTimestamp"},
		{"-n", controller.DefaultFluxNamespace, "logs", "-l", "app.kubernetes.io/component=controller", "--tail=300"},
		{"-n", "flux-system", "logs", "deploy/source-controller", "--tail=150"},
		{"-n", "flux-system", "logs", "deploy/kustomize-controller", "--tail=150"},
	} {
		if res := h.kubectl(args...); res.code != 0 {
			h.t.Logf("diagnostic `kubectl %s` failed (exit %d) — continuing", strings.Join(args, " "), res.code)
		}
	}
}

// --- small readers ----------------------------------------------------------

// condition is one status condition, flattened to strings so this package needs
// no Kubernetes types.
type condition struct {
	Type    string `json:"type"`
	Status  string `json:"status"`
	Reason  string `json:"reason"`
	Message string `json:"message"`
}

func (c condition) String() string {
	if c.Type == "" {
		return "(absent)"
	}
	return fmt.Sprintf("%s/%s: %s", c.Status, c.Reason, c.Message)
}

func conditionOf(e *v1alpha1.Environment, name string) condition {
	for _, c := range e.Status.Conditions {
		if c.Type == name {
			return condition{Type: c.Type, Status: string(c.Status), Reason: c.Reason, Message: c.Message}
		}
	}
	return condition{}
}

// environmentLine is the one-line summary every observation carries.
func environmentLine(e *v1alpha1.Environment) string {
	return fmt.Sprintf("generation=%d observed=%d phase=%s revision=%s rollback=%s@%d history=[%s] Ready=%s Progressing=%s",
		e.Generation, e.Status.ObservedGeneration, defaultTo(e.Status.Phase, "-"),
		defaultTo(e.Status.Revision, "-"), defaultTo(e.Status.RollbackRevision, "-"), e.Status.RollbackGeneration,
		historyRevisions(e), conditionOf(e, v1alpha1.ConditionReady), conditionOf(e, v1alpha1.ConditionProgressing))
}

func historyLine(e *v1alpha1.Environment) string {
	return fmt.Sprintf("history=[%s]", historyRevisions(e))
}

func historyRevisions(e *v1alpha1.Environment) string {
	out := make([]string, 0, len(e.Status.History))
	for _, entry := range e.Status.History {
		out = append(out, entry.Revision+"="+defaultTo(entry.Outcome, "-"))
	}
	return strings.Join(out, " ")
}

func validationLine(e *v1alpha1.Environment) string {
	out := make([]string, 0, len(e.Status.ValidationErrors))
	for _, v := range e.Status.ValidationErrors {
		out = append(out, v.Code+" at "+v.Field)
	}
	return strings.Join(out, "; ")
}

func defaultTo(s, fallback string) string {
	if s == "" {
		return fallback
	}
	return s
}

func containsString(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}

// nested walks a decoded JSON object. It exists because this package holds no
// Kubernetes types, so there is no unstructured.NestedFieldNoCopy to call.
func nested(m map[string]any, path ...string) any {
	var current any = m
	for _, key := range path {
		obj, ok := current.(map[string]any)
		if !ok {
			return nil
		}
		current, ok = obj[key]
		if !ok {
			return nil
		}
	}
	return current
}

func nestedString(m map[string]any, path ...string) string {
	s, _ := nested(m, path...).(string)
	return s
}

func nestedBool(m map[string]any, path ...string) bool {
	b, _ := nested(m, path...).(bool)
	return b
}
