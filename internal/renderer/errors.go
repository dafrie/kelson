package renderer

import (
	"fmt"
	"strings"
)

// Render error codes. These are stable identifiers, in the spirit of
// docs/model.md's validation taxonomy.
const (
	// ErrOverlayLoad: an overlay body could not be read through the
	// resolver injected by the caller.
	ErrOverlayLoad = "overlay/load"
	// ErrOverlayInvalid: an overlay body is not usable — unparseable YAML,
	// or a patch without the apiVersion/kind/metadata.name identity it
	// targets.
	ErrOverlayInvalid = "overlay/invalid"
	// ErrOverlayTarget: a patch names a resource that does not exist in
	// the set rendered so far.
	ErrOverlayTarget = "overlay/unknown-target"
	// ErrImageUnresolved: an application reached the renderer without a usable
	// image — the spec builds it from source and no build result was supplied,
	// so its image is still model.ImageUnresolved (issue #136).
	ErrImageUnresolved = "image/unresolved"
	// ErrGatewayAPIMissing: the spec asks for routing but the ClusterProfile
	// reports no Gateway API. kelson renders Gateway API only (#140), so this
	// is a capability gap the caller must see rather than an Ingress rendered
	// behind their back.
	ErrGatewayAPIMissing = "render/gateway-api-missing"
	// ErrPostgresUnsupported: the ClusterProfile says this cluster cannot host
	// the requested preset — no CloudNativePG, a version below the capability's
	// floor, or a CRD the API server does not serve. Only a definite No lands
	// here; an Unknown verdict renders (docs/data-services.md, issue #144).
	ErrPostgresUnsupported = "render/postgres-unsupported"
	// ErrServiceNotImplemented: a service the model accepts and the renderer
	// does not render yet — `type: valkey` (#98), `preset: branch` (#99). The
	// error names the issue rather than rendering something else quietly
	// (issue #141).
	ErrServiceNotImplemented = "render/service-not-implemented"
	// ErrServiceName: <project>-<environment>-<service> is too long for the
	// object names CloudNativePG derives from it.
	ErrServiceName = "render/service-name-too-long"
	// ErrBindingUnknownService: an env binding names a service the resolved
	// spec does not declare. Model validation catches this for authored specs;
	// the renderer is also fed a Resolved directly by the API plane.
	ErrBindingUnknownService = "render/binding-unknown-service"
	// ErrBindingUnknownKey: an env binding names a key kelson does not map to
	// a key of the credential Secret the operator generates.
	ErrBindingUnknownKey = "render/binding-unknown-key"
	// ErrInternal: an invariant failed inside the renderer itself.
	ErrInternal = "render/internal"
)

// Error is one structured render problem.
type Error struct {
	Code        string `json:"code"`
	Application string `json:"application,omitempty"` // application whose spec is at fault
	Overlay     string `json:"overlay,omitempty"`     // overlay path, for overlay failures
	Target      string `json:"target,omitempty"`      // "Kind/name", for targeting failures
	Message     string `json:"message"`
	Remediation string `json:"remediation,omitempty"` // the fix, stated as an action
}

func (e Error) Error() string {
	var loc []string
	if e.Application != "" {
		loc = append(loc, "application "+e.Application)
	}
	if e.Overlay != "" {
		loc = append(loc, "overlay "+e.Overlay)
	}
	if e.Target != "" {
		loc = append(loc, "targeting "+e.Target)
	}
	out := fmt.Sprintf("[%s]", e.Code)
	if len(loc) > 0 {
		out += " (" + strings.Join(loc, " ") + ")"
	}
	out += " " + e.Message
	if e.Remediation != "" {
		out += "; " + e.Remediation
	}
	return out
}

// Errors is the collected result of a failed render. Rendering fails fast on
// the first overlay problem (later patches would apply against a resource set
// that already diverged from intent), but reports it structurally.
type Errors []Error

func (e Errors) Error() string {
	var b strings.Builder
	fmt.Fprintf(&b, "%d render error(s):", len(e))
	for _, err := range e {
		fmt.Fprintf(&b, "\n  - %s", err.Error())
	}
	return b.String()
}
