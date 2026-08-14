package main

import (
	"strings"
	"testing"

	"github.com/dafrie/kelson/internal/delivery"
)

// --- fixtures ---------------------------------------------------------------

func deploymentDoc(replicas, image string) []byte {
	return []byte("apiVersion: apps/v1\nkind: Deployment\nmetadata:\n" +
		"  name: web\n  namespace: hello-development\n" +
		"  labels:\n    app.kubernetes.io/managed-by: kelson\n" +
		"    kelson.dev/project: hello\n    kelson.dev/environment: development\n" +
		"spec:\n  replicas: " + replicas + "\n  selector:\n    matchLabels:\n      app: web\n" +
		"  template:\n    metadata:\n      labels:\n        app: web\n" +
		"    spec:\n      containers:\n        - name: web\n          image: " + image + "\n")
}

func webManifest(replicas, image string) delivery.Manifest {
	return delivery.Manifest{
		APIVersion: "apps/v1",
		Kind:       "Deployment",
		Name:       "web",
		Namespace:  "hello-development",
		YAML:       deploymentDoc(replicas, image),
	}
}

// recordedHistory is the two-revision fixture every rollback test uses: 000002
// is live, 000001 is the entry a bare `kelson rollback` restores.
func recordedHistory() (fakeRecorded, []delivery.Entry) {
	src := fakeRecorded{
		current: []delivery.Manifest{webManifest("3", "ghcr.io/acme/hello:2.0.0")},
		byRev: map[string][]delivery.Manifest{
			"000001": {webManifest("2", "ghcr.io/acme/hello:1.0.0")},
			"000002": {webManifest("3", "ghcr.io/acme/hello:2.0.0")},
		},
	}
	entries := []delivery.Entry{
		{Revision: "000002", SpecHash: "sha256:bbb", CommittedAt: "2026-08-12T10:00:00Z"},
		{Revision: "000001", SpecHash: "sha256:aaa", CommittedAt: "2026-08-11T10:00:00Z"},
	}
	return src, entries
}

func rollbackAdapter() (*fakeAdapter, fakeRecorded) {
	src, entries := recordedHistory()
	adapter := newFakeAdapter("direct")
	adapter.history = entries
	return adapter, src
}

// --- tests ------------------------------------------------------------------

// TestRollbackPreviewPrecedesTheApply is the property the command exists to
// guarantee (issue #55): the irreversibility findings are on screen before
// anything is applied, so nobody discovers afterwards that the rollback could
// not restore what they assumed.
func TestRollbackPreviewPrecedesTheApply(t *testing.T) {
	spec, history := deploySpec(t)
	adapter, src := rollbackAdapter()

	stdout, code, msg := runDelivery(t, planeOf([]delivery.Adapter{adapter}, nil, src),
		"rollback", "-f", spec, "--env", "development", "--history", history, "--yes")
	if code != exitOK {
		t.Fatalf("exit = %d (%s), want 0\n%s", code, msg, stdout)
	}
	preview := strings.Index(stdout, "What a rollback cannot revert")
	applied := strings.Index(stdout, "Restored revision")
	switch {
	case preview < 0:
		t.Fatalf("the irreversibility preview was not printed:\n%s", stdout)
	case applied < 0:
		t.Fatalf("the rollback result was not printed:\n%s", stdout)
	case preview > applied:
		t.Fatalf("the preview must precede the apply:\n%s", stdout)
	}
	// kelson runs no migrations (#104), so every preview carries that caveat
	// rather than implying the rollback is fully safe.
	if !strings.Contains(stdout, "migrations-not-covered") {
		t.Fatalf("the preview should carry the migrations caveat:\n%s", stdout)
	}
}

// TestRollbackDefaultsToThePreviousEntry: "undo the last deploy" is the common
// intent, and history is newest first.
func TestRollbackDefaultsToThePreviousEntry(t *testing.T) {
	spec, history := deploySpec(t)
	adapter, src := rollbackAdapter()

	stdout, code, msg := runDelivery(t, planeOf([]delivery.Adapter{adapter}, nil, src),
		"rollback", "-f", spec, "--env", "development", "--history", history, "--yes")
	if code != exitOK {
		t.Fatalf("exit = %d (%s), want 0", code, msg)
	}
	adapter.mu.Lock()
	targets := append([]delivery.Entry(nil), adapter.rolledTo...)
	adapter.mu.Unlock()
	if len(targets) != 1 || targets[0].Revision != "000001" {
		t.Fatalf("expected a rollback to 000001, got %+v", targets)
	}
	if !strings.Contains(stdout, "Restored revision 000001 as 000009") {
		t.Fatalf("stdout should name the restored and the new revision:\n%s", stdout)
	}
}

// TestRollbackRequiresConfirmation: without --yes and with nothing to answer
// the prompt, the rollback refuses loudly and the adapter is never called. A
// closed stdin must never be able to mean "yes" — and it is not a quiet "no"
// either, because an unattended run that silently did nothing would be read
// as a rollback that happened.
func TestRollbackRequiresConfirmation(t *testing.T) {
	spec, history := deploySpec(t)
	adapter, src := rollbackAdapter()

	stdout, code, msg := runDelivery(t, planeOf([]delivery.Adapter{adapter}, nil, src),
		"rollback", "-f", spec, "--env", "development", "--history", history)
	if code != exitErr || !strings.Contains(msg, "stdin closed") {
		t.Fatalf("expected the stdin-closed refusal, got %d: %s", code, msg)
	}
	for _, call := range adapter.callLog() {
		if call == "rollback" {
			t.Fatalf("the adapter must not act without confirmation:\n%s", stdout)
		}
	}
	if !strings.Contains(stdout, "What a rollback cannot revert") {
		t.Fatalf("the preview is printed even when the rollback is declined:\n%s", stdout)
	}
}

