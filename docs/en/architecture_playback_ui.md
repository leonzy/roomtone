# Playback and Speaker Interaction Design (Frontend)

> **Status: design proposal, not implemented.** For the backend contract see [Playback Control and Speaker Output Architecture](./architecture_playback.md); this document covers only the client (`clients/player`, a separate repository) — interaction, state, and where the code lands.
>
> Applies to the six Flutter platforms. TV and Lynx differences are covered in §8.
>
> **Interactive mockup**: [playback-ui-mockup.html](/roomtone/design/playback-ui-mockup.html) (click outputs, drag volume, edit group members); five-screen static preview: [playback-ui-board.png](/roomtone/design/playback-ui-board.png).

## 1. Design Principles

| # | Principle | Meaning |
|---|---|---|
| P1 | **One concept only: the output target** | A standalone speaker and a group are peers in the UI — both are "targets". A standalone speaker is a group of one. Users never need to understand "groups" as an implementation detail. |
| P2 | **The server is the source of truth** | Queue, position, volume, and membership all come from `/playback` plus WS events; the client does optimistic updates and reconciles on events. |
| P3 | **Switching must be honest** | Transferring, joining a member, and switching the master all incur 0.5–3s gaps. The UI shows a transition state that says "connecting" — **never pretend it is instant**. |
| P4 | **Editable during playback, without leaving the player** | Grouping is a high-frequency action and must be doable in the player; settings only handles low-frequency work (naming, deleting, discovery). |
| P5 | **This device is a target too** | Phone/desktop can be an output target sharing a group with speakers — not a separate feature. |
| P6 | **A party is not a bigger group** | A party shares content (which track, which second); a group shares a clock (±20ms). They are different things, and the UI must not mash them into one "select several devices" control. |
| P7 | **Every speaker has its own ears** | EQ belongs to the device, not the content. The living room's placement problems are not the kitchen's, so there is no implicit coupling. |

## 2. Concepts and State Model

```dart
/// Output target: a group (possibly with a single member)
class OutputTarget {
  final String groupId;
  final String name;              // "Living Room" / "Bedroom speaker"
  final List<Speaker> members;
  final bool isLocal;             // members include this device
  final SyncTier syncTier;        // exact | aligned
  final bool online;
  final String? partyId;          // non-null = this target is in a party
}

class Speaker {
  final String id;
  final String name;
  final SpeakerKind kind;         // local | raop | dlna
  final bool online;
  final int volume;               // 0-100
  final int volumeOffset;         // relative volume within the group
  final int delayMs;              // delay compensation
  final String model;
  final EqProfile eq;             // per-speaker EQ
}

class EqProfile {
  final bool enabled;
  final String preset;            // flat/rock/pop/jazz/classical/bass_boost/treble_boost/vocal/custom
  final List<double> bands;       // 10 bands, -12…+12 dB
  final double preamp;            // filled in by the server, read-only; always a ≤ 0 attenuation
}

class Party {
  final String id;
  final List<String> groupIds;    // participating groups
  final String leaderGroupId;     // group that advances the logical position
  final int? songId;
  final double position;          // logical position
  final Map<String, int> driftMs; // per-group drift, drives the "out of sync" hint
}

class PlaybackState {
  final String groupId;           // current target
  final int? songId;
  final double position;
  final PlaybackStatus status;    // idle|loading|buffering|playing|paused|error
  final int seq;                  // server monotonic sequence, drops stale events
  final String? errorCode;
}
```

- `isLocal` decides whether this client takes over the local player (§7.3).
- When `syncTier == aligned`, the picker shows an amber "approximate sync" badge and **forbids** joining an existing exact group (the backend rejects it too; the frontend blocks it first).
- Targets sort by "online first → same network → name". Offline targets are dimmed but not hidden (users need to see that the study speaker is still in the list).
- A target with a non-null `partyId` shows a `Party` badge plus the participating group names, and taps behave differently from ordinary targets (§4.6).

## 3. Information Architecture

### 3.1 New routes

| Route | Page | Notes |
|---|---|---|
| `/settings/speakers` | Speaker management | A new category in the settings master-detail layout |
| `/output` | Full-screen output page | Desktop/tablet/TV only; phones use a BottomSheet |

### 3.2 Three entry points

