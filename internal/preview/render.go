package preview

import (
	"strconv"
	"strings"

	"github.com/dafrie/kelson/internal/clusterprofile"
	"github.com/dafrie/kelson/internal/model"
	"github.com/dafrie/kelson/internal/preview/naming"
	"github.com/dafrie/kelson/internal/renderer"
)

// Options is one publish or one preview render: which documents, which
// environment, which change request, and what image its components run.
type Options struct {
	// Project and Environment are the authored documents, exactly as the CLI
	// loaded them. They are not modified: [Render] copies what it must change.
	Project     *model.Project
	Environment *model.Environment

	// PR is the change request number as the forge numbers it ("412"), and SHA
	// its head commit. Together they are the preview's identity: PR names the
	// namespace a human goes looking in, SHA tags the artifact flux-operator
	// fetches (ADR-0017 decisions 2 and 3).
	PR  string
	SHA string

	// Image is the image the preview's components run, standing in for
	// spec.image exactly as `kelson deploy --image` does — which is how this
	// composes with `kelson build` in the same CI job.
	Image string

	// Profile is what the renderer is told about the target cluster. A preview
	// is judged against the same cluster as its parent environment, so the same
	// profile the environment renders against belongs here.
	Profile clusterprofile.ClusterProfile

	// Overlays loads overlay bodies referenced by path from the spec. It is
	// required iff the spec carries overlays, as everywhere else.
	Overlays renderer.OverlayResolver
}

// Set is a rendered preview: the concrete manifests and the two addresses they
// have to agree with — the namespace they were rendered for and the tag they
// will be published under.
type Set struct {
	Project     string
	Environment string
	PR          string
	SHA         string

	// Namespace is <project>-<environment>-pr<id>, which is also the name of
	// the OCIRepository and the Kustomization the parent environment's
	// ResourceSet generates for this change request.
	Namespace string
	// ParentNamespace is the environment's own namespace: where the lifecycle
	// pair runs and where the operator's Secrets live. A publish never renders
	// into it — that is the split ADR-0017 decision 3 exists for — but a
	// credential is looked up there.
	ParentNamespace string
	// Repository is previews.artifacts.repository with the oci:// prefix
	// stripped: the repository this set is pushed to.
	Repository string
	// Tag is the head commit, which is what the rendered OCIRepository pins.
	Tag string
	// SourceRepo is the forge repository whose change requests become previews
	// (previews.repo), recorded on the artifact as its source annotation.
	SourceRepo string

	// Manifests are the rendered set in apply order, Namespace first.
	Manifests []renderer.Manifest
}

// Render renders one pull request's manifests.
//
// It is the parent environment's own render with three substitutions, and it is
// worth being explicit about why each one is not optional:
//
//   - The namespace becomes the preview's. Every resource in the artifact
//     names it, and the cluster-scoped Namespace object the renderer emits for
//     every set is where the preview's namespace actually comes from — the
//     ResourceSet's targetNamespace cannot create it. A publisher that rendered
//     the parent's namespace here would apply a pull request's manifests on top
//     of the environment it is previewing.
//   - The previews block is dropped. A preview that carried it would render a
//     ResourceSetInputProvider and a ResourceSet of its own, and every pull
//     request would spawn a full set of previews of itself.
//   - Hostnames gain the change request. See [naming.Host]: without it every
//     preview of an environment claims the same hostname as the environment,
//     and a component with an authored production hostname claims production's.
//
// Everything else — the provenance labels, the spec hashes, the ordering, the
// data services, the capability gates — is the parent environment's render,
// unchanged, because a preview that differed anywhere else would be previewing
// something other than the change.
func Render(opts Options) (*Set, error) {
	if opts.Project == nil || opts.Environment == nil {
		return nil, Error{
			Reason:      ReasonNoPreviews,
			Message:     "a preview render needs both a Project and an Environment document",
			Remediation: "pass the spec files that declare them with -f",
		}
	}
	if err := naming.ValidateID(opts.PR); err != nil {
		return nil, Error{
			Reason:      ReasonInvalidChangeRequest,
			Message:     err.Error(),
			Remediation: "pass the forge's own change request number, e.g. --pr 412 (in GitHub Actions, ${{ github.event.pull_request.number }})",
		}
	}
	if err := naming.ValidateSHA(opts.SHA); err != nil {
		return nil, Error{
			Reason:      ReasonInvalidSHA,
			Message:     err.Error(),
			Remediation: "pass the change request's head commit in full, e.g. --sha ${{ github.event.pull_request.head.sha }}",
		}
	}

	project := opts.Project.Metadata.Name
	environment := opts.Environment.Metadata.Name
	previews := opts.Environment.Spec.Previews
	if previews == nil {
		return nil, Error{
			Reason: ReasonNoPreviews,
			Message: "environment " + quoted(environment) + " has no spec.previews, so no ResourceSet exists to fetch " +
				"a published artifact and nothing would ever apply it",
			Remediation: "add spec.previews to this Environment (see docs/model.md, Previews), or name an environment that has one with --env",
		}
	}
	if strings.TrimSpace(previews.Artifacts.Repository) == "" {
		return nil, Error{
			Reason:      ReasonNoArtifactRepository,
			Message:     "environment " + quoted(environment) + " sets no spec.previews.artifacts.repository, so there is nowhere to publish to",
			Remediation: "set spec.previews.artifacts.repository to the oci:// repository the previews are published to and pulled from",
		}
	}
	if base := naming.Base(project, environment); naming.BaseTooLong(base) {
		return nil, Error{
			Reason: ReasonNameTooLong,
			Message: "a preview of this environment is named " + quoted(base+"-"+naming.Infix+opts.PR) +
				", and " + quoted(base) + " already exceeds the " + strconv.Itoa(naming.MaxBase) + "-character cap on <project>-<environment>",
			Remediation: "shorten the project or environment name. A preview's name is also its namespace, which is a DNS-1123 " +
				"label capped at 63, and kelson reserves " + strconv.Itoa(naming.IDDigits) + " digits for the change request number",
		}
	}

	resolved, parent, err := resolvePreview(opts)
	if err != nil {
		return nil, err
	}
	manifests, err := renderer.Render(resolved, opts.Profile, opts.Overlays)
	if err != nil {
		return nil, err
	}

	return &Set{
		Project:         project,
		Environment:     environment,
		PR:              opts.PR,
		SHA:             naming.Tag(opts.SHA),
		Namespace:       resolved.Environment.Namespace,
		ParentNamespace: parent.Environment.Namespace,
		Repository:      strings.TrimPrefix(strings.TrimSpace(previews.Artifacts.Repository), ociPrefix),
		Tag:             naming.Tag(opts.SHA),
		SourceRepo:      previews.Repo,
		Manifests:       manifests,
	}, nil
}

