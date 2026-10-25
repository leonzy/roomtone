# 播放控制与音箱输出 · 开发计划

> **状态：可执行计划 v2（2026-10-25 审计后修订）。** 设计与决策见[播放控制与音箱输出架构](./architecture_playback.md)（后端）与[播放与音箱交互设计](./architecture_playback_ui.md)（前端）；与 SlimServer 的能力对照与验收结论见[播放能力对照矩阵](./playback_compat_matrix.md)。本文不重复设计理由，只回答三件事：**按什么顺序做、动哪些文件、怎么算做完**。
>
> 任务粒度 = 一次可独立提交、可独立验证的变更。任务 ID 写进提交信息（`feat(player): 队列语义与传输状态机 (B1-02)`），实现与追溯一一对应。

## 0. 使用方式

- 进度维护在 §11 进度表，完成一项勾一项；任务表本身不标状态，避免两处不一致。
- 门禁（§6）不通过不进下一阶段。宁可砍范围（例如把 DLNA 推迟到 v0.4），也不下调验收标准——同步精度和削波这类指标一旦放宽，等于把这个功能的卖点删掉。
- 估算是**单人全职人日，含自测与文档**，不含等真机、等硬件的时间（单独标注）。
- 每周结束跑一次全量 `make check`；跨周不改验收标准。

## 0.1 v2 修订：审计结论（2026-10-25）

v1 的失败模式不是「没写任务」，而是**把验收写成了描述**：B5-04 写「`delay_ms` 生效」、B6-03 写「每 sink 独立实例」、B7-03 写「触发校正」，实现里分别退化成「字段存在」「组级只有一份」「只发事件」，`make check` 全绿但功能是半成品。

v2 只做两件事：

1. **验收改成可执行断言**——假 sink 帧偏移、FFT 误差、互相关峰值、HTTP 状态码、`rg` 无残留。写不出断言的任务不许开工，断言不过不算完成。
2. **补齐审计发现的 9 个缺口**，每个都有独立任务 ID，不允许「顺手带过」。

| 审计发现（证据） | 对应任务 | v2 处理 |
|---|---|---|
| `AddMember` 无 tier 校验，DLNA 能进精确组（`manager.go:519`） | B5-02 | 验收补「HTTP 400 + `detail` 文案」断言 |
| 中途入组要等下一曲（`engine.go:499` 捕获 sinks 切片） | B5-01 | 拆出 **B5-09** 热替换；B5-01 验收改「3 帧内出声」 |
| 无漂移校正环，全仓只有检测（`party.go:296`） | — | 新增 **B5-08** |
| 无 `delay_ms` / `volume_offset` 字段 | B5-04 | 保留，验收改「帧偏移误差 ≤1 帧」 |
| 转移无 +800ms 补偿（`manager.go:615`） | B5-05 | 验收改「首帧位置差 ∈ [780,820]ms」 |
| 同步精度脚本不存在 | B5-06 | 保留，落点明确为 `scripts/tests/sync_correlation.py` |
| 无组音量、无同步开关机 | — | 新增 **B5-10**、**B8-05** |
| 每曲重开解码器与 sink，无预取 | — | 新增 **B5-11** |
| EQ 走 ffmpeg `-af`、组级只取主控（`pipeline/eq.go:22`、`manager.go:724`） | B6-01/03/06 | 重写验收 + 新增 **B6-07** 删旧路径 |
| 无 `GET /players/{id}/eq/presets` | B6-05 | 补端点与常量复用断言 |
| 派对只检测不校正，且无 `party_test.go` | B7-03/06 | B7-03 降为检测，新增 **B7-07** 校正闭环 |
| 无 `metrics.go` | B8-03 | 保留 |
| 无 `DELETE /players` 与 `DELETE /groups`、无主控切换端点 | — | 新增 **B8-04** |
| 无 power（待机/唤醒） | — | 新增 **B8-05** |
| 无 CI 守卫，半成品能过 `make check` | — | 新增 **X-07** |

**M5 / M6 / M7 的所有任务按 v2 验收重跑一遍**，已通过的保留结论，只补跑新增断言；不因为「之前过了」而跳过。

## 1. 交付边界

| 版本 | 里程碑 | 用户能感知到什么 | 累计人日 |
|---|---|---|---|
| **v0.1（内部）** | M0 + M1 + M2 | 服务端有队列与播放状态，能控制客户端播放（`local` sink） | ~31 |
| **v0.2** | + M3 | **能推给 AirPlay 音箱出声**，单台，不同步 | ~44 |
| **v0.3** | + M4 | 加 DLNA 音箱，单台/多台各自播 | ~54 |
| **v0.4** | + M5 | **多房间同步**（RAOP 精确、含 DLNA 降级、含漂移校正环） | ~71.5 |
| **v0.5** | + M6 | 每台音箱独立 EQ（服务端 biquad，非滤镜） | ~78.5 |
| **v1.0** | + M7 + M8 + X | 派对模式（含校正）、断点续播、可观测、安全与文档收口 | ~103 |

> **v2 对「完成」的定义**：任务表里每条验收都必须是可执行断言。写不出断言的任务不许开工；断言不过不算完成；「字段存在」「事件发出」「代码里搜得到」都不算。

非目标沿用设计稿 §1.2：**AirPlay 2**、Chromecast / 原生 Sonos、视频推送、插件控制播放器。写在这里是为了让"这个需求要不要做"有据可依，不用每次重新讨论。

## 2. 基线盘点

计划与设计稿的第一处差异就在这里：设计稿是按目标架构写的，落到现有代码上有几个假设不成立。

### 2.1 可以直接复用的

| 现有资产 | 位置 | 在播放器里的用法 |
|---|---|---|
| 槽位限流模式 | `internal/services/seek_stream.go:48` `seekStreamMaxConcurrent = 4` + `seekStreamSem` + `ErrSeekStreamUnavailable` | 复制同一模式做 `playerDecodeSem`，**独立计数**，两套预算互不抢占 |
| WebSocket 库 | `go.mod` 已有 `gorilla/websocket v1.5.3`（jsplugin 当客户端用） | 服务端 hub 直接用它，不引新依赖 |
| ffmpeg 路径配置 | `internal/app/app.go:303` `cacheService.SetFFmpegPath(ffmpegConfig.Path)`，key `ffmpeg_path` | 解码器复用同一路径，配置改了跟着生效 |
| 10 段 EQ 预设 | `internal/handlers/equalizer_setting.go`（key `equalizer`） | 抽出共享 preset 常量表，服务端 EQ 与客户端 EQ 用同一套名字 |
| 响度归一参数 | `internal/services/cache_service_loudness.go` | 组级 loudnorm 复用同一组参数（含 `TP=-1.5`），不另调一套 |
| 内存库测试 | `internal/database/testutil.OpenMemoryDB(t)` | 6 张新表的仓储测试全部走真实 `:memory:` |
| 路由分组与鉴权 | `internal/app/routers.go:118` `r.Route("/api/v1", ...)` | 新端点挂同一组，自动获得 JWT 与 `respondError` |
| 迁移编号 | 当前最大 `0039_scan_support_dsf.sql` | 新表从 `0040` 起 |

