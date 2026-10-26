# Playback and Speaker Interaction Design (Frontend)

> **Status: partially implemented.** For the backend contract see [Playback Control and Speaker Output Architecture](./architecture_playback.md); this document covers only the client (`clients/player`, a separate repository) — interaction, state, and where the code lands. For delivery order and task breakdown see the [development plan](./playback_development_plan.md); **real-device and TV/Lynx walkthroughs are `Not run`**.
>
> **Implementation status (2026-10-26 audit)**: only the settings-page `/settings/output` panel has landed (F0 panel, F1 grouping, F2 transfer, F3 EQ, F4 party, F5 metrics, F6 standby); **the player-bar entry, zone picker, WS client, This Device registration and dual-mode takeover have not started**. The gap list is in [development plan §0.2](./playback_development_plan.md#02-v3-revision-design-vs-implementation-gaps-2026-10-26-audit).
>
> **v3 revision (Roon model)**: Zone is the only user-facing concept; **multiple zones can play in parallel**, and the picker switches "which zone I am controlling", not "where playback moves"; transfer is an explicit action. See §2 and §4.
>
> Applies to the six Flutter platforms. TV and Lynx differences are covered in §8.
>
> **Interactive mockup**: [playback-ui-mockup.html](/roomtone/design/playback-ui-mockup.html) (click outputs, drag volume, edit group members); five-screen static preview: [playback-ui-board.png](/roomtone/design/playback-ui-board.png).

## 1. Design Principles

| # | Principle | Meaning |
|---|---|---|
| P1 | **One concept only: the Zone** | A standalone speaker and a group are peers in the UI — both are "zones". A standalone speaker is a group of one. Users never need to understand "groups" as an implementation detail. |
| P2 | **The server is the source of truth** | Queue, position, volume, and membership all come from `/playback` plus WS events; the client does optimistic updates and reconciles on events. |
| P3 | **Switching must be honest** | Transferring, joining a member, and switching the master all incur 0.5–3s gaps. The UI shows a transition state that says "connecting" — **never pretend it is instant**. |
| P4 | **Editable during playback, without leaving the player** | Grouping is a high-frequency action and must be doable in the player; settings only handles low-frequency work (naming, deleting, discovery). |
| P5 | **This Device is a zone too** | Phone/desktop can be an output sharing a zone with speakers — not a separate feature. |
| P6 | **A party is not a bigger group** | A party shares content (which track, which second); a group shares a clock (±20ms). They are different things, and the UI must not mash them into one "select several devices" control. |
| P7 | **Every speaker has its own ears** | EQ belongs to the device, not the content. The living room's placement problems are not the kitchen's, so there is no implicit coupling. |
| P8 | **Zones run in parallel, not one moving point** | Each zone has its own queue and playback state and can sound at the same time. The picker switches **control focus**, it does not move playback; transfer is an explicit action (Roon's Zone model). |
| P9 | **This Device is always in the list** | This Device is a built-in zone; it depends on neither discovery nor the network. With no speakers at all, it is the only target. |
| P10 | **The picker only picks a zone** | A zone row is just icon + name, with the selected row highlighted; volume, playback state and progress live on the player panel and are not duplicated here. Switching zones switches the whole player (queue + current track + position, see §12). |
| P11 | **The signal path is visible** | Users have the right to know the audio was processed: passthrough / through DSP / lossy transcode are distinguished by colour, and tapping opens the full chain. |

## 2. Concepts and State Model

**Terminology alignment (v3)**: the only user-facing concept is the **Zone**; the backend calls it `group`, and a single-member group is a standalone zone. UI copy no longer says "group/output target"; those words remain only in technical docs and APIs.

```dart
/// Zone: a group (possibly with a single member), the atomic unit of playback.
/// Each zone has its own queue, transport state and volume, and can sound
/// alongside other zones.
class Zone {
  final String id;                // = group id
  final String name;              // user-editable; defaults to "Living Room" / "This Device"
  final String? icon;             // icon key, whitelist in §4.5
  final List<Speaker> outputs;    // members (field renamed from members)
  final bool isLocal;             // includes this device
  final SyncTier syncTier;        // exact | aligned
  final bool online;
  final String? partyId;          // non-null = this zone is in a party
  final ZonePlayback playback;    // per zone, no longer a global singleton
  final int volume;               // zone base volume (0-100)
  final bool muted;
  final SignalPath signalPath;    // passthrough / through DSP / lossy (§4.6)
}

/// A zone's own playback state. With zones running in parallel, each has one.
class ZonePlayback {
  final int? songId;
  final double position;
  final PlaybackStatus status;    // idle|loading|buffering|playing|paused|error
  final int seq;                  // server monotonic sequence
  final String? errorCode;
}

/// Control focus: which zone the app is currently "looking at". Persisted
/// locally; changes no zone's playback.
class PlaybackFocus {
  final String zoneId;
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

class SignalPath {
  final SignalPathKind kind;      // lossless | enhanced | lossy
  final List<SignalStage> stages; // source→decode→DSP→output→device
}
```

- **Zones run in parallel**: `GET /playback` returns every zone and its `status`; the backend runs one engine per zone (`engines["group:"+id]` in `manager.go`), so several can sound at once. The UI must not assume "only one zone is playing".
- **Focus = ownership**: switching focus switches the whole player (queue + current track + position) but does not change any zone's playback state; only an explicit transfer sends commands.
- `isLocal` decides whether this client takes over the local player (§7.3).
- When `syncTier == aligned`, the picker shows an amber "approximate sync" badge and **forbids** joining an existing exact group (the backend rejects it too; the frontend blocks it first).
- Zones sort by "online first → same network → name". Offline zones are dimmed but not hidden (users need to see that the study speaker is still in the list).
- A zone with a non-null `partyId` shows a `Party` badge plus the participating zone names; joining/leaving goes through the row-end menu (§4.10).
- **This Device zone**: the client registers at startup and keeps a stable `id` (persisted device id), so a restart does not create a new zone.

## 3. Information Architecture

### 3.1 New routes

| Route | Page | Notes |
|---|---|---|
| `/settings/speakers` | Speaker management | A new category in the settings master-detail layout |
| `/output` | Full-screen output page | Desktop/tablet/TV only; phones use a BottomSheet |

### 3.2 Three entry points

```
Player bar, left side (mini / full / desktop bar / mobile)
  └─ zone name + icon + status dot ──► ZonePicker (Roon-style zone picker; 90% of use cases end here)

Settings
  └─ Playback & Speakers ──► Audio devices (discovery, enable/disable, naming, device settings) + Zone management

Desktop/TV large screen
  └─ "Signal path" on the player bar ──► Full-screen zone page (drag members, per-device volume)
```

**Entry placement is a hard Roon constraint**: the zone name must sit on the **persistent player bar**, not hide behind a button on the player page. Only when users can see "which zone I am controlling" can they understand "another zone is still playing".

### 3.3 Why a Sheet rather than a dedicated page

Switching zones, adjusting volume and pausing another room are all things you do *while* listening; navigating away breaks context. The Sheet keeps the player bar visible and completes the action in one tap. Settings handles only low-frequency management (discovering devices, changing device parameters, deleting zones).

## 4. Player Page: Output Picker

### 4.1 Wireframe

Roon's picker is a **zone list**, not "pick a target and jump". Each row is only a device icon and a name, with the focused row highlighted; transport and volume stay on the player panel so the same state never appears twice and drifts.

```
┌──────────────────────────────────────┐
│  Zones                       [Group] │
├──────────────────────────────────────┤
│ ● Living Room               ▶  ───○─ │  ← current focus (filled dot)
│   Playing · Sunny Day — Jay Chou     │
│   🔊 Living Room Soundbar            │  ← expanded members (focus zone only)
│   🔊 Dining Room HomePod             │
├──────────────────────────────────────┤
│ ○ Bedroom speaker           ▶  ───○─ │
│   Paused · Nocturne — Jay Chou       │
├──────────────────────────────────────┤
│ ○ This Device (MacBook)     ▶  ───○─ │
│   Idle                               │
├──────────────────────────────────────┤
│  🎉 Party: Kitchen, Study      Edit  │  ← only while a party is running
├──────────────────────────────────────┤
│  ＋ New zone       ⚙ Device settings │
└──────────────────────────────────────┘
```

**Key difference from the old version**: the old version was "tap another target = transfer playback"; the new one is "tap another zone = switch control focus". The latter is Roon's model, and the model the backend already supports (one engine and queue per zone).

### 4.2 Tap semantics (the easiest place to get it wrong)

| Tap target | Behaviour | Request |
|---|---|---|
| Zone row body | **Switch control focus**; changes no zone's playback | none (writes local `PlaybackFocus` only) |
| Inline ▶ / ⏸ | Control **that zone's** transport | `POST /groups/{id}/transport` |
| Row-end ⋯ → Transfer here | Explicit transfer (§4.7) | `POST /groups/{to}/transfer` |
| Row-end ⋯ → Group with current zone | Merge (§4.4) | `POST /groups/{id}/members` |
| Row-end ⋯ → Rename/icon | Zone identity (§4.5) | `PUT /groups/{id}` |
| Long-press and drag onto another zone | Quick grouping (desktop/TV) | same as "Group" |
| Tap a member row | Expand volume/delay/EQ/split (§4.8) | — |

- **Switching focus must not pause anything.** When the user moves from the living room to the bedroom to look at its queue, the living room must keep playing.
- The inline ▶ on the focus zone is equivalent to the player bar's main button; both must read the same state (the same `ZonePlayback`).
- With several zones playing, the player bar shows only the **focus zone's** track and progress; other zones are represented by their status lines in the picker.

### 4.3 States and badges

| Case | Presentation |
|---|---|
| Exact-sync zone | Green `Sync` badge |
| Zone contains DLNA | Amber `Approximate sync` badge + subtitle "DLNA devices cannot align precisely" |
| Zone offline | Row dimmed + `Offline` + tap offers "Reconnect" |
| No speakers at all | The list still contains "This Device"; a footer row offers "Scan for speakers / Add by IP" (with the Docker network hint) |
| This Device failed to register | The This Device row shows "Output unavailable" + Retry; **never hidden** |
| Bundle mode | Only "This Device"; footer note "Local mode does not support speaker push" |

### 4.4 Grouping (Group Zones)

Roon's grouping is an **action between zones**, not "add a device to the current zone". Two entry points are equivalent:

1. Top "Group" → multi-select → tick ≥2 zones → "Merge".
2. Drag one zone row onto another (desktop/TV).

Rules:

- After merging, the **target zone's queue and playback position are kept**; the other zones stop but **keep their queues** (they can continue after splitting).
- If the source zone is playing and the target zone is idle, the source's content wins (the user's intent is "let this one play too").
- An exact group does not accept `aligned` members: incompatible items are dimmed with a reason; the frontend blocks first and the backend also returns 400.
- Split: member row ⋯ → "Remove" → it becomes its own zone, **joining at the current position** (§4.8).

### 4.5 Zone naming and icons

- Double-click the zone name or ⋯ → "Rename"; 1–40 characters, empty rejected.
- Icons come from a built-in whitelist (`speaker / bookshelf / soundbar / headphone / tv / phone / laptop / multiroom`); uploads are not supported.
- Name and icon are stored server-side (`groups.name` / `groups.icon`) and survive device changes; `PUT /groups/{id}` is implemented (`name` 1–40 characters, `icon` from the whitelist, empty = default).
- Default names: a single-member zone uses the device name; a multi-member zone gets an automatic name such as "Living Room + 1", with a prompt to rename on first merge.

### 4.6 Signal path

A dot on the right of the player bar plus a panel:

| Colour | Meaning | Determined by |
|---|---|---|
| Green | Lossless passthrough | no EQ, no loudness, no resampling, no transcoding |
| Purple | Through DSP | any of EQ / loudness normalisation / resampling enabled |
| Orange | Lossy | transcoded to a lossy format (AAC/MP3) or downsampled |

Panel content: `Source (FLAC 44.1/16) → Decode → DSP (EQ: Rock, -3.2dB preamp) → Output (L16 44.1/16) → Device (Living Room Soundbar)`.

- Data source: `GET /playback` gains a `signal_path` field per zone (backend implemented, F7-08; frontend panel pending).
- The signal path is a **read-only diagnostic**; it offers no shortcuts. To change something, go to the EQ panel or device settings.
- A multi-member zone shows a "members differ" hint (for example, one has EQ on); tap to see the per-member chain.

### 4.7 Transferring playback

**Semantics**: move `(queue, current track, position, repeat/shuffle)` from the source zone to the destination; the source stops. Equivalent to the backend's `POST /groups/{to}/transfer {from_group_id}`.

**Entry**: zone row ⋯ → "Transfer here". **Not a tap on the zone row** — a tap only switches focus (§4.2).

**Sequence (frontend view)**:

```
1. User picks "Transfer here"
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
- **No double actions during transition**: other zone rows are disabled to prevent a double tap from spinning up several groups.
- **Failure rollback**: the error offers "Retry" and "Back to Living Room"; no silent rollback (users need to know what happened).

### 4.8 Split playback (zone → single speaker)

Member row menu → "Play on this speaker only":

```
POST /groups/{newGroupId}/transfer { from_group_id: <original>, only_player_id: <member> }
```

Result: the original zone keeps its remaining members and keeps playing; the member becomes its own zone and **joins at the current position**. Use case: "living room is playing, I'll listen alone in the kitchen".

### 4.9 Source zone state and undo

- After a successful transfer the source zone goes `idle` and **keeps its queue** (not destroyed): the user can return to it at any time or clear it manually. Destroying the queue would turn a mis-tap into an irreversible loss.
- For 5 seconds after success, the player shows an undo affordance: `Moved to Bedroom speaker · Undo`.
- **Undo is a reverse transfer that takes the current zone's position at that moment**, not the original position — otherwise time would jump backwards.
- After 5 seconds the undo disappears; the user simply transfers again from the picker (equivalent action, no special entry point).

### 4.10 Party mode

**In one sentence: a party means "several rooms play the same track", and it does not promise they are exactly in step.** Backend contract in [architecture_playback.md §4.7](./architecture_playback.md); the frontend only has to keep "shared content" and "shared clock" visually separate.

#### Entry points and shape

| State | In the picker | Tap behaviour |
|---|---|---|
| No party | A `🎉 Start a party` row at the bottom of the list | Creates a party with the current focus zone as first member |
| Party running | Participating zone rows show a `Party` badge; a `🎉 Party: Kitchen, Study` row at the bottom | Opens party editing (tick other zones to join) |

A party has **no page of its own** — it is a section inside the picker. "Where does the audio go" and "which rooms share this track" are two answers to the same question, and splitting them across pages makes users bounce between them.

#### Key interaction decisions

**1. During a party, tapping another zone switches focus — it does not join.**

v2 treated "tap = join the party" as a shortcut, but it directly conflicts with §4.2's "tap = switch focus": the same gesture meant different things in different states, so users could not predict it. v3 unifies: **a tap always only switches focus**; joining a party goes through an explicit entry.

| Gesture | Behaviour |
|---|---|
| Tap another zone | Switch focus (party membership unchanged) |
| Zone row ⋯ → "Join party" | Join; toast `Added to party · Undo` |
| Zone row ⋯ → "Play on this zone only (leave party)" | Leave and play independently |
| Zone row ⋯ → "Make this the main room" | Change master |
| Untick in party editing | That zone leaves, defaulting to "split and continue" |

**2. There is no party master volume.**

The player's volume control **does not change** during a party — it still adjusts the current focus zone. For whole-house adjustment, provide an explicit "apply to all" button instead of implicit coupling: rooms differ acoustically, and one shared level guarantees someone cannot hear.

**3. Transport broadcasts; volume does not.**

| Control | Scope |
|---|---|
| Play / pause / previous / next / progress | Whole party (`/playback/party/transport`) |
| Volume / mute | Current focus zone only (`/groups/{id}/volume`) |
| EQ | A single speaker only (§5.5) |

**4. Drift must be visible without becoming noise.**

- Normal: no indication at all (within 1.5s a party is indistinguishable).
- One group over the threshold: that row shows grey `Aligning…`; no toast, no playback interruption.
- Three failed corrections in a row: the row turns amber with `Out of sync`, and expanding it suggests `Use a sync group when both targets share a room`.
- Do not surface a "party sync accuracy" metric — it is not something the user can act on, and showing it only creates anxiety.

#### Conflicts with existing interactions

- **This device being taken over during a party**: not applicable. A party does not change the *current focus zone*, only *who else is playing*, so `target_changed` never fires and the local player is not paused.
- **"Play on this speaker only" during a party**: equivalent to leaving the party and transferring exclusively; require confirmation (other rooms will stop).
- **Switching the master during a party**: the master is an in-group concept; the party-level leader (`leaderGroupId`) is chosen by the server and **never exposed**. Exposing it only starts arguments about "why isn't my choice the leader".

#### Wireframe (party editing)

```
┌────────────────────────────────────┐
│  Party                        Done │
├────────────────────────────────────┤
│  ✓ Living Room (current)           │
│  ✓ Kitchen speaker                 │
│  ✓ Study speaker         Aligning… │
│  ○ Bedroom speaker         Offline │
│  ○ This Device (iPhone)            │
├────────────────────────────────────┤
│  ⚠ Aligns progress, not timing     │
│  End party                         │
└────────────────────────────────────┘
```

## 5. Dynamic Group Editing

### 5.1 Adding a member (during playback)

- Entry: top "Group" multi-select, or zone row ⋯ → "Add output" (§4.4).
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

- **Zone volume**: the main slider inside the zone row, applied to the whole zone (`POST /groups/{id}/volume`).
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
- **Double-application guard**: while this device is a server output, its local equalizer switch is **greyed out** and labelled `EQ applied by server`. Otherwise both sides apply EQ and the sound is completely off.
- **Slot pre-check**: enabling EQ forces the group onto the decoding pipeline and consumes one player slot. Check `max_groups` and current usage before enabling; when full, disable the switch with a reason instead of failing after the tap.
- **TV**: faders become ± buttons (1dB per press) and presets cycle with left/right; no vertical dragging.

## 6. Settings

### 6.1 New category "Playback & Speakers"

Mirrors Roon's **Settings → Audio** (device layer) and **Settings → Setup** (zone layer), kept separate: devices answer "can it be used", zones answer "how do they play together".

```
Settings
└─ Playback & Speakers
   ├─ Audio devices (Roon: Settings > Audio)
   │   ├─ 🔊 Living Room Soundbar   [AirPlay] online · enabled
   │   ├─ 🔊 Study speaker          [DLNA]    offline · reconnect
   │   ├─ 💻 This Device (MacBook)  [Local]   online · enabled
   │   └─ ＋ Add device
   ├─ Zones (Roon: Zones)
   │   ├─ Living Room (2 outputs)   rename · icon · delete
   │   └─ Bedroom (1 output)
   ├─ Discovery
   │   ├─ Auto-discovery (on/off)
   │   └─ Protocols (AirPlay / DLNA, multi-select)
   ├─ Output parameters (advanced)
   │   ├─ Concurrent playback limit (default 4 groups)
   │   ├─ Buffer latency (default 2000ms)
   │   ├─ Periodic DLNA re-align (default off)
   │   ├─ Party (on/off)
   │   └─ Party drift tolerance (default 1500ms)
   ├─ This Device
   │   ├─ When another device takes over (pause / follow)
   │   └─ Equalizer (applied by server)
   └─ Add manually
       └─ IP + port
```

### 6.2 Speaker row actions

| Action | Notes |
|---|---|
| Enable/disable | A disabled device no longer appears in the zone picker but keeps its configuration (Roon's Enable/Disable) |
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
lib/features/playback/                    # new in v3: WS + local output (F7-01/02)
├── data/
│   ├── playback_socket.dart              # ticket → WS → backoff reconnect → seq dedup → degraded polling
│   └── local_output.dart                 # register_player / report / command execution
└── domain/
    ├── playback_event.dart
    └── command.dart                      # load/play/pause/seek/volume/skip

lib/features/output/                      # zones and devices
├── data/
│   ├── output_api.dart                   # /players /groups /playback wrappers
│   └── output_repository.dart            # aggregation + cache + error mapping
├── domain/
│   ├── zone.dart                         # formerly output_target.dart (renamed in v3)
│   ├── speaker.dart
│   ├── sync_tier.dart
│   ├── signal_path.dart                  # passthrough/through DSP/lossy + five-stage chain
│   ├── eq_profile.dart                   # per-speaker EQ (read-only preamp filled in)
│   ├── party.dart                        # party model and drift state
│   └── use_cases/
│       ├── transfer_playback.dart        # transfer orchestration (optimistic + rollback)
│       ├── group_edit.dart               # group/split/switch master
│       ├── zone_identity.dart            # rename + icon
│       ├── party_edit.dart               # start/join/leave/end a party
│       └── eq_edit.dart                  # debounced commit + slot pre-check
└── presentation/
    ├── widgets/
    │   ├── zone_button.dart              # persistent player-bar entry (zone name + icon + status dot)
    │   ├── zone_picker.dart              # Roon-style zone console (list + inline volume/transport)
    │   ├── zone_row.dart
    │   ├── speaker_tile.dart
    │   ├── group_member_list.dart
    │   ├── group_volume.dart             # group volume + member offsets (F7-06)
    │   ├── sync_badge.dart
    │   ├── signal_path.dart              # three-colour dot + five-stage panel (F7-08)
    │   ├── party_section.dart            # party section inside the picker
    │   ├── party_badge.dart
    │   ├── drift_indicator.dart          # Aligning… / Out of sync
    │   ├── eq_sheet.dart                 # per-speaker EQ (reuses equalizer_panel visuals)
    │   └── transfer_overlay.dart         # transition state
    └── providers/
        ├── output_provider.dart
        ├── zone_focus_provider.dart      # control focus (persisted locally)
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
                                  ├─► zonesProvider         (AsyncNotifier<List<Zone>>)   ← GET /playback
                                  └─► zonePlaybackProvider  (Notifier<Map<String, ZonePlayback>>)

playbackSocketProvider (StreamProvider<PlaybackEvent>)
   └─► zonePlaybackProvider (writes by group_id, dedup by seq)
   └─► speakersProvider (incremental updates from speaker_seen / offline)

zoneFocusProvider      (Notifier<String>)       ← persisted locally, sends no request
currentZoneProvider    = derived (zoneFocus → zone from zonesProvider)
isLocalZoneProvider    = derived (currentZone.isLocal)

partyProvider          (AsyncNotifier<Party?>)  ← /playback/party + party_changed / group_drift
eqProfileProvider(id)  (AsyncNotifier<EqProfile>) ← /players/{id}/eq
signalPathProvider(id) = derived (currentZone.signalPath)
```

`partyProvider` does not copy the zone member list: participating zones still come from `zonesProvider`, and the party only carries `id / leaderGroupId / position / driftMs`, so state can never disagree in two places.

**`zoneFocusProvider` and `zonePlaybackProvider` must stay separate**: focus is local UI state, playback is server state. Merging the two into one provider is the root of v2's "tap means transfer" design.

Project convention: **hand-written providers, no code generation**.

### 7.3 Local player takeover (dual mode)

> **Status: not implemented (F7-02).** The client never registers as a `local` device, so This Device never appears in the zone list — that is the root cause of "the playback output page has no This Device".

This is the trickiest part of the frontend. One rule: **ownership follows the zone**.

| Current focus zone | Client role | Behaviour |
|---|---|---|
| Includes this device (`isLocal`) | Local authority | User actions drive `just_audio` directly (zero latency); position is reported asynchronously |
| Excludes this device | Pure controller | Sends REST commands only; the local player stays silent |

**Registration lifecycle (a prerequisite for the two rows above)**:

1. Startup → generate/read the persisted `device_id` (UUID in prefs) → connect the WS.
2. Once connected, immediately send `register_player {id, kind:"local", name:<device name>, caps:{codecs, sample_rates, max_channels}}`.
3. Every 500ms send `report {position_ms, buffering, volume, state, rtt_ms}`; the server uses it to decide online status.
4. Disconnected for more than 30s → the server marks `online=false` and the zone row is dimmed; on reconnect register again (same `id`, no new zone).
5. Send `unregister_player` before exiting; an abnormal exit is cleaned up by heartbeat timeout.

**Command execution**: when the server sends `load {song_id, position, play_at}`, the client starts at `play_at - local clock offset`; `play/pause/seek/volume/skip` map directly onto `just_audio`. While the server is in control, **local EQ must be greyed out** (otherwise EQ is applied twice).

Conflict handling:

- This device is playing A and another device moves the zone away → on `target_changed`, follow `players.takeover_behavior`: `pause` (**default**) pauses immediately with a non-blocking notice, `follow` silently becomes a pure controller.
- The preference lives on the server's `players` row (it travels with the device, so a new phone keeps it). Settings path: Playback & Speakers → This Device → When another device takes over. Fall back to a locally cached value when offline.
- This device is playing A and a remote party asks it to play B → non-blocking SnackBar: "Living Room wants to take over this device", undoable for 3 seconds.
- Report throttling: position every 1s; immediately on pause/track change/seek.

### 7.4 WebSocket client

> **Status: not implemented (F7-01).** `clients/player` has no WebSocket code at all; `OutputPage` polls every 4s, i.e. it is permanently degraded.

- Auth: `POST /playback/events/ticket` for a one-time ticket, then `?ticket=` on upgrade (browsers cannot set an Authorization header).
- Reconnect: exponential backoff 1s→2s→4s→…→30s cap; after a successful reconnect, immediately fetch `/playback` for a full reconcile.
- Dedup: events carry `seq`; anything below the current `seq` is dropped (reordering/replay).
- Degraded mode: 3 consecutive failures → show "Live state unavailable" and fall back to polling `/playback` every 3s.
- **One connection, many zones**: a single WS connection receives events for every zone and dispatches by `group_id`; do not open one per zone.

### 7.5 Optimistic updates

- **Transfer**: mark the target "switching" on tap; do not wait for the response.
- **Volume**: instant slider feedback, debounce 300ms before submitting.
- **Member add/remove**: update the local list immediately; roll back with a message on failure.
- Every optimistic state carries a `pending` flag cleared by the matching server event; `pending` older than 5s counts as failed.

## 8. Platform Differences

| Platform | Zone picker | Notes |
|---|---|---|
| Mobile | BottomSheet (60% height, draggable) | Gesture-first; long-press for menus; inline volume uses a slider |
| Tablet / Desktop | Side panel or full page | Drag to group, per-device volume bars |
| Web | Sheet | A browser can only be a `local` output or a pure controller; it cannot be an RAOP output |
| TV | Full page + D-pad | Must be fully keyboard-navigable: focus order = current zone → members → other zones → manage; **no drag**, use "Move up/down" buttons |
| Lynx | Read-only | No Bundle, no local player; shows the current zone and a transfer action |

Bundle mode: only the `local` zone exists. The zone button reads "This Device", and settings explains "Local mode does not support speaker push" (iOS background/mDNS limits, see backend doc §8.3).

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
| GET | `/playback` | Shared by the picker and first paint: all groups + status + caps + `party_id` (no `/playback/targets` since v2) |
| GET | `/playback/metrics` | Metrics panel: decode slots, underruns, dropped frames, resyncs, per-owner drift |
| POST | `/groups/{id}/transfer` | Transfer playback `{from_group_id, only_player_id?}` (`{id}` is the destination group) |
| POST | `/groups/{id}/members` | Join during playback `{player_id}` (joins at the current position) |
| PATCH | `/groups/{id}/members/{player_id}` | Tune a member `{delay_ms?, volume_offset?}` |
| DELETE | `/groups/{id}/members/{player_id}` | Leave `?mode=continue\|stop` |
| POST | `/players/{id}/reconnect` | Manually reconnect an offline speaker |
| POST | `/players/{id}/power` | Standby / wake `{on}`; standby members leave the sync ring |
| POST | `/playback/party` | Start a party `{name?, group_ids[]}` |
| GET | `/playback/party/{id}` | Party status (member groups, offsets, `drifting` / `unaligned`) |
| POST | `/playback/party/{id}/groups` | `{action: join\|leave\|end, group_id?, mode?}` |
| POST | `/playback/party/{id}/transport` | Broadcast `play\|pause\|stop` |
| GET / PUT | `/players/{id}/eq` | Read/write one speaker's EQ (PUT returns the recomputed `preamp`) |
| GET | `/players/{id}/eq/presets` | Preset list (same set as `/settings/equalizer`) |
| PUT | `/players/{id}` | Also writes `takeover_behavior` besides renaming |

WS events:

The full contract is in backend doc §7.4; the frontend only uses these:

```
state           {group_id, song_id, position_ms, status}
target_changed  {group_id, from_group_id, reason}
member_changed  {group_id?, player_id, action: join|update|leave|offline|online|deleted|standby|wake|reorder}
group_changed   {group_id, player_id?, action: deleted|master_changed}   // creation emits nothing; refetch /playback
speaker_seen    {player_id, name, kind}
party_changed   {party_id, group_id?, name?, action: created|joined|left|ended}
group_drift     {party_id, group_id, offset_ms, corrected, unaligned}
eq_changed      {player_id, enabled, preset}
error           {group_id?, code, message}
```

Metrics (decode slots, underruns, dropped frames, resyncs) never use the event stream; the panel pulls `GET /playback/metrics` every 2s while open.

## 11. Phased Delivery

| Phase | Frontend scope | Backend dependency |
|---|---|---|
| **F0** | Speaker settings page + output picker (read-only listing, transfer between standalone targets) + WS client | P0 (`local` sink) |
| **F1** | Local player takeover (`isLocal` dual mode) + takeover preference (pause/follow) | P0 |
| **F2** | Dynamic member add/remove, group/member volume, sync badges | P2 |
| **F3** | Split playback, master switching, delay compensation, deleting a device/group (with confirmation) | P3 |
| **F4** | Per-speaker EQ (faders, presets, preamp display, double-application guard) | P2.5 |
| **F5** | Party (start/join/leave, drift hints, long-press exclusivity) | P3.5 |
| **F6** | TV/Lynx adaptation, standby/wake (standby members greyed out, cannot join an exact group), member reordering (move up/down buttons — no drag on TV), `member_caps.syncing` → "syncing" hint | F2 |

F4 and F5 are independent and can run in parallel; both touch only the picker and the settings page, not the player page structure.

F0 can be developed in parallel with backend P0 by mocking `/playback`.

## 12. Acceptance Checklist

Frontend UI verification follows [AI Development Flow · Frontend UI Verification](./ai-development.md#frontend-ui-verification), driving Flutter Web in headless Chrome via Docker:

- [ ] Empty state: opening the picker still lists **This Device**, with scan/manual-add entries at the bottom, never a blank page
- [ ] The player bar always shows the zone name + icon; with no speakers it shows "This Device"
- [ ] Zones run in parallel: with the living room playing, switching focus to the bedroom leaves the living room's `status` as `playing` (compare `GET /playback` before and after)
- [ ] Inline ▶ changes only that zone's state and leaves other zones untouched
- [ ] Zone rows show only icon and name, the selected row is highlighted, no volume or playback state
- [ ] Explicit transfer: ⋯ → Transfer here, audio within 3s, position continuous (error < 2s)
- [ ] Grouping: drag/multi-select merge yields one zone, members keep individual volume; after splitting, playback joins at the current position
- [ ] A renamed zone survives a server restart; empty/over-long names are rejected
- [ ] Signal path: after enabling EQ the dot turns from green to purple, and the five-stage panel matches the actual parameters
- [ ] Join during playback: the new member starts at the current position; existing members never drop
- [ ] Leave: the member detaches and keeps playing; the original group continues
- [ ] DLNA mixed in: badge turns amber, copy is correct
- [ ] Offline target: dimmed, reconnect works
- [ ] WS disconnect: banner appears; state reconciles automatically after reconnect
- [ ] Bundle mode: only "This Device", no external target entry
- [ ] Party: three targets sound together within 1.5s; removing one leaves the rest untouched
- [ ] During a party, tapping another zone switches focus (membership unchanged); joining goes through the ⋯ menu and can be undone within 5s
- [ ] During a party the volume control changes only the current zone; no "master volume" appears
- [ ] Per-speaker EQ: takes effect within 300ms of dragging, no click on preset change, preamp shown
- [ ] While this device is an output, its local EQ switch is greyed out with "EQ applied by server"
- [ ] Takeover preference: set to "follow", being taken over does not pause; set back to "pause" restores the default
- [ ] Accessibility: badges do not rely on colour alone (icon + semantic label); all TV actions reachable via D-pad

## 13. Settled Decisions and Open Questions

### Settled

| Question | Decision |
|---|---|
| **Zone model** | Adopt Roon's Zone model: **zones run in parallel**, tapping a zone switches control focus, and transfer is an explicit action (§4.2). The backend already runs one engine per zone, so no protocol change is needed |
| **Terminology** | User-facing copy says "zone" everywhere; the backend and API keep `group`, and the contract is not changed just for a rename |
| **This Device** | A built-in zone, registered at startup, independent of discovery; with no speakers it is the only target (§7.3) |
| **Signal path** | **Build it.** Three colours (green/purple/orange) + a five-stage chain, read-only diagnostic (§4.6) |
| **Zone naming/icons** | **Build it.** Stored server-side; needs a new `PUT /groups/{id}` (§4.5) |
| What this device does when the zone is moved away | **Pause** by default, as a server-side preference `takeover_behavior` (`pause` / `follow`), editable in settings |
| Party mode | **Build it.** Shared content, independent timelines, aligned within 1.5s, no master volume (§4.10) |
| Per-speaker EQ | **Build it.** Inside the server-side PCM pipeline, one per speaker, automatic preamp (§5.5) |

### Open

1. **Per-group track changes inside a party**: not allowed in the current design (a party shares content). If users insist, the fallback is "leave the party, then change track"; needs real feedback first.
2. **EQ import/export**: whether to accept REW / AutoEQ measurement files depends on whether anyone actually measures a room with a microphone.
3. **How hard to warn about the party/sync-group boundary**: today the hint appears only after three failed corrections. Whether to warn at join time based on "same subnet / same name prefix" needs real-world testing.
4. **Zone count limit**: Roon does not limit it; we are bounded by decode slots (default 4). Whether to queue or reject beyond that is open (leaning towards: the fifth zone returns 503 with "simultaneous playback limit").
5. **Whether focus follows playback**: in "follow playback" mode Roon automatically moves focus to the zone that starts playing. Off by default (switching focus is user intent), as an optional setting.
