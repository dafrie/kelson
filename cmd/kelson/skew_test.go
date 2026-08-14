package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/dafrie/kelson/internal/clusterprofile"
)

// TestSkewIsSilentForASupportedCluster: the report says nothing when there is
// nothing to say. A warning on every render would train the reader to skip the
// one that matters.
func TestSkewIsSilentForASupportedCluster(t *testing.T) {
	var b bytes.Buffer
	writeSkew(&b, clusterprofile.ClusterProfile{
		Kubernetes:    &clusterprofile.Kubernetes{Version: "v1.31.2"},
		CloudNativePG: &clusterprofile.CloudNativePG{Version: "1.30.0"},
	})
	if b.Len() != 0 {
		t.Errorf("supported cluster produced output:\n%s", b.String())
	}
}

// TestSkewNilWriterIsSafe: the internal renders that pass nil must not panic,
// and must not find another way to print.
func TestSkewNilWriterIsSafe(t *testing.T) {
	writeSkew(nil, clusterprofile.ClusterProfile{
		CloudNativePG: &clusterprofile.CloudNativePG{Version: "1.10.0"},
	})
}

// TestSkewWarnsWithTheNamedDegradation: a too-old operator is reported as a
// warning that names the component, both versions, and what stops working.
func TestSkewWarnsWithTheNamedDegradation(t *testing.T) {
	var b bytes.Buffer
	writeSkew(&b, clusterprofile.ClusterProfile{
		CloudNativePG: &clusterprofile.CloudNativePG{Version: "1.10.0"},
	})
	out := b.String()
	for _, want := range []string{"warning: version skew", "[unsupported]", "cnpg 1.10.0", "1.23.0", "kind: postgres"} {
		if !strings.Contains(out, want) {
			t.Errorf("skew report missing %q:\n%s", want, out)
		}
	}
}

// TestSkewNoteIsNotAWarning: a cluster newer than tested leads with "note",
// because nothing is wrong and calling it a warning would make the vocabulary
// useless for the case where something is.
func TestSkewNoteIsNotAWarning(t *testing.T) {
	var b bytes.Buffer
	writeSkew(&b, clusterprofile.ClusterProfile{
		Kubernetes: &clusterprofile.Kubernetes{Version: "v1.99.0"},
	})
	out := b.String()
	if !strings.HasPrefix(out, "note: version skew") {
		t.Errorf("newer-than-tested report should lead with a note:\n%s", out)
	}
	if !strings.Contains(out, "[note]") {
		t.Errorf("statement is not labelled a note:\n%s", out)
	}
}

// TestRenderReportsSkewOnStderr is the acceptance criterion at the CLI
// boundary: an unsupported combination is reported when the profile is
// resolved — preview time — and never on stdout, which is a manifest pipe.
func TestRenderReportsSkewOnStderr(t *testing.T) {
	project, env := examplesHello(t)
	// The same cluster the other CLI render tests use, with cert-manager added
	// at a version below its floor.
	profile := profileFile(t, gatewayProfileYAML+"certManager:\n  version: v1.9.0\n  clusterIssuers: [letsencrypt]\n")

	stdout, stderr, err := runKelson(t, "render", "-f", project, "-f", env, "--profile", profile)
	if err != nil {
		t.Fatalf("render failed: %v\n%s", err, stderr)
	}
	for _, want := range []string{"warning: version skew", "cert-manager v1.9.0", "1.14.0", "TLS"} {
		if !strings.Contains(stderr, want) {
			t.Errorf("stderr missing %q:\n%s", want, stderr)
		}
	}
	if strings.Contains(stdout, "version skew") {
		t.Errorf("the skew report leaked into stdout, which is a manifest pipe:\n%s", stdout)
	}
	// Reported, not refused: a too-old cert-manager must not stop a render that
	// the specific per-capability judgements have not refused.
	if !strings.Contains(stdout, "kind: Deployment") {
		t.Errorf("render produced no manifests:\n%s", stdout)
	}
}

// TestDiffReportsSkewOnce: `kelson diff --from` resolves the same profile
// twice, and the second resolve must stay quiet — two identical warnings read
// as two different findings.
func TestDiffReportsSkewOnce(t *testing.T) {
	project, env := examplesHello(t)
	profile := profileFile(t, gatewayProfileYAML+"certManager:\n  version: v1.9.0\n  clusterIssuers: [letsencrypt]\n")

	_, stderr, _ := runKelson(t, "diff", "-f", project, "-f", env, "--profile", profile, "--from", project)
	if got := strings.Count(stderr, "warning: version skew"); got != 1 {
		t.Errorf("the skew report appeared %d times, want 1:\n%s", got, stderr)
	}
}