```
Player page (mini / full / desktop bar)
  └─ 🔊 Output button ──► OutputSheet (bottom picker; 90% of use cases end here)

Settings
  └─ Playback & Speakers ──► Speaker management (discovery, naming, deletion, delay, output params)

Desktop/TV large screen
  └─ "Output" on the player bar ──► Full-screen output page (drag members, per-device volume)
```

### 3.3 Why a Sheet rather than a dedicated page

Transferring playback is something you do *while* listening; navigating away breaks context. The Sheet keeps the player visible and completes the switch in one tap. Settings handles only low-frequency management.

## 4. Player Page: Output Picker

### 4.1 Wireframe

```
┌────────────────────────────────────┐
│  Play on                      Done │
├────────────────────────────────────┤
│  ● Living Room              [Sync] │   ← current target, highlighted
│    🔊 Living Room Soundbar ────○── │   ← group member, volume inline
│    🔊 Dining Room HomePod  ────○── │
│    ＋ Add speaker                  │
├────────────────────────────────────┤
│  🎉 Party: Kitchen, Study     Edit │   ← only while a party is running
├────────────────────────────────────┤
│  ○ Bedroom speaker                 │
│  ○ Study speaker            Offline│
│  ○ This device (iPhone)            │
├────────────────────────────────────┤
│  ⚙ Manage speakers & groups        │
└────────────────────────────────────┘
```

- Tapping **another target** → transfer playback (§4.3).
- Tapping **a member** → expands its volume/delay/equalizer plus "Play on this speaker only" (split).
- Tapping **"＋ Add speaker"** → inline list of compatible speakers; incompatible ones are dimmed with a reason.
- Tapping the **party row** → party editing (add/remove participating groups, §4.6); with no party it reads "Start a party".
- While a party runs, tapping **another target** *adds it to the party* (it does not transfer); a toast offers undo. Exclusive transfer lives in the long-press menu (§4.6).

### 4.2 States and badges

| Case | Presentation |
|---|---|
| Exact-sync group | Green `Sync` badge |
| Group contains DLNA | Amber `Approximate sync` badge + subtitle "DLNA devices cannot align precisely" |
| Target offline | Row dimmed + `Offline` + tap offers "Reconnect" |
| No speakers at all | Empty-state card: explanation + "Scan for speakers" + "Add by IP" (with the Docker network hint) |
| Bundle mode | Only "This device"; footer note "Local mode does not support speaker push" |

### 4.3 Transferring playback

**Semantics**: move `(queue, current track, position, repeat/shuffle)` from the source target to the destination; the source stops. Equivalent to the backend's `POST /groups/{to}/transfer {from_group_id}`.

**Sequence (frontend view)**:

```
1. User taps "Bedroom speaker"
   → enter transferring state immediately: the sheet stays open, the row shows a spinner,
     the progress bar freezes, and local playback pauses (so two devices never overlap)
2. Server returns 202 accepted
   → show "Connecting to Bedroom speaker…" (RAOP handshake ~100-300ms, DLNA ~200-1000ms)
3. WS state{group: bedroom, status: buffering}
   → show "Buffering…"
4. WS state{group: bedroom, status: playing, position}
   → transition ends, sheet closes, player refreshes
5. Not playing within 10s
   → error message + automatic rollback to the previous target (server-driven; the client only informs)
```

**Key details**:

- **Position compensation**: a transfer loses 0.5–3s. The backend starts the destination at `position + transfer_compensation_ms` (default 800ms, configurable). The frontend must **not** compensate on its own — double compensation skips tracks.
- **No double actions during transition**: other rows are disabled to prevent a double tap from spinning up several groups.
- **Failure rollback**: the error offers "Retry" and "Back to Living Room"; no silent rollback (users need to know what happened).

### 4.4 Split playback (group → single speaker)

Member row menu → "Play on this speaker only":

```
POST /groups/{newGroupId}/transfer { from_group_id: <original>, only_player_id: <member> }
```

Result: the original group keeps its remaining members and keeps playing; the member becomes its own group and **joins at the current position**. Use case: "living room is playing, I'll listen alone in the kitchen".

### 4.5 Source group state and undo

