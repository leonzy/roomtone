# Roomtone plugin fixes for large libraries

The patches target `charce526/songloft-library-plus` at `2fefd1afb1ac342820a17560c619c615e47a7ae2` and `songloft-org/songloft-plugin-tag` at `bf97f7db5a96e0db0aafb6f3165e95b2379281b1`. Permissions and settings are preserved. Local versions are `0.2.16+roomtone.1` for Plus and `2.3.1+roomtone.4` for Tag. The host automatically updates only to higher versions, ignoring equal-version build metadata. Reapply and verify patches when upgrading upstream code.

The host adds opt-in `songs.list({omitFingerprint:true})`, excluding fingerprint columns in SQL while preserving default lists and single-track fingerprints. Plus reads 1000 songs per batch, builds a background directory index with progress, yields during indexing, and caches metadata for 15 minutes. Writes invalidate the cache; external changes can be refreshed manually.

Tag now loads at most 100 songs per page using existing host song pagination, folder, ID and batch endpoints. The directory tree expands one level at a time. Song, review and organization views no longer accumulate or render the entire library. Review classification and statistics are computed on the server. The browser song cache is capped at 500 entries; explicitly selected songs are retained separately. Search, paging and directory navigation abort old requests and discard stale responses. Directory caching is limited to 16 entries and 30 seconds, with invalidation on writes. Legacy host folder endpoints still return direct songs in the current directory; the plugin slices them before returning a page to the browser.

Apply each patch at its pinned commit, install development dependencies, and run `pnpm exec tsc --noEmit`. Verification used Plus builder/SDK 2.9.4 and Tag builder/SDK 2.18.0. Use a fresh output directory to avoid bundling stale hashed assets twice:

```sh
node --input-type=module -e "import {buildPlugin} from '@songloft/plugin-builder'; await buildPlugin({cwd:process.cwd(),outDir:'dist/roomtone-build'});"
node scripts/plugin-overrides/check-ui.mjs <plus-source> <tag-source>
```

Update through `/api/v1/jsplugins/upload`, retaining old ZIPs for rollback. Do not edit database hashes or widen permissions.

2026-10-29 verification: all 4 Plus and 10 Tag regression tests pass, including complete-HTML JSDOM tests and server route tests. With a 70,518-song total fixture, song, review and organization DOMs remain bounded to 100 rows. Assertions cover cache limits, directory expansion, search, selection across pages and stale responses. Production leonFN endpoints verify root pages, an album directory, search, review and statistics, with no more than 100 songs returned to the browser per page. Server queries can still take seconds; pagination does not imply instant queries. No playback, scraping or audio tag writes were performed.

Prerequisite host verification: metadata bridge tests pass. Two existing Windows path-fixture failures, together with the new test, pass in the leonFN Linux container. Isolated Plus indexing takes about 75 seconds and yields 5,421 directory nodes without 504 errors, with original music mounted read-only.

2026-10-31: Tag adds `POST /scrape/prepare/:id`, reusing the fingerprint/text engine without writing tags, backups or scraped markers. Failures report provider/configuration/low-score reasons with service addresses and credentials removed. The song-info dialog writes tags only after user confirmation. Plugin tests (14), related client regressions and a real NAS read-only request passed; no real song tags or audio playback were changed.

Configuration saves now check HTTP status and response data, retaining inputs and reporting errors on failure. The QQ field explicitly requires an API URL. All 18 plugin regressions pass.
