#!/bin/sh
set -e

# Create the linkgate system user (no login) and its state dir.
if ! id linkgate >/dev/null 2>&1; then
  useradd --system --home /var/lib/linkgate --shell /usr/sbin/nologin linkgate 2>/dev/null || true
fi
mkdir -p /etc/linkgate /var/lib/linkgate
chown linkgate:linkgate /var/lib/linkgate 2>/dev/null || true
if [ -f /etc/linkgate/config.yaml ]; then
  chown root:linkgate /etc/linkgate/config.yaml 2>/dev/null || true
  chmod 0640 /etc/linkgate/config.yaml 2>/dev/null || true
fi

if command -v systemctl >/dev/null 2>&1; then
  systemctl daemon-reload >/dev/null 2>&1 || true
fi
