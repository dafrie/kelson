#!/usr/bin/env bash
# Shared helpers for the kind-based E2E harness (issue #86). Sourced by
# up.sh, down.sh and run.sh — never executed directly.

# Every caller sets -euo pipefail itself; this file only defines functions
# and constants, so sourcing it must not change caller behaviour.

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
E2E_ROOT="$(cd "$SCRIPT_DIR/../.." && pwd)"
BIN_DIR="$E2E_ROOT/hack/bin"

CLUSTER_NAME="kelson-e2e"

# Pinned so a run today and a run in six months create the same cluster.
# Bump deliberately, and re-verify the example against the new node image
# before committing the bump.
KIND_VERSION="v0.32.0"
NODE_IMAGE="kindest/node:v1.34.8@sha256:02722c2dedddcfc00febf5d27fbeb9b7b2c14294c82109ff4a85d89ac9ba3256"

KIND="$BIN_DIR/kind"
KELSON="$BIN_DIR/kelson"
KUBECONFIG_FILE="$BIN_DIR/e2e.kubeconfig"
GO="${GO:-go}"

log() { printf '[e2e] %s\n' "$*"; }
die() {
	printf '[e2e] ERROR: %s\n' "$*" >&2
	exit 1
}

require_cmd() {
	command -v "$1" >/dev/null 2>&1 || die "$1 is required but not found on PATH. $2"
}

# check_docker fails loudly and in plain language rather than letting kind
# fail three layers down with a cryptic socket error.
check_docker() {
	require_cmd docker "Install Docker Desktop: https://www.docker.com/products/docker-desktop/"
	if ! docker info >/dev/null 2>&1; then
		die "the Docker daemon is not reachable. Start Docker Desktop (or your Docker engine) and wait for it to finish starting, then re-run this command."
	fi
}

# kind_checksum returns the published sha256 for the pinned kind release, one
# entry per platform kind publishes a binary for that this harness supports
# (darwin for local dev, linux for CI). Verified against
# https://github.com/kubernetes-sigs/kind/releases/download/v0.32.0/kind-<platform>.sha256sum
kind_checksum() {
	case "$1" in
	darwin-amd64) echo "295ac6d0d634c9819c9907df45e3017d1f13166bd13c3404c45e79f7faa47498" ;;
	darwin-arm64) echo "dca67911095a110c2b5c36e26df6cac860c602033e456c0db47be498cdef1ebb" ;;
	linux-amd64) echo "50030de23cf40a18505f20426f6a8506bedf13c6e509244bd1fa9463721b0f54" ;;
	linux-arm64) echo "b92cd615e97585de8ddade28ed5cd7feb4248d717c233eea5b03c37298900f5d" ;;
	*) die "no pinned checksum for platform '$1' (kind $KIND_VERSION) — supported: darwin-amd64, darwin-arm64, linux-amd64, linux-arm64" ;;
	esac
}

sha256_of() {
	if command -v sha256sum >/dev/null 2>&1; then
		sha256sum "$1" | awk '{print $1}'
	else
		shasum -a 256 "$1" | awk '{print $1}'
	fi
}

detect_platform() {
	local os arch
	os="$(uname -s | tr '[:upper:]' '[:lower:]')"
	arch="$(uname -m)"
	case "$arch" in
	x86_64) arch="amd64" ;;
	arm64 | aarch64) arch="arm64" ;;
	*) die "unsupported CPU architecture '$arch'" ;;
	esac
	echo "${os}-${arch}"
}

# install_kind installs the pinned kind binary into hack/bin if it is not
# already there at the right version. hack/bin is gitignored: this is
# tooling the harness fetches for itself, not something to vendor.
install_kind() {
	mkdir -p "$BIN_DIR"
	if [[ -x "$KIND" ]] && "$KIND" version 2>/dev/null | grep -q "kind ${KIND_VERSION} "; then
		return
	fi

	require_cmd curl "Needed to download the pinned kind binary."
	local platform url tmp expected actual
	platform="$(detect_platform)"
	expected="$(kind_checksum "$platform")"
	url="https://github.com/kubernetes-sigs/kind/releases/download/${KIND_VERSION}/kind-${platform}"

	log "installing kind ${KIND_VERSION} (${platform}) into ${BIN_DIR}"
	tmp="$(mktemp "${BIN_DIR}/.kind.XXXXXX")"
	if ! curl -fsSL --retry 3 -o "$tmp" "$url"; then
		rm -f "$tmp"
		die "failed to download kind from $url"
	fi
	actual="$(sha256_of "$tmp")"
	if [[ "$actual" != "$expected" ]]; then
		rm -f "$tmp"
		die "checksum mismatch downloading kind ${KIND_VERSION}: expected ${expected}, got ${actual}"
	fi
	chmod +x "$tmp"
	mv "$tmp" "$KIND"
}

cluster_exists() {
	"$KIND" get clusters 2>/dev/null | grep -qx "$CLUSTER_NAME"
}
