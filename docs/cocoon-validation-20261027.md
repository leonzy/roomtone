# Cocoon Portable 握手与出声验证（2026-10-27）

对象：父仓库 `708ec132` 加未提交工作树，Windows amd64、Go 1.26.10、嵌入 Flutter Web；Cocoon Portable DSD300 / RAOP。未提交、推送或发布。

## 失败证据与修正

- 用户截图为 OPTIONS 453，随后日志还出现 OPTIONS 超时。此前一次连接已完成 SETUP，收到第一个 timing 请求后未进入音频发送。
- `Open` 在整个 RTSP 握手期间持有 `s.mu`；校时读取循环回复第一包后也等待同一锁，因此不能回复后续请求。新回归在旧实现下明确失败：第 2 个校时响应超时（`cocoon-timing-before.log`）。改用独立校时锁，并用连接身份隔离旧会话回执。
- Open 失败及 Close 会尝试释放**本连接已获确认**的 RTSP 会话；没有本方 Session 时不发送 TEARDOWN，不尝试抢占其他发送端。453 提示改为会话拒绝及可能占用，不直接归咎音频编码。
- 连接失败的信号路径弹层显示“连接失败”与错误，不同时展示绿色成功链路。

## 验证

- 回环 UDP 验证：持有握手锁时连续 4 次校时均能正确响应。假 RTSP 接收端必须先完成 4 次校时才返回 RECORD；Open 成功。模拟 RECORD 失败时确认后续 TEARDOWN 与本地 socket 清理。上述回归重复 10 次通过。
- `go test -tags dev,lite ./internal/app ./internal/player/...`、RAOP vet 通过；7 项相关 Flutter widget 测试与静态分析通过，release Web 构建成功。
- 用户随后明确授权实际播放测试。07:51:49 握手成功、07:51:50 发送首个音频包，英语音频；组基准设为 8%，既有成员偏移 +8，因此设备实际为 16%（后续 API 与界面确认）。用户明确反馈“有声音且连续”。测试期间约 50 秒输出，暂停时累计 2,238,368 个采样，retransmitted=0、resend_misses=0。
- 07:52:39 暂停后立即恢复，第二次完成 SETUP/校时/RECORD 并发送音频；状态 playing、进度增长、underruns=0。07:52:43 再次暂停，无 453、握手超时或 TEARDOWN 错误。结束保留 paused。

边界：只证明本次 Cocoon 指定音频与短时暂停/恢复；未覆盖所有音源、长时间播放、多房间同步或其他发送端正在占用时的接管。
