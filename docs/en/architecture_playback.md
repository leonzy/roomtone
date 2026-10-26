# Playback Control and Speaker Output Architecture (Architecture and Implementation)

> **Status (after the 2026-10-25 acceptance).** This document describes how Songloft extends from a "client-authoritative media server" into one that owns playback state and pushes audio to speakers on the local network.
>
> Implementation progress: **M0–M8 are all implemented** (queue/state machine, decode pipeline, `local`/`raop`/`dlna` sinks, device discovery, sync groups and the correction loop, per-speaker EQ, party mode, observability). Per-item acceptance assertions live in the [development plan](./playback_development_plan.md); **real-device items (G1 matrix, G2 dual-device cross-correlation, G3 listening, G4 party) are all `Not run`**.
>
> For the item-by-item comparison against SlimServer (LMS) see the **[playback capability matrix](./playback_compat_matrix.md)** (42 items: 33 on par or better, 4 gaps, 4 unimplemented).
>
> **For the implementation path see the [development plan](./playback_development_plan.md)** (task breakdown, files touched, gates, and schedule).
>
> References: Lyrion Music Server (`../slimserver`, Perl) — `Slim/Player/Source.pm`, `Sync.pm`, `CapabilitiesHelper.pm`; shairport-sync (reverse reference for RAOP receivers); OwnTone (RAOP sender reference).

## 1. Goals and Non-Goals

### 1.1 Goals

| # | Goal | Notes |
|---|---|---|
| G1 | **Server-side queue and playback state** | Queue, current track, position, repeat/shuffle held by the server, consistent across clients |
| G2 | **Push to AirPlay speakers (AirPlay 1 / RAOP)** | Server pushes PCM to the speaker; no client involved |
| G3 | **Push to DLNA/UPnP speakers** | Server drives the renderer over SOAP; the device pulls the stream |
| G4 | **Multi-room sync** | Exact sync inside AirPlay groups; aligned start only for DLNA |
| G5 | **First-party client joins as a player** | Flutter client registers as a `local` sink and can share a group. ⏳ **not implemented on the client** (the server side is ready, see §5.2) |
| G6 | **Resume after restart** | Restore track and position after a server restart or device reconnect |
| G7 | **Party mode** | Multiple groups play the same track at once; each group keeps its own volume and can join/leave without affecting the others (§4.7) |
| G8 | **Per-speaker equalizer** | Independent 10-band EQ per speaker, applied inside the server-side PCM pipeline (§4.8) |

### 1.2 Non-Goals

- **AirPlay 2** (HomePod, Apple TV, and other AP2-only devices). Rationale in §2.
- **Chromecast / native Sonos / native TIDAL Connect.** Chromecast may become a separate sink later.
- **Video casting.** The existing HLS path (`/songs/{id}/stream?media=video`) is unchanged.
- **Plugin scripts controlling players.** The first version exposes REST/WS only; the plugin bridge is deferred (§8.5).
- **Sample-accurate sync across a party.** A party only guarantees "same track, same logical position, within 1.5s" (§4.7), never ±20ms across groups. Use a sync group when sample accuracy matters.

## 2. Protocol Reality Check

Settle feasibility up front to avoid rework.

| Protocol | Server push | Sync accuracy | Shared clock | Open-source sender status |
|---|---|---|---|---|
| **RAOP (AirPlay 1)** | ✅ RTP push | **±10–20 ms** | ✅ sync packets map `rtp ↔ ntp` | OwnTone `raop.c`; no usable Go library (3 repos, all stale/zero-star) |
| **AirPlay 2** | ✅ buffered stream | ±10 ms | ✅ PTP/NTP | **No mature open-source sender**; needs FairPlay SAP + HAP pairing + encrypted buffered audio |
| **DLNA / UPnP** | ⚠️ commands only, device pulls | **±200–500 ms, drifting** | ❌ no clock in the protocol | Mature control-plane libraries (`goupnp`); sync is unsolvable |

Conclusions:

1. **RAOP must be written in-house**, but the protocol is public and has a receiver implementation to mirror; scope is manageable (~3k lines of Go).
2. **AirPlay 2 is out of scope.** It requires reverse-engineered FairPlay handshakes and pairing, and there is no dependable sender implementation anywhere. AP2-only devices are explicitly marked "unsupported" in the UI.
3. **DLNA cannot be tightly synced.** This is a protocol ceiling, not an implementation-quality issue. It must be modelled explicitly via `SyncTier` and surfaced to users (§4.3).

> Additional fact: LMS itself has no native AirPlay/DLNA output either — its ecosystem relies on `airupnp`/`ShairTunes2W` bridges, and **those bridges have no sync at all**. This design goes beyond LMS on that point.

## 3. Overall Architecture

```
                     REST / WebSocket
   ┌─────────────────────────────────────────────────────────────┐
   │  internal/player                                            │
   │                                                             │
   │  Party ─► Queue ─► Transport ─► Timeline ─┬─► EQ ─► raop.Sink
   │  (party)   (queue)  (state machine)(clock) ├─► EQ ─► local.Sink
   │                                │          └─► EQ ─► dlna.Sink
   │                          Decoder(ffmpeg)
   │                          one decode · one PCM stream
   └─────────────────────────────────────────────────────────────┘
                                    │
              ┌─────────────────────┼─────────────────────┐
              ▼                     ▼                     ▼
        RTP/RTSP → AirPlay      WS cmds → client       SOAP → DLNA
```

Three responsibilities: **Party decides *what plays*** (content unit), **Group decides *who is exactly in step*** (sync unit), **EQ decides *how each device sounds*** (device unit). They never replace each other — see §4.7 / §4.8.

Package layout (mirrors the existing `internal/` layering: Service calls it, Handler only converts protocols):

```
internal/player/
  manager.go       player registry, discovery scheduling, state event bus
  player.go        Player entity and capability model
  group.go         sync group: master/slave, delay compensation, format negotiation
  queue.go         queue semantics (current / repeat / shuffle / jump / preload)
  transport.go     transport state machine
  timeline.go      unified timeline and frame fan-out
  party.go         party: shared queue and logical position across groups, drift correction
  budget.go        concurrent *decoding group* limit (independent of transcode slots)
  eq.go            per-speaker EQ config and preamp computation
  pipeline/
    decoder.go     ffmpeg → s16le single decode source + ring buffer
    resample.go    in-group sample-rate alignment (via ffmpeg -ar)
    biquad.go      RBJ peaking cascade (one instance per sink, sample-count preserving)
  sink/
    sink.go        Sink / PushSink / PullSink interfaces
    local/         WebSocket commands + client pull
    raop/          RTSP + RTP + AES + pacing
    dlna/          SSDP + SOAP + DIDL-Lite
  clock.go         monotonic ↔ NTP conversion (injectable for tests)
```

## 4. Core Mechanics

### 4.0 Zone semantics

The user-visible unit of playback is the **zone**; the backend calls it `group`. They map one to one, with no new entity:

| Concept | Backend entity | Notes |
|---|---|---|
| Zone | `groups` row | One or more output devices; a single-member group = a standalone zone |
| Output | `players` row | `local` / `raop` / `dlna` |
| Control focus | client-local state | "Which zone I am looking at"; **never sent to the server**, affects no playback |

Four hard constraints:

