package main

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/dafrie/kelson/internal/controlstore"
	"github.com/dafrie/kelson/internal/delivery/kube"
)

// `kelson agent` issues, lists and revokes agent identities (issue #74,
// ADR-0024).
//
// # Why this talks to the cluster and not to kelson-server
//
// Every other way into an agent identity is a credential: the server's shared
// password, or another agent's token. This one is a kube context, which is the
// same authorization `kelson deploy` and `kelson secret` already rely on — the
// operator's own RBAC on the state namespace. That makes issuance the one
// operation whose authority does not come from kelson at all, which is exactly
// what you want of the thing that mints credentials: an attacker who has the
// server's password cannot use this command, and the cluster's audit log
// records who ran it.
//
// AgentService is the same three operations for a human at the API, and it is
// refused to agent credentials by the scope table (internal/api/scope.go).
// Neither surface lets an agent mint an agent.
//
// # Rotation is create-then-revoke, and it is not automated here
//
// There is no `kelson agent rotate`. A rotation that revoked the old credential
// before the new one was in place would break the agent, and one that did it
// afterwards would need to know when "afterwards" is — which is the operator's
// knowledge, not kelson's. So `create` the successor, put its token where the
// agent reads it, and then `revoke` the predecessor.
//
// # A token is printed once, to stdout, and never again
//
// The server stores a salted HMAC; nothing can recover the value. `create`
// prints it on its own line so `kelson agent create … | tail -1` is a usable
// pipeline, and says out loud that this is the only time it exists.

// agentStore is the capability these commands need. It is declared here rather
// than used as a concrete type for the same reason secretStore is: the
// production implementation needs a live cluster and the command wiring under
// test does not.
type agentStore interface {
	Create(ctx context.Context, spec controlstore.AgentSpec) (controlstore.Agent, string, error)
	List(ctx context.Context) ([]controlstore.Agent, error)
	Revoke(ctx context.Context, name string) (controlstore.Agent, error)
}

// agentConnector builds the store for one command run. It is the seam the tests
// replace.
type agentConnector func(kubeconfig, namespace string) (agentStore, error)

// defaultAgentNamespace is where kelson-server keeps its state, and therefore
// where identities live. It matches kelson-server's --namespace default.
const defaultAgentNamespace = "kelson-system"

// connectAgents is the production connector: one cluster connection, the typed
// clientset behind it — the same reach `kelson secret` makes.
func connectAgents(kubeconfig, namespace string) (agentStore, error) {
	cluster, err := kube.Connect(kubeconfig)
	if err != nil {
		return nil, err
	}
	return controlstore.NewAgentStore(controlstore.AgentStoreOptions{
		Client:    cluster.Typed,
		Namespace: namespace,
	})
}

func newAgentCmd() *cobra.Command {
	return newAgentCmdWith(connectAgents)
}

func newAgentCmdWith(connect agentConnector) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "agent",
		Short: "Issue, list and revoke agent identities for kelson-server",
		Long: "An agent is a principal, not a human with a borrowed token. These commands mint credentials that\n" +
			"are named, scoped, time-limited and individually revocable, so an agent's actions can be attributed,\n" +
			"its permissions can be narrower than yours, and revoking it locks nobody out.\n\n" +
			"They talk to the cluster, not to kelson-server: the authority to issue a credential is your kube\n" +
			"context and the RBAC on the state namespace, never the server's shared password. An agent cannot run\n" +
			"them, and neither can anyone who only has the password.\n\n" +
			"Rotation is create-then-revoke: create the successor, configure the agent with its token, then revoke\n" +
			"the predecessor.",
		Args: cobra.NoArgs,
	}
	cmd.AddCommand(newAgentCreateCmd(connect), newAgentListCmd(connect), newAgentRevokeCmd(connect))
	return cmd
}

// agentOptions is the addressing every subcommand shares.
type agentOptions struct {
	kubeconfig string
	namespace  string
	connect    agentConnector
}

func (o *agentOptions) bind(cmd *cobra.Command) {
	f := cmd.Flags()
	f.StringVar(&o.kubeconfig, "kubeconfig", "",
		"path to a kubeconfig (default: $KUBECONFIG, in-cluster credentials, then ~/.kube/config)")
	f.StringVar(&o.namespace, "namespace", defaultAgentNamespace,
		"namespace holding kelson-server's state, where identities live")
}

func (o *agentOptions) store() (agentStore, error) {
	return o.connect(o.kubeconfig, o.namespace)
}

