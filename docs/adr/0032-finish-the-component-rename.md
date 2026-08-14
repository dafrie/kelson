# ADR-0032: Finish the component rename — the label, the selector and the MCP tools

**Status:** Accepted
**Date:** 2026-08-14
**Amends:** [ADR-0014](0014-components.md) (its third negative consequence — the carve-out at the
cluster boundary), and the tool names in [ADR-0008](0008-mcp-surface.md)

## Context

[ADR-0014](0014-components.md) renamed the model's leaf from *Application* to *Component* and unified
`spec.applications` and `spec.services` into one `spec.components` list. It rewrote every stored spec,
every example, every fixture and every document in the repository — and then stopped at one line,
deliberately. From its Consequences:

> **The rename stops at the cluster boundary.** Rendered resources keep the `kelson.dev/application`
> label, and Deployments keep selecting on it. A Deployment's selector is immutable in Kubernetes, so
> renaming that label would orphan every running workload on upgrade — the spec's vocabulary changed,
> the cluster's identity did not. The same applies to the log selector and to `list_applications` and
> `diagnose_application` on the MCP surface: those name a domain an operator already knows, not a YAML
> key.

That carve-out rests on exactly one load-bearing fact: **there are running workloads that an upgrade
would orphan.** Everything else in the paragraph is a rationalisation of it. "Those name a domain an
operator already knows" is not an argument that survives on its own — the operator learns the domain
from kelson's own spec, docs and CLI, all of which now say *component*, and the only place the retired
noun still lived was the surface an agent selects a tool from.

The fact has expired. Nothing is deployed anywhere except one disposable local kind cluster, which is
recreated by `make kind-up` on demand. There is no fleet to orphan.

The window is also closing, and it closes permanently. A Deployment's selector is immutable *forever*,
not just across one upgrade: the first real user makes this rename impossible to perform without
telling them to delete their production workloads. Today it costs one `make kind-down`.

## Decision

**A. Rename the label.** `kelson.dev/application` becomes `kelson.dev/component` everywhere it is
emitted, selected on, or asserted: the renderer's provenance labels and its Deployment/Service
selectors, the log query's pod selector, the diff's provenance-suppression list, the promotion's
workload correlation, both build workload builders, the e2e harness and script, and the examples. It
is the last `kelson.dev/*` key that carried the retired vocabulary; `kelson.dev/installed-component`
and `kelson.dev/component-ownership` already used the settled word for a different concept (a platform
component kelson installed, per [ADR-0021](0021-installing-missing-components.md)) and are untouched.

**B. Rename the three MCP tools.** `list_applications` → `list_components`, `diagnose_application` →
`diagnose_component`, `promote_application` → `promote_component`, together with their Go identifiers,
their titles and every cross-reference inside other tools' descriptions. `logs_window`'s `application`
input parameter becomes `component` for the same reason. This amends the tool names
[ADR-0008](0008-mcp-surface.md) chose; its design rule — tools are task-shaped, not endpoint-shaped —
is untouched, and nine tools remain nine tools.

**C. The spec-hash payload key moves too.** The renderer's `kelson.dev/spec-hash` is a sha256 over a
small JSON document whose third key was `"application"`. ADR-0014 held it there with the same argument
in miniature: renaming it would churn the annotation on every workload in every cluster to say nothing
new. Decision A already forces every one of those workloads to be deleted and recreated, so there is
no annotation left to spare. Every spec-hash in the golden files changes.

**D. The wire fields keep their v1alpha1 names.** `LogSelector.application` and `Error.application`
in `proto/kelson/v1alpha1` still say *application*; only their comments changed. Renaming a proto
field is a separate break with a separate blast radius (every generated client, in two languages) and
it buys nothing decision A did not already buy — the label is what the cluster matches on, and the
wire field is a string an RPC carries. The Go types on both sides of the boundary do say *component*,
so the mismatch is confined to two mapping sites, each with a comment naming this ADR. When the schema
next takes a breaking change, these two fields go with it.

**E. A dedicated error code for the fallout.** `delivery/immutable-field` is added to the delivery
taxonomy, and the direct adapter classifies the API server's 422 into it (see Consequences).

## Rationale

**Why not keep the label and rename only the docs.** Because the label is not documentation, it is the
identity an operator types into `kubectl get -l`, and it is the identity the log query, the diff, the
promotion and both build paths all agree on. One vocabulary in the spec and another in the cluster is
a translation step at every boundary, and the person paying for it is whoever is debugging at the time.

