// Package install offers to install the platform components a cluster is
// missing, and tracks which of them kelson installed (issue #60).
//
// # Offer, never assume
//
// ADR-0003's rule is "never install what is already there", and detection is
// how kelson finds out. This package is the other half of that sentence: when
// detection reports a component absent, kelson may offer to install it — with
// a preview, a confirmation, and a record of what it did.
//
// Detection comes first and is a refusal, not a hint. A component the profile
// reports present is refused: "never modify a component kelson did not install"
// starts with never double-installing one. So does a component the profile
// could not read — a detection [clusterprofile.Gap] makes presence Unknown, and
// installing on an unknown is exactly the guess the tri-state exists to
// prevent (issue #144).
//
// # Reference upstream, never vendor
//
// Each component is installed from the install manifest its own project
// publishes, at a version pinned in [Components], fetched at install time and
// verified against a recorded SHA-256 digest. Nothing is vendored into this
// repository and no chart is re-packaged.
//
// That is ADR-0005's Bitnami lesson made operational. Kubero vendored Bitnami
// charts for its add-ons; when Broadcom withdrew the catalog in September 2025,
// working installations broke and the add-on system had to be reworked under
// time pressure. Taking custody of someone else's packaging means inheriting a
// vendor decision you have no influence over. A pinned URL plus a digest is a
// dependency kelson can state exactly and a user can verify with curl and
// sha256sum.
//
// The cost is stated rather than hidden: an install needs network access to the
// upstream release host. Without it this package refuses, names the URL it
// could not reach, and applies nothing (see [Fetcher]).
//
// # Flux is installed by installing flux-operator
//
// `kelson install flux` applies flux-operator's pinned install manifest and
// then creates one `FluxInstance` custom resource. It does not vendor Flux's
// own manifests and does not reimplement the install or upgrade lifecycle that
// flux-operator already owns — the FluxInstance names the distribution version
// and the operator does the rest (ADR-0016).
//
// LICENSE: flux-operator is AGPL-3.0 and kelson is MIT. The integration is
// CR-only, through the unstructured dynamic client, and no Go module of theirs
// is imported anywhere in this repository (docs/architecture.md, "Living with
// flux-operator"). That constraint is the reason this package writes a
// FluxInstance as a map rather than as a typed object.
//
// # Provenance: what kelson installed, object by object
//
// The acceptance criterion of issue #60 is that uninstall removes only what
// kelson installed. That is a claim about the cluster, so it is recorded in the
// cluster, per object, at the moment it can be observed — the same discipline
// kelson applies to namespaces (delivery.AnnNamespaceOwnership).
//
//	kelson.dev/installed-component: <name>   label, the selector handle
//	kelson.dev/installed-version:   <version> label, the pin kelson applied
//	kelson.dev/component-ownership: created | adopted   annotation, the licence to delete
//
// Every object in the pinned manifest is read immediately before it is applied.
// Absent means this apply creates it, and it is stamped `created`. Present means
// it was already there — a `flux-system` namespace somebody made by hand, a
// ClusterRole left behind by an earlier install — and it is stamped `adopted`.
// [Remover] deletes `created` objects and refuses `adopted` ones, re-reading and
// re-checking every one against its live labels with a UID precondition on the
// delete, exactly as internal/delivery/uninstall does for workloads.
//
// Deliberately NOT stamped: app.kubernetes.io/managed-by=kelson. That label is
// half of the project-scoped uninstall selector, and a platform component is not
// a project deployment; claiming it there would put cert-manager's ClusterRoles
// in the same query as an application's Deployments. Upstream sets that label
// for its own purposes too, and taking the field from its owner is the silent
// overwrite ADR-0001 forbids.
package install
