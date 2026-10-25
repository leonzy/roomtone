# Playback Control and Speaker Output · Development Plan

> **Status: executable plan v1 (2026-10-25).** For design and decisions see [Playback Control and Speaker Output Architecture](./architecture_playback.md) (backend) and [Playback and Speaker Interaction Design](./architecture_playback_ui.md) (frontend). This document does not repeat the reasoning; it answers three things: **in what order, which files, and what counts as done**.
>
> Task granularity = one independently committable, independently verifiable change. Task IDs go into commit messages (`feat(player): queue semantics and transport state machine (B1-02)`), so implementation and traceability map one to one.

## 0. How to use this plan

- Progress is tracked in the §11 progress table; tick an item when done. The task tables carry no status column, so the two can never disagree.
- Gates (§6) block the next milestone. Cut scope instead of lowering acceptance criteria (for example, push DLNA to v0.4) — once something like sync accuracy or clipping tolerance is relaxed, the selling point of the feature is gone.
- Estimates are **person-days for one full-time developer, including self-testing and docs**, excluding time waiting for real hardware (called out separately).
- Run a full `make check` at the end of each week; do not change acceptance criteria mid-week.

## 1. Delivery scope

| Version | Milestones | What the user can perceive | Cumulative person-days |
|---|---|---|---|
| **v0.1 (internal)** | M0 + M1 + M2 | Server holds a queue and playback state, can drive client playback (`local` sink) | ~31 |
| **v0.2** | + M3 | **Audio actually comes out of an AirPlay speaker**, single device, no sync | ~44 |
| **v0.3** | + M4 | DLNA speakers added; single or multiple devices playing independently | ~54 |
| **v0.4** | + M5 | **Multi-room sync** (exact over RAOP, degraded with DLNA) | ~62 |
| **v0.5** | + M6 | Per-speaker EQ | ~68 |
| **v1.0** | + M7 + M8 + X | Party mode, resume after restart, observability, security and docs wrap-up | ~85 |

Non-goals follow design §1.2: **AirPlay 2**, Chromecast / native Sonos, video push, plugin control of players. They are repeated here so that "should we build this?" has a documented answer instead of being re-litigated every time.

## 2. Baseline inventory

This is where the plan first diverges from the design doc: the design was written against a target architecture, and several of its assumptions do not hold against the current code.

### 2.1 Directly reusable

| Existing asset | Location | How the player uses it |
|---|---|---|
| Slot-limiting pattern | `internal/services/seek_stream.go:48` `seekStreamMaxConcurrent = 4` + `seekStreamSem` + `ErrSeekStreamUnavailable` | Same pattern for `playerDecodeSem`, **counted independently**; the two budgets never starve each other |
| WebSocket library | `go.mod` already has `gorilla/websocket v1.5.3` (used by jsplugin as a client) | Server-side hub uses it directly, no new dependency |
| ffmpeg path config | `internal/app/app.go:303` `cacheService.SetFFmpegPath(ffmpegConfig.Path)`, key `ffmpeg_path` | Decoder reuses the same path; config changes take effect |
| 10-band EQ presets | `internal/handlers/equalizer_setting.go` (key `equalizer`) | Extract a shared preset table so server and client EQ use identical names |
| Loudness parameters | `internal/services/cache_service_loudness.go` | Group-level loudnorm reuses the same parameters (including `TP=-1.5`) |
| In-memory DB tests | `internal/database/testutil.OpenMemoryDB(t)` | All repository tests for the six new tables use real `:memory:` |
| Router groups and auth | `internal/app/routers.go:118` `r.Route("/api/v1", ...)` | New endpoints join the same group and inherit JWT plus `respondError` |
| Migration numbering | Current max is `0039_scan_support_dsf.sql` | New tables start at `0040` |

### 2.2 Confirmed missing, must be built

| Gap | Impact | Task |
|---|---|---|
| No server-side playback state, queue, or transport state machine | The foundation of the whole feature | B1 group |
| No WebSocket **server** (existing ws is only a client to plugins) | Event push and `local` sink commands have nowhere to go | B0-06 |
| No LAN address resolution — `internal_url.go` only produces `127.0.0.1` | **Neither DLNA nor RAOP can produce a stream URL the device can reach** | B0-05 |
| No `internal/player` package | All backend tasks | from B1-01 |
| No mDNS / SSDP client | Devices cannot be discovered | B3-01 / B4-01 |
| No audio DSP (only ffmpeg filters, one chain per process) | Per-speaker EQ | B6-01 |
| `clients/player` submodule is not checked out and is a separate repo | Frontend tasks cannot land in this repo | D1 + F group |
| No notion of a device being "taken over" | Multi-client conflicts cannot be expressed | B1-01 schema + F1 |

