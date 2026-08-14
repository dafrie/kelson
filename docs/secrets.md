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

| Backend | Where the value lives | Who writes it | In the Git artifact |
|---|---|---|---|
| `cluster` (default) | a Kubernetes Secret | `kelson secret set` | no |
| `sops` | your delivery repository, encrypted with age | `kelson secret set` | **yes**, encrypted |
| `externalSecrets` | Vault, AWS/GCP/Azure secret manager | you, in that store | no |

`cluster` needs no setup at all and is the right answer for most people. Its one real cost is written
into ADR-0009's own consequences: **rebuilding a cluster from Git alone will not restore the secrets.**
`sops` is what closes that, and this page is mostly about it.

---

## The `sops` backend

Values are encrypted with [age](https://age-encryption.org) and committed to the delivery repository.
Flux's kustomize-controller decrypts them on the way into the cluster. A cluster rebuilt from the
repository comes back with its secrets, and the only thing that has to survive outside Git is one age
identity.

It requires `delivery.mode: flux`. Direct mode has no decryptor, so kelson refuses at render time with
`render/sops-requires-flux` rather than committing a file nothing would ever decrypt.

### Setup, once per cluster

**1. Generate an age key.** The public half is a *recipient* and goes in your spec. The private half is
an *identity* and must never reach Git.

```sh
age-keygen -o age.key
# Public key: age13w78znajf5kee8msacel80jz6qeuc9tyxhuqkwnqcsaymlrj7clsy4fgdw
```

**2. Give the identity to Flux.** kelson never holds it — `kelson secret set` needs only the public
recipient — so this step is yours:

```sh
kubectl -n flux-system create secret generic sops-age --from-file=age.agekey=age.key
```

**3. Tell the Kustomization to decrypt.** The Kustomization that reconciles your delivery path is part of
the cluster's bootstrap, not something kelson writes ([ADR-0012](adr/0012-flux-only-gitops.md)):

```yaml
apiVersion: kustomize.toolkit.fluxcd.io/v1
kind: Kustomization
metadata:
  name: checkout
  namespace: flux-system
spec:
  interval: 5m
  path: ./clusters/prod/checkout
  prune: true
  sourceRef: { kind: GitRepository, name: deploy }
  decryption:
    provider: sops
    secretRef:
      name: sops-age
```

Without that block the encrypted file is applied verbatim: a Secret whose value is the literal string
`ENC[AES256_GCM,…]`, and a workload that starts with a credential that is not one. `kelson secret set`
prints this block every time it writes an encrypted file for exactly that reason.

**4. Put the recipient in the Environment.**

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
    # ageKeySecret: sops-age   # optional; this is the default
```

`ageRecipients` holds **public** keys. They are not secret and they belong in the repository in the
clear — that is the shape of the mechanism, not a compromise: encrypting needs the recipient, decrypting
needs the identity, and kelson only ever does the first.

### Writing a secret

```sh
kelson secret set checkout-db -f spec.yaml --env production url=postgres://user:pw@db/checkout
```

`-f` is what tells kelson which backend to use — it reads `secrets.backend` from the spec. Without it,
kelson assumes `cluster` and writes to the API server instead.

The value is encrypted **in memory** and the ciphertext is committed. No plaintext file is ever created:
kelson writes through an in-memory filesystem, so neither the plaintext nor the encrypted form touches
your disk. Use `--from-stdin` or `--from-file` to keep the value out of your shell history:

```sh
read -rs PW && printf '%s' "$PW" |
  kelson secret set checkout-db -f spec.yaml --env production --from-stdin password
```

What lands in the repository is `clusters/prod/checkout/secrets/checkout-db.enc.yaml`:

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
what makes an encrypted secret reviewable: a pull request shows which Secret gained which key, and shows
the value as ciphertext.

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

Pass every key the Secret should have. To drop a key deliberately, `kelson secret delete` first.

### Listing

```sh
kelson secret list -f spec.yaml --env production
```

reads the committed files and reports names and key names. It needs no key at all — SOPS encrypts
values, not structure. There is no age column: a file's age is a fact about the repository rather than
about the credential.

There is no `kelson secret get` under any backend. Reading a value is `kubectl get secret` (or
`sops -d`), with the cluster's or the repository's own access control behind it.

### Deleting

```sh
kelson secret delete checkout-db -f spec.yaml --env production
```

removes the encrypted file; Flux prunes the Secret on its next reconcile because the Kustomization owns
what it applied.

**The value is still in the repository's history.** Anyone with the age identity and a clone can read it.
A deleted secret is a secret that needs rotating, not a secret that is gone.

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

Rotation is add-then-remove, and `kelson secret rotate` is what tells you where you are in it.

**1. Add the new recipient** to `ageRecipients` alongside the old one, and commit the spec.

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
- **The identity backed up outside the repository** — a password manager, a hardware token, the cluster
  secret store you already run.

**If the identity is gone and there is no second recipient**, the encrypted values are lost and the only
path forward is to reissue the credentials at their sources and write them again:

```sh
age-keygen -o age.key
kubectl -n flux-system delete secret sops-age
kubectl -n flux-system create secret generic sops-age --from-file=age.agekey=age.key
# put the new recipient in the Environment spec, replacing the old one, then for each Secret:
kelson secret set checkout-db -f spec.yaml --env production url=<new value> token=<new value>
```

The old `.enc.yaml` files are inert once every Secret has been rewritten; `kelson secret rotate` will
list any you missed.

**If the cluster is gone but the repository and the identity survive**, there is nothing to recover:
install Flux, create the `sops-age` Secret, point a Kustomization at the path, and the secrets come back
with everything else.

---

## When it goes wrong

**`kelson deploy` reports a rejected Kustomization mentioning a data key.** The Kustomization cannot
decrypt. kelson names the cause after the controller's own message:

```
flux: Kustomization flux-system/checkout rejected the change (BuildFailed): failed to decrypt secret …
  cause: this Kustomization could not decrypt the SOPS-encrypted manifests at clusters/prod/checkout.
  fix: check, in this order — (1) the Kustomization has spec.decryption: {provider: sops, secretRef:
  {name: …}}; (2) that Secret exists in namespace flux-system and holds the age identity under a
  .agekey key; (3) the identity is one of the recipients the files are encrypted to — `kelson secret
  rotate` lists them without needing a key.
```

**A pod fails with `CreateContainerConfigError`.** The Secret the reference names does not exist yet.
Either nothing wrote it (`kelson secret list`), or the Kustomization has not reconciled the commit yet,
or it could not decrypt — see above.

**`secret/sops-not-encrypted`.** There is a file where an encrypted Secret belongs that is not a SOPS
document — almost always a plaintext Secret committed by hand. kelson reports it rather than overwriting
it, because the first step is treating its contents as compromised: it is in the repository's history.

**`secret/sops-write-failed` with a conflict.** The delivery branch moved while the command was running.
kelson never force-pushes; re-run it and it re-reads the branch.

---

## What kelson does not do

- **kelson never holds an age private key.** There is no flag for one and no code path that could use
  one. Everything kelson does with an encrypted file — writing it, listing it, detecting recipient drift
  — is built out of what SOPS leaves in the clear.
- **kelson does not write the Kustomization that decrypts.** The reconciler's own objects are the
  cluster's bootstrap, not kelson's ([ADR-0012](adr/0012-flux-only-gitops.md)). `kelson secret set`
  prints the block you need.
- **kelson does not read values back.** No backend has a `get`, and no message in the API schema has a
  field a value could arrive in.
- **kelson does not content-sniff your logs.** The guarantee is the one kelson can keep: kelson never
  adds a secret to a log ([ADR-0009](adr/0009-secrets.md), amendment). What your own process prints is
  yours to control.
- **The API and MCP surfaces write cluster Secrets only.** `set_secret` addresses a project and an
  environment and has no spec, so it cannot see `secrets.backend`. Under a sops environment, use the CLI.

## See also

- [ADR-0009](adr/0009-secrets.md) — secrets are references; values never enter Git
- [ADR-0018](adr/0018-secret-references.md) — the `{secret, key}` reference schema
- [ADR-0020](adr/0020-external-secrets.md) — the `externalSecrets` backend
- [ADR-0022](adr/0022-sops-age.md) — the `sops` backend, and why kelson holds no key
- [Model](model.md) — where `secrets:` sits in the spec
