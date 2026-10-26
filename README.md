# Linkgate

给所有 AI harness 一个固定的本地地址，背后挂多条线路，哪条通用哪条。

[English](README.en.md)

## 它解决什么问题

你有多个 AI 工具（Claude Code、Cursor、OpenCode……），都连同一个后端。地址写死在每个工具的配置里。那条线路断了，所有工具同时报错，你得逐个改地址。

Linkgate 在本地开一个端口，所有工具指向它。它按优先级尝试多条线路，第一条连不上自动换第二条。工具不需要知道发生了切换。

## 三个不变量

1. **透明转发。** 不改请求体，不改 Authorization，不改模型字段，不碰任何业务头。
2. **连接建立后不重发。** TCP 连接建立前失败（DNS、dial、TLS）才换线路。连接建立后失败直接返回 502，因为无法证明上游没收到请求，重发有重复计费风险。
3. **不统计 Token。** 只统计请求数、失败数、换线路次数和延迟。Linkgate 不解析请求体内容。

## 快速开始

需要 Go 1.27+。

```bash
# 构建
make build
# 或者：go build -trimpath -o bin/linkgate ./cmd/linkgate

# 生成示例配置（默认写入 ~/.linkgate/config.yaml）
bin/linkgate -init

# 按你的线路改写配置
vi ~/.linkgate/config.yaml

# 启动
bin/linkgate

# 验证
curl http://127.0.0.1:8765/health
```

`-init` 写入一份带注释的示例配置。文件已存在时拒绝覆盖。

其他参数：`-config <路径>` 指定配置文件，`-check` 只校验不启动，`-doctor` 逐条探测线路，`-version` 打印版本。

## 配置说明

完整字段和注释见 [`configs/linkgate.example.yaml`](configs/linkgate.example.yaml)，下面只讲关键项。

### 线路组

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

优先级是整数，越小越优先。同优先级按书写顺序。每条线路可以单独设 `host`、`headers`（比如 CF Access 的 Service Token）和独立超时。

### 超时

```yaml
http:
  connect_timeout: 2s          # 建连超时，不通则快速失败
  tls_handshake_timeout: 5s
  upload_idle_timeout: 60s      # 上传请求体多久没有进展算卡死（看进展，不看总时长）
  response_header_timeout: 900s # 模型思考可能很久，不能当故障
  stream_idle_timeout: 900s     # 流中途多久没数据算卡死
```

这些值可以在组级别覆盖，也可以在面板里运行时调整。

### 重试

```yaml
retry:
  max_attempts: 3
  mode: connect    # 只在连接未建立时换线路
  backoff: 150ms
```

`mode: connect` 是默认值，只对“请求头还没写出去”的失败换线路；`none` 关闭换线路。

### 健康检查

```yaml
health:
  enabled: true
  interval: 15s
  timeout: 3s
  fail_threshold: 2     # 连续 2 次探测失败 → 标记不可用
  recover_threshold: 2  # 连续 2 次成功 → 恢复
  probe_path: /health
```

上游 5xx 默认不计入线路故障——那是后端问题，不是线路问题。如果某个状态码只可能由某条线路产生（例如 Cloudflare 的 524），可以在该线路下用 `failure_statuses: [524]` 单独声明。

每条线路还可以设 `max_body_bytes`：超过该体积的请求不会走这条线路（例如 CF 约 100s 就 524，大请求不该排给它）。

### 请求体缓冲

请求体完整读入后才能在换线路时重发。小于 8 MiB 放内存，超过落临时文件，上限 128 MiB。

## 跨平台

内核是纯 Go，没有 cgo 依赖。`make dist` 一次交叉编译五个目标：

- `darwin/arm64`、`darwin/amd64`
- `linux/amd64`、`linux/arm64`
- `windows/amd64`

