# Minimal image for kelson-server.
#
# Built by goreleaser (CGO_ENABLED=0 → fully static). The binary for the
# target goos/goarch is copied into the build context as `kelson-server`.
# See .goreleaser.yml and docs/release-policy.md. Issue #21.
#
# FROM scratch still holds now that the server talks to the API server (#139):
# in-cluster credentials come from the projected service-account token and the
# cluster CA the kubelet mounts at /var/run/secrets/kubernetes.io/serviceaccount,
# so no system CA bundle is needed. A deployment pointed at an external cluster
# over public TLS would need one — that is a --kubeconfig setup, not the
# in-cluster path this image exists for.

FROM scratch
COPY kelson-server /kelson-server
EXPOSE 8420
ENTRYPOINT ["/kelson-server"]
