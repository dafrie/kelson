// Package observation is the OBSERVATION plane (docs/architecture.md).
//
// It watches live resources, correlates them to the application model via
// kelson.dev provenance labels, and reports status, logs and metrics. This
// package is the health gate that populates the deployment state machine's
// "Applied -> Healthy" and "Applied -> Degraded" decisions with real signal
// (issue #53): the state machine owns the phases; observation owns the verdict.
//
// The core is a typed Verdict (Code, Resource, Reason, Remediation) computed
// by a Probe from a live Deployment and the pods in its selector set. The
// verdict is precise where the kubelet names a reason (CrashLoopBackOff,
// ImagePullBackOff) and heuristic where it must infer (a running-but-not-ready
// container is treated as a failing probe; an unschedulable pod splits into
// insufficient-resources versus scheduling-failed by reading the message). A
// Tracker wraps a Probe with a configurable timeout that reports Stuck — a
// state distinct from failed, never reported as one.
//
// Everything talks through injected clients the way the delivery adapters do
// (dynamic for the health signal, typed for logs), so no test needs a cluster.
//
// The log query engine (LogQuery) is the transport-agnostic layer a later API
// (#69) will wrap: a bounded, structured, deterministically-ordered query over
// an application's replicas, or a live follow — the "last N lines around this
// failure" the issue #54 design note asks for. It carries no transport types.
package observation
