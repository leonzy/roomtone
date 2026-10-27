# RAAT 协议实测记录与已知边界

> 逆向自 Roon 2.74 (build 1712)：`Roon.Broker.Core.dll`（RaatZonePlayer /
> RaatTransportClient / RaatTcpAudioSource / RaatFlacAudioSource /
> RaatClockSource / RaatSignProxy）、`Roon.Audio.Raat.dll`、
> `libraatmanager.dylib`（设备端原生实现，RAAT 协议版本 1.1.48）。
> 本文档记录 Roomtone `raat/` 包实现中的协议常量速查、实测结论与
> 已知边界（诚实边界——写清楚比假装能追上更有用）。

## 协议常量速查

| 层 | 常量 | 值 |
|---|---|---|
| SOOD | 组播组 | `239.255.90.90:9003`（UDP） |
| SOOD | 报文头 | `"SOOD"` + ver `0x02` + type `81`/`82` |
| SOOD | KV | `[1B keylen][key][2B BE vallen][value]` |
| SOOD | RaatServiceID | `5e2042ad-9bc5-4508-be92-ff68f19bdc93`（service_id 必须等于它） |
| 控制通道 | 帧 | `[4B BE total_length（含头 8B）][4B BE message_type][body]` |
| 控制通道 | REQUEST | `0x80000001`，body=`[4B rid][JSON]` |
| 控制通道 | RESPONSE | `0x80000002`，body=`[4B rid][1B flags(bit0=is_final)][JSON]` |
| 控制通道 | KEEPALIVE | `0x80000003`，空帧，每 2000ms |
| 控制通道 | 服务器请求 | type=`2`，body=`[4B rid][1B flags][JSON]` |
| 控制通道 | 封面数据 | type=`3`，body=原始图片字节（无 JSON；先发裸帧再发 `update_artwork`） |
| 控制通道 | 帧上限 | 1MB（越界断连） |
| 音频通道 | 命令前缀 | DATA=0 / CHMAP=1 / STREAMID=2(`[4B BE]`) / STREAMSAMPLE=3(`[8B BE]`) / NORMALIZATION=4(`[8B gain][8B peak]`) / CODEC_DATA=5 |
| 时钟 | 查询包 | `"RAATCLKQ"`(8B) + `[4B BE rid]`（12B） |
| 时钟 | 响应包 | `"RAATCLKR"`(8B) + `[4B BE rid]` + `[8B BE remote_time ns]` + `[8B BE output_delay ns]`（28B） |
| 时钟 | 错误包 | `"RAATCLKE "`（注意尾部空格） |
| 时钟 | Sync 节奏 | 最多 200 迭代 × 5ms sleep，收 3 个响应即完成（min-RTT 样本取 offset） |
| DoP | 帧 | 每声道 3B `[DSD][DSD][marker]`；marker `_flipper ? 5 : 250`（**初始 false → 首帧 0xFA**）；立体声 6B/帧（16 位字内交换 `buf[j*4+2],buf[j*4]` / `buf[j*4+3],buf[j*4+1]`）；单声道 3B/帧 |
| FLAC | 设备能力 | info 响应顶层 `"codecs"` 列表（如 `["flac"]`） |
| FLAC | setup | `"wire_codec": "flac"`；采样率门 `ShouldUseFlacWireCodec`：0 < rate < 655360，且 rate ≥ 65536 时 rate%10==0（streamable subset） |
| 签名 | raat-sign | `POST https://raat-sign.roonlabs.net/sign`，body `{"contentHashes":[sha256hex],"keypairNames":[名]}`，响应 `{"contentHashes":{hash:{keypair:signature}}}`；缓存键 `<sha256>:<keypair>` |

## 时钟线格式提取结论（B8）

`RaatClockSource` 位于 **Roon.Broker.Core.dll**（`Roon.Broker.Transport`
命名空间；计划初稿假设在 Roon.Audio.dll，提取时修正）。线格式与
"UDP 时间戳探测对"假设**完全吻合**（RTT/offset/drift 三元组），停止
条件未触发：

- 查询 `RAATCLKQ` + rid（12B），响应 `RAATCLKR` + rid + remote_time +
  output_delay（28B）
