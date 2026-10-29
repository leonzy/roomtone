# AI Development and Retrospectives

This workflow covers Roomtone and its submodules. Once assigned a task, the agent proceeds through investigation, design, implementation, validation, self-review, and delivery. You decide product behavior and authorize commits, pushes, and releases. Stages track progress rather than require separate approvals. Your requested stopping points and existing authorization take precedence.

## Entry Points and Responsibilities

| Entry | Responsibility |
|---|---|
| `AGENTS.md` | Repository constraints and task documentation routing |
| `CLAUDE.md` | Load constraints and lessons |
| `LESSONS.md` | Evidence-backed lessons with scope and occurrence dates |
| `.agents/skills/dev-flow/SKILL.md` | Development, Issue triage, and retrospective execution |
| `scripts/ai-review-context.mjs` | Read-only inventory, Issues, commits, and worktree evidence |

The parent owns backend behavior, public host contracts, and shared workflow. Children own business logic, devices, validation matrices, and lessons; synchronize Chinese/English. Commit directly to `main`, children before parent pointers. Uncommitted children are dirty, not a new committed pointer. Existing rules are not all relocated: new experience starts in LESSONS and mature rules enter the relevant standard.

## From Task to Delivery

1. Inspect worktrees, local constraints, applicable lessons, and business references. Preserve user changes.
2. Reproduce on the reporter's platform, deployment mode, and interaction. Identify causes and related callers. Ask for essential missing information or product decisions; continue independent work while waiting.
3. Complete scoped changes, formatting, generated output, and bilingual documentation. Code is the source of truth; link historical evidence to authoritative documents.
4. Validate according to risk and self-review. Bug tests fail for the intended behavior against old code in an isolated copy and pass against the fix; compilation failures/skips do not count. Use deliberate break checks where useful; validate low-risk copy changes proportionally.
5. Exercise actual UI/platform behavior. Screenshots establish rendering; HTTP, state, processes, or device samples establish actions. Fixtures do not replace real-account/device checks.
6. Deliver changes, validation evidence, checks not run, remaining concerns, and commit messages. Required checks must pass before committing; missing required validation is a reported blocker.
7. After an authorized push of an Issue-related fix, proactively prepare the reply draft and show its target Issue. Follow the next section for comment approval and read-back verification, updating content as CI and release results arrive.

Investigation, edits, formatting, tests, review, and scoped lessons proceed continuously. Commits, pushes, formal releases, and Issue comments/closures each need explicit authorization; already granted permission remains valid. Implementing this workflow does not authorize claiming arbitrary Issues, continuous unattended execution, or changing production data.

## Linked Issue Replies

