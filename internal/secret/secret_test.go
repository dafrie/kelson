package secret_test

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/dafrie/kelson/internal/redact"
	"github.com/dafrie/kelson/internal/secret"
)

// The cluster secret backend against a fake clientset (issue #116). What these
// tests assert is the three properties the package exists for: an apply is
// idempotent and provenanced, a read-back carries keys and never values, and
// kelson refuses to touch a Secret it does not manage.

func target() secret.Target {
	return secret.Target{Project: "checkout", Environment: "production"}
}

// derivedNamespace is what target() resolves to. It is written out rather than
// derived here so a change to the derivation fails this file too.
const derivedNamespace = "checkout-production"

func mustSet(t *testing.T, store *secret.Store, req secret.SetRequest) secret.Secret {
	t.Helper()
	got, err := store.Set(context.Background(), req)
	if err != nil {
		t.Fatalf("Set: %v", err)
	}
	return got
}

func liveSecret(t *testing.T, cli *fake.Clientset, namespace, name string) *corev1.Secret {
	t.Helper()
	got, err := cli.CoreV1().Secrets(namespace).Get(context.Background(), name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("reading back %s/%s: %v", namespace, name, err)
	}
	return got
}

func codeOf(t *testing.T, err error) secret.Code {
	t.Helper()
	var se secret.Error
	if !errors.As(err, &se) {
		t.Fatalf("want a secret.Error, got %T: %v", err, err)
	}
	return se.Code
}

// TestSetCreatesALabelledSecretAndIsIdempotent is the apply contract: the first
// set creates, the second changes nothing, and both carry the provenance that
// makes listing a label query.
func TestSetCreatesALabelledSecretAndIsIdempotent(t *testing.T) {
	cli := fake.NewClientset()
	store := secret.New(cli)

	first := mustSet(t, store, secret.SetRequest{
		Target: target(), Name: "checkout-db",
		Values: map[string]string{"url": "postgres://user:pw@db/checkout", "api-key": "sk-live-0001"},
	})
	if !reflect.DeepEqual(first.Keys, []string{"api-key", "url"}) {
		t.Errorf("keys = %v, want them sorted and complete", first.Keys)
	}
	if first.Namespace != derivedNamespace {
		t.Errorf("namespace = %q, want the derived %q", first.Namespace, derivedNamespace)
	}

	live := liveSecret(t, cli, derivedNamespace, "checkout-db")
	for label, want := range map[string]string{
		"app.kubernetes.io/managed-by": "kelson",
		"kelson.dev/project":           "checkout",
		"kelson.dev/environment":       "production",
		secret.LabelManaged:            "true",
	} {
		if got := live.Labels[label]; got != want {
			t.Errorf("label %s = %q, want %q", label, got, want)
		}
	}
	if live.Type != corev1.SecretTypeOpaque {
		t.Errorf("type = %q, want Opaque", live.Type)
	}
	if got := string(live.Data["url"]); got != "postgres://user:pw@db/checkout" {
		t.Errorf("the value did not reach the cluster: %q", got)
	}

	second := mustSet(t, store, secret.SetRequest{
		Target: target(), Name: "checkout-db",
		Values: map[string]string{"url": "postgres://user:pw@db/checkout", "api-key": "sk-live-0001"},
	})
	if !reflect.DeepEqual(first.Keys, second.Keys) {
		t.Errorf("a repeated set changed the key set: %v then %v", first.Keys, second.Keys)
	}
	after := liveSecret(t, cli, derivedNamespace, "checkout-db")
	if !reflect.DeepEqual(live.Data, after.Data) {
		t.Errorf("a repeated set changed the stored data")
	}
	if len(after.ManagedFields) != 1 || after.ManagedFields[0].Manager != secret.FieldManager {
		t.Errorf("managed fields = %+v, want exactly one entry owned by %q", after.ManagedFields, secret.FieldManager)
	}
}