- 每次 Sync 最多 200 次迭代（×5ms ≈ 1s 上限），收到 3 个响应即完成
- 每个响应：`local_mid = xmit + (recv-xmit)/2`、
  `offset = remote_time - local_mid`、`rtt = (recv-xmit)/2`；
  **取 rtt 最小的样本的 offset** 为本次结果
- 首次 Sync 记录 `first_local_time`；后续 Sync 的 offset 变化累计进
  `cumulative_delta`（`AverageDrift = cumDelta / elapsed`）

## RAATServer 联调记录（AC-B3）

（待用户环境实测后补：发现 `raat:<unique_id>` 出现在 /players、起播
出声、进度推进、pause/resume/seek、subscribe_transport 按钮可达、
连续播放 ≥10 分钟无 Dropout 刷屏。）

## 已知边界（诚实记录）

1. **db 型音量设备量纲待定标**：`update_volume{value: v×100}` 按
   number 型（0-100）实现；db 型设备的量纲换算需真机联调定标
   （错误方向只影响音量手感不影响可用性）。
2. **无播放中重连**：控制通道断连 → `EventError`+`EventOffline`，
   v1 不做播放中重连（与 raop 现状一致）。
3. **Dropout 策略**：设备缓冲欠载经 stream 流式响应上报，v1 记日志
   + 计数，不杀流（对齐 Roon 的 30 秒窗口 > 3 秒采样才杀流的宽松版）。
4. **FLAC 基准门通过**（101× 实时，57.2% 压缩比，往返逐字节一致）。
   mewkiz/flac v1.0.14 三个坑（实测钉死）：
   - Subframe 必须用 `PredVerbatim` + 默认分析器（自动比较
     constant/fixed/verbatim 并填 Order/RiceSubframe）；手动
     `PredFixed` + 手造 `RiceSubframe{PartOrder:0}` 会静默丢数据
     （`encodeRicePart` 读 `Partitions[0]`——空切片，0 字节写出）
   - 帧头必须自描述（SampleRate/BitsPerSample/Channels 全带）；
     `BitsPerSample=0`（"继承"）会让编码器以 0 bit/采样写空帧
   - CODEC_DATA 帧在线上自定界（帧头 blocksize + CRC16 帧尾），
     设备端是流式解码器
5. **DSD 直出未通过 go/no-go**：本机 ffmpeg 无 DSD 编码器与 DSF
   muxer（`-muxers` 无 dsf、`-encoders` 无 pcm_dsd），无法直出 DSD
   字节流——DSD 曲目继续走 PCM 路径。DoP 封装层照常交付（golden
   测试逐字对齐 ZonePlayerDopEncapsulation.cs；注意 marker 首帧是
   **0xFA** 不是 0x05——`_flipper ? 5 : 250` 初始 false）。
6. **事件通道载荷语义无反编译依据**：Roon 2.74 的 `_EventBeginRecv`
   只解密记录不解析事件载荷；`raop2/event.go` 的 volume 键解析是
   我们的扩展（bplist `volume` 键 → `OnControl("setvolume")`），
   完整事件语义待真机抓包。
7. **AirPlay 2 元数据为 DMAP 而非 bplist**（反编译钉定，修正计划初稿
   的 bplist 假设）：V2 的 `UpdateMetadata` 与 V1 相同走
   `application/x-dmap-tagged`，另加 `POST /command`
   `updateMRPlaybackState`（SupportsBPlistNowPlaying 时）。
8. **AirPlay 2 V2 路径不发 ANNOUNCE**（反编译钉定）：ANNOUNCE 仅在
   V1 兼容路径（`SetupAirPlay1` = Announce → **RECORD → SETUP**，
   注意 RECORD 先于 SETUP）；V2 = OPTIONS → 配对 → SETUP#1(bplist)
   → event 通道 → SETUP#2(Transport) → RECORD。
9. **pair-setup-pin 的 GCM IV 递增是 Roon 怪癖**：`256 == ++byte[n]`
   在 C# 恒为 false（编译器警告 CS0652 证实）——IV 末字节 +1 带
   wrap **无进位**。经真实 C# 编译器实测钉死。
10. **加密 RTSP Events 通道密钥交叉**（非笔误）：Events密钥 info=
    `"Control-Read-Encryption-Key"`、读密钥 info=
    `"Events-Write-Encryption-Key"`（salt 均 `Events-Salt`）。
    Control 通道正常（写=Write、读=Read）。
