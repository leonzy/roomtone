# 播放控制与音箱输出架构（架构与实现）

> **状态（2026-10-25 验收后）。** 本文描述 Songloft 从"客户端权威的媒体服务器"扩展为"服务端持有播放状态、可推送到局域网音箱"的架构。
>
> 落地进度：**M0–M8 全部实现**（队列/状态机、解码管线、`local`/`raop`/`dlna` sink、设备发现、同步组与校正环、每音箱 EQ、派对、可观测），逐项验收断言见[开发计划](./playback_development_plan.md)；**真机项（G1 矩阵、G2 双设备互相关、G3 听感、G4 派对）一律 `Not run`**。
>
> 与 SlimServer（LMS）的逐项能力对照见**[播放能力对照矩阵](./playback_compat_matrix.md)**（42 项：33 持平或超过、4 有差距、4 未实现）。
>
> **实施路径见[开发计划](./playback_development_plan.md)**（任务拆分、涉及文件、门禁与排期）。
>
> 参考实现：Lyrion Music Server（`../slimserver`，Perl）的 `Slim/Player/Source.pm`、`Sync.pm`、`CapabilitiesHelper.pm`；shairport-sync（RAOP 接收端反向参考）；OwnTone（RAOP 发送端参考）。

## 1. 目标与非目标

### 1.1 目标

| # | 目标 | 说明 |
|---|---|---|
| G1 | **服务端播放队列与状态** | 队列、当前曲目、进度、repeat/shuffle 由服务端持有，多端一致 |
| G2 | **推送到 AirPlay 音箱（AirPlay 1 / RAOP）** | 服务端主动把 PCM 推给音箱，不依赖客户端 |
| G3 | **推送到 DLNA/UPnP 音箱** | 服务端通过 SOAP 控制 renderer，由设备拉流 |
| G4 | **多房间同步** | AirPlay 组精确同步；DLNA 仅对齐起播 |
| G5 | **自研客户端作为播放器入组** | Flutter 客户端注册为 `local` sink，可与其他音箱同组。⏳ **客户端侧未实现**（服务端已就绪，见 §5.2） |
| G6 | **断点续播** | 服务重启 / 设备重连后恢复曲目与位置 |
| G7 | **派对模式** | 多个组同时播放同一首歌，各组独立音量、独立进出，互不影响（§4.7） |
| G8 | **每音箱均衡器** | 每台音箱独立的 10 段 EQ，在服务端 PCM 管线内生效（§4.8） |

### 1.2 非目标

- **AirPlay 2**（HomePod、Apple TV 等 AP2-only 设备）。理由见 §2。
- **Chromecast / 原生 Sonos / 原生 TIDAL Connect**。Chromecast 可作为后续独立 sink 扩展。
- **视频推送**。现有 HLS 视频投屏链路（`/songs/{id}/stream?media=video`）保持不变。
- **插件脚本直接控制播放器**。第一版只暴露 REST/WS，插件桥接留待后续（§8.5）。
- **派对模式下的跨组采样级同步**。派对只保证"同一首歌、同一逻辑进度、1.5s 内一致"（§4.7），不做跨组 ±20ms。需要采样级精确请用同步组。

## 2. 协议能力事实

先把可行性钉死，避免后期返工。

| 协议 | 服务端推送 | 同步精度 | 共享时钟 | 开源发送端现状 |
|---|---|---|---|---|
| **RAOP（AirPlay 1）** | ✅ RTP 推流 | **±10~20ms** | ✅ sync 包携带 `rtp ↔ ntp` 映射 | OwnTone `raop.c`；Go 侧无可用库（3 个仓，均停更/零 star） |
| **AirPlay 2** | ✅ 缓冲流 | ±10ms | ✅ PTP/NTP | **无成熟开源发送端**；需 FairPlay SAP + HAP 配对 + 加密缓冲流 |
| **DLNA / UPnP** | ⚠️ 仅发指令，设备自己拉流 | **±200~500ms 且会漂移** | ❌ 协议层无时钟 | 控制面有成熟库（`goupnp`），同步无解 |

结论：

1. **RAOP 必须自研**，但协议公开、有接收端反向实现可对照，工作量可控（约 3k 行 Go）。
2. **AirPlay 2 不做**。它需要逆向 FairPlay 握手与配对流程，业界没有可依赖的发送端实现，投入产出比不可接受。AP2-only 设备在 UI 中明确标记为"不支持"。
3. **DLNA 不做精确同步**。这不是实现质量问题，是协议缺失共享时钟导致的上限。必须通过 `SyncTier` 显式建模并向用户明示（§4.3）。

> 补充事实：LMS 自身也没有原生 AirPlay/DLNA 输出，其生态依赖 `airupnp`/`ShairTunes2W` 桥接，且**该桥接不具备同步能力**。本设计在这一点上超出 LMS。

## 3. 总体架构

```
                     REST / WebSocket
   ┌─────────────────────────────────────────────────────────────┐
   │  internal/player                                            │
   │                                                             │
   │  Party ─► Queue ─► Transport ─► Timeline ─┬─► EQ ─► raop.Sink
   │  (派对)    (队列)     (状态机)    (统一时钟)  ├─► EQ ─► local.Sink
   │                                │          └─► EQ ─► dlna.Sink
   │                          Decoder(ffmpeg)
   │                          单解码源 · 单 PCM 流
   └─────────────────────────────────────────────────────────────┘
                                    │
              ┌─────────────────────┼─────────────────────┐
              ▼                     ▼                     ▼
        RTP/RTSP → AirPlay      WS 指令 → 客户端       SOAP → DLNA
```

三层职责：**Party 决定"放什么"**（内容单位），**Group 决定"谁跟谁完全同拍"**（同步单位），**EQ 决定"每台设备听起来怎样"**（设备单位）。三者互不替代，详见 §4.7 / §4.8。

包结构（对齐 `internal/` 现有分层，Service 层调用，Handler 只做协议转换）：

```
internal/player/
  manager.go       播放器注册表、发现调度、状态事件总线
  player.go        Player 实体与能力模型
  group.go         同步组：主从、延迟补偿、格式协商
  queue.go         队列语义（current / repeat / shuffle / jump / preload）
  transport.go     传输状态机
  timeline.go      统一时间轴与帧分发
  party.go         派对：多组共享队列与逻辑进度、漂移校正
  budget.go        并发解码组数上限（独立于转码槽位）
  eq.go            每音箱 EQ 配置读写与 preamp 计算
  pipeline/
    decoder.go     ffmpeg → s16le 单解码源 + 环形缓冲
    resample.go    组内采样率对齐（复用 ffmpeg -ar）
    biquad.go      RBJ peaking 级联（每 sink 独立实例，保样本数）
  sink/
    sink.go        Sink / PushSink / PullSink 接口
    local/         WebSocket 指令 + 客户端拉流
    raop/          RTSP + RTP + AES + 定时
    dlna/          SSDP + SOAP + DIDL-Lite
  clock.go         monotonic ↔ NTP 换算（可注入，供测试）
```

## 4. 核心机制

### 4.0 区域（Zone）语义

用户可见的播放单位是**区域**，后端叫 `group`。两者一一对应，不引入新实体：

| 概念 | 后端实体 | 说明 |
|---|---|---|
| 区域 | `groups` 行 | 一个或多个输出设备；单成员组 = 单机区域 |
| 输出 | `players` 行 | `local` / `raop` / `dlna` |
| 控制焦点 | 客户端本地状态 | “我在看哪个区域”，**不进服务端**，不影响任何播放 |

三条硬约束：

1. **多区域并行**。每个区域一个引擎（`manager.go` 的 `engines["group:"+id]`），各自持有队列、传输状态与时间轴，可同时出声。`GET /playback` 返回全部区域，不是“当前区域”。
2. **切焦点不发命令**。客户端切区域只改本地 `PlaybackFocus`；只有行内传输、音量、显式转移才发请求。
3. **转移是显式动作**。`POST /groups/{to}/transfer` 不会被“点击另一个区域”隐式触发（v2 前端设计曾如此，v3 已改，见[交互设计 §4.2](./architecture_playback_ui.md)）。
4. **并行有上限**。每个正在播放的区域占 1 个解码槽位（默认 4，`budget.go`），派对里每个组各占 1，EQ 非 flat 的组强制占 1。第 5 个区域起播返回 503 `ErrBudgetExhausted`，UI 必须提示“同时播放数已达上限”而不是“转码繁忙”。这是与 Roon 的硬差距之一（§11.1 H5），不靠 UI 能消除。

