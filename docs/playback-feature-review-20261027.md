# 新播放协议更新检查（2026-10-27）

对象：主仓库 `b34daad`（相对 `4440ed2` 新增 28 个提交、90 个文件），Flutter `305f93c`；Windows amd64、Go 1.26.10、Flutter 3.47.0-0.1.pre，嵌入 Web。以下修复保留在未提交工作树；Go 代码内容摘要 SHA-256：`d7d111324b6d6df22d95c36e3c9542193657b3ba7880da7029d623e03e343167`。

## 已修正的问题

1. `initPlayback` 创建 Builder 时捕获了尚未赋值的 manager 接收者；DACP/RAAT 反控会调用 nil manager。改为在回调执行时解析当前 manager。
2. AirPlay 2 发现合并遗漏独立 `_airplay._tcp` 端口、V2-only DeviceID、声道/采样率/位深和基础控制能力，导致走错端口或路径。补齐完整传输身份与能力。
3. V2 的 Digest 挑战写入了授权头，但 RTSP 请求根本没有发送它。现与现有 V1 会话契约一致发送授权头。
4. AirPlay 2 timing 回复误读请求的时间戳，响应 NTP 字段位置错误且不回显序号。修为与 V1 相同的 32 字节结构，并用捕获的 socket 回包。
5. AirPlay 2 把重传请求的 count 当作第二个序号，且漏包不带重传封装。现修复连续范围（包括序号回绕）、封装及设备 IP/包长验证。
6. V2 首个同步包从未发出（lastSync 为零时跳过），同步包还被发到音频端口，时间轴字段也未带缓冲延迟。改为首帧及随后周期发送到 control 端口，并携带正确延迟关系。
7. V1/V2 屏显网络请求及图片文件读取持有 PCM 发送锁；设备响应慢时会拖住音频。改为短锁快照后在锁外等待网络和文件处理。
8. RAAT 反控回调同步运行在唯一响应读循环内，再次发送 stop/seek/volume 等请求会等待自身读循环。改为有界、顺序执行的独立反控队列。
9. RAAT P0 起播固定提前 200ms，忽略引擎未来的 startAt，使组内 master 可能提前播放。尊重计划起播时刻，保留最少 200ms 预留；Ready 后延迟起播也在锁与超时保护下执行。
10. 设备密码虽有数据库/manager 支持，但 PUT /players/{id} 丢弃 password 字段。补齐设置/保留/清空与响应不泄漏的 HTTP 回归，重新生成 Swagger。

另外修正两项测试：RAAT 真机探测改为显式 `TEST_RAAT_ADDRESS` 选择加入；流式响应测试等待终段后才断言，不再依赖线程调度。

## 验证

- `go test -p 1 -tags dev,lite ./internal/app ./internal/player/... ./internal/database` 通过；播放相关 handlers 定向测试通过；相关包 `go vet` 通过。
- 9 项 wire/锁/时序回归在旧实现 overlay 上全部失败，在新实现上通过；关键回归重复 10 次通过（包括额外的 manager 绑定与流式响应检查）。
- Flutter 输出/播放后台/队列测试 96 项通过；release Web 与完整 Go dev 嵌入编译通过。
- 本机启动后登录、设备/区域 API 可用；数据库 quick_check=ok，迁移 45/46 已应用，scan_config 包含 dsf；各区域队列与原先一致，Cocoon/9764 保持 paused，Web/另一台保持 idle。未触发新的音箱播放。
- 一致性数据库备份保存在本机工具目录 `before-feature-review-1793109486.sqlite`。
- 完整首轮并非全绿：旧 handlers/services 的 /bin/echo、/bin/true 参数测试不适用于 Windows；DSF 测试的 .dsf/.DSF 在本机大小写不敏感文件系统只生成一个文件。它们相对上一版本源码未变。pipeline 并行测试曾少收帧，串行全套通过；不将其首次失败隐藏为全量通过。

## 尚未闭环的功能边界

- 新 RAAT 和 AirPlay 2 完整真机出声、长期连续性、设备编组、采样级同步与硬件密码认证：Not run。回环/假设备不能代替这些验收。
- 本机 discovery.protocols 保持原配置 `[raop, dlna]`，RAAT 没有自动开启；后端接受 raat，但现有前端没有发现协议开关或 RAAT 手动添加入口。
- RAAT FLAC 是可选底层实现，应用 Builder 未启用 FLAC 选项；DoP 封装已存在，但 DSD 输入仍走 PCM（上游记录的 go/no-go 未通过）。本次没有把这些底层实现宣称为完整上线功能。
- 初次 fetch 递归抓取历史的孤立子模块提交 fdfba7f 失败；最新主仓库已改为可取得的 305f93c，使用当前指针已同步成功，未改写远端历史。

