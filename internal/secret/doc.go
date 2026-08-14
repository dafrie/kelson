// Package secret is the authoring half of ADR-0009's secret backends: the one
// thing in kelson that writes a Secret, in either of the two places a value
// can live.
//
// [Store] is the `cluster` backend (issue #116, ADR-0018): a value written
// through the Kubernetes API into the environment's namespace, read back
// masked, never persisted by kelson. [SOPSStore] is the `sops` backend (issue
// #81, [ADR-0022]): the same value encrypted in memory with age recipients and
// committed to the delivery repository, decrypted in-cluster by Flux. The two
// have the same three methods so `kelson secret` selects a backend rather than
// a code path, and everything the rest of this doc says about redaction,
// masking and what kelson does not keep holds for both. Where they differ is
// [SOPSStore]'s own comment, and it differs in exactly one way: `set` writes
// the whole Secret there, because merging would need a key kelson never holds.
// The `externalSecrets` backend has no authoring path here at all — under it
// no process on kelson's side ever holds a value (ADR-0020).
//
// [ADR-0018] decided the reading half. An environment value that is a mapping
// is a reference — `{secret: <name>, key: <key>}` — and the renderer turns it
// into a `valueFrom.secretKeyRef` against a Secret in the environment's
// namespace that it never creates, reads, diffs or owns. Until this package
// existed, nothing kelson-shaped created that Secret: the remediation named
// `kubectl create secret generic`, and ADR-0018 recorded that as the gap #116
// would close.
//
// # kelson does not persist secret values
//
// The cluster is the store. This package holds a value for exactly as long as
// one [Store.Set] call takes, writes it through the Kubernetes API, and then
// reads back keys and metadata only. [Secret] — what Set returns and what List
// returns — has no field a value could travel in, which is why "List masks the
// values" is a property of the type rather than of a formatting function
// somebody has to remember to call.
//
// Every value this package is handed is passed to [redact.Register] as the
// first statement of [Store.Set], before validation and before any client call.
// From then on it cannot reach a log, an error or an API response anywhere in
// the process (issue #117). Registering at the moment the value is learned,
// rather than at each surface that might echo it, is what makes that a property
// of the process instead of a rule every future caller has to follow.
//
// # kelson deletes and overwrites only what kelson manages
//
// Every Secret written here carries `kelson.dev/managed-secret: "true"`
// alongside the ordinary provenance labels. Three things follow, and they are
// the reason the label exists rather than being a decoration:
//
//   - **Listing is a label query.** [Store.List] asks the API server for the
//     labelled Secrets, so a namespace's TLS material, service-account tokens
//     and image-pull credentials are not kelson's to enumerate and are never
//     reported as though they were.
//   - **Deleting an unlabelled Secret is refused** with [ErrNotManaged]. A
//     namespace holds Secrets kelson never wrote, and `kelson secret delete`
//     must not be a way to remove one by guessing its name.
//   - **Writing into an unlabelled Secret is refused too**, with the same code.
//     Adopting somebody else's Secret by name would make the label — and
//     therefore the listing and the delete rule — a lie. The remediation names
//     the one-line `kubectl label` that adopts it deliberately.
//
// # Set merges; it does not replace the Secret
//
// A server-side apply owns the fields it sends, so applying `{url}` after
// `{token}` would prune `token` — two commands, one silently lost credential.
// So the apply configuration is the union of the keys being written and the
// keys the live Secret already has, and `kelson secret set` reads as "set these
// keys" rather than "this is now the whole Secret". Removing a key is deleting
// the Secret and writing it again; a narrower `unset` is #116's remaining
// scope.
//
// Carrying the untouched keys through means Set briefly holds values it was not
// given. They are registered with [redact.Register] on the way past, exactly
// like the ones the caller supplied, and they are never formatted into
// anything: they go from the read straight into the apply configuration.
//
// # Why a package outside internal/delivery
//
// Everything that talks to an API server is delivery-plane by the architecture's
// own rule, and this package's lint allow-list says so (.golangci.yml lists it
// beside internal/serverstate for the same reason). It is not a delivery
// *adapter*, though: it applies nothing the renderer produced, takes part in no
// revision, and records no history. It is one narrow capability — write this
// Secret, list what kelson wrote, delete what kelson wrote — that the CLI, the
// API and the MCP surface all reach through the same seam.
//
// [ADR-0018]: https://github.com/dafrie/kelson/blob/main/docs/adr/0018-secret-references.md
// [ADR-0022]: https://github.com/dafrie/kelson/blob/main/docs/adr/0022-sops-age.md
package secret
