# Linkgate 参考：Ops 侧线路与 AxonHub 约束

> 只读侦察，来源为 `~/Ops` 现有记录（路径均相对 `~/Ops`）；未连接远程机器、未修改 Ops。地址/出口统一用占位符（如 `<ZT_HOST>`、`<CF_HOST>`、`<DOCKER_GW>`）。

## 1. AxonHub 超时与流式行为

- **两个总时长都在 `config.yml`、不在面板：`server.request_timeout=30s`（语义未核实）、`server.llm_request_timeout` 已从 600s 改为 900s，但 2026-10-24 明确“未重启”，运行进程仍按 600s。** 来源：`changes/2026-10-24-AxonHub全局LLM请求超时600s调整为900s.md`
- **600s 墙的实测形态：流在 600006ms 被切、`error_message=context deadline exceeded`、响应状态码为空、无 usage 记录；30 天内 63 次全部落在 591–600s。** 来源：`上游Issue与PR台账.md`（AH-011）
- **面板两个超时都是 0 且只有全局：`stream_first_event_timeout_seconds=0`、`non_stream_response_timeout_seconds=0`，渠道/Key 无覆盖。** 来源：`上游Issue与PR台账.md`（AH-011）、`changes/2026-10-12-grok2api断流排查与出口节点治理.md`
- **首事件超时是唯一能跳过“同渠道重试”的路径；设为 0 时，慢渠道必须先耗尽同渠道预算才换渠道。** 来源：`Runbooks/axonhub-grok2api-只读查询手册.md`
- **全局重试参数：`max_single_channel_retries=3`、`max_channel_retries=6`、`retry_delay_ms=10000`、`failover`、`empty_response_detection=true`。** 来源：`changes/2026-10-18-AxonHub-grok渠道failover与grok2api断流.md`
- **换渠道时间 = 同渠道尝试次数 × 单次上游耗时 + 3×10s：上游 120s 时约 510s，60s 时约 270s，常超过客户端 300s 超时。** 来源：`changes/2026-10-18-AxonHub-grok渠道failover与grok2api断流.md`
- **401/403 默认不可重试；429/5xx/传输层错误可重试；`DialContext` 对黑洞路由单次 30s，按默认 2 次同渠道重试最坏约 90s，当前全局 3 次则约 120s。** 来源：`上游Issue与PR台账.md`（AH-004）
- **空响应检测会把两类合法终态误判为可重试：`finish_reason=sensitive` 与流内 `error`（如 `context_length_exceeded`），HTTP 仍是 200，同一请求在多渠道白跑。** 来源：`上游Issue与PR台账.md`（AH-001/AH-002）
- **`server.request_timeout=30s` 不限制响应总时长：已观测到 600s 长流，真正的一刀切是 `llm_request_timeout`。** 来源：`changes/2026-10-24-AxonHub全局LLM请求超时600s调整为900s.md`
- **AxonHub 本体没有请求体上限：30.06MB 请求曾 completed；但完整请求/响应体会存 Postgres，库以约 4GB/天增长且自动清理全关。** 来源：`Machines/mint/changes/2026-10-06-axonhub-opencode-4.5MiB只读排查.md`、`Machines/mint/README.md`
- **请求体含 `\u0000`/孤立代理项时落库失败，只写 32 字节占位符，请求本身仍 200；丢的是审计与详情，不是传输。** 来源：`上游Issue与PR台账.md`（AH-010）

## 2. 大请求体：实测尺寸与逐跳耗时