**B0-05 is the most easily underestimated item in this inventory**: the design says "reuse the LAN-reachable absolute URL produced by `internal_url.go`", but that file produces `http://127.0.0.1:{port}`, which exists for the backend's own callbacks. A DLNA renderer handed `127.0.0.1` will only ever pull from its own loopback. The correct approach is to resolve the egress interface from the **target device's IP** (dial `udp` to the peer, then read `LocalAddr`), which is the only way to pick correctly with multiple NICs, Docker, or a VPN.

### 2.3 Conflicts with existing constraints (must be handled explicitly, not worked around)

| Conflict | Current behavior | Handling |
|---|---|---|
| Private network blocking | `internal/services/whitelist.go` rejects private IPs by default | New `player_output_allowlist`, **not** a reuse of the proxy allowlist (X-02) |
| ffmpeg slots | 4 concurrent transcode slots, `seekStreamSem` | Independent `player_max_groups`, default 4 (B0-08) |
| Passthrough has no PCM | Without transcoding the server 302s to the original file | Any non-flat EQ in the group forces decoding and occupies a slot (B6-04) |
| Docker cannot receive multicast | Bridge network blocks 239.255.255.250 / 224.0.0.251 | Document `--network host`, show a hint in the empty state, manual IP entry as fallback (X-03) |
| Outbound proxy | `httputil.NewClient` is proxy-aware | **DLNA SOAP must use a bare `http.Client`**; otherwise a user with a global proxy can never reach LAN devices (B4-03) |

## 3. Dependency graph

```
B0-01 ──┬─► B1-01 ─► B1-02 ─► B1-03 ─┬─► B1-04 ─► B1-05 ─► B1-06
        │                            └─► B2-01 ─► B2-02 ─┐
B0-02 ──┴─► B0-03 ──────────────────────────────────────┴─► B2-04 ─► B2-06
B0-05 ─────────────────────────────────────────► B4-04 ─┘      │
B0-06 ────────────────────────────────────────────────────────┘
B0-08 ─────────────────────────────────► B6-04 / B7-04
        └─► B2-04 ─► B3-02 ─► B3-03 ─► B3-04 ─► B3-05 ─► [G1] ─► B3-08
        └─► B2-04 ─► B4-03 ─► B4-06 ─► [G2]
B3-05 + B4-03 ─► B5-01 ─► B5-05 ─► B5-06 ─► [G2]
B2-01 ─► B6-01 ─► B6-03 ─► [G3]
B5-01 ─► B7-01 ─► B7-03 ─► [G4]
```

Critical path: `B0-01 → B1-02 → B2-01 → B2-04 → B3-05 → G1 → B5-01 → B5-06 → G2 → B7-03 → G4`.

Frontend F0 can start before B1-05 (stub `/playback/targets`). That is the only line that truly parallelizes without adding rework risk.

## 4. Backend task breakdown

### M0 Foundations (B0, 11 person-days)

No user-visible functionality, but every later milestone stands on it.

| ID | Task | Files | Acceptance | d |
|---|---|---|---|---|
| B0-01 | Migration `0040_players.sql` with six tables (`players` / `player_groups` / `party_sessions` / `player_group_members` / `playback_queues` / `playback_states`) + `queries/*.sql` + `make sqlc` + repository skeleton | `internal/database/migrations/0040_players.sql`, `internal/database/queries/players.sql`, `internal/database/player_repository.go`, `internal/database/sqlc/*` | Tables created in memory DB; rejection cases for the `kind` / `takeover_behavior` CHECK constraints; test for `party_id` delete behavior | 2 |
| B0-02 | Injectable clock + NTP conversion | `internal/player/clock.go` | Unit tests with a fake clock; assert **a 1h wall-clock step does not disturb the timeline**; NTP conversion is monotonic | 1 |
| B0-03 | Unified timeline | `internal/player/timeline.go` | Frame index ↔ absolute time for 44100 and 48000; configurable latency offset | 1 |
| B0-04 | Ring buffer | `internal/player/pipeline/ring.go` | 3s capacity; underrun **inserts silence instead of blocking**; `underruns` counter; clean under `-race` | 1.5 |
| B0-05 | LAN address resolution | `internal/player/lanurl.go` | Egress interface chosen from peer IP (dial `udp`, read `LocalAddr`); honors `base_path` deployments; returns an explicit error for loopback peers instead of silently handing out 127.0.0.1 | 1 |
| B0-06 | WS server hub + ticket auth | `internal/app/ws/hub.go`, `internal/handlers/playback_events.go` | `POST /playback/events/ticket`: one-shot, 30s, bound to user, invalidated after use; slow clients dropped without blocking the broadcast; monotonic `seq` | 2.5 |
| B0-07 | Two settings endpoints | `internal/handlers/player_setting.go`, `routers.go` | `/settings/player-discovery`, `/settings/player-output` as strongly typed JSON with defaults applied in the handler; out-of-range `party_drift_tolerance_ms` returns 400; swagger generated | 1 |
| B0-08 | Player budget | `internal/player/budget.go` | `player_max_groups` defaults to 4; **independent of `seekStreamSem`** (test asserts neither blocks the other when both are saturated); over-limit errors distinguish causes | 0.5 |
| B0-09 | Event contract | `internal/player/event.go` | Eight event structs + JSON encoding; unit tests for the `seq` type and dedup semantics | 0.5 |