### 2.2 确认缺失、必须新建的

| 缺口 | 影响 | 对应任务 |
|---|---|---|
| 没有服务端播放状态、队列、传输状态机 | 整个功能的地基 | B1 组 |
| 没有 WebSocket **服务端**（现有 ws 只作为客户端连插件） | 事件推送、`local` sink 指令下发全落空 | B0-06 |
| 没有局域网地址解析——`internal_url.go` 只产出 `127.0.0.1` | **DLNA 与 RAOP 都拿不到设备能访问的流地址** | B0-05 |
| 没有 `internal/player` 包 | 全部后端任务 | B1-01 起 |
| 没有 mDNS / SSDP 客户端 | 发现不了设备 | B3-01 / B4-01 |
| 没有音频 DSP 能力（只有 ffmpeg 滤镜，且一进程一条链） | 每音箱 EQ | B6-01 |
| `clients/player` 子模块未检出，且属独立仓库 | 前端任务不能在本仓库落地 | D1 + F 组 |
| 没有"设备被接管"的概念 | 多端冲突无法表达 | B1-01 表字段 + F1 |

**B0-05 是这轮盘点里最容易被低估的一条**：设计稿写"复用 `internal_url.go` 解析出的局域网可达绝对 URL"，但该文件解析出来的是 `http://127.0.0.1:{port}`，那是给后端自己回调用的。DLNA renderer 拿到 `127.0.0.1` 只会拉自己的回环。正确做法是按**目标设备的 IP** 反查出口网卡（`net.Dial("udp", peer)` 后读 `LocalAddr`），多网卡 / Docker / VPN 场景下才不会选错。

### 2.3 与现有约束的冲突点（必须显式处理，不能绕过）

| 冲突 | 现有行为 | 处理 |
|---|---|---|
| 私网封禁 | `internal/services/whitelist.go` 对私网 IP 默认拒绝 | 新增 `player_output_allowlist`，**不复用**代理白名单（X-02） |
| ffmpeg 槽位 | 4 个并发转码槽位，`seekStreamSem` | 播放器独立 `player_max_groups`，默认 4（B0-08） |
| 直通路径无 PCM | 无转码时直接 302 到原文件 | 组内任一 EQ 非 flat 时强制解码，并占槽位（B6-04） |
| Docker 收不到组播 | bridge 网络屏蔽 239.255.255.250 / 224.0.0.251 | 文档写明 `--network host`、UI 空态提示、手动填 IP 兜底（X-03） |
| 外发代理 | `httputil.NewClient` 是代理感知客户端 | **DLNA SOAP 必须用裸 `http.Client`**，否则用户配了全局代理就连不上局域网设备（B4-03） |

## 3. 依赖图

```
B0-01 ──┬─► B1-01 ─► B1-02 ─► B1-03 ─┬─► B1-04 ─► B1-05 ─► B1-06
        │                            └─► B2-01 ─► B2-02 ─┐
B0-02 ──┴─► B0-03 ──────────────────────────────────────┴─► B2-04 ─► B2-06
B0-05 ─────────────────────────────────────────► B4-04 ─┘      │
B0-06 ────────────────────────────────────────────────────────┘
B0-08 ─────────────────────────────────► B6-04 / B7-04
        └─► B2-04 ─► B3-02 ─► B3-03 ─► B3-04 ─► B3-05 ─► [G1] ─► B3-08
        └─► B2-04 ─► B4-03 ─► B4-06 ─► [G2]
B3-05 + B4-03 ─► B5-01 ─► B5-05 ─► B5-06 ─► [G2]
B5-01 ─► B5-09 ─► B5-08 ─► [G2]
B2-01 ─► B6-01 ─► B6-03 ─► [G3]
B5-01 ─► B7-01 ─► B7-03 ─► [G4]
```

关键路径：`B0-01 → B1-02 → B2-01 → B2-04 → B3-05 → G1 → B5-01 → B5-09 → B5-08 → G2 → B7-07 → G4`。

前端 F0 可以在 B1-05 之前开工（对 `/playback` 打桩），这是唯一一条真正并行、且不增加返工风险的线。

## 4. 后端任务分解

### M0 地基（B0，11 人日）

无用户可见功能，但后面每个里程碑都踩在上面。

| ID | 任务 | 涉及文件 | 验收 | d |
|---|---|---|---|---|
| B0-01 | 迁移 `0040_players.sql` 建 6 张表（`players` / `player_groups` / `party_sessions` / `player_group_members` / `playback_queues` / `playback_states`）+ `queries/*.sql` + `make sqlc` + 仓储骨架 | `internal/database/migrations/0040_players.sql`、`internal/database/queries/players.sql`、`internal/database/player_repository.go`、`internal/database/sqlc/*` | 内存库起表；`players` 的 `kind` / `takeover_behavior` CHECK 约束有拒绝用例；`party_id` 外键删除行为有测试 | 2 |
| B0-02 | 可注入时钟 + NTP 换算 | `internal/player/clock.go` | 假时钟下单测；断言**墙钟回拨 1h 不影响时间轴**，NTP 换算单调 | 1 |
| B0-03 | 统一时间轴 | `internal/player/timeline.go` | 帧索引 ↔ 绝对时刻换算，44100/48000 两种采样率；latency 偏移可配 | 1 |
| B0-04 | 环形缓冲 | `internal/player/pipeline/ring.go` | 3s 容量；欠载时**补静音而非阻塞**；`underruns` 计数；并发读写 `-race` 干净 | 1.5 |
| B0-05 | 局域网地址解析 | `internal/player/lanurl.go` | 按对端 IP 选出口网卡（`udp` dial 后读 `LocalAddr`）；尊重 `base_path` 部署；对端为回环时明确报错而不是静默返回 127.0.0.1 | 1 |
| B0-06 | WS 服务端 hub + ticket 鉴权 | `internal/app/ws/hub.go`、`internal/handlers/playback_events.go` | `POST /playback/events/ticket` 一次性 30s、绑定用户、用后失效；慢客户端按丢弃策略处理（不阻塞广播）；`seq` 单调 | 2.5 |
| B0-07 | 两个配置端点 | `internal/handlers/player_setting.go`、`routers.go` | `/settings/player-discovery`、`/settings/player-output` 强类型 JSON + 默认值在 handler 内；`party_drift_tolerance_ms` 越界返回 400；swagger 已生成 | 1 |
| B0-08 | 播放器预算 | `internal/player/budget.go` | `player_max_groups` 默认 4；与 `seekStreamSem` **互不干扰**（各占满时互不阻塞，有测试）；超限错误可区分原因 | 0.5 |
| B0-09 | 事件契约 | `internal/player/event.go` | 8 类事件结构体 + JSON 编解码；`seq` 类型与去重语义单测 | 0.5 |

