package secret

import (
	"context"
	"encoding/base64"
	"errors"
	"strings"
	"testing"

	"github.com/dafrie/kelson/internal/delivery/git"
	"github.com/dafrie/kelson/internal/redact"
)

// The redaction property of issue #117, carried onto the sops paths (issue
// #81, ADR-0022).
//
// ADR-0009's amendment states the guarantee as **kelson never adds a secret to
// a log**, and #117 made it a property rather than a convention by registering
// every value at the moment it is learned. The `cluster` backend's first
// statement is redact.Register and so is this one; what this file checks is
// that the property survives the paths only this backend has — an encryption
// failure, a git failure, a refusal to drop keys — where a value is in hand and
// an error is being built.
//
// These tests share the process-wide registry with everything else in the
// package, which is the point: registration is process-wide precisely so a
// value learned in one place cannot be printed from another.

// leakSentinel is distinctive enough that finding it anywhere is conclusive,
// and long enough to be registrable (redact.MinScrubLength).
const leakSentinel = "LEAK-SENTINEL-7f3a91c4-value"

// assertRegistered is the property, checked through the mechanism every
// surface uses: after the store has seen a value, the process scrubber blanks
// it out of arbitrary text. Nothing in kelson has to remember to call anything
// for this to hold — errors.go, the log writers and the API's error mapping
// all run their free text through the same registry.
func assertRegistered(t *testing.T, value string) {
	t.Helper()
	line := "deploy log: connecting with " + value + " ..."
	if scrubbed := redact.Scrub(line); strings.Contains(scrubbed, value) {
		t.Fatalf("the value is not registered with internal/redact; a log line would print it:\n%s", scrubbed)
	}
}

func assertNoLeak(t *testing.T, where, body, value string) {
	t.Helper()
	if strings.Contains(body, value) {
		t.Errorf("the value reached %s:\n%s", where, body)
	}
	if b64 := base64.StdEncoding.EncodeToString([]byte(value)); strings.Contains(body, b64) {
		t.Errorf("the base64 of the value reached %s:\n%s", where, body)
	}
}

// TestSOPSSetRegistersBeforeAnythingElse: the value is registered before
// validation, before the repository is opened and before anything could fail,
// so no failure between here and the commit can print it.
func TestSOPSSetRegistersBeforeAnythingElse(t *testing.T) {
	store, _ := sopsStore(t)
	// A request that fails validation as late as possible: the name is fine,
	// the key is not, so Validate reaches the key loop and refuses.
	_, err := store.Set(context.Background(), SetRequest{
		Target: sopsTarget(), Name: "checkout-db",
		Values: map[string]string{"not a key": leakSentinel},
	})
	if err == nil {
		t.Fatalf("expected a refusal")
	}
	assertRegistered(t, leakSentinel)
	assertNoLeak(t, "the validation error", err.Error(), leakSentinel)
}

