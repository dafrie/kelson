# Should kelson accept Score as an input format?

Research deliverable for [#31](https://github.com/dafrie/kelson/issues/31). Recommendation only —
nothing is built under that issue.

**Recommendation: yes to a one-way importer in M15, no to Score as a first-class input.**
Two schema gaps it exposes are worth fixing on their own merits, independently of Score.

## What Score is

A [CNCF Sandbox](https://www.cncf.io/blog/2024/08/08/score-accepted-as-a-cncf-sandbox-project/)
workload specification — a file format with no runtime, no CRDs and no controller. Implementations
(`score-k8s`, `score-compose`) translate a Score file into platform primitives. It is not a competitor;
it occupies the authoring plane only.

The `score-v1b1` schema requires `apiVersion`, `metadata` and `containers`, and optionally carries
`service.ports` and `resources`. Three facts drive everything below:

- **`containers` is a map**, so a workload may declare several containers.
- **`service.ports` is a map**, so a workload may expose several named ports.
- **There is no cron, schedule or replica concept anywhere in the schema.**

## How much maps cleanly

The single-container, single-port, one-database shape maps almost 1:1:

| Score | kelson |
|---|---|
| `metadata.name` | Application `name` |
| `containers.<x>.image` | `image` |
| `containers.<x>.command` / `args` | `command` |
| `containers.<x>.variables` | `env` |
| `containers.<x>.resources.{requests,limits}` | `resources.{requests,limits}` |
| `containers.<x>.livenessProbe.httpGet.path` | `health` |
| `service.ports.<x>.port` | `port` |
| `resources.<x>.type: postgres` | Project `services[].type: postgres` |

Past that shape it stops mapping, in four places:

**Multiple containers.** kelson's Application is one container by design; sidecars are an overlay
([docs/architecture.md](../architecture.md)). A two-container Score file has no faithful Application
representation. An importer can emit the first container as the Application and the rest as an overlay
patch, but that is a lossy translation and must say so.

**Multiple named ports.** kelson has a single `port: int`. A workload exposing HTTP plus a metrics port
is ordinary, and today it requires an overlay to add the second Service port.

**`files`.** Score injects config files by `content`, `binaryContent` or `source`. kelson has no
first-class file-injection mechanism at all; the equivalent is a hand-written ConfigMap in an overlay.

**Variable interpolation vs. ADR-0009.** This is the sharpest conflict, and it is not cosmetic.
Score's idiom is string interpolation:

```yaml
variables:
  DATABASE_URL: "postgres://${resources.db.username}:${resources.db.password}@${resources.db.host}/app"
```

[ADR-0009](../adr/0009-secrets.md) makes that value unrepresentable in kelson: an env value is a plain
string, a `{secret: <name>, key: <key>}` reference or a `{from: {service, key}}` binding
([ADR-0018](../adr/0018-secret-references.md)) and nothing else, and `internal/model` rejects a
password-bearing URL with `secret/literal`. An importer can only translate the *whole-value* case
(`${resources.db.uri}` → `{from: {service: db, key: uri}}`, or a reference where the value is not a
kelson-managed service). Partially interpolated strings must be
rejected with a real explanation, not silently flattened — flattening them would write a credential into
the spec, which is the one thing the secrets model exists to prevent.

## Importer, or first-class input?

**Importer.** First-class Score input fails on three counts:

1. **Score has no environment model.** No environments, no per-environment overrides, no delivery mode,
   no routing, no replicas. kelson's P1–P6 precedence rules ([docs/model.md](../model.md)) are most of
   what the authoring plane actually does. A Score file alone can never produce a deployable
   configuration — you would still author a kelson Environment beside it. "First-class alternative
   input" is therefore incoherent rather than merely expensive.
2. **It doubles the validation surface.** The structured error taxonomy (#28) is defined over kelson
   field paths. A second input dialect needs its own codes, paths and remediations, or it degrades to
   worse errors for Score users — the opposite of the point.
3. **No cron.** `schedule:` has no Score representation, so a whole workload kind would be
   unreachable through that input.

A one-way `kelson import score` that emits a kelson Project for a human to review and commit keeps the
renderer's single input model intact (ADR-0001) and costs one command.

## What Score reveals that is worth fixing anyway

Two of the mismatches are not Score's problem — they are ours, and they would show up without Score:

- **Multiple named ports.** HTTP + metrics is a normal shape, and forcing an overlay for it is a poor
  default. Worth considering a `ports:` form in a later schema revision, with `port:` kept as the
  one-port shorthand.
- **File injection.** Config-file-driven software (nginx, Prometheus, most JVM apps) needs a file, and
  "write a ConfigMap in an overlay" is a steep first step.

Multi-container is a third signal but a weaker one: delegating sidecars to overlays is a deliberate
decision ([ADR-0005](../adr/0005-delegate-to-operators.md) reasoning), and Score's support for it is not
by itself an argument to reverse that.

Score's open-ended resource types (`dns`, `s3`, `volume`, …) are explicitly *not* a gap. kelson's narrow
`postgres`/`valkey` set with real presets is a decision recorded in [ADR-0007](../adr/0007-data-services.md);
an importer should fail loudly on resource types it cannot honour rather than approximate them.

## What adopting Score wholesale would cost

The environment and precedence model, the no-literal-secrets guarantee, the derived workload kind
(`port:`/`schedule:`), bindings as enumerable data, and the error taxonomy tied to our own field paths.
Each would have to be rebuilt as a platform-specific extension *around* Score — which is the position
Score's own design intends platforms to be in, and is a worse place to stand than translating into a
model that already covers it.

## Conclusion

Ship `kelson import score` in M15 as a lossy, one-way, explicitly-reported translation: clean for the
common shape, loud about multi-container, multi-port, `files` and partial interpolation. Treat named
ports and file injection as independent schema questions.
