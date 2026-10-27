# AirPlay stuttering and stale volume targets (2026-10-27)

Target: `708ec132` plus uncommitted tree, Windows amd64, Go 1.26.10 and local embedded Flutter Web. Not committed, pushed or released.

- The reported missing-device error came from cached volume members: after transfer to 9764, PATCH still targeted 9EC4 inside the destination group and returned 404. Zone/member changes now reset volume state and debounce queues, reject stale asynchronous results and check membership before submission.
- Distributor frames contain 882 samples. The previous RAOP sender emitted 352/352/178 although SDP negotiated 352. It now buffers the remainder across calls and sends complete 352-sample packets; deliberate sample discontinuities clear the remainder.
- Added a bounded 1024-packet retransmission cache and control-port reader. Missing packets are returned byte-identically after encryption, including sequence wrap, without advancing normal stream counters. Expired entries cannot match unrelated sequence numbers.
- Added 15-second RTSP OPTIONS keepalive without holding the PCM writer mutex while awaiting replies. The prior TEARDOWN EOF after roughly 34 seconds suggests a closed control connection; idle timeout is **not proven as the stutter cause**.

Offline evidence: 176 distributor frames (3.52 seconds) produce 441 complete RTP packets with continuous samples/timestamps. Encrypted ALAC was received over UDP loopback, decrypted and actually decoded by FFmpeg; resulting PCM matched the input byte for byte. Restoring only the old packetization via Go overlay made the regression fail as expected on packet 3: 724 bytes/178 samples instead of 1420 bytes/352 samples in the L16 test path. Local evidence: `short-packet-before.log` in the Roomtone tools directory. Retransmission tests cover sequences 65534, 65535 and 0, exact encrypted bytes and unchanged normal counters. Keepalive uses a fake local RTSP receiver. Widget regressions cover member removal and zone changes with pending slider edits. App/player Go tests with TEST_FFMPEG, related vet and Dart analysis passed. Full output test log: `output-regression-20261027.log`.

The user requested no more playback tests. This turn did not start hardware playback or execute a playback transfer. Actual network loss, receiver handling of short packets, audible improvement and long playback are **Not run**. Verified code defects do not prove stuttering is fully resolved. Existing zero underrun/drop counters measure decoding/distribution, not RTP network loss.

References: [RAOP-Player](https://github.com/philippe44/RAOP-Player/blob/master/src/raop_client.c), [pyatv control/retransmission](https://github.com/postlund/pyatv/blob/master/pyatv/protocols/raop/stream_client.py).
