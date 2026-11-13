#!/bin/sh
# Install gta from a GitHub release of cmyolo441-coder/go-terminal-agent.
#
#   curl -fsSL https://raw.githubusercontent.com/cmyolo441-coder/go-terminal-agent/main/scripts/install.sh | sh
#   curl -fsSL .../install.sh | sh -s -- v1.2.3      # a pinned version
#   GTA_VERSION=1.2.3 sh install.sh                  # same, from the environment
#   GTA_INSTALL_DIR=/usr/local/bin sh install.sh     # default: ~/.local/bin
#
# The archive is checked against the release's checksums.txt (SHA-256) before
# anything is installed; any mismatch or missing tool stops the install.

set -eu

REPO="cmyolo441-coder/go-terminal-agent"
BINARY="gta"

say() { printf 'gta-install: %s\n' "$*" >&2; }
die() {
	say "error: $*"
	exit 1
}

usage() {
	cat <<'EOF'
Usage: install.sh [VERSION]

Installs the gta binary from GitHub releases.

  VERSION          release to install, e.g. v1.2.3 or 1.2.3 (default: latest)

Environment:
  GTA_VERSION      same as the VERSION argument (the argument wins)
  GTA_INSTALL_DIR  where to put the binary (default: $HOME/.local/bin)
EOF
}

# ---- arguments -------------------------------------------------------------

version="${GTA_VERSION:-}"
case "${1:-}" in
-h | --help)
	usage
	exit 0
	;;
-*) die "unknown option: $1 (see --help)" ;;
"") ;;
*) version="$1" ;;
esac
[ "$#" -le 1 ] || die "too many arguments (see --help)"

if [ -n "${GTA_INSTALL_DIR:-}" ]; then
	install_dir="$GTA_INSTALL_DIR"
else
	[ -n "${HOME:-}" ] || die "HOME is not set; set GTA_INSTALL_DIR"
	install_dir="$HOME/.local/bin"
fi

# ---- platform --------------------------------------------------------------

detect_os() {
	case "$(uname -s)" in
	Linux) echo linux ;;
	Darwin) echo darwin ;;
	MINGW* | MSYS* | CYGWIN* | Windows_NT)
		die "on Windows use scripts/install.ps1 (PowerShell)"
		;;
	*) die "unsupported operating system: $(uname -s)" ;;
	esac
}

detect_arch() {
	arch="$(uname -m)"
	case "$arch" in
	x86_64 | amd64)
		# A shell running under Rosetta 2 on Apple silicon reports x86_64.
		if [ "$os" = darwin ] && [ "$(sysctl -n sysctl.proc_translated 2>/dev/null || echo 0)" = 1 ]; then
			echo arm64
		else
			echo amd64
		fi
		;;
	aarch64 | arm64) echo arm64 ;;
	*) die "unsupported CPU architecture: $arch (release builds exist for amd64 and arm64)" ;;
	esac
}

os="$(detect_os)"
arch="$(detect_arch)"

# ---- downloads -------------------------------------------------------------

if command -v curl >/dev/null 2>&1; then
	downloader=curl
elif command -v wget >/dev/null 2>&1; then
	downloader=wget
else
	die "need curl or wget"
fi

# download URL FILE: fetch over HTTPS only, failing on any HTTP error.
download() {
	case "$1" in
	https://*) ;;
	*) die "refusing a non-HTTPS URL: $1" ;;
	esac
	if [ "$downloader" = curl ]; then
		curl --proto '=https' --tlsv1.2 -fsSL --retry 3 -o "$2" "$1"
	else
		wget --https-only -q -O "$2" "$1"
	fi
}

