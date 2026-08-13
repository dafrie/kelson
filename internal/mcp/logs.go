package mcp

import (
	"context"
	"errors"
	"fmt"

	"connectrpc.com/connect"
	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"

	kelsonv1alpha1 "github.com/dafrie/kelson/internal/api/gen/kelson/v1alpha1"
)

const logsWindowDescription = `Return one bounded window of an application's logs.

READ-ONLY. Changes nothing. It never follows or streams: this is a window, always finite, at most 200 lines whatever tail you ask for.

Preconditions: the project must be stored and you must know which application you want. The Kubernetes namespace is resolved from the environment's status automatically; if the server cannot report it, the model default (<project>-<environment>) is used and the answer says so.

Prefer diagnose_application when you do not yet know what is wrong — it picks the failing workload for you and returns status, verdicts, history and a log window together. Use this tool when you already know the application and want more lines, a different window, or a filter.

Set around_termination=true for the lines immediately before a container died (the crash-loop question). Set match to keep only lines containing that substring; filtering happens on the server, before the window is cut, so a filtered window is still the last N matching lines.`

const defaultLogTail = 100

type logsWindowInput struct {
	Project           string `json:"project" jsonschema:"the stored project name"`
	Environment       string `json:"environment" jsonschema:"the environment whose workloads to read, e.g. production"`
	Application       string `json:"application,omitempty" jsonschema:"the application to read; omitted selects the first failing workload the environment reports"`
	Tail              int    `json:"tail,omitempty" jsonschema:"how many lines to return; default 100, capped at 200"`
	AroundTermination bool   `json:"around_termination,omitempty" jsonschema:"return the lines just before the container terminated instead of the plain tail"`
	Match             string `json:"match,omitempty" jsonschema:"keep only lines containing this substring"`
}

func logsWindowTool(c *clients) tool {
	def := readOnlyTool("logs_window", "Read a log window", logsWindowDescription)
	return tool{
		def:  def,
		rpcs: []rpc{rpcStatus, rpcQueryLogs},
		add: func(srv *mcpsdk.Server) {
			mcpsdk.AddTool(srv, def, func(ctx context.Context, _ *mcpsdk.CallToolRequest, in logsWindowInput) (*mcpsdk.CallToolResult, any, error) {
				return c.logsWindow(ctx, in)
			})
		},
	}
}

// logsWindow composes Status (for the namespace, and for which application to
// read when the caller did not say) with one bounded QueryLogs.
//
// FollowLogs exists on the API and is deliberately not exposed: an unbounded
// stream is correct for a terminal and unusable in a context window (ADR-0008
// §2). Everything this tool can return is finite and stated.
func (c *clients) logsWindow(ctx context.Context, in logsWindowInput) (*mcpsdk.CallToolResult, any, error) {
	tail := in.Tail
	if tail <= 0 {
		tail = defaultLogTail
	}
	clamped := false
	if tail > maxLogLines {
		tail, clamped = maxLogLines, true
	}

	var r report
	namespace, application, statusErr := c.logTarget(ctx, in)
	fallback := namespace == ""
	if fallback {
		namespace = defaultNamespace(in.Project, in.Environment)
	}
	if in.Application != "" {
		application = in.Application
	}
	if application == "" {
		reason := "the environment reports no workload verdicts"
		if statusErr != nil {
			reason = "the environment's status was unreadable — " + connectMessage(statusErr)
		}
		return nil, nil, errors.New("logs_window cannot pick a workload for you: " + reason +
			". Name the application explicitly and retry, or call diagnose_application to see what this environment declares.")
	}

	request := &kelsonv1alpha1.QueryLogsRequest{
		Selector: &kelsonv1alpha1.LogSelector{Namespace: namespace, Application: application},
	}
	window := fmt.Sprintf("tail %d", tail)
	if in.AroundTermination {
		window = fmt.Sprintf("last %d lines before termination", tail)
		request.Around = &kelsonv1alpha1.LogAround{Lines: int32(tail), AtTermination: true} //nolint:gosec // tail is clamped to maxLogLines
	} else {
		request.Tail = int32(tail) //nolint:gosec // tail is clamped to maxLogLines
	}
	if in.Match != "" {
		request.Match = &kelsonv1alpha1.LogMatch{Substring: in.Match}
	}

	r.addf("logs %s/%s application=%s namespace=%s (%s)", in.Project, in.Environment, application, namespace, window)
	if fallback {
		reason := "the server reported none"
		if statusErr != nil {
			reason = "the environment's status was unreadable — " + connectMessage(statusErr)
		}
		r.addf("note: the namespace is the model default <project>-<environment> rather than the resolved one (%s); a spec that sets spec.namespace would make this wrong.", reason)
	}
	if clamped {
		r.addf("note: tail %d was clamped to the tool's cap of %d lines.", in.Tail, maxLogLines)
	}
	if in.Match != "" {
		r.addf("note: filtered to lines containing %q, before the window was cut.", in.Match)
	}

	res, err := c.logs.QueryLogs(ctx, connect.NewRequest(request))
	if err != nil {
		return nil, nil, c.fail(rpcQueryLogs, err)
	}
	r.section(fmt.Sprintf("LINES (at most %d)", tail))
	writeLogLines(&r, res.Msg.GetLines(), tail)
	return text(&r)
}

// logTarget resolves the namespace and, when the caller named no application,
// which workload to read. Both come from Status: the namespace because it is
// the only RPC that resolves a spec's override (#161), and the application
// because the failing workload is the one worth reading. An empty namespace
// means Status could not answer, which the caller reports rather than papers
// over.
func (c *clients) logTarget(ctx context.Context, in logsWindowInput) (namespace, application string, err error) {
	res, err := c.deploy.Status(ctx, connect.NewRequest(&kelsonv1alpha1.StatusRequest{
		Spec:        specRef(in.Project),
		Environment: in.Environment,
	}))
	if err != nil {
		return "", "", err
	}
	if verdicts := res.Msg.GetVerdicts(); len(verdicts) > 0 {
		application = resourceName(verdicts[0].GetResource())
		if failing := failingVerdicts(verdicts); len(failing) > 0 {
			application = resourceName(failing[0].GetResource())
		}
	}
	return res.Msg.GetNamespace(), application, nil
}

// defaultNamespace is the model's own default for an environment that sets no
// spec.namespace (internal/model resolve). It is a fallback and never a
// substitute: a spec that overrides the namespace makes it wrong, which is why
// every use of it is announced in the answer.
func defaultNamespace(project, environment string) string {
	return fmt.Sprintf("%s-%s", project, environment)
}
