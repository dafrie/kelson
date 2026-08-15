# kelson Helm chart

Installs `kelson-server` — the API the CLI, the web UI and the MCP surface talk to
([docs/server.md](../../../docs/server.md)) — and the RBAC it needs.

```sh
kubectl create namespace kelson-system
kubectl -n kelson-system create secret generic kelson-auth \
  --from-literal=password="$(openssl rand -base64 24)"

helm install kelson ./deploy/chart/kelson \
  --namespace kelson-system \
  --set image.tag=0.0.1 \
  --set auth.existingSecret.name=kelson-auth
```

Two values have no default and the chart refuses to render without them. That is the whole
design of this chart in one sentence, so both are spelled out below.

## Port-forward the Service and you have the UI in a browser

`kelson-server` carries the web UI inside its own binary and serves it from the same port as the
API ([docs/server.md](../../../docs/server.md)), so there is nothing else to install and nothing
else to expose:

```sh
kubectl -n kelson-system port-forward svc/kelson 8420:8420
# then open http://127.0.0.1:8420 and log in with the shared password
```

The UI is served without authentication and the login happens inside it — the assets are public,
the cluster is not. The published `ghcr.io/dafrie/kelson-server` images carry the real UI; an image
you built yourself from a checkout that never ran `make ui` serves a placeholder page saying so,
and the server says the same on startup.

## Authentication is required, or refused out loud

In a cluster the server binds `0.0.0.0` — a Service cannot reach a process listening on
loopback. `kelson-server` itself refuses that bind when no password is set, and tells you the
three ways out ([ADR-0013](../../../docs/adr/0013-server-state-and-api-v0.md) §3). The chart
makes the same refusal at render time, before anything reaches the cluster:

| Value | Posture |
|---|---|
| `auth.existingSecret.name` | **Preferred.** The chart references a Secret you made and never sees the value, so the password is not in your values file, the Helm release record or `helm get values` output. |
| `auth.password` | The lesser option. The chart creates the Secret from a literal, which means the value is in the release record and in whatever shell history the `--set` came from. It still reaches the pod as a `secretKeyRef`, never as a plain env value. |
| `auth.insecure=true` | Serve with no authentication, deliberately. Renders `--insecure-bind`. Anything that can reach the Service can deploy to your cluster. |

Setting none of them fails the render with a message naming all three. Setting `auth.insecure`
alongside a password fails too: `--insecure-bind` means *serve with no authentication*, and with
a password it is neither true nor needed.

