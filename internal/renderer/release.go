package renderer

import (
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/dafrie/kelson/internal/model"
)

// Release commands, rendered as the Job that must succeed before a revision's
// workloads roll (issue #104, ADR-0019).
//
// # What it is
//
// `release:` on a workload component is the migration hook: a command run to
// completion, with that component's image, environment, bindings and identity,
// between "the data services exist" and "the new pods roll". It is Heroku's
// release phase with Kubernetes' vocabulary — and, like Heroku's, it is the
// component's own command rather than a separate deployable, because everything
// it needs is already the component's.
//
// # Where it sits in the set, and what that does and does not buy
//
// The Job is emitted after the data services and the charts and before every
// workload, because order is the only sequencing a rendered set can express
// (issue #89). Order alone does not *wait*: applying a Job before a Deployment
// says nothing about the Job having finished. The waiting is the delivery
// plane's, which is why this field renders in direct mode only — see the gate
// below, and internal/delivery/direct/release.go for the wait itself.
//
// The component's ServiceAccount travels with the Job rather than staying with
// the rest of its resources. It has to: the pod names it, and a Job applied
// ahead of the ServiceAccount it references produces a pod the API server
// refuses to create until the SA appears — which, in a delivery mode that waits
// for the Job before applying anything else, is a deadlock rather than a delay.
// componentManifests therefore skips the ServiceAccount for a component with a
// release hook, and it is emitted here instead. One resource, moved, never
// duplicated.
//
// # The direct-only gate
//
// Every other delivery-mode gate in the renderer points at Flux (ADR-0016
// decision 4 for charts, ADR-0017 for previews). This one points the other way,
// and the asymmetry is the honest reading of what each mode can express: kelson
// owns the apply in direct mode and can therefore stop between two resources,
// whereas in Flux mode it writes files into a path somebody else's Kustomization
// reconciles — one apply, no ordering barrier, no health gate kelson controls.
// Rendering the Job into a Flux path would produce migrations that run *beside*
// the rollout instead of before it, which is the quiet half-success this project
// refuses to ship (issue #141). So it is refused, and the refusal names the gap.
//
// The gate is decided here, from spec data, exactly like its two siblings: the
// delivery mode is resolved from the Environment document (P4) and arrives on
// model.Resolved, so the same document renders the same way against every
// cluster.

const (
	// releaseContainer is the name of the release Job's one container. It is a
	// fixed word rather than the component's name because the delivery plane
	// streams logs from it by name, and because "release" is what the pod is:
	// the component's image running the component's release command.
	releaseContainer = "release"

	// releaseBackoffLimit is how many times Kubernetes re-creates the pod after
	// a failure before failing the Job.
	//
	// It is small but not zero, and the two is a considered number. Zero would
	// fail a first deploy for a reason that has nothing to do with the
	// migration: a data service applied moments earlier is not accepting
	// connections yet, and the pod's first attempt loses that race. Kubernetes
	// backs off 10s then 20s between attempts, which absorbs that window
	// without turning a genuinely broken migration into a long wait — a broken
	// one fails three times fast and the deploy stops.
	releaseBackoffLimit = 2

	// releaseHookLabel marks the Job as the release hook of its revision. The
	// delivery plane finds it by this label rather than by name (it never parses
	// a rendered name), and an operator reading a namespace can tell a migration
	// from a workload without reading the pod spec.
	releaseHookLabel = "kelson.dev/release-hook"
)

// maxReleaseJobName is the DNS-1123 ceiling a Job name has to fit under. The
// name is release-<component>-<8 hex of the spec hash>, and the component name
// is itself allowed 63 characters, so the combination can overflow. Refusing at
// render time names the field; truncating would silently make two components'
// Jobs collide, which is a migration running under another component's identity.
const maxReleaseJobName = 63

// releaseRequiresDirect is the delivery-mode gate. It reports every component
// with a release hook, not just the first: one run should list all the work.
//
// It is called from Render alongside helmRequiresFlux and previewsRequireFlux,
// before anything is emitted, so a spec that cannot render produces errors
// rather than a partial manifest set.
func releaseRequiresDirect(resolved *model.Resolved) Errors {
	if resolved.Environment.Mode == model.DeliveryDirect {
		return nil
	}
	var errs Errors
	for i := range resolved.Components {
		c := &resolved.Components[i]
		if c.Release == nil {
			continue
		}
		errs = append(errs, Error{
			Code:        ErrReleaseRequiresDirect,
			Application: c.Name,
			Message: "component " + quoted(c.Name) + " declares a release command, which must finish before this " +
				"revision's workloads roll, but environment " + quoted(resolved.Environment.Name) +
				" has delivery mode " + quoted(string(resolved.Environment.Mode)),
			Remediation: "set delivery.mode: direct on this environment, or remove the release hook and run the " +
				"migration yourself. kelson refuses rather than rendering the Job here on purpose (ADR-0019): in " +
				"Flux mode the manifests are applied by a Kustomization kelson does not own, in one apply and with " +
				"no barrier between the Job and the Deployments — the migration would run beside the rollout " +
				"instead of before it, and a failed one would not stop it",
		})
	}
	return errs
}

