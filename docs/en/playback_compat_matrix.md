# Playback Capability Matrix (vs. Lyrion Music Server / SlimServer)

> **Status: v1.0 acceptance record (2026-10-25).** This document answers one question: **compared with SlimServer (LMS), what do we match, what do we beat, and where are we still behind?**
> For design and implementation details see [Playback Control and Speaker Output Architecture](./architecture_playback.md); for tasks and acceptance see the [development plan](./playback_development_plan.md). This page only compares.

## 1. Why LMS is the reference

The only mature reference for multi-room sync is the Slim Devices lineage (SlimServer → SqueezeCenter → Logitech Media Server → Lyrion Music Server, "LMS"). It has run "server-side decoding plus sample-level alignment across devices" on a private protocol (SlimProto) for twenty years, and `Slim/Player/Sync.pm` is only 156 lines. Our architecture borrowed the key decisions from it, so we also use its capability envelope as the acceptance bar.

Sources for the comparison:

- SlimProto protocol reference: <https://lyrion.org/reference/slimproto-protocol/> (`strm`'s `trans_type` / `replay_gain` / `autostart` / `strm-t` timestamp echo, `STAT`'s `jiffies` and buffer fields)
- LMS source layout: `Slim/Player/Source.pm` (single source fan-out), `Slim/Player/Sync.pm` (sync groups), `Slim/Player/CapabilitiesHelper.pm` (capability negotiation)
- LMS behaviour below follows the official docs and source layout; **community-experience numbers (e.g. "typical sync accuracy") are labelled as such** and are not treated as official figures.

## 2. Summary of findings

| Verdict | Items |
|---|---|
| **Better than LMS** | Output protocol coverage (RAOP + DLNA + local client; LMS needs plugin bridges that cannot sync), explicit sync-tier modelling with degradation hints, per-speaker server-side EQ, party mode (coarse sync across groups), REST + WebSocket event contract |
| **On par with LMS** | Single-decode-source fan-out, sync groups (master/members/per-device delay compensation), gapless, queue and resume, format negotiation and transcoding, seek, group volume |
| **Still behind** | **crossfade**, **ReplayGain**, **server-side alarm / timed wake** (all three unimplemented, see §4) |
| **Deliberately not done** | AirPlay 2 (no usable open-source sender), accurate DLNA sync (protocol has no shared clock) |

**Verdict: the core sync chain (fan-out → timeline → correction loop → per-device compensation → gapless → resume) meets the bar and can ship v1.0; crossfade / ReplayGain / alarm are recorded as known gaps and are not counted as "passing".**

## 3. Item-by-item comparison

Legend: ✅ on par or better / ⚠️ gap but acceptable / ❌ not implemented / — neither side does it

### 3.1 Transport and distribution

| Capability | LMS | This project | Verdict |
|---|---|---|---|
| Server-side decoding | ✅ `Slim/Player/Source.pm` single source | ✅ `internal/player/pipeline/distribute.go` single decode source fan-out | ✅ |
| Devices share one decoded stream | ✅ master source fan-out | ✅ one PCM stream per group, per-device sink writes | ✅ |
| Native push protocol | SlimProto (private, needs dedicated players) | RAOP (AirPlay 1, standard) | ✅ wider coverage |
| DLNA / UPnP output | ❌ needs the `airupnp` bridge | ✅ native control point (`internal/player/sink/dlna`) | ✅ |
| AirPlay output | ❌ needs the `ShairTunes2W` bridge | ✅ native RAOP sender | ✅ |
| Can bridged devices sync with native ones | ❌ bridges cannot sync | ✅ same-family grouping (exact RAOP / degraded DLNA) | ✅ |
| AirPlay 2 | — | ❌ deliberately not done (FairPlay SAP + HAP, no usable sender) | — |

### 3.2 Synchronisation

| Capability | LMS | This project | Verdict |
|---|---|---|---|
| Sync-group model | ✅ `Slim/Player/Sync.pm`, one master many slaves | ✅ `groups` + `master_id` + `members` | ✅ |
| Per-device delay compensation | ✅ per-player sync adjustment (ms) | ✅ `delay_ms`, clamped to ±2000ms | ✅ |
| Per-device volume offset | ✅ per-player volume adjustment | ✅ `volume_offset`, added on top of group volume | ✅ |
| How drift is measured | `strm-t` timestamp → `STAT` echo, compared against jiffies | periodic sampling of sink position vs. the unified timeline | ✅ same goal, different mechanism |
| Correction period | event driven (correct on every STAT) | 950ms sampling, at most one member touched per cycle | ⚠️ coarser but steadier (no jitter) |
| Minimum adjustment | aligned to samples internally | 10ms (below that, do nothing to avoid audible jumps) | ✅ |
| Handling beyond 200ms | restart playback | silence fill + seek back, logged via `player_drift_ms` | ✅ |
| Accuracy commitment | community experience: single-digit ms on wired hardware players | design target ≤20ms, verifiable by the cross-correlation script (§5.2) | ⚠️ looser target, but **explicitly committed** and reproducible |
| Cross-family grouping | ❌ (bridges do not sync) | ✅ but explicitly degraded via `SyncTier`, shown in the UI | ✅ |
| "Aligning" hint after join/wake | ❌ no such concept | ✅ `member_caps[].syncing`, 3 correction cycles (≈2.85s) | ✅ |
| Member ordering | join order | ✅ explicit `position` + `PUT /groups/{id}/members/order` | ✅ |
| Member change events | CLI subscription | ✅ WS `member_changed` (`added`/`removed`/`updated`/`reorder`) | ✅ |

