# Minimal image for kelson-server.
#
# Built by goreleaser (CGO_ENABLED=0 → fully static). The binary for the
# target goos/goarch is copied into the build context as `kelson-server`.
# See .goreleaser.yml and docs/release-policy.md. Issue #21.

FROM scratch
COPY kelson-server /kelson-server
ENTRYPOINT ["/kelson-server"]
