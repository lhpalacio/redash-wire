#!/bin/sh
# Installs the latest redash-wire release binary.
#   curl -fsSL https://raw.githubusercontent.com/lhpalacio/redash-wire/main/install.sh | sh
#
# Options (environment variables):
#   VERSION  release tag to install, e.g. v1.2.0 (default: latest)
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

version="${tag#v}"
archive="redash-wire_${version}_${os}_${arch}.tar.gz"
base_url="https://github.com/$REPO/releases/download/$tag"

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

echo "Downloading redash-wire $tag ($os/$arch)..."
fetch "$base_url/$archive" >"$tmp/$archive"
fetch "$base_url/checksums.txt" >"$tmp/checksums.txt"

(
	cd "$tmp"
	if have sha256sum; then
		grep " $archive\$" checksums.txt | sha256sum -c - >/dev/null
	elif have shasum; then
		grep " $archive\$" checksums.txt | shasum -a 256 -c - >/dev/null
	else
		err "sha256sum or shasum is required to verify the download"
	fi
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
