package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/dafrie/kelson/internal/delivery"
	"github.com/dafrie/kelson/internal/delivery/kube"
	"github.com/dafrie/kelson/internal/model"
	"github.com/dafrie/kelson/internal/secret"
)

// `kelson secret` is the authoring half of ADR-0009's cluster backend (issue
// #116): the command ADR-0018 said the secret-literal remediation would name
// once it existed.
//
// It is the only command in this binary that takes a credential as an argument,
// so three rules govern the whole file:
//
//   - **A value is never echoed.** Every confirmation prints key names, the
//     Secret's name and its namespace. There is no --show, no readback of a
//     value and no verbose mode that would add one; internal/secret's read-back
//     type has no field a value could arrive in, so this is enforced by the
//     seam rather than by discipline here.
//   - **A value need never reach the shell history.** `key=value` is the
//     convenient form and it lands in ~/.bash_history and in a CI job's
//     recorded command line, so --from-file and --from-stdin exist beside it
//     and the help text says why rather than leaving the reader to work it out.
//   - **The target is a (project, environment) pair, not a namespace.** A
//     secret's lifecycle is the environment's (issue #116), and the namespace
//     is derived exactly as the renderer derives it, so a Secret cannot land
//     somewhere the workload will not look. --namespace exists only for an
//     Environment whose spec.namespace overrides the default.

// secretStore is the capability `kelson secret` needs from whichever backend
// holds the value, so this file selects a backend and never a code path. It is
// declared here rather than imported as a concrete type for the same reason
// observationConnector is: the production implementation needs a live cluster,
// and none of the command wiring under test does.
//
// One implementation satisfies it today — internal/secret's cluster Store. The
// sops backend had a second, writing encrypted Secrets into the delivery
// repository through the git writer, and it went with the writer (ADR-0028).
// What replaces it is decided and not built: ADR-0028 decision 7 keeps SOPS
// end to end and ships the encrypted Secrets *inside the published artifact*,
// beside the workloads that reference them, with the Kustomization kelson owns
// carrying spec.decryption. Until then the backend is refused by name
// ([sopsUnavailable]), never quietly written somewhere else.
type secretStore interface {
	Set(ctx context.Context, req secret.SetRequest) (secret.Secret, error)
	List(ctx context.Context, t secret.Target) ([]secret.Secret, error)
	Delete(ctx context.Context, req secret.DeleteRequest) error
}

// secretConnector builds the cluster store for one command run. It is the seam
// the tests replace.
type secretConnector func(kubeconfig string) (secretStore, error)

// connectSecrets is the production cluster connector: one cluster connection,
// the typed clientset behind it. It is the same reach `kelson status` and
// `kelson deploy` make — kube.Connect resolves the kubeconfig and this plane's
// lint allow-list keeps the client libraries out of cmd/ (.golangci.yml).
func connectSecrets(kubeconfig string) (secretStore, error) {
	cluster, err := kube.Connect(kubeconfig)
	if err != nil {
		return nil, err
	}
	return secret.New(cluster.Typed), nil
}

// sopsUnavailable is the refusal the sops backend now gets. It names what the
// backend was doing, what replaces it and where that is tracked, so an author
// whose Environment selects sops learns that kelson stopped writing rather than
// that kelson wrote somewhere they did not expect.
func sopsUnavailable(environment string) error {
	return delivery.NotImplemented("secret",
		"environment "+environment+" selects secret backend sops, and kelson cannot write it: the git "+
			"writer that committed the encrypted Secret was deleted with the old delivery machinery. "+
			"SOPS itself is unaffected — encryption, the age recipients and in-cluster decryption are "+
			"unchanged (ADR-0028 decision 7) — what is missing is the destination",
		"#224")
}

func newSecretCmd() *cobra.Command { return newSecretCmdFactory(connectSecrets) }

