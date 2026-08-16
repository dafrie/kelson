package renderer

import (
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/dafrie/kelson/internal/model"
	"github.com/dafrie/kelson/internal/preview/naming"
)

// PR previews, rendered as the two flux-operator resources that own the
// per-pull-request lifecycle and nothing else (ADR-0017, ADR-0016 decision 5).
//
// # The division of labour, and why it is the whole design
//
// An Environment with `previews:` renders a ResourceSetInputProvider — which
// polls the forge and turns each open change request into an input — and a
// ResourceSet, whose template instantiates an OCIRepository and a Kustomization
// per input. That template contains no component manifests. It cannot: the
// manifests for a preview are rendered concretely, by this same renderer, and
// published as an OCI artifact the Kustomization applies.
//
// That boundary is the entire reason ADR-0016 chose artifact-per-PR over
// templating. kelson's output must never carry a hole another controller fills
// in, because the diff, the dry-run and every golden file rest on the output
// being fully evaluated. The four templated holes here — `<< inputs.id >>` in
// three names and a namespace, `<< inputs.sha >>` in one tag — are addresses of
// artifacts, not descriptions of workloads. If a Deployment ever needs to
// appear in resourcesTemplate, the decision has failed rather than grown.
//
// # Why the tag is the SHA
//
// A tag of `pr-<id>` would be mutable by construction: updating a preview means
// repointing it, which discards the artifact history for that change request
// and makes "what is running in preview 412" depend on when you ask. The head
// commit is immutable, one artifact per push, and it is already what CI tags
// the image with — so a preview's manifests and its image are addressed by the
// same string. When the input's sha changes the OCIRepository object changes,
// so source-controller fetches immediately rather than on its interval.
//
// # The Flux-only gate
//
// A ResourceSet applied where flux-operator does not run is worse than the inert
// HelmRelease the same gate refuses for charts: the CRD is usually not served at
// all, so a direct-mode apply fails on an unknown kind with a message about
// fluxcd.controlplane.io/v1 that says nothing about previews. The gate is
// decided here, from spec data, so the same document renders the same way
// against every cluster; whether flux-operator is *installed* is a ClusterProfile
// finding (issue #157), never a rendering decision.
//
// # Who else knows this scheme
//
// The namespace this template writes, the tag it pins and the cap it enforces
// are not local decisions: the publisher (internal/preview) has to render into
// that namespace and push to that tag, or flux-operator creates an
// OCIRepository pointing at an artifact nobody will ever push. Both sides call
// internal/preview/naming, which is why ADR-0017's "agree by convention"
// negative is now "agree by shared code". Nothing in this file spells a preview
// name itself.

const (
	// resourceSetAPIVersion is flux-operator's API. kelson writes it and never
	// imports it: flux-operator is AGPL-3.0 and kelson is MIT, so this group is
	// known as YAML and as unstructured reads only (docs/architecture.md,
	// "Living with flux-operator").
	resourceSetAPIVersion = "fluxcd.controlplane.io/v1"
	// kustomizationAPIVersion is kustomize-controller's stable API, matching
	// the version the delivery plane already reads Kustomization status from
	// (internal/delivery/flux/dynamic.go).
	kustomizationAPIVersion = "kustomize.toolkit.fluxcd.io/v1"
)

// annReconcileEvery is how often flux-operator calls the forge. kelson writes
// it from the spec rather than leaving the operator's default implicit, so the
// polling rate a cluster will actually use is visible in the manifest.
const annReconcileEvery = "fluxcd.controlplane.io/reconcileEvery"

// previewInterval is the reconcile interval of the two resources the template
// generates. Ten minutes is drift correction, not update latency: a push moves
// the input's sha, which rewrites the OCIRepository's tag, which source-controller
// acts on at once.
const previewInterval = "10m"

// previewArtifactPath is the directory inside the artifact the Kustomization
// builds. kelson's rendered output is a flat manifest set at the artifact root
// — or, when the set carries a release hook, a root that lists only the
// workload stage in a generated kustomization.yaml (internal/artifact).
const previewArtifactPath = "./"

// previewReleasePath is where the release stage lands inside an artifact.
//
// The authority for the directory name is [delivery.ReleaseStageDir], which is
// what the publisher writes and what the controller's own Kustomization points
// at; this package may not import the delivery plane, so the string is repeated
// here and TestPreviewReleasePathMatchesTheArtifactLayout
// (internal/controller/release_test.go) is what keeps the two from drifting.
const previewReleasePath = "./release"

// releasePreviewName is the release Kustomization of one preview. The suffix is
// the same one the environment's own pair uses (internal/controller's
// ReleaseObjectName), for the reason every other name in this file is shared:
// an operator should not have to learn two spellings of the same idea.
func releasePreviewName(child string) string { return child + "-release" }

