# 播放与音箱交互设计（前端）

> **状态：设计提案，尚未实现。** 后端契约见 [播放控制与音箱输出架构](./architecture_playback.md)；本文只描述客户端（`clients/player`，独立仓库）的交互、状态与代码落点。
>
> 适用客户端：Flutter 六端。TV 与 Lynx 的差异见 §8。

## 1. 设计原则

| # | 原则 | 含义 |
|---|---|---|
| P1 | **只有一个概念：输出目标** | 单机音箱和分组在 UI 上平级，都是"目标"。单机 = 只含自己的组。用户不需要理解"组"这个实现细节。 |
| P2 | **状态以服务端为准** | 队列、进度、音量、成员关系全部来自 `/playback` + WS 事件；客户端只做乐观更新，收到事件即校正。 |
| P3 | **切换必须诚实** | 转移、加入成员、换主控都有 0.5~3 秒空档。UI 用过渡态明说"正在连接"，**不假装瞬时**。 |
| P4 | **播放中可改，不用跳设置页** | 分组是高频动作，必须能在播放页当场增删成员；设置页只做低频的"命名/删除/发现"。 |
| P5 | **本机也是目标之一** | 手机/桌面本身可作为输出目标，与音箱同组；不是"另一个功能"。 |
| P6 | **派对不是更大的组** | 派对共享内容（哪首歌、第几秒），组共享时钟（±20ms）。两者是不同的东西，UI 不能把它们揉成一个"多选设备"。 |
| P7 | **每台音箱有自己的耳朵** | EQ 属于设备，不属于内容。客厅的摆位问题和厨房无关，不提供隐式联动。 |

## 2. 概念与状态模型

```dart
/// 输出目标：一个 group（可能只含 1 个成员）
class OutputTarget {
  final String groupId;
  final String name;              // "客厅" / "卧室音箱"
  final List<Speaker> members;
  final bool isLocal;             // 成员含本机
  final SyncTier syncTier;        // exact | aligned
  final bool online;
  final String? partyId;          // 非空 = 该目标正在派对中
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

class PlaybackState {
  final String groupId;           // 当前目标
  final int? songId;
  final double position;
  final PlaybackStatus status;    // idle|loading|buffering|playing|paused|error
  final int seq;                  // 服务端单调序号，用于丢弃过期事件
  final String? errorCode;
}
```

- `isLocal` 决定客户端是否接管本地播放器（§7.3）。
- `syncTier == aligned` 时输出选择器显示琥珀色"近似同步"徽标，且**不允许**把它加入已有精确同步组（后端也会拒绝，前端先拦）。
- 目标列表按"在线优先 → 同网络 → 名称"排序，离线目标置灰但不隐藏（用户需要知道"我的书房音箱还在列表里"）。
- `partyId` 非空的目标行显示 `派对` 徽标与参与组名，点击行为与普通目标不同（§4.6）。

## 3. 信息架构

### 3.1 路由新增

| 路由 | 页面 | 说明 |
|---|---|---|
| `/settings/speakers` | 音箱管理 | 设置主从布局中的新分类 |
| `/output` | 全屏输出页 | 仅桌面/平板/TV 使用；手机走 BottomSheet |

### 3.2 三个入口

```
播放页（mini / full / desktop bar）
  └─ 🔊 输出按钮 ──► OutputSheet（底部选择器，90% 的场景在这里完成）

设置
  └─ 播放与音箱 ──► 音箱管理（发现、命名、删除、延迟补偿、输出参数）

桌面/TV 大屏
  └─ 播放栏右侧「输出」──► 全屏输出页（可拖拽成员、显示每台音量）
```

### 3.3 为什么选择器是 Sheet 而不是独立页

转移播放是"播放中随手做"的动作，跳页会打断音乐上下文。Sheet 保持播放条可见，一次点击完成切换。设置页只承担低频管理。

## 4. 播放页：输出选择器

### 4.1 线框

```
┌────────────────────────────────────┐
│  播放到                       完成 │
├────────────────────────────────────┤
│  ● 客厅                    [同步]  │   ← 当前目标，高亮
│    🔊 客厅 Soundbar       ────○──  │   ← 组内成员，可直接调音量
│    🔊 餐厅 HomePod        ────○──  │
│    ＋ 添加音箱                      │
├────────────────────────────────────┤
│  🎉 派对中：厨房、书房         管理 │   ← 仅派对进行中显示
├────────────────────────────────────┤
│  ○ 卧室音箱                        │
│  ○ 书房音箱                 离线 ⚠ │
│  ○ 本机（iPhone）                  │
├────────────────────────────────────┤
│  ⚙ 管理音箱与分组                  │
└────────────────────────────────────┘
```

