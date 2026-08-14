// Package renderer is the RENDERING plane (see docs/architecture.md).
//
// It is the pure function at the centre of kelson:
//
//	(resolved spec, ClusterProfile) → manifests
//
// # Dependency rule (enforced by CI, issue #20)
//
// This package MUST remain deterministically pure. It must not import:
//
//   - any Kubernetes client (client-go, controller-runtime, dynamic clients)
//   - any HTTP or network client
//   - the standard library's `time` (no clock) or `os` (no ambient inputs)
//   - `math/rand` without an injected, deterministic source
//
// `ClusterProfile` is passed in as an input, never looked up. Overlay bodies
// referenced by path in the spec arrive through the caller-injected
// OverlayResolver; the renderer performs no I/O. See ADR-0001 and issue #20.
// Adding a forbidden import fails CI with an explanation; an exception
// requires an ADR, not a //nolint.
//
// # Determinism
//
// Manifests are built as ordered yaml.Node trees; env maps are sorted before
// emission; the spec hash is a sha256 over the canonical JSON encoding of the
// resolved per-component spec (JSON marshals maps with sorted keys). Same
// inputs render byte-identical output, which the golden harness asserts on
// every fixture (issue #27).
package renderer

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/dafrie/kelson/internal/clusterprofile"
	"github.com/dafrie/kelson/internal/model"
)

// Version is the renderer's own version, stamped onto every resource as
// kelson.dev/renderer-version. It distinguishes "the user changed the spec"
// from "kelson changed how it renders" across upgrades (docs/architecture.md,
// Provenance). Bump it deliberately when rendered output changes shape.
//
// 0.2.0: every component renders its own ServiceAccount and its pod template
// names it (ADR-0014 decision D). Worker and cron workloads that previously ran
// as `default` acquire an identity, which is a shape change for unchanged
// input — exactly what this constant exists to announce.
const Version = "0.2.0"

// OverlayResolver loads the body of an overlay referenced by path from the
// spec (paths are relative to the authoring documents). The renderer never
// touches the filesystem itself; it is required iff the resolved spec carries
// overlays.
type OverlayResolver func(path string) ([]byte, error)

