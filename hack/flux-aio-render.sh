#!/usr/bin/env bash
# Renders the flux-aio install manifests kelson's `flux-aio` catalog row serves
# (issue #226, ADR-0030 decision 2).
#
# # Why bytes are committed here at all
#
# ADR-0021 decision 2 is emphatic that kelson vendors no upstream manifest: the
# repository holds a URL and a digest, and `kelson install` fetches at install
# time. flux-aio cannot follow that rule, because upstream publishes it ONLY as
# a timoni module — there is no `install.yaml` release asset to pin a URL and a
# SHA-256 against. ADR-0030 decision 2 states the exception and its price: what
# is committed is a *mechanically regenerated snapshot in kelson's custody*, not
# a hand-edited copy, and this script is the whole of that claim.
#
# Four properties make the snapshot a build artifact rather than a vendored one,
# and every one of them is enforced below rather than asserted:
#
#   - it is the output of ONE script against TWO pins (the timoni binary and the
#     module digest), so it cannot be hand-edited without the drift test in
#     internal/delivery/install/fluxaio_test.go failing;
#   - the upstream reference is recorded — module reference, tag and OCI digest —
#     both in the rendered file's own header and in the pins table row;
#   - anybody can re-derive it in one command: run this script and compare;
#   - it is small: one pod's worth of Flux, not a chart ecosystem.
#
# # Timoni is not a kelson dependency, and this script is why that is true
#
# ADR-0030 decision 3 refuses timoni three separate ways: never a runtime
# dependency, never a Go dependency, never a user-visible concept. This script
# is the entire surface timoni has in this repository. It downloads a pinned
# binary into a temporary directory, runs it once, and deletes it. Nothing in
# go.mod changes, nothing on a user's machine invokes timoni, and a user
# installing Flux on their k3s box runs `kelson install flux-aio` and gets a
# Deployment.
#
# # Usage
#
#	hack/flux-aio-render.sh            # render and write the committed file
#	hack/flux-aio-render.sh --check    # render and FAIL if it differs (CI)
#
# `make flux-aio` is the first form. `--check` is what .github/workflows/release.yml
# runs, so a release cannot ship a snapshot that disagrees with its own pins.
#
# # Updating the pins
#
# There are two, and they move independently.
#
# TIMONI_VERSION: pick the release, then take the checksums verbatim from
#
#	curl -fsSL https://github.com/stefanprodan/timoni/releases/download/vX.Y.Z/timoni_X.Y.Z_checksums.txt
#
# MODULE_VERSION / MODULE_DIGEST: list what upstream published and take the
# digest of the tag you are pinning, never a tag alone —
#
#	timoni mod list oci://ghcr.io/stefanprodan/modules/flux-aio
#
# FLUX_VERSION is not a third pin: it is what MODULE_VERSION means, and the
# assertion block below fails when the two disagree. It must also stay inside
# install.FluxDistributionVersion's minor (internal/delivery/install/pins.go),
# for the reason hack/flux-crds.sh gives at length — the Flux this product
# installs and the Flux schemas its tests validate against have to be the same
# Flux.
#
# Then run this script and paste the block it prints into pins.go.
set -euo pipefail

# ---------------------------------------------------------------------------
# The pins.
# ---------------------------------------------------------------------------

# The timoni release that renders the module. Pinned exactly — a patch bump is
# a different renderer and therefore potentially different bytes, which is the
# whole reason this is a pin and not "whatever is on PATH".
TIMONI_VERSION="v0.32.0"

# Checksums for every platform this script can run on, taken verbatim from that
# release's own checksums.txt. The one for the host platform is verified before
# the binary is unpacked; an unlisted platform returns non-zero and is a hard
# failure, because an unverified renderer is not a pin.
#
# It is a function rather than four variables so that no platform's checksum can
# be selected by string interpolation — the shape where a typo silently skips
# the check instead of failing it.
timoni_sha256() {
	case "$1" in
	linux/amd64) printf '%s' "4e1cf845f521bf780000cbef1931768772fc9713afa1c45c54413b8ddfb90aab" ;;
	linux/arm64) printf '%s' "a83b06dc0a3c252be90ba3095347a4734c6a647a817a59c61dc004208359e8cb" ;;
	darwin/amd64) printf '%s' "1c24b7501d9d87554ca89b284f38553ea2c70171bf2466f997258e2871d12b88" ;;
	darwin/arm64) printf '%s' "e389763b0d72b8eaa35e5c4f910dec47b228ebc27667ffc68a2e097aa192852b" ;;
	*) return 1 ;;
	esac
}

