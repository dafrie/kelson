package dryrun

import (
	"errors"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// Ordering handling (issue #43): a resource whose prerequisite does not exist
// yet — a CR before its CRD, anything before its Namespace — fails dry-run for
// an uninteresting reason. Because lists arrive in the renderer's order
// (namespaces first, CRDs before CRs), the prerequisite is only being created
// in the same batch. We recognise these and report them as ordering findings
// rather than policy failures, and never silently swallow them.

// batchInfo describes the set of resources being dry-run together.
type batchInfo struct {
	// namespaces is every namespace the batch will create or address.
	namespaces map[string]bool
	// crds is every GroupKind the batch's CustomResourceDefinitions register,
	// so a "no matches for kind" on one of them is clearly an ordering artifact.
	crds map[schema.GroupKind]bool
}

func namespacesOf(targets []target) map[string]bool {
	ns := map[string]bool{}
	for _, t := range targets {
		if t.obj.GetKind() == "Namespace" {
			ns[t.obj.GetName()] = true
			continue
		}
		if t.ref.Namespace != "" {
			ns[t.ref.Namespace] = true
		}
	}
	return ns
}

func crdsOf(targets []target) map[schema.GroupKind]bool {
	out := map[schema.GroupKind]bool{}
	for _, t := range targets {
		if t.obj.GetKind() != "CustomResourceDefinition" {
			continue
		}
		group, _, _ := unstructured.NestedString(t.obj.Object, "spec", "group")
		kind, _, _ := unstructured.NestedString(t.obj.Object, "spec", "names", "kind")
		if group == "" || kind == "" {
			continue
		}
		out[schema.GroupKind{Group: group, Kind: kind}] = true
	}
	return out
}

// prerequisitePending reports whether a NotFound error is explained by a
// prerequisite being created in the same batch, rather than a genuine absence.
func (b batchInfo) prerequisitePending(t target, err error) bool {
	var status apierrors.APIStatus
	if !errors.As(err, &status) || !apierrors.IsNotFound(err) {
		return false
	}
	msg := status.Status().Message
	ns := t.ref.Namespace

	// Namespace not yet created, but the batch declares it.
	if ns != "" && b.namespaces[ns] &&
		(strings.Contains(msg, `"`+ns+`" not found`) ||
			(strings.Contains(msg, "namespace") && strings.Contains(msg, "not found"))) {
		return true
	}

	// CRD not yet registered, but the batch declares that exact GroupKind.
	if b.crds[t.obj.GroupVersionKind().GroupKind()] &&
		strings.Contains(msg, "no matches for kind") {
		return true
	}
	return false
}