// TestSetMergesRatherThanReplacing: two sets of different keys leave both keys
// in place. A server-side apply of only the second key would prune the first,
// which is one command and one silently lost credential.
func TestSetMergesRatherThanReplacing(t *testing.T) {
	cli := fake.NewClientset()
	store := secret.New(cli)

	mustSet(t, store, secret.SetRequest{Target: target(), Name: "payments",
		Values: map[string]string{"api-key": "sk-live-merge-1111"}})
	got := mustSet(t, store, secret.SetRequest{Target: target(), Name: "payments",
		Values: map[string]string{"webhook-secret": "whsec-merge-2222"}})

	if !reflect.DeepEqual(got.Keys, []string{"api-key", "webhook-secret"}) {
		t.Fatalf("keys = %v, want both keys preserved", got.Keys)
	}
	live := liveSecret(t, cli, derivedNamespace, "payments")
	if string(live.Data["api-key"]) != "sk-live-merge-1111" {
		t.Errorf("the first key's value was lost: %q", live.Data["api-key"])
	}
	if string(live.Data["webhook-secret"]) != "whsec-merge-2222" {
		t.Errorf("the second key was not written: %q", live.Data["webhook-secret"])
	}
}

// TestSetOverwritesAKeyInPlace: setting an existing key replaces its value and
// leaves the rest alone. This is the rotation path ADR-0009 wants to be one
// command.
func TestSetOverwritesAKeyInPlace(t *testing.T) {
	cli := fake.NewClientset()
	store := secret.New(cli)

	mustSet(t, store, secret.SetRequest{Target: target(), Name: "payments",
		Values: map[string]string{"api-key": "sk-live-old-1111", "url": "https://api.example/v1"}})
	mustSet(t, store, secret.SetRequest{Target: target(), Name: "payments",
		Values: map[string]string{"api-key": "sk-live-new-2222"}})

	live := liveSecret(t, cli, derivedNamespace, "payments")
	if string(live.Data["api-key"]) != "sk-live-new-2222" {
		t.Errorf("the rotated key kept its old value: %q", live.Data["api-key"])
	}
	if string(live.Data["url"]) != "https://api.example/v1" {
		t.Errorf("rotating one key disturbed another: %q", live.Data["url"])
	}
}

// TestSetRegistersEveryValueItLearns is the redaction contract (#117): a value
// handed to Set is unprintable process-wide afterwards, and so is a value Set
// only read in passing while merging.
func TestSetRegistersEveryValueItLearns(t *testing.T) {
	const (
		supplied = "supplied-value-SENTINEL-b41c"
		carried  = "carried-value-SENTINEL-9e02"
	)
	cli := fake.NewClientset()
	store := secret.New(cli)

	mustSet(t, store, secret.SetRequest{Target: target(), Name: "sentinels",
		Values: map[string]string{"carried": carried}})
	mustSet(t, store, secret.SetRequest{Target: target(), Name: "sentinels",
		Values: map[string]string{"supplied": supplied}})

	for _, value := range []string{supplied, carried} {
		line := "the API server said: " + value
		if got := redact.Scrub(line); strings.Contains(got, value) {
			t.Errorf("%q was not registered for redaction: Scrub returned %q", value, got)
		}
	}
}

// TestSetRefusesToAdoptAnUnlabelledSecret: kelson does not take over a Secret
// it did not write. Labelling somebody else's object would make every later
// listing and delete claim it.
func TestSetRefusesToAdoptAnUnlabelledSecret(t *testing.T) {
	cli := fake.NewClientset(&corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "cert-manager-tls", Namespace: derivedNamespace},
		Data:       map[string][]byte{"tls.key": []byte("private")},
	})
	_, err := secret.New(cli).Set(context.Background(), secret.SetRequest{
		Target: target(), Name: "cert-manager-tls", Values: map[string]string{"url": "https://example"}})
	if err == nil {
		t.Fatal("kelson adopted an unlabelled Secret")
	}
	if got := codeOf(t, err); got != secret.ErrNotManaged {
		t.Errorf("code = %q, want %q", got, secret.ErrNotManaged)
	}
	live := liveSecret(t, cli, derivedNamespace, "cert-manager-tls")
	if _, wrote := live.Data["url"]; wrote {
		t.Error("the refused set wrote anyway")
	}
	if _, labelled := live.Labels[secret.LabelManaged]; labelled {
		t.Error("the refused set labelled the Secret")
	}
}

