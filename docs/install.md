# Installing kelson

The CLI needs nothing installed in your cluster. `kelson render`, `kelson diff`, `kelson eject`
and `kelson profile` talk to the API server with your kube context, so the cluster's RBAC is the
access control and there is nothing to deploy ([the server](server.md), "Who may reach it").

You install something when you want the **server** — the API the web UI and the MCP surface talk
to, and the thing that holds project specs and deployment history. That is what the Helm chart in
`deploy/chart/kelson` installs. Issue
[#58](https://github.com/dafrie/kelson/issues/58); the chart's own reference is
`deploy/chart/kelson/README.md`.

## Helm

```sh
kubectl create namespace kelson-system

kubectl -n kelson-system create secret generic kelson-auth \
  --from-literal=password="$(openssl rand -base64 24)"

helm install kelson ./deploy/chart/kelson \
  --namespace kelson-system \
  --set image.tag=v0.1.0 \
  --set auth.existingSecret.name=kelson-auth
```

Then reach it:

```sh
kubectl -n kelson-system port-forward svc/kelson 8420:8420
curl http://127.0.0.1:8420/healthz
```

Two values have no default, and the chart refuses to render without them rather than guessing.

### Authentication is required unless you turn it off out loud

In a cluster the server binds `0.0.0.0`, because a Service cannot reach a process listening on
loopback. `kelson-server` itself refuses that bind with no password set
([ADR-0013](adr/0013-server-state-and-api-v0.md) §3, [the server](server.md)), and the chart
reproduces the refusal at render time — the same posture decision, made before anything reaches
the cluster instead of after the first pod crashes.

- `auth.existingSecret.name` — **preferred**. The chart references a Secret you created and never
  sees the value, so the password is not in your values file, the Helm release record, or
  `helm get values` output.
- `auth.password` — a literal. Supported, and documented as the lesser option: the value ends up
  in the release record too. It still reaches the pod as a `secretKeyRef`.
- `auth.insecure=true` — serve with no authentication, deliberately. Renders `--insecure-bind`.
  Anything that can reach the Service can deploy to your cluster.

Setting none of them fails the render with a message naming all three. There is still no TLS in
`kelson-server`: port-forward, or put a TLS-terminating proxy in front of the Service. The real
answer is [#84](https://github.com/dafrie/kelson/issues/84).

### `image.tag` has no default

Not `latest` — a mutable tag makes a Deployment's identity unknowable. Not the chart's
`appVersion` either, which tracks `internal/version` and is a development placeholder until a
release is cut. Pass the tag you mean. Publishing the images is release work
([release policy](release-policy.md)), not the chart's.

## What the chart deliberately does not do

This is [ADR-0003](adr/0003-install-model.md) in manifest form: kelson adopts a cluster, it does
not colonise one.

- **No CRDs.** kelson's state is ConfigMaps, not custom resources
  ([ADR-0013](adr/0013-server-state-and-api-v0.md) §1). The chart therefore has no CRD lifecycle
  problem to solve, and no `helm upgrade` hazard around one. Deciding this deliberately was
  #58's design question; the answer is that there is nothing to ship because the design does not
  use CRDs.
- **No Ingress and no HTTPRoute.** The server terminates no TLS and how you expose it is your
  cluster's routing decision. Port-forward, or route to the `ClusterIP` Service yourself.
- **Nothing that duplicates what you already run** — no ingress controller, no cert-manager, no
  external-secrets, no Flux, no CloudNativePG, no Prometheus objects. "Never install what is
  already there" is the rule ADR-0003 states, and detection is how kelson finds out
  ([cluster detection](detection.md)). Installing components that are genuinely *missing* is a
  separate, opt-in story: [#60](https://github.com/dafrie/kelson/issues/60).
- **No write access to your workloads.** The chart grants the server its own state ConfigMaps,
  the managed Secrets ADR-0009 defines, build Jobs, and the reads behind status and logs. The
  grant that applies a rendered spec into your namespaces is as wide as the renderer's output and
  belongs to those namespaces rather than to the installer; it is bound per namespace by you, and
  least-privilege for it is #84's work. The chart's README has the full table.

## Uninstalling

```sh
helm uninstall kelson --namespace kelson-system
```

Everything the chart creates carries `kelson.dev/install: <release>`, so what would go away is a
selector query rather than a promise:

```sh
kubectl get all,secret,configmap,role,rolebinding,serviceaccount -A -l kelson.dev/install=kelson
kubectl get clusterrole,clusterrolebinding -l kelson.dev/install=kelson
```

Workloads kelson deployed are not in that set. They carry `kelson.dev/project` labels, they are
plain Kubernetes objects, and they keep running with kelson gone — the additivity claim
[#59](https://github.com/dafrie/kelson/issues/59) owns and that the E2E harness already proves
for the rendered set ([E2E harness](e2e.md)). Your specs and history are not in that set either:
they are ConfigMaps in the state namespace, and `helm uninstall` leaves them.

## Without Helm

The detection ClusterRole is a plain manifest at `deploy/rbac/detect-clusterrole.yaml` and can be
applied on its own — it is all the CLI ever needs, and it is read-only
([cluster detection](detection.md)). The chart's copy of it is kept identical by a test in
`deploy/chart`.
