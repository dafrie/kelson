# kelson Helm chart

Installs `kelson-server` — the API the CLI, the web UI and the MCP surface talk to
([docs/server.md](../../../docs/server.md)) — and the RBAC it needs.

```sh
kubectl create namespace kelson-system
kubectl -n kelson-system create secret generic kelson-auth \
  --from-literal=password="$(openssl rand -base64 24)"

helm install kelson ./deploy/chart/kelson \
  --namespace kelson-system \
  --set image.tag=v0.1.0 \
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
- **No write access to your workloads.** See RBAC below.

## What it creates

| Kind | Scope | Why |
|---|---|---|
| `Namespace` | cluster | Only when `namespace.create=true`. |
| `ServiceAccount` | release ns | The identity everything below binds to. |
| `ClusterRole`/`ClusterRoleBinding` `<name>-detect` | cluster | The read-only detection grant, a verbatim copy of `deploy/rbac/detect-clusterrole.yaml`. Get and list only, no write verb anywhere ([#56](https://github.com/dafrie/kelson/issues/56)). |
| `Role`/`RoleBinding` `<name>-state` | state ns | `configmaps: get, list, create, update, delete` — the spec and history stores (`internal/serverstate`). Plus `secrets: get, list, create, update` for the agent identity store ([#74](https://github.com/dafrie/kelson/issues/74)): one Secret per identity, holding a salted HMAC of the credential and never the credential. No `delete` — a revoked identity is kept so past actions stay attributable. |
| `Role`/`RoleBinding` `<name>-env` | state ns + `rbac.targetNamespaces` | Managed Secrets, the pod and deployment reads behind status and logs, and build Jobs. |
| `Role`/`RoleBinding` `<name>-build` | `server.buildNamespace` | Only when the build namespace is outside the served ones: Jobs, pod logs, and a read of the push Secret. Nothing writes Secrets there. |
| `Secret` | release ns | Only when `auth.password` is a literal. |
| `Deployment`, `Service` | release ns | The server, and a `ClusterIP` in front of it. |

Every rule exists because a named package makes that call. There are no wildcards, and no verb
is granted that nothing calls — `watch` is absent from the ConfigMap and Job grants because
neither the state stores nor the build executor watch (the executor polls).

**The delivery grant is not here.** Applying a rendered spec means writing Deployments, Services
and whatever else a project's manifests contain, into the namespaces you deploy to. That grant
is as wide as the renderer's output and it belongs to those namespaces rather than to the
installer; least-privilege for it is [#84](https://github.com/dafrie/kelson/issues/84)'s work
along with the rest of the threat model. Bind it yourself, per namespace, and scope
`rbac.targetNamespaces` to the namespaces the server is meant to serve —
[docs/server.md](../../../docs/server.md) says the same thing about the Secret grant.

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
| `server.buildNamespace` | `""` | `--build-namespace`: where build Jobs run. |
| `server.gitTokenSecret.name` / `.key` | `""` / `token` | Secret supplying `$KELSON_GIT_TOKEN` for Git-backed environments. |
| `server.extraArgs` | `[]` | Appended verbatim. |
| `server.extraEnv` | `[]` | Extra `EnvVar` entries. |
| `replicaCount` | `1` | The process holds no state, so >1 is real HA (ADR-0013 §1). |
| `namespace.create` / `namespace.name` | `false` / release ns | See below. |
| `serviceAccount.create` / `.name` / `.annotations` | `true` / `""` / `{}` | |
| `rbac.create` | `true` | The namespaced Roles and bindings. |
| `rbac.createDetectClusterRole` | `true` | The read-only detect ClusterRole. Without it, detection reports gaps instead of facts — it degrades, it does not break. |
| `rbac.targetNamespaces` | `[]` | Extra namespaces the server may serve. |
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
- `appVersion` tracks `internal/version`;
- `helm lint` passes and every rendered document is valid YAML carrying the uninstall label;
- the auth gate refuses a render with no password, and `image.tag` refuses to default.

The render tests need the `helm` binary and skip with a message when it is absent
(`go install helm.sh/helm/v3/cmd/helm@latest`).
