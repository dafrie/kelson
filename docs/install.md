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

There are **three layers**, and removing one never removes another. That separation is
[ADR-0003](adr/0003-install-model.md)'s additive doctrine seen from the exit:

| Layer | What removes it | What it leaves |
|---|---|---|
| An application environment kelson deployed | `kelson uninstall --project <p> --env <e>` | everything in the namespace that is not kelson's |
| The kelson server | `helm uninstall kelson -n kelson-system` | every application kelson deployed, still running |
| The operators kelson delegates to | your own tooling — never kelson's | — |

### The applications: `kelson uninstall`

```sh
kelson uninstall --project checkout --env production
```

It removes what kelson deployed for one (project, environment) and nothing else. Three steps, in
this order, always:

1. **Preview.** Every object it would delete, by kind and name, grouped in deletion order, with a
   loud `DATA` section for the resources whose contents do not come back — a CloudNativePG
   `Cluster`, a `ValkeyCluster`, a `PersistentVolumeClaim` — including the volumes an operator
   will garbage-collect on kelson's behalf. Managed Secrets get their own line, because kelson
   stores no copy of a secret value ([ADR-0009](adr/0009-secrets.md)): the cluster was it.
2. **Confirmation.** Nothing is deleted without `--yes` or an answered prompt. A non-terminal
   stdin with no `--yes` is refused rather than assumed either way, so a CI job cannot delete a
   database by accident. `--yes` skips the question, never the preview.
3. **Deletion**, with a per-object result — `deleted`, `gone`, `left`, `failed`. Every object is
   re-read and re-checked against its live labels immediately before its delete, with a UID
   precondition on the delete itself, so anything that stopped being kelson's in between is
   reported and left standing.

The handle is the provenance label set and only that:

```sh
kubectl get all,secret,configmap,serviceaccount,pvc -n checkout-production \
  -l app.kubernetes.io/managed-by=kelson,kelson.dev/project=checkout,kelson.dev/environment=production
```

The command prints that selector, so what it will delete is checkable with kubectl before you
answer the prompt.

**Order.** Routes first, so traffic stops arriving at something that is disappearing. Then
workloads, so nothing is left holding a connection to a database being deleted. Then configuration.
Then data, last, because it is the only irreversible step. Then the namespace.

**The namespace is deleted only when kelson created it.** A deploy records that fact in
`kelson.dev/namespace-ownership` — `created` when the apply brought the namespace into existence,
`adopted` when it was already there. Deleting a namespace cascades to everything inside it, so an
adopted namespace stays, and so does everything in it that is not kelson's.

Two flags for the two things people want kept:

- `--keep-data` leaves data services and their volumes alone. The namespace then stays too — deleting
  it would delete what was kept.
- `--keep-history` keeps the local rendered history as an audit trail. By default it goes: a journal
  describing a set that no longer exists would give the next deploy of the same name a revision
  sequence and a prune baseline inherited from a deployment that is gone.

`--all-environments` removes every environment of a project.

### What `kelson uninstall` deliberately does not remove

- **The server.** That is Helm's, below.
- **CRDs.** kelson installs none — its state is ConfigMaps
  ([ADR-0013](adr/0013-server-state-and-api-v0.md) §1) — so there are none of its to remove.
- **Operators.** CloudNativePG, the Valkey operator, Flux, cert-manager. kelson delegates to them
  and never installs them ([ADR-0005](adr/0005-delegate-to-operators.md)); removing one would break every
  other tenant of the cluster.
- **PersistentVolumes, and volumes an operator owns.** Deleting a CloudNativePG `Cluster` hands its
  PVCs to CloudNativePG's own garbage collection. They are named in the preview because they are
  about to be destroyed; kelson does not delete them itself.
- **Anything without kelson's provenance labels**, including Secrets kelson did not write.
- **The server's stored specs and history.** Those are ConfigMaps in the server's namespace, and
  removing them is an authorization decision that does not exist yet. Today `kelson uninstall`
  removes the CLI's own local rendered history and says so; the API and UI uninstall is future work
  gated on [#84](https://github.com/dafrie/kelson/issues/84)'s authorization design, which is why
  there is no uninstall RPC or MCP tool. The verb belongs to whoever holds the kube context, and
  the cluster's RBAC is the access control.

### The server: `helm uninstall`

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
[#59](https://github.com/dafrie/kelson/issues/59) owns, which the E2E harness proves both as a
property of the label selector and as the `kelson uninstall` verb ([E2E harness](e2e.md)). Your
specs and history are not in that set either: they are ConfigMaps in the state namespace, and
`helm uninstall` leaves them.

## Without Helm

The detection ClusterRole is a plain manifest at `deploy/rbac/detect-clusterrole.yaml` and can be
applied on its own — it is all the CLI ever needs, and it is read-only
([cluster detection](detection.md)). The chart's copy of it is kept identical by a test in
`deploy/chart`.
