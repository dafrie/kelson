package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"github.com/dafrie/kelson/internal/secret"
)

// `kelson secret` is the one command that takes a credential as an argument, so
// the assertion running through this file is the same one every time: the value
// went where it was supposed to go and appeared in none of the output.
//
// The store is faked for the reason every command test here fakes its plane —
// the real one needs a cluster. What the fake preserves is the part the command
// depends on: the request it was handed, so the tests can assert what reached
// the store rather than what the command claimed to have sent.

// setSentinel is the value every test writes. It is distinctive so a search
// through output is conclusive rather than suggestive.
const setSentinel = "s3cr3t-VALUE-SENTINEL-4d9a"

type fakeSecretStore struct {
	sets    []secret.SetRequest
	lists   []secret.Target
	deletes []secret.DeleteRequest

	secrets []secret.Secret
	// keys is the read-back's key list; empty means "whatever the set wrote".
	keys []string
	err  error
}

func (f *fakeSecretStore) Set(_ context.Context, req secret.SetRequest) (secret.Secret, error) {
	f.sets = append(f.sets, req)
	if f.err != nil {
		return secret.Secret{}, f.err
	}
	keys := f.keys
	if keys == nil {
		for k := range req.Values {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	namespace, err := req.Resolve()
	if err != nil {
		return secret.Secret{}, err
	}
	return secret.Secret{Name: req.Name, Namespace: namespace, Keys: keys}, nil
}

func (f *fakeSecretStore) List(_ context.Context, t secret.Target) ([]secret.Secret, error) {
	f.lists = append(f.lists, t)
	return f.secrets, f.err
}

func (f *fakeSecretStore) Delete(_ context.Context, req secret.DeleteRequest) error {
	f.deletes = append(f.deletes, req)
	return f.err
}

func (f *fakeSecretStore) connector() secretConnector {
	return func(string) (secretStore, error) { return f, nil }
}

func rootWithSecrets(connect secretConnector) *cobra.Command {
	return rootWithSecretBackends(connect, nil)
}

func rootWithSecretBackends(connect secretConnector, connectSops sopsSecretConnector) *cobra.Command {
	root := newRootCmd()
	for _, c := range root.Commands() {
		if c.Name() == "secret" {
			root.RemoveCommand(c)
		}
	}
	root.AddCommand(newSecretCmdFactory(connect, connectSops))
	return root
}

func runSecret(t *testing.T, store *fakeSecretStore, args ...string) (stdout string, code int, msg string) {
	t.Helper()
	return runSecretStdin(t, store, "", args...)
}

func runSecretStdin(t *testing.T, store *fakeSecretStore, stdin string, args ...string) (stdout string, code int, msg string) {
	t.Helper()
	cmd := rootWithSecrets(store.connector())
	var outBuf, errBuf bytes.Buffer
	cmd.SetOut(&outBuf)
	cmd.SetErr(&errBuf)
	cmd.SetIn(strings.NewReader(stdin))
	cmd.SetArgs(args)
	err := cmd.Execute()
	msg, code = resolveExit(err)
	return outBuf.String(), code, msg
}

// assertNoValue is the property this whole file exists for.
func assertNoValue(t *testing.T, where, body string) {
	t.Helper()
	if strings.Contains(body, setSentinel) {
		t.Errorf("the value reached %s:\n%s", where, body)
	}
}

// --- set ----------------------------------------------------------------------

// TestSecretSetSendsTheValueAndPrintsOnlyKeys is the command's contract: the
// value reaches the store, the target is derived from --project/--env, and the
// confirmation names keys.
func TestSecretSetSendsTheValueAndPrintsOnlyKeys(t *testing.T) {
	store := &fakeSecretStore{}
	stdout, code, msg := runSecret(t, store,
		"secret", "set", "checkout-db", "--project", "checkout", "--env", "production",
		"url="+setSentinel)
	if code != exitOK {
		t.Fatalf("exit = %d (%s)\n%s", code, msg, stdout)
	}
	if len(store.sets) != 1 {
		t.Fatalf("the store saw %d sets, want 1", len(store.sets))
	}
	got := store.sets[0]
	if got.Name != "checkout-db" || got.Project != "checkout" || got.Environment != "production" {
		t.Errorf("request = %+v, want the flags to address it", got)
	}
	if got.Values["url"] != setSentinel {
		t.Errorf("the value did not reach the store: %q", got.Values["url"])
	}
	if got.DryRun {
		t.Error("a plain set asked for a dry run")
	}

	assertNoValue(t, "stdout", stdout)
	assertNoValue(t, "the error message", msg)
	for _, want := range []string{"checkout-db", "checkout-production", "url"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("stdout should name %q:\n%s", want, stdout)
		}
	}
	// The confirmation earns its keep by showing how to reference what was
	// written; an author who just set a secret is one step from the spec edit.
	if !strings.Contains(stdout, "{ secret: checkout-db, key: url }") {
		t.Errorf("the confirmation should show the reference form:\n%s", stdout)
	}
}

// TestSecretSetFromStdinKeepsTheValueOffTheCommandLine is why --from-stdin
// exists: a credential typed as an argument outlives the secret in a shell
// history and in a CI job's recorded command line.
func TestSecretSetFromStdinKeepsTheValueOffTheCommandLine(t *testing.T) {
	store := &fakeSecretStore{}
	stdout, code, msg := runSecretStdin(t, store, setSentinel+"\n",
		"secret", "set", "checkout-db", "--project", "checkout", "--env", "production",
		"--from-stdin", "password")
	if code != exitOK {
		t.Fatalf("exit = %d (%s)\n%s", code, msg, stdout)
	}
	got := store.sets[0].Values["password"]
	if got != setSentinel {
		t.Errorf("value = %q, want the sentinel with its trailing newline stripped", got)
	}
	assertNoValue(t, "stdout", stdout)
}

// TestSecretSetFromFileReadsBytesVerbatim: a file is the form for a value that
// legitimately contains newlines — a private key, a service-account JSON — so
// nothing is trimmed.
func TestSecretSetFromFileReadsBytesVerbatim(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "tls.key")
	body := "-----BEGIN KEY-----\n" + setSentinel + "\n-----END KEY-----\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	store := &fakeSecretStore{}
	stdout, code, msg := runSecret(t, store,
		"secret", "set", "tls", "--project", "checkout", "--env", "production",
		"--from-file", "tls.key="+path)
	if code != exitOK {
		t.Fatalf("exit = %d (%s)\n%s", code, msg, stdout)
	}
	if got := store.sets[0].Values["tls.key"]; got != body {
		t.Errorf("value = %q, want the file's bytes verbatim", got)
	}
	assertNoValue(t, "stdout", stdout)
}