# The flux-aio timoni module. MODULE_DIGEST is what is actually pulled —
# MODULE_VERSION is documentation, and the two are checked against each other
# before anything is rendered.
MODULE_REF="oci://ghcr.io/stefanprodan/modules/flux-aio"
MODULE_VERSION="2.9.4-0"
MODULE_DIGEST="sha256:2fdfc00b5a1b59017f63ec0ab78be8b013fa7d542a57d6f1a6db4df64eab5a5a"

# The Flux release MODULE_VERSION packages. This is what the pins table's
# Component.Version records and what kelson.dev/installed-version stamps on
# every applied object, so it is the version a cluster reports about itself.
FLUX_VERSION="v2.9.4"

# The timoni instance name, which is also the name flux-aio gives its objects.
# Upstream's own documented install is `timoni -n flux-system apply flux <module>`;
# changing this renames every object in the snapshot and orphans whatever an
# earlier kelson installed, so it is not a preference.
INSTANCE_NAME="flux"
INSTANCE_NAMESPACE="flux-system"

# The controllers kelson renders for, and therefore the ones the snapshot must
# actually contain. This is install.FluxComponents restated as an assertion:
# helm-controller is here because `kind: helm` renders a HelmRelease and a Flux
# with no helm-controller is a cluster reconciling nothing for it (ADR-0016).
FLUX_CONTROLLERS=(
	"source-controller"
	"kustomize-controller"
	"helm-controller"
	"notification-controller"
)

# The CustomResourceDefinitions the delivery spine itself applies (ADR-0028,
# internal/delivery/flux). A flux-aio render that did not register these would
# install a Flux that cannot serve the two kinds kelson writes.
REQUIRED_CRDS=(
	"kustomizations.kustomize.toolkit.fluxcd.io"
	"ocirepositories.source.toolkit.fluxcd.io"
)

# ---------------------------------------------------------------------------

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"
OUT_DIR="$ROOT/internal/delivery/install/rendered"
OUT_FILE="$OUT_DIR/flux-aio.yaml"

CHECK_ONLY=0
case "${1:-}" in
"") ;;
--check) CHECK_ONLY=1 ;;
*)
	printf 'usage: %s [--check]\n' "$0" >&2
	exit 2
	;;
esac

log() { printf '[flux-aio] %s\n' "$*"; }
die() {
	printf '[flux-aio] error: %s\n' "$*" >&2
	exit 1
}

sha256_of() {
	if command -v sha256sum >/dev/null 2>&1; then
		sha256sum "$1" | awk '{print $1}'
	else
		shasum -a 256 "$1" | awk '{print $1}'
	fi
}

command -v curl >/dev/null 2>&1 || die "curl is needed to download timoni ${TIMONI_VERSION}"
command -v tar >/dev/null 2>&1 || die "tar is needed to unpack the timoni release"

# The pins have to agree with each other before anything is downloaded: a
# MODULE_VERSION that does not carry FLUX_VERSION means the row documents one
# Flux and installs another, which is exactly the drift the pins exist to stop.
[[ "$MODULE_VERSION" == "${FLUX_VERSION#v}"-* ]] ||
	die "MODULE_VERSION ${MODULE_VERSION} does not package FLUX_VERSION ${FLUX_VERSION}: the pins disagree"
[[ "$MODULE_DIGEST" == sha256:* ]] ||
	die "MODULE_DIGEST ${MODULE_DIGEST} is not an OCI digest; pin the digest, never only a tag"

TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

# ---------------------------------------------------------------------------
# 1. The pinned renderer.
# ---------------------------------------------------------------------------

case "$(uname -s)" in
Linux) os="linux" ;;
Darwin) os="darwin" ;;
*) die "unsupported OS $(uname -s); this script pins checksums for linux and darwin only" ;;
esac
case "$(uname -m)" in
x86_64 | amd64) arch="amd64" ;;
aarch64 | arm64) arch="arm64" ;;
*) die "unsupported architecture $(uname -m); this script pins checksums for amd64 and arm64 only" ;;
esac

want_sha="$(timoni_sha256 "${os}/${arch}")" ||
	die "no pinned checksum for ${os}/${arch}; refusing to run an unverified renderer"

TIMONI_TARBALL="timoni_${TIMONI_VERSION#v}_${os}_${arch}.tar.gz"
TIMONI_URL="https://github.com/stefanprodan/timoni/releases/download/${TIMONI_VERSION}/${TIMONI_TARBALL}"