**Exit criteria**: `go test ./internal/player/...` green, `make swagger` produces no diff, both settings endpoints visible in `/swagger/index.html`.

### M1 Queue and transport (B1, 9.5 person-days)

| ID | Task | Files | Acceptance | d |
|---|---|---|---|---|
| B1-01 | Player entity, capability model, registry | `internal/player/player.go`, `manager.go` | `Caps` encode/decode; `takeover_behavior` only meaningful for `local`; concurrent register/unregister is safe | 1.5 |
| B1-02 | Queue semantics | `internal/player/queue.go` | `current/repeat(all\|one\|off)/shuffle/jump/insertNext`; table-driven cases for "next still advances when repeat=one" and "disabling shuffle restores original order"; **preload decision (remaining <30s)** | 2 |
| B1-03 | Transport state machine | `internal/player/transport.go` | `idle→loading→buffering→playing⇄paused→error→recover`; the `buffering→playing` predicate is the **2s ring-buffer level**, not first frame; a follower going offline does not change group state | 1.5 |
| B1-04 | Queue and state persistence + restart recovery | `internal/player/snapshot.go` | Snapshots written with a **separate `flushCtx`** so a cancelled request cannot abort them; queue and position restored after restart; write failures never interrupt playback | 2 |
| B1-05 | Queue / transport / snapshot REST | `internal/handlers/playback.go` | `/groups`, `/groups/{id}/queue`, `/groups/{id}/queue/reorder`, `/groups/{id}/transport`, `/playback`; all with the seven required swag annotations; errors via `respondError` | 2 |
| B1-06 | Auto single-member group | `internal/player/manager.go` | Registering a player creates a group containing only itself; deleting a device cleans the group up, leaving no orphans | 0.5 |

**Exit criteria**: with no real sink attached, a fake sink can run "load → play → pause → seek → next → restart recovery".

### M2 Decode pipeline and `local` sink (B2, 10.5 person-days)

| ID | Task | Files | Acceptance | d |
|---|---|---|---|---|
| B2-01 | ffmpeg single decode source | `internal/player/pipeline/decoder.go` | `s16le/44100/2ch` output; exit code and stderr tail surfaced in the error; cancellation kills the child process with no leak; path from `ffmpeg_path` | 3 |
| B2-02 | Preload and gapless continuation | `internal/player/pipeline/decoder.go` | The next track continues on the same timeline (no Timeline rebuild); fake sink asserts `rtp` timestamps are continuous across tracks | 1.5 |
| B2-03 | Sink interfaces + fake sink | `internal/player/sink/sink.go`, `sink/fake/` | `Sink` / `PushSink` / `PullSink`; the fake sink records `(sample_index, play_at, bytes)` per frame for assertions | 1 |
| B2-04 | `local` sink | `internal/player/sink/local/` | WS delivers `load{play_at}` and `play/pause/seek`; client reports `{position,buffering,volume,state}` every 500ms; clock offset estimated by halving RTT | 2.5 |
| B2-05 | Event wiring | `internal/player/manager.go`, `ws/hub.go` | `state` / `member_changed` / `error` flow from the state machine all the way to WS; after reconnect the client can realign via a full `/playback` fetch | 1 |
| B2-06 | Pipeline integration test | `internal/player/pipeline/pipeline_test.go` | Fake sink asserts: 20ms framing (882 samples), monotonic timestamps, underrun fills silence without dragging the timeline, clean under `-race` | 1.5 |

**Exit criteria (v0.1)**: `make run` plus a Flutter client (a debug screen is enough) can be driven by the server to play, pause, seek, and change tracks.

### M3 RAOP (B3, 12.5 person-days) → gate G1

