package mcp

import (
	"context"
	"fmt"
	"strings"

	"connectrpc.com/connect"
	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"

	kelsonv1alpha1 "github.com/dafrie/kelson/internal/api/gen/kelson/v1alpha1"
	"github.com/dafrie/kelson/internal/clusterprofile"
	"github.com/dafrie/kelson/internal/clusterprofile/support"
	"github.com/dafrie/kelson/internal/model"
)

const diagnoseApplicationDescription = `Answer "what is wrong with this environment, and why" in one call.

Composes, for one (project, environment): the server's own causal answer — WHY — with a confidence and the evidence behind each cause; the delivery phase, revision, namespace and cause; every workload's health verdict with the server's own remediation; a bounded window of the failing workload's logs (the lines before it terminated, when it is failing); the last 5 deployment revisions; a compact summary of what the spec declares (components, images, kinds); the Secrets kelson manages in the namespace, by name and key; and the cluster's version skew against what kelson renders against.

Read the WHY section first, and weigh each cause by its confidence rather than by its position. "high" means a controller named the reason — a kubelet waiting reason, a scheduler message, an external-secrets condition, an API-server rejection — or that two independent signals agreed; act on it. "medium" means one signal only, or a heuristic the server itself calls one, and its sentence says which half is a correlation; verify before acting. "low" is an audit-mode policy finding that vetoed nothing. Each cause carries the evidence it rests on (the controller's own words, a bounded log excerpt, a field reference in the rendered manifest) and, where the recorded history shows one, the revision that introduced the change being blamed — which is the answer to "what changed" without a second call. The section also lists what the server could not read, so a partial answer is never mistaken for a complete one.

The Secrets are there for one specific failure: a workload stuck in CreateContainerConfigError is usually a spec referencing { secret: <name>, key: <key> } that does not exist. Compare the SECRETS section against the references in the spec; set_secret writes a missing one.

Under the externalSecrets backend the same failure has a different cause and it is already in the WORKLOADS section: a verdict on an external-secrets.io/ExternalSecret resource with code secret-sync-failed means the external-secrets controller could not read the value from the backing store or could not write the Secret, and the verdict carries the controller's own reason (SecretSyncedError, SecretMissing) and message. Read that before the pods below it — it is why they cannot start. The SECRETS section lists only the Secrets kelson itself writes, so an environment on this backend will normally show none there and that is not a fault.

The VERSION SKEW section is there for another: an adopted operator too old to serve the API kelson writes accepts the manifest and then never reconciles it, and the symptom arrives long after the deploy with nothing in the workload's logs to point at the version. An [unsupported] line names the component, both versions and what specifically degrades; [unknown] means the version could not be read, not that it is fine; [note] means newer than kelson has tested, which is never a fault.

READ-ONLY. Changes nothing. Never reports a secret value — kelson does not store them and no API returns one.

Preconditions: the project must be stored on the server (put_spec) and must declare the named environment.

Prefer this over logs_window when you do not yet know what is wrong: logs_window needs you to already know which workload to read. Prefer it over calling status, history and logs separately — this is the same information for one round trip instead of six.

Costs several server calls and returns at most 80 log lines, the last 5 revisions and 12 workload verdicts, each truncated explicitly. It never returns full manifests or the spec YAML.`

type diagnoseApplicationInput struct {
	Project     string `json:"project" jsonschema:"the stored project name, as reported by list_applications"`
	Environment string `json:"environment" jsonschema:"the environment to diagnose, e.g. production"`
}

func diagnoseApplicationTool(c *clients) tool {
	def := readOnlyTool("diagnose_application", "Diagnose an application", diagnoseApplicationDescription)
	return tool{
		def:  def,
		rpcs: []rpc{rpcExplain, rpcStatus, rpcQueryLogs, rpcHistory, rpcGetSpec, rpcListSecrets, rpcGetProfile},
		add: func(srv *mcpsdk.Server) {
			mcpsdk.AddTool(srv, def, func(ctx context.Context, _ *mcpsdk.CallToolRequest, in diagnoseApplicationInput) (*mcpsdk.CallToolResult, any, error) {
				return c.diagnoseApplication(ctx, in)
			})
		},
	}
}

