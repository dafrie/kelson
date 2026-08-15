#!/usr/bin/env bash
# Refreshes the Flux CustomResourceDefinition fixtures the controller's envtest
# suite installs into its API server (issue #243).
#
# # Why bytes are committed here at all
#
# ADR-0021 decision 2 is emphatic that kelson vendors no upstream manifest: the
# repository holds a URL and a digest, and `kelson install` fetches at install
# time. That rule is about what kelson *applies to a user's cluster*, and it
# still holds — nothing this script writes is ever applied anywhere but a
# throwaway envtest API server.
#
# A test fixture cannot follow it. envtest reads CRDs off local disk before its
# API server starts; a suite that downloaded them would fail on an air-gapped
# machine, in an offline CI runner, and on the reviewer's laptop on a train, and
# it would make every run depend on GitHub being up. So this is ADR-0030
# decision 2's shape instead: a mechanically regenerated snapshot in kelson's
# custody, not a hand-edited copy. The pins below are the provenance, the
# extraction is deterministic, and internal/controller/fluxcrds_test.go fails if
# the committed bytes are not what this script produced.
#
# # Updating the pin
#
#	curl -fsSLO <the new INSTALL_URL>
#	sha256sum install.yaml
#
# Paste the version and that digest below, run this script, and paste the block
# it prints into internal/controller/fluxcrds_test.go. The version must stay
# inside install.FluxDistributionVersion's minor (ADR-0030), because the point
# of the fixture is that the schemas kelson's tests validate against are the
# schemas the Flux this product installs actually serves — the drift test
# asserts exactly that and fails when the two part company.
set -euo pipefail

# The pinned Flux distribution. FLUX_VERSION must satisfy the semver expression
# in install.FluxDistributionVersion (internal/delivery/install/pins.go); the
# newest patch of that minor is the right choice, since flux-operator reconciles
# patches within the expression by itself.
FLUX_VERSION="v2.9.4"
INSTALL_URL="https://github.com/fluxcd/flux2/releases/download/${FLUX_VERSION}/install.yaml"
INSTALL_SHA256="9eb86c5f9d606b2ac2cfe71223ab2f23faa2d59ccb21df4e08e5610e54d535f8"

# The CRDs kelson's controller writes, and only those. Flux's install manifest
# carries fifteen; installing the other thirteen into an envtest API server
# would be schema kelson never applies against, slower start-up, and a fixture
# whose refresh diff nobody can read.
#
# ResourceSet and ResourceSetInputProvider are deliberately absent: they are
# flux-operator's CRDs, not Flux's, they are not in this manifest, and
# internal/controller never applies one — the renderer emits them into the
# artifact and kustomize-controller applies them in the cluster (ADR-0017,
# ADR-0033 decision 4). FluxInstance is absent for the same reason one step
# further out: `kelson install` creates it, and that path is covered by the e2e
# harness against a real cluster (hack/e2e/spine.sh).
CRDS=(
	"kustomizations.kustomize.toolkit.fluxcd.io"
	"ocirepositories.source.toolkit.fluxcd.io"
)

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"
OUT_DIR="$ROOT/internal/controller/testdata/flux-crds"

log() { printf '[flux-crds] %s\n' "$*"; }
die() {
	printf '[flux-crds] error: %s\n' "$*" >&2
	exit 1
}

sha256_of() {
	if command -v sha256sum >/dev/null 2>&1; then
		sha256sum "$1" | awk '{print $1}'
	else
		shasum -a 256 "$1" | awk '{print $1}'
	fi
}

command -v curl >/dev/null 2>&1 || die "curl is needed to fetch ${INSTALL_URL}"

TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

log "fetching flux ${FLUX_VERSION} install manifest"
curl -fsSL --retry 3 -o "$TMP/install.yaml" "$INSTALL_URL" ||
	die "could not download $INSTALL_URL"

actual="$(sha256_of "$TMP/install.yaml")"
if [[ "$actual" != "$INSTALL_SHA256" ]]; then
	die "checksum mismatch for ${INSTALL_URL}: expected ${INSTALL_SHA256}, got ${actual}. Either the pin is stale or the bytes at that URL changed; do not paste the new digest in without knowing which."
fi

mkdir -p "$OUT_DIR"

# extract writes one CRD document, verbatim, to its own file. Verbatim matters:
# the digest recorded in Go is the digest of upstream's own bytes, so anybody
# can re-derive it from the manifest above without knowing anything about this
# script beyond which document it kept.
extract() {
	local name="$1" dest="$2"
	awk -v want="  name: ${name}" -v out="$dest" '
		function flush() {
			if (n > 0 && iscrd && matched) {
				for (i = 1; i <= n; i++) print buf[i] > out
				found = 1
			}
			n = 0; iscrd = 0; matched = 0
		}
		$0 == "---" { flush(); next }
		{
			buf[++n] = $0
			if ($0 == "kind: CustomResourceDefinition") iscrd = 1
			if ($0 == want) matched = 1
		}
		END { flush(); exit found ? 0 : 1 }
	' "$TMP/install.yaml" || die "no CustomResourceDefinition named ${name} in ${INSTALL_URL}"
}

declare -a lines=()
for crd in "${CRDS[@]}"; do
	# <group>_<plural>.yaml, the naming deploy/crds/ already uses for kelson's
	# own generated CRDs, so the two directories read the same way.
	plural="${crd%%.*}"
	group="${crd#*.}"
	file="${group}_${plural}.yaml"
	extract "$crd" "$OUT_DIR/$file"
	digest="$(sha256_of "$OUT_DIR/$file")"
	log "wrote ${file} (${digest})"
	lines+=("	\"${file}\": \"${digest}\",")
done

log ""
log "paste into internal/controller/fluxcrds_test.go:"
cat <<EOF

	fluxCRDVersion   = "${FLUX_VERSION}"
	fluxCRDSourceURL = "${INSTALL_URL}"
	fluxCRDSourceSHA = "${INSTALL_SHA256}"

var fluxCRDDigests = map[string]string{
$(printf '%s\n' "${lines[@]}")
}
EOF