**本机是内置区域**：客户端启动后必须通过 WS `register_player` 把自己注册为 `local` 设备，服务端自动建单机组。未注册时本机不会出现在任何列表里——这是“播放输出页没有本机”的根因（见 §5.2）。

### 4.1 单解码源多路分发（同步的地基）

**绝不能**每个音箱各自起一个 ffmpeg：进程启动抖动 + 缓冲深度差异，起步就是几百毫秒偏差，且无法收敛。

正确做法（即 LMS `Source.pm` 的 `shift->master()` 语义：主控持有唯一源，从属共享）：

```
一个 ffmpeg 进程 → s16le/44100/2ch → 环形缓冲(3s) → 每 20ms 切 882 帧
                                                   ├─ raop A: 同一份字节 + 同一 RTP 时间戳
                                                   ├─ raop B: 同一份字节 + 同一 RTP 时间戳
                                                   └─ local C: 同一份字节 + 同一 play_at
```

同步因此退化为"每路按自己的时间戳播放"，精度只取决于协议时钟能力，与 Go 侧调度无关。

**但这只是「起播对齐」，不是持续同步**：设备侧缓冲深度差异、Wi-Fi 抖动与重传会让偏移随时间累积，10 分钟后两台音箱就是两个声音。要长期维持 ±20ms 必须叠加 §4.3.1 的校正环——这也是 LMS 与所有成熟多房间系统的必备组件。

**副产品**：单解码源天然支持**无缝播放**——下一首直接续写同一个 RTP 时间轴，不需要设备支持 gapless。DLNA 侧仍需 `SetNextAVTransportURI`。

**唯一的例外是 EQ**：每台音箱的均衡器不同，分发出去的字节必然不同。EQ 因此插在**分发之后、编码之前**，但因为它保样本数，时间轴仍然共用（§4.8）。

### 4.2 统一时间轴

```go
type Timeline struct {
    t0         time.Time     // 组起播时刻（monotonic 基准）
    sampleRate int           // 组内统一采样率
    anchor     uint32        // 起播时的 RTP 时间戳
    latency    time.Duration // 默认 2s（RAOP 缓冲窗口）
}

// 第 n 个采样帧的绝对播放时刻
func (t *Timeline) At(n uint64) time.Time {
    return t.t0.Add(time.Duration(float64(n)/float64(t.sampleRate)*float64(time.Second)) + t.latency)
}
```

- 时钟用 `time.Now()` 的 monotonic 读数，**不使用系统墙钟**（NTP 跳变会撕裂时间轴）。
- RAOP 需要的 NTP 时间由 `clock.go` 从同一基准换算，保证 `rtp ↔ ntp` 映射单调一致。
- `clock` 必须可注入（`type Clock interface{ Now() time.Time }`），否则定时相关逻辑无法确定性测试。

### 4.3 同步等级（必须显式建模）

```go
type SyncTier int

const (
    SyncExact   SyncTier = iota // RAOP / local：±20ms
    SyncAligned                 // DLNA：仅对齐起播，会漂移
)
```

规则：

- 组内全是 `SyncExact` 成员 → 精确同步。
- 组内含任一 DLNA 成员 → 整组降级为 `SyncAligned`，API 返回 `sync_tier`，UI 明确提示"包含 DLNA 设备，同步精度下降"。
- **禁止**把 DLNA 成员加入已有精确同步组而不降级——这会让用户体验从"还行"直接掉到"没法听"。

可选项（默认关闭）：`dlna_realign_interval`，每 N 分钟静默 stop→seek→play 重对齐，代价是一次可听断点。

### 4.3.1 同步校正环（v2 新增，`SyncExact` 的必要条件）

对齐起播只能保证"一起开始"，保证不了"一直一起"。校正环是 LMS `StreamingController.pm:483 _CheckSync` 的等价物，参数与动作直接对应：

| 参数 | 值 | 说明 |
|---|---|---|
| `sync_check_interval_ms` | 950 | 每周期采一次各成员实际播放位置 |
| `min_sync_adjust_ms` | 10 | 小于此值不动，避免来回抖 |
| `max_deviation_ms` | 10000 | 超过视为设备异常，交给掉线流程（B8-02） |
| 参考设备 | 组内**位置最落后且不可跳帧**的成员 | 追最慢的，而不是追最快的：追快的要丢弃大量音频，必然可听 |

动作：

- **偏快**（`pos > ref + eps`）→ `SkipFrames(delta)`：RAOP 丢弃对应 RTP 时间戳的帧，时间戳连续、不产生空洞；`local` 下发 `skip`。
- **偏慢**（`pos < ref − eps`）→ 注入静音帧补齐（**不暂停解码器、不改时间轴**）；对 DLNA 等价于暂时降速拉流。
- 位置来源：RAOP = 已发送 RTP 时间戳换算 − 协议缓冲；`local` = 客户端 500ms 上报；DLNA = 已发送字节 / 码率 + 估计缓冲（精度差，因此 DLNA 组只做 `SyncAligned`）。

**为什么不是「整组停下来等最慢的」**：那会让整个组反复卡顿；校正环是"快的等一点、慢的补一点"，用户听到的是稳态。

**为什么还需要每设备 `delay_ms`**：不同型号的 DSP 与缓冲差异是**恒定偏移**，校正环只能修漂移，修不了恒定偏移（强行修会让那台设备长期处于跳帧状态）。因此每台设备一个 `delay_ms`（±500ms），起播时一次性施加，校正环只处理残余。

**可听性约束**：单周期单成员只调一次；单次调整超过 200ms 时改用静音补齐而非连续跳帧；所有调整都记 `player_drift_ms` 与 `slog`，便于事后定位是哪台设备在拖。

### 4.4 队列与传输状态机

队列语义对齐 LMS `Playlist.pm`：

- `currentIndex` + `repeat(all|one|off)` + `shuffle`
- `jump(index)`、`next/prev`、`insertNext`（插播）
- **预加载**：当前曲目剩余 ≤30s 启动下一首解码（填环形缓冲），剩余 ≤3s 完成 sink 预开（RAOP 续写同一 RTP 时间轴；DLNA 调 `SetNextAVTransportURI`），保证无缝

状态机：

```
idle ──load──► loading ──► buffering ──► playing ⇄ paused
                   │            │            │
                   └────────────┴────────────┴──► error ──recover──► idle
```

- `buffering` 到 `playing` 的判据是**环形缓冲水位**（默认 2s），不是"第一帧到达"。
- `error` 携带可读原因（连不上设备 / 格式不支持 / ffmpeg 缺失），直接映射到 API 错误。
- 组状态 = 主控状态；从属成员掉线不改变组状态，只标记 `member_offline`。

### 4.5 能力协商与格式统一

```go
type Caps struct {
    Codecs        []Codec // l16 / alac / aac / mp3 / wav / flac
    MaxSampleRate int
    Channels      int
    VolumeRange   [2]float64 // dB 或 0-100，按 kind 解释
}
```

协商规则（对应 LMS `CapabilitiesHelper::samplerateLimit`）：

1. 采样率取组内**所有成员上限的最小值**；混入只支持 48k 的设备则整组 48k。
2. 编码按兼容性降级：`l16 → aac → mp3`（无损优先，但以"能出声"兜底）。
3. 声道统一为 2ch；单声道源上混。
4. DSD / APE / M4A 等由 ffmpeg 在解码阶段统一转 PCM，Go 侧不碰解码。

### 4.6 缓冲与欠载

