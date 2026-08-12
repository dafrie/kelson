package observation

// Code is the machine-actionable health verdict of one workload (issue #53).
// Like diff.Risk and delivery.Code it is a typed value a caller branches on
// without parsing prose: "Applied -> Healthy" and "Applied -> Degraded" are
// decided by whether the verdict's code is a failure. Codes are a
// compatibility promise, so an existing code never changes meaning.
type Code string

// The non-failure codes. Healthy is the only one that ever gates a rollout
// green. Progressing is "wait, there is no failure yet"; a caller that wants a
// decisive answer after waiting must use a Tracker, which turns the wait into
// the distinct Stuck state instead of a failure.
const (
	CodeHealthy     Code = "healthy"
	CodeProgressing Code = "progressing"
)

// The failure codes. Any of these on a verdict makes the workload Degraded,
// not Healthy: they are the concrete reasons an "Applied" rollout must not
// read as live-and-well (the conflation issue #53 exists to prevent).
const (
	// CodeCrashLoopBackOff: the kubelet named the container's waiting reason
	// CrashLoopBackOff, or the container has terminated repeatedly. Attached
	// container logs are the diagnosis.
	CodeCrashLoopBackOff Code = "crash-loop-back-off"
	// CodeImagePullBackOff: the image cannot be pulled (ImagePullBackOff or
	// ErrImagePull).
	CodeImagePullBackOff Code = "image-pull-back-off"
	// CodeFailingProbe: a readiness/liveness probe is failing — heuristic
	// (a container Running but not Ready).
	CodeFailingProbe Code = "failing-probe"
	// CodeInsufficientResources: unschedulable because the cluster lacks the
	// requested CPU/memory.
	CodeInsufficientResources Code = "insufficient-resources"
	// CodeSchedulingFailed: unschedulable for a non-resource reason (node
	// selector, affinity, taints).
	CodeSchedulingFailed Code = "scheduling-failed"
	// CodeMissing: the workload object is not in the cluster.
	CodeMissing Code = "missing"
)

// failure codes is the set a Verdict carries when it is a definitive failure,
// as opposed to a wait state. IsFailure and the Tracker both branch on it, so
// "the rollout is broken" and "the rollout has not finished yet" can never be
// confused.
var failureCodes = map[Code]bool{
	CodeCrashLoopBackOff:      true,
	CodeImagePullBackOff:      true,
	CodeFailingProbe:          true,
	CodeInsufficientResources: true,
	CodeSchedulingFailed:      true,
}

// IsFailure reports whether the code is a definitive health failure. The
// enumerating codes are exported so a caller can branch exactly the way the
// package does (the map is private on purpose; the predicate is the contract).
func IsFailure(c Code) bool { return failureCodes[c] }

// String renders the code, defaulting empty to CodeHealthy for humans.
func (c Code) String() string {
	if c == "" {
		return string(CodeHealthy)
	}
	return string(c)
}

// remediation is the one-line fix stated as an action, mirroring the shape of
// the model/delivery error taxonomy (a stable code, a reason, and an action)
// so a UI or agent can show "what to do" without the caller inventing it.
func (c Code) remediation() string {
	switch c {
	case CodeCrashLoopBackOff:
		return "the container keeps crashing: read its logs, fix the command or startup error, then redeploy"
	case CodeImagePullBackOff:
		return "the image cannot be pulled: check the image name and tag, and that the image pull secret exists"
	case CodeFailingProbe:
		return "a probe is failing: check the probe path and that the application is actually serving"
	case CodeInsufficientResources:
		return "the pod cannot be scheduled: the cluster lacks the requested resources — reduce requests or add capacity"
	case CodeSchedulingFailed:
		return "the pod cannot be scheduled: check node selectors, affinity rules and taints"
	case CodeMissing:
		return "the workload is not in the cluster: check the namespace and that it was applied"
	default:
		return ""
	}
}

// Container is the per-container half of a Verdict: the container whose status
// was read, its health code, the kubelet's reason, and (when a LogSource was
// available) the container's recent logs. Logs are best-effort: a verdict is
// valid without them, and a fetch failure is reported on the container, never
// turned into a verdict error.
type Container struct {
	Name     string `json:"name"`
	Pod      string `json:"pod,omitempty"`
	Code     Code   `json:"code"`
	Reason   string `json:"reason,omitempty"`
	Logs     string `json:"logs,omitempty"`
	LogError string `json:"logError,omitempty"`
}

// Verdict is the typed health gate for one workload: the signal that decides
// the state machine's Applied -> Healthy versus Applied -> Degraded transition
// (issue #53). Producing it is this package's reason to exist, and it is the
// M14 foundation: an automatic-rollback component branches on Code and Health
// without parsing prose.
type Verdict struct {
	// Healthy is true only when every polled signal agrees the workload is
	// live and well. It is the single boolean a health gate may act on.
	Healthy bool `json:"healthy"`
	// Stuck is set when a Tracker gave up waiting: the workload made no
	// progress before the timeout but was not failing. Stuck is deliberately
	// NOT a failure — Code stays a wait code (e.g. progressing) so a caller
	// branching on IsFailure never mistakes "stuck" for "broken".
	Stuck bool `json:"stuck,omitempty"`
	// Code is precise: empty/healthy on a green verdict, the specific reason
	// otherwise. Branch on this, never on Reason.
	Code Code `json:"code,omitempty"`
	// Reason is the human-readable detail: the kubelet reason, the
	// unschedulable message, the probe condition.
	Reason string `json:"reason,omitempty"`
	// Resource names the workload this verdict is about, e.g. "Deployment/prod/web".
	Resource string `json:"resource"`
	// Remediation is the fix stated as an action, one line per code.
	Remediation string `json:"remediation,omitempty"`
	// Containers carries one entry per failing container across the workload's
	// pods, so a diagnostic reads exactly which pod and container broke and,
	// when a LogSource was configured, that container's recent logs. Healthy
	// containers are implied by Healthy and omitted to keep the report focused.
	Containers []Container `json:"containers,omitempty"`
}

// String renders a one-line summary, e.g. "Deployment/prod/web degraded:
// crash-loop-back-off (back-off restarting failed container)".
func (v Verdict) String() string {
	if v.Healthy {
		return v.Resource + " healthy"
	}
	state := "progressing"
	if v.Stuck {
		state = "stuck"
	} else if IsFailure(v.Code) {
		state = "degraded"
	}
	s := v.Resource + " " + state + ": " + v.Code.String()
	if v.Reason != "" {
		s += " (" + v.Reason + ")"
	}
	return s
}