- **观测到的最大真实会话请求体 12.11MB；其中 9.6MB 的一例有 8.3MB 是内嵌 base64 图片。** 来源：`changes/2026-10-16-大请求体会话卡死-跨渠道复盘与处置.md`
- **历史最大单请求 30.06MB（已归档渠道，completed）；现役渠道最大 14.39MB。** 来源：`Machines/mint/changes/2026-10-06-axonhub-opencode-4.5MiB只读排查.md`
- **上游确有 4.5MiB 硬限：ch103 在 2026-09-07 对 4.60MiB 返回 413、4.41MiB 通过；全库仅 3 次，此后 12MB 也能过，限制疑似放宽。** 来源：`Machines/mint/changes/2026-10-06-axonhub-opencode-4.5MiB只读排查.md`
- **客户端→mint 不是瓶颈：12MB 请求体 0.1–1.0s 收完，AxonHub 收到完整体后 0.1–1.0s 发起上游调用。** 来源：`changes/2026-10-16-大请求体会话卡死-跨渠道复盘与处置.md`
- **体积不是主导变量：同样 4.14MB，ch124 首字节 61–94s，官方直连渠道 1.5–6.1s。** 来源：`changes/2026-10-17-AxonHub大请求慢-逐跳取证与旧结论修正.md`
- **9–11MB 的 ch103 请求首字节 28–294s，随时段拥塞大幅漂移；同段 0.75–0.83MB 小请求也要 36–97s。** 来源：`changes/2026-10-17-AxonHub大请求慢-逐跳取证与旧结论修正.md`
- **关键上行速率：国内官方直连 7.5MB/s；`<OC_HOST>` 直连 229–263KB/s（10-24 退化到 73–149）；`<OD_HOST>` 直连 TLS 不通；杭州 2.25MB/s；AWS 澳洲 204–215KB/s；Azure JP 1.1MB/s；东京代理 73–108KB/s。** 来源：`changes/2026-10-17-AxonHub大请求慢-逐跳取证与旧结论修正.md`、`changes/2026-10-24-mint固定出口三端口7901-7903.md`
- **12MB ÷ 45KB/s ≈ 270s，正好压满客户端 300s header 超时；10-16 近 3 小时 15 个请求有 8 个未完成。** 来源：`changes/2026-10-16-大请求体会话卡死-跨渠道复盘与处置.md`
- **CF 边缘约 100s：1.8MB 上传 100s+ 即返回 524，大请求/慢首字节不应走 CF。** 来源：`上游Issue与PR台账.md`（OTHER-001）、`changes/2026-10-16-大请求体会话卡死-跨渠道复盘与处置.md`
- **token 数不是主因：30–55 万 token、0.94–1.24MB 的请求 10–40s 正常返回。** 来源：`changes/2026-10-16-大请求体会话卡死-跨渠道复盘与处置.md`

## 3. mint 的 mihomo/Clash TUN 与出口

- **mint 跑 Sparkle/mihomo：`tun.enable=true`、`auto-route=true`、`mode=rule`；尾部规则 `GeoIP cn → DIRECT`、`Match → Manual Select → 东京Pro+[AI]`。** 来源：`changes/2026-10-17-AxonHub大请求慢-逐跳取证与旧结论修正.md`
- **AxonHub 容器流量经 veth 进 TUN 表 2022，因此容器出站同样受这套规则约束，不是“宿主代理、容器直连”。** 来源：`changes/2026-10-17-AxonHub大请求慢-逐跳取证与旧结论修正.md`
- **结构上 CN 目标（GeoIP cn）走 DIRECT、其余落 Match 走海外节点；国内 DeepSeek 官方 API（`<DS_HOST>`）实测 2MB 上行 7.5MB/s、0.28s。** 来源：`changes/2026-10-17-AxonHub大请求慢-逐跳取证与旧结论修正.md`
- **`<OC_HOST>`（OpenCode 上游）原走东京（73KB/s），10-18 改为 DIRECT（8MB 38.96s/215KB/s）；10-24 再测直连退化到 73–149KB/s。** 来源：`changes/2026-10-18-mint-AI分流-opencode直连与open-design杭州出口.md`、`changes/2026-10-24-mint固定出口三端口7901-7903.md`
- **`<OD_HOST>`（OpenDesign 上游）直连 TLS 20s 无响应、必须代理；改走杭州出口后 8MB 3.73s/2.25MB/s。** 来源：`changes/2026-10-18-mint-AI分流-opencode直连与open-design杭州出口.md`
- **固定出口三端口：7901 杭州 CN、7902 Azure 日本西 JP、7903 AWS 澳洲 AU；7902 抖动极大（25–284KB/s，4090 上 0/6），7903 最稳（204–215KB/s）。** 来源：`changes/2026-10-24-mint固定出口三端口7901-7903.md`、`changes/2026-10-22-4090与9000X固定出口三端口.md`
- **790x 监听在 Docker 网桥网关 `<DOCKER_GW>`（不是通配监听），容器可用、局域网不可用；三个端口无认证；网桥重建后网关地址变化需同步改监听。** 来源：`changes/2026-10-24-mint固定出口三端口7901-7903.md`
- **mihomo 内核 v1.19.25、mixed-port 7897、allow-lan、unix socket 控制口；改配置可 `PUT /configs` 热加载，不必重启 GUI。** 来源：`changes/2026-10-24-mint固定出口三端口7901-7903.md`
- **Tailscale 控制面在 mint 上借道本机 7897：`HTTPS_PROXY` 挂在 tailscaled；clash 挂掉时控制面会暂时断，代理恢复后自动回归。** 来源：`changes/2026-10-06-mint-TS升级与控制面代理.md`
- **Fake-IP 段 /16 与 TUN 路由 /30 错配曾造成全机国外域名解析黑洞；修法是 fake-ip-filter 豁免；VM105 的对应做法是 `tun.route-exclude-address` + `DIRECT` 规则。** 来源：`changes/2026-10-06-mint-TS升级与控制面代理.md`、`Runbooks/vm105-mihomo直连例外.md`
- **Sparkle 的订阅级 YAML override 会整体替换 `proxy-groups` 数组；listener 指向被替换掉的组名时仍会绑定端口，但每个请求 502 且内核无错误日志。** 来源：`changes/2026-10-22-4090与9000X固定出口三端口.md`
- **自建 SS2022 节点多标 `udp: false`，QUIC/UDP 不会走；便宜机场节点 UDP 支持普遍差，RustDesk 一类 UDP 服务必须直连例外。** 来源：`Runbooks/mac-clash-固定端口-杭州与日本西.md`、`Runbooks/vm105-mihomo直连例外.md`

