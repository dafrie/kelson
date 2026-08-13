package buildkit

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/dafrie/kelson/internal/build"
	"github.com/dafrie/kelson/internal/redact"
)

// Build-time secrets: the mount reaches the build, the value never reaches an
// output surface (ADR-0009, issue #117).

// buildSecretValue is the value a real Kubernetes Secret would hold. It is
// declared here so the assertions can grep for it, and it deliberately never
// enters any kelson type: SecretMount has no field that could carry it.
const buildSecretValue = "npm-t0ken-SENTINEL-never-print-b41d"

func secretRequest() build.Request {
	return build.Request{
		Project:     "shop",
		Environment: "production",
		SourceGit:   "https://github.com/acme/shop.git",
		SourceRef:   "9f1c2b3a4d5e6f70819a2b3c4d5e6f7081920a1b",
		Image:       "ghcr.io/acme/shop",
		Tag:         "shop-9f1c2b3a",
		Revision:    "9f1c2b3a4d5e6f70819a2b3c4d5e6f7081920a1b",
	}
}

// TestWorkloadMountsSecretsAsBuildKitSecretsNotBuildArgs is the acceptance test
// for ADR-0009's build half: the credential reaches the build through
// --secret / a projected volume, and the manifest carries only references.
func TestWorkloadMountsSecretsAsBuildKitSecretsNotBuildArgs(t *testing.T) {
	cfg := Config{
		Namespace: "builds",
		Secrets: []SecretMount{
			{ID: "npm-token", SecretName: "npm-credentials", Key: "token"},
			{ID: "ca-bundle", SecretName: "corp-ca"},
		},
	}
	out, err := cfg.Workload(secretRequest())
	if err != nil {
		t.Fatalf("Workload: %v", err)
	}
	got := string(out)

	if strings.Contains(got, buildSecretValue) {
		t.Fatalf("a secret value reached the rendered Job:\n%s", got)
	}
	for _, want := range []string{
		"--secret id=ca-bundle,src=/run/kelson/secrets/ca-bundle/ca-bundle",
		"--secret id=npm-token,src=/run/kelson/secrets/npm-token/npm-token",
		"secretName: npm-credentials",
		"secretName: corp-ca",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("the rendered Job is missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "--build-arg npm") || strings.Contains(got, "--build-arg ca") {
		t.Errorf("a secret was passed as a build argument:\n%s", got)
	}
	// Key defaults to the id, so the common case needs no configuration.
	if !strings.Contains(got, "key: token") || !strings.Contains(got, "key: ca-bundle") {
		t.Errorf("the projected keys are wrong:\n%s", got)
	}
	// The projection is read-only and not world-readable.
	if !strings.Contains(got, "defaultMode: 256") {
		t.Errorf("a projected build secret is not mode 0400:\n%s", got)
	}
}

// Rendering is a pure function everywhere else in this package, and the mounts
// must not be the exception: a set order that depends on map iteration would
// make the Job manifest differ between two identical builds.
func TestWorkloadSecretMountsAreOrderIndependent(t *testing.T) {
	a := Config{Namespace: "builds", Secrets: []SecretMount{
		{ID: "zulu", SecretName: "s1"}, {ID: "alpha", SecretName: "s2"},
	}}
	b := Config{Namespace: "builds", Secrets: []SecretMount{
		{ID: "alpha", SecretName: "s2"}, {ID: "zulu", SecretName: "s1"},
	}}
	first, err := a.Workload(secretRequest())
	if err != nil {
		t.Fatalf("Workload: %v", err)
	}
	second, err := b.Workload(secretRequest())
	if err != nil {
		t.Fatalf("Workload: %v", err)
	}
	if !bytes.Equal(first, second) {
		t.Fatalf("mount order changed the rendered Job:\n%s\n---\n%s", first, second)
	}
}

func TestWorkloadRefusesMalformedSecretMounts(t *testing.T) {
	tests := []struct {
		name   string
		mounts []SecretMount
	}{
		{"no id", []SecretMount{{SecretName: "s"}}},
		{"no secret name", []SecretMount{{ID: "npm-token"}}},
		{"invalid id", []SecretMount{{ID: "NPM Token", SecretName: "s"}}},
		{"duplicate id", []SecretMount{{ID: "a", SecretName: "s1"}, {ID: "a", SecretName: "s2"}}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := Config{Namespace: "builds", Secrets: tc.mounts}
			if _, err := cfg.Workload(secretRequest()); err == nil {
				t.Fatal("want a refusal, got a rendered Job")
			}
		})
	}
}

