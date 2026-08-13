package mcp

import (
	"context"
	"fmt"

	"connectrpc.com/connect"
	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"

	kelsonv1alpha1 "github.com/dafrie/kelson/internal/api/gen/kelson/v1alpha1"
	"github.com/dafrie/kelson/internal/model"
)

const diagnoseApplicationDescription = `Answer "what is wrong with this environment, and why" in one call.

Composes, for one (project, environment): the delivery phase, revision, namespace and cause; every workload's health verdict with the server's own remediation; a bounded window of the failing workload's logs (the lines before it terminated, when it is failing); the last 5 deployment revisions; and a compact summary of what the spec declares (applications, images, ports).

READ-ONLY. Changes nothing.

Preconditions: the project must be stored on the server (put_spec) and must declare the named environment.

Prefer this over logs_window when you do not yet know what is wrong: logs_window needs you to already know which application to read. Prefer it over calling status, history and logs separately — this is the same information for one round trip instead of six.

Costs several server calls and returns at most 80 log lines, the last 5 revisions and 12 workload verdicts, each truncated explicitly. It never returns full manifests or the spec YAML.`

type diagnoseApplicationInput struct {
	Project     string `json:"project" jsonschema:"the stored project name, as reported by list_applications"`
	Environment string `json:"environment" jsonschema:"the environment to diagnose, e.g. production"`
}

func diagnoseApplicationTool(c *clients) tool {
	def := readOnlyTool("diagnose_application", "Diagnose an application", diagnoseApplicationDescription)
	return tool{
		def:  def,
		rpcs: []rpc{rpcStatus, rpcQueryLogs, rpcHistory, rpcGetSpec},
		add: func(srv *mcpsdk.Server) {
			mcpsdk.AddTool(srv, def, func(ctx context.Context, _ *mcpsdk.CallToolRequest, in diagnoseApplicationInput) (*mcpsdk.CallToolResult, any, error) {
				return c.diagnoseApplication(ctx, in)
			})
		},
	}
}

// diagnoseApplication is the composition ADR-0008 §1 names: status, verdicts, a
// log window around the failure, recent history and a spec summary, in one
// answer.
//
// Status is the spine and its failure is the answer; everything after it is
// additive, so a history store that cannot be read or a log engine the server
// was started without degrades to a note instead of taking the diagnosis with
// it. Nothing here classifies anything: the verdicts, their remediations and
// the phase are relayed exactly as the server reported them.
func (c *clients) diagnoseApplication(ctx context.Context, in diagnoseApplicationInput) (*mcpsdk.CallToolResult, any, error) {
	res, err := c.deploy.Status(ctx, connect.NewRequest(&kelsonv1alpha1.StatusRequest{
		Spec:        specRef(in.Project),
		Environment: in.Environment,
	}))
	if err != nil {
		return nil, nil, c.fail(rpcStatus, err)
	}
	status := res.Msg

	var r report
	r.addf("%s/%s: %s", in.Project, in.Environment, verdictLine(status))

	r.section("STATUS")
	r.addf("  phase      %s", status.GetPhase())
	r.addf("  revision   %s", orDash(status.GetRevision()))
	r.addf("  namespace  %s", orDash(status.GetNamespace()))
	if cause := status.GetCause(); cause != "" {
		r.addf("  cause      %s", cause)
	}
	for _, key := range sortedKeys(status.GetDetail()) {
		r.addf("  %s%s", pad(key, 11), status.GetDetail()[key])
	}

	c.reportWorkloads(&r, status)
	c.reportLogs(ctx, &r, status, in)
	c.reportHistory(ctx, &r, in)
	c.reportSpec(ctx, &r, in)
	return text(&r)
}