- 点**其他目标** → 转移播放（§4.3）。
- 点**组内成员** → 展开该成员的音量/延迟/均衡器，以及"仅在此音箱播放"（拆分）。
- 点**"＋ 添加音箱"** → 内联展开可加入的音箱（只列兼容成员；不兼容的置灰并给原因）。
- 点**派对行** → 进入派对编辑（加/减参与组，§4.6）；非派对时该行变为"发起派对"。
- 派对进行中，点**其他目标** = 把该目标**加入派对**（不是转移），toast 可撤销；要独占转移用长按菜单（§4.6）。

### 4.2 状态与徽标

| 情况 | 表现 |
|---|---|
| 精确同步组 | 绿色 `同步` 徽标 |
| 含 DLNA 成员 | 琥珀 `近似同步` 徽标 + 副标题"DLNA 设备无法精确对齐" |
| 目标离线 | 整行置灰 + `离线` + 点击弹"重新连接" |
| 无任何音箱 | 空状态卡片：说明 + "扫描音箱" + "手动添加 IP"（含 Docker 网络提示） |
| Bundle 模式 | 只显示"本机"，底部一行说明"本地模式不支持音箱推送" |

### 4.3 转移播放

**语义**：把 `(队列, 当前曲目, 位置, repeat/shuffle)` 从源目标搬到目标目标，源停止。等价于后端 `POST /groups/{to}/transfer {from_group_id}`。

**时序（前端视角）**：