**退出标准**：`go test ./internal/player/...` 全绿，`make swagger` 无差异，两个配置端点可在 `/swagger/index.html` 里看到。

### M1 队列与传输（B1，9.5 人日）

| ID | 任务 | 涉及文件 | 验收 | d |
|---|---|---|---|---|
| B1-01 | Player 实体、能力模型、注册表 | `internal/player/player.go`、`manager.go` | `Caps` 编解码；`takeover_behavior` 只对 `local` 生效；注册/注销并发安全 | 1.5 |
| B1-02 | 队列语义 | `internal/player/queue.go` | `current/repeat(all\|one\|off)/shuffle/jump/insertNext`；表驱动用例覆盖"repeat=one 时 next 仍推进"、"shuffle 关闭后恢复原序"；**预加载判定（剩余 <30s）** | 2 |
| B1-03 | 传输状态机 | `internal/player/transport.go` | `idle→loading→buffering→playing⇄paused→error→recover`；`buffering→playing` 判据是**环形缓冲水位 2s**，不是首帧到达；从属成员掉线不改组状态 | 1.5 |
| B1-04 | 队列与状态落库 + 重启恢复 | `internal/player/snapshot.go` | 快照写入用**独立 `flushCtx`**，主循环取消不影响落库；重启后队列与位置恢复；写库失败不中断播放 | 2 |
| B1-05 | 队列 / 传输 / 快照 REST | `internal/handlers/playback.go` | `/groups`、`/groups/{id}/queue`、`/groups/{id}/queue/reorder`、`/groups/{id}/transport`、`/playback`；全部带 7 项 swag 注释；错误走 `respondError` | 2 |
| B1-06 | 单机自动成组 | `internal/player/manager.go` | 注册 player 即建一个只含自己的组；删除设备时组随之清理，不留孤儿 | 0.5 |

**退出标准**：不接任何真实 sink，用假 sink 能跑完"加载 → 播放 → 暂停 → seek → 下一首 → 重启恢复"。

### M2 解码管线与 `local` sink（B2，10.5 人日）

| ID | 任务 | 涉及文件 | 验收 | d |
|---|---|---|---|---|
| B2-01 | ffmpeg 单解码源 | `internal/player/pipeline/decoder.go` | `s16le/44100/2ch` 输出；进程退出码与 stderr 尾部进错误；取消时 kill 且不泄漏子进程；ffmpeg 路径取 `ffmpeg_path` | 3 |
| B2-02 | 预加载与无缝续接 | `internal/player/pipeline/decoder.go` | 下一首续写同一时间轴（不重建 Timeline）；假 sink 断言跨曲目 `rtp` 时间戳连续 | 1.5 |
| B2-03 | Sink 接口 + 假 sink | `internal/player/sink/sink.go`、`sink/fake/` | `Sink` / `PushSink` / `PullSink` 三接口；假 sink 记录每帧 `(sample_index, play_at, bytes)` 供断言 | 1 |
| B2-04 | `local` sink | `internal/player/sink/local/` | WS 下发 `load{play_at}`、`play/pause/seek`；客户端 500ms 上报 `{position,buffering,volume,state}`；RTT 折半估算时钟偏移 | 2.5 |
| B2-05 | 事件接线 | `internal/player/manager.go`、`ws/hub.go` | `state` / `member_changed` / `error` 从状态机到 WS 全链路；断线重连后客户端能靠 `/playback` 全量对齐 | 1 |
| B2-06 | 管线集成测试 | `internal/player/pipeline/pipeline_test.go` | 假 sink 断言：20ms 切帧（882 样本）、时间戳单调、欠载补静音不拖时间轴、`-race` 干净 | 1.5 |

**退出标准（v0.1）**：`make run` + Flutter 客户端（可先用调试页）能被服务端控制播放、暂停、seek、切歌。

### M3 RAOP（B3，12.5 人日）→ 门禁 G1

| ID | 任务 | 涉及文件 | 验收 | d |
|---|---|---|---|---|
| B3-01 | mDNS 发现 `_raop._tcp` | `internal/player/sink/raop/discover.go` | 引入 `github.com/hashicorp/mdns v1.0.7`（2026-06 仍维护；`grandcat/zeroconf` 停在 2020 且无 go.mod，不选）；解析 TXT（`ch/cn/et/sr/ss/pw/pk/tp`）与 `port`；`pw=true` 的设备标记为需要密码并跳过 | 1.5 |
| B3-02 | RTSP 客户端 | `internal/player/sink/raop/rtsp.go` | `OPTIONS/ANNOUNCE/SETUP/RECORD/FLUSH/TEARDOWN/SET_PARAMETER`；报文 golden bytes 测试；超时与重连 | 2 |
| B3-03 | SDP + 加密 | `internal/player/sink/raop/crypto.go`、`sdp.go` | RSA-OAEP-SHA1 封装 AES key、AES-128-CBC 加密；**已知向量测试**（固定 key/iv 的密文逐字节比对）；SDP 字段齐全 | 2 |
| B3-04 | RTP 打包 | `internal/player/sink/raop/rtp.go` | 12B 头、payload type `0x60`、seq/ts 递增、20ms 一包；sync 包 `0x54`（rtp↔ntp 映射）；golden bytes | 1.5 |
| B3-05 | 定时与延迟 | `internal/player/sink/raop/sink.go` | 起播 `at` 对齐到 RTP 时间戳；默认 2s latency；`clock.go` 换算单调 | 1 |
| B3-06 | 音量与进度 | `internal/player/sink/raop/sink.go` | `SET_PARAMETER volume` / `progress`；音量变更不打断流 | 0.5 |
| B3-07 | **G1 出声验证** | `scripts/tests/`（新增脚本） | 容器内 `shairport-sync --output pipe` 收流写文件；`ffprobe` 校验时长/采样率；波形相似度比对源文件 | 1 |
| B3-08 | 真机兼容矩阵 | `docs/playback_compat_matrix.md`（新增，双语） | ≥4 类设备实测（AirPort Express / Sonos / 软路由 shairport-sync / Soundbar），逐项记录 L16 是否被接受、音量是否生效、seek 是否准确 | 3（含等设备） |

