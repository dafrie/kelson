package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"

	kelsonv1alpha1 "github.com/dafrie/kelson/internal/api/gen/kelson/v1alpha1"
	"github.com/dafrie/kelson/internal/clusterprofile"
	"github.com/dafrie/kelson/internal/delivery"
	"github.com/dafrie/kelson/internal/model"
	"github.com/dafrie/kelson/internal/redact"
	"github.com/dafrie/kelson/internal/renderer"
)

// The server's spec pipeline: decode documents, select the environment, apply
// --image, resolve, render. It is the same sequence cmd/kelson's
// resolveAndRender runs and it calls the same functions, but it cannot reuse
// that code: the CLI's version is unexported, reads -f files from disk, and
// resolves overlay paths relative to the directories those files came from. A
// stored or inline spec has no directory, which is why overlays are refused
// here rather than silently dropped (see checkOverlays).

// decoded is a spec after decoding: the Project document and every Environment
// document that came with it.
type decoded struct {
	project      *model.Project
	environments []*model.Environment
}

// rendered is the outcome of the pipeline for one environment.
type rendered struct {
	project     *model.Project
	environment *model.Environment
	resolved    *model.Resolved
	manifests   []renderer.Manifest
	profile     clusterprofile.ClusterProfile
}

// decodeSpec decodes the authored documents. Environment documents are decoded
// in sorted key order so a spec with several environments produces the same
// diagnostics on every call — a map range would make the reported error order
// depend on Go's hash seed.
func decodeSpec(projectDoc []byte, envDocs map[string][]byte) (decoded, error) {
	var out decoded
	if len(projectDoc) == 0 {
		return out, fmt.Errorf("api: the spec carries no Project document")
	}

	names := make([]string, 0, len(envDocs))
	for name := range envDocs {
		names = append(names, name)
	}
	sort.Strings(names)

	bodies := make([][]byte, 0, len(envDocs)+1)
	bodies = append(bodies, projectDoc)
	for _, name := range names {
		bodies = append(bodies, envDocs[name])
	}

	var errs model.Errors
	for _, body := range bodies {
		docs, derrs := model.DecodeDocuments(body)
		errs = append(errs, derrs...)
		for _, d := range docs {
			switch v := d.(type) {
			case *model.Project:
				if out.project != nil {
					return decoded{}, fmt.Errorf("api: the spec carries two Project documents (%s and %s); a spec has exactly one project",
						out.project.Metadata.Name, v.Metadata.Name)
				}
				out.project = v
			case *model.Environment:
				out.environments = append(out.environments, v)
			}
		}
	}
	if len(errs) > 0 {
		return decoded{}, errs
	}
	if out.project == nil {
		return decoded{}, fmt.Errorf("api: no Project document found in the spec")
	}
	if len(out.environments) == 0 {
		return decoded{}, fmt.Errorf("api: no Environment document found in the spec")
	}
	return out, nil
}

// resolveSpec turns a SpecRef into decoded documents: a stored spec by project
// name, or the inline documents that keep the CLI's -f mode a first-class peer.
func (s *Server) resolveSpec(ctx context.Context, ref *kelsonv1alpha1.SpecRef) (decoded, error) {
	switch spec := ref.GetSpec().(type) {
	case *kelsonv1alpha1.SpecRef_Project:
		if s.specs == nil {
			return decoded{}, unimplemented("the spec store")
		}
		stored, err := s.specs.Get(ctx, spec.Project)
		if err != nil {
			return decoded{}, err
		}
		return decodeSpec(stored.Documents.Project, stored.Documents.Environments)
	case *kelsonv1alpha1.SpecRef_Documents:
		return decodeSpec(spec.Documents.GetProject(), spec.Documents.GetEnvironments())
	default:
		return decoded{}, fmt.Errorf("api: a spec reference is required: name a stored project or supply inline documents")
	}
}

// resolveProfile resolves the ClusterProfile input.
//
// # A request that names no profile gets the server's own cluster
//
// The renders on this server are pre-flight renders of what kelson-controller
// is about to render, and the controller renders against the profile it
// detected (internal/controller reads it from its Profiles source on every
// reconcile). A server that answered the same question against the *empty*
// profile would disagree with the controller on both halves of what a caller
// asks it: a routed spec (`domains:`, or a `port:` under a `domainSuffix`)
// refuses with `render/gateway-api-missing` on Deploy and Status even though
// the cluster has Gateway API, and the resource count in `kelson deploy`'s
// confirmation prompt is not the count the controller publishes.
//
// So an absent ProfileRef means "the cluster you are attached to", which is the
// only profile this server can honestly speak for — the CLI sends no ProfileRef
// unless `--profile` was given (cmd/kelson's inlineProfileRef), and that is the
// ordinary case, not the exotic one. A server started without profile capture
// (a test, a build with no cluster) still falls back to the zero profile rather
// than refusing, because those callers had nothing better before either.
//
// An explicit ProfileRef is unchanged and always wins: `yaml` renders against
// exactly those bytes, `from_cluster: true` captures, and `from_cluster: false`
// is the one way left to ask for a render against nothing detected — valid and
// deterministic, and since #140 a refusal for a spec whose services declare
// domains, because there is no routing substrate to attach them to.
func (s *Server) resolveProfile(ctx context.Context, ref *kelsonv1alpha1.ProfileRef) (clusterprofile.ClusterProfile, error) {
	switch profile := ref.GetProfile().(type) {
	case *kelsonv1alpha1.ProfileRef_FromCluster:
		if !profile.FromCluster {
			return clusterprofile.ClusterProfile{}, nil
		}
		if s.profile == nil {
			return clusterprofile.ClusterProfile{}, unimplemented("live profile capture")
		}
		return s.captureProfile(ctx)
	case *kelsonv1alpha1.ProfileRef_Yaml:
		return clusterprofile.Unmarshal(profile.Yaml)
	default:
		if s.profile == nil {
			return clusterprofile.ClusterProfile{}, nil
		}
		return s.captureProfile(ctx)
	}
}