log "downloading timoni ${TIMONI_VERSION} (${os}/${arch})"
curl -fsSL --retry 3 -o "$TMP/$TIMONI_TARBALL" "$TIMONI_URL" ||
	die "could not download $TIMONI_URL"

got_sha="$(sha256_of "$TMP/$TIMONI_TARBALL")"
[[ "$got_sha" == "$want_sha" ]] ||
	die "checksum mismatch for ${TIMONI_URL}: expected ${want_sha}, got ${got_sha}. Either the pin is stale or the bytes at that URL changed; do not paste the new digest in without knowing which."

tar -xzf "$TMP/$TIMONI_TARBALL" -C "$TMP" timoni || die "could not unpack ${TIMONI_TARBALL}"
TIMONI="$TMP/timoni"
chmod +x "$TIMONI"

# ---------------------------------------------------------------------------
# 2. The pinned module, rendered.
# ---------------------------------------------------------------------------
#
# --digest is what makes this reproducible: the tag is recorded for humans, the
# digest is what is pulled, and timoni fails if the tag no longer resolves to
# it. The render needs network access to ghcr.io for the module's blobs; this
# is the ONE step that does, and it runs in CI at release time, never on a
# user's machine (ADR-0030 decision 3).

log "rendering ${MODULE_REF} ${MODULE_VERSION} (${MODULE_DIGEST})"
"$TIMONI" build "$INSTANCE_NAME" "$MODULE_REF" \
	--version "$MODULE_VERSION" \
	--digest "$MODULE_DIGEST" \
	--namespace "$INSTANCE_NAMESPACE" \
	>"$TMP/raw.yaml" ||
	die "timoni could not render ${MODULE_REF}@${MODULE_DIGEST}. Nothing was written. If this is a values-schema failure, the module's defaults changed and this script needs teaching what to pass — do not hand-edit ${OUT_FILE} to work around it."

[[ -s "$TMP/raw.yaml" ]] || die "timoni produced no output for ${MODULE_REF}@${MODULE_DIGEST}"

# ---------------------------------------------------------------------------
# 3. Per-object provenance.
# ---------------------------------------------------------------------------
#
# ADR-0021's provenance is three labels stamped on every applied object, and
# `kelson install` still stamps them (internal/delivery/install/install.go).
# What that machinery cannot say is where a kelson-shipped byte came from, so
# the committed file says it per object, in the file, where a reviewer reading
# the diff is: which module, which digest, which renderer.
#
# The comments are dropped on the way into the cluster — the installer decodes
# YAML to objects — so they cost a reader nothing and cost the cluster nothing.