- 环形缓冲目标水位 2s（与 RAOP latency 对齐），最低 500ms。
- 欠载时**补静音**而不是等：保持 RTP 时间轴连续，否则整组会被拖偏。
- 欠载计数暴露为 `player_underruns_total` 指标，用于定位慢盘 / 慢网络。

### 4.7 派对模式（多组同播同一内容）

**问题**：同步组回答"这几台设备如何完全同拍"，但一个组只有一条队列、一套传输状态。用户真正要的是"厨房和客厅放同一首歌，音量各调各的，谁先进谁先出都不影响别人"——这是**内容层**的共享，不是**时钟层**的合并。

**结论：派对不是"更大的组"，而是组之上的一层。** 组仍是同步单位（±20ms），派对是内容单位（1.5s 内）。这也解释了为什么不能简单把两组合并：合并会让客厅的 DLNA 成员把厨房的精确组一起降级。

```go
type Party struct {
    ID       string
    Name     string
    Queue    *Queue          // 共享队列，与组队列同一套语义
    Groups   map[string]*Group
    Leader   string          // 最早加入的在线组，负责推进 Position
    Position time.Duration   // 逻辑进度，由 Leader 的 state 驱动
}
```

规则：

- **共享内容，独立时间轴**：每个组仍各自持有解码器与输出管线（格式、音量、同步等级都按组算）。派对只共享"哪首歌、第几秒"。
- **对齐方式**：`|group.position - party.position| > 1500ms` 时对该组发 `seek`；连续 3 次校正失败标记 `out_of_sync`，UI 明示并建议改用同步组。不做更细的校正——派对场景容忍 1 秒级偏差，追求更细只会引入可听断点。
- **控制面**：`play/pause/next/prev/seek` 广播到全派对；`volume/mute` 永远按组或按音箱，**不提供派对总音量**（各房间声学条件不同，统一音量必然有人听不清）。
- **队列归属**：发起派对时把发起组的队列提升为派对队列，各组原队列保留不动（与 transfer 一样不销毁）。退出派对时该组用自己的原队列恢复 `idle`。
- **加入 / 退出**：加入从 `party.position` 接入（与"播放中加入成员"同语义）；退出默认"分离并继续"——用派对当前曲目与位置新建该组自己的队列。
- **容量**：每组占 1 个解码器，派对总组数同样受 `player_max_groups`（默认 4）约束，见 §8.4。
- **结束**：`DELETE /playback/party` 让所有组停播并回到各自队列，不删除任何队列。
- **与转移互斥**：派对期间对成员组调 `POST /groups/{id}/transfer` 返回 **409**，错误信息为"该组在派对中，请先退出派对"。队列是派对共享的，单组把队列抽走会让其余组无内容可放，语义无法定义。想"只要这一台"就先把该组退出派对。
- **不共享解码器的理由**：派对内的组格式可能不同（48k 的 DLNA 组 + 44.1k 的 RAOP 组），且 RAOP 有 2s 固定缓冲而 DLNA 没有；强行共享只会把两组拖进同一个降级。宁可多花一个 ffmpeg 进程换"每组保持自己最好状态"。

### 4.8 每音箱均衡器（服务端 DSP）

**为什么必须在服务端**：现有 EQ（`/settings/equalizer`，10 段、-12~+12dB、mpv `af` 实现）作用于客户端本地播放器。一旦播放由服务端推流，客户端只收到已编码字节，本地 EQ 完全失效——音箱听到的是未经校正的声音。

**为什么在 Go 里做，而不是 ffmpeg 滤镜**：ffmpeg 的 `equalizer` 滤镜只能作用于它自己的输出，一个进程只能挂一条滤镜链。按 sink 各起一个 ffmpeg 就退回"多解码源"，同步地基直接塌掉（§4.1）。所以 EQ 放在**分发之后、编码之前**，由 Go 逐样本处理。

```
单解码源 ──► 分发 ──┬─► EQ(客厅, +3dB@100Hz) ──► ALAC ──► RAOP
                    ├─► EQ(厨房, flat)        ──► AAC  ──► RAOP
                    └─► EQ(书房, -2dB@2kHz)   ──► WAV  ──► DLNA
```

**同步为什么不受影响**：10 段 peaking biquad 级联是**保样本数**的逐样本滤波，不插入也不吞掉采样。同一个采样索引在任何一路都对应同一个时间戳，"同一份字节 + 同一时间戳"退化为"同一采样索引 + 同一时间戳"——同步成立的前提（时间轴一致）没有变，只是各路振幅不同。

- **滤波器**：RBJ peaking EQ，中心频率沿用现有 10 段（31/62/125/250/500/1k/2k/4k/8k/16k Hz），Q 固定 1.41；`preset` 复用 `/settings/equalizer` 的同一套名字（flat/rock/pop/jazz/classical/bass_boost/treble_boost/vocal/custom），避免维护两套预设。
- **管线顺序**：`decode → loudnorm(可选, 组级) → 分发 → EQ(每 sink) → 编码`。loudnorm 是内容级归一，EQ 是房间级校正，后者必须在后；loudnorm 的 `TP=-1.5` 也为 EQ 的 preamp 留了余量。
- **群延迟**：同一采样率下各 sink 的级联群延迟完全一致（毫秒级），组内相对同步零影响；绝对延迟并入 `default_latency_ms`，不做单独补偿。
- **削波**：`+12dB` 必然削波。启用非 flat EQ 时自动施加 `preamp = -max(0, max(band))`，保证峰值不超过 0dBFS；API 必须返回实际 `preamp`，UI 必须显示，否则用户会以为"开了 EQ 声音变小是 bug"。
- **热切换**：播放中改 EQ 会重置滤波器状态导致爆音。做法是 20ms 线性斜坡过渡 preamp，滤波器状态在新系数下继续；只有 `idle` 时才允许硬重置。
- **必须解码**：直通路径（无转码、远程 URL 直出）拿不到 PCM。规则是**该组任一成员 EQ 非 flat 时，强制走服务端解码管线**，并如实占用一个 `player_max_groups` 槽位；槽位不足返回 503 且错误信息说明是 EQ 导致。
- **`local` sink 也走服务端 EQ**：与其它 sink 一致，避免出现"同一组内音箱和本机 EQ 不同源"。本机作为服务端输出目标期间，客户端**自动禁用**本地 EQ 并在 UI 注明"EQ 由服务端应用"，防止叠加两次；客户端自主播放（服务端不介入）时仍用本地 EQ。

## 5. Sink 实现

### 5.1 接口划分

```go
type Sink interface {
    ID() string
    Kind() Kind
    Caps() Caps
    Open(ctx context.Context, plan StreamPlan) error
    Start(at time.Time) error
    Seek(sample uint64) error
    SetVolume(v float64) error
    Stop() error
    Close() error
    Events() <-chan Event
}

// PushSink 接收帧（raop、local）
type PushSink interface {
    Sink
    WriteFrame(f Frame) error
}

// PullSink 只给地址，设备自己拉（dlna）
type PullSink interface {
    Sink
    StreamURL(plan StreamPlan) (string, error)
}
```

`Timeline` 只向 `PushSink` 分发帧；`PullSink` 仅在 `Start` 时刻对齐。

### 5.2 `local` sink（自研客户端）

> **状态：服务端已实现，客户端未实现（F7-02）。** `internal/app/playback.go:356` 的 `register_player` 分支与 `sink/local` 都在，但 `clients/player` 从未发过这条消息，所以 `local` sink 目前只有单测里的假客户端。

```
客户端 WS 连 /api/v1/playback/events
  → register_player {id, kind:"local", name, caps:{codecs:[...], sample_rates:[44100,48000]}}
服务端下发 load {song_id, position, play_at:<unix_ms>}
客户端按 (play_at - 本地时钟偏移) 起播
客户端每 500ms 上报 {position, buffering, volume, state, rtt_ms}
```

- 时间偏移用服务端返回的 `server_time` 与客户端本地时间做一次 RTT 折半估算，局域网内可稳定在 ±50ms。
- 客户端拉流复用现有 `/songs/{id}/stream`，`format` 由协商结果决定。
- **这是性价比最高的一档**：不引入任何外部协议风险，却能让"手机 + 客厅音箱"立刻可同组。