- After a successful transfer the source group goes `idle` and **keeps its queue** (not destroyed): the user can return to it at any time or clear it manually. Destroying the queue would turn a mis-tap into an irreversible loss.
- For 5 seconds after success, the player shows an undo affordance: `Moved to Bedroom speaker · Undo`.
- **Undo is a reverse transfer that takes the destination's position at that moment**, not the original position — otherwise time would jump backwards.
- After 5 seconds the undo disappears; the user simply transfers again from the picker (equivalent action, no special entry point).

### 4.6 Party mode

**In one sentence: a party means "several rooms play the same track", and it does not promise they are exactly in step.** Backend contract in [architecture_playback.md §4.7](./architecture_playback.md); the frontend only has to keep "shared content" and "shared clock" visually separate.

#### Entry points and shape

| State | In the picker | Tap behaviour |
|---|---|---|
| No party | A `🎉 Start a party` row under the current target | Creates a party with the current target as first member |
| Party running | Current target row shows a `Party` badge; a `🎉 Party: Kitchen, Study` row below | Opens party editing (tick other targets to join) |

A party has **no page of its own** — it is a section inside the picker. "Where does the audio go" and "which rooms share this track" are two answers to the same question, and splitting them across pages makes users bounce between them.

#### Key interaction decisions

**1. During a party, tapping another target joins it — it does not transfer.**

When content is already playing in several rooms, tapping one more speaker almost always means "let this one play too". So a tap joins, with a 5-second undo (consistent with §4.5); the opposite intent (only this one) lives in the long-press menu.

| Gesture | Behaviour |
|---|---|
| Tap another target | Joins the party; toast `Added to party · Undo` |
| Long-press / right-click another target | Menu: `Play on this target only (leave party)`, `Make this the main room` |
| Untick a group in the party row | That group leaves, defaulting to "split and continue" |

**2. There is no party master volume.**

The player's volume control **does not change** during a party — it still adjusts the current target (the group this device is in). For whole-house adjustment, provide an explicit "apply to all" button instead of implicit coupling: rooms differ acoustically, and one shared level guarantees someone cannot hear.

**3. Transport broadcasts; volume does not.**

| Control | Scope |
|---|---|
| Play / pause / previous / next / progress | Whole party (`/playback/party/transport`) |
| Volume / mute | Current target only (`/players/{id}/volume`) |
| EQ | A single speaker only (§5.5) |

**4. Drift must be visible without becoming noise.**

- Normal: no indication at all (within 1.5s a party is indistinguishable).
- One group over the threshold: that row shows grey `Aligning…`; no toast, no playback interruption.
- Three failed corrections in a row: the row turns amber with `Out of sync`, and expanding it suggests `Use a sync group when both targets share a room`.
- Do not surface a "party sync accuracy" metric — it is not something the user can act on, and showing it only creates anxiety.

#### Conflicts with existing interactions

- **This device being taken over during a party**: not applicable. A party does not change the *current target*, only *who else is playing*, so `target_changed` never fires and the local player is not paused.
- **"Play on this speaker only" during a party**: equivalent to leaving the party and transferring exclusively; require confirmation (other rooms will stop).
- **Switching the master during a party**: the master is an in-group concept; the party-level leader (`leaderGroupId`) is chosen by the server and **never exposed**. Exposing it only starts arguments about "why isn't my choice the leader".

#### Wireframe (party editing)

```
┌────────────────────────────────────┐
│  Party                        Done │
├────────────────────────────────────┤
│  ✓ Living Room (current)           │
│  ✓ Kitchen speaker                 │
│  ✓ Study speaker          Aligning…│
│  ○ Bedroom speaker          Offline│
│  ○ This device (iPhone)            │
├────────────────────────────────────┤
│  ⚠ Aligns progress, not timing     │
│  End party                         │
└────────────────────────────────────┘
```

## 5. Dynamic Group Editing

### 5.1 Adding a member (during playback)

- Entry: "＋ Add speaker" in the sheet.
- Interaction: checking applies immediately (no "Confirm" button) — it is a light, reversible action.
- Semantics: the new member **joins at the current position**, not from the start. The UI states this explicitly.
- If the member only supports a lower sample rate → the whole group downgrades; show "Quality reduced to 48kHz to match X".

### 5.2 Removing a member

| Option | Behaviour | Default |
|---|---|---|
| Detach and continue | The member becomes its own group, continuing from the current position | ✅ |
| Stop the member | The member stops; the original group is unaffected | |

Implementation: `DELETE /groups/{id}/members/{player_id}?mode=continue|stop`.