func newSecretCmdFactory(connect secretConnector) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "secret",
		Short: "Write, list and delete the Secrets an environment's references point at",
		Long: "A kelson spec carries secret references, never values (ADR-0009). `{secret: <name>, key: <key>}`\n" +
			"renders as a valueFrom.secretKeyRef against a Secret in the environment's namespace; these commands\n" +
			"are what writes that Secret.\n\n" +
			"Where the value goes is the Environment's secrets.backend, and passing -f is what lets these commands\n" +
			"read it:\n\n" +
			"  cluster (default)  the value is written straight to the cluster's API server. kelson does not store\n" +
			"                     it and reads it back masked. Secrets are NOT in the Git artifact — rebuilding a\n" +
			"                     cluster from Git alone will not restore them.\n" +
			"  sops               NOT AVAILABLE while the delivery spine is rebuilt (issue #224). The value is\n" +
			"                     meant to be encrypted with age and shipped inside the published artifact for\n" +
			"                     Flux to decrypt in-cluster; the writer that used to commit it is deleted, so\n" +
			"                     kelson refuses rather than writing it somewhere else.\n" +
			"  externalSecrets    no value passes through kelson at all — write it in your secret manager.\n\n" +
			"kelson never holds an age private key. `kelson secret set` needs only the public age1… recipients from\n" +
			"secrets.ageRecipients; the identity that decrypts lives in a Secret you create for Flux.",
		Args: cobra.NoArgs,
	}
	cmd.AddCommand(
		newSecretSetCmd(connect),
		newSecretListCmd(connect),
		newSecretDeleteCmd(connect),
		newSecretRotateCmd(connect),
	)
	return cmd
}

// secretTarget is the addressing every subcommand takes: a (project,
// environment) pair, given directly or read from a spec.
type secretTarget struct {
	files       []string
	project     string
	environment string
	namespace   string
	kubeconfig  string
	connect     secretConnector
}

func (t *secretTarget) bind(cmd *cobra.Command) {
	f := cmd.Flags()
	f.StringArrayVarP(&t.files, "file", "f", nil,
		"spec YAML holding the Project and Environment (repeatable). Required for backend sops and for any backend "+
			"whose spec you want kelson to read; without it kelson assumes backend cluster")
	f.StringVar(&t.project, "project", "", "name of the Project the secret belongs to (derived from -f when given)")
	f.StringVar(&t.environment, "env", "", "name of the Environment the secret belongs to")
	f.StringVar(&t.namespace, "namespace", "",
		"target namespace, overriding the derived <project>-<environment> (only needed when the Environment sets spec.namespace)")
	f.StringVar(&t.kubeconfig, "kubeconfig", "",
		"path to a kubeconfig (default: $KUBECONFIG, in-cluster credentials, then ~/.kube/config); backend cluster only")
	cobra.CheckErr(cmd.MarkFlagRequired("env"))
}

func (t *secretTarget) target() secret.Target {
	return secret.Target{Project: t.project, Environment: t.environment, Namespace: t.namespace}
}

// resolve loads the spec when -f is given and fills in the addressing from it.
// Without -f the flags are the whole of the addressing and the backend is
// assumed to be `cluster` — which is what it is for every environment that
// never set one.
func (t *secretTarget) resolve() (*model.Resolved, error) {
	if len(t.files) == 0 {
		if t.project == "" {
			return nil, fmt.Errorf("pass --project, or pass -f with the spec so kelson can read it (which is also " +
				"how kelson learns the environment's secrets.backend)")
		}
		return nil, nil
	}
	project, environments, _, err := loadSpecFiles(t.files)
	if err != nil {
		return nil, err
	}
	environment, err := selectEnvironment(environments, t.environment)
	if err != nil {
		return nil, err
	}
	resolved, errs := model.Resolve(project, environment)
	if len(errs) > 0 {
		return nil, errs
	}
	t.project = resolved.Project
	t.environment = resolved.Environment.Name
	if t.namespace == "" {
		t.namespace = resolved.Environment.Namespace
	}
	return resolved, nil
}

// store resolves the spec and returns the backend's store, or the backend's
// refusal. Every subcommand goes through here, so a backend kelson cannot write
// is refused once, in one voice, whichever verb asked.
func (t *secretTarget) store() (secretStore, error) {
	resolved, err := t.resolve()
	if err != nil {
		return nil, err
	}
	if resolved == nil {
		return t.connect(t.kubeconfig)
	}
	switch resolved.Environment.Secrets.Backend {
	case model.SecretsSOPS:
		return nil, sopsUnavailable(resolved.Environment.Name)
	case model.SecretsExternalSecrets:
		// Writing a cluster Secret here would put kelson in a fight with the
		// external-secrets controller over an object it owns
		// (creationPolicy: Owner, ADR-0020) — kelson's keys would survive
		// until the next sync and then vanish. Under that backend no value
		// passes through kelson at all, and saying so is the whole point.
		return nil, fmt.Errorf("environment %q uses secret backend externalSecrets, where the value is "+
			"written in your secret manager and external-secrets syncs it into the cluster. kelson never holds "+
			"it (ADR-0020): write it at the store this environment reads from (%s), or change "+
			"secrets.backend if you meant kelson to hold it",
			resolved.Environment.Name, storeOrAny(resolved.Environment.Secrets.Store))
	default:
		return t.connect(t.kubeconfig)
	}
}

