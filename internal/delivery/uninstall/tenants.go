package uninstall

import (
	"context"
	"fmt"
	"sort"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"

	"github.com/dafrie/kelson/internal/delivery"
)

// managedByKelson selects everything kelson manages, of every project and every
// environment. It is the one query in this package that is deliberately not
// scoped to the uninstall's own (project, environment): the question it answers
// is "whose ELSE is in here", and a selector pinned to this scope cannot see
// anybody else by construction.
var managedByKelson = labels.SelectorFromSet(labels.Set{
	delivery.LabelManagedBy: delivery.ManagedByKelson,
}).String()

// tenancy is what a namespace holds for kelson deployments other than the one
// being uninstalled, plus how much of the namespace the question could actually
// be asked about.
type tenancy struct {
	// owners is every other "<project>/<environment>" found here, sorted.
	owners []string
	// count is how many resources those owners have here.
	count int
	// blind names the parts of the namespace the check could not read. It is
	// separate from count because "nobody else is here" and "kelson could not
	// tell" are different answers, and only one of them licenses a delete.
	blind []string
}

// clear reports whether the namespace was seen whole and holds nothing of
// anybody else's. Only a clear answer licenses deleting it.
func (t tenancy) clear() bool { return t.count == 0 && len(t.blind) == 0 }

// summary names what stayed and whose it is.
func (t tenancy) summary() string {
	return fmt.Sprintf("left behind: %d resource(s) of %s live here", t.count, strings.Join(t.owners, ", "))
}

// blindness names where the check could not look.
func (t tenancy) blindness() string {
	return "kelson could not tell whether another deployment lives here too (" + strings.Join(t.blind, "; ") + ")"
}

// reason is the preview's explanation for a namespace kelson created and is
// leaving standing anyway.
func (t tenancy) reason() string {
	const cascade = ", and deleting a namespace deletes everything inside it"
	switch {
	case t.count > 0 && len(t.blind) > 0:
		return "kelson created it, but it is not only kelson's any more — " + t.summary() +
			" — and " + t.blindness() + cascade
	case t.count > 0:
		return "kelson created it, but it is not only kelson's any more — " + t.summary() + cascade
	default:
		return "kelson created it, but " + t.blindness() + cascade
	}
}

// refusal is the per-object detail when the same finding arrives at delete time
// instead, where it is a race rather than a plan.
func (t tenancy) refusal() string {
	const cascade = ", and deleting a namespace deletes everything inside it"
	if t.count > 0 {
		return "another kelson deployment lives in this namespace — " + t.summary() + cascade
	}
	return t.blindness() + cascade
}

// otherTenants reports what in one namespace carries kelson's provenance for a
// (project, environment) other than the one being uninstalled.
//
// # Why the ownership annotation is not enough on its own
//
// A namespace belongs to an Environment, but its name is overridable
// (spec.namespace), so two projects — or two environments of one project — can
// be pointed at the same one. Apply already handles that by adopting rather than
// colonising, and the resource sweep handles it by being label-scoped. The
// namespace tier did not: it deleted on the strength of
// delivery.AnnNamespaceOwnership alone, which records who CREATED the namespace
// and knows nothing about who moved in afterwards. Uninstalling the creator
// then evicted the other tenant, data included (issue #215).
//
// # The boundary
//
// The query covers the kinds the sweep itself enumerates, and no others: a
// resource of a kind outside the Catalog is invisible here exactly as it is
// invisible to the sweep. Within those kinds the handle is
// app.kubernetes.io/managed-by=kelson — a resource labelled for another project
// but not managed by kelson is not another kelson deployment, and a namespace
// kelson created has always taken its unlabelled neighbours with it.
//
// # Which way it fails
//
// Toward leaving the namespace standing. Every failure to read — a forbidden
// kind, an API group discovery could not resolve — is recorded as blindness, and
// blindness alone is enough to keep the namespace: deleting one is the only act
// in this package that reaches resources the scope never selected, and it cannot
// be taken back. A namespace left behind costs one `kubectl delete namespace`;
// a tenant's database costs the database.
func (u *Uninstaller) otherTenants(ctx context.Context, scope Scope, namespace string, resources []APIResource, gaps []string) tenancy {
	t := tenancy{}
	for _, g := range gaps {
		t.blind = append(t.blind, "API group "+g+" could not be read")
	}

	owners := map[string]bool{}
	for _, res := range resources {
		gvk := res.GVR.GroupVersion().WithKind(res.Kind)
		if !swept(gvk.GroupKind()) {
			continue
		}
		list, err := u.client.Resource(res.GVR).Namespace(namespace).List(ctx, metav1.ListOptions{LabelSelector: managedByKelson})
		if err != nil {
			// A kind with no storage in this cluster can hold nobody's
			// resources, so it is not a hole in the answer. Everything else is.
			if apierrors.IsNotFound(err) {
				continue
			}
			why := err.Error()
			if unreadable, classified := readFailure(err); unreadable {
				why = classified
			}
			t.blind = append(t.blind, res.GVR.Resource+" could not be listed ("+why+")")
			continue
		}
		for i := range list.Items {
			lbls := list.Items[i].GetLabels()
			// The selector already answered the managed-by half; asking the
			// object's own labels again is what the sweep does for the same
			// reason, and here it is what keeps a namespace from being spared
			// on the strength of a resource that is not kelson's at all.
			if lbls[delivery.LabelManagedBy] != delivery.ManagedByKelson {
				continue
			}
			if lbls[delivery.LabelProject] == "" || scope.owns(lbls) {
				continue
			}
			t.count++
			owners[tenantOf(lbls)] = true
		}
	}

	t.owners = make([]string, 0, len(owners))
	for o := range owners {
		t.owners = append(t.owners, o)
	}
	sort.Strings(t.owners)
	return t
}

// namespaceTenants asks the same question with a catalog of its own, for the
// delete-time re-check where the plan's enumeration is not in hand. A catalog
// that cannot be read is blindness, which keeps the namespace.
func (u *Uninstaller) namespaceTenants(ctx context.Context, scope Scope, namespace string) tenancy {
	resources, gaps, err := u.catalog.Namespaced(ctx)
	if err != nil {
		return tenancy{blind: []string{"the cluster's API surface could not be read (" + err.Error() + ")"}}
	}
	return u.otherTenants(ctx, scope, namespace, resources, gaps)
}

// tenantOf names one resource's deployment the way the preview prints it.
func tenantOf(lbls map[string]string) string {
	project := lbls[delivery.LabelProject]
	if env := lbls[delivery.LabelEnvironment]; env != "" {
		return project + "/" + env
	}
	return project
}
