package preview

import (
	"context"

	"github.com/dafrie/kelson/internal/artifact"
	"github.com/dafrie/kelson/internal/build/registry"
)

// The server-side publisher: the second caller ADR-0017 decision 8 deferred and
// [ADR-0034](docs/adr/0034-forge-driven-delivery.md) decision 3 called in.
//
// # Why there is a type here at all
//
// `kelson preview publish` composes the four steps below by hand
// (cmd/kelson/preview.go), and it may: a CLI resolves its credential from flags
// and prints a plan between the steps. A server has neither, and every caller
// that grows one would compose the same four steps again — which is exactly the
// "agree by convention, not by type" that decision 9 closed for the naming
// scheme. So the composition is a value: render, package, resolve, push, in
// that order, once.
//
// This is deliberately *not* a second implementation of anything. [Render] and
// [Package] are the same functions the CLI calls, [artifact.Pusher] is the same
// uploader, and the credential comes out of the same docker config the
// controller's spine publish reads (internal/controller/publish.go). ADR-0017
// decision 8 promised the deferred RPC would "call the same package this CLI
// verb calls"; this is that package, and the RPC's handler holds a narrow
// interface over this type rather than the type itself.
//
// # It needs no cluster, which is why it is here and not in the delivery plane
//
// A publish is a render (pure), a tar (pure) and an HTTPS conversation with a
// registry. The one input that could have demanded a Kubernetes client is the
// push credential, and it does not: the controller already established that a
// server-side publisher reads a mounted docker config (`--registry-config`),
// because a mounted Secret is rotatable under a running process and a client
// read is not cheaper for it. So this package stays on the standard-library
// allow-list (`main`, .golangci.yml) and the api plane reaches it through a
// seam for the *other* half of the fence's reason: a handler must be testable
// without a registry, not merely without a cluster.

// ArtifactPusher uploads a packaged preview. It is an interface for the reason
// internal/controller's identically-shaped one is: the production
// implementation talks to a registry over the network, and what this package is
// responsible for — what gets packaged, under which tag, with which credential
// — is worth testing without one.
type ArtifactPusher interface {
	Push(ctx context.Context, a Artifact) (string, error)
}

// PusherFor builds the uploader for one publish. It is a constructor rather
// than a single pusher because the credential is resolved per publish — the
// docker config is a mounted Secret an operator may rotate under a running
// server — and because [artifact.Pusher] caches a bearer token for the life of
// one push.
type PusherFor func(cred registry.Credential, insecure bool) ArtifactPusher

func defaultPusher(cred registry.Credential, insecure bool) ArtifactPusher {
	return &artifact.Pusher{Credential: cred, Insecure: insecure}
}

// Published is one completed publish: what was rendered, and where it now is.
type Published struct {
	// Set is the render that was published, so a caller can report the
	// namespace and the tag it agreed with without re-deriving either.
	Set *Set
	// Reference is `<repository>@sha256:…` as the registry now serves it — the
	// same string `kelson preview publish` prints on stdout.
	Reference string
}

// Publisher renders one change request's preview and pushes it.
//
// # Republishing is a no-op, by construction rather than by bookkeeping
//
// The artifact is a deterministic function of its inputs down to the digest
// (ADR-0017 decision 10: fixed modes, render order, a fixed timestamp in place
// of the wall clock). So a second publish of the same (spec, profile, change
// request, commit, images) re-derives byte-identical blobs, the pusher HEADs
// them and uploads nothing, and the manifest PUT rewrites the manifest the tag
// already resolves to. There is no dedupe cache here and there must not be one:
// a cache would be a second answer to "has this been published" that could
// disagree with the registry, which is the only authority on it.
//
// The converse is worth stating as plainly. Two publishes of one commit with
// *different* images are not a replay — they produce different bytes under the
// same SHA tag, and the second wins. That is deliberate: the tag names the
// commit, the artifact says what runs at it, and a rebuilt image at the same
// commit is a newer answer to the same question. A caller that needs the two to
// be distinguishable has the digest in [Published.Reference], which differs.
type Publisher struct {
	// RegistryConfig is a docker config.json holding the push credential,
	// normally a mounted Secret. Empty falls back to $DOCKER_CONFIG and then
	// ~/.docker/config.json, and a file that is not there at all is an
	// anonymous push — which is what a cluster-local registry takes, and what a
	// registry that disagrees refuses out loud (artifact.CredentialFromDockerConfig).
	RegistryConfig string

	// InsecureRegistries are registry hosts served over plain HTTP. It is
	// operator configuration and never a request field, for the reason
	// kelson-server's flag of the same name states: a caller who could name a
	// registry insecure could make this process send a credential in clear to a
	// host of their choosing.
	InsecureRegistries []string

	// Pusher builds the uploader. Nil selects the real one.
	Pusher PusherFor
}

// Publish renders the preview and pushes it, returning what the registry now
// serves.
//
// Every refusal is the one the step that refused already speaks: [Render]
// answers with this package's [Error] vocabulary, [Package] and the push answer
// with internal/artifact's. Nothing is re-wrapped, because a caller branching on
// `preview/requires-flux` or on artifact.DeniedError must read the same value
// here that it reads from the CLI.
func (p *Publisher) Publish(ctx context.Context, opts Options) (*Published, error) {
	set, err := Render(opts)
	if err != nil {
		return nil, err
	}
	a, err := Package(set)
	if err != nil {
		return nil, err
	}
	host, err := RegistryHost(set.Repository)
	if err != nil {
		return nil, err
	}
	// Resolved per publish rather than once at construction: the config is a
	// mounted Secret, and an operator who rotates it must not have to restart
	// the server. This is internal/controller's rule for the same file.
	cred, err := CredentialFromDockerConfig(p.RegistryConfig, host)
	if err != nil {
		return nil, err
	}
	ref, err := p.pusher()(cred, registry.IsInsecure(p.InsecureRegistries, set.Repository)).Push(ctx, a)
	if err != nil {
		return nil, err
	}
	return &Published{Set: set, Reference: ref}, nil
}

func (p *Publisher) pusher() PusherFor {
	if p.Pusher != nil {
		return p.Pusher
	}
	return defaultPusher
}
