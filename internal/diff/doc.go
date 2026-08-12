// Package diff is part of the RENDERING plane (docs/architecture.md). It
// turns two rendered manifest sets into one structured, machine-readable Diff
// (issue #44), and renders that Diff for terminals and JSON.
//
// # Why a shared, typed representation
//
// ADR-0002 makes the UI, CLI and MCP server peers. If the diff format were
// designed for terminal output, agents would parse colour codes; if designed
// for the UI, the CLI would reimplement it. The rendering differs per client;
// the data must not. The Diff is also the unit of interaction for agents — an
// agent proposes, a renderer produces a diff, policy checks it, a human
// approves — so its Risk field is a typed enum, never prose an agent would
// have to string-match (issue #44 acceptance criterion).
//
// # Levels
//
// LevelRendered (L1, issue #42) diffs the output of the pure renderer: old
// spec rendered, new spec rendered, structured diff. LevelServer (L2, issue
// #43) diffs the API server's own dry-run verdict. Both populate the same
// Diff; this package ships the L1 engine only — L2 arrives with the delivery
// agent that talks to the cluster.
//
// # Purity
//
// Like the renderer it consumes, this package is deterministic: it imports no
// Kubernetes client, performs no I/O, and never depends on map iteration
// order. The same two manifest sets always produce the same Diff, which the
// tests assert on every fixture. It necessarily touches the renderer (its
// input type) and therefore never crosses into the DELIVERY plane.
package diff