```
1. 用户点击"卧室音箱"
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
- **过渡期间禁止二次操作**：Sheet 内其他目标行禁用，避免连点导致多个组同时被拉起。
- **失败回滚**：错误提示带"重试"和"回到客厅"，不做静默回滚（用户需要知道发生了什么）。

### 4.4 拆分播放（组 → 单机）

组内成员行右侧菜单 → "仅在此音箱播放"：

```
POST /groups/{newGroupId}/transfer { from_group_id: <原组>, only_player_id: <该成员> }
```

结果：原组保留其余成员并继续播放，该成员独立成组并**从当前时间点接入**。用于"客厅在放，我去厨房单独听"。

### 4.5 源组状态与撤销

- 转移成功后源组进入 `idle`，**队列保留**（不销毁）：用户可随时"回到客厅"继续，或手动清空。销毁队列会让"手滑切换"变成不可逆的损失。
- 成功后 5 秒内在播放页顶部给出可撤销提示：`已转移到卧室音箱 · 撤销`。
- **撤销 = 反向 transfer，位置取当前目标此刻的位置**，不是原始位置——否则时间会倒流。
- 超过 5 秒不再提供撤销，用户从选择器重新转移即可（等价操作，不做特殊入口）。

### 4.6 派对模式

**一句话：派对是"让几个房间同时放同一首歌"，不保证它们完全同拍。** 后端契约见 [architecture_playback.md §4.7](./architecture_playback.md)，前端只需把"内容共享"和"时钟同步"两件事分开表达。

#### 入口与形态

| 状态 | 选择器里的表现 | 点击行为 |
|---|---|---|
| 未发起 | 当前目标下方一行 `🎉 发起派对` | 以当前目标为第一个成员创建派对 |
| 派对中 | 当前目标行显示 `派对` 徽标；下方一行 `🎉 派对中：厨房、书房` | 进入派对编辑（勾选其他目标加入） |

派对**没有独立的页面**，它就是选择器里的一段。理由是它与"输出到哪里"是同一个问题的两种答案，拆页会让用户在两处之间来回跳。

#### 关键交互决定

**1. 派对中点其他目标 = 加入派对，不是转移。**

内容已经在多个房间同时播放时，用户点另一台音箱的意图绝大多数是"也让这台出声"。因此点击是"加入"，并且带 5 秒撤销（与 §4.5 一致）；反向意图（只要这一台）走长按菜单。

| 手势 | 行为 |
|---|---|
| 点击其他目标 | 加入派对，toast：`已加入派对 · 撤销` |
| 长按 / 右键其他目标 | 菜单：`仅在此目标播放（退出派对）`、`设置为主控房间` |
| 派对行内取消勾选某组 | 该组退出派对，默认"分离并继续" |

**2. 没有派对总音量。**

播放页的音量控件在派对期间**保持不变**——它调的仍是"当前目标"（本机所在组）的音量。想要全屋一起调，给一个显式的"应用到全部"按钮，而不是隐式联动：各房间声学条件不同，统一音量必然有人听不清。

**3. 传输控制广播，音量不广播。**

| 控件 | 作用域 |
|---|---|
| 播放 / 暂停 / 上一首 / 下一首 / 进度条 | 全派对（`/playback/party/transport`） |
| 音量 / 静音 | 仅当前目标（`/players/{id}/volume`） |
| EQ | 仅单台音箱（§5.5） |

**4. 漂移必须可见，但不能变成噪音。**

- 正常：无任何提示（1.5s 内偏差在派对场景里听不出来）。
- 某组偏差超阈值：该组行显示灰字`正在对齐…`，不弹 toast、不中断播放。
- 连续 3 次校正失败：该组行变琥珀色 + `未对齐`，展开后给一句建议`两个目标在同一房间时建议用同步组`。
- 不做"派对同步精度"这种指标展示——它不是用户能改变的东西，说了只会增加焦虑。

#### 与现有交互的冲突处理

- **派对期间本机被切走**：不适用。派对不改变"当前目标"，只改变"还有谁在放"，因此不触发 `target_changed`，也就不会暂停本机。
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

- 入口：Sheet 内"＋ 添加音箱"。
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

- **组音量**：Sheet 顶部一个主滑杆，作用于全组。
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
- **本机叠加保护**：本机作为服务端输出目标期间，本地均衡器开关**自动置灰**并显示`EQ 由服务端应用`。否则两处都开就是两次 EQ，听感完全错位。
- **槽位预检**：打开 EQ 会强制该组走解码管线、占一个播放器槽位。打开前先看 `/settings/player-output.max_groups` 与当前占用，满了就禁用开关并给出原因，不要让用户点了才报错。
- **TV 端**：推子换成 ± 按钮（每档 1dB），预设 chips 用左右键切换；不做垂直拖拽。

## 6. 设置页

### 6.1 新增分类「播放与音箱」

```
设置
└─ 播放与音箱
   ├─ 我的音箱
   │   ├─ 🔊 客厅 Soundbar    [AirPlay] 在线
   │   ├─ 🔊 书房音箱          [DLNA]   离线 · 重连
   │   └─ ＋ 添加音箱
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
lib/features/output/                      # 新模块
├── data/
│   ├── output_api.dart                   # /players /groups /playback 封装
│   ├── output_socket.dart                # WS 订阅（退避重连 + seq 校验）
│   └── output_repository.dart            # 聚合 + 缓存 + 错误映射
├── domain/
│   ├── output_target.dart
│   ├── speaker.dart
│   ├── sync_tier.dart
│   ├── eq_profile.dart                   # 每音箱 EQ（含 preamp 只读回填）
│   ├── party.dart                        # 派对模型与漂移状态
│   └── use_cases/
│       ├── transfer_playback.dart        # 转移编排（乐观更新 + 回滚）
│       ├── group_edit.dart               # 加入/移出/换主控
│       ├── party_edit.dart               # 发起/加入/退出/结束派对
│       └── eq_edit.dart                  # EQ 防抖提交 + 槽位预检
└── presentation/
    ├── widgets/
    │   ├── output_button.dart            # 播放页入口（图标 + 当前目标名）
    │   ├── output_sheet.dart
    │   ├── speaker_tile.dart
    │   ├── group_member_list.dart
    │   ├── sync_badge.dart
    │   ├── party_section.dart            # 选择器内的派对段
    │   ├── party_badge.dart
    │   ├── drift_indicator.dart          # 正在对齐… / 未对齐
    │   ├── eq_sheet.dart                 # 每音箱 EQ（复用 equalizer_panel 视觉）
    │   └── transfer_overlay.dart         # 过渡态
    └── providers/
        ├── output_provider.dart
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
                                  ├─► groupsProvider        (AsyncNotifier<List<OutputTarget>>)
                                  └─► playbackStateProvider (Notifier<PlaybackState>)

outputSocketProvider (StreamProvider<OutputEvent>)
   └─► playbackStateProvider（写入，带 seq 去重）
   └─► speakersProvider（speaker_seen / offline 增量更新）

currentTargetProvider  = 派生（playbackState.groupId → groupsProvider 中的目标）
isLocalTargetProvider  = 派生（currentTarget.isLocal）

