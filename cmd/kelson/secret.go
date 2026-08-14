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

	"github.com/dafrie/kelson/internal/delivery/git"
	"github.com/dafrie/kelson/internal/delivery/kube"
	"github.com/dafrie/kelson/internal/model"
	"github.com/dafrie/kelson/internal/renderer"
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
// holds the value. Both implementations satisfy it — internal/secret's cluster
// Store and its SOPSStore — so this file selects a backend and never a code
// path (ADR-0022). It is declared here rather than imported as a concrete type
// for the same reason deliveryConnector is: the production implementations
// need a live cluster or a live repository, and none of the command wiring
// under test does.
type secretStore interface {
	Set(ctx context.Context, req secret.SetRequest) (secret.Secret, error)
	List(ctx context.Context, t secret.Target) ([]secret.Secret, error)
	Delete(ctx context.Context, req secret.DeleteRequest) error
}

// sopsStore is the sops backend's extra capability: what `kelson secret
// rotate` needs and what the cluster backend has no analogue for, because a
// cluster Secret is not encrypted to anybody.
type sopsStore interface {
	secretStore
	Drift(ctx context.Context, t secret.Target) ([]secret.SOPSDrift, error)
	Recipients() []string
	RepoPath(name string) string
}

// secretConnector builds the cluster store for one command run. It is the seam
// the tests replace.
type secretConnector func(kubeconfig string) (secretStore, error)

// sopsSecretConnector builds the sops store from a resolved Environment. It is
// a second seam rather than a branch inside the first because the two need
// entirely different things: a kubeconfig, or a delivery repository and a
// credential.
type sopsSecretConnector func(resolved *model.Resolved) (sopsStore, error)

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

// connectSOPS is the production sops connector. It builds the same git writer
// `kelson deploy` builds for this environment, from the same fields and the
// same credential, so a secret lands in the repository a deploy commits to
// rather than in one only this command believes in.
func connectSOPS(resolved *model.Resolved) (sopsStore, error) {
	target := resolved.Environment.Delivery.Git
	if target == nil {
		return nil, fmt.Errorf("environment %q selects secret backend sops but sets no delivery.git target", resolved.Environment.Name)
	}
	writer, err := git.New(git.Config{
		Target: git.Target{Repo: target.Repo, Branch: target.Branch, Path: target.Path},
		// Commit mode, not pull-request mode. A pull request holding a
		// credential is a credential sitting in an open branch for as long as
		// review takes, and the encrypted file is reviewable in the merge
		// commit either way. ADR-0022 records this rather than leaving it to
		// whichever mode happened to be the default.
		Mode:     git.ModeCommit,
		Identity: git.IdentityFromEnv(nil),
		Auth:     gitAuth(),
	})
	if err != nil {
		return nil, err
	}
	return secret.NewSOPS(secret.SOPSConfig{
		Writer:     writer,
		Recipients: resolved.Environment.Secrets.AgeRecipients,
	})
}

func newSecretCmd() *cobra.Command { return newSecretCmdFactory(connectSecrets, connectSOPS) }

func newSecretCmdFactory(connect secretConnector, connectSops sopsSecretConnector) *cobra.Command {
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
			"  sops               the value is encrypted with age and committed to the delivery repository, and\n" +
			"                     Flux decrypts it in-cluster. Plaintext never touches your disk: kelson encrypts\n" +
			"                     in memory and commits the ciphertext. Rebuilding from Git restores everything.\n" +
			"  externalSecrets    no value passes through kelson at all — write it in your secret manager.\n\n" +
			"kelson never holds an age private key. `kelson secret set` needs only the public age1… recipients from\n" +
			"secrets.ageRecipients; the identity that decrypts lives in a Secret you create for Flux.",
		Args: cobra.NoArgs,
	}
	cmd.AddCommand(
		newSecretSetCmd(connect, connectSops),
		newSecretListCmd(connect, connectSops),
		newSecretDeleteCmd(connect, connectSops),
		newSecretRotateCmd(connect, connectSops),
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
	connectSops sopsSecretConnector
	// ageKeySecret is the resolved secrets.ageKeySecret, kept so the sops
	// reminder names the Secret this environment's Kustomization must
	// reference rather than the default.
	ageKeySecret string
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
	t.ageKeySecret = resolved.Environment.Secrets.AgeKeySecret
	if t.namespace == "" {
		t.namespace = resolved.Environment.Namespace
	}
	return resolved, nil
}

