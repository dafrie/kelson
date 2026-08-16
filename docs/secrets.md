# Secrets

A kelson spec carries **references, never values** ([ADR-0009](adr/0009-secrets.md)). An environment
variable that needs a credential is written as a mapping:

```yaml
env:
  DATABASE_URL: { secret: checkout-db, key: url }
```

and that renders as a `valueFrom.secretKeyRef` under every backend
([ADR-0018](adr/0018-secret-references.md)). What changes between backends is the mechanism that puts a
value where the reference points — one field on one Environment, and no spec text moves.

| Backend | Where the value lives | Who writes it | In the delivered artifact |
|---|---|---|---|
| `cluster` (default) | a Kubernetes Secret | `kelson secret set` / `unset` | no |
| `sops` | the artifact kelson publishes, encrypted with age | nothing, today — see below | **yes**, encrypted |
| `externalSecrets` | Vault, AWS/GCP/Azure secret manager | you, in that store | no |

`cluster` needs no setup at all and is the right answer for most people. Its one real cost is written
into ADR-0009's own consequences: **rebuilding a cluster from the delivered artifact alone will not
restore the secrets.** `sops` is what closes that, and this page is mostly about it.

---

## The `cluster` backend, in four verbs

```sh
kelson secret set checkout-db --project checkout --env production url=postgres://…
kelson secret list --project checkout --env production
kelson secret unset checkout-db --project checkout --env production old-url
kelson secret delete checkout-db --project checkout --env production
```

`set` **merges**: keys it does not name are preserved, so rotating one credential never silently drops
another. That is why removing a key needs its own verb.

`unset` removes the keys you name and nothing else. Two rules follow from kelson reporting no values
anywhere:

- **A key that is not there is refused**, with the keys the Secret does hold listed, and nothing is
  removed. A typo that quietly removed nothing would leave you believing a credential is gone.
- **Removing the last key leaves an empty Secret** rather than deleting it. Removing keys never removes
  an object: `kelson secret delete` is the verb that does, and it is the one that asks first. An empty
  Secret still lists, and `set` refills it.

Both refuse a Secret kelson did not write (`secret/not-managed`), exactly as `delete` does. Over the API
and on the agent surface the same removal is `UnsetSecret` and `set_secret`'s `remove_keys`; there is no
tool that deletes a Secret.

---

## The `sops` backend

