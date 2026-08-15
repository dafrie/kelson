# ADR-0033: Git connections — the forge credentials kelson holds, and how they are scoped

- **Status:** Accepted
- **Date:** 2026-08-15

> Amends [ADR-0009](0009-secrets.md)'s boundary — *"credentials in Secrets somebody else manages, and
> the spec carries a reference"* — for one named class of credential: forge credentials that kelson
> itself uses. Everything else in ADR-0009 stands, including where values live (Kubernetes Secrets)
> and what the spec may carry (references, never values). Read with
> [ADR-0034](0034-forge-driven-delivery.md), which is what these credentials are *for*.

## Context

kelson's entire git-credential story today is one process-wide environment variable. `KELSON_GIT_TOKEN`
(`internal/gitref/auth.go`, `cmd/kelson-server/main.go`) authenticates exactly one operation — the
`git ls-remote` that resolves a ref to a commit — and nothing else. The consequences are visible at
every surface that touches a repository:

- **A private repository cannot actually be built.** The server resolves its ref, and then the build
  pod's clone init container (`internal/build/source.go`) fetches with no credential at all and fails.
  The one credential that exists is not projected to the one place that needs it.
- **Previews require a hand-made Secret.** `previews.secretRef` names a Secret whose shape is
  flux-operator's (`username`/`password` or the `githubApp*` keys), created out of band, per
  environment ([ADR-0017](0017-pr-previews.md) decision 1). The category's competitors ask the same of
  their users; the experience this project wants — install an app, get previews — asks it of nobody.
- **There is no repo picker, no webhook, no commit status and no PR comment**, because each of those
  is a forge API call and kelson has nothing to make it with.
- **`propose-only` agent policy stops one step short.** [ADR-0025](0025-agent-policy.md)'s
  proposal flow refuses the mutation and points at the diff; opening the pull request itself *"needs
  a forge credential, which is a class of secret ADR-0009 deliberately keeps out of the control
  plane"* (docs/architecture.md). The same wall, hit from a fourth direction.

[ADR-0017](0017-pr-previews.md) decision 8 and [ADR-0028](0028-delivery-spine.md) both cite the
missing forge credential as a load-bearing constraint, and both chose designs that route around it.
Those designs were right on their premises. This ADR changes the premise, deliberately and narrowly:
the product direction is a Vercel/Heroku-grade connect experience — pick a repository from a list,
get previews and statuses without editing CI — and every step of that experience is a forge API call.
A PaaS that refuses to hold any forge credential is a PaaS whose competitors' onboarding is one click
shorter forever.

What must not change while changing that: values stay out of specs, out of Git and out of kelson's
own storage (ADR-0009); the renderer stays pure and never sees a credential
([ADR-0001](0001-hybrid-state-model.md), [ADR-0029](0029-renderer-stays-go.md)); and whatever is
introduced must already have a place for the ownership model that tenancy
([ADR-0031](0031-single-cluster-single-tenant.md), #231) will need, so that credentials do not have
to be re-homed when principals arrive.

## Decision

### 1. `GitConnection` is a CRD, and credentials stay in Secrets it references

A new namespaced kind in `kelson.dev/v1alpha1`, living in `kelson-system`, following
[ADR-0027](0027-crd-native-control-plane.md)'s conventions (spec struct in `internal/model`, wrapper
in `api/kelson/v1alpha1`, CRD YAML out of `internal/schemagen`, status subresource):

```yaml
apiVersion: kelson.dev/v1alpha1
kind: GitConnection
metadata:
  name: acme-github
  namespace: kelson-system
spec:
  provider: github                  # github | generic; the enum grows per adapter
  host: https://github.com          # forge base URL; self-hosted GHE/GitLab set it
  auth:                             # exactly one of:
    githubApp:
      appID: 12345
      installationID: 678910
      secretRef: acme-github-app    # keys: privateKey, webhookSecret; kelson-system Secret
    token:
      secretRef: acme-git-token     # keys: token, username (optional)
  owner:
    kind: instance                  # instance | user | team — only instance is enforced today
    # name: alice                   # set when kind is user/team; reserved until #231
status:
  conditions: []                    # Ready, Reachable
  account: acme                     # provider-reported: who the credential acts as
  repositories: 42                  # provider-reported: what the installation can see
```

The CR carries identifiers and references; the Secret carries the material. That half of ADR-0009 is
untouched. **What this ADR amends is the *use*:** kelson-server and kelson-controller now read those
Secrets to mint short-lived tokens and to call forge APIs on the user's behalf. The credential is no
longer something kelson only hands to flux-operator by name — it is something kelson wields.

The renderer never sees a `GitConnection`. Connections are resolved in the planes that already have
cluster access — the server, the controller, the build plane — which is the same split
`ClusterProfile` already draws.

### 2. The flagship connect path is a per-instance GitHub App, created by the manifest flow

