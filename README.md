# Linkgate

[![中文文档](https://img.shields.io/badge/文档-中文-red.svg)](README.zh.md)
![License: MIT](https://img.shields.io/badge/License-MIT-blue.svg)
![Go](https://img.shields.io/badge/Go-1.27%2B-00ADD8?logo=go&logoColor=white)
![macOS](https://img.shields.io/badge/macOS-000000?logo=apple&logoColor=white)
![Windows](https://img.shields.io/badge/Windows-0078D6?logo=windows11&logoColor=white)
![Linux](https://img.shields.io/badge/Linux-FCC624?logo=linux&logoColor=black)

Linkgate is a transparent HTTP reverse proxy that runs locally. It gives every client tool on your machine one fixed local address, and silently fails over between multiple upstream routes in the background.

![Linkgate dashboard](docs/images/dashboard.png)

When you run tools like Claude Code, Cursor, Codex, or OpenCode against a remote inference service, that backend usually sits behind several paths at once — Tailscale, ZeroTier, a Cloudflare Tunnel, a self-hosted FRP relay. Hardcode any one of those addresses into every tool, and the moment that tunnel flaps or drops, everything errors at once and you end up editing configs by hand. Linkgate listens on a fixed local port instead. Clients point at localhost; it picks the first reachable route and forwards. Clients never see the switch.

Everything it forwards stays transparent. It moves bytes without touching the request body, the `Authorization` header, or model parameters, and it leaves gzip handling alone by default. It does not parse traffic to count tokens — it only counts requests, failures, failovers, and latency. To prevent double-billing or duplicated work upstream, it only fails over before a single byte has been sent. Once a connection is established, a stalled header or a broken stream returns `502`; it never replays a request on another route.

## How it works

A **service** (`groups` in the config) is a local listen port plus the backend it forwards to. You can run several, one port each. Under every service you attach **routes** (`upstreams`) — the paths that reach that backend — each tagged with a **channel** (`kind`: Tailscale, ZeroTier, Cloudflare Tunnel, FRP, direct, or your own label). When the top route can't establish a connection, Linkgate moves down the priority list.

Timeouts are layered so failover stays fast without punishing slow inference: connect `2s`, TLS handshake `5s` so a dead route fails quickly; upload idle `60s` measured on actual progress, so a big request body still finishes on a slow route; response-header and stream idle `900s` for thinking models that take minutes to emit a first byte; and `reconnect_after_idle` (default `60s`) redials pooled connections that went stale, turning a silent reuse failure into a clean connect failure that can legitimately trigger failover.

## Features

**Multi-route failover with request buffering.** Routes sort by priority, lowest number first. The request body is fully buffered on ingress (under `8 MiB` in memory, above that into a temp file, `128 MiB` cap) so a failed connect can be replayed to the next route intact. Per-route guards handle channel quirks — e.g. Cloudflare Tunnel's ~100s edge timeout can be excluded from large bodies via `max_body_bytes`, and `failure_statuses: [524]` marks that status as a route fault. Ordinary upstream `5xx` is treated as the backend's own problem, not a route failure.

**Reachability-aware health probes.** Background probes (`http` or `tcp_connect`) plus real-request feedback track route health; routes past the failure threshold drop out of rotation, with unhealthy routes kept as last-resort fallbacks. A probe classifies results as `healthy`, `auth`, `notfound`, `server_error`, or `unreachable` — a `401` or `404` counts as *connected*, not broken, so an auth-gated endpoint doesn't look like a dead route.

**UA rules (`ua_rules`).** Per service, ordered rules pin matching clients to a preferred route: `[{match, line, comment}]`, with substring / `exact:` / `prefix:` / `regex:` matching (case-insensitive). A pin is a preference, not a lock — an unhealthy target is still tried first and falls back on a clean connect failure. Rules are frozen per request, so in-flight requests keep their original target across config reloads.

**Dashboard and caller→route flows.** A zero-dependency single-page panel (embedded HTML + vanilla JS) serves the UI. The dashboard shows every service with live route, health, and counters, sortable by config order / recency / volume. Each service page shows route cards plus a caller→route flow graph — callers are normalized to friendly classes (Claude Code, Codex, OpenCode, ZCode, curl, Python, …) and line width tracks request volume. Raw `User-Agent` strings are never stored.

![Dashboard](docs/images/dashboard.png)

**Structured editing with transactional saves.** Add, edit, delete services and routes through forms, or drag to reorder — no YAML hand-editing needed (a full-YAML editor remains under Settings → advanced). Every save validates, stages new listeners, writes a `.bak`, then atomically swaps the config; a failed preflight leaves the old runtime untouched.

**Bottom drawer with dual event rings.** `⌘J` / `Ctrl+J` or a drag handle opens a global event and runtime drawer. Lifecycle events (failover, health, config) live in a separate ring from request logs, so a burst of request errors can't flush important entries.

**Grouped settings with honest effect labels.** Settings are grouped (events & logs, health probe, retry, concurrency, HTTP timeouts, startup resources) with per-field labels: hot, next cycle, or restart-required. Startup resources save but only apply on restart.

**Pure Go, cross-platform.** The kernel is dependency-free Go (no cgo): one static binary for macOS (arm64/amd64), Linux (amd64/arm64), Windows (amd64). An optional tray (`linkgate-tray`) shows the active route per service in the macOS menu bar / Windows tray.

![Service page with flow graph](docs/images/service.png)

## Getting started

Requires Go 1.27.1+.

```bash
make build                 # build kernel to bin/linkgate
./bin/linkgate -init       # write ~/.linkgate/config.yaml (won't overwrite)
./bin/linkgate -check      # validate config
./bin/linkgate -doctor     # probe every route, no listeners
./bin/linkgate             # run
```

The panel opens at `http://127.0.0.1:8766/__linkgate/ui`. A `0600` admin token is generated at `~/.linkgate/admin.token` on first run. `make dist` cross-compiles all platform binaries into `bin/dist/`.

## Config example

```yaml
version: 1
admin:
  listen: 127.0.0.1:8766

groups:
  - name: axonhub
    listen: 127.0.0.1:8765
    protocol: http
    ua_rules:
      - match: claude-code
        line: tailscale
        comment: prefer Tailscale for Claude Code
    upstreams:
      - name: tailscale
        kind: ts
        url: http://100.64.0.10:8090
        priority: 10
      - name: zerotier
        kind: zt
        url: http://10.147.20.10:8090
        priority: 20
      - name: cloudflare
        kind: cf
        url: https://cf-tunnel.example.com
        priority: 30
        max_body_bytes: 2097152
        failure_statuses: [524]
```

See `configs/linkgate.example.yaml` for the full field reference.

## Deployment

### macOS

`./scripts/install-macos.sh` builds, initializes config, and registers user LaunchAgents for auto-start and respawn. `./scripts/uninstall-macos.sh` removes the agents while keeping `~/.linkgate/`.

### Windows

![Windows](https://img.shields.io/badge/Windows-0078D6?logo=windows11&logoColor=white)

For Windows 10/11 x64, use the prebuilt ZIP. No Go toolchain or administrator account is needed.

1. Download `linkgate_x.x.x_windows_amd64.zip` from [Releases](https://github.com/jasonwentf/linkgate/releases), for example `linkgate_0.2.2_windows_amd64.zip`.
2. Right-click the ZIP → **Extract All**. Open the extracted folder, not the ZIP preview.
3. Double-click `install.bat`. Wait for `Installed Linkgate` and press any key to close the window. The script copies both EXEs to `%LOCALAPPDATA%\Linkgate`, creates the default config only if missing, and starts the kernel and tray.
4. Click the colored Linkgate logo at the bottom right → **打开面板** (Open panel). If hidden, click the tray's **^** arrow first. Add your services and routes in the panel; the initial config contains example addresses, not usable backends.

The kernel runs as a current-user logon task with crash-restart. A Startup-folder shortcut starts the tray with the same config. Existing config/token files are kept. If SmartScreen blocks the unsigned download, check that it came from this repository and verify `checksums.txt` before choosing **More info → Run anyway**. Managed PCs may prohibit scripts or scheduled tasks; the installer reports the error rather than requesting elevation.

#### Check and install an update

Double-click `%LOCALAPPDATA%\Linkgate\check-update.bat` to compare the installed version with GitHub's latest stable release. This is an anonymous, opt-in request; it does not download or replace binaries. The kernel and tray also report their own versions with `-version`.

1. Download the new `linkgate_x.x.x_windows_amd64.zip` and extract it into a new folder.
2. Finish any active requests. Double-click `update.bat` **in the new folder**, then type `YES` when prompted.
3. The script validates the new config compatibility and matching kernel/tray versions, refuses downgrades, backs up the old EXEs under `%LOCALAPPDATA%\Linkgate\backups`, stops tray then kernel, replaces both, and restarts them. Config and tokens are not replaced. Windows task/process termination interrupts in-flight requests; it is not a graceful drain.
4. Open the panel from the tray and check your services. If replacement/startup fails, the updater attempts to restore both old EXEs and reports the backup path. For manual rollback, stop the Linkgate task and exit the tray, copy both backed-up EXEs to the installation folder, then start the task and the tray shortcut again.

From a source checkout, run `make dist`, then `powershell -NoProfile -ExecutionPolicy Bypass -File .\deploy\windows\install.ps1 -SourceDir .\bin\dist`. A custom config can be passed with `-Config`; its path is kept for updates. Updates work offline from an extracted ZIP; only `check-update.bat` needs internet access.

### Linux

Copy the binary, config, and `deploy/linkgate.service`, then `systemctl enable --now linkgate`. The unit uses `ProtectSystem=strict` with only `/etc/linkgate` and `/var/lib/linkgate` writable.

## Management API

Listens on `127.0.0.1:8766` only. All `/__linkgate/v1/*` endpoints (except `/health` and the panel) need the `X-Linkgate-Token` header; mutating calls also check `Origin`.

| Method | Path | Purpose |
| :--- | :--- | :--- |
| `GET` | `/__linkgate/ui` | Web panel |
| `GET` | `/__linkgate/v1/health` | Liveness (no token) |
| `GET` | `/__linkgate/v1/status` | Live service/route state |
| `GET` | `/__linkgate/v1/stats` | Counters, caller matrix, ring usage |
| `GET` | `/__linkgate/v1/events` | Event stream (`group`/`stream`/`kind`/`after`/`limit`) |
| `GET`/`PUT` | `/__linkgate/v1/settings` | Read / patch global settings (hot / next-cycle / restart) |
| `PUT` | `/__linkgate/v1/ui/order` | Persist service sort |
| `GET`/`PUT` | `/__linkgate/v1/config` | Read / submit full YAML |
| `POST` | `/__linkgate/v1/reload` | Reload from disk (≈ `SIGHUP`) |
| `POST`/`PUT`/`DELETE` | `/__linkgate/v1/groups[/<name>/upstreams]` | Service & route CRUD |
| `PUT` | `/__linkgate/v1/groups/<name>/upstreams/order` | Reorder routes |
| `POST` | `/__linkgate/v1/probe` | Probe one route |
| `POST` | `/__linkgate/v1/toggle` | Runtime enable/disable a route |

## Roadmap

- Generic TCP forwarding (SSH, database ports) — `protocol: tcp` is already reserved in the schema.
- Prebuilt packages for macOS, Windows, and major Linux distributions.
- HarmonyOS support (verification in progress).

## License

[MIT](LICENSE)
