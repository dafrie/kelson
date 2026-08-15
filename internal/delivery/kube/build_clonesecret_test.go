package kube_test

import (
	"bytes"
	"context"
	"strings"
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/dafrie/kelson/internal/build"
	"github.com/dafrie/kelson/internal/delivery/kube"
)

// The per-run clone credential's lifecycle (ADR-0033 decision 5): it is
// created with the build, owned by the build's Job, and gone when the build is.

const (
	cloneSecretName = testJobName + "-clone"
	cloneProbeToken = "ghs_executor_clone_probe_token"
)

// credentialManifest is what a driver submits for a private build: the Secret
// first, then the Job that projects it.
func credentialManifest(t *testing.T) []byte {
	t.Helper()
	secret, err := build.CloneSecretManifest(cloneSecretName, testNS,
		map[string]string{"kelson.dev/project": "shop"},
		build.CloneCredential{Username: "x-access-token", Password: cloneProbeToken})
	if err != nil {
		t.Fatalf("CloneSecretManifest: %v", err)
	}
	return append(append(secret, []byte("---\n")...), jobManifest(t)...)
}

// stampUIDs makes the fake behave like an API server in the one respect this
// test depends on: a created object comes back with a UID. An ownerReference
// without one is rejected by a real API server, so the executor deliberately
// does not write one — and a fake that never assigns UIDs would make the
// assertion below vacuous in both directions.
func stampUIDs(cli *fake.Clientset) {
	cli.PrependReactor("create", "jobs", func(action k8stesting.Action) (bool, runtime.Object, error) {
		job, ok := action.(k8stesting.CreateAction).GetObject().(*batchv1.Job)
		if !ok {
			return false, nil, nil
		}
		job.UID = types.UID("uid-" + job.Name)
		return false, nil, nil
	})
}

func TestSubmitWritesTheCloneSecretAndTheJobOwnsIt(t *testing.T) {
	cli := newFakeBuildCluster(t, testBuildLogs, buildPod())
	stampUIDs(cli)
	ctx := context.Background()

	name, err := kube.NewBuildExecutor(cli).Submit(ctx, credentialManifest(t))
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	if name != testJobName {
		t.Fatalf("Submit returned %q, want the Job's name", name)
	}

	secret, err := cli.CoreV1().Secrets(testNS).Get(ctx, cloneSecretName, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("the per-run Secret was not written: %v", err)
	}
	if got := string(secret.Data[build.CloneCredentialPasswordKey]); got != cloneProbeToken {
		if got := secret.StringData[build.CloneCredentialPasswordKey]; got != cloneProbeToken {
			t.Errorf("the Secret does not carry the credential: %v", secret.StringData)
		}
	}
	if len(secret.OwnerReferences) != 1 || secret.OwnerReferences[0].Kind != "Job" {
		t.Fatalf("ownerReferences = %v, want the build's Job: the Secret must die with it even if this "+
			"process never reaches Wait", secret.OwnerReferences)
	}
	if secret.OwnerReferences[0].Name != testJobName {
		t.Errorf("owner = %q, want %q", secret.OwnerReferences[0].Name, testJobName)
	}
}

// The credential must not outlive the build on any terminal path — success,
// failure or timeout — and the ownerReference is a backstop, not the plan.
func TestWaitDeletesTheCloneSecretOnEveryOutcome(t *testing.T) {
	cases := []struct {
		name   string
		cond   batchv1.JobConditionType
		reason string
	}{
		{"success", batchv1.JobComplete, ""},
		{"failure", batchv1.JobFailed, "BackoffLimitExceeded"},
		{"timeout", batchv1.JobFailed, batchv1.JobReasonDeadlineExceeded},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			logs := testBuildLogs + "#22 pushing manifest for " + testRepo + "@" + overflowDgst + " done\n"
			cli := newFakeBuildCluster(t, logs, buildPod())
			ctx := context.Background()
			ex := kube.NewBuildExecutor(cli)

			if _, err := ex.Submit(ctx, credentialManifest(t)); err != nil {
				t.Fatalf("Submit: %v", err)
			}
			markJobTerminal(t, cli, tc.cond, tc.reason)
			// The outcome itself is asserted elsewhere; what matters here is
			// that the credential is gone either way.
			_, _ = ex.Wait(ctx, testJobName, &bytes.Buffer{})

			_, err := cli.CoreV1().Secrets(testNS).Get(ctx, cloneSecretName, metav1.GetOptions{})
			if !apierrors.IsNotFound(err) {
				t.Errorf("the clone credential survived a %s build (err=%v)", tc.name, err)
			}
		})
	}
}

// A resubmitted build re-mints, and the old value is the one about to expire:
// the Secret is refreshed rather than adopted.
func TestSubmitRefreshesAnExistingCloneSecret(t *testing.T) {
	cli := newFakeBuildCluster(t, testBuildLogs, buildPod())
	ctx := context.Background()
	ex := kube.NewBuildExecutor(cli)

	if _, err := ex.Submit(ctx, credentialManifest(t)); err != nil {
		t.Fatalf("first Submit: %v", err)
	}
	fresh, err := build.CloneSecretManifest(cloneSecretName, testNS, nil,
		build.CloneCredential{Username: "x-access-token", Password: "ghs_second_mint"})
	if err != nil {
		t.Fatalf("CloneSecretManifest: %v", err)
	}
	if _, err := ex.Submit(ctx, append(append(fresh, []byte("---\n")...), jobManifest(t)...)); err != nil {
		t.Fatalf("second Submit: %v", err)
	}

	secret, err := cli.CoreV1().Secrets(testNS).Get(ctx, cloneSecretName, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get secret: %v", err)
	}
	if secret.StringData[build.CloneCredentialPasswordKey] != "ghs_second_mint" {
		t.Errorf("a resubmitted build kept the stale credential: %v", secret.StringData)
	}
}

// The decoder is the last thing between a rendered manifest and `create`, so a
// kind it does not expect is a sentence rather than a silent drop.
func TestSubmitRefusesAManifestThatIsNotAJobAndItsSecrets(t *testing.T) {
	cli := newFakeBuildCluster(t, testBuildLogs)
	manifest := append([]byte("apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: nope\n  namespace: apps\n---\n"), jobManifest(t)...)

	_, err := kube.NewBuildExecutor(cli).Submit(context.Background(), manifest)
	if err == nil {
		t.Fatal("a manifest declaring a ConfigMap was accepted")
	}
	if !strings.Contains(err.Error(), "ConfigMap") {
		t.Errorf("the refusal must name the kind it refused: %v", err)
	}
}

func TestSubmitStillAcceptsASingleJob(t *testing.T) {
	cli := newFakeBuildCluster(t, testBuildLogs)
	name, err := kube.NewBuildExecutor(cli).Submit(context.Background(), jobManifest(t))
	if err != nil {
		t.Fatalf("a public build submits one document and must keep working: %v", err)
	}
	if name != testJobName {
		t.Errorf("Submit returned %q", name)
	}
}
