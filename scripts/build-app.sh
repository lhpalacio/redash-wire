#!/usr/bin/env bash
set -euo pipefail

# Builds RedashWire.app with a universal redash-wire embedded in it.
#
# The app spawns the binary it ships with, so the two can never disagree about
# the JSON contract between them. That only holds if the binary actually lands
# inside the bundle, which is what this script exists to guarantee.

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
BUILD_DIR="${BUILD_DIR:-$ROOT/build}"
DERIVED="$BUILD_DIR/DerivedData"
APP="$BUILD_DIR/RedashWire.app"
VERSION="${VERSION:-$(git -C "$ROOT" describe --tags --always --dirty 2>/dev/null || echo dev)}"

require_universal () {
  local path="$1" label="$2"
  local info
  info="$(lipo -info "$path")"
  case "$info" in
    *arm64*x86_64*|*x86_64*arm64*) echo "    $label: universal" ;;
    *) echo "ERROR: $label is not universal: $info" >&2; exit 1 ;;
  esac
}

rm -rf "$APP"
mkdir -p "$BUILD_DIR"

echo "==> Building redash-wire (universal)"
for arch in arm64 amd64; do
  CGO_ENABLED=0 GOOS=darwin GOARCH="$arch" go build \
    -ldflags "-s -w -X main.version=$VERSION" \
    -o "$BUILD_DIR/redash-wire-$arch" "$ROOT/cmd/redash-wire"
done
lipo -create -output "$BUILD_DIR/redash-wire" \
  "$BUILD_DIR/redash-wire-arm64" "$BUILD_DIR/redash-wire-amd64"
rm -f "$BUILD_DIR/redash-wire-arm64" "$BUILD_DIR/redash-wire-amd64"

echo "==> Building RedashWire.app"
# generic/platform=macOS builds every arch in ARCHS. A concrete destination
# silently narrows the build to whichever arch this machine happens to run,
# which is how you ship an "x86_64 too" app that only has arm64 in it.
xcodebuild_args=(
  -project "$ROOT/macos/RedashWire.xcodeproj"
  -scheme RedashWire
  -configuration Release
  -destination 'generic/platform=macOS'
  -derivedDataPath "$DERIVED"
)
# MARKETING_VERSION has to look like a version, because the app compares it
# against the latest GitHub tag to decide whether an update exists. An untagged
# checkout describes as a bare commit sha; requiring a dot keeps a sha that
# happens to start with a digit from being stamped in as a version number.
case "$VERSION" in
  [0-9]*.[0-9]*|v[0-9]*.[0-9]*) xcodebuild_args+=("MARKETING_VERSION=${VERSION#v}") ;;
esac
xcodebuild "${xcodebuild_args[@]}" build

cp -R "$DERIVED/Build/Products/Release/RedashWire.app" "$APP"

echo "==> Embedding redash-wire"
# Xcode only creates Resources/ when the target has resources, and this one has
# none of its own.
mkdir -p "$APP/Contents/Resources"
cp "$BUILD_DIR/redash-wire" "$APP/Contents/Resources/redash-wire"
chmod +x "$APP/Contents/Resources/redash-wire"

echo "==> Signing"
# Sign the nested binary before the bundle that seals it. --deep is deprecated
# and signs in the wrong order, so it is deliberately not used here.
codesign --force --sign - "$APP/Contents/Resources/redash-wire"
codesign --force --sign - "$APP"
codesign --verify --strict "$APP"

echo "==> Verifying"
require_universal "$APP/Contents/MacOS/RedashWire" "RedashWire"
require_universal "$APP/Contents/Resources/redash-wire" "redash-wire"
echo "    signature: $(codesign -dv "$APP" 2>&1 | grep -o 'flags=[^ ]*')"

echo
echo "==> Built $APP"