**There is no TLS in kelson-server.** A password sent in clear is a password anyone on the path
has. Reach the Service with `kubectl port-forward`, or put a TLS-terminating proxy in front of
it — behind a proxy that sets `X-Forwarded-Proto: https` the session cookie is marked `Secure`
automatically. The real answer — per-project identity, OIDC, TLS — is
[#84](https://github.com/dafrie/kelson/issues/84).

## `image.tag` has no default

Not `latest`, because a mutable tag makes a Deployment's identity unknowable: two pods of the
"same" release can be different builds. Not the chart's `appVersion` either, because that tracks
`internal/version` and is a development placeholder until a release is cut. Pass the tag you
mean. Publishing the images is release work (`.goreleaser.yml`,
[docs/release-policy.md](../../../docs/release-policy.md)), not this chart's.

## What the chart deliberately does not do

- **No CRDs.** kelson's model lives in ConfigMaps, not in custom resources
  ([ADR-0013](../../../docs/adr/0013-server-state-and-api-v0.md) §1), so the chart has no CRD
  lifecycle problem to solve — the upgrade pain that question usually leads to does not exist
  here. It is also the concrete form of [ADR-0003](../../../docs/adr/0003-install-model.md):
  kelson adopts a cluster, it does not extend its API surface.
- **No Ingress and no HTTPRoute.** The server terminates no TLS, and how you expose it is your
  cluster's routing decision, not the installer's. Port-forward, or route to the `ClusterIP`
  Service yourself.
- **No cert-manager, no external-secrets, no Flux, no CloudNativePG, no ingress controller.**
  Never install what is already there ([ADR-0003](../../../docs/adr/0003-install-model.md)).
  Installing components that are genuinely missing is a separate, opt-in story —
  [#60](https://github.com/dafrie/kelson/issues/60).
- **No observability objects.** No ServiceMonitor, no PodMonitor, no dashboards. Adding one
  would assume a Prometheus stack the profile is supposed to detect.
- **No write access to your workloads, until you ask for it.** A default install cannot apply a
  rendered spec anywhere. `rbac.createDeployClusterRole=true` grants that, cluster-wide, and a
  server you expect to deploy needs it — see RBAC below.

## What it creates

| Kind | Scope | Why |
|---|---|---|
| `Namespace` | cluster | Only when `namespace.create=true`. |
| `ServiceAccount` | release ns | The identity everything below binds to. |
| `ClusterRole`/`ClusterRoleBinding` `<name>-detect` | cluster | The read-only detection grant, a verbatim copy of `deploy/rbac/detect-clusterrole.yaml`. Get and list only, no write verb anywhere ([#56](https://github.com/dafrie/kelson/issues/56)). |
| `ClusterRole`/`ClusterRoleBinding` `<name>-deploy` | cluster | **Only when `rbac.createDeployClusterRole=true`, and off by default.** The delivery grant: `get, list, create, patch, delete` on every kind the renderer emits, `get, list, create, patch` on `namespaces` (no `delete`), plus the managed-Secret writes and the pod, log and preview reads — in every namespace. See below. |
| `Role`/`RoleBinding` `<name>-state` | state ns | `configmaps: get, list, create, update, delete` — the spec and history stores (`internal/serverstate`). Plus `secrets: get, list, create, update` for the agent identity store ([#74](https://github.com/dafrie/kelson/issues/74)): one Secret per identity, holding a salted HMAC of the credential and never the credential. No `delete` — a revoked identity is kept so past actions stay attributable. |
| `Role`/`RoleBinding` `<name>-env` | state ns + `rbac.targetNamespaces` | Managed Secrets, the pod and deployment reads behind status and logs, and build Jobs. |
| `Role`/`RoleBinding` `<name>-build` | `server.buildNamespace` | Only when the build namespace is outside the served ones: Jobs, pod logs, and a read of the push Secret. Nothing writes Secrets there. |
| `Secret` | release ns | Only when `auth.password` is a literal. |
| `Deployment`, `Service` | release ns | The server, and a `ClusterIP` in front of it. |
| `ServiceAccount`, `ClusterRole`/`ClusterRoleBinding` `<name>-controller` | release ns / cluster | **Only when `controller.enabled=true`.** kelson-controller's identity and its cluster-scoped grant: `kelson.dev` Projects/Environments and their `status`/`finalizers` subresources, plus `coordination.k8s.io` Leases when `controller.leaderElection.enabled` (on by default). See [Configuring the controller](#configuring-kelson-controller). |
| `Role`/`RoleBinding` `<name>-controller-flux` | `controller.fluxNamespace` | **Only when `controller.enabled=true`.** CRUD on the `OCIRepository`/`Kustomization` pair the delivery spine owns (ADR-0028 decision 3). |
| `Deployment` `<name>-controller` | release ns | **Only when `controller.enabled=true`.** kelson-controller itself. |

Every rule exists because a named package makes that call. There are no wildcards, and no verb
is granted that nothing calls — `watch` is absent from the ConfigMap and Job grants because
neither the state stores nor the build executor watch (the executor polls).

### The delivery grant: `rbac.createDeployClusterRole`

A server that deploys needs `rbac.createDeployClusterRole=true`. Without it the first deploy from
the web UI fails like this:

```
not permitted to apply this resource: namespaces "podinfo-development" is forbidden:
User "system:serviceaccount:kelson-system:kelson" cannot patch resource "namespaces"
```

**And it cannot be fixed by binding a Role per namespace**, which is what this README used to say.
Two reasons, both structural:

1. The renderer emits the environment's `Namespace`
   ([#150](https://github.com/dafrie/kelson/issues/150)) and the direct adapter PATCHes it to
   record whether kelson created or adopted it. Namespaces are **cluster-scoped**; no namespaced
   Role can grant a verb on one, ever.
2. Creating an application in the UI targets a **new** namespace, `<project>-<environment>`. You
   cannot pre-bind a Role in a namespace that does not exist, so `rbac.targetNamespaces` cannot
   name it at install time. The same is true one click later for the reads behind the first
   status verdict and the first log stream, which is why the ClusterRole carries those too.

So the grant is cluster-scoped, and it is off by default because it is broad:

- It is write access to **every namespace in the cluster** — yours, kelson's, `kube-system`.
- It is bounded by the **server's** authentication and authorization, not by RBAC: the shared
  password, the per-identity agent scopes ([#74](https://github.com/dafrie/kelson/issues/74),
  [ADR-0024](../../../docs/adr/0024-agent-identities.md)) and the per-environment agent policy
  ([ADR-0025](../../../docs/adr/0025-agent-policy.md)). The cluster sees one service account with
  one grant and cannot tell one caller from another.
- Least-privilege for this path — what a per-project or per-environment identity would look like
  — is [#84](https://github.com/dafrie/kelson/issues/84)'s threat-model work. Until it lands,
  this value is the whole of the fence.

What it is not: not a wildcard (every apiGroup, resource and verb is named, and a test fails the
build if a `*` appears), and not a promise that any spec will apply — `spec.overlays` can emit any
kind at all, and one the ClusterRole does not name fails with a plain `forbidden`. Bind a
ClusterRole of your own alongside it for those.

Leave it `false` for a server used only to read status, preview diffs, or drive Git-backed
delivery, where Flux does the applying and kelson only writes to a repository.
`templates/clusterrole-deploy.yaml` carries the reasoning rule by rule, and
[docs/server.md](../../../docs/server.md) has the posture.

## Configuring kelson-controller

`controller.enabled=true` turns on `kelson-controller`, which reconciles `Project`/`Environment`
custom resources through the ADR-0028 delivery spine: render, push an OCI artifact, SSA-apply a
Flux `OCIRepository` + `Kustomization` pair, read the result back
([docs/delivery.md](../../../docs/delivery.md)). A registry is a hard requirement of that spine —
without `controller.registry` the controller reports `RegistryNotConfigured` on every `Environment`
and deploys nothing, rather than crash-looping:

```sh
helm upgrade kelson ./deploy/chart/kelson --namespace kelson-system --reuse-values \
  --set controller.enabled=true \
  --set controller.registry=ghcr.io/acme
```

Have no registry of your own? `kelson install registry` stands up an in-cluster one at
`kelson-registry.kelson-system.svc.cluster.local:5000` — point `controller.registry` at it and set
`controller.insecureRegistries` to match, since it serves plain HTTP
([docs/install.md](../../../docs/install.md#providing-a-registry-if-you-dont-have-one)).

| Value | Flag | What it does |
|---|---|---|
| `controller.registry` | `--registry` | Push prefix, e.g. `ghcr.io/acme`. Empty means `RegistryNotConfigured`. |
| `controller.pushSecret` | `--registry-config` | An existing `kubernetes.io/dockerconfigjson` Secret. Mounted into the pod (its `.dockerconfigjson` key projected onto `/etc/kelson/registry/config.json`) rather than read from the API, because the controller reads a file — the same one a CI `docker login` writes. Empty is an anonymous push. |
| `controller.pullSecret` | `--pull-secret` | A *different* dockerconfigjson Secret, in `controller.fluxNamespace`, that source-controller pulls with. Named on the `OCIRepository`, never mounted here — the controller pushes, source-controller pulls, and they are different processes. |
| `controller.insecureRegistries` | `--insecure-registries` | Hosts served over plain HTTP, e.g. `{localhost:5000}`. Exactly these and nothing else is downgraded from TLS. |
| `controller.fluxNamespace` | `--flux-namespace` | Where the `OCIRepository`/`Kustomization` pair lives. Empty means the release namespace, and the namespaced Role above (`<name>-controller-flux`) is created in the same place — the flag and the Role read one value and cannot disagree. |
| `controller.reconcileInterval` | `--reconcile-interval` | How often Flux re-checks the pair for drift. Empty means the binary's own default (5m); this is not deploy latency, a new revision reaches Flux through the apply. |

**Leader election is on by default** (`controller.leaderElection.enabled=true`), because the
controller now pushes artifacts and applies Flux objects, and two replicas racing to do either is a
real hazard rather than a harmless duplicate status write. A single replica always wins its own
Lease immediately, so this is what makes `controller.replicaCount: 2` a value change rather than a
two-step migration.

**No RBAC over workloads.** The ClusterRole and the flux Role both deliberately omit Pods and
Deployments: the `Kustomization` kelson writes carries `wait: true`, so `Ready` already means
*healthy* by the time the controller reads it back, and R1's exit gate needs nothing more.
Wiring `internal/observation`'s finer-grained classification into the reconcile loop — and the
read-only workload RBAC that would need — is tracked in
[#240](https://github.com/dafrie/kelson/issues/240), not forgotten.

## Uninstalling changes nothing about your apps

```sh
helm uninstall kelson --namespace kelson-system
```

Every object the chart creates also carries `kelson.dev/install: <release>`, so the claim is
checkable without trusting Helm's bookkeeping:

```sh
kubectl get all,secret,configmap,role,rolebinding,serviceaccount -A -l kelson.dev/install=kelson
kubectl get clusterrole,clusterrolebinding -l kelson.dev/install=kelson
```

That is the complete list of what goes away. Workloads kelson deployed are not in it: they carry
`kelson.dev/project` labels instead and keep running, which is the additivity claim
[#59](https://github.com/dafrie/kelson/issues/59) owns. The spec and history ConfigMaps are not
in it either — they are your data, they live in the state namespace, and `helm uninstall` does
not delete them.

## Values

| Key | Default | What it does |
|---|---|---|
| `image.repository` | `ghcr.io/dafrie/kelson-server` | Image to run. |
| `image.tag` | *(required)* | Release tag. No default — see above. |
| `image.pullPolicy` | `IfNotPresent` | |
| `imagePullSecrets` | `[]` | For a mirrored or private copy. |
| `auth.existingSecret.name` | `""` | Secret holding the shared password. Preferred. |
| `auth.existingSecret.key` | `password` | Key within it. |
| `auth.password` | `""` | The password as a literal. The lesser option. |
| `auth.insecure` | `false` | Serve with no authentication (`--insecure-bind`). |
| `server.port` | `8420` | Container port; the server listens on `0.0.0.0:<port>`. |
| `server.namespace` | release ns | `--namespace`: where the spec and history ConfigMaps live. |
| `server.keep` | `20` | `--keep`: deployment revisions retained per environment. |
| `server.registry` | `""` | `--registry`: destination registry for builds. |
| `server.pushSecret` | `""` | `--push-secret`: existing `dockerconfigjson` Secret. Not created here. |
| `server.artifactPushSecret` | `""` | `--registry-config`, mounted from an existing `dockerconfigjson` Secret: the credential the server publishes *preview artifacts* with (ADR-0034 decision 3). A different credential from `server.pushSecret`, read by a different process — that one is a Secret name handed to a build Job, this one is a file this process reads because it pushes the artifact itself. Not created here; empty is an anonymous push. |
| `server.buildNamespace` | `""` | `--build-namespace`: where build Jobs run. |
| `server.insecureRegistries` | `[]` | `--insecure-registries`: registry hosts served over plain HTTP, e.g. `{localhost:5000}`. Exactly these, never a request's. |
| `server.gitTokenSecret.name` / `.key` | `""` / `token` | Secret supplying `$KELSON_GIT_TOKEN` for Git-backed environments. |
| `server.extraArgs` | `[]` | Appended verbatim. |
| `server.extraEnv` | `[]` | Extra `EnvVar` entries. |
| `replicaCount` | `1` | The process holds no state, so >1 is real HA (ADR-0013 §1). |
| `namespace.create` / `namespace.name` | `false` / release ns | See below. |
| `serviceAccount.create` / `.name` / `.annotations` | `true` / `""` / `{}` | |
| `rbac.create` | `true` | The namespaced Roles and bindings. |
| `rbac.createDetectClusterRole` | `true` | The read-only detect ClusterRole. Without it, detection reports gaps instead of facts — it degrades, it does not break. |
| `rbac.createDeployClusterRole` | `false` | The cluster-wide delivery grant. **A server expected to deploy needs it**; without it the first deploy fails on `namespaces`. Broad — read the section above before setting it. |
| `rbac.targetNamespaces` | `[]` | Extra namespaces the server may serve. |
| `controller.enabled` | `false` | Deploy kelson-controller. See [Configuring the controller](#configuring-kelson-controller). |
| `controller.image.repository` / `.tag` | `ghcr.io/dafrie/kelson-controller` / `""` | `tag` falls back to `image.tag`. |
| `controller.replicaCount` | `1` | Safe at any count — see `controller.leaderElection.enabled`. |
| `controller.probePort` | `8081` | `/healthz` and `/readyz`. |
| `controller.metricsBindAddress` | `"0"` | `0` disables the metrics endpoint. |
| `controller.registry` | `""` | `--registry`: push prefix for published artifacts. Empty means `RegistryNotConfigured`. |
| `controller.pushSecret` | `""` | `--registry-config`, mounted from an existing `dockerconfigjson` Secret. Not created here. |
| `controller.pullSecret` | `""` | `--pull-secret`: dockerconfigjson Secret in `controller.fluxNamespace` that source-controller pulls with. |
| `controller.insecureRegistries` | `[]` | `--insecure-registries`, e.g. `{localhost:5000}`. |
| `controller.fluxNamespace` | `""` | `--flux-namespace`. Empty means the release namespace; also where `<name>-controller-flux` is created. |
| `controller.reconcileInterval` | `""` | `--reconcile-interval`. Empty means the binary's own default (5m). |
| `controller.leaderElection.enabled` | `true` | Contend for a Lease before publishing or applying. On by default (ADR-0028). |
| `controller.leaderElection.namespace` | `""` | Defaults to the release namespace. |
| `controller.serviceAccount.create` / `.name` / `.annotations` | `true` / `""` / `{}` | |
| `controller.rbac.create` | `true` | The controller's ClusterRole/binding and the flux namespace's Role/binding. |
| `controller.resources` | 50m/64Mi requests, 256Mi limit | |
| `controller.extraArgs` / `.extraEnv` | `[]` | Appended verbatim. |
| `service.type` / `.port` / `.annotations` | `ClusterIP` / `8420` / `{}` | |
| `resources` | 50m/64Mi requests, 256Mi limit | |
| `livenessProbe` / `readinessProbe` | see values.yaml | Both hit `/healthz`, which never requires authentication. |
| `podSecurityContext` / `securityContext` | non-root 65532, read-only rootfs, all caps dropped | The image is `FROM scratch` with a static binary. |
| `podAnnotations`, `podLabels`, `nodeSelector`, `tolerations`, `affinity`, `topologySpreadConstraints` | empty | |
| `commonLabels` | `{}` | Added to every object the chart creates. |

### About `namespace.create`

Helm needs the release namespace to exist before it writes its own release record, so this is
not a substitute for `helm install --create-namespace` (and combining the two conflicts — Helm
would create the namespace and then try to apply one it does not own). It exists for the case
where the namespace should be part of the release and swept by `helm uninstall`, which in
practice means a `namespace.name` that is not the release namespace. The plain path is
`kubectl create namespace kelson-system` and leaving this `false`.

## Tests

`deploy/chart/` holds them, and `go test ./deploy/...` runs them:

- the detect ClusterRole here matches `deploy/rbac/detect-clusterrole.yaml`, rule for rule, both
  as files and as rendered output;
- the deploy ClusterRole covers every kind `internal/renderer` emits — the set is derived from the
  renderer's own golden files rather than written down twice, so a new rendered kind fails the
  build until its rule exists — and it is absent from a default render;
- `appVersion` tracks `internal/version`;
- `helm lint` passes and every rendered document is valid YAML carrying the uninstall label;
- the auth gate refuses a render with no password, and `image.tag` refuses to default.

The render tests need the `helm` binary and skip with a message when it is absent
(`go install helm.sh/helm/v3/cmd/helm@latest`).
