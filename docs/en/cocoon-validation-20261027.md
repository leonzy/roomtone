# Cocoon Portable handshake and audible validation (2026-10-27)

Target: parent `708ec132` plus uncommitted tree, Windows amd64, Go 1.26.10, embedded Flutter Web, Cocoon Portable DSD300 / RAOP. Not committed, pushed or released.

The screenshot reported OPTIONS 453, followed by OPTIONS timeouts in later state. An earlier attempt completed SETUP and received one timing request without starting audio. Open held s.mu throughout RTSP negotiation; after replying once, the timing reader waited on the same mutex and could not process subsequent timing requests. A regression on the old implementation failed on timing reply 2 (`cocoon-timing-before.log`). Timing state now uses an independent mutex and connection identity to reject old-session updates.

Failed Open and Close release only RTSP sessions acknowledged on this connection. No Session means no TEARDOWN, avoiding interference with another sender. Error 453 is presented as a rejected/possibly occupied session rather than an encoding diagnosis. Failed signal paths no longer show a simultaneous green success panel.

Offline verification: four UDP timing exchanges complete while the handshake mutex is held. A fake RTSP receiver requires four timing exchanges before RECORD succeeds. Failed RECORD triggers TEARDOWN and socket cleanup. These regressions passed 10 repetitions. App/player Go tests, RAOP vet, seven related widget tests, relevant Dart analysis and release Web build passed.

The user explicitly authorized hardware playback. Handshake succeeded at 07:51:49 and the first audio packet was sent at 07:51:50, streaming the English recording. Group base volume was set to 8%; an existing +8 member offset made actual device volume 16%, confirmed by the API and UI. The user confirmed audible, continuous playback. Roughly 50 seconds were output; first pause logged 2,238,368 samples, zero retransmissions and zero resend misses. Immediate pause/resume at 07:52:39 completed a second handshake/timing/RECORD and sent audio, with advancing playing position and zero underruns. The test ended paused at 07:52:43 without 453, handshake timeout or TEARDOWN error.

Limits: this verifies the stated Cocoon recording and short pause/resume only, not all sources, long playback, multiroom sync or takeover while another sender owns the receiver.
