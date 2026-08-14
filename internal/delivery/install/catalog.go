package install

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

// APIResource is one kind the component sweep covers.
type APIResource struct {
	GVR        schema.GroupVersionResource
	Kind       string
	Namespaced bool
}

// Catalog enumerates the kinds a component uninstall sweeps.
//
// It is a seam for the same reason uninstall.Catalog is: the production
// implementation needs discovery, and the tests that assert what is deleted and
// what is refused need neither a cluster nor an opinion about a cluster's API
// surface.
//
// It is a separate interface from uninstall.Catalog rather than a shared one
// because the two sweeps need different halves of discovery. A project
// uninstall covers namespaced kinds only — kelson renders nothing
// cluster-scoped except the Namespace it addresses directly. A platform
// component is mostly cluster-scoped: CRDs, ClusterRoles, ClusterRoleBindings
// and webhook configurations are the bulk of every manifest in the pins table,
// and a sweep that skipped them would leave a component's most durable pieces
// behind while reporting it removed. Widening uninstall.Catalog to serve both
// would make every project uninstall list every cluster-scoped kind for
// resources it can never own.
//
// The sweep is discovery-driven rather than a fixed list of the kinds the
// pinned manifests contain today, for the reason uninstall states: a kind that
// appears in a later upstream release, applied and labelled by kelson and then
// missing from kelson's own removal preview, is precisely the failure this
// package exists to prevent.
type Catalog interface {
	// Resources returns the kinds that can be both listed and deleted, plus the
	// names of API groups discovery could not read at all. The gaps are
	// returned rather than swallowed: an incomplete sweep reported as a
	// complete one is a lie about what is left in the cluster.
	Resources(ctx context.Context) (resources []APIResource, gaps []string, err error)
}

// DiscoveryCatalog is the production Catalog.
type DiscoveryCatalog struct {
	Client discovery.DiscoveryInterface
}

var _ Catalog = DiscoveryCatalog{}

// Resources implements Catalog. The context is accepted for the seam and
// unused: client-go's discovery interface takes none.
func (d DiscoveryCatalog) Resources(_ context.Context) ([]APIResource, []string, error) {
	if d.Client == nil {
		return nil, nil, fmt.Errorf("install: a discovery client is required to enumerate what to sweep")
	}
	lists, err := d.Client.ServerPreferredResources()
	var gaps []string
	if err != nil {
		var failed *discovery.ErrGroupDiscoveryFailed
		if !errors.As(err, &failed) {
			return nil, nil, fmt.Errorf("install: reading the cluster's API surface: %w", err)
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
			if !supportsVerbs(r.Verbs, "list", "delete") || strings.Contains(r.Name, "/") {
				continue
			}
			gvr := gv.WithResource(r.Name)
			if seen[gvr] {
				continue
			}
			seen[gvr] = true
			out = append(out, APIResource{GVR: gvr, Kind: r.Kind, Namespaced: r.Namespaced})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].GVR.String() < out[j].GVR.String() })
	return out, gaps, nil
}

func supportsVerbs(have []string, want ...string) bool {
	for _, w := range want {
		if !slices.Contains(have, w) {
			return false
		}
	}
	return true
}