**退出标准（v0.2）**：真机上连续播放 30 分钟不中断、不爆音、seek 后位置准确。

### M4 DLNA（B4，10 人日）

| ID | 任务 | 涉及文件 | 验收 | d |
|---|---|---|---|---|
| B4-01 | SSDP 发现 | `internal/player/sink/dlna/ssdp.go` | `M-SEARCH urn:schemas-upnp-org:device:MediaRenderer:1`；多网卡逐个发包；`MX` 与重发；注入式 transport 单测（不发真实组播） | 1.5 |
| B4-02 | desc.xml 解析 | `internal/player/sink/dlna/desc.go` | 取 `AVTransport` / `RenderingControl` / `ConnectionManager` 的 `controlURL` + `eventSubURL`；相对 URL 按 `LOCATION` 归一 | 1 |
| B4-03 | SOAP 客户端 | `internal/player/sink/dlna/soap.go` | `SetAVTransportURI/Play/Pause/Stop/Seek/SetVolume/GetPositionInfo/GetTransportInfo/SetNextAVTransportURI`；`SOAPAction` 头与 body golden 测试；**必须用裸 `http.Client`（不走代理感知客户端）** | 2 |
| B4-04 | DIDL-Lite + 流地址 | `internal/player/sink/dlna/didl.go` | 元数据含 title/artist/album/`protocolInfo`；流地址来自 B0-05 的局域网 URL | 0.5 |
| B4-05 | 事件订阅 + 轮询兜底 | `internal/player/sink/dlna/subscribe.go` | `SUBSCRIBE` 成功则用事件，失败退 3s 轮询；订阅续期 | 1.5 |
| B4-06 | 假 renderer 集成测试 | `internal/player/sink/dlna/soap_test.go` | `httptest` 起假设备，断言调用序列（URI → Play → 状态查询）；设备返回 500 时的错误映射 | 1.5 |
| B4-07 | 厂商方言补丁 | `internal/player/sink/dlna/vendor.go` | 按 `modelName` 注册补丁（空 `CurrentURIMetaData` 容忍、`Seek` 单位、无事件订阅）；未命中走默认 | 1.5 |
| B4-08 | 手动添加设备 | `internal/handlers/playback.go` | `POST /players` 接受 `{address, kind}` 直连；地址必须过 `player_output_allowlist` | 0.5 |

**退出标准（v0.3）**：≥3 款真机可被拉流播放、可暂停/seek/调音量；组播不可用时手动添加可用。

### M5 同步组（B5，17.5 人日）→ 门禁 G2

> 本里程碑是 v1 半成品最集中的地方。**“字段存在”“组能建”“事件发了”一律不算完成**，下面每条验收都写了可执行的断言形式。

| ID | 任务 | 涉及文件 | 验收 | d |
|---|---|---|---|---|
| B5-01 | 组主从与动态成员 | `internal/player/manager.go`、`group.go` | 播放中加入成员，**3 帧（≤70ms）内**新成员收到音频、原成员 `underruns` 不增；移出支持 `continue`/`stop` 两模式且不重开曲 | 2 |
| B5-02 | SyncTier 判定与准入 | `internal/player/manager.go`、`handlers/playback.go` | `AddMember` 把 `dlna` 加进 `raop/local` 组时返回 **400**，`detail` 含“不支持精确同步”；纯 RAOP 组恒为 `exact`；成员变化后 `sync_tier` 实时更新 | 0.5 |
| B5-03 | 能力协商 | `internal/player/player.go` | 采样率取组内最小；编码降级 `l16→aac→mp3`；2ch 上混（单声道源升 2ch 后两路样本相等） | 1 |
| B5-04 | 延迟补偿与音量偏移 | `internal/player/group.go`、`sink/*` | `delay_ms` 换算成帧数写入 sink 起播偏移，假 sink 断言偏移误差 **≤1 帧**；`volume_offset` 与设备音量叠加（`clamp(base+offset,0,100)`）有上下边界用例 | 1.5 |
| B5-05 | 转移与拆分 | `internal/player/transfer.go` | `only_player_id` 拆分；目标首帧位置 − 源当前位置 **∈ [780,820]ms**（假 sink 断言）；补偿只做一次，连续两次转移不叠加 | 1.5 |
| B5-06 | **同步精度测试** | `scripts/tests/sync_correlation.py`（新增） | 两个 shairport-sync 实例各 pipe 到独立文件，互相关求峰值延迟，断言 `< 20ms`；连续 10 分钟不漂移；结果写进 `docs/playback_compat_matrix.md`。**已完成脚本与自检：`--selftest` 注入 12ms 测得 12.0ms（相关 1.000），`--window 2` 分段无漂移；真实双实例 Not run** | 2 |
| B5-07 | 主控切换 | `internal/player/manager.go` | 切换主控不断流（时间轴不变，仅换持有者）；单成员组返回 400 | 0.5 |
| B5-08 | **同步校正环**（新） | `internal/player/sync_loop.go`（新）、`engine.go` | 每 950ms（`sync_check_interval_ms`）采一次各成员实际位置；参考设备 = 组内**最落后且不可跳帧**的成员；偏差 < `min_sync_adjust_ms`(10) 不动、> 10s 放弃；偏快 `SkipFrames`、偏慢注入静音（**不改变时间轴、不暂停解码器**）。假时钟注入 ±50ms / ±2s，**1 个周期内收敛到 ≤10ms**；10 分钟仿真残余漂移 ≤5ms；`-race` 干净 | 3 |
| B5-09 | **sink 热替换 + 每 sink 独立写协程**（新） | `pipeline/distribute.go`、`sink/sink.go` | `Sinks` 改原子快照，`SetSinks` **下一帧**生效；每 sink 独立有界队列（满则丢最旧并计数）；假 sink 注入 500ms 卡顿，**其他成员 `underruns` 不增** | 2 |
| B5-10 | 组音量联动（新） | `manager.go`、`handlers/playback.go` | `POST /groups/{id}/volume` 同步下发并各自叠加 `volume_offset`；`GET` 返回每成员实际值；部分失败返回 `failed[]` 且已成功的不回滚 | 1.5 |
| B5-11 | gapless 预取（新） | `engine.go`、`pipeline/`、`sink/dlna` | 剩余 ≤3s 预开下一曲解码器与 sink；交界处样本序号连续、无 `underrun`、间隔 <5ms；DLNA 调 `SetNextAVTransportURI`，不支持则降级为普通切换并记日志 | 2 |