**注册生命周期（客户端必须遵守）**：

| 时机 | 动作 | 服务端行为 |
|---|---|---|
| 启动 / WS 连上 | `register_player`（`id` 持久化，重启不变） | `UpsertPlayer` + 自动建单机组 + 广播 `speaker_seen` |
| 每 500ms | `report` | 刷新 `last_seen_at`，判在线 |
| 断线 > 30s | — | 标 `online=false`，广播 `member_changed{action:offline}` |
| 重连 | 同 `id` 重新 `register_player` | 复用原区域，不新建 |
| 正常退出 | `unregister_player` | 摘出所有组、清理孤儿组、释放 sink |

- **`id` 必须持久化**：用随机 UUID 存本地，不要用设备型号或 IP——否则重装/换网就变成一个新区域，旧区域变僵尸。
- **服务端接管期间客户端不得自行应用 EQ**：EQ 在服务端 PCM 管线里已经做过（§4.8），客户端再开一次就是双重均衡。
- **未注册时的降级**：客户端仍可本地播放（纯本地模式），但区域列表里显示“本机输出不可用 + 重试”，不能静默隐藏。

### 5.3 `raop` sink（AirPlay 1）

报文序列（RTSP over TCP，端口取 mDNS TXT 的 `port`，默认 5000）：

```
mDNS:  _raop._tcp  TXT: txtvers=1 ch=2 cn=0,1,2,3 et=0,3,5 sr=44100 ss=16
                        pw=false pk=<设备 RSA 公钥> tp=UDP

OPTIONS  *              → Public: ANNOUNCE, SETUP, RECORD, PAUSE, FLUSH, TEARDOWN,
                                  OPTIONS, GET_PARAMETER, SET_PARAMETER
ANNOUNCE rtsp://…       → SDP:
                            m=audio 0 RTP/AVP 96
                            a=rtpmap:96 L16/44100/2
                            a=rsaaeskey:<RSA-OAEP-SHA1(AESKey, pk)>
                            a=aesiv:<base64(16B)>
SETUP                   → Transport: RTP/AVP/UDP;unicast;interleaved=0-1;
                          control_port=<c>;timing_port=<t>
RECORD                  → 起播
[20ms]  RTP 音频包       payload type 0x60，12B 头 + AES-128-CBC(payload)
[1s]    sync 包          类型 0x54：rtp_now + ntp_sec + ntp_frac + rtp_latency
[按需]  SET_PARAMETER    volume: -20.0 / progress: 0/12.5/240
FLUSH                    RTP-Info: seq=…;rtptime=…   ← seek
TEARDOWN
```

加密全部使用标准库：`crypto/rsa`（OAEP-SHA1）、`crypto/aes`（CBC）、`encoding/base64`。

**同步原理**：`sync` 包把 `rtp_now` 与 `ntp_time` 配对，接收端据此把 RTP 时间戳映射到自身时钟；组内所有音箱收到同一份映射，因此天然对齐。这是 AirPlay 能真同步、DLNA 永远不能的根本原因。

**编码格式是唯一的技术赌点**：

| 方案 | 兼容性 | 成本 | 备注 |
|---|---|---|---|
| L16（裸 PCM） | shairport-sync 系 ✅；老 AirPort Express / 部分硬件 ❌ | 零（无需编码器） | **首选，先打通** |
| AAC-ADTS | 多数 ✅ | 低（ffmpeg `-f adts`） | 有损，与无损定位冲突，仅兜底 |
| ALAC | 最佳 | 高（Go 无成熟编码器） | 若 L16 兼容性不足再评估 |

上线前必须产出**真机兼容矩阵**（至少：AirPort Express、Sonos、软路由 + shairport-sync、主流国产/日系 Soundbar、Apple TV 不测因不支持）。

### 5.4 `dlna` sink

```
SSDP M-SEARCH  ST: urn:schemas-upnp-org:device:MediaRenderer:1
  → 200 OK  LOCATION: http://<ip>:<port>/desc.xml
  → 解析 desc.xml，取 AVTransport / RenderingControl / ConnectionManager 的 controlURL

SOAP SetAVTransportURI(InstanceID=0, CurrentURI=<流地址>, CurrentURIMetaData=<DIDL-Lite>)
SOAP Play(Speed=1)
SOAP GetPositionInfo / GetTransportInfo      ← 轮询（有事件订阅时改用 SUBSCRIBE）
SOAP SetVolume(Channel=Master, DesiredVolume=0..100)
SOAP SetNextAVTransportURI(…)                ← 无缝下一首（非所有设备支持）
SUBSCRIBE <eventSubURL>                      ← 设备主动上报状态
```

- 流地址复用 `internal/services/internal_url.go` 解析出的**局域网可达绝对 URL**（不要给 `127.0.0.1`）。
- 起播对齐：全部先 `SetAVTransportURI` 进入暂停态，再同一时刻并发 `Play`。
- 漂移不可避免：各设备 buffer 深度不同，实测 200~500ms 且持续累积。按 §4.3 降级处理。
- 国产音箱 SOAP 方言差异大（`CurrentURIMetaData` 空值容忍度、`Seek` 单位、事件订阅缺失），需要按 `modelName` 打兼容补丁，并保留"手动填 IP 添加"兜底。

### 5.5 设备发现（`internal/player/discovery`）

```
mDNS  _raop._tcp  →  ServiceEntry(Name/Host/Port/TXT)  →  raop.Found
SSDP  M-SEARCH    →  200 OK(LOCATION)                  →  desc.xml  →  dlna.Device
                     ↓
             Scanner（每种协议一个）→ Speaker{ID,Kind,Name,Address,Caps}
                     ↓
   Discovery：并发扫描 → 按 id 去重 → 跳过 pw=true → Manager.RegisterPlayer
```

- **扫描与登记分开**：`sink/raop`、`sink/dlna` 只解析协议报文，翻译成 `Speaker` 的逻辑在 `discovery.Scanner`，登记（建组、落库、发 `speaker_seen`）交给 `player.Manager`。协议层因此不需要认识数据库。
- **多网卡逐个发包**：SSDP 绑定的源地址取自 `net.Interfaces()` 的每个 IPv4，Docker macvlan / 双网卡机器上只发默认路由那张会漏设备；mDNS 由 `hashicorp/mdns` 内部遍历网卡。
- **MX 与重发**：`M-SEARCH` 带 `MX: 2`，默认重发 2 次（间隔按超时均分）。UDP 丢包在局域网里很常见，只发一次会漏掉一半设备。
- **地址归一**：RAOP 的 `pk` 是 base64 的 DER，入库前统一转 PEM（`raop.NormalizePublicKey`）；DLNA 的 `Address` 存完整的 `LOCATION` URL，因为 `desc.xml` 路径各家不同，只存 `host:port` 建 sink 时还得再猜。
- **跳过需要密码的设备**：RAOP 的 `pw=true` 是 FairPlay 认证，不是明文密码，做了也放不出声，直接记入 `skipped` 并在 UI 提示。
- **配置**：`/settings/player-discovery` 的 `enabled` 只控制周期扫描；手动 `POST /players/discover` 始终可用，受 `protocols` 限制。保存后立即唤醒扫描循环，不用等旧间隔睡完。
- **手动添加兜底**：`POST /players` 收 `{address, kind}`，DLNA 先取 `desc.xml` 校验 AVTransport，RAOP 需要 `device_id`（RTSP 路径里的 MAC）。`/settings/player-output` 的 `allow_private_network` 为 false 时拒绝私网地址。

## 6. 数据模型

迁移 `0040_players.sql`：

