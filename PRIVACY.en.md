# Privacy Notice

> Last updated: 2026-06-10

Roomtone is **self-hosted** software; all data is stored on your own server. This document explains the software's own data-handling behavior so you can assess where the compliance boundaries lie.

## 1. Telemetry and Monitoring Reports

The Roomtone server integrates the [Tracely](https://github.com/hanxi/tracely) monitoring SDK. This feature is **opt-in at compile time** — it is only enabled when the three parameters `AppID`, `AppSecret`, and `Host` are injected via `-ldflags` at build time.

- **Pre-built binaries and Docker images from GitHub Releases**: Tracely parameters are injected, so **monitoring is enabled by default**. Reported content includes: panic stack traces on crashes, first-install events, version-upgrade events, and periodic activity heartbeats. It does **not** include user data, music files, or account information.
- **Building it yourself** (`make build` without passing `TRACELY_APP_ID` / `TRACELY_APP_SECRET` / `TRACELY_HOST`): monitoring is **not enabled**, and the server will not send any report requests.

The startup log clearly shows Tracely's enablement status:
```
Tracely 监控未启用（编译时未注入 AppSecret/Host）   # not enabled
Tracely 监控初始化成功                              # enabled
```

You can verify this via packet capture (tcpdump / Wireshark) or firewall rules.

Beyond this, Roomtone does not embed any anonymous statistics, user-behavior analytics, or advertising SDKs.

## 2. List of Outbound Requests Initiated by Roomtone

Roomtone initiates **outbound** requests in the following scenarios:

| Trigger scenario | Request target | Data content |
|---------|---------|---------|
| Server panic (only when Tracely is enabled at compile time) | Maintainer's self-hosted Tracely service | Error stack trace, server version number, platform information. Does **not** include user data, music files, or account information |
| Server first startup or startup after a version upgrade (only when Tracely is enabled at compile time) | Maintainer's self-hosted Tracely service | Install/upgrade event, current version number, pre-upgrade version number, platform information. Does **not** include user data |
| User clicks "Check for Updates" in "Settings" | `github.com/songloft-org/songloft` | Only an HTTP GET of version.json, carrying no user identifiers |
| User installs / enables a JS plugin and triggers its network capabilities | Determined by that plugin's code (plain HTTP requests are a default host capability; UDP sockets are constrained by the `permissions: ["net"]` sandbox permission) | Determined by the plugin's implementation |
| User loads the badges from this repository's README in the Web UI (e.g. visitorbadge.io) | `api.visitorbadge.io` | Only proxied by GitHub's servers when rendering the GitHub README; not within the Roomtone software |

## 3. Where Data Is Stored

| Data | Location | Notes |
|------|------|------|
| User accounts / password hashes | `data/songloft.db` (SQLite) | bcrypt hashes, no plaintext stored |
| JWT tokens | Client-side local storage (browser LocalStorage / Flutter secure_storage) | The server only stores the hash of the refresh token |
| Music metadata / covers / lyrics | `data/songloft.db` + `data/cache/` | Local only, never uploaded |
| Playback history / favorites | `data/songloft.db` | Local only |
| Plugin config / state | The `plugin_storage` table in `data/songloft.db` | Written by plugins via the sandboxed `storage` API |

**All data is stored within your own deployment environment.** The project maintainers cannot access your data, because there is simply no "reporting" pathway at all.

## 4. Data Collection by JS Plugins

JS plugins **may** use host-provided network requests or their declared `permissions` to read the music library, write to storage, use UDP sockets, and access other capabilities. **The data-collection behavior of third-party plugins is entirely determined by the plugins themselves and is unrelated to the main Roomtone project.**

- Plain HTTP network requests (`fetch`) are a default host capability; raw UDP sockets must be explicitly declared via `permissions: ["net"]` in the manifest and are validated at runtime by the host within the QuickJS sandbox.
- Before installing a third-party plugin, read its source code or permission manifest to confirm that the scope of its network requests matches your expectations.
- To block a particular plugin's network access, disable that plugin in the Roomtone Web UI, or block the corresponding domains at the firewall layer during deployment.

## 5. If You Deploy Roomtone for Others to Use

If you deploy Roomtone on the public internet or in a multi-user environment, **you yourself become a "personal information processor" under regulations such as the PIPL and GDPR**, and you are responsible for:

- Disclosing the scope of data processing to your users (accounts, playback history, IP addresses, etc.);
- Providing means for data export / deletion;
- Ensuring the security of transmission and storage (HTTPS, disk encryption, etc.).

The project maintainers bear **no responsibility** for this; see the [README Copyright and Disclaimer](./README.md#️-copyright-and-disclaimer) for details.

## 6. Contact

If you find any discrepancy between this document and the software's actual behavior, please report it via [GitHub Issues](https://github.com/leonzy/roomtone/issues).
