package mcp

import (
	"context"
	"fmt"

	"connectrpc.com/connect"
	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"

	kelsonv1alpha1 "github.com/dafrie/kelson/internal/api/gen/kelson/v1alpha1"
)

const listApplicationsDescription = `List every project kelson has stored and the live state of each of its environments: delivery phase, deployed revision, how many workloads are healthy, and the cause when one is not.

READ-ONLY. Changes nothing.

Use it to find out what exists and which environment is unhappy, when you do not already know. Once you know which environment is in trouble, prefer diagnose_application: it answers "why" in one call, and this tool deliberately answers only "what and where".

Returns no spec bodies, no manifests and no logs. Costs one status call per (project, environment) on the server, so it is the most expensive read on a large installation; the listing is capped at 25 projects and 10 environments per project and says so when it truncates.`

type listApplicationsInput struct{}

func listApplicationsTool(c *clients) tool {
	def := readOnlyTool("list_applications", "List applications", listApplicationsDescription)
	return tool{
		def:  def,
		rpcs: []rpc{rpcListSpecs, rpcStatus},
		add: func(srv *mcpsdk.Server) {
			mcpsdk.AddTool(srv, def, func(ctx context.Context, _ *mcpsdk.CallToolRequest, _ listApplicationsInput) (*mcpsdk.CallToolResult, any, error) {
				return c.listApplications(ctx)
			})
		},
	}
}

// listApplications composes ListSpecs with one Status per environment.
//
// A status that cannot be read is reported on its own line rather than failing
// the call: one unreachable environment must not hide the other nine, and the
// failure text carries the server's own code so an agent can tell "this
// environment is broken" from "kelson could not look".
func (c *clients) listApplications(ctx context.Context) (*mcpsdk.CallToolResult, any, error) {
	res, err := c.spec.ListSpecs(ctx, connect.NewRequest(&kelsonv1alpha1.ListSpecsRequest{}))
	if err != nil {
		return nil, nil, c.fail(rpcListSpecs, err)
	}

	var r report
	all := res.Msg.GetSpecs()
	if len(all) == 0 {
		r.addf("No projects are stored on kelson-server at %s. Store one with put_spec.", c.addr)
		return text(&r)
	}

	specs, droppedProjects := limit(all, maxProjects)
	r.addf("%d project(s) stored on kelson-server at %s.", len(all), c.addr)
	for _, spec := range specs {
		r.section(spec.GetProject())
		environments, droppedEnvironments := limit(spec.GetEnvironments(), maxEnvironments)
		if len(environments) == 0 {
			r.addf("  (declares no environments)")
		}
		for _, environment := range environments {
			r.addf("  %s%s", pad(environment, 14), c.environmentLine(ctx, spec.GetProject(), environment))
		}
		r.truncated(droppedEnvironments, "environments")
	}
	r.truncated(droppedProjects, "projects")
	return text(&r)
}

// environmentLine is one environment's summary, minus the leading name column.
func (c *clients) environmentLine(ctx context.Context, project, environment string) string {
	res, err := c.deploy.Status(ctx, connect.NewRequest(&kelsonv1alpha1.StatusRequest{
		Spec:        specRef(project),
		Environment: environment,
	}))
	if err != nil {
		return "status unavailable — " + connectMessage(err)
	}
	status := res.Msg

	line := pad(status.GetPhase(), 12) + pad(revisionLabel(status.GetRevision()), 12) + workloadSummary(status.GetVerdicts())
	if cause := status.GetCause(); cause != "" {
		line += " — cause: " + cause
	}
	return line
}

func revisionLabel(revision string) string {
	if revision == "" {
		return "rev -"
	}
	return "rev " + revision
}

// workloadSummary counts the observation plane's verdicts. The classification
// is the server's — this counts what it said (issue #53).
func workloadSummary(verdicts []*kelsonv1alpha1.WorkloadVerdict) string {
	if len(verdicts) == 0 {
		return "no workload verdicts"
	}
	healthy, degraded := 0, 0
	for _, v := range verdicts {
		if v.GetHealthy() {
			healthy++
		}
		if v.GetDegraded() {
			degraded++
		}
	}
	summary := pad(fmt.Sprintf("%d/%d workloads healthy", healthy, len(verdicts)), 24)
	if degraded > 0 {
		summary += fmt.Sprintf("%d degraded", degraded)
	}
	return summary
}
