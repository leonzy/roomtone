# MCP Agent Control

Roomtone includes an MCP server that lets agents manage the library, playback, and speakers over Streamable HTTP. It shares the existing HTTP process and port, so no extra container is required.

## Enablement and Security

MCP is disabled by default. Enabling it requires a dedicated token; do not substitute the admin credentials or a Web JWT:

```bash
MCP_ENABLED=true
MCP_TOKEN='replace-with-a-high-entropy-random-string'
MCP_SCOPES=read,playback,speakers,metadata,automation
```

The endpoint is `http://<server>:58091/mcp`, or `<BASE_PATH>/mcp` when a base path is configured. Send the token as `Authorization: Bearer ...`, never in a URL. Cross-origin browser requests are rejected. Listen only on a trusted LAN; public access requires HTTPS and source access controls.

Available scopes:

| Scope | Capabilities |
|---|---|
| `read` | Library, devices, zones, history, statistics, recommendations, and settings queries |
| `playback` | Playback, queues, transport, party transport, and playback transfer |
| `speakers` | Devices, groups, volume, EQ, power, party membership, and playback settings |
| `metadata` | Song metadata and local audio tag updates |
| `automation` | Sleep timers and alarms |

If `MCP_SCOPES` is unset, it defaults to `read` only.

### Settings Page

The Lynx client's **Settings → MCP Agent** page provides a runtime switch, scopes, token regeneration, and copyable agent configuration. For deployments not managed by environment variables, settings are stored in the database `mcp` entry and take effect immediately without a restart. Instances managed by `MCP_ENABLED` / `MCP_TOKEN` show a read-only notice and must be changed in the deployment configuration.

## Tools

- Library: `search_songs`, `get_songs`, `list_playlists`, `get_playlist_songs`, `update_song_tags`.
- Playback: `get_playback`, `play_songs`, `control_playback`, `remove_queue_items`, `reorder_queue`, `transfer_playback`.
- Speakers: `list_speakers`, `discover_speakers`, `add_speaker`, `update_speaker`, `remove_speaker`, `reconnect_speaker`, `set_speaker_volume`, `set_speaker_power`, `get_speaker_eq`, `list_eq_presets`, `set_speaker_eq`.
- Zones: `list_zones`, `create_zone`, `update_zone`, `delete_zone`, `add_zone_member`, `remove_zone_member`, `update_zone_member`, `reorder_zone_members`, `set_zone_master`, `get_zone_volume`, `set_zone_volume`.
- Party: `get_party`, `create_party`, `update_party`, `control_party`.
- Automation: `get_sleep_timer`, `set_sleep_timer`, `list_alarms`, `create_alarm`, `update_alarm`, `delete_alarm`, `test_alarm`.
- History and recommendations: `get_listening_history`, `get_listening_stats`, `recommend_songs`.
- Settings: `get_playback_settings`, `update_playback_settings`. The network output security allowlist cannot be changed through MCP.

Every mutation uses explicit IDs. The agent resolves natural-language references such as “this song” or “living room”; the server never guesses. Tools return structured results, actual state, and partial failures.

## Typical Call Sequences

```text
Search → search_songs
Resolve target zone → list_zones / get_playback
Start playback → play_songs(mode=now|next|end)
Adjust zone volume → set_zone_volume
```

Grouping:

```text
list_speakers
→ create_zone(member_ids, master_id)
→ set_zone_volume
```

Metadata editing:

```text
Resolve song_id using search or playback state
→ update_song_tags(changes={artist:"周杰伦"}, write_to_file=true)
```

Artist updates change both `songs.artist` and the `song_artists` relations. File writes use complete tags and atomic replacement without moving the file. A CUE virtual track cannot rewrite its shared whole-disc file. If file writing fails after the database commit, the tool returns a warning and `file_write=failed`.

## Listening History and Recommendations

The new `listening_events` table is a global event stream containing starts, ends, effective listening time, completion, zone, and source. It does not replace the existing context-specific `play_history`, which deduplicates songs and retains at most 50 entries per context.

`recommend_songs` uses structured filters, seed songs, effective-listening affinity, recent exclusions, and artist diversity to return candidates with reasons. It calls no external LLM, never autoplays, and does not treat audio fingerprints as style similarity.

## Agent Configuration Example

Configuration fields vary by client; the essential parameters are:

```json
{
  "url": "http://leonFN:58091/mcp",
  "headers": {"Authorization": "Bearer ${ROOMTONE_MCP_TOKEN}"}
}
```

The Docker music mount must be writable to update audio tags. With a read-only mount, only database updates are possible; file writes report failure or unavailability.