// Render returns the Kubernetes manifests for one resolved (Project,
// Environment) pair against one ClusterProfile. It is a pure function: no
// cluster, no network, no clock, no filesystem beyond the injected resolver.
func Render(resolved *model.Resolved, profile clusterprofile.ClusterProfile, resolver OverlayResolver) ([]Manifest, error) {
	if errs := unresolvedImages(resolved); len(errs) > 0 {
		return nil, errs
	}
	// The delivery-mode gate of ADR-0016: a helm component renders in Flux mode
	// only. It is decided here, from spec data, before anything is emitted —
	// see internal/renderer/helm.go for why the gate is the renderer's and not
	// the delivery plane's.
	if errs := helmRequiresFlux(resolved); len(errs) > 0 {
		return nil, errs
	}
	// The same gate for previews (ADR-0017), which cites the Helm precedent
	// deliberately: a ResourceSet outside Flux mode has nothing to reconcile it.
	if errs := PreviewsRequireFlux(resolved); len(errs) > 0 {
		return nil, errs
	}
	// And the mirror image of both for release commands (ADR-0019): only direct
	// mode can stop between two resources long enough to wait for a migration,
	// so only direct mode may render one. See internal/renderer/release.go.
	if errs := releaseRequiresDirect(resolved); len(errs) > 0 {
		return nil, errs
	}
	// And for the secret backend (ADR-0018): a backend that is not one of the
	// three has no mechanism at all behind it. See internal/renderer/secrets.go.
	if errs := secretBackendSupported(resolved); len(errs) > 0 {
		return nil, errs
	}
	// And the delivery-mode gate for `sops` (ADR-0021), which is the same gate
	// again for the same reason: its decryption step is kustomize-controller's,
	// so an environment that is not in Flux mode has nothing that could turn
	// the encrypted file in the repository into a Secret in the cluster.
	if errs := sopsRequiresFlux(resolved); len(errs) > 0 {
		return nil, errs
	}
	// The Namespace leads the set: delivery.ManifestSet documents apply order as
	// "namespaces first", and every following resource targets it (issue #150).
	// Overlays append after the core resources, so nothing can displace it.
	ns, err := namespaceManifest(resolved)
	if err != nil {
		return nil, err
	}
	out := []Manifest{ns}

	// The `externalSecrets` backend's resources come next, ahead of everything
	// that reads a Secret — the data services whose operators read an `auth:`
	// Secret as well as the workloads (ADR-0020). Nothing waits for the sync;
	// order is the only sequencing a rendered set can express (issue #89), and
	// a Secret that is not populated yet is a pod that retries, which is the
	// benign end of this failure. Under every other backend this emits nothing.
	// See internal/renderer/externalsecrets.go.
	es, err := externalSecretsManifests(resolved, profile)
	if err != nil {
		return nil, err
	}
	out = append(out, es...)

	// Data services come before the workloads that bind to them: a Deployment
	// applied ahead of the Cluster whose credentials it references would start
	// by failing to find a Secret. Nothing waits for readiness — ordering is
	// the only sequencing a rendered set can express (issue #89).
	services := map[string]boundService{}
	for i := range resolved.DataServices {
		svc := &resolved.DataServices[i]
		ms, bound, err := serviceManifests(resolved, svc, profile)
		if err != nil {
			return nil, err
		}
		out = append(out, ms...)
		services[svc.Name] = bound
	}

	// Charts follow, for the same ordering reason and one more: a chart is
	// often the dependency a workload talks to, and kelson's own inventory ends
	// at the HelmRelease — everything the chart installs arrives later, on
	// helm-controller's schedule, which no apply order can express.
	for i := range resolved.Charts {
		ms, err := chartManifests(resolved, &resolved.Charts[i])
		if err != nil {
			return nil, err
		}
		out = append(out, ms...)
	}

	// Release hooks come between the two: after everything a migration talks to,
	// before everything that must not roll until it has finished (issue #104).
	// The set expresses that as order and nothing more — the waiting is the
	// direct adapter's, which is the whole reason the field is direct-only
	// (ADR-0019, internal/delivery/direct/release.go).
	for i := range resolved.Components {
		c := &resolved.Components[i]
		if c.Release == nil {
			continue
		}
		ms, err := releaseManifests(resolved, c, services)
		if err != nil {
			return nil, err
		}
		out = append(out, ms...)
	}

	for i := range resolved.Components {
		ms, err := componentManifests(resolved, &resolved.Components[i], profile, services)
		if err != nil {
			return nil, err
		}
		out = append(out, ms...)
	}
	// The previews pair goes last of the core resources. It is environment-level
	// machinery about *other* namespaces: it depends on this environment's
	// Namespace existing and nothing in this environment depends on it, so the
	// ordering contract — the things workloads need, before the workloads — has
	// nothing to say about where it goes, and the end is where a reader looks
	// for what is not part of the running application.
	if resolved.Environment.Previews != nil {
		ms, err := previewsManifests(resolved)
		if err != nil {
			return nil, err
		}
		out = append(out, ms...)
	}
	if len(resolved.Overlays) > 0 {
		if resolver == nil {
			return nil, Errors{{
				Code:    ErrOverlayLoad,
				Message: "spec carries overlays but no overlay resolver was supplied",
			}}
		}
		if err := applyOverlays(&out, resolved, resolver); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// unresolvedImages rejects every application whose image is not a reference a
// cluster could pull.
//
// A spec that builds from source resolves to model.ImageUnresolved until a
// build fills the digest in. Emitting that sentinel produced `image: "@"` — a
// manifest that no cluster accepts and that nothing explained (issue #136), so
// the placeholder now fails the render instead of reaching the output. The
// decision is pure data: the renderer never asks whether a build ran, only
// whether the resolved spec names an image. Every offending component is
// reported, not just the first — one run should list all the work.
func unresolvedImages(resolved *model.Resolved) Errors {
	var errs Errors
	for i := range resolved.Components {
		app := &resolved.Components[i]
		var message string
		switch app.Image {
		case model.ImageUnresolved:
			message = "no image yet: the spec builds this component from source and no build result was supplied"
		case "":
			message = "no image: the resolved spec names none"
		default:
			continue
		}
		errs = append(errs, Error{
			Code:        ErrImageUnresolved,
			Application: app.Name,
			Message:     message,
			Remediation: "pass the built reference with --image, or set spec.image on the Project or image on the component",
		})
	}
	return errs
}

// provenance carries the identity stamped onto every rendered resource
// (docs/architecture.md, Provenance).
type provenance struct {
	project      string
	environment  string
	application  string // empty for overlay-contributed manifests
	resourceName string // resource metadata.name; defaults to application
	namespace    string
	specHash     string
	overlays     []string // overlay paths that touched this resource, in order
	// extraLabels are alternating key/value pairs appended after the provenance
	// labels every resource carries. One resource uses it today — the release
	// Job, which the delivery plane finds by kelson.dev/release-hook rather than
	// by parsing its name (internal/renderer/release.go) — and it appends rather
	// than merges so the provenance labels stay in one fixed order.
	extraLabels []string
}

func (p provenance) labels() *yaml.Node {
	kv := []any{
		"app.kubernetes.io/managed-by", "kelson",
	}
	if p.application != "" {
		kv = append(kv, "kelson.dev/application", p.application)
	}
	kv = append(kv,
		"kelson.dev/environment", p.environment,
		"kelson.dev/project", p.project,
	)
	for i := 0; i+1 < len(p.extraLabels); i += 2 {
		kv = append(kv, p.extraLabels[i], p.extraLabels[i+1])
	}
	return mapNode(kv...)
}

func (p provenance) annotations() *yaml.Node {
	kv := []any{
		"kelson.dev/renderer-version", Version,
		"kelson.dev/spec-hash", p.specHash,
	}
	if len(p.overlays) > 0 {
		kv = append(kv, "kelson.dev/overlays", strings.Join(p.overlays, ","))
	}
	return mapNode(kv...)
}

func metadataNode(p provenance) *yaml.Node {
	return mapNode(
		"name", p.name(),
		"namespace", p.namespace,
		"labels", p.labels(),
		"annotations", p.annotations(),
	)
}

func (p provenance) name() string {
	if p.resourceName != "" {
		return p.resourceName
	}
	return p.application
}

// baseManifest assembles the canonical document shape:
// apiVersion, kind, metadata, spec.
func baseManifest(apiVersion, kind string, prov provenance, spec *yaml.Node) Manifest {
	root := mapNode(
		"apiVersion", apiVersion,
		"kind", kind,
		"metadata", metadataNode(prov),
		"spec", spec,
	)
	return Manifest{
		APIVersion: apiVersion,
		Kind:       kind,
		Name:       prov.name(),
		Namespace:  prov.namespace,
		doc:        docNode(root),
	}
}

// selectorLabels are the stable identity labels pod templates and Services
// match on. Provenance annotations and non-identity labels stay out of
// selectors so selector sets remain minimal and immutable.
func selectorLabels(prov provenance) *yaml.Node {
	return mapNode(
		"kelson.dev/project", prov.project,
		"kelson.dev/application", prov.application,
	)
}

// specHash computes kelson.dev/spec-hash for one component: a sha256 over
// the canonical JSON of the resolved per-component input. JSON marshalling
// is deterministic (struct field order fixed, map keys sorted), so equal
// inputs hash identically. Per-component scoping means an unchanged
// component produces an unchanged artifact across sibling edits
// (docs/model.md, "What is versioned").
//
// The payload's JSON key stays "application" through ADR-0014's rename. The
// hash is a change detector, not a document: renaming the key would churn the
// annotation on every workload in every cluster to say nothing new, and the
// thing that genuinely changed about rendering — the per-component
// ServiceAccount — is what Version is for.
func specHash(resolved *model.Resolved, app *model.ResolvedComponent) (string, error) {
	payload := struct {
		Project     string                  `json:"project"`
		Environment hashEnv                 `json:"environment"`
		Application model.ResolvedComponent `json:"application"`
	}{
		Project: resolved.Project,
		Environment: hashEnv{
			Name:      resolved.Environment.Name,
			Namespace: resolved.Environment.Namespace,
			Routing:   resolved.Environment.Routing,
		},
		Application: *app,
	}
	return hashJSON(payload)
}

// overlayHash computes the spec-hash for a manifest contributed by an
// overlay: bound to the environment and to the overlay body, so an overlay
// edit changes the artifact.
func overlayHash(resolved *model.Resolved, overlayPath string, body []byte) (string, error) {
	payload := struct {
		Project     string  `json:"project"`
		Environment hashEnv `json:"environment"`
		Overlay     string  `json:"overlay"`
		Body        string  `json:"body"`
	}{
		Project: resolved.Project,
		Environment: hashEnv{
			Name:      resolved.Environment.Name,
			Namespace: resolved.Environment.Namespace,
			Routing:   resolved.Environment.Routing,
		},
		Overlay: overlayPath,
		Body:    string(body),
	}
	return hashJSON(payload)
}

type hashEnv struct {
	Name      string                `json:"name"`
	Namespace string                `json:"namespace"`
	Routing   model.ResolvedRouting `json:"routing"`
}

func hashJSON(payload any) (string, error) {
	b, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}
