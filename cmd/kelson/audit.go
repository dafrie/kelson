package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/dafrie/kelson/internal/controlstore"
	"github.com/dafrie/kelson/internal/delivery/kube"
)

// `kelson audit` reads the audit trail: what each principal actually did
// (issue #78, ADR-0026).
//
// # Why this talks to the cluster and not to kelson-server
//
// The same argument `kelson agent` makes, with one addition. Reading the trail
// is administrative, and the authority to do it is your kube context and the
// RBAC on the state namespace — not a credential kelson issued, which an
// attacker who compromised the server would also hold. And it works when
// kelson-server is down, which is one of the moments you most want to know what
// happened.
//
// AuditService.QueryAudit is the same query for a human at the API, refused to
// agent credentials by the scope table. Neither surface lets an agent read it.
//
// # The window is printed, always
//
// A trail has a horizon, and a reader who cannot see it will over-trust the
// answer. So every run ends with a line saying what the store retains and
// whether anything inside the queried range was dropped — not only when
// something was. That is the repo's no-silent-caps rule applied to a query
// result rather than to a list.
//
// # Export is the same query, paged
//
// `--export jsonl` follows the page token to exhaustion and writes one JSON
// object per line as each page arrives, so a month of records does not have to
// fit in memory to be exported. The window is written to stderr in that mode:
// stdout is the data, and a note in the middle of a JSONL stream would corrupt
// it.

// auditStore is the capability this command needs, declared as an interface for
// the same reason agentStore is: the production implementation needs a live
// cluster and the command wiring under test does not.
type auditStore interface {
	Query(ctx context.Context, q controlstore.AuditQuery) (controlstore.AuditPage, error)
}

// auditConnector builds the store for one command run. It is the seam the tests
// replace.
type auditConnector func(kubeconfig, namespace string) (auditStore, error)

// connectAudit is the production connector: one cluster connection, the typed
// clientset behind it — the same reach `kelson agent` makes.
func connectAudit(kubeconfig, namespace string) (auditStore, error) {
	cluster, err := kube.Connect(kubeconfig)
	if err != nil {
		return nil, err
	}
	return controlstore.NewAuditStore(controlstore.AuditOptions{
		Client:    cluster.Typed,
		Namespace: namespace,
	})
}

// exportJSONL is the one export format. It is a format for a pipeline —
// `| jq`, `| grep`, an append into a long-term store — rather than a report,
// which is what an export of an append log is for.
const exportJSONL = "jsonl"

type auditOptions struct {
	kubeconfig  string
	namespace   string
	project     string
	environment string
	agent       string
	principal   string
	procedure   string
	outcome     string
	since       time.Duration
	limit       int
	export      string
	connect     auditConnector
}

func newAuditCmd() *cobra.Command {
	return newAuditCmdWith(connectAudit)
}

func newAuditCmdWith(connect auditConnector) *cobra.Command {
	opts := &auditOptions{connect: connect}
	cmd := &cobra.Command{
		Use:   "audit",
		Short: "Read the audit trail: what each principal actually did",
		Long: "Audit answers \"what did it actually do?\" — every mutation kelson-server performed, who performed\n" +
			"it, under what scope, against which project and environment, and what came of it. Agent and human\n" +
			"actions are one trail told apart by the principal type, so --agent is a filter and not a second log.\n\n" +
			"It reads the cluster directly, not kelson-server: the authority is your kube context and the RBAC on\n" +
			"the state namespace, and it therefore still works when the server is down — which is one of the\n" +
			"moments you most want to know what happened.\n\n" +
			"Every run ends by stating the store's retention window and whether anything inside the range queried\n" +
			"was dropped. Read it: a trail has a horizon, and an answer that did not say where it is would be an\n" +
			"answer you would over-trust.\n\n" +
			"Refused requests are recorded too, with the code the caller was given. Allowed *reads* are not: they\n" +
			"change nothing, and their volume would evict the mutations from a bounded store (ADR-0026).",
		Example: "  kelson audit --since 24h\n" +
			"  kelson audit --project shop --env production\n" +
			"  kelson audit --agent deploybot --outcome refused\n" +
			"  kelson audit --since 720h --export jsonl > trail.jsonl",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error { return runAudit(cmd, opts) },
	}
	f := cmd.Flags()
	f.StringVar(&opts.kubeconfig, "kubeconfig", "",
		"path to a kubeconfig (default: $KUBECONFIG, in-cluster credentials, then ~/.kube/config)")
	f.StringVar(&opts.namespace, "namespace", defaultAgentNamespace,
		"namespace holding kelson-server's state, where the trail lives")
	f.StringVar(&opts.project, "project", "", "only records acting on this project")
	f.StringVar(&opts.environment, "env", "", "only records acting on this environment")
	f.StringVar(&opts.agent, "agent", "",
		"only records made by this agent identity (shorthand for --principal <name> restricted to agents)")
	f.StringVar(&opts.principal, "principal", "",
		"only records made by this principal, agent or human, by name")
	f.StringVar(&opts.procedure, "procedure", "",
		"only records whose RPC name contains this, e.g. Deploy or SetSecret")
	f.StringVar(&opts.outcome, "outcome", "",
		"only records with this outcome: allowed, refused or failed")
	f.DurationVar(&opts.since, "since", 0,
		"only records from the last duration, e.g. 24h; omit to reach as far back as the store retains")
	f.IntVar(&opts.limit, "limit", controlstore.DefaultAuditPageSize,
		fmt.Sprintf("how many records to print (maximum %d; --export pages past it)", controlstore.MaxAuditPageSize))
	f.StringVar(&opts.export, "export", "",
		"write every matching record instead of a page, as `jsonl` — one JSON object per line, for a pipeline")
	return cmd
}