### 5.3 Switching the master

- Entry: member row → "Make master".
- Must confirm: "Switching the master interrupts playback for about 1 second. Continue?" — the timeline is rebuilt.
- Delay compensation values survive, but relative volume should be re-confirmed (device gains differ).

### 5.4 Delay compensation and volume

- **Group volume**: a single slider at the top of the sheet, applied to the whole group.
- **Member volume**: the slider inside a member row is `volume_offset` (-12…+12 dB), for "the living-room speaker is far from the sofa, turn it up".
- **Delay compensation**: member row → Advanced → milliseconds (-500…+500). Only shown for `aligned` groups (exact groups do not need it).
- Gestures: desktop/TV supports drag-to-reorder (defines master candidate order); phones use long-press menus to avoid fighting scroll.

### 5.5 Per-speaker equalizer

The backend does the DSP inside the server-side PCM pipeline ([architecture_playback.md §4.8](./architecture_playback.md)); the frontend only edits and displays it.

#### Entry points

- Member row in the picker → menu `Equalizer`
- Settings → Playback & Speakers → speaker row → `Equalizer`

Both open the same page (`/settings/speakers/{id}/eq`). EQ is a device property, not playback context, so it must not fork by entry point.

#### Screen

```
┌────────────────────────────────────┐
│  ← Living Room Soundbar    [on ▢] │
├────────────────────────────────────┤
│  flat  rock  pop  jazz  bass …  │   ← preset chips, horizontal scroll
├────────────────────────────────────┤
│  +12 ┤                            │
│    0 ┤─●──●──●──●──●──●──●──●──●──●─│   ← 10 faders
│  -12 ┤                            │
│      31 62 125 250 500 1k 2k 4k 8k 16k │
├────────────────────────────────────┤
│  Auto attenuation -3.0 dB (anti-clip)│   ← filled in by the server, read-only
│  Apply to all speakers in this group │
└────────────────────────────────────┘
```

#### Rules

- **One preset set**: flat / rock / pop / jazz / classical / bass_boost / treble_boost / vocal / custom, identical to `/settings/equalizer`, so users never see two names for the same curve.
- **Dragging previews immediately**: the fader moves locally and `PUT /players/{id}/eq` is debounced by 300ms (consistent with §7.5); preset taps commit immediately, no debounce.
- **preamp is read-only**: computed by the server; the UI only shows "auto attenuation -3.0 dB". There is no control for it — getting it wrong means clipping, and users do not know what clipping is.
- **No implicit coupling**: editing one speaker edits one speaker. Consistency is an explicit `Apply to all speakers in this group` button, which then reports "applied to 3 speakers".
- **Double-application guard**: while this device is a server output target, its local equalizer switch is **greyed out** and labelled `EQ applied by server`. Otherwise both sides apply EQ and the sound is completely off.
- **Slot pre-check**: enabling EQ forces the group onto the decoding pipeline and consumes one player slot. Check `max_groups` and current usage before enabling; when full, disable the switch with a reason instead of failing after the tap.
- **TV**: faders become ± buttons (1dB per press) and presets cycle with left/right; no vertical dragging.

## 6. Settings

### 6.1 New category "Playback & Speakers"

```
Settings
└─ Playback & Speakers
   ├─ My speakers
   │   ├─ 🔊 Living Room Soundbar   [AirPlay] online
   │   ├─ 🔊 Study speaker          [DLNA]    offline · reconnect
   │   └─ ＋ Add speaker
   ├─ Discovery
   │   ├─ Auto-discovery (on/off)
   │   └─ Protocols (AirPlay / DLNA, multi-select)
   ├─ Output parameters (advanced)
   │   ├─ Concurrent playback limit (default 4 groups)
   │   ├─ Buffer latency (default 2000ms)
   │   ├─ Periodic DLNA re-align (default off)
   │   ├─ Party (on/off)
   │   └─ Party drift tolerance (default 1500ms)
   ├─ This device
   │   ├─ When another device takes over (pause / follow)
   │   └─ Equalizer (applied by server)
   └─ Add manually
       └─ IP + port
```

### 6.2 Speaker row actions

