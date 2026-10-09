#!/usr/bin/env bash
# Rewrites the macOS app screenshots in dev/ from a demo copy of the app: a
# Debug build pointed at dev/fake-redash.py and a throwaway config, on ports
# that leave an installed copy alone. It shares the installed app's bundle id,
# since macOS only shows a menu bar item it already knows, so every preference
# the app writes at launch is shadowed by a launch argument, which lives only
# in memory. The terminal running this needs Screen Recording permission.
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
WORK="$HOME/.redash-wire-demo"
DERIVED="$ROOT/build/ScreenshotsDerivedData"
PORT=18080

cleanup() {
  [[ -n "${APP_PID:-}" ]] && kill "$APP_PID" 2>/dev/null || true
  [[ -n "${REDASH_PID:-}" ]] && kill "$REDASH_PID" 2>/dev/null || true
  rm -rf "$WORK"
}
trap cleanup EXIT

mkdir -p "$WORK"
make -C "$ROOT" build >/dev/null
xcodebuild -project "$ROOT/macos/RedashWire.xcodeproj" -scheme RedashWire -configuration Debug \
  -destination 'generic/platform=macOS' -derivedDataPath "$DERIVED" CODE_SIGNING_ALLOWED=NO build >/dev/null
APP_BIN="$DERIVED/Build/Products/Debug/RedashWire.app/Contents/MacOS/RedashWire"

python3 -I "$ROOT/dev/fake-redash.py" "$PORT" &
REDASH_PID=$!

cat > "$WORK/config.yaml" <<YAML
postgres_listen_addr: "127.0.0.1:25432"
mysql_listen_addr: "127.0.0.1:23306"
default_profile: analytics
profiles:
  analytics:
    redash_url: "http://localhost:$PORT"
    api_key: "demo"
  production:
    redash_url: "http://127.0.0.1:$PORT"
    api_key: "demo"
    read_only: true
YAML

# Prints the id of the demo's largest window on the given layer: 101 for an
# open menu, 0 for an ordinary window.
cat > "$WORK/window-id.swift" <<'SWIFT'
import CoreGraphics
let pid = Int32(CommandLine.arguments[1])!, layer = Int(CommandLine.arguments[2])!
let windows = CGWindowListCopyWindowInfo(.optionOnScreenOnly, kCGNullWindowID) as! [[String: Any]]
let match = windows
    .filter { $0[kCGWindowOwnerPID as String] as? Int32 == pid && $0[kCGWindowLayer as String] as? Int == layer }
    .max { a, b in
        let area = { (w: [String: Any]) -> Double in
            let r = w[kCGWindowBounds as String] as! [String: Double]
            return r["Width"]! * r["Height"]!
        }
        return area(a) < area(b)
    }
if let id = match?[kCGWindowNumber as String] as? Int { print(id) }
SWIFT

shoot() {
  local name="$1" layer="$2"
  shift 2
  REDASH_WIRE_BINARY="$ROOT/bin/redash-wire" REDASH_WIRE_CONFIG="$WORK/config.yaml" \
    "$APP_BIN" -readOnlyProfiles '{}' -checksForUpdatesAutomatically NO -notificationsEnabled NO "$@" &
  APP_PID=$!
  sleep 8
  local id
  id="$(swift "$WORK/window-id.swift" "$APP_PID" "$layer")"
  screencapture -x -l"$id" "$ROOT/dev/$name.png"
  echo "    dev/$name.png"
  kill "$APP_PID"
  wait "$APP_PID" 2>/dev/null || true
  APP_PID=""
}

echo "==> Capturing"
shoot menu 101 -present menu
shoot settings 0 -present settings -settingsTab profiles
