#!/bin/bash
# Usage: build-app.sh [version [input-dir [output-dir]]]
# Inputs: darwin_{arm64,amd64}/{linkgate,linkgate-tray}, or GoReleaser tar.gz assets.
# VERSION, INPUT_DIR and OUTPUT_DIR also work, including in a no-argument CI invocation.
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
VERSION="${1:-${VERSION:-}}"
INPUT_DIR="${2:-${INPUT_DIR:-$ROOT/dist}}"
OUTPUT_DIR="${3:-${OUTPUT_DIR:-$ROOT/dist/macos}}"
if [ -z "$VERSION" ]; then
  VERSION="$(git -C "$ROOT" describe --tags --abbrev=0 2>/dev/null || printf '0.0.0')"
fi
source "$ROOT/deploy/macos/version.sh"
linkgate_macos_version "$VERSION"
for tool in lipo vtool sips iconutil codesign ditto; do
  command -v "$tool" >/dev/null || { printf 'Required macOS tool missing: %s\n' "$tool" >&2; exit 1; }
done
mkdir -p "$OUTPUT_DIR"
OUTPUT_DIR="$(cd "$OUTPUT_DIR" && pwd)"
APP="$OUTPUT_DIR/Linkgate.app"
if [ -e "$APP" ]; then
  printf 'Refusing to overwrite %s; use a new OUTPUT_DIR.\n' "$APP" >&2
  exit 1
fi
WORK="$(mktemp -d "${TMPDIR:-/tmp}/linkgate-app.XXXXXX")"
chmod 700 "$WORK"

for arch in arm64 amd64; do
  source="$INPUT_DIR/darwin_$arch"
  if [ ! -f "$source/linkgate" ] || [ ! -f "$source/linkgate-tray" ]; then
    archive="$INPUT_DIR/linkgate_${VERSION}_darwin_${arch}.tar.gz"
    if [ ! -f "$archive" ]; then
      printf 'Missing binaries or release archive for %s in %s\n' "$arch" "$INPUT_DIR" >&2
      exit 1
    fi
    source="$WORK/darwin_$arch"
    mkdir "$source"
    # Extract only the two root-level binaries into a fresh architecture directory.
    tar -xzf "$archive" -C "$source" linkgate linkgate-tray
  fi
  for binary in linkgate linkgate-tray; do
    if [ ! -f "$source/$binary" ] || [ -L "$source/$binary" ]; then
      printf 'Expected a regular binary: %s\n' "$source/$binary" >&2
      exit 1
    fi
  done
  if [ "$arch" = arm64 ]; then ARM64="$source"; else AMD64="$source"; fi
done

STAGED_APP="$WORK/Linkgate.app"
mkdir -p "$STAGED_APP/Contents/MacOS" "$STAGED_APP/Contents/Resources"
for binary in linkgate linkgate-tray; do
  lipo "$ARM64/$binary" -verify_arch arm64
  lipo "$AMD64/$binary" -verify_arch x86_64
  lipo -create "$ARM64/$binary" "$AMD64/$binary" -output "$STAGED_APP/Contents/MacOS/$binary"
  lipo "$STAGED_APP/Contents/MacOS/$binary" -verify_arch arm64
  lipo "$STAGED_APP/Contents/MacOS/$binary" -verify_arch x86_64
  chmod 755 "$STAGED_APP/Contents/MacOS/$binary"
done

# Match the actual deployment targets, including both slices of both binaries.
# CGO may otherwise silently inherit the runner's newest macOS version.
MINIMUM_OS=""
MINIMUM_SCORE=0
for binary in linkgate linkgate-tray; do
  for arch in arm64 x86_64; do
    while read -r field value rest; do
      if [ "$field" = minos ]; then
        IFS=. read -r major minor patch <<< "$value"
        score=$((major * 10000 + ${minor:-0} * 100 + ${patch:-0}))
        if [ "$score" -gt "$MINIMUM_SCORE" ]; then
          MINIMUM_SCORE="$score"
          MINIMUM_OS="$value"
        fi
      fi
    done < <(vtool -arch "$arch" -show-build "$STAGED_APP/Contents/MacOS/$binary")
  done
done
if [ -z "$MINIMUM_OS" ]; then
  printf 'Could not determine the macOS deployment target.\n' >&2
  exit 1
fi

ICONSET="$WORK/AppIcon.iconset"
mkdir "$ICONSET"
for size in 16 32 128 256 512; do
  sips -z "$size" "$size" "$ROOT/cmd/linkgate-tray/icons/logo.png" --out "$ICONSET/icon_${size}x${size}.png" >/dev/null
  double=$((size * 2))
  sips -z "$double" "$double" "$ROOT/cmd/linkgate-tray/icons/logo.png" --out "$ICONSET/icon_${size}x${size}@2x.png" >/dev/null
done
iconutil -c icns "$ICONSET" -o "$STAGED_APP/Contents/Resources/AppIcon.icns"

printf '%s\n' "<?xml version=\"1.0\" encoding=\"UTF-8\"?>
<!DOCTYPE plist PUBLIC \"-//Apple//DTD PLIST 1.0//EN\" \"http://www.apple.com/DTDs/PropertyList-1.0.dtd\">
<plist version=\"1.0\"><dict>
  <key>CFBundleName</key><string>Linkgate</string>
  <key>CFBundleDisplayName</key><string>Linkgate</string>
  <key>CFBundleIdentifier</key><string>dev.linkgate.app</string>
  <key>CFBundleExecutable</key><string>linkgate-tray</string>
  <key>CFBundlePackageType</key><string>APPL</string>
  <key>CFBundleIconFile</key><string>AppIcon</string>
  <key>CFBundleShortVersionString</key><string>$BUNDLE_VERSION</string>
  <key>CFBundleVersion</key><string>$BUNDLE_VERSION</string>
  <key>LinkgateReleaseVersion</key><string>$VERSION</string>
  <key>LSUIElement</key><true/>
  <key>LSMinimumSystemVersion</key><string>$MINIMUM_OS</string>
  <key>NSHighResolutionCapable</key><true/>
</dict></plist>" > "$STAGED_APP/Contents/Info.plist"
plutil -lint "$STAGED_APP/Contents/Info.plist"
# Ad-hoc signatures provide bundle integrity, not Developer ID trust or notarization.
for binary in linkgate linkgate-tray; do
  codesign --force -s - "$STAGED_APP/Contents/MacOS/$binary"
done
codesign --force --deep -s - "$STAGED_APP"
codesign --verify --deep --strict "$STAGED_APP"
ditto "$STAGED_APP" "$APP"
printf 'App: %s\nBuild staging (retained): %s\n' "$APP" "$WORK"