// TestSecretSetRefusesADuplicateKey: `password=…` and `--from-stdin password`
// in one command means one of them, and picking either would write a credential
// the caller did not choose.
func TestSecretSetRefusesADuplicateKey(t *testing.T) {
	store := &fakeSecretStore{}
	stdout, code, msg := runSecretStdin(t, store, "other-value\n",
		"secret", "set", "checkout-db", "--project", "checkout", "--env", "production",
		"password="+setSentinel, "--from-stdin", "password")
	if code != exitErr {
		t.Fatalf("exit = %d, want %d\n%s", code, exitErr, stdout)
	}
	if len(store.sets) != 0 {
		t.Error("the refused command still wrote")
	}
	if !strings.Contains(msg, "supplied twice") {
		t.Errorf("message = %q, want it to name the duplicate", msg)
	}
	assertNoValue(t, "the error message", msg)
	assertNoValue(t, "stdout", stdout)
}

// TestSecretSetRefusesAMalformedPair: a bare word is not key=value, and the
// message points at the two forms that keep a value out of the shell history.
func TestSecretSetRefusesAMalformedPair(t *testing.T) {
	store := &fakeSecretStore{}
	_, code, msg := runSecret(t, store,
		"secret", "set", "checkout-db", "--project", "checkout", "--env", "production", "url")
	if code != exitErr {
		t.Fatalf("exit = %d, want %d", code, exitErr)
	}
	if !strings.Contains(msg, "--from-stdin") {
		t.Errorf("message = %q, want it to name the safer forms", msg)
	}
}

