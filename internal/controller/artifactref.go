package controller

import (
	"fmt"
	"sort"
	"strconv"
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
	repository, err = ArtifactRepository(prefix, project, environment)
	if err != nil {
		return "", "", err
	}
	return repository, fmt.Sprintf("%d-%s", generation, short), nil
}

// ArtifactRepository is the first half of [ArtifactRef]: where one
// environment's artifacts live, without saying which revision.
//
// It is separate because reading the registry needs the repository and has no
// revision to derive it from — listing what an environment has published, or
// asking whether one aged-out tag is still there, is a question about the
// repository as a whole (issue #241, internal/artifact's Tags and Resolve).
// Both processes that ask it call this rather than restating the rule: kelson's
// naming (ADR-0028 decision 2) has to be one function, or the controller and
// the server can look in two different places and each be sure it is right.
func ArtifactRepository(prefix, project, environment string) (string, error) {
	project, environment = strings.TrimSpace(project), strings.TrimSpace(environment)
	if project == "" || environment == "" {
		return "", &DeliveryError{
			Reason:  v1alpha1.ReasonArtifactRefInvalid,
			Message: "an artifact reference needs both a project and an environment name",
		}
	}
	// Checked before anything is derived, because "no registry" and "a bad
	// registry" have different fixes and an empty prefix would otherwise
	// resolve to a repository on Docker Hub — a push nobody asked for, to a
	// registry nobody named.
	prefix = strings.Trim(strings.TrimSpace(prefix), "/")
	if prefix == "" {
		return "", &DeliveryError{
			Reason: v1alpha1.ReasonRegistryNotConfigured,
			Message: "this controller was started without a registry, and the spine publishes an OCI " +
				"artifact for every revision. Set --registry (or KELSON_REGISTRY), e.g. ghcr.io/acme.",
		}
	}
	// registry.Repository owns the grammar — which component may hold what, and
	// where the host ends and the namespace begins — so a malformed --registry
	// is refused by the same code that refuses a malformed build destination.
	repository, err := registry.Repository(prefix+"/"+ArtifactNamespace, project+"-"+environment)
	if err != nil {
		return "", &DeliveryError{
			Reason:  v1alpha1.ReasonArtifactRefInvalid,
			Message: err.Error(),
			Err:     err,
		}
	}
	return repository, nil
}

// ParseRevision reads a tag back into the two things ADR-0028 decision 2 puts
// in one: the generation that allocated it and the leading eight characters of
// its spec hash.
//
// It is the reverse of [ArtifactRef]'s tag, and it exists because the registry
// answers with a flat list of tags in its own lexical order — where "10-…"
// sorts before "9-…" — so anything that wants revisions newest-first has to
// read the number out rather than sort the strings.
//
// A tag that is not a revision is not an error and not a guess: repositories
// may hold tags kelson did not write, and ok is false for every one of them.
func ParseRevision(tag string) (generation int64, specHashShort string, ok bool) {
	number, short, found := strings.Cut(strings.TrimSpace(tag), "-")
	if !found || len(short) != model.ShortHashLength {
		return 0, "", false
	}
	// The generation is a positive decimal with no leading zero, because that
	// is what .metadata.generation formats as; accepting "007-…" would make two
	// spellings of one revision.
	if number == "" || number[0] == '0' {
		return 0, "", false
	}
	for _, c := range number {
		if c < '0' || c > '9' {
			return 0, "", false
		}
	}
	for _, c := range short {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return 0, "", false
		}
	}
	generation, err := strconv.ParseInt(number, 10, 64)
	if err != nil {
		return 0, "", false
	}
	return generation, short, true
}

// SortRevisions puts revision tags newest-first — by generation, which is the
// only ordering the tag grammar supports and the order every reader of a
// history wants. Tags that are not revisions are dropped, because a caller
// showing them as revisions would be showing something kelson never published.
func SortRevisions(tags []string) []string {
	type revision struct {
		tag        string
		generation int64
	}
	revisions := make([]revision, 0, len(tags))
	for _, tag := range tags {
		generation, _, ok := ParseRevision(tag)
		if !ok {
			continue
		}
		revisions = append(revisions, revision{tag: tag, generation: generation})
	}
	sort.SliceStable(revisions, func(i, j int) bool { return revisions[i].generation > revisions[j].generation })
	out := make([]string, 0, len(revisions))
	for _, r := range revisions {
		out = append(out, r.tag)
	}
	return out
}