| Action | Notes |
|---|---|
| Rename | Local alias wins over the device's self-reported name (`modelName` is often garbage) |
| Delete | Removes it from the list only; a rescan will find it again (needs an "ignore" flag) |
| Reconnect | Manual retry for offline devices |
| Equalizer | Independent 10-band EQ per speaker (§5.5); the row shows `flat` / `custom` |
| Info | Address, protocol, supported formats/sample rates, last seen |

### 6.3 Fitting the existing settings layout

Settings already use a master-detail layout (`SettingsMasterDetail` + category index). The new category is a regular category — **do not build a separate page framework**; `/settings/speakers` is a mobile secondary page reusing the existing route pattern.

## 7. State Management and Data Flow

### 7.1 Where the code lands (Feature-First)

```
lib/features/output/                      # new module
├── data/
│   ├── output_api.dart                   # /players /groups /playback wrappers
│   ├── output_socket.dart                # WS subscription (backoff + seq checks)
│   └── output_repository.dart            # aggregation + cache + error mapping
├── domain/
│   ├── output_target.dart
│   ├── speaker.dart
│   ├── sync_tier.dart
│   ├── eq_profile.dart                   # per-speaker EQ (read-only preamp filled in)
│   ├── party.dart                        # party model and drift state
│   └── use_cases/
│       ├── transfer_playback.dart        # transfer orchestration (optimistic + rollback)
│       ├── group_edit.dart               # join/leave/switch master
│       ├── party_edit.dart               # start/join/leave/end a party
│       └── eq_edit.dart                  # debounced commit + slot pre-check
└── presentation/
    ├── widgets/
    │   ├── output_button.dart            # player entry (icon + current target name)
    │   ├── output_sheet.dart
    │   ├── speaker_tile.dart
    │   ├── group_member_list.dart
    │   ├── sync_badge.dart
    │   ├── party_section.dart            # party section inside the picker
    │   ├── party_badge.dart
    │   ├── drift_indicator.dart          # Aligning… / Out of sync
    │   ├── eq_sheet.dart                 # per-speaker EQ (reuses equalizer_panel visuals)
    │   └── transfer_overlay.dart         # transition state
    └── providers/
        ├── output_provider.dart
        └── playback_sync_provider.dart
```

Settings-related files go into `features/settings`:

```
features/settings/presentation/
├── speaker_settings_page.dart
├── speaker_eq_page.dart                  # per-speaker EQ (§5.5)
├── widgets/speaker_list_section.dart
└── providers/speaker_settings_provider.dart
```

The EQ panel reuses the fader visuals from the existing `features/player/presentation/widgets/equalizer_panel.dart` instead of drawing a second control set.

### 7.2 Provider graph

```
apiClientProvider
   └─► outputApiProvider ──► outputRepositoryProvider
                                  ├─► speakersProvider      (AsyncNotifier<List<Speaker>>)
                                  ├─► groupsProvider        (AsyncNotifier<List<OutputTarget>>)
                                  └─► playbackStateProvider (Notifier<PlaybackState>)

outputSocketProvider (StreamProvider<OutputEvent>)
   └─► playbackStateProvider (writes, dedup by seq)
   └─► speakersProvider (incremental updates from speaker_seen / offline)

currentTargetProvider  = derived (playbackState.groupId → target from groupsProvider)
isLocalTargetProvider  = derived (currentTarget.isLocal)

partyProvider          (AsyncNotifier<Party?>)  ← /playback/party + party_changed / group_drift
eqProfileProvider(id)  (AsyncNotifier<EqProfile>) ← /players/{id}/eq
```

`partyProvider` does not copy the member list: participating groups still come from `groupsProvider`, and the party only carries `id / leaderGroupId / position / driftMs`, so state can never disagree in two places.

Project convention: **hand-written providers, no code generation**.

### 7.3 Local player takeover (dual mode)

This is the trickiest part of the frontend. One rule: **ownership follows the target**.

| Current target | Client role | Behaviour |
|---|---|---|
| Includes this device (`isLocal`) | Local authority | User actions drive `just_audio` directly (zero latency); position is reported asynchronously |
| Excludes this device | Pure controller | Sends REST commands only; the local player stays silent |

Conflict handling:

