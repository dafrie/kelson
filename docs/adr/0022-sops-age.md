# ADR-0022: The `sops` backend — encrypt on write, decrypt in-cluster, and kelson holds no key

- **Status:** Accepted
- **Date:** 2026-08-14

## Context

[ADR-0009](0009-secrets.md) named three backends and shipped one.
[ADR-0018](0018-secret-references.md) decided the syntax — `{secret: <name>, key: <key>}` in an env
value's own place — and shipped `cluster`. [ADR-0020](0020-external-secrets.md) shipped
`externalSecrets` and left the third with a sentence that reads, in hindsight, like a dare:

> `sops` keeps `render/secret-backend-unsupported` naming [#81](https://github.com/dafrie/kelson/issues/81),
> unchanged. **SOPS is not implemented here and nothing in this ADR moves toward it**: its mechanism is
> a decryption step in the delivery path, not a resource in the rendered set, and it is the one place
> ADR-0018's "revisit when" might still turn out to be right.

That reading was correct about the mechanism and wrong about the consequence. The decryption step is in
the delivery path, and ADR-0018's decision survives anyway: the workload half of a `sops` render is
byte-identical to a `cluster` render, and `internal/renderer/secrets_test.go` asserts it rather than
claiming it. The spec text does not change for any of the three. That was the stated test and this is
the third and last time it had to hold.

**What this backend is for** is stated in #81's own 2026-08-12 amendment. ADR-0009 made `cluster` the
v0.1 answer, so SOPS is not needed for Git mode to be *safe*. It closes the gap ADR-0009 wrote into its
own consequences and asked to have documented rather than discovered:

> **Secrets are not in the reproducible artifact.** Rebuilding a cluster from Git alone will not restore
> them. This is the real cost of the v0.1 backend.

Under `sops` the credential is in the artifact, encrypted. A cluster rebuilt from the repository comes
back with its secrets, and the only thing that has to survive outside Git is one age identity.

**And the design constraint is #81's closing sentence**, which is the reason several of the decisions
below are refusals rather than conveniences:

> It must be genuinely easy, or users will look for a way to commit plaintext — and the correct response
> to that pressure is a better encrypted path, not an exception.

## Decision

### 1. The spec: `secrets.backend: sops`, plus the public recipients

```yaml
apiVersion: kelson.dev/v1alpha1
kind: Environment
metadata: {name: production}
spec:
  project: checkout
  delivery:
    mode: flux
    git:
      repo: https://github.com/acme/deploy
      branch: main
      path: clusters/prod/checkout
  secrets:
    backend: sops
    ageRecipients:
      - age13w78znajf5kee8msacel80jz6qeuc9tyxhuqkwnqcsaymlrj7clsy4fgdw
    ageKeySecret: sops-age          # optional; this is the default
```

Two fields, scoped to this backend exactly as `store` and `refreshInterval` are scoped to
`externalSecrets` (ADR-0020 §6): setting either under `cluster` is `schema/mutually-exclusive`, not
silence.

**`ageRecipients` is a list of *public* keys and it belongs in the spec in the clear.** That is not a
compromise, it is the shape of the mechanism: encrypting needs the recipient, decrypting needs the
identity, and kelson only ever does the first. A reader who finds an `age1…` string in a repository has
found nothing they did not already have.

**It is required, not defaulted.** There is nothing to default an encryption key to, and the refusal
fires at validation with the `age-keygen` line in it. Failing at the first `kelson secret set` instead
would put the error in front of a human who is holding a credential and least wants to go and read
documentation.

**A value beginning `AGE-SECRET-KEY-` gets its own message.** Every other bad recipient is a typo; that
one is a private key on its way into Git, and "invalid format" would not say so. The message says what
happened and what to do about a key that has already been committed.

**`ageKeySecret` is defaulted during resolution**, beside the `externalSecrets` refresh interval and for
the same reason: the pure renderer writes it into a Kustomization's `spec.decryption.secretRef` and must
never have to decide what an empty one means.

### 2. The gate: sops requires flux, refused at render time from spec data alone

`render/sops-requires-flux` is the third instance of a gate this project already has twice — charts
([ADR-0016](0016-delivery-flows-v0.md) decision 4) and previews ([ADR-0017](0017-pr-previews.md)
decision 5) — and it is decided in the same place for the same reason: from spec data, before anything
is emitted, so the same document renders the same way against every cluster.

The failure it prevents is not subtle. Direct mode has no kustomize-controller and no repository. kelson
would apply the workloads, nothing would ever turn the encrypted file into a Secret, and every pod would
fail at start against a Secret that exists only as ciphertext in a directory nobody applied.

The git target is deliberately **not** checked a second time. `delivery.mode: flux` already requires one
(`semantic/git-target-missing`), and a second check would be a second spelling of an error the author
has already been given.

### 3. Encrypt on write: the plaintext never leaves kelson's address space

```
kelson secret set checkout-db -f spec.yaml --env production url=postgres://…
        │
        ├── redact.Register(value)          ← first statement, before anything can fail
        ├── read the spec → backend: sops, recipients, delivery.git target
        ├── clone the delivery repo at HEAD (in-memory billy filesystem)
        ├── refuse if this write would drop keys the committed file already has  (§5)
        ├── sops.EncryptSecret(...)         ← AES-256-GCM in memory, age-wrapped data key
        └── commit <path>/secrets/checkout-db.enc.yaml and push
```

**There is no temporary file and no subprocess.** The value exists as the caller's own map and as the
byte slices of one AES-GCM seal. The git writer works on an in-memory filesystem, so even the
*ciphertext* never reaches the caller's disk. #81's acceptance criterion — "plaintext never appears in
the working tree or in kelson's storage" — is satisfied by there being no code path that could write
one, which is the same shape of guarantee ADR-0018 §4 makes about the renderer.

**The file lives at `<delivery path>/secrets/<name>.enc.yaml`.** Inside the path, because
kustomize-controller walks the path it reconciles recursively: a file there is applied by the same
Kustomization as the workloads that reference it, decrypted by that Kustomization's `spec.decryption`,
and **pruned by it** when the file goes. Outside the path it would need a second Kustomization, a second
decryption block and a second thing to remember to delete.

Two writers now share one directory with different ideas of what they own, and both wrong answers lose
production data. So `internal/delivery/git` grew the distinction explicitly: `Stage` (a deploy) makes
the path match the render and prunes what is not in it, with `secrets/` added to the preserved set
beside dotfiles; `Put` (a secret write) changes exactly the files it names and prunes nothing. A deploy
that pruned would delete every credential in the environment; a `secret set` that pruned would delete
the environment.

**The rendered set is unchanged and still contains no Secret.** The renderer has no plaintext and no way
to acquire one — it is pure (ADR-0001) — so the encrypted file is written entirely outside the render
path. `kelson render` under this backend produces exactly what it produces under `cluster`.

### 4. The format is implemented, not imported — and here is the measurement

kelson implements the SOPS file format directly over `filippo.io/age` and the standard library's AES-GCM
and SHA-512 (`internal/sops`). Encryption only: there is no `Decrypt` and no code path that could hold a
private key.

Using `github.com/getsops/sops/v3` as a library was the first choice and was measured against v3.13.3:

| Import | Additional modules |
|---|---|
| `github.com/getsops/sops/v3/age` alone | 12 — but it cannot build a tree |
| `github.com/getsops/sops/v3` (root) + `aes` + `age` | **98** |
| `github.com/getsops/sops/v3/stores/yaml` | 119 |

The 98 include the AWS SDK, the Azure SDK, Google Cloud KMS, HashiCorp Vault's API client, the MongoDB
driver, OpenTelemetry and gRPC. They are unavoidable: `sops.Metadata` names every key provider the tool
supports, and the root package is not separable from them. kelson's entire dependency list is 16 direct
modules, and the `sops` binary built from that graph is **74 MB**. That is not a dependency a CLI takes
on in order to encrypt a two-key Secret.

Shelling out to a `sops` binary was the second choice, and it is the honest fallback this ADR records
rather than a straw man. It was rejected on two counts: it makes a second executable a runtime
dependency of `kelson secret set` — one the CLI would have to check for and name when missing — and the
plaintext then has to reach that process, through a pipe or, worse, a file. §3's guarantee is worth more
than the third-party implementation would have been.

**What was implemented is deliberately narrow**: one document shape (a Kubernetes Secret), one key type
(age X25519 recipients), one `encrypted_regex` (`^(data|stringData)$`, which is Flux's own guide's), and
encryption only. The cost is stated plainly: kelson now owns a cryptographic file format it did not
design, and a change upstream is a change kelson has to follow.

**So correctness is checked twice, and the second check is against the implementation that matters.**
`TestRoundTrip` decrypts with a decryptor written from the format description rather than from
`encrypt.go`, and verifies the MAC covers unencrypted leaves too — the property that stops a ciphertext
being moved to a Secret of another name. `TestSopsBinaryDecrypts` runs the real `getsops/sops` against
kelson's output, and `TestSopsBinaryDetectsTampering` checks it rejects an edited file. Both skip when no
binary is on PATH, which is the normal case in CI; both pass against v3.13.3.

The added dependency footprint is `filippo.io/age` and `filippo.io/hpke`, both BSD-3-Clause, and
**+184 KB** of `kelson` binary — 32,813,321 → 32,997,641 bytes stripped, +0.56%, measured against the
commit this branch was rebased onto.

### 5. `set` writes the whole Secret, and says so when that would cost you a key

This is the one place the two authoring backends behave differently, and it follows from the guarantee
rather than from an implementation shortcut.

Under `cluster`, `set` merges: it reads the live Secret and carries the untouched keys forward
(ADR-0018's successor note). Here the untouched keys are ciphertext under a data key kelson **cannot
open**, because opening it needs the age identity kelson deliberately never holds. There is no version
of "merge" available that does not first undo §6.

So a `set` that would drop keys is **refused by name** (`secret/sops-partial-set`) rather than done
quietly. SOPS encrypts values, not structure, so the key names in the committed file are readable
without any key at all — the refusal lists exactly which keys would be lost and what to pass. A write
that names every existing key, which is the ordinary case, simply works, and so does one that adds keys.

Deliberately **no `--replace` flag.** Dropping a key is `kelson secret delete` followed by
`kelson secret set`, which is two commands that each say what they do; a flag whose meaning is "yes,
lose those" is a flag people pass to make an error go away.

### 6. kelson never holds an age private key, and that is why rotation is a report

The identity that opens these files lives in a Kubernetes Secret the **operator** creates, read by
kustomize-controller and by nothing of kelson's:

```sh
age-keygen -o age.key                     # keep this out of Git
kubectl -n flux-system create secret generic sops-age --from-file=age.agekey=age.key
```

and the Kustomization that reconciles the path carries:

```yaml
spec:
  decryption:
    provider: sops
    secretRef:
      name: sops-age
```

**kelson does not write that Kustomization**, because the reconciler's own objects belong to the
cluster's bootstrap path and not to kelson (ADR-0012, `internal/delivery/eject/bootstrap.go`). It writes
the one Kustomization it does own — the per-preview one inside the previews ResourceSet template — from
the same function that prints the block to the operator, so the two cannot drift. And
`kelson secret set` prints that block and the `kubectl` line every time it writes an encrypted file,
because a Kustomization without it applies the ciphertext verbatim: a Secret whose value is the literal
string `ENC[AES256_GCM,…]`, and a workload that starts with a credential that is not one.

**Rotation therefore reports rather than re-encrypts.** `kelson secret rotate` compares every committed
file's recipient list — stored in the clear beside the data key it wrapped, precisely so a reader can
answer "who can open this" without being one of them — against the environment's `ageRecipients`, and
names per file which identities cannot read it, which retired ones still can, and both commands that fix
it:

```
kelson secret set <name> -f <spec> --env production url=<value> token=<value>
    re-encrypts from the values. Needs no key at all; needs the values.
sops updatekeys <path>
    re-wraps the existing data key. Needs an identity that can already read it; needs no values.
```

It exits 0 with no drift and 2 with drift, on the `kelson diff` contract, so it can gate CI.

Doing the re-encryption itself would mean kelson taking an age identity as input, and every argument for
that is an argument for the thing this decision exists to prevent. The report is the part humans get
wrong anyway: knowing *which* files are stale after a key change.

**Recovery, stated plainly and documented in `docs/secrets.md`:** lose the identity and the encrypted
files are unreadable by anyone, kelson included. There is no escrow, no recovery code and no back door,
which is the same sentence as "kelson holds no key" read from the other side. The mitigations are
mechanical — more than one recipient, so a second identity opens every file; and the identity backed up
outside the repository — and the ADR says so rather than implying a safety net that does not exist.

### 7. A failed decryption is named as its own cause

A Kustomization that cannot decrypt reports `BuildFailed`, and kustomize-controller's message describes
a *mechanism* — "Error getting data key: 0 successful groups required, got 0" — rather than a situation.
The situation is almost always one of three setup mistakes, all of them far from where the reader is
standing: no `spec.decryption`, no Secret where it points, or an identity that is not one of the file's
recipients.

So `internal/delivery/flux` appends kelson's own cause and fix after the controller's words verbatim,
when the message carries a decryption marker or the reason is `DecryptionFailed`. This follows ADR-0020
§4's shape — the controller's own reason relayed, kelson's explanation added, in the surface that
already exists — rather than inventing a second vocabulary. The markers are specific enough not to fire
on an ordinary build failure, which a test pins; `age` is deliberately not one of them, being a
substring of *image*, *message* and *storage*.

### 8. Redaction, on the paths only this backend has

`internal/secret`'s sops store registers every value with `internal/redact` as its first statement,
exactly as the cluster store does ([#117](https://github.com/dafrie/kelson/issues/117)). What #81 adds
are paths where a value is in hand while an error is being built out of somebody else's words — age's
parser refusing a recipient, go-git refusing a push, a hand-committed plaintext file failing to parse —
and each is now covered by a test that checks the whole structured refusal (message, remediation, cause,
resource) for both spellings of the value, raw and base64.

Both spellings, because a Secret's `data` is base64: a test that only looked for the raw string would
pass for an implementation that forgot to encrypt and merely encoded. The same double check runs over
the committed bytes themselves.

## Consequences

**Positive.**

- **Secrets are in the reproducible artifact.** ADR-0009's documented gap — "rebuilding a cluster from
  Git alone will not restore them" — is closed for anyone who wants it closed, at the cost of one age
  identity kept outside Git.
- **Plaintext has no code path to disk.** Not as a policy but as an absence: encryption happens in
  memory and the git writer's filesystem is in memory too. There is no temporary file to clean up and no
  subprocess to pipe a value to.
- **ADR-0018's promise survived its third and last migration.** The spec text, the union, the merge
  rules and the workload render are untouched across all three backends, and a test asserts the sops and
  cluster renders are byte-identical rather than claiming it.
- **kelson holds no private key, structurally.** `internal/sops` has no `Decrypt` and no parameter one
  could arrive in. Every capability that would normally need a key — listing, drift detection, refusing
  a partial write — is built out of what SOPS leaves in the clear.
- **Two new modules, both BSD-3-Clause, +184 KB.** The dependency question was measured rather than
  argued, and the measurement is in §4 where it can be re-run.

**Negative.**

- **kelson now owns a cryptographic file format it did not design.** A change to the SOPS format, or a
  bug in this implementation, is kelson's to follow and kelson's to have. The cross-implementation test
  is the mitigation and it is skipped in CI, so it only helps someone who remembers to run it — which is
  why the test's own doc comment says to say in the pull request that you did.
- **`set` cannot merge.** Rotating one key of a three-key Secret means passing all three. The refusal is
  loud and names them, but it is more typing than the cluster backend needs and it is the first thing
  someone will ask for a flag for.
- **kelson cannot complete a key rotation.** `rotate` reports; a human runs `sops updatekeys` or
  re-supplies the values. This is a deliberate consequence of §6 and it will feel like an omission to
  anyone who has not read why.
- **The environment's own Kustomization is still the operator's to write.** kelson prints the block it
  needs and cannot apply it, so "committed but never decrypted" remains reachable by skipping a
  documented step. §7 makes the resulting failure legible; it does not prevent it. The honest fix is for
  kelson to be able to *check* the covering Kustomization's decryption block at deploy time, which needs
  the backend to reach the delivery adapter and is not done here.
- **Deleting an encrypted Secret does not delete the credential.** It is in the repository's history,
  readable by anyone with the identity and a clone. `kelson secret delete` says so; it cannot do
  anything about it.
- **Losing the age identity loses the secrets.** No escrow, no recovery. §6 states it and
  `docs/secrets.md` states it again with the two mitigations.
- **The API and the MCP surface do not reach this backend.** `SecretService` and `set_secret` address a
  (project, environment) pair and have no spec, so they write cluster Secrets — which under a sops
  environment is a Secret that Flux will fight over. Closing this needs the backend on the wire, which
  is a schema change and its own decision.
- **`kelson secret` grew a flag whose absence changes behaviour.** Without `-f`, kelson assumes
  `cluster`, because that is what an environment that never set a backend has. A sops user who forgets
  `-f` writes into the cluster instead of the repository and gets a Secret that Flux will overwrite. The
  alternative — making `-f` mandatory — would have broken every existing invocation.

## Revisit when

- **Somebody asks for a secret rotation kelson performs.** The shape is an `--age-key-file` on `rotate`,
  and the question to answer first is whether a kelson that can decrypt is still a kelson worth the
  guarantee in §6. This ADR says no; a real operational pain might change the balance.
- **The delivery adapter can see the secret backend.** Then `kelson deploy` can refuse when the covering
  Kustomization has no `spec.decryption`, and the fifth negative above becomes a render-time error
  rather than a documented step. That needs a field on `delivery.ManifestSet`, which is a contract every
  adapter shares.
- **The API grows a backend-aware `SetSecret`.** The wire schema would have to carry either the spec or
  the resolved backend, and an agent writing a credential into the wrong store is exactly the quiet
  failure ADR-0009 exists to prevent.
- **`valuesFrom` or a previews `secretRef` needs to be encrypted.** Both name a Secret without naming
  keys. ADR-0020 hit the same wall from the other side; here it is milder — kelson could encrypt a whole
  file if it were given one — but it needs a spelling and a decision about what a Secret with unknown
  keys means.
- **getsops publishes a stable, provider-free library package.** If the root package ever separates from
  its key providers, §4's measurement changes and importing becomes the better answer. The measurement
  is written down so that can be checked rather than assumed.