func storeOrAny(name string) string {
	if name == "" {
		return "the SecretStore this environment resolves to"
	}
	return "secrets.store " + name
}

// --- set ---------------------------------------------------------------------

type secretSetOptions struct {
	secretTarget
	fromFile  map[string]string
	fromStdin string
	dryRun    bool
}

func newSecretSetCmd(connect secretConnector) *cobra.Command {
	opts := &secretSetOptions{secretTarget: secretTarget{connect: connect}}
	cmd := &cobra.Command{
		Use:   "set <name> --env <environment> [-f spec.yaml] [key=value ...]",
		Short: "Write keys into a Secret in the environment's namespace",
		Long: "Set writes the given keys into the named Secret, creating it if it does not exist, and prints the\n" +
			"resulting key list. Values are never printed back.\n\n" +
			"Under backend cluster, keys not named here are preserved: `set` adds and replaces keys, it does not\n" +
			"replace the Secret. Under backend sops it replaces the file, because carrying the other keys forward\n" +
			"would need the age identity kelson never holds — so a set that would drop keys is refused with those\n" +
			"keys named, and you pass them all.\n\n" +
			"Three ways to supply a value, and the first is the one to avoid for a real credential:\n\n" +
			"  key=value            convenient, but the value lands in your shell history and in a CI job's\n" +
			"                       recorded command line, where it outlives the secret\n" +
			"  --from-stdin <key>   reads the whole of stdin as that key's value; one trailing newline is\n" +
			"                       stripped, because a value piped through `echo` carries one\n" +
			"  --from-file <key>=<path>\n" +
			"                       reads the file's bytes verbatim, newlines and all\n\n" +
			"kelson labels what it writes and refuses to take over a Secret it did not write, so a Secret created\n" +
			"by an operator or by kubectl is reported rather than silently adopted.",
		Example: "  kelson secret set checkout-db --project checkout --env production url=postgres://…\n" +
			"  read -rs PW && printf '%s' \"$PW\" | kelson secret set checkout-db --project checkout --env production --from-stdin password\n" +
			"  kelson secret set checkout-db -f spec.yaml --env production url=postgres://…   # encrypted under backend sops\n" +
			"  kelson secret set tls --project checkout --env production --from-file tls.key=./tls.key --from-file tls.crt=./tls.crt",
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error { return runSecretSet(cmd, opts, args) },
	}
	opts.bind(cmd)
	f := cmd.Flags()
	f.StringToStringVar(&opts.fromFile, "from-file", nil,
		"read a key's value from a file, verbatim: --from-file <key>=<path> (repeatable; keeps the value out of your shell history)")
	f.StringVar(&opts.fromStdin, "from-stdin", "",
		"read this key's value from stdin, stripping one trailing newline (keeps the value out of your shell history)")
	f.BoolVar(&opts.dryRun, "dry-run", false,
		"validate the write and discard it; nothing is stored and nothing is committed")
	return cmd
}

func runSecretSet(cmd *cobra.Command, opts *secretSetOptions, args []string) error {
	name := args[0]
	values, err := collectValues(cmd, opts, args[1:])
	if err != nil {
		return err
	}
	store, err := opts.store()
	if err != nil {
		return err
	}

	written, err := store.Set(cmd.Context(), secret.SetRequest{
		Target: opts.target(), Name: name, Values: values, DryRun: opts.dryRun,
	})
	if err != nil {
		return err
	}

	out := &printer{w: cmd.OutOrStdout()}
	verb := "wrote"
	if opts.dryRun {
		verb = "would write"
	}
	out.printf("%s Secret %s in namespace %s\n", verb, written.Name, written.Namespace)
	out.printf("  keys set:     %s\n", strings.Join(sortedNames(values), ", "))
	if preserved := preservedKeys(written.Keys, values); len(preserved) > 0 {
		out.printf("  keys kept:    %s\n", strings.Join(preserved, ", "))
	}
	if opts.dryRun {
		out.printf("\ndry run: the API server validated this and stored nothing.\n")
		return out.err
	}
	out.printf("\nreference a key from a component's env:\n")
	out.printf("  env:\n    MY_VARIABLE: { secret: %s, key: %s }\n", written.Name, sortedNames(values)[0])
	return out.err
}

