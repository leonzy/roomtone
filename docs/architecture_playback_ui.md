# 播放与音箱交互设计（前端）

> **状态：部分实现。** 后端契约见 [播放控制与音箱输出架构](./architecture_playback.md)；本文只描述客户端（`clients/player`，独立仓库）的交互、状态与代码落点。实施顺序与任务拆分见[开发计划](./playback_development_plan.md)；**真机与 TV/Lynx 走查 `Not run`**。
>
> **实现现状（2026-10-26 审计）**：只有设置页形态的 `/settings/output` 面板（F0 面板、F1 建组、F2 转移、F3 EQ、F4 派对、F5 指标、F6 待机）落地；**播放条入口、区域选择器、WS 客户端、本机注册与双模接管均未开工**，缺口清单见[开发计划 §0.2](./playback_development_plan.md#02-v3-修订设计-vs-实现缺口2026-10-26-审计)。
>
> **v3 修订（Roon 模型）**：区域（Zone）是唯一用户概念，**多区域可并行播放**，选择器切换的是“我在控制谁”而不是“把播放搬过去”；转移是显式动作。见 §2、§4。
>
> 适用客户端：Flutter 六端。TV 与 Lynx 的差异见 §8。
>
> **可交互设计稿**：[playback-ui-mockup.html](/roomtone/design/playback-ui-mockup.html)（可直接点选输出、拖动音量、切换分组成员）；五屏静态预览：[playback-ui-board.png](/roomtone/design/playback-ui-board.png)。

## 1. 设计原则

| # | 原则 | 含义 |
|---|---|---|
| P1 | **只有一个概念：区域（Zone）** | 单机音箱和分组在 UI 上平级，都是“区域”。单机 = 只含自己的组。用户不需要理解“组”这个实现细节。 |
| P2 | **状态以服务端为准** | 队列、进度、音量、成员关系全部来自 `/playback` + WS 事件；客户端只做乐观更新，收到事件即校正。 |
| P3 | **切换必须诚实** | 转移、加入成员、换主控都有 0.5~3 秒空档。UI 用过渡态明说"正在连接"，**不假装瞬时**。 |
| P4 | **播放中可改，不用跳设置页** | 分组是高频动作，必须能在播放页当场增删成员；设置页只做低频的"命名/删除/发现"。 |
| P5 | **本机也是区域之一** | 手机/桌面本身可作为输出，与音箱同区域；不是"另一个功能"。 |
| P6 | **派对不是更大的组** | 派对共享内容（哪首歌、第几秒），组共享时钟（±20ms）。两者是不同的东西，UI 不能把它们揉成一个"多选设备"。 |
| P7 | **每台音箱有自己的耳朵** | EQ 属于设备，不属于内容。客厅的摆位问题和厨房无关，不提供隐式联动。 |
| P8 | **区域并行，不是单点搬运** | 每个区域有自己的队列与播放状态，可同时出声。选择器切换的是**控制焦点**，不是把播放搬过去；转移是显式动作（Roon 的 Zone 模型）。 |
| P9 | **本机永远在列表里** | 本机（This Device）是内置区域，不依赖发现、不依赖网络。没有任何音箱时，它就是唯一目标。 |
| P10 | **选择器即遥控器** | 选择器里能直接调任意区域的音量、暂停/继续，不必先切过去。切焦点是“看它”，不是“控制它”的前置条件。 |
| P11 | **信号路径可见** | 用户有权知道声音被处理过：直通/经 DSP/有损转码用颜色区分，点开看完整链路。 |

## 2. 概念与状态模型

**术语对齐（v3）**：用户可见的概念只有 **区域（Zone）**；后端叫 `group`，单成员组就是单机区域。UI 文案不再出现“组/输出目标”，只在技术文档与 API 里保留。

```dart
/// 区域：一个 group（可能只含 1 个成员），是播放的原子单位。
/// 每个区域有自己的队列、传输状态、音量，可与其他区域同时出声。
class Zone {
  final String id;                // = group id
  final String name;              // 用户可改；默认 "客厅" / "本机"
  final String? icon;             // 图标 key，白名单见 §4.5
  final List<Speaker> outputs;    // 成员（原 members）
  final bool isLocal;             // 含本机
  final SyncTier syncTier;        // exact | aligned
  final bool online;
  final String? partyId;          // 非空 = 该区域正在派对中
  final ZonePlayback playback;    // 每区域独立，不再全局单例
  final int volume;               // 区域基准音量（0-100）
  final bool muted;
  final SignalPath signalPath;    // 直通 / 经 DSP / 有损（§4.6）
}

/// 区域自己的播放状态。多区域并行时，每个区域各有一份。
class ZonePlayback {
  final int? songId;
  final double position;
  final PlaybackStatus status;    // idle|loading|buffering|playing|paused|error
  final int seq;                  // 服务端单调序号
  final String? errorCode;
}

/// 控制焦点：当前 App 在“看”哪个区域。本地持久化，不改变任何区域的播放。
class PlaybackFocus {
  final String zoneId;
}

class Speaker {
  final String id;
  final String name;
  final SpeakerKind kind;         // local | raop | dlna
  final bool online;
  final int volume;               // 0-100
  final int volumeOffset;         // 组内相对音量
  final int delayMs;              // 延迟补偿
  final String model;
  final EqProfile eq;             // 每音箱 EQ
}

class EqProfile {
  final bool enabled;
  final String preset;            // flat/rock/pop/jazz/classical/bass_boost/treble_boost/vocal/custom
  final List<double> bands;       // 10 段，-12…+12 dB
  final double preamp;            // 服务端回填，只读；永远是 ≤ 0 的自动衰减
}

class Party {
  final String id;
  final List<String> groupIds;    // 参与的组
  final String leaderGroupId;     // 推进逻辑进度的组
  final int? songId;
  final double position;          // 逻辑进度
  final Map<String, int> driftMs; // 每组偏差，用于"某组未对齐"提示
}

class SignalPath {
  final SignalPathKind kind;      // lossless | enhanced | lossy
  final List<SignalStage> stages; // 源→解码→DSP→输出→设备
}
```

- **多区域并行**：`GET /playback` 返回全部区域及其 `status`；后端每区域一个引擎（`manager.go` 的 `engines["group:"+id]`），可同时出声。UI 不得假设“只有一个区域在放”。
- **焦点 ≠ 所有权**：切焦点只改 `PlaybackFocus`，不发任何播放命令；只有行内 ▶/⏸、音量、显式转移才发命令。
- `isLocal` 决定客户端是否接管本地播放器（§7.3）。
- `syncTier == aligned` 时选择器显示琥珀色"近似同步"徽标，且**不允许**把它加入已有精确同步组（后端也会拒绝，前端先拦）。
- 区域列表按"在线优先 → 同网络 → 名称"排序，离线区域置灰但不隐藏（用户需要知道"我的书房音箱还在列表里"）。
- `partyId` 非空的区域行显示 `派对` 徽标与参与区域名，加入/退出走行尾菜单（§4.10）。
- **本机区域**：客户端启动即注册，`id` 稳定（设备 id 持久化），不因重启变成新区域。

## 3. 信息架构

### 3.1 路由新增

| 路由 | 页面 | 说明 |
|---|---|---|
| `/settings/speakers` | 音箱管理 | 设置主从布局中的新分类 |
| `/output` | 全屏输出页 | 仅桌面/平板/TV 使用；手机走 BottomSheet |

### 3.2 三个入口

```
播放条左侧（mini / full / desktop bar / mobile）
  └─ 区域名 + 图标 + 状态点 ──► ZonePicker（Roon 式区域选择器，90% 的场景在这里完成）

设置
  └─ 播放与音箱 ──► 音频设备（发现、启用/禁用、命名、设备设置）+ 区域管理

桌面/TV 大屏
  └─ 播放栏右侧「信号路径」──► 全屏区域页（可拖拽成员、显示每台音量）
```

**入口位置是 Roon 的硬约束**：区域名必须在**常驻播放条**上，而不是藏在播放页的某个按钮里。用户看得到“现在在控制哪个区域”，才可能理解“另一个区域还在放”。

### 3.3 为什么选择器是 Sheet 而不是独立页

切区域、调音量、暂停另一个房间都是"播放中随手做"的动作，跳页会打断音乐上下文。Sheet 保持播放条可见，一次点击完成。设置页只承担低频管理（发现设备、改设备参数、删区域）。

## 4. 播放页：输出选择器

### 4.1 线框

Roon 的选择器是**区域控制台**，不是“选一个目标然后跳转”。每个区域行自带传输与音量，当前焦点用左侧实心点标记。

```
┌──────────────────────────────────────┐
│  区域                          [分组] │
├──────────────────────────────────────┤
│ ● 客厅                      ▶  ───○─ │  ← 当前焦点（实心点）
│   正在播放 · 晴天 — 周杰伦            │
│   🔊 客厅 Soundbar                    │  ← 展开的成员（仅焦点区域）
│   🔊 餐厅 HomePod                     │
├──────────────────────────────────────┤
│ ○ 卧室音箱                  ▶  ───○─ │
│   已暂停 · 夜曲 — 周杰伦              │
├──────────────────────────────────────┤
│ ○ 本机（MacBook）            ▶  ───○─ │
│   空闲                                │
├──────────────────────────────────────┤
│  🎉 派对中：厨房、书房           管理 │  ← 仅派对进行中显示
├──────────────────────────────────────┤
│  ＋ 新建区域      ⚙ 音频设备设置      │
└──────────────────────────────────────┘
```

**与旧版的关键差异**：旧版是“点另一个目标 = 转移播放”，新版是“点另一个区域 = 切控制焦点”。后者才是 Roon 的模型，也是后端已经支持的模型（每区域独立引擎与队列）。

### 4.2 点击语义（最容易做错的地方）

| 点击位置 | 行为 | 发什么请求 |
|---|---|---|
| 区域行主体 | **切控制焦点**，不改变任何区域的播放 | 无（只写本地 `PlaybackFocus`） |
| 行内 ▶ / ⏸ | 控制**该区域**传输 | `POST /groups/{id}/transport` |
| 行内音量滑杆 | 调**该区域**音量 | `POST /groups/{id}/volume`（300ms 防抖） |
| 行尾 ⋯ → 转移到此区域 | 显式转移（§4.7） | `POST /groups/{to}/transfer` |
| 行尾 ⋯ → 与当前区域分组 | 合并（§4.4） | `POST /groups/{id}/members` |
| 行尾 ⋯ → 重命名/图标 | 区域身份（§4.5） | `PUT /groups/{id}` |
| 长按拖到另一区域上 | 快捷分组（桌面/TV） | 同“分组” |
| 点成员行 | 展开音量/延迟/EQ/拆分（§4.8） | — |

- **切焦点不能暂停任何东西**。用户从客厅切到卧室看队列，客厅必须继续放。
- 行内 ▶ 在焦点区域上等价于播放条的主按钮，两者状态必须同源（同一个 `ZonePlayback`）。
- 多区域同时播放时，播放条只显示**焦点区域**的曲目与进度；其他区域靠选择器里的状态行体现。

### 4.3 状态与徽标

| 情况 | 表现 |
|---|---|
| 精确同步区域 | 绿色 `同步` 徽标 |
| 含 DLNA 成员 | 琥珀 `近似同步` 徽标 + 副标题"DLNA 设备无法精确对齐" |
| 区域离线 | 整行置灰 + `离线` + 点击弹"重新连接" |
| 无任何音箱 | 列表仍含“本机”，底部一行“扫描音箱 / 手动添加 IP”（含 Docker 网络提示） |
| 本机未注册成功 | 本机行显示“输出不可用”+ 重试，**不隐藏** |
| Bundle 模式 | 只显示"本机"，底部一行说明"本地模式不支持音箱推送" |

### 4.4 分组（Group Zones）

Roon 的分组是**区域之间的动作**，不是“往当前区域里加设备”。两种入口等价：

1. 顶部「分组」→ 多选态 → 勾选 ≥2 个区域 → 「合并」。
2. 把一个区域行拖到另一个区域行上（桌面/TV）。

规则：

- 合并后保留**被拖入方（目标区域）的队列与播放位置**，其余区域停止但**队列保留**（可拆分后继续）。
- 合并时若源区域正在播放、目标区域空闲 → 以源区域内容为准（用户意图是“让这台也出声”）。
- 精确组不接受 `aligned` 成员：不兼容项置灰并给原因，前端先拦，后端也会 400。
- 拆分：成员行 ⋯ → 「移出」→ 独立成区域，**从当前时间点接入**（§4.8）。

### 4.5 区域命名与图标

- 双击区域名或 ⋯ → 「重命名」；1~40 字符，空串拒绝。
- 图标从内置白名单选（`speaker / bookshelf / soundbar / headphone / tv / phone / laptop / multiroom`），不开放上传。
- 名称与图标存服务端（`groups.name` / `groups.icon`），换设备不丢；`PUT /groups/{id}` 已实现（`name` 1~40 字符，`icon` 限白名单，空串=默认）。
- 默认名：单成员区域用设备名；多成员区域用“客厅 + 1”这类自动名，首次合并时提示可改。

### 4.6 信号路径

播放条右侧一个圆点 + 点开面板：

| 颜色 | 含义 | 判定 |
|---|---|---|
| 绿 | 无损直通 | 无 EQ、无响度、无重采样、无转码 |
| 紫 | 经 DSP | EQ / 响度归一 / 重采样任一开启 |
| 橙 | 有损 | 转码到有损格式（AAC/MP3）或降采样 |

面板内容：`源（FLAC 44.1/16）→ 解码 → DSP（EQ: Rock, -3.2dB preamp）→ 输出（L16 44.1/16）→ 设备（客厅 Soundbar）`。

- 数据来源：`GET /playback` 每区域新增 `signal_path` 字段（后端已实现，F7-08；前端面板待接入）。
- 信号路径是**只读诊断**，不提供快捷开关；要改去 EQ 面板或设备设置。
- 多成员区域显示“成员间不一致”提示（例如一台开了 EQ），点开看逐成员链路。

### 4.7 转移播放

**语义**：把 `(队列, 当前曲目, 位置, repeat/shuffle)` 从源区域搬到目标区域，源停止。等价于后端 `POST /groups/{to}/transfer {from_group_id}`。

**入口**：区域行 ⋯ → 「转移到此区域」。**不是点击区域行**——点击只切焦点（§4.2）。

**时序（前端视角）**：

```
1. 用户选「转移到此区域」
   → 立即进入 transferring 态：Sheet 不关闭，目标行显示转圈，进度条冻结
   → 同时本地暂停（避免两台设备同时出声）
2. 服务端返回 202 accepted
   → 显示"正在连接卧室音箱…"（连接 RAOP 约 100-300ms，DLNA 约 200-1000ms）
3. 收到 WS state{group: bedroom, status: buffering}
   → 显示"正在缓冲…"
4. 收到 WS state{group: bedroom, status: playing, position}
   → 过渡结束，Sheet 关闭，播放页刷新
5. 10s 内未到 playing
   → 失败提示 + 自动回滚到原目标（服务端负责恢复，前端只提示）
```

**关键细节**：

- **位置补偿**：转移会丢 0.5~3 秒。后端在目标起播位置加上 `transfer_compensation_ms`（默认 800ms，可配），前端**不要**自己做补偿计算，否则两端叠加会跳曲。
- **过渡期间禁止二次操作**：Sheet 内其他区域行禁用，避免连点导致多个组同时被拉起。
- **失败回滚**：错误提示带"重试"和"回到客厅"，不做静默回滚（用户需要知道发生了什么）。

### 4.8 拆分播放（区域 → 单机）

组内成员行右侧菜单 → "仅在此音箱播放"：

```
POST /groups/{newGroupId}/transfer { from_group_id: <原组>, only_player_id: <该成员> }
```

结果：原区域保留其余成员并继续播放，该成员独立成区域并**从当前时间点接入**。用于"客厅在放，我去厨房单独听"。

### 4.9 源区域状态与撤销

- 转移成功后源区域进入 `idle`，**队列保留**（不销毁）：用户可随时"回到客厅"继续，或手动清空。销毁队列会让"手滑切换"变成不可逆的损失。
- 成功后 5 秒内在播放页顶部给出可撤销提示：`已转移到卧室音箱 · 撤销`。
- **撤销 = 反向 transfer，位置取当前区域此刻的位置**，不是原始位置——否则时间会倒流。
- 超过 5 秒不再提供撤销，用户从选择器重新转移即可（等价操作，不做特殊入口）。

### 4.10 派对模式

**一句话：派对是"让几个房间同时放同一首歌"，不保证它们完全同拍。** 后端契约见 [architecture_playback.md §4.7](./architecture_playback.md)，前端只需把"内容共享"和"时钟同步"两件事分开表达。

#### 入口与形态

| 状态 | 选择器里的表现 | 点击行为 |
|---|---|---|
| 未发起 | 列表底部一行 `🎉 发起派对` | 以当前焦点区域为第一个成员创建派对 |
| 派对中 | 参与区域行显示 `派对` 徽标；底部一行 `🎉 派对中：厨房、书房` | 进入派对编辑（勾选其他区域加入） |

派对**没有独立的页面**，它就是选择器里的一段。理由是它与"输出到哪里"是同一个问题的两种答案，拆页会让用户在两处之间来回跳。

#### 关键交互决定

**1. 派对中点其他区域 = 切焦点，不是加入。**

v2 曾把“点击 = 加入派对”当作快捷方式，但它与 §4.2 的“点击 = 切焦点”直接冲突：同一个手势在不同状态下含义不同，用户无法预测。v3 统一为：**点击永远只切焦点**，加入派对走显式入口。

| 手势 | 行为 |
|---|---|
| 点击其他区域 | 切焦点（不改变派对成员） |
| 区域行 ⋯ → 「加入派对」 | 加入，toast：`已加入派对 · 撤销` |
| 区域行 ⋯ → 「仅在此区域播放（退出派对）」 | 退出并独立播放 |
| 区域行 ⋯ → 「设为主控房间」 | 换主控 |
| 派对编辑里取消勾选 | 该区域退出派对，默认"分离并继续" |

**2. 没有派对总音量。**

播放页的音量控件在派对期间**保持不变**——它调的仍是"当前焦点区域"的音量。想要全屋一起调，给一个显式的"应用到全部"按钮，而不是隐式联动：各房间声学条件不同，统一音量必然有人听不清。

**3. 传输控制广播，音量不广播。**

| 控件 | 作用域 |
|---|---|
| 播放 / 暂停 / 上一首 / 下一首 / 进度条 | 全派对（`/playback/party/transport`） |
| 音量 / 静音 | 仅当前焦点区域（`/groups/{id}/volume`） |
| EQ | 仅单台音箱（§5.5） |

**4. 漂移必须可见，但不能变成噪音。**

- 正常：无任何提示（1.5s 内偏差在派对场景里听不出来）。
- 某组偏差超阈值：该组行显示灰字`正在对齐…`，不弹 toast、不中断播放。
- 连续 3 次校正失败：该组行变琥珀色 + `未对齐`，展开后给一句建议`两个目标在同一房间时建议用同步组`。
- 不做"派对同步精度"这种指标展示——它不是用户能改变的东西，说了只会增加焦虑。

#### 与现有交互的冲突处理

- **派对期间本机被切走**：不适用。派对不改变“当前焦点区域”，只改变“还有谁在放”，因此不触发 `target_changed`，也就不会暂停本机。
- **派对期间点"仅在此音箱播放"**：等同于退出派对并独占转移，需确认（会停掉其他房间）。
- **派对期间切换主控**：主控是组内概念，派对层的主控（`leaderGroupId`）由服务端自动选择，**不向用户暴露**。暴露它只会引发"为什么我选的不是主控"的争论。

#### 线框（派对编辑态）

```
┌────────────────────────────────────┐
│  派对                         完成 │
├────────────────────────────────────┤
│  ✓ 客厅（当前）                    │
│  ✓ 厨房音箱                        │
│  ✓ 书房音箱             正在对齐… │
│  ○ 卧室音箱                 离线 ⚠ │
│  ○ 本机（iPhone）                  │
├────────────────────────────────────┤
│  ⚠ 派对不保证完全同步，仅对齐进度 │
│  结束派对                          │
└────────────────────────────────────┘
```

## 5. 分组动态编辑

### 5.1 加入成员（播放中）

- 入口：顶部「分组」多选态，或区域行 ⋯ → 「添加输出」（§4.4）。
- 交互：勾选即生效（不等"确定"按钮），因为这是可逆的轻动作。
- 语义：新成员**从当前时间点接入**，不是从头。UI 明确提示"将从当前进度开始播放"。
- 若目标成员只支持更低采样率 → 整组降级，提示"为兼容 X，音质已调整为 48kHz"。

### 5.2 移出成员

| 选项 | 行为 | 默认 |
|---|---|---|
| 分离并继续 | 该成员独立成组，从当前位置继续播 | ✅ |
| 停止该成员 | 该成员停止，原组不受影响 | |

实现：`DELETE /groups/{id}/members/{player_id}?mode=continue|stop`。

### 5.3 切换主控

- 入口：组成员行 → "设为主控"。
- 必须弹确认："切换主控会中断约 1 秒，是否继续？"——因为要重建时间轴。
- 切换后组内延迟补偿值保留，但相对音量需要用户重新确认（不同设备增益不同）。

### 5.4 延迟补偿与音量

- **区域音量**：区域行内主滑杆，作用于全区域（`POST /groups/{id}/volume`）。
- **成员音量**：成员行内滑杆 = `volume_offset`（-12…+12 dB），用于"客厅音箱离沙发远，调高一点"。
- **延迟补偿**：成员行 → 高级 → 毫秒输入（-500…+500），仅对 `aligned` 组显示（精确组不需要）。
- 手势：桌面/TV 支持拖拽排序（决定"主控候选"顺序）；手机用长按菜单，避免与滚动冲突。

### 5.5 每音箱均衡器

后端在服务端 PCM 管线里做（[architecture_playback.md §4.8](./architecture_playback.md)），前端只负责编辑与展示。

#### 入口

- 选择器里组内成员行 → 菜单`均衡器`
- 设置 → 播放与音箱 → 音箱行 → `均衡器`

两处进同一个页面（`/settings/speakers/{id}/eq`），不因入口不同而分层——EQ 是设备属性，不是播放上下文。

#### 界面

```
┌────────────────────────────────────┐
│  ← 客厅 Soundbar         [开 ▢] │
├────────────────────────────────────┤
│  flat  rock  pop  jazz  bass …  │   ← 预设 chips，横向滚动
├────────────────────────────────────┤
│  +12 ┤                            │
│    0 ┤─●──●──●──●──●──●──●──●──●──●─│   ← 10 段推子
│  -12 ┤                            │
│      31 62 125 250 500 1k 2k 4k 8k 16k │
├────────────────────────────────────┤
│  自动衰减 -3.0 dB（防止削波）    │   ← 服务端回填，只读
│  应用到组内全部音箱                 │
└────────────────────────────────────┘
```

#### 规则

- **预设同一套**：flat / rock / pop / jazz / classical / bass_boost / treble_boost / vocal / custom，与 `/settings/equalizer` 完全一致，避免用户在两处看到不同名字。
- **拖动即时预览**：本地先画推子，300ms 防抖后 `PUT /players/{id}/eq`（与 §7.5 一致）；切预设立即提交，不等防抖。
- **preamp 只读**：由服务端算，UI 只展示"自动衰减 -3.0 dB"。不给用户调 preamp 的入口——调错了就削波，而用户不知道什么是削波。
- **不隐式联动**：改一台就是改一台。需要一致时点显式的`应用到组内全部音箱`，并在完成后提示"已应用到 3 台"。
- **本机叠加保护**：本机作为服务端输出期间，本地均衡器开关**自动置灰**并显示`EQ 由服务端应用`。否则两处都开就是两次 EQ，听感完全错位。
- **槽位预检**：打开 EQ 会强制该组走解码管线、占一个播放器槽位。打开前先看 `/settings/player-output.max_groups` 与当前占用，满了就禁用开关并给出原因，不要让用户点了才报错。
- **TV 端**：推子换成 ± 按钮（每档 1dB），预设 chips 用左右键切换；不做垂直拖拽。

## 6. 设置页

### 6.1 新增分类「播放与音箱」

对应 Roon 的 **Settings → Audio**（设备层）与 **Settings → Setup**（区域层），两层分开：设备是“能不能用”，区域是“怎么一起放”。

```
设置
└─ 播放与音箱
   ├─ 音频设备（Roon: Settings > Audio）
   │   ├─ 🔊 客厅 Soundbar    [AirPlay] 在线 · 已启用
   │   ├─ 🔊 书房音箱          [DLNA]   离线 · 重连
   │   ├─ 💻 本机（MacBook）   [Local]  在线 · 已启用
   │   └─ ＋ 添加设备
   ├─ 区域（Roon: Zones）
   │   ├─ 客厅（2 个输出）     重命名 · 图标 · 删除
   │   └─ 卧室（1 个输出）
   ├─ 扫描设置
   │   ├─ 自动发现（开/关）
   │   └─ 协议（AirPlay / DLNA 多选）
   ├─ 输出参数（高级）
   │   ├─ 同时播放上限（默认 4 组）
   │   ├─ 缓冲延迟（默认 2000ms）
   │   ├─ DLNA 定期重对齐（默认关）
   │   ├─ 派对（开/关）
   │   └─ 派对漂移容差（默认 1500ms）
   ├─ 本机
   │   ├─ 被其他设备接管时（暂停 / 跟随）
   │   └─ 均衡器（服务端应用）
   └─ 手动添加
       └─ IP + 端口
```

### 6.2 音箱行交互

| 动作 | 说明 |
|---|---|
| 启用/禁用 | 禁用后不再出现在区域选择器，但保留配置（Roon 的 Enable/Disable） |
| 重命名 | 本地别名优先于设备自报名（`modelName` 常是乱码） |
| 删除 | 只从列表移除，不卸载设备；重新扫描会再出现（需要"忽略"标记） |
| 重连 | 离线设备的手动重试 |
| 均衡器 | 每台独立 10 段 EQ（§5.5），行尾显示 `flat` / `自定义` |
| 查看信息 | 地址、协议、支持的格式/采样率、最近在线时间 |

### 6.3 与现有设置布局的融合

现有设置是主从布局（`SettingsMasterDetail` + 分类索引）。新分类作为普通分类加入，**不新建独立页面框架**；`/settings/speakers` 作为移动端二级页复用现有路由模式。

## 7. 状态管理与数据流

### 7.1 目录落点（Feature-First）

```
lib/features/playback/                    # v3 新增：WS + 本机输出（F7-01/02）
├── data/
│   ├── playback_socket.dart              # ticket → WS → 退避重连 → seq 去重 → 降级轮询
│   └── local_output.dart                 # register_player / report / 指令执行
└── domain/
    ├── playback_event.dart
    └── command.dart                      # load/play/pause/seek/volume/skip

lib/features/output/                      # 区域与设备
├── data/
│   ├── output_api.dart                   # /players /groups /playback 封装
│   └── output_repository.dart            # 聚合 + 缓存 + 错误映射
├── domain/
│   ├── zone.dart                         # 原 output_target.dart（v3 改名）
│   ├── speaker.dart
│   ├── sync_tier.dart
│   ├── signal_path.dart                  # 直通/经 DSP/有损 + 五段链路
│   ├── eq_profile.dart                   # 每音箱 EQ（含 preamp 只读回填）
│   ├── party.dart                        # 派对模型与漂移状态
│   └── use_cases/
│       ├── transfer_playback.dart        # 转移编排（乐观更新 + 回滚）
│       ├── group_edit.dart               # 分组/拆分/换主控
│       ├── zone_identity.dart            # 重命名 + 图标
│       ├── party_edit.dart               # 发起/加入/退出/结束派对
│       └── eq_edit.dart                  # EQ 防抖提交 + 槽位预检
└── presentation/
    ├── widgets/
    │   ├── zone_button.dart              # 播放条常驻入口（区域名 + 图标 + 状态点）
    │   ├── zone_picker.dart              # Roon 式区域控制台（列表 + 行内音量/传输）
    │   ├── zone_row.dart
    │   ├── speaker_tile.dart
    │   ├── group_member_list.dart
    │   ├── group_volume.dart             # 组音量 + 成员偏移（F7-06）
    │   ├── sync_badge.dart
    │   ├── signal_path.dart              # 三色圆点 + 五段面板（F7-08）
    │   ├── party_section.dart            # 选择器内的派对段
    │   ├── party_badge.dart
    │   ├── drift_indicator.dart          # 正在对齐… / 未对齐
    │   ├── eq_sheet.dart                 # 每音箱 EQ（复用 equalizer_panel 视觉）
    │   └── transfer_overlay.dart         # 过渡态
    └── providers/
        ├── output_provider.dart
        ├── zone_focus_provider.dart      # 控制焦点（本地持久化）
        └── playback_sync_provider.dart
```

设置页相关文件并入 `features/settings`：

```
features/settings/presentation/
├── speaker_settings_page.dart
├── speaker_eq_page.dart                  # 每音箱 EQ（§5.5）
├── widgets/speaker_list_section.dart
└── providers/speaker_settings_provider.dart
```

EQ 面板直接复用现有 `features/player/presentation/widgets/equalizer_panel.dart` 的推子视觉，不另画一套控件。

### 7.2 Provider 图

```
apiClientProvider
   └─► outputApiProvider ──► outputRepositoryProvider
                                  ├─► speakersProvider      (AsyncNotifier<List<Speaker>>)
                                  ├─► zonesProvider         (AsyncNotifier<List<Zone>>)   ← GET /playback
                                  └─► zonePlaybackProvider  (Notifier<Map<String, ZonePlayback>>)

playbackSocketProvider (StreamProvider<PlaybackEvent>)
   └─► zonePlaybackProvider（按 group_id 写入，带 seq 去重）
   └─► speakersProvider（speaker_seen / offline 增量更新）

zoneFocusProvider      (Notifier<String>)       ← 本地持久化，不发请求
currentZoneProvider    = 派生（zoneFocus → zonesProvider 中的区域）
isLocalZoneProvider    = 派生（currentZone.isLocal）

partyProvider          (AsyncNotifier<Party?>)  ← /playback/party + party_changed / group_drift
equalityProvider(id)   (AsyncNotifier<EqProfile>) ← /players/{id}/eq
signalPathProvider(id) = 派生（currentZone.signalPath）
```

`partyProvider` 不复制区域成员列表：派对的参与区域仍以 `zonesProvider` 为准，party 只提供 `id / leaderGroupId / position / driftMs`，避免两处状态不一致。

**`zoneFocusProvider` 与 `zonePlaybackProvider` 必须分开**：焦点是本地 UI 状态，播放是服务端状态。把两者合成一个 provider 是 v2 设计里“点击即转移”的根源。

沿用项目约定：**手写 Provider，不使用 code generation**。

### 7.3 本地播放器接管（双模）

> **状态：未实现（F7-02）。** 当前客户端从不注册为 `local` 设备，所以本机永远不会出现在区域列表里——这是“播放输出页没有本机”的根因。

这是前端最需要小心的地方。规则一句话：**所有权跟随区域**。

| 当前焦点区域 | 客户端角色 | 行为 |
|---|---|---|
| 含本机（`isLocal`） | 本地权威 | 用户操作直接驱动 `just_audio`（零延迟），位置异步上报服务端 |
| 不含本机 | 纯控制器 | 只发 REST 命令；本地播放器静默 |

**注册生命周期（必须先有它，上面两条才成立）**：

1. 启动 → 生成/读取持久化 `device_id`（UUID，存 prefs）→ 连 WS。
2. 连上后立即发 `register_player {id, kind:"local", name:<设备名>, caps:{codecs, sample_rates, max_channels}}`。
3. 每 500ms 发 `report {position_ms, buffering, volume, state, rtt_ms}`；服务端据此判在线。
4. 断线超过 30s → 服务端标 `online=false`，区域行置灰；重连后重新注册（同 `id`，不新建区域）。
5. 退出应用前发 `unregister_player`；异常退出靠心跳超时清理。

**指令执行**：服务端下发 `load {song_id, position, play_at}` 时，客户端按 `play_at - 本地时钟偏移` 起播；`play/pause/seek/volume/skip` 直接映射到 `just_audio`。服务端接管期间**本地 EQ 必须置灰**（否则 EQ 叠两次）。

冲突处理：

- 本机正在播放 A，另一台设备把区域切走 → 收到 `target_changed` → 按 `players.takeover_behavior` 处理：`pause`（**默认**）立即暂停并弹非阻塞提示，`follow` 静默转为纯控制器。
- 该偏好存在服务端 `players` 行（跟设备走，换手机不丢），设置页路径：播放与音箱 → 本机 → 被其他设备接管时。离线时用本地缓存值兰底。
- 本机正在播放 A，远端要求本机播放 B → 非阻塞 SnackBar："客厅要求接管本机播放"，3 秒内可撤销。
- 上报节流：位置上报 1s 一次，暂停/切歌/seek 立即上报。

### 7.4 WS 客户端

> **状态：未实现（F7-01）。** `clients/player` 无任何 WebSocket 代码；`OutputPage` 固定 4s 轮询，等于永久处于降级态。

- 鉴权：先 `POST /playback/events/ticket` 拿一次性 ticket，再 `?ticket=` 升级（浏览器无法带 Authorization 头）。
- 重连：指数退避 1s→2s→4s→…→30s 封顶，重连成功后立即拉一次 `/playback` 全量对齐。
- 去重：事件带 `seq`，小于当前 `seq` 的丢弃（网络乱序/重放）。
- 断线降级：连续 3 次失败 → 顶部显示"实时状态不可用"，退回 3s 轮询 `/playback`。
- **单连接多区域**：一个 WS 连接接收全部区域的事件，按 `group_id` 分发；不要每个区域各开一条。

### 7.5 乐观更新

- **转移**：点击即把目标标记为"切换中"，不等响应。
- **音量**：滑杆即时反馈，300ms 防抖后提交。
- **成员增删**：立即改本地列表，失败则回滚并提示。
- 任何乐观状态都带 `pending` 标记，收到服务端事件后清除；`pending` 超过 5s 视为失败。

## 8. 多端差异

| 端 | 区域选择器 | 说明 |
|---|---|---|
| Mobile | BottomSheet（60% 高度，可拖拽） | 手势为主，长按出菜单；行内音量用滑杆 |
| Tablet / Desktop | 侧边面板或全屏页 | 支持拖拽分组、显示每台音量条 |
| Web | Sheet | 浏览器只能作为 `local` 输出或纯控制器；不能成为 RAOP 输出端 |
| TV | 全屏页 + D-pad | 必须全键盘可达：焦点顺序 = 当前区域 → 成员 → 其他区域 → 管理；**不提供拖拽**，用"上移/下移"按钮 |
| Lynx | 只读展示 | 无 Bundle、无本地播放器；仅显示当前区域与转移按钮 |

Bundle 模式：只保留 `local` 区域，区域按钮显示"本机"，并在设置页说明"本地模式不支持音箱推送"（iOS 后台/mDNS 限制，见后端文档 §8.3）。

## 9. 错误与边界

| 场景 | 提示 | 恢复 |
|---|---|---|
| 目标音箱离线 | "卧室音箱已离线" | 重连 / 换目标 |
| 目标组正在播放别的内容 | "客厅正在播放 X，是否替换？" | 确认后覆盖；取消则保持原状 |
| 转移超时（10s） | "无法连接到卧室音箱" | 重试 / 回到原目标 |
| 格式不支持 | "该音箱不支持当前格式，已转为 MP3" | 自动降级，仅提示 |
| 组内混入 DLNA | 琥珀徽标 + "近似同步" | 允许但明示 |
| 并发上限 | "同时播放数已达上限（4）" | 引导释放某个目标 |
| 无 ffmpeg | "服务器缺少 ffmpeg，无法推送到音箱" | 设置页跳转配置 |
| 组播被阻断 | 空列表 + "扫描不到音箱？可能是 Docker 网络限制" | 手动填 IP |
| 派对成员未对齐 | 该组行灰字 `正在对齐…`；连续 3 次失败转琥珀 `未对齐` | 自动重试；建议改用同步组 |
| 派对人数超上限 | "派对最多 4 个目标，已达上限" | 引导退出某个目标 |
| 派对功能已关闭 | 不显示"发起派对"入口（而不是点了报错） | 管理员开启 `party_enabled` |
| EQ 打开但槽位不足 | "服务器槽位已满，无法应用均衡器" | 关闭开关 / 稍后再试 |
| EQ 削波保护 | 不弹提示，EQ 页只读显示 `自动衰减 -3.0 dB` | — |

## 10. 接口契约（前端视角）

依赖后端文档 §7，并新增以下端点（已同步至 [architecture_playback.md](./architecture_playback.md)）：

| 方法 | 路径 | 用途 |
|---|---|---|
| GET | `/playback` | 选择器与首屏共用：所有组 + 状态 + 能力 + `party_id`（v2 起不再有 `/playback/targets`） |
| GET | `/playback/metrics` | 指标面板：解码槽位、欠载、丢帧、重挂、各 owner 偏差 |
| POST | `/groups/{id}/transfer` | 转移播放 `{from_group_id, only_player_id?}`（路径 `{id}` 是目标组） |
| POST | `/groups/{id}/members` | 播放中加入成员 `{player_id}`（从当前时间点接入） |
| PATCH | `/groups/{id}/members/{player_id}` | 调整成员 `{delay_ms?, volume_offset?}` |
| DELETE | `/groups/{id}/members/{player_id}` | 移出成员 `?mode=continue\|stop` |
| PUT | `/groups/{id}/members/order` | 成员排序 `{player_ids[]}`（全排列，400 表示顺序非法；不改主控） |
| POST | `/players/{id}/reconnect` | 手动重连离线音箱 |
| POST | `/players/{id}/power` | 待机 / 唤醒 `{on}`；待机成员不在同步环里 |
| POST | `/playback/party` | 发起派对 `{name?, group_ids[]}` |
| GET | `/playback/party/{id}` | 派对状态（成员组、偏移、`drifting` / `unaligned`） |
| POST | `/playback/party/{id}/groups` | `{action: join\|leave\|end, group_id?, mode?}` |
| POST | `/playback/party/{id}/transport` | 广播 `play\|pause\|stop` |
| GET / PUT | `/players/{id}/eq` | 读/写单台音箱 EQ（PUT 后回填 `preamp`） |
| GET | `/players/{id}/eq/presets` | 预设列表（与 `/settings/equalizer` 同一套） |
| PUT | `/players/{id}` | 除重命名外还写 `takeover_behavior` |

WS 事件：

完整契约见后端文档 §7.4，前端只用这几种：

```
state           {group_id, song_id, position_ms, status}
target_changed  {group_id, from_group_id, reason}
member_changed  {group_id?, player_id, action: join|update|leave|offline|online|deleted|standby|wake|reorder}
group_changed   {group_id, player_id?, action: deleted|master_changed}   // 建组不发事件，需拉 /playback
speaker_seen    {player_id, name, kind}
party_changed   {party_id, group_id?, name?, action: created|joined|left|ended}
group_drift     {party_id, group_id, offset_ms, corrected, unaligned}
eq_changed      {player_id, enabled, preset}
error           {group_id?, code, message}
```

指标（解码槽位、欠载、丢帧、重挂）不走事件流，面板打开时按 2s 拉 `GET /playback/metrics`。

## 11. 分阶段交付

| 阶段 | 前端范围 | 依赖后端 |
|---|---|---|
| **F0** | 音箱管理页 + 输出选择器（只读展示、转移单机）+ WS 客户端 | P0（local sink） |
| **F1** | 本地播放器接管（`isLocal` 双模）+ 接管偏好（暂停/跟随） | P0 |
| **F2** | 组内动态增删成员、组音量/成员音量、同步徽标 | P2 |
| **F3** | 拆分播放、主控切换、延迟补偿、删除设备/组（二次确认） | P3 |
| **F4** | 每音箱 EQ（推子、预设、preamp 展示、本机叠加保护） | P2.5 |
| **F5** | 派对（发起/加入/退出/漂移提示/长按独占） | P3.5 |
| **F6** | TV/Lynx 适配、待机/唤醒（待机成员置灰、不可入精确组）、成员排序（上移/下移按钮，TV 无拖拽）、`member_caps.syncing` → 「对齐中」 | F2 |

F4 与 F5 互不依赖，可以并行；两者都只改选择器与设置页，不碰播放页主结构，风险可控。

F0 可与后端 P0 并行开发，用 mock 服务（`/playback` 返回假数据）先做 UI。

## 12. 验收清单

前端 UI 验证按 [AI 开发流程 · 前端 UI 验证](./ai-development.md#frontend-ui-verification) 用 Docker 无头 Chrome 跑 Flutter Web：

- [ ] 空状态下打开选择器：**本机仍在列表里**，底部有扫描/手动添加入口，不出现空白页
- [ ] 播放条常驻区域名 + 图标；无任何音箱时显示“本机”
- [ ] 多区域并行：客厅在放时切焦点到卧室，客厅 `status` 仍为 `playing`（前后 `GET /playback` 对比）
- [ ] 行内 ▶ 只改变该区域状态，不影响其他区域
- [ ] 行内音量只改该区域，300ms 防抖后下发
- [ ] 显式转移：⋯ → 转移到此区域，3 秒内出声，位置连续（误差 < 2s）
- [ ] 分组：拖拽/多选合并后成为一个区域，成员可单独调音量；拆分后从当前时间点接入
- [ ] 区域重命名后重启服务仍保留；空名/超长名被拒
- [ ] 信号路径：开 EQ 后圆点从绿变紫，面板五段与实际参数一致
- [ ] 组内加入成员：播放中加入后，新成员从当前进度出声，原成员不断音
- [ ] 移出成员：分离后独立播放，原组继续
- [ ] 混入 DLNA：徽标变琥珀，文案正确
- [ ] 目标离线：置灰 + 重连可用
- [ ] WS 断线：顶部提示出现，重连后状态自动对齐
- [ ] Bundle 模式：只显示"本机"，无外部目标入口
- [ ] 派对：三个目标同时出声，进度差 < 1.5s；退出其中一个不影响其余
- [ ] 派对进行中点其他区域 = 切焦点（不改变成员）；加入派对走 ⋯ 菜单且 5 秒内可撤销
- [ ] 派对中音量控件只改当前区域，不出现"总音量"
- [ ] 每音箱 EQ：拖动后 300ms 内生效，切预设无爆音，preamp 自动显示
- [ ] 本机作为输出时，本地 EQ 开关自动置灰并提示"EQ 由服务端应用"
- [ ] 接管偏好：设成"跟随"后本机被切走不暂停；设回"暂停"后恢复默认行为
- [ ] 无障碍：徽标不只靠颜色（有图标 + 语义标签）；TV 端 D-pad 可达全部动作

## 13. 已定决策与待定项

### 已定

| 问题 | 结论 |
|---|---|
| **区域模型** | 采用 Roon 的 Zone 模型：**多区域并行**，点击区域 = 切控制焦点，转移是显式动作（§4.2）。后端每区域独立引擎已支持，无需改协议 |
| **术语** | 用户可见统一叫“区域”；后端与 API 仍叫 `group`，不为了改名动契约 |
| **本机** | 内置区域，启动即注册，不依赖发现；无音箱时它是唯一目标（§7.3） |
| **信号路径** | **做**。三色（绿/紫/橙）+ 五段链路，只读诊断（§4.6） |
| **区域命名/图标** | **做**。存服务端，需新增 `PUT /groups/{id}`（§4.5） |
| 区域被切走时本机怎么办 | **暂停**为默认，做成服务端偏好 `takeover_behavior`（`pause` / `follow`），设置页可改 |
| 派对模式 | **做**。共享内容、独立时间轴，1.5s 内对齐，不提供总音量（§4.10） |
| 每音箱 EQ | **做**。服务端 PCM 管线内实现，每台独立，自动 preamp（§5.5） |

### 待定

1. **派对内能否单独换歌**：当前设计不允许（派对共享内容）。若用户强需，备选是"退出派对后再换"，需看实际使用反馈。
2. **EQ 配置的导入导出**：是否支持 REW / AutoEQ 格式的测量文件导入，取决于是否有用户真的拿麦克风测房间。
3. **派对与同步组的边界提示强度**：两个区域同处一室时，目前只在连续 3 次校正失败后提示。是否要在加入时就根据"同 IP 段 / 同名称前缀"主动提醒，待实测。
4. **区域数量上限**：Roon 不限制；我们受解码槽位（默认 4）约束。超过时是排队还是拒绝，待定（倾向：第 5 个区域返回 503 并提示“同时播放上限”）。
5. **焦点是否跟随播放**：Roon 在“跟随播放”模式下会自动切焦点到开始播放的区域。默认关闭（切焦点是用户意图），作为可选设置。
