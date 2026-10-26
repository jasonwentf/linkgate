#!/bin/bash
# 卸载 macOS LaunchAgent（不删除配置文件与日志）。
set -euo pipefail
LA="$HOME/Library/LaunchAgents"
for f in dev.linkgate.tray.plist dev.linkgate.kernel.plist; do
  if [ -f "$LA/$f" ]; then
    launchctl unload "$LA/$f" 2>/dev/null || true
    rm -f "$LA/$f"
    echo "已卸载 $f"
  fi
done
echo "配置文件与令牌保留在 ~/.linkgate，需要时手动删除。"