"Connect GitHub" in the UI does not ask for a token. It runs GitHub's
[app-manifest flow](https://docs.github.com/en/apps/sharing-github-apps/registering-a-github-app-from-a-manifest):

1. kelson-server generates an app manifest — name `kelson-<instance>`, webhook URL
   `<server>/forge/github/webhook`, redirect URL `<server>/forge/github/manifest/callback`,
   permissions `contents:read`, `metadata:read`, `pull_requests:write`, `statuses:write`,
   `checks:write`, events `push`, `pull_request` — and sends the browser to GitHub with it.
2. The user approves creation (on their account or an organization). GitHub redirects back with a
   one-time code; the server exchanges it for the app ID, the private key and the webhook secret, and
   writes them into a Secret in `kelson-system` plus the `GitConnection` CR.
3. The user is forwarded to the installation page and picks repositories. The resulting
   `installation` webhook event records the installation ID on the connection.

Every instance gets its **own** app: the private key never leaves the instance that minted it, the
webhook URL is the instance's own, and no central kelson-operated relay exists to be trusted or to go
down. This is the shape the self-hosted category has converged on, and the reason a central hosted
app was not considered for long: it cannot deliver webhooks to a private instance without a relay,
and it makes one leaked key everyone's incident.

Short-lived installation tokens (~1h) are minted per operation from the app key — for clones, API
calls and the flux-operator Secret below — and registered with `internal/redact` the moment they are
minted.

**`token` is the universal fallback**, and the only path for every other forge until its adapter
lands: a PAT / project token / deploy token pasted into a Secret-backed form. Less magical, works
everywhere, and it is the same `BasicAuth` shape `internal/gitref` already speaks.

### 3. `internal/forge` is the provider seam, and it is capability-shaped

One new package, one core interface, optional capabilities:

```go
type Provider interface {
    Name() string
    // MintCloneCredential returns a short-lived HTTPS basic-auth credential
    // (or the stored token, for token auth) for read access to one repository.
    MintCloneCredential(ctx, conn, repo) (Credential, error)
}

// Optional. Absence degrades the UI, never the deploy.
type RepoBrowser interface{ ListRepositories; ListBranches }
type WebhookSource interface{ VerifySignature; ParseEvent }
type StatusReporter interface{ ReportStatus; UpsertPRComment }
type PRProposer interface{ OpenPullRequest }
```

The UI discovers capabilities rather than assuming them: a `generic` token connection offers private
clones and nothing else; the GitHub adapter offers all five. A forge is added by writing one adapter,
not by touching every caller — the [ADR-0017](0017-pr-previews.md) posture (*"the enum stays at two
until somebody wants a third and can say what it does"*) applied to the whole seam. GitHub ships
first; GitLab and Forgejo are follow-ups against a proven interface.

Integration is plain HTTPS. No forge SDK enters the module graph — the calls this seam needs (mint an
installation token, list repos, create a status, upsert a comment, open a PR) are a small REST
surface plus one RS256-signed JWT, and the dependency-weight rule of
[ADR-0022](0022-sops-age.md)/[ADR-0030](0030-flux-aio-install.md) applies. `.golangci.yml` gains one
rule for `internal/forge/**`.

### 4. Projects resolve their connection by host, with an explicit override

`Project.spec.source` gains one optional field:

```yaml
source:
  git: https://github.com/acme/checkout
  ref: main
  connection: acme-github     # optional; resolved by host/org match when absent
```

When absent, the connection is chosen by longest host-then-owner match against the source URL —
one GitHub connection means zero configuration, which is the common case. Two connections matching
the same repository is a resolution error naming both and the field that disambiguates, never a
silent pick. Public repositories need no connection at all; anonymous stays the default
(`gitref.Anonymous`).

The same resolution serves previews: when the environment's `previews.repo` matches a connection,
kelson **materializes the flux-operator-shaped Secret from it** — `username`/`password` from a
token connection, the `githubApp*` keys from an app connection — and `previews.secretRef` becomes
optional. The field stays for anyone bringing their own Secret; nothing breaks.

### 5. Build pods receive minted credentials, correctly, for the first time

The clone init container gets the connection's minted credential via a projected Secret and a git
credential helper — never in the URL (visible in `ps` and error output), never in an argument. The
server mints per build, writes the short-lived value into the build namespace, and the Secret is
deleted with the build's other per-run objects. This closes the gap named in Context: the private
repository that can resolve but not fetch.

`KELSON_GIT_TOKEN` survives as bootstrap: at startup the server surfaces it as an implicit,
instance-owned `generic` connection, marked deprecated in `kelson doctor`-style output. One
credential story, not two.

### 6. Ownership is designed now, enforced when principals exist

`spec.owner` is a discriminated reference: `instance` today; `user` and `team` reserved. The
semantics are fixed by this ADR so tenancy does not have to re-litigate them:

- **Use is granted by visibility, mutation by ownership.** Any project that can see a connection may
  build and preview through it. Editing, rotating or deleting it belongs to its owner (and to
  instance admins).
- **A non-owner sees the connection read-only.** Alice's connection used by a project Bob works on
  renders in Bob's UI as provider, host, account and health — selectable, not editable. Secret
  material is displayed to nobody, owner included; that is already ADR-0009's rule.
- **Until #231 lands, everything is effectively instance-scoped** — the shared-password trust model
  ([ADR-0013](0013-server-state-and-api-v0.md) §3) has no principal to enforce against. The field is
  stored, shown and validated, and enforcement arrives with the subject it needs.

An instance-wide connection is therefore the day-one experience the product wants: one person
installs the GitHub App, every subsequent project creation offers its repositories.

## Rationale

- **A CRD rather than server config**, because a connection has exactly what
  [ADR-0027](0027-crd-native-control-plane.md) says earns a status subresource: observed state
  (reachable? how many repos? which account?) that a controller refreshes and three clients read.
  `kubectl get gitconnections` answering "what can this cluster pull from" is the same
  anti-lock-in property the rest of the control plane just bought.
- **Per-instance app rather than PAT-first**, because the two differ in kind, not convenience: an
  installation token is scoped to chosen repositories, expires in an hour and is minted on demand; a
  PAT is a person's standing credential pasted into a box. The manifest flow makes the right thing
  the easy thing.
- **Capability interfaces rather than one fat `Forge` interface**, because the forges genuinely
  differ (GitHub has an app-manifest flow; GitLab has group tokens and no manifest; a bare git host
  has neither) and the degradation path must be legible in the type system, not in `if provider ==`
  scattered through callers.
- **Host-match resolution rather than a mandatory field**, because the common case is one connection,
  and a spec field that must name it in every project is vocabulary without information — the same
  argument ADR-0006 made for deriving component kinds.

## Consequences

**Positive.**

- Private repositories build, previews configure themselves, the New Project page grows a repo
  picker, statuses and PR comments become possible ([ADR-0034](0034-forge-driven-delivery.md)), and
  `propose-only` agents can finally open the pull request they propose.
- One connect ceremony per forge account, ever. Everything downstream reuses it.
- The ownership model exists before the principals do, so tenancy attaches to it rather than
  migrating it.

**Negative — stated as plainly as the positives.**

- **kelson's threat model now includes forge-credential compromise.** A compromised kelson instance
  can read every connected repository and write statuses, comments and PRs as the app. Mitigations
  are real but partial: short-lived minted tokens, least-privilege app permissions, per-repository
  installation choice, and `contents:read` rather than write. The threat-model issue (#84) must gain
  this section before the feature ships.
- **This is the reversal ADR-0017 decision 8 and ADR-0028 leaned on.** Both cited "a forge credential
  kelson does not hold" as a wall; this ADR removes the wall knowingly. The designs built against it
  (CI publisher, OCI-not-git transport) remain right for their own reasons — ADR-0034 amends the
  first and this ADR does not touch the second — but the argument "kelson *cannot*" becomes "kelson
  *chooses not to*", which is weaker, and future decisions must not quietly lean on the old version.
- **The webhook endpoint is a new public attack surface.** HMAC verification with the app's webhook
  secret gates every event, and no event mutates state directly (ADR-0034: events poke reconcilers
  and enqueue work that re-reads the source of truth) — but the listener exists, and an instance
  behind NAT that cannot receive deliveries silently degrades to polling. The UI must show delivery
  state honestly (*"no deliveries received — polling every 10m"*), in the checked-versus-could-not-check
  vocabulary the ClusterProfile already uses.
- **Ownership is display-only until #231.** A `user`-owned connection today is a label, not a
  boundary, and saying otherwise in the UI would be authorization theatre — the exact failure
  ADR-0031 refuses. The UI copy must say "visible to everyone on this instance" until it is not.
- **One more thing between a user and a build.** A deleted Secret, a revoked installation or an
  expired PAT is now a build failure with a new cause, and the connection's `Reachable` condition
  plus a structured build error naming the connection are what keep it diagnosable.
- **Two auth shapes per adapter is a real testing matrix**, and the manifest flow itself is only
  testable against a live GitHub — a seam that needs a recorded-fixture strategy from day one.

## Revisit when

- **Tenancy lands (#231).** `owner.kind: user|team` gets enforcement, and the read-only rule decided
  here gets a subject.
- **A second forge adapter is written.** GitLab's shape (OAuth app or group token, no manifest flow)
  is the first real test of the capability seam; Forgejo/Gitea follows it.
- **Someone needs SSH.** Everything here is HTTPS-only, matching `internal/gitref`. Deploy keys are a
  different credential class with a different UX, and they earn their own decision.
- **Multi-cluster (#232).** Where connections live when there is more than one cluster is part of
  that ADR's credential story, not this one's.
