package api

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"connectrpc.com/connect"

	kelsonv1alpha1 "github.com/dafrie/kelson/internal/api/gen/kelson/v1alpha1"
	"github.com/dafrie/kelson/internal/controlstore"
	"github.com/dafrie/kelson/internal/diff"
	"github.com/dafrie/kelson/internal/explain"
	"github.com/dafrie/kelson/internal/observation"
	"github.com/dafrie/kelson/internal/redact"
	"github.com/dafrie/kelson/internal/secret"
)

// The single cross-surface sentinel test for issue #117.
//
// One value is registered once, then driven through every scrubbing surface
// kelson has: the structured API error, the redacted L1 diff of a Secret, an
// audit record's free text, an explain cause, and a build-log write through
// the scrubbing writer. Each surface has its own focused tests already
// (internal/redact, internal/diff, internal/api, internal/controlstore); what
// this test adds is that all five are read in one place against the same
// value, so a future surface that forgets to scrub cannot hide behind "some
// other surface's test covers this value".
//
// Runtime pod-log streaming (internal/api/logs.go) is deliberately excluded —
// it is a documented boundary, not a gap (internal/redact/doc.go, ADR-0009
// amendment): kelson does not content-sniff a workload's own stdout.
const crossSurfaceSentinel = "cross-surface-SENTINEL-9d21f6"