```sql
CREATE TABLE players (
    id            TEXT PRIMARY KEY,          -- raop:AA:BB:… / dlna:uuid:… / local:<device_id>
    kind          TEXT NOT NULL CHECK(kind IN ('local','raop','dlna')),
    name          TEXT NOT NULL,
    address       TEXT NOT NULL DEFAULT '',
    model         TEXT NOT NULL DEFAULT '',
    capabilities  TEXT NOT NULL DEFAULT '{}', -- JSON，Caps
    volume        INTEGER NOT NULL DEFAULT 0, -- 归一化 0-100
    muted         INTEGER NOT NULL DEFAULT 0,
    online        INTEGER NOT NULL DEFAULT 0,
    eq_profile    TEXT NOT NULL DEFAULT '{}',  -- JSON：{enabled,preset,bands[10],preamp}
    takeover_behavior TEXT NOT NULL DEFAULT 'pause'  -- 本机被切走时：pause | follow（仅 local 有意义）
                  CHECK(takeover_behavior IN ('pause','follow')),
    volume_mode   TEXT NOT NULL DEFAULT 'device'     -- 0043：device | fixed | dsp
                  CHECK(volume_mode IN ('device','fixed','dsp')),
    max_volume    INTEGER NOT NULL DEFAULT 100       -- 0043：音量硬上限 0-100
                  CHECK(max_volume BETWEEN 0 AND 100),
    last_seen_at  DATETIME,
    created_at    DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE TABLE player_groups (
    id               TEXT PRIMARY KEY,
    name             TEXT NOT NULL DEFAULT '',
    icon             TEXT NOT NULL DEFAULT '', -- 0044：区域图标 key，空串=默认
    master_player_id TEXT NOT NULL DEFAULT '',
    party_id         TEXT,                   -- 非空 = 该组正在某个派对中
    created_at       DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE TABLE party_sessions (
    id              TEXT PRIMARY KEY,
    name            TEXT NOT NULL DEFAULT '',
    leader_group_id TEXT NOT NULL DEFAULT '', -- 推进逻辑进度的组
    created_at      DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE TABLE player_group_members (
    group_id      TEXT NOT NULL,
    player_id     TEXT NOT NULL,
    position      INTEGER NOT NULL DEFAULT 0,  -- 列表顺序，不改 master
    delay_ms      INTEGER NOT NULL DEFAULT 0,  -- 人工延迟补偿
    volume_offset INTEGER NOT NULL DEFAULT 0,  -- 组内相对音量
    PRIMARY KEY (group_id, player_id)
);

CREATE TABLE playback_queues (
    owner_kind TEXT NOT NULL CHECK(owner_kind IN ('group','party')),
    owner_id   TEXT NOT NULL,
    position   INTEGER NOT NULL,
    song_id    INTEGER NOT NULL,
    PRIMARY KEY (owner_kind, owner_id, position)
);

CREATE TABLE playback_states (
    owner_kind TEXT NOT NULL CHECK(owner_kind IN ('group','party')),
    owner_id   TEXT NOT NULL,
    song_id    INTEGER NOT NULL DEFAULT 0,
    position   REAL NOT NULL DEFAULT 0,
    state      TEXT NOT NULL DEFAULT 'idle',
    updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    PRIMARY KEY (owner_kind, owner_id)
);
```

设计说明：

- **单机也建一个只含自己的组**，队列与传输逻辑只写一份。等价于 LMS 的 `master()` 简化。
- `playback_states` 承担断点续播（对应 LMS `persistPlaybackStateForPowerOff`）。
- 队列落库而非纯内存，是为了"服务重启不丢队列"；内存态仍以 group 结构体为准，落库做快照。
- `playback_queues` / `playback_states` 用 `owner_kind + owner_id` 泛化，**派对复用同一套队列与状态机代码**，不写第二份实现；`owner_kind='party'` 时 `owner_id` 为 `party_sessions.id`。
- `eq_profile` 存在 `players` 行而不是独立表：EQ 是设备属性，与 volume 同级，一起读一起改；`preamp` 是服务端算出的派生值，存库只为避免每次重算，读时以最新 `bands` 校验。
- `party_id` 放 `player_groups` 而非独立的成员表：一个组同时只能在一个派对里，用外键列表达比多对多表更贴近真实约束。

## 7. API

客户端如何消费这些端点（选择器、转移、动态分组、双模本地播放）见[播放与音箱交互设计（前端）](./architecture_playback_ui.md)。

### 7.1 REST

路径相对 `/api/v1`（`main.go` 的 `@BasePath`），与 `internal/app/routers.go` 一一对应；字段与状态码以开发模式下的 `/swagger/index.html` 为准。

**设备**

| 方法 | 路径 | 请求体 / 参数 | 说明 |
|---|---|---|---|
| GET | `/players` | — | 设备列表：在线、待机、能力、音量、EQ |
| POST | `/players` | `{address, kind, name?, device_id?, public_key?}` | 手动添加；`kind` 为 `raop` / `dlna`，成功 201；网络音箱被关闭时 403 |
| POST | `/players/discover` | — | 触发 SSDP + mDNS 扫描，返回 `{speakers, added, updated, skipped, errors}`；发现被关闭 503 |
| PUT | `/players/{id}` | `{name?, takeover_behavior?, volume_mode?, max_volume?}` | 重命名 / 多端冲突策略（`pause` / `follow`）/ 音量模式（`device` / `fixed` / `dsp`）与上限（0-100，调低时当前音量一并钳位） |
| DELETE | `/players/{id}` | — | 永久删除：先停引擎再摘成员；单机组在派对里会先退出派对 |
| POST | `/players/{id}/volume` | `{volume, muted}` | 单机音量；先钳到 `max_volume`，`volume_mode=fixed` 时忽略并返回当前值 |
| POST | `/players/{id}/power` | `{on}` | 待机 / 唤醒；`standby` 只表示服务端不推流，不代表断电 |
| POST | `/players/{id}/reconnect` | — | 丢弃缓存的 sink 重新握手；设备仍离线 409 |
| GET | `/players/{id}/eq` | — | 含服务端算出的 `preamp` |
| PUT | `/players/{id}/eq` | `{enabled, preset, bands[10]}` | 服务端钳增益、重算 `preamp` 后回填，客户端必须用返回值刷新 |
| GET | `/players/{id}/eq/presets` | — | 预设列表（与 `/settings/equalizer` 同一套） |

**同步组**

| 方法 | 路径 | 请求体 / 参数 | 说明 |
|---|---|---|---|
| GET | `/groups` | — | 组列表（含 `sync_tier`、`member_caps`、`status`、`signal_path`）；`member_caps[].syncing` 为真表示该成员入组/唤醒后仍在收敛。`signal_path` 为每区域只读诊断（`lossless\|enhanced\|lossy` + 五段链路，F7-08 后端已实现） |
| POST | `/groups` | `{name, member_ids, master_id?}` | 201；成员已在别的组 409，同步等级不匹配 400 |
| GET | `/groups/{id}` | — | 单组详情 |
| PUT | `/groups/{id}` | `{name, icon?}` | 区域重命名与图标；`name` 去首尾空白后 1~40 字符，`icon` 限白名单（空串=默认）；持久化，重启后保留 |
| DELETE | `/groups/{id}` | — | 解散组，成员各自回到单机组；组在派对中 409（先退出派对） |
| POST | `/groups/{id}/master` | `{player_id}` | 换主控；非成员 400 |
| POST | `/groups/{id}/members` | `{player_id}` | 播放中加入成员，从当前时间点接入；已在别的组 409 |
| PATCH | `/groups/{id}/members/{player_id}` | `{delay_ms?, volume_offset?}` | 每成员延迟补偿与音量偏移 |
| DELETE | `/groups/{id}/members/{player_id}` | `?mode=continue\|stop` | 移出成员 |
| PUT | `/groups/{id}/members/order` | `{player_ids[]}` | 调整成员顺序；必须是该组全排列，否则 400。只改 `position`，**不改 `master_id`** |
| GET | `/groups/{id}/volume` | — | 组音量 + 每成员实际值（含失败明细） |
| POST | `/groups/{id}/volume` | `{volume, muted}` | 逐设备下发，单台失败不回滚其余 |
| POST | `/groups/{id}/transfer` | `{from_group_id, only_player_id?}` | 路径里的 `{id}` 是**目标组**；`only_player_id` 非空时后端先把它拆成单机组，目标组被忽略 |
| GET | `/groups/{id}/queue` | — | 队列快照 |
| POST | `/groups/{id}/queue` | `{song_ids[]}` | 追加歌曲 |
| PUT | `/groups/{id}/queue/reorder` | `{from, to}` 或 `{remove_index}` | move 语义；删除与重排互斥 |
| POST | `/groups/{id}/transport` | `{action, position?, repeat?, shuffle?}` | `play\|pause\|stop\|next\|prev\|seek`（seek 用秒）；`repeat=off\|all\|one` 与 `shuffle=bool` 也走这里；解码槽位满 503 |
| POST | `/groups/{id}/sleep` | `{minutes}` | 设置一次性睡眠定时器；`0` 取消，`1~720` 分钟，到点暂停该组并广播 `state`；只存在内存中，重启失效 |
| GET | `/groups/{id}/sleep` | — | `{remaining_seconds, active}`；`remaining_seconds` 向上取整，取消或到点后 `active=false` |