1. **Zones run in parallel.** Each zone has one engine (`engines["group:"+id]` in `manager.go`), each holding its own queue, transport state and timeline, and several can sound at once. `GET /playback` returns every zone, not "the current zone".
2. **Switching focus sends no command.** Switching zones on the client only changes the local `PlaybackFocus`; only inline transport, volume and explicit transfer send requests.
3. **Transfer is an explicit action.** `POST /groups/{to}/transfer` is never triggered implicitly by "tapping another zone" (the v2 frontend design did that; v3 changed it — see [interaction design §4.2](./architecture_playback_ui.md)).
4. **Parallelism has a ceiling.** Every playing zone holds one decode slot (default 4, `budget.go`); each group in a party holds one, and a group with non-flat EQ is forced to hold one. Starting a fifth zone returns 503 `ErrBudgetExhausted`, and the UI must say "simultaneous playback limit reached" rather than "transcoder busy". This is one of the hard gaps versus Roon (§11.1 H5) and cannot be removed by UI work.

**This Device is a built-in zone**: after startup the client must register itself as a `local` device over WS `register_player`, and the server creates a single-member group automatically. Until it registers, This Device appears in no list — that is the root cause of "the playback output page has no This Device" (see §5.2).

### 4.1 One decode source, many sinks (the foundation of sync)

**Never** start one ffmpeg per speaker: process-startup jitter plus differing buffer depths means hundreds of milliseconds of skew at t=0, and it never converges.

