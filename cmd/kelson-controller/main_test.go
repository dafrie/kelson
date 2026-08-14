package main

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
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
	if cfg.leaderElect {
		t.Error("leader election must be off by default while the controller only writes statuses")
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
		t.Errorf("cluster profile is %q, want empty: the controller has not looked at the cluster", cfg.clusterProfile)
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
	if !strings.Contains(profileBanner(config{}), "empty") {
		t.Errorf("profile banner must say the profile is empty, got %q", profileBanner(config{}))
	}
	if !strings.Contains(profileBanner(config{clusterProfile: "/etc/p.yaml"}), "/etc/p.yaml") {
		t.Error("profile banner must name the file it read")
	}
}
