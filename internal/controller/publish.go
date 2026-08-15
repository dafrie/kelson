package controller

import (
	"context"
	"errors"
	"fmt"
	"strconv"

	"github.com/dafrie/kelson/api/kelson/v1alpha1"
	"github.com/dafrie/kelson/internal/artifact"
	"github.com/dafrie/kelson/internal/build/registry"
	"github.com/dafrie/kelson/internal/delivery"
)

// The provenance annotations a spine artifact carries beyond the OCI ones.
// They are the labels the renderer stamps on every resource, spelled as
// annotations, so an artifact in a registry answers the same "whose is this,
// and of what?" question a resource in a cluster does.
const (
	// AnnSpecHash is the full hash of the resolved spec this artifact was
	// rendered from. The tag carries only its leading eight characters, so this
	// is what makes two artifacts comparable without fetching their layers.
	AnnSpecHash = "kelson.dev/spec-hash"
	// AnnGeneration is the .metadata.generation the artifact was published for.
	AnnGeneration = "kelson.dev/generation"
)

// ArtifactPusher uploads a packaged artifact. It is an interface for the same
// reason [ProfileSource] is one: the production implementation talks to a
// registry over the network, and everything this package is responsible for —
// what gets packaged, under which tag, with which annotations, and what happens
// when the push is refused — is worth testing without one.
type ArtifactPusher interface {
	Push(ctx context.Context, a artifact.Artifact) (string, error)
}

// PusherFor builds the pusher for one publish, given the credential and whether
// the registry speaks plain HTTP. The seam is a constructor rather than a
// single Pusher because the credential is resolved per push (from the mounted
// docker config, which an operator may rotate under a running controller) and
// because [artifact.Pusher] caches a bearer token for the life of one push.
type PusherFor func(cred registry.Credential, insecure bool) ArtifactPusher

func defaultPusher(cred registry.Credential, insecure bool) ArtifactPusher {
	return &artifact.Pusher{Credential: cred, Insecure: insecure}
}

// publish is ADR-0028 step 4: package the rendered set as an immutable OCI
// artifact and push it.
//
// # Why an unchanged spec costs nothing
//
// Packaging is deterministic to the digest (internal/artifact), so republishing
// a spec that has not changed produces blobs the registry already holds: the
// pusher HEADs them, uploads nothing, and rewrites a manifest that is byte for
// byte the one already stored. The skip in [FluxDeliverer.Deliver] is therefore
// an optimisation and not a correctness requirement — which is the property
// that makes it safe to have at all.
func (d *FluxDeliverer) publish(ctx context.Context, rev Revision, repository, tag string) (artifact.Artifact, string, error) {
	set := delivery.ManifestSet{
		Project:     rev.Project,
		Environment: rev.Environment,
		SpecHash:    rev.SpecHash,
		Revision:    tag,
	}
	for _, m := range rev.Manifests {
		body, err := m.YAML()
		if err != nil {
			return artifact.Artifact{}, "", newDeliveryError(v1alpha1.ReasonArtifactRefInvalid,
				fmt.Sprintf("encoding the rendered %s %s for publication", m.Kind, m.Name), err)
		}
		set.Manifests = append(set.Manifests, delivery.Manifest{
			APIVersion: m.APIVersion,
			Kind:       m.Kind,
			Name:       m.Name,
			Namespace:  m.Namespace,
			YAML:       body,
		})
	}

	a, err := artifact.Package(artifact.Contents{
		Repository: repository,
		Tag:        tag,
		Files:      artifact.ManifestFiles(set),
		Annotations: map[string]string{
			artifact.AnnRevision:    tag,
			artifact.AnnProject:     rev.Project,
			artifact.AnnEnvironment: rev.Environment,
			AnnSpecHash:             rev.SpecHash,
			AnnGeneration:           strconv.FormatInt(rev.Generation, 10),
		},
	})
	if err != nil {
		return artifact.Artifact{}, "", newDeliveryError(v1alpha1.ReasonArtifactRefInvalid,
			"packaging the rendered set", err)
	}

	host, err := artifact.RegistryHost(repository)
	if err != nil {
		return artifact.Artifact{}, "", newDeliveryError(v1alpha1.ReasonArtifactRefInvalid,
			"reading the registry host out of "+repository, err)
	}
	// The credential is resolved per push rather than once at start-up: the
	// docker config is a mounted Secret, and an operator who rotates it must not
	// have to restart the controller. A missing file is an anonymous push, which
	// is what a local registry with no auth takes — the registry says otherwise
	// if it disagrees, and that refusal is PushDenied.
	cred, err := artifact.CredentialFromDockerConfig(d.RegistryConfig, host)
	if err != nil {
		return artifact.Artifact{}, "", newDeliveryError(v1alpha1.ReasonPushDenied,
			"reading the registry credential at "+d.RegistryConfig, err)
	}

	ref, err := d.pusher()(cred, registry.IsInsecure(d.InsecureRegistries, repository)).Push(ctx, a)
	if err != nil {
		return artifact.Artifact{}, "", classifyPush(err, repository, tag)
	}
	return a, ref, nil
}

// classifyPush turns a push failure into the one reason that decides what
// happens next (see errors.go). The distinction the taxonomy rests on is
// whether the registry answered:
//
//   - it answered and refused the caller → PushDenied. A credential or a
//     permission, which a human fixes; retrying into a rate limit helps nobody.
//   - it answered and refused the request → PushDenied too, because a registry
//     rejecting this artifact will reject it identically on every retry.
//   - it never answered → RegistryUnreachable, the one delivery failure that
//     takes controller-runtime's exponential backoff.
//   - kelson never got as far as asking → ArtifactRefInvalid: the repository is
//     not one anything can push to, and no amount of waiting changes that.
func classifyPush(err error, repository, tag string) error {
	doing := fmt.Sprintf("publishing %s:%s", repository, tag)

	var refusal artifact.Error
	if errors.As(err, &refusal) {
		return newDeliveryError(v1alpha1.ReasonArtifactRefInvalid, doing, err)
	}
	var unreachable *artifact.UnreachableError
	if errors.As(err, &unreachable) {
		return newDeliveryError(v1alpha1.ReasonRegistryUnreachable, doing, err)
	}
	var denied *artifact.DeniedError
	if errors.As(err, &denied) {
		return newDeliveryError(v1alpha1.ReasonPushDenied, doing, err)
	}
	// An unclassified failure is treated as transient. That is the safe way
	// round: a retry costs a request, and refusing to retry something that
	// would have worked costs a deployment.
	return newDeliveryError(v1alpha1.ReasonRegistryUnreachable, doing, err)
}

func (d *FluxDeliverer) pusher() PusherFor {
	if d.Pusher != nil {
		return d.Pusher
	}
	return defaultPusher
}
