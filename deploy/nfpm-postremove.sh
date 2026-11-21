#!/bin/sh
set -e
if command -v systemctl >/dev/null 2>&1; then
  systemctl daemon-reload >/dev/null 2>&1 || true
fi
# Keep /etc/linkgate/config.yaml and /var/lib/linkgate (config, token) by default.