# latest_tag prints the tag of the newest (non pre-release) release.
latest_tag() {
	if [ "$downloader" = curl ]; then
		# .../releases/latest redirects to .../releases/tag/<tag>; this avoids
		# the API's rate limit.
		url="$(curl --proto '=https' --tlsv1.2 -fsSL -o /dev/null -w '%{url_effective}' \
			"https://github.com/$REPO/releases/latest")" || return 1
		tag="${url##*/}"
	else
		json="$(wget --https-only -q -O - "https://api.github.com/repos/$REPO/releases/latest")" || return 1
		tag="$(printf '%s\n' "$json" | sed -n 's/^[[:space:]]*"tag_name":[[:space:]]*"\([^"]*\)".*/\1/p' | head -n 1)"
	fi
	[ -n "$tag" ] && [ "$tag" != latest ] || return 1
	printf '%s\n' "$tag"
}

if [ -z "$version" ] || [ "$version" = latest ]; then
	version="$(latest_tag)" || die "could not find the latest release of $REPO (pin one: install.sh v1.2.3)"
fi
version="${version#v}"

# Only a plain semantic version may go into a URL or a file name.
if ! printf '%s\n' "$version" | grep -Eq '^[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?$'; then
	die "not a release version: $version (want e.g. 1.2.3 or v1.2.3)"
fi

archive="${BINARY}_${version}_${os}_${arch}.tar.gz"
base="https://github.com/$REPO/releases/download/v$version"

tmp="$(mktemp -d 2>/dev/null || mktemp -d -t gta-install)" || die "could not create a temporary directory"
cleanup() { rm -rf "$tmp"; }
trap cleanup EXIT
trap 'cleanup; exit 130' INT
trap 'cleanup; exit 143' TERM

say "downloading gta v$version for $os/$arch"
download "$base/$archive" "$tmp/$archive" || die "download failed: $base/$archive (does release v$version exist?)"
download "$base/checksums.txt" "$tmp/checksums.txt" || die "download failed: $base/checksums.txt"

# ---- verify ----------------------------------------------------------------

sha256_of() {
	if command -v sha256sum >/dev/null 2>&1; then
		sha256sum "$1" | awk '{print $1}'
	elif command -v shasum >/dev/null 2>&1; then
		shasum -a 256 "$1" | awk '{print $1}'
	elif command -v openssl >/dev/null 2>&1; then
		openssl dgst -sha256 -r "$1" | awk '{print $1}'
	else
		die "need sha256sum, shasum or openssl to verify the download"
	fi
}

expected="$(awk -v f="$archive" '$2 == f || $2 == "*" f { print $1 }' "$tmp/checksums.txt")"
[ -n "$expected" ] || die "checksums.txt has no entry for $archive"
[ "$(printf '%s\n' "$expected" | wc -l | tr -d ' ')" = 1 ] || die "checksums.txt lists $archive more than once"
expected="$(printf '%s' "$expected" | tr 'A-F' 'a-f')"
printf '%s\n' "$expected" | grep -Eq '^[0-9a-f]{64}$' || die "checksums.txt has a malformed entry for $archive"

actual="$(sha256_of "$tmp/$archive")"
[ "$actual" = "$expected" ] || die "checksum mismatch for $archive
  expected $expected
  got      $actual
nothing was installed"
say "checksum verified ($expected)"

# ---- install ---------------------------------------------------------------

mkdir "$tmp/x"
tar -xzf "$tmp/$archive" -C "$tmp/x" "$BINARY" || die "could not unpack $BINARY from $archive"
[ -f "$tmp/x/$BINARY" ] && [ ! -L "$tmp/x/$BINARY" ] || die "$archive does not contain a regular $BINARY file"

mkdir -p "$install_dir" || die "could not create $install_dir"
# Copy next to the target, then rename: a running gta is never half-written.
staged="$install_dir/.$BINARY.install.$$"
cp "$tmp/x/$BINARY" "$staged" || die "could not write to $install_dir"
chmod 0755 "$staged" || {
	rm -f "$staged"
	die "could not make $staged executable"
}
mv -f "$staged" "$install_dir/$BINARY" || {
	rm -f "$staged"
	die "could not install to $install_dir/$BINARY"
}

say "installed $install_dir/$BINARY"
"$install_dir/$BINARY" --version >&2 2>/dev/null || say "warning: $install_dir/$BINARY did not run"

case ":${PATH:-}:" in
*":$install_dir:"*) ;;
*) say "note: $install_dir is not on your PATH; add it, e.g. export PATH=\"$install_dir:\$PATH\"" ;;
esac
