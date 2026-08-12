// Package support detects version skew against the declared support matrix
// and reports it explicitly, so kelson never renders silently against an API
// a cluster cannot serve (issue #57).
//
// kelson adopts components a cluster already has rather than installing its
// own (ADR-0003), which means inheriting their version skew. Detecting that
// cert-manager is *present* is not enough: a version too old to serve the API
// we render against fails confusingly long after deploy. This package turns
// that into a question asked up front, at preview time.
//
// It is a pure package: no cluster access, no client-go, no network, no
// clock. Its whole input is a clusterprofile.ClusterProfile value, so the
// renderer and preview can import it without breaking purity (issue #20).
//
// # Supported, unsupported, and unknown are three different things
//
// The whole point of a version is that "we checked and it is good", "we
// checked and it is too old", and "we could not tell" are distinct answers.
// This mirrors the rule the codebase already follows in two other places:
// internal/diff.Unvalidated for policy and ClusterProfile.Incomplete for
// detection. Collapsing unknown into either of the other two is how a boring
// support check quietly becomes the confusing runtime failure it exists to
// prevent:
//
//   - Supported — version known and at or above the minimum.
//   - Unsupported — version known and below the minimum: a refusal or a
//     documented degradation.
//   - Unknown — version absent, unparseable, or hidden behind a detection
//     Gap. Not silently fine, not a failure: a distinct result a caller can
//     decide how to treat.
//
// A component that is simply not present in the ClusterProfile is not a version
// problem and is not reported — whether an absent cert-manager is allowed is the
// renderer's call, not a version-skew one.
package support

import "github.com/dafrie/kelson/internal/clusterprofile"

// Outcome is the three-way answer to "is this component's version supported?".
type Outcome int

const (
	// Supported means the version is known and at or above the minimum.
	Supported Outcome = iota
	// Unsupported means the version is known and below the minimum.
	Unsupported
	// Unknown means the version is absent, unparseable, or hidden behind a
	// detection gap: neither fine nor failing, left for the caller to decide.
	Unknown
)

// String renders an Outcome for error messages and docs.
func (o Outcome) String() string {
	switch o {
	case Supported:
		return "supported"
	case Unsupported:
		return "unsupported"
	default:
		return "unknown"
	}
}

// Result is the skew verdict for one component in a ClusterProfile.
type Result struct {
	// Component is the matrix id, e.g. "cert-manager".
	Component string
	// Found is the raw detected version, or "" when none was reported.
	Found string
	// Required is the minimum version the matrix declares.
	Required string
	// Degrade is the matrix's decision for a too-old version.
	Degrade Degrade
	// Outcome is the verdict.
	Outcome Outcome
	// Message is a human-readable verdict naming the component, the version
	// found, and the version required — for Unsupported it is the message the
	// issue's acceptance criterion demands at preview time.
	Message string
}

// Report is the full set of skew verdicts for one profile.
type Report struct {
	Results []Result
}

// Check evaluates every adopted component the profile reports as present
// against the declared support matrix. It never panics and never consults a
// cluster; it only reads the profile. See the package comment for what each
// Outcome means and why absent components are skipped.
func Check(p clusterprofile.ClusterProfile) Report {
	var out []Result

	// The single-valued profile fields. All are pointer fields: present is
	// the nil/non-nil pointer test (a nil component is *absent*, not unknown —
	// that distinction is the whole point), and version reads the value only
	// when present.
	apiComponents := []struct {
		name    string
		present func() bool
		version func() string
	}{
		{"kubernetes", func() bool { return p.Kubernetes != nil },
			func() string { return p.Kubernetes.Version }},
		{"gateway-api", func() bool { return p.GatewayAPI != nil },
			func() string { return p.GatewayAPI.Version }},
		{"cert-manager", func() bool { return p.CertManager != nil },
			func() string { return p.CertManager.Version }},
		{"cnpg", func() bool { return p.CloudNativePG != nil },
			func() string { return p.CloudNativePG.Version }},
		{"flux", func() bool { return p.Flux != nil },
			func() string { return p.Flux.Version }},
		{"argo-cd", func() bool { return p.ArgoCD != nil },
			func() string { return p.ArgoCD.Version }},
		{"metrics-server", func() bool { return p.MetricsServer != nil },
			func() string { return p.MetricsServer.Version }},
		{"prometheus", func() bool { return p.Prometheus != nil },
			func() string { return p.Prometheus.Version }},
	}

	for _, c := range apiComponents {
		if !c.present() {
			// Absent is not a version-skew problem; whether a missing cert-manager
			// is allowed is the renderer's call, not this package's.
			continue
		}
		out = append(out, checkField(p, c.name, c.version()))
	}

	for i := range p.PolicyEngines {
		pg := &p.PolicyEngines[i]
		if _, tracked := Lookup(pg.Name); !tracked {
			// An engine the matrix does not track has no floor to enforce.
			continue
		}
		out = append(out, checkField(p, pg.Name, pg.Version))
	}

	return Report{Results: out}
}