// TestListReportsKeysAndAgesAndNeverValues is the masked read-back: keys and
// metadata, nothing else, and only for Secrets kelson manages.
func TestListReportsKeysAndAgesAndNeverValues(t *testing.T) {
	created := time.Date(2026, 8, 14, 9, 0, 0, 0, time.UTC)
	cli := fake.NewClientset(
		&corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name: "unmanaged", Namespace: derivedNamespace,
				CreationTimestamp: metav1.NewTime(created),
			},
			Data: map[string][]byte{"token": []byte("not-kelsons")},
		},
	)
	store := secret.New(cli)
	mustSet(t, store, secret.SetRequest{Target: target(), Name: "zeta",
		Values: map[string]string{"b-key": "value-zeta-0001", "a-key": "value-zeta-0002"}})
	mustSet(t, store, secret.SetRequest{Target: target(), Name: "alpha",
		Values: map[string]string{"url": "value-alpha-0003"}})

	// The fake tracker does not stamp a creation timestamp on an apply, so the
	// age of a Secret it created is asserted through one whose timestamp the
	// fixture set.
	if err := cli.Tracker().Update(schema.GroupVersionResource{Version: "v1", Resource: "secrets"},
		&corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name: "alpha", Namespace: derivedNamespace,
				Labels:            map[string]string{secret.LabelManaged: "true"},
				CreationTimestamp: metav1.NewTime(created),
			},
			Data: map[string][]byte{"url": []byte("value-alpha-0003")},
		}, derivedNamespace); err != nil {
		t.Fatal(err)
	}

	got, err := store.List(context.Background(), target())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("listed %d Secrets, want the 2 kelson manages: %+v", len(got), got)
	}
	if got[0].Name != "alpha" || got[1].Name != "zeta" {
		t.Errorf("listing is not sorted by name: %+v", got)
	}
	if !reflect.DeepEqual(got[1].Keys, []string{"a-key", "b-key"}) {
		t.Errorf("keys = %v, want them sorted", got[1].Keys)
	}
	if age := got[0].Age(created.Add(90 * time.Minute)); age != 90*time.Minute {
		t.Errorf("age = %s, want 1h30m0s", age)
	}

	// The masking is structural: there is no field on the returned type a value
	// could travel in, which this assertion states as a compile-and-reflect
	// check rather than as a search through formatted output.
	for _, field := range reflect.VisibleFields(reflect.TypeOf(secret.Secret{})) {
		switch field.Name {
		case "Name", "Namespace", "Keys", "CreatedAt":
		default:
			t.Errorf("secret.Secret grew field %q: a masked read-back must have nowhere to put a value", field.Name)
		}
	}
}

// TestListSkipsAnUnmanagedNamespaceMember is the same rule stated from the
// other side: a namespace with nothing of kelson's lists empty rather than
// listing what belongs to somebody else.
func TestListSkipsAnUnmanagedNamespaceMember(t *testing.T) {
	cli := fake.NewClientset(&corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "default-token", Namespace: derivedNamespace},
		Data:       map[string][]byte{"token": []byte("not-kelsons")},
	})
	got, err := secret.New(cli).List(context.Background(), target())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("listed %+v, want nothing: kelson manages none of these", got)
	}
}

// TestDeleteRefusesAnUnlabelledSecret is the delete rule: a name is not a
// licence. A namespace holds Secrets kelson never wrote and delete-by-name with
// no ownership check is one typo from removing one.
func TestDeleteRefusesAnUnlabelledSecret(t *testing.T) {
	cli := fake.NewClientset(&corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "default-token", Namespace: derivedNamespace},
		Data:       map[string][]byte{"token": []byte("not-kelsons")},
	})
	err := secret.New(cli).Delete(context.Background(), secret.DeleteRequest{Target: target(), Name: "default-token"})
	if err == nil {
		t.Fatal("kelson deleted a Secret it does not manage")
	}
	if got := codeOf(t, err); got != secret.ErrNotManaged {
		t.Errorf("code = %q, want %q", got, secret.ErrNotManaged)
	}
	var se secret.Error
	_ = errors.As(err, &se)
	if !strings.Contains(se.Remediation, "kubectl") || !strings.Contains(se.Remediation, secret.LabelManaged) {
		t.Errorf("the refusal should name the deliberate adoption step, got %q", se.Remediation)
	}
	if _, err := cli.CoreV1().Secrets(derivedNamespace).Get(context.Background(), "default-token", metav1.GetOptions{}); err != nil {
		t.Errorf("the refused delete removed the Secret anyway: %v", err)
	}
}

