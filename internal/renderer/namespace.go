package renderer

import (
	"github.com/dafrie/kelson/internal/model"
)

// AnnNamespaceOwnership marks the Namespace kelson renders for an environment.
//
// It exists for the non-destructive uninstall guarantee (issue #59). Deleting a
// namespace deletes everything inside it, including resources kelson never
// created, so uninstall may never treat the ordinary provenance labels on a
// Namespace as a licence to delete it: a server-side apply stamps those onto a
// namespace that already existed just as readily as onto one kelson created.
//
// This annotation therefore says exactly one thing — kelson's rendered set
// declares this Namespace — and deliberately does not claim authorship. The
// create-versus-adopt fact is only observable at apply time, which is the
// delivery plane's business, not the renderer's (ADR-0001). Since #59 the
// direct adapter overwrites this value with "created" or "adopted" as it
// applies (internal/delivery.NamespaceOwnershipCreated and its neighbours), and
// "created" is the only value `kelson uninstall` accepts as licence to delete a
// Namespace.
const AnnNamespaceOwnership = "kelson.dev/namespace-ownership"

// NamespaceOwnershipDeclared is the value the renderer stamps: kelson declares
// the Namespace, authorship unknown. The delivery plane resolves it at apply
// time into "created" (kelson brought it into existence, so uninstall may
// remove it) or "adopted" (it predated kelson, so uninstall must leave it). A
// Namespace still carrying this value is one whose authorship nothing recorded,
// and uninstall leaves those alone too.
const NamespaceOwnershipDeclared = "declared"

// namespaceManifest renders the environment's Namespace. Without it a direct
// deploy into a fresh cluster fails on the first resource with
// `namespaces "<ns>" not found` (issue #150); every other rendered resource
// targets this namespace, so it has to exist first.
//
// It is derived from resolved spec data only — the namespace name, the project
// and the environment — so the renderer stays pure.
func namespaceManifest(resolved *model.Resolved) (Manifest, error) {
	hash, err := namespaceHash(resolved)
	if err != nil {
		return Manifest{}, Errors{{Code: ErrInternal, Message: err.Error()}}
	}
	prov := provenance{
		project:      resolved.Project,
		environment:  resolved.Environment.Name,
		resourceName: resolved.Environment.Namespace,
		specHash:     hash,
	}

	annotations := prov.annotations()
	mapSet(annotations, AnnNamespaceOwnership, strNode(NamespaceOwnershipDeclared))

	// A Namespace is cluster-scoped, so its metadata carries no namespace of
	// its own — unlike every other resource the renderer emits.
	root := mapNode(
		"apiVersion", "v1",
		"kind", "Namespace",
		"metadata", mapNode(
			"name", prov.name(),
			"labels", prov.labels(),
			"annotations", annotations,
		),
	)
	return Manifest{APIVersion: "v1", Kind: "Namespace", Name: prov.name(), doc: docNode(root)}, nil
}

// namespaceHash is the Namespace's kelson.dev/spec-hash. It covers only the
// inputs the Namespace document is built from, so unrelated spec edits — a new
// component, a changed image, different routing — leave it untouched.
func namespaceHash(resolved *model.Resolved) (string, error) {
	return hashJSON(struct {
		Project     string `json:"project"`
		Environment string `json:"environment"`
		Namespace   string `json:"namespace"`
	}{
		Project:     resolved.Project,
		Environment: resolved.Environment.Name,
		Namespace:   resolved.Environment.Namespace,
	})
}