// checkField records the verdict for a component the matrix tracks. It assumes
// name is present in the matrix (Check filters untracked names first).
func checkField(p clusterprofile.ClusterProfile, name, found string) Result {
	comp, _ := Lookup(name)

	r := Result{Component: comp.Name, Found: found, Required: comp.Minimum, Degrade: comp.Degrade}

	gap := gapCovers(p.Incomplete, comp.GapField)

	// A detection gap hides the component: we cannot distinguish absent from
	// too-old, so it is Unknown rather than a guess.
	if gap {
		r.Outcome = Unknown
		r.Message = unknownMessage(comp.Name, "hidden by a detection gap: "+gapReason(p.Incomplete, comp.GapField))
		return r
	}

	if found == "" {
		r.Outcome = Unknown
		r.Message = unknownMessage(comp.Name, "no version was reported")
		return r
	}

	req, ok := parseVersion(comp.Minimum)
	if !ok {
		// A corrupt matrix row must not silently pass or fail every profile;
		// surface it as Unknown so the model's data bug is visible.
		r.Outcome = Unknown
		r.Message = unknownMessage(comp.Name, "the support matrix declares an unparseable minimum "+comp.Minimum)
		return r
	}
	has, ok := parseVersion(found)
	if !ok {
		r.Outcome = Unknown
		r.Message = unknownMessage(comp.Name, "unparseable version "+found)
		return r
	}

	switch {
	case has.compare(req) >= 0:
		r.Outcome = Supported
		r.Message = "supported"
	case comp.Degrade == DegradeRenderOlder:
		r.Outcome = Unsupported
		r.Message = decidedMessage(comp.Name, found, comp.Minimum, "render the older API")
	default:
		r.Outcome = Unsupported
		r.Message = decidedMessage(comp.Name, found, comp.Minimum, "refuse with a reason")
	}
	return r
}

// unknownMessage renders the verdict for a version we could not judge.
func unknownMessage(component, why string) string {
	return component + " version unknown (" + why + "); neither confirmed supported nor rejected"
}

// decidedMessage renders the Unsupported verdict, naming the component, the
// version found and the version required, and what the matrix's degradation
// decision is. This is the message the issue's acceptance criterion asks to
// see at preview time for a too-old cert-manager.
func decidedMessage(component, found, required, degrade string) string {
	return component + " " + found + " is below the minimum supported version " + required +
		" needed to serve the API kelson renders against; decision: " + degrade
}

// gapReason returns the first gap Reason matching root, for the Unknown message.
func gapReason(gaps []clusterprofile.Gap, root string) string {
	for _, g := range gaps {
		if g.Field == root || len(g.Field) > len(root) && g.Field[:len(root)] == root && g.Field[len(root)] == '.' {
			return g.Reason
		}
	}
	return ""
}

// Supported returns the results that passed.
func (r Report) Supported() []Result { return r.filter(Supported) }

// Unsupported returns the results that are too old.
func (r Report) Unsupported() []Result { return r.filter(Unsupported) }

// Unknown returns the results we could not judge.
func (r Report) Unknown() []Result { return r.filter(Unknown) }

func (r Report) filter(o Outcome) []Result {
	var out []Result
	for _, res := range r.Results {
		if res.Outcome == o {
			out = append(out, res)
		}
	}
	return out
}
