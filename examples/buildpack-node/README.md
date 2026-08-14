# buildpack-node — a repository with no Dockerfile

The zero-config path of [ADR-0010](../../docs/adr/0010-build-strategy.md): no
Dockerfile, no build configuration, and an image anyway. `server.js` and
`package.json` are the whole application; the Cloud Native Buildpacks lifecycle
reads `package.json`, picks the Node.js buildpacks and builds it.

```
project.yaml        the Project — spec.build.strategy: buildpacks
development.yaml    one Environment
package.json        what the lifecycle detects the app from
server.js           the app: hello world, plus /healthz
```

## Build it

`spec.source.git` is a placeholder, because the build clones the *source
repository* — a local copy is never what reaches the image. Push this directory
as a repository of its own and point the spec at it, then:

```sh
kelson build -f examples/buildpack-node/project.yaml --registry ghcr.io/acme
```

Nothing on the command line says "buildpacks": the spec does, and `--registry`
says only where the result goes ([docs/build.md](../../docs/build.md)). Against
a local registry with no TLS, name it:

```sh
kelson build -f examples/buildpack-node/project.yaml \
  --registry localhost:5000 --insecure-registries localhost:5000
```

The last line of stdout is the digest-pinned reference, which is what `deploy`
takes:

```sh
IMAGE=$(kelson build -f examples/buildpack-node/project.yaml --registry ghcr.io/acme | tail -1)
kelson deploy -f examples/buildpack-node/project.yaml -f examples/buildpack-node/development.yaml \
  --env development --image "$IMAGE"
```

## Why the strategy is written down

`strategy: auto` reaches the same answer — no Dockerfile, a language signal,
therefore buildpacks — but only where kelson can read the tree, which means the
CLI with `-C .`. A server has no checkout, so `auto` is refused there with
`build/detection-needs-source` until in-cluster detection lands
([#50](https://github.com/dafrie/kelson/issues/50)). Naming the strategy makes
this spec buildable from both.

## Rendering it without building

Rendering needs an image, and this Project has none until a build produces one,
so `kelson render` refuses with `image/unresolved` and says so. Supply one and
it renders:

```sh
kelson render -f examples/buildpack-node/project.yaml -f examples/buildpack-node/development.yaml \
  --env development --image ghcr.io/acme/greeter@sha256:…
```