The correct approach (exactly LMS `Source.pm`'s `shift->master()` semantics: the master owns the single source, members share it):

```
one ffmpeg process → s16le/44100/2ch → ring buffer (3s) → 882 frames every 20ms
                                                        ├─ raop A: same bytes + same RTP timestamp
                                                        ├─ raop B: same bytes + same RTP timestamp
                                                        └─ local C: same bytes + same play_at
```

Sync then degenerates into "each sink plays according to its own timestamp"; accuracy depends only on the protocol's clock capability, not on Go-side scheduling.

**But that is only start alignment, not continuous sync.** Per-device buffer depth, Wi-Fi jitter and retransmits accumulate offset over time — ten minutes later two speakers are two distinct sounds. Sustaining ±20ms requires the correction loop in §4.3.1, which is why every mature multi-room system (including LMS) has one.

**Side benefit:** a single decode source gives **gapless playback for free** — the next track simply continues the same RTP timeline, with no device-side gapless support required. DLNA still needs `SetNextAVTransportURI`.

**The one exception is EQ**: every speaker has a different equalizer, so the bytes leaving the fan-out necessarily differ. EQ therefore sits **after fan-out and before encoding**, yet shares the same timeline because it preserves the sample count (§4.8).

### 4.2 Unified timeline

```go
type Timeline struct {
    t0         time.Time     // group start (monotonic base)
    sampleRate int           // unified sample rate for the group
    anchor     uint32        // RTP timestamp at start
    latency    time.Duration // default 2s (RAOP buffer window)
}

// absolute play time of the n-th sample frame
func (t *Timeline) At(n uint64) time.Time {
    return t.t0.Add(time.Duration(float64(n)/float64(t.sampleRate)*float64(time.Second)) + t.latency)
}
```

- Use the monotonic reading of `time.Now()`, **never the wall clock** (an NTP jump would tear the timeline apart).
- RAOP's NTP timestamps are derived from the same base in `clock.go`, keeping the `rtp ↔ ntp` mapping monotonic.
- The clock must be injectable (`type Clock interface{ Now() time.Time }`); otherwise timing logic cannot be tested deterministically.

### 4.3 Sync tiers (must be modelled explicitly)

```go
type SyncTier int

const (
    SyncExact   SyncTier = iota // RAOP / local: ±20ms
    SyncAligned                 // DLNA: aligned start only, drifts
)
```

Rules:

- All members `SyncExact` → exact sync.
- Any DLNA member → the whole group degrades to `SyncAligned`; the API returns `sync_tier` and the UI states "includes a DLNA device, sync accuracy reduced".
- **Never** add a DLNA member to an exact group without degrading it — that turns "acceptable" into "unlistenable" instantly.

Optional (default off): `dlna_realign_interval`, silently re-align every N minutes via stop→seek→play, at the cost of one audible gap.

### 4.3.1 Sync correction loop (new in v2, a prerequisite for `SyncExact`)

Aligned start only guarantees "started together", never "stays together". The correction loop is the equivalent of LMS `StreamingController.pm:483 _CheckSync`; parameters and actions map one to one:

| Parameter | Value | Meaning |
|---|---|---|
| `sync_check_interval_ms` | 950 | Sample every member's actual position each cycle |
| `min_sync_adjust_ms` | 10 | Below this, do nothing — avoids oscillating |
| `max_deviation_ms` | 10000 | Above this, treat the device as faulty and hand it to the offline flow (B8-02) |
| Reference device | the **most-behind member that cannot skip frames** | Chase the slowest, never the fastest: chasing the fastest means dropping large amounts of audio, which is audible |

Actions:

- **Too fast** (`pos > ref + eps`) → `SkipFrames(delta)`: RAOP drops the frames for the corresponding RTP timestamps (timestamps stay contiguous, no hole); `local` receives a `skip`.
- **Too slow** (`pos < ref − eps`) → inject silence to fill (**never pause the decoder, never move the timeline**); for DLNA this is equivalent to briefly slowing the pull rate.
- Position sources: RAOP = sent RTP timestamps minus protocol buffering; `local` = client 500ms reports; DLNA = bytes sent / bitrate + estimated buffer (imprecise, hence DLNA groups stay `SyncAligned`).

**Why not "stop the whole group and wait for the slowest"**: that makes the whole group stutter repeatedly; the loop makes the fast ones wait a little and the slow ones catch up a little, so the user hears a steady state.

**Why `delay_ms` is still needed per device**: different models have **constant** DSP/buffer offsets. The loop fixes drift, not constants (forcing it would leave that device permanently skipping). So each device gets a `delay_ms` (±500ms) applied once at start, and the loop only handles the residue.

**Audibility constraints**: one adjustment per member per cycle; adjustments above 200ms use silence fill instead of continuous skipping; every adjustment is recorded in `player_drift_ms` and `slog` so the culprit device can be identified later.

### 4.4 Queue and transport state machine

Queue semantics mirror LMS `Playlist.pm`:

- `currentIndex` + `repeat(all|one|off)` + `shuffle`
- `jump(index)`, `next/prev`, `insertNext`
- **Preload**: start decoding the next track when ≤30s remain (filling the ring buffer), and pre-open the sinks when ≤3s remain (RAOP continues the same RTP timeline; DLNA uses `SetNextAVTransportURI`), guaranteeing gapless

State machine:

```
idle ──load──► loading ──► buffering ──► playing ⇄ paused
                   │            │            │
                   └────────────┴────────────┴──► error ──recover──► idle
```

- The `buffering → playing` transition is gated on **ring-buffer water level** (default 2s), not "first frame arrived".
- `error` carries a readable reason (unreachable device / unsupported format / ffmpeg missing) and maps directly onto an API error.
- Group state = master state; a member going offline does not change group state, it only sets `member_offline`.

### 4.5 Capability negotiation and format unification

```go
type Caps struct {
    Codecs        []Codec // l16 / alac / aac / mp3 / wav / flac
    MaxSampleRate int
    Channels      int
    VolumeRange   [2]float64 // dB or 0-100, interpreted per kind
}
```

Negotiation rules (mirroring LMS `CapabilitiesHelper::samplerateLimit`):

1. Sample rate = **the minimum of all members' maximums**; adding a 48k-only device forces the group to 48k.
2. Codec degrades by compatibility: `l16 → aac → mp3` (lossless first, lossy as a fallback so audio still plays).
3. Channels unified to 2; mono sources are upmixed.
4. DSD / APE / M4A and friends are converted to PCM by ffmpeg during decode; Go never decodes audio itself.

### 4.6 Buffering and underrun

- Ring buffer target 2s (aligned with RAOP latency), floor 500ms.
- On underrun, **insert silence** rather than stalling: the RTP timeline must stay continuous, otherwise the whole group is dragged off.
- Expose `player_underruns_total` to diagnose slow disks / slow networks.

### 4.7 Party mode (many groups, one track)

**The problem:** a sync group answers "how do these devices stay exactly in step", but a group has exactly one queue and one transport state. What users actually want is "kitchen and living room play the same track, each with its own volume, and either can join or leave without disturbing the other" — that is sharing at the **content layer**, not merging at the **clock layer**.

**Conclusion: a party is not "a bigger group", it is a layer above groups.** A group stays the sync unit (±20ms); a party is the content unit (within 1.5s). This also explains why merging two groups is not an option: merging would let a DLNA member in the living room drag the kitchen's exact group down with it.

```go
type Party struct {
    ID       string
    Name     string
    Queue    *Queue          // shared queue, same semantics as a group queue
    Groups   map[string]*Group
    Leader   string          // earliest-joined online group; advances Position
    Position time.Duration   // logical position, driven by the leader's state
}
```

Rules:

- **Shared content, independent timelines**: each group still owns its decoder and output pipeline (format, volume and sync tier are all per group). A party only shares "which track, which second".
- **Alignment**: when `|group.position - party.position| > 1500ms`, send `seek` to that group; after 3 consecutive failed corrections mark it `out_of_sync`, surface it in the UI and suggest a sync group instead. No finer correction — a party tolerates ~1s of skew, and chasing tighter only adds audible breaks.
- **Control surface**: `play/pause/next/prev/seek` broadcast to the whole party; `volume/mute` always stay per group or per speaker. **There is no party master volume** (rooms differ acoustically; one shared level guarantees someone can't hear).
- **Queue ownership**: starting a party promotes the initiating group's queue to the party queue, while every group's original queue is left untouched (same as transfer — nothing is destroyed). Leaving a party returns that group to its own queue in `idle`.
- **Join / leave**: joining attaches at `party.position` (same semantics as adding a member mid-playback); leaving defaults to "split and continue" — a new group queue built from the party's current track and position.
- **Capacity**: each group costs one decoder, and the total party size is bounded by the same `player_max_groups` (default 4), see §8.4.
- **Ending**: `DELETE /playback/party` stops every group and returns each to its own queue; no queue is deleted.
- **Mutually exclusive with transfer**: during a party, `POST /groups/{id}/transfer` on a member group returns **409** with "this group is in a party, leave it first". The queue is shared by the party, and letting one group pull it out leaves the others with nothing to play — the semantics are undefinable. "Only this speaker" means leaving the party first.
- **Why not share one decoder**: groups in a party may need different formats (a 48k DLNA group plus a 44.1k RAOP group), and RAOP has a fixed 2s buffer that DLNA does not. Forcing them to share only drags both into one downgrade. One extra ffmpeg process buys "each group stays at its best".

### 4.8 Per-speaker equalizer (server-side DSP)

**Why it must be server-side**: the existing EQ (`/settings/equalizer`, 10 bands, -12..+12dB, implemented with mpv `af`) applies to the local client player. Once playback is pushed by the server, the client only receives encoded bytes and the local EQ does nothing — the speaker plays uncorrected audio.

**Why in Go rather than an ffmpeg filter**: ffmpeg's `equalizer` filter can only act on its own output, and one process carries one filter chain. Starting an ffmpeg per sink falls back to "many decode sources" and the sync foundation collapses (§4.1). EQ therefore sits **after fan-out and before encoding**, processed sample by sample in Go.

```
single decode ──► fan-out ──┬─► EQ(living room, +3dB@100Hz) ──► ALAC ──► RAOP
                            ├─► EQ(kitchen, flat)            ──► AAC  ──► RAOP
                            └─► EQ(study, -2dB@2kHz)        ──► WAV  ──► DLNA
```

**Why sync is unaffected**: a 10-band peaking-biquad cascade is a **sample-count-preserving** per-sample filter; it neither inserts nor drops samples. The same sample index maps to the same timestamp on every path, so "same bytes + same timestamp" becomes "same sample index + same timestamp" — the premise of sync (one timeline) is intact, only the amplitudes differ.

- **Filters**: RBJ peaking EQ on the existing 10 centre frequencies (31/62/125/250/500/1k/2k/4k/8k/16k Hz), Q fixed at 1.41; `preset` reuses the exact same names as `/settings/equalizer` (flat/rock/pop/jazz/classical/bass_boost/treble_boost/vocal/custom) so we never maintain two preset sets.
- **Pipeline order**: `decode → loudnorm(optional, group-level) → fan-out → EQ(per sink) → encode`. loudnorm normalizes content, EQ corrects a room, so the latter must come last; loudnorm's `TP=-1.5` also leaves headroom for EQ's preamp.
- **Group delay**: at the same sample rate the cascades have identical group delay (a few ms), so relative sync inside a group is unaffected; absolute latency folds into `default_latency_ms` with no separate compensation.
- **Clipping**: `+12dB` will clip. When a non-flat EQ is enabled the server applies `preamp = -max(0, max(band))` so peaks never exceed 0dBFS; the API must return the actual `preamp` and the UI must show it, otherwise users report "turning on EQ makes it quieter" as a bug.
- **Hot switching**: changing EQ mid-playback resets filter state and clicks. The approach is a 20ms linear ramp on preamp while filter state continues under the new coefficients; hard reset only allowed when `idle`.
- **Decoding is mandatory**: passthrough paths (no transcode, remote-URL direct) have no PCM. The rule is that **if any member of a group has a non-flat EQ, the group is forced onto the server decode pipeline**, honestly consuming one `player_max_groups` slot; when no slot is free return 503 with an error message that names EQ as the cause.
- **`local` sink uses server-side EQ too**: same as any other sink, so one group never mixes two EQ sources. While the local device is a server output target, the client **disables its own EQ automatically** and labels it "EQ applied by server" to prevent double application; when the client plays on its own (server not involved) it keeps using local EQ.

## 5. Sink Implementations

### 5.1 Interface split

```go
type Sink interface {
    ID() string
    Kind() Kind
    Caps() Caps
    Open(ctx context.Context, plan StreamPlan) error
    Start(at time.Time) error
    Seek(sample uint64) error
    SetVolume(v float64) error
    Stop() error
    Close() error
    Events() <-chan Event
}

// PushSink receives frames (raop, local)
type PushSink interface {
    Sink
    WriteFrame(f Frame) error
}

// PullSink only hands out a URL; the device pulls (dlna)
type PullSink interface {
    Sink
    StreamURL(plan StreamPlan) (string, error)
}
```

`Timeline` fans frames out to `PushSink` only; `PullSink`s are merely aligned at `Start`.

### 5.2 `local` sink (first-party client)

> **Status: implemented on the server, not implemented on the client (F7-02).** The `register_player` branch at `internal/app/playback.go:356` and `sink/local` both exist, but `clients/player` has never sent this message, so the `local` sink currently only has the fake client from unit tests.

```
client connects WS /api/v1/playback/events
  → register_player {id, kind:"local", name, caps:{codecs:[...], sample_rates:[44100,48000]}}
server sends load {song_id, position, play_at:<unix_ms>}
client starts at (play_at - local clock offset)
client reports {position, buffering, volume, state, rtt_ms} every 500ms
```

- The clock offset is estimated once from the server's `server_time` plus a half-RTT correction; ±50ms is achievable on a LAN.
- The client pulls from the existing `/songs/{id}/stream`, with `format` chosen by negotiation.
- **This is the highest value-per-effort tier**: no external protocol risk, yet "phone + living-room speaker" works immediately.

**Registration lifecycle (the client must follow it)**:

| Moment | Action | Server behaviour |
|---|---|---|
| Startup / WS connected | `register_player` (`id` persisted, unchanged across restarts) | `UpsertPlayer` + auto-create a single-member group + broadcast `speaker_seen` |
| Every 500ms | `report` | Refresh `last_seen_at`, determine online status |
| Disconnected > 30s | — | Mark `online=false`, broadcast `member_changed{action:offline}` |
| Reconnect | `register_player` again with the same `id` | Reuse the original zone, do not create a new one |
| Normal exit | `unregister_player` | Detach from all groups, clean orphan groups, release the sink |

- **`id` must be persisted**: store a random UUID locally; do not use the device model or IP — otherwise a reinstall or network change creates a new zone and leaves a zombie behind.
- **The client must not apply EQ while the server is in control**: EQ has already been applied in the server-side PCM pipeline (§4.8); enabling it again on the client means double equalisation.
- **Degraded state before registration**: the client can still play locally (pure local mode), but the zone list shows "This Device output unavailable + Retry" and must not silently hide it.

### 5.3 `raop` sink (AirPlay 1)

Message sequence (RTSP over TCP, port from mDNS TXT `port`, default 5000):

```
mDNS:  _raop._tcp  TXT: txtvers=1 ch=2 cn=0,1,2,3 et=0,3,5 sr=44100 ss=16
                        pw=false pk=<device RSA public key> tp=UDP

OPTIONS  *              → Public: ANNOUNCE, SETUP, RECORD, PAUSE, FLUSH, TEARDOWN,
                                  OPTIONS, GET_PARAMETER, SET_PARAMETER
ANNOUNCE rtsp://…       → SDP:
                            m=audio 0 RTP/AVP 96
                            a=rtpmap:96 L16/44100/2
                            a=rsaaeskey:<RSA-OAEP-SHA1(AESKey, pk)>
                            a=aesiv:<base64(16B)>
SETUP                   → Transport: RTP/AVP/UDP;unicast;interleaved=0-1;
                          control_port=<c>;timing_port=<t>
RECORD                  → start
[20ms]  RTP audio        payload type 0x60, 12B header + AES-128-CBC(payload)
[1s]    sync packet      type 0x54: rtp_now + ntp_sec + ntp_frac + rtp_latency
[as needed] SET_PARAMETER  volume: -20.0 / progress: 0/12.5/240
FLUSH                    RTP-Info: seq=…;rtptime=…   ← seek
TEARDOWN
```

All crypto uses the standard library: `crypto/rsa` (OAEP-SHA1), `crypto/aes` (CBC), `encoding/base64`.

**Why sync works:** the `sync` packet pairs `rtp_now` with `ntp_time`; each receiver maps RTP timestamps onto its own clock. Every speaker in the group receives the same mapping, so they align by construction. This is exactly why AirPlay can sync and DLNA never can.

**The audio codec is the one real bet:**

| Option | Compatibility | Cost | Notes |
|---|---|---|---|
| L16 (raw PCM) | shairport-sync family ✅; old AirPort Express / some hardware ❌ | none (no encoder) | **First choice, validate first** |
| AAC-ADTS | most devices ✅ | low (ffmpeg `-f adts`) | Lossy; conflicts with a lossless-first server; fallback only |
| ALAC | best | high (no mature Go encoder) | Evaluate only if L16 compatibility proves insufficient |

Before release, a **real-device compatibility matrix** is mandatory (at minimum: AirPort Express, Sonos, router + shairport-sync, mainstream soundbars; Apple TV is excluded because it is AP2-only).

### 5.4 `dlna` sink

```
SSDP M-SEARCH  ST: urn:schemas-upnp-org:device:MediaRenderer:1
  → 200 OK  LOCATION: http://<ip>:<port>/desc.xml
  → parse desc.xml, extract controlURL for AVTransport / RenderingControl / ConnectionManager

SOAP SetAVTransportURI(InstanceID=0, CurrentURI=<stream URL>, CurrentURIMetaData=<DIDL-Lite>)
SOAP Play(Speed=1)
SOAP GetPositionInfo / GetTransportInfo      ← polling (switch to SUBSCRIBE when supported)
SOAP SetVolume(Channel=Master, DesiredVolume=0..100)
SOAP SetNextAVTransportURI(…)                ← gapless next track (not universally supported)
SUBSCRIBE <eventSubURL>                      ← device pushes state
```

- The stream URL must be the **LAN-reachable absolute URL** produced by `internal/services/internal_url.go` (never `127.0.0.1`).
- Start alignment: put every device into a paused `SetAVTransportURI` state, then issue `Play` concurrently.
- Drift is unavoidable: buffer depths differ per device; measured 200–500ms and accumulating. Handle it via §4.3.
- Chinese-brand speakers differ widely in SOAP dialect (tolerance for empty `CurrentURIMetaData`, `Seek` units, missing event subscriptions). Patch by `modelName` and always keep "add device by IP" as a fallback.

### 5.5 Device discovery (`internal/player/discovery`)

```
mDNS  _raop._tcp  →  ServiceEntry(Name/Host/Port/TXT)  →  raop.Found
SSDP  M-SEARCH    →  200 OK(LOCATION)                  →  desc.xml  →  dlna.Device
                     ↓
             Scanner (one per protocol) → Speaker{ID,Kind,Name,Address,Caps}
                     ↓
   Discovery: concurrent scan → dedupe by id → skip pw=true → Manager.RegisterPlayer
```

- **Scanning is separate from registration**: `sink/raop` and `sink/dlna` only parse protocol messages; translating them into `Speaker` lives in `discovery.Scanner`, and registration (solo group, persistence, `speaker_seen` event) is left to `player.Manager`. Protocol layers therefore never need to know about the database.
- **Send from every NIC**: SSDP binds one socket per IPv4 from `net.Interfaces()`. On Docker macvlan or dual-NIC hosts, sending only from the default-route interface misses devices. mDNS walks interfaces inside `hashicorp/mdns`.
- **MX and retransmission**: `M-SEARCH` carries `MX: 2` and is retransmitted twice by default (interval derived from the timeout). UDP loss is common on LANs; a single shot misses roughly half the devices.
- **Address normalization**: the RAOP `pk` TXT value is base64 DER and is converted to PEM before storage (`raop.NormalizePublicKey`); DLNA `Address` keeps the full `LOCATION` URL because `desc.xml` paths differ per vendor.
- **Password-protected devices are skipped**: RAOP `pw=true` means FairPlay authentication, not a plain password, so such devices are reported in `skipped` instead of failing later.
- **Configuration**: `enabled` in `/settings/player-discovery` only controls periodic scanning; manual `POST /players/discover` always works and is limited by `protocols`. Saving wakes the scan loop immediately instead of waiting out the old interval.
- **Manual fallback**: `POST /players` takes `{address, kind}`; DLNA validates AVTransport via `desc.xml`, RAOP requires `device_id` (the MAC in the RTSP path). Private addresses are rejected when `allow_private_network` in `/settings/player-output` is false.

## 6. Data Model

Migration `0040_players.sql`:

```sql
CREATE TABLE players (
    id            TEXT PRIMARY KEY,          -- raop:AA:BB:… / dlna:uuid:… / local:<device_id>
    kind          TEXT NOT NULL CHECK(kind IN ('local','raop','dlna')),
    name          TEXT NOT NULL,
    address       TEXT NOT NULL DEFAULT '',
    model         TEXT NOT NULL DEFAULT '',
    capabilities  TEXT NOT NULL DEFAULT '{}', -- JSON, Caps
    volume        INTEGER NOT NULL DEFAULT 0, -- normalized 0-100
    muted         INTEGER NOT NULL DEFAULT 0,
    online        INTEGER NOT NULL DEFAULT 0,
    eq_profile    TEXT NOT NULL DEFAULT '{}',  -- JSON: {enabled,preset,bands[10],preamp}
    takeover_behavior TEXT NOT NULL DEFAULT 'pause'  -- when this device is taken over: pause | follow (local only)
                  CHECK(takeover_behavior IN ('pause','follow')),
    volume_mode   TEXT NOT NULL DEFAULT 'device'     -- 0043: device | fixed | dsp
                  CHECK(volume_mode IN ('device','fixed','dsp')),
    max_volume    INTEGER NOT NULL DEFAULT 100       -- 0043: hard volume ceiling 0-100
                  CHECK(max_volume BETWEEN 0 AND 100),
    last_seen_at  DATETIME,
    created_at    DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE TABLE player_groups (
    id               TEXT PRIMARY KEY,
    name             TEXT NOT NULL DEFAULT '',
    icon             TEXT NOT NULL DEFAULT '', -- 0044: zone icon key, empty = default
    master_player_id TEXT NOT NULL DEFAULT '',
    party_id         TEXT,                   -- non-null = this group is in a party
    created_at       DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE TABLE party_sessions (
    id              TEXT PRIMARY KEY,
    name            TEXT NOT NULL DEFAULT '',
    leader_group_id TEXT NOT NULL DEFAULT '', -- group that advances the logical position
    created_at      DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE TABLE player_group_members (
    group_id      TEXT NOT NULL,
    player_id     TEXT NOT NULL,
    delay_ms      INTEGER NOT NULL DEFAULT 0,  -- manual delay compensation
    volume_offset INTEGER NOT NULL DEFAULT 0,  -- relative volume within the group
    PRIMARY KEY (group_id, player_id)
);

CREATE TABLE playback_queues (
    owner_kind TEXT NOT NULL CHECK(owner_kind IN ('group','party')),
    owner_id   TEXT NOT NULL,
    position   INTEGER NOT NULL,
    song_id    INTEGER NOT NULL,
    PRIMARY KEY (owner_kind, owner_id, position)
);

CREATE TABLE playback_states (
    owner_kind TEXT NOT NULL CHECK(owner_kind IN ('group','party')),
    owner_id   TEXT NOT NULL,
    song_id    INTEGER NOT NULL DEFAULT 0,
    position   REAL NOT NULL DEFAULT 0,
    state      TEXT NOT NULL DEFAULT 'idle',
    updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    PRIMARY KEY (owner_kind, owner_id)
);
```

Design notes:

- **A standalone player also gets a group containing only itself**, so queue/transport logic exists once. Equivalent to LMS's `master()` simplification.
- `playback_states` backs resume-after-restart (LMS `persistPlaybackStateForPowerOff`).
- The queue is persisted so a restart does not lose it; the in-memory group struct remains authoritative and the DB row is a snapshot.
- `playback_queues` / `playback_states` are generalized with `owner_kind + owner_id` so a **party reuses the same queue and state-machine code** instead of a second implementation; with `owner_kind='party'`, `owner_id` is `party_sessions.id`.
- `eq_profile` lives on the `players` row rather than its own table: EQ is a device property on par with volume, read and written together. `preamp` is a derived value kept only to avoid recomputation and is revalidated against the current `bands` on read.
- `party_id` on `player_groups` instead of a membership table: a group can only be in one party at a time, and a nullable FK column expresses that better than a many-to-many table.

## 7. API

How clients consume these endpoints (picker, transfer, dynamic grouping, dual-mode local playback) is covered in [Playback and Speaker Interaction Design (Frontend)](./architecture_playback_ui.md).

### 7.1 REST

Paths are relative to `/api/v1` (`@BasePath` in `main.go`) and map one-to-one onto `internal/app/routers.go`; fields and status codes are authoritative in `/swagger/index.html` (dev mode).

**Devices**

| Method | Path | Body / params | Notes |
|---|---|---|---|
| GET | `/players` | — | Device list: online, standby, caps, volume, EQ |
| POST | `/players` | `{address, kind, name?, device_id?, public_key?}` | Manual add; `kind` is `raop` / `dlna`, 201 on success; 403 when network sinks are disabled |
| POST | `/players/discover` | — | Trigger SSDP + mDNS scan, returns `{speakers, added, updated, skipped, errors}`; 503 when discovery is disabled |
| PUT | `/players/{id}` | `{name?, takeover_behavior?, volume_mode?, max_volume?}` | Rename / multi-client conflict policy (`pause` / `follow`) / volume mode (`device` / `fixed` / `dsp`) and ceiling (0-100; lowering it clamps the current volume) |
| DELETE | `/players/{id}` | — | Permanent delete: stops the engine before detaching members; a solo group inside a party leaves the party first |
| POST | `/players/{id}/volume` | `{volume, muted}` | Single-device volume; clamped to `max_volume`, ignored (returns the current value) when `volume_mode=fixed` |
| POST | `/players/{id}/power` | `{on}` | Standby / wake; `standby` only means the server stops pushing, not a powered-off device |
| POST | `/players/{id}/reconnect` | — | Drop the cached sink and re-handshake; 409 while still offline |
| GET | `/players/{id}/eq` | — | Includes the server-computed `preamp` |
| PUT | `/players/{id}/eq` | `{enabled, preset, bands[10]}` | The server clamps gains and recomputes `preamp`; clients must refresh from the response |
| GET | `/players/{id}/eq/presets` | — | Preset list (same set as `/settings/equalizer`) |

**Sync groups**

| Method | Path | Body / params | Notes |
|---|---|---|---|
| GET | `/groups` | — | Groups (including `sync_tier`, `member_caps`, `status`, `signal_path`); `member_caps[].syncing` is true while a member is still converging after joining/waking. `signal_path` is a read-only per-zone diagnostic (`lossless\|enhanced\|lossy` + five-stage chain, F7-08 backend implemented) |
| POST | `/groups` | `{name, member_ids, master_id?}` | 201; 409 when a member already belongs to another group, 400 on sync-tier mismatch |
| GET | `/groups/{id}` | — | Single group |
| PUT | `/groups/{id}` | `{name, icon?}` | Zone rename and icon; `name` is 1–40 characters after trimming, `icon` must be in the whitelist (empty = default); persisted across restarts |
| DELETE | `/groups/{id}` | — | Dissolve the group, members fall back to their solo groups; 409 while the group is in a party (leave the party first) |
| POST | `/groups/{id}/master` | `{player_id}` | Change master; 400 for a non-member |
| POST | `/groups/{id}/members` | `{player_id}` | Join during playback, attaching at the current position; 409 if already in another group |
| PATCH | `/groups/{id}/members/{player_id}` | `{delay_ms?, volume_offset?}` | Per-member delay compensation and volume offset |
| DELETE | `/groups/{id}/members/{player_id}` | `?mode=continue\|stop` | Remove a member |
| PUT | `/groups/{id}/members/order` | `{player_ids[]}` | Reorder members; must be a full permutation of the group, otherwise 400. Only `position` changes, **never `master_id`** |
| GET | `/groups/{id}/volume` | — | Group volume plus each member's effective value (with per-device failures) |
| POST | `/groups/{id}/volume` | `{volume, muted}` | Pushed per device; a single failure does not roll back the others |
| POST | `/groups/{id}/transfer` | `{from_group_id, only_player_id?}` | `{id}` in the path is the **destination** group; a non-empty `only_player_id` first splits that device into a new solo group and the destination is ignored |
| GET | `/groups/{id}/queue` | — | Queue snapshot |
| POST | `/groups/{id}/queue` | `{song_ids[]}` | Append songs |
| PUT | `/groups/{id}/queue/reorder` | `{from, to}` or `{remove_index}` | Move semantics; remove and reorder are mutually exclusive |
| POST | `/groups/{id}/transport` | `{action, position?, repeat?, shuffle?}` | `play\|pause\|stop\|next\|prev\|seek` (seek in seconds); `repeat=off\|all\|one` and `shuffle=bool` also go here; 503 when the decode budget is exhausted |
| POST | `/groups/{id}/sleep` | `{minutes}` | Set a one-shot sleep timer; `0` cancels, `1~720` minutes; pauses the group and broadcasts `state` when it fires; in-memory only, lost on restart |
| GET | `/groups/{id}/sleep` | — | `{remaining_seconds, active}`; `remaining_seconds` is rounded up, `active=false` after cancel or firing |

**Alarms**

| Method | Path | Body / params | Notes |
|---|---|---|---|
| GET | `/alarms` | — | Alarms ordered by trigger time; `last_fired_at` is the last minute it fired |
| POST | `/alarms` | `{name, target_id, time_of_day, days_mask, playlist_id, volume, fade_seconds, enabled}` | 201; `time_of_day` is `HH:MM`, `days_mask` bit0=Mon…bit6=Sun (127=daily, 0=today only) |
| PUT | `/alarms/{id}` | same | Overwrite update; `id` and `last_fired_at` are preserved |
| DELETE | `/alarms/{id}` | — | 204; does not affect current playback |
| POST | `/alarms/{id}/test` | — | Start playback immediately per config (does not write `last_fired_at`), for debugging |

**Global and metrics**

| Method | Path | Body / params | Notes |
|---|---|---|---|
| GET | `/playback` | — | First-paint snapshot: every group + status + caps + `party_id` |
| GET | `/playback/metrics` | — | Metrics snapshot: decode slots `decode_slots_used/max`, `player_underruns_total` (silence padding), `player_sink_dropped_frames_total` (downstream drops), `player_resyncs_total` (drift re-attach), `player_events_dropped_total`, `player_drift_ms`, plus per-owner (group/party) detail; see §4.6 for interpretation |

**Parties**

| Method | Path | Body / params | Notes |
|---|---|---|---|
| POST | `/playback/party` | `{name?, group_ids[]}` | 201; 403 when `party_enabled=false`; 409 when a group is already in another party |
| GET | `/playback/party/{id}` | — | Member groups, leader status, per-group offsets |
| POST | `/playback/party/{id}/groups` | `{action, group_id?, mode?}` | `join` / `leave` (`mode=continue\|stop`) / `end` (ends the whole party) |
| POST | `/playback/party/{id}/transport` | `{action}` | Only `play\|pause\|stop`, broadcast to every group in the party |

**Events**

| Method | Path | Body / params | Notes |
|---|---|---|---|
| POST | `/playback/events/ticket` | — | Exchange for a one-time ticket (30s, bound to the user, single use) |
| GET | `/playback/events` | `?ticket=…` | WS upgrade 101; no JWT (browsers cannot set `Authorization` on WS) |

Every endpoint needs swag annotations and `@Security BearerAuth`; `@Router` paths must not include the `/api/v1` prefix.

**There is no `/playback/targets`**: v2 returns everything from `GET /playback` (groups, status, caps, `party_id`), so the output picker needs no separate endpoint. There is also no "current party" GET: the party id lives in each group's `party_id`, so clients read `/playback` first and then `/playback/party/{id}`.

**Transfer compensation semantics**: `transfer` starts the destination at `position + transfer_compensation_ms` (default 800ms), compensating for the gap between the source stopping and the destination sounding. Compensation happens exactly once, on the server; clients must not add their own (see frontend doc §4.7).

**Standby semantics**: `standby=true` from `POST /players/{id}/power` only means the server stops pushing to that device (DLNA sends Stop and clears the URI, RAOP stops RTP); it does not power the hardware off, since neither RTSP nor DLNA has a generic power-on command. Standby members leave the sync ring and re-attach at the current position when woken.

### 7.2 Settings endpoints

Following the "user-visible switches live on business endpoints" rule:

- `/settings/player-discovery` — `{enabled, protocols:["raop","dlna"], interval_seconds}`; `interval_seconds` ranges 10–600, default 30
- `/settings/player-output` — `{max_groups, default_latency_ms, dlna_realign_interval_minutes, allow_private_network, network_sinks_enabled, party_enabled, party_drift_tolerance_ms}`
- `/settings/crossfade` — `{enabled, seconds}`; off by default, 3 seconds, `seconds` in 0.5–12. When enabled the boundary uses equal-power (cos/sin) mixing and **gives up gapless continuation** (product decision, see compat matrix §4)
- `/settings/replaygain` — `{mode: "off"|"track"|"album", prevent_clipping: bool}`; defaults to `off` with clipping prevention on. Tags are read by ffprobe the first time a track plays and cached, not stored at scan time (existing libraries work right after the upgrade)
- `/settings/replaygain` — `{mode: "off"|"track"|"album", prevent_clipping: bool}`; defaults to `off` with clipping prevention on. Tags are read by ffprobe the first time a track plays and cached, not stored at scan time (existing libraries work right after the upgrade)
- `/settings/player-output-allowlist` — `{entries:["192.168.1.0/24", …]}`; an empty array does not restrict private networks, otherwise only these ranges are allowed and public addresses are always rejected

`party_drift_tolerance_ms` defaults to 1500 (range 500–5000); with `party_enabled=false` starting a party returns 403 (managed or public deployments may want to forbid "every room at once"). With `network_sinks_enabled=false` manual add and reconnect return 403: iOS Bundle is always false, desktop Bundle requires an explicit opt-in.

### 7.3 WebSocket authentication

Browsers cannot set an `Authorization` header on WS handshakes, and putting the token in the URL leaks it into access logs. Approach: `POST /playback/events/ticket` returns a one-time ticket (30s TTL, bound to the user), then `GET /playback/events?ticket=…` upgrades.

### 7.4 Event contract

Common envelope: `{seq, type, time_ms, …}`. `seq` is monotonic and shared by every type, so clients only compare `seq` to drop stale messages instead of keeping one counter per type. The remaining fields appear per type (`omitempty` on the Go side); the table lists what each event actually carries.

| `type` | Fields | Emitted when | Client behaviour |
|---|---|---|---|
| `state` | `group_id, song_id, position_ms, status` | Playback state, track or position changes | The only source of playback state; decide relevance by `group_id` |
| `target_changed` | `group_id, from_group_id, reason` | Playback was moved to another group (`reason=transfer`) | Follow `players.takeover_behavior`: `pause` stops immediately and informs the user, `follow` silently becomes a pure controller |
| `member_changed` | `group_id?, player_id, action` | `action=join\|update\|leave\|offline\|online\|deleted\|standby\|wake\|reorder`; `join`/`update`/`leave`/`reorder` carry `group_id` (`reorder` has no `player_id`), `offline`/`online` fire once per containing group | Standby and wake only travel here; there are no separate `power` / `standby` / `wake` events |
| `group_changed` | `group_id, player_id?, action` | `action=deleted` (dissolved), `master_changed` (carries `player_id`), `renamed` (name/icon changed, carries `name`) | **Creating a group emits nothing**: the caller refetches `/playback` or waits for the first `state` |
| `speaker_seen` | `player_id, name, kind` | Discovery found a new device | Offer "new speaker found"; never auto-join any group |
| `party_changed` | `party_id, group_id?, name?, action` | `action=created` (carries `name`), `joined` / `left` (carry `group_id`), `ended` | Refetch `/playback/party/{id}`; clear party state after `ended` |
| `group_drift` | `party_id, group_id, offset_ms, corrected, unaligned` | Each correction-loop measurement inside a party; `corrected=false` marks a failed correction, `unaligned=true` means retries were abandoned | After 3 `corrected=false` events or `unaligned=true`, mark the group "out of sync"; do not repeat toasts |
| `eq_changed` | `player_id, enabled, preset` | Any client changed EQ | Refresh that device's EQ panel |
| `error` | `group_id?, code, message` | Internal engine error (decode, downstream write, …) | Show the error; `stop` and retry when appropriate |

- `state` never says *who* triggered it; when several clients act at once, the highest `seq` wins.
- Runtime metrics (decode slots, underruns, dropped frames, resyncs) **never enter the event stream**: `GET /playback/metrics` is a snapshot pulled on demand (the frontend panel polls every 2s while open), so high-frequency samples do not crowd out slow clients.
- A slow client drops its own copy only; drops are counted in `player_events_dropped_total`. A steadily rising counter means a stuck client, not server-side data loss.
- After a reconnect, clients must refetch `/playback` for a full reconcile rather than trusting incremental events alone.

## 8. Constraints Imposed by the Existing System

### 8.1 Security model conflict (must be handled separately)

DLNA/RAOP require the server to **connect into the LAN**, which directly conflicts with the "block private networks" rule in `internal/services/whitelist.go`.

- Add an independent `player_output_allowlist`; do **not** reuse the resource-proxy allowlist.
- Per `docs/pitfalls.md#private-allowlist`, the existing allowlist only affects resource and transcode proxying and never loosens HLS; player output is a third, separate path.
- A device address must fall inside the allowlist before connecting; otherwise return 403 and write an audit log entry.

### 8.2 Multicast and container networking

SSDP (239.255.255.250:1900) and mDNS (224.0.0.251:5353) are invisible under Docker's default bridge network.

- Documentation must state `--network host` or macvlan.
- When discovery returns nothing, the UI must show that hint instead of an empty list.
- Provide "add device by IP + port" as the fallback when multicast is unavailable.

### 8.3 Bundle mode

- iOS is restricted (mDNS permission, background execution, long-lived sockets) → speaker output is **off by default** in Bundle mode; only the `local` sink is available.
- Desktop Bundle (macOS/Windows/Linux) may enable it explicitly.

### 8.4 ffmpeg budget

The existing four transcode slots (`docs/pitfalls.md#url-transcode`) must not be starved by playback.

- Players use an independent counter `player_max_groups` (default 4), counted in **concurrent decoding groups**:
  - each actively playing sync group costs one;
  - **each group inside a party costs one** (no shared decoder, §4.7), so a 4-group party exhausts the budget;
  - a group forced onto decoding because some member has a non-flat EQ (§4.8) costs one too.
- Each budget throttles separately; exceeding either returns 503 with distinct messages ("transcode busy" vs "player busy"), and the player side further separates "too many simultaneous outputs" from "equalizer needs decoding, no slots left".
- Worked example: 3 sync groups + one 2-group party = 5 > 4; the fifth request is rejected and told which target to release.

### 8.5 Plugin permissions (deferred)

Allowing JS plugins to control players (a new `player` bridge namespace) requires updating three whitelists in lockstep:

1. Host `internal/jsplugin/permissions.go` → `AllPermissions`
2. `plugins/toolchain` → `VALID_PERMISSIONS`
3. Scaffold → `AVAILABLE_PERMISSIONS`

Version one exposes REST/WS only, avoiding a three-repo change.

## 9. Phased Plan and Acceptance

| Phase | Scope | Estimate | Acceptance |
|---|---|---|---|
| **P0** | Queue + state machine + `local` sink + WS events | 3–4 weeks | State-machine/queue unit tests; end-to-end "server drives client playback" |
| **P1** | RAOP with L16, single device | 2 weeks | Docker `shairport-sync` plays audio; captured PCM verified for duration and waveform |
| **P1.5** | RAOP real-device compatibility matrix | 1–2 weeks | ≥4 device classes tested; matrix documented |
| **P2** | DLNA discovery + SOAP + unicast | 3 weeks | Fake renderer (httptest) validates SOAP/DIDL; ≥3 real devices |
| **P3** | Sync groups (exact RAOP + cross-protocol degradation + **correction loop**) | 3–4 weeks | Cross-correlation delay between two instances < 20ms; slowing one by 2s realigns within 5s; mid-playback join is audible within 70ms |
| **P2.5** | Per-speaker EQ (Go-side RBJ biquad + preamp + hot switch) | 1 week | White noise through +12dB@1kHz lands within 0.2dB at 1kHz; no sample above 0dBFS anywhere in the chain; switching EQ produces no click (no step in the waveform) |
| **P3.5** | Party mode (multi-group shared track + drift detection and correction) | 2–2.5 weeks | Three-group party stays within 1.5s; a 2s slowdown realigns within 5s; one group leaving does not disturb the rest; decoder count never exceeds the budget |
| **P4** | AirPlay 2 | — | **Not planned** unless a usable open-source sender appears |

**MVP (P0+P1+P2, unicast without sync) ≈ 2.5 months; with sync (including the correction loop) + EQ ≈ 3.6 months; with party and engineering wrap-up ≈ 4.4 months (19 weeks, ~103 person-days).**

> **P0 status (2026-10-26)**: the server side (queue / state machine / `local` sink / WS events) is complete and tested; **the two-end integration was never done** — the client has neither a WS client nor `register_player`, so the `local` sink only has the fake client from unit tests. The gap list and follow-up tasks are in [development plan §0.2 / §5.1](./playback_development_plan.md#02-v3-revision-design-vs-implementation-gaps-2026-10-26-audit).

### 9.1 Automated verification

- **Unit**: timeline math (injected fake clock), SDP generation, AES/RSA known-answer vectors, RTP golden bytes, queue state machine.
- **RAOP integration**: CI runs `shairport-sync --output pipe` into a file; verify duration/sample rate with `ffprobe` and content via waveform similarity.
- **Sync accuracy**: two shairport-sync instances piped to separate files; cross-correlate to find the delay peak and assert `< 20ms`. This makes sync accuracy automatically testable.
- **Correction convergence**: fake clock + fake sinks injected with ±50ms / ±2s; assert convergence to **≤10ms within one 950ms cycle**, with residual drift ≤5ms over a 10-minute simulation (B5-08).
- **Bit-exact isolation**: with EQ enabled on A only, assert the samples reaching B are identical to the source (max diff = 0) (B6-03).
- **DLNA**: `httptest` fake renderer asserting SOAP bodies and `SOAPAction` headers; real devices verified manually.
- **EQ**: white noise / sweep input, FFT and assert each band's gain within 0.2dB of the setting; assert peak ≤ 0dBFS with all bands at `+12dB` (clipping regression); assert no step larger than 0.1 in the sample stream when hot-switching.
- **Party**: fake sinks record the logical position of their first sample and assert the spread across 3 groups is < 1.5s; slow one group by 2s artificially and assert a `group_drift{corrected:true}` plus a corrective seek within 5s.

## 10. Risks

| Risk | Likelihood | Impact | Mitigation |
|---|---|---|---|
| L16 rejected by real AirPlay hardware | Medium-high | P1 rework | Validate on real hardware during the spike; AAC-ADTS fallback |
| Inconsistent DLNA SOAP dialects | High | Compatibility complaints | Use `SUBSCRIBE` events instead of polling; patch by model; manual add as fallback |
| Mixed-protocol groups ruining the experience | High | Headline feature fails | Explicit `SyncTier` degradation + UI disclosure + cross-family grouping disabled by default |
| Multicast blocked by the user's network | Medium | No devices discovered | Manual IP entry; documented Docker network requirements |
| Underrun on the single decode source causing pops | Medium | Audible defects | 3s ring buffer + silence fill + metrics alerting |
| Wall-clock jump tearing the timeline | Low | Whole group desyncs | Monotonic clock only; wall clock used solely for NTP conversion |
| Party drift heard as an echo | Medium | Experience collapses | 1.5s threshold only does coarse correction; suggest a sync group when two targets share a room; UI states that a party is not exact |
| EQ clipping / click when switching EQ | Medium | Listening quality, possible tweeter damage | Automatic preamp + 20ms ramp + clipped-sample assertion (§9.1) |
| The correction loop introduces audible skips | Medium | Worse listening experience | Only act above 10ms, once per member per cycle, silence fill above 200ms; `player_drift_ms` records every adjustment |
| EQ forcing decoding and exhausting slots | Medium | Feature unusable | Error message names the cause; the UI shows remaining slots before EQ is enabled |

## 11. Capability Boundary vs Roon

The interaction model is aligned with Roon (Zone / focus / grouping / transfer / signal path — see the [interaction design](./architecture_playback_ui.md)), but **"fully achieving Roon's effect" does not hold at the audio layer**. The table below splits the gaps into two kinds: those a design change can close, and those that need a different protocol.

### 11.1 Hard gaps (a design change cannot close them)

| # | Capability | Roon | This project | Nature of the gap |
|---|---|---|---|---|
| H1 | **Sync precision** | RAAT end-to-end ~1ms (shared clock + device-side buffer feedback) | RAOP targets ±20ms; DLNA only aligns start times (200–500ms, drifting) | **Protocol ceiling**: RAOP has no shared clock, so a correction loop can only pull it back after the fact; two speakers in one room will comb-filter |
| H2 | **Local output fidelity** | macOS hog mode / WASAPI exclusive, bit-perfect, native DSD | Client `just_audio` → OS mixer, resampled by the system, DSD must be converted to PCM | **Client audio stack**: bit-perfect needs CoreAudio/AAudio exclusive access; no server change can fix it |
| H3 | **DSP depth** | Parametric EQ, convolution (room correction), upsampling, crossfeed, headroom | 10-band graphic EQ + loudness normalization | **Scope**: convolution and upsampling are a different order of engineering |
| H4 | **Device coverage** | Roon Ready / RAAT devices (hundreds) | AirPlay 1 + DLNA + This Device | **Ecosystem**: RAAT is proprietary and cannot be joined |
| H5 | **Concurrent zones** | Scales with CPU; dozens of zones playing different content | 4 concurrent decode groups by default; the fifth returns 503 (`budget.go`) | **Resource model**: tunable, but bounded by CPU and single-host decoding |

### 11.2 Fixable but not yet fixed (design layer)

| # | Capability | Roon's approach | Current state | Recommendation |
|---|---|---|---|---|
| S1 | **Volume modes** | Per device, one of Device volume / Fixed volume / DSP volume | Only `volume` + `volume_offset` | Add a `volume_mode` column; Fixed is needed by users feeding a preamp (avoids double volume) |
| S2 | **Volume limits** | Per-zone maximum to prevent accidental blasting | None | Add `max_volume`, clamp server-side and echo it back |
| S3 | **Sleep timer** | Yes ("stop in 30 minutes") | Implemented via `/groups/{id}/sleep` (in-memory, pauses the group when it fires) | Could persist to the alarms table to survive restarts |
| S4 | **Queue "play next"** | Play Now / Play Next / Add to Queue | `Queue.InsertNext` exists, **not exposed in the UI** | Frontend wiring only |
| S5 | **Focus follows playback** | Optional: focus switches to the zone that starts playing | Listed as undecided | Make it a setting, default off |
| S6 | **Transfer gap** | RAAT pre-connects, near-instant | 0.5–3s (RAOP handshake + buffering) | Protocol-determined; only an honest transition state can soften it. **Implemented**: target row shows “Switching…”, source row shows “Handing over”, cleared on the target's `state` event, and a “Switch timed out, check the device” hint after 5s (`zone_transfer_provider.dart`) |

### 11.3 Conclusion

- **If the goal is "Roon-style multi-room control"** → the design is sufficient; finishing F7 gets you there, no new protocol needed.
- **If the goal is "Roon's fidelity and sync"** → it needs a RAAT-like protocol (shared clock + device-side buffer feedback) and an exclusive client audio stack — a different project in scope, outside this design.
- **Do not** file H1–H5 as "to be optimized": they are boundaries, not backlog items. Stating them plainly is more useful than pretending they can be closed.

## 12. Zone Queue Ownership (Roon Model)

Roon's model is **the zone owns the queue**: every Zone has its own play queue, and switching zones switches the whole player (queue + current track + position). This project follows the same semantics with the following authority split.

### 12.1 Authority Split

| Role | Owner | Notes |
|---|---|---|
| Queue contents | **Client** (editor) | Selecting, reordering and deleting happen client-side; the whole list is pushed |
| Queue storage | **Server** (store) | `PUT /groups/{id}/queue` replaces the list; `SetQueue` stores without playing |
| Sound output | Zone members | Local member → local `just_audio`; otherwise the server pushes to that zone's devices |
| State display | Server + report mirror | Local output mirrors via `report`; remote zones mirror via `state` events |

### 12.2 Client Sync (`zone_queue_sync.dart`)

- Focused zone changes (or the zone list finishes loading) → `GET /groups/{id}/queue` + `GET /songs/batch?ids=` → `adoptZoneQueue` takes over the local queue.
- Local queue changes → 250 ms debounce → `PUT /groups/{id}/queue` pushes the whole list back.
- Loop prevention: `_pushedSongs/_pushedIndex` remember the last aligned value and `_adopting` marks the adoption window; no push happens while adopting, otherwise the previous zone's queue would be pushed to the new zone.
- Only zones containing this device produce sound: `autoPlay = zone.isLocal && status.isPlaying`; switching to a silent zone calls `stopForServer()` to stop local audio.
- When the focused zone does not contain this device, `remoteZoneIdProvider` holds its id and play/pause/next/prev/seek are delegated to the server via `POST /groups/{id}/transport`, with playback state mirrored from `state` events (`applyRemoteZoneStatus`).

### 12.3 Server Constraints

- `Engine.SetQueue` never starts decoding; if decoding is running and the current track changes (including clearing), decoding stops and the state falls back to `paused`, so the server never keeps playing a track that is no longer in the queue.
- When not decoding, only `Transport.SelectSong` updates the current track; the state machine is untouched (pushing a queue is not "start playing").
- An empty array clears the queue (`Queue.Replace` already supports `current = -1`).
- The zone panel no longer offers a play button: picking a zone only switches focus and queue, playback belongs to the main player (Roon semantics).

## 13. Appendix: Reference Index

| Topic | Reference |
|---|---|
| Single source, multi-sink | `slimserver/Slim/Player/Source.pm` (`shift->master()`) |
| Sync groups and delay compensation | `slimserver/Slim/Player/Sync.pm` (156 lines; the core logic is tiny) |
| Capability negotiation / min sample rate | `slimserver/Slim/Player/CapabilitiesHelper.pm` |
| Playback state machine | `slimserver/Slim/Player/StreamingController.pm` |
| Queue semantics | `slimserver/Slim/Player/Playlist.pm` |
| Resume after restart | `slimserver/Slim/Networking/Slimproto.pm:305 persistPlaybackStateForPowerOff` |
| RAOP receiver (reverse reference) | shairport-sync |
| RAOP sender | OwnTone `src/outputs/raop.c` |