| ID | Task | Files | Acceptance | d |
|---|---|---|---|---|
| B3-01 | mDNS discovery of `_raop._tcp` | `internal/player/sink/raop/discover.go` | Use `github.com/hashicorp/mdns v1.0.7` (still maintained as of 2026-06; `grandcat/zeroconf` has been stale since 2020 and has no go.mod, so it is not chosen); parse TXT (`ch/cn/et/sr/ss/pw/pk/tp`) and `port`; mark `pw=true` devices as password-protected and skip them | 1.5 |
| B3-02 | RTSP client | `internal/player/sink/raop/rtsp.go` | `OPTIONS/ANNOUNCE/SETUP/RECORD/FLUSH/TEARDOWN/SET_PARAMETER`; golden-bytes tests; timeouts and reconnect | 2 |
| B3-03 | SDP + crypto | `internal/player/sink/raop/crypto.go`, `sdp.go` | RSA-OAEP-SHA1 wrapping of the AES key, AES-128-CBC encryption; **known-answer tests** (byte-for-byte comparison with fixed key/iv); complete SDP fields | 2 |
| B3-04 | RTP packetization | `internal/player/sink/raop/rtp.go` | 12-byte header, payload type `0x60`, incrementing seq/ts, one packet per 20ms; sync packet `0x54` (rtp↔ntp mapping); golden bytes | 1.5 |
| B3-05 | Timing and latency | `internal/player/sink/raop/sink.go` | Start time aligned to the RTP timestamp; 2s default latency; monotonic conversion via `clock.go` | 1 |
| B3-06 | Volume and progress | `internal/player/sink/raop/sink.go` | `SET_PARAMETER volume` / `progress`; volume changes do not interrupt the stream | 0.5 |
| B3-07 | **G1 audio-out verification** | `scripts/tests/` (new script) | In-container `shairport-sync --output pipe` captures the stream to a file; `ffprobe` checks duration/sample rate; waveform similarity against the source | 1 |
| B3-08 | Real-device compatibility matrix | `docs/playback_compat_matrix.md` (new, bilingual) | ≥4 device classes tested (AirPort Express / Sonos / router-hosted shairport-sync / soundbar), recording whether L16 is accepted, whether volume works, whether seek is accurate | 3 (incl. waiting for devices) |

**Exit criteria (v0.2)**: 30 minutes of continuous playback on real hardware without dropouts or clicks; accurate position after seek.

### M4 DLNA (B4, 10 person-days)

| ID | Task | Files | Acceptance | d |
|---|---|---|---|---|
| B4-01 | SSDP discovery | `internal/player/sink/dlna/ssdp.go` | `M-SEARCH urn:schemas-upnp-org:device:MediaRenderer:1`; send per NIC; `MX` and retransmission; injectable transport for unit tests (no real multicast) | 1.5 |
| B4-02 | desc.xml parsing | `internal/player/sink/dlna/desc.go` | Extract `controlURL` + `eventSubURL` for `AVTransport` / `RenderingControl` / `ConnectionManager`; resolve relative URLs against `LOCATION` | 1 |
| B4-03 | SOAP client | `internal/player/sink/dlna/soap.go` | `SetAVTransportURI/Play/Pause/Stop/Seek/SetVolume/GetPositionInfo/GetTransportInfo/SetNextAVTransportURI`; golden tests for the `SOAPAction` header and body; **must use a bare `http.Client` (not the proxy-aware client)** | 2 |
| B4-04 | DIDL-Lite + stream URL | `internal/player/sink/dlna/didl.go` | Metadata with title/artist/album/`protocolInfo`; stream URL comes from B0-05 | 0.5 |
| B4-05 | Event subscription + polling fallback | `internal/player/sink/dlna/subscribe.go` | Use `SUBSCRIBE` events when available, otherwise fall back to 3s polling; subscription renewal | 1.5 |
| B4-06 | Fake renderer integration test | `internal/player/sink/dlna/soap_test.go` | `httptest` fake device asserting the call sequence (URI → Play → status query); error mapping when the device returns 500 | 1.5 |
| B4-07 | Vendor dialect patches | `internal/player/sink/dlna/vendor.go` | Register patches by `modelName` (tolerating empty `CurrentURIMetaData`, `Seek` units, missing event subscription); default when unmatched | 1.5 |
| B4-08 | Manual device entry | `internal/handlers/playback.go` | `POST /players` accepts `{address, kind}` for direct connection; address must pass `player_output_allowlist` | 0.5 |

**Exit criteria (v0.3)**: ≥3 real devices can pull and play, pause/seek/volume work; manual entry works when multicast is unavailable.

### M5 Sync groups (B5, 8.5 person-days) → gate G2