// previewProviderType maps kelson's forge enum onto flux-operator's provider
// types. The enum is two wide on purpose (ADR-0017): the operator also speaks
// Azure DevOps, Gitea and AWS CodeCommit, and each is one line here, but an
// enum value is a claim that the shape has been run.
func previewProviderType(p model.PreviewProvider) string {
	switch p {
	case model.PreviewGitLab:
		return "GitLabMergeRequest"
	default:
		return "GitHubPullRequest"
	}
}

// The delivery-mode gate ADR-0017 decision 5 took from ADR-0016 —
// `render/previews-require-flux`, exported so the API's ListPreviews could state
// the same refusal without rendering — is deleted (ADR-0028 decision 8). It
// refused every mode that was not flux, and flux is the only mode there has ever
// been since. An environment that declares previews renders them.
//
// previewsManifests renders the ResourceSetInputProvider and the ResourceSet,
// in that order: the ResourceSet names the provider, and a rendered set
// expresses sequencing only through order (issue #89).
func previewsManifests(resolved *model.Resolved) ([]Manifest, error) {
	previews := resolved.Environment.Previews
	base := naming.Base(resolved.Project, resolved.Environment.Name)
	// Refusing here costs nothing and points at the field. The alternative is
	// flux-operator templating a name the API server rejects at reconcile time,
	// which surfaces as a ResourceSet condition far from the spec that caused it.
	if naming.BaseTooLong(base) {
		return nil, Errors{{
			Code: ErrPreviewName,
			Message: "previews name every child " + quoted(base+"-"+naming.Infix+"<change request>") +
				", and " + quoted(base) + " is already " + strconv.Itoa(len(base)) + " characters",
			Remediation: "shorten the project or environment name so that <project>-<environment> is at most " +
				strconv.Itoa(naming.MaxBase) + " characters. A preview's name is also its namespace, which is a " +
				"DNS-1123 label capped at 63, and kelson reserves " + strconv.Itoa(naming.IDDigits) +
				" digits for the change request number",
		}}
	}
	name := naming.Lifecycle(resolved.Project, resolved.Environment.Name)

	hash, err := previewsHash(resolved, previews, name)
	if err != nil {
		return nil, Errors{{Code: ErrInternal, Message: err.Error()}}
	}
	prov := provenance{
		project:      resolved.Project,
		environment:  resolved.Environment.Name,
		resourceName: name,
		namespace:    resolved.Environment.Namespace,
		specHash:     hash,
	}
	return []Manifest{
		previewInputProvider(previews, prov),
		previewResourceSet(resolved, previews, prov),
	}, nil
}

// previewInputProvider renders the poller: which forge, which repository, which
// change requests, and how often to ask.
func previewInputProvider(previews *model.ResolvedPreviews, prov provenance) Manifest {
	specKV := []any{
		"type", previewProviderType(previews.Provider),
		"url", previews.Repo,
		"secretRef", mapNode("name", previewSecretName(previews, prov)),
	}

	filterKV := []any{}
	if len(previews.Filter.Labels) > 0 {
		filterKV = append(filterKV, "labels", previews.Filter.Labels)
	}
	if previews.Filter.IncludeBranch != "" {
		filterKV = append(filterKV, "includeBranch", previews.Filter.IncludeBranch)
	}
	if previews.Filter.ExcludeBranch != "" {
		filterKV = append(filterKV, "excludeBranch", previews.Filter.ExcludeBranch)
	}
	// The ceiling is always written. It is a cost control, and a cost control
	// nobody can read off the manifest is one nobody checks (ADR-0017).
	filterKV = append(filterKV, "limit", previews.Filter.Limit)
	specKV = append(specKV, "filter", mapNode(filterKV...))

	if len(previews.Skip) > 0 {
		specKV = append(specKV, "skip", mapNode("labels", previews.Skip))
	}

	m := baseManifest(resourceSetAPIVersion, "ResourceSetInputProvider", prov, mapNode(specKV...))
	annotate(m, annReconcileEvery, previews.Interval)
	return m
}

// previewSecretName is the Secret the ResourceSetInputProvider authenticates
// with: the one the author named, or the one kelson materializes when they
// named none (ADR-0033 decision 4).
//
// The derived spelling is [naming.Lifecycle] — the same
// `<project>-<environment>-previews` this pair already carries — and it is
// *derived* on both sides rather than stored anywhere: the controller's
// materializer spells it with the same function
// (internal/forgeconn.PreviewSecretName), which is the "agree by shared code"
// rule the rest of this file follows for the preview namespace and tag.
//
// Rendering the name is what makes the empty field mean something. A provider
// whose secretRef were left blank would be a reference to a Secret with no
// name, and flux-operator would poll anonymously while a perfectly good
// credential sat in the namespace beside it. The renderer stays pure doing it:
// this is a string derived from two names already in the spec, not a lookup of
// anything.
func previewSecretName(previews *model.ResolvedPreviews, prov provenance) string {
	if previews.SecretRef != "" {
		return previews.SecretRef
	}
	return naming.Lifecycle(prov.project, prov.environment)
}

