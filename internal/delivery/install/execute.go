package install

import (
	"context"
	"fmt"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/dafrie/kelson/internal/delivery"
)

// Outcome is what happened to one object.
type Outcome string

const (
	// OutcomeCreated: the object did not exist and kelson's apply created it.
	// It carries kelson.dev/component-ownership=created and uninstall may
	// remove it.
	OutcomeCreated Outcome = "created"
	// OutcomeAdopted: the object already existed. kelson applied over it and
	// stamped it adopted, so uninstall will leave it standing.
	OutcomeAdopted Outcome = "adopted"
	// OutcomeFailed: the API server refused the apply.
	OutcomeFailed Outcome = "failed"
)

// Result is one object's outcome.
type Result struct {
	Ref     Ref
	Outcome Outcome
	// Detail explains an outcome that is not a plain create.
	Detail string
}

// ComponentReport is what happened to one component.
type ComponentReport struct {
	Component Component
	Results   []Result
	Created   int
	Adopted   int
	Failed    int
	// Installed is true when every object of this component reached the
	// cluster. A component reported as not installed is the honest half of a
	// partial failure — re-running converges, and until then the component is
	// not there.
	Installed bool
}

// Report is what an execution did.
type Report struct {
	Components []ComponentReport
}

// Adopted returns the objects kelson applied over rather than created, across
// every component. They are the reason uninstall can be correct about ownership,
// and they are reported at install time so nobody is surprised later by a
// component that "kelson installed" leaving pieces behind.
func (r *Report) Adopted() []Result {
	var out []Result
	for _, c := range r.Components {
		for _, res := range c.Results {
			if res.Outcome == OutcomeAdopted {
				out = append(out, res)
			}
		}
	}
	return out
}

// Execute applies the plan, component by component, in the order the plan holds.
//
// Three rules, and they are the install-side mirror of what
// internal/delivery/uninstall does on the way out:
//
//   - Ownership is decided by a read taken immediately before each apply, and a
//     read that fails is fatal for that component. "Could not tell whether this
//     existed" must never resolve to "created", because created is the licence
//     to delete it later.
//   - A failed apply stops that component, not the run. The components after it
//     are independent installs and there is no reason a cluster that refuses
//     cert-manager's webhook configuration should also be denied CloudNativePG.
//     Every failure is collected and returned as one structured error, so the
//     exit code is honest.
//   - Everything is a server-side apply, never forced. Re-running converges:
//     the objects kelson already applied are unchanged, ownership annotations
//     already stamped are preserved (stampOwnership never rewrites one), and
//     what failed last time is retried.
func (i *Installer) Execute(ctx context.Context, plan *Plan) (*Report, error) {
	if plan == nil {
		return nil, delivery.ApplyFailed("(plan)", "",
			"no plan to execute",
			"call Plan() first; an install never applies anything it did not preview")
	}
	report := &Report{}
	var failures []string

	for _, item := range plan.Items {
		cr := i.installComponent(ctx, item)
		report.Components = append(report.Components, cr)
		if !cr.Installed {
			for _, res := range cr.Results {
				if res.Outcome == OutcomeFailed {
					failures = append(failures, item.Component.Name+": "+res.Ref.String()+": "+res.Detail)
				}
			}
		}
	}

	if len(failures) > 0 {
		return report, delivery.ApplyFailed(strings.Join(failedComponents(report), ", "), "",
			"the API server refused part of the install: "+strings.Join(failures, "; "),
			"nothing kelson applied is left in a state a re-run cannot fix — every apply is a server-side apply "+
				"and re-running `kelson install` converges. Check RBAC first: installing a platform component "+
				"needs create on cluster-scoped RBAC, CRDs and webhook configurations")
	}
	return report, nil
}

func failedComponents(r *Report) []string {
	var out []string
	for _, c := range r.Components {
		if !c.Installed {
			out = append(out, c.Component.Name)
		}
	}
	return out
}

// installComponent applies one component's objects in plan order.
func (i *Installer) installComponent(ctx context.Context, item Item) ComponentReport {
	cr := ComponentReport{Component: item.Component, Installed: true}
	for _, o := range item.Objects {
		// The FluxInstance is the one object whose CRD this same install just
		// registered. The API server needs a moment to serve it, and applying
		// into an unserved group returns a NotFound that reads like a missing
		// object rather than like a race.
		if o.GVR == fluxInstanceGVR {
			if err := i.awaitEstablished(ctx, fluxInstanceCRD); err != nil {
				cr.Results = append(cr.Results, Result{Ref: o.Ref, Outcome: OutcomeFailed, Detail: err.Error()})
				cr.Failed++
				cr.Installed = false
				return cr
			}
		}
		res := i.applyObject(ctx, o)
		cr.Results = append(cr.Results, res)
		switch res.Outcome {
		case OutcomeCreated:
			cr.Created++
		case OutcomeAdopted:
			cr.Adopted++
		case OutcomeFailed:
			cr.Failed++
			cr.Installed = false
			// Stop this component: the manifest's document order is a
			// dependency order, and applying a Deployment whose ServiceAccount
			// was refused produces a second, more confusing failure.
			return cr
		}
	}
	return cr
}