// captureProfile reads the server's own cluster through the capture seam.
func (s *Server) captureProfile(ctx context.Context) (clusterprofile.ClusterProfile, error) {
	p, err := s.profile.Capture(ctx)
	if err != nil {
		return clusterprofile.ClusterProfile{}, unavailable("api: capturing the cluster profile: %w", err)
	}
	return p, nil
}

// selectEnvironment picks the environment to act on, matching the CLI's --env
// rule: an unnamed environment is only unambiguous when the spec holds one.
func selectEnvironment(environments []*model.Environment, name string) (*model.Environment, error) {
	names := make([]string, len(environments))
	for i, e := range environments {
		names[i] = e.Metadata.Name
	}
	if name == "" {
		if len(environments) == 1 {
			return environments[0], nil
		}
		return nil, fmt.Errorf("api: the spec declares several environments (%s); name one in the request", strings.Join(names, ", "))
	}
	for _, e := range environments {
		if e.Metadata.Name == name {
			return e, nil
		}
	}
	return nil, fmt.Errorf("api: environment %q is not declared by this spec (available: %s)", name, strings.Join(names, ", "))
}

// resolve decodes, selects and resolves — everything the pipeline does short of
// rendering. History needs the delivery stanza but no manifests, so it stops
// here rather than paying for a render (and rather than failing a spec that
// builds from source and has no image yet, #136).
//
// `globals` are the instance's GitSources, the global tier a component may bind
// to by name (ADR-0035 decision 2). It is variadic and almost every caller
// passes nothing, which means "resolve against an empty global tier" and is the
// honest answer for a path that has no reason to read the cluster's sources:
// nothing about a source reaches a manifest, so a render, a diff and a status
// resolve identically with or without them. The build path is the one that must
// pass them, because a component bound to a global name is precisely a build
// input (build.go's globalSources).
func (s *Server) resolve(ctx context.Context, ref *kelsonv1alpha1.SpecRef, envName, image string, globals ...model.Source) (*model.Project, *model.Environment, *model.Resolved, error) {
	spec, err := s.resolveSpec(ctx, ref)
	if err != nil {
		return nil, nil, nil, err
	}
	if image != "" {
		// --image stands in for spec.image, so it is subject to the same
		// precedence: a component that names its own image still wins
		// (rule P3, docs/model.md).
		spec.project.Spec.Image = image
	}
	environment, err := selectEnvironment(spec.environments, envName)
	if err != nil {
		return nil, nil, nil, err
	}
	resolved, errs := model.Resolve(spec.project, environment, globals...)
	if len(errs) > 0 {
		return nil, nil, nil, errs
	}
	return spec.project, environment, resolved, nil
}

// renderSpec is the server's resolveAndRender: the single place render, diff,
// deploy, status and rollback build the current manifest set, so they cannot
// drift on what "the current render" means.
func (s *Server) renderSpec(ctx context.Context, ref *kelsonv1alpha1.SpecRef, envName, image string, profileRef *kelsonv1alpha1.ProfileRef) (*rendered, error) {
	profile, err := s.resolveProfile(ctx, profileRef)
	if err != nil {
		return nil, err
	}
	return s.renderWith(ctx, ref, envName, image, profile)
}

// renderWith is renderSpec with the ClusterProfile already resolved. Promote
// renders the same environment twice — before and after the pin — and a
// from_cluster profile must be captured once for both, or the two sides could
// be judged against two different clusters.
func (s *Server) renderWith(ctx context.Context, ref *kelsonv1alpha1.SpecRef, envName, image string, profile clusterprofile.ClusterProfile) (*rendered, error) {
	project, environment, resolved, err := s.resolve(ctx, ref, envName, image)
	if err != nil {
		return nil, err
	}
	if err := checkOverlays(resolved); err != nil {
		return nil, err
	}
	manifests, err := renderer.Render(resolved, profile, nil)
	if err != nil {
		return nil, err
	}
	return &rendered{
		project:     project,
		environment: environment,
		resolved:    resolved,
		manifests:   manifests,
		profile:     profile,
	}, nil
}