// verdictLine is the one-line answer the rest of the report explains.
func verdictLine(status *kelsonv1alpha1.StatusResponse) string {
	verdicts := status.GetVerdicts()
	failing := failingVerdicts(verdicts)
	switch {
	case len(verdicts) == 0:
		return fmt.Sprintf("%s at revision %s, no workload verdicts (the server reported no observable workloads for this environment)",
			status.GetPhase(), orDash(status.GetRevision()))
	case len(failing) == 0:
		return fmt.Sprintf("%s — %d of %d workloads healthy at revision %s",
			status.GetPhase(), len(verdicts), len(verdicts), orDash(status.GetRevision()))
	default:
		return fmt.Sprintf("%s — %d of %d workloads failing, first is %s (%s)",
			status.GetPhase(), len(failing), len(verdicts),
			resourceName(failing[0].GetResource()), failing[0].GetCode())
	}
}

func (c *clients) reportWorkloads(r *report, status *kelsonv1alpha1.StatusResponse) {
	r.section("WORKLOADS")
	if len(status.GetVerdicts()) == 0 {
		r.addf("  none reported")
		return
	}
	verdicts, dropped := limit(status.GetVerdicts(), maxVerdicts)
	for _, v := range verdicts {
		marker := "ok  "
		if !v.GetHealthy() {
			marker = "FAIL"
		}
		r.addf("  %s %s  %s", marker, v.GetResource(), v.GetCode())
		if message := v.GetMessage(); message != "" {
			r.addf("       %s", message)
		}
		if remediation := v.GetRemediation(); remediation != "" {
			r.addf("       fix: %s", remediation)
		}
	}
	r.truncated(dropped, "workloads")
}

// reportLogs embeds the window around the failure.
//
// Which window is the server's verdict, not this tool's guess: a workload the
// observation plane called unhealthy gets Around.AtTermination — the lines
// before the container died, which is the crash-loop diagnosis — and anything
// else gets a plain tail. The selector's namespace comes from Status, which is
// the only RPC that resolves it (#161).
func (c *clients) reportLogs(ctx context.Context, r *report, status *kelsonv1alpha1.StatusResponse, in diagnoseApplicationInput) {
	verdicts := status.GetVerdicts()
	if len(verdicts) == 0 {
		r.section("LOGS")
		r.addf("  skipped: no workload to select logs for. Use logs_window with an explicit application.")
		return
	}
	focus := verdicts[0]
	if failing := failingVerdicts(verdicts); len(failing) > 0 {
		focus = failing[0]
	}
	application := resourceName(focus.GetResource())
	atTermination := !focus.GetHealthy()

	request := &kelsonv1alpha1.QueryLogsRequest{
		Selector: &kelsonv1alpha1.LogSelector{
			Namespace:   status.GetNamespace(),
			Application: application,
		},
	}
	window := "tail"
	if atTermination {
		window = "at termination"
		request.Around = &kelsonv1alpha1.LogAround{Lines: diagnoseLogLines, AtTermination: true}
	} else {
		request.Tail = diagnoseLogLines
	}

	r.section(fmt.Sprintf("LOGS (%s, %s, at most %d lines)", application, window, diagnoseLogLines))
	if status.GetNamespace() == "" {
		r.addf("  skipped: the server reported no namespace for this environment, so the pods cannot be selected.")
		return
	}
	res, err := c.logs.QueryLogs(ctx, connect.NewRequest(request))
	if err != nil {
		r.addf("  unavailable — %s", connectMessage(err))
		return
	}
	writeLogLines(r, res.Msg.GetLines(), diagnoseLogLines)
}

// writeLogLines renders a log window, capped a second time here. The engine is
// asked for a bounded query and returns what it was asked for, but the cap is
// this package's promise rather than the server's, so it is enforced where it
// is promised.
func writeLogLines(r *report, lines []*kelsonv1alpha1.LogLine, max int) {
	if len(lines) == 0 {
		r.addf("  (no lines matched)")
		return
	}
	// The newest lines are the ones that explain a failure, so an over-long
	// window loses its head, not its tail.
	dropped := 0
	if len(lines) > max {
		dropped = len(lines) - max
		lines = lines[len(lines)-max:]
		r.addf("  "+truncation, dropped, "earlier lines")
	}
	for _, line := range lines {
		r.addf("  %s %s  %s", logStamp(line.GetTimestampUnixMs()), line.GetPod(), line.GetMessage())
	}
}

