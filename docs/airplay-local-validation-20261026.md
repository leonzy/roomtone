# Windows AirPlay 实机验证（2026-10-26）

基线：父仓库 `708ec132` 加未提交工作树，Windows amd64，Go 1.26.10，Flutter Web 嵌入本地服务。没有提交、推送或发布。

## 已确认

- 曲库为用户的 Downloads。通过 Roomtone 向 EDIFIER 9EC4 播放英语音频，组音量 15%；用户在 RSA/AES 修正后明确确认“有声音”。之前所有“PLAYING”状态而用户听不到的轮次均判为失败。
- 该设备广播 `cn=1, et=0,1`，没有 `pk`。明文 ANNOUNCE 返回无 CSeq 的空 200；使用旧版 RAOP 公共 RSA 密钥封装会话 AES key 后，ANNOUNCE 返回匹配 CSeq，SETUP 分配接收端口，实际出声。
- AES-CBC 每包只加密完整 16 字节块，尾部保持明文，不增加填充；ALAC 编码经 FFmpeg 解码逐字节还原 PCM。RTSP 请求现在拒绝无匹配 CSeq 的响应，播放引擎传播握手错误。
- 同一地址同时发现 AirPlay 和 DLNA 时优先显示 AirPlay；本次扫描得到两台 EDIFIER 和 Cocoon Portable。
- `CGO_ENABLED=0 go test -tags dev,lite ./internal/app ./internal/player/...` 通过；设置 `TEST_FFMPEG` 后 ALAC 实际解码回归通过。新增回归覆盖无 pk 的 RSA 设备、AES 尾部与包长、空/错误 CSeq 成功响应。

## 验收边界

补充（2026-10-27）：修正暂停未关闭推送会话、恢复时间轴未扣除歌曲位置。新增 `TestPauseClosesPushSession`、`TestResumeTimelineDoesNotWaitElapsedTrackTime`，同范围测试通过。最新本机二进制完成暂停→恢复，状态回到 playing、进度继续增长且无握手错误；测试结束留在 paused。此轮状态检查不替代新的听感确认。

- 仅 9EC4 的上述音频出声经用户确认；9764、Cocoon、双设备同步、长时间播放、不同格式/采样率矩阵未据此判为通过。
- 界面四个入口及高级 DSP 功能不由此音频验收证明。完整 Roon 功能对等与多房间同步仍需独立验证。
- 独立 pyatv 对照工具安装在用户本地工具目录，针对本设备的空 `/info`、ALAC/RSA 兼容探针用于排障；最终出声确认使用 Roomtone 本身。

协议对照：[RAOP-Player](https://github.com/philippe44/RAOP-Player/blob/master/src/raop_client.c)、[pyatv RTSP](https://github.com/postlund/pyatv/blob/master/pyatv/support/rtsp.py)。