partyProvider          (AsyncNotifier<Party?>)  ← /playback/party + party_changed / group_drift
equalityProvider(id)   (AsyncNotifier<EqProfile>) ← /players/{id}/eq
```

`partyProvider` 不复制组成员列表：派对的参与组仍以 `groupsProvider` 为准，party 只提供 `id / leaderGroupId / position / driftMs`，避免两处状态不一致。

沿用项目约定：**手写 Provider，不使用 code generation**。

### 7.3 本地播放器接管（双模）

这是前端最需要小心的地方。规则一句话：**所有权跟随目标**。

| 当前目标 | 客户端角色 | 行为 |
|---|---|---|
| 含本机（`isLocal`） | 本地权威 | 用户操作直接驱动 `just_audio`（零延迟），位置异步上报服务端 |
| 不含本机 | 纯控制器 | 只发 REST 命令；本地播放器静默 |

冲突处理：

- 本机正在播放 A，另一台设备把目标切走 → 收到 `target_changed` → 按 `players.takeover_behavior` 处理：`pause`（**默认**）立即暂停并弹非阻塞提示，`follow` 静默转为纯控制器。
- 该偏好存在服务端 `players` 行（跟设备走，换手机不丢），设置页路径：播放与音箱 → 本机 → 被其他设备接管时。离线时用本地缓存值兰底。
- 本机正在播放 A，远端要求本机播放 B → 非阻塞 SnackBar："客厅要求接管本机播放"，3 秒内可撤销。
- 上报节流：位置上报 1s 一次，暂停/切歌/seek 立即上报。

### 7.4 WS 客户端

- 鉴权：先 `POST /playback/events/ticket` 拿一次性 ticket，再 `?ticket=` 升级（浏览器无法带 Authorization 头）。
- 重连：指数退避 1s→2s→4s→…→30s 封顶，重连成功后立即拉一次 `/playback` 全量对齐。
- 去重：事件带 `seq`，小于当前 `seq` 的丢弃（网络乱序/重放）。
- 断线降级：连续 3 次失败 → 顶部显示"实时状态不可用"，退回 3s 轮询 `/playback`。

### 7.5 乐观更新

- **转移**：点击即把目标标记为"切换中"，不等响应。
- **音量**：滑杆即时反馈，300ms 防抖后提交。
- **成员增删**：立即改本地列表，失败则回滚并提示。
- 任何乐观状态都带 `pending` 标记，收到服务端事件后清除；`pending` 超过 5s 视为失败。

## 8. 多端差异

| 端 | 输出选择器 | 说明 |
|---|---|---|
| Mobile | BottomSheet（60% 高度，可拖拽） | 手势为主，长按出菜单 |
| Tablet / Desktop | 侧边面板或全屏页 | 支持拖拽排序、显示每台音量条 |
| Web | Sheet | 浏览器只能作为 `local` 目标或纯控制器；不能成为 RAOP 输出端 |
| TV | 全屏页 + D-pad | 必须全键盘可达：焦点顺序 = 当前组 → 成员 → 其他目标 → 管理；**不提供拖拽**，用"上移/下移"按钮 |
| Lynx | 只读展示 | 无 Bundle、无本地播放器；仅显示当前目标与转移按钮 |

Bundle 模式：只保留 `local` 目标，输出按钮显示"本机"，并在设置页说明"本地模式不支持音箱推送"（iOS 后台/mDNS 限制，见后端文档 §8.3）。

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
| GET | `/playback/targets` | 选择器一次性拉取：可选目标 + 当前目标 + 每个目标的能力/在线态 |
| POST | `/groups/{id}/transfer` | 转移播放 `{from_group_id, only_player_id?}` |
| POST | `/groups/{id}/members` | 播放中加入成员 `{player_id}`（从当前时间点接入） |
| DELETE | `/groups/{id}/members/{player_id}` | 移出成员 `?mode=continue\|stop` |
| POST | `/players/{id}/reconnect` | 手动重连离线音箱 |
| GET | `/playback/party` | 当前派对（无派对 404） |
| POST | `/playback/party` | 发起派对 `{group_id, name?}` |
| POST | `/playback/party/groups` | 加入派对 `{group_id}` |
| DELETE | `/playback/party/groups/{group_id}` | 退出派对 `?mode=continue\|stop` |
| POST | `/playback/party/transport` | 广播传输命令 `{action, position?}` |
| DELETE | `/playback/party` | 结束派对 |
| GET / PUT | `/players/{id}/eq` | 读/写单台音箱 EQ（PUT 后回填 `preamp`） |
| GET | `/players/{id}/eq/presets` | 预设列表（与 `/settings/equalizer` 同一套） |
| PUT | `/players/{id}` | 除重命名外还写 `takeover_behavior` |

WS 事件：

```
state           {group_id, song_id, position, status, seq}
target_changed  {group_id, from_group_id, reason}
member_changed  {group_id, player_id, action: join|leave|offline|online}
speaker_seen    {player_id, name, kind}
party_changed   {party_id, action: created|joined|left|ended, group_id, seq}
group_drift     {party_id, group_id, offset_ms, corrected}
eq_changed      {player_id, enabled, preset}
error           {group_id, code, message}
```

## 11. 分阶段交付

| 阶段 | 前端范围 | 依赖后端 |
|---|---|---|
| **F0** | 音箱管理页 + 输出选择器（只读展示、转移单机）+ WS 客户端 | P0（local sink） |
| **F1** | 本地播放器接管（`isLocal` 双模）+ 接管偏好（暂停/跟随） | P0 |
| **F2** | 组内动态增删成员、组音量/成员音量、同步徽标 | P2 |
| **F3** | 拆分播放、主控切换、延迟补偿 | P3 |
| **F4** | 每音箱 EQ（推子、预设、preamp 展示、本机叠加保护） | P2.5 |
| **F5** | 派对（发起/加入/退出/漂移提示/长按独占） | P3.5 |
| **F6** | TV/Lynx 适配、拖拽排序 | F2 |

F4 与 F5 互不依赖，可以并行；两者都只改选择器与设置页，不碰播放页主结构，风险可控。

F0 可与后端 P0 并行开发，用 mock 服务（`/playback/targets` 返回假数据）先做 UI。

## 12. 验收清单

前端 UI 验证按 [AI 开发流程 · 前端 UI 验证](./ai-development.md#frontend-ui-verification) 用 Docker 无头 Chrome 跑 Flutter Web：

- [ ] 空状态下打开选择器：显示引导 + 扫描按钮，不出现空白页
- [ ] 单机音箱转移：点击后 3 秒内出声，位置连续（误差 < 2s）
- [ ] 组内加入成员：播放中加入后，新成员从当前进度出声，原成员不断音
- [ ] 移出成员：分离后独立播放，原组继续
- [ ] 混入 DLNA：徽标变琥珀，文案正确
- [ ] 目标离线：置灰 + 重连可用
- [ ] WS 断线：顶部提示出现，重连后状态自动对齐
- [ ] Bundle 模式：只显示"本机"，无外部目标入口
- [ ] 派对：三个目标同时出声，进度差 < 1.5s；退出其中一个不影响其余
- [ ] 派对进行中点其他目标 = 加入派对，且 5 秒内可撤销
- [ ] 派对中音量控件只改当前目标，不出现"总音量"
- [ ] 每音箱 EQ：拖动后 300ms 内生效，切预设无爆音，preamp 自动显示
- [ ] 本机作为输出目标时，本地 EQ 开关自动置灰并提示"EQ 由服务端应用"
- [ ] 接管偏好：设成"跟随"后本机被切走不暂停；设回"暂停"后恢复默认行为
- [ ] 无障碍：徽标不只靠颜色（有图标 + 语义标签）；TV 端 D-pad 可达全部动作

## 13. 已定决策与待定项

### 已定

| 问题 | 结论 |
|---|---|
| 目标被切走时本机怎么办 | **暂停**为默认，做成服务端偏好 `takeover_behavior`（`pause` / `follow`），设置页可改 |
| 派对模式 | **做**。共享内容、独立时间轴，1.5s 内对齐，不提供总音量（§4.6） |
| 每音箱 EQ | **做**。服务端 PCM 管线内实现，每台独立，自动 preamp（§5.5） |

### 待定

1. **派对内能否单独换歌**：当前设计不允许（派对共享内容）。若用户强需，备选是"退出派对后再换"，需看实际使用反馈。
2. **EQ 配置的导入导出**：是否支持 REW / AutoEQ 格式的测量文件导入，取决于是否有用户真的拿麦克风测房间。
3. **派对与同步组的边界提示强度**：两个目标同处一室时，目前只在连续 3 次校正失败后提示。是否要在加入时就根据"同 IP 段 / 同名称前缀"主动提醒，待实测。
