package renderer

import (
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/dafrie/kelson/internal/model"
)

// Release commands, rendered as the Job that must succeed before a revision's
// workloads roll (issue #104, ADR-0019, rebuilt on the spine by issue #227).
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
// # Where it sits in the set, and where the barrier actually lives
//
// The Job is emitted after the data services and the charts and before every
// workload, exactly as ADR-0019 decision 2 says, because order is the only
// sequencing a rendered set can express (issue #89). Order alone does not
// *wait*: applying a Job before a Deployment says nothing about the Job having
// finished. That is why ADR-0019 gated the field to the one delivery mode that
// performed its own apply, and why ADR-0028 deleting that mode turned the field
// into a validated refusal.
//
// What replaces it is not a rendering trick. The renderer marks this Job
// [StageRelease] and everything it depends on [StagePrerequisite], and the
// *controller* turns that partition into two Flux Kustomizations with a
// `dependsOn` between them: the first applies the release stage and health-gates
// on the Job completing, the second applies the workloads and cannot start until
// the first is Ready (internal/controller/fluxobjects.go, ADR-0028 decision 3).
// The barrier is kustomize-controller's, kelson only says where it goes — which
// is the division of labour ADR-0029 keeps this package on the right side of.
//
// The component's ServiceAccount travels with the Job rather than staying with
// the rest of its resources. It has to: the pod names it, and a Job applied
// ahead of the ServiceAccount it references produces a pod the API server
// refuses to create until the SA appears — which, in a stage that waits for the
// Job, is a deadlock rather than a delay. [componentManifests] therefore skips
// the ServiceAccount for a component with a release hook and it is emitted here
// instead. One resource, moved, never duplicated *in the set* — the delivery
// plane writes it into both stages because both name it, which is what
// [StagePrerequisite] is for.

const (
	// releaseContainer is the name of the release Job's one container. It is a
	// fixed word rather than the component's name because a reader (and a
	// `kubectl logs -c`) wants one name to know, and because "release" is what
	// the pod is: the component's image running the component's release command.
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
	//
	// The race it absorbs is smaller than it was: the release stage carries the
	// data services and waits for them to be healthy before it reports Ready,
	// so the first attempt now usually finds a database that is accepting
	// connections. It stays because "usually" is not "always".
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

// releaseManifests renders one component's release hook: its ServiceAccount,
// then the Job that runs under it. The ServiceAccount comes first because the
// Job's pod names it, and a rendered set expresses sequencing only through
// order (issue #89).
//
// The two carry different stages. The ServiceAccount is a prerequisite — both
// the Job and the component's own Deployment name it, so both stages apply it
// and the workload stage keeps owning it. The Job is the release stage alone:
// it must never reach the Kustomization that applies the workloads, because a
// Job applied beside a Deployment is the migration-runs-during-the-rollout
// failure this whole design exists to prevent.
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
			Code:      ErrReleaseName,
			Component: c.Name,
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
		component:   c.Name,
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

	account := serviceAccount(prov)
	account.Stage = StagePrerequisite
	job := baseManifest("batch/v1", "Job", jobProv, spec)
	job.Stage = StageRelease
	return []Manifest{account, job}, nil
}

// releaseJobName is the Job's identity: the component it belongs to and the
// spec hash of the revision it runs for.
//
// The hash is what makes the Job idempotent-safe in the way a release command
// has to be, and on the spine it does more work than it did in direct mode.
// Re-applying an unchanged revision addresses the Job that already ran and
// finds it complete, so the migration does not run twice for one spec; a
// changed revision has a different hash and therefore a different Job, so a new
// deploy does run it. Because the release stage never prunes
// (internal/controller/fluxobjects.go), a *rollback* also addresses the Job that
// already ran rather than a fresh one — which is how ADR-0019 decision 6's "a
// rollback does not re-run the release command" survives a delivery plane that
// re-applies whatever the artifact holds.
//
// That is the same trick the build plane's Job names play with the build request
// (internal/build/buildkit/workload.go), for the same reason: the name IS the
// idempotency key.
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

// ReleaseTimeoutSeconds is the longest a release stage may take: the largest
// activeDeadlineSeconds any of its Jobs carries, or zero when the spec declares
// no hook.
//
// It is here rather than in the delivery plane because it is a fact about what
// was rendered, and it exists because the Kustomization that waits for these
// Jobs needs a deadline of its own. A Flux Kustomization's health wait is
// bounded by `spec.timeout`, which defaults to its interval — five minutes,
// which is *shorter* than the ten-minute default a release command gets. Left
// alone, a perfectly healthy fifteen-minute migration would be reported as a
// failed reconciliation twelve times before it finished.
func ReleaseTimeoutSeconds(resolved *model.Resolved) int {
	longest := 0
	for i := range resolved.Components {
		r := resolved.Components[i].Release
		if r != nil && r.TimeoutSeconds > longest {
			longest = r.TimeoutSeconds
		}
	}
	return longest
}