**Why now rather than never.** "Never" was a real option: the carve-out could have been left standing
and the divergence documented as permanent. It was rejected because the cost of the divergence is paid
forever by everyone, while the cost of the rename is paid once, today, by nobody — and because the
divergence would have kept spreading. The spec-hash key of decision C is what that spreading looks
like: a second carve-out, argued from the first, in a place a reader would not think to look.

**Why the wire fields are treated differently from the label.** The label is matched on by the API
server and cannot be changed without deleting objects; that is a cost this change accepts because it
is currently zero. A proto field is matched on by generated clients, and changing it costs a
regeneration in every consumer plus a break for anyone pinned to the old schema — a cost that is *not*
currently zero, because the UI is under concurrent development against these types. Two breaks are not
better than one when the second buys nothing.

## Consequences

**Positive.**

- One vocabulary end to end. The spec, the model, the renderer, the cluster labels, the CLI, the docs
  and the agent surface all say *component*, and `kubectl get -l kelson.dev/component=web` works
  without anyone having to remember the old word.
- The last deliberate carve-out from ADR-0014 is closed, and the ADR record says so rather than
  leaving a reader to discover the divergence from a golden file.
- `delivery/immutable-field` makes a whole class of failure legible, not just this one. Any immutable
  field on any resource — not only a Deployment's selector — now produces a remediation that works
  instead of one that cannot.

**Negative.**

- **A Deployment's selector is immutable.** A workload deployed before this change cannot be updated
  in place after it — the apply fails on the immutable selector. The fix is to delete the Deployment
  (or the whole environment) and redeploy. There is no migration path and this ADR does not pretend to
  offer one. Concretely: `kelson uninstall --project <p> --env <e>`, then `kelson deploy`. In the local
  kind cluster, `make kind-down && make kind-up`.
- **Every golden file changes twice over**: once for the label, once for every `kelson.dev/spec-hash`
  value (decision C). Rendered output changed for unchanged input, and it is reviewed as a behaviour
  change.
- **Any recorded revision from before this change is stale in two ways.** Its rendered manifests carry
  the old label, and its spec-hash was computed under the old payload key. A rollback to such a
  revision re-applies manifests whose selector the live object cannot accept, and fails the same way a
  forward deploy does. Deleting the environment is the only route back.
- **The MCP tool names are a breaking change for any agent that hard-codes them.** kelson is pre-alpha
  with no compatibility promise, and nothing in this repository pins the old names, but a saved agent
  prompt elsewhere would break with a "tool not found" rather than a rename hint.
- **The spec and the wire now disagree about one word.** Decision D is a deliberate half-measure and
  will read as an inconsistency to anyone who meets the proto before the Go. The two mapping sites
  carry comments; the inconsistency is real and stays until the next schema break.
- **ADR-0014's text is now partly false as written.** Its third negative consequence describes a
  carve-out that no longer exists. Per this repository's convention (ADR-0006 kept the word
  *Application* throughout its body and carries the amendment in its Status line), the text stands as
  the record of what was decided then, and the Status line points here. A reader who finds ADR-0014
  alone will read a carve-out that has been reversed.

**What the operator actually sees.** An apply against a workload from before this change is a 422 from
the API server: `Deployment.apps "web" is invalid: spec.selector: Invalid value: …: field is
immutable`. Before this ADR the direct adapter classified that as the generic `delivery/apply-failed`
with the remediation *"fix the spec or the admission policy that rejected it, then re-deploy"* — which
is precisely the wrong instruction, because the spec is what the object should be and re-deploying
fails identically. It now classifies as `delivery/immutable-field`, names the stuck field, keeps the
API server's own words in `cause`, and says:

> delete `Deployment/checkout-production/web` and deploy again — re-deploying without deleting fails
> the same way, and there is no in-place edit that reaches the new value

Detection is on the validation cause's message (`field is immutable`, taken from apimachinery's own
`validation.FieldImmutableErrorMsg`) rather than on the status reason, because the API server reports
every immutability rejection as `Invalid` and only the cause distinguishes "you cannot change this"
from "this value is malformed". See `internal/delivery/direct/direct.go` and
[docs/delivery.md](../delivery.md).

## Revisit when

- **The v1alpha1 schema takes its next breaking change.** Decision D's two wire fields go with it, and
  the spec/wire disagreement closes.
- **There are users.** This ADR is only cheap because there are none. The same reasoning must not be
  reused to justify a later rename: after the first real deployment, "delete the Deployment and
  redeploy" stops being free and starts being someone's outage.
