# Rendered install snapshots

Generated files. Do not edit anything in this directory by hand.

Each `*.yaml` here is the output of `hack/*-render.sh` against a pair of pins, compiled into the kelson
binary by `//go:embed` (see `../fluxaio.go`) and applied to a cluster by `kelson install`. Today there
is exactly one: `flux-aio.yaml`, written by [`hack/flux-aio-render.sh`](../../../../hack/flux-aio-render.sh).

## Why bytes live in this repository at all

[ADR-0021](../../../../docs/adr/0021-installing-missing-components.md) decision 2 says kelson vendors no
upstream manifest — the repository holds a URL and a digest and `kelson install` fetches at install
time, because Kubero vendored Bitnami's charts and its users' running systems broke when Broadcom
withdrew them.

flux-aio cannot follow that rule: upstream publishes it **only as a timoni module**, so there is no
`install.yaml` release asset to pin a URL against.
[ADR-0030](../../../../docs/adr/0030-flux-aio-install.md) decision 2 states the exception and what
earns it — this is a *mechanically regenerated snapshot in kelson's custody*, not a vendored copy:

- it is the output of one script against two pins (a checksum-verified timoni binary and a
  digest-pinned OCI module), and it cannot be hand-edited without
  `internal/delivery/install/fluxaio_test.go` failing;
- the upstream reference is recorded in the file's own header and in the pins table row, so the
  provenance question has a documented answer;
- anyone can re-derive it in one command and compare checksums;
- it is one pod's worth of Flux, not a chart ecosystem.

`.github/workflows/release.yml` runs `hack/flux-aio-render.sh --check` before a release, so a release
cannot ship a snapshot that disagrees with its own pins.

## Regenerating

```sh
make flux-aio                     # render, write, and print the pins block
hack/flux-aio-render.sh --check   # what CI runs: render and fail on any difference
```

Then paste the printed block into `../pins.go` and commit the YAML and the pins together. A half-updated
row cannot merge — the drift test compares the committed bytes against `Component.SHA256`.

## When this directory becomes empty

[ADR-0030](../../../../docs/adr/0030-flux-aio-install.md)'s "revisit when" says it plainly: if flux-aio
ever publishes a plain YAML release asset, the render script and this snapshot both disappear and the
catalog row becomes an ordinary pinned URL. That is the outcome the ADR would prefer.
