package controlstore

import (
	"context"
	"fmt"
	"sort"

	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/dafrie/kelson/api/kelson/v1alpha1"
	"github.com/dafrie/kelson/internal/model"
)

// The instance's tier of declared sources, backed by GitSource custom resources
// (ADR-0035 decision 2).
//
// # It is a reader and nothing else
//
// The connection store beside it creates and deletes, because
// gitconnection.proto has RPCs that do. Nothing in this schema authors a
// GitSource: an operator applies one with kubectl, which is the whole of what
// ADR-0035 decision 2 asks for, and kelson reads it so a component may bind to
// it by name. So this store has one method, and the chart's grant for it is
// `get, list` — a server that cannot write a GitSource cannot repoint a
// repository every project builds from.
//
// Its status is somebody else's too. `GitSourceStatus` is validation and
// nothing more, and internal/controller's GitSourceReconciler writes it — which
// is why there is no UpdateStatus here and no `gitsources/status` in the
// server's Role, unlike the connection store, whose status the server does own
// because no reconciler exists to race it.

// GitSourceStoreOptions configures a [GitSourceStore].
type GitSourceStoreOptions struct {
	// Client reads GitSource custom resources. It must be built against a
	// scheme carrying v1alpha1 — [NewClient] builds exactly that.
	Client client.Client
	// Namespace is where the sources live: kelson's own namespace, beside the
	// connections, which is the only place ADR-0035 decision 2 puts them.
	Namespace string
}

// GitSourceStore reads the GitSources one instance offers.
type GitSourceStore struct {
	client    client.Client
	namespace string
}

// NewGitSourceStore returns a store over one namespace.
func NewGitSourceStore(opts GitSourceStoreOptions) (*GitSourceStore, error) {
	if opts.Client == nil {
		return nil, fmt.Errorf("controlstore: a Kubernetes client is required")
	}
	if opts.Namespace == "" {
		return nil, fmt.Errorf("controlstore: a namespace is required")
	}
	return &GitSourceStore{client: opts.Client, namespace: opts.Namespace}, nil
}

// ListSources returns every GitSource the instance offers, ordered by name, in
// the shape [model.Resolve] takes.
//
// The conversion is [model.GitSource.AsSource]'s, reached through the same
// authoring type rather than reimplemented here, so the name a component binds
// to is the object's own `metadata.name` and cannot drift from what `kubectl
// get gitsources` prints.
//
// It is not filtered by ownership, for the reason the connection store's List
// is not: ADR-0035 decision 2 makes an instance-owned source visible to and
// selectable by every project, and a list that hid some would resolve a
// different instance than the one an operator configured. Ownership is about
// who may *edit* one, and there is no edit here.
func (s *GitSourceStore) ListSources(ctx context.Context) ([]model.Source, error) {
	var list v1alpha1.GitSourceList
	if err := s.client.List(ctx, &list, client.InNamespace(s.namespace)); err != nil {
		return nil, fmt.Errorf("controlstore: list gitsources in %s: %w", s.namespace, err)
	}
	out := make([]model.Source, 0, len(list.Items))
	for i := range list.Items {
		doc := model.GitSource{
			Metadata: model.ObjectMeta{Name: list.Items[i].Name},
			Spec:     list.Items[i].Spec,
		}
		out = append(out, doc.AsSource())
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}