托盘（`linkgate-tray`）是可选的图形外壳，目前支持 macOS 和 Windows。macOS 版本用 cgo（需要 Xcode 命令行工具）；Windows 版的 systray 实现是纯 Go，可以直接从 macOS 交叉编译出 `.exe`。Linux 不提供托盘。

内核单独运行就能工作，面板用浏览器打开即可。

## 面板与托盘

面板是内嵌的单页 HTML（原生 JS，无框架），通过管理接口访问：

```
http://127.0.0.1:8766/__linkgate/ui
```

面板展示每条线路的状态、延迟、请求计数和事件日志。可以在面板里修改配置、手动探测线路、临时启停线路。

管理接口默认监听 `127.0.0.1:8766`，只接受回环地址。首次启动在 `~/.linkgate/admin.token` 生成令牌（权限 0600），API 请求需带 `X-Linkgate-Token` 头。

主要接口：

| 方法 | 路径 | 说明 |
|---|---|---|
| GET | `/__linkgate/v1/status` | 版本、运行时长、每条线路的状态和计数 |
| GET | `/__linkgate/v1/events?after=N` | 增量事件 |
| PUT | `/__linkgate/v1/config` | 提交 YAML，校验通过后写盘并热加载 |
| POST | `/__linkgate/v1/probe?group=&upstream=` | 立即探测一条线路 |
| POST | `/__linkgate/v1/toggle?group=&upstream=&enabled=` | 运行时启停线路（不写盘） |

Unix 下发送 `SIGHUP` 也会触发配置热加载。

托盘做两件事：在系统栏显示当前状态，点击打开面板。

## 部署

### macOS — LaunchAgent

```bash
./scripts/install-macos.sh
```

脚本构建内核和托盘（托盘需要 Xcode 命令行工具，没有就只装内核），注册 LaunchAgent，登录时自动启动。首次运行复制示例配置到 `~/.linkgate/config.yaml`。

卸载：`./scripts/uninstall-macos.sh`（保留配置和令牌）。

### Linux — systemd

```bash
make build
sudo cp bin/linkgate /usr/local/bin/
sudo mkdir -p /etc/linkgate /var/lib/linkgate
sudo cp configs/linkgate.example.yaml /etc/linkgate/config.yaml
# 改写配置
sudo vi /etc/linkgate/config.yaml

sudo cp deploy/linkgate.service /etc/systemd/system/
sudo systemctl daemon-reload
sudo systemctl enable --now linkgate
```

### Windows — 计划任务

```powershell
# 先生成配置
.\dist\linkgate-windows-amd64.exe -init
# 改写 %USERPROFILE%\.linkgate\config.yaml 后：
.\deploy\windows\install.ps1
```

脚本注册计划任务（登录启动、崩溃重启）和托盘启动快捷方式。

## 边界与不能解决的问题

Linkgate 是传输层代理。以下超出它的范围：

- **流中途断开。** 连接建立、响应开始输出后中断，没有代理能在另一条连接上续写。需要 harness 自己重试。
- **上游 5xx。** 后端或更深层的错误，不是线路问题。Linkgate 不因 5xx 切换线路。
- **Cloudflare 100 秒超时（524）。** 线路经过 CF 时，模型思考超过 100 秒 CF 返回 524。这是 CF 的限制，绕过方式是走不经过 CF 的线路。
- **模型路由、协议转换、Token 计费。** Linkgate 不解析请求内容，不知道什么是模型。它只转发字节。
- **系统代理。** 内核不读系统代理设置，避免流量绕进 Clash 等工具造成二次故障。如果线路本身需要代理，在线路 URL 层面处理。

## 冒烟测试

```bash
LINKGATE_UPSTREAM=http://<your-upstream>:8090 ./scripts/smoke-test.sh
```

用独立端口（18765/18766），不影响正式配置。验证：第一条线路故意失败后自动切换，9 MB 请求体正确透传。

## 许可证

<!-- TODO: 确认并补充许可证 -->
