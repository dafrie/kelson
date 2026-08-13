# The MCP server

`kelson-mcp` is kelson's agent surface: a [Model Context Protocol](https://modelcontextprotocol.io)
server that lets an agent deploy, diagnose and roll back applications through the same v1alpha1 API
the CLI and the UI use (issue [#73](https://github.com/dafrie/kelson/issues/73),
[ADR-0008](adr/0008-mcp-surface.md)).

It speaks MCP over **stdio**: an MCP client launches it as a child process and it turns tool calls
into ConnectRPC calls against a running `kelson-server`. It holds no state, opens no listener and
ships no container image — it lives and dies with the client that started it.

## Capability parity, not surface parity

The rule the surface is designed around ([ADR-0008](adr/0008-mcp-surface.md)):

> If MCP needs a **capability** the API lacks, the API is wrong. If it needs a different **shape**,
> that is the point.

Every tool decomposes into API calls any other client could make, which is what keeps the agent
surface from becoming a privileged backdoor with its own logic. Nothing in `internal/mcp` reads a
cluster, classifies a workload's health or decides what a delivery phase means — it composes
`SpecService`, `DeployService`, `LogService` and `EventService` and relays their answers, including
their structured errors, verbatim.

The *shape* is deliberately not the API's:

- **Task-shaped.** `diagnose_application` is one call where a resource-shaped mapping would be six.
- **Bounded.** The API streams; tools return windows. Every list is capped and says
  `… N more (truncated)` when it truncates. `LogService.FollowLogs` has no tool at all, and a test
  asserts it never gains one.
- **Described for models.** Every tool states its preconditions, whether it mutates, what it costs
  and when to prefer a neighbouring tool.
- **Seven tools.** The count is a design decision: every tool added costs tool-selection accuracy for
  the ones already there.

## The tools

| Tool | Mutates | What it does |
|---|---|---|
| `list_applications` | no | Every stored project with the live phase, revision and workload health of each environment. Start here when you do not know what exists. |
| `diagnose_application` | no | The flagship composition: phase, revision, namespace and cause, workload verdicts with remediation, a log window around the failure, the last 5 revisions and a compact spec summary — in one call. |
| `logs_window` | no | A bounded log window (≤ 200 lines) for one application, optionally the lines before a container terminated, optionally filtered. Never follows. |
| `deploy` | **yes**, unless `dry_run` (default `render`) | Renders, server-side dry-runs or deploys. With `dry_run="none"` it consumes the deploy stream to the settled outcome and returns that — never a stream. |
| `rollback` | **yes**, when `execute=true` | Previews what a rollback cannot revert (unrecoverable findings flagged) plus the change counts; applies it on request. |
| `put_spec` | **yes**, when `dry_run=false` | Validates a spec and returns structured errors (code, field, line, remediation); stores it on request, with optimistic concurrency on `version`. |
| `wait_for_outcome` | no | Consumes the event stream and returns on the first terminal signal — Healthy, Rejected, a workload turning unhealthy, or the timeout. This is what makes "deploy, then react" cheap. |

Which RPCs each tool composes is declared in a table in `internal/mcp/tools.go`, and a test walks the
generated Protobuf descriptors to assert every declared pair names a real service and method. A second
check, on every tool call in the package's tests, asserts a tool called nothing it did not declare.
The Protobuf schema is a consistency check here, never the design.

### Bounds

| What | Cap |
|---|---|
| Projects / environments per project in a listing | 25 / 10 |
| Workload verdicts, applications, findings | 12 / 20 / 15 |
| Log lines — `diagnose_application` / `logs_window` | 80 / 200 |
| History entries | 5 |
| Deploy transitions, watch events | 20 |
| Manifests in a deploy preview | 30, identities and sizes only — never bodies |

## Configuration

`kelson-mcp` needs a reachable `kelson-server`:

```sh
kelson-server &                 # loopback, port 8420 by default
kelson-mcp --server http://127.0.0.1:8420
```

The address comes from `--server`, then `$KELSON_SERVER`, then `http://127.0.0.1:8420`. Diagnostics
go to stderr — stdout is the protocol.

A generic MCP client configuration:

```json
{
  "mcpServers": {
    "kelson": {
      "command": "kelson-mcp",
      "args": ["--server", "http://127.0.0.1:8420"],
      "env": {}
    }
  }
}
```

Claude Code registers the same thing from the command line:

```sh
claude mcp add kelson -- kelson-mcp --server http://127.0.0.1:8420
```

## Authentication: there is none in v0

`kelson-server` has no authentication and no TLS in v0. It binds loopback and refuses a non-loopback
`--listen` without `--insecure-bind` ([ADR-0013](adr/0013-server-state-and-api-v0.md) §3). `kelson-mcp`
matches that posture exactly: it holds no credential and sends none, so it must run somewhere that can
reach the server directly. A failure to connect says so in the tool error rather than looking like a
missing token.

Two consequences worth stating plainly, because both are part of issue #73 and neither is implemented:

- **Agents are not yet principals.** Scoped, expiring, agent-owned credentials are
  [#74](https://github.com/dafrie/kelson/issues/74). Until then an agent has whatever access the
  machine running `kelson-mcp` has.
- **Tool exposure is not policy-aware.** [ADR-0008](adr/0008-mcp-surface.md) §4 says tools a caller's
  credentials cannot use should not be exposed at all. That needs a policy engine to ask, and there
  is none until [#75](https://github.com/dafrie/kelson/issues/75). **Every tool is currently exposed
  to every caller**, including the mutating ones — a fact stated here rather than implied by an
  ergonomic filter that would not be enforcing anything.

What does exist today is the guardrail that matters most for an agent: every mutating tool defaults
to a preview (`deploy` to `dry_run="render"`, `rollback` to `execute=false`, `put_spec` to
`dry_run=true`), carries an idempotency key so a retry is not a second deployment, and reports
failures as structured errors with a code an agent can branch on and a remediation it can act on.

## Where the code lives

- `cmd/kelson-mcp` — the stdio binary and its flags.
- `internal/mcp` — the tool implementations, over the generated ConnectRPC clients. The package's
  depguard rule (`.golangci.yml`, the `api` rule) gives it the transport, the generated code and the
  MCP SDK and no cluster client, so a tool cannot reach past the API it claims to compose.