- This device is playing A and another device moves the target away → on `target_changed`, follow `players.takeover_behavior`: `pause` (**default**) pauses immediately with a non-blocking notice, `follow` silently becomes a pure controller.
- The preference lives on the server's `players` row (it travels with the device, so a new phone keeps it). Settings path: Playback & Speakers → This device → When another device takes over. Fall back to a locally cached value when offline.
- This device is playing A and a remote party asks it to play B → non-blocking SnackBar: "Living Room wants to take over this device", undoable for 3 seconds.
- Report throttling: position every 1s; immediately on pause/track change/seek.

### 7.4 WebSocket client

- Auth: `POST /playback/events/ticket` for a one-time ticket, then `?ticket=` on upgrade (browsers cannot set an Authorization header).
- Reconnect: exponential backoff 1s→2s→4s→…→30s cap; after a successful reconnect, immediately fetch `/playback` for a full reconcile.
- Dedup: events carry `seq`; anything below the current `seq` is dropped (reordering/replay).
- Degraded mode: 3 consecutive failures → show "Live state unavailable" and fall back to polling `/playback` every 3s.

### 7.5 Optimistic updates

- **Transfer**: mark the target "switching" on tap; do not wait for the response.
- **Volume**: instant slider feedback, debounce 300ms before submitting.
- **Member add/remove**: update the local list immediately; roll back with a message on failure.
- Every optimistic state carries a `pending` flag cleared by the matching server event; `pending` older than 5s counts as failed.

## 8. Platform Differences

| Platform | Output picker | Notes |
|---|---|---|
| Mobile | BottomSheet (60% height, draggable) | Gesture-first; long-press for menus |
| Tablet / Desktop | Side panel or full page | Drag to reorder, per-device volume bars |
| Web | Sheet | A browser can only be a `local` target or a pure controller; it cannot be an RAOP output |
| TV | Full page + D-pad | Must be fully keyboard-navigable: focus order = current group → members → other targets → manage; **no drag**, use "Move up/down" buttons |
| Lynx | Read-only | No Bundle, no local player; shows the current target and a transfer action |

Bundle mode: only the `local` target exists. The output button reads "This device", and settings explains "Local mode does not support speaker push" (iOS background/mDNS limits, see backend doc §8.3).

## 9. Errors and Edge Cases

| Case | Message | Recovery |
|---|---|---|
| Target offline | "Bedroom speaker is offline" | Reconnect / pick another target |
| Destination group is playing something else | "Living Room is playing X. Replace it?" | Confirm to overwrite; cancel leaves everything as-is |
| Transfer timeout (10s) | "Cannot connect to Bedroom speaker" | Retry / back to previous target |
| Unsupported format | "This speaker does not support the current format; transcoded to MP3" | Automatic downgrade, informational only |
| DLNA joins the group | Amber badge + "Approximate sync" | Allowed but disclosed |
| Concurrency limit | "Concurrent playback limit reached (4)" | Guide the user to release a target |
| ffmpeg missing | "The server lacks ffmpeg and cannot push to speakers" | Link to the setting |
| Multicast blocked | Empty list + "Cannot find speakers? Docker networking may be the cause" | Add by IP |
| Party member out of sync | Grey `Aligning…` on that row; amber `Out of sync` after 3 failures | Automatic retry; suggest a sync group |
| Party size limit | "A party holds at most 4 targets" | Guide the user to remove one |
| Party feature disabled | Hide "Start a party" entirely (do not show it and fail on tap) | Admin enables `party_enabled` |
| EQ enabled but no slot | "Server slots are full; cannot apply the equalizer" | Turn the switch off / retry later |
| EQ clipping protection | No prompt; the EQ page shows read-only `Auto attenuation -3.0 dB` | — |

## 10. Interface Contract (Frontend View)

Depends on backend §7, plus the following endpoints (mirrored into [architecture_playback.md](./architecture_playback.md)):

| Method | Path | Purpose |
|---|---|---|
| GET | `/playback/targets` | One-shot fetch for the picker: available targets + current target + capabilities/online state |
| POST | `/groups/{id}/transfer` | Transfer playback `{from_group_id, only_player_id?}` |
| POST | `/groups/{id}/members` | Join during playback `{player_id}` (joins at the current position) |
| DELETE | `/groups/{id}/members/{player_id}` | Leave `?mode=continue\|stop` |
| POST | `/players/{id}/reconnect` | Manually reconnect an offline speaker |
| GET | `/playback/party` | Current party (404 when none) |
| POST | `/playback/party` | Start a party `{group_id, name?}` |
| POST | `/playback/party/groups` | Join a party `{group_id}` |
| DELETE | `/playback/party/groups/{group_id}` | Leave a party `?mode=continue\|stop` |
| POST | `/playback/party/transport` | Broadcast a transport command `{action, position?}` |
| DELETE | `/playback/party` | End the party |
| GET / PUT | `/players/{id}/eq` | Read/write one speaker's EQ (PUT returns the recomputed `preamp`) |
| GET | `/players/{id}/eq/presets` | Preset list (same set as `/settings/equalizer`) |
| PUT | `/players/{id}` | Also writes `takeover_behavior` besides renaming |