// --- create ------------------------------------------------------------------

type agentCreateOptions struct {
	agentOptions
	ttl          time.Duration
	projects     []string
	environments []string
	allow        []string
	rate         int
	burst        int
	quiet        bool
}

func newAgentCreateCmd(connect agentConnector) *cobra.Command {
	opts := &agentCreateOptions{agentOptions: agentOptions{connect: connect}}
	cmd := &cobra.Command{
		Use:   "create <name>",
		Short: "Mint an agent identity and print its token once",
		Long: "Create mints a named identity and prints its bearer token. The server stores only a salted HMAC of\n" +
			"that token, so this is the only time it exists: a lost token is replaced, never recovered.\n\n" +
			"Scope defaults to the narrow end. Without --allow the identity may only read; without --project or\n" +
			"--env it is unrestricted in that dimension, which is deliberately something you have to choose by\n" +
			"omission rather than something a flag hands you quietly.\n\n" +
			"An identity scoped to particular projects or environments is refused the RPCs whose target cannot be\n" +
			"read from the request — an inline spec, or ListSpecs, which spans every project. That is enforced by\n" +
			"the server, not by this command.",
		Example: "  kelson agent create deploybot --project shop --env development --allow mutate\n" +
			"  kelson agent create reporter --allow read --ttl 168h\n" +
			"  export KELSON_AGENT_TOKEN=$(kelson agent create deploybot --env development --allow mutate --quiet)",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error { return runAgentCreate(cmd, opts, args[0]) },
	}
	opts.bind(cmd)
	f := cmd.Flags()
	f.DurationVar(&opts.ttl, "ttl", controlstore.DefaultAgentTTL,
		fmt.Sprintf("how long the credential is valid (maximum %s); rotate rather than asking for a longer one", controlstore.MaxAgentTTL))
	f.StringArrayVar(&opts.projects, "project", nil,
		"a project the identity may act on (repeatable; omit for every project)")
	f.StringArrayVar(&opts.environments, "env", nil,
		"an environment the identity may act on (repeatable; omit for every environment)")
	f.StringArrayVar(&opts.allow, "allow", []string{string(controlstore.OpRead)},
		"an operation class to grant: read or mutate (repeatable; mutate implies read)")
	f.IntVar(&opts.rate, "rate", 0,
		fmt.Sprintf("requests per minute this identity may make (default %d)", controlstore.DefaultRequestsPerMinute))
	f.IntVar(&opts.burst, "burst", 0,
		fmt.Sprintf("how many requests it may make back to back (default %d)", controlstore.DefaultBurst))
	f.BoolVar(&opts.quiet, "quiet", false, "print the token alone, for capture into an environment variable")
	return cmd
}

func runAgentCreate(cmd *cobra.Command, opts *agentCreateOptions, name string) error {
	operations, err := parseOperations(opts.allow)
	if err != nil {
		return err
	}
	store, err := opts.store()
	if err != nil {
		return err
	}
	agent, token, err := store.Create(cmd.Context(), controlstore.AgentSpec{
		Name: name,
		TTL:  opts.ttl,
		Scope: controlstore.Scope{
			Projects:     opts.projects,
			Environments: opts.environments,
			Operations:   operations,
		},
		Limit: controlstore.Limit{RequestsPerMinute: opts.rate, Burst: opts.burst},
	})
	if err != nil {
		return err
	}

	out := &printer{w: cmd.OutOrStdout()}
	if opts.quiet {
		out.printf("%s\n", token)
		return out.err
	}
	out.printf("created agent identity %s\n", agent.Name)
	out.printf("  expires:      %s\n", agent.Expires.Format(time.RFC3339))
	out.printf("  projects:     %s\n", orAny(agent.Scope.Projects))
	out.printf("  environments: %s\n", orAny(agent.Scope.Environments))
	out.printf("  operations:   %s\n", strings.Join(opts.allow, ", "))
	out.printf("  budget:       %d requests/minute, burst %d\n", agent.Limit.RequestsPerMinute, agent.Limit.Burst)
	out.printf("\ntoken (shown once — the server keeps only a hash of it):\n")
	out.printf("%s\n", token)
	out.printf("\ngive it to the agent as a bearer credential, for example:\n")
	out.printf("  KELSON_AGENT_TOKEN=<token> kelson-mcp --server http://127.0.0.1:8420\n")
	return out.err
}

// --- list --------------------------------------------------------------------