## 4. ZeroTier / Tailscale 断线表现

- **4090 的 ZT 是长期劣化项：RTT 0.4–0.6s、丢包约 7%、一晚三次短时断流；断流会掐 SSH、让下载在约 50KB 处被重置、并出现数分钟 TCP 拒连。** 来源：`changes/2026-10-16-4090生图恢复与语言模型中断复盘.md`、`机器清单.md`
- **根因是对称型 NAT + 校园网多层 NAT，打洞不可行，ZT 降级到中继（含 TCP fallback），不是配置错误。** 来源：`changes/2026-10-16-4090生图恢复与语言模型中断复盘.md`
- **另一台 ZT 侧实测：4090→对方只能 RELAY、314ms、10% 丢包；Mac→mint 86.9ms/0%、mint→4090 136ms/0% 但 max 612ms。** 来源：`changes/2026-10-24-阿德客户端频繁重试只读排查.md`
- **ZT 1.14 与 1.16 混版在中继/moon 处理上有已知冲突，可能让本该 DIRECT 的 peer 长期 RELAY。** 来源：`changes/2026-10-24-阿德客户端频繁重试只读排查.md`
- **断线形态以“静默丢包/黑洞 + 中途 RST”为主，不是干净立即 RST：TCP 可能建连成功后再 stall，长上传/长流会被丢包直接打断。** 来源：`changes/2026-10-16-4090生图恢复与语言模型中断复盘.md`、`changes/2026-10-24-阿德客户端频繁重试只读排查.md`
- **mint 的 TS 控制面在国内出口会被定向干扰：TCP 可通、Noise 握手被杀；症状是重启后离线、`register request ... deadline exceeded`，重试不自愈。** 来源：`Tailnet-组网与DERP.md`、`changes/2026-10-06-mint-TS升级与控制面代理.md`
- **DERP 只中继数据面；控制面永远走官方海外端点。自建 DERP 在北京/杭州，实测最近 42.8ms。** 来源：`Tailnet-组网与DERP.md`
- **云主机 TS 会话建立后即消失（check mode），日常可用路径是 ZT + 本机 sshd；两台云主机已于 10-26 关闭 Tailscale SSH。** 来源：`audits/2026-10-26-Tailscale-Tailworm暴露面核对.md`
- **4090 的 TS 身份可能被朋友切到其 tailnet；飞牛 tailscaled 在 10-26 未运行；TS 通道不是稳定前提。** 来源：`机器清单.md`、`audits/2026-10-26-Tailscale-Tailworm暴露面核对.md`
- **故障频率的直观证据：10-22 因 ZT 卡顿手动切 CF，10-23 因 CF 大请求 524 又切回 ZT；两天内两条路先后都不可用。** 来源：`上游Issue与PR台账.md`（OTHER-001）
- **AxonHub 对黑洞路由靠 30s dial 超时兜底，当前 3 次同渠道重试下最坏约 120s 才换渠道；这与“2s 建连、快速失败”的代理策略正好相反。** 来源：`上游Issue与PR台账.md`（AH-004）

## 5. Cloudflare Tunnel、FRP、NATFrp

