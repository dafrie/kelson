package mcp

import (
	"context"
	"fmt"
	"time"

	"connectrpc.com/connect"
	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"

	kelsonv1alpha1 "github.com/dafrie/kelson/internal/api/gen/kelson/v1alpha1"
	"github.com/dafrie/kelson/internal/delivery"
)

const waitForOutcomeDescription = `Block until an environment reaches an outcome, then return what happened.

READ-ONLY. Changes nothing — it watches the server's event stream.

Returns as soon as one of these arrives: the environment transitions to Healthy or Rejected, a workload's health verdict turns to a failure, or the timeout expires. The answer is the triggering signal plus a bounded trail of the events seen on the way.

Use it after deploy or rollback, or after any change you expect the cluster to react to: it is how "deploy, then react to what happened" costs one blocking call instead of a polling loop, and polling diagnose_application in a loop is the thing this tool exists to replace.

Preconditions: the project must be stored and declare the environment. timeout_seconds defaults to 60 and is capped at 600.

A timeout is an answer, not an error: it means nothing terminal happened in the window, and the environment may still be progressing. Follow up with diagnose_application.`

const (
	defaultWaitSeconds = 60
	maxWaitSeconds     = 600
)

type waitForOutcomeInput struct {
	Project        string `json:"project" jsonschema:"the stored project name"`
	Environment    string `json:"environment" jsonschema:"the environment to watch, e.g. production"`
	TimeoutSeconds int    `json:"timeout_seconds,omitempty" jsonschema:"how long to wait; default 60, capped at 600"`
}

func waitForOutcomeTool(c *clients) tool {
	def := readOnlyTool("wait_for_outcome", "Wait for an outcome", waitForOutcomeDescription)
	return tool{
		def:  def,
		rpcs: []rpc{rpcWatch, rpcStatus},
		add: func(srv *mcpsdk.Server) {
			mcpsdk.AddTool(srv, def, func(ctx context.Context, _ *mcpsdk.CallToolRequest, in waitForOutcomeInput) (*mcpsdk.CallToolResult, any, error) {
				return c.waitForOutcome(ctx, in)
			})
		},
	}
}

// waitForOutcome consumes EventService.Watch until something terminal happens.
//
// Which signals are terminal is the schema's vocabulary, not this tool's
// invention: the phases are delivery's own (Healthy and Rejected are the two
// states nothing follows), and a health change is a failure when the server
// says the workload is not healthy. Resync is honoured the way the schema
// documents it — relist DeployService.Status, then keep reading the same
// stream — because a Resync means the watcher's view has a gap, and a tool that
// ignored it would report "nothing happened" for events it simply missed.
func (c *clients) waitForOutcome(ctx context.Context, in waitForOutcomeInput) (*mcpsdk.CallToolResult, any, error) {
	seconds := in.TimeoutSeconds
	switch {
	case seconds <= 0:
		seconds = defaultWaitSeconds
	case seconds > maxWaitSeconds:
		seconds = maxWaitSeconds
	}
	deadline := time.Duration(seconds) * time.Second

	watchCtx, cancel := context.WithTimeout(ctx, deadline)
	defer cancel()

	stream, err := c.events.Watch(watchCtx, connect.NewRequest(&kelsonv1alpha1.WatchRequest{
		Scopes: []*kelsonv1alpha1.WatchRequest_Scope{{Project: in.Project, Environment: in.Environment}},
	}))
	if err != nil {
		return nil, nil, c.fail(rpcWatch, err)
	}
	defer func() { _ = stream.Close() }()

	started := time.Now()
	var (
		trail   []string
		outcome string
	)
	for outcome == "" && stream.Receive() {
		switch body := stream.Msg().GetBody().(type) {
		case *kelsonv1alpha1.WatchResponse_Resync_:
			reason := body.Resync.GetReason()
			trail = append(trail, fmt.Sprintf("%s resync — %s", elapsed(started), reason))
			trail = append(trail, fmt.Sprintf("%s relisted status — %s", elapsed(started), c.relist(watchCtx, in)))
		case *kelsonv1alpha1.WatchResponse_Event_:
			event := body.Event
			trail = append(trail, fmt.Sprintf("%s %s", elapsed(started), describeEvent(event)))
			outcome = terminalOutcome(event)
		}
	}
	timedOut := watchCtx.Err() != nil && ctx.Err() == nil

	var r report
	switch {
	case outcome != "":
		r.addf("wait %s/%s: OUTCOME after %s — %s", in.Project, in.Environment, elapsed(started), outcome)
	case timedOut:
		r.addf("wait %s/%s: TIMEOUT after %ds — nothing terminal happened in the window. The environment may still be progressing; call diagnose_application to see where it is.",
			in.Project, in.Environment, seconds)
	default:
		if err := stream.Err(); err != nil {
			return nil, nil, c.fail(rpcWatch, err)
		}
		r.addf("wait %s/%s: STREAM ENDED after %s with no terminal signal. The server closes a watch whose scope matches nothing stored — check the project and environment names with list_applications.",
			in.Project, in.Environment, elapsed(started))
	}

	shown, dropped := limit(trail, maxEvents)
	r.section(fmt.Sprintf("EVENTS (%d)", len(trail)))
	if len(trail) == 0 {
		r.addf("  none")
	}
	for _, line := range shown {
		r.addf("  %s", line)
	}
	r.truncated(dropped, "events")
	return text(&r)
}