**退出标准（v0.4）**：两台同型号 AirPlay 音箱同组播放，人耳听不出前后；自动化互相关 < 20ms；**人为把一台拖慢 2s，5s 内自动回位**；播放中加入第三台，70ms 内出声且前两台不断音。

### M6 每音箱 EQ（B6，7 人日）→ 门禁 G3

> 现有实现是 `pipeline/eq.go` 拼 ffmpeg `-af equalizer=...`，且 `manager.groupEQ` 只取主控一份。**这是必须拆掉的错方案**，不是“以后再优化”：每台音箱一个 ffmpeg 就退回多解码源，同步地基直接塔。B6-07 负责删除旧路径。

| ID | 任务 | 涉及文件 | 验收 | d |
|---|---|---|---|---|
| B6-01 | RBJ peaking biquad 级联 | `internal/player/pipeline/biquad.go` | 10 段（31/62/125/250/500/1k/2k/4k/8k/16k Hz）、Q=1.41；白噪/扫频 FFT 断言各频点增益误差 **< 0.2dB**；**保样本数**（输入 N 输出 N） | 2 |
| B6-02 | preamp 与削波保护 | `internal/player/pipeline/biquad.go` | `preamp = -max(0, max(band))`；+12dB 全开时全链路峰值 ≤ 0dBFS；API 返回实际 preamp | 0.5 |
| B6-03 | 每 sink 独立实例 + 热切换 | `internal/player/pipeline/` | EQ 在 **Distributor 之后、编码之前**逐样本处理，每路状态隔离；组内只给 A 开 +12dB 时，B 收到的样本与原信号逐位相等（断言 max diff = 0）；播放中改参数走 20ms 线性斜坡，波形无阶跃（断言相邻样本差 ≤ 0.1）；仅 `idle` 允许硬重置 | 1 |
| B6-04 | 强制解码规则 + 预算 | `internal/player/budget.go`、`group.go` | 组内任一 EQ 非 flat → 强制走服务端解码并占槽位；槽位满返回 503 且错误码区分"同时播放数已满"与"EQ 需要解码，槽位已满" | 1 |
| B6-05 | EQ REST + 预设复用 | `internal/handlers/playback.go`、`internal/services/equalizer_preset.go` | `GET/PUT /players/{id}/eq`、`GET /players/{id}/eq/presets`；预设与 `/settings/equalizer` **同一份常量**，不维护两套 | 1 |
| B6-06 | `local` sink 一致性 | `internal/player/sink/local/` | 本机入组时 EQ 也在服务端应用；下发字段让客户端禁用本地 EQ 并显示"EQ 由服务端应用" | 0.5 |
| B6-07 | **删除 ffmpeg 滤镜 EQ 路径**（新） | `internal/player/pipeline/eq.go`（删）、`decoder.go` | `rg -e BuildFilterChain -e 'equalizer=f='` 在 `internal/` 无命中；解码器不再接受 EQ 参数；EQ 非 flat 时 ffmpeg 命令行无 `-af`；现有 `/settings/equalizer` 行为不变 | 1 |

**退出标准（v0.5）**：任一音箱单独 +12dB@1kHz 可听出提升且不削波；组内其他音箱**逐位不受影响**；切 EQ 无爆音；`ffmpeg` 进程参数里搜不到 `equalizer`。

### M7 派对（B7，9.5 人日）→ 门禁 G4

| ID | 任务 | 涉及文件 | 验收 | d |
|---|---|---|---|---|
| B7-01 | 派对核心 | `internal/player/party.go` | `leader_group_id` 推进逻辑进度；`owner_kind='party'` 复用同一套队列/状态机代码（不写第二份）；派对不提供总音量 | 2 |
| B7-02 | 加入 / 退出 | `internal/player/party.go` | 加入从当前逻辑进度接入；退出 `continue`（继续自己队列）/`stop` 两模式；单组退出不影响其余 | 1 |
| B7-03 | 漂移**检测**（拆出） | `internal/player/party.go` | 每 950ms 算一次 offset；超 `party_drift_tolerance_ms`（默认 1500）发 `group_drift{corrected:false, offset_ms}`；校正动作不在本任务（B7-07） | 1 |
| B7-04 | 派对传输广播 + 预算 | `internal/player/party.go` | `POST /playback/party/transport` 广播到全组；**每组各占 1 个解码槽位**，4 组派对即吃满预算并明确拒绝 | 1 |
| B7-05 | `party_enabled=false` 拒绝 | `internal/handlers/playback.go` | 发起派对返回 403 + 可读原因；已在派对中的组不受影响 | 0.5 |
| B7-06 | 派对测试 | `internal/player/party_test.go`（**文件必须存在并进 CI**） | 假 sink 记录每组首个样本逻辑位置，3 组互差 < 1.5s；退出后邻组 `position` 抖动 < 100ms | 1.5 |
| B7-07 | **派对校正闭环**（新） | `internal/player/party.go` | 复用 B5-08 的采样，但只做**逻辑进度对齐**（落后组 seek 到 leader 当前进度，领先组等）；5s 内收敛到 <1.5s；连续 3 次失败 → `unaligned` 且停止重试、只广播一次；假 sink 拖慢 2s，断言 5s 内发 `corrected:true` 且 offset < 1500ms | 2 |

**退出标准（v1.0 功能面）**：3 组派对同时出声、进度差 < 1.5s、各自音量独立、任一组退出其余不断音；**人为拖慢一组能自己回位**；`party_test.go` 在 CI 里跑。

### M8 韧性与可观测（B8，6.5 人日）

| ID | 任务 | 涉及文件 | 验收 | d |
|---|---|---|---|---|
| B8-01 | 重启恢复 | `internal/player/snapshot.go`、`manager.go` | 服务重启后恢复到上次曲目与位置（误差 < 2s），设备自动重连 | 1 |
| B8-02 | 掉线 / 上线 | `internal/player/manager.go` | 成员掉线只标 `member_offline`，组状态不变；重连后自动重新入组并从当前时间点接入 | 1 |
| B8-03 | 指标与日志 | `internal/player/metrics.go` | `player_underruns_total`、`player_decode_slots_used`、`player_drift_ms`、`player_sink_dropped_frames_total`；`GET /playback/metrics` 返回快照；关键路径 `slog` 带 `player_id`/`group_id` | 1 |
| B8-04 | 设备/组生命周期端点（新） | `handlers/playback.go`、`manager.go` | `DELETE /players/{id}`（正在播放先 stop 再摘出所有组、清理孤儿组）、`DELETE /groups/{id}`（先 stop）、`POST /groups/{id}/master`；三者都有 swagger 与单测，含“删最后一台设备”与“删正在播放的组”两个边界 | 2 |
| B8-05 | 播放器 power / 待机（新） | `manager.go`、`sink/raop`、`sink/dlna` | `POST /players/{id}/power`；DLNA 发 `Stop` + 清空 URI，RAOP 停发 RTP 并标 `standby`（RTSP 无标准待机指令，不得假装已待机）；组级 `sync_power` 开关默认关；待机成员从同步环中排除 | 1.5 |