// store resolves the spec and returns the backend's store. The sops store is
// returned separately as well, so a caller that needs its extra capabilities —
// `rotate`, and the reminder `set` prints — does not have to type-assert.
func (t *secretTarget) store() (secretStore, sopsStore, error) {
	resolved, err := t.resolve()
	if err != nil {
		return nil, nil, err
	}
	if resolved == nil {
		store, err := t.connect(t.kubeconfig)
		return store, nil, err
	}
	switch resolved.Environment.Secrets.Backend {
	case model.SecretsSOPS:
		if t.connectSops == nil {
			return nil, nil, fmt.Errorf("the sops backend is unavailable in this build")
		}
		store, err := t.connectSops(resolved)
		if err != nil {
			return nil, nil, err
		}
		return store, store, nil
	case model.SecretsExternalSecrets:
		// Writing a cluster Secret here would put kelson in a fight with the
		// external-secrets controller over an object it owns
		// (creationPolicy: Owner, ADR-0020) — kelson's keys would survive
		// until the next sync and then vanish. Under that backend no value
		// passes through kelson at all, and saying so is the whole point.
		return nil, nil, fmt.Errorf("environment %q uses secret backend externalSecrets, where the value is "+
			"written in your secret manager and external-secrets syncs it into the cluster. kelson never holds "+
			"it (ADR-0020): write it at the store this environment reads from (%s), or change "+
			"secrets.backend if you meant kelson to hold it",
			resolved.Environment.Name, storeOrAny(resolved.Environment.Secrets.Store))
	default:
		store, err := t.connect(t.kubeconfig)
		return store, nil, err
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

func newSecretSetCmd(connect secretConnector, connectSops sopsSecretConnector) *cobra.Command {
	opts := &secretSetOptions{secretTarget: secretTarget{connect: connect, connectSops: connectSops}}
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
	store, sops, err := opts.store()
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
	if sops != nil {
		out.printf("  encrypted to: %s\n", strings.Join(sops.Recipients(), ", "))
		out.printf("  committed at: %s\n", sops.RepoPath(written.Name))
	}
	if opts.dryRun {
		if sops != nil {
			out.printf("\ndry run: kelson encrypted this and committed nothing.\n")
		} else {
			out.printf("\ndry run: the API server validated this and stored nothing.\n")
		}
		return out.err
	}
	out.printf("\nreference a key from a component's env:\n")
	out.printf("  env:\n    MY_VARIABLE: { secret: %s, key: %s }\n", written.Name, sortedNames(values)[0])
	if sops != nil {
		printSOPSDecryptionReminder(out, opts.ageKeySecret)
	}
	return out.err
}

// printSOPSDecryptionReminder states the operator's half of the setup, which
// kelson cannot do for them.
//
// kelson writes the encrypted file; the Kustomization that reconciles the path
// belongs to the cluster's bootstrap and is not kelson's to write (ADR-0012,
// internal/delivery/eject/bootstrap.go). Without the decryption block the file
// is applied verbatim — a Secret whose values are the literal string
// "ENC[AES256_GCM,…]" — and every workload reading it starts with a credential
// that is not one. That failure is far from its cause, so the cause is printed
// at the moment the first encrypted file is written.
//
// The block comes from renderer.SOPSDecryptionBlock, the same function that
// writes it onto the preview Kustomization, so this text cannot drift from
// what kelson itself emits.
func printSOPSDecryptionReminder(out *printer, ageKeySecret string) {
	out.printf("\nthe Flux Kustomization reconciling this path must decrypt it:\n")
	for _, line := range strings.Split(strings.TrimRight(renderer.SOPSDecryptionBlock(ageKeySecret, "  "), "\n"), "\n") {
		out.printf("%s\n", line)
	}
	out.printf("and the Secret it names holds the age identity, which kelson never has:\n")
	out.printf("  kubectl -n flux-system create secret generic %s --from-file=age.agekey=age.key\n",
		ageKeySecretName(ageKeySecret))
}

func ageKeySecretName(name string) string {
	if name == "" {
		return model.DefaultAgeKeySecret
	}
	return name
}

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

func newSecretListCmd(connect secretConnector, connectSops sopsSecretConnector) *cobra.Command {
	opts := &secretTarget{connect: connect, connectSops: connectSops}
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
			store, sops, err := opts.store()
			if err != nil {
				return err
			}
			secrets, err := store.List(cmd.Context(), opts.target())
			if err != nil {
				return err
			}
			return printSecretList(cmd, opts, secrets, sops != nil)
		},
	}
	opts.bind(cmd)
	return cmd
}