// diagnoseApplication is the composition ADR-0008 §1 names: the server's
// causes, status, verdicts, a log window around the failure, recent history and
// a spec summary, in one answer.
//
// Status is the spine and its failure is the answer; everything after it is
// additive, so a history store that cannot be read or a log engine the server
// was started without degrades to a note instead of taking the diagnosis with
// it. Nothing here classifies anything: the verdicts, their remediations and
// the phase are relayed exactly as the server reported them — and since #77 so
// is the causal answer itself, which ExplainService produces and the WHY
// section renders (why.go, ADR-0023). This tool is the task-shaped
// presentation of that capability, never a second implementation of it.
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

	c.reportWhy(ctx, &r, in)

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
	c.reportSecrets(ctx, &r, in)
	c.reportSkew(ctx, &r)
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
// components exist, what images they run and what each one is — not the
// authored file. Kinds are Project-level facts, but the image a component runs
// is an environment-scoped fact since ADR-0016: an Environment can pin a
// component's image (rule P3, the promotion primitive), so the pin for the
// diagnosed environment is read alongside the Project document.
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
	pins := environmentImagePins(res.Msg.GetSpec().GetDocuments().GetEnvironments()[in.Environment])
	components, dropped := limit(project.Spec.Components, maxComponents)
	for _, c := range components {
		r.addf("  %s %s %s", pad(c.Name, 16), pad(componentImage(project, pins, c), 40), componentShape(c))
	}
	r.truncated(dropped, "components")
}

// reportSecrets lists the Secrets kelson manages in the environment's
// namespace, by name and key (issue #116).
//
// It belongs in a diagnosis rather than in a read tool of its own because of
// the failure it explains. A spec referencing `{secret: checkout-db, key: url}`
// against a Secret nobody wrote renders cleanly, applies cleanly and fails at
// pod start with CreateContainerConfigError; ADR-0018 records that nothing in
// kelson correlates a reference with the Secret it names, so an agent holding
// the SPEC section above and this section is the correlation. A key that is
// missing here and referenced there is the answer.
//
// It is additive like every other section: a server without the secret backend,
// or one whose credentials cannot list Secrets, degrades to a line rather than
// taking the diagnosis with it. And it can only ever print names and keys —
// ListSecrets has no field a value could arrive in.
func (c *clients) reportSecrets(ctx context.Context, r *report, in diagnoseApplicationInput) {
	r.section("SECRETS (kelson-managed, keys only — values are never readable)")
	res, err := c.secrets.ListSecrets(ctx, connect.NewRequest(&kelsonv1alpha1.ListSecretsRequest{
		Target: &kelsonv1alpha1.SecretTarget{Project: in.Project, Environment: in.Environment},
	}))
	if err != nil {
		r.addf("  unavailable — %s", connectMessage(err))
		return
	}
	secrets, dropped := limit(res.Msg.GetSecrets(), maxSecrets)
	if len(secrets) == 0 {
		r.addf("  none: kelson manages no Secrets in %s. A component referencing one will fail at pod start "+
			"with CreateContainerConfigError; set_secret writes it.", orDash(res.Msg.GetNamespace()))
		return
	}
	for _, s := range secrets {
		r.addf("  %s %s", pad(s.GetName(), 20), strings.Join(s.GetKeys(), ", "))
	}
	r.truncated(dropped, "secrets")
}

