package uninstall

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"

	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/discovery"
)

// APIResource is one kind an uninstall may sweep: what to list through, and
// what to call it in the preview.
type APIResource struct {
	GVR  schema.GroupVersionResource
	Kind string
}

// Catalog enumerates the namespaced kinds a sweep covers.
//
// It is a seam for the same reason direct.Mapper is: the production
// implementation needs a live discovery client, and the tests that assert
// ordering, selection and refusals need neither a cluster nor an opinion about
// what a cluster's API surface looks like.
//
// The sweep is discovery-driven rather than a fixed list of the kinds the
// renderer emits today, because an overlay can contribute any kind at all
// (internal/renderer/overlay.go). A fixed list would silently leave those
// behind — and a resource kelson applied, labelled, and then failed to mention
// in its own uninstall preview is the exact failure this package exists to
// prevent.
type Catalog interface {
	// Namespaced returns the namespaced kinds that can be both listed and
	// deleted, plus the names of API groups discovery could not read at all.
	//
	// The gaps are returned rather than swallowed: an aggregated API service
	// that is down means the sweep is incomplete, and an incomplete sweep
	// reported as a complete one is a lie about what is left in the cluster.
	Namespaced(ctx context.Context) (resources []APIResource, gaps []string, err error)
}

// DiscoveryCatalog is the production Catalog, backed by the API server's own
// discovery document.
type DiscoveryCatalog struct {
	Client discovery.DiscoveryInterface
}

var _ Catalog = DiscoveryCatalog{}

// Namespaced implements Catalog.
//
// The context is accepted for the seam and unused: client-go's discovery
// interface takes none. A CLI invocation is short-lived and the caller's
// timeout still bounds the surrounding operation.
func (d DiscoveryCatalog) Namespaced(_ context.Context) ([]APIResource, []string, error) {
	if d.Client == nil {
		return nil, nil, fmt.Errorf("uninstall: a discovery client is required to enumerate what to sweep")
	}
	lists, err := d.Client.ServerPreferredResources()
	var gaps []string
	if err != nil {
		// A partial answer is the normal case on a cluster with an unhealthy
		// aggregated API, and it is far more useful than no answer: the groups
		// that did resolve are still swept, and the ones that did not are
		// named. Anything else is a real failure.
		var failed *discovery.ErrGroupDiscoveryFailed
		if !errors.As(err, &failed) {
			return nil, nil, fmt.Errorf("uninstall: reading the cluster's API surface: %w", err)
		}
		for gv := range failed.Groups {
			gaps = append(gaps, gv.String())
		}
		sort.Strings(gaps)
	}

	seen := map[schema.GroupVersionResource]bool{}
	var out []APIResource
	for _, list := range lists {
		if list == nil {
			continue
		}
		gv, parseErr := schema.ParseGroupVersion(list.GroupVersion)
		if parseErr != nil {
			gaps = append(gaps, list.GroupVersion)
			continue
		}
		for _, r := range list.APIResources {
			if !r.Namespaced || !supportsVerbs(r.Verbs, "list", "delete") {
				continue
			}
			// Subresources ("pods/log") are addressed through their parent and
			// are never independently deletable.
			if strings.Contains(r.Name, "/") {
				continue
			}
			gvr := gv.WithResource(r.Name)
			if seen[gvr] {
				continue
			}
			seen[gvr] = true
			out = append(out, APIResource{GVR: gvr, Kind: r.Kind})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].GVR.String() < out[j].GVR.String() })
	return out, gaps, nil
}

// supportsVerbs reports whether a discovered resource admits every verb the
// sweep needs. Both matter: a kind kelson can list but not delete would be
// previewed and then refused by the API server, and a kind it can delete but
// not list can never be found in the first place.
func supportsVerbs(have []string, want ...string) bool {
	for _, w := range want {
		if !slices.Contains(have, w) {
			return false
		}
	}
	return true
}
