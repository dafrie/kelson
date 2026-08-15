package controller

import (
	"bytes"
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/dafrie/kelson/api/kelson/v1alpha1"
	"github.com/dafrie/kelson/internal/delivery"
	"github.com/dafrie/kelson/internal/forgeconn"
	"github.com/dafrie/kelson/internal/model"
)

// Materializing the flux-operator Secret a `previews:` block needs
// ([ADR-0033](docs/adr/0033-git-connections.md) decision 4: "kelson
// materializes the flux-operator-shaped Secret from it").
//
// # Why here
//
// "At deploy time, in the right namespace" is this package's neighbourhood and
// nobody else's. The ResourceSetInputProvider that reads the Secret is rendered
// into the artifact and applied by the Kustomization this deliverer writes, so
// the Secret has to exist where that provider lands and by the time it does.
// The server resolves connections but does not deploy; the renderer must never
// see a credential at all (ADR-0001, ADR-0033 decision 1).
//
// # Whose Secret it is
//
// Only kelson's. It carries the same `kelson.dev/managed-by` provenance every
// other object this controller writes does, and a Secret at the same name
// *without* that label is left strictly alone — the rule internal/secret
// already applies from the other side. kelson does not adopt objects by
// guessing a name.
//
// # What a failure is allowed to break
//
// Two failures are refusals, and they are the two where kelson tried to write
// and could not: the API server said no, or the name is taken by somebody
// else's Secret. Those are the same class as the Flux applies beside them and
// they are reported the same way.
//
// Everything else is a skip with no error — a repository no connection covers,
// an app nobody has installed yet, a connection with no credential. Those are
// states the connection's own Ready and Reachable conditions report, and
// failing an environment's *whole deploy* because its previews cannot
// authenticate would put a revoked forge installation between a user and
// production. Previews degrade to whatever flux-operator can do without a
// credential; the workloads deploy.
//
// # Staleness, and why the preferred shape has none
//
// A [forgeconn.ShapeGitHubApp] Secret carries the app key, and flux-operator
// mints its own installation tokens from it on the schedule it already polls
// on. Nothing in it expires, so "refresh when stale" is vacuous for the shape
// this design prefers — which is the reason to prefer it. A
// [forgeconn.ShapeBasicAuth] Secret carries the user's standing token, which
// changes when they rotate the Secret it was read from, so the desired bytes
// are compared with the live ones each reconcile and written when they differ.
// Neither case needs a clock.

// PreviewSecrets resolves an environment's `previews.repo` to a connection and
// writes the Secret flux-operator reads.
//
// It is an interface so [FluxDeliverer] is testable without a forge or a
// connection store, and so an instance that holds neither simply has none: nil
// materializes nothing, which is exactly the behaviour before ADR-0033.
type PreviewSecrets interface {
	// Ensure writes or refreshes the Secret for one environment and returns its
	// name and the shape it was written in. An empty name means nothing was
	// materialized, which is the ordinary answer for an environment with no
	// previews, one that brought its own Secret, or a repository no connection
	// covers.
	Ensure(ctx context.Context, rev Revision) (name string, shape forgeconn.Shape, err error)
}

// SecretClient is the slice of a Kubernetes client this needs. It is declared
// rather than taking client.Client so a test drives three methods instead of
// faking a reader, a writer and a status writer.
type SecretClient interface {
	Get(ctx context.Context, key types.NamespacedName, obj *corev1.Secret) error
	Create(ctx context.Context, obj *corev1.Secret) error
	Update(ctx context.Context, obj *corev1.Secret) error
}

// ClientSecrets adapts a controller-runtime client to [SecretClient].
//
// The client handed here should be a *direct* one and not the manager's:
// reading Secrets through the manager's cache would start an informer over
// every Secret in the cluster, which is a memory profile and an RBAC grant
// nothing about this feature justifies. cmd/kelson-controller builds one from
// the same rest.Config, exactly as cmd/kelson-server does for its stores.
func ClientSecrets(c client.Client) SecretClient { return clientSecrets{c: c} }

type clientSecrets struct{ c client.Client }

func (s clientSecrets) Get(ctx context.Context, key types.NamespacedName, obj *corev1.Secret) error {
	return s.c.Get(ctx, key, obj)
}

func (s clientSecrets) Create(ctx context.Context, obj *corev1.Secret) error {
	return s.c.Create(ctx, obj, client.FieldOwner(FieldOwner))
}

func (s clientSecrets) Update(ctx context.Context, obj *corev1.Secret) error {
	return s.c.Update(ctx, obj, client.FieldOwner(FieldOwner))
}

// ConnectionPreviewSecrets is the production [PreviewSecrets].
type ConnectionPreviewSecrets struct {
	// Sources resolves `previews.repo` to a connection and reads its material.
	Sources *forgeconn.Resolver
	// Client writes the Secret into the environment's namespace.
	Client SecretClient
}

var _ PreviewSecrets = ConnectionPreviewSecrets{}

