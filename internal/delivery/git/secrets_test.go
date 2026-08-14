package git

import (
	"context"
	"strings"
	"testing"
)

// The two properties the sops backend rests on (issue #81, ADR-0021): a deploy
// never prunes an encrypted Secret, and a secret write never prunes a
// manifest. They are the same repository path written by two commands with
// different ideas of what they own, and getting either wrong loses production
// data — a deploy that pruned would delete every credential in the
// environment, and a `secret set` that pruned would delete the environment.

func secretMsg() Message {
	return Message{Subject: "set secret", Project: "shop", Environment: "production"}
}

func TestDeployDoesNotPruneEncryptedSecrets(t *testing.T) {
	remote := bareRemote(t)
	w := testWriter(t, remote, nil)

	if _, err := w.Put(context.Background(), PutRequest{
		Files:   []File{{Path: SecretPath("checkout-db"), Data: []byte("sops: {}\n")}},
		Message: secretMsg(),
	}); err != nil {
		t.Fatalf("put: %v", err)
	}
	if _, err := w.Write(context.Background(), WriteRequest{
		Files:   []File{{Path: "001-deployment-web.yaml", Data: []byte("kind: Deployment\n")}},
		Message: deployMsg(),
	}); err != nil {
		t.Fatalf("deploy: %v", err)
	}

	tree := remoteTree(t, remote, "main")
	if _, ok := tree["manifests/secrets/checkout-db.enc.yaml"]; !ok {
		t.Fatalf("a deploy pruned an encrypted Secret; the environment just lost its credentials")
	}
	if _, ok := tree["manifests/001-deployment-web.yaml"]; !ok {
		t.Fatalf("the deploy did not land")
	}
}

func TestSecretWriteDoesNotPruneManifests(t *testing.T) {
	remote := bareRemote(t)
	w := testWriter(t, remote, nil)

	if _, err := w.Write(context.Background(), WriteRequest{
		Files: []File{
			{Path: "001-deployment-web.yaml", Data: []byte("kind: Deployment\n")},
			{Path: "002-service-web.yaml", Data: []byte("kind: Service\n")},
		},
		Message: deployMsg(),
	}); err != nil {
		t.Fatalf("deploy: %v", err)
	}
	if _, err := w.Put(context.Background(), PutRequest{
		Files:   []File{{Path: SecretPath("checkout-db"), Data: []byte("sops: {}\n")}},
		Message: secretMsg(),
	}); err != nil {
		t.Fatalf("put: %v", err)
	}

	tree := remoteTree(t, remote, "main")
	for _, want := range []string{"manifests/001-deployment-web.yaml", "manifests/002-service-web.yaml"} {
		if _, ok := tree[want]; !ok {
			t.Fatalf("a secret write pruned %s", want)
		}
	}
}

func TestPutRemovesAndIsIdempotent(t *testing.T) {
	remote := bareRemote(t)
	w := testWriter(t, remote, nil)

	if _, err := w.Put(context.Background(), PutRequest{
		Files: []File{
			{Path: SecretPath("checkout-db"), Data: []byte("sops: {}\n")},
			{Path: SecretPath("payments"), Data: []byte("sops: {}\n")},
		},
		Message: secretMsg(),
	}); err != nil {
		t.Fatalf("put: %v", err)
	}
	if _, err := w.Put(context.Background(), PutRequest{
		Remove:  []string{SecretPath("payments")},
		Message: secretMsg(),
	}); err != nil {
		t.Fatalf("remove: %v", err)
	}

	tree := remoteTree(t, remote, "main")
	if _, ok := tree["manifests/secrets/payments.enc.yaml"]; ok {
		t.Fatalf("the removed Secret is still in the tree")
	}
	if _, ok := tree["manifests/secrets/checkout-db.enc.yaml"]; !ok {
		t.Fatalf("the other Secret was removed too")
	}

	// Removing it again is a no-op rather than an error: `kelson secret
	// delete` must be idempotent, and a caller retrying after a network
	// failure has no way to know whether the first attempt landed.
	res, err := w.Put(context.Background(), PutRequest{
		Remove:  []string{SecretPath("payments")},
		Message: secretMsg(),
	})
	if err != nil {
		t.Fatalf("removing an absent Secret must not fail: %v", err)
	}
	if !res.NoChange {
		t.Errorf("removing an absent Secret must produce no commit")
	}
}