// ociPrefix is how the spec spells an OCI repository, matching what the
// rendered OCIRepository's url field carries. The registry API wants the bare
// reference, so it comes off here and nowhere else.
const ociPrefix = "oci://"

// resolvePreview resolves the parent environment and then the preview, and
// returns both.
//
// The parent is resolved first, from the authored documents untouched, for two
// reasons worth the second pass over a pure function: it is what validates the
// previews block this publish depends on (the preview's own copy no longer
// carries it), and its namespace is where an operator's credentials live — the
// preview's namespace does not exist yet, since creating it is what the
// artifact is for.
//
// The preview is then resolved through the ordinary model rules with its
// identity substituted, so it inherits every precedence rule and every default
// rather than a second implementation of them.
func resolvePreview(opts Options) (resolved, parent *model.Resolved, err error) {
	project := *opts.Project
	if opts.Image != "" {
		// --image stands in for spec.image and is subject to the same
		// precedence: a component that names its own image still wins
		// (rule P3, docs/model.md), exactly as `kelson deploy --image` behaves.
		project.Spec.Image = opts.Image
	}

	parent, errs := model.Resolve(&project, opts.Environment)
	if len(errs) > 0 {
		return nil, nil, errs
	}
	// The gate is read from the resolved mode rather than the document, because
	// delivery.mode is a P4 chain: the Project's default counts.
	if parent.Environment.Mode != model.DeliveryFlux {
		return nil, nil, Error{
			Reason: ReasonRequiresFlux,
			Message: "environment " + quoted(opts.Environment.Metadata.Name) + " declares previews but its delivery mode is " +
				quoted(string(parent.Environment.Mode)) + ", so nothing would reconcile the artifact this would publish",
			Remediation: "set delivery.mode: flux on this environment. Previews are the flux-operator ResourceSet lifecycle and " +
				"ADR-0017 gates them on it deliberately",
		}
	}

	environment := *opts.Environment
	environment.Spec.Namespace = naming.Preview(opts.Project.Metadata.Name, opts.Environment.Metadata.Name, opts.PR)
	environment.Spec.Previews = nil
	resolved, errs = model.Resolve(&project, &environment)
	if len(errs) > 0 {
		return nil, nil, errs
	}
	previewHosts(resolved, opts.PR)
	return resolved, parent, nil
}

// previewHosts rewrites every hostname the set would serve so it belongs to
// this change request. See [naming.Host] for why the first label is what
// changes and why authored hostnames are rewritten too.
//
// The rewrite allocates rather than writing through the slice it was given:
// resolution carries a component's authored domains by reference, so mutating
// in place would edit the caller's Project document — which the caller may
// still render for the parent environment afterwards.
func previewHosts(resolved *model.Resolved, id string) {
	for i := range resolved.Components {
		c := &resolved.Components[i]
		if len(c.Domains) == 0 {
			continue
		}
		hosts := make([]string, len(c.Domains))
		for j, d := range c.Domains {
			hosts[j] = naming.Host(d, id)
		}
		c.Domains = hosts
	}
}

func quoted(s string) string { return `"` + s + `"` }