### X 跨端与工程收尾（非里程碑，8 人日，贯穿全程）

这些任务不属于某个里程碑，但**必须在对应里程碑内完成**，拖到最后会变成一次性补丁。

| ID | 任务 | 涉及文件 | 验收 | 归入 | d |
|---|---|---|---|---|---|
| X-01 | swagger 全量核对 | `docs/swagger.json`、`docs/swagger.yaml`、`docs/docs.go` | 新端点逐条 `grep` 有命中；`@Router` 无 `/api/v1`；每个 handler 7 项注释齐 | 每个里程碑 | 1 |
| X-02 | 播放器出口白名单 | `internal/services/whitelist.go`（新增独立入口）、`internal/handlers/player_setting.go` | `player_output_allowlist` 与代理白名单**互不影响**（双向用例：只开 A 时 B 仍被封）；拒绝时记审计日志 | M3 前 | 1.5 |
| X-03 | Docker 网络与发现兜底 | `docs/faq.md`、`docs/architecture.md`、前端空态 | 文档写明 `--network host` / macvlan；发现为空时 UI 给提示而非空白；手动填 IP 路径可用 | M3/M4 | 1 |
| X-04 | Bundle 模式限制 | `docs/architecture_frontend.md`、`internal/app/app.go`（启动时判定） | iOS Bundle 默认只留 `local` sink；桌面 Bundle 需显式开启；未开启时接口返回明确原因 | M2 | 1 |
| X-05 | 性能实测 | `scripts/tests/decode_baseline.sh`（新增） | 4 组并发解码时 CPU / 内存基线记录；单核设备上 4 组不卡顿的结论有数据支撑。**已完成：3 次实测 wall 0.14~0.17s、4 路 RSS 峰值 6~29 MiB、1085~1294× 实时** | M7 后 | 1 |
| X-06 | 文档回填 | `docs/architecture_playback*.md`、`docs/pitfalls.md`、`docs/playback_compat_matrix.md` | 设计稿状态从"尚未实现"改为"已实现（版本号）"；踩坑按 `LESSONS.md` 流程入库；双语同步。**已完成：对照矩阵落地（42 项，35 ✅ / 4 ⚠️ / 2 ❌）；ReplayGain 与闹钟已补齐，仅 crossfade 仍列差距** | v1.0 | 1 |
| X-07 | **半成品守卫（CI）**（新） | `Makefile`（`check-playback`）、`scripts/tests/guards.sh` | 一条命令跑完：同步收敛、EQ 逐位隔离、派对回位、`rg -e BuildFilterChain -e 'equalizer=f='` 无命中、`party_test.go` 存在、新端点 swagger 有命中；任一不过则 `make check` 红 | v0.4 起持续 | 1.5 |

## 5. 前端任务分解（另一仓库）

前端在 `clients/player`，落点见[交互设计 §7.1](./architecture_playback_ui.md)。**仓库已定（D1）**：子模块指向 `github.com/leonzy/roomtone-player`（fork 自 `songloft-org/songloft-player`，历史与子模块指针 SHA 一致），本地已检出。

| ID | 任务 | 主要文件 | 验收 | 依赖 | d |
|---|---|---|---|---|---|
| F0-01 | `output` 模块骨架 + API/仓储 | `lib/features/output/data/*` | `/playback` 打桩可跑；错误映射统一 | — | 2 |
| F0-02 | WS 客户端（ticket / 退避 / seq 去重 / 轮询降级） | `data/output_socket.dart` | 断线 3 次后退 3s 轮询并提示"实时状态不可用"；重连后拉 `/playback` 全量对齐 | B0-06 | 2 |
| F0-03 | 输出按钮 + 选择器 Sheet | `presentation/widgets/output_button.dart`、`output_sheet.dart` | 空态有引导 + 扫描按钮；离线目标置灰不隐藏；排序"在线→同网→名称" | F0-01 | 2.5 |
| F0-04 | 音箱管理页 | `features/settings/presentation/speaker_settings_page.dart` | 发现/命名/删除/延迟补偿/输出参数齐备；发现为空时给 Docker 网络提示 | B0-07 | 2 |
| F1-01 | 本地播放器双模接管 | `presentation/providers/playback_sync_provider.dart` | `isLocal` 时驱动 `just_audio` 零延迟，位置 1s 上报；非本机时静默 | B2-04 | 2 |
| F1-02 | 接管偏好 | `speaker_settings_provider.dart` | `pause`/`follow` 可切；被切走时按偏好暂停或静默降级；离线用本地缓存值 | F1-01 | 1 |
| F2-01 | 组内动态增删 + 音量 | `widgets/group_member_list.dart` | 播放中加入不断音；移出后独立播放；乐观更新失败回滚 | B5-01 | 2 |
| F2-02 | 同步徽标 | `widgets/sync_badge.dart` | `aligned` 琥珀色 + 图标 + 语义标签（不只靠颜色）；精确组里加 DLNA 前端先拦 | B5-02 | 1 |
| F3-01 | 转移 / 拆分 / 主控切换 | `use_cases/transfer_playback.dart`、`group_edit.dart` | 点击即标"切换中"；3s 内出声；位置误差 < 2s；失败回滚并提示 | B5-05 | 2 |
| F3-02 | 延迟补偿 UI | `widgets/group_member_list.dart` | 每成员 ±ms 可调，范围与后端校验一致 | B5-04 | 1 |
| F4-01 | 每音箱 EQ 页 | `features/settings/presentation/speaker_eq_page.dart`、`widgets/eq_sheet.dart` | 复用 `equalizer_panel.dart` 推子视觉；300ms 防抖；preamp 只读展示 | B6-05 | 2 |
| F4-02 | 本机 EQ 叠加保护 | `speaker_eq_page.dart` | 本机作为输出目标时本地 EQ 置灰 + 文案"EQ 由服务端应用" | B6-06 | 1 |
| F5-01 | 派对发起/加入/退出 | `widgets/party_section.dart`、`use_cases/party_edit.dart` | 派对中点赞其他目标 = 加入，5s 内可撤销；无"总音量"控件 | B7-02 | 2 |
| F5-02 | 漂移提示 | `widgets/drift_indicator.dart` | "正在对齐…"→"未对齐"两态；连续失败不反复弹 toast | B7-03 | 1 |
| F6-01 | TV / Lynx 适配 + 拖拽排序 | `features/output/**` | D-pad 可达全部动作；拖拽排序在 TV 上降级为按钮 | F2 | 3 |
| F2-03 | 组音量与偏移（新） | `widgets/group_volume.dart` | 组滑杆实时下发；成员可单独 ±10 微调并即时回显；部分失败提示失败成员 | B5-10 | 1 |
| F3-03 | 删除设备/组（新） | `use_cases/speaker_manage.dart` | 二次确认；删正在播放的组先提示“会停止播放”；失败回滚且列表不错位 | B8-04 | 1 |
| F6-02 | 待机/唤醒（新） | `widgets/power_toggle.dart` | 待机成员置灰、不可加入精确组；唤醒后自动回组并标“对齐中” | B8-05 | 1 |