> **Not writable today ([#224](https://github.com/dafrie/kelson/issues/224)).** Everything in this
> section describes the mechanism, which is decided and unchanged. What is missing is the destination:
> the writer that used to commit the encrypted Secret went with the git delivery machinery
> ([ADR-0028](adr/0028-delivery-spine.md)), and nothing puts one inside the published artifact yet. So
> **every `kelson secret` verb refuses an environment on `backend: sops`** — by name, with
> `delivery/not-implemented` and the issue number — rather than writing the value somewhere the delivery
> spine would overwrite it. kelson-server and the `set_secret` tool refuse it for the same reason
> ([#269](https://github.com/dafrie/kelson/issues/269)). Encryption itself is unaffected: the age
> recipients, the file format and in-cluster decryption are all still here and still tested.

Values are encrypted with [age](https://age-encryption.org) and travel **inside the artifact kelson
publishes**, beside the workloads that reference them. kustomize-controller decrypts them on the way
into the cluster. A cluster rebuilt from the artifact comes back with its secrets, and the only thing
that has to survive outside is one age identity.

It needs no mode and no gate. kustomize-controller decrypts a `Kustomization`'s sources
per-Kustomization and does not care whether the source is a `GitRepository` or an `OCIRepository`, which
is why [ADR-0022](adr/0022-sops-age.md)'s mechanism survived the transport change of
[ADR-0028](adr/0028-delivery-spine.md) §7 intact — and why the old `render/sops-requires-flux` refusal
is gone with the modes.

### Setup, once per cluster

**1. Generate an age key.** The public half is a *recipient* and goes in your spec. The private half is
an *identity* and must never reach Git.

```sh
age-keygen -o age.key
# Public key: age13w78znajf5kee8msacel80jz6qeuc9tyxhuqkwnqcsaymlrj7clsy4fgdw
```

**2. Give the identity to the cluster.** kelson never holds it — `kelson secret set` needs only the
public recipient — so this step is yours, and it is the only one:

```sh
kubectl -n kelson-system create secret generic sops-age --from-file=age.agekey=age.key
```

The Secret goes in the namespace the `Kustomization` lives in, which is `kelson-system`
([ADR-0028](adr/0028-delivery-spine.md) decision 3).

**3. Put the recipient in the Environment.**

```yaml
apiVersion: kelson.dev/v1alpha1
kind: Environment
metadata: {name: production}
spec:
  project: checkout
  namespace: checkout-production
  secrets:
    backend: sops
    ageRecipients:
      - age13w78znajf5kee8msacel80jz6qeuc9tyxhuqkwnqcsaymlrj7clsy4fgdw
    # ageKeySecret: sops-age   # optional; this is the default
```

**There is no third step.** kelson writes the `spec.decryption` block on the `Kustomization` it owns,
from `ageKeySecret`:

```yaml
  decryption:
    provider: sops
    secretRef:
      name: sops-age
```

This **reverses [ADR-0022](adr/0022-sops-age.md) §5**, which said *"kelson does not write that
Kustomization, because the reconciler's own objects belong to the operator"*. That was right when a
user's own Kustomization reconciled a path in a user's own repository. Now kelson publishes the artifact
and owns the Kustomization that consumes it, so the decryption block has no other plausible owner —
and ADR-0022's worst failure mode goes with it: **"encrypted but never decrypted" is no longer reachable
by skipping a step.** Without that block an encrypted file is applied verbatim, as a Secret whose value
is the literal string `ENC[AES256_GCM,…]` and a workload that starts with a credential that is not one.
Nobody has to remember it now.

`ageRecipients` holds **public** keys. They are not secret and they belong in the repository in the
clear — that is the shape of the mechanism, not a compromise: encrypting needs the recipient, decrypting
needs the identity, and kelson only ever does the first.

### Writing a secret

```sh
kelson secret set checkout-db -f spec.yaml --env production url=postgres://user:pw@db/checkout
```

`-f` is what tells kelson which backend to use — it reads `secrets.backend` from the spec. Without it,
kelson assumes `cluster` and writes to the API server instead.

**Today this refuses**, for the reason in the callout above: there is nowhere to put the ciphertext that
the delivery spine will read. The refusal names the backend and
[#224](https://github.com/dafrie/kelson/issues/224), and nothing is written anywhere — in particular not
as a plain Secret in the namespace, which would be a credential in a place a `sops` environment must not
have one. Until it returns, write the value with `kubectl create secret generic` (and keep it out of the
artifact), or move the environment to `backend: cluster` and use `kelson secret set`.

The rest of this section is what the mechanism does when it writes, because none of it changed.

The value is encrypted **in memory** and only the ciphertext is stored. No plaintext file is ever
created: kelson encrypts inside its own process, so neither the plaintext nor the encrypted form touches
your disk. `--from-stdin` and `--from-file` keep the value out of your shell history:

```sh
read -rs PW && printf '%s' "$PW" |
  kelson secret set checkout-db -f spec.yaml --env production --from-stdin password
```

What kelson writes is an encrypted Secret manifest that ships with the workloads that reference it:

```yaml
apiVersion: v1
kind: Secret
metadata:
  name: checkout-db
  namespace: checkout-production
  labels:
    app.kubernetes.io/managed-by: kelson
    kelson.dev/managed-secret: "true"
type: Opaque
data:
  url: ENC[AES256_GCM,data:kZkq0aJG…,iv:1YYTuTio…,tag:kuFytXCj…,type:str]
sops:
  age:
    - enc: |
        -----BEGIN AGE ENCRYPTED FILE-----
        …
        -----END AGE ENCRYPTED FILE-----
      recipient: age13w78znajf5kee8msacel80jz6qeuc9tyxhuqkwnqcsaymlrj7clsy4fgdw
  encrypted_regex: ^(data|stringData)$
  lastmodified: "2026-08-14T09:30:00Z"
  mac: ENC[AES256_GCM,…]
  version: 3.13.3
```

Only the **values** are encrypted. The Secret's name, namespace and key names stay readable, which is
what makes an encrypted secret reviewable: a diff shows which Secret gained which key, and shows the
value as ciphertext.

> **Where that file lives ([#225](https://github.com/dafrie/kelson/issues/225)).** Nowhere, today.
> kelson used to write it to a path in a delivery git repository, and both the writer and the
> `delivery.git` field it was addressed by are gone with [ADR-0028](adr/0028-delivery-spine.md), which
> decides that the ciphertext travels **inside the published artifact** instead. Where `kelson secret set`
> holds it on the way there, so the publisher picks it up, is the open question — and it is why the verb
> refuses rather than writing the file somewhere provisional.

### `set` writes the whole Secret

This is the one place `sops` behaves differently from `cluster`, and it follows from the guarantee.
Under `cluster`, `set` merges — kelson reads the live Secret and carries the untouched keys forward.
Here the untouched keys are ciphertext under a data key kelson **cannot open**, because opening it needs
the age identity kelson never holds.

So a `set` that would drop keys is refused, with the keys named:

```
Secret/checkout-production/checkout-db [secret/sops-partial-set]: this write names "url" and the
encrypted Secret also holds "token", which it would drop
```

Pass every key the Secret should have. `kelson secret unset` is a `cluster` verb for the same reason:
removing one key from a file kelson cannot open would need the identity it never holds.

### Listing

```sh
kelson secret list -f spec.yaml --env production
```

reads the encrypted files and reports names and key names. It needs no key at all — SOPS encrypts
values, not structure. There is no age column: a file's age is a fact about where it is stored rather
than about the credential. Like the write, it is refused while there are no files to read
([#224](https://github.com/dafrie/kelson/issues/224)).

There is no `kelson secret get` under any backend. Reading a value is `kubectl get secret` (or
`sops -d`), with the cluster's or the store's own access control behind it.

### Deleting

```sh
kelson secret delete checkout-db -f spec.yaml --env production
```

removes the encrypted file; kustomize-controller prunes the Secret on the next reconcile, because the
`Kustomization` owns what it applied and runs with `prune: true`. It is refused today for the same
reason the write is ([#224](https://github.com/dafrie/kelson/issues/224)).

**Every previous version is still readable by anyone holding the age identity** — in the repository's
history, and in the immutable artifacts kelson has already published, which are never deleted. A deleted
secret is a secret that needs rotating, not a secret that is gone.

---

## Key management

### More than one recipient

A file is wrapped once per recipient, and **any matching identity opens it**. That is what makes both a
handover and a break-glass copy work:

```yaml
  secrets:
    backend: sops
    ageRecipients:
      - age13w78znajf5kee8msacel80jz6qeuc9tyxhuqkwnqcsaymlrj7clsy4fgdw   # the cluster's key
      - age1cnnsmzvurnpxzzggcwm4rvk44hgskzhrz92nryk6879kf8zll3jqqjhvt3   # the break-glass key, offline
```

Adding a second recipient costs one line in each encrypted file and buys the recovery story below.

### Rotating the age key

Rotation is add-then-remove, and `kelson secret rotate` is what tells you where you are in it — once
there are encrypted files again to compare against ([#224](https://github.com/dafrie/kelson/issues/224);
it refuses alongside the other verbs until then).

**1. Add the new recipient** to `ageRecipients` alongside the old one, and apply the spec.

**2. Find what is stale.**

```sh
kelson secret rotate -f spec.yaml --env production
```

```
environment production encrypts to 2 age recipient(s):
  age13w78znajf5kee8msacel80jz6qeuc9tyxhuqkwnqcsaymlrj7clsy4fgdw
  age1cnnsmzvurnpxzzggcwm4rvk44hgskzhrz92nryk6879kf8zll3jqqjhvt3

1 encrypted Secret(s) are wrapped for a different set:

  checkout-db (clusters/prod/checkout/secrets/checkout-db.enc.yaml)
    missing:  age1cnnsmzvurnpxzzggcwm4rvk44hgskzhrz92nryk6879kf8zll3jqqjhvt3 — whoever holds this identity cannot read this Secret
    fix:      kelson secret set checkout-db -f <spec> --env production token=<value> url=<value>
    or:       sops updatekeys clusters/prod/checkout/secrets/checkout-db.enc.yaml   (needs an identity that can already read it)

nothing was changed.
```

It exits 0 when nothing is stale and 2 when something is, so it can gate CI.

**3. Re-encrypt each stale file**, either way:

- `kelson secret set …` — re-encrypts from the values. Needs no key at all; needs the values.
- `sops updatekeys <path>` — re-wraps the existing data key for the new recipients. Needs an identity
  that can already read the file; needs no values. Run it from a checkout with
  `SOPS_AGE_KEY_FILE=age.key` set, and commit the result.

**4. Give the cluster the new identity**, then **remove the old recipient** and re-encrypt again.

`kelson secret rotate` reports and never re-encrypts by itself. Doing the re-encryption would mean
kelson taking an age identity as input, and not having one is the property everything else here rests on
([ADR-0022](adr/0022-sops-age.md) §6).

**Removing a recipient does not make the old key useless.** Every previous version of every file is
still in Git, still wrapped for it. If a key was *compromised* rather than merely retired, rotate the
credentials too — the age rotation only changes who can read the files from now on.

---

## Recovery

**If you lose the age identity, the encrypted files are unreadable. By anyone, kelson included.** There
is no escrow, no recovery code and no back door — that is the same sentence as "kelson holds no key"
read from the other side.

Two mitigations, both to be set up before you need them:

- **A second recipient whose identity lives offline.** Every file is wrapped for it, so it opens all of
  them. A printed key in a safe is a legitimate answer here.
- **The identity backed up somewhere else entirely** — a password manager, a hardware token, the
  secret store you already run.

**If the identity is gone and there is no second recipient**, the encrypted values are lost and the only
path forward is to reissue the credentials at their sources and write them again:

```sh
age-keygen -o age.key
kubectl -n kelson-system delete secret sops-age
kubectl -n kelson-system create secret generic sops-age --from-file=age.agekey=age.key
# put the new recipient in the Environment spec, replacing the old one, then for each Secret:
kelson secret set checkout-db -f spec.yaml --env production url=<new value> token=<new value>
```

The old `.enc.yaml` files are inert once every Secret has been rewritten; `kelson secret rotate` will
list any you missed.

**If the cluster is gone but your spec repository and the identity survive**, there is nothing to
recover: install Flux, create the `sops-age` Secret, apply the Project and Environment, and the secrets
come back with everything else — kelson rebuilds the artifact and the Kustomization that decrypts it.

---

## When it goes wrong

**`kelson deploy` reports a rejected Kustomization mentioning a data key.** The Kustomization cannot
decrypt. kelson names the cause after the controller's own message:

```
flux: Kustomization kelson-system/checkout-production rejected the change (BuildFailed): failed to
  decrypt secret …
  cause: this Kustomization could not decrypt the SOPS-encrypted manifests in the artifact.
  fix: check, in this order — (1) the Secret named by ageKeySecret exists in namespace kelson-system
  and holds the age identity under a .agekey key; (2) the identity is one of the recipients the files
  are encrypted to — `kelson secret rotate` lists them without needing a key.
```

The block itself is no longer on that list: kelson writes `spec.decryption` on the Kustomization it
owns, so a missing one is a kelson bug rather than a setup step somebody skipped.

**A pod fails with `CreateContainerConfigError`.** The Secret the reference names does not exist yet.
Either nothing wrote it (`kelson secret list`), or the Kustomization has not reconciled this revision
yet, or it could not decrypt — see above.

**`secret/sops-not-encrypted`.** There is a file where an encrypted Secret belongs that is not a SOPS
document — almost always a plaintext Secret written by hand. kelson reports it rather than overwriting
it, because the first step is treating its contents as compromised.

---

## What kelson does not do

- **kelson never holds an age private key.** There is no flag for one and no code path that could use
  one. Everything kelson does with an encrypted file — writing it, listing it, detecting recipient drift
  — is built out of what SOPS leaves in the clear.
- **kelson does not hold, and does not ask for, the cluster's age identity.** Creating the Secret that
  holds it is the one setup step that stays yours. kelson *does* write the `spec.decryption` block that
  points at it — that reversal is [ADR-0028](adr/0028-delivery-spine.md) §7, and it is what closed
  ADR-0022's "encrypted but never decrypted" gap.
- **kelson does not read values back.** No backend has a `get`, and no message in the API schema has a
  field a value could arrive in.
- **kelson does not content-sniff your logs.** The guarantee is the one kelson can keep: kelson never
  adds a secret to a log ([ADR-0009](adr/0009-secrets.md), amendment). What your own process prints is
  yours to control.
- **The API and MCP surfaces write only where the backend says they may.** A `SetSecret` request names a
  project and an environment and carries no spec, so kelson-server reads the environment's effective
  `secrets.backend` off the stored spec. Under `cluster` it writes as before; under `sops` and
  `externalSecrets` it refuses by name — `delivery/not-implemented` naming
  [#224](https://github.com/dafrie/kelson/issues/224), and `secret/external-backend` naming the store the
  value belongs in. It does not fall back to writing a plain cluster Secret, which is what it used to do
  ([#269](https://github.com/dafrie/kelson/issues/269)). A stored spec kelson cannot read or resolve is
  refused too, with `secret/backend-unreadable`: an unknown backend is not the default backend.

## See also

- [ADR-0009](adr/0009-secrets.md) — secrets are references; values never enter the spec
- [ADR-0018](adr/0018-secret-references.md) — the `{secret, key}` reference schema
- [ADR-0020](adr/0020-external-secrets.md) — the `externalSecrets` backend
- [ADR-0022](adr/0022-sops-age.md) — the `sops` backend, and why kelson holds no key
- [ADR-0028](adr/0028-delivery-spine.md) §7 — the transport is the artifact, and kelson writes the
  decryption block
- [Model](model.md) — where `secrets:` sits in the spec
