package main

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dafrie/kelson/internal/clusterprofile"
	"github.com/dafrie/kelson/internal/controller"
)

// The command's own surface: what the flags resolve to, and what the process
// refuses before it touches a cluster. Everything below the flags is covered by
// internal/controller; what is worth pinning here is that a mistake in the
// invocation is caught at startup rather than by a controller that started and
// then did the wrong thing quietly.

func TestParseFlagsDefaults(t *testing.T) {
	cfg, err := parseFlags(nil, io.Discard)
	if err != nil {
		t.Fatalf("parseFlags: %v", err)
	}
	if !cfg.leaderElect {
		t.Error("leader election must be on by default: the controller publishes artifacts and applies Flux objects, and two replicas racing to do either is a real hazard")
	}
	if cfg.metricsAddr != "0" {
		t.Errorf("metrics bind address is %q, want \"0\" (disabled): an unscraped port is exposure for no benefit", cfg.metricsAddr)
	}
	if cfg.probeAddr == "" {
		t.Error("the health probe address must have a default: a Deployment's probes need somewhere to go")
	}
	if cfg.leaderElectionNamespace != defaultNamespace {
		t.Errorf("leader-election namespace is %q, want %q", cfg.leaderElectionNamespace, defaultNamespace)
	}
	if cfg.clusterProfile != "" {
		t.Errorf("cluster profile is %q, want empty: an unset path means the controller probes the cluster", cfg.clusterProfile)
	}
	if cfg.fluxNamespace != controller.DefaultFluxNamespace {
		t.Errorf("flux namespace is %q, want %q", cfg.fluxNamespace, controller.DefaultFluxNamespace)
	}
	if cfg.registryConfig != defaultRegistryConfig {
		t.Errorf("registry config is %q, want the mounted default %q", cfg.registryConfig, defaultRegistryConfig)
	}
	if cfg.reconcileInterval != controller.DefaultInterval {
		t.Errorf("reconcile interval is %s, want %s", cfg.reconcileInterval, controller.DefaultInterval)
	}
	// An unset registry is not a startup failure: the controller must be able to
	// run and *say* it has nowhere to publish (ADR-0028, "Consequences").
	if cfg.registry != "" {
		t.Errorf("registry defaulted to %q; it has no default", cfg.registry)
	}
	if cfg.pullSecret != "" || len(cfg.insecureRegistries) != 0 {
		t.Errorf("pull secret %q / insecure registries %v must default to nothing", cfg.pullSecret, cfg.insecureRegistries)
	}
}

// TestParseFlagsDeliveryKnobs: the publish half of the command line, which is
// what turns a controller that renders into one that deploys.
func TestParseFlagsDeliveryKnobs(t *testing.T) {
	cfg, err := parseFlags([]string{
		"--registry=ghcr.io/acme",
		"--registry-config=/tmp/config.json",
		"--insecure-registries=localhost:5000, registry.internal",
		"--flux-namespace=flux-system",
		"--pull-secret=ghcr-pull",
		"--reconcile-interval=90s",
	}, io.Discard)
	if err != nil {
		t.Fatalf("parseFlags: %v", err)
	}
	if cfg.registry != "ghcr.io/acme" || cfg.registryConfig != "/tmp/config.json" {
		t.Errorf("registry = %q, config = %q", cfg.registry, cfg.registryConfig)
	}
	if len(cfg.insecureRegistries) != 2 || cfg.insecureRegistries[0] != "localhost:5000" {
		t.Errorf("insecure registries = %v, want the two hosts with the whitespace trimmed", cfg.insecureRegistries)
	}
	if cfg.fluxNamespace != "flux-system" || cfg.pullSecret != "ghcr-pull" {
		t.Errorf("flux namespace = %q, pull secret = %q", cfg.fluxNamespace, cfg.pullSecret)
	}
	if cfg.reconcileInterval != 90*time.Second {
		t.Errorf("reconcile interval = %s", cfg.reconcileInterval)
	}
}