// Ensure implements [PreviewSecrets].
func (p ConnectionPreviewSecrets) Ensure(ctx context.Context, rev Revision) (string, forgeconn.Shape, error) {
	previews, ok := previewsOf(rev)
	if !ok || p.Sources == nil || p.Client == nil {
		return "", "", nil
	}
	name := forgeconn.PreviewSecretName(rev.Project, rev.Environment)
	if !kelsonManages(previews.SecretRef, name) {
		// The author brought their own Secret. ADR-0033 decision 4: "the field
		// stays for anyone bringing their own Secret; nothing breaks".
		return "", "", nil
	}

	// The same resolution the server runs for a build's clone: the Project's
	// `source.connection` when the author named one, host matching against
	// `previews.repo` otherwise, and a refusal — which is a skip here — when
	// two connections tie.
	//
	// The override is honoured only when `previews.repo` and `source.git` are
	// on the same forge host, because `source.connection` is a sentence about
	// the *source* repository and a preview is allowed to watch a different
	// one: applying a GitHub App named for github.com to a previews repo on
	// gitlab.com would be kelson inventing an authorization the author never
	// wrote. Same host is where the sentence still holds, and it is the
	// overwhelmingly common shape — previews on the very repository the project
	// builds from. [forgeconn.Resolver.ResolvePreviews] owns the rule.
	res, resolved, err := p.Sources.ResolvePreviews(ctx, previews.Repo, sourceOf(rev))
	if err != nil || !resolved {
		// A resolution refusal is not a deploy failure: see the header. The
		// connection's own conditions are where "two connections cover this
		// repository" is reported.
		return "", "", nil
	}

	data, shape, err := forgeconn.PreviewSecretData(res)
	if err != nil {
		// An app with no installation, or a connection with no credential.
		// Both are the connection's Ready condition, not this environment's.
		return "", "", nil
	}
	if err := p.write(ctx, rev.TargetNamespace, name, rev, data); err != nil {
		return "", "", err
	}
	return name, shape, nil
}

// write creates the Secret, updates it when the desired bytes differ from the
// live ones, and refuses to touch one kelson does not own.
func (p ConnectionPreviewSecrets) write(ctx context.Context, namespace, name string, rev Revision, data map[string][]byte) error {
	labels := map[string]string{
		delivery.LabelManagedBy:   delivery.ManagedByKelson,
		delivery.LabelProject:     rev.Project,
		delivery.LabelEnvironment: rev.Environment,
	}

	var live corev1.Secret
	err := p.Client.Get(ctx, types.NamespacedName{Namespace: namespace, Name: name}, &live)
	switch {
	case apierrors.IsNotFound(err):
		desired := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace, Labels: labels},
			Type:       corev1.SecretTypeOpaque,
			Data:       data,
		}
		if err := p.Client.Create(ctx, desired); err != nil && !apierrors.IsAlreadyExists(err) {
			return previewSecretWriteError(namespace, name, "writing", err)
		}
		return nil
	case err != nil:
		return previewSecretWriteError(namespace, name, "reading", err)
	}

	if live.Labels[delivery.LabelManagedBy] != delivery.ManagedByKelson {
		return newDeliveryError(v1alpha1.ReasonNameConflict, fmt.Sprintf(
			"Secret %s/%s already exists and is not kelson's, so kelson will not overwrite it with the credential "+
				"it would materialize from the git connection. Either delete it and let kelson own the name, or set "+
				"previews.secretRef to a different name and keep managing that Secret yourself",
			namespace, name), nil)
	}
	if sameData(live.Data, data) {
		return nil
	}
	live.Data = data
	if live.Labels == nil {
		live.Labels = map[string]string{}
	}
	for k, v := range labels {
		live.Labels[k] = v
	}
	if err := p.Client.Update(ctx, &live); err != nil {
		return previewSecretWriteError(namespace, name, "refreshing", err)
	}
	return nil
}

// previewSecretWriteError classifies an API-server refusal the way
// classifyApply does for the Flux objects: RBAC is an operator's to grant, so
// it waits on a timer rather than backing off against a permission that will
// not appear by itself.
func previewSecretWriteError(namespace, name, doing string, err error) error {
	return newDeliveryError(v1alpha1.ReasonFluxApplyForbidden,
		fmt.Sprintf("%s the previews credential Secret %s/%s", doing, namespace, name), err)
}

// sameData compares two Secret payloads by value, so a reconcile that changes
// nothing writes nothing: an Update per reconcile would bump the
// resourceVersion of a Secret every watcher of the namespace sees.
func sameData(live, desired map[string][]byte) bool {
	if len(live) != len(desired) {
		return false
	}
	for k, v := range desired {
		if !bytes.Equal(live[k], v) {
			return false
		}
	}
	return true
}

// kelsonManages reports whether kelson may write the Secret an environment's
// previews read.
//
// Two spellings qualify. An empty `previews.secretRef` is ADR-0033 decision 4's
// case — the field is optional, and the renderer writes the derived name into
// the ResourceSetInputProvider for exactly this Secret. Naming that derived
// name outright is the same request said explicitly, and it stays supported
// because a spec written while the field was still required means what it
// always meant.
//
// Anything else is somebody's own Secret and is never touched.
func kelsonManages(secretRef, derived string) bool {
	return secretRef == "" || secretRef == derived
}

// sourceOf is the Project's source block as resolution carried it, or nil for a
// project that deploys a pre-built image. It guards the whole chain for the
// same reason [previewsOf] does: a Revision is a plain struct, so nothing in
// the type system says its resolved spec is there.
func sourceOf(rev Revision) *model.ResolvedSource {
	if rev.Resolved == nil {
		return nil
	}
	return rev.Resolved.Source
}

// previewsOf is the environment's resolved previews block, when it has one.
func previewsOf(rev Revision) (*model.ResolvedPreviews, bool) {
	if rev.Resolved == nil || rev.Resolved.Environment.Previews == nil {
		return nil, false
	}
	p := rev.Resolved.Environment.Previews
	if p.Repo == "" {
		return nil, false
	}
	return p, true
}
