#!/bin/sh
set -e
if command -v systemctl >/dev/null 2>&1; then
  systemctl stop linkgate >/dev/null 2>&1 || true
  systemctl disable linkgate >/dev/null 2>&1 || true
fi