// TestSecretSetDryRunSaysNothingWasStored: the flag reaches the store and the
// output does not claim a write.
func TestSecretSetDryRunSaysNothingWasStored(t *testing.T) {
	store := &fakeSecretStore{}
	stdout, code, msg := runSecret(t, store,
		"secret", "set", "checkout-db", "--project", "checkout", "--env", "production",
		"--dry-run", "url="+setSentinel)
	if code != exitOK {
		t.Fatalf("exit = %d (%s)\n%s", code, msg, stdout)
	}
	if !store.sets[0].DryRun {
		t.Error("--dry-run did not reach the store")
	}
	if !strings.Contains(stdout, "stored nothing") {
		t.Errorf("a dry run must say so:\n%s", stdout)
	}
	if strings.Contains(stdout, "wrote Secret") {
		t.Errorf("a dry run claimed a write:\n%s", stdout)
	}
	assertNoValue(t, "stdout", stdout)
}

// TestSecretSetReportsPreservedKeys makes the merge visible: a user who
// expected `set` to replace the Secret finds out here.
func TestSecretSetReportsPreservedKeys(t *testing.T) {
	store := &fakeSecretStore{keys: []string{"api-key", "url"}}
	stdout, code, msg := runSecret(t, store,
		"secret", "set", "checkout-db", "--project", "checkout", "--env", "production",
		"url="+setSentinel)
	if code != exitOK {
		t.Fatalf("exit = %d (%s)\n%s", code, msg, stdout)
	}
	if !strings.Contains(stdout, "keys kept:") || !strings.Contains(stdout, "api-key") {
		t.Errorf("the untouched key should be reported:\n%s", stdout)
	}
	assertNoValue(t, "stdout", stdout)
}

// TestSecretSetSurfacesTheStoreRefusal: a `secret/*` refusal reaches the user
// with its own remediation rather than being reworded here.
func TestSecretSetSurfacesTheStoreRefusal(t *testing.T) {
	store := &fakeSecretStore{err: secret.Error{
		Code:        secret.ErrNotManaged,
		Resource:    "Secret/checkout-production/cert",
		Message:     "Secret \"cert\" in namespace \"checkout-production\" exists but is not managed by kelson",
		Remediation: "use a different name",
	}}
	stdout, code, msg := runSecret(t, store,
		"secret", "set", "cert", "--project", "checkout", "--env", "production", "url="+setSentinel)
	if code != exitErr {
		t.Fatalf("exit = %d, want %d\n%s", code, exitErr, stdout)
	}
	if !strings.Contains(msg, string(secret.ErrNotManaged)) {
		t.Errorf("message = %q, want the store's own code", msg)
	}
	assertNoValue(t, "the error message", msg)
}

// TestSecretSetPassesTheNamespaceOverride: an Environment with spec.namespace
// does not target `<project>-<environment>`.
func TestSecretSetPassesTheNamespaceOverride(t *testing.T) {
	store := &fakeSecretStore{}
	stdout, code, msg := runSecret(t, store,
		"secret", "set", "checkout-db", "--project", "checkout", "--env", "production",
		"--namespace", "shop-live", "url="+setSentinel)
	if code != exitOK {
		t.Fatalf("exit = %d (%s)\n%s", code, msg, stdout)
	}
	if store.sets[0].Namespace != "shop-live" {
		t.Errorf("namespace = %q, want the override", store.sets[0].Namespace)
	}
	if !strings.Contains(stdout, "shop-live") {
		t.Errorf("the confirmation should name the namespace written to:\n%s", stdout)
	}
}

