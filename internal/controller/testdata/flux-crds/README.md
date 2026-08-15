# Flux CRD fixtures

The CustomResourceDefinitions for the two kinds `internal/controller` server-side applies —
`Kustomization` (kustomize.toolkit.fluxcd.io/v1) and `OCIRepository`
(source.toolkit.fluxcd.io/v1) — installed into the envtest API server so the delivery path
(ADR-0028 steps 5 and 6) is exercised against Flux's real schemas rather than against a fake
client that stores whatever it is handed.

## Provenance

| | |
|---|---|
| Upstream | [fluxcd/flux2](https://github.com/fluxcd/flux2) |
| Version | `v2.9.4` |
| Source | `https://github.com/fluxcd/flux2/releases/download/v2.9.4/install.yaml` |
| Source sha256 | `9eb86c5f9d606b2ac2cfe71223ab2f23faa2d59ccb21df4e08e5610e54d535f8` |
| Extracted by | [`hack/flux-crds.sh`](../../../../hack/flux-crds.sh) (`make flux-crds`) |
| License | Apache-2.0 |

Each file is one document out of that manifest, **verbatim** — nothing is added, reordered or
reformatted — so the digest of any file can be re-derived from the published manifest without
reading the extraction script.

## Do not edit these by hand

`internal/controller/fluxcrds_test.go` records a sha256 per file and fails when the bytes differ,
and it fails again if `v2.9.4` ever falls outside the semver expression
`install.FluxDistributionVersion` puts in the `FluxInstance` kelson creates (ADR-0030). Both run
in plain `go test ./...`, with no build tag and no control-plane binaries, so drift is caught on
every CI run.

To move the pin: edit `FLUX_VERSION` and `INSTALL_SHA256` in `hack/flux-crds.sh`, run
`make flux-crds`, and paste the block it prints into `fluxcrds_test.go`.

## Why these bytes live in the repository at all

[ADR-0021](../../../../docs/adr/0021-installing-missing-components.md) decision 2 says kelson
vendors no upstream manifest: the pins table holds a URL and a digest and `kelson install` fetches
at install time. That rule governs what kelson applies to a **user's cluster**, and it is
untouched — nothing here is ever applied anywhere but a throwaway envtest API server.

A test fixture cannot follow it. envtest reads CRDs off local disk before its API server starts, so
a suite that fetched them would fail air-gapped, fail in an offline runner, and make every run
depend on GitHub being reachable. What this follows instead is
[ADR-0030](../../../../docs/adr/0030-flux-aio-install.md) decision 2's shape: a mechanically
regenerated snapshot in kelson's custody, with its upstream reference recorded and its bytes
reproducible in one command.