func newAgentListCmd(connect agentConnector) *cobra.Command {
	opts := &agentOptions{connect: connect}
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List agent identities, including revoked and expired ones",
		Long: "List reports every identity the server holds and never a credential — there is nothing stored that\n" +
			"a token could be recovered from.\n\n" +
			"Revoked and expired identities stay listed on purpose: an audit trail that cannot name the principal\n" +
			"behind a past action is not one.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error { return runAgentList(cmd, opts) },
	}
	opts.bind(cmd)
	return cmd
}

func runAgentList(cmd *cobra.Command, opts *agentOptions) error {
	store, err := opts.store()
	if err != nil {
		return err
	}
	agents, err := store.List(cmd.Context())
	if err != nil {
		return err
	}
	out := &printer{w: cmd.OutOrStdout()}
	if len(agents) == 0 {
		out.printf("no agent identities in namespace %s\n", opts.namespace)
		return out.err
	}
	now := time.Now()
	for _, agent := range agents {
		out.printf("%s  %s\n", agent.Name, agentState(agent, now))
		out.printf("  expires:      %s\n", agent.Expires.Format(time.RFC3339))
		out.printf("  projects:     %s\n", orAny(agent.Scope.Projects))
		out.printf("  environments: %s\n", orAny(agent.Scope.Environments))
		out.printf("  operations:   %s\n", operationList(agent.Scope.Operations))
		out.printf("  budget:       %d requests/minute, burst %d\n", agent.Limit.RequestsPerMinute, agent.Limit.Burst)
	}
	return out.err
}

// --- revoke ------------------------------------------------------------------

func newAgentRevokeCmd(connect agentConnector) *cobra.Command {
	opts := &agentOptions{connect: connect}
	cmd := &cobra.Command{
		Use:   "revoke <name>",
		Short: "Kill an agent credential, immediately and for that identity alone",
		Long: "Revoke marks one identity dead. The server reads identities from the cluster on every request and\n" +
			"caches no allow decision, so the next request that credential makes is refused — there is no window\n" +
			"and no cache to wait out.\n\n" +
			"It touches one object. No human session, no other identity and no password is affected.\n\n" +
			"Revoking twice is not an error, and the identity is kept rather than deleted so past actions stay\n" +
			"attributable.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error { return runAgentRevoke(cmd, opts, args[0]) },
	}
	opts.bind(cmd)
	return cmd
}

func runAgentRevoke(cmd *cobra.Command, opts *agentOptions, name string) error {
	store, err := opts.store()
	if err != nil {
		return err
	}
	agent, err := store.Revoke(cmd.Context(), name)
	if err != nil {
		return err
	}
	out := &printer{w: cmd.OutOrStdout()}
	out.printf("revoked agent identity %s at %s\n", agent.Name, agent.RevokedAt.Format(time.RFC3339))
	out.printf("its credential is refused from the next request on; nothing else was touched.\n")
	return out.err
}

// --- shared ------------------------------------------------------------------

// parseOperations turns --allow values into classes. An unknown one is refused
// rather than ignored: a typo that silently granted nothing would produce an
// identity that fails on its first call for a reason nothing explained.
func parseOperations(allow []string) ([]controlstore.Operation, error) {
	if len(allow) == 0 {
		return nil, fmt.Errorf("--allow needs at least one operation class: read, or read and mutate")
	}
	out := make([]controlstore.Operation, 0, len(allow))
	for _, value := range allow {
		switch controlstore.Operation(strings.TrimSpace(strings.ToLower(value))) {
		case controlstore.OpRead:
			out = append(out, controlstore.OpRead)
		case controlstore.OpMutate:
			out = append(out, controlstore.OpMutate)
		default:
			return nil, fmt.Errorf("unknown operation class %q: --allow takes read or mutate. "+
				"Finer-grained policy is issue #75; there is nothing here that could enforce it yet", value)
		}
	}
	return out, nil
}

func agentState(agent controlstore.Agent, now time.Time) string {
	switch {
	case agent.Revoked:
		return "(revoked " + agent.RevokedAt.Format(time.RFC3339) + ")"
	case agent.Expired(now):
		return "(expired)"
	default:
		return "(live)"
	}
}

func operationList(ops []controlstore.Operation) string {
	out := make([]string, 0, len(ops))
	for _, op := range ops {
		out = append(out, string(op))
	}
	return orAny(out)
}

func orAny(values []string) string {
	if len(values) == 0 {
		return "(any)"
	}
	return strings.Join(values, ", ")
}