// A build arg is baked into image history and into the Job's command line, so
// a credential-shaped name is refused outright rather than redacted afterwards
// (ADR-0009's other documented Coolify gap).
func TestWorkloadRefusesCredentialShapedBuildArgs(t *testing.T) {
	for _, name := range []string{"NPM_TOKEN", "DB_PASSWORD", "STRIPE_API_KEY", "aws_secret"} {
		t.Run(name, func(t *testing.T) {
			req := secretRequest()
			req.Args = map[string]string{name: buildSecretValue}
			cfg := Config{Namespace: "builds"}
			out, err := cfg.Workload(req)
			if err == nil {
				t.Fatalf("build arg %q was accepted:\n%s", name, out)
			}
			if !strings.Contains(err.Error(), name) {
				t.Errorf("the refusal does not name the offending argument: %v", err)
			}
			if !strings.Contains(err.Error(), "--mount=type=secret") {
				t.Errorf("the refusal does not say what to do instead: %v", err)
			}
		})
	}
}

func TestWorkloadStillAcceptsOrdinaryBuildArgs(t *testing.T) {
	req := secretRequest()
	req.Args = map[string]string{"BUILDKIT_INLINE_CACHE": "1", "NODE_ENV": "production"}
	out, err := Config{Namespace: "builds"}.Workload(req)
	if err != nil {
		t.Fatalf("an ordinary build arg was refused: %v", err)
	}
	if !strings.Contains(string(out), "--build-arg NODE_ENV=production") {
		t.Errorf("the build arg did not reach buildctl:\n%s", out)
	}
}

// TestBuildLogNeverCarriesAResolvedCredential: the other half of the property.
// A mounted Secret is never in this process, but a *resolved* registry
// credential is (registry.Resolver, #51), and buildkitd has been known to print
// its auth config. Once the value is registered it cannot reach the stream.
func TestBuildLogNeverCarriesAResolvedCredential(t *testing.T) {
	const pushPassword = "ghp-push-cred-SENTINEL-never-print-1c9e"
	redact.Register(pushPassword)

	fc := &fakeCluster{
		// The builder echoing its own auth config, split so the value would
		// straddle nothing in particular — the writer has to catch it anyway.
		logs:   "#1 resolving auth for ghcr.io\n#2 auth={\"password\":\"" + pushPassword + "\"}\n#3 DONE\n",
		result: build.Result{Reference: "ghcr.io/acme/shop@" + testDigest, Digest: testDigest},
	}
	d := newDriver(t, fc, Config{Namespace: "builds", PushSecret: "ghcr-push"})

	var logs bytes.Buffer
	if _, err := d.Build(context.Background(), secretRequest(), &logs); err != nil {
		t.Fatalf("Build: %v", err)
	}
	if strings.Contains(logs.String(), pushPassword) {
		t.Fatalf("a resolved credential reached the build log:\n%s", logs.String())
	}
	if !strings.Contains(logs.String(), redact.Sentinel) {
		t.Errorf("the log was not marked where the value was:\n%s", logs.String())
	}
	// Everything else the build said still gets through: redaction must not
	// truncate a log stream.
	for _, want := range []string{"#1 resolving auth for ghcr.io", "#3 DONE"} {
		if !strings.Contains(logs.String(), want) {
			t.Errorf("redaction swallowed build output %q:\n%s", want, logs.String())
		}
	}
}

// The mounted-secret case has nothing to scrub, and that is the point: kelson
// never held the value, so it cannot leak it and cannot redact what the build
// itself chooses to print.
func TestMountedSecretValueIsNeverInTheProcess(t *testing.T) {
	fc := &fakeCluster{
		logs:   "#1 [build 2/3] RUN --mount=type=secret,id=npm-token npm ci\n#2 DONE\n",
		result: build.Result{Reference: "ghcr.io/acme/shop@" + testDigest, Digest: testDigest},
	}
	d := newDriver(t, fc, Config{
		Namespace: "builds",
		Secrets:   []SecretMount{{ID: "npm-token", SecretName: "npm-credentials", Key: "token"}},
	})

	var logs bytes.Buffer
	if _, err := d.Build(context.Background(), secretRequest(), &logs); err != nil {
		t.Fatalf("Build: %v", err)
	}
	if strings.Contains(logs.String(), buildSecretValue) {
		t.Fatalf("a mounted secret value reached the build log:\n%s", logs.String())
	}
	if strings.Contains(string(fc.submitted()), buildSecretValue) {
		t.Fatalf("a mounted secret value reached the Job manifest:\n%s", fc.submitted())
	}
	if !strings.Contains(string(fc.submitted()), "secretName: npm-credentials") {
		t.Fatalf("the mount did not reach the build:\n%s", fc.submitted())
	}
}