// terminalOutcome reports the answer an event ends the wait with, or "" when
// the wait continues.
func terminalOutcome(event *kelsonv1alpha1.WatchResponse_Event) string {
	switch payload := event.GetPayload().(type) {
	case *kelsonv1alpha1.WatchResponse_Event_StatusTransition:
		transition := payload.StatusTransition
		phase := delivery.Phase(transition.GetPhase())
		if phase != delivery.PhaseHealthy && phase != delivery.PhaseRejected {
			return ""
		}
		outcome := fmt.Sprintf("the environment reached %s (from %s), revision %s",
			transition.GetPhase(), orDash(transition.GetPreviousPhase()), orDash(transition.GetRevision()))
		if cause := transition.GetCause(); cause != "" {
			outcome += ", cause: " + cause
		}
		return outcome
	case *kelsonv1alpha1.WatchResponse_Event_HealthChange:
		change := payload.HealthChange
		if change.GetHealthy() || change.GetCode() == "" {
			return ""
		}
		return fmt.Sprintf("workload %s turned unhealthy: %s (was %s) — %s",
			change.GetResource(), change.GetCode(), orDash(change.GetPreviousCode()), change.GetMessage())
	default:
		return ""
	}
}

func describeEvent(event *kelsonv1alpha1.WatchResponse_Event) string {
	switch payload := event.GetPayload().(type) {
	case *kelsonv1alpha1.WatchResponse_Event_StatusTransition:
		transition := payload.StatusTransition
		return fmt.Sprintf("status %s -> %s (revision %s)",
			orDash(transition.GetPreviousPhase()), transition.GetPhase(), orDash(transition.GetRevision()))
	case *kelsonv1alpha1.WatchResponse_Event_HealthChange:
		change := payload.HealthChange
		return fmt.Sprintf("health %s: %s -> %s", change.GetResource(), orDash(change.GetPreviousCode()), change.GetCode())
	default:
		return "unrecognised event type (this server speaks a newer schema than this tool)"
	}
}

// relist is the Resync half of the contract: one Status read to replace the
// view that may have a gap. Its failure is reported in the trail rather than
// ending the wait — the stream is still live, and the caller asked to wait.
func (c *clients) relist(ctx context.Context, in waitForOutcomeInput) string {
	res, err := c.deploy.Status(ctx, connect.NewRequest(&kelsonv1alpha1.StatusRequest{
		Spec:        specRef(in.Project),
		Environment: in.Environment,
	}))
	if err != nil {
		return "unavailable: " + connectMessage(err)
	}
	return fmt.Sprintf("phase %s, revision %s, %s",
		res.Msg.GetPhase(), orDash(res.Msg.GetRevision()), workloadSummary(res.Msg.GetVerdicts()))
}

func elapsed(since time.Time) string {
	return fmt.Sprintf("+%.0fs", time.Since(since).Seconds())
}
