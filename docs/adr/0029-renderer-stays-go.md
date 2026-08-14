# ADR-0029: The renderer stays pure Go; CUE and timoni are rejected as the rendering engine

- **Status:** Accepted
- **Date:** 2026-08-14

> Confirms [ADR-0001](0001-hybrid-state-model.md)'s pure-renderer half rather than changing it. Written
> because the rebuild that produced [ADR-0027](0027-crd-native-control-plane.md) and
> [ADR-0028](0028-delivery-spine.md) put every other layer on the table, and the renderer deserved to
> be put on it too rather than surviving by inattention.

## Context

[timoni.sh](https://timoni.sh) is a CUE-based package manager for Kubernetes with a well-chosen model:
a **Module** is roughly a chart, an **Instance** is roughly a release, a **Bundle** is roughly an
umbrella chart, and all three are distributed as OCI artifacts. Its values are typed and validated by
CUE rather than checked by a template engine after the fact, which is the exact failure mode Helm's
`values.yaml` has. It overlaps kelson's territory closely enough that "why is kelson not built on
timoni?" is a question a reader will ask, and the honest answer is that during the rebuild it was
seriously evaluated as the rendering engine and rejected.

Two things made the question live rather than theoretical. [ADR-0028](0028-delivery-spine.md) makes
every kelson deployment an OCI artifact, which is timoni's distribution model, so the two systems now
speak the same transport. And [ADR-0030](0030-flux-aio-install.md) has kelson consuming a timoni module
at release time, so the toolchain is not foreign to the project any more.

The evaluation therefore asked one question: should `internal/renderer` be replaced by CUE definitions
and `timoni build`?

## Decision

**kelson's authoring layer renders through the existing pure Go renderer. CUE is not adopted as the
rendering engine, and timoni is not a dependency of the render path.**

`internal/renderer` stays what [ADR-0001](0001-hybrid-state-model.md) made it: a pure function
`(spec, ClusterProfile) → manifests`, emitting ordered `*yaml.Node` values, enforced against `os`, `io`,
`time` and every network client by depguard.

**`kind: timoni` is reserved as a future component kind**, mirroring `kind: helm`
([ADR-0016](0016-delivery-flows-v0.md) decision 4): a component that names a module, a version and
values, and delegates instantiation to a controller or CLI rather than to kelson's renderer. It is not
built, not in the enum, and tracked in the issue tracker. Reserving it costs nothing and states the
relationship: timoni is a packaging layer that could sit *below* kelson's authoring layer, not a
competitor to it.

## Rationale

**The thing CUE would bring is the thing kelson already has.** CUE's pitch for configuration is typed,
validated, composable values with errors that point at the mistake. kelson has typed structs in
`internal/model`, a single `validate.go`, a structured error taxonomy with slash codes that agents
branch on (`schema/unknown-field`, `render/image-unresolved`, …), remediation prose on every error, and
line/column positions carried from the authored YAML through to the CLI, the wire and — after
ADR-0027 — an object's status. Adopting CUE would not add typed validation; it would *replace* kelson's
typed validation with a different one whose error messages kelson does not control.

**Error quality is the moat, and it is the first thing an engine swap surrenders.** This project's
distinguishing claim is that it refuses loudly and usefully: `notimplemented.go`'s gate table exists
specifically so that a field which renders nothing produces an error naming the tracked work rather
than silence, on the stated grounds that *"for an agent that silence is indistinguishable from success,
which makes it the worst failure shape this project can ship"*. That mechanism is Go code holding a
table of paths, issue references and remediation text. In CUE it would become a disjunction failure with
a CUE-shaped message, and every one of the four properties that make it valuable — the code, the field
path, the remediation, the tracked issue — would have to be smuggled back in.

**The golden-test corpus is the asset, and it is engine-shaped.** Correctness in kelson lives in
golden-file tests, deliberately, since ADR-0001: *"most correctness lives in golden-file tests instead
of an integration suite needing a live cluster"*, and AGENTS.md treats a `testdata/` change as a
behaviour change requiring explicit review. A rendering-engine swap invalidates the corpus wholesale —
not the expectations, which could be kept, but every reason to believe the new engine produces them for
the right reasons. That is the largest single body of accumulated verification the project owns.

**Timoni-as-engine buys little in a Flux-only world.** Timoni's genuine value over `helm template` is
its *lifecycle* machinery: instance inventory, apply ordering, drift detection, garbage collection,
atomic upgrade with rollback. All of that is client-side in the `timoni` CLI; there is no GA in-cluster
timoni controller. In the architecture [ADR-0028](0028-delivery-spine.md) settles, kustomize-controller
already owns inventory, ordering, drift and pruning, and it owns them for everything kelson deploys
rather than for a subset. So kelson would use exactly one part of timoni — `timoni build`, the
templating — and would pay for it with the whole CUE toolchain in the render path.

**The dependency shape is wrong for a load-bearing layer.** Rendering is the one component of kelson
that must never be blocked. Timoni is a small project with a single primary maintainer, and the CUE
runtime is a large dependency with its own evaluator performance characteristics and its own release
cadence. [ADR-0030](0030-flux-aio-install.md) accepts timoni at *release time*, in CI, where a stall is
an inconvenience; accepting it at *render time* would put it on the critical path of every deploy, every
diff, every preview and every test. [ADR-0021](0021-installing-missing-components.md)'s Bitnami lesson
is about exactly this asymmetry: where a third party's decision can stop your users, the dependency has
to be one you can survive.

**Timoni is below kelson, not beside it.** A timoni Module packages one piece of software for
distribution. A kelson Project describes an organisation's applications, their environments, their data
services and the bindings between them, and *derives* the packaging. They are different layers, and the
correct integration is the reserved `kind: timoni` above — the same delegation ADR-0005 makes to
operators and ADR-0016 makes to helm-controller — not a substitution.

**What was actually attractive, and was not enough.** CUE's constraint composition is better than
anything a Go struct plus a validator can express: `#Deployment & {replicas: >0}` composes where
kelson's overrides precedence P1–P6 is hand-written code. If kelson's precedence rules keep growing,
that argument gets stronger. It is not stronger today than the four costs above.

## Consequences

**Positive.**

- The golden corpus, the error taxonomy, the gate mechanism and the line/column provenance all survive
  a rebuild that changed the storage layer, the delivery layer and the install layer around them.
- Rendering stays a pure Go function with no external runtime, so it runs in the CLI offline, in the
  server, in the controller and in tests, at the same speed, with the same output.
- The `main` depguard rule stays as narrow as it is. A CUE runtime in the renderer would have widened
  the one fence this project most wants narrow.
- The relationship to timoni is now recorded rather than repeatedly rediscovered.

**Negative.**

- **kelson maintains a renderer forever.** Every Kubernetes API a user wants is Go code someone writes,
  reviews and golden-tests. A CUE-based project inherits schema-driven generation for that; kelson does
  not, and the cost is paid per resource kind, indefinitely.
- **Composition stays hand-written.** The precedence rules, the override merging and the defaults
  cascade are imperative Go, and each new axis of override is more of it. This is the argument most
  likely to reverse this decision.
- **Users already invested in CUE get nothing from kelson's authoring layer.** They can package with
  timoni and, if `kind: timoni` is ever built, run it beside kelson components — but their CUE
  definitions do not describe a kelson Project.
- **Rejecting an overlapping tool is a claim that has to keep being true.** If timoni's module ecosystem
  becomes the way software is distributed for Kubernetes, "kelson renders it itself" ages from a moat
  into an isolation.

## Revisit when

- **A timoni controller ships and reaches GA.** That changes the lifecycle argument entirely: timoni
  would then be a delegation target in the ADR-0005 sense, and `kind: timoni` becomes work to schedule
  rather than a reservation.
- **The timoni module ecosystem matures** to where the modules a kelson user wants exist and are
  maintained, which is the same test [ADR-0015](0015-valkey-operator.md) applied to an operator.
- **kelson's own composition rules outgrow hand-written Go.** If precedence, defaults and overrides
  reach the point where each new rule risks breaking two others, CUE's constraint model is the known
  answer and this ADR is what gets superseded.
- **A second renderer target appears** — a non-Kubernetes backend, or a materially different API
  surface — at which point "one hand-written renderer" becomes "two", and a schema-driven engine looks
  different.
