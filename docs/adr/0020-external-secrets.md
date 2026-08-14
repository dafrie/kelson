# ADR-0020: The `externalSecrets` backend — an ExternalSecret per referenced Secret, and no per-backend code

- **Status:** Accepted
- **Date:** 2026-08-14

## Context

[ADR-0009](0009-secrets.md) decided the doctrine — the spec carries references, never values — and
named three backends. [ADR-0018](0018-secret-references.md) decided the syntax: a mapping in an env
value's own place, `{secret: <name>, key: <key>}`, backend-agnostic by construction, with
`Environment.spec.secrets.backend` selecting the mechanism that puts a value where the reference
points. It shipped one of the three and refused the other two by name.

[#80](https://github.com/dafrie/kelson/issues/80) is the second. Its own framing states the preference
plainly: *"kelson renders `ExternalSecret` resources and never holds a value."* That is not a
convenience — it is the strongest form of ADR-0009's guarantee available. Under `cluster` a human types
the credential into `kelson secret set`, and kelson's API handler holds those bytes for the length of a
server-side apply ([#116](https://github.com/dafrie/kelson/issues/116)). Under `externalSecrets` no
process on kelson's side ever holds it at all: the value travels from a secret manager to the
external-secrets controller to a Secret in the cluster, on a path kelson writes the address of and
never stands on.

ADR-0018's "revisit when" clause set the test this ADR has to pass:

> **#80 or #81 lands a second backend.** The question then is whether `secretKeyRef` remains the
> rendered shape for all three… If the spec text has to change for either, this decision was wrong and
> the ADR that fixes it should say so.

It does not have to change. The workload half of the render is byte-identical under both backends, and
`internal/renderer/externalsecrets_test.go` asserts exactly that rather than asserting it in prose.

Three questions were open, and #80's own text asks the third out loud.

**Where do the stores come from?** The ClusterProfile has recorded `externalSecrets` since detection
was written, and store discovery was already in the type. What it recorded was two lists of *names*.

**What does the spec have to say?** `secrets.store` has existed as a validated, gated field since the
model was written, described in the Go doc as "the ClusterSecretStore for backend externalSecrets" — a
narrower reading than the API supports, and one that nothing had yet been forced to make precise.

**Does supporting four secret managers mean four code paths?** The issue notes the suspicion and asks
for it to be checked against the API rather than assumed. §5 checks it.

## Decision

### 1. One ExternalSecret per referenced Secret, ahead of everything that reads one

```yaml
env:
  DATABASE_URL:   { secret: checkout-db, key: url }
  STRIPE_API_KEY: { secret: payments,    key: stripe-api-key }
  SESSION_PEPPER: { secret: payments,    key: session.pepper }
```

renders, under `backend: externalSecrets`, the same three `valueFrom.secretKeyRef` entries it renders
under `cluster` — plus two ExternalSecrets, one per *Secret name*:

```yaml
apiVersion: external-secrets.io/v1
kind: ExternalSecret
metadata:
  name: payments
  namespace: checkout-staging
spec:
  refreshInterval: 1h
  secretStoreRef:
    name: vault-backend
    kind: ClusterSecretStore
  target:
    name: payments
    creationPolicy: Owner
  data:
    - secretKey: session.pepper
      remoteRef: { key: payments, property: session.pepper }
    - secretKey: stripe-api-key
      remoteRef: { key: payments, property: stripe-api-key }
```

**Per Secret name, not per reference.** Two variables reading two keys of `payments` are one remote
secret with two properties. Rendering two ExternalSecrets against one `target.name` would produce two
controllers-eye-view owners of one Secret, each pruning the other's keys on every refresh — a fight
that resolves differently depending on reconcile order.

**The resource takes the target Secret's own name.** There is one ExternalSecret per Secret, they live
in the same namespace, and a derived name (`<project>-<environment>-payments`, say) would only make a
reader map between two spellings of one thing. The cost is accepted and stated: a name collision with
an ExternalSecret somebody else wrote surfaces as a field-ownership conflict at apply. That is the loud
outcome; a second writer for one Secret that nobody notices is not.

**They render immediately after the Namespace**, ahead of the data services and the workloads. A
rendered set expresses sequencing only through order ([#89](https://github.com/dafrie/kelson/issues/89)),
nothing waits for a sync, and the failure this ordering avoids is benign either way — a pod that
retries. It is ordered anyway because a reader looking for "what populates this" should find it above
"what reads this", and because a data component's `auth:` Secret is read by an operator that starts
immediately.

**`creationPolicy: Owner` is written explicitly** even though it is external-secrets' own default. It
is the field that decides whether `kelson uninstall` leaves a credential behind — Owner means the
controller garbage-collects the Secret with the ExternalSecret — and [ADR-0001](0001-hybrid-state-model.md)'s
deletability requirement is not something a reader should have to know a controller's defaults to
verify.

### 2. `remoteRef` is `{key: <secret name>, property: <key>}`, and no prefix lives in the spec

The spec's two parts map onto the API's two parts, unchanged. An author who wrote
`{secret: payments, key: stripe-api-key}` finds the value at `payments`.`stripe-api-key` in their
secret manager, with no translation table between the YAML and the store.

This is the shape every provider already uses for a structured secret: a Vault KV entry with fields, an
AWS Secrets Manager secret holding JSON, a GCP secret holding JSON, an Azure Key Vault secret with an
object value. `property` is the field selector in all of them.

**Where the *prefix* comes from is deliberately not kelson's question.** A Vault mount and path, an AWS
name prefix, a GCP project — these are SecretStore configuration, written once by whoever owns the
store. A `pathPrefix:` in the spec would give every environment two places to be wrong about where its
secrets live, and the one that already exists is the one the store's owner controls.

**Explicit `data` entries, never `dataFrom`.** Pulling the whole remote secret in one go would be
fewer lines and would hide the failure #80 exists to surface: a missing property would sync happily,
produce a Secret without that key, and leave the workload to fail at pod start against a key that is
simply absent. With one entry per key, external-secrets fails the sync and says which property it could
not find, and §4 relays that.

### 3. The store is resolved from the profile, and ambiguity is refused rather than broken

`Environment.spec.secrets.store` is a **name**, optional, resolved at render time against the
ClusterProfile:

| The spec says | The profile offers | Result |
|---|---|---|
| a name | a ClusterSecretStore with that name | `kind: ClusterSecretStore` |
| a name | a SecretStore with that name **in this environment's namespace** | `kind: SecretStore` |
| a name | both | `render/external-secrets-store-ambiguous` |
| a name | neither | `render/external-secrets-store-not-found`, listing what the cluster does have |
| nothing | exactly one store | that one |
| nothing | several | `render/external-secrets-store-ambiguous`, listing them |
| nothing | none | `render/external-secrets-store-not-found` |

Three decisions are folded into that table.

**The *kind* is always the profile's, never the spec's.** Whether `vault-backend` is a namespaced
SecretStore or a cluster-scoped ClusterSecretStore is a fact about the cluster. Asking an author to
restate it — a `storeKind:` field beside `store:` — would add a field whose only failure mode is
disagreeing with reality, and whose correct value they would have to look up in the same profile kelson
already has.

**The ClusterProfile now records a SecretStore's namespace.** It recorded names alone, which cannot
answer this question: a SecretStore is readable only from its own namespace, so a bare name would make
a store in `team-a` look available to `team-b`. `ClusterSecretStores` stays a name list, because a
cluster-scoped object has no namespace to record — the asymmetry is the API's and flattening it would
lose the fact the resolution turns on.

**Defaulting happens only when there is exactly one answer.** This is the same rule
`routing.gatewayClass` already follows, and the reasoning is stronger here: picking between several
stores would bind every credential in an environment to whichever one sorted first. A wrong store is
not a render failure that someone notices — it is a workload reading a credential from somewhere nobody
intended, or an ExternalSecret stalling with an unresolved reference far from the spec that caused it.
Stopping is cheaper than either.

### 4. A failed sync is a verdict, in the list that already exists

`internal/observation` reads the ExternalSecret's `Ready` condition and folds it into an ordinary
`Verdict` with a new failure code, `secret-sync-failed`:

```
external-secrets.io/ExternalSecret/checkout-staging/payments degraded: secret-sync-failed
  (SecretSyncedError: could not get secret data from provider: cannot get secret "payments": permission denied)
  fix: external-secrets could not sync this Secret: …
```

**It is the existing shape, deliberately.** The verdict list is what `kelson status` prints, what the
Status RPC returns, what the event stream diffs and what `diagnose_application` reads. Adding a
parallel "secret status" surface would mean four consumers learning a second vocabulary for "this
environment is broken and here is why". Instead the sync verdicts are listed **first**, ahead of the
workloads: a Secret that never synced is *why* the pods below it are stuck, and the cause belongs above
the symptom.

**Ready=False is a failure; Ready=Unknown and a not-yet-reconciled object are Progressing.** The
tri-state discipline of [#144](https://github.com/dafrie/kelson/issues/144) applies here as everywhere:
the controller has looked and said no, or it has not looked yet, and those are different answers. A
deploy must not flash red on its way to green.

**The probe reads the ExternalSecret and never the Secret it produces.** Checking whether the keys
actually arrived would mean kelson reading a Secret — the one thing ADR-0009 exists to prevent, and the
first place in the codebase that would ever hold a credential. The ExternalSecret's own status answers
the question anyway. The controller's `reason` and `message` are relayed verbatim, as the kubelet's
reasons already are; they describe a failure to *reach* a value, and external-secrets does not put
secret material in them.

### 5. Four backends, no per-backend code — verified against the ESO API

#80 asks for this reading to be checked rather than assumed. It was, against
`external-secrets/external-secrets` at `apis/externalsecrets/v1`:

- **`SecretStoreSpec.Provider` is a union with one arm per backend.** `aws`, `azurekv`, `vault`,
  `gcpsm` and roughly forty more, each carrying that provider's endpoints, auth and options. Every
  backend-specific field in the whole API lives under it.
- **`ExternalSecretSpec` has no provider arm.** Its fields are `secretStoreRef`, `target`,
  `refreshPolicy`, `refreshInterval`, `syncWindows`, `data`, `dataFrom` — all provider-agnostic. A
  `remoteRef` is `{key, property, version, metadataPolicy, conversionStrategy, decodingStrategy}`, and
  what those *mean* is the provider's business on the other side of the store reference.
- **`secretStoreRef.kind` is exactly `SecretStore` or `ClusterSecretStore`.**
- **`target.creationPolicy` is `Owner | Orphan | Merge | None | CreateOrMerge`**; `refreshInterval`
  defaults to `1h0m0s`; the `Ready` condition's reasons are `SecretSynced`, `SecretSyncedError`,
  `SecretDeleted`, `SecretMissing`.

**So the reading holds, and it is recorded here as a claim that can be falsified.** kelson renders the
identical document for Vault, AWS Secrets Manager, GCP Secret Manager and Azure Key Vault, and for the
forty-odd others; supporting a fifth is somebody installing a newer external-secrets, not a kelson
release. This is [ADR-0005](0005-delegate-to-operators.md) working exactly as intended: kelson writes a
CR and delegates, and the operator's own extension point is the one that grows.

The boundary this puts on kelson is worth stating as plainly: **kelson does not create, configure or
validate a SecretStore.** Vault addresses and roles, IRSA annotations, workload-identity bindings and
Key Vault tenant IDs are the cluster administrator's, written once, outside kelson's spec. There is no
`kelson secretstore create` and this ADR does not want one — it would mean kelson holding the
credentials that reach the credentials.

### 6. `refreshInterval`, and why there is no spelling for "never"

`Environment.spec.secrets.refreshInterval` is a Go duration, defaulting to `1h` — external-secrets' own
default, written into the manifest explicitly for the reason the previews interval is: the manifest
should say what the cluster will do.

It is parsed in **validation**, not in the renderer, because the renderer may not import `time`
([ADR-0001](0001-hybrid-state-model.md), [#20](https://github.com/dafrie/kelson/issues/20)). The
renderer writes the validated string through verbatim. The default is filled in during **resolution**,
beside the previews defaults, so the renderer never has to know what an unset interval means.

**A non-positive interval is refused.** external-secrets reads `0` as "sync once and never again",
which is a real behaviour with real uses — and it is not one an author arrives at by typing `0` into a
field named `refreshInterval`. Accepting it would mean a credential that silently stops rotating.
Giving it a name would mean deciding what that name is and what it promises; that decision is not taken
here, and the error says so rather than pretending the field is simply invalid.

`store` and `refreshInterval` both configure this backend and nothing else, so setting either under
`cluster` or `sops` is `schema/mutually-exclusive`. Accepting a store name that configures nothing is
the quiet success [#141](https://github.com/dafrie/kelson/issues/141) exists to prevent.

### 7. The #141 gate table loses its last secrets row

`secrets.store` was the narrowed remnant ADR-0018 left behind. It and `refreshInterval` are now
consumed — they are the ExternalSecret's `secretStoreRef` and `spec.refreshInterval` — so both are on
the rendered allow-list in `internal/model/coverage_test.go` and the gate rows are gone.

`sops` keeps `render/secret-backend-unsupported` naming [#81](https://github.com/dafrie/kelson/issues/81),
unchanged. **SOPS is not implemented here and nothing in this ADR moves toward it**: its mechanism is a
decryption step in the delivery path, not a resource in the rendered set, and it is the one place
ADR-0018's "revisit when" might still turn out to be right.

## Consequences

**Positive.**

- **kelson never holds a value under this backend.** Not as a policy — as an absence of any code path
  that could. The renderer emits addresses, the observation plane reads a condition, and the value
  moves from the secret manager to the cluster without passing through kelson's memory, storage or
  logs. That is #80's acceptance criterion satisfied structurally.
- **ADR-0018's promise survived contact with its first migration.** The spec text, the union, the merge
  rules and the workload render are untouched; switching a real environment is one field on one
  Environment, and a test proves the two renders differ by nothing but the ExternalSecrets.
- **Four secret managers, zero per-backend code.** The claim is now checked against the API and written
  down where it can be falsified.
- **The failure #80 names as unacceptable is visible with its cause.** A sync error is a degraded
  verdict carrying the controller's own reason, above the workloads it broke, in every surface that
  already shows verdicts.

**Negative.**

- **A `valuesFrom` Secret and a previews `secretRef` get no ExternalSecret.** Both name a Secret
  without naming keys, and an ExternalSecret's `data` is a list of keys — kelson does not know what a
  chart's values file or a forge credential contains. Under this backend those Secrets still have to be
  written out of band, which is a real inconsistency: the backend is Environment-wide and its coverage
  is not. Fixing it needs a spelling for "the whole remote secret", which is `dataFrom` and the failure
  mode §2 rejects, so it needs its own decision rather than an extension of this one.
- **The remote layout is fixed by convention, not configuration.** `remoteRef.key` is the Secret's name
  and nothing changes that. An organisation whose Vault paths do not match their Kubernetes Secret
  names has to reshape one side or write per-store SecretStores whose provider config supplies the
  path — which works, and is more setup than a `pathPrefix:` would have been.
- **Rendering now depends on the cluster in one more way.** An environment on this backend cannot
  render without a profile: `render/external-secrets-not-installed` for a profile that reports no
  operator, and a store cannot be resolved from nothing. That matches the data-service gates and it
  narrows `kelson render`'s offline usefulness for these specs by exactly as much.
- **The ExternalSecret takes the Secret's name, so kelson can collide with a human.** An
  administrator's hand-written ExternalSecret named `payments` and kelson's are one object. The
  conflict surfaces at apply rather than being designed out.
- **A namespaced SecretStore does not reach a PR preview.** A preview renders through this same
  renderer into its own namespace (`internal/preview`, [ADR-0017](0017-pr-previews.md)), so a
  `SecretStore` living in the parent environment's namespace is not a candidate there and the preview
  refuses with `render/external-secrets-store-not-found`. That is the loud failure rather than a
  preview quietly running without its credentials, and the working shape is a `ClusterSecretStore` —
  but it means "which store" is now a question an environment answers on behalf of namespaces it does
  not know the names of yet.
- **kelson still does not know whether the *remote* secret exists.** ADR-0018's negative — "a reference
  to a Secret nobody created renders cleanly and fails at pod start" — moves rather than closes: it now
  fails at sync time with a named reason, which is far better, and it is still not something render or
  validation can catch.
- **`secret-sync-failed` is a new failure code, and codes are a compatibility promise.** A consumer
  branching on the code set sees one it does not know. It is additive and `IsFailure` already covers
  it, so the degradation is graceful, but it is a widened contract.

## Revisit when

- **Somebody needs a Secret whose keys kelson does not know.** That is the `valuesFrom` gap above, and
  the answer is probably `dataFrom` with the missing-key failure mode accepted and stated — which is a
  decision, not an omission to quietly fix.
- **Somebody needs a remote path that is not the Secret name.** A `pathPrefix:` or a per-reference
  remote key is the obvious shape; the question to answer first is why the SecretStore's own provider
  config is not the right place for it.
- **#81 lands SOPS.** If its mechanism turns out to need something other than `secretKeyRef` in the
  workload, ADR-0018's decision was wrong for one of three backends and the ADR that fixes it should
  say so plainly.
- **external-secrets promotes a `v2`.** kelson writes `external-secrets.io/v1` in the renderer, reads
  it in detection and reads it in the observation plane; those three must move together or the writer
  and the readers will disagree about which objects exist.
- **`refreshPolicy` or `syncWindows` is asked for.** Both exist upstream, neither is exposed, and
  either would be the second and third fields under `secrets:` — at which point whether this block
  should mirror the ESO API rather than curate it becomes a real question.