| ID | Task | Files | Acceptance | d |
|---|---|---|---|---|
| B5-01 | Group master/follower and dynamic members | `internal/player/group.go` | A member joining mid-playback starts from **the current point** without interrupting existing members; removal supports both `continue` and `stop` | 2 |
| B5-02 | SyncTier determination | `internal/player/group.go` | Any DLNA member makes the whole group `aligned` and returns `sync_tier`; **reject** adding DLNA to an existing exact group (400 with a clear message) | 0.5 |
| B5-03 | Capability negotiation | `internal/player/caps.go` | Sample rate is the minimum across the group; codec fallback `l16→aac→mp3`; upmix to 2ch | 1 |
| B5-04 | Delay compensation and per-member volume | `internal/player/group.go` | `delay_ms` takes effect (fake sink asserts frame offset); rules for combining `volume_offset` with device volume are explicit | 1 |
| B5-05 | Transfer and split | `internal/player/transfer.go` | `POST /groups/{id}/transfer` supports `only_player_id` for splitting; the target starts at `position + 800ms` and **compensation happens exactly once** | 1.5 |
| B5-06 | **Sync accuracy test** | `scripts/tests/` (new) | Two shairport-sync instances pipe to separate files; cross-correlate to find the peak delay and assert `< 20ms`; no drift over 10 minutes | 2 |
| B5-07 | Master switching | `internal/player/group.go` | Switching the master does not interrupt audio (same timeline, only the holder changes); not allowed for single-member groups | 0.5 |

**Exit criteria (v0.4)**: two identical AirPlay speakers in one group sound simultaneous to the ear; automated cross-correlation < 20ms.

### M6 Per-speaker EQ (B6, 6 person-days) → gate G3

| ID | Task | Files | Acceptance | d |
|---|---|---|---|---|
| B6-01 | RBJ peaking biquad cascade | `internal/player/pipeline/biquad.go` | 10 bands (31/62/125/250/500/1k/2k/4k/8k/16k Hz), Q=1.41; white-noise/sweep FFT asserts per-band gain error **< 0.2dB**; **sample count preserved** (N in, N out) | 2 |
| B6-02 | Preamp and clipping protection | `internal/player/pipeline/biquad.go` | `preamp = -max(0, max(band))`; peak ≤ 0dBFS end to end with all bands at +12dB; API returns the actual preamp | 0.5 |
| B6-03 | Per-sink instances + live switching | `internal/player/pipeline/` | EQ state isolated per path; live parameter changes use a 20ms linear ramp with no waveform step (assert sample delta ≤ 0.1); hard reset only when `idle` | 1 |
| B6-04 | Forced decoding rule + budget | `internal/player/budget.go`, `group.go` | Any non-flat EQ in the group forces server-side decoding and consumes a slot; full budget returns 503 with codes distinguishing "too many concurrent playbacks" from "EQ requires decoding, no slots left" | 1 |
| B6-05 | EQ REST + preset reuse | `internal/handlers/playback.go`, `internal/services/equalizer_preset.go` | `GET/PUT /players/{id}/eq`, `GET /players/{id}/eq/presets`; presets share **one constant set** with `/settings/equalizer` | 1 |
| B6-06 | `local` sink consistency | `internal/player/sink/local/` | EQ is applied server-side when the local device is in a group; a field tells the client to disable its local EQ and show "EQ applied by server" | 0.5 |

**Exit criteria (v0.5)**: +12dB@1kHz on a single speaker is audible with no clipping; other speakers in the group are unaffected; switching EQ produces no pop.

### M7 Party (B7, 7.5 person-days) → gate G4

| ID | Task | Files | Acceptance | d |
|---|---|---|---|---|
| B7-01 | Party core | `internal/player/party.go` | `leader_group_id` advances the logical position; `owner_kind='party'` reuses the same queue/state-machine code (no second implementation); no master volume | 2 |
| B7-02 | Join / leave | `internal/player/party.go` | Joining starts from the current logical position; leaving supports `continue` (own queue) and `stop`; one group leaving never disturbs the rest | 1 |
| B7-03 | Drift detection and correction | `internal/player/party.go` | Exceeding `party_drift_tolerance_ms` (default 1500) triggers correction and broadcasts `group_drift{corrected}`; after 3 consecutive failures stop retrying and set `unaligned` | 1.5 |
| B7-04 | Party transport broadcast + budget | `internal/player/party.go` | `POST /playback/party/transport` broadcasts to all groups; **each group consumes its own decode slot**, so a 4-group party exhausts the budget and is rejected explicitly | 1 |
| B7-05 | `party_enabled=false` rejection | `internal/handlers/playback.go` | Starting a party returns 403 with a readable reason; groups already in a party are unaffected | 0.5 |
| B7-06 | Party tests | `internal/player/party_test.go` | Fake sink records each group's first-sample logical position, asserting 3 groups within 1.5s; slowing one group by 2s produces `corrected:true` and realignment within 5s | 1.5 |

