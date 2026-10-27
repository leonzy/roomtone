# Windows AirPlay hardware validation (2026-10-26)

Baseline: parent `708ec132` plus uncommitted working tree, Windows amd64, Go 1.26.10, embedded Flutter Web. No commit, push or release.

- The library is the user's Downloads. Roomtone streamed an English recording to EDIFIER 9EC4 at group volume 15%. After the RSA/AES correction the user explicitly confirmed audible sound. Earlier PLAYING responses without audible sound were failed trials.
- This receiver advertises `cn=1, et=0,1` without `pk`. Clear ANNOUNCE returned an empty 200 without CSeq. Legacy RAOP RSA wrapping of the session AES key produced a matching ANNOUNCE acknowledgement and allocated receiver ports; Roomtone then produced audible sound.
- Each AES-CBC packet encrypts complete 16-byte blocks only and preserves the clear remainder and packet length. FFmpeg round-trip decoding reproduces the ALAC input PCM exactly. RTSP now rejects missing or mismatched CSeq, and the engine propagates handshake failures.
- Same-address AirPlay/DLNA discoveries prefer AirPlay. The scan found both EDIFIER speakers and Cocoon Portable.
- `CGO_ENABLED=0 go test -tags dev,lite ./internal/app ./internal/player/...` passed, including actual ALAC decoding with `TEST_FFMPEG`. New regressions cover legacy RSA without pk, encrypted packet tails and lengths, and unconfirmed 200 responses.

Limits: only the stated 9EC4 playback was confirmed audible. This does not certify 9764, Cocoon, two-device synchronization, long playback, format/rate matrices, the four UI workflows, or full Roon/DSP parity. These require separate validation. An isolated local pyatv probe was adapted for this receiver's optional empty /info reply and ALAC/RSA support during diagnosis; final audible verification used Roomtone itself.

Protocol references: [RAOP-Player](https://github.com/philippe44/RAOP-Player/blob/master/src/raop_client.c), [pyatv RTSP](https://github.com/postlund/pyatv/blob/master/pyatv/support/rtsp.py).

Follow-up (2026-10-27): fixed push sessions remaining open on pause and resume timelines failing to subtract track position. Added `TestPauseClosesPushSession` and `TestResumeTimelineDoesNotWaitElapsedTrackTime`; the same targeted suite passed. The latest local binary completed pause/resume, returned to playing with advancing position and no handshake error, and was left paused. This state check is not an additional audible confirmation.
