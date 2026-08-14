# ADR-0018: The secret reference schema — `{secret, key}` in an env value, and what the renderer guarantees

- **Status:** Accepted
- **Date:** 2026-08-14

## Context

[ADR-0009](0009-secrets.md) decided the doctrine: the spec carries references, never values; the
backend is an Environment-level choice among `cluster`, `externalSecrets` and `sops`; build secrets
are mounts; logs are redacted. It deliberately decided no *syntax*. Its own closing paragraph says so
— "this amendment does not decide the reference model" — and leaves
[#79](https://github.com/dafrie/kelson/issues/79) holding three questions:

**What does an author write?** Today the answer is nothing. There is exactly one way to get a
credential into a workload — `{from: {service, key}}`, a binding to a data component this Project
declares ([#89](https://github.com/dafrie/kelson/issues/89)) — and it only works for a database kelson
manages. Every other credential (a Stripe key, an SMTP password, a webhook signing secret) has no
spelling at all, and the validation error that fires on the literal has been sending authors to an
overlay patch as the escape hatch. That is the migration failure ADR-0009's own consequences list
predicted: "users migrating from a `.env` file hit a hard failure on first render… or this reads as
obstruction".

**How does the guarantee become structural?** [#82](https://github.com/dafrie/kelson/issues/82)'s
review note (2026-08-12) is blunt about the gap: ADR-0009 promises a spec literal "fails to render",
and the implementation is a name regex — `SESSION_PEPPER=hunter2` renders fine. A guarantee that
depends on guessing which variable names sound like credentials is a heuristic wearing a guarantee's
clothes, and ADR-0009 already carries an honesty note saying so.

**Where does the backend selector attach?** `Environment.spec.secrets.backend` has existed as a typed,
validated field since the model was written, and has been refused with `schema/not-implemented` the
whole time because nothing read it ([#141](https://github.com/dafrie/kelson/issues/141)).

Two things already decided frame the answers. **The reference-not-value shape has a landed
precedent**: `kind: helm` components carry `valuesFrom: [{secretRef: <name>}]`
([ADR-0016](0016-delivery-flows-v0.md), [#182](https://github.com/dafrie/kelson/issues/182)) — a Secret
*name* in the spec, resolved by helm-controller, never read by kelson. And **redaction is already
enforced** with `internal/redact` ([#117](https://github.com/dafrie/kelson/issues/117)), which covers
the display surface for the one thing that can still put a Secret into a rendered set: an overlay.

## Decision

### 1. A reference is a typed object in the env value's own place

```yaml
env:
  LOG_LEVEL: info                                    # a plain string is a plain value
  DATABASE_URL: { secret: checkout-db, key: url }    # a mapping is a reference
  STRIPE_API_KEY: { secret: payments, key: api-key }
```

**A scalar is a value; a mapping is a reference.** That is the whole rule, and it is the rule the model
already had — `{from: {service, key}}` has been the second arm of this union since the beginning. The
union grows a third arm, told apart by its own discriminating key, and nothing else about env changes.

There is **no templating language**. No `${secret.foo}`, no `$SECRET_FOO`, no interpolation into a
larger string. A variable's value comes from exactly one place, and the spec says which place in a form
a schema can describe and an agent can generate without parsing prose. Partial interpolation
(`postgres://user:${password}@host/db`) is refused by construction rather than by rule: there is no
syntax for it. The workload assembles such a string from parts, or the Secret holds the whole URL.

**The alternative considered and rejected: a `secretRef:` sibling map.**

```yaml
env:
  LOG_LEVEL: info
secretRef:                    # rejected
  DATABASE_URL: { name: checkout-db, key: url }
```

It has one real advantage — every entry under `secretRef:` is unambiguously a reference, so a reader
scanning for credentials has one place to look — and three costs that outweigh it.

*Locality.* The variable and its source stop reading together. `DATABASE_URL` appears in one map and
its source in another, and answering "where does this variable come from?" becomes a cross-reference
against a second block that may live at a different scope. The inline form answers it on the line.

*Two merge rules where one will do.* P1 merges `env` key by key, innermost scope wins. A second map
needs the same rule written again, plus an answer to the case P1 never has to consider: what happens
when `env.DATABASE_URL` and `secretRef.DATABASE_URL` are both set, at the same scope or at different
ones. The inline form has no such case — one key, one value, one merge.

*It is not how the model already works.* Bindings are inline. Making references a sibling map would
mean two mechanisms that render into the identical `valueFrom.secretKeyRef` are written in two
unrelated shapes, and an author would have to know which kind of credential they had before knowing
where to type it.

The flat spelling (`{secret: <name>, key: <key>}`) rather than nesting under a wrapper the way `from:`
does is deliberate too. `from:` is a preposition and needs an object to point at — `from: {service, key}`
reads as a sentence. `secret:` is a noun that carries its own value, and `{secret: {name, key}}` would
say "secret" twice to no one's benefit.

### 2. `<name>` is a Secret name, and the reference is backend-agnostic

Under the v0 `cluster` backend, `secret:` names **an existing Kubernetes Secret in the target
namespace**. kelson references it and does nothing else to it — it does not create it, does not read
it, does not diff it, does not own it. What appears in the rendered manifest is
`valueFrom.secretKeyRef`, and the kubelet performs the projection at pod start.

Writing the Secret is out of band, and its successors are named:

| Successor | What it adds |
|---|---|
| [#116](https://github.com/dafrie/kelson/issues/116) | `kelson secret set\|get\|list\|unset` — the command that writes the Secret, with masked read-back. Until it lands, the remediation names `kubectl create secret generic`, because a next step that exists beats a command that does not |
| [#80](https://github.com/dafrie/kelson/issues/80) | the `externalSecrets` backend: an `ExternalSecret` per reference, resolved from Vault or a cloud secret manager |
| [#81](https://github.com/dafrie/kelson/issues/81) | the `sops` backend: values encrypted in Git with age keys, decrypted in-cluster |

**The spec text does not change when the backend does.** `{secret: checkout-db, key: url}` is what an
author writes under all three; what differs is the mechanism that puts a value where the reference
points, and that is `Environment.spec.secrets.backend`'s job (rule P4: an Environment value wins whole
over a Project default, ADR-0009's "backend is an Environment-level choice"). This is what ADR-0009
meant by "designed for three backends from day one": migrating between them is editing one field on one
Environment, not rewriting every spec.

In v0 the renderer emits the `cluster` shape and refuses the other two with
`render/secret-backend-unsupported`, naming #80 and #81 (decision 5).

### 3. Scope: wherever an env value already goes, and nowhere else

A reference is legal in `Project.spec.env`, in a component's `env`, and in
`Environment.spec.components[].env`. **The P1/P2 merge rules apply unchanged**: a reference merges like
any other env value, key by key, innermost scope wins, and the winner is taken whole. An Environment
may replace a plain value with a reference or a reference with a plain value; nothing in the merge asks
what form either side has, so no rule was added for it.

What this deliberately is **not**:

- **Not a new document kind.** A secret is not a kelson object. It is a Secret in a namespace, and
  kelson's spec holds a pointer to it. Introducing a `kind: Secret` document would be the beginning of
  kelson holding credentials, which ADR-0009 exists to prevent.
- **Not cross-project.** `secret:` names a Secret in *this* environment's namespace. There is no
  syntax for `<namespace>/<name>` and none is coming through this ADR: a pod cannot mount a Secret
  from another namespace anyway, so a spelling for it would be a promise the cluster refuses at pod
  start. The same reasoning already applies to a binding against a `shared`-preset data component
  ([#93](https://github.com/dafrie/kelson/issues/93)).
- **Not a way to name a file.** Projecting a Secret as a mounted volume is a different authoring
  surface with a different set of questions (path, mode, whole-Secret versus one key). It is not
  attempted here; an overlay does it today.

### 4. The renderer guarantee, and its boundary (#82)

**Rendered output never contains a secret value, by construction.** The property, stated exactly:

1. **Everything typed as a reference stays a reference.** Both mapping arms — `{secret, key}` and
   `{from: {service, key}}` — render into the same `valueFrom.secretKeyRef` node, through one shared
   function. There is no branch on which one, because they are one mechanism: a binding is a reference
   whose Secret name kelson derives from the Project's own data component, and a `{secret, key}` is one
   the author names directly.
2. **Nothing in kelson inlines a Secret's data into a workload manifest.** The renderer is pure
   (ADR-0001): it has no cluster client, so it *cannot* read a Secret even if some future code path
   asked it to. The `cluster` backend keeps the value in the cluster and kelson never fetches it.
3. **A plaintext Secret is not something the renderer can emit.** There is no function in
   `internal/renderer` that writes `data:` or `stringData:`, and no spec field that would call one. The
   guarantee here is the absence of a mechanism, not the suppression of one — which is why the test for
   it asserts structure (no `kind: Secret`, no `stringData`) rather than scanning output for
   credential-shaped strings.

And the boundary, stated as plainly as ADR-0009's redaction amendment states its own:

**kelson cannot stop a user writing a password as a plain string.** `SESSION_PEPPER: hunter2` renders,
exactly as #82's review note said. `model.SecretShapedName` catches the common spellings — `PASSWORD`,
`TOKEN`, `API_KEY` and their relatives, plus any URL with embedded userinfo — and it is a heuristic;
ADR-0009's honesty note already says so and this ADR does not repeal it. What became structural is a
narrower and truer claim: **the typed path cannot leak.** A reference is a reference from the YAML the
author writes to the bytes the cluster receives, with no stage in between that could resolve it into a
value.

Two consequences follow and are accepted:

- **`spec.overlays` remains the escape hatch**, including for a raw `kind: Secret` manifest with real
  `stringData`. That is deliberate — an overlay is how kelson stays honest about what it does not
  model — and it is the one path that can put a value into a rendered set. `internal/redact` covers its
  *display* (#117): a Secret's `data`/`stringData` is replaced with `[redacted]` in every diff, preview,
  API read-back and log, selected by the resource's `kind` rather than by what the value looks like.
  Delivery bytes are not redacted, because writing `[redacted]` into a cluster as a credential would be
  a silent corruption worse than the leak.
- **The heuristic still fires, and its remediation now names a fix that exists.** Before this ADR the
  error pointed at an overlay, because there was nothing else. It now leads with
  `{secret: <name>, key: <key>}` and the `kubectl` line that creates the Secret, keeps the managed-service
  binding as the shorter path where it applies, and keeps the overlay last.

### 5. `secrets.backend` is ungated; `secrets.store` stays gated

The #141 gate table loses two whole-block rows and gains two narrower ones.

**Ungated, because it is consumed:** `Environment.spec.secrets.backend` and
`Project.spec.defaults.secrets.backend`. `cluster` renders — a secret reference becomes a secretKeyRef
and the backend needs nothing else emitted — and `externalSecrets` and `sops` are a structured render
error, `render/secret-backend-unsupported`, whose remediation names #80 or #81 and the backend that
works today. Changing the value changes the outcome, which is what "consumed" has to mean; refusing a
backend by name is not silence.

**Still gated:** `secrets.store`, with `schema/not-implemented` naming #80. It configures the
`externalSecrets` backend and nothing else, and that backend renders nothing.

The refusal lives in the **renderer**, not in validation, for the reason ADR-0016's Helm gate does: it
is decided from spec data alone, before anything is emitted, so the same document renders the same way
against every cluster, and every surface that can reach a cluster passes through `Render`. (#82's
review note asked for the enforcement boundary to be stated rather than implied. It is stated here:
*shape* is the model's — an env value is one of three forms and a fourth is unrepresentable — and
*mechanism* is the renderer's.)

### 6. Successors, and the Valkey password (noted, not implemented)

[#98](https://github.com/dafrie/kelson/issues/98)'s open acceptance criterion — "adding a Valkey
service and referencing it yields a working connection with no manual credential handling" — is not
closed by this ADR and is no longer blocked by a missing design. `kind: valkey` binds `uri`, `host` and
`port` and *withholds* `password` with a structured render error
([ADR-0015](0015-valkey-operator.md)): the Valkey operator generates no application credential, it
reads user passwords from a Secret it never creates, and a pure renderer has no random source
([#20](https://github.com/dafrie/kelson/issues/20)).

The path that closes it is now three pieces that all exist or are specified: a Secret holding the
password, authored with `kelson secret set` (#116); a Valkey ACL user rendered against that Secret's
name; and this reference schema for the workload side, so a component reads the same password with
`{secret: <name>, key: password}`. **Nothing here implements it** — it needs a spec surface for the
user, which is #98's work and a decision this ADR does not take.

*(2026-08-14: all three now exist. `kelson secret set` (#116) writes the Secret; the
[ADR-0015 amendment](0015-valkey-operator.md#amendment-2026-08-14--auth-a-cache-with-a-password) adds
`auth: {secret, key}` to a `kind: valkey` component, which renders the operator's ACL user against
that Secret and turns the `password` binding into a `secretKeyRef` against the same one. It took the
spec-surface decision this ADR declined to take, and it took it in this ADR's own shape: `auth:` is a
`model.SecretRef`, so the reference schema decided here now describes both a value a workload reads
and a credential an operator is configured with, through one type and one validator. Nothing in §1–§5
changed, which was the stated test. The one thing the amendment refused on this ADR's behalf is a
`redis://:password@host` URI — a value in a manifest, which §4's guarantee does not permit and would
have had to be withdrawn to allow.)*

### Successor note (2026-08-14): #116 landed, and the remediation changed

[#116](https://github.com/dafrie/kelson/issues/116) shipped the `cluster` backend's authoring path —
`kelson secret set|list|delete`, a `SecretService` on the API, and a `set_secret` MCP tool — over
`internal/secret`. Nothing in this ADR's decision changed, which was the stated test of whether it was
right: the spec text, the union, the merge rules and the renderer guarantee are untouched.

What changed is the sentence this ADR predicted would: the `secret/literal` remediation and
docs/model.md now lead with `kelson secret set` and keep `kubectl create secret generic` as the
alternative rather than as the only answer. The `.env` migration path in the first positive
consequence below reads the same way now.

Three decisions inside #116 are worth recording here because they narrow this ADR's frame rather than
following from it:

- **kelson labels what it writes and touches nothing else.** Every Secret it authors carries
  `kelson.dev/managed-secret: "true"`; listing is a label query over it, and deleting *or overwriting*
  an unlabelled Secret is refused (`secret/not-managed`). A reference may still name any Secret in the
  namespace — this ADR's §2 is unchanged — but the authoring path claims only its own. Adopting one is
  a deliberate `kubectl label`, which the refusal spells out.
- **`set` merges rather than replaces.** A server-side apply of only the keys given would prune the
  rest, so one command would silently drop a credential written by an earlier one.
- **No value is readable back through kelson.** There is no `get`, and no message in the schema has a
  field a value could arrive in — the masked read-back of ADR-0009 as a type property rather than as a
  handler's promise. Reading a value is `kubectl get secret`, with the cluster's own RBAC and audit
  trail behind it.

The third negative consequence below stands unchanged: `kelson secret delete` does **not** look for
referrers, because nothing in kelson correlates a reference with the Secret it names. It says so when
it deletes. What partly closes the gap is that `diagnose_application` now reports the environment's
kelson-managed Secrets by name and key alongside the spec summary, so the comparison an agent (or a
reader) has to make is at least in one answer.

## Consequences

**Positive.**

- Every credential has a spelling. The `.env` migration path is `kelson secret set` (or
  `kubectl create secret generic`) plus one mapping per variable, and the error an author hits on
  their first render tells them exactly that.
- One mechanism, one rendered shape. Bindings and references both become `secretKeyRef`, through the
  same code, so a reader who understands one understands both.
- The backend is a one-field migration. A spec written today against `cluster` is the spec that runs
  against `externalSecrets` or `sops` when they land.
- The guarantee is testable as a structural property rather than as a promise about strings.

**Negative.**

- **The heuristic is still the only thing standing between an author and a plaintext credential under
  a creatively named key.** This ADR narrows what "the guarantee" claims; it does not widen what is
  caught. A shape-based rule that could catch the rest — say, refusing every plain string whose value
  is high-entropy — was considered and rejected: it would refuse legitimate configuration (a base64
  fixture, a long feature-flag list) and still miss the credential shaped like a word.
- **kelson does not know whether the Secret exists.** A reference to a Secret nobody created renders
  cleanly, applies cleanly, and fails at pod start with a `CreateContainerConfigError`. Validation
  cannot check it (no cluster, ADR-0001) and the renderer must not (purity). Surfacing it belongs to
  the observation plane, and until then the failure is one the cluster reports and kelson relays.
- **The dependency between a reference and the Secret it names is invisible to kelson's own tooling.**
  Deleting a Secret breaks a workload with nothing in kelson saying so; `kelson secret unset` (#116)
  will have to decide whether to look for referrers.
- **The UI's form editor cannot yet author a reference.** `ui/src/spec/edit.ts` round-trips env values
  as strings, so a spec carrying references opens in YAML-only mode until the editor learns the shape.
  This is safe automatically rather than by anyone remembering: the editor's byte-guard refuses to
  write back a document it cannot represent faithfully, so the failure mode is "you must edit this in
  YAML", not "your reference was silently flattened into the string `map[secret:…]`".
- **Two mapping arms is one more shape than the schema had.** `oneOf: [string, {from}, {secret, key}]`
  is a union an author can get wrong in a new way, and the reference documentation now has to say
  "object {from}, object {secret, key}" where it used to say "object". The remediation on a malformed
  mapping lists all three forms for exactly this reason.
- **A reference cannot name a Secret in another namespace, and the error for trying is an unknown
  field** rather than a considered refusal, because there is no field to consider.

## Revisit when

- **#116 lands `kelson secret set`.** The remediation stops naming `kubectl` and the migration story
  becomes one command. Nothing in the schema changes, which is the test of whether this decision was
  right.
- **#80 or #81 lands a second backend.** The question then is whether `secretKeyRef` remains the
  rendered shape for all three (for `externalSecrets` it does — an `ExternalSecret` *creates* a Secret
  the reference then addresses) or whether `sops` needs something else in the delivery path. If the
  spec text has to change for either, this decision was wrong and the ADR that fixes it should say so.
- **Somebody needs a secret as a file.** Mounted-volume projection is the next authoring surface a real
  workload asks for (TLS material, service-account JSON), and it is a different set of questions than
  an env value.
- **The observation plane can report a missing Secret.** That turns the second negative consequence
  from "the cluster tells you" into "kelson tells you", and it is where the reference model stops being
  purely a spec concern.
