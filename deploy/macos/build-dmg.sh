#!/bin/bash
# Usage: build-dmg.sh [version [app-path [output-dir]]]
# A no-argument invocation packages dist/macos/Linkgate.app; VERSION/APP_PATH/OUTPUT_DIR override it.
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
OUTPUT_DIR="${3:-${OUTPUT_DIR:-$ROOT/dist/macos}}"
APP_PATH="${2:-${APP_PATH:-$OUTPUT_DIR/Linkgate.app}}"
VERSION="${1:-${VERSION:-}}"
if [ ! -d "$APP_PATH/Contents/MacOS" ]; then
  printf 'Missing app bundle: %s\n' "$APP_PATH" >&2
  exit 1
fi
if [ -z "$VERSION" ]; then
  VERSION="$(/usr/libexec/PlistBuddy -c 'Print :LinkgateReleaseVersion' "$APP_PATH/Contents/Info.plist" 2>/dev/null || /usr/libexec/PlistBuddy -c 'Print :CFBundleShortVersionString' "$APP_PATH/Contents/Info.plist")"
fi
source "$ROOT/deploy/macos/version.sh"
linkgate_macos_version "$VERSION"
codesign --verify --deep --strict "$APP_PATH"
mkdir -p "$OUTPUT_DIR"
OUTPUT_DIR="$(cd "$OUTPUT_DIR" && pwd)"
DMG="$OUTPUT_DIR/Linkgate-$VERSION.dmg"
if [ -e "$DMG" ]; then
  printf 'Refusing to overwrite %s; use a new OUTPUT_DIR.\n' "$DMG" >&2
  exit 1
fi
WORK="$(mktemp -d "${TMPDIR:-/tmp}/linkgate-dmg.XXXXXX")"
chmod 700 "$WORK"
mkdir "$WORK/payload"
ditto "$APP_PATH" "$WORK/payload/Linkgate.app"
ln -s /Applications "$WORK/payload/Applications"
hdiutil create -volname Linkgate -srcfolder "$WORK/payload" -fs HFS+ -format UDRW "$WORK/Linkgate-rw.dmg"
hdiutil convert "$WORK/Linkgate-rw.dmg" -format UDZO -imagekey zlib-level=9 -o "$DMG"
hdiutil verify "$DMG"
printf 'DMG: %s\nBuild staging (retained): %s\n' "$DMG" "$WORK"
