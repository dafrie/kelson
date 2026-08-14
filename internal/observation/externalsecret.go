package observation

import (
	"context"
	"fmt"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// Secret sync, read back from the ExternalSecret the renderer wrote (issue
// #80, ADR-0020).
//
// # Why this is here and not left to the pods
//
// A secret that fails to sync is the most invisible failure in the whole
// backend. Nothing about it is loud: the ExternalSecret applies cleanly, the
// Deployment applies cleanly, and the only symptom is a pod stuck in
// CreateContainerConfigError against a Secret that was never created — or,
// worse, a pod running happily on the *last* value the store returned before
// the credential was rotated out from under it. ADR-0018 listed "the cluster
// tells you" as an accepted negative and named this plane as where it stops
// being one; #80's acceptance criterion makes it explicit: a failed sync must
// surface as a component-level problem with the cause named.
//
// So the verdict here is an ordinary [Verdict], in the same list as the
// workload verdicts, carrying the controller's own reason and message. It
// reaches `kelson status`, the Status RPC, the event stream and
// diagnose_component without any of them learning a new shape.
//
// # What is read, and what is deliberately not
//
// Only the ExternalSecret, and only its `status.conditions`. Not the Secret it
// produces — kelson has no reason to read a Secret and does not (ADR-0009,
// ADR-0018 §4), and a probe that fetched one to check whether the keys arrived
// would be the first thing in kelson ever to hold a credential. The
// ExternalSecret's own status answers the question anyway: external-secrets
// sets Ready=False with a reason when it cannot write the Secret, and the
// reason names the cause.
//
// The condition's `message` is relayed verbatim, as the kubelet's reasons
// already are. It is controller output describing a failure to *reach* a value
// — a store that refused authentication, a remote key that does not exist —
// and external-secrets does not put secret material in it. What kelson would
// never relay is the Secret's data, and nothing in this file can reach it.

// externalSecretGVR is the resource the renderer writes and this reads. One API
// version per group across kelson: it matches internal/renderer's
// externalSecretAPIVersion and detection's store coordinates.
var externalSecretGVR = schema.GroupVersionResource{
	Group:    "external-secrets.io",
	Version:  "v1",
	Resource: "externalsecrets",
}

// externalSecretReadyCondition is the condition type external-secrets sets on
// every ExternalSecret it reconciles. Its reasons — SecretSynced,
// SecretSyncedError, SecretDeleted, SecretMissing — are relayed rather than
// re-encoded: kelson's own vocabulary says whether the sync failed, and the
// controller's says why.
const externalSecretReadyCondition = "Ready"

// SecretSyncEvaluator computes the sync verdict for one ExternalSecret. It is
// separate from [Evaluator] rather than folded into it because the two answer
// different questions about different resources, and because a caller that has
// only a workload evaluator must keep working: the wiring in `kelson status`
// and the API asks for this interface and degrades to no sync verdicts when the
// evaluator it holds does not implement it.
type SecretSyncEvaluator interface {
	EvaluateSecretSync(ctx context.Context, namespace, name string) (Verdict, error)
}

// EvaluateSecretSync computes the verdict for one ExternalSecret by
// name/namespace. [Probe] implements [SecretSyncEvaluator] with it, through the
// same injected dynamic client the workload verdicts use — no second client, no
// second connection, and no cluster needed in a test.
func (p *Probe) EvaluateSecretSync(ctx context.Context, namespace, name string) (Verdict, error) {
	resource := res(externalSecretGVR.Group, "ExternalSecret", namespace, name)
	es, err := p.client.Resource(externalSecretGVR).Namespace(namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) {
			return Verdict{
				Healthy:  false,
				Code:     CodeMissing,
				Reason:   fmt.Sprintf("%s/%s is not in the cluster", namespace, name),
				Resource: resource,
				// CodeMissing's own remediation is about a workload that was
				// never applied. For an ExternalSecret the shape is the same
				// and so is the fix, so it is reused rather than duplicated.
				Remediation: CodeMissing.remediation(),
			}, nil
		}
		return Verdict{}, fmt.Errorf("observation: reading externalsecret %s/%s: %w", namespace, name, err)
	}
	return classifyExternalSecret(es, resource), nil
}

// classifyExternalSecret reduces an ExternalSecret's Ready condition to a
// verdict. It is pure and deterministic, like classify: the same object always
// produces the same answer.
//
// Three states, and the third is the point. Ready=True is a synced Secret.
// Ready=False is a definite failure with a cause. Anything else — Ready=Unknown,
// or no Ready condition at all because the controller has not reached this
// object yet — is Progressing, never a failure: a resource applied a second ago
// has not failed, it has not been looked at, and reporting that as broken would
// make every deploy flash red on its way to green.
func classifyExternalSecret(es *unstructured.Unstructured, resource string) Verdict {
	cond, found := readyCondition(es)
	if !found {
		return Verdict{
			Healthy:  false,
			Code:     CodeProgressing,
			Reason:   "external-secrets has not reported on this ExternalSecret yet",
			Resource: resource,
		}
	}
	switch cond.status {
	case "True":
		return Verdict{Healthy: true, Code: CodeHealthy, Resource: resource}
	case "False":
		return Verdict{
			Healthy:     false,
			Code:        CodeSecretSyncFailed,
			Reason:      syncReason(cond),
			Resource:    resource,
			Remediation: CodeSecretSyncFailed.remediation(),
		}
	default:
		return Verdict{
			Healthy:  false,
			Code:     CodeProgressing,
			Reason:   syncReason(cond),
			Resource: resource,
		}
	}
}

// condition is the slice of a status condition this package reads.
type condition struct {
	status  string
	reason  string
	message string
}

// readyCondition finds the Ready condition on an ExternalSecret's status.
func readyCondition(es *unstructured.Unstructured) (condition, bool) {
	conds, _, _ := unstructured.NestedSlice(es.Object, "status", "conditions")
	for _, raw := range conds {
		c, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		typ, _ := c["type"].(string)
		if typ != externalSecretReadyCondition {
			continue
		}
		status, _ := c["status"].(string)
		reason, _ := c["reason"].(string)
		message, _ := c["message"].(string)
		return condition{status: status, reason: reason, message: message}, true
	}
	return condition{}, false
}

// syncReason composes the controller's reason and message into one line. Both
// are relayed rather than summarised: the reason is the machine-readable token
// (SecretSyncedError, SecretMissing) and the message is the part that names the
// store, the key or the permission that actually failed, which is the whole
// point of surfacing this at all.
func syncReason(c condition) string {
	switch {
	case c.reason != "" && c.message != "":
		return c.reason + ": " + c.message
	case c.message != "":
		return c.message
	case c.reason != "":
		return c.reason
	}
	return "external-secrets reported " + externalSecretReadyCondition + "=" + orUnknown(c.status)
}

func orUnknown(s string) string {
	if strings.TrimSpace(s) == "" {
		return "Unknown"
	}
	return s
}