awk \
	-v module="$MODULE_REF" \
	-v modver="$MODULE_VERSION" \
	-v digest="$MODULE_DIGEST" \
	-v timoni="$TIMONI_VERSION" \
	'
	function emit(   i, line, kind, name, ns, inmeta, ident) {
		if (n == 0) return
		kind = ""; name = ""; ns = ""; inmeta = 0
		for (i = 1; i <= n; i++) {
			line = buf[i]
			if (line ~ /^kind: /) { if (kind == "") kind = substr(line, 7); inmeta = 0; continue }
			if (line ~ /^metadata:[ \t]*$/) { inmeta = 1; continue }
			if (line ~ /^[^ \t#]/) { inmeta = 0; continue }
			if (!inmeta) continue
			if (line ~ /^  name: / && name == "") name = substr(line, 9)
			if (line ~ /^  namespace: / && ns == "") ns = substr(line, 14)
		}
		gsub(/[" ]/, "", kind); gsub(/[" ]/, "", name); gsub(/[" ]/, "", ns)
		ident = (kind == "" ? "?" : kind) "/" (ns == "" ? "" : ns "/") (name == "" ? "?" : name)
		print "---"
		print "# " ident
		print "# flux-aio " modver ", rendered by timoni " timoni
		print "# from " module "@" digest
		for (i = 1; i <= n; i++) print buf[i]
		n = 0
	}
	/^---[ \t]*$/ { emit(); next }
	{ if (n > 0 || $0 !~ /^[ \t]*$/) buf[++n] = $0 }
	END { emit() }
	' "$TMP/raw.yaml" >"$TMP/objects.yaml"

[[ -s "$TMP/objects.yaml" ]] || die "the provenance pass produced no output; this is a bug in this script"

{
	cat <<EOF
# flux-aio ${MODULE_VERSION} — every Flux controller in one pod.
#
# GENERATED FILE. DO NOT EDIT.
#
# Produced by hack/flux-aio-render.sh from a pinned timoni binary and a pinned
# OCI module, and served to a cluster by \`kelson install flux-aio\`
# (internal/delivery/install, ADR-0030). Editing it by hand breaks the drift
# test in internal/delivery/install/fluxaio_test.go, which is the point: these
# bytes are a build artifact in kelson's custody, not a vendored copy.
#
#   flux version   ${FLUX_VERSION}
#   module         ${MODULE_REF}
#   module version ${MODULE_VERSION}
#   module digest  ${MODULE_DIGEST}
#   rendered by    timoni ${TIMONI_VERSION}
#   instance       ${INSTANCE_NAME} in namespace ${INSTANCE_NAMESPACE}
#
# Reproduce it: run hack/flux-aio-render.sh and compare. Nothing about that
# command needs kelson, a cluster, or anything but the two pins above.
EOF
	cat "$TMP/objects.yaml"
} >"$TMP/flux-aio.yaml"

# ---------------------------------------------------------------------------
# 4. Assertions: is this actually the Flux kelson renders against?
# ---------------------------------------------------------------------------
#
# A render that succeeded is not the same as a render that produced what this
# product needs. Upstream's default values are upstream's to change, and the
# failure mode of a silent change — a Flux installed without helm-controller,
# say — surfaces as a `kind: helm` component that never reconciles, weeks
# later, on somebody's cluster. So the shape is checked here, at the one moment
# a human is looking.

grep -q '^kind: Deployment' "$TMP/flux-aio.yaml" ||
	die "the render contains no Deployment; flux-aio is one Deployment and this is not it"

for controller in "${FLUX_CONTROLLERS[@]}"; do
	grep -q -- "$controller" "$TMP/flux-aio.yaml" ||
		die "the render never mentions ${controller}, which kelson renders for (install.FluxComponents, ADR-0016). The module's default values changed — teach this script which values to pass; do not edit the output."
done

for crd in "${REQUIRED_CRDS[@]}"; do
	grep -q -- "name: ${crd}" "$TMP/flux-aio.yaml" ||
		die "the render registers no CustomResourceDefinition ${crd}, which the delivery spine applies (ADR-0028)"
done

# ---------------------------------------------------------------------------
# 5. Write, or check.
# ---------------------------------------------------------------------------

digest="$(sha256_of "$TMP/flux-aio.yaml")"

if [[ "$CHECK_ONLY" == 1 ]]; then
	if [[ ! -f "$OUT_FILE" ]]; then
		die "${OUT_FILE#"$ROOT"/} does not exist. This kelson cannot install flux-aio and the release must not claim it can: run \`make flux-aio\`, review the output, paste the printed block into internal/delivery/install/pins.go, and commit both."
	fi
	if ! diff -u "$OUT_FILE" "$TMP/flux-aio.yaml" >"$TMP/diff.txt"; then
		head -n 80 "$TMP/diff.txt" >&2
		die "the committed snapshot is not what the pins render. Either somebody edited ${OUT_FILE#"$ROOT"/} by hand, or a pin was bumped without re-running this script. Run \`make flux-aio\` and commit the result — never resolve this by editing the YAML."
	fi
	log "committed snapshot matches the pins (${digest})"
	exit 0
fi

mkdir -p "$OUT_DIR"
cp "$TMP/flux-aio.yaml" "$OUT_FILE"
objects="$(grep -c '^---$' "$OUT_FILE")"
log "wrote ${OUT_FILE#"$ROOT"/} — ${objects} object(s), ${digest}"

log ""
log "paste into internal/delivery/install/pins.go's flux-aio row:"
cat <<EOF

		Version:       "${FLUX_VERSION}",
		ModuleRef:     "${MODULE_REF}",
		ModuleVersion: "${MODULE_VERSION}",
		ModuleDigest:  "${MODULE_DIGEST#sha256:}",
		SHA256:        "${digest}",
EOF

log ""
log "review before committing — these are bytes kelson will apply with cluster-admin-shaped RBAC:"
log "  * the object list reads as one pod of Flux, its CRDs and its RBAC, and nothing else"
log "  * no timoni.sh/* annotation or managed-by: timoni label survived into the output"
log "    (ADR-0030 decision 3: timoni is never a user-visible concept). If one did,"
log "    teach this script to strip it rather than editing the file"
log "  * \`go test ./internal/delivery/install/...\` passes, which is the pin agreeing with the bytes"