// previewResourceSet renders the fan-out: one OCIRepository and one
// Kustomization per input, and the provenance labels that make the children
// discoverable by the selector everything else kelson writes answers to.
func previewResourceSet(resolved *model.Resolved, previews *model.ResolvedPreviews, prov provenance) Manifest {
	spec := mapNode(
		"inputsFrom", seqNode(mapNode(
			"apiVersion", resourceSetAPIVersion,
			"kind", "ResourceSetInputProvider",
			"name", prov.name(),
		)),
		"commonMetadata", mapNode("labels", prov.labels()),
		// Health checks are on: a preview that applied but never became ready
		// is the state a reviewer most needs told apart from a working one, and
		// flux-operator reports it on the ResourceSet rather than leaving it to
		// be discovered per object.
		"wait", true,
		"resourcesTemplate", blockNode(previewResourcesTemplate(resolved, previews)),
	)
	return baseManifest(resourceSetAPIVersion, "ResourceSet", prov, spec)
}

// previewResourcesTemplate builds the multi-document YAML flux-operator
// instantiates per change request.
//
// It is assembled as text rather than encoded from nodes because that is what
// the field is — a string the operator parses after substitution — and because
// `<< inputs.sha >>` is not something a YAML encoder should be asked to have an
// opinion about. Assembly is plain concatenation of resolved spec values, so it
// is deterministic byte-for-byte, which the golden fixtures assert.
func previewResourcesTemplate(resolved *model.Resolved, previews *model.ResolvedPreviews) string {
	// The per-preview name and its namespace are the same string: the objects
	// that manage a preview live beside the environment, and what they apply
	// lives in a namespace of its own (ADR-0017 decision 3). It is spelled by
	// the package the publisher renders from, so the two cannot drift.
	child := naming.PreviewTemplate(resolved.Project, resolved.Environment.Name)
	ns := resolved.Environment.Namespace

	var b strings.Builder
	b.WriteString("---\n")
	b.WriteString("apiVersion: " + ociRepositoryAPIVersion + "\n")
	b.WriteString("kind: OCIRepository\n")
	b.WriteString("metadata:\n")
	b.WriteString("  name: " + child + "\n")
	b.WriteString("  namespace: " + ns + "\n")
	b.WriteString("spec:\n")
	b.WriteString("  interval: " + previewInterval + "\n")
	b.WriteString("  url: " + previews.Artifacts.Repository + "\n")
	b.WriteString("  ref:\n")
	b.WriteString("    tag: " + naming.InputSHA + "\n")
	if previews.Artifacts.SecretRef != "" {
		b.WriteString("  secretRef:\n")
		b.WriteString("    name: " + previews.Artifacts.SecretRef + "\n")
	}
	// The release stage, when the project declares one. A preview's artifact is
	// rendered by this same renderer, so it carries the same `release/`
	// directory the environment's does, and a preview that applied it in one
	// pass would run the migration beside the rollout — the exact failure the
	// split exists to prevent, reintroduced by the one Kustomization kelson
	// writes as text. This is what closes the gap ADR-0019 left open and #104's
	// second acceptance criterion asked for: previews run migrations, and they
	// run them the same way production does, because it is one renderer and one
	// publisher (ADR-0028 decision 2).
	//
	// prune is false here for the reason it is false on the environment's own
	// release Kustomization (internal/controller/fluxobjects.go): the Job's name
	// is its idempotency key, and a pruned Job is a migration that runs again.
	// Nothing is lost by not pruning — a preview's whole namespace goes when the
	// change request closes, which is the lifecycle flux-operator owns.
	release := releasePreviewName(child)
	if hasReleaseHook(resolved) {
		b.WriteString("---\n")
		b.WriteString("apiVersion: " + kustomizationAPIVersion + "\n")
		b.WriteString("kind: Kustomization\n")
		b.WriteString("metadata:\n")
		b.WriteString("  name: " + release + "\n")
		b.WriteString("  namespace: " + ns + "\n")
		b.WriteString("spec:\n")
		b.WriteString("  interval: " + previewInterval + "\n")
		b.WriteString("  prune: false\n")
		b.WriteString("  wait: true\n")
		b.WriteString("  timeout: " + releaseStageTimeout(resolved) + "\n")
		b.WriteString("  targetNamespace: " + child + "\n")
		b.WriteString("  sourceRef:\n")
		b.WriteString("    kind: OCIRepository\n")
		b.WriteString("    name: " + child + "\n")
		b.WriteString("  path: " + previewReleasePath + "\n")
		if resolved.Environment.Secrets.Backend == model.SecretsSOPS {
			b.WriteString(SOPSDecryptionBlock(resolved.Environment.Secrets.AgeKeySecret, "  "))
		}
	}

	b.WriteString("---\n")
	b.WriteString("apiVersion: " + kustomizationAPIVersion + "\n")
	b.WriteString("kind: Kustomization\n")
	b.WriteString("metadata:\n")
	b.WriteString("  name: " + child + "\n")
	b.WriteString("  namespace: " + ns + "\n")
	b.WriteString("spec:\n")
	b.WriteString("  interval: " + previewInterval + "\n")
	// prune and wait together are what make issue #103 hold: a preview database
	// is a data component inside the artifact, so it is applied by this
	// Kustomization and deleted by this Kustomization. There is no second thing
	// to remember.
	b.WriteString("  prune: true\n")
	b.WriteString("  wait: true\n")
	if hasReleaseHook(resolved) {
		// The barrier. kustomize-controller does not begin applying this
		// Kustomization until the one named here reports Ready, and the release
		// stage reports Ready only once its Job has completed — so a failed
		// migration leaves this Kustomization on its previous revision instead of
		// rolling a workload against a database that was never migrated.
		b.WriteString("  dependsOn:\n")
		b.WriteString("    - name: " + release + "\n")
	}
	// targetNamespace is a boundary, not a convenience. The artifact is already
	// rendered for this namespace, so it is normally redundant — it is written
	// so that the blast radius of a mis-published artifact is a property of the
	// manifest kelson wrote rather than of the artifact somebody else pushed.
	b.WriteString("  targetNamespace: " + child + "\n")
	b.WriteString("  sourceRef:\n")
	b.WriteString("    kind: OCIRepository\n")
	b.WriteString("    name: " + child + "\n")
	b.WriteString("  path: " + previewArtifactPath + "\n")
	// Under the sops backend a preview's artifact carries the same encrypted
	// Secrets the parent environment's path does, so its Kustomization needs
	// the same decryption block or every preview pod fails at start against a
	// Secret nothing decrypted (ADR-0022). It is written through the same
	// function that states the requirement to the operator, so the two cannot
	// drift.
	if resolved.Environment.Secrets.Backend == model.SecretsSOPS {
		b.WriteString(SOPSDecryptionBlock(resolved.Environment.Secrets.AgeKeySecret, "  "))
	}
	return b.String()
}