func printSecretList(cmd *cobra.Command, opts *secretTarget, secrets []secret.Secret, encrypted bool) error {
	out := &printer{w: cmd.OutOrStdout()}
	namespace, err := opts.target().Resolve()
	if err != nil {
		return err
	}
	where := "namespace " + namespace
	if encrypted {
		where = "the delivery repository for namespace " + namespace
	}
	if len(secrets) == 0 {
		out.printf("kelson manages no Secrets in %s.\n", where)
		out.printf("Write one with `kelson secret set <name> --project %s --env %s <key>=<value>`.\n",
			opts.project, opts.environment)
		return out.err
	}

	out.printf("%d Secret(s) kelson manages in %s\n\n", len(secrets), where)
	if encrypted {
		out.printf("%s%s\n", pad(secretNameColumn, "NAME"), "KEYS")
		for _, s := range secrets {
			out.printf("%s%s\n", pad(secretNameColumn, s.Name), strings.Join(s.Keys, ", "))
		}
		return out.err
	}
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

func newSecretDeleteCmd(connect secretConnector, connectSops sopsSecretConnector) *cobra.Command {
	opts := &secretDeleteOptions{secretTarget: secretTarget{connect: connect, connectSops: connectSops}}
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
	store, sops, err := opts.store()
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
		if sops != nil {
			out.printf("dry run: kelson would remove %s and committed nothing.\n", sops.RepoPath(name))
			return out.err
		}
		out.printf("dry run: the API server accepted the delete of Secret %s in namespace %s and removed nothing.\n", name, namespace)
		return out.err
	}
	if sops != nil {
		out.printf("removed %s from the delivery repository\n", sops.RepoPath(name))
		out.printf("Flux prunes the Secret on its next reconcile. The encrypted value stays in the repository's\n")
		out.printf("history, so treat the credential as needing rotation rather than gone.\n")
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
func newSecretRotateCmd(connect secretConnector, connectSops sopsSecretConnector) *cobra.Command {
	opts := &secretTarget{connect: connect, connectSops: connectSops}
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
	_, sops, err := opts.store()
	if err != nil {
		return err
	}
	if sops == nil {
		return fmt.Errorf("`kelson secret rotate` is for secret backend sops, and environment %q does not use it. "+
			"Pass -f with the spec so kelson can read secrets.backend; under backend cluster a Secret is not "+
			"encrypted to anybody and there is nothing to rotate keys for", opts.environment)
	}
	drift, err := sops.Drift(cmd.Context(), opts.target())
	if err != nil {
		return err
	}

	out := &printer{w: cmd.OutOrStdout()}
	recipients := sops.Recipients()
	out.printf("environment %s encrypts to %d age recipient(s):\n", opts.environment, len(recipients))
	for _, r := range recipients {
		out.printf("  %s\n", r)
	}
	if len(drift) == 0 {
		out.printf("\nevery encrypted Secret is wrapped for exactly those recipients.\n")
		return out.err
	}

	out.printf("\n%d encrypted Secret(s) are wrapped for a different set:\n", len(drift))
	for _, d := range drift {
		out.printf("\n  %s (%s)\n", d.Name, d.Path)
		for _, r := range d.Missing {
			out.printf("    missing:  %s — whoever holds this identity cannot read this Secret\n", r)
		}
		for _, r := range d.Extra {
			out.printf("    extra:    %s — whoever holds this identity can still read this Secret\n", r)
		}
		out.printf("    fix:      kelson secret set %s -f <spec> --env %s %s\n",
			d.Name, opts.environment, keyPlaceholders(d.Keys))
		out.printf("    or:       sops updatekeys %s   (needs an identity that can already read it)\n", d.Path)
	}
	out.printf("\nnothing was changed.\n")
	return &exitError{code: exitDiff}
}

// keyPlaceholders spells the key list a re-encrypting `set` has to name, so
// the printed command is a template rather than a hint. Every key is there
// because under this backend a set writes the whole file.
func keyPlaceholders(keys []string) string {
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, k+"=<value>")
	}
	return strings.Join(parts, " ")
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