日志：本机工具目录 `new-features-final-tests.log`、`new-features-handler-tests.log`、`new-features-vet.log`、`new-features-before-regressions.log`、`new-features-stress.log`、`new-features-flutter-test.log`、`new-features-flutter-build.log`。


## 用户授权后的实际播放复验（2026-10-27 22:33）

- 用户要求“推送。启动实际程序测试”。本机运行程序与最终构建 roomtone-next.exe 的内容一致，Web 实际操作起播英语 MP3（song_id=5），Cocoon 实际音量保持原 16%。
- 首次 SETUP 返回 500；单次人工重试后会话成功，完成 SETUP/RECORD、校时、DACP 注册和音频发送。首次拒绝原因尚未确定，不能宣称已修复该偶发现象。
- 用户明确确认“有声音且连续”。Cocoon 会话发送 1,845,888 个采样，约 41.9 秒，重传 0、miss 0；Web 当时保持 idle，DOM 无 audio/video 元素。
- 随后用户在实际界面把播放转移到 EDIFIER 9764：POST transfer 返回 200，源 Cocoon 停止，目标建立 ALAC 会话并推进进度。此项有服务端/界面证据，未取得 9764 的独立听感确认。
- 用户继续手动操作播放区域与曲库，本轮保留其最新选择，没有用测试前快照覆盖用户的新队列。暂停/继续/seek 未另行自动操作，不将这些项算作本轮新增验收。
- 新 RAAT / AirPlay 2 硬件、长时间连续性、采样级同步仍 Not run；以上真机出声仅覆盖 AirPlay 1 的 Cocoon。


## 转换区域追加修正（2026-10-28）

- 根因：Engine.Load 在目标正在运行时只换队列，startLocked 因 running=true 返回，旧解码循环继续播放旧曲目。现在加载前停止旧会话，再加载来源队列和位置；旧运行上下文的失败、进度与结束回调不能覆盖替换后的会话。
- 新增 TestTransferReplacesBusyTargetAndStopsSource：来源 11、目标 22 同时播放，转换后目标必须收到 11，来源 idle 且不再写帧。旧代码 overlay 超时失败，新实现及相关 Transfer/Next/Seek 检查重复 5 次通过。
- 最终 Go app/player/.../database 串行套件与相关 go vet 通过；前端 370 项相关测试、Dart 分析和 Web/Go 嵌入构建通过。音量面板先关闭再打开转换面板已实际验证。
- 实机忙碌目标转换未完成：餐厅连接中断，Cocoon 首次 SETUP 500；重试期间用户要求停止播放，已执行 stop，Web/Cocoon/餐厅均 idle。未宣称新的实机转换或听感通过。后续不再自动播放。


## 双向实机转换与 Web 队列接管（2026-10-28）

- a816851 / ea947cd 本机嵌入版：Cocoon(song 5) 与 Web(song 2) 先分别播放；界面将 Cocoon 转到忙碌 Web 后，Cocoon idle，Web 接续 song 5。用户确认“只有 Web 出声，正常”。
- 随后 Cocoon 先播放 song 2，Web 继续 song 5；界面反向转移后 Web idle、Cocoon song 5 连续推进，用户确认“只有 Cocoon 出声，连续”。转换对话框均自动关闭，旧音量面板没有残留。
- 额外发现 Web 收到 load 会 playSong，把来源曲目追加到旧队列并回推服务端。修正为按本机持久设备 ID 读取所属区域的完整队列、下标与曲目；校验服务端当前曲目与命令相符后替换本机队列。异步读取保留代次检查，stop 可取消；play 等待 load 完成，避免网络读取较慢时恢复旧曲。
- 373 项相关测试通过；新增完整队列替换、读取中 stop、play 等待 load/stop 取消等待的回归。四个变更文件 Dart 分析无问题，release Web 构建通过。证据存于本机工具目录 transfer-busy-start.json、transfer-cocoon-to-web.json、transfer-web-to-cocoon.json、transfer-queue-final-tests.log。

最终嵌入构建实机复验：Web 原 8 首队列(song 13)与 Cocoon 6 首英语队列(song 2)均 playing 后，从界面转移 Cocoon→Web。断言 Web song_id=2、playing、完整队列 [5,2,3,4,7,6]、current=1；Cocoon idle。界面队列显示 6 首、进度持续推进（72.604s），证据 transfer-complete-queue-live.json / transfer-complete-queue-fixed.png。完成后停止测试并恢复本轮开始时 Web/Cocoon 的队列与下标，未恢复自动播放。
