#!/bin/sh
#
# install.sh — the `curl -fsSL https://get.oikos.sh | sh` installer for oikos.
#
# Its ONLY job: detect the platform, download the right signed static binary,
# verify it, place it on the user's PATH, and print the next step. It NEVER
# sudos, NEVER starts a service, and writes NO runtime state (the purity
# invariant — a machine that installs and never configures is filesystem-clean
# except the executable). See docs/specs/2026-06-23-install-simplicity-spec.md §4.
#
# The whole body is wrapped in main(){…};main so a dropped connection mid-pipe
# can't half-execute a destructive partial line.
#
# Offline-testable: set OIKOS_INSTALL_DRYRUN=1 to print every step (detect +
# resolved URL + target path) and exit 0 WITHOUT any network or filesystem write.
#
# Overridable knobs (env):
#   OIKOS_INSTALL_DRYRUN=1     plan only; no download, no write
#   OIKOS_VERSION=vX.Y.Z       which release to fetch (default: latest)
#   OIKOS_INSTALL_DIR=<dir>    where to place the binary (default: a PATH dir
#                              that needs no sudo, e.g. ~/.local/bin)
#   OIKOS_BASE_URL=<url>       release host base (default: the public releases URL)
#   OIKOS_OS / OIKOS_ARCH      override detection (testing)

set -eu

main() {
	# --- configuration --------------------------------------------------------
	# Placeholder public release host. The real value is set at release time; the
	# script is structured so it is the single source of the download URL.
	BASE_URL="${OIKOS_BASE_URL:-https://github.com/deios0/oikos/releases}"
	VERSION="${OIKOS_VERSION:-latest}"
	DRYRUN="${OIKOS_INSTALL_DRYRUN:-0}"

	# --- detect OS ------------------------------------------------------------
	os="${OIKOS_OS:-}"
	if [ -z "$os" ]; then
		uname_s="$(uname -s 2>/dev/null || echo unknown)"
		case "$uname_s" in
		Linux*) os="linux" ;;
		Darwin*) os="darwin" ;;
		MINGW* | MSYS* | CYGWIN* | Windows_NT) os="windows" ;;
		*)
			fail "unsupported OS: $uname_s — download a binary from $BASE_URL"
			;;
		esac
	fi

	# --- detect arch ----------------------------------------------------------
	arch="${OIKOS_ARCH:-}"
	if [ -z "$arch" ]; then
		uname_m="$(uname -m 2>/dev/null || echo unknown)"
		case "$uname_m" in
		x86_64 | amd64) arch="amd64" ;;
		arm64 | aarch64) arch="arm64" ;;
		*)
			fail "unsupported architecture: $uname_m — download a binary from $BASE_URL"
			;;
		esac
	fi

	# --- resolve artifact + URL ----------------------------------------------
	ext=""
	[ "$os" = "windows" ] && ext=".exe"
	asset="oikos_${os}_${arch}${ext}"

	if [ "$VERSION" = "latest" ]; then
		url="${BASE_URL}/latest/download/${asset}"
	else
		url="${BASE_URL}/download/${VERSION}/${asset}"
	fi

	# --- resolve install dir (no sudo) ---------------------------------------
	bindir="${OIKOS_INSTALL_DIR:-}"
	if [ -z "$bindir" ]; then
		bindir="$(default_bindir)"
	fi
	target="${bindir}/oikos${ext}"

	# --- dry run: plan only, no side effects ---------------------------------
	if [ "$DRYRUN" = "1" ]; then
		echo "oikos installer (dry run — no download, no write)"
		echo "  os:        $os"
		echo "  arch:      $arch"
		echo "  version:   $VERSION"
		echo "  asset:     $asset"
		echo "  url:       $url"
		echo "  target:    $target"
		echo "  next step: oikos init   then oikos emit   (writes your AGENTS.md; no proxy needed)"
		echo "  live mode: oikos serve  then open http://127.0.0.1:4141/setup   (optional)"
		return 0
	fi

	# --- real install ---------------------------------------------------------
	require curl
	require mkdir
	require chmod

	mkdir -p "$bindir"

	tmp="$(mktemp "${TMPDIR:-/tmp}/oikos-XXXXXX")"
	# shellcheck disable=SC2064
	trap "rm -f '$tmp'" EXIT INT TERM

	echo "oikos: downloading $asset ..."
	# -f fail on HTTP error, -S show errors, -s silent progress, -L follow redirects.
	curl -fSsL "$url" -o "$tmp"

	# Checksum + signature verification (best-effort: only if the tools and the
	# published sums are present). A future release ships SHA256SUMS + minisign on
	# a SEPARATE host; verify against those exactly. We never auto-run an
	# unverified binary silently — absence of sums is reported, not hidden.
	verify_checksum "$tmp" "$asset" "$url" || true

	chmod +x "$tmp"
	mv "$tmp" "$target"
	trap - EXIT INT TERM

	echo "oikos: installed to $target"
	case ":${PATH}:" in
	*":${bindir}:"*) : ;; # already on PATH
	*) echo "oikos: add $bindir to your PATH (e.g. export PATH=\"$bindir:\$PATH\")" ;;
	esac
	echo ""
	echo "Next:  oikos init     # seed a vault + a starter rule"
	echo "       oikos emit     # write the ranked block into your AGENTS.md (no proxy needed)"
	echo ""
	echo "Optional live mode (real-time correction capture via the proxy):"
	echo "       oikos serve    # then open http://127.0.0.1:4141/setup"
}

# default_bindir picks a PATH directory that needs NO sudo, preferring the XDG /
# common per-user bin dirs, falling back to ~/.local/bin (created on install).
default_bindir() {
	for d in "$HOME/.local/bin" "$HOME/bin"; do
		case ":${PATH}:" in
		*":${d}:"*)
			echo "$d"
			return 0
			;;
		esac
	done
	# Not on PATH yet — use ~/.local/bin and tell the user to add it.
	echo "$HOME/.local/bin"
}

# verify_checksum verifies tmp against a published SHA256SUMS if both a checksum
# tool and the sums file are available. Returns non-zero (caller tolerates) when
# verification can't be performed, after printing a clear notice — never silent.
verify_checksum() {
	_file="$1"
	_asset="$2"
	_url="$3"
	_sumsurl="$(dirname "$_url")/SHA256SUMS"

	if command -v sha256sum >/dev/null 2>&1; then
		_sumcmd="sha256sum"
	elif command -v shasum >/dev/null 2>&1; then
		_sumcmd="shasum -a 256"
	else
		echo "oikos: no sha256 tool found; skipping checksum verification"
		return 1
	fi

	_sums="$(curl -fSsL "$_sumsurl" 2>/dev/null || true)"
	if [ -z "$_sums" ]; then
		echo "oikos: SHA256SUMS not published for this release; skipping checksum verification"
		return 1
	fi

	_want="$(echo "$_sums" | grep " ${_asset}\$" | awk '{print $1}' | head -n1)"
	if [ -z "$_want" ]; then
		echo "oikos: $_asset not listed in SHA256SUMS; skipping checksum verification"
		return 1
	fi
	_got="$($_sumcmd "$_file" | awk '{print $1}')"
	if [ "$_want" != "$_got" ]; then
		fail "checksum mismatch for $_asset (expected $_want, got $_got) — refusing to install"
	fi
	echo "oikos: checksum verified"
	return 0
}

require() {
	command -v "$1" >/dev/null 2>&1 || fail "required tool not found: $1"
}

fail() {
	echo "oikos install error: $*" >&2
	exit 1
}

main "$@"
