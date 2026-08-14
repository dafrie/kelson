# Local kelson on kind

One command stands up the whole thing on your machine: a [kind](https://kind.sigs.k8s.io)
cluster, an in-cluster image registry the build plane can push to, and a
Helm-installed `kelson-server` with the web UI baked in.

```sh
make kind-up
```

Then, using the two lines the script prints at the end:

```sh
export KUBECONFIG=hack/bin/local.kubeconfig
kubectl -n kelson-system port-forward svc/kelson 8420:8420
```

Open <http://127.0.0.1:8420> and log in with the password the script printed.
That is the full product: create a project, deploy it, watch the rollout,
read logs, roll back — all from the browser, all against the cluster on your
machine.

Tear it down with `make kind-down`. Re-running `make kind-up` is safe at any
time: it converges, and it rebuilds the server image from your checkout and
redeploys it, so it doubles as the "see my change in the real UI" loop.

## Prerequisites

- Docker (kind runs the cluster in it)
- `kubectl` and `helm`
- Node 22+ (builds the web UI that gets embedded into the server)
- Go (builds `kelson-server`; the pinned `kind` binary installs itself into
  `hack/bin/`)

## Your first project

The quickest win needs no build at all: **New project → From a container
image**, give it `docker.io/traefik/whoami:v1.12.0` and port 80, deploy, and
watch it turn healthy.

The real flow is **From a Git repository**. The form asks which strategy
builds the image:

- **Dockerfile** — the repository has one at its root.
- **Buildpacks** — it does not; the Cloud Native Buildpacks lifecycle detects
  the language and builds a rootless image with no build config in the repo
  ([build](build.md)).

The repository URL must be one the *cluster* can clone — the build runs in a
Job, not on your machine — so a public GitHub URL is the easy case.
[`examples/buildpack-node`](https://github.com/dafrie/kelson/tree/main/examples/buildpack-node)
is a ready-made Dockerfile-less app: push a copy of that directory to a
repository of your own and point the form at it. Builds push to the in-cluster
registry and the deployment pulls from it; nothing leaves your machine.

## What `make kind-up` actually did

Four things, each visible with ordinary kubectl:

1. **A kind cluster** named `kelson-local` — deliberately separate from the
   e2e harness's `kelson-e2e`, which tests delete freely.
2. **A registry inside the cluster** (`kelson-registry` Deployment + NodePort
   Service in `kelson-system`, plain HTTP). One name works from every vantage
   point: build pods push to
   `kelson-registry.kelson-system.svc.cluster.local:5000` through cluster DNS,
   and each node's containerd pulls from the same name through a `hosts.toml`
   mapping it onto the NodePort on loopback — written by the script, because
   containerd cannot resolve Service DNS itself. Registry storage is an
   `emptyDir`: images vanish if the registry pod restarts, and a rebuild is one
   `kelson build` away.
3. **A dev image** — `make ui` builds the web UI into the server's embed
   directory ([server](server.md)), a static linux `kelson-server` is compiled
   and wrapped in the `FROM scratch` Dockerfile as `kelson-server:dev`, and
   `kind load` places it on the node. No registry involved for the server
   itself.
4. **The Helm chart** ([install](install.md)), with the local wiring passed as
   values: `image.pullPolicy=Never` (the image is already on the node),
   `auth.existingSecret.name=kelson-auth` (generated once, printed each run),
   `server.registry` pointing builds at the in-cluster registry,
   `server.insecureRegistries` naming it — the build plane refuses plain HTTP
   to any host it was not explicitly told about — and
   `rbac.createDeployClusterRole=true`, the cluster-wide delivery grant the
   chart leaves off by default. The UI deploys into a namespace that does not
   exist until the first apply creates it, so without that value the first
   deploy fails on `cannot patch resource "namespaces"`
   ([server](server.md#the-delivery-grant-is-opt-in-and-cluster-wide)). Here it
   is right; on a cluster you share with anything else, read that section
   first.

## Honest limits

- **No TLS anywhere.** The password crosses loopback only (port-forward), and
  the registry speaks HTTP inside the cluster. Both are the local trade; the
  production posture is in [server](server.md).
- **The registry forgets on restart** (emptyDir). Deployed pods keep running —
  the image is on the node — but a new rollout of an old pin would need a
  rebuild.
- **PR previews and Git-backed environments** need more than this script sets
  up (a deployment repository, a Flux install — [delivery](delivery.md));
  the direct-apply path is what works out of the box.
- **The server can write to every namespace of this cluster.** That is
  `rbac.createDeployClusterRole=true` above, and it is the honest cost of
  "click deploy and it works": anything that can reach the server with the
  password can apply anywhere in `kelson-local`. Fine for a cluster that
  `make kind-down` deletes; a decision to make deliberately anywhere else.
