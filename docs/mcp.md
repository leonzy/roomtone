# MCP Agent 控制

Roomtone 内置 MCP Server，通过 Streamable HTTP 让 Agent 管理曲库、播放和音箱。服务与现有 HTTP API 共用进程和端口，不需要额外容器。

## 启用与安全

MCP 默认关闭。启用时必须设置独立 Token，不能使用管理员账号或 Web JWT 代替：

```bash
MCP_ENABLED=true
MCP_TOKEN='替换为高强度随机字符串'
MCP_SCOPES=read,playback,speakers,metadata,automation
```

端点为 `http://<服务器>:58091/mcp`；设置 `BASE_PATH` 时使用 `<BASE_PATH>/mcp`。Token 通过 `Authorization: Bearer ...` 传递，不要放进 URL。跨源浏览器请求会被拒绝。仅监听可信局域网；公网访问应增加 HTTPS 和来源访问控制。

Scope 可单独授予：

| Scope | 能力 |
|---|---|
| `read` | 曲库、设备、区域、历史、统计、推荐和设置查询 |
| `playback` | 播放、队列、传输控制、派对传输和播放转移 |
| `speakers` | 设备、分组、音量、EQ、电源、派对成员和播放设置 |
| `metadata` | 修改歌曲元数据及本地音频文件标签 |
| `automation` | 睡眠定时器与闹钟 |

未设置 `MCP_SCOPES` 时默认仅 `read`。

## 工具

- 曲库：`search_songs`、`get_songs`、`list_playlists`、`get_playlist_songs`、`update_song_tags`。
- 播放：`get_playback`、`play_songs`、`control_playback`、`remove_queue_items`、`reorder_queue`、`transfer_playback`。
- 音箱：`list_speakers`、`discover_speakers`、`add_speaker`、`update_speaker`、`remove_speaker`、`reconnect_speaker`、`set_speaker_volume`、`set_speaker_power`、`get_speaker_eq`、`list_eq_presets`、`set_speaker_eq`。
- 区域：`list_zones`、`create_zone`、`update_zone`、`delete_zone`、`add_zone_member`、`remove_zone_member`、`update_zone_member`、`reorder_zone_members`、`set_zone_master`、`get_zone_volume`、`set_zone_volume`。
- 派对：`get_party`、`create_party`、`update_party`、`control_party`。
- 自动化：`get_sleep_timer`、`set_sleep_timer`、`list_alarms`、`create_alarm`、`update_alarm`、`delete_alarm`、`test_alarm`。
- 历史与推荐：`get_listening_history`、`get_listening_stats`、`recommend_songs`。
- 设置：`get_playback_settings`、`update_playback_settings`。出口网络安全白名单不能通过 MCP 修改。

所有写操作使用明确 ID。自然语言中的“这首歌”“客厅”等指代由 Agent 解析；服务端不会猜测。工具返回结构化结果、实际状态和部分失败信息。

## 典型调用顺序

```text
搜索歌曲 → search_songs
确定目标区域 → list_zones / get_playback
起播 → play_songs(mode=now|next|end)
调整区域音量 → set_zone_volume
```

分组：

```text
list_speakers
→ create_zone(member_ids, master_id)
→ set_zone_volume
```

修改元数据：

```text
search_songs 或播放状态确定 song_id
→ update_song_tags(changes={artist:"周杰伦"}, write_to_file=true)
```

歌手修改会同时更新 `songs.artist` 和 `song_artists` 关联。文件回写使用完整标签和原子替换，不移动文件；CUE 虚拟曲目不能单独回写共享整轨文件。文件失败时数据库仍已更新，工具会返回警告和 `file_write=failed`。

## 听歌历史与推荐

`listening_events` 是新增的全局事件流水，记录开始、结束、有效播放时长、完成度、区域和来源。它与原有按上下文去重、最多 50 条的 `play_history` 续播记录互不替代。

`recommend_songs` 使用结构化筛选、种子歌曲、有效收听偏好、近期排除和艺术家多样性生成候选及理由。它不调用外部 LLM，不自动播放，也不把音频指纹当作风格相似度。

## Agent 配置示例

不同 Agent 客户端的配置字段可能不同，核心参数为：

```json
{
  "url": "http://leonFN:58091/mcp",
  "headers": {"Authorization": "Bearer ${ROOMTONE_MCP_TOKEN}"}
}
```

若需要回写音频标签，Docker 的音乐目录必须可写；只读挂载只能修改数据库，文件写入会返回失败或不可用状态。