**全局与指标**

| 方法 | 路径 | 请求体 / 参数 | 说明 |
|---|---|---|---|
| GET | `/playback` | — | 首屏全量：所有组 + 状态 + 能力 + `party_id` |
| GET | `/playback/metrics` | — | 运行指标快照：解码槽位 `decode_slots_used/max`、`player_underruns_total`（欠载补静音）、`player_sink_dropped_frames_total`（下游丢帧）、`player_resyncs_total`（偏差重挂）、`player_events_dropped_total`、`player_drift_ms`，以及各 owner（组/派对）明细；判读见 §4.6 |

**闹钟**

| 方法 | 路径 | 请求体 / 参数 | 说明 |
|---|---|---|---|
| GET | `/alarms` | — | 按触发时刻排序的闹钟列表；`last_fired_at` 是最近触发到的分钟 |
| POST | `/alarms` | `{name, target_id, time_of_day, days_mask, playlist_id, volume, fade_seconds, enabled}` | 201；`time_of_day` 为 `HH:MM`，`days_mask` bit0=周一…bit6=周日（127=每天，0=只今天一次） |
| PUT | `/alarms/{id}` | 同上 | 覆盖式更新，`id` 与 `last_fired_at` 不变 |
| DELETE | `/alarms/{id}` | — | 204；不影响正在播放的内容 |
| POST | `/alarms/{id}/test` | — | 立即按配置起播（不写 `last_fired_at`），调试用 |

**派对**

| 方法 | 路径 | 请求体 / 参数 | 说明 |
|---|---|---|---|
| POST | `/playback/party` | `{name?, group_ids[]}` | 201；`party_enabled=false` 403；组已在别的派对 409 |
| GET | `/playback/party/{id}` | — | 成员组、主控组状态、各组相对偏移 |
| POST | `/playback/party/{id}/groups` | `{action, group_id?, mode?}` | `join` / `leave`（`mode=continue\|stop`）/ `end`（结束整个派对） |
| POST | `/playback/party/{id}/transport` | `{action}` | 只支持 `play\|pause\|stop`，广播到派对内全部组 |

**事件**

| 方法 | 路径 | 请求体 / 参数 | 说明 |
|---|---|---|---|
| POST | `/playback/events/ticket` | — | 换一次性 ticket（30s、绑定用户、用后即焚） |
| GET | `/playback/events` | `?ticket=…` | WS 升级 101；不走 JWT（浏览器无法带 `Authorization` 头） |

所有端点必须带 swag 注释与 `@Security BearerAuth`；`@Router` 不含 `/api/v1` 前缀。

**不再有 `/playback/targets`**：v2 用 `GET /playback` 一次拿全（组、状态、能力、`party_id`），输出选择器无需单独端点。派对也没有"当前派对"的 GET：派对 id 记在各组的 `party_id` 上，客户端先读 `/playback` 再取 `/playback/party/{id}`。

**转移的补偿语义**：`transfer` 在目标组以 `position + transfer_compensation_ms` 起播（默认 800ms），补偿源组停止到目标出声的空档。补偿只在服务端做一次，客户端不得重复补偿（见前端文档 §4.7）。

**待机语义**：`POST /players/{id}/power` 的 `standby=true` 只表示服务端不再向该设备推流（DLNA 发 Stop 并清 URI，RAOP 停发 RTP），不代表硬件断电；RTSP 与 DLNA 都没有通用的上电指令。待机成员不在同步环里，唤醒后从当前进度重新接入。

### 7.2 配置端点

遵循"用户可见开关走业务端点"：

- `/settings/player-discovery` — `{enabled, protocols:["raop","dlna"], interval_seconds}`；`interval_seconds` 范围 10–600，默认 30
- `/settings/player-output` — `{max_groups, default_latency_ms, dlna_realign_interval_minutes, allow_private_network, network_sinks_enabled, party_enabled, party_drift_tolerance_ms}`
- `/settings/crossfade` — `{enabled, seconds}`；默认关闭、3 秒，`seconds` 范围 0.5–12。开启时交界处用等功率（cos/sin）混合，**放弃 gapless 接续**（产品决策，见对照矩阵 §4）
- `/settings/replaygain` — `{mode: "off"|"track"|"album", prevent_clipping: bool}`；默认 `off` + 防削波开启。标签在首次播放该曲时由 ffprobe 读取并缓存，不进扫描入库（存量曲库升级后立即可用）
- `/settings/player-output-allowlist` — `{entries:["192.168.1.0/24", …]}`；空数组表示不限制私网，配置后只允许这些网段，公网地址恒拒绝

`party_drift_tolerance_ms` 默认 1500，下限 500、上限 5000；`party_enabled=false` 时发起派对返回 403（企业/公共部署可能需要关掉"让所有房间一起响"）。`network_sinks_enabled=false` 时手动添加与重连网络音箱返回 403：iOS Bundle 恒为 false，桌面 Bundle 需用户显式开启。

### 7.3 WebSocket 鉴权

浏览器无法给 WS 带 `Authorization` 头，且 token 进 URL 会落访问日志。方案：`POST /playback/events/ticket` 换取一次性 ticket（30s 有效、绑定用户），再 `GET /playback/events?ticket=…` 升级。

### 7.4 事件契约

统一信封：`{seq, type, time_ms, …}`。`seq` 单调递增且所有类型共用，客户端只需比较 `seq` 丢弃旧消息，不必按类型各维护一份。其余字段按类型出现（Go 侧 `omitempty`），下表列出每种事件实际携带的字段。

| `type` | 携带字段 | 触发时机 | 客户端动作 |
|---|---|---|---|
| `state` | `group_id, song_id, position_ms, status` | 播放状态、曲目、位置变化 | 唯一播放状态来源，据 `group_id` 判断是否与自己相关 |
| `target_changed` | `group_id, from_group_id, reason` | 播放被转移到别的组（`reason=transfer`） | 按 `players.takeover_behavior` 处理：`pause` 立即暂停并提示，`follow` 静默转为纯控制器 |
| `member_changed` | `group_id?, player_id, action` | `action=join\|update\|leave\|offline\|online\|deleted\|standby\|wake\|reorder`；`join`/`update`/`leave`/`reorder` 带 `group_id`（`reorder` 不带 `player_id`），`offline`/`online` 每个所属组各发一条 | 待机/唤醒只走这里，没有独立的 `power` / `standby` / `wake` 事件 |
| `group_changed` | `group_id, player_id?, action` | `action=deleted`（解散）、`master_changed`（换主控，带 `player_id`）、`renamed`（改名/换图标，带 `name`） | **建组不发事件**：新建后由调用方自行拉 `/playback` 或等首条 `state` |
| `speaker_seen` | `player_id, name, kind` | 发现流程扫到新设备 | 提示"发现新音箱"，不自动加入任何组 |
| `party_changed` | `party_id, group_id?, name?, action` | `action=created`（带 `name`）、`joined` / `left`（带 `group_id`）、`ended` | 重拉 `/playback/party/{id}`；`ended` 后清空派对态 |
| `group_drift` | `party_id, group_id, offset_ms, corrected, unaligned` | 派对内校正环每次测量；`corrected=false` 表示本次校正失败，`unaligned=true` 表示已放弃重试 | 累计 3 次 `corrected=false` 或收到 `unaligned=true` 时把该组标成"未对齐"，不要反复弹 toast |
| `eq_changed` | `player_id, enabled, preset` | 任一客户端改了 EQ | 刷新该设备的 EQ 面板 |
| `error` | `group_id?, code, message` | 引擎内部错误（解码、下游写入等） | 展示错误，必要时 `stop` 后重试 |