func runAudit(cmd *cobra.Command, opts *auditOptions) error {
	query, err := opts.query(time.Now())
	if err != nil {
		return err
	}
	if opts.export != "" && opts.export != exportJSONL {
		return fmt.Errorf("unknown export format %q: --export takes %q. "+
			"It is one JSON object per line, which is what an append log exports as", opts.export, exportJSONL)
	}

	store, err := opts.connect(opts.kubeconfig, opts.namespace)
	if err != nil {
		return err
	}
	if opts.export == exportJSONL {
		return exportAudit(cmd, store, query)
	}
	return printAudit(cmd, store, query)
}

// query builds the store query from the flags. `now` is passed in so --since is
// deterministic under test.
func (o *auditOptions) query(now time.Time) (controlstore.AuditQuery, error) {
	q := controlstore.AuditQuery{
		Project:     o.project,
		Environment: o.environment,
		Principal:   o.principal,
		Procedure:   o.procedure,
		Limit:       o.limit,
	}
	if o.agent != "" {
		if o.principal != "" && o.principal != o.agent {
			return controlstore.AuditQuery{}, fmt.Errorf(
				"--agent %q and --principal %q name different principals; use one of them", o.agent, o.principal)
		}
		q.Principal = o.agent
		q.PrincipalType = string(auditPrincipalAgent)
	}
	if o.outcome != "" {
		outcome := controlstore.AuditOutcome(strings.ToLower(strings.TrimSpace(o.outcome)))
		if !controlstore.ValidAuditOutcome(outcome) {
			return controlstore.AuditQuery{}, fmt.Errorf(
				"unknown outcome %q: --outcome takes allowed, refused or failed. "+
					"A refused request never happened; a failed one was allowed and then broke", o.outcome)
		}
		q.Outcome = outcome
	}
	if o.since > 0 {
		q.Since = now.Add(-o.since)
	}
	return q, nil
}

// auditPrincipalAgent is the principal type an agent identity records under. It
// mirrors api.PrincipalAgent, which this plane's depguard rule keeps it from
// importing — the value is part of the stored record's vocabulary, not of the
// API's transport.
const auditPrincipalAgent = "agent"

// printAudit prints one page, newest first, then the window.
func printAudit(cmd *cobra.Command, store auditStore, q controlstore.AuditQuery) error {
	page, err := store.Query(cmd.Context(), q)
	if err != nil {
		return err
	}
	out := &printer{w: cmd.OutOrStdout()}
	if len(page.Records) == 0 {
		out.printf("no audit records match\n")
	}
	for _, rec := range page.Records {
		writeAuditRecord(out, rec)
	}
	if page.NextPageToken != "" {
		out.printf("\nmore records match than --limit %d printed. "+
			"Narrow the filters, raise --limit, or use --export jsonl to get all of them.\n", q.Limit)
	}
	writeAuditWindow(out, page.Window)
	return out.err
}