**Exit criteria (v1.0 feature surface)**: three groups play together, positions within 1.5s, independent volume, and any group leaving leaves the rest uninterrupted.

### M8 Resilience and observability (B8, 3 person-days)

| ID | Task | Files | Acceptance | d |
|---|---|---|---|---|
| B8-01 | Restart recovery | `internal/player/snapshot.go`, `manager.go` | After restart, resume the previous track and position (< 2s error); devices reconnect automatically | 1 |
| B8-02 | Offline / online | `internal/player/manager.go` | A member going offline only sets `member_offline`; group state is unchanged; on reconnect it rejoins and starts from the current point | 1 |
| B8-03 | Metrics and logging | `internal/player/metrics.go` | `player_underruns_total`, `player_decode_slots_used`, `player_drift_ms`; hot paths log `player_id`/`group_id` | 1 |

### X Cross-cutting and engineering wrap-up (not a milestone, 6.5 person-days, runs throughout)

These are not tied to a milestone but **must be completed within the relevant milestone**; deferring them all to the end turns them into one big patch.

| ID | Task | Files | Acceptance | Belongs to | d |
|---|---|---|---|---|---|
| X-01 | Full swagger verification | `docs/swagger.json`, `docs/swagger.yaml`, `docs/docs.go` | Every new endpoint found by `grep`; no `/api/v1` in `@Router`; seven annotations per handler | Every milestone | 1 |
| X-02 | Player egress allowlist | `internal/services/whitelist.go` (new independent entry), `internal/handlers/player_setting.go` | `player_output_allowlist` and the proxy allowlist **do not affect each other** (bidirectional cases: enabling only A still blocks B); rejections are audit-logged | Before M3 | 1.5 |
| X-03 | Docker networking and discovery fallback | `docs/en/faq.md`, `docs/en/architecture.md`, frontend empty state | Docs state `--network host` / macvlan; empty discovery shows a hint instead of a blank panel; manual IP entry works | M3/M4 | 1 |
| X-04 | Bundle mode restrictions | `docs/en/architecture_frontend.md`, `internal/app/app.go` (startup detection) | iOS Bundle keeps only the `local` sink by default; desktop Bundle requires explicit opt-in; APIs return an explicit reason when disabled | M2 | 1 |
| X-05 | Performance measurement | `scripts/tests/` (new benchmark) | CPU / memory baseline with 4 concurrent decode groups; the "4 groups run smoothly on a single-core device" claim is backed by data | After M7 | 1 |
| X-06 | Documentation backfill | `docs/en/architecture_playback*.md`, `docs/en/pitfalls.md`, `docs/en/playback_compat_matrix.md` | Design docs change from "not implemented" to "implemented (version)"; gotchas recorded per `LESSONS.md`; bilingual sync | v1.0 | 1 |

## 5. Frontend task breakdown (separate repo)

The frontend lives in `clients/player` (a separate repo; the submodule is not checked out here). Landing points are in [interaction design §7.1](./architecture_playback_ui.md). **Prerequisite: D1 must be decided** (which repo to fork), otherwise these tasks have nowhere to be committed.

