#!/usr/bin/env bash
#
# build-release.sh — cross-compile the `oikos` release artifacts.
#
# Produces a static, CGO-free `oikos` binary for every supported
# os/arch pair and a single `SHA256SUMS` covering them all, written into
# ./dist (gitignored). These are the EXACT assets the GitHub Release publishes
# and that scripts/install.sh{,.ps1} fetch + checksum-verify on a clean box.
#
# Build invariant (identical for local + CI so a locally-built asset is
# bit-for-bit what the release ships):
#   CGO_ENABLED=0   — fully static; no libc dependency, runs on any clean box.
#   -trimpath       — strip local filesystem paths from the binary (reproducible).
#   -ldflags "-s -w -X main.version=$VERSION"
#                     -s -w drop the symbol/debug tables (smaller); the -X stamps
#                     the version into `var version` in cmd/oikos/main.go so
#                     `oikos version` prints the real release string.
#
# Asset naming matches what the installers resolve:
#   oikos_<os>_<arch>        (linux, darwin)
#   oikos_<os>_<arch>.exe    (windows)
# and one SHA256SUMS listing every asset (plain `sha256sum` format).
#
# Usage:
#   VERSION=v1.2.3 scripts/build-release.sh     # explicit version
#   scripts/build-release.sh                    # version derived from git
#
# Env knobs:
#   VERSION     release string stamped into the binary + used nowhere else.
#               Default: `git describe --tags --always --dirty` or 0.0.0-dev.
#   DIST_DIR    output directory (default: ./dist).

set -euo pipefail

# --- locate repo root (script lives in <root>/scripts) -----------------------
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$HERE/.." && pwd)"
cd "$REPO_ROOT"

# --- resolve VERSION ---------------------------------------------------------
if [ -z "${VERSION:-}" ]; then
	if VERSION="$(git describe --tags --always --dirty 2>/dev/null)"; then
		:
	else
		VERSION="0.0.0-dev"
	fi
fi

DIST_DIR="${DIST_DIR:-$REPO_ROOT/dist}"

# --- the release matrix ------------------------------------------------------
# os/arch pairs published for every release. Keep in lock-step with the
# detection tables in scripts/install.sh and scripts/install.ps1.
PLATFORMS="
linux/amd64
linux/arm64
darwin/amd64
darwin/arm64
windows/amd64
windows/arm64
"

echo "build-release: VERSION=$VERSION"
echo "build-release: output  $DIST_DIR"
echo

# Start from a clean dist so SHA256SUMS only covers THIS build's assets.
rm -rf "$DIST_DIR"
mkdir -p "$DIST_DIR"

LDFLAGS="-s -w -X main.version=$VERSION"

assets=""
for platform in $PLATFORMS; do
	os="${platform%/*}"
	arch="${platform#*/}"

	ext=""
	[ "$os" = "windows" ] && ext=".exe"
	asset="oikos_${os}_${arch}${ext}"
	out="$DIST_DIR/$asset"

	echo "  building $asset ..."
	# CGO_ENABLED=0 + -trimpath + the version stamp — see the header invariant.
	# Quote the -X value so a VERSION containing a space can't split into a stray
	# linker arg and break the build (gemini review).
	CGO_ENABLED=0 GOOS="$os" GOARCH="$arch" \
		go build -trimpath -ldflags "-s -w -X 'main.version=$VERSION'" -o "$out" ./cmd/oikos

	assets="$assets $asset"
done

# --- SHA256SUMS over every asset (relative names, so `sha256sum -c` works
#     from within dist/) -----------------------------------------------------
echo
echo "  writing SHA256SUMS ..."
(
	cd "$DIST_DIR"
	# shellcheck disable=SC2086
	# macOS ships `shasum -a 256`, not `sha256sum` — so a LOCAL build on a Mac
	# produces the same SHA256SUMS as CI (gemini review).
	if command -v sha256sum >/dev/null 2>&1; then
		sha256sum $assets >SHA256SUMS
	elif command -v shasum >/dev/null 2>&1; then
		shasum -a 256 $assets >SHA256SUMS
	else
		echo "error: neither sha256sum nor shasum found" >&2
		exit 1
	fi
)

echo
echo "build-release: done. dist/ contents:"
find "$DIST_DIR" -maxdepth 1 -type f -printf '  %f\n' | sort
echo
echo "build-release: SHA256SUMS:"
sed 's/^/  /' "$DIST_DIR/SHA256SUMS"
