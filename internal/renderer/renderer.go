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
// resolved per-application spec (JSON marshals maps with sorted keys). Same
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
const Version = "0.1.0"

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
	out := []Manifest{}
	for i := range resolved.Applications {
		ms, err := appManifests(resolved, &resolved.Applications[i], profile)
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
// whether the resolved spec names an image. Every offending application is
// reported, not just the first — one run should list all the work.
func unresolvedImages(resolved *model.Resolved) Errors {
	var errs Errors
	for i := range resolved.Applications {
		app := &resolved.Applications[i]
		var message string
		switch app.Image {
		case model.ImageUnresolved:
			message = "no image yet: the spec builds this application from source and no build result was supplied"
		case "":
			message = "no image: the resolved spec names none"
		default:
			continue
		}
		errs = append(errs, Error{
			Code:        ErrImageUnresolved,
			Application: app.Name,
			Message:     message,
			Remediation: "pass the built reference with --image, or set spec.image on the Project or image on the application",
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

// specHash computes kelson.dev/spec-hash for one application: a sha256 over
// the canonical JSON of the resolved per-application input. JSON marshalling
// is deterministic (struct field order fixed, map keys sorted), so equal
// inputs hash identically. Per-application scoping means an unchanged
// application produces an unchanged artifact across sibling edits
// (docs/model.md, "What is versioned").
func specHash(resolved *model.Resolved, app *model.ResolvedApplication) (string, error) {
	payload := struct {
		Project     string                    `json:"project"`
		Environment hashEnv                   `json:"environment"`
		Application model.ResolvedApplication `json:"application"`
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
