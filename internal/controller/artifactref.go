package controller

import (
	"fmt"
	"strings"

	"github.com/dafrie/kelson/api/kelson/v1alpha1"
	"github.com/dafrie/kelson/internal/build/registry"
	"github.com/dafrie/kelson/internal/model"
)

// ArtifactNamespace is the path component every kelson artifact sits under,
// between the operator's registry prefix and the project-environment pair
// (ADR-0028 decision 2). It exists so that pointing `--registry` at a
// repository that also holds images does not put artifacts and images in the
// same namespace.
const ArtifactNamespace = "kelson"

// ArtifactRef derives where one environment's revision is published:
//
//	<registry>/kelson/<project>-<environment>:<generation>-<spec-hash-short>
//
// # Why the prefix is a flag and not a spec field
//
// Where an artifact is pushed is infrastructure configuration, not application
// description — the same argument internal/build/registry.Repository makes for
// images, which is why this reuses that function rather than re-deriving the
// grammar. ADR-0028 decision 2 states it for artifacts: "where an artifact is
// pushed is not application description".
//
// # Why the tag is both a number and a hash
//
// The generation is the API server's, monotonic per object and bumped exactly
// on spec change, so kelson allocates no revision numbers of its own. The hash
// makes the tag content-identified as well: two tags with the same suffix are
// the same input, so a rollback target is recognisable without fetching it.
//
// Neither half is optional. A generation alone could not tell a rollback from a
// re-publish; a hash alone would collide with itself the moment a spec was
// reverted, and an immutable tag that is written twice with different bytes is
// the one thing this scheme must never allow.
func ArtifactRef(prefix, project, environment string, generation int64, specHash string) (repository, tag string, err error) {
	project, environment = strings.TrimSpace(project), strings.TrimSpace(environment)
	if project == "" || environment == "" {
		return "", "", &DeliveryError{
			Reason:  v1alpha1.ReasonArtifactRefInvalid,
			Message: "an artifact reference needs both a project and an environment name",
		}
	}
	if generation <= 0 {
		return "", "", &DeliveryError{
			Reason: v1alpha1.ReasonArtifactRefInvalid,
			Message: fmt.Sprintf("generation %d is not a revision number: .metadata.generation is assigned by "+
				"the API server and starts at 1", generation),
		}
	}
	// Checked before anything is derived, because "no registry" and "a bad
	// registry" have different fixes and an empty prefix would otherwise
	// resolve to a repository on Docker Hub — a push nobody asked for, to a
	// registry nobody named.
	prefix = strings.Trim(strings.TrimSpace(prefix), "/")
	if prefix == "" {
		return "", "", &DeliveryError{
			Reason: v1alpha1.ReasonRegistryNotConfigured,
			Message: "this controller was started without a registry, and the spine publishes an OCI " +
				"artifact for every revision. Set --registry (or KELSON_REGISTRY), e.g. ghcr.io/acme.",
		}
	}
	short := model.ShortHash(strings.TrimSpace(specHash))
	if len(short) != model.ShortHashLength {
		return "", "", &DeliveryError{
			Reason: v1alpha1.ReasonArtifactRefInvalid,
			Message: fmt.Sprintf("spec hash %q is too short to name a revision by: the tag carries its "+
				"leading %d characters", specHash, model.ShortHashLength),
		}
	}

	// registry.Repository owns the grammar — which component may hold what, and
	// where the host ends and the namespace begins — so a malformed --registry
	// is refused by the same code that refuses a malformed build destination.
	repository, err = registry.Repository(prefix+"/"+ArtifactNamespace, project+"-"+environment)
	if err != nil {
		return "", "", &DeliveryError{
			Reason:  v1alpha1.ReasonArtifactRefInvalid,
			Message: err.Error(),
			Err:     err,
		}
	}
	return repository, fmt.Sprintf("%d-%s", generation, short), nil
}