func TestSecretFilesListsOnlyEncryptedSecrets(t *testing.T) {
	remote := bareRemote(t)
	w := testWriter(t, remote, nil)

	if _, err := w.Put(context.Background(), PutRequest{
		Files: []File{
			{Path: SecretPath("payments"), Data: []byte("sops: {payments}\n")},
			{Path: SecretPath("checkout-db"), Data: []byte("sops: {checkout-db}\n")},
			// Neither of these is one of kelson's encrypted Secrets, and a
			// listing that claimed them would have `rotate` reporting drift on
			// a README.
			{Path: "secrets/README.md", Data: []byte("# how to rotate\n")},
			{Path: "001-deployment-web.yaml", Data: []byte("kind: Deployment\n")},
		},
		Message: secretMsg(),
	}); err != nil {
		t.Fatalf("put: %v", err)
	}

	s, err := w.Open(context.Background(), secretMsg())
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	files, err := s.SecretFiles()
	if err != nil {
		t.Fatalf("SecretFiles: %v", err)
	}
	if strings.Join(files, " ") != "secrets/checkout-db.enc.yaml secrets/payments.enc.yaml" {
		t.Fatalf("SecretFiles = %v", files)
	}
	body, err := s.ReadFile(SecretPath("payments"))
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(body) != "sops: {payments}\n" {
		t.Errorf("ReadFile returned %q", body)
	}
}

// TestSecretPathRoundTrips: the writer, the reader and the prune rule agree on
// one spelling. A `set` that wrote one and a `delete` that looked for another
// would leave a credential in Git that kelson reports as gone.
func TestSecretPathRoundTrips(t *testing.T) {
	if got := SecretPath("checkout-db"); got != "secrets/checkout-db.enc.yaml" {
		t.Fatalf("SecretPath = %q", got)
	}
	name, ok := SecretName("clusters/prod/secrets/checkout-db.enc.yaml")
	if !ok || name != "checkout-db" {
		t.Errorf("SecretName = %q, %v", name, ok)
	}
	for _, notASecret := range []string{
		"secrets/README.md",
		"secrets/checkout-db.yaml",
		"manifests/checkout-db.enc.yaml",
		"secrets/.enc.yaml",
	} {
		if _, ok := SecretName(notASecret); ok {
			t.Errorf("SecretName claimed %q", notASecret)
		}
	}
}

// TestSecretsDirIsPreservedAtTheRepositoryRoot: the prune rule is relative to
// the configured delivery path, so an environment writing to the repository
// root protects `secrets/` there and not somebody else's `secrets/` elsewhere.
func TestSecretsDirIsPreservedAtTheRepositoryRoot(t *testing.T) {
	remote := bareRemote(t)
	w := testWriter(t, remote, func(c *Config) { c.Target.Path = "" })

	if _, err := w.Put(context.Background(), PutRequest{
		Files:   []File{{Path: SecretPath("checkout-db"), Data: []byte("sops: {}\n")}},
		Message: secretMsg(),
	}); err != nil {
		t.Fatalf("put: %v", err)
	}
	if _, err := w.Write(context.Background(), WriteRequest{
		Files:   []File{{Path: "001-deployment-web.yaml", Data: []byte("kind: Deployment\n")}},
		Message: deployMsg(),
	}); err != nil {
		t.Fatalf("deploy: %v", err)
	}
	if _, ok := remoteTree(t, remote, "main")["secrets/checkout-db.enc.yaml"]; !ok {
		t.Fatalf("a deploy at the repository root pruned an encrypted Secret")
	}
}

// TestPutStaysWithinThePath: Put is a write path like any other and the
// containment guarantee of this package applies to it unchanged.
func TestPutStaysWithinThePath(t *testing.T) {
	w := testWriter(t, bareRemote(t), nil)
	for _, escape := range []string{"../evil.yaml", "/etc/passwd", ""} {
		if _, err := w.Put(context.Background(), PutRequest{
			Files:   []File{{Path: escape, Data: []byte("x")}},
			Message: secretMsg(),
		}); err == nil {
			t.Errorf("Put accepted the path %q", escape)
		}
		if _, err := w.Put(context.Background(), PutRequest{
			Remove:  []string{escape},
			Message: secretMsg(),
		}); err == nil {
			t.Errorf("Put accepted the removal path %q", escape)
		}
	}
}
