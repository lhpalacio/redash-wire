#!/bin/sh
# Installs the latest redash-wire release binary.
#   curl -fsSL https://raw.githubusercontent.com/lhpalacio/redash-wire/main/install.sh | sh
#
# Options (environment variables):
#   VERSION  release tag to install, e.g. v1.2.0 or 1.2.0 (default: latest)
#   BIN_DIR  install directory (default: /usr/local/bin)
set -eu

REPO="lhpalacio/redash-wire"
BIN_DIR="${BIN_DIR:-/usr/local/bin}"

err() {
	printf 'install.sh: %s\n' "$1" >&2
	exit 1
}

have() {
	command -v "$1" >/dev/null 2>&1
}

fetch() {
	if have curl; then
		curl -fsSL "$1"
	elif have wget; then
		wget -qO- "$1"
	else
		err "curl or wget is required"
	fi
}

case "$(uname -s)" in
Linux) os="linux" ;;
Darwin) os="darwin" ;;
*) err "unsupported OS '$(uname -s)'. Releases are built for Linux and macOS only" ;;
esac

case "$(uname -m)" in
x86_64 | amd64) arch="amd64" ;;
arm64 | aarch64) arch="arm64" ;;
*) err "unsupported architecture '$(uname -m)'" ;;
esac

tag="${VERSION:-}"
if [ -z "$tag" ]; then
	tag=$(fetch "https://api.github.com/repos/$REPO/releases/latest" |
		sed -n 's/.*"tag_name": *"\([^"]*\)".*/\1/p' | head -n1)
	[ -n "$tag" ] || err "could not determine the latest release tag"
fi

# Release tags carry a leading v; accept VERSION given with or without it.
case "$tag" in
v*) ;;
*) tag="v$tag" ;;
esac

version="${tag#v}"
archive="redash-wire_${version}_${os}_${arch}.tar.gz"
base_url="https://github.com/$REPO/releases/download/$tag"

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

echo "Downloading redash-wire $tag ($os/$arch)..."
fetch "$base_url/$archive" >"$tmp/$archive"
fetch "$base_url/checksums.txt" >"$tmp/checksums.txt"

# The verifier's exit status alone is not enough: some sha256sum builds exit 0
# on empty input, so a checksums.txt without this archive would pass. Require
# the line first, then require the verifier to say OK for this file.
(
	cd "$tmp"
	expected=$(grep " $archive\$" checksums.txt || true)
	[ -n "$expected" ] || err "checksums.txt has no entry for $archive"
	if have sha256sum; then
		result=$(printf '%s\n' "$expected" | sha256sum -c - 2>&1) || true
	elif have shasum; then
		result=$(printf '%s\n' "$expected" | shasum -a 256 -c - 2>&1) || true
	else
		err "sha256sum or shasum is required to verify the download"
	fi
	case "$result" in
	*"$archive: OK"*) ;;
	*) err "checksum verification failed for $archive: $result" ;;
	esac
)
echo "Checksum verified."

tar -xzf "$tmp/$archive" -C "$tmp" redash-wire

if [ -w "$BIN_DIR" ]; then
	install -m 0755 "$tmp/redash-wire" "$BIN_DIR/redash-wire"
elif have sudo; then
	echo "Installing to $BIN_DIR (requires sudo)..."
	sudo install -m 0755 "$tmp/redash-wire" "$BIN_DIR/redash-wire"
else
	err "$BIN_DIR is not writable and sudo is unavailable. Re-run with BIN_DIR=~/.local/bin (or another writable directory on your PATH)"
fi

echo "Installed $("$BIN_DIR/redash-wire" -version) to $BIN_DIR/redash-wire"
echo "Run 'redash-wire' to get started (first run launches the setup wizard)."
