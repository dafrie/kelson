package buildpacks

import (
	"context"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/dafrie/kelson/internal/build"
)

// The private-build fix as the lifecycle driver renders it (ADR-0033 decision
// 5). It is the same shape buildkit's is — one Secret, one mount, on the clone
// container only — and the point of asserting it twice is that the two drivers
// share the script and the constants but render their own pod specs.

const cloneToken = "ghs_buildpacks_clone_probe_token"

type fakeCloneAuth struct{ cred build.CloneCredential }

func (f fakeCloneAuth) CloneCredential(context.Context, build.Request) (build.CloneCredential, error) {
	return f.cred, nil
}

func TestWorkloadWithCredentialMountsItOnTheCloneOnly(t *testing.T) {
	manifest, err := Config{Namespace: testNS}.WorkloadWithCredential(srcReq(),
		build.CloneCredential{Username: "x-access-token", Password: cloneToken})
	if err != nil {
		t.Fatalf("WorkloadWithCredential: %v", err)
	}
	docs := strings.SplitN(string(manifest), "\n---\n", 2)
	if len(docs) != 2 {
		t.Fatalf("expected a Secret and a Job:\n%s", manifest)
	}
	secretDoc, jobDoc := docs[0], docs[1]

	if !strings.Contains(secretDoc, "kind: Secret") || !strings.Contains(secretDoc, cloneToken) {
		t.Fatalf("the Secret must carry the credential:\n%s", secretDoc)
	}
	if strings.Contains(jobDoc, cloneToken) {
		t.Fatalf("the credential reached the Job manifest:\n%s", jobDoc)
	}
	for _, want := range []string{
		"name: " + build.CloneCredentialVolume,
		"secretName: " + build.CloneSecretName(jobName(srcReq())),
		"mountPath: " + build.CloneCredentialDir,
		build.CloneCredentialDir + "/" + build.CloneCredentialPasswordKey,
	} {
		if !strings.Contains(jobDoc, want) {
			t.Errorf("the Job must contain %q\n%s", want, jobDoc)
		}
	}
	// Only the clone sees it. The lifecycle container that follows would be one
	// layer away from baking the credential into the image.
	var job struct {
		Spec struct {
			Template struct {
				Spec struct {
					Containers []struct {
						Name         string `yaml:"name"`
						VolumeMounts []struct {
							Name string `yaml:"name"`
						} `yaml:"volumeMounts"`
					} `yaml:"containers"`
				} `yaml:"spec"`
			} `yaml:"template"`
		} `yaml:"spec"`
	}
	if err := yaml.Unmarshal([]byte(jobDoc), &job); err != nil {
		t.Fatalf("decoding the Job: %v", err)
	}
	if len(job.Spec.Template.Spec.Containers) == 0 {
		t.Fatalf("the Job declares no build container:\n%s", jobDoc)
	}
	for _, c := range job.Spec.Template.Spec.Containers {
		for _, m := range c.VolumeMounts {
			if m.Name == build.CloneCredentialVolume {
				t.Errorf("container %q mounts the clone credential; only the clone may see it", c.Name)
			}
		}
	}
}

func TestDriverSubmitsTheSecretBesideTheJob(t *testing.T) {
	cluster := &fakeCluster{}
	d, err := New(Options{
		Cluster:   cluster,
		CloneAuth: fakeCloneAuth{cred: build.CloneCredential{Username: "x-access-token", Password: cloneToken}},
		Config:    Config{Namespace: testNS},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := d.Build(context.Background(), srcReq(), nil); err != nil {
		t.Fatalf("Build: %v", err)
	}
	if len(cluster.submitted) != 1 {
		t.Fatalf("submitted %d manifests, want 1", len(cluster.submitted))
	}
	manifest := string(cluster.submitted[0])
	if !strings.Contains(manifest, "kind: Secret") || !strings.Contains(manifest, "kind: Job") {
		t.Fatalf("the submitted manifest must declare both objects:\n%s", manifest)
	}
}