| ID | Task | Main files | Acceptance | Depends | d |
|---|---|---|---|---|---|
| F0-01 | `output` module skeleton + API/repository | `lib/features/output/data/*` | Stubbed `/playback/targets` works end to end; unified error mapping | — | 2 |
| F0-02 | WS client (ticket / backoff / seq dedup / polling fallback) | `data/output_socket.dart` | After 3 failures fall back to 3s polling and show "live state unavailable"; realign with a full `/playback` fetch on reconnect | B0-06 | 2 |
| F0-03 | Output button + selector sheet | `presentation/widgets/output_button.dart`, `output_sheet.dart` | Empty state has guidance + scan button; offline targets greyed but not hidden; order "online → same network → name" | F0-01 | 2.5 |
| F0-04 | Speaker management page | `features/settings/presentation/speaker_settings_page.dart` | Discover/rename/delete/delay compensation/output settings all present; empty discovery shows the Docker networking hint | B0-07 | 2 |
| F1-01 | Local player dual-mode takeover | `presentation/providers/playback_sync_provider.dart` | With `isLocal`, drives `just_audio` with zero latency and reports position every 1s; silent otherwise | B2-04 | 2 |
| F1-02 | Takeover preference | `speaker_settings_provider.dart` | `pause`/`follow` switchable; on takeover, pause or silently degrade per preference; uses cached value when offline | F1-01 | 1 |
| F2-01 | Dynamic member add/remove + volume | `widgets/group_member_list.dart` | Mid-playback join does not interrupt audio; removal continues independently; optimistic updates roll back on failure | B5-01 | 2 |
| F2-02 | Sync badge | `widgets/sync_badge.dart` | `aligned` shown in amber with icon + semantic label (not color alone); the client blocks adding DLNA to an exact group first | B5-02 | 1 |
| F3-01 | Transfer / split / master switch | `use_cases/transfer_playback.dart`, `group_edit.dart` | Click marks "switching" immediately; audio within 3s; position error < 2s; rollback with a message on failure | B5-05 | 2 |
| F3-02 | Delay compensation UI | `widgets/group_member_list.dart` | Per-member ±ms adjustment, range matching backend validation | B5-04 | 1 |
| F4-01 | Per-speaker EQ page | `features/settings/presentation/speaker_eq_page.dart`, `widgets/eq_sheet.dart` | Reuses the `equalizer_panel.dart` fader visuals; 300ms debounce; read-only preamp display | B6-05 | 2 |
| F4-02 | Local EQ double-application guard | `speaker_eq_page.dart` | When the local device is the output target, the local EQ is disabled with "EQ applied by server" | B6-06 | 1 |
| F5-01 | Party create/join/leave | `widgets/party_section.dart`, `use_cases/party_edit.dart` | Tapping another target during a party joins it, undoable for 5s; no master volume control | B7-02 | 2 |
| F5-02 | Drift indicator | `widgets/drift_indicator.dart` | Two states: "aligning…" → "unaligned"; no repeated toasts on repeated failures | B7-03 | 1 |
| F6-01 | TV / Lynx adaptation + drag reorder | `features/output/**` | Every action reachable by D-pad; drag reorder degrades to buttons on TV | F2 | 3 |

Frontend total ≈ 25 person-days. F4 and F5 are independent and can run in parallel.

## 6. Gates

When a gate fails, **do not change the acceptance criteria**; change scope or technology. Each gate records the fallback up front so the decision is not improvised later.

| Gate | Method | Pass criteria | Fallback | Decision point |
|---|---|---|---|---|
| **G1** L16 compatibility | B3-07 automated audio-out + B3-08 real-device matrix | ≥4 device classes accept L16; volume/seek work | Try AAC-ADTS first (compatibility bought with lossy audio); if that still fails, mark RAOP experimental and drop it from v0.2, pulling DLNA forward | End of M3 |
| **G2** Sync accuracy | B5-06 two-instance cross-correlation | Peak delay < 20ms with no drift over 10 minutes | If only some devices fail → per-device default `delay_ms` compensation; if all fail → position becomes `SyncAligned` ("approximate") and product wording follows | End of M5 |
| **G3** EQ quality | B6-01/02/03 assertions | Frequency response error < 0.2dB; no sample > 0dBFS; no step on live switch | Live switch fails → only allow EQ edits when `idle` (UI disables editing during playback); clipping fails → add 1dB preamp headroom and retest | End of M6 |
| **G4** Party alignment | B7-06 fake sink | 3 groups < 1.5s; leaving does not disturb others | If unreachable → party degrades to "same song in multiple groups, independent progress" with the UI stating it is not synced | End of M7 |

## 7. Verification matrix

Anything automatable lives in CI; only real-device items are manual.

| Milestone | Automated | Manual |
|---|---|---|
| M0 | Clock/timeline/ring-buffer/budget units; ticket auth; `-race` | — |
| M1 | Table-driven queue, all state-machine paths, restart recovery | — |
| M2 | Pipeline integration (fake sink asserting frames/timestamps/silence fill) | Client integration listening test |
| M3 | SDP/RTP/crypto golden bytes; in-container shairport-sync audio-out + waveform comparison | **Real devices ≥4 classes** (G1) |
| M4 | SOAP call-sequence assertions against a fake renderer | ≥3 real devices |
| M5 | Two-instance cross-correlation < 20ms (G2) | Listening test in one group |
| M6 | FFT response, clipping, step assertions (G3) | Listening (pops, volume change) |
| M7 | Fake-sink logical position delta < 1.5s, correction convergence (G4) | Three-room test |
| M8 | Restart recovery, offline reconnect | Power-pull / network-cut drill |

The only real-hardware stand-ins available in CI are shairport-sync (Linux container) and a fake DLNA renderer — so real-device conclusions for DLNA and AirPlay must land in `docs/playback_compat_matrix.md`, not just in an issue comment.

## 8. Schedule

One full-time developer, including verification. Weeks count from kickoff, not tied to calendar dates.