// TestSOPSRefusalsCarryNoValue walks the failure modes this backend adds and
// asserts the same thing about each: the error names the Secret, the keys and
// the file, and never the value.
func TestSOPSRefusalsCarryNoValue(t *testing.T) {
	ctx := context.Background()

	t.Run("partial set", func(t *testing.T) {
		store, _ := sopsStore(t)
		if _, err := store.Set(ctx, SetRequest{
			Target: sopsTarget(), Name: "checkout-db",
			Values: map[string]string{"url": leakSentinel, "token": leakSentinel + "-2"},
		}); err != nil {
			t.Fatalf("first Set: %v", err)
		}
		_, err := store.Set(ctx, SetRequest{
			Target: sopsTarget(), Name: "checkout-db", Values: map[string]string{"url": leakSentinel},
		})
		if !AsCode(err, ErrSOPSPartialSet) {
			t.Fatalf("want %s, got %v", ErrSOPSPartialSet, err)
		}
		assertNoLeak(t, "the partial-set refusal", errorText(err), leakSentinel)
		// And it does name what a user needs, which is the key, not the value.
		if !strings.Contains(errorText(err), "token") {
			t.Errorf("the refusal must name the key that would be lost:\n%s", errorText(err))
		}
	})

	t.Run("encryption failure", func(t *testing.T) {
		// A recipient age cannot parse. The value is in hand and an error is
		// being built from a library's message, which is the shape that leaks
		// if a cause is relayed carelessly.
		store, err := NewSOPS(SOPSConfig{
			Writer:     newSOPSWriter(t),
			Recipients: []string{"age1thisisnotarealrecipientatallandwillnotparsecorrectlyxx"},
		})
		if err != nil {
			t.Fatalf("NewSOPS: %v", err)
		}
		_, err = store.Set(ctx, SetRequest{
			Target: sopsTarget(), Name: "checkout-db", Values: map[string]string{"url": leakSentinel},
		})
		if !AsCode(err, ErrSOPSEncryptFailed) {
			t.Fatalf("want %s, got %v", ErrSOPSEncryptFailed, err)
		}
		assertNoLeak(t, "the encryption failure", errorText(err), leakSentinel)
	})

	t.Run("git failure", func(t *testing.T) {
		// A repository that is not one. The push path builds an error from
		// go-git's message while the value is still in scope.
		store, err := NewSOPS(SOPSConfig{
			Writer:     newSOPSWriterTo(t, "/nonexistent/not-a-repository"),
			Recipients: []string{testRecipient},
		})
		if err != nil {
			t.Fatalf("NewSOPS: %v", err)
		}
		_, err = store.Set(ctx, SetRequest{
			Target: sopsTarget(), Name: "checkout-db", Values: map[string]string{"url": leakSentinel},
		})
		if err == nil {
			t.Fatalf("expected a git failure")
		}
		assertNoLeak(t, "the git failure", errorText(err), leakSentinel)
	})

	t.Run("plaintext file", func(t *testing.T) {
		// A hand-committed plaintext Secret. kelson relays a parse failure and
		// names the path — and must not quote the file's contents back, which
		// are somebody's actual credential.
		store, _ := sopsStore(t)
		session, err := storeWriter(store).Open(ctx, git.Message{Subject: "by hand"})
		if err != nil {
			t.Fatalf("open: %v", err)
		}
		if err := session.Put([]git.File{{
			Path: git.SecretPath("checkout-db"),
			Data: []byte("apiVersion: v1\nkind: Secret\nstringData:\n  url: " + leakSentinel + "\n"),
		}}, nil); err != nil {
			t.Fatalf("put: %v", err)
		}
		if _, err := session.Commit(ctx); err != nil {
			t.Fatalf("commit: %v", err)
		}
		_, err = store.Set(ctx, SetRequest{
			Target: sopsTarget(), Name: "checkout-db", Values: map[string]string{"url": "x"},
		})
		if !AsCode(err, ErrSOPSNotEncrypted) {
			t.Fatalf("want %s, got %v", ErrSOPSNotEncrypted, err)
		}
		assertNoLeak(t, "the not-encrypted refusal", errorText(err), leakSentinel)
	})
}

// TestSOPSListAndDriftCarryNoValue: the read paths hand their results to
// formatters, and neither the type nor the report has a field a value could be
// in. This asserts it over the whole rendered text rather than field by field,
// which is what would catch a future field somebody adds.
func TestSOPSListAndDriftCarryNoValue(t *testing.T) {
	store, _ := sopsStore(t)
	ctx := context.Background()
	if _, err := store.Set(ctx, SetRequest{
		Target: sopsTarget(), Name: "checkout-db", Values: map[string]string{"url": leakSentinel},
	}); err != nil {
		t.Fatalf("Set: %v", err)
	}

	list, err := store.List(ctx, sopsTarget())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	for _, s := range list {
		assertNoLeak(t, "the listing", s.Name+" "+s.Namespace+" "+strings.Join(s.Keys, " "), leakSentinel)
	}

	rotated, err := NewSOPS(SOPSConfig{
		Writer:     storeWriter(store),
		Recipients: []string{"age1cnnsmzvurnpxzzggcwm4rvk44hgskzhrz92nryk6879kf8zll3jqqjhvt3"},
	})
	if err != nil {
		t.Fatalf("NewSOPS: %v", err)
	}
	drift, err := rotated.Drift(ctx, sopsTarget())
	if err != nil {
		t.Fatalf("Drift: %v", err)
	}
	if len(drift) != 1 {
		t.Fatalf("expected one stale file, got %+v", drift)
	}
	d := drift[0]
	report := strings.Join(append(append(append([]string{d.Name, d.Path}, d.Keys...), d.Recipients...),
		append(d.Missing, d.Extra...)...), " ")
	assertNoLeak(t, "the drift report", report, leakSentinel)
}

// errorText is the whole of a structured refusal as a caller would print it:
// message, remediation, cause and resource. A leak in any one of them is a
// leak, and the cause is the one that carries somebody else's words.
func errorText(err error) string {
	var e Error
	if errors.As(err, &e) {
		return strings.Join([]string{e.Message, e.Remediation, e.Cause, e.Resource, e.Error()}, "\n")
	}
	return err.Error()
}
