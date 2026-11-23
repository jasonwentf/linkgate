#!/bin/bash
# Shared SemVer validation; Apple bundle version fields use the numeric core only.
linkgate_macos_version() {
  local value="${1#v}" without_metadata prerelease identifier
  local pattern='^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(-[0-9A-Za-z-]+(\.[0-9A-Za-z-]+)*)?(\+[0-9A-Za-z-]+(\.[0-9A-Za-z-]+)*)?$'
  if [[ ! "$value" =~ $pattern ]]; then
    printf 'Invalid semantic version: %s\n' "$value" >&2
    return 1
  fi
  without_metadata="${value%%+*}"
  if [[ "$without_metadata" == *-* ]]; then
    prerelease="${without_metadata#*-}"
    local identifiers=()
    IFS=. read -r -a identifiers <<< "$prerelease"
    for identifier in "${identifiers[@]}"; do
      if [[ "$identifier" =~ ^[0-9]+$ ]] && [[ "$identifier" == 0* ]] && [ "${#identifier}" -gt 1 ]; then
        printf 'Invalid numeric prerelease identifier: %s\n' "$identifier" >&2
        return 1
      fi
    done
  fi
  VERSION="$value"
  BUNDLE_VERSION="${without_metadata%%-*}"
}
