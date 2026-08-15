package preview

import (
	"fmt"

	"github.com/dafrie/kelson/internal/artifact"
	"github.com/dafrie/kelson/internal/build/registry"
	"github.com/dafrie/kelson/internal/delivery"
	"github.com/dafrie/kelson/internal/preview/naming"
)

// The publisher lives in internal/artifact now.
//
// ADR-0028 decision 2 converges the preview pipeline and the delivery spine on
// one publisher — "one media type, one determinism test, two callers" — so the
// packaging, the push and the docker-config credential lookup moved out of this
// package and into one neither caller owns. What stays here is the half that is
// actually about previews: which annotations a preview artifact carries, and
// which tag it goes under.
//
// The aliases and wrappers below are not compatibility shims for their own
// sake. `kelson preview publish` (cmd/kelson/preview.go) is written in this
// package's vocabulary and its contract test asserts that what a publish
// produces is exactly what the rendered ResourceSet asks the cluster to fetch;
// keeping the names means that test still reads as one statement about
// previews.

// Artifact is a packaged preview. See [artifact.Artifact].
type Artifact = artifact.Artifact

// Pusher uploads a packaged preview. See [artifact.Pusher].
type Pusher = artifact.Pusher

// File is one file inside a published artifact. See [artifact.File].
type File = artifact.File

// The media types source-controller dispatches on.
const (
	ConfigMediaType   = artifact.ConfigMediaType
	LayerMediaType    = artifact.LayerMediaType
	ManifestMediaType = artifact.ManifestMediaType
)

// The OCI and provenance annotations a preview artifact carries. AnnPreview is
// the one that is a preview's alone: the namespace the change request runs in.
const (
	AnnRevision    = artifact.AnnRevision
	AnnSource      = artifact.AnnSource
	AnnCreated     = artifact.AnnCreated
	AnnProject     = artifact.AnnProject
	AnnEnvironment = artifact.AnnEnvironment
	AnnPreview     = "kelson.dev/preview"
)

// Package turns a rendered preview into a Flux OCI artifact.
//
// It is the preview half of the publisher: the layout is [ManifestFiles]', the
// flat directory ADR-0017 decision 10 specifies, and the annotations are the
// ones a reviewer asks an artifact in a registry about — which change request,
// which commit, whose is it. Everything downstream of that decision is
// internal/artifact's.
//
// The Kustomization the ResourceSet templates builds `path: ./`, so the files
// sit at the root of the tar with no directory above them.
func Package(set *Set) (Artifact, error) {
	if set == nil {
		return Artifact{}, fmt.Errorf("preview: packaging needs a rendered set")
	}
	ms := delivery.ManifestSet{Project: set.Project, Environment: set.Environment}
	for _, m := range set.Manifests {
		body, err := m.YAML()
		if err != nil {
			return Artifact{}, fmt.Errorf("preview: encoding manifest %s/%s: %w", m.Kind, m.Name, err)
		}
		ms.Manifests = append(ms.Manifests, delivery.Manifest{
			APIVersion: m.APIVersion,
			Kind:       m.Kind,
			Name:       m.Name,
			Namespace:  m.Namespace,
			YAML:       body,
		})
	}
	files := ManifestFiles(ms)
	if len(files) == 0 {
		return Artifact{}, fmt.Errorf("preview: the render produced no manifests to publish")
	}

	annotations := map[string]string{
		AnnRevision:    naming.Revision(set.PR, set.SHA),
		AnnProject:     set.Project,
		AnnEnvironment: set.Environment,
		AnnPreview:     set.Namespace,
	}
	if set.SourceRepo != "" {
		annotations[AnnSource] = set.SourceRepo
	}

	return artifact.Package(artifact.Contents{
		Repository:  set.Repository,
		Tag:         set.Tag,
		Files:       files,
		Annotations: annotations,
	})
}

// ManifestFiles lays a rendered ManifestSet out as files.
// See [artifact.ManifestFiles].
func ManifestFiles(set delivery.ManifestSet) []File { return artifact.ManifestFiles(set) }

// RegistryHost is the registry an artifact repository lives on.
// See [artifact.RegistryHost].
func RegistryHost(repository string) (string, error) { return artifact.RegistryHost(repository) }

// CredentialFromDockerConfig reads the credential for host out of a docker
// config file. See [artifact.CredentialFromDockerConfig].
func CredentialFromDockerConfig(path, host string) (registry.Credential, error) {
	return artifact.CredentialFromDockerConfig(path, host)
}