- `state` 不区分"谁触发的"；多端同时操作时以最后到达的 `seq` 为准。
- 运行指标（解码槽位、欠载、丢帧、重同步）**不进事件流**：`GET /playback/metrics` 是快照，需要时按需拉取（前端面板打开时 2s 一次），避免高频事件挤占慢客户端。
- 慢客户端丢弃的是自己那份副本，丢弃数计入 `player_events_dropped_total`；该计数持续增长说明有客户端卡住，而不是服务端丢数据。
- 断线重连后客户端必须重新拉 `/playback` 全量对齐，不能只依赖增量事件。

## 8. 与现有系统的约束

### 8.1 安全模型冲突（必须单独处理）

DLNA/RAOP 要求服务端**主动连接局域网设备**，与 `internal/services/whitelist.go` 的"私网一律封禁"直接冲突。

- 新增独立配置 `player_output_allowlist`，**不复用**资源代理白名单。
- 依据 `docs/pitfalls.md#private-allowlist`：现有白名单仅影响资源与转码代理，不放宽 HLS；播放器输出是第三条独立路径。
- 设备地址必须在 `allowlist` 覆盖范围内才允许连接，否则返回 403 并记录审计日志。

### 8.2 组播与容器网络

SSDP（239.255.255.250:1900）与 mDNS（224.0.0.251:5353）在 Docker bridge 网络下收不到。

- 文档必须写明：`--network host` 或 macvlan。
- UI 在发现为空时给出该提示，而不是静默空白。
- 提供"手动填 IP + 端口"添加设备，作为组播不可用时的兜底。

### 8.3 Bundle 模式

- iOS 受限（mDNS 权限、后台进程、无法长期持有 socket）→ Bundle 模式**默认关闭**音箱推送，只保留 `local` sink。
- 桌面 Bundle（macOS/Windows/Linux）可开启，但需显式配置。

### 8.4 ffmpeg 预算

现有 4 个转码槽位（`docs/pitfalls.md#url-transcode`）不能被播放器饿死。

- 播放器独立计数 `player_max_groups`（默认 4），计数单位是**并发解码组数**：
  - 每个正在播放的同步组占 1 个；
  - 派对里**每个组各占 1 个**（不共享解码器，见 §4.7），所以 4 组派对就把预算吃满；
  - 某组任一成员 EQ 非 flat 时会强制解码（§4.8），同样占槽位。
- 两套预算各自限流，超限返回 503，错误信息区分"转码繁忙"与"播放器繁忙"；播放器侧再细分"同时播放数已达上限"与"均衡器需要解码，槽位已满"。
- 估算：3 组同步组 + 1 个 2 组派对 = 5 > 4，第 5 个直接拒绝并告诉用户释放哪个。

### 8.5 插件权限（第一版不做）

若要允许 JS 插件控制播放器（新增 `player` 桥接 namespace），必须同时修改三处白名单：

1. 宿主 `internal/jsplugin/permissions.go` 的 `AllPermissions`
2. `plugins/toolchain` 的 `VALID_PERMISSIONS`
3. 脚手架的 `AVAILABLE_PERMISSIONS`

第一版只开放 REST/WS，避免跨三仓联动。

## 9. 分阶段计划与验收

| 阶段 | 内容 | 估算 | 验收标准 |
|---|---|---|---|
| **P0** | 队列 + 状态机 + `local` sink + WS 事件 | 3~4 周 | 状态机/队列单测覆盖；双端联调可"服务端控制客户端播放" |
| **P1** | RAOP 打通（L16）单设备 | 2 周 | Docker `shairport-sync` 出声，抓 PCM 校验时长与波形 |
| **P1.5** | RAOP 真机兼容矩阵 | 1~2 周 | ≥4 类设备实测，产出矩阵表入文档 |
| **P2** | DLNA 发现 + SOAP + 单播 | 3 周 | 假 renderer（httptest）校验 SOAP/DIDL；真机 ≥3 款 |
| **P3** | 同步组（RAOP 精确 + 跨协议降级 + **校正环**） | 3~4 周 | 两实例互相关延迟 < 20ms；人为拖慢 2s 能在 5s 内回位；播放中加入成员 ≤70ms 出声 |
| **P2.5** | 每音箱 EQ（Go 侧 RBJ biquad + preamp + 热切换） | 1 周 | 白噪经 +12dB@1kHz 后 1kHz 增益误差 < 0.2dB；全链路无 >0dBFS 样本；切 EQ 无爆音（波形无阶跃） |
| **P3.5** | 派对模式（多组同播 + 漂移检测与校正） | 2~2.5 周 | 3 组派对粗对齐 < 1.5s；拖慢一组 5s 内回位；单组退出不影响其余；解码器计数不超限 |
| **P4** | AirPlay 2 | — | **不做**，除非出现可用开源发送端 |

**MVP（P0+P1+P2，单播不同步）≈ 2.5 个月；含同步（含校正环）+ EQ ≈ 3.6 个月；含派对与工程收尾 ≈ 4.4 个月（19 周，~103 人日）。**

