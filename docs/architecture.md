# Linkgate 架构

> 一句话：给所有 harness 一个固定的本地地址，背后挂多条通往同一个后端的网络线路，哪条通用哪条。

## 1. 定位与边界

Linkgate 是**传输层**的透明反向代理。它不理解模型、渠道、Token、计费、请求体结构，只做四件事：

1. 收下请求（含完整请求体）；
2. 按优先级挑一条线路；
3. 原样转发，原样流回；
4. 记录每条线路的成败与切换次数。

明确不做：模型路由、渠道管理、Token 统计、协议转换、改请求头/请求体、接管系统路由、修改 DNS 或 Clash 规则。

## 2. 进程结构

```
cmd/linkgate          内核（纯 Go，无 cgo，darwin/windows/linux 都能编译）
cmd/linkgate-tray     托盘外壳（cgo，只做状态展示与打开面板；Linux 不提供）
internal/app          配置、状态、代理引擎、健康检查、管理接口
internal/ui           内嵌单页面板（原生 JS，无框架）
```

托盘是可选壳：内核单独跑在服务器上也能用（面板用浏览器打开）。

## 3. 一次请求的生命周期

```
client → linkgate:8765
  ├─ 读完整请求体（<8MiB 用内存，否则落临时文件；上限默认 128MiB）
  ├─ 候选排序：可用线路（按 priority）→ 不可用线路（按 priority，兜底）
  │    · 运行时停用的线路、请求体超过该线路 max_body_bytes 的线路直接排除
  ├─ 尝试 1：connect(2s) → TLS → 发请求 → 等响应头(900s)
  │    ├─ 成功 → 立刻回写状态行/头，逐块 flush 流式转发
  │    └─ 失败 → 用 httptrace 判断“请求发出去了没有”
  │         ├─ 从未拿到连接（GotConn 没触发）→ 换下一条线路重试（安全）
  │         ├─ 拿到过连接、但拨号失败且没写请求头 → 换下一条（安全）
  │         └─ 其余（写过请求头/等响应头超时/流中断）→ 直接 502，不重发
  └─ 记录：选了哪条、试了几次、建连耗时、总耗时、状态码
```

## 4. 为什么只在「连接未建立」时换线路

- 连接没建立 = 请求体一个字节都没发出去，上游不可能处理，重发不会重复计费。
- 连接建立后，即使立刻失败，也无法证明上游没收到或没开始处理；重发有重复计费风险。
- 流已经开始输出后中断，任何代理都无法在另一条连接上续写；必须由 harness 自己重试。
  这是权责边界，不是缺陷。

判定不用错误类型、更不用错误字符串——评审实测过两次：

- TLS 层可以在服务端读完整个 POST 之后才返回畸形记录；
- Happy Eyeballs 下一个地址拨号失败、另一个成功，请求头已经写出若干字节才报错，
  而 `WroteHeaders` 还没触发。

部分写出的字节通过 httptrace 观察不到，所以许可条件只有一个：

| 信号 | 含义 |
|---|---|
| `GotConn` 从未触发 | 从未拿到连接，DNS/TCP/TLS 全部失败，零字节发出 → 允许换路 |
| `GotConn` 已触发 | 连接已建立（可能是复用的），此后任何失败都不换路 → 502 |

代价：连接池里的旧连接失效时会直接 502。缓解办法是 `reconnect_after_idle`
（默认 60s）：闲置超过该时长的线路在下次请求前先关掉空闲连接、强制重新拨号，
线路已死时就会表现为干净的拨号失败，从而可以安全换路。

Go 的 `http.Transport` 对复用的空闲连接还有一条内置重试：仅当连接是复用的、且请求还没写出去时才会重试。这条重试是安全的，我们依赖它。

## 5. 超时的分层

| 层次 | 默认 | 说明 |
|---|---|---|
| 建连 | 2s | 线路不通要快速失败，不能拖住请求 |
| TLS 握手 | 5s | |
| 上传无进展 | 60s | 对端接受连接却不读时，请求体不能无限期阻塞；看的是“有没有进展”，不是总时长；直到 `WroteRequest` 才停表 |
| 等响应头 | 900s（组可覆盖） | 模型思考很久是正常现象，不能当故障 |
| 流空闲 | 900s | 流中途这么久没有任何数据才判定卡死 |
| 空闲连接 | 90s | 连接池回收 |
| 慢速预警 | 120s 未出首字节 | 只记事件，不判定失败 |

