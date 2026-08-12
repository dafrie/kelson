# Release policy

kelson is pre-1.0. The versioning, deprecation and upgrade-path rules below
are the rules we live by *now*, not a target for the future. They are written
down so that "is this a breaking change?" has a single answer: this document.

## Semantic versioning

Releases follow [Semantic Versioning 2.0.0](https://semver.org/) as
`MAJOR.MINOR.PATCH`.

Before `1.0.0`, the rules deliberately loosen:

- `0.MINOR.PATCH` releases **may** contain breaking changes, but only on a
  `MINOR` bump. **PATCH releases are bug-fix only** — they must not change the
  rendered output of any spec that rendered successfully before. This is the
  convention the broader Go and Kubernetes ecosystems operate under, and it
  sets a usable expectation while the spec is still moving.
- The renderer version stamped into rendered-manifest provenance
  ([architecture](architecture.md#provenance)) is the `kelson` binary
  version. A PATCH bump changes nothing the user sees; a MINOR bump is the only
  signal that "how we render may have changed."

A release is cut by pushing a tag of the form `v0.X.Y`. The GitHub workflow
[`.github/workflows/release.yml`](../.github/workflows/release.yml) runs
goreleaser, builds the binaries and a container image, and opens a draft GitHub
release. Cutting a release is one command:

```
git tag v0.X.Y && git push origin v0.X.Y
```

### What this means for the Application spec

The Application spec (`kelson.dev/v1alpha1`) is a versioned input to the
renderer, not a private schema. It is what users, agents and the UI all author
against. Compatibility rules:

- **Renaming or removing a field** is a breaking change. Pre-`1.0.0` it is
  allowed on a `MINOR` bump but must be gated by a deprecation cycle
  (below).
- **Adding a new optional field** is never breaking.
- **Tightening validation** is a breaking change — strictness that rejects
  input that previously rendered successfully belongs on a `MINOR` bump with a
  release note. *This is the rule that makes `kelson preview` worth trusting
  across upgrades.*
- **Loosening validation** is not breaking and may ship on a PATCH.

## CRD / spec API versioning

The control-plane CRDs and the Application/Project/Environment spec share the
`kelson.dev/v1alpha1` API version. A breaking change to the schema *that
cannot be expressed as a field deprecation* requires a new API version
(`v1beta1`, then `v1`). The cycle:

1. Introduce the new version alongside the old: `served: true, storage: true`
   on the new, `served: true, storage: false` on the old.
2. Ship a conversion webhook that round-trips losslessly between old and new.
   The renderer accepts both versions; the storage version is the new one.
3. Mark the old version `served: true, storage: false` for at least one minor
   release.
4. Remove the old version no earlier than `1.0.0`, and only after one full
   release cycle of `served: false`.

Renaming a field — the common cause of "I want v2" — is *not* by itself a
reason to bump the API version. Rename-by-deprecation is preferred: add the
new field, accept both for one cycle, error if both are set, remove the old.
This is the Kubernetes convention and matches `kubectl explain` behavior on
`apiextensions.k8s.io`.

## Deprecation policy

A deprecation cycle is the contract that lets users upgrade kelson without
their manifests breaking. A field deprecation:

1. **Announce.** In the minor release where the field is marked deprecated,
   the renderer writes a deprecation marker into provenance, `kelson render
   --validate` warns, and the release notes name it.
2. **Coexist.** For the next `N−1` minor releases — `N ≥ 2` pre-`1.0.0`,
   `N ≥ 4` post-`1.0.0` — both the old and new spelling render successfully.
   The old spelling is never silently rewritten.
3. **Remove.** In the `N`th minor release, the old field is removed. Rendering
   a spec that still sets it is a hard error that names the removal version
   and the replacement.

The same cycle applies to CLI flags and MCP tool arguments, with one
difference: tool surfaces follow [capability parity, not surface
parity](architecture.md#agent-surface) with the API, so a tool argument goes
away when the corresponding API field does — never independently.

## Supported-version policy

- **Pre-`1.0.0`**: the latest minor release is supported. We accept bug-fix
  backports to the previous minor where the cost is low, with no formal
  guarantee.
- **Post-`1.0.0`**: the latest two minor releases (N and N-1) receive bug
  fixes. Security fixes apply to N and N-1 for the lifetime of 1.x.

The supported-version policy mirrors the deprecation cycle: bug-fix support
for N-1 ends one release after a deprecation closes, so a user running N-1
gets the deprecation warning they need *before* the removal lands.

## Upgrade path

- **Renderer is upgrade-safe by construction**: same spec, same
  `ClusterProfile`, byte-identical output across renderer versions, unless a
  deprecation cycle has explicitly closed. This is the property
  [ADR-0001](adr/0001-hybrid-state-model.md) buys, and why renderer purity is
  enforced in CI ([issue #20](https://github.com/dafrie/kelson/issues/20)).
- **Direct-mode users** receive rendered-manifest history: rolling the
  renderer back to N-1 is a `git revert` against that history.
- **GitOps-mode users** receive Kubernetes manifests: rollback is the GitOps
  system's normal mechanism (a Git revert reconciled by Flux).
- A kelson upgrade never rewrites a working spec; it may re-render different
  bytes from the same spec, gated by the deprecation rules above. **A kelson
  uninstall must not break a running application** ([README](../README.md#whats-different))
  — the rendered manifests stay behind, plain YAML, owner-unsupported.

## Spec compatibility promise

For the lifetime of `1.x` this is a hard promise:

> An Application spec that rendered successfully against `kelson vX.Y.Z` will
> render successfully against any `kelson vX.Y'.Z'` where `Y' ≥ Y`, *unless*
> a deprecation cycle has explicitly closed for a field the spec still sets.
> When a cycle closes, the spec continues to render; the deprecated field is
> dropped or renamed per the rules above, and the release notes name both
> fields.

Pre-`1.0.0`, the promise is the same within a single minor: a PATCH never
breaks a render, and a MINOR only breaks a render through a deprecation that
the previous minor already named.

This is what makes `kelson preview` worth trusting across upgrades, what lets
LLM-authored specs survive a kelson upgrade, and what lets agents treat kelson
as a stable surface. It is the same promise the **renderer purity** rule
(ADR-0001, issue #20) buys in the first place: the rendering plane is the same
pure function it was last release; only its inputs and some of its outputs
change.