// The operator-half reminder that used to print here — the decryption block a
// Flux Kustomization needs and the Secret holding the age identity — went with
// the sops writer (ADR-0028). It belongs beside whatever writes the encrypted
// Secret next, and printing it from a command that writes nothing would tell an
// operator to wire decryption for a file that is not there.

// collectValues assembles the key set from the three input forms, refusing a
// key supplied twice.
//
// A duplicate is an error rather than a last-one-wins because the two forms
// exist for opposite reasons: someone who typed `password=hunter2` *and*
// `--from-stdin password` meant one of them, and quietly picking either writes
// a credential they did not choose.
func collectValues(cmd *cobra.Command, opts *secretSetOptions, pairs []string) (map[string]string, error) {
	values := make(map[string]string, len(pairs)+len(opts.fromFile)+1)
	add := func(key, value, source string) error {
		if _, dup := values[key]; dup {
			return fmt.Errorf("key %q was supplied twice (the second time by %s): a key has one value, and guessing which you meant would write a credential you did not choose", key, source)
		}
		values[key] = value
		return nil
	}

	for _, pair := range pairs {
		key, value, found := strings.Cut(pair, "=")
		if !found || key == "" {
			return nil, fmt.Errorf("%q is not a key=value pair; write key=value, or use --from-stdin <key> / --from-file <key>=<path> to keep the value out of your shell history", pair)
		}
		if err := add(key, value, "a key=value argument"); err != nil {
			return nil, err
		}
	}

	for key, path := range opts.fromFile {
		body, err := os.ReadFile(path) //nolint:gosec // reading the file the operator named is the point of the flag
		if err != nil {
			return nil, fmt.Errorf("reading the value for key %q: %w", key, err)
		}
		if err := add(key, string(body), "--from-file"); err != nil {
			return nil, err
		}
	}

	if opts.fromStdin != "" {
		body, err := io.ReadAll(cmd.InOrStdin())
		if err != nil {
			return nil, fmt.Errorf("reading the value for key %q from stdin: %w", opts.fromStdin, err)
		}
		// One trailing newline goes. `printf '%s' "$PW" |` is the careful
		// spelling and `echo "$PW" |` is the one people actually type, and a
		// credential that legitimately ends in a newline is rare enough that
		// --from-file — which reads bytes verbatim — is the right place for it.
		if err := add(opts.fromStdin, strings.TrimSuffix(string(body), "\n"), "--from-stdin"); err != nil {
			return nil, err
		}
	}
	return values, nil
}

// preservedKeys is what the Secret already held and this command did not touch.
// Reporting it is what makes the merge visible: a user who expected `set` to
// replace the Secret finds out here rather than by wondering why an old key is
// still projected.
func preservedKeys(all []string, written map[string]string) []string {
	var out []string
	for _, key := range all {
		if _, set := written[key]; !set {
			out = append(out, key)
		}
	}
	return out
}

// --- list ---------------------------------------------------------------------

func newSecretListCmd(connect secretConnector) *cobra.Command {
	opts := &secretTarget{connect: connect}
	cmd := &cobra.Command{
		Use:   "list --env <environment> [-f spec.yaml]",
		Short: "List the Secrets kelson manages for the environment",
		Long: "List reports name, keys and age for every Secret kelson wrote for this environment.\n\n" +
			"It never reports a value, and there is no flag that would. kelson does not store secret values\n" +
			"(ADR-0009): the cluster (or, under backend sops, the delivery repository) is the store and this is\n" +
			"the masked read-back.\n\n" +
			"Under backend cluster only Secrets kelson manages are listed: a namespace's TLS material,\n" +
			"service-account tokens and image-pull credentials are not kelson's to enumerate. Under backend sops\n" +
			"the listing is the encrypted files kelson wrote, read without any key — SOPS encrypts values, not\n" +
			"key names — and there is no age column, because a file's age is a fact about the repository rather\n" +
			"than about the credential.",
		Example: "  kelson secret list --project checkout --env production\n" +
			"  kelson secret list -f spec.yaml --env production",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			store, err := opts.store()
			if err != nil {
				return err
			}
			secrets, err := store.List(cmd.Context(), opts.target())
			if err != nil {
				return err
			}
			return printSecretList(cmd, opts, secrets)
		},
	}
	opts.bind(cmd)
	return cmd
}

