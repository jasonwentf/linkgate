# Linkgate

A local failover proxy for AI backends. Point all your tools at one address; Linkgate tries multiple routes and switches when one goes down.

[中文](README.md)

## The problem

You run several AI tools (Claude Code, Cursor, OpenCode, etc.) against the same backend. Each has the backend URL hardcoded in its config. When that route breaks, every tool fails at once, and you have to update each one by hand.

Linkgate listens on a local port. All tools point there. It tries routes in priority order and falls back to the next one if a connection cannot be established. The tools never notice the switch.

## Three invariants

1. **Transparent forwarding.** Request body, Authorization header, model fields, and all other headers pass through untouched.
2. **No replay after connection.** If a TCP connection was established (past DNS, dial, TLS), Linkgate returns 502 on failure instead of retrying. It cannot prove the upstream did not receive the request, so replaying risks double billing.
3. **No token accounting.** Linkgate tracks request counts, failures, failovers, and latency. It never parses request bodies.

## Quick start

Requires Go 1.27+.

```bash
# Build
make build
# Or: go build -trimpath -o bin/linkgate ./cmd/linkgate

# Generate a sample config (defaults to ~/.linkgate/config.yaml)
bin/linkgate -init

# Edit the config with your own routes
vi ~/.linkgate/config.yaml

# Run
bin/linkgate

# Verify
curl http://127.0.0.1:8765/health
```

`-init` writes a commented sample config. It refuses to overwrite an existing file.

Other flags: `-config <path>` for a custom config path, `-check` to validate without starting, `-doctor` to probe every route once, `-version` to print version.

## Configuration

See [`configs/linkgate.example.yaml`](configs/linkgate.example.yaml) for every field with comments. Key sections below.

### Route groups

```yaml
groups:
  - name: axonhub
    listen: 127.0.0.1:8765
    upstreams:
      - name: relay
        url: https://your-relay.example.com
        priority: 5
        host: your-relay.example.com
      - name: tailscale
        url: http://100.x.x.x:8090
        priority: 10
      - name: zerotier
        url: http://10.x.x.x:8090
        priority: 20
```

Priority is an integer. Lower numbers go first. Same priority follows file order. Each upstream can set its own `host`, `headers` (e.g. CF Access service tokens), and independent timeouts.

### Timeouts

```yaml
http:
  connect_timeout: 2s          # Fail fast if a route is down
  tls_handshake_timeout: 5s
  upload_idle_timeout: 60s      # No upload progress for this long = stuck (progress, not total time)
  response_header_timeout: 900s # Models can think for minutes; don't treat that as failure
  stream_idle_timeout: 900s     # No data for this long mid-stream = stuck
```

These can be overridden per group and adjusted at runtime through the dashboard.

### Retry

```yaml
retry:
  max_attempts: 3
  mode: connect    # Only switch routes before the connection is established
  backoff: 150ms
```

`mode: connect` is the default and only switches routes when no request header has been written yet; `none` disables switching.

### Health checks

```yaml
health:
  enabled: true
  interval: 15s
  timeout: 3s
  fail_threshold: 2     # 2 consecutive probe failures → marked unhealthy
  recover_threshold: 2  # 2 consecutive successes → restored
  probe_path: /health
```

Upstream 5xx responses do not count as route failures — that is a backend problem, not a route problem. If a status code can only come from a specific route (for example Cloudflare's 524), declare it on that route with `failure_statuses: [524]`.

Each route can also set `max_body_bytes`: requests larger than that never take that route (CF returns 524 after about 100 seconds, so large requests should not be routed there).

### Request body buffering

The full request body is read before forwarding so it can be replayed on a different route. Bodies under 8 MiB stay in memory; larger ones spill to a temp file. The hard limit is 128 MiB.

## Cross-platform

The core is pure Go with no cgo. `make dist` cross-compiles five targets in one pass:

- `darwin/arm64`, `darwin/amd64`
- `linux/amd64`, `linux/arm64`
- `windows/amd64`

The tray (`linkgate-tray`) is an optional GUI shell for macOS and Windows. The macOS build uses cgo (Xcode command-line tools required); the Windows systray implementation is pure Go, so the `.exe` can be cross-compiled from macOS. No tray on Linux.

The core runs standalone. Open the dashboard in a browser.

## Dashboard and tray

The dashboard is an embedded single-page HTML file (vanilla JS, no framework), served by the admin interface:

```
http://127.0.0.1:8766/__linkgate/ui
```

It shows per-route status, latency, request counts, and an event log. You can edit the config, trigger a probe, or toggle routes on and off from here.

The admin interface listens on `127.0.0.1:8766` by default, loopback only. On first start it writes a token to `~/.linkgate/admin.token` (mode 0600). API requests require the `X-Linkgate-Token` header.

Key endpoints:

| Method | Path | Description |
|---|---|---|
| GET | `/__linkgate/v1/status` | Version, uptime, per-route state and counters |
| GET | `/__linkgate/v1/events?after=N` | Incremental events |
| PUT | `/__linkgate/v1/config` | Submit YAML; validated, written to disk, hot-reloaded |
| POST | `/__linkgate/v1/probe?group=&upstream=` | Probe a route immediately |
| POST | `/__linkgate/v1/toggle?group=&upstream=&enabled=` | Enable/disable a route at runtime (not persisted) |

On Unix, sending `SIGHUP` also triggers a config reload.

The tray does two things: shows current status in the system tray, and opens the dashboard on click.

## Deployment

### macOS — LaunchAgent

```bash
./scripts/install-macos.sh
```

Builds the core and tray (tray needs Xcode CLI tools; skipped if unavailable), registers LaunchAgents, starts on login. Copies the sample config to `~/.linkgate/config.yaml` on first run.

Uninstall: `./scripts/uninstall-macos.sh` (keeps config and token).

### Linux — systemd

```bash
make build
sudo cp bin/linkgate /usr/local/bin/
sudo mkdir -p /etc/linkgate /var/lib/linkgate
sudo cp configs/linkgate.example.yaml /etc/linkgate/config.yaml
# Edit the config
sudo vi /etc/linkgate/config.yaml

sudo cp deploy/linkgate.service /etc/systemd/system/
sudo systemctl daemon-reload
sudo systemctl enable --now linkgate
```

### Windows — Scheduled Task

```powershell
# Generate config first
.\dist\linkgate-windows-amd64.exe -init
# Edit %USERPROFILE%\.linkgate\config.yaml, then:
.\deploy\windows\install.ps1
```

Registers a scheduled task (starts on login, restarts on crash) and a startup shortcut for the tray.

## Limitations

Linkgate is a transport-layer proxy. The following are out of scope:

- **Mid-stream disconnects.** Once the connection is up and bytes are flowing, no proxy can resume on a different connection. The harness must retry.
- **Upstream 5xx.** Backend errors are not route failures. Linkgate does not switch routes on 5xx.
- **Cloudflare 100-second timeout (524).** When a route goes through CF and model thinking exceeds 100 seconds, CF returns 524. The workaround is a route that does not go through CF.
- **Model routing, protocol translation, token billing.** Linkgate does not parse request content. It does not know what a model is. It forwards bytes.
- **System proxy.** The core ignores system proxy settings to avoid traffic looping through tools like Clash. If a route needs a proxy, handle it at the URL level.

## Smoke test

```bash
LINKGATE_UPSTREAM=http://<your-upstream>:8090 ./scripts/smoke-test.sh
```

Uses separate ports (18765/18766) and does not touch your production config. Verifies automatic failover when the first route is dead, and correct pass-through of a 9 MB request body.

## License

<!-- TODO: confirm and add license -->
