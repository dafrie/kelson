package controller

import (
	"context"
	"errors"
	"fmt"

	"github.com/dafrie/kelson/api/kelson/v1alpha1"
	"github.com/dafrie/kelson/internal/artifact"
	"github.com/dafrie/kelson/internal/build/registry"
)

// RevisionLister is the durable record of what an environment has published:
// the registry's own tag list (ADR-0028 decision 4, issue #241).
//
// # Why a seam, and why it is not the [Deliverer]
//
// `Environment.status.history[]` is a mirror bounded at
// [v1alpha1.MaxHistoryEntries], and the ADR says plainly that it "is not the
// record; the record is the registry". Everything that has to reach past the
// window — a rollback to a revision that aged out, a history listing that does
// not silently stop at twenty — asks this, and both kelson processes ask it:
// kelson-controller when it verifies a rollback target, kelson-server when it
// serves History and refuses a bad target before writing an annotation.
// [RegistryRevisions] is the implementation both wire, so the two cannot
// disagree about what a repository is called or what is in it.
//
// It is not a method on [Deliverer] because delivering is what a reconcile does
// and this is what a reconcile *asks*: a nil one is a controller that can see
// only the mirror, which is exactly the behaviour before #241 and still the
// right answer for a process with no registry credential.
type RevisionLister interface {
	// Revisions lists every revision the registry holds for one environment,
	// newest first. An environment that has published nothing is an empty list
	// and no error.
	Revisions(ctx context.Context, project, environment string) ([]string, error)

	// Resolve reports whether one revision is in the registry and which bytes
	// it names. found=false with a nil error is "there is no such revision",
	// which is a different answer from "kelson could not look" and must stay
	// distinguishable: one is a typo, the other is a credential.
	Resolve(ctx context.Context, project, environment, revision string) (digest string, found bool, err error)
}

// RegistryReader is the registry client [RegistryRevisions] reads through.
// *artifact.Pusher implements it; the seam exists so the reconciler's tests and
// the server's need no registry, the same argument [ArtifactPusher] makes for
// the write half.
type RegistryReader interface {
	Tags(ctx context.Context, repository string) ([]string, error)
	Resolve(ctx context.Context, repository, tag string) (digest string, found bool, err error)
}

// ReaderFor builds the reader for one query, given the credential and whether
// the registry speaks plain HTTP — the read-side twin of [PusherFor], and a
// constructor for the same reason: the credential is resolved per query from a
// mounted docker config an operator may rotate under a running process.
type ReaderFor func(cred registry.Credential, insecure bool) RegistryReader

func defaultReader(cred registry.Credential, insecure bool) RegistryReader {
	return &artifact.Pusher{Credential: cred, Insecure: insecure}
}

// RegistryRevisions answers [RevisionLister] from the registry the spine
// publishes to.
//
// It carries the same three pieces of configuration a publish does, and it must
// be given the same values: an instance pointed at a different registry from
// the controller's would answer confidently about artifacts nobody published.
// Both binaries build it from their own `--registry` / `--registry-config`
// flags, which are the same two variables (KELSON_REGISTRY,
// KELSON_REGISTRY_CONFIG) and the same mounted Secret by design.
//
// # The credential needs read scope
//
// Pushing and listing are different scopes on most registries. A controller
// whose credential may push but not pull gets a refusal here rather than
// silence — [v1alpha1.ReasonRegistryReadDenied], which names the credential as
// the thing to fix — because the alternative is reporting a revision that
// exists as one that does not, and that is the one answer this type must never
// give.
type RegistryRevisions struct {
	// Registry is the push prefix, e.g. "ghcr.io/acme". Artifacts live under
	// <Registry>/kelson/<project>-<environment>.
	Registry string
	// RegistryConfig is the path to a docker config.json holding the
	// credential. Absent is an anonymous read.
	RegistryConfig string
	// InsecureRegistries are the hosts that speak plain HTTP, named by the
	// operator and never guessed.
	InsecureRegistries []string
	// Reader is the seam tests replace. Nil is the real registry client.
	Reader ReaderFor
}