// releaseManifests renders one component's release hook: its ServiceAccount,
// then the Job that runs under it. The ServiceAccount comes first because the
// Job's pod names it, and a rendered set expresses sequencing only through
// order (issue #89).
func releaseManifests(
	resolved *model.Resolved,
	c *model.ResolvedComponent,
	services map[string]boundService,
) ([]Manifest, error) {
	hash, err := specHash(resolved, c)
	if err != nil {
		return nil, Errors{{Code: ErrInternal, Message: err.Error()}}
	}
	env, berrs := envList(c, services)
	if len(berrs) > 0 {
		return nil, berrs
	}

	name := releaseJobName(c.Name, hash)
	if len(name) > maxReleaseJobName {
		return nil, Errors{{
			Code:        ErrReleaseName,
			Application: c.Name,
			Message: "the release Job of component " + quoted(c.Name) + " would be named " + quoted(name) +
				", which is longer than the 63 characters a Kubernetes object name allows",
			Remediation: "shorten the component name to at most " +
				strconv.Itoa(maxReleaseJobName-len(releaseJobName("", hash))) +
				" characters; the Job's name carries the component and the spec hash so that re-applying a " +
				"revision finds the Job it already ran",
		}}
	}

	prov := provenance{
		project:     resolved.Project,
		environment: resolved.Environment.Name,
		application: c.Name,
		namespace:   resolved.Environment.Namespace,
		specHash:    hash,
	}
	jobProv := prov
	jobProv.resourceName = name
	jobProv.extraLabels = []string{releaseHookLabel, "true"}

	spec := mapNode(
		"backoffLimit", releaseBackoffLimit,
		"activeDeadlineSeconds", c.Release.TimeoutSeconds,
		"template", mapNode(
			"metadata", mapNode("labels", jobProv.labels()),
			"spec", mapNode(
				"serviceAccountName", c.Name,
				"restartPolicy", "Never",
				"containers", seqNode(releaseContainerNode(c, env)),
			),
		),
	)
	return []Manifest{
		serviceAccount(prov),
		baseManifest("batch/v1", "Job", jobProv, spec),
	}, nil
}

// releaseJobName is the Job's identity: the component it belongs to and the
// spec hash of the revision it runs for.
//
// The hash is what makes the Job idempotent-safe in the way a release command
// has to be. Re-applying an unchanged revision addresses the Job that already
// ran and finds it complete, so the migration does not run twice for one spec;
// a changed revision has a different hash and therefore a different Job, so a
// new deploy does run it. That is the same trick the build plane's Job names
// play with the build request (internal/build/buildkit/workload.go), for the
// same reason: the name IS the idempotency key.
//
// The delivery plane never parses this name — it finds the Job by the
// release-hook label — so the format stays a rendering detail.
func releaseJobName(component, specHash string) string {
	return "release-" + component + "-" + shortHash(specHash)
}

// shortHash is the leading 8 hex characters of a kelson.dev/spec-hash, the same
// length the build plane truncates a revision to. Eight hex characters is 4
// bytes of collision resistance against a set of Jobs that is one per revision
// of one component, which is not a set collisions happen in.
func shortHash(specHash string) string {
	h := strings.TrimPrefix(specHash, "sha256:")
	if len(h) > 8 {
		return h[:8]
	}
	return h
}

// releaseContainerNode is the Job's container: the component's image and
// resources, the release command, and the component's environment — the same
// env list its Deployment gets, bindings and secret references included, so a
// migration reads the database URL the application reads.
//
// It deliberately has no ports and no probes. A release command is not serving
// anything, and a liveness probe on a migration would be a way to kill it
// halfway.
func releaseContainerNode(c *model.ResolvedComponent, env *yaml.Node) *yaml.Node {
	kv := []any{
		"name", releaseContainer,
		"image", c.Image,
		"command", asNode(c.Release.Command),
	}
	if env != nil {
		kv = append(kv, "env", env)
	}
	if r := resourcesNode(c.Resources); r != nil {
		kv = append(kv, "resources", r)
	}
	return mapNode(kv...)
}