// applyObject stamps ownership and server-side applies one object.
func (i *Installer) applyObject(ctx context.Context, o Object) Result {
	client := i.client.Resource(o.GVR).Namespace(o.Ref.Namespace)

	ownership, err := i.stampOwnership(ctx, o)
	if err != nil {
		return Result{Ref: o.Ref, Outcome: OutcomeFailed, Detail: err.Error()}
	}

	if _, err := client.Apply(ctx, o.Ref.Name, o.obj, metav1.ApplyOptions{
		FieldManager: i.manager,
		// Never Force. A field another manager owns is the cluster telling
		// kelson that something else installed part of this component, which is
		// exactly the situation "never modify what kelson did not install"
		// exists for.
		Force: false,
	}); err != nil {
		return Result{Ref: o.Ref, Outcome: OutcomeFailed, Detail: applyDetail(err)}
	}

	if ownership == OwnershipAdopted {
		return Result{Ref: o.Ref, Outcome: OutcomeAdopted,
			Detail: "it was already there, so kelson applied over it and will never delete it"}
	}
	return Result{Ref: o.Ref, Outcome: OutcomeCreated}
}

// stampOwnership decides created-versus-adopted and writes the annotation onto
// the object about to be applied.
//
// This is the whole ownership claim, and it is observable exactly once — at the
// moment of the apply, by the process performing it. The same reasoning as
// direct.stampNamespaceOwnership, applied per object because a platform
// component is a set of objects and a user may already have some of them: a
// hand-made flux-system namespace, a ClusterRole left over from a previous
// install, a Service somebody recreated.
//
// An existing object that ALREADY carries an ownership value keeps it. A
// re-run of an install that created an object must not downgrade it to adopted
// just because it now exists — the annotation records who brought it into the
// world, not who wrote to it last.
func (i *Installer) stampOwnership(ctx context.Context, o Object) (string, error) {
	client := i.client.Resource(o.GVR).Namespace(o.Ref.Namespace)
	live, err := client.Get(ctx, o.Ref.Name, metav1.GetOptions{})

	ownership := OwnershipCreated
	switch {
	case err == nil:
		ownership = OwnershipAdopted
		if prior := live.GetAnnotations()[AnnOwnership]; prior != "" {
			ownership = prior
		}
	case apierrors.IsNotFound(err):
		// Absent: this apply creates it.
	default:
		return "", delivery.ApplyFailed(o.Ref.String(), "",
			"could not be read before applying: "+err.Error(),
			"kelson decides whether it created an object by reading it first, and an install that cannot tell "+
				"must not claim it — grant get on this kind, or install the component yourself and let "+
				"detection adopt it")
	}

	ann := o.obj.GetAnnotations()
	if ann == nil {
		ann = map[string]string{}
	}
	ann[AnnOwnership] = ownership
	o.obj.SetAnnotations(ann)
	return ownership, nil
}

// awaitEstablished waits for a CRD this install just applied to be served.
//
// A negative EstablishTimeout skips the wait entirely, which is what a caller
// that has its own readiness handling asks for.
func (i *Installer) awaitEstablished(ctx context.Context, name string) error {
	if i.timeout < 0 {
		return nil
	}
	deadline := time.Now().Add(i.timeout)
	for {
		crd, err := i.client.Resource(crdGVR).Get(ctx, name, metav1.GetOptions{})
		switch {
		case err == nil && established(crd):
			return nil
		case err != nil && !apierrors.IsNotFound(err):
			return delivery.ApplyFailed(name, "",
				"reading the CustomResourceDefinition failed: "+err.Error(),
				"kelson waits for the CRD it just applied to be served before creating a resource of that kind")
		}
		if time.Now().After(deadline) {
			return delivery.ApplyFailed(name, "",
				fmt.Sprintf("the CustomResourceDefinition was not established within %s", i.timeout),
				"the operator's own manifest applied, so re-running `kelson install` will finish the job once "+
					"the API server serves the CRD; nothing is in a broken state")
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(i.poll):
		}
	}
}

// established reads the CRD condition the API server sets once the kind is
// actually servable.
func established(crd *unstructured.Unstructured) bool {
	conditions, found, err := unstructured.NestedSlice(crd.Object, "status", "conditions")
	if err != nil || !found {
		return false
	}
	for _, raw := range conditions {
		cond, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		if cond["type"] == "Established" && cond["status"] == "True" {
			return true
		}
	}
	return false
}

// applyDetail keeps a field-manager conflict readable. The API server's message
// is long and structured; the part that matters is that somebody else owns the
// field, which means somebody else installed part of this component.
func applyDetail(err error) string {
	if apierrors.IsConflict(err) {
		return "another field manager owns part of this object, so something else installed or manages it: " +
			err.Error()
	}
	return err.Error()
}
