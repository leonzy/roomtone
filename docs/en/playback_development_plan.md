# Playback Control and Speaker Output · Development Plan

> **Status: executable plan v2 (2026-10-25, post-audit revision).** For design and decisions see [Playback Control and Speaker Output Architecture](./architecture_playback.md) (backend) and [Playback and Speaker Interaction Design](./architecture_playback_ui.md) (frontend); for the capability comparison against SlimServer and the acceptance verdict see the [playback capability matrix](./playback_compat_matrix.md). This document does not repeat the reasoning; it answers three things: **in what order, which files, and what counts as done**.
>
> Task granularity = one independently committable, independently verifiable change. Task IDs go into commit messages (`feat(player): queue semantics and transport state machine (B1-02)`), so implementation and traceability map one to one.

## 0. How to use this plan

- Progress is tracked in the §11 progress table; tick an item when done. The task tables carry no status column, so the two can never disagree.
- Gates (§6) block the next milestone. Cut scope instead of lowering acceptance criteria (for example, push DLNA to v0.4) — once something like sync accuracy or clipping tolerance is relaxed, the selling point of the feature is gone.
- Estimates are **person-days for one full-time developer, including self-testing and docs**, excluding time waiting for real hardware (called out separately).
- Run a full `make check` at the end of each week; do not change acceptance criteria mid-week.

## 0.1 v2 revision: audit findings (2026-10-25)

v1 did not fail by missing tasks; it failed by **writing acceptance criteria as prose**. B5-04 said "`delay_ms` takes effect", B6-03 said "per-sink instances", B7-03 said "triggers correction" — and each degraded into "the field exists", "one copy per group", "an event is emitted". `make check` stayed green while the feature was half-built.

v2 does exactly two things:

1. **Acceptance criteria become executable assertions** — fake-sink frame offsets, FFT error, cross-correlation peaks, HTTP status codes, `rg` finding no leftovers. A task that cannot state an assertion does not start; a task whose assertion fails is not done.
2. **Close the 9 gaps the audit found**, each with its own task ID, none folded into another task as an afterthought.

| Audit finding (evidence) | Task | v2 handling |
|---|---|---|
| `AddMember` has no tier check; DLNA can join an exact group (`manager.go:519`) | B5-02 | Acceptance adds an "HTTP 400 + `detail` text" assertion |
| Mid-playback joins wait for the next track (`engine.go:499` captures the sink slice) | B5-01 | Split out **B5-09** hot-swap; B5-01 acceptance becomes "audible within 3 frames" |
| No drift-correction loop; only detection exists (`party.go:296`) | — | New **B5-08** |
| No `delay_ms` / `volume_offset` fields | B5-04 | Kept; acceptance becomes "frame offset error ≤1 frame" |
| Transfer has no +800ms compensation (`manager.go:615`) | B5-05 | Acceptance becomes "first-frame position delta ∈ [780,820]ms" |
| Sync accuracy script does not exist | B5-06 | Kept; location fixed as `scripts/tests/sync_correlation.py` |
| No group volume, no sync power | — | New **B5-10**, **B8-05** |
| Decoder and sinks reopened per track; no prefetch | — | New **B5-11** |
| EQ runs through ffmpeg `-af` and only the master's profile (`pipeline/eq.go:22`, `manager.go:724`) | B6-01/03/06 | Rewritten acceptance + new **B6-07** to delete the old path |
| No `GET /players/{id}/eq/presets` | B6-05 | Add the endpoint and a constant-reuse assertion |
| Party only detects, never corrects; no `party_test.go` | B7-03/06 | B7-03 becomes detection only; new **B7-07** closes the loop |
| No `metrics.go` | B8-03 | Kept |
| No `DELETE /players` or `DELETE /groups`, no master-switch endpoint | — | New **B8-04** |
| No power (standby/wake) | — | New **B8-05** |
| No CI guard; half-built features pass `make check` | — | New **X-07** |

**Every M5 / M6 / M7 task is re-run against the v2 criteria.** Results that already pass are kept; only the new assertions need re-running. Nothing is skipped because "it passed before".

## 1. Delivery scope