// --- list ---------------------------------------------------------------------

// TestSecretListPrintsKeysAndAgesAndNoValues is the masked read-back at the
// command layer. The store's type has no value to leak, so what is asserted
// here is that the command prints what it should and nothing extra.
func TestSecretListPrintsKeysAndAgesAndNoValues(t *testing.T) {
	now := time.Date(2026, 8, 14, 12, 0, 0, 0, time.UTC)
	original := secretClock
	secretClock = func() time.Time { return now }
	t.Cleanup(func() { secretClock = original })

	store := &fakeSecretStore{secrets: []secret.Secret{
		{Name: "checkout-db", Namespace: "checkout-production", Keys: []string{"url"}, CreatedAt: now.Add(-90 * time.Minute)},
		{Name: "payments", Namespace: "checkout-production", Keys: []string{"api-key", "webhook-secret"}, CreatedAt: now.Add(-72 * time.Hour)},
	}}
	stdout, code, msg := runSecret(t, store, "secret", "list", "--project", "checkout", "--env", "production")
	if code != exitOK {
		t.Fatalf("exit = %d (%s)\n%s", code, msg, stdout)
	}
	for _, want := range []string{"checkout-production", "checkout-db", "url", "payments", "api-key", "webhook-secret", "1h", "3d"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("stdout should contain %q:\n%s", want, stdout)
		}
	}
	assertNoValue(t, "stdout", stdout)
	if store.lists[0].Project != "checkout" || store.lists[0].Environment != "production" {
		t.Errorf("target = %+v, want the flags", store.lists[0])
	}
}

// TestSecretListEmptyNamesTheCommandThatFillsIt: an empty listing is a normal
// state and the next step belongs in it.
func TestSecretListEmptyNamesTheCommandThatFillsIt(t *testing.T) {
	store := &fakeSecretStore{}
	stdout, code, msg := runSecret(t, store, "secret", "list", "--project", "checkout", "--env", "production")
	if code != exitOK {
		t.Fatalf("exit = %d (%s)\n%s", code, msg, stdout)
	}
	if !strings.Contains(stdout, "kelson secret set") {
		t.Errorf("an empty listing should name the command that writes one:\n%s", stdout)
	}
}

// --- delete -------------------------------------------------------------------

// TestSecretDeleteAsksBeforeDeleting: an unconfirmed delete removes nothing. A
// closed stdin is a "no", which is what an unattended run must get.
func TestSecretDeleteAsksBeforeDeleting(t *testing.T) {
	store := &fakeSecretStore{}
	stdout, code, msg := runSecret(t, store,
		"secret", "delete", "checkout-db", "--project", "checkout", "--env", "production")
	if code != exitOK {
		t.Fatalf("exit = %d (%s)\n%s", code, msg, stdout)
	}
	if len(store.deletes) != 0 {
		t.Error("an unconfirmed delete removed the Secret")
	}
	if !strings.Contains(stdout, "aborted") {
		t.Errorf("stdout should say nothing happened:\n%s", stdout)
	}
}

// TestSecretDeleteWithYesDeletes is the unattended path.
func TestSecretDeleteWithYesDeletes(t *testing.T) {
	store := &fakeSecretStore{}
	stdout, code, msg := runSecret(t, store,
		"secret", "delete", "checkout-db", "--project", "checkout", "--env", "production", "--yes")
	if code != exitOK {
		t.Fatalf("exit = %d (%s)\n%s", code, msg, stdout)
	}
	if len(store.deletes) != 1 || store.deletes[0].Name != "checkout-db" {
		t.Fatalf("deletes = %+v", store.deletes)
	}
	if !strings.Contains(stdout, "deleted Secret checkout-db") {
		t.Errorf("stdout should confirm the delete:\n%s", stdout)
	}
	// The dependency between a reference and the Secret it names is invisible
	// to kelson (ADR-0018), so the command says so rather than implying a check
	// it did not make.
	if !strings.Contains(stdout, "next pod start") {
		t.Errorf("the delete should warn about referencing workloads:\n%s", stdout)
	}
}