// hasReleaseHook reports whether any component declares one. It reads the
// resolved spec rather than the rendered set because the preview template is
// built beside the manifests rather than from them.
func hasReleaseHook(resolved *model.Resolved) bool {
	for i := range resolved.Components {
		if resolved.Components[i].Release != nil {
			return true
		}
	}
	return false
}

// releaseStageTimeout is how long a preview's release Kustomization may wait for
// its Job, as a Go duration string. See [ReleaseTimeoutSeconds] for why the
// Kustomization needs one at all; the extra minute covers the apply and the
// scheduling that precede the command itself.
func releaseStageTimeout(resolved *model.Resolved) string {
	return strconv.Itoa(ReleaseTimeoutSeconds(resolved)+60) + "s"
}

// blockNode emits a string as a YAML literal block scalar. The default style
// for a multi-line string is quoted with escaped newlines, which would make the
// one template in kelson's output the one thing in it nobody can read.
func blockNode(s string) *yaml.Node {
	return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Style: yaml.LiteralStyle, Value: s}
}

// annotate adds one annotation to an already-built manifest, for the keys that
// belong to a specific kind rather than to kelson's provenance. It mirrors what
// namespaceManifest does for kelson.dev/namespace-ownership.
func annotate(m Manifest, key, value string) {
	mapSet(mapGet(mapGet(docRoot(m.doc), "metadata"), "annotations"), key, strNode(value))
}

// previewsHash is the pair's kelson.dev/spec-hash. Like every other per-resource
// hash it covers only what these two documents are built from, so an unrelated
// spec edit leaves the annotation — and therefore the objects — untouched.
func previewsHash(resolved *model.Resolved, previews *model.ResolvedPreviews, name string) (string, error) {
	return hashJSON(struct {
		Project     string                 `json:"project"`
		Environment string                 `json:"environment"`
		Namespace   string                 `json:"namespace"`
		Resource    string                 `json:"resource"`
		Previews    model.ResolvedPreviews `json:"previews"`
	}{
		Project:     resolved.Project,
		Environment: resolved.Environment.Name,
		Namespace:   resolved.Environment.Namespace,
		Resource:    name,
		Previews:    *previews,
	})
}