- **mint 的 cloudflared 是独立 compose 项目 `runtime`，host 网络、token 托管（remotely-managed）；ingress 规则只在 Cloudflare 后台，本地没有 config.yml。** 来源：`Machines/mint/README.md`
- **AxonHub 有经 CF 的入口（`<CF_HOST>`）；慢首字节会撞 CF 约 100s 边缘超时并返回 524。** 来源：`changes/2026-10-16-大请求体会话卡死-跨渠道复盘与处置.md`
- **CF 的 TLS 在边缘终结、源站由 cloudflared 主动外连，因此源站看到的 scheme 是 http；反代必须正确重建 `X-Forwarded-Proto`/`Host`，否则应用层同源校验失败。** 来源：`Machines/mint/changes/2026-09-01-neochat-cloudflare-正式收尾.md`
- **记录中未见 mint 已部署 Cloudflare Access 的证据；NeoChat 用的是应用内访问口令，4090 的 ComfyUI 只是“必须配 Access”的建议，azure 的 CPA 是“可选再叠”。** 来源：`Machines/mint/changes/2026-09-01-neochat-cloudflare-正式收尾.md`、`changes/2026-10-16-4090生图恢复与语言模型中断复盘.md`、`Machines/azure-jp/README.md`
- **4090 的 CF Tunnel 是 token 服务 + Caddy 回环 basic_auth；CF 后台的 Public Hostname 必须指 `http://`，指 `https://` 会 502。** 来源：`changes/2026-10-18-4090推理控制台改造与网络联调.md`
- **mint 的 NATFrp 服务在 `my_api_stack` 里运行，记录只到“在运行”，没有 ingress、端口或用途细节。** 来源：`Machines/mint/README.md`
- **阿里杭州的 frps 是宿主 systemd 的 0.53.2（不是容器），动态 remotePort 包含 443/8090/8998/25565/5667；8090 对应 AxonHub。** 来源：`Machines/阿里杭州/frp-修复方案-2026-10-21.md`
- **8090 曾“云安全组已放行但不通”，原因是宿主 firewalld 只放了 8998；`No route to host`=宿主 REJECT，超时才是云安全组拦截。** 来源：`Machines/阿里杭州/frp-修复方案-2026-10-21.md`
- **同一台机器曾有两套 frps：容器因 10086 被宿主占用而崩溃重启 7 万余次；宿主 frps 原先未开机自启，10-21/22 才补上并停掉容器。** 来源：`Machines/阿里杭州/frp-修复方案-2026-10-21.md`、`audits/2026-10-21-PHP-RCE-机队PHP暴露面排查.md`
- **frps 的 kcp 15443/udp 未放行、dashboard 7001 不对公网开放；这些端口“在监听”不等于“可用”。** 来源：`Machines/阿里杭州/frp-修复方案-2026-10-21.md`

## 6. 已踩过的坑

- **413 有两类：上游 ch103 的 4.5MiB 硬限（`Request body exceeds the 4.5 MiB limit`）与历史上 nginx 的 413；AxonHub 自身未见 413。** 来源：`Machines/mint/changes/2026-10-06-axonhub-opencode-4.5MiB只读排查.md`
- **没有观测到网络层把请求体截断；真正的“截断”是入库层：非法 Unicode 让 body 落 32 字节占位符，请求照常返回。** 来源：`上游Issue与PR台账.md`（AH-010）
- **断流典型形态是“上游 200 + 0 token + 约 120s 后关闭”：grok2api 对 AxonHub 仍记 200，AxonHub 对下游记为 502 `upstream_stream_interrupted`。** 来源：`changes/2026-10-05-grok-chat-fast搜索故障只读排查.md`、`changes/2026-10-18-AxonHub-grok渠道failover与grok2api断流.md`
- **流内 `error` 与 `finish_reason=sensitive` 会被当成空响应重试，HTTP 状态仍是 200，真实原因被掩盖。** 来源：`上游Issue与PR台账.md`（AH-001/AH-002）
- **客户端重试会把整个请求体重新上传：OpenCode 最多 5 次、退避 base 2s ×2、抖动 25%、上限 30s；524/5xx/连接类错误都可触发。** 来源：`changes/2026-10-24-阿德客户端频繁重试只读排查.md`
- **记录中未见网关侧幂等键/去重机制；每次重试是新的 request/execution。usage 只在成功执行时落账，失败重试不重复计费；但“上游已成功、响应没回到客户端”会在客户端重试时再算一次。** 来源：`changes/2026-10-24-AxonHub全局LLM请求超时600s调整为900s.md`、`changes/2026-10-24-阿德客户端频繁重试只读排查.md`
- **600s 超时的那次请求没有 usage 记录：网关侧不计费，但上游可能已经生成 token，成本由订阅额度承担，且网关看不到已生成量。** 来源：`上游Issue与PR台账.md`（AH-011）
- **同渠道重试预算可能比客户端超时还长，导致 fallback 永远不发生；这是“看起来没切线路”的常见原因。** 来源：`changes/2026-10-18-AxonHub-grok渠道failover与grok2api断流.md`
- **DCP/auto-compact 按 token 计量，管不到图片字节：会话峰值约 26 万 token 未触发 40% 阈值，ZCode 侧 51 万 token 也未触发约 90 万阈值。** 来源：`changes/2026-10-16-大请求体会话卡死-跨渠道复盘与处置.md`
- **端口冲突是实打实的历史事故：mint 的 8090/3000/30001/8180/3300、1Panel 10086、未定位的宿主 :80；阿里杭州的 10086 与 80/443；新增监听前必须逐项核。** 来源：`Machines/mint/README.md`、`Machines/阿里杭州/frp-修复方案-2026-10-21.md`
- **Docker 网桥重建会改网关地址，绑在 `<DOCKER_GW>` 上的 790x 会整体失联。** 来源：`changes/2026-10-24-mint固定出口三端口7901-7903.md`
- **listener 指向不存在的 proxy-group 时仍会 bind，但请求 502，且 mihomo 核心日志没有对应错误——这是最难查的一类“端口通但不可用”。** 来源：`changes/2026-10-22-4090与9000X固定出口三端口.md`
- **反代保留 `Host`/`X-Forwarded-Proto` 是硬要求：NeoChat 曾因 proto 被重建成 http 而全量 `CSRF_ORIGIN_BLOCKED`；RikkaHub 不保留 Host 直接 403。** 来源：`Machines/mint/changes/2026-09-01-neochat-cloudflare-正式收尾.md`、`Machines/mint/changes/2026-09-10-docker只读盘点与rikkahub调研.md`
- **AxonHub 默认会改写 `Authorization`/`Content-Type`/`Accept`/`Host` 并丢弃 `x-opencode-*`，请求体透传默认关；透明代理不能假设“原样头”会到达上游。** 来源：`Machines/mint/README.md`
- **多层超时是串联的：CF 100s < 客户端 300s < AxonHub 600s（配置 900s 未生效）；任何一层先到，长请求就死在该层。** 来源：`changes/2026-10-16-大请求体会话卡死-跨渠道复盘与处置.md`、`changes/2026-10-24-AxonHub全局LLM请求超时600s调整为900s.md`