| Version | Milestones | What the user can perceive | Cumulative person-days |
|---|---|---|---|
| **v0.1 (internal)** | M0 + M1 + M2 | Server holds a queue and playback state, can drive client playback (`local` sink) | ~31 |
| **v0.2** | + M3 | **Audio actually comes out of an AirPlay speaker**, single device, no sync | ~44 |
| **v0.3** | + M4 | DLNA speakers added; single or multiple devices playing independently | ~54 |
| **v0.4** | + M5 | **Multi-room sync** (exact over RAOP, degraded with DLNA, including the drift-correction loop) | ~71.5 |
| **v0.5** | + M6 | Per-speaker EQ (server-side biquads, not filters) | ~78.5 |
| **v1.0** | + M7 + M8 + X | Party mode (with correction), resume after restart, observability, security and docs wrap-up | ~103 |

> **v2's definition of "done"**: every acceptance item must be an executable assertion. No assertion, no start; failing assertion, not done; "the field exists", "the event fires" and "it can be found in the code" do not count.

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
B5-01 ─► B5-09 ─► B5-08 ─► [G2]
B2-01 ─► B6-01 ─► B6-03 ─► [G3]
B5-01 ─► B7-01 ─► B7-03 ─► [G4]
```

Critical path: `B0-01 → B1-02 → B2-01 → B2-04 → B3-05 → G1 → B5-01 → B5-09 → B5-08 → G2 → B7-07 → G4`.

Frontend F0 can start before B1-05 (stub `/playback`). That is the only line that truly parallelizes without adding rework risk.

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

### M5 Sync groups (B5, 17.5 person-days) → gate G2

> This is where v1 accumulated the most half-built work. **"The field exists", "the group can be created" and "the event fires" never count as done**; every acceptance item below is stated as an executable assertion.

| ID | Task | Files | Acceptance | d |
|---|---|---|---|---|
| B5-01 | Group master/follower and dynamic members | `internal/player/manager.go`, `group.go` | A member joining mid-playback is audible **within 3 frames (≤70ms)** while existing members' `underruns` stay flat; removal supports both `continue` and `stop` without reopening the track | 2 |
| B5-02 | SyncTier determination and admission | `internal/player/manager.go`, `handlers/playback.go` | Adding a `dlna` member to a `raop/local` group returns **400** with `detail` containing "no exact sync"; a pure RAOP group is always `exact`; `sync_tier` updates live as members change | 0.5 |
| B5-03 | Capability negotiation | `internal/player/player.go` | Sample rate is the minimum across the group; codec fallback `l16→aac→mp3`; 2ch upmix (a mono source yields two identical channels) | 1 |
| B5-04 | Delay compensation and volume offset | `internal/player/group.go`, `sink/*` | `delay_ms` converts to a frame count applied as the sink start offset; fake sink asserts offset error **≤1 frame**; `volume_offset` combines with device volume (`clamp(base+offset,0,100)`) with lower/upper boundary cases | 1.5 |
| B5-05 | Transfer and split | `internal/player/transfer.go` | `only_player_id` split; target first-frame position minus source position **∈ [780,820]ms** (fake-sink assertion); compensation applied once, two consecutive transfers do not stack | 1.5 |
| B5-06 | **Sync accuracy test** | `scripts/tests/sync_correlation.py` (new) | Two shairport-sync instances pipe to separate files; cross-correlate to find the peak delay and assert `< 20ms`; no drift over 10 minutes; result recorded in `docs/playback_compat_matrix.md`. **Script and self-test done: `--selftest` injected 12ms and measured 12.0ms (correlation 1.000); `--window 2` segments show no drift; real dual instances Not run** | 2 |
| B5-07 | Master switching | `internal/player/manager.go` | Switching the master does not interrupt audio (same timeline, only the holder changes); single-member group returns 400 | 0.5 |
| B5-08 | **Sync correction loop** (new) | `internal/player/sync_loop.go` (new), `engine.go` | Every 950ms (`sync_check_interval_ms`) sample each member's actual position; reference = the **most-behind member that cannot skip frames**; deviation < `min_sync_adjust_ms` (10) is ignored, > 10s is abandoned; too-fast members `SkipFrames`, too-slow members get injected silence (**timeline unchanged, decoder never paused**). Fake clock injects ±50ms / ±2s and must converge to **≤10ms within one cycle**; residual drift ≤5ms over a 10-minute simulation; `-race` clean | 3 |
| B5-09 | **Sink hot-swap + per-sink writer** (new) | `pipeline/distribute.go`, `sink/sink.go` | `Sinks` becomes an atomic snapshot so `SetSinks` takes effect **on the next frame**; each sink gets its own bounded queue (drop-oldest with a counter); stalling one fake sink for 500ms must not raise other members' `underruns` | 2 |
| B5-10 | Group volume sync (new) | `manager.go`, `handlers/playback.go` | `POST /groups/{id}/volume` fans out with each member's `volume_offset`; `GET` returns the effective per-member value; partial failure returns `failed[]` without rolling back the successes | 1.5 |
| B5-11 | Gapless prefetch (new) | `engine.go`, `pipeline/`, `sink/dlna` | With ≤3s left, pre-open the next decoder and sinks; the junction has continuous sample numbering, no `underrun`, gap <5ms; DLNA uses `SetNextAVTransportURI` and falls back to a normal switch with a log line when unsupported | 2 |

**Exit criteria (v0.4)**: two identical AirPlay speakers in one group sound simultaneous to the ear; automated cross-correlation < 20ms; **artificially slowing one by 2s recovers within 5s**; adding a third speaker mid-playback is audible within 70ms without interrupting the first two.

### M6 Per-speaker EQ (B6, 7 person-days) → gate G3

> The current implementation builds an ffmpeg `-af equalizer=...` chain in `pipeline/eq.go`, and `manager.groupEQ` returns only the master's profile. **That is the wrong approach and must be removed**, not "optimised later": one ffmpeg per speaker means multiple decode sources and the sync foundation collapses. B6-07 deletes the old path.

| ID | Task | Files | Acceptance | d |
|---|---|---|---|---|
| B6-01 | RBJ peaking biquad cascade | `internal/player/pipeline/biquad.go` | 10 bands (31/62/125/250/500/1k/2k/4k/8k/16k Hz), Q=1.41; white-noise/sweep FFT asserts per-band gain error **< 0.2dB**; **sample count preserved** (N in, N out) | 2 |
| B6-02 | Preamp and clipping protection | `internal/player/pipeline/biquad.go` | `preamp = -max(0, max(band))`; peak ≤ 0dBFS end to end with all bands at +12dB; API returns the actual preamp | 0.5 |
| B6-03 | Per-sink instances + live switching | `internal/player/pipeline/` | EQ runs **after the Distributor and before encoding**, sample by sample, with isolated state per path; with +12dB on A only, B receives bit-identical samples (assert max diff = 0); live changes use a 20ms linear ramp with no waveform step (assert adjacent-sample delta ≤ 0.1); hard reset only when `idle` | 1 |
| B6-04 | Forced decoding rule + budget | `internal/player/budget.go`, `group.go` | Any non-flat EQ in the group forces server-side decoding and consumes a slot; full budget returns 503 with codes distinguishing "too many concurrent playbacks" from "EQ requires decoding, no slots left" | 1 |
| B6-05 | EQ REST + preset reuse | `internal/handlers/playback.go`, `internal/services/equalizer_preset.go` | `GET/PUT /players/{id}/eq`, `GET /players/{id}/eq/presets`; presets share **one constant set** with `/settings/equalizer` | 1 |
| B6-06 | `local` sink consistency | `internal/player/sink/local/` | EQ is applied server-side when the local device is in a group; a field tells the client to disable its local EQ and show "EQ applied by server" | 0.5 |
| B6-07 | **Delete the ffmpeg filter EQ path** (new) | `internal/player/pipeline/eq.go` (delete), `decoder.go` | `rg -e BuildFilterChain -e 'equalizer=f='` finds nothing under `internal/`; the decoder no longer accepts EQ parameters; with non-flat EQ the ffmpeg command line contains no `-af`; `/settings/equalizer` behaviour unchanged | 1 |

**Exit criteria (v0.5)**: +12dB@1kHz on a single speaker is audible with no clipping; other speakers in the group are **bit-for-bit unaffected**; switching EQ produces no pop; `equalizer` cannot be found in ffmpeg's arguments.

### M7 Party (B7, 9.5 person-days) → gate G4

| ID | Task | Files | Acceptance | d |
|---|---|---|---|---|
| B7-01 | Party core | `internal/player/party.go` | `leader_group_id` advances the logical position; `owner_kind='party'` reuses the same queue/state-machine code (no second implementation); no master volume | 2 |
| B7-02 | Join / leave | `internal/player/party.go` | Joining starts from the current logical position; leaving supports `continue` (own queue) and `stop`; one group leaving never disturbs the rest | 1 |
| B7-03 | Drift **detection** (split out) | `internal/player/party.go` | Compute the offset every 950ms; exceeding `party_drift_tolerance_ms` (default 1500) emits `group_drift{corrected:false, offset_ms}`; the correction itself is not part of this task (B7-07) | 1 |
| B7-04 | Party transport broadcast + budget | `internal/player/party.go` | `POST /playback/party/transport` broadcasts to all groups; **each group consumes its own decode slot**, so a 4-group party exhausts the budget and is rejected explicitly | 1 |
| B7-05 | `party_enabled=false` rejection | `internal/handlers/playback.go` | Starting a party returns 403 with a readable reason; groups already in a party are unaffected | 0.5 |
| B7-06 | Party tests | `internal/player/party_test.go` (**the file must exist and run in CI**) | Fake sink records each group's first-sample logical position, asserting 3 groups within 1.5s; after a group leaves, neighbouring `position` jitter stays < 100ms | 1.5 |
| B7-07 | **Party correction loop** (new) | `internal/player/party.go` | Reuses B5-08 sampling but only aligns **logical progress** (a lagging group seeks to the leader's position, a leading group waits); converges below 1.5s within 5s; after 3 consecutive failures set `unaligned`, stop retrying and broadcast once; a fake sink slowed by 2s must produce `corrected:true` with offset < 1500ms within 5s | 2 |

**Exit criteria (v1.0 feature surface)**: three groups play together, positions within 1.5s, independent volume, and any group leaving leaves the rest uninterrupted; **an artificially slowed group realigns by itself**; `party_test.go` runs in CI.

### M8 Resilience and observability (B8, 6.5 person-days)

| ID | Task | Files | Acceptance | d |
|---|---|---|---|---|
| B8-01 | Restart recovery | `internal/player/snapshot.go`, `manager.go` | After restart, resume the previous track and position (< 2s error); devices reconnect automatically | 1 |
| B8-02 | Offline / online | `internal/player/manager.go` | A member going offline only sets `member_offline`; group state is unchanged; on reconnect it rejoins and starts from the current point | 1 |
| B8-03 | Metrics and logging | `internal/player/metrics.go` | `player_underruns_total`, `player_decode_slots_used`, `player_drift_ms`, `player_sink_dropped_frames_total`; `GET /playback/metrics` returns a snapshot; hot paths log `player_id`/`group_id` | 1 |
| B8-04 | Device/group lifecycle endpoints (new) | `handlers/playback.go`, `manager.go` | `DELETE /players/{id}` (stop first, detach from every group, clean orphan groups), `DELETE /groups/{id}` (stop first), `POST /groups/{id}/master`; all three have swagger and unit tests, including "delete the last device" and "delete a playing group" | 2 |
| B8-05 | Player power / standby (new) | `manager.go`, `sink/raop`, `sink/dlna` | `POST /players/{id}/power`; DLNA sends `Stop` and clears the URI, RAOP stops sending RTP and is marked `standby` (RTSP has no standard standby command — never pretend otherwise); group-level `sync_power` defaults off; standby members are excluded from the sync loop | 1.5 |

### X Cross-cutting and engineering wrap-up (not a milestone, 8 person-days, runs throughout)

These are not tied to a milestone but **must be completed within the relevant milestone**; deferring them all to the end turns them into one big patch.

| ID | Task | Files | Acceptance | Belongs to | d |
|---|---|---|---|---|---|
| X-01 | Full swagger verification | `docs/swagger.json`, `docs/swagger.yaml`, `docs/docs.go` | Every new endpoint found by `grep`; no `/api/v1` in `@Router`; seven annotations per handler | Every milestone | 1 |
| X-02 | Player egress allowlist | `internal/services/whitelist.go` (new independent entry), `internal/handlers/player_setting.go` | `player_output_allowlist` and the proxy allowlist **do not affect each other** (bidirectional cases: enabling only A still blocks B); rejections are audit-logged | Before M3 | 1.5 |
| X-03 | Docker networking and discovery fallback | `docs/en/faq.md`, `docs/en/architecture.md`, frontend empty state | Docs state `--network host` / macvlan; empty discovery shows a hint instead of a blank panel; manual IP entry works | M3/M4 | 1 |
| X-04 | Bundle mode restrictions | `docs/en/architecture_frontend.md`, `internal/app/app.go` (startup detection) | iOS Bundle keeps only the `local` sink by default; desktop Bundle requires explicit opt-in; APIs return an explicit reason when disabled | M2 | 1 |
| X-05 | Performance measurement | `scripts/tests/decode_baseline.sh` (new) | CPU / memory baseline with 4 concurrent decode groups; the "4 groups run smoothly on a single-core device" claim is backed by data. **Done: 3 runs, wall 0.14–0.17s, peak RSS 6–29 MiB across 4 streams, 1085–1294× realtime** | After M7 | 1 |
| X-06 | Documentation backfill | `docs/en/architecture_playback*.md`, `docs/en/pitfalls.md`, `docs/en/playback_compat_matrix.md` | Design docs change from "not implemented" to "implemented (version)"; gotchas recorded per `LESSONS.md`; bilingual sync. **Done: the compatibility matrix landed (42 items, 37 ✅ / 4 ⚠️ / 0 ❌); ReplayGain, alarms and crossfade are all closed, crossfade implemented with the documented gapless trade-off** | v1.0 | 1 |
| X-07 | **Half-built guard (CI)** (new) | `Makefile` (`check-playback`), `scripts/tests/guards.sh` | One command runs: sync convergence, bit-exact EQ isolation, party realignment, `rg -e BuildFilterChain -e 'equalizer=f='` finds nothing, `party_test.go` exists, new endpoints appear in swagger; any failure turns `make check` red | From v0.4 onwards | 1.5 |

## 5. Frontend task breakdown (separate repo)

The frontend lives in `clients/player`; landing points are in [interaction design §7.1](./architecture_playback_ui.md). **Repo decided (D1)**: the submodule points at `github.com/leonzy/roomtone-player` (forked from `songloft-org/songloft-player`; same history, so the pinned submodule SHA resolves), and it is checked out locally.

| ID | Task | Main files | Acceptance | Depends | d |
|---|---|---|---|---|---|
| F0-01 | `output` module skeleton + API/repository | `lib/features/output/data/*` | Stubbed `/playback` works end to end; unified error mapping | — | 2 |
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
| F2-03 | Group volume and offsets (new) | `widgets/group_volume.dart` | Group slider sends immediately; per-member ±10 trim echoes back instantly; partial failures name the failing members | B5-10 | 1 |
| F3-03 | Delete device/group (new) | `use_cases/speaker_manage.dart` | Double confirmation; deleting a playing group warns "playback will stop"; failure rolls back without reordering the list | B8-04 | 1 |
| F6-02 | Standby / wake (new) | `widgets/power_toggle.dart` | Standby members are greyed out and cannot join an exact group; on wake they rejoin automatically and show "aligning" | B8-05 | 1 |

Frontend total ≈ 28 person-days. F4 and F5 are independent and can run in parallel.

## 6. Gates

When a gate fails, **do not change the acceptance criteria**; change scope or technology. Each gate records the fallback up front so the decision is not improvised later.

| Gate | Method | Pass criteria | Fallback | Decision point |
|---|---|---|---|---|
| **G1** L16 compatibility | B3-07 automated audio-out + B3-08 real-device matrix | ≥4 device classes accept L16; volume/seek work | Try AAC-ADTS first (compatibility bought with lossy audio); if that still fails, mark RAOP experimental and drop it from v0.2, pulling DLNA forward | End of M3 |
| **G2** Sync accuracy | B5-06 two-instance cross-correlation + B5-08 simulation + B5-09 hot-swap | ①Peak delay < 20ms with no drift over 10 minutes; ②slowing one member by 2s **recovers within 5s**; ③mid-playback join is **audible within 70ms** with no extra `underruns` elsewhere; ④with `delay_ms=50` the frame offset error is ≤1 frame | ①Fails → per-device `delay_ms`; only if all fail does the position become `SyncAligned`. ②③fail → **v0.4 must not ship**; "DLNA is never exact anyway" is not an excuse for a broken RAOP group | End of M5 |
| **G3** EQ quality | B6-01/02/03/07 assertions | Response error < 0.2dB; no sample > 0dBFS; no step on live switch; **members without EQ are bit-identical (max diff = 0)**; `rg 'equalizer=f='` finds nothing | Live switch fails → only allow EQ edits when `idle`. Bit-exact isolation fails → fall back to group-level EQ and **state "shared by the group" in the UI**, never pretend it is per-speaker | End of M6 |
| **G4** Party alignment | B7-06 + B7-07 fake sink | 3 groups < 1.5s; **a 2s slowdown realigns within 5s**; consecutive failures set `unaligned` and broadcast once; leaving does not disturb others | If unreachable → party degrades to "same song in multiple groups, independent progress" with the UI stating it is not synced | End of M7 |

## 7. Verification matrix

Anything automatable lives in CI; only real-device items are manual.

| Milestone | Automated | Manual |
|---|---|---|
| M0 | Clock/timeline/ring-buffer/budget units; ticket auth; `-race` | — |
| M1 | Table-driven queue, all state-machine paths, restart recovery | — |
| M2 | Pipeline integration (fake sink asserting frames/timestamps/silence fill) | Client integration listening test |
| M3 | SDP/RTP/crypto golden bytes; in-container shairport-sync audio-out + waveform comparison | **Real devices ≥4 classes** (G1) |
| M4 | SOAP call-sequence assertions against a fake renderer | ≥3 real devices |
| M5 | Two-instance cross-correlation < 20ms; fake-clock convergence simulation; hot-swap ≤70ms; `delay_ms` frame offset; `-race` (G2) | Listening test in one group |
| M6 | FFT response, clipping, step, **bit-exact per-member isolation**, `rg` finds no filter leftovers (G3) | Listening (pops, volume change) |
| M7 | Fake-sink logical position delta < 1.5s, **2s slowdown realignment**, `unaligned` broadcast once (G4) | Three-room test |
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
| W13–W15 | M5 sync groups (correction loop / hot-swap / prefetch) → **G2** → **v0.4** | F2 + F3 | Sync accuracy + convergence simulation |
| W16 | M6 EQ (biquad rewrite + delete the filter path) → **G3** → **v0.5** | F4 EQ | EQ listening test |
| W17–W18 | M7 party (detection + correction) → **G4** | F5 party | Three-room test |
| W19 | M8 resilience + X wrap-up → **v1.0** | F6 TV/Lynx | Full regression |

v2 is 4 weeks longer than v1 (~19 weeks ≈ 4.4 months). All of it is "finishing what was half-built": correction loop 3d, hot-swap and per-sink writers 2d, prefetch 2d, biquad rewrite and removing the old path 3d, party correction 2d, lifecycle and power 3.5d, CI guard 1.5d. If blocked waiting for hardware, push forward on the hardware-independent parts (M5-08/09, M6, M7 — all testable with fake sinks) rather than idling.

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
| **D1** | Which repo receives the frontend code | **Decided**: forked as `leonzy/roomtone-player`; both `.gitmodules` and the local checkout point at it | ✅ Done |
| **D2** | mDNS library choice | `github.com/hashicorp/mdns v1.0.7` (maintained as of 2026-06); `grandcat/zeroconf` has been stale for six years. If no new dependency is acceptable, budget +2d for a hand-written query | Before B3-01 |
| **D3** | After G1 fails: AAC-ADTS or drop RAOP | Depends on the matrix: if ≥3 classes work, keep L16 with AAC as per-device fallback; below that, reassess | On G1 day |
| **D4** | Can a party member change the song individually | Not allowed today (shared content). If real usage demands it, implement "leave the party, then change" rather than decoupling content | After v1.0 feedback |
| **D5** | REW / AutoEQ import for EQ | Defer until someone actually measures a room with a microphone | After v1.0 |
| **D6** | Real-device test list | **Partially decided**: both AirPlay and DLNA devices are available, enough for G1 and M4 manual acceptance; exact models get recorded in the compatibility matrix when B3-08 starts | Models recorded with B3-08 |

## 11. Progress

Task detail lives in §4 / §5; only milestones are tracked here so the two never disagree.

- [x] M0 foundations (B0-01 ~ B0-09) — done
- [x] M1 queue and transport (B1-01 ~ B1-06) — done
- [x] M2 decode and local sink (B2-01 ~ B2-06) → v0.1 — done; client integration Not run
- [x] M3 RAOP (B3-01 ~ B3-08) → G1 → v0.2 — code complete; **G1 real-device matrix Not run**
- [x] M4 DLNA (B4-01 ~ B4-08) → v0.3 — code complete; real devices Not run
- [x] M5 sync groups (B5-01 ~ B5-11) → G2 → v0.4 — code and scripts complete; **G2 real dual-device cross-correlation Not run** (script self-test passes, see X-06)
- [x] M6 per-speaker EQ (B6-01 ~ B6-07) → G3 → v0.5 — done; G3 real-device listening Not run
- [x] M7 party (B7-01 ~ B7-07) → G4 — done (`party_test.go` in CI); G4 real devices Not run
- [x] M8 resilience and observability (B8-01 ~ B8-05) → v1.0 — done (B8-04 lifecycle endpoints, B8-03 metrics, B8-05 power)
- [x] Frontend F0 ~ F6 — done (`clients/player`: F0 panel/standby, F1 group creation and member params, F2 transfer, F3 EQ, F4 party, F5 metrics, F6 standby greying); real-device and TV/Lynx walkthroughs Not run; member ordering uses up/down buttons (no drag on TV) against `PUT /groups/{id}/members/order`, and the "aligning" badge uses `member_caps[].syncing`
- [~] X wrap-up: X-02 allowlist / X-04 Bundle limits / X-05 performance baseline / X-06 documentation backfill (incl. the compatibility matrix) / X-07 guard done; **X-01/X-03 partial** (Swagger fully checked through this round's new endpoints; the Docker discovery fallback doc awaits real-device conclusions)

### 11.1 Next commit sequence (dependency order, one commit each)

1. **Close the sync loop**: `B5-09` (hot-swap + per-sink writers) → `B5-08` (correction loop) → `B5-04` (delay/offset) → `B5-02` (400 admission) → `B5-01` (70ms join) → `B5-05` (+800ms) → re-run `B5-06` and pass G2 once.
2. **EQ rewrite**: `B6-01` (biquad) → `B6-03` (per-sink instances + ramp) → `B6-07` (delete the filter path) → `B6-06` (local consistency) → `B6-05` (presets) → G3.
3. **Party loop**: `B7-03` (detection rewrite) → `B7-07` (correction) → `B7-06` (`party_test.go`) → G4.
4. **Lifecycle and resilience**: `B8-04` (DELETE/master) → `B8-03` (metrics) → `B8-05` (power) → `B5-11` (prefetch) → `B5-10` (group volume) → `B8-02` (offline event).
5. **Guard and wrap-up**: `X-07` (`make check-playback`) → `X-02` → `X-04` → `X-05` → `X-06`.
6. **Frontend**: `F0` → `F1` → `F2` → `F3` → `F4` → `F5` → `F6` (one F stage right after each milestone; never all at the end).

> Every step is only committed once the X-07 guard is green. If the guard fails, do not commit — do not hand the problem to the next batch.

## 12. Risks and buffers

The risk table in design §10 still applies; only the ones that **change the shape of the plan** are listed here.

| Risk | Impact on plan | Buffer |
|---|---|---|
| G1 fails (L16 not accepted) | M3 grows by ~2 weeks (AAC path rework + retest matrix) | One spare week in W7–W9 |
| Hardware not available (D6) | B3-08 / M4 manual acceptance stalls | Prioritize the fake-sink portions of M6/M7 |
| Single-source underrun clicks | M2 rework affecting every later milestone | Do B0-04 buffering and silence fill thoroughly in M0 |
| Party drift heard as echo | M7 degraded, product positioning shifts | Fallback already defined in gate G4 |
| ~~Frontend repo decision delayed (D1)~~ | Eliminated: `leonzy/roomtone-player` exists and the submodule points at it | — |
| **Acceptance written as prose → half-built features** | Already happened in v1: three tasks "done" with no closed loop, 4 weeks of rework | From v2 on, acceptance must be executable; X-07 enters CI; prose acceptance is rejected at self-review |
| Correction loop touches the engine core | Possible M2 regression | Do B5-09 first (isolated writers), then B5-08 as sample-and-inject only; never touch the decoder |
| Claiming "implemented" without hardware | Docs diverge from reality; expensive to debug later | Anything unverified on hardware is reported as Not run, never as done |