| Week | Backend | Frontend | Verification / other |
|---|---|---|---|
| W1–W2 | M0 foundations | — | — |
| W3–W4 | M1 queue and transport | F0-01 skeleton (stubbed) | — |
| W5–W6 | M2 decode + local sink → **v0.1** | F0-02/03 selector | Client integration |
| W7–W9 | M3 RAOP → **G1** → **v0.2** | F0-04 speaker management | Real-device matrix (needs hardware) |
| W10–W12 | M4 DLNA → **v0.3** | F1 dual-mode takeover | Real devices ≥3 |
| W13–W14 | M5 sync groups → **G2** → **v0.4** | F2 + F3 | Sync accuracy test |
| W15 | M6 EQ → **G3** → **v0.5** | F4 EQ | EQ listening test |
| W16–W17 | M7 party → **G4** | F5 party | Three-room test |
| W18 | M8 resilience + X wrap-up → **v1.0** | F6 TV/Lynx | Full regression |

This matches design §9 (P0~P3.5 ≈ 3.75 months). If blocked waiting for hardware, push forward on M6 / M7 (testable with fake sinks) rather than idling.

## 9. Commit and collaboration conventions

- Commit directly to `main`, one task per commit, with the task ID: `feat(player): queue semantics and transport state machine (B1-02)`.
- Migration files, `make sqlc` output, and `make swagger` output go into the **same commit** as the code.
- Every handler gets the seven swag annotations as it lands (`@Summary/@Description/@Tags/@Produce/@Success/@Security/@Router`); `@Router` never includes `/api/v1`.
- New or changed docs are mirrored in English; new docs go into both `docs/` and `docs/en/`.
- Before committing: `gofmt -w .`, `go vet ./...`, `make check`; frontend `dart format lib/ test/`.
- Anything not run is reported as `Not run`, never omitted.

## 10. Open decisions

| ID | Question | Recommendation | Deadline |
|---|---|---|---|
| **D1** | Which repo receives the frontend code | Fork `songloft-player` as `leonzy/roomtone-player` and point the `clients/player` submodule at it | Before F0-01 |
| **D2** | mDNS library choice | `github.com/hashicorp/mdns v1.0.7` (maintained as of 2026-06); `grandcat/zeroconf` has been stale for six years. If no new dependency is acceptable, budget +2d for a hand-written query | Before B3-01 |
| **D3** | After G1 fails: AAC-ADTS or drop RAOP | Depends on the matrix: if ≥3 classes work, keep L16 with AAC as per-device fallback; below that, reassess | On G1 day |
| **D4** | Can a party member change the song individually | Not allowed today (shared content). If real usage demands it, implement "leave the party, then change" rather than decoupling content | After v1.0 feedback |
| **D5** | REW / AutoEQ import for EQ | Defer until someone actually measures a room with a microphone | After v1.0 |
| **D6** | Real-device test list | Needs the user's actual device models (some AirPlay, some DLNA); without them B3-08 and M4 manual acceptance cannot be scheduled | Before M3 |

## 11. Progress

Task detail lives in §4 / §5; only milestones are tracked here so the two never disagree.

- [ ] M0 foundations (B0-01 ~ B0-09)
- [ ] M1 queue and transport (B1-01 ~ B1-06)
- [ ] M2 decode and local sink (B2-01 ~ B2-06) → v0.1
- [ ] M3 RAOP (B3-01 ~ B3-08) → G1 → v0.2
- [ ] M4 DLNA (B4-01 ~ B4-08) → v0.3
- [ ] M5 sync groups (B5-01 ~ B5-07) → G2 → v0.4
- [ ] M6 per-speaker EQ (B6-01 ~ B6-06) → G3 → v0.5
- [ ] M7 party (B7-01 ~ B7-06) → G4
- [ ] M8 resilience and observability (B8-01 ~ B8-03) → v1.0
- [ ] Frontend F0 ~ F6
- [ ] X wrap-up: full swagger, `player_output_allowlist`, Docker networking docs, Bundle restrictions, performance measurement, design-doc status update

## 12. Risks and buffers

The risk table in design §10 still applies; only the ones that **change the shape of the plan** are listed here.

| Risk | Impact on plan | Buffer |
|---|---|---|
| G1 fails (L16 not accepted) | M3 grows by ~2 weeks (AAC path rework + retest matrix) | One spare week in W7–W9 |
| Hardware not available (D6) | B3-08 / M4 manual acceptance stalls | Prioritize the fake-sink portions of M6/M7 |
| Single-source underrun clicks | M2 rework affecting every later milestone | Do B0-04 buffering and silence fill thoroughly in M0 |
| Party drift heard as echo | M7 degraded, product positioning shifts | Fallback already defined in gate G4 |
| Frontend repo decision delayed (D1) | All F tasks blocked; backend proceeds | D1 due before F0-01 |