func (c *clients) reportHistory(ctx context.Context, r *report, in diagnoseApplicationInput) {
	r.section(fmt.Sprintf("HISTORY (last %d)", maxHistory))
	res, err := c.deploy.History(ctx, connect.NewRequest(&kelsonv1alpha1.HistoryRequest{
		Spec:        specRef(in.Project),
		Environment: in.Environment,
	}))
	if err != nil {
		r.addf("  unavailable — %s", connectMessage(err))
		return
	}
	entries, dropped := limit(res.Msg.GetEntries(), maxHistory)
	if len(entries) == 0 {
		r.addf("  no recorded revisions: nothing has been deployed to this environment")
		return
	}
	for _, e := range entries {
		r.addf("  %s %s  %s", pad(e.GetRevision(), 8), pad(e.GetCommittedAt(), 22), e.GetMessage())
	}
	r.truncated(dropped, "older revisions")
}

// reportSpec summarises what the spec declares, from the stored documents.
//
// It is a summary and never the YAML: the documents are the user's and can be
// arbitrarily long, and an agent diagnosing a failure needs to know which
// applications exist, what images they run and which ports they serve — not the
// authored file. Images and ports are Project-level facts (model rule P3;
// an Environment override cannot change either), so the Project document is the
// honest source for this.
func (c *clients) reportSpec(ctx context.Context, r *report, in diagnoseApplicationInput) {
	r.section("SPEC")
	res, err := c.spec.GetSpec(ctx, connect.NewRequest(&kelsonv1alpha1.GetSpecRequest{Project: in.Project}))
	if err != nil {
		r.addf("  unavailable — %s", connectMessage(err))
		return
	}
	project, err := decodeProject(res.Msg.GetSpec().GetDocuments().GetProject())
	if err != nil {
		r.addf("  unavailable — %s", err)
		return
	}
	components, dropped := limit(project.Spec.Components, maxComponents)
	for _, c := range components {
		r.addf("  %s %s %s", pad(c.Name, 16), pad(componentImage(project, c), 40), componentShape(c))
	}
	r.truncated(dropped, "components")
}

// decodeProject reads the Project document with the authoring plane's own
// decoder, so the summary cannot disagree with what the server validated.
func decodeProject(document []byte) (*model.Project, error) {
	if len(document) == 0 {
		return nil, fmt.Errorf("the stored spec carries no Project document")
	}
	documents, errs := model.DecodeDocuments(document)
	if len(errs) > 0 {
		return nil, fmt.Errorf("the stored Project document does not decode: %s", errs[0].Message)
	}
	for _, d := range documents {
		if project, ok := d.(*model.Project); ok {
			return project, nil
		}
	}
	return nil, fmt.Errorf("the stored spec carries no Project document")
}

// componentImage is the image a workload component runs. A data component has
// none: its pods are the operator's, not the spec's (ADR-0005).
func componentImage(project *model.Project, c model.Component) string {
	switch {
	case c.EffectiveKind().IsData():
		return "(operator-managed)"
	case c.Image != "":
		return c.Image
	case project.Spec.Image != "":
		return project.Spec.Image
	default:
		return "(built from source)"
	}
}

// componentShape is the one-phrase summary of what a component renders to:
// the kind, plus the field that distinguishes it from its siblings.
func componentShape(c model.Component) string {
	kind := c.EffectiveKind()
	switch kind {
	case model.ComponentService:
		return fmt.Sprintf("port %d", c.Port)
	case model.ComponentCron:
		return "schedule " + c.Schedule
	case model.ComponentPostgres, model.ComponentValkey:
		preset := c.Preset
		if preset == "" {
			preset = model.PresetShared
		}
		return fmt.Sprintf("%s preset %s", kind, preset)
	default:
		return string(kind)
	}
}

func failingVerdicts(verdicts []*kelsonv1alpha1.WorkloadVerdict) []*kelsonv1alpha1.WorkloadVerdict {
	var out []*kelsonv1alpha1.WorkloadVerdict
	for _, v := range verdicts {
		if !v.GetHealthy() {
			out = append(out, v)
		}
	}
	return out
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}
