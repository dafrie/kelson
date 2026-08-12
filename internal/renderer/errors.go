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
	// ErrInternal: an invariant failed inside the renderer itself.
	ErrInternal = "render/internal"
)

// Error is one structured render problem.
type Error struct {
	Code    string `json:"code"`
	Overlay string `json:"overlay,omitempty"` // overlay path, for overlay failures
	Target  string `json:"target,omitempty"`  // "Kind/name", for targeting failures
	Message string `json:"message"`
}

func (e Error) Error() string {
	loc := ""
	if e.Overlay != "" {
		loc = fmt.Sprintf("overlay %s", e.Overlay)
	}
	if e.Target != "" {
		if loc != "" {
			loc += " "
		}
		loc += "targeting " + e.Target
	}
	if loc != "" {
		loc = " (" + loc + ")"
	}
	return fmt.Sprintf("[%s]%s %s", e.Code, loc, e.Message)
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