// writeAuditRecord prints one record as two lines: what happened, and the
// detail beneath it. The headline leads with the outcome because that is what a
// person scanning for trouble is looking for.
func writeAuditRecord(out *printer, rec controlstore.AuditRecord) {
	out.printf("%s  %-8s %-14s %s\n",
		rec.Time.Format(time.RFC3339),
		rec.Outcome,
		rec.Principal.String(),
		auditMethod(rec.Procedure))

	detail := make([]string, 0, 6)
	if target := rec.Target.String(); target != "" {
		detail = append(detail, "target="+target)
	}
	if rec.DryRun != "" && rec.DryRun != "none" {
		detail = append(detail, "dry-run="+rec.DryRun)
	}
	if rec.Code != "" {
		detail = append(detail, "code="+rec.Code)
	}
	if change := rec.Change; change != nil {
		if change.Revision != "" {
			detail = append(detail, "revision="+change.Revision)
		}
		if change.From != "" {
			detail = append(detail, "from="+change.From)
		}
		if summary := auditChangeSummary(change); summary != "" {
			detail = append(detail, summary)
		}
	}
	if len(detail) > 0 {
		out.printf("  %s\n", strings.Join(detail, "  "))
	}
	if rec.Scope != "" {
		out.printf("  scope: %s\n", rec.Scope)
	}
	if rec.Reason != "" {
		out.printf("  reason: %s\n", rec.Reason)
	}
	if rec.Message != "" {
		out.printf("  %s\n", rec.Message)
	}
}

// auditChangeSummary renders the bounded diff stats, saying which kind of count
// they are. "3 resources" and "1 added, 2 modified" are different claims and the
// record keeps them apart, so the printer must too.
func auditChangeSummary(change *controlstore.AuditChange) string {
	parts := make([]string, 0, 2)
	switch change.Source {
	case controlstore.ChangeFromDiff:
		parts = append(parts, fmt.Sprintf("%d added, %d modified, %d removed",
			change.Added, change.Modified, change.Removed))
		if change.MaxRisk != "" {
			parts = append(parts, "risk="+change.MaxRisk)
		}
	case controlstore.ChangeFromRendered:
		if change.Resources > 0 {
			parts = append(parts, fmt.Sprintf("%d resources applied", change.Resources))
		}
	}
	if len(change.Kinds) > 0 {
		parts = append(parts, "kinds="+strings.Join(change.Kinds, ","))
	}
	return strings.Join(parts, "  ")
}

// writeAuditWindow states the horizon, every time. A reader who cannot see
// where the store's memory ends is a reader who will over-trust the answer
// above it.
func writeAuditWindow(out *printer, w controlstore.AuditWindow) {
	out.printf("\nwindow: the store retains %d days, back to %s",
		w.RetainDays, w.RetainedFrom.Format("2006-01-02"))
	if !w.OldestRecorded.IsZero() {
		out.printf("; oldest record in range %s", w.OldestRecorded.Format(time.RFC3339))
	}
	out.printf("\n")
	if w.Complete {
		out.printf("this window is complete: nothing inside the range queried was dropped.\n")
		return
	}
	out.printf("THIS WINDOW IS INCOMPLETE: %s\n", w.Reason)
}

// exportAudit follows the page token to exhaustion, writing each record as it
// arrives. The window goes to stderr: stdout is the data, and a note in the
// middle of a JSONL stream would corrupt it for the pipeline it was written for.
func exportAudit(cmd *cobra.Command, store auditStore, q controlstore.AuditQuery) error {
	q.Limit = controlstore.MaxAuditPageSize
	encoder := json.NewEncoder(cmd.OutOrStdout())
	var window controlstore.AuditWindow
	total := 0

	for {
		page, err := store.Query(cmd.Context(), q)
		if err != nil {
			return err
		}
		window = page.Window
		for _, rec := range page.Records {
			if err := encoder.Encode(rec); err != nil {
				return fmt.Errorf("writing the export: %w", err)
			}
			total++
		}
		if page.NextPageToken == "" {
			break
		}
		q.PageToken = page.NextPageToken
	}

	note := &printer{w: cmd.ErrOrStderr()}
	note.printf("exported %d records; the store retains %d days, back to %s\n",
		total, window.RetainDays, window.RetainedFrom.Format("2006-01-02"))
	if !window.Complete {
		note.printf("THIS EXPORT IS INCOMPLETE: %s\n", window.Reason)
	}
	return note.err
}

// auditMethod shortens a procedure to `Service.Method`. The full procedure is
// in the exported record; a listing wants the part that differs.
func auditMethod(procedure string) string {
	trimmed := strings.TrimPrefix(procedure, "/kelson.v1alpha1.")
	if trimmed == procedure {
		return procedure
	}
	return strings.ReplaceAll(trimmed, "/", ".")
}
