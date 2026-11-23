#!/bin/bash
set -euo pipefail
source "$(cd "$(dirname "$0")" && pwd)/version.sh"
for value in 0.0.0 1.2.3 1.2.3+build 1.2.3-rc.1+build.007 v1.2.3-alpha-beta+sha-abc; do
  linkgate_macos_version "$value"
  test "$VERSION" = "${value#v}"
  test "$BUNDLE_VERSION" = "${VERSION%%[-+]*}"
done
for value in 01.2.3 1.02.3 1.2 1.2.3.4 1.2.3+ 1.2.3- 1.2.3-01 1.2.3-rc..1 1.2.3+build..1; do
  if linkgate_macos_version "$value" >/dev/null 2>&1; then
    printf 'Accepted invalid SemVer: %s\n' "$value" >&2
    exit 1
  fi
done
printf 'macOS SemVer tests passed.\n'