### 3.3 Playback continuity

| Capability | LMS | This project | Verdict |
|---|---|---|---|
| gapless (continuous albums) | ✅ player keeps the decoder open (`strm` flag `0x40`) | ✅ `GaplessSink.Continue`, continuous sample index, 3s prefetch window | ✅ |
| Next-track pre-open | ✅ server sends it ahead of time | ✅ `prefetch.go`, opens the next decoder when <3s remain | ✅ |
| crossfade | ✅ `strm`'s `trans_type` 1–4 + `trans_period` | ❌ not implemented | ❌ |
| fade in/out | ✅ same (`trans_type` 2/3) | ❌ not implemented | ❌ |
| Underrun handling | ✅ `STMu`/`STMo` reports + rebuffer | ✅ ring buffer + silence fill + `underruns` metric | ✅ |

### 3.4 Loudness and audio quality

| Capability | LMS | This project | Verdict |
|---|---|---|---|
| ReplayGain (track/album) | ✅ `strm` carries `replay_gain` (16.16 fixed point), applied player-side | ❌ not implemented | ❌ |
| Smart Gain / cross-album loudness | ✅ | ⚠️ streaming-side `loudnorm` normalisation exists (cache/transcode paths) but is **not** ReplayGain tag semantics | ⚠️ |
| Per-speaker EQ | ❌ none (only what the player offers) | ✅ server-side RBJ biquad + preamp + presets + hot switching | ✅ |
| Clipping protection | — | ✅ automatic preamp + peak assertion | ✅ |

### 3.5 Scheduling and automation

| Capability | LMS | This project | Verdict |
|---|---|---|---|
| Server-side alarm (timed playback) | ✅ CLI `alarm`, repeat days / volume / playlist | ❌ not implemented | ❌ |
| Sleep timer (timed stop) | ✅ | ⚠️ implemented client-side (Flutter `SleepTimer`); per-device, not persisted across clients | ⚠️ |
| Party / multi-group playback | ❌ no such concept | ✅ coarse alignment across groups (1500ms tolerance, seek-back when exceeded) | ✅ |

### 3.6 Queue, library, interfaces

| Capability | LMS | This project | Verdict |
|---|---|---|---|
| Queue add/remove/reorder | ✅ `Slim/Player/Playlist.pm` | ✅ `/groups/{id}/queue` + `queue/reorder` | ✅ |
| Resume after power off | ✅ `persistPlaybackStateForPowerOff` | ✅ group state persisted, restored on wake | ✅ |
| Playback transfer (hand-off between devices) | ✅ CLI `transfer` | ✅ `POST /groups/{id}/transfer`, 800ms compensation | ✅ |
| seek | ✅ | ✅ server-side seek (exact for CBR / 3s tail guard for VBR) | ✅ |
| Format negotiation | ✅ `CapabilitiesHelper.pm` (lowest common denominator) | ✅ intersection across the group, transcode when needed | ✅ |
| Server-side transcoding | ✅ `custom-convert.conf` (sox/flac/lame) | ✅ ffmpeg, 4 transcode slots, 503 when exhausted | ✅ |
| Metadata / artwork | ✅ | ✅ scanning + tag read/write + sidecar cover/lyrics | ✅ |
| Open interfaces | CLI (JSON-RPC) + Perl plugins | REST + WebSocket (ticket auth) + JS plugin sandbox | ✅ |
| Event push | CLI subscription | ✅ WS `playback/events`: `group_drift` / `member_changed` / `syncing` | ✅ |
| Runtime metrics | limited | ✅ `GET /playback/metrics` (underruns, drift, slot usage) | ✅ |
| Client forms | hardware Squeezebox + squeezelite + Web UI | Flutter (6 platforms) + Lynx (Android/iOS/HarmonyOS/Web) + Android TV | ✅ |

### 3.7 Score