// TestParseFlagsReadsTheEnvironment: an operator sets the destination once in
// the Deployment, in the same variables `kelson build` and kelson-server read.
func TestParseFlagsReadsTheEnvironment(t *testing.T) {
	t.Setenv(registryEnv, "registry.internal/team")
	t.Setenv(insecureRegistriesEnv, "registry.internal")
	t.Setenv(fluxNamespaceEnv, "gitops")
	t.Setenv(pullSecretEnv, "pull")

	cfg, err := parseFlags(nil, io.Discard)
	if err != nil {
		t.Fatalf("parseFlags: %v", err)
	}
	if cfg.registry != "registry.internal/team" || cfg.fluxNamespace != "gitops" || cfg.pullSecret != "pull" {
		t.Errorf("the environment was not read: %+v", cfg)
	}
	if len(cfg.insecureRegistries) != 1 {
		t.Errorf("insecure registries = %v", cfg.insecureRegistries)
	}
	// A flag still wins over the environment.
	cfg, err = parseFlags([]string{"--registry=ghcr.io/acme"}, io.Discard)
	if err != nil {
		t.Fatalf("parseFlags: %v", err)
	}
	if cfg.registry != "ghcr.io/acme" {
		t.Errorf("the flag did not win over $%s: %q", registryEnv, cfg.registry)
	}
}

func TestParseFlagsRefusals(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want string
	}{
		{
			name: "a positional argument",
			args: []string{"reconcile"},
			want: "takes flags only",
		},
		{
			name: "leader election with nowhere to put the Lease",
			args: []string{"--leader-elect", "--leader-election-namespace="},
			want: "where the Lease lives",
		},
		{
			name: "an unknown flag",
			args: []string{"--reconcile-everything"},
			want: "flag provided but not defined",
		},
		{
			name: "nowhere for the Flux objects to live",
			args: []string{"--flux-namespace="},
			want: "must not be empty",
		},
		{
			name: "a reconcile interval Flux would reject",
			args: []string{"--reconcile-interval=0"},
			want: "must be positive",
		},
		{
			// A scheme or a path here would be written into an OCIRepository's
			// insecure list, match no registry at all, and surface as a TLS
			// error pointing at the wrong thing.
			name: "an insecure registry carrying a scheme",
			args: []string{"--insecure-registries=http://localhost:5000"},
			want: "must not carry a scheme",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := parseFlags(tc.args, io.Discard)
			if err == nil {
				t.Fatalf("%v was accepted", tc.args)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q does not mention %q", err, tc.want)
			}
		})
	}
}

// TestLoadProfileMissingFileIsAnError: an operator who passed --cluster-profile
// asked for that document. Falling back to an empty profile would render
// manifests missing every optional resource, and they would look intentional.
func TestLoadProfileMissingFileIsAnError(t *testing.T) {
	if _, err := loadProfile(filepath.Join(t.TempDir(), "absent.yaml")); err == nil {
		t.Fatal("a missing --cluster-profile file must fail at startup")
	}
	profile, err := loadProfile("")
	if err != nil {
		t.Fatalf("an unset --cluster-profile must be an empty profile, got %v", err)
	}
	if profile.Kubernetes != nil {
		t.Errorf("an unset --cluster-profile must produce an empty profile, got %+v", profile)
	}
}

func TestLoadProfileReadsADocument(t *testing.T) {
	path := filepath.Join(t.TempDir(), "profile.yaml")
	const doc = "kubernetes:\n  version: v1.34.1\n"
	if err := os.WriteFile(path, []byte(doc), 0o600); err != nil {
		t.Fatalf("writing the fixture: %v", err)
	}
	profile, err := loadProfile(path)
	if err != nil {
		t.Fatalf("loadProfile: %v", err)
	}
	if profile.Kubernetes == nil || profile.Kubernetes.Version != "v1.34.1" {
		t.Fatalf("the profile did not load: %+v", profile)
	}
}

