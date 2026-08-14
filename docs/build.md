# Build plane — how kelson turns source into an image

Reference for `kelson build` (M4) and for `BuildService`, the same plane over
the API ([#54]). Implements issues [#48], [#49] and [#51]; the strategy decision
is [ADR-0010](adr/0010-build-strategy.md).

A kelson Project can name a git repository instead of an image. `kelson build`
is what turns that repository into a digest-pinned image reference, which
`kelson render`, `kelson diff` and `kelson deploy` then consume through
`--image`.

```sh
kelson build -f project.yaml --registry ghcr.io/acme --push-secret ghcr-push
```

The last line of stdout is the reference, so the two commands compose:

```sh
IMAGE=$(kelson build -f project.yaml --registry ghcr.io/acme | tail -1)
kelson deploy -f project.yaml -f production.yaml --env production --image "$IMAGE"
```

That pipeline is the whole integration. There is deliberately no `kelson deploy
--build`: it would need six extra flags on `deploy`, all inert unless `--build`
were passed, to replace a shell pipeline that already works.

## Builds run in the cluster, rootless

A build is a Kubernetes `Job` in the build namespace, not a local `docker
build`. kelson never needs a Docker daemon, and the machine running the CLI
never needs to be able to build the image.

The Job has two steps, whichever strategy runs:

1. an **init container** clones the source at the resolved commit, depth 1, into
   an `emptyDir` workspace;
2. the **build container** builds it and pushes:
   - **dockerfile** runs [BuildKit](https://github.com/moby/buildkit)'s
     `*-rootless` image — `buildkitd` in the background, then `buildctl build
     --frontend dockerfile.v0`;
   - **buildpacks** runs a [Cloud Native Buildpacks](https://buildpacks.io)
     builder image and its `/cnb/lifecycle/creator`, which detects the app's
     language from the workspace, picks the buildpacks and exports the image.

**No privileged container is involved, and that is a tested property, not an
intention.** `TestWorkloadBuildIsNotPrivileged` and
`TestWorkloadLifecycleIsNotPrivileged` assert `runAsNonRoot: true`,
`allowPrivilegeEscalation: false`, `capabilities.drop: [ALL]`, and no
`CAP_SYS_ADMIN` anywhere in the rendered manifest. Running privileged builders
in a shared cluster is what issue #48 rules out, so the rootless posture is the
acceptance criterion rather than a configuration choice, and it is the same
criterion for both drivers.

Cloning inside the pod rather than uploading a context keeps the control plane
out of the data path: kelson never streams a source tree through itself, and a
large repository costs the build pod's bandwidth instead of the server's.

The Job is deleted when the build finishes, in every outcome. Success, failure
and timeout are classified apart — a build that exceeded
`activeDeadlineSeconds` reports as a timeout, not as a generic failure.

**One executor serves both drivers.** Nothing in it is strategy-specific: it
submits the Job, streams whichever container the Job declares, and reads what
was pushed from the Job's own `kelson.dev/image` and `kelson.dev/tag`
annotations rather than out of a builder's command line. buildctl spells the
destination `--output name=…` and the lifecycle spells it `-image …`; an
executor that scraped either would have needed a second parser for the second
strategy.

## The destination: `--registry` and one image per Project

Nothing in the spec says where an image is pushed, and that is on purpose. The
registry is infrastructure configuration, not application description: the same
Project must build against a team's `ghcr.io/acme` and against a kind cluster's
`localhost:5000`. ADR-0010 already keeps build configuration out of the spec;
this is the same boundary from the registry side.

So the destination arrives as a flag:

| | |
|---|---|
| `--registry ghcr.io/acme` | required; `KELSON_REGISTRY` supplies the default |
| repository | `<registry>/<project>` — e.g. `ghcr.io/acme/shop` |
| tag | `<project>-<project>-<short revision>` — e.g. `shop-shop-0123456789ab` |
| reference | `<repository>@sha256:…` — what the command prints |

**One repository per Project, not per Component.** The source is
project-level (`spec.source`), so the build is too. Model rule P3 resolves a
Component's image to its own `image:` if it has one and to the Project's
otherwise, which means one built image feeds every component that does not
name one. One build, one repository, one digest pinned into all of them.

The tag repeats the project name because `registry.Tag` takes
`(project, component, revision)` and a project-level build has no single
component to put in the middle slot. Naming the first component there would
read as "this image belongs to `web`", which is exactly what it does not mean;
repeating the project is redundant but true. Nothing depends on the tag —
reproducibility comes from the digest, and the tag exists so a human reading a
registry listing can tell what they are looking at.

**The result is always digest-pinned.** The executor scrapes the pushed digest
out of the build's output and returns `repository@sha256:…`; the command
refuses a result that is not pinned. A mutable tag in a rendered manifest would
break the guarantee that the same commit deploys the same thing, which is the
one thing the build exists to provide (issue #51).

For BuildKit that digest is on its own push line. The lifecycle does not print
one — it announces `*** Images (<id>)` in prose and states the digest as data
only in `report.toml`, inside a pod that is deleted when the Job finishes — so
the buildpacks build reads its own report and echoes
`kelson: pushed <repository>@sha256:…` as its last line. A report with no
digest fails the build rather than reporting success with nothing to deploy.

## Plain-HTTP registries

A local registry has no TLS: kind's `localhost:5000`, or an in-cluster registry
reached by its Service name. Neither builder will talk plain HTTP to a host it
was not told about, and neither should — silently downgrading because a TLS
handshake failed is how a push credential ends up on the wire in clear.

So the exception is named, per host, by whoever runs the build:

```sh
kelson build -f project.yaml --registry localhost:5000 \
  --insecure-registries localhost:5000
```

| | |
|---|---|
| `--insecure-registries host[:port],…` | CLI and server; `KELSON_INSECURE_REGISTRIES` supplies the default for both |
| `server.insecureRegistries: [localhost:5000]` | the Helm value that renders the server's flag |

Only the listed hosts are affected. A registry that was not named is reached
exactly as before, and an entry carrying a scheme (`http://…`) or a repository
path is refused at parse time — it would match no image, and the push would
then fail on TLS with nothing pointing at the typo.

It is an operator knob and never a request field. A caller who could name a
registry insecure could make the server push a credential in clear to a host of
their choosing.

The mechanism differs per driver, because the two tools disagree about what
"insecure" means:

- **BuildKit** gets a generated `buildkitd` config, `[registry."host"] http =
  true`, loaded with `--config`. `http = true` is deliberately *not* paired with
  `insecure = true`: buildkit's resolver forces HTTPS for a registry marked
  insecure and cannot fall back, so the pair breaks the very registry this
  exists to reach ([moby/buildkit#5872](https://github.com/moby/buildkit/issues/5872)).
  The exporter's own `registry.insecure=true` is added as well, but only when
  the *destination* was listed; it covers a registry serving TLS the build
  cannot verify and does not decide the scheme.
- **The CNB lifecycle** gets `CNB_INSECURE_REGISTRIES`, comma-separated — the
  input the [platform spec](https://github.com/buildpacks/spec/blob/main/platform.md)
  defines for `creator` (`-insecure-registry`, `CNB_INSECURE_REGISTRIES`), which
  the lifecycle splits on commas. kelson passes the variable rather than the
  flags because the input first exists in lifecycle **v0.18.0**: an older
  lifecycle exits on an unknown flag, but ignores a variable it does not read,
  which fails as "the push could not use TLS" instead of "flag provided but not
  defined". The default builder is an unpinned Paketo tag, so which lifecycle a
  build actually gets is Paketo's to say, not kelson's.

## Push credentials

`--push-secret <name>` names an **existing**
`kubernetes.io/dockerconfigjson` Secret in the build namespace. kelson does not
create it, does not read it, and never writes a credential into a manifest —
credentials are references, never values ([ADR-0009](adr/0009-secrets.md)).

The mechanism is a file mount, because both builders authenticate a *push*
through the Docker CLI's config file, not through a Kubernetes
`imagePullSecret`. An `imagePullSecret` on the build pod would do nothing here.
So the Secret's `.dockerconfigjson` key is projected as `config.json` and
`DOCKER_CONFIG` points the builder at that directory — `/home/user/.docker`
for the rootless BuildKit image, `/home/cnb/.docker` for a CNB builder, each
being that image's own home. The clone init container sees neither the mount
nor the variable.

The projection is read-only in both cases. Its mode differs: `0400` under
BuildKit, `0444` under the lifecycle, because a Secret volume's files are owned
by root and the lifecycle runs as uid 1000, so a `0400` projection would be
unreadable by the process that needs it.

Creating the Secret is out of scope, and one command already does it:

```sh
kubectl create secret docker-registry ghcr-push \
  --namespace shop-production \
  --docker-server=ghcr.io \
  --docker-username="$GITHUB_USER" \
  --docker-password="$GITHUB_TOKEN"
```

Omitting `--push-secret` means an unauthenticated push. That is correct for a
cluster-internal registry and fails at push time for anything else.

## Build-time secrets

A credential the *build itself* needs — a private npm token, a corporate CA —
goes through a **BuildKit secret mount**, never a build argument and never an
image layer ([ADR-0009](adr/0009-secrets.md), issue #117):

```dockerfile
RUN --mount=type=secret,id=npm-token \
    npm config set //registry.npmjs.org/:_authToken="$(cat /run/secrets/npm-token)" && npm ci
```

The executor projects one key of an existing Kubernetes Secret per mount
(`buildkit.Config.Secrets`), read-only and mode `0400`, and points `buildctl` at
it with `--secret id=…,src=…`. Only the Secret's *name* enters the rendered Job:
kelson never holds the value, so it cannot leak it. The mount exists for the
duration of one `RUN` on a tmpfs and is not committed to a layer.

A **build argument** whose name looks like a credential (`NPM_TOKEN`,
`DB_PASSWORD`, `…_API_KEY`) is refused rather than redacted. A build arg is
recorded in the image's own history and in the Job's command line, so by the
time output could be cleaned up the value is already in the pushed image,
readable by anyone who can pull it. This is the documented gap in Coolify that
ADR-0009 names.

There is no spec field or CLI flag for build secrets yet — the reference model
is [#79](https://github.com/dafrie/kelson/issues/79). What exists today is the
executor-level mount, so the safe path is already the only path when it lands.

## Strategy selection

Precedence is [ADR-0010](adr/0010-build-strategy.md)'s, unchanged:

1. an explicit `spec.build.strategy` always wins — `none` means "use `image:`,
   build nothing";
2. a `Dockerfile` in the source (or at `spec.build.dockerfile`) selects
   **dockerfile**;
3. otherwise **buildpacks**.

Both build ([#48], [#49]). The resolved strategy is what selects the driver, and
it is resolved once, by the function `kelson build` and `BuildService` share, so
the two cannot disagree about what a spec selects.

**`none`** is the one answer that is a refusal: the spec asked for nothing to be
built, and the reason is `build/nothing-to-build`.

### Buildpacks

No Dockerfile, no build configuration, an image anyway — the zero-config default
ADR-0010 chose. The lifecycle reads the workspace, picks the buildpacks that
match the language, and reports what it chose in output the build streams back.
kelson supplies the source and the parameters and re-implements none of that
detection.

The builder image, the run image and any extra buildpacks are **driver
configuration**, not spec: `buildpacks.Config` carries them, defaulting to
Paketo's `builder-jammy-base` / `run-jammy-base` pair. "Configure buildpacks"
means setting that config, never teaching the kelson spec a builder DSL —
ADR-0010 forbids the DSL, and `spec.build` has exactly two fields for that
reason.

`rebase` — patching a built image onto a new run image without rebuilding the
application — exists on the driver (`Driver.Rebase`, digest-pinned on both
sides, failing closed without an injected rebaser) and has no CLI or RPC yet.
Neither surface injects a rebaser, because a rebaser needs an OCI registry
client that the build plane's lint allow-list forbids and neither command has a
rebase verb to hang it on. It is also not a complete CVE story: rebase replaces
compatible run-image layers, and a vulnerable dependency in the application's
own layers still needs a rebuild.

There is no cache, so every buildpacks build is cold — including the
lifecycle's own `analyze`/`restore` reuse, which needs a previous app image the
driver does not point it at ([#52], [ADR-0011](adr/0011-build-cache.md)).

### `auto` needs a local checkout

Detection reads the source tree. For a remote repository the CLI has no tree to
read — the tree only exists inside the build pod, after the clone. So:

```sh
# auto-detection over a local checkout
kelson build -f project.yaml --registry ghcr.io/acme -C .

# no checkout needed: the spec names the strategy
#   spec.build.strategy: dockerfile
kelson build -f project.yaml --registry ghcr.io/acme
```

Without `-C` and without an explicit strategy, `auto` fails with reason
`build/detection-needs-source` and says which of the two fixes to apply.
Guessing "probably a Dockerfile" would be exactly the magic ADR-0010 exists to
avoid. Detecting inside the cluster before choosing a driver is tracked by
[#50].

`-C` is used **only** for detection. The build still clones the remote
repository at the resolved commit; a dirty local checkout does not reach the
image.

## Revisions

`--ref` takes a branch, a tag or a commit; without it, `spec.source.ref` is
used, and without that, the repository's default branch.

Anything that is not already a 40-character commit is resolved once, up front,
with the `git ls-remote` question — no clone, no working tree. A branch wins
over a tag of the same name (git's own precedence) and an annotated tag
resolves to the commit it points at, not to the tag object.

Resolving once is the point: the image tag, the recorded
`build.Request.Revision`, the `kelson.dev/revision` annotation on the Job and
the commit the build pod checks out are then all the same string. A branch
resolved separately by each of them could disagree, and "built from main" is
not a record of anything.

Reading the source repository reuses the delivery credential
(`KELSON_GIT_TOKEN`); a public repository needs none.

## Command surface

```
kelson build -f spec.yaml [--env <name>] --registry <prefix>
             [--push-secret <name>] [--insecure-registries <hosts>]
             [--ref <git-ref>] [-C <dir>]
             [--namespace <ns>] [--kubeconfig <path>] [--timeout 30m]
```

| Flag | Meaning |
|---|---|
| `-f`, `--file` | spec documents, repeatable (required) |
| `--env` | which Environment; optional when the input holds exactly one |
| `--registry` | destination registry and namespace; `KELSON_REGISTRY` is the default |
| `--push-secret` | existing dockerconfigjson Secret authenticating the push |
| `--insecure-registries` | comma-separated hosts served over plain HTTP; `KELSON_INSECURE_REGISTRIES` is the default |
| `--ref` | branch, tag or commit to build |
| `-C`, `--source-dir` | local checkout, used only to detect the strategy |
| `--namespace` | namespace for the build Job; defaults to the environment's namespace |
| `--kubeconfig` | cluster credentials; the usual precedence chain |
| `--timeout` | budget for clone, build and push; default 30m |

**Output.** The build log streams to stdout as it is produced. The plan (chosen
strategy and why, source and resolved commit, destination) and the deploy hint
go to stderr, so the last line of stdout is the digest-pinned reference and
nothing else. A failed build exits 1 with the executor's classified error.

A build needs no `--profile` and does no rendering: it reads `spec.source`,
`spec.build` and the Environment's identity, and nothing a ClusterProfile
decides.

## The same build over the API

`kelson-server` serves the identical pipeline as `BuildService.Build`
([ADR-0013](adr/0013-server-state-and-api-v0.md), issue [#54]), because a
browser has no kubeconfig and the UI's create-from-a-git-repository path
([#63]) has to reach the build plane somehow.

The stream is three events: `Started` once the strategy and the ref are
resolved to settled facts (strategy, image repository, tag, revision), `Log`
chunks carrying the build's raw output while the Job runs, and `Finished` with
the digest-pinned reference. A failed build is a ConnectRPC error with the same
`build/*` codes the CLI prints — it produced no image, so there is no result
message that could honestly describe one.

The destination is the server's configuration rather than each caller's:

| Flag | Meaning |
|---|---|
| `--registry` | default destination prefix; `KELSON_REGISTRY` supplies it |
| `--push-secret` | default push credential, by name |
| `--build-namespace` | where build Jobs run; empty keeps the environment's own namespace, as in the CLI |
| `--insecure-registries` | hosts served over plain HTTP; `KELSON_INSECURE_REGISTRIES`, or `server.insecureRegistries` in the chart |

A request may override the registry and the push secret, because where an image
is pushed is not application description. It may **not** override the strategy:
that is the spec's, and a per-request override would put build configuration in
the caller's hands, which is what ADR-0010 forbids. There is no
`idempotency_key` either — a build is named from the resolved commit, so
re-running one is already a replay.

Two limits are the server's own and are documented on the RPC rather than
discovered at runtime:

- **`auto` cannot be detected server-side at all.** Detection reads a source
  tree, the server has none, and there is no server-side equivalent of `-C`. A
  spec that leaves the strategy to `auto` fails with
  `build/detection-needs-source` and says to set `spec.build.strategy`. This is
  why the UI's create form offers `dockerfile` and not `auto` ([#50] is the fix).
- **Cancelling the stream does not cancel the build.** It stops the server
  watching. The executor returns as soon as its context is done, before the
  branch that deletes the Job, so the Job keeps running, finishes or hits its
  `activeDeadlineSeconds`, and is left in the build namespace for an operator to
  find. "I cancelled the build" and "the build stopped" are different facts.

## Structure

| Package | Role |
|---|---|
| `internal/build` | the `Request` / `Result` / `Builder` contract, the workload annotations and the shared clone script, plus the pure build plan (strategy resolution, destination tag, ref classification) both callers share |
| `internal/build/buildkit` | the Dockerfile driver; `Config.Workload` is a **pure** `(Request, Config) → Job YAML` |
| `internal/build/buildpacks` | the buildpacks driver, the same shape, plus `Rebase` behind a `Rebaser` seam |
| `internal/build/detect` | strategy detection over an `fs.FS`, with a typed reason and the evidence path |
| `internal/build/registry` | reference parsing, digest pinning, tag and destination derivation, credential references, the insecure-registry list |
| `internal/delivery/kube` | `BuildExecutor`: submits the Job, streams pod logs, classifies the outcome, parses the digest — for either driver |
| `internal/gitref` | `RemoteResolver`: `ls-remote` ref resolution (it lived in `internal/delivery/git` until ADR-0028 deleted the writer around it) |
| `cmd/kelson` | `kelson build`: flags, spec, and the `buildConnector` seam, where the resolved strategy picks a driver |
| `internal/api` | `BuildService.Build`: the same plan over the wire, behind the `BuildConnector` seam |

The split is the depguard allow-lists (`.golangci.yml`), not taste.
`internal/build` may not import client-go, so the cluster-facing half of each
driver lives behind its `Cluster` interface and is implemented in
`internal/delivery/kube`; one `BuildExecutor` satisfies both structurally.
`cmd/kelson` may import neither client-go nor go-git, so it consumes both
through `buildConnector` — which is also why every test of the command runs
with no cluster and no network. `internal/api` may not import client-go either,
which is why the resolved strategy travels to the connector on `BuildTarget`
and the driver is constructed in `cmd/kelson-server`.

## Not here yet

- **In-cluster strategy detection** — [#50]; today `auto` needs `-C`, and a
  server has no equivalent.
- **Build caching** — [#52], [ADR-0011](adr/0011-build-cache.md). Every build
  is cold, under either strategy.
- **`rebase` as a command** — the driver has it, no CLI or RPC calls it, and
  nothing injects a rebaser. See above.
- **Multi-platform builds** — `build.Request.Platforms` reaches `buildctl`, but
  no flag sets it, and the lifecycle is not given it at all.
- **Build-time secrets under buildpacks** — `buildkit.Config.Secrets` mounts
  them for a Dockerfile build; the lifecycle has no equivalent wired here.
- **Live verification** — the build path has no end-to-end coverage yet; it
  belongs to the e2e harness ([#86]), which is where a real registry and a real
  cluster exist. See [docs/e2e.md](e2e.md).

[#48]: https://github.com/dafrie/kelson/issues/48
[#49]: https://github.com/dafrie/kelson/issues/49
[#50]: https://github.com/dafrie/kelson/issues/50
[#51]: https://github.com/dafrie/kelson/issues/51
[#52]: https://github.com/dafrie/kelson/issues/52
[#54]: https://github.com/dafrie/kelson/issues/54
[#63]: https://github.com/dafrie/kelson/issues/63
[#86]: https://github.com/dafrie/kelson/issues/86