func printSecretList(cmd *cobra.Command, opts *secretTarget, secrets []secret.Secret) error {
	out := &printer{w: cmd.OutOrStdout()}
	namespace, err := opts.target().Resolve()
	if err != nil {
		return err
	}
	where := "namespace " + namespace
	if len(secrets) == 0 {
		out.printf("kelson manages no Secrets in %s.\n", where)
		out.printf("Write one with `kelson secret set <name> --project %s --env %s <key>=<value>`.\n",
			opts.project, opts.environment)
		return out.err
	}

	out.printf("%d Secret(s) kelson manages in %s\n\n", len(secrets), where)
	out.printf("%s%s%s\n", pad(secretNameColumn, "NAME"), pad(secretAgeColumn, "AGE"), "KEYS")
	now := secretClock()
	for _, s := range secrets {
		out.printf("%s%s%s\n", pad(secretNameColumn, s.Name), pad(secretAgeColumn, humanAge(s.Age(now))), strings.Join(s.Keys, ", "))
	}
	return out.err
}

const (
	secretNameColumn = 24
	secretAgeColumn  = 10
)

// --- delete --------------------------------------------------------------------

type secretDeleteOptions struct {
	secretTarget
	yes    bool
	dryRun bool
}

func newSecretDeleteCmd(connect secretConnector) *cobra.Command {
	opts := &secretDeleteOptions{secretTarget: secretTarget{connect: connect}}
	cmd := &cobra.Command{
		Use:   "delete <name> --env <environment> [-f spec.yaml]",
		Short: "Delete a Secret kelson manages",
		Long: "Delete removes a Secret kelson wrote. A Secret kelson did not write is refused by name rather than\n" +
			"deleted, so a typo cannot take out a service account's token or an operator's generated credential.\n\n" +
			"Under backend sops it removes the encrypted file from the delivery repository, and the Kustomization\n" +
			"that reconciles the path prunes the Secret from the cluster on its next reconcile. The value stays in\n" +
			"the repository's history: a deleted encrypted Secret is still readable by anyone holding the age\n" +
			"identity and a copy of the history, so treat the credential as needing rotation rather than gone.\n\n" +
			"kelson does not know which components reference the Secret: a reference is a name in a spec and\n" +
			"nothing correlates the two yet (ADR-0018). Deleting a Secret a running workload reads leaves that\n" +
			"workload running on the value it already has and breaks its next pod start.",
		Example: "  kelson secret delete checkout-db --project checkout --env production\n" +
			"  kelson secret delete checkout-db -f spec.yaml --env production --yes",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error { return runSecretDelete(cmd, opts, args[0]) },
	}
	opts.bind(cmd)
	f := cmd.Flags()
	f.BoolVar(&opts.yes, "yes", false, "delete without asking for confirmation")
	f.BoolVar(&opts.dryRun, "dry-run", false,
		"validate the delete and discard it; nothing is removed and nothing is committed")
	return cmd
}

func runSecretDelete(cmd *cobra.Command, opts *secretDeleteOptions, name string) error {
	store, err := opts.store()
	if err != nil {
		return err
	}
	namespace, err := opts.target().Resolve()
	if err != nil {
		return err
	}

	if !opts.yes && !opts.dryRun {
		ok, err := confirm(cmd, fmt.Sprintf("Delete Secret %s in namespace %s?", name, namespace))
		if err != nil {
			return err
		}
		if !ok {
			out := &printer{w: cmd.OutOrStdout()}
			out.printf("aborted: nothing was deleted. Pass --yes to delete without asking.\n")
			return out.err
		}
	}

	if err := store.Delete(cmd.Context(), secret.DeleteRequest{
		Target: opts.target(), Name: name, DryRun: opts.dryRun,
	}); err != nil {
		return err
	}
	out := &printer{w: cmd.OutOrStdout()}
	if opts.dryRun {
		out.printf("dry run: the API server accepted the delete of Secret %s in namespace %s and removed nothing.\n", name, namespace)
		return out.err
	}
	out.printf("deleted Secret %s in namespace %s\n", name, namespace)
	out.printf("any component referencing it keeps its current value until its next pod start, which will fail.\n")
	return out.err
}