// TestDeleteRemovesAManagedSecret is the other half: what kelson wrote, kelson
// deletes.
func TestDeleteRemovesAManagedSecret(t *testing.T) {
	cli := fake.NewClientset()
	store := secret.New(cli)
	mustSet(t, store, secret.SetRequest{Target: target(), Name: "checkout-db",
		Values: map[string]string{"url": "postgres://delete-me-0001"}})

	if err := store.Delete(context.Background(), secret.DeleteRequest{Target: target(), Name: "checkout-db"}); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	_, err := cli.CoreV1().Secrets(derivedNamespace).Get(context.Background(), "checkout-db", metav1.GetOptions{})
	if !apierrors.IsNotFound(err) {
		t.Errorf("the Secret survived the delete: %v", err)
	}

	err = store.Delete(context.Background(), secret.DeleteRequest{Target: target(), Name: "checkout-db"})
	if got := codeOf(t, err); got != secret.ErrNotFound {
		t.Errorf("deleting it twice: code = %q, want %q", got, secret.ErrNotFound)
	}
}

// TestDryRunAsksTheAPIServerForOne. The fake's tracker does not honour dry-run
// — it persists whatever it is handed — so what is asserted is the request
// kelson makes, which is the only half of a server-side dry run that belongs to
// kelson. The reactor short-circuits the write so the assertion is not read
// against an object the fake persisted regardless.
func TestDryRunAsksTheAPIServerForOne(t *testing.T) {
	cli := fake.NewClientset()
	var seen []string
	cli.PrependReactor("patch", "secrets", func(action k8stesting.Action) (bool, runtime.Object, error) {
		patch, ok := action.(k8stesting.PatchActionImpl)
		if !ok {
			t.Fatalf("want a PatchActionImpl, got %T", action)
		}
		seen = patch.PatchOptions.DryRun
		return true, &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: patch.Name, Namespace: patch.Namespace},
			Data:       map[string][]byte{"url": nil},
		}, nil
	})

	if _, err := secret.New(cli).Set(context.Background(), secret.SetRequest{
		Target: target(), Name: "checkout-db", DryRun: true,
		Values: map[string]string{"url": "postgres://dry-run-0001"},
	}); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if !reflect.DeepEqual(seen, []string{metav1.DryRunAll}) {
		t.Errorf("dry-run option = %v, want [All]", seen)
	}
	if _, err := cli.CoreV1().Secrets(derivedNamespace).Get(context.Background(), "checkout-db", metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Errorf("the dry run persisted something: %v", err)
	}
}

// TestDryRunDeleteAsksTheAPIServerForOne is the same for the delete path.
func TestDryRunDeleteAsksTheAPIServerForOne(t *testing.T) {
	cli := fake.NewClientset(&corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name: "checkout-db", Namespace: derivedNamespace,
			Labels: map[string]string{secret.LabelManaged: "true"},
		},
	})
	var seen []string
	cli.PrependReactor("delete", "secrets", func(action k8stesting.Action) (bool, runtime.Object, error) {
		del, ok := action.(k8stesting.DeleteActionImpl)
		if !ok {
			t.Fatalf("want a DeleteActionImpl, got %T", action)
		}
		seen = del.DeleteOptions.DryRun
		return true, nil, nil
	})

	if err := secret.New(cli).Delete(context.Background(), secret.DeleteRequest{
		Target: target(), Name: "checkout-db", DryRun: true}); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if !reflect.DeepEqual(seen, []string{metav1.DryRunAll}) {
		t.Errorf("dry-run option = %v, want [All]", seen)
	}
}