var _ RevisionLister = RegistryRevisions{}

// Revisions implements [RevisionLister].
func (r RegistryRevisions) Revisions(ctx context.Context, project, environment string) ([]string, error) {
	repository, reader, err := r.connect(project, environment)
	if err != nil {
		return nil, err
	}
	tags, err := reader.Tags(ctx, repository)
	if err != nil {
		return nil, classifyRead(err, repository)
	}
	// The registry's order is lexical, so "10-…" arrives before "9-…". Sorting
	// by generation is what makes this a history rather than a listing.
	return SortRevisions(tags), nil
}

// Resolve implements [RevisionLister].
func (r RegistryRevisions) Resolve(ctx context.Context, project, environment, revision string) (string, bool, error) {
	repository, reader, err := r.connect(project, environment)
	if err != nil {
		return "", false, err
	}
	digest, found, err := reader.Resolve(ctx, repository, revision)
	if err != nil {
		return "", false, classifyRead(err, repository)
	}
	return digest, found, nil
}

// connect derives the repository and resolves the credential. Both are done per
// query rather than once: the repository because it is a pure function of the
// names, and the credential because the docker config is a mounted Secret and
// an operator who rotates it must not have to restart anything.
func (r RegistryRevisions) connect(project, environment string) (string, RegistryReader, error) {
	repository, err := ArtifactRepository(r.Registry, project, environment)
	if err != nil {
		return "", nil, err
	}
	host, err := artifact.RegistryHost(repository)
	if err != nil {
		return "", nil, newDeliveryError(v1alpha1.ReasonArtifactRefInvalid,
			"reading the registry host out of "+repository, err)
	}
	cred, err := artifact.CredentialFromDockerConfig(r.RegistryConfig, host)
	if err != nil {
		return "", nil, newDeliveryError(v1alpha1.ReasonRegistryReadDenied,
			"reading the registry credential at "+r.RegistryConfig, err)
	}
	build := r.Reader
	if build == nil {
		build = defaultReader
	}
	return repository, build(cred, registry.IsInsecure(r.InsecureRegistries, repository)), nil
}

// classifyRead turns a failed read into the one reason that decides what
// happens next, on the same three-way split [classifyPush] makes and for the
// same reason: whether the registry answered.
//
// The difference is what a refusal means. A push refused is a credential that
// may not write; a read refused is a credential that may not *look*, which is
// its own reason because the fix is different — a token scoped `pull` — and
// because the failure it would otherwise be confused with is far worse: an
// unreadable registry reported as an empty one turns "kelson may not ask" into
// "that revision does not exist".
func classifyRead(err error, repository string) error {
	doing := "reading the revisions of " + repository

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
		return newDeliveryError(v1alpha1.ReasonRegistryReadDenied, doing, err)
	}
	// Unclassified is treated as transient, the safe way round: a retry costs a
	// request, and refusing to retry something that would have worked costs a
	// rollback.
	return newDeliveryError(v1alpha1.ReasonRegistryUnreachable, doing, err)
}

// describeRevisions is the one sentence a refusal adds about the durable
// record: how much of it there is, and where it starts. It is deliberately not
// the whole list — a repository may hold thousands — and it is deliberately not
// silent when there are none, because "the registry holds no revisions for this
// environment" is the answer that tells an operator they are looking at the
// wrong environment.
func describeRevisions(revisions []string) string {
	switch len(revisions) {
	case 0:
		return "the registry holds no revisions for this environment either"
	case 1:
		return fmt.Sprintf("the registry holds one revision, %s", revisions[0])
	default:
		return fmt.Sprintf("the registry holds %d revisions, %s down to %s",
			len(revisions), revisions[0], revisions[len(revisions)-1])
	}
}
