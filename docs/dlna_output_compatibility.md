# Roomtone DLNA 输出与 AirConnect 对照

2026-11-01。仅对照 Roomtone 自己向 renderer 播放的输出路径。英文版：[English](dlna_output_compatibility.en.md)。

依据：[AirConnect af345def](https://github.com/philippe44/AirConnect/tree/af345def2e862f69e2c47fcd46b3ec3ec9b542f3)，重点为 `airupnp/src/airupnp.c`、`avt_util.c`、`common/libraop/src/raop_streamer.c` 和 README 的 HTTP/缓冲说明。采用独立实现，没有引入 AirPlay 接收器或复制其源码。

| 对照项 | Roomtone 处理 |
|---|---|
| 缓冲/HTTP 初始突发 | 有限本地文件直接供设备按 TCP 流控拉取；远程文件及必要转换在交付 URI 前准备完成。不需要为已经完整的文件额外添加静音前导 |
| Content-Length、HEAD、Range | 完整文件真实长度与字节定位；真实 FFmpeg 冷缓存回归覆盖 MP3、L16、ADTS |
| DLNA 协议声明 | DIDL 与 HTTP contentFeatures 一致区分转换/原格式、有限文件/直播；只声明已实现的字节定位，不声明 DLNA 时间范围请求 |
| 格式协商 | 默认保留原格式；明确不兼容时按接收器列表选择，优先无损；AAC ADTS 与 M4A 区分，L16 固定大端 44.1kHz 双声道，仅选择匹配该布局的声明 |
| 控制顺序 | SOAP 控制与轮询串行；装载/播放/定位临时失败有限重试，停止取消延迟播放；重载停止旧会话 |
| 混合组热加入 | 首个 AirPlay 推流成员加入 DLNA/Web 播放组、或移除最后一个推流成员时，在当前曲目/进度重建解码/委派循环并保留预算槽，不能只握手而不启动 PCM 分发 |
| 混合组时间对齐 | 定位补偿 Play 后耗时，支持者保留毫秒，S12 按整秒兼容。可靠回报者按 PCM 呈现时钟粗校正（连续两次同向 ≥1.2 秒、15 秒冷却）；S12 WAV 的 RelTime 错误，禁用其反馈并校准成员延迟。不保证声学精确同步 |
| WAV 定位确认 | 根据签名流真实 206 响应起点和 PCM WAV 的数据偏移/byteRate 确认，不把 SOAP ACK 当作成功，也不相信已证实错误的 RelTime。观察器随输出释放 |
| 暂停与跳转 | Pause 实际下发到拉流音箱并取消延迟 Play；不支持则 Stop。跳转由新会话的 Open(plan.Position) 执行，避免额外 Seek 抢到旧会话 |
| 状态回读 | 持续 1 秒兜底轮询，不因订阅成功而依赖事件；报告间插值、分数秒、不可用进度不清零 |
| GENA 生命周期 | 按设备实际 TIMEOUT 半程续订，续订失败重订阅/保留轮询，关闭退订；只采用实例 0、Master 声道 |
| 设备描述 | 支持显式 URLBase 和内嵌 renderer，服务来自同一个 renderer |
| 音量 | 使用 RenderingControl 声明的范围/步长；重连应用已保存的音量/静音/上限；零值优先 SetMute，非零先音量后解除静音；不支持 SetMute 时仍尝试设置硬件音量，不能保证这种硬件绝对静音 |
| 定位方言 | S12 等待 Play 后设备就绪再定位；时间定位不再误用 TRACK_NR（该单位要求曲目号） |

不需要移植的桥接路径：AirPlay 接收/配对、RTP 重传与抖动缓冲、AirPlay 控制器反馈、aircast/Chromecast，以及面向实时 AirPlay 输入的虚假文件长度和静音前导。Roomtone 已有完整文件时不应伪造长度或增加这类桥接延迟。

AirConnect 的 `AVTSetNextURI` 在所核对版本只有定义，没有播放调用点，不能据此宣称其曲目连播可直接搬入 Roomtone。Roomtone 保留自己的队列/自动下一首与每曲装载；DLNA 样本级无缝/精确同步不在本次承诺内。

餐厅当前是原生 EDIFIER AirPlay，不是小爱 DLNA。旧音量映射让 3% 变为 −30.46 dB，越过常用 −30..0 dB 范围；现为 3%/4% = −29.1/−28.8 dB，0 独立静音。代码回归可证明协议值，不能替代真实音箱听感。不要把修过映射描述成已确认低音量均匀。

后续暂停/WAV 回读修复验证：Windows 的 player、DLNA、app 包及 vet 通过；NAS 容器 DLNA 全包、相关 player 回归通过。NAS player 全套出现既有 `TestStandbyExcludesFromSyncB0805` 的排队帧时序失败（22→29），不能称全套稳定通过。本轮未改该测试或分发队列来绕过失败。隔离移除 Pause/恢复错误 WAV 计数器会分别使新回归失败，旧 Engine.Seek 的重复控制会话 Seek 也被新回归抓住。实机字节证据为：WAV PCM 44.1kHz/16bit/双声道、数据偏移 44，40s/50s 请求 7056044/8820044，真实响应均为 206。

验证基线 `847d5e83`；回归包括短租期自动续订、并发命令互斥、URLBase/内嵌设备、事件实例筛选、原生音量范围/不支持静音、L16 参考 PCM 逐字节比对、ADTS 文件头及 AAC→M4A 容器、HEAD/Range。隔离 overlay 故意恢复错误 URLBase 后，新回归因解析到旧地址而失败；当前实现通过。Windows 相关五个包、vet 及 NAS 容器中的 DLNA 全包/真实 FFmpeg HTTP 回归通过。Windows 未配置 C 编译器且 CGO 关闭，race 未运行。生产实机与发布证据在任务交付中单独记录；其他型号、Sonos 原生组拓扑和直播/ICY 厂商差异仍需对应设备验收，不宣称所有 DLNA 设备等价兼容。

混合组起停补充（2026-11-01）：拉流与推送成员均先完成 Open，再统一起播时间；准备耗时不消耗初始缓冲提前量。Pause/Stop 并发通知各成员，避免 RAOP TEARDOWN 阻塞 DLNA Pause。成员延迟通过逐路 PCM 静音/丢采样及对应时间戳偏移生效，无缝续曲继承偏移，不重复加延迟。同步环扣除 RAOP 合成位置中的已插入偏移。回归覆盖实际音频前导、负延迟时间戳、无缝继承、准备顺序与停止并发；声学同步仍需听音验收。

本轮 PCM/并发起停验证：Windows 相关五包通过，新增准备/续曲回归及 vet 通过；NAS 分发器全包、混合起播/并发暂停/延迟/同步/热加入/完成事件回归通过。NAS player 全包仍出现既有 TestStandbyExcludesFromSyncB0805 排队帧失败（43→45），没有修改测试绕过。

校准界面补充（2026-11-01）：Lynx 区域设置与编组提供“组内延迟校准”，范围 −2000..2000ms、整数输入、10ms 滑块、±10/±50ms 微调及单设备归零。保存前重读组成员与参数，仅写变化成员并保留最新音量偏移。后端保存失败返回错误，不更新运行态；应用失败明确已保存但未应用。推送成员继续实际 PCM 校准；拉流成员按已保存延迟调整 Start 时间（下次播放/继续/切歌生效），反馈使用已应用的补偿，不能用尚未起播的配置重对齐。DLNA 仍不是样本级同步。

验证：Windows player、DLNA、local 全包及 vet；校准 PCM 定向回归；NAS 控制校准/反馈/保存恢复与既有混合组定向回归通过。管线全包出现既有 TestStalledSinkDoesNotBlockOthers 调度失败（108/120），未修改该测试绕过。Lynx 类型检查与输出/mini-player 的 51 项测试通过；原生平台未运行，不安装 SDK。
