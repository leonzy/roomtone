# New playback protocol review (2026-10-27)

Target: parent `b34daad` (28 commits/90 files since `4440ed2`), Flutter `305f93c`; Windows amd64, Go 1.26.10, Flutter 3.47.0-0.1.pre, embedded Web. Fixes remain uncommitted. Go content SHA-256: `d7d111324b6d6df22d95c36e3c9542193657b3ba7880da7029d623e03e343167`.

## Fixed defects

1. Builder captured a nil manager method receiver during initialization. Device control now resolves the manager when invoked.
2. AirPlay 2 discovery lost the AirPlay endpoint, V2-only DeviceID, audio format and control capabilities. Transport identity is now complete.
3. V2 Digest authorization was saved but never transmitted. RTSP now sends it using the existing V1 session contract.
4. Timing responses used the wrong request timestamp/field offsets and sequence. They now match V1's 32-byte response and use the captured socket.
5. Retransmission treated count as another sequence and omitted the retransmission header. Consecutive ranges, sequence wrap, framing and sender validation are covered.
6. No first sync packet was sent, subsequent packets used the audio port, and timestamps omitted buffering latency. Sync now starts with audio, uses the control port, and carries the latency relation.
7. V1/V2 metadata/network/image I/O held the PCM mutex. Requests now snapshot briefly and wait outside the audio lock.
8. RAAT remote commands ran on the sole response reader and blocked their own replies. A bounded ordered worker handles commands.
9. RAAT P0 start ignored future startAt by always using 200ms. It now respects the deadline with a 200ms floor; delayed Ready startup has locking and timeout protection.
10. PUT /players/{id} discarded password despite database/manager support. Added set/preserve/clear and redaction regressions and regenerated Swagger.

Two test defects were fixed: the live probe is opt-in with TEST_RAAT_ADDRESS, and streaming assertions wait for the final callback.

## Validation

- Serialized app/player/database tests, playback-related handler tests and relevant Go vet passed.
- Nine regressions fail against the old-code overlay and pass with fixes. Key regressions repeated ten times passed.
- 96 Flutter output/background/queue tests passed. Release Web and the full embedded Go dev executable built.
- Local startup/login/device/group APIs passed; SQLite quick_check=ok; migrations 45/46 applied and dsf enabled for scanning. Original queues and paused/idle states were retained. No new speaker playback was triggered.
- Consistent DB backup: local tools directory, before-feature-review-1793109486.sqlite.
- The first broad run was not fully green: unchanged handlers/services tests require /bin/echo or /bin/true, and the DSF case-sensitive fixture collides on this filesystem. A pipeline test lost frames under parallel load and passed in the serialized suite. These failures are recorded rather than represented as a full-suite pass.

## Remaining boundaries

Full RAAT/AirPlay 2 hardware playback, sustained streaming, groups/sample-level synchronization and hardware password authentication: Not run. Loopback tests are not hardware acceptance.

Discovery retains the user's [raop, dlna] configuration; RAAT is not enabled. The backend accepts raat, but the current frontend has no protocol toggle or manual RAAT addition. FLAC is an optional lower-level implementation not enabled by the application Builder. DoP encapsulation exists, but DSD input still uses PCM per the existing failed go/no-go. These are not claimed as fully integrated features.

Initial recursive fetch failed on the unreachable historical submodule commit fdfba7f. The latest parent points at available 305f93c and synced successfully; remote history was not rewritten.

Evidence: local tools directory, new-features-final-tests.log, new-features-handler-tests.log, new-features-vet.log, new-features-before-regressions.log, new-features-stress.log, new-features-flutter-test.log, new-features-flutter-build.log.


## Authorized live playback recheck (2026-10-27 22:33)

- User requested push plus testing the actual application. The running executable matches the final roomtone-next.exe build. English MP3 song_id=5 was started from the real Web UI to Cocoon at its existing 16% volume.
- First SETUP returned 500. One manual retry completed SETUP/RECORD, timing, DACP registration and audio transmission. The initial refusal remains unexplained and is not claimed fixed.
- User explicitly confirmed audible, continuous playback. Cocoon sent 1,845,888 samples (about 41.9 seconds), zero retransmissions/misses. Web was idle then with no DOM audio/video elements.
- User then transferred playback to EDIFIER 9764 from the actual UI: transfer returned 200, Cocoon stopped, and the target established an ALAC session and advanced. No independent 9764 listening confirmation was obtained.
- User continued operating zones/library manually. Their latest state was preserved instead of overwriting it with the test snapshot. Pause/resume/seek were not separately automated and are not counted as new acceptance tests.
- RAAT/AirPlay 2 hardware, extended continuity and sample-level synchronization remain Not run; the new listening confirmation covers Cocoon over AirPlay 1 only.


## Additional transfer fixes (2026-10-28)

- Root cause: Engine.Load replaced a busy target queue, but startLocked returned while running remained true, leaving its previous decoder playing. Load now stops the previous session first; stale failure, progress and completion callbacks cannot overwrite the replacement session.
- TestTransferReplacesBusyTargetAndStopsSource starts song 11 on the source and 22 on the target, then requires target song 11, an idle source and no further source frames. The old-code overlay timed out; the fix and related Transfer/Next/Seek checks passed five repeats.
- Final serialized Go app/player/.../database suite and relevant go vet passed, as did 370 related Flutter tests, changed-file Dart analysis and Web/Go embedded builds. Actual UI confirmed close-volume-before-open-transfer behavior.
- Busy-target hardware transfer was not completed: the dining speaker disconnected and Cocoon initially returned SETUP 500. During retry the user requested no further playback; explicit stops left Web, Cocoon and the dining speaker idle. No new hardware-transfer or listening pass is claimed. Do not automatically resume playback.