WS events:

```
state           {group_id, song_id, position, status, seq}
target_changed  {group_id, from_group_id, reason}
member_changed  {group_id, player_id, action: join|leave|offline|online}
speaker_seen    {player_id, name, kind}
party_changed   {party_id, action: created|joined|left|ended, group_id, seq}
group_drift     {party_id, group_id, offset_ms, corrected}
eq_changed      {player_id, enabled, preset}
error           {group_id, code, message}
```

## 11. Phased Delivery

| Phase | Frontend scope | Backend dependency |
|---|---|---|
| **F0** | Speaker settings page + output picker (read-only listing, transfer between standalone targets) + WS client | P0 (`local` sink) |
| **F1** | Local player takeover (`isLocal` dual mode) + takeover preference (pause/follow) | P0 |
| **F2** | Dynamic member add/remove, group/member volume, sync badges | P2 |
| **F3** | Split playback, master switching, delay compensation | P3 |
| **F4** | Per-speaker EQ (faders, presets, preamp display, double-application guard) | P2.5 |
| **F5** | Party (start/join/leave, drift hints, long-press exclusivity) | P3.5 |
| **F6** | TV/Lynx adaptation, drag-to-reorder | F2 |

F4 and F5 are independent and can run in parallel; both touch only the picker and the settings page, not the player page structure.

F0 can be developed in parallel with backend P0 by mocking `/playback/targets`.

## 12. Acceptance Checklist

Frontend UI verification follows [AI Development Flow · Frontend UI Verification](./ai-development.md#frontend-ui-verification), driving Flutter Web in headless Chrome via Docker:

- [ ] Empty state: opening the picker shows guidance + a scan button, never a blank page
- [ ] Standalone transfer: audio within 3s, position continuous (error < 2s)
- [ ] Join during playback: the new member starts at the current position; existing members never drop
- [ ] Leave: the member detaches and keeps playing; the original group continues
- [ ] DLNA mixed in: badge turns amber, copy is correct
- [ ] Offline target: dimmed, reconnect works
- [ ] WS disconnect: banner appears; state reconciles automatically after reconnect
- [ ] Bundle mode: only "This device", no external target entry
- [ ] Party: three targets sound together within 1.5s; removing one leaves the rest untouched
- [ ] During a party, tapping another target joins it and can be undone within 5s
- [ ] During a party the volume control changes only the current target; no "master volume" appears
- [ ] Per-speaker EQ: takes effect within 300ms of dragging, no click on preset change, preamp shown
- [ ] While this device is an output target, its local EQ switch is greyed out with "EQ applied by server"
- [ ] Takeover preference: set to "follow", being taken over does not pause; set back to "pause" restores the default
- [ ] Accessibility: badges do not rely on colour alone (icon + semantic label); all TV actions reachable via D-pad

## 13. Settled Decisions and Open Questions

### Settled

| Question | Decision |
|---|---|
| What this device does when the target is moved away | **Pause** by default, as a server-side preference `takeover_behavior` (`pause` / `follow`), editable in settings |
| Party mode | **Build it.** Shared content, independent timelines, aligned within 1.5s, no master volume (§4.6) |
| Per-speaker EQ | **Build it.** Inside the server-side PCM pipeline, one per speaker, automatic preamp (§5.5) |

### Open

1. **Per-group track changes inside a party**: not allowed in the current design (a party shares content). If users insist, the fallback is "leave the party, then change track"; needs real feedback first.
2. **EQ import/export**: whether to accept REW / AutoEQ measurement files depends on whether anyone actually measures a room with a microphone.
3. **How hard to warn about the party/sync-group boundary**: today the hint appears only after three failed corrections. Whether to warn at join time based on "same subnet / same name prefix" needs real-world testing.
