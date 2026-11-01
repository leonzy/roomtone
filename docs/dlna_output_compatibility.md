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
| 状态回读 | 持续 1 秒兜底轮询，不因订阅成功而依赖事件；报告间插值、分数秒、不可用进度不清零 |
| GENA 生命周期 | 按设备实际 TIMEOUT 半程续订，续订失败重订阅/保留轮询，关闭退订；只采用实例 0、Master 声道 |
| 设备描述 | 支持显式 URLBase 和内嵌 renderer，服务来自同一个 renderer |
| 音量 | 使用 RenderingControl 声明的范围/步长；零值优先 SetMute，非零先音量后解除静音；不支持 SetMute 时仍尝试设置硬件音量，不能保证这种硬件绝对静音 |
| 定位方言 | S12 等待 Play 后设备就绪再定位；时间定位不再误用 TRACK_NR（该单位要求曲目号） |

不需要移植的桥接路径：AirPlay 接收/配对、RTP 重传与抖动缓冲、AirPlay 控制器反馈、aircast/Chromecast，以及面向实时 AirPlay 输入的虚假文件长度和静音前导。Roomtone 已有完整文件时不应伪造长度或增加这类桥接延迟。

AirConnect 的 `AVTSetNextURI` 在所核对版本只有定义，没有播放调用点，不能据此宣称其曲目连播可直接搬入 Roomtone。Roomtone 保留自己的队列/自动下一首与每曲装载；DLNA 样本级无缝/精确同步不在本次承诺内。

餐厅当前是原生 EDIFIER AirPlay，不是小爱 DLNA。旧音量映射让 3% 变为 −30.46 dB，越过常用 −30..0 dB 范围；现为 3%/4% = −29.1/−28.8 dB，0 独立静音。代码回归可证明协议值，不能替代真实音箱听感。不要把修过映射描述成已确认低音量均匀。

验证基线 `847d5e83`；回归包括短租期自动续订、并发命令互斥、URLBase/内嵌设备、事件实例筛选、原生音量范围/不支持静音、L16 参考 PCM 逐字节比对、ADTS 文件头及 AAC→M4A 容器、HEAD/Range。隔离 overlay 故意恢复错误 URLBase 后，新回归因解析到旧地址而失败；当前实现通过。Windows 相关五个包、vet 及 NAS 容器中的 DLNA 全包/真实 FFmpeg HTTP 回归通过。Windows 未配置 C 编译器且 CGO 关闭，race 未运行。生产实机与发布证据在任务交付中单独记录；其他型号、Sonos 原生组拓扑和直播/ICY 厂商差异仍需对应设备验收，不宣称所有 DLNA 设备等价兼容。