func TestSentinelNeverReachesAnyScrubbingSurface(t *testing.T) {
	redact.Register(crossSurfaceSentinel)

	t.Run("api error Message/Remediation/Cause", func(t *testing.T) {
		store := newFakeSecrets()
		store.err = secret.Error{
			Code:        secret.ErrWriteFailed,
			Resource:    "Secret/hello-db",
			Message:     "the API server rejected the write: could not authenticate with " + crossSurfaceSentinel,
			Remediation: "check the credential " + crossSurfaceSentinel,
			Cause:       "unauthorized: " + crossSurfaceSentinel,
		}
		c := serve(t, Options{Secrets: store})

		_, streamErr := c.secrets.SetSecret(context.Background(), connect.NewRequest(&kelsonv1alpha1.SetSecretRequest{
			Target: secretTargetOf("hello", "development"),
			Name:   "hello-db",
			Values: map[string]string{"url": "irrelevant"},
		}))
		if streamErr == nil {
			t.Fatal("the failing write did not surface as an error")
		}
		assertNoSentinel(t, "connect error message", []byte(streamErr.Error()), crossSurfaceSentinel)

		var cerr *connect.Error
		if !errors.As(streamErr, &cerr) {
			t.Fatalf("want a *connect.Error, got %T", streamErr)
		}
		details := 0
		for _, d := range cerr.Details() {
			value, verr := d.Value()
			if verr != nil {
				continue
			}
			wire, ok := value.(*kelsonv1alpha1.Error)
			if !ok {
				continue
			}
			details++
			assertNoSentinel(t, "structured error",
				[]byte(wire.GetMessage()+wire.GetRemediation()+wire.GetCause()), crossSurfaceSentinel)
		}
		if details == 0 {
			t.Error("the structured detail was dropped; scrubbing must not cost the taxonomy")
		}
	})

	t.Run("diff of a Secret's data and last-applied annotation", func(t *testing.T) {
		const before = `apiVersion: v1
kind: Secret
metadata:
  name: checkout-db
  namespace: shop
type: Opaque
data:
  DATABASE_URL: b2xkLXZhbHVl
`
		lastApplied := `{"apiVersion":"v1","kind":"Secret","data":{"DATABASE_URL":"` + crossSurfaceSentinel + `"}}`
		after := `apiVersion: v1
kind: Secret
metadata:
  name: checkout-db
  namespace: shop
  annotations:
    kubectl.kubernetes.io/last-applied-configuration: '` + lastApplied + `'
type: Opaque
data:
  DATABASE_URL: ` + crossSurfaceSentinel + `
`
		d, err := diff.BetweenDocuments("shop", "production",
			[][]byte{[]byte(before)}, [][]byte{[]byte(after)}, nil)
		if err != nil {
			t.Fatalf("BetweenDocuments: %v", err)
		}
		encoded, err := diff.EncodeJSON(d)
		if err != nil {
			t.Fatalf("EncodeJSON: %v", err)
		}
		assertNoSentinel(t, "JSON diff", encoded, crossSurfaceSentinel)

		var term bytes.Buffer
		if err := diff.Write(&term, d, false); err != nil {
			t.Fatalf("Write: %v", err)
		}
		assertNoSentinel(t, "terminal diff", term.Bytes(), crossSurfaceSentinel)
		if !strings.Contains(term.String(), redact.Sentinel) {
			t.Errorf("nothing was marked as redacted, so the diff is silently incomplete:\n%s", term.String())
		}
	})

	t.Run("audit record free text", func(t *testing.T) {
		sink := newFakeAuditSink()
		a := newAuditor(sink, nil)
		a.write(context.Background(), controlstore.AuditRecord{
			Principal:     controlstore.AuditPrincipal{Type: "agent", Name: "deploybot"},
			Procedure:     "/kelson.v1alpha1.SecretService/SetSecret",
			Target:        controlstore.AuditTarget{Project: "shop", Environment: "production"},
			Reason:        "rotating to " + crossSurfaceSentinel,
			Message:       "the store refused the value " + crossSurfaceSentinel,
			DryRunSummary: "would write " + crossSurfaceSentinel,
		})
		rec := sink.only(t)
		for field, value := range map[string]string{
			"reason":        rec.Reason,
			"message":       rec.Message,
			"dryRunSummary": rec.DryRunSummary,
		} {
			if strings.Contains(value, crossSurfaceSentinel) {
				t.Errorf("the %s field of a stored record carries the sentinel: %q", field, value)
			}
			if !strings.Contains(value, redact.Sentinel) {
				t.Errorf("the %s field was not scrubbed at all: %q", field, value)
			}
		}
	})

	t.Run("explain cause", func(t *testing.T) {
		e := explain.Explain(context.Background(), explain.Input{
			Project:     "shop",
			Environment: "production",
			Namespace:   "shop-production",
			Verdicts: []observation.Verdict{{
				Healthy:  false,
				Code:     observation.CodeCrashLoopBackOff,
				Resource: "Deployment/shop-production/web",
				Reason:   "container failed: " + crossSurfaceSentinel,
			}},
		})
		if len(e.Causes) == 0 {
			t.Fatal("no cause was derived; the verdict this test staged did not reach explain.Explain")
		}
		found := false
		for _, c := range e.Causes {
			assertNoSentinel(t, "cause message", []byte(c.Message), crossSurfaceSentinel)
			assertNoSentinel(t, "cause remediation", []byte(c.Remediation), crossSurfaceSentinel)
			for _, ev := range c.Evidence {
				assertNoSentinel(t, "cause evidence detail", []byte(ev.Detail), crossSurfaceSentinel)
				if strings.Contains(ev.Detail, redact.Sentinel) {
					found = true
				}
			}
		}
		if !found {
			t.Error("nothing in the explanation was marked as redacted, so the reason never reached the scrubber")
		}
	})

	t.Run("build log write through the scrubbing writer", func(t *testing.T) {
		builder := &fakeBuilder{
			logs: "#1 [internal] load build definition\n" +
				"#2 authenticating: password=" + crossSurfaceSentinel + "\n" +
				"#3 exporting to image DONE\n",
		}
		c := serve(t, Options{
			Build:         buildPlaneFor(builder, &fakeRevisions{}, nil),
			BuildDefaults: BuildDefaults{Registry: "ghcr.io/acme"},
		})

		out, err := collectBuild(t, c, &kelsonv1alpha1.BuildRequest{Spec: buildSpec(), Environment: "production"})
		if err != nil {
			t.Fatalf("Build: %v", err)
		}
		for i, chunk := range out.chunks {
			assertNoSentinel(t, "build log chunk", chunk, crossSurfaceSentinel)
			if i > 100 {
				break
			}
		}
		logs := out.logs()
		if !strings.Contains(logs, redact.Sentinel) {
			t.Errorf("the log was not marked where the sentinel was:\n%s", logs)
		}
	})
}
