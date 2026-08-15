package buildkit

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/dafrie/kelson/internal/build"
)

// The private-build fix as this driver renders it (ADR-0033 decision 5). The
// value must reach the clone container and nothing else: not the build
// container, not an argument, not the URL, not any other field of the pod spec.

const cloneToken = "ghs_buildkit_clone_probe_token"

func credentialed() build.CloneCredential {
	return build.CloneCredential{Username: "x-access-token", Password: cloneToken}
}

// fakeCloneAuth is the CloneAuth seam under test control.
type fakeCloneAuth struct {
	cred build.CloneCredential
	err  error
	sawe string
}

func (f *fakeCloneAuth) CloneCredential(_ context.Context, req build.Request) (build.CloneCredential, error) {
	f.sawe = req.SourceGit
	return f.cred, f.err
}

func TestWorkloadWithCredentialProjectsItIntoTheCloneOnly(t *testing.T) {
	manifest, err := Config{Namespace: "kelson"}.WorkloadWithCredential(srcReq(), credentialed())
	if err != nil {
		t.Fatalf("WorkloadWithCredential: %v", err)
	}
	secretDoc, jobDoc := splitTwo(t, manifest)

	// The Secret carries the value…
	if !strings.Contains(secretDoc, "kind: Secret") || !strings.Contains(secretDoc, cloneToken) {
		t.Fatalf("the first document must be the Secret carrying the credential:\n%s", secretDoc)
	}
	// …and the Job carries only its name.
	if strings.Contains(jobDoc, cloneToken) {
		t.Fatalf("the credential reached the Job manifest:\n%s", jobDoc)
	}

	var job struct {
		Spec struct {
			Template struct {
				Spec struct {
					InitContainers []struct {
						Name         string   `yaml:"name"`
						Command      []string `yaml:"command"`
						VolumeMounts []struct {
							Name      string `yaml:"name"`
							MountPath string `yaml:"mountPath"`
						} `yaml:"volumeMounts"`
					} `yaml:"initContainers"`
					Containers []struct {
						Name         string `yaml:"name"`
						VolumeMounts []struct {
							Name string `yaml:"name"`
						} `yaml:"volumeMounts"`
					} `yaml:"containers"`
					Volumes []struct {
						Name   string `yaml:"name"`
						Secret *struct {
							SecretName  string `yaml:"secretName"`
							DefaultMode int32  `yaml:"defaultMode"`
						} `yaml:"secret"`
					} `yaml:"volumes"`
				} `yaml:"spec"`
			} `yaml:"template"`
		} `yaml:"spec"`
	}
	if err := yaml.Unmarshal([]byte(jobDoc), &job); err != nil {
		t.Fatalf("decoding the Job: %v", err)
	}
	pod := job.Spec.Template.Spec

	var mounted bool
	for _, c := range pod.InitContainers {
		if c.Name != "clone" {
			continue
		}
		for _, m := range c.VolumeMounts {
			if m.Name == build.CloneCredentialVolume && m.MountPath == build.CloneCredentialDir {
				mounted = true
			}
		}
		for _, arg := range c.Command {
			if strings.Contains(arg, cloneToken) {
				t.Fatal("the credential reached the clone container's command line")
			}
		}
	}
	if !mounted {
		t.Fatalf("the clone container does not mount the credential at %s", build.CloneCredentialDir)
	}

	for _, c := range pod.Containers {
		for _, m := range c.VolumeMounts {
			if m.Name == build.CloneCredentialVolume {
				t.Errorf("container %q mounts the clone credential; only the clone may see it", c.Name)
			}
		}
	}

	var projected bool
	for _, v := range pod.Volumes {
		if v.Name != build.CloneCredentialVolume {
			continue
		}
		projected = true
		if v.Secret == nil || v.Secret.SecretName != build.CloneSecretName("build-shop-web-9f1c0de") {
			t.Errorf("the volume must name the per-run Secret, got %+v", v.Secret)
		}
		if v.Secret != nil && v.Secret.DefaultMode != build.CloneCredentialMode {
			t.Errorf("defaultMode = %o, want %o", v.Secret.DefaultMode, build.CloneCredentialMode)
		}
	}
	if !projected {
		t.Error("no volume projects the per-run Secret")
	}
}

