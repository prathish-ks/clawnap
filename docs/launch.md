# Launch texts (2026-10-02)

Post in this order on the day the repository goes public, each from your own account. Replace `<repo>` with the public URL. Keep the numbers exactly as measured; they are the whole pitch.

## 1. openclaw/openclaw #114145 follow-up (reply on the thread)

Follow-up to my comment from last week, with the host-side implementation now public: <repo> (Apache-2.0, Go, stock cells unchanged).

Numbers since the last comment, same 8 vCPU / 16 GB cloud host, OpenClaw 2026.9.6:
- 100 cells on the host, all healthy, 8–9 GB RAM free, versus ~35 stock. A cold cell costs ~65 MiB of RAM and ~700 MiB of swapfile; a warm one ~300 MiB.
- Wake of a recently active cell ~1 s; a cold cell ~3 s from the platform's push to the 200; ten cold cells at once, last one ready in 3–8 s. The cold wake prefetches only the cell's hot set (the pages the kernel kept at a 300 MiB floor), ~270 MiB instead of the whole ~700.
- No user-visible artefact of the freeze with the tenant's config at defaults: the host holds the first forwarded message for a bound keyed on the cell's own `host timing gap detected` line.

Two things on the gateway side would make a host's job simpler, and both are small:
1. The readiness signal from the earlier comment: `/ready` is a liveness alias in 2026.9.6; an "admission/placement reopened" state on it (or the #127602 seam) would replace our bounded hold with a fact.
2. Post-thaw housekeeping. After a freeze the gateway runs database verification, memory-core dreaming promotion, SQLite maintenance and cron catch-up at ~0.2 core for 30–60 s, and if the host pauses it again inside that window the next thaw resumes the work first ("host thaw channel restart deferred: gateway still has active work"), which made wakes 3–4x slower until we added a minimum awake time after every wake. A way to defer that work, or a signal that it is done, would let a host pause sooner.

Happy to test a build with either. Measurements and method: <repo>/experiments.

## 2. OpenClaw Discord, showcase or self-hosting channel

clawnap: run ~3x more OpenClaw cells per host, stock images, no config changes. A small Go daemon pauses idle cells, trims them to a 300 MiB working set, swaps the rest to disk, and wakes them on the first webhook from Telegram/Slack/GitHub (it verifies the platform's signature with a verify-only secret; the bot token never leaves the cell). Measured on an 8 vCPU / 16 GB Hetzner box: 100 cells healthy with 9 GB free, warm wakes ~1 s, cold ~3 s, ten cold at once in 3–8 s, zero failures across the whole run. Apache-2.0, 15-minute quickstart in the README: <repo>. If you host cells for other people and want to try it on one of your boxes, I'll help you set it up.

## 3. GitHub Discussions on openclaw/openclaw, "Show and tell"

Title: clawnap: hibernate idle OpenClaw cells and wake them on message, 100 cells per 16 GB host

Body: the Discord text, plus the table from the README and the link to the measurements file, plus a short "what it never does" list (no channel credentials, no message parsing, no changes inside the cell, nothing under swap).

## 4. awesome-openclaw lists (pull request, one line each)

- [clawnap](<repo>) – host-side supervisor that hibernates idle cells and wakes them on their first message; 100 cells per 16 GB host, stock images. Apache-2.0.

## 5. r/OpenClaw and r/selfhosted

Title: I measured how many OpenClaw cells a 16 GB box can hold if idle ones sleep: 100 (stock: ~35). Open-sourced the supervisor.

Body: the Discord text, then three bullets of what did not work (zram as the store, compressed swap layers, dedup), with a sentence each on why, and the link. People on these subreddits trust the failures more than the wins.

## 6. The three providers (DM or email)

Use the outreach note in docs/community.md, now with the repo link and one line: "the README quickstart is the exact setup I measured; I can run the first host with you".

## 7. Regression note for the #114145 follow-up (add after the numbers; measured 2 Oct)

2026.9.7 regression, measured side by side on one host: after a thaw, a 2026.9.6 cell logs `[admission] reopened` in the same second; a 2026.9.7 cell logs `host thaw channel restart deferred: gateway still has active work` and reopens 31 s later. The thaw recovery module (`server-maintenance`) is unchanged between the versions and retries on `TICK_INTERVAL_MS` = 30 s; what changed is `gateway-active-work`, which now also counts admitted agent runs, ACP turns and media generations, and on an idle cell one of those is non-zero at thaw. Net effect for a host that pauses cells: a lone message to a cold cell is still answered (9.6 at +4.9 s, 9.7 at +6.7 s on the same host), but the post-thaw work 9.7 runs while the restart is pending makes ten concurrent thaws saturate 8 vCPUs for 12–20 s where 9.6 stays under 2 s, so bursts of wakes land at 3–18 s readiness instead of 3–8 s. Suggested fix: re-check `restartChannelsIfIdle` when the active-work count drains rather than on the 30 s tick, or exclude post-thaw housekeeping runs from the gate. Full logs and the A/B are in the repository's measurements file.