// TestRollbackConfirmedAtThePrompt covers the interactive yes.
func TestRollbackConfirmedAtThePrompt(t *testing.T) {
	spec, history := deploySpec(t)
	adapter, src := rollbackAdapter()

	stdout, code, msg := runDeliveryStdin(t, planeOf([]delivery.Adapter{adapter}, nil, src), "yes\n",
		"rollback", "-f", spec, "--env", "development", "--history", history)
	if code != exitOK {
		t.Fatalf("exit = %d (%s), want 0\n%s", code, msg, stdout)
	}
	if !strings.Contains(stdout, "Restored revision 000001") {
		t.Fatalf("stdout should report the restore:\n%s", stdout)
	}
}

// TestRollbackToExplicitRevision covers --to.
func TestRollbackToExplicitRevision(t *testing.T) {
	spec, history := deploySpec(t)
	adapter, src := rollbackAdapter()

	_, code, msg := runDelivery(t, planeOf([]delivery.Adapter{adapter}, nil, src),
		"rollback", "-f", spec, "--env", "development", "--history", history, "--to", "000002", "--yes")
	if code != exitOK {
		t.Fatalf("exit = %d (%s), want 0", code, msg)
	}
	adapter.mu.Lock()
	defer adapter.mu.Unlock()
	if len(adapter.rolledTo) != 1 || adapter.rolledTo[0].Revision != "000002" {
		t.Fatalf("expected a rollback to 000002, got %+v", adapter.rolledTo)
	}
}

// TestRollbackUnknownRevision: an unretained revision is named as such and the
// retained ones are listed, rather than failing three steps later inside the
// adapter.
func TestRollbackUnknownRevision(t *testing.T) {
	spec, history := deploySpec(t)
	adapter, src := rollbackAdapter()

	_, code, msg := runDelivery(t, planeOf([]delivery.Adapter{adapter}, nil, src),
		"rollback", "-f", spec, "--env", "development", "--history", history, "--to", "000042", "--yes")
	if code != exitErr {
		t.Fatalf("exit = %d, want %d", code, exitErr)
	}
	if !strings.Contains(msg, "000042") || !strings.Contains(msg, "000001") {
		t.Fatalf("error should name the missing revision and list the retained ones, got: %s", msg)
	}
}

func TestRollbackWithNoHistory(t *testing.T) {
	spec, history := deploySpec(t)
	adapter := newFakeAdapter("direct")
	_, code, msg := runDelivery(t, planeOf([]delivery.Adapter{adapter}, nil, fakeRecorded{}),
		"rollback", "-f", spec, "--env", "development", "--history", history, "--yes")
	if code != exitErr || !strings.Contains(msg, "nothing has been deployed") {
		t.Fatalf("expected a no-history error, got %d: %s", code, msg)
	}
}

func TestRollbackWithOnlyOneRevision(t *testing.T) {
	spec, history := deploySpec(t)
	adapter := newFakeAdapter("direct")
	adapter.history = []delivery.Entry{{Revision: "000001"}}
	_, code, msg := runDelivery(t, planeOf([]delivery.Adapter{adapter}, nil, fakeRecorded{}),
		"rollback", "-f", spec, "--env", "development", "--history", history, "--yes")
	if code != exitErr || !strings.Contains(msg, "no previous state") {
		t.Fatalf("expected a single-revision error, got %d: %s", code, msg)
	}
}

// TestRollbackUnsupportedAdapter: an adapter that cannot roll back says so
// instead of appearing to succeed.
func TestRollbackUnsupportedAdapter(t *testing.T) {
	spec, history := deploySpec(t)
	adapter := newFakeAdapter("direct")
	adapter.caps = delivery.Capabilities{SupportsRollback: false}
	_, code, msg := runDelivery(t, planeOf([]delivery.Adapter{adapter}, nil, fakeRecorded{}),
		"rollback", "-f", spec, "--env", "development", "--history", history, "--yes")
	if code != exitErr || !strings.Contains(msg, "cannot roll back") {
		t.Fatalf("expected an unsupported-rollback error, got %d: %s", code, msg)
	}
}

// TestRollbackWithoutRecordedHistorySaysSo: a mode whose rendered history
// kelson cannot read gets an explicit "no preview" line. An absent warning
// must never be mistaken for "nothing to warn about".
func TestRollbackWithoutRecordedHistorySaysSo(t *testing.T) {
	spec, history := deploySpec(t)
	adapter, _ := rollbackAdapter()

	stdout, code, msg := runDelivery(t, planeOf([]delivery.Adapter{adapter}, nil, nil),
		"rollback", "-f", spec, "--env", "development", "--history", history, "--yes")
	if code != exitOK {
		t.Fatalf("exit = %d (%s), want 0", code, msg)
	}
	if !strings.Contains(stdout, "irreversibility preview is unavailable") {
		t.Fatalf("stdout should say the preview could not be produced:\n%s", stdout)
	}
}
