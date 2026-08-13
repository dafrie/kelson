# Build plane — how kelson turns source into an image

Reference for `kelson build` (M4). Implements issues [#48] and [#51]; the
strategy decision is [ADR-0010](adr/0010-build-strategy.md).

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

The Job has two steps:

1. an **init container** clones the source at the resolved commit, depth 1, into
   an `emptyDir` workspace;
2. the **build container** runs [BuildKit](https://github.com/moby/buildkit)'s
   `*-rootless` image: `buildkitd` in the background, then `buildctl build
   --frontend dockerfile.v0`, pushing the result.

**No privileged container is involved, and that is a tested property, not an
intention.** `TestWorkloadBuildIsNotPrivileged` asserts `runAsNonRoot: true`,
`allowPrivilegeEscalation: false`, `capabilities.drop: [ALL]`, and no
`CAP_SYS_ADMIN` anywhere in the rendered manifest. Running privileged builders
in a shared cluster is what issue #48 rules out, so the rootless posture is the
acceptance criterion rather than a configuration choice.

Cloning inside the pod rather than uploading a context keeps the control plane
out of the data path: kelson never streams a source tree through itself, and a
large repository costs the build pod's bandwidth instead of the server's.

The Job is deleted when the build finishes, in every outcome. Success, failure
and timeout are classified apart — a build that exceeded
`activeDeadlineSeconds` reports as a timeout, not as a generic failure.

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

**One repository per Project, not per Application.** The source is
project-level (`spec.source`), so the build is too. Model rule P3 resolves an
Application's image to its own `image:` if it has one and to the Project's
otherwise, which means one built image feeds every application that does not
name one. One build, one repository, one digest pinned into all of them.

The tag repeats the project name because `registry.Tag` takes
`(project, application, revision)` and a project-level build has no single
application to put in the middle slot. Naming the first application there would
read as "this image belongs to `web`", which is exactly what it does not mean;
repeating the project is redundant but true. Nothing depends on the tag —
reproducibility comes from the digest, and the tag exists so a human reading a
registry listing can tell what they are looking at.

**The result is always digest-pinned.** The executor scrapes the pushed digest
out of BuildKit's push line and returns `repository@sha256:…`; the command
refuses a result that is not pinned. A mutable tag in a rendered manifest would
break the guarantee that the same commit deploys the same thing, which is the
one thing the build exists to provide (issue #51).

## Push credentials

`--push-secret <name>` names an **existing**
`kubernetes.io/dockerconfigjson` Secret in the build namespace. kelson does not
create it, does not read it, and never writes a credential into a manifest —
credentials are references, never values ([ADR-0009](adr/0009-secrets.md)).

The mechanism is a file mount, because `buildctl` authenticates a *push*
through the Docker CLI's config file, not through a Kubernetes
`imagePullSecret`. An `imagePullSecret` on the build pod would do nothing here.
So the Secret's `.dockerconfigjson` key is projected as `config.json` at
`/home/user/.docker` (mode `0400`, read-only) and `DOCKER_CONFIG` points
`buildctl` at that directory. The clone init container sees neither the mount
nor the variable.

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

## Strategy selection

Precedence is [ADR-0010](adr/0010-build-strategy.md)'s, unchanged:

1. an explicit `spec.build.strategy` always wins — `none` means "use `image:`,
   build nothing";
2. a `Dockerfile` in the source (or at `spec.build.dockerfile`) selects
   **dockerfile**;
3. otherwise **buildpacks**.

Two of those three answers are refusals in this release, and both say so:

- **`buildpacks`** is deferred out of the v0.1 cut ([#49]). ADR-0010 still makes
  it the eventual *default* — the deferral is of the implementation, not of the
  decision. Resolving to it fails with reason `build/strategy-not-implemented`.
- **`none`** means the spec asked for nothing to be built. Reason
  `build/nothing-to-build`.

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
             [--push-secret <name>] [--ref <git-ref>] [-C <dir>]
             [--namespace <ns>] [--kubeconfig <path>] [--timeout 30m]
```

| Flag | Meaning |
|---|---|
| `-f`, `--file` | spec documents, repeatable (required) |
| `--env` | which Environment; optional when the input holds exactly one |
| `--registry` | destination registry and namespace; `KELSON_REGISTRY` is the default |
| `--push-secret` | existing dockerconfigjson Secret authenticating the push |
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

## Structure

| Package | Role |
|---|---|
| `internal/build` | the `Request` / `Result` / `Builder` contract |
| `internal/build/buildkit` | the Dockerfile driver; `Config.Workload` is a **pure** `(Request, Config) → Job YAML` |
| `internal/build/detect` | strategy detection over an `fs.FS`, with a typed reason and the evidence path |
| `internal/build/registry` | reference parsing, digest pinning, tag and destination derivation, credential references |
| `internal/delivery/kube` | `BuildExecutor`: submits the Job, streams pod logs, classifies the outcome, parses the digest |
| `internal/delivery/git` | `RemoteResolver`: `ls-remote` ref resolution |
| `cmd/kelson` | `kelson build`: flags, spec, and the `buildConnector` seam |

The split is the depguard allow-lists (`.golangci.yml`), not taste.
`internal/build` may not import client-go, so the cluster-facing half of the
driver lives behind the `buildkit.Cluster` interface and is implemented in
`internal/delivery/kube`. `cmd/kelson` may import neither client-go nor go-git,
so it consumes both through `buildConnector` — which is also why every test of
the command runs with no cluster and no network.

## Not here yet

- **Buildpacks** — [#49], deferred from v0.1, see above.
- **In-cluster strategy detection** — [#50]; today `auto` needs `-C`.
- **Build caching** — [#52], [ADR-0011](adr/0011-build-cache.md). Every build
  is cold.
- **Multi-platform builds** — `build.Request.Platforms` reaches `buildctl`, but
  no flag sets it.
- **Live verification** — the build path has no end-to-end coverage yet; it
  belongs to the e2e harness ([#86]), which is where a real registry and a real
  cluster exist. See [docs/e2e.md](e2e.md).

[#48]: https://github.com/dafrie/kelson/issues/48
[#49]: https://github.com/dafrie/kelson/issues/49
[#50]: https://github.com/dafrie/kelson/issues/50
[#51]: https://github.com/dafrie/kelson/issues/51
[#52]: https://github.com/dafrie/kelson/issues/52
[#86]: https://github.com/dafrie/kelson/issues/86