// TestSecretDeleteRefusalReachesTheUser: the store's `secret/not-managed` is
// what a delete-by-name of somebody else's Secret gets, and the command relays
// it rather than translating it.
func TestSecretDeleteRefusalReachesTheUser(t *testing.T) {
	store := &fakeSecretStore{err: secret.Error{
		Code:        secret.ErrNotManaged,
		Resource:    "Secret/checkout-production/default-token",
		Message:     "not managed by kelson",
		Remediation: "kelson deletes only what it labelled",
	}}
	_, code, msg := runSecret(t, store,
		"secret", "delete", "default-token", "--project", "checkout", "--env", "production", "--yes")
	if code != exitErr {
		t.Fatalf("exit = %d, want %d", code, exitErr)
	}
	if !strings.Contains(msg, string(secret.ErrNotManaged)) {
		t.Errorf("message = %q, want the store's own code", msg)
	}
}

// TestSecretDeleteDryRunRemovesNothing: a server-side dry run needs no
// confirmation, because there is nothing to confirm.
func TestSecretDeleteDryRunRemovesNothing(t *testing.T) {
	store := &fakeSecretStore{}
	stdout, code, msg := runSecret(t, store,
		"secret", "delete", "checkout-db", "--project", "checkout", "--env", "production", "--dry-run")
	if code != exitOK {
		t.Fatalf("exit = %d (%s)\n%s", code, msg, stdout)
	}
	if len(store.deletes) != 1 || !store.deletes[0].DryRun {
		t.Fatalf("deletes = %+v, want one dry run", store.deletes)
	}
	if !strings.Contains(stdout, "removed nothing") {
		t.Errorf("a dry run must say so:\n%s", stdout)
	}
}

// --- wiring -------------------------------------------------------------------

// TestSecretCommandsRequireTheirTarget: a secret belongs to an environment, and
// a command that guessed one would write into the wrong namespace.
//
// --env stays a required flag. --project became optional when -f arrived
// (ADR-0021): the spec names the project, and reading the spec is also how
// kelson learns which backend holds the value. Without either, the refusal has
// to name both ways out.
func TestSecretCommandsRequireTheirTarget(t *testing.T) {
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"secret", "set", "db", "--project", "checkout", "url=x"}, "required flag"},
		{[]string{"secret", "delete", "db", "--project", "checkout", "--yes"}, "required flag"},
		{[]string{"secret", "set", "db", "--env", "production", "url=x"}, "pass --project, or pass -f"},
		{[]string{"secret", "list", "--env", "production"}, "pass --project, or pass -f"},
	} {
		store := &fakeSecretStore{}
		_, code, msg := runSecret(t, store, tc.args...)
		if code != exitErr {
			t.Errorf("%v: exit = %d, want %d", tc.args, code, exitErr)
		}
		if !strings.Contains(msg, tc.want) {
			t.Errorf("%v: message = %q, want %q", tc.args, msg, tc.want)
		}
	}
}

// TestSecretIsRegisteredOnTheRoot: the command has to exist on the real root,
// not only on the test harness's.
func TestSecretIsRegisteredOnTheRoot(t *testing.T) {
	var found *cobra.Command
	for _, c := range newRootCmd().Commands() {
		if c.Name() == "secret" {
			found = c
		}
	}
	if found == nil {
		t.Fatal("`kelson secret` is not registered on the root command")
	}
	want := map[string]bool{"set": false, "list": false, "delete": false}
	for _, c := range found.Commands() {
		if _, ok := want[c.Name()]; ok {
			want[c.Name()] = true
		}
	}
	for name, present := range want {
		if !present {
			t.Errorf("`kelson secret %s` is missing", name)
		}
	}
}
