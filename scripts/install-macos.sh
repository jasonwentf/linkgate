#!/bin/bash
# 在 macOS 上安装内核与托盘的 LaunchAgent。
# 用法：./scripts/install-macos.sh [安装目录，默认 ~/.linkgate]
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
PREFIX="${1:-$HOME/.linkgate}"
LA="$HOME/Library/LaunchAgents"

mkdir -p "$PREFIX/bin" "$LA" "$HOME/Library/Logs"

echo "== 构建 =="
(cd "$ROOT" && go build -trimpath -o "$PREFIX/bin/linkgate" ./cmd/linkgate)
if go build -trimpath -o "$PREFIX/bin/linkgate-tray" ./cmd/linkgate-tray 2>/dev/null; then
  echo "托盘已构建"
else
  echo "托盘构建失败（可能缺 Xcode 命令行工具），只安装内核"
fi
if [ ! -f "$PREFIX/config.yaml" ]; then
  cp "$ROOT/configs/linkgate.example.yaml" "$PREFIX/config.yaml"
  chmod 600 "$PREFIX/config.yaml"
  echo "已写入示例配置 $PREFIX/config.yaml，请先改成你自己的线路再启动"
fi

echo "== 安装内核 LaunchAgent =="
cat > "$LA/dev.linkgate.kernel.plist" <<PLIST
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key><string>dev.linkgate.kernel</string>
  <key>ProgramArguments</key>
  <array>
    <string>$PREFIX/bin/linkgate</string>
    <string>-config</string>
    <string>$PREFIX/config.yaml</string>
  </array>
  <key>RunAtLoad</key><true/>
  <key>KeepAlive</key><true/>
  <key>ProcessType</key><string>Background</string>
  <key>StandardOutPath</key><string>$HOME/Library/Logs/linkgate.kernel.log</string>
  <key>StandardErrorPath</key><string>$HOME/Library/Logs/linkgate.kernel.log</string>
</dict>
</plist>
PLIST

if [ -x "$PREFIX/bin/linkgate-tray" ]; then
  echo "== 安装托盘 LaunchAgent =="
  cat > "$LA/dev.linkgate.tray.plist" <<PLIST
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key><string>dev.linkgate.tray</string>
  <key>ProgramArguments</key>
  <array>
    <string>$PREFIX/bin/linkgate-tray</string>
  </array>
  <key>EnvironmentVariables</key>
  <dict>
    <key>LINKGATE_CONFIG</key><string>$PREFIX/config.yaml</string>
  </dict>
  <key>RunAtLoad</key><true/>
  <key>KeepAlive</key><false/>
  <key>LimitLoadToSessionType</key><string>Aqua</string>
  <key>StandardOutPath</key><string>$HOME/Library/Logs/linkgate.tray.log</string>
  <key>StandardErrorPath</key><string>$HOME/Library/Logs/linkgate.tray.log</string>
</dict>
</plist>
PLIST
fi

launchctl unload "$LA/dev.linkgate.kernel.plist" 2>/dev/null || true
launchctl load "$LA/dev.linkgate.kernel.plist"
if [ -f "$LA/dev.linkgate.tray.plist" ]; then
  launchctl unload "$LA/dev.linkgate.tray.plist" 2>/dev/null || true
  launchctl load "$LA/dev.linkgate.tray.plist"
fi

echo "完成。检查：curl -s http://127.0.0.1:8765/health"
