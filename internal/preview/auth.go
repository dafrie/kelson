package preview

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/dafrie/kelson/internal/build/registry"
	"github.com/dafrie/kelson/internal/redact"
)

// dockerHubKeys are the names Docker Hub is written under in a docker config,
// none of which is the host a reference parses to. Every other registry keys
// itself by its own host.
var dockerHubKeys = []string{"https://index.docker.io/v1/", "index.docker.io", "registry-1.docker.io"}

// CredentialFromDockerConfig reads the credential for host out of a docker
// config file, which is what a CI runner has after a `docker login` step.
//
// It is the credential source a publish uses by default, because the publisher
// runs in the application repository's CI (ADR-0017, "Stage 2") where a login
// step already ran and a cluster may not even be reachable. The other source is
// a dockerconfigjson Secret resolved from the cluster through
// internal/delivery/kube — the same registry.Credential comes out of both,
// which is the point of sharing the type.
//
// A missing file is not an error: an anonymous push to a registry that permits
// one is a legitimate outcome, and a registry that does not permit one refuses
// with a message naming both ways to supply a credential. A file that exists
// and cannot be parsed IS an error — that is a broken login, not the absence of
// one.
func CredentialFromDockerConfig(path, host string) (registry.Credential, error) {
	file, err := dockerConfigPath(path)
	if err != nil {
		return registry.Credential{}, err
	}
	data, err := os.ReadFile(filepath.Clean(file))
	if errors.Is(err, fs.ErrNotExist) {
		return registry.Credential{}, nil
	}
	if err != nil {
		return registry.Credential{}, fmt.Errorf("preview: reading the registry login at %s: %w", file, err)
	}
	// A file that exists and is not JSON is a broken login, which is a
	// different thing from no login at all and must not degrade into one: an
	// anonymous push would then fail against the registry with a 401 that says
	// nothing about the file that caused it.
	if !json.Valid(data) {
		return registry.Credential{}, fmt.Errorf("preview: the registry login at %s is not valid JSON", file)
	}

	keys := []string{host}
	if host == "docker.io" {
		keys = append(keys, dockerHubKeys...)
	}
	for _, key := range keys {
		cred, credErr := registry.CredentialFromDockerConfigJSON(data, key)
		if credErr != nil {
			continue
		}
		// This is the moment the process learns a credential, so it is the
		// moment the value becomes unprintable — the same registration
		// kube.SecretResolver performs for the cluster-side source (issue #117).
		redact.Register(cred.SecretValues()...)
		return cred, nil
	}
	// No entry for this registry is the anonymous case, not a failure: the
	// runner may be logged in to a different registry, or to none.
	return registry.Credential{}, nil
}

// dockerConfigPath resolves which file to read: an explicit path, else
// $DOCKER_CONFIG/config.json, else ~/.docker/config.json — the precedence the
// docker CLI itself documents, so a runner configured for docker is configured
// for this.
func dockerConfigPath(path string) (string, error) {
	if path != "" {
		return path, nil
	}
	if dir := os.Getenv("DOCKER_CONFIG"); dir != "" {
		return filepath.Join(dir, "config.json"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("preview: locating the registry login: %w", err)
	}
	return filepath.Join(home, ".docker", "config.json"), nil
}