// An empty credential renders exactly what Workload does: one document, no
// Secret, no mount. The golden assertions about a public build are unaffected
// by this whole decision.
func TestWorkloadWithoutACredentialIsTheOldWorkload(t *testing.T) {
	plain, err := Config{Namespace: "kelson"}.Workload(srcReq())
	if err != nil {
		t.Fatalf("Workload: %v", err)
	}
	with, err := Config{Namespace: "kelson"}.WorkloadWithCredential(srcReq(), build.CloneCredential{})
	if err != nil {
		t.Fatalf("WorkloadWithCredential: %v", err)
	}
	if string(plain) != string(with) {
		t.Error("an empty credential changed the rendered Job")
	}
	if strings.Contains(string(plain), build.CloneCredentialVolume) {
		t.Error("an anonymous build must project no credential volume")
	}
}

// A request with no source has nothing to authenticate to, so nothing is
// minted — the case a caller that pre-populates the workspace relies on.
func TestDriverDoesNotMintWithoutASource(t *testing.T) {
	auth := &fakeCloneAuth{cred: credentialed()}
	d, err := New(Options{Cluster: &fakeCluster{}, CloneAuth: auth, Config: Config{Namespace: "kelson"}})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	req := srcReq()
	req.SourceGit = ""
	if _, err := d.Build(context.Background(), req, nil); err != nil {
		t.Fatalf("Build: %v", err)
	}
	if auth.sawe != "" {
		t.Error("a sourceless build asked for a clone credential")
	}
}

// A minting failure fails the build rather than falling back to an anonymous
// fetch: an anonymous fetch of a private repository is a 404 far from its
// cause, which is the failure mode ADR-0033 exists to remove.
func TestDriverFailsWhenTheCredentialCannotBeMinted(t *testing.T) {
	auth := &fakeCloneAuth{err: errors.New("connection acme-github is not reachable")}
	d, err := New(Options{Cluster: &fakeCluster{}, CloneAuth: auth, Config: Config{Namespace: "kelson"}})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := d.Build(context.Background(), srcReq(), nil); err == nil {
		t.Fatal("a build whose credential could not be minted must fail")
	}
}

// The driver submits the Secret beside the Job, and registers the value before
// the build runs — so a builder that echoes the credential into its log has it
// scrubbed on the way out (issue #117), which is the guarantee the log stream
// already makes for every other resolved credential.
func TestDriverSubmitsTheSecretAndRegistersTheValue(t *testing.T) {
	cluster := &fakeCluster{
		logs:   "warning: authenticating as x-access-token with " + cloneToken + "\n",
		result: build.Result{Reference: "ghcr.io/acme/web@sha256:abc", Digest: "sha256:abc"},
	}
	d, err := New(Options{
		Cluster:   cluster,
		CloneAuth: &fakeCloneAuth{cred: credentialed()},
		Config:    Config{Namespace: "kelson"},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	var out bytes.Buffer
	if _, err := d.Build(context.Background(), srcReq(), &out); err != nil {
		t.Fatalf("Build: %v", err)
	}
	manifest := string(cluster.submitted())
	if !strings.Contains(manifest, "kind: Secret") || !strings.Contains(manifest, "kind: Job") {
		t.Fatalf("the submitted manifest must declare both objects:\n%s", manifest)
	}
	if strings.Contains(out.String(), cloneToken) {
		t.Errorf("the credential reached the build log: %q", out.String())
	}
}

func splitTwo(t *testing.T, manifest []byte) (string, string) {
	t.Helper()
	parts := strings.SplitN(string(manifest), "\n---\n", 2)
	if len(parts) != 2 {
		t.Fatalf("expected two documents, got:\n%s", manifest)
	}
	return parts[0], parts[1]
}