// TestBannersSayThePosture: both defaults are choices an operator can be
// surprised by, so the process says them out loud on stdout — the same reason
// kelson-server names its authentication posture there.
func TestBannersSayThePosture(t *testing.T) {
	off := leaderBanner(config{})
	if !strings.Contains(off, "off") {
		t.Errorf("leader banner %q does not say the election is off", off)
	}
	on := leaderBanner(config{leaderElect: true, leaderElectionNamespace: "kelson-system"})
	if !strings.Contains(on, leaderElectionID) || !strings.Contains(on, "kelson-system") {
		t.Errorf("leader banner %q must name the Lease and its namespace", on)
	}
	if !strings.Contains(profileBanner(config{}), "detected") {
		t.Errorf("profile banner must say the profile was detected, got %q", profileBanner(config{}))
	}
	if !strings.Contains(profileBanner(config{clusterProfile: "/etc/p.yaml"}), "/etc/p.yaml") {
		t.Error("profile banner must name the file it read")
	}

	// A registry is a hard requirement of the spine, so a controller running
	// without one says so on stdout instead of discovering it per environment.
	if got := registryBanner(config{}); !strings.Contains(got, "unset") {
		t.Errorf("registry banner %q does not say the registry is unset", got)
	}
	if got := registryBanner(config{registry: "ghcr.io/acme"}); !strings.Contains(got, "ghcr.io/acme/kelson") {
		t.Errorf("registry banner %q does not name where artifacts go", got)
	}

	// And a cluster with no Flux gets the sentence that names the fix,
	// including the restart, because nothing in the status could imply it.
	noFlux := fluxBanner(config{fluxNamespace: "kelson-system"}, clusterprofile.ClusterProfile{})
	if !strings.Contains(noFlux, "watches are off") || !strings.Contains(noFlux, "restart") {
		t.Errorf("flux banner %q must say the watches are off and that a restart is needed", noFlux)
	}
	withFlux := fluxBanner(config{fluxNamespace: "kelson-system"},
		clusterprofile.ClusterProfile{Flux: &clusterprofile.Component{}})
	if !strings.Contains(withFlux, "kelson-system") {
		t.Errorf("flux banner %q must name the namespace being watched", withFlux)
	}
}

// TestStartupProfileSurvivesAFailedProbe: a controller that refused to start
// because it could not read one API group would be a controller that cannot
// report the gap. Detection failure costs the Flux watches and nothing else.
func TestStartupProfileSurvivesAFailedProbe(t *testing.T) {
	// An unreadable kubeconfig is the cheapest way to make detection fail
	// without a cluster.
	path := filepath.Join(t.TempDir(), "not-a-kubeconfig")
	if err := os.WriteFile(path, []byte("this is not yaml: [\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	profile, source, err := startupProfile(config{kubeconfig: path})
	if err != nil {
		t.Fatalf("a failed probe must not stop the process: %v", err)
	}
	if profile.Flux != nil {
		t.Errorf("a failed probe must not claim a finding: %+v", profile)
	}
	if !strings.Contains(source, "detection failed") {
		t.Errorf("the banner does not say detection failed: %q", source)
	}
}

// TestCacheOptionsScopeTheFluxKinds: kelson owns exactly one pair in one
// namespace, and a cache that listed every Kustomization in the cluster would
// hold every other team's GitOps state in this process.
func TestCacheOptionsScopeTheFluxKinds(t *testing.T) {
	if opts := cacheOptions(config{fluxNamespace: "kelson-system"}, clusterprofile.ClusterProfile{}); len(opts.ByObject) != 0 {
		t.Fatalf("a cluster with no Flux must get no cache entry for an unserved kind, got %d", len(opts.ByObject))
	}
	opts := cacheOptions(config{fluxNamespace: "gitops"},
		clusterprofile.ClusterProfile{Flux: &clusterprofile.Component{}})
	if len(opts.ByObject) != 2 {
		t.Fatalf("got %d scoped kinds, want the OCIRepository and the Kustomization", len(opts.ByObject))
	}
	for obj, by := range opts.ByObject {
		if _, ok := by.Namespaces["gitops"]; !ok || len(by.Namespaces) != 1 {
			t.Errorf("%s is cached across %v, want only the flux namespace", obj.GetObjectKind().GroupVersionKind(), by.Namespaces)
		}
	}
}