// --- rotate --------------------------------------------------------------------

// `kelson secret rotate` reports which encrypted Secrets are not encrypted to
// the environment's current age recipients, and what to run for each.
//
// It reports rather than re-encrypts, and the reason is the same one that
// makes this backend safe: re-encrypting an existing file to a new recipient
// set needs the data key, the data key needs an age identity, and kelson never
// holds one (ADR-0022). What it can do without a key is exactly the part
// people get wrong — knowing *which* files are stale after a key change, and
// remembering that a file still wrapped for a retired key is still readable by
// whoever kept it.
//
// The exit code follows the `kelson diff` contract: 0 for no drift, 2 for
// drift present. Both are expected outcomes, so neither prints an error.
func newSecretRotateCmd(connect secretConnector) *cobra.Command {
	opts := &secretTarget{connect: connect}
	cmd := &cobra.Command{
		Use:   "rotate -f spec.yaml --env <environment>",
		Short: "Report which encrypted Secrets are not encrypted to the current age recipients",
		Long: "Rotate compares every encrypted Secret in the delivery repository against the environment's\n" +
			"secrets.ageRecipients and reports the ones that differ, with the command that fixes each.\n\n" +
			"It changes nothing. Re-encrypting a file to a new recipient set needs the data key inside it, which\n" +
			"needs an age identity — and kelson holds none, which is what lets the recipients live in the spec in\n" +
			"the clear. Two things do the re-encryption:\n\n" +
			"  kelson secret set <name> ... <every key>=<value>\n" +
			"      re-encrypts from the values. Needs no key at all; needs the values.\n" +
			"  sops updatekeys <path>\n" +
			"      re-wraps the existing data key for the new recipients. Needs an identity that can already\n" +
			"      read the file; needs no values.\n\n" +
			"Adding a recipient before removing one is what makes a handover gapless: a file is wrapped once per\n" +
			"recipient and any matching identity opens it. Removing a recipient does NOT make the old key useless\n" +
			"— it is still in the repository's history — so a compromised key means rotating the credentials too.\n\n" +
			"Exits 0 when nothing is stale and 2 when something is, so it can gate CI.",
		Example: "  kelson secret rotate -f spec.yaml --env production",
		Args:    cobra.NoArgs,
		RunE:    func(cmd *cobra.Command, _ []string) error { return runSecretRotate(cmd, opts) },
	}
	opts.bind(cmd)
	return cmd
}

func runSecretRotate(cmd *cobra.Command, opts *secretTarget) error {
	// The drift report reads the encrypted files kelson wrote, and kelson has
	// nowhere to have written them: the git writer that committed them is
	// deleted (ADR-0028). store() is still called first, so an environment on
	// backend cluster gets the "there is nothing to rotate keys for" answer it
	// has always got, rather than a gate about a backend it does not use.
	if _, err := opts.store(); err != nil {
		return err
	}
	return fmt.Errorf("`kelson secret rotate` is for secret backend sops, and environment %q does not use it. "+
		"Pass -f with the spec so kelson can read secrets.backend; under backend cluster a Secret is not "+
		"encrypted to anybody and there is nothing to rotate keys for", opts.environment)
}

// --- formatting ----------------------------------------------------------------

func sortedNames(values map[string]string) []string {
	out := make([]string, 0, len(values))
	for key := range values {
		out = append(out, key)
	}
	sort.Strings(out)
	return out
}

func pad(width int, s string) string {
	if len(s) >= width {
		return s + " "
	}
	return s + strings.Repeat(" ", width-len(s))
}

// secretClock is what the listing's age column is measured against. It is a
// variable so a test can pin it and assert a rendered age, rather than assert
// only that some age was printed.
var secretClock = time.Now

// humanAge renders a duration the way `kubectl get` does: one unit, largest
// that fits, because an age column is scanned and not read.
func humanAge(d time.Duration) string {
	switch {
	case d <= 0:
		return "-"
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	}
}