所有超时都可配置，面板里也能改。没有「一刀切」的超时。

上游如果把请求体读完了却迟迟不给响应头，上传计时会自动切换成响应头超时，覆盖“最后一块已发完但仍阻塞在 socket 写”的窗口。

## 6. 健康状态

- 主动探针：默认每 15s 对每条可用线路发一次 `GET /health`（超时 3s，连续 2 次失败判死，恢复需连续 2 次成功）。
- 被动失败：真实请求的传输层失败同样计入同一套阈值。
- 判死的线路不参与优先选择，但保留为兜底：三条全死时仍按顺序尝试，并把每条线路的失败原因写进返回体。
- 5xx **不**计入线路故障（那是 AxonHub 或更深层的问题，不是线路问题）。

## 7. 管理 API（仅回环地址）

所有 `/v1` 接口要求 `X-Linkgate-Token` 头；令牌首次启动生成在 `~/.linkgate/admin.token`（0600）。
写接口额外校验 `Origin`，防止浏览器里的恶意页面改配置。

| 方法 | 路径 | 说明 |
|---|---|---|
| GET | `/__linkgate/v1/health` | 存活检查（不需令牌） |
| GET | `/__linkgate/v1/status` | 版本、运行时长、每组每条线路的状态与计数 |
| GET | `/__linkgate/v1/events?after=N&limit=200` | 增量事件 |
| GET | `/__linkgate/v1/config` | `{path, yaml}` |
| PUT | `/__linkgate/v1/config` | 请求体是 YAML 文本，校验通过后写盘并热加载 |
| POST | `/__linkgate/v1/probe?group=&upstream=` | 立即探测一条线路 |
| POST | `/__linkgate/v1/toggle?group=&upstream=&enabled=false` | 运行时启用/停用（不写盘） |
| GET | `/__linkgate/ui` | 面板 |

`status` 返回结构：

```json
{
  "version": "0.1.0",
  "started_at": "2026-10-27T00:00:00Z",
  "uptime_seconds": 1234,
  "config_path": "/Users/x/.linkgate/config.yaml",
  "last_seq": 42,
  "groups": [
    {
      "name": "axonhub",
      "listen": "127.0.0.1:8765",
      "active": "tailscale",
      "upstreams": [
        {
          "group": "axonhub", "name": "relay", "url": "https://<CF_HOST>",
          "priority": 10, "status": "healthy", "enabled": true,
          "consecutive_failures": 0, "consecutive_successes": 3,
          "last_probe_at": "...", "last_probe_latency_ms": 45,
          "last_probe_error": "",
          "last_request_at": "...", "last_request_latency_ms": 812,
          "last_request_error": "",
          "requests_ok": 120, "requests_failed": 1,
          "failovers_out": 1, "selected_total": 118, "selected_as_backup": 2
        }
      ]
    }
  ]
}
```

`status` 取值：`healthy` / `unhealthy` / `unknown` / `disabled`。

事件 `kind`：`start` / `request` / `failover` / `health` / `probe` / `config`。

## 8. 配置

见 `configs/linkgate.example.yaml`。真实地址只写在用户自己的 `~/.linkgate/config.yaml`，不进仓库、不进示例。

优先级用整数表示，越小越优先；同优先级保持书写顺序。线路可单独设置 `host`、`headers`（CF Access 的 Service Token 放这里）、`tls_insecure_skip_verify`、独立的建连/响应头超时。

## 9. 关键不变量

1. 不修改请求体、Authorization、模型字段、任何业务头。
2. 不在连接建立后重发。
3. 不因上游 5xx 切换线路。
4. 面板与托盘都不统计 Token；只统计请求数、失败数、换线路次数、延迟。
5. 内核不读系统代理设置（`Proxy: nil`），避免绕进 Clash 造成二次故障。
6. 不支持 HTTP trailers：请求与响应的 trailer 会被丢弃。LLM API 不依赖 trailer；
   如果将来要代理依赖 trailer 的协议，需要显式实现，不能假定透明。