// reportSkew names the version skew between the cluster and what kelson renders
// against (issue #57).
//
// It belongs in a diagnosis for the same reason the Secrets do: it explains a
// specific failure that is otherwise a mystery. An operator too old to serve the
// API kelson writes accepts the manifest and then does nothing useful with it,
// and the symptom — a `kind: postgres` component that never becomes ready, a
// HelmRelease nothing reconciles — arrives long after the deploy that caused it,
// with nothing in the workload's own logs to point at the version.
//
// The statements are composed here from the profile YAML rather than read off a
// wire field, because they are a judgement *about* the profile: storing them
// would carry a stale answer into every consumer that read the document back.
// internal/clusterprofile/support is pure, so composing them costs a parse.
//
// Additive like every other section: a server started without a cluster to
// profile answers Unimplemented, and that degrades to one line instead of taking
// the diagnosis with it.
func (c *clients) reportSkew(ctx context.Context, r *report) {
	r.section("VERSION SKEW (cluster vs. what kelson renders against)")
	if c.profile == nil {
		r.addf("  unavailable — this build has no profile client")
		return
	}
	res, err := c.profile.GetProfile(ctx, connect.NewRequest(&kelsonv1alpha1.GetProfileRequest{}))
	if err != nil {
		r.addf("  unavailable — %s", connectMessage(err))
		return
	}
	profile, err := clusterprofile.Unmarshal(res.Msg.GetYaml())
	if err != nil {
		r.addf("  unavailable — the server's profile document does not decode: %s", err)
		return
	}
	statements := support.Check(profile).Statements()
	if len(statements) == 0 {
		r.addf("  none: every component this cluster reports is at or above the version kelson requires")
		return
	}
	for _, s := range statements {
		r.addf("  %s", s)
	}
}

// environmentImagePins reads the diagnosed Environment's per-component image
// pins (rule P3, ADR-0016). A missing or undecodable document yields no pins:
// this is a summary, and the validator — not the summariser — owns rejecting a
// broken document.
func environmentImagePins(document []byte) map[string]string {
	if len(document) == 0 {
		return nil
	}
	documents, errs := model.DecodeDocuments(document)
	if len(errs) > 0 {
		return nil
	}
	for _, d := range documents {
		environment, ok := d.(*model.Environment)
		if !ok {
			continue
		}
		pins := map[string]string{}
		for _, override := range environment.Spec.Components {
			if override.Image != "" {
				pins[override.Name] = override.Image
			}
		}
		return pins
	}
	return nil
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

// componentImage is the image a workload component runs in the diagnosed
// environment: the environment's pin wins over the component's image, which
// wins over the Project's (rule P3). A data component has none: its pods are
// the operator's, not the spec's (ADR-0005). Neither does a helm component:
// what its chart runs is the chart's, and kelson's inventory ends at the
// HelmRelease (ADR-0016).
func componentImage(project *model.Project, pins map[string]string, c model.Component) string {
	switch {
	case c.EffectiveKind().IsData():
		return "(operator-managed)"
	case c.EffectiveKind().IsChart():
		return "(chart-managed)"
	case pins[c.Name] != "":
		return pins[c.Name] + " (pinned)"
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
		shape := fmt.Sprintf("%s preset %s", kind, preset)
		// Whether a cache has a password is the second thing anyone diagnosing
		// one wants to know, and it is the answer to two different questions at
		// once: why a client gets NOAUTH, and why one does not have to. The
		// Secret is named because this tool already lists the environment's
		// kelson-managed Secrets, so a missing one is visible in one answer
		// (ADR-0018's successor note).
		if kind == model.ComponentValkey {
			if c.Auth != nil {
				shape += fmt.Sprintf(", auth from secret %s key %s", c.Auth.Name, c.Auth.Key)
			} else {
				shape += ", no auth (reachable without a password in this namespace)"
			}
		}
		return shape
	case model.ComponentHelm:
		// The version belongs in the summary: it is the whole of what a chart
		// component pins, and the field a diagnosis most often turns on.
		return fmt.Sprintf("chart %s %s", c.Chart, c.ChartVersion)
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