前端合计 ~28 人日。F4 与 F5 互不依赖，可并行。

## 6. 门禁

门禁不通过时**不修改验收标准**，改范围或改技术方案。每条都写明失败后的 B 计划，避免到时临时拍脑袋。

| 门禁 | 判定方法 | 通过标准 | 失败后的 B 计划 | 决策时点 |
|---|---|---|---|---|
| **G1** L16 兼容性 | B3-07 自动化出声 + B3-08 真机矩阵 | ≥4 类设备接受 L16，音量/seek 正常 | 先试 AAC-ADTS（兼容性换有损）；仍不行则把 RAOP 降级为"实验性"并从 v0.2 移除，DLNA 提前 | M3 结束 |
| **G2** 同步精度 | B5-06 双实例互相关 + B5-08 仿真 + B5-09 热替换 | ①峰值延迟 < 20ms 且 10 分钟不漂移；②人为拖慢 2s，**5s 内回位**；③播放中入组 **≤70ms 出声**且原成员 `underruns` 不增；④`delay_ms=50` 时帧偏移误差 ≤1 帧 | ①不达标 → 个别设备走 `delay_ms`，整体不达标才降为 `SyncAligned`；②③不达标 → **不得发布 v0.4**，不得用“DLNA 本来就不精确”掩盖 RAOP 组的问题 | M5 结束 |
| **G3** EQ 质量 | B6-01/02/03/07 断言 | 频响误差 < 0.2dB；无 >0dBFS 样本；热切换无阶跃；**组内未开 EQ 的成员逐位不变（max diff = 0）**；`rg 'equalizer=f='` 无命中 | 热切换失败 → 只允许 `idle` 时改 EQ（UI 禁用播放中编辑）；逐位隔离失败 → 回到“组级 EQ”并**在 UI 明写“整组共用”**，不得假装每台独立 | M6 结束 |
| **G4** 派对对齐 | B7-06 + B7-07 假 sink | 3 组 < 1.5s；**拖慢 2s 能在 5s 内回位**；连续失败置 `unaligned` 且只广播一次；退出不扰邻组 | 达不到 → 派对降级为“多组同播同一首歌、各自进度”，并在 UI 明说不同步 | M7 结束 |

## 7. 验证矩阵

自动化能覆盖的都在 CI 里；只有真机项标人工。

| 里程碑 | 自动化 | 人工 |
|---|---|---|
| M0 | 时钟/时间轴/环形缓冲/预算单测；ticket 鉴权；`-race` | — |
| M1 | 队列表驱动、状态机全路径、重启恢复 | — |
| M2 | 管线集成（假 sink 断言帧/时间戳/补静音） | 客户端联调听感 |
| M3 | SDP/RTP/crypto golden bytes；容器 shairport-sync 出声 + 波形比对 | **真机矩阵 ≥4 类**（G1） |
| M4 | 假 renderer 的 SOAP 序列断言 | ≥3 款真机 |
| M5 | 双实例互相关 < 20ms；假时钟收敛仿真；热替换 ≤70ms；`delay_ms` 帧偏移；`-race`（G2） | 人耳同组听感 |
| M6 | FFT 频响、削波、阶跃、**组内逐位隔离**、`rg` 无滤镜残留（G3） | 听感（爆音、音量变化） |
| M7 | 假 sink 逻辑位置差 < 1.5s、**拖慢 2s 回位**、`unaligned` 只广播一次（G4） | 三房间实测 |
| M8 | 重启恢复、掉线重连 | 拔电/断网演练 |

CI 里能跑的真机替代物只有 shairport-sync（Linux 容器），DLNA 只有假 renderer——所以 DLNA 与 AirPlay 的真机结论必须落进 `docs/playback_compat_matrix.md`，不能只写在 Issue 评论里。

## 8. 排期

单人全职，含验证。周次从开工算起，不绑日历日期。

| 周 | 后端 | 前端 | 验证/其他 |
|---|---|---|---|
| W1–W2 | M0 地基 | — | — |
| W3–W4 | M1 队列与传输 | F0-01 骨架（打桩） | — |
| W5–W6 | M2 解码 + local sink → **v0.1** | F0-02/03 选择器 | 客户端联调 |
| W7–W9 | M3 RAOP → **G1** → **v0.2** | F0-04 音箱管理 | 真机矩阵（需设备） |
| W10–W12 | M4 DLNA → **v0.3** | F1 双模接管 | 真机 ≥3 款 |
| W13–W15 | M5 同步组（校正环 / 热替换 / 预取）→ **G2** → **v0.4** | F2 + F3 | 同步精度 + 收敛仿真 |
| W16 | M6 EQ（biquad 重写 + 删滤镜路径）→ **G3** → **v0.5** | F4 EQ | EQ 听感 |
| W17–W18 | M7 派对（检测 + 校正）→ **G4** | F5 派对 | 三房间实测 |
| W19 | M8 韧性 + X 收尾 → **v1.0** | F6 TV/Lynx | 全量回归 |

v2 比 v1 多 4 周（~19 周 ≈ 4.4 月），多出来的全是“把半成品做完整”的部分：校正环 3d、热替换与独立写协程 2d、预取 2d、biquad 重写与删旧路径 3d、派对校正 2d、生命周期与 power 3.5d、CI 守卫 1.5d。若中途卡在真机等待上，优先推进不依赖硬件的 M5-08/09、M6、M7（假 sink 可测），不要空转。

## 9. 提交与协作约定