// TestMissingNamespaceIsItsOwnCode: an apply into a namespace that is not there
// is not a generic write failure. The remediation has to say "deploy the
// environment", and a caller has to be able to branch on it.
func TestMissingNamespaceIsItsOwnCode(t *testing.T) {
	cli := fake.NewClientset()
	cli.PrependReactor("patch", "secrets", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewNotFound(schema.GroupResource{Resource: "namespaces"}, derivedNamespace)
	})
	_, err := secret.New(cli).Set(context.Background(), secret.SetRequest{
		Target: target(), Name: "checkout-db", Values: map[string]string{"url": "postgres://x-0001"}})
	if got := codeOf(t, err); got != secret.ErrNamespaceMissing {
		t.Errorf("code = %q, want %q", got, secret.ErrNamespaceMissing)
	}
	if !strings.Contains(err.Error(), derivedNamespace) {
		t.Errorf("the error should name the namespace, got %q", err.Error())
	}
}

// TestRequestsThatCannotBeWrittenAreRefusedBeforeTheCluster: every shape that
// has no reading is a structured refusal, and no client call is made for it.
func TestRequestsThatCannotBeWrittenAreRefusedBeforeTheCluster(t *testing.T) {
	for _, tc := range []struct {
		name string
		req  secret.SetRequest
		want secret.Code
	}{
		{"no environment", secret.SetRequest{
			Target: secret.Target{Project: "checkout"}, Name: "db",
			Values: map[string]string{"url": "postgres://x-0001"}}, secret.ErrInvalidTarget},
		{"no project", secret.SetRequest{
			Target: secret.Target{Environment: "production"}, Name: "db",
			Values: map[string]string{"url": "postgres://x-0001"}}, secret.ErrInvalidTarget},
		{"no name", secret.SetRequest{
			Target: target(), Values: map[string]string{"url": "postgres://x-0001"}}, secret.ErrInvalidName},
		{"name is not a DNS-1123 label", secret.SetRequest{
			Target: target(), Name: "Checkout_DB",
			Values: map[string]string{"url": "postgres://x-0001"}}, secret.ErrInvalidName},
		{"no values", secret.SetRequest{Target: target(), Name: "db"}, secret.ErrNoValues},
		{"key has a slash", secret.SetRequest{
			Target: target(), Name: "db",
			Values: map[string]string{"db/url": "postgres://x-0001"}}, secret.ErrInvalidKey},
		{"key has a space", secret.SetRequest{
			Target: target(), Name: "db",
			Values: map[string]string{"db url": "postgres://x-0001"}}, secret.ErrInvalidKey},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cli := fake.NewClientset()
			_, err := secret.New(cli).Set(context.Background(), tc.req)
			if got := codeOf(t, err); got != tc.want {
				t.Fatalf("code = %q, want %q (%v)", got, tc.want, err)
			}
			if len(cli.Actions()) != 0 {
				t.Errorf("a refused request still talked to the cluster: %+v", cli.Actions())
			}
		})
	}
}

// TestNamespaceOverrideIsHonoured: an Environment whose spec sets
// spec.namespace does not target `<project>-<environment>`, and a Secret
// written into the derived name would be one nothing reads.
func TestNamespaceOverrideIsHonoured(t *testing.T) {
	cli := fake.NewClientset()
	got := mustSet(t, secret.New(cli), secret.SetRequest{
		Target: secret.Target{Project: "checkout", Environment: "production", Namespace: "shop-live"},
		Name:   "checkout-db", Values: map[string]string{"url": "postgres://override-0001"}})
	if got.Namespace != "shop-live" {
		t.Fatalf("namespace = %q, want the override", got.Namespace)
	}
	liveSecret(t, cli, "shop-live", "checkout-db")
}

// TestErrorsCarryTheirDocsURL: `secret/*` is a taxonomy, and a code with no
// documentation link is one an agent cannot follow up on.
func TestErrorsCarryTheirDocsURL(t *testing.T) {
	_, err := secret.New(fake.NewClientset()).Set(context.Background(), secret.SetRequest{Target: target()})
	var se secret.Error
	if !errors.As(err, &se) {
		t.Fatalf("want a secret.Error, got %T", err)
	}
	if se.DocsURL != secret.DocsBaseURL+"/secret-invalid-name" {
		t.Errorf("docs URL = %q", se.DocsURL)
	}
	if !secret.AsCode(err, secret.ErrInvalidName) {
		t.Error("AsCode did not recognise the error it was written for")
	}
}
