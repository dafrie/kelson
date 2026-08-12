# ADR-0009: Secrets are references; values never enter Git

- **Status:** Accepted
- **Date:** 2026-08-12

## Context

Git mode makes this unavoidable: whatever the spec can hold will eventually be committed. But the
constraint has to be affordable, or people route around it.

### What the category does

**Coolify** encrypts sensitive variables at rest in its Postgres and makes them write-only in the UI once
saved. Two documented holes: build-time variables are stored in plaintext, and deployment logs have
printed environment variables in the clear.

**Canine** stores `environment_variables.value` as `text` with a `storage_type` enum of `config` or
`secret`. Code search for `encrypts`, `attr_encrypted` and `lockbox` returns nothing. The enum decides
whether the value renders as a Secret or a ConfigMap in the cluster; in Canine's own database it is
plaintext either way.

**Kubero** could not be verified — its code search returned nothing. Its README states all state lives in
etcd via CRDs with no separate database, which would put environment variables in the `KuberoApp` CRD.
If so that is worse than a plain Secret, since CRDs are not encrypted at rest by default and appear in
`kubectl get -o yaml`. Recorded as inference.

The bar is low. Two of the three store secrets in plaintext somewhere, and the third has holes.

## Decision

### The spec carries references, never values

A spec containing a secret literal **fails validation**, in every delivery mode. Not a warning.

Enforcing this in the shared model validation (which every render passes through) means one rule covers
the CLI, the UI, the API and agents at once. There is no second path to secure. A warning is suppressed
under deadline pressure, and the failure mode it permits is a credential in Git history — which is not
fixed by editing the file.

*Honesty note (2026-08-12 review):* the current implementation is a **heuristic** — an env-name pattern
(`PASSWORD|SECRET|TOKEN|API_KEY|…`) plus URL-credential detection in `internal/model/validate.go`. It
catches the common shapes; a literal under a creatively named key renders fine. The hard guarantee this
section promises becomes structural (shape-based, not vocabulary-based) in
[#82](https://github.com/dafrie/kelson/issues/82); until then, read "fails validation" as "fails for
recognizable secret shapes".

### v0.1: values live in Kubernetes Secrets, written out-of-band

kelson writes the `Secret` through the Kubernetes API. Rendered manifests contain only `secretKeyRef`.
Git never sees a value in any delivery mode.

**kelson does not persist secret values.** The cluster is the store; kelson reads back masked for display.
This keeps kelson out of the credential-storage business, which is what the threat model most wants to
avoid, and it avoids reintroducing the platform state that [ADR-0001](0001-hybrid-state-model.md) works to
eliminate.

Setting a secret is one command:

```bash
kelson secret set DATABASE_URL=postgres://...
```

That matters more than it looks. The strongest argument against forbidding literals was that it forces
someone with a throwaway application to set up SOPS or Vault before passing an API key. With this backend
there is no setup, so the rule stops being friction and becomes simply how a secret is set.

### The reference model is designed for three backends from day one

Even though only the first ships in v0.1, the schema accommodates all three so adding them is not a
breaking change:

| Backend | Where the value lives | Ships |
|---|---|---|
| `cluster` | Kubernetes Secret, written by kelson | v0.1 |
| `externalSecrets` | Vault, AWS/GCP/Azure secret manager | v0.2 |
| `sops` | Encrypted in Git, age keys | v0.2 |

Backend is an Environment-level choice, consistent with how delivery mode and data-service presets work.

### Two holes we do not repeat

**Build-time secrets** go through BuildKit secret mounts, never build arguments and never image layers.
This is the gap Coolify has.

**Logs are redacted.** No secret value is ever written to a build log, a deploy log, an event, an error
message or a diff. This is Coolify's other documented gap, and it needs to be a tested property rather
than a convention.

## Consequences

**Positive.**
- No prerequisites. A user on a bare VPS sets a secret with one command.
- kelson never holds a credential, so compromising kelson does not yield the secrets.
- Git is clean by construction rather than by discipline.
- Better than anything else in the category, without requiring Vault.

**Negative.**
- **Secrets are not in the reproducible artifact.** Rebuilding a cluster from Git alone will not restore
  them. This is the real cost of the v0.1 backend and it must be documented plainly, not discovered. The
  `sops` and `externalSecrets` backends in v0.2 close it for anyone who needs it.
- Kubernetes Secrets are base64, not encrypted, unless etcd encryption at rest is enabled. kelson should
  detect this and say so rather than implying a guarantee it does not provide. The bootstrap path should
  enable it.
- Reading back masked values means kelson needs read access to Secrets, which is a meaningful RBAC grant
  and belongs in the threat model.
- Users migrating from a `.env` file hit a hard failure on first render. The error must name the offending
  field and give the exact command to fix it, or this reads as obstruction.

## Revisit when

The v0.2 backends land and real usage shows whether the DR gap in the `cluster` backend drove people to
them, or whether it never mattered in practice.
