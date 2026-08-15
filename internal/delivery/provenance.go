package delivery

// Provenance is the vocabulary the delivery plane reads back off a live
// cluster. The renderer stamps it (internal/renderer/manifest.go,
// internal/renderer/namespace.go); every adapter here has to agree on it
// exactly, because it is the only handle kelson has on "what is mine".
//
// It lives in this package rather than in each adapter because two of them now
// depend on the same answer from opposite directions: the direct adapter writes
// the namespace-ownership fact at apply time, and internal/delivery/uninstall
// reads it to decide whether the Namespace may be deleted (issue #59). Two
// private copies of that key would be one rename away from an uninstall that
// silently stops recognising its own namespaces.
const (
	// LabelManagedBy carries "kelson" on everything kelson applies.
	LabelManagedBy = "app.kubernetes.io/managed-by"
	// LabelProject and LabelEnvironment scope a resource to one deployment.
	LabelProject     = "kelson.dev/project"
	LabelEnvironment = "kelson.dev/environment"
	// ManagedByKelson is the LabelManagedBy value kelson claims.
	ManagedByKelson = "kelson"

	// LabelEnvironmentNamespace records where an object's *effect* lands, as
	// opposed to where the object itself lives.
	//
	// It exists because of the two objects kelson owns under
	// [ADR-0028](docs/adr/0028-delivery-spine.md) decision 3. An `OCIRepository`
	// and a `Kustomization` live in `kelson-system` and are named
	// `<project>-<environment>`, while the workloads they apply land in the
	// environment's own namespace — so the object's namespace says nothing
	// about what it deploys, and two Environments in different namespaces can
	// resolve to the same object name.
	//
	// That collision is the reason this is a label rather than a comment. A
	// second Environment applying over the first's Kustomization would silently
	// redirect somebody else's deployment, and server-side apply would do it
	// without complaint. The controller reads this label off the live object
	// first and refuses with `NameConflict` when it names a different
	// namespace: the check is only possible because the fact is recorded.
	//
	// It is also the reverse index. The controller watches the Flux objects and
	// has to map an event back to the Environment that owns it, which needs the
	// namespace as well as the name — this label and [LabelEnvironment] are
	// exactly that pair.
	LabelEnvironmentNamespace = "kelson.dev/environment-namespace"
)

// AnnNamespaceOwnership records what kelson knows about a Namespace's origin.
//
// The renderer stamps NamespaceOwnershipDeclared, which says only "kelson's
// rendered set declares this Namespace" — a server-side apply stamps the
// ordinary provenance labels onto a namespace that already existed just as
// readily as onto one kelson created, so the labels alone can never justify
// deleting it (internal/renderer/namespace.go).
//
// Whether kelson *created* it is observable exactly once, at apply time, and
// only by the plane that performs the apply. The direct adapter records that
// verdict here, and it is the sole licence uninstall accepts for deleting a
// Namespace: deleting one cascades to everything inside it, including resources
// kelson never created.
const AnnNamespaceOwnership = "kelson.dev/namespace-ownership"

const (
	// NamespaceOwnershipDeclared is the renderer's value: kelson declares the
	// Namespace, authorship unknown. Uninstall leaves it.
	NamespaceOwnershipDeclared = "declared"
	// NamespaceOwnershipCreated means the apply that first declared this
	// Namespace also brought it into existence. Uninstall may remove it.
	NamespaceOwnershipCreated = "created"
	// NamespaceOwnershipAdopted means the Namespace was already there when
	// kelson first applied into it. Uninstall must leave it, and everything in
	// it that is not kelson's stays with it.
	NamespaceOwnershipAdopted = "adopted"
)

// OwnedBy reports whether a live object's labels mark it as kelson's for this
// project and environment. An empty environment matches any environment, which
// is what a project-wide uninstall needs — it is never a wildcard over the
// project or over the managed-by claim, both of which must always match.
func OwnedBy(labels map[string]string, project, environment string) bool {
	if labels[LabelManagedBy] != ManagedByKelson || project == "" {
		return false
	}
	if labels[LabelProject] != project {
		return false
	}
	return environment == "" || labels[LabelEnvironment] == environment
}