## 7. 对 Linkgate 的直接约束

- **只在“连接未建立”时换线路；一旦发出请求体/收到响应头，重发就可能重复计费，流中断只能交回 harness。** 来源：`changes/2026-10-24-阿德客户端频繁重试只读排查.md`、`上游Issue与PR台账.md`（AH-011）
- **等响应头超时要大于 AxonHub 的“实际生效”总超时：当前运行值 600s，配置 900s 未重启；设 600s 会与网关同时到点，设 900s 以上才有余量。** 来源：`changes/2026-10-24-AxonHub全局LLM请求超时600s调整为900s.md`
- **建连超时必须短（2s 级别）：ZT/TS 黑洞会让默认 dial 拖到 30s，当前重试预算下最坏约 120s，健康检查会被拖死。** 来源：`上游Issue与PR台账.md`（AH-004）
- **健康探针只能证明“小请求可达”；大请求体的真实可用性取决于出口速率与上游处理，必须靠真实请求的错误率补充。** 来源：`changes/2026-10-17-AxonHub大请求慢-逐跳取证与旧结论修正.md`
- **线路排序应把“稳定 200KB/s 级”的出口放在前面：AWS 澳洲最稳，杭州次之，Azure 日本西抖动大，东京代理最慢；CF 只适合小请求/快首字节。** 来源：`changes/2026-10-24-mint固定出口三端口7901-7903.md`、`changes/2026-10-22-4090与9000X固定出口三端口.md`
- **同机部署时绑回环、避开 8090/7897/790x/3000/30001/8180/3300；不要假设 8765 一定空闲，启动前实测。** 来源：`Machines/mint/README.md`
- **必须原样保留 `Host`、`Authorization`、`Content-Type` 与请求体；不要注入 `X-Forwarded-*`，否则会触发应用层同源/CSRF 检查。** 来源：`Machines/mint/changes/2026-09-01-neochat-cloudflare-正式收尾.md`
- **流式转发要逐块 flush，不能攒包：CF 的 100s 边缘超时与 Nginx 的默认 buffering 是现成教训，Nginx 侧已用关闭缓冲 + 保留 SSE 超时解决。** 来源：`Machines/mint/changes/2026-09-01-neochat-origin-proxy.md`
- **实测存在 12MB 级请求；本地落盘/读体策略应按此量级设计，链路慢时真正卡住的是上游首字节，不是本地落盘。** 来源：`changes/2026-10-16-大请求体会话卡死-跨渠道复盘与处置.md`