- 直接提交 `main`，一个任务一个提交，提交信息带任务 ID：`feat(player): 队列语义与传输状态机 (B1-02)`。
- 迁移文件、`make sqlc` 产物、`make swagger` 产物与业务代码**同一提交**，不拆。
- 每个 handler 落地即写 7 项 swag 注释（`@Summary/@Description/@Tags/@Produce/@Success/@Security/@Router`），`@Router` 不带 `/api/v1`。
- 新增/修改文档同步英文版；新增文档同时进 `docs/` 与 `docs/en/`。
- 提交前 `gofmt -w .`、`go vet ./...`、`make check`；前端 `dart format lib/ test/`。
- 相关未跑项在交付说明里写 `Not run`，不省略。

## 10. 待定决策

| 编号 | 问题 | 建议 | 截止 |
|---|---|---|---|
| **D1** | 前端代码提交到哪个仓库 | **已定**：fork 为 `leonzy/roomtone-player`，`.gitmodules` 与本地检出均指向该仓库 | ✅ 已决 |
| **D2** | mDNS 库选型 | `github.com/hashicorp/mdns v1.0.7`（2026-06 维护中）；`grandcat/zeroconf` 停更 6 年不选。若不愿加依赖，B3-01 预算 +2d 手写查询 | B3-01 开工前 |
| **D3** | G1 失败后走 AAC-ADTS 还是砍 RAOP | 看真机矩阵：≥3 类可用就保留 L16 + 个别设备 AAC 兜底；<3 类再评估 | G1 当天 |
| **D4** | 派对内能否单独换歌 | 当前不允许（共享内容）。若实测强烈需要，做"退出派对再换"而非"脱离内容" | v1.0 后收集反馈 |
| **D5** | EQ 是否支持 REW / AutoEQ 导入 | 先不做，等有用户真拿麦克风测房间 | v1.0 后 |
| **D6** | 真机测试清单 | **部分确定**：AirPlay 与 DLNA 两类设备都有，可支撑 G1 与 M4 人工验收；具体型号待 B3-08 开测时填入兼容矩阵 | 型号随 B3-08 补录 |

## 11. 进度表

任务级明细见 §4 / §5，这里只留里程碑勾选，避免两处状态打架。

- [x] M0 地基（B0-01 ~ B0-09）——已完成
- [x] M1 队列与传输（B1-01 ~ B1-06）——已完成
- [x] M2 解码与 local sink（B2-01 ~ B2-06）→ v0.1——已完成；客户端联调 Not run
- [x] M3 RAOP（B3-01 ~ B3-08）→ G1 → v0.2——代码完成；**G1 真机矩阵 Not run**
- [x] M4 DLNA（B4-01 ~ B4-08）→ v0.3——代码完成；真机 Not run
- [x] M5 同步组（B5-01 ~ B5-11）→ G2 → v0.4——代码与脚本全部完成；**G2 真机双设备互相关 Not run**（脚本自检通过，见 X-06）
- [x] M6 每音箱 EQ（B6-01 ~ B6-07）→ G3 → v0.5——已完成；G3 真机听感 Not run
- [x] M7 派对（B7-01 ~ B7-07）→ G4——已完成（`party_test.go` 在 CI）；G4 真机 Not run
- [x] M8 韧性与可观测（B8-01 ~ B8-05）→ v1.0——已完成（B8-04 生命周期端点、B8-03 metrics、B8-05 power）
- [x] 前端 F0 ~ F6——已完成（`clients/player`：F0 面板/待机、F1 建组与成员参数、F2 转移、F3 EQ、F4 派对、F5 指标、F6 待机置灰）；真机与 TV/Lynx 走查 Not run；成员排序用上移/下移（TV 无拖拽）接 `PUT /groups/{id}/members/order`，"对齐中"接 `member_caps[].syncing`
- [~] X 收尾：X-02 白名单 / X-04 Bundle 限制 / X-05 性能基线 / X-06 文档回填（含对照矩阵）/ X-07 守卫已完成；**X-01/X-03 部分完成**（Swagger 已全量核对至本轮新增端点，Docker 发现兜底文档待补真机结论）

### 11.1 下一批提交顺序（按依赖排，一次一个提交）

1. **同步闭环**：`B5-09`（热替换 + 独立写协程）→ `B5-08`（校正环）→ `B5-04`（delay/offset）→ `B5-02`（400 准入）→ `B5-01`（70ms 入组）→ `B5-05`（+800ms）→ 重跑 `B5-06`，一次过 G2。
2. **EQ 重写**：`B6-01`（biquad）→ `B6-03`（每 sink 实例 + 斜坡）→ `B6-07`（删滤镜路径）→ `B6-06`（local 一致性）→ `B6-05`（补 presets）→ G3。
3. **派对闭环**：`B7-03`（检测重写）→ `B7-07`（校正）→ `B7-06`（`party_test.go`）→ G4。
4. **生命周期与韧性**：`B8-04`（DELETE/master）→ `B8-03`（metrics）→ `B8-05`（power）→ `B5-11`（预取）→ `B5-10`（组音量）→ `B8-02`（offline 事件）。
5. **守卫与收尾**：`X-07`（`make check-playback`）→ `X-02` → `X-04` → `X-05` → `X-06`。
6. **前端**：`F0` → `F1` → `F2` → `F3` → `F4` → `F5` → `F6`（每完成一个里程碑即接一个 F 阶段，不留到最后）。

> 每一步都以 X-07 的守卫跑绿为提交前提；守卫不过就不提交，不把问题留给下一批。

## 12. 风险与缓冲

设计稿 §10 的风险表继续有效，这里只列**会改变计划形状**的几条：

| 风险 | 对计划的影响 | 缓冲 |
|---|---|---|
| G1 失败（L16 不被接受） | M3 追加 2 周（AAC 链路重做 + 重新测矩阵） | W7–W9 预留 1 周 |
| 真机迟迟不到位（D6） | M3-08 / M4 人工验收全部挂起 | 优先做 M6/M7 的假 sink 部分 |
| 单解码源欠载爆音 | M2 返工，影响后面所有里程碑 | B0-04 缓冲与补静音提前到 M0 做扎实 |
| 派对漂移被听成回声 | M7 降级，产品定位调整 | 门禁 G4 的 B 计划已定 |
| ~~前端仓库决策拖延（D1）~~ | 已消除：`leonzy/roomtone-player` 已建并指向子模块 | — |
| **验收写成描述→半成品** | v1 已发生：三个任务“完成”但功能未闭环，返工 4 周 | v2 起验收必须可执行；X-07 守卫进 CI；描述性验收在自审阶段直接打回 |
| 校正环改动触及 engine 核心 | 可能引发 M2 回归 | 先做 B5-09 隔离写协程，B5-08 只做“采样 + 注入”，不改解码器 |
| 真机不到位但声称“已实现” | 文档与实际不符，后续排查成本高 | 未真机验证的一律写 Not run，不写“已完成” |
