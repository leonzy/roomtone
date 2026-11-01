# Roomtone DLNA output compared with AirConnect

2026-11-01. Scope: Roomtone's own playback to renderers. [中文](dlna_output_compatibility.md).

Reference: [AirConnect af345def](https://github.com/philippe44/AirConnect/tree/af345def2e862f69e2c47fcd46b3ec3ec9b542f3), especially `airupnp.c`, `avt_util.c`, `raop_streamer.c` and the README HTTP/buffering sections. Independent implementation; no AirPlay receiver or source code was imported.

| Concern | Roomtone behavior |
|---|---|
| Initial buffering | Complete local files allow the renderer's TCP-controlled initial burst. Remote downloads and necessary conversions complete before URI handoff; complete files need no artificial silence prefix |
| Length, HEAD, Range | Real complete-file lengths and byte seeking; real FFmpeg cold-cache regressions for MP3, L16 and ADTS |
| Protocol declarations | Matching DIDL and HTTP contentFeatures distinguish converted/original and finite/live representations; advertise byte seeking only, not unsupported DLNA time-range requests |
| Codec choice | Original first; convert only on explicit incompatibility or requested processing. Distinguish ADTS AAC from M4A. L16 output is big-endian stereo 44.1kHz and selected only for matching declarations |
| Control ordering | Serialize SOAP controls with polling. Bounded transient load/play/seek retries; stop cancels scheduled playback. Reload stops the old session |
| Mixed-group hot join | Rebuild the decoded/delegated loop at the current track/position when the first push member joins DLNA/Web playback or the last push member leaves. Preserve the budget slot; handshake without PCM production is insufficient |
| Mixed-group alignment | Compensate post-Play delay; retain milliseconds where supported and use whole seconds for S12. Reliable reporters receive coarse PCM-clock correction (two same-direction reports ≥1.2s, 15s cooldown). S12 WAV RelTime is wrong: disable its feedback and calibrate member delay. Acoustic precision is not guaranteed |
| WAV seek confirmation | Confirm actual 206 offsets on the signed stream using PCM WAV data offset/byteRate, rather than SOAP ACKs or known-bad RelTime. Release observers with the output |
| Pause/seek | Send Pause to pull renderers and cancel scheduled Play; fall back to Stop if unsupported. Reload seeks through Open(plan.Position), without a second Seek racing the old session |
| Status | Persistent one-second fallback polling, even with events. Interpolate between reports, support fractional seconds and ignore unavailable positions |
| GENA | Renew halfway through the granted lease; re-subscribe on failure and keep polling. Unsubscribe on close; accept instance 0/Master only |
| Description | Honor URLBase and embedded renderers; keep services within one renderer |
| Volume | Native advertised range/step; reconnect applies saved volume/mute/limit. Mute before zero, set volume before unmuting. Unsupported SetMute falls back to setting native volume, without promising absolute silence |
| Seek quirks | S12 waits for post-Play readiness; time seeks no longer misuse TRACK_NR, which expects a track number |

Excluded bridge concerns: incoming AirPlay/pairing, RTP retransmission/jitter buffering, controller feedback, aircast/Chromecast, and fake lengths/silence prefixes needed for real-time AirPlay input. Complete Roomtone files should not acquire artificial bridge latency.

In the inspected AirConnect version AVTSetNextURI has no playback call sites. It is not evidence of portable track-level gapless support. Roomtone retains its own queue/auto-next and per-track loading; sample-exact DLNA gapless/synchronization is not promised here.

The dining-room EDIFIER uses native AirPlay, distinct from Xiaomi DLNA. Previously 3% mapped to −30.46 dB outside the usual −30..0 dB range. Current 3%/4% map to −29.1/−28.8 dB; zero uses separate mute. Tests prove protocol values, not perceptually smooth hardware loudness.

Subsequent Pause/WAV fix validation: Windows player/DLNA/app packages and vet pass; NAS-container full DLNA and relevant player regressions pass. Full NAS player testing exhibits the existing `TestStandbyExcludesFromSyncB0805` queued-frame timing failure (22→29); full-suite stability is not claimed. That test/distribution queue was not changed to bypass failure. Isolated removal of Pause/restoration of the bad WAV counter fails new regressions; a regression also catches duplicate Engine.Seek commands. Hardware evidence: PCM WAV stereo 44.1kHz/16bit, data offset 44; seeks to 40s/50s request 7056044/8820044 and receive actual 206 responses.

Baseline: `847d5e83`. Regressions cover short leases, serialized requests, URLBase/embedded renderers, event filtering, native volume/unsupported mute, exact L16 reference bytes, ADTS headers, AAC-to-M4A containers and HEAD/Range. An isolated overlay restoring incorrect URLBase resolution fails the new regression on the old address; the current code passes. Five related Windows packages, vet, and the NAS container's full DLNA package/real FFmpeg HTTP regression pass. Windows race testing was not run (CGO disabled and no configured C compiler). Deployment/hardware evidence is reported separately. Other models, native Sonos topology and live/ICY quirks require device-specific validation; universal renderer parity is not claimed.