| Area | Items | ✅ | ⚠️ | ❌ |
|---|---|---|---|---|
| Transport and distribution | 7 | 6 | 0 | 0 (1 item is skipped by both) |
| Synchronisation | 12 | 10 | 2 | 0 |
| Playback continuity | 5 | 3 | 0 | 2 |
| Loudness and audio quality | 4 | 2 | 1 | 1 |
| Scheduling and automation | 3 | 1 | 1 | 1 |
| Queue / library / interfaces | 11 | 11 | 0 | 0 |
| **Total** | **42** | **33** | **4** | **4** (1 of them skipped by both) |

## 4. Known gaps (not counted as passing)

None of the three is an architectural limit; each is a scheduling trade-off. Ordered by "cost to close / value to users":

| Gap | How LMS does it | What we lack | Path to close | Cost |
|---|---|---|---|---|
| **crossfade / fade** | `strm` sends `trans_type` + `trans_period` and the **player** performs the fade; RAOP/DLNA devices ignore the field | no crossfade mixer in the server-side decode path | add a 200ms linear / equal-power mixer in the PCM stage of `pipeline/distribute.go`, open the next track one fade window early on queue advance; the whole group must share one fade duration | small (~3 person-days), but gapless and crossfade become mutually exclusive — needs a product decision |
| **ReplayGain** | `strm` carries `replay_gain`, the player applies the gain; tags come from the scan stage | the scanner does not read `REPLAYGAIN_*` tags, and the decode path has no gain application point | read the tags at scan time into the DB (`pkg/tag` can already read them), apply a 16.16 fixed-point gain per track/album mode in the decode/distribute stage, expose three modes (off / track / album) in the API | medium (~5 person-days, including UI and clipping regression) |
| **Server-side alarm** | CLI `alarm`: multiple alarms per player, repeat days, volume, playlist, fade-in | no scheduler and no "start playback at time T" orchestration | add an `alarms` table + scheduler on the existing timer facilities, call `transport` at the scheduled time with volume fade-in | small (~3 person-days), but needs a settings page and timezone handling |

**Why v1.0 can ship with these three open**: LMS's crossfade and ReplayGain both depend on **player-side** support, while our primary outputs are RAOP / DLNA devices — neither protocol honours those fields, so not doing it server-side is equivalent to not having it at all. Alarm is a standalone module with no coupling to the sync chain. Therefore none of them makes the sync capability "fail".

## 5. Verification record

### 5.1 Decode performance baseline (X-05)

`scripts/tests/decode_baseline.sh`: starts 4 concurrent ffmpeg decodes using the same `decoderArgs` as `pipeline/decoder.go` (s16le / 44.1kHz / 2ch → pipe) on a freshly synthesised 3-minute stereo track.

| Metric | Measured (3 runs) | Threshold | Result |
|---|---|---|---|
| wall time | 0.14 – 0.17 s | ≤ 180 s (track length) | pass |
| realtime factor | 1085 – 1294× | ≥ 1× | pass |
| peak RSS (4 streams) | 6 – 29 MiB | < 512 MiB | pass |

Conclusion: on a single-core device, 4 concurrent decodes leave ~1000× headroom, so **sync-group size is not limited by decode compute**; the bottleneck is the network and sink writes.

### 5.2 Sync accuracy script (B5-06)

`scripts/tests/sync_correlation.py`: standard library only; reads two `s16le` raw PCM streams, finds the peak delay by normalised cross-correlation, and supports `--window` to inspect drift over segments.

| Item | Result |
|---|---|
| `--selftest` (12ms injected, broadband pseudo-random, interleaved stereo) | **pass**: measured 12.0ms, correlation 1.000 |
| `--window 2` segmentation (6s of material) | **pass**: +12.0ms in every segment, no drift, worst 12.0ms < 20ms threshold |
| Real dual instances (two `shairport-sync` piping to separate files) | **Not run**: requires two RAOP receivers online simultaneously; the script is ready and runs as-is once two instances or a CI harness exist |

```bash
# self-test (no devices)
python3 scripts/tests/sync_correlation.py --selftest

# dual-instance measurement
shairport-sync -a A --output pipe --pipe-path /tmp/a.raw &
shairport-sync -a B --output pipe --pipe-path /tmp/b.raw &
python3 scripts/tests/sync_correlation.py /tmp/a.raw /tmp/b.raw --window 2
```

## 6. Reproducing and maintaining this table

- The table is organised by **capability**, not by feature list; when adding a capability, update both language versions and the score in §3.7.
- Changes on the LMS side (e.g. native AirPlay support) require re-checking §3.1 and revisiting the "better than" claims in §2.
- Once a gap is closed, flip its ❌ to ✅ and update §2 and §4.
