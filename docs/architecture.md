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
cmd/linkgate-tray     托盘外壳（macOS 用 cgo，Windows 纯 Go；Linux 不提供）
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
  │         └─ 拿到过连接（含部分请求头/等响应头超时/流中断）→ 不重发；未输出响应时返回 502
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

## 9. 配置事务与协议边界

`ConfigManager` 是 YAML 保存、JSON CRUD、SIGHUP/API reload 的唯一写入口，串行处理更新。它先严格解析并全量校验，预建 HTTP transports 和监听 plan；任何准备失败都不改变运行态。写盘采用同目录 0600 临时文件、Sync、Rename，成功保存保留上一份已接受 YAML 到 `.bak`。短 gate 锁内提交 store、engine、admin、monitor、事件容量和监听，提交阶段没有可失败操作。外部 reload 成功不创建 `.bak`；失败恢复上次接受的 YAML，恢复失败会明确报错。进程崩溃/断电与外部编辑器并发写入不在跨文件事务保证内。

`ListenerManager` 只负责地址到 `ListenerHandler` 的 socket 生命周期，不含 HTTP 路由。`ListenerHandler` 提供 Serve/Update/Shutdown。当前适配器为 `HTTPListener`，未来 TCP 引擎可提供自己的 serving loop。相同地址复用 socket，新的 handler 通过原子指针替换，keep-alive 下次请求也用新 handler；移除监听立即关闭端口，已有请求最多 5s drain。

组 `protocol` 默认 http，tcp 暂未支持；线路 `kind` 仅元数据。健康探针通过 `healthProbe` 按 `probe.type` 分派，当前提供 HTTP 和纯 dial 的 tcp_connect。探针包含 URL path 前缀、组级头及 timeout_overrides，不依赖转发引擎实现。全局并发 budget 在 reload 前后共享，默认所有组合计 64；每组上限仍为 64。

结构化 API 的完整方法、字段、合并语义和错误码见 README 的“结构化配置 API”。PUT 按 name 更新已给字段；maps/lists 整体替换；CRUD 会重新序列化 YAML，原注释在 `.bak` 中保留，当前文件注释不保留。日志文件/格式/级别/轮转参数、data_dir、admin.token_file 为启动资源，热更新返回 409；log.ring_size 可热更新。

## 10. 关键不变量

1. 不修改请求体、Authorization、模型字段、任何业务头。
2. 不在连接建立后重发。
3. 不因上游 5xx 切换线路。
4. 面板与托盘都不统计 Token；只统计请求数、失败数、换线路次数、延迟。
5. 内核不读系统代理设置（`Proxy: nil`），避免绕进 Clash 造成二次故障。
6. 不支持 HTTP trailers：请求与响应的 trailer 会被丢弃。LLM API 不依赖 trailer；
   如果将来要代理依赖 trailer 的协议，需要显式实现，不能假定透明。

## 11. 按 User-Agent 优先选路（UA pinning）

服务级 `ua_rules` 是有序数组，每项包含 `match`、本组线路名 `line` 和可选 `comment`。默认按入站原始 User-Agent 做不区分大小写的子串匹配；`exact:` 为不区分大小写的全文匹配，`prefix:` 为不区分大小写的前缀匹配，`regex:` 使用 Go 正则语法（需忽略大小写时显式加 `(?i)`）。前缀标识本身不区分大小写，匹配内容不自动去除首尾空格。match 及前缀后的内容不能为空或含控制字符；无效正则、重复 match、引用非本组线路均拒绝。字面匹配按模式和忽略大小写后的内容去重；正则按模式和原表达式去重。

```yaml
groups:
  - name: service
    listen: 127.0.0.1:18765
    ua_rules:
      - match: Chrome/
        line: cf
        comment: 浏览器优先走 CF
    upstreams:
      - name: direct
        url: http://upstream.invalid
        priority: 10
      - name: cf
        url: https://relay.invalid
        priority: 20
```

**Pin ≠ 锁死。** 数组中第一条命中的规则决定目标，命中后不继续寻找其他规则。候选仍经过原有 enabled、运行时停用、请求体 `max_body_bytes` 与代际资格过滤；目标若在候选中，移到最前，其余保持原有“可用线路按优先级 → 不健康线路按优先级”的顺序。当前代中，不健康但启用的目标也先尝试，零字节建连失败才 fallback；被停用或排除的目标不会复活，直接按其余候选选路。`retry.mode` / `max_attempts` 仍生效。拿到连接后失败、上游 5xx 或流中断均不因 pinning 重发。

**冻结。** 编译后的规则属于 `groupRuntime` 的配置代际；请求入站时冻结 UA 命中的目标和 `freezeLines` / `lineSnapshot` 资格。缓冲期间发生一次或多次配置提交，旧请求仍使用旧规则与入站资格，不读新代规则，也不能复活入站时已停用/不健康的目标。当前代选路仍按原模式读取实时健康与运行时开关。

规则随组 CRUD 管理，不新增独立路由：`POST /__linkgate/v1/groups` 可连同 `upstreams` 一次创建；`PUT /__linkgate/v1/groups` 按不可变 name 更新 `ua_rules`，整个数组替换，省略不变，`[]` 清空，`null` 拒绝。例如：

```json
{"name":"service","ua_rules":[{"match":"Chrome/","line":"cf","comment":"浏览器优先走 CF"}]}
```

JSON 沿用 `buildSchema` / `decodeObjectStrict` 的严格嵌套键校验，未知、大小写/Unicode 折叠变体和重复键均报 400。更新经 `ConfigManager.Mutate` 全量校验、预检、原子发布并保留 `.bak`；被规则引用的线路不能直接删除，需先清除对应规则。`GET /__linkgate/v1/status` 与 `/stats` 每个服务都有 `ua_rules`（无规则时为 `[]`），`/config` 的结构化 groups 也包含规则；`active` 仍是无 UA 的默认优先线路，不代表每个客户端。UA 命中不记原始 UA 或匹配值到事件/日志，非法 match 错误不回显表达式；`ua_lines` 继续按既有 UA 分类与线路聚合，不新增 `pinned` 统计标记。此阶段不新增 UI 表单字段。
