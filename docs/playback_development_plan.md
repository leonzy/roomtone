# 播放控制与音箱输出 · 开发计划

> **状态：可执行计划 v1（2026-10-25）。** 设计与决策见[播放控制与音箱输出架构](./architecture_playback.md)（后端）与[播放与音箱交互设计](./architecture_playback_ui.md)（前端）。本文不重复设计理由，只回答三件事：**按什么顺序做、动哪些文件、怎么算做完**。
>
> 任务粒度 = 一次可独立提交、可独立验证的变更。任务 ID 写进提交信息（`feat(player): 队列语义与传输状态机 (B1-02)`），实现与追溯一一对应。

## 0. 使用方式

- 进度维护在 §11 进度表，完成一项勾一项；任务表本身不标状态，避免两处不一致。
- 门禁（§6）不通过不进下一阶段。宁可砍范围（例如把 DLNA 推迟到 v0.4），也不下调验收标准——同步精度和削波这类指标一旦放宽，等于把这个功能的卖点删掉。
- 估算是**单人全职人日，含自测与文档**，不含等真机、等硬件的时间（单独标注）。
- 每周结束跑一次全量 `make check`；跨周不改验收标准。

## 1. 交付边界

| 版本 | 里程碑 | 用户能感知到什么 | 累计人日 |
|---|---|---|---|
| **v0.1（内部）** | M0 + M1 + M2 | 服务端有队列与播放状态，能控制客户端播放（`local` sink） | ~31 |
| **v0.2** | + M3 | **能推给 AirPlay 音箱出声**，单台，不同步 | ~44 |
| **v0.3** | + M4 | 加 DLNA 音箱，单台/多台各自播 | ~54 |
| **v0.4** | + M5 | **多房间同步**（RAOP 精确、含 DLNA 降级） | ~62 |
| **v0.5** | + M6 | 每台音箱独立 EQ | ~68 |
| **v1.0** | + M7 + M8 + X | 派对模式、断点续播、可观测、安全与文档收口 | ~85 |

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
B2-01 ─► B6-01 ─► B6-03 ─► [G3]
B5-01 ─► B7-01 ─► B7-03 ─► [G4]
```

关键路径：`B0-01 → B1-02 → B2-01 → B2-04 → B3-05 → G1 → B5-01 → B5-06 → G2 → B7-03 → G4`。

前端 F0 可以在 B1-05 之前开工（对 `/playback/targets` 打桩），这是唯一一条真正并行、且不增加返工风险的线。

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

### M5 同步组（B5，8.5 人日）→ 门禁 G2

| ID | 任务 | 涉及文件 | 验收 | d |
|---|---|---|---|---|
| B5-01 | 组主从与动态成员 | `internal/player/group.go` | 播放中加入成员从**当前时间点**接入，原成员不断音；移出支持 `continue`/`stop` 两模式 | 2 |
| B5-02 | SyncTier 判定 | `internal/player/group.go` | 含 DLNA 成员 → 整组 `aligned` 并返回 `sync_tier`；**拒绝**把 DLNA 加入已有精确组（400 + 明确文案） | 0.5 |
| B5-03 | 能力协商 | `internal/player/caps.go` | 采样率取组内最小；编码降级 `l16→aac→mp3`；2ch 上混 | 1 |
| B5-04 | 延迟补偿与组内音量 | `internal/player/group.go` | `delay_ms` 生效（假 sink 断言帧偏移）；`volume_offset` 与设备音量叠加规则明确 | 1 |
| B5-05 | 转移与拆分 | `internal/player/transfer.go` | `POST /groups/{id}/transfer` 支持 `only_player_id` 拆分；目标以 `position + 800ms` 起播，**补偿只做一次** | 1.5 |
| B5-06 | **同步精度测试** | `scripts/tests/`（新增） | 两个 shairport-sync 实例各 pipe 到独立文件，互相关求峰值延迟，断言 `< 20ms`；连续 10 分钟不漂移 | 2 |
| B5-07 | 主控切换 | `internal/player/group.go` | 切换主控不断流（时间轴不变，仅换持有者）；单成员组不允许切换 | 0.5 |

**退出标准（v0.4）**：两台同型号 AirPlay 音箱同组播放，人耳听不出前后；自动化互相关 < 20ms。

### M6 每音箱 EQ（B6，6 人日）→ 门禁 G3

| ID | 任务 | 涉及文件 | 验收 | d |
|---|---|---|---|---|
| B6-01 | RBJ peaking biquad 级联 | `internal/player/pipeline/biquad.go` | 10 段（31/62/125/250/500/1k/2k/4k/8k/16k Hz）、Q=1.41；白噪/扫频 FFT 断言各频点增益误差 **< 0.2dB**；**保样本数**（输入 N 输出 N） | 2 |
| B6-02 | preamp 与削波保护 | `internal/player/pipeline/biquad.go` | `preamp = -max(0, max(band))`；+12dB 全开时全链路峰值 ≤ 0dBFS；API 返回实际 preamp | 0.5 |
| B6-03 | 每 sink 独立实例 + 热切换 | `internal/player/pipeline/` | 每路 EQ 状态隔离；播放中改参数走 20ms 线性斜坡，波形无阶跃（断言样本差 ≤ 0.1）；仅 `idle` 允许硬重置 | 1 |
| B6-04 | 强制解码规则 + 预算 | `internal/player/budget.go`、`group.go` | 组内任一 EQ 非 flat → 强制走服务端解码并占槽位；槽位满返回 503 且错误码区分"同时播放数已满"与"EQ 需要解码，槽位已满" | 1 |
| B6-05 | EQ REST + 预设复用 | `internal/handlers/playback.go`、`internal/services/equalizer_preset.go` | `GET/PUT /players/{id}/eq`、`GET /players/{id}/eq/presets`；预设与 `/settings/equalizer` **同一份常量**，不维护两套 | 1 |
| B6-06 | `local` sink 一致性 | `internal/player/sink/local/` | 本机入组时 EQ 也在服务端应用；下发字段让客户端禁用本地 EQ 并显示"EQ 由服务端应用" | 0.5 |

**退出标准（v0.5）**：任一音箱单独 +12dB@1kHz 可听出提升且不削波；组内其他音箱不受影响；切 EQ 无爆音。

### M7 派对（B7，7.5 人日）→ 门禁 G4

| ID | 任务 | 涉及文件 | 验收 | d |
|---|---|---|---|---|
| B7-01 | 派对核心 | `internal/player/party.go` | `leader_group_id` 推进逻辑进度；`owner_kind='party'` 复用同一套队列/状态机代码（不写第二份）；派对不提供总音量 | 2 |
| B7-02 | 加入 / 退出 | `internal/player/party.go` | 加入从当前逻辑进度接入；退出 `continue`（继续自己队列）/`stop` 两模式；单组退出不影响其余 | 1 |
| B7-03 | 漂移检测与校正 | `internal/player/party.go` | 超 `party_drift_tolerance_ms`（默认 1500）触发校正并广播 `group_drift{corrected}`；连续 3 次未对齐后停止重试并置 `unaligned` 状态 | 1.5 |
| B7-04 | 派对传输广播 + 预算 | `internal/player/party.go` | `POST /playback/party/transport` 广播到全组；**每组各占 1 个解码槽位**，4 组派对即吃满预算并明确拒绝 | 1 |
| B7-05 | `party_enabled=false` 拒绝 | `internal/handlers/playback.go` | 发起派对返回 403 + 可读原因；已在派对中的组不受影响 | 0.5 |
| B7-06 | 派对测试 | `internal/player/party_test.go` | 假 sink 记录每组首个样本逻辑位置，3 组互差 < 1.5s；人为拖慢一组 2s，5s 内发出 `corrected:true` 并回位 | 1.5 |

**退出标准（v1.0 功能面）**：3 组派对同时出声、进度差 < 1.5s、各自音量独立、任一组退出其余不断音。

### M8 韧性与可观测（B8，3 人日）

| ID | 任务 | 涉及文件 | 验收 | d |
|---|---|---|---|---|
| B8-01 | 重启恢复 | `internal/player/snapshot.go`、`manager.go` | 服务重启后恢复到上次曲目与位置（误差 < 2s），设备自动重连 | 1 |
| B8-02 | 掉线 / 上线 | `internal/player/manager.go` | 成员掉线只标 `member_offline`，组状态不变；重连后自动重新入组并从当前时间点接入 | 1 |
| B8-03 | 指标与日志 | `internal/player/metrics.go` | `player_underruns_total`、`player_decode_slots_used`、`player_drift_ms`；关键路径 `slog` 带 `player_id`/`group_id` | 1 |

### X 跨端与工程收尾（非里程碑，6.5 人日，贯穿全程）

这些任务不属于某个里程碑，但**必须在对应里程碑内完成**，拖到最后会变成一次性补丁。

| ID | 任务 | 涉及文件 | 验收 | 归入 | d |
|---|---|---|---|---|---|
| X-01 | swagger 全量核对 | `docs/swagger.json`、`docs/swagger.yaml`、`docs/docs.go` | 新端点逐条 `grep` 有命中；`@Router` 无 `/api/v1`；每个 handler 7 项注释齐 | 每个里程碑 | 1 |
| X-02 | 播放器出口白名单 | `internal/services/whitelist.go`（新增独立入口）、`internal/handlers/player_setting.go` | `player_output_allowlist` 与代理白名单**互不影响**（双向用例：只开 A 时 B 仍被封）；拒绝时记审计日志 | M3 前 | 1.5 |
| X-03 | Docker 网络与发现兜底 | `docs/faq.md`、`docs/architecture.md`、前端空态 | 文档写明 `--network host` / macvlan；发现为空时 UI 给提示而非空白；手动填 IP 路径可用 | M3/M4 | 1 |
| X-04 | Bundle 模式限制 | `docs/architecture_frontend.md`、`internal/app/app.go`（启动时判定） | iOS Bundle 默认只留 `local` sink；桌面 Bundle 需显式开启；未开启时接口返回明确原因 | M2 | 1 |
| X-05 | 性能实测 | `scripts/tests/`（新增基准） | 4 组并发解码时 CPU / 内存基线记录；单核设备上 4 组不卡顿的结论有数据支撑 | M7 后 | 1 |
| X-06 | 文档回填 | `docs/architecture_playback*.md`、`docs/pitfalls.md`、`docs/playback_compat_matrix.md` | 设计稿状态从"尚未实现"改为"已实现（版本号）"；踩坑按 `LESSONS.md` 流程入库；双语同步 | v1.0 | 1 |

## 5. 前端任务分解（另一仓库）

前端在 `clients/player`，落点见[交互设计 §7.1](./architecture_playback_ui.md)。**仓库已定（D1）**：子模块指向 `github.com/leonzy/roomtone-player`（fork 自 `songloft-org/songloft-player`，历史与子模块指针 SHA 一致），本地已检出。

| ID | 任务 | 主要文件 | 验收 | 依赖 | d |
|---|---|---|---|---|---|
| F0-01 | `output` 模块骨架 + API/仓储 | `lib/features/output/data/*` | `/playback/targets` 打桩可跑；错误映射统一 | — | 2 |
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

前端合计 ~25 人日。F4 与 F5 互不依赖，可并行。

## 6. 门禁

门禁不通过时**不修改验收标准**，改范围或改技术方案。每条都写明失败后的 B 计划，避免到时临时拍脑袋。

| 门禁 | 判定方法 | 通过标准 | 失败后的 B 计划 | 决策时点 |
|---|---|---|---|---|
| **G1** L16 兼容性 | B3-07 自动化出声 + B3-08 真机矩阵 | ≥4 类设备接受 L16，音量/seek 正常 | 先试 AAC-ADTS（兼容性换有损）；仍不行则把 RAOP 降级为"实验性"并从 v0.2 移除，DLNA 提前 | M3 结束 |
| **G2** 同步精度 | B5-06 双实例互相关 | 峰值延迟 < 20ms 且 10 分钟不漂移 | 若仅个别设备不达标 → 该设备默认 `delay_ms` 补偿；整体不达标 → 定位改为 `SyncAligned`（"近似同步"），产品文案同步下调 | M5 结束 |
| **G3** EQ 质量 | B6-01/02/03 断言 | 频响误差 < 0.2dB；无 >0dBFS 样本；热切换无阶跃 | 热切换失败 → 只允许 `idle` 时改 EQ（UI 禁用播放中编辑）；削波失败 → preamp 加 1dB 余量并复测 | M6 结束 |
| **G4** 派对对齐 | B7-06 假 sink | 3 组 < 1.5s，退出不扰邻组 | 达不到 → 派对降级为"多组同播同一首歌、各自从头/各自进度"，并在 UI 明说不同步 | M7 结束 |

## 7. 验证矩阵

自动化能覆盖的都在 CI 里；只有真机项标人工。

| 里程碑 | 自动化 | 人工 |
|---|---|---|
| M0 | 时钟/时间轴/环形缓冲/预算单测；ticket 鉴权；`-race` | — |
| M1 | 队列表驱动、状态机全路径、重启恢复 | — |
| M2 | 管线集成（假 sink 断言帧/时间戳/补静音） | 客户端联调听感 |
| M3 | SDP/RTP/crypto golden bytes；容器 shairport-sync 出声 + 波形比对 | **真机矩阵 ≥4 类**（G1） |
| M4 | 假 renderer 的 SOAP 序列断言 | ≥3 款真机 |
| M5 | 双实例互相关 < 20ms（G2） | 人耳同组听感 |
| M6 | FFT 频响、削波、阶跃断言（G3） | 听感（爆音、音量变化） |
| M7 | 假 sink 逻辑位置差 < 1.5s、校正收敛（G4） | 三房间实测 |
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
| W13–W14 | M5 同步组 → **G2** → **v0.4** | F2 + F3 | 同步精度测试 |
| W15 | M6 EQ → **G3** → **v0.5** | F4 EQ | EQ 听感 |
| W16–W17 | M7 派对 → **G4** | F5 派对 | 三房间实测 |
| W18 | M8 韧性 + X 收尾 → **v1.0** | F6 TV/Lynx | 全量回归 |

与设计稿 §9 的估算一致（P0~P3.5 ≈ 3.75 月）。若中途卡在真机等待上，优先推进不依赖硬件的 M6 / M7（假 sink 可测），不要空转。

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

- [ ] M0 地基（B0-01 ~ B0-09）
- [ ] M1 队列与传输（B1-01 ~ B1-06）
- [ ] M2 解码与 local sink（B2-01 ~ B2-06）→ v0.1
- [ ] M3 RAOP（B3-01 ~ B3-08）→ G1 → v0.2
- [ ] M4 DLNA（B4-01 ~ B4-08）→ v0.3
- [ ] M5 同步组（B5-01 ~ B5-07）→ G2 → v0.4
- [ ] M6 每音箱 EQ（B6-01 ~ B6-06）→ G3 → v0.5
- [ ] M7 派对（B7-01 ~ B7-06）→ G4
- [ ] M8 韧性与可观测（B8-01 ~ B8-03）→ v1.0
- [ ] 前端 F0 ~ F6
- [ ] X 收尾：swagger 全量、`player_output_allowlist`、Docker 网络文档、Bundle 限制、性能实测、设计稿状态回填

## 12. 风险与缓冲

设计稿 §10 的风险表继续有效，这里只列**会改变计划形状**的几条：

| 风险 | 对计划的影响 | 缓冲 |
|---|---|---|
| G1 失败（L16 不被接受） | M3 追加 2 周（AAC 链路重做 + 重新测矩阵） | W7–W9 预留 1 周 |
| 真机迟迟不到位（D6） | M3-08 / M4 人工验收全部挂起 | 优先做 M6/M7 的假 sink 部分 |
| 单解码源欠载爆音 | M2 返工，影响后面所有里程碑 | B0-04 缓冲与补静音提前到 M0 做扎实 |
| 派对漂移被听成回声 | M7 降级，产品定位调整 | 门禁 G4 的 B 计划已定 |
| ~~前端仓库决策拖延（D1）~~ | 已消除：`leonzy/roomtone-player` 已建并指向子模块 | — |
