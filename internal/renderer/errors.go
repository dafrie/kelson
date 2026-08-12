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