> **P0 现状（2026-10-26）**：服务端侧（队列/状态机/`local` sink/WS 事件）已完成并有测试；**「双端联调」未做**——客户端没有 WS 客户端也没有 `register_player`，`local` sink 只有单测里的假客户端。缺口清单与补做任务见[开发计划 §0.2 / §5.1](./playback_development_plan.md#02-v3-修订设计-vs-实现缺口2026-10-26-审计)。

### 9.1 自动化验证方法

- **单元**：时间轴换算（注入假时钟）、SDP 生成、AES/RSA 已知向量、RTP 包 golden bytes、队列状态机。
- **RAOP 集成**：CI 起 `shairport-sync --output pipe` 写入文件，用 `ffprobe` 校验时长/采样率，用波形相似度校验内容。
- **同步精度**：两个 shairport-sync 实例各 pipe 到独立文件，互相关求峰值延迟，断言 `< 20ms`。这是可自动化的同步精度测试。
- **校正收敛**：假时钟 + 假 sink 注入 ±50ms / ±2s 偏差，断言 **1 个 950ms 周期内收敛到 ≤10ms**，10 分钟仿真残余漂移 ≤5ms（B5-08）。
- **逐位隔离**：组内只给 A 开 EQ，断言 B 收到的样本与原信号 max diff = 0（B6-03）。
- **DLNA**：`httptest` 起假 renderer，断言 SOAP body 与 `SOAPAction` 头；真机矩阵人工验收。
- **EQ**：白噪 / 扫频输入，FFT 后断言各频点增益与设定值误差 < 0.2dB；`+12dB` 全开时断言峰值 ≤ 0dBFS（削波回归）；热切换时断言样本差无 >0.1 的阶跃。
- **派对**：假 sink 记录每组首个样本的"逻辑位置"，断言 3 组互差 < 1.5s；注入一组人为拖慢 2s，断言 5s 内发出 `group_drift{corrected:true}` 并 seek 回位。

## 10. 风险

| 风险 | 概率 | 影响 | 缓解 |
|---|---|---|---|
| L16 在真实 AirPlay 硬件上不被接受 | 中高 | P1 返工 | PoC 阶段即用真机验证；备选 AAC-ADTS |
| DLNA 厂商 SOAP 方言不一致 | 高 | 兼容性差评 | `SUBSCRIBE` 事件代替轮询；按 model 打补丁；手动添加兜底 |
| 同步组混协议导致体验崩塌 | 高 | 核心卖点失效 | `SyncTier` 显式降级 + UI 明示 + 默认禁止跨族组队 |
| 组播被用户网络阻断 | 中 | 发现不到设备 | 手动填 IP；文档写明 Docker 网络要求 |
| 单解码源欠载导致爆音 | 中 | 听感问题 | 3s 环形缓冲 + 欠载补静音 + 指标告警 |
| 时间轴被系统时钟跳变撕裂 | 低 | 整组失步 | 只用 monotonic 时钟，墙钟仅用于换算 NTP |
| 派对漂移被用户听成"回声" | 中 | 体验崩塌 | 1.5s 阈值只做粗校正；两个目标在同一房间时提示改用同步组；UI 明示派对不保证精确同步 |
| EQ 削波 / 切 EQ 爆音 | 中 | 听感、可能伤高音单元 | 自动 preamp + 20ms 斜坡 + 削波样本断言（§9.1） |
| EQ 强制解码把槽位吃满 | 中 | 功能不可用 | 错误信息区分原因；UI 在打开 EQ 前就提示当前槽位 |
| 校正环引入可听跳帧 | 中 | 听感变差 | 只在 >10ms 时动、单周期单成员一次、>200ms 改用静音补齐；`player_drift_ms` 留痕 |

## 11. 与 Roon 的能力边界

交互模型已按 Roon 对齐（Zone / 焦点 / 分组 / 转移 / 信号路径，见[交互设计](./architecture_playback_ui.md)），但**“完全实现 Roon 的效果”在音频层不成立**。下表把差距分成两类：改设计能补的，和换协议才能补的。

### 11.1 硬差距（改设计也补不了）

| # | 能力 | Roon | 本项目 | 差距性质 |
|---|---|---|---|---|
| H1 | **同步精度** | RAAT 端到端 ~1ms（共享时钟 + 设备侧缓冲反馈） | RAOP 目标 ±20ms；DLNA 只能起播对齐（200~500ms 且持续漂移） | **协议天花板**：RAOP 无共享时钟，只能靠校正环事后拉回；同房间双音箱会有梳状滤波 |
| H2 | **本地输出音质** | macOS hog mode / WASAPI exclusive，bit-perfect，DSD native | 客户端 `just_audio` → OS 混音器，被系统重采样，DSD 必须转 PCM | **客户端音频栈**：要 bit-perfect 得接 CoreAudio/AAudio 独占接口，不是改服务端能解决的 |
| H3 | **DSP 深度** | 参数 EQ、卷积（房间校正）、升频、crossfeed、headroom | 10 段图示 EQ + 响度归一 | **实现量**：卷积与升频是另一个量级的工程 |
| H4 | **设备覆盖** | Roon Ready / RAAT 设备（数百款） | AirPlay 1 + DLNA + 本机 | **生态**：RAAT 是私有协议，接不进来 |
| H5 | **并发区域数** | 按 CPU 扩展，几十个 zone 同时放不同内容 | 默认 4 个并发解码组，第 5 个 503（`budget.go`） | **资源模型**：可调高，但受 CPU 与单机解码能力约束 |

### 11.2 可补但未补（设计层）

| # | 能力 | Roon 做法 | 现状 | 建议 |
|---|---|---|---|---|
| S1 | **音量模式** | 每设备三选一：Device volume / Fixed volume / DSP volume | 只有 `volume` + `volume_offset` | 加 `volume_mode` 列；Fixed 用于接前级的用户（避免双音量） |
| S2 | **音量上限** | 每区域可设最大音量，防误触炸音箱 | 无 | 加 `max_volume`，服务端钳位并回填 |
| S3 | **睡眠定时器** | 有（“30 分钟后停止”） | 已实现 `/groups/{id}/sleep`（内存态，到点暂停该组） | 可再持久化到 alarms 表，跨重启保留 |
| S4 | **队列的“下一首播放”** | Play Now / Play Next / Add to Queue 三动作 | `Queue.InsertNext` 已实现，**UI 未暴露** | 纯前端接线 |
| S5 | **焦点跟随播放** | 可选：开始播放时自动切焦点 | 已列为待定 | 做成设置项，默认关 |
| S6 | **转移空档** | RAAT 预连接，近乎瞬时 | 0.5~3s（RAOP 握手 + 缓冲） | 协议决定，只能靠 UI 诚实过渡态缓解。**已实现**：目标行「正在切换…」、源行「正在移交」、收到目标区域 `state` 事件即恢复、5s 未收到提示「切换超时，请检查设备」（`zone_transfer_provider.dart`） |

### 11.3 结论

- **目标是“Roon 式的多房间控制体验”** → 设计已足够，按 F7 做完即可，不需要新协议。
- **目标是“Roon 的音质与同步”** → 需要自研 RAAT-like 协议（共享时钟 + 设备侧缓冲反馈）与客户端独占音频栈，属于另一个项目量级，不在本设计范围内。
- **不建议**把 H1~H5 写成“待优化”：它们是边界，不是待办。写清楚比假装能追上更有用。

## 12. 区域队列所有权（Roon 模型）

Roon 的模型是**区域拥有队列**：每个 Zone 有一条独立的 play queue，切区域就是切整个播放器（队列 + 当前曲 + 进度）。本项目按同一语义落地，权威划分如下。

### 12.1 权威划分

| 角色 | 谁负责 | 说明 |
|---|---|---|
| 队列内容 | **客户端**（编辑器） | 选歌、拖拽、删除都在客户端完成，整条推给服务端 |
| 队列存储 | **服务端**（保存者） | `PUT /groups/{id}/queue` 全量覆盖，`SetQueue` 只存不播 |
| 出声 | 区域成员 | 本机参与 → 本机 `just_audio`；本机不参与 → 服务端推给该区域设备 |
| 状态展示 | 服务端 + 上报镜像 | 本机出声时靠 `report` 镜像；远端区域靠 `state` 事件镜像 |

### 12.2 客户端同步（`zone_queue_sync.dart`）

- 焦点区域变化（或区域列表刚加载出来）→ `GET /groups/{id}/queue` + `GET /songs/batch?ids=` → `adoptZoneQueue` 接管为本地队列。
- 本地队列变化 → 250ms 防抖 → `PUT /groups/{id}/queue` 整条推回。
- 防回环：`_pushedSongs/_pushedIndex` 记录最近对齐值，`_adopting` 标记接管窗口；接管期间不推送，否则会把上一个区域的队列推给新区域。
- 只有本机参与的区域才出声：`autoPlay = zone.isLocal && status.isPlaying`；切到不出声的区域会 `stopForServer()` 停掉本机音频。
- 焦点区域不含本机时，`remoteZoneIdProvider` 置为该区域 id，播放器的 play/pause/next/prev/seek 改为 `POST /groups/{id}/transport` 委派给服务端，并用 `state` 事件镜像播放状态（`applyRemoteZoneStatus`）。

### 12.3 服务端约束

- `Engine.SetQueue` 不启动解码；正在解码且当前曲目被换掉（含清空）时停掉解码并落到 `paused`，避免继续播一首已经不在队列里的歌。
- 未在解码时只 `Transport.SelectSong` 换当前曲目，状态机不动（推队列 ≠ 开始播放）。
- 空数组表示清空队列（`Queue.Replace` 已支持 `current = -1`）。
- 区域面板不再提供播放按钮：选区域只切焦点与队列，播放交给主播放器（Roon 语义）。

## 13. 附录：参考实现索引

| 主题 | 参考位置 |
|---|---|
| 单源多路分发 | `slimserver/Slim/Player/Source.pm`（`shift->master()`） |
| 同步组与延迟补偿 | `slimserver/Slim/Player/Sync.pm`（156 行，核心逻辑极简） |
| 能力协商 / 采样率取最小 | `slimserver/Slim/Player/CapabilitiesHelper.pm` |
| 播放状态机 | `slimserver/Slim/Player/StreamingController.pm` |
| 队列语义 | `slimserver/Slim/Player/Playlist.pm` |
| 断点续播 | `slimserver/Slim/Networking/Slimproto.pm:305 persistPlaybackStateForPowerOff` |
| RAOP 接收端（反向对照） | shairport-sync |
| RAOP 发送端 | OwnTone `src/outputs/raop.c` |
