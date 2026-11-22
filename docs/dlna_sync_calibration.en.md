# DLNA device sync compensation

The per-device `dlna_sync_delay_ms` setting (integer −2000 to +2000ms, default 0) estimates audible offset relative to native AirPlay: positive means slower, negative means faster. It is not another delay added to that DLNA device. No model-based value is guessed.

For multiple non-standby members, compensate each by `largest configured member lag − its configured lag`; non-DLNA lag is zero. Solo playback and groups without DLNA add no compensation. Legacy member `delay_ms` remains in the API/storage but no longer affects audio, preventing double compensation.

For DLNA lags of 1000/600ms and two AirPlay outputs, added delays are 0/400/1000/1000ms. Each device plays alone without extra delay. The setting persists on the device across rediscovery and regrouping. Lynx exposes it in DLNA device settings and the corresponding device in zone settings; grouping only selects members. Draft edits do not write until Save, which updates only this field and reports failures.

Push outputs use PCM silence/timestamp offsets; pull outputs use scheduled starts and preserve active offsets in reliable feedback. Push changes can apply immediately, while multiple-DLNA pull changes apply fully on the next play/resume/track. Pause/Stop dispatch remains concurrent without artificial command delay. Ordinary DLNA buffering can vary; sample-exact synchronization is not promised.

2026-11-01 validation: Windows player/database/handlers/app suites, vet and sqlc generation passed, as did targeted NAS player and database migration/rediscovery persistence tests. Lynx types, 55 related tests and the added failure-save regression passed. Component tests use standard input/slider host stand-ins; production browser checks are recorded separately. Native SDKs and a new acoustic listening run were not run; no client SDK was installed.

2026-11-22: a DLNA offset of −500ms adds 500ms to that renderer in a mixed group, with no added AirPlay delay. Existing positive settings and solo behavior are preserved.

Optional channel assignment is independent for every member, including the master, AirPlay, DLNA and Lynx Web. Local load commands carry the route; DLNA stream tickets sign it and transcode caches isolate it. Absent discrete channels remain silent. Source files are unchanged. Changing a route reloads a playing group at its current position; ungrouping restores stereo. Older clients must upgrade to handle channel commands.