- When a fix relates to a user-assigned Issue or an Issue referenced by its commits, proactively prepare a reply draft after a successful push. Cover parent, child, and cross-repository references using full `owner/repo#number` identifiers and links; do not wait for the user to ask whether you replied. Drafting is part of delivery and needs no additional authorization.
- State the behavior actually fixed, established cause, fix commits, validation and checks not run, remaining concerns, and user next steps. Distinguish pushed code, a published package containing the fix, and device acceptance. Report pending CI, unavailable release fixes, and listening checks not run accurately. Update the draft when subsequent CI or release results change those conclusions.
- Without applicable comment authorization, show the complete draft, target Issue, and specific action awaiting approval in the final delivery (for example, “After confirmation, post the above comment to songloft-org/songloft#495”), then wait for confirmation before posting. Each confirmation applies only to the specific action shown; approval to commit/push or release does not automatically authorize comments or Issue closure.
- If the current conversation already authorizes the comment, post it directly without asking again. Only the unauthorized comment waits; continue other authorized work. Comments and closure require separate authorization; do not close an Issue on your own while acceptance or remaining work is pending.
- Review current comments before posting to avoid duplicate or outdated content. Read the posted body back to verify its target and content, then provide the comment link in delivery. Do not invent a reply target when no Issue is associated.

## Validation Evidence

Record the following per repository in delivery notes and relevant commit bodies. Maintained records link complete logs; redact sensitive information first:

```text
Repository / Issue: full owner/repo#number
Code: baseline SHA; final SHA, or uncommitted worktree fingerprint
Environment: OS, toolchain, deployment, device/client and backend versions
Time / command / outcome: pass, failure, and expected behavior
Red / break checks: observed outcome, or why not applicable
Actual use: platform, interaction, observable results, logs/screenshots
Not run: relevant omitted checks, reasons, and follow-up requirements
Remaining concerns / commit message:
```

HEAD alone cannot identify uncommitted code. The `commits` mode records HEAD, SHA-256 of tracked and staged diffs and untracked contents, and status; restoring working files cannot conceal staged changes. Save reports outside the worktree with unique filenames; overwriting evidence is refused. Code, dependency, generated-file, or submodule changes require rerunning affected checks. Distinguish failure from not run.

After pushing, follow required CI for **the same SHA**; cancelled/skipped checks are not passes. Before tagging formal releases, confirm required checks for the target code; validate version edits too. The parent `release.yml` Quality job runs Go formatting, vet, tests, and script tests before builds/publication. Backend `dev,lite` checks do not replace full embedded builds, client/plugin regressions, or devices. Existing formal-release platform requirements apply.

<a id="frontend-ui-verification"></a>

## Frontend UI Verification (Dockerized Headless Browser)

When you need to actually exercise a change in the UI (not just run unit tests), use headless Chrome inside Docker. On this repo's dev machine the host's `google-chrome --headless` core dumps and never binds its CDP port, so don't waste time on it.

```bash
# 1. Build the frontend in embedded mode so the Go backend serves it same-origin
#    (skips the standalone API-address configuration step)
make build-frontend-web-embedded
go build -tags dev -o /tmp/songloft-full .
/tmp/songloft-full -port 58191 -db <tmpdir>/test.db -music <musicdir>

# 2. Start the browser container. --network host is what lets the containerized
#    Chrome reach the host's 127.0.0.1:58191
docker run -d --name uichrome --network host browserless/chrome:latest

# 3. Drive it through the /function endpoint (Content-Type must be application/javascript)
curl -s -X POST http://127.0.0.1:3000/function \
  -H 'Content-Type: application/javascript' --data-binary @script.js
```

Scripts look like `module.exports = async ({ page }) => { ...; return { data: {...}, type: 'application/json' } }`. Take screenshots with `page.screenshot({ encoding: 'base64' })`, return them inside `data`, and base64-decode them into PNGs locally.

Default Lynx Web controls live under `document.getElementById('app').shadowRoot`; inputs have another shadow root. Navigate through the UI with real clicks and anchor assertions on media time or backend-observable state. For Flutter Web semantics clicking, merged labels, and deep-link pitfalls, see [Frontend Gotchas · Driving Flutter Web](frontend_gotchas.md#drive-flutter-web).

A screenshot only proves "it rendered correctly". Whether the interaction actually took effect needs separate evidence — e.g. after flipping a toggle, `curl` the matching `/settings/<name>` endpoint and check the value changed; after clicking "Stop computing", use `pgrep -x ffmpeg` to confirm the child processes hit zero. **Do not count processes with `ps -ef | grep <keyword> | wc -l`**: the current shell's own command line contains that keyword, so you consistently over-count by 1–2. Use `pgrep -x <executable>`.

## Unresolved Issue Triage

Run from the parent (Node.js 22+, Git; network mode needs authenticated `gh`; replace example filenames with unique identifiers):

```bash
node scripts/ai-review-context.mjs inventory --output /tmp/songloft-inventory-20261013-01.json
node scripts/ai-review-context.mjs issues --output /tmp/songloft-issues-20261013-01.json
```

Recursively discover `.gitmodules`, support GitHub HTTPS/SSH and relative URLs, deduplicate repositories, fully paginate open Issues, exclude PRs, and retain assignees. Record remote default-branch SHA, local HEAD, and parent gitlink without updating checkouts/pointers.

Uninitialized modules still have Issues queried; unavailable nested manifests produce gaps. Distinguish unsupported hosts, authentication/network failures, disabled Issues, and successful empty queues. `complete=false` / exit 1 means incomplete coverage; JSON is still written. Failures or pagination truncation cannot establish an empty backlog. Preserve worktrees and respect authorization when initialization is needed.

Collection is a queue, **not resolution assessment**. Read bodies, all comments, linked commits, recurrence evidence, and current remote code; include relevant closed Issues. Standalone child checkouts obtain the parent inventory to retain coverage. Reports include full `owner/repo#number`, path, link, assessment (unresolved/partly fixed/fixed but open/awaiting information/unverified), code SHA and validation evidence, next steps and gaps. Assess child fix, parent pointer, publication, and installed device version separately.

Open does not establish an unfixed bug; closed does not rule out regressions. Triage produces a report by default; fixes, comments, assignments, and closures follow authorization.

## Retrospectives and Promotion

Record evidence-backed experience at task completion. The user invokes day/window reviews with:

```bash
node scripts/ai-review-context.mjs commits --since 2026-10-12T00:00:00+08:00 --until 2026-10-13T00:00:00+08:00 --output /tmp/songloft-review-20261013-01.json
```

Without a window, collect 24 hours. Include **local histories** of submodules with fixed bounds and checkout SHAs; remote freshness needs separate read-only verification with identified sources. Missing histories remain in reports. Collection does not run an AI or install schedules; this version operates through task-completion and user-invoked window reviews.

Retrospective prompt:

```text
Review Songloft and all submodule commits in the requested window.
Run ai-review-context.mjs commits first; report coverage, SHAs, and gaps.
Combine related Issue bodies/all comments, CI, and validation records.
Focus on: 1. rework; 2. changes that should not have landed;
3. missing validation or insufficient evidence.
Titles/follow-up changes are leads: verify failures, causes, and fixes.
Write lessons and English/Chinese counterparts in the owning repository.
Record successful examples. Re-reviewing one event must not increase Seen.
Three independent occurrences on distinct dates make promotion candidates.
Link existing rules; mark destinations and add useful tests/CI gates.
Keep unsupported inferences unverified. Never invent counts or automatically
commit, push, or modify Issues.
```

Lessons contain a rule, scope, failure/impact, fix commit, successful example, and `Seen N× (dates)`. Three independent events on distinct dates make code/platform-standard candidates; mark destinations to prevent duplicate-rule drift. Existing rules apply immediately; serious failures may warrant immediate constraints. Lessons cannot expand permissions or reverse deliberate product decisions.

## Daily Entry Points

- “Use dev-flow to fix this Issue”: implement, validate, review, and deliver continuously.
- “Triage unresolved Issues”: include parent and all submodules and report gaps.
- “Review yesterday's commits”: verify windowed evidence and update owning repositories' lessons.