// checkOverlays refuses a spec that carries overlays.
//
// Overlay paths are relative to the authoring documents (renderer.OverlayResolver),
// and a spec that arrived over the API has no authoring directory: the server
// stores documents, not the tree they came from. Rendering it with a nil
// resolver would either fail deep inside the renderer or — worse, if overlays
// were skipped — return manifests that silently omit what the user asked for.
// The refusal is structural and named, so a client can tell "kelson cannot do
// this yet" from "your spec is wrong".
func checkOverlays(resolved *model.Resolved) error {
	if len(resolved.Overlays) == 0 {
		return nil
	}
	errs := make(renderer.Errors, 0, len(resolved.Overlays))
	for _, o := range resolved.Overlays {
		path := o.Patch
		if path == "" {
			path = o.Manifest
		}
		errs = append(errs, renderer.Error{
			Code:        renderer.ErrOverlayLoad,
			Overlay:     path,
			Message:     "overlays are not supported over the API yet",
			Remediation: "render this spec with the CLI, where overlay paths resolve against the spec files, or remove the overlay",
		})
	}
	return errs
}

// manifestSet lifts a render into the delivery.ManifestSet shape the adapters
// and preview engines consume, mirroring cmd/kelson's manifestsToSet.
func manifestSet(out *rendered) (delivery.ManifestSet, error) {
	set := delivery.ManifestSet{
		Project:     out.project.Metadata.Name,
		Environment: out.environment.Metadata.Name,
	}
	for _, m := range out.manifests {
		body, err := m.YAML()
		if err != nil {
			return set, fmt.Errorf("api: encoding manifest %s/%s: %w", m.Kind, m.Name, err)
		}
		set.Manifests = append(set.Manifests, delivery.Manifest{
			APIVersion: m.APIVersion,
			Kind:       m.Kind,
			Name:       m.Name,
			Namespace:  m.Namespace,
			YAML:       body,
		})
	}
	set.SpecHash = setSpecHash(set.Manifests)
	return set, nil
}

// setSpecHash is the set-level provenance hash carried on ManifestSet.SpecHash,
// computed exactly like cmd/kelson's: a digest of the rendered bytes in render
// order. It is deterministic for the same reason the render is (ADR-0001), so
// a set deployed by the CLI and the same set deployed through the API carry the
// same hash.
func setSpecHash(manifests []delivery.Manifest) string {
	h := sha256.New()
	for _, m := range manifests {
		h.Write(m.YAML)
	}
	return "sha256:" + hex.EncodeToString(h.Sum(nil))
}

// wireManifests projects a render onto the wire, with Secret values redacted
// (issue #117).
//
// Redacting here and not in [manifestSet] is the display/delivery split the
// internal/redact package doc states. These two are the only wire surfaces that
// carry manifest bytes — RenderResponse.manifests and the dry-run RENDER
// Proposed event — and both exist to be *read*: a caller that wants a change
// applied calls Deploy, which renders again and hands manifestSet's real bytes
// to the adapter without passing through here. A caller that wants appliable
// bytes on disk runs `kelson render`, which never touches this package.
//
// The alternative — shipping real Secret values to every client that previews a
// spec — is the leak this issue is about, and the cost of the choice is bounded
// and visible: a redacted document says so where the value was.
func wireManifests(manifests []renderer.Manifest) ([]*kelsonv1alpha1.Manifest, error) {
	out := make([]*kelsonv1alpha1.Manifest, 0, len(manifests))
	for _, m := range manifests {
		body, err := m.YAML()
		if err != nil {
			return nil, fmt.Errorf("api: encoding manifest %s/%s: %w", m.Kind, m.Name, err)
		}
		if body, err = redact.Document(body); err != nil {
			return nil, fmt.Errorf("api: redacting manifest %s/%s: %w", m.Kind, m.Name, err)
		}
		out = append(out, &kelsonv1alpha1.Manifest{
			ApiVersion: m.APIVersion,
			Kind:       m.Kind,
			Name:       m.Name,
			Namespace:  m.Namespace,
			Yaml:       body,
		})
	}
	return out, nil
}

// target derives the addressing a cluster-reading RPC needs from a resolved
// spec: which project, which environment, which namespace.
//
// It used to also resolve a delivery mode — the Environment's, or the request's
// override — and hand the flux adapter the profile's flux-operator finding.
// ADR-0028 decision 9 deleted the adapters and the mode with them, so the
// request's `mode` field is accepted by the schema and consulted by nothing;
// it stays on the wire because ADR-0027 decision 6 keeps the ConnectRPC surface
// unchanged.
func target(out *rendered) Target {
	return Target{
		Project:     out.project.Metadata.Name,
		Environment: out.environment.Metadata.Name,
		Namespace:   out.resolved.Environment.Namespace,
	}
}
