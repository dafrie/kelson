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

	"github.com/dafrie/kelson/internal/delivery/kube"
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

// secretStore is the capability `kelson secret` needs from a cluster. It is
// declared here rather than imported as a concrete type for the same reason
// deliveryConnector is: the production implementation needs a live clientset,
// and none of the command wiring under test does.
type secretStore interface {
	Set(ctx context.Context, req secret.SetRequest) (secret.Secret, error)
	List(ctx context.Context, t secret.Target) ([]secret.Secret, error)
	Delete(ctx context.Context, req secret.DeleteRequest) error
}

// secretConnector builds the store for one command run. It is the seam the
// tests replace.
type secretConnector func(kubeconfig string) (secretStore, error)

// connectSecrets is the production connector: one cluster connection, the
// typed clientset behind it. It is the same reach `kelson status` and
// `kelson deploy` make — kube.Connect resolves the kubeconfig and this plane's
// lint allow-list keeps the client libraries out of cmd/ (.golangci.yml).
func connectSecrets(kubeconfig string) (secretStore, error) {
	cluster, err := kube.Connect(kubeconfig)
	if err != nil {
		return nil, err
	}
	return secret.New(cluster.Typed), nil
}

func newSecretCmd() *cobra.Command { return newSecretCmdFactory(connectSecrets) }

func newSecretCmdFactory(connect secretConnector) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "secret",
		Short: "Write, list and delete the Kubernetes Secrets an environment's references point at",
		Long: "A kelson spec carries secret references, never values (ADR-0009). `{secret: <name>, key: <key>}`\n" +
			"renders as a valueFrom.secretKeyRef against a Secret in the environment's namespace; these commands\n" +
			"are what writes that Secret.\n\n" +
			"kelson does not store the value. It goes to the cluster's API server and kelson reads it back masked:\n" +
			"listing reports names, keys and ages, never a value. That also means secrets are NOT in the Git\n" +
			"artifact — rebuilding a cluster from Git alone will not restore them.\n\n" +
			"kelson labels every Secret it writes and touches nothing else: listing is a label query, and deleting\n" +
			"or overwriting a Secret kelson did not write is refused rather than done.",
		Args: cobra.NoArgs,
	}
	cmd.AddCommand(newSecretSetCmd(connect), newSecretListCmd(connect), newSecretDeleteCmd(connect))
	return cmd
}

// secretTarget is the (project, environment) addressing every subcommand takes.
type secretTarget struct {
	project     string
	environment string
	namespace   string
	kubeconfig  string
	connect     secretConnector
}

func (t *secretTarget) bind(cmd *cobra.Command) {
	f := cmd.Flags()
	f.StringVar(&t.project, "project", "", "name of the Project the secret belongs to")
	f.StringVar(&t.environment, "env", "", "name of the Environment the secret belongs to")
	f.StringVar(&t.namespace, "namespace", "",
		"target namespace, overriding the derived <project>-<environment> (only needed when the Environment sets spec.namespace)")
	f.StringVar(&t.kubeconfig, "kubeconfig", "",
		"path to a kubeconfig (default: $KUBECONFIG, in-cluster credentials, then ~/.kube/config)")
	cobra.CheckErr(cmd.MarkFlagRequired("project"))
	cobra.CheckErr(cmd.MarkFlagRequired("env"))
}

func (t *secretTarget) target() secret.Target {
	return secret.Target{Project: t.project, Environment: t.environment, Namespace: t.namespace}
}

func (t *secretTarget) store() (secretStore, error) { return t.connect(t.kubeconfig) }

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
		Use:   "set <name> --project <project> --env <environment> [key=value ...]",
		Short: "Write keys into a Secret in the environment's namespace",
		Long: "Set writes the given keys into the named Secret, creating it if it does not exist, and prints the\n" +
			"resulting key list. Values are never printed back.\n\n" +
			"Keys not named in this command are preserved: `set` adds and replaces keys, it does not replace the\n" +
			"Secret. Rotating one credential is one command and leaves the others alone.\n\n" +
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
		"ask the API server to validate the write and discard it (server-side dry run); nothing is stored")
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
		Use:   "list --project <project> --env <environment>",
		Short: "List the Secrets kelson manages in the environment's namespace",
		Long: "List reports name, keys and age for every Secret kelson wrote in this environment's namespace.\n\n" +
			"It never reports a value, and there is no flag that would. kelson does not store secret values\n" +
			"(ADR-0009): the cluster is the store and this is the masked read-back.\n\n" +
			"Only Secrets kelson manages are listed. A namespace's TLS material, service-account tokens and\n" +
			"image-pull credentials are not kelson's to enumerate, so they are absent rather than filtered.",
		Example: "  kelson secret list --project checkout --env production",
		Args:    cobra.NoArgs,
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
	if len(secrets) == 0 {
		out.printf("kelson manages no Secrets in namespace %s.\n", namespace)
		out.printf("Write one with `kelson secret set <name> --project %s --env %s <key>=<value>`.\n",
			opts.project, opts.environment)
		return out.err
	}

	out.printf("%d Secret(s) kelson manages in namespace %s\n\n", len(secrets), namespace)
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
		Use:   "delete <name> --project <project> --env <environment>",
		Short: "Delete a Secret kelson manages",
		Long: "Delete removes a Secret kelson wrote. A Secret kelson did not write is refused by name rather than\n" +
			"deleted, so a typo cannot take out a service account's token or an operator's generated credential.\n\n" +
			"kelson does not know which components reference the Secret: a reference is a name in a spec and\n" +
			"nothing correlates the two yet (ADR-0018). Deleting a Secret a running workload reads leaves that\n" +
			"workload running on the value it already has and breaks its next pod start.",
		Example: "  kelson secret delete checkout-db --project checkout --env production\n" +
			"  kelson secret delete checkout-db --project checkout --env production --yes",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error { return runSecretDelete(cmd, opts, args[0]) },
	}
	opts.bind(cmd)
	f := cmd.Flags()
	f.BoolVar(&opts.yes, "yes", false, "delete without asking for confirmation")
	f.BoolVar(&opts.dryRun, "dry-run", false,
		"ask the API server to validate the delete and discard it (server-side dry run); nothing is removed")
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
