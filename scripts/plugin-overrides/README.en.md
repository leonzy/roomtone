# Roomtone plugin fixes for large libraries

The patches target `charce526/songloft-library-plus` at `2fefd1afb1ac342820a17560c619c615e47a7ae2` and `songloft-org/songloft-plugin-tag` at `bf97f7db5a96e0db0aafb6f3165e95b2379281b1`. Existing permissions and settings are preserved; versions gain `+roomtone.1`. Upstream plugin updates replace these builds, so reapply and verify the patches when upgrading.

The host adds opt-in `songs.list({omitFingerprint:true})`, excluding fingerprint columns in SQL while preserving default lists and single-track fingerprints. Plus reads 1000 songs per batch, exposes background indexing progress, yields during indexing, and caches metadata for 15 minutes. Writes still invalidate the index; external changes can be refreshed manually. Tag loads 1000-song pages and replaces the UI list after completion; the browser builds its directory tree.

Apply each patch at its pinned commit, install repository development dependencies, and run `pnpm exec tsc --noEmit`. Verification used Plus builder/SDK 2.9.4 and Tag builder/SDK 2.18.0. Use a fresh output directory to avoid bundling stale hashed assets twice:

```sh
node --input-type=module -e "import {buildPlugin} from '@songloft/plugin-builder'; await buildPlugin({cwd:process.cwd(),outDir:'dist/roomtone-build'});"
node scripts/plugin-overrides/check-ui.mjs <plus-source> <tag-source>
```

Update through `/api/v1/jsplugins/upload`, retaining old ZIPs for rollback. Do not edit database hashes or widen permissions. No scraping, tag writes or playback is performed.

2026-10-29 verification: database and metadata bridge tests pass. Two existing Windows path-fixture failures in the full plugin suite, together with the new test, pass in the Linux container on leonFN. The existing Plus UI test passes. An isolated full-library run reads 70,518 songs: Tag paging takes about 44 seconds; Plus indexing takes about 75 seconds and yields 5,421 directory nodes. Directory song pages return correctly without 504 errors. Initial indexing still takes time and displays progress. Original music is mounted read-only during isolated verification.
