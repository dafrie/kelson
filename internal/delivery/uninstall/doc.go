// Package uninstall removes what kelson deployed for one (project,
// environment), and nothing else (issue #59).
//
// # Why this is a verb and not a script
//
// ADR-0003's additive doctrine says kelson adopts a cluster rather than
// colonising it, and the sharpest form of that claim is that its own deployment
// is reversible: what kelson put in a cluster can be taken back out without
// taking anything else with it. `test/e2e/additivity_test.go` proves the
// underlying property — the set selected by kelson's provenance labels is
// exactly the set the renderer produced — and this package turns that property
// into an operation with a preview, a confirmation and a per-object result.
//
// The handle is the provenance label set and only that:
//
//	app.kubernetes.io/managed-by=kelson, kelson.dev/project=<p>, kelson.dev/environment=<e>
//
// Every candidate is listed by that selector, re-read immediately before its
// delete, and checked again against its live labels with a UID precondition on
// the delete itself. A resource whose labels stopped matching in between is
// reported and left alone, exactly as internal/secret refuses to delete a
// Secret it did not write. kelson never deletes by name, and never by
// "everything in this namespace".
//
// # Order
//
// Deletion runs in tiers, and the order is about what a half-finished uninstall
// looks like from outside:
//
//  1. Routes. Traffic stops arriving at something that is about to disappear,
//     rather than arriving and getting a 503 from a Service with no endpoints.
//  2. Workloads. The readers of the data services go before the data services,
//     so nothing is left connecting to a database that is being deleted.
//  3. Configuration — Services, ServiceAccounts, ConfigMaps, Secrets, and
//     anything else kelson labelled that is not in another tier.
//  4. Data. Last, because it is the only irreversible step: everything above is
//     re-creatable from the spec, and a Postgres cluster is not.
//  5. The Namespace, and only when both halves of the licence hold: the
//     delivery plane recorded that kelson created it
//     (delivery.AnnNamespaceOwnership), AND nothing of anyone else's is still
//     living in it. Deleting a namespace cascades to everything inside it
//     including resources kelson never created, so a namespace that predates
//     kelson is left behind holding whatever else lives there.
//
// The second half of that licence exists because a namespace is per-Environment
// but its NAME is overridable (spec.namespace), so two projects — or two
// environments of one project — can be pointed at the same one. Ownership
// records who created the namespace and cannot see who moved in afterwards, so
// before deleting one kelson lists it for resources carrying
// app.kubernetes.io/managed-by=kelson whose kelson.dev/project /
// kelson.dev/environment pair is not the one being uninstalled. Any such
// resource keeps the namespace standing, and the run says what stayed and whose
// it is. The check runs at plan time, so the preview is honest, and again
// immediately before the delete, so a tenant who arrived in between is not
// evicted by a stale plan (issue #215). Its boundary is the sweep's own kind
// set, and both directions of failure — an unreadable kind, an unresolvable API
// group — leave the namespace alone: deleting one is the only step here that
// reaches resources the scope never selected.
//
// # What this deliberately does NOT remove
//
//   - The kelson server install. That is the Helm chart's
//     (`helm uninstall kelson`); everything it creates carries
//     kelson.dev/install=<release> and none of it carries a project label.
//   - CRDs. kelson installs none (ADR-0013 §1: its state is ConfigMaps), so
//     there are none of its to remove.
//   - Operators. CloudNativePG, the Valkey operator, Flux, cert-manager —
//     kelson delegates to them and never installs them (ADR-0005), so removing
//     them here would break every other tenant of the cluster.
//   - PersistentVolumes and volumes an operator owns. Deleting a CloudNativePG
//     Cluster hands its PVCs to CloudNativePG's garbage collection; those PVCs
//     are named in the preview because they are about to be destroyed, but
//     kelson does not delete them itself.
//   - The server's stored specs and history. Those are ConfigMaps in the
//     server's namespace (ADR-0013 §1), reachable only through an authorization
//     decision that does not exist yet (issue #84). The CLI removes its own
//     local rendered history instead; see direct.Store.Forget.
package uninstall
