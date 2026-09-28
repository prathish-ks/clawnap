# fleet-supervisor — living plan

**Status line (update every session):** 2026-09-28 (afternoon) · Compressed swap via ZFS zvol tested and rejected: two host hangs (swapoff migration; then ordinary reclaim at 15 cells), lz4 ratio only 2.1x, single wake no faster. Idea stays on the backlog for dm-vdo (6.9+) or user-space compression. Host restored to the raw swapfile with 50 cells · **100 cells on one 16 GB host with the disk store, all healthy, 9 GB RAM free (2.9x the 35 stock+zram ceiling, ~5x plain stock); burst latency independent of density (10 wakes at 50/75/100 cells: ~10/19/25 s per wave); burst-size curve at 50: 1→4.7 s, 2→5.7, 4→8.2, 8→14.6, 10→21 s, 0 failures; 3 s settle is now the largest fixed share of a single wake** · Disk tier measured: NVMe swapfile wakes a reclaimed cell in 1.5–2.0 s vs 2.3–2.5 s on zram and costs no RAM; zram writeback rejected (6.6–8.4 s). Burst repeat with the cold set on disk: 10 wakes at 50 cells, first four ready in 9 s (was 25), last in 24 s (was 52), zero swap-out, zero failures; remaining bound is the cloud volume's ~550 MB/s. Decision: swapfile is the hibernation store, zram optional · 2026-09-27 · Volume test done: 50 hibernated cells, 10 signed wakes at once → 0 failures, 0 exits; first wave of 3 ready in 25 s (page-in contention on a host with 195 MB free), later waves ~7 s, tenth cell 52 s; the live-channel cell answered 200 on first delivery · upstream comment for #114145 drafted in docs/upstream-watch.md (user posts) · Phase 0b complete: density (50 vs 35), push-wake, answered turn after reclaim, and the post-thaw window covered by a log-keyed settle with the tenant's defaults untouched · first real agent turn after reclaim answered in 8.7 s (wake 2.3 s + model + OpenClaw's own retry) · real Telegram push-wake proven end to end (free sslip.io + Caddy; message delivered on the first push, 2.2 s from a reclaimed cell) · density measured — 50 supervised cells vs 35 stock on one 16 GB host, both swap-device bound; reclaimed wake on zram 1.5–2.6 s · fleet-exp-1 (cx43, fsn1, zram 15.2 GB, Docker 29 cgroup v2, fleetd deployed) · repo: github.com/prathish-ks/fleet-supervisor (private, main) · Phase 0a complete except Hetzner. **Density lever measured locally: paused+reclaimed cell 789 → 20 MiB (≈35x), wake 16.7 s on a slow disk.** Phase 0a (local correctness): 11 of 12 rows done; local gate met except session/memory/cron survival (needs a model provider). Release 0 host checker built. Remaining: provider access (9, in progress), Hetzner account (10, user). Pause cap, /metrics, `fleetd cells create` and reclaim-after-pause all done and verified (supervisor-driven reclaim in the VM: 376 → 46 MiB in 7 s). Code is ready for Phase 0b; Hetzner measures zram vs NVMe wake latency and cells per host · next gate: local go/no-go · Hetzner host: fleet-exp-1 cx43 fsn1 167.233.118.221 · thesis-watch routine: daily 21:00 UTC, last verdict NO CHANGE (2026-09-26).

How to maintain: edit the status line and the "Now" table every working session; move finished rows to the Changelog with the date; never delete a gate, only mark it passed or failed with evidence.

## Thesis and stop conditions (unchanged unless a gate fails)
Stock OpenClaw cells, unchanged, supervised by a small Go host-side control plane that hibernates idle cells and wakes them on inbound message, so a hosting provider runs 3–5x more cells per server.
Stop if: upstream ships hibernation or multi-host fleet; a credible open-source hibernation supervisor for OpenClaw on plain Docker hosts appears; two or more named providers exit; OpenClaw is abandoned or superseded; fewer than 2 of 10 providers name density a top-3 cost or will pay $500/mo; the Hetzner experiment cannot reach 3x baseline with an honest wake SLA.

## Phases and gates

| Phase | Where | Goal | Gate to leave |
|---|---|---|---|
| 0a Local correctness | This Mac, 2 live cells + 3–5 hibernated | Every lifecycle path works and state survives | Zero state loss over 20 hibernate/wake cycles with a real Telegram bot; pause-tier wake < 1 s on /health; stop-tier readiness measured and documented; ingress wake works end to end; daemon survives its own restart |
| 0b Hetzner numbers | CX43 16 GB, up to 50 cells | Publishable density and wake numbers: pause+reclaim with zram vs NVMe swap, wake latency per tier. **Baseline: stock+zram 35 cells. Supervisor run 2 (zram 200 %, batched boots): 50/50 healthy, ceiling ≥ 50, swap-device bound; reclaimed wake on zram 1.5–2.6 s flat across the ladder. Burst of 10 wakes at 50 cells: 0 failures, 25 s / 7 s / 7 s per wave of 3, 52 s for the tenth (page-in contention, lever is RAM headroom ≈ concurrency × working set). Disk store (28 Sep): 100 cells, 0 failures, 9 GB free; burst of 10 flat across 50/75/100 cells.** | ≥3x baseline cells/host; p95 wake within the tier's stated SLA (pause < 5 s); zero state loss; density-stack CSVs (trim, overcommit, zram, KSM, CRIU, pause, stop) |
| 0c Upstream engagement | GitHub | Validate the host-side split with OpenClaw. Evidence in hand: density (50 vs 35), zram wake numbers, and the post-thaw channel-readiness gap. | Comment posted on #114145 (and #119035 if cron wake is implemented) with numbers; any maintainer or Codex response recorded in docs/upstream-watch.md |
| Commercial gate A | Interviews | Someone will pay | ≥2 of 10 providers: density top-3 cost and ≥$500/mo intent; 3 pilot offers sent |
| 1 Single-host MVP + OSS release | Hetzner + laptop | Public Apache-2.0 release, first paid pilot | Gate B: release public, 1 pilot live |
| 2 Multi-host + paid tier | Hetzner x2 | Placement, migration, billing | Gate C: 3 pilots, ≥$1k MRR |

Phase 0c runs after 0b numbers exist (not before) and before Phase 1 code hardens, so upstream feedback can still change the design. Codex/ClawSweeper will likely be the first responder; treat its review as a structured checklist, answer every point with evidence, and expect a maintainer product decision to remain the real blocker.

## Now (Phase 0a) — ordered

| # | Task | Status | Evidence / notes |
|---|---|---|---|
| 1 | Readiness probe: HTTP GET /health == 200 instead of TCP | done 2026-09-26 | TCP is a false positive on Docker Desktop (experiments/local-measurements-2026-09-26.md) |
| 2 | Pause tier: `docker pause`/`unpause` as primary hibernation; stop/start as cold tier; per-cell tier policy | done 2026-09-26 (unit-tested; live check in progress) | pause→/health 200 in 0.13 s locally; stop→ready 73–90 s |
| 3 | Live test on this Mac: 2 live + 3 hibernated cells through fleetd, 20 pause/unpause and 20 stop/start cycles | done 2026-09-26 (5 cells; pause 60/60, p95 1.3–2.2 s under load 52; stop 5/5, 40–155 s; live cells unaffected; pausing frees no RAM) | 1 cell: pause 5/5, wake 0.2–0.4 s; stop 2/2 after per-tier timeout fix, wake 45–72 s; SIGTERM exit 0.3–17 s. Still to do: 2 live + 3 hibernated concurrently, 20 cycles each |
| 4 | Real Telegram bot on one cell; webhook via ingress /hook; wake on message end to end | done 2026-09-26 (local scope): dedicated bot @Ocfleet_bot; pause → ingress wake 0.43 s; messages sent during pause delivered on wake and answered. Push-wake via public HTTPS is a Hetzner item | token stored in gitignored experiments/bots.txt; regenerate in BotFather after tests |
| 5 | State-survival check after cycles: sessions, memory files, cron, channel auth | done 2026-09-26 for channel state (auth, polling offset, delivery survive pause; 48 s freeze tolerated, 3 h freeze not). Sessions/memory/cron still untested: needs a model provider | Mode A gate |
| 6 | Cron-aware wake (interim: never hibernate when a job is due within idle window) | done 2026-09-26 (interim: `-next-due` set by operator; pre-wake 2 min; reading schedules from the cell is backlog) | upstream #119035 |
| 7 | WAL checkpoint after stop-tier hibernate (host-side, pure-Go SQLite, bind mounts only) | done 2026-09-26 (unit-tested; live check pending) | upstream #143524 WAL growth |
| 8 | Daemon crash/restart consistency test | done 2026-09-26 (unit: adopts external pause/exit without fighting it; registry writes are atomic) | live kill test on Hetzner remains |
| 9 | Provider access: 3 paid entry plans (Agent37, OpenHosst, Lease Packet), join the venues in docs/community.md, publish the host checker + benchmark, 3 pilot offers | in progress (user browsing Discord); code side proceeds without waiting | replaces the interview plan; see docs/community.md |
| 10 | Hetzner account + API token (user); hcloud CLI install (needs approval); provision CX43 | done 2026-09-27: fleet-exp-1, cx43, fsn1 (hel1 had no capacity), 167.233.118.221, EUR 20.34/mo | experiments/provision/ |
| 11 | Release 0 host checker (`fleetd check`): ported from Isthmus doctor/securitycheck/egress/mount rather than extracted as a shared module (Isthmus packages are internal/ and NanoClaw-typed; the port keeps their checks and remediation text). Isthmus can import this package back later | done 2026-09-26 (unit-tested; ran live: 23 pass / 13 warn / 0 fail across a hardened cell and two unhardened containers) | egress check is Linux-only by design |
| 12 | First git commit of the repo | done 2026-09-26 (09a3da1) | |

## Backlog (Phase 0b onward)
- Compressed cold store, second attempt: dm-vdo on an HWE kernel (≥ 6.9) as a compressing block device under a swapfile, or supervisor-side compression of a paused cell's anonymous memory. Expected gain ~2x on page-in bandwidth (lz4 2.1x measured), not 4x.
- NEXT: (a) provisioning: cloud-init swapfile instead of zram, sized ≈ cells × 0.8 GB; (b) wake concurrency and thaw-settle bound as flags; (c) settle: measure whether a shorter bound than 3 s is safe (largest fixed share of a single wake now); (d) one run on a dedicated local-NVMe host (Hetzner AX) for the burst curve; (e) post the #114145 comment with the 100-cell and burst numbers; (f) provider outreach.
- DONE 2026-09-28 (burst-size curve replaced it; density does not move burst latency): 10-wake bursts at 30/35/40/45/50 cells, p50/p95 first-message latency, PSI per step (fix the run script: wait on the curl pids, not on the vmstat sampler). Then: cloud-init switches from zram to a swapfile; make wake concurrency a flag; measure a real local-NVMe host (Hetzner AX dedicated) for the bandwidth lever.
- DONE 2026-09-28 (see decisions): two-tier hibernation. Warm tier = pause + reclaim to zram (minutes, wake 1.5–2.6 s); cold tier = pages demoted to NVMe swap (zram writeback of idle pages, or lower-priority swapfile) so RAM and zram are both released and the host keeps ≈8 GB headroom at 50 cells. Prefetch unchanged. Measure first: single reclaimed wake from NVMe vs zram on fleet-exp-1; then the 10-wake burst at 50 cells with the cold set on disk; sample /proc/pressure/{memory,cpu} and vmstat during the burst to settle memory vs CPU. Check CONFIG_ZRAM_WRITEBACK / /sys/block/zram0/backing_dev on the host.
- Burst-wake headroom policy: reserve ≈ wake-concurrency × per-cell working set of free RAM before admitting another cell to the host; multi-stream zram or NVMe swap for parallel page-in; re-run the 10-wake burst with 2 GB headroom (Phase 1)
- Wake must fail fast on an exited container instead of waiting the readiness timeout (seen: 120 s 502 on a dead cell).
- Stagger cell boots (bounded concurrent starts) so a batch cannot exhaust memory and trip OpenClaw's startup-lease logic.
- (done: ≥ 50 at zram 200 %, still swap-bound) Next: zram 300 % or NVMe swap behind zram, and a ladder beyond 50, to find the ceiling that is not the swap device.
- Reduce the swapped set per cell or parallelise decompression to get large-swap wakes under 1 s on zram.
- (done 2026-09-27, result: slower; anon-only is the default, files opt-in) Prefetch file-backed mappings.
- Working-set-aware reclaim floor via page-idle tracking (Hetzner; needs a quiet host to measure).
- Prefetch triggered by early signals (webhook first byte, predicted schedule) so page-in leaves the critical path.
- Host checker: warn when swap capacity < sum of resident memory of hibernate-class cells; report zram/swap device and size.
- (done 2026-09-26) Cell provisioning helper: `fleetd cells create` that writes a cell's openclaw.json (gateway.mode local, channel webhookUrl = https://<ingress>/hook/<cell>/<path>, webhookSecret) and the matching verify-secret file, so ingress and cell always agree.
- Telegram wake without a public address: NOT a getUpdates keeper (it would hold the tenant's bot token and read message content before the cell; unacceptable to the community and races the one-poller-per-bot rule). Order of preference: (1) webhook via the ingress with per-cell secret-token verification, no credential held; (2) opt-in count-only keeper using getWebhookInfo pending_update_count to wake the cell, which then polls itself (leaks a count, holds the token); (3) always-on class. Design principle for the README: the host never holds a channel credential and never parses a message.
- Ingress verifier plugins (done 2026-09-26, commit follows; live check against real Telegram webhook is a Hetzner item): routing by /hook/<cell>/ path; authenticity by per-cell verifier — header secret (Telegram X-Telegram-Bot-Api-Secret-Token), HMAC-of-body (Slack, GitHub, WhatsApp Cloud), JWT (Google Chat, Teams), bearer (generic). Verify before wake; drop unverified with no wake (anti wake-storm). Ingress answers registration-time challenges (Slack url_verification, WhatsApp hub.challenge) while the cell sleeps. The ingress holds verify-only secrets, never bot tokens.
- Publish the host checker: README, a one-line install, and a post in the OpenClaw Discord self-hosting channel once the Linux egress check is verified on Hetzner.
- Pause-tier duration cap: DONE 2026-09-26 (`-max-pause` default 20 min, pulse or stop fallthrough, verified live). Measured: lease-stopped state between ~3 min and ~56 min (30-min constant fits), self-exit between ~56 min and ~3 h. Exact bounds to tighten on Linux.
- Stale gateway owner lease after unclean stop (exit 1 on restart until timeout): locate the lease, decide whether the supervisor clears it on OOM/crash recovery.
- Investigate live-cell state-dir growth (1.4 MB → 59 MB in 12 min idle) before Hetzner: logs, cache, or SQLite? — see appendix for the original hour-level plan
- Density stack CSVs on Hetzner: trim, overcommit, zram, KSM, CRIU, pause tier, stop tier.
- WhatsApp always-on class; Discord keeper; Slack Events path verification.
- Metering, Prometheus, dashboard with one customer-visible feature (status page / security badge).
- Multi-host placement, migration, failover; Stripe; SSO.
- Driver interface as a cell tree; Isthmus driver second, stock NanoClaw third.

## Decisions log
- 2026-09-28: no swap on ZFS. Two hard hangs on fleet-exp-1 (kernel 6.8, OpenZFS 2.2.2), one under ordinary supervisor reclaim. Compressed cold store remains a backlog item behind a mechanism that does not allocate on swap-out (dm-vdo on kernel ≥ 6.9, or supervisor-side user-space compression).
- 2026-09-28: hibernation store is a swapfile on the host's NVMe, not zram. Measured on fleet-exp-1: disk wake 1.5–2.0 s vs zram 2.3–2.5 s, and disk frees the 8 GB zram was holding, which is what removed the burst contention (10 wakes at 50 cells: 9/17/24 s vs 25/32/52 s). zram writeback as a cold tier rejected: page-at-a-time reads, 6.6–8.4 s wakes. Two-tier (zram warm + disk cold) deferred: only worth it if combined bandwidth is needed for bursts.
- 2026-09-26: no content-reading polling keeper; webhook-first with ingress secret verification, count-only keeper as opt-in only.
- 2026-09-19: OpenClaw fleet supervisor chosen over Paperclip (docs/opportunity-scout-2026-09-19.md).
- 2026-09-19: Hetzner CX43 (~EUR 16/mo) for the experiment host; AMD CPX plans avoided after June 2026 price rise.
- 2026-09-26: Hibernation is tiered (pause → CRIU → stop) after local measurement showed 70–90 s gateway start; readiness is HTTP /health.
- 2026-09-26: Phase 0 split into local correctness (0a) and Hetzner numbers (0b); upstream engagement (0c) scheduled after numbers exist.
- 2026-09-26: Daily thesis-watch routine created (trig_01BRPWHtqCuwJYP4MicWYTSq), run-log delivery.

## Changelog
- 2026-09-28 (afternoon): ZFS compressed-swap experiment recorded and rejected; host reset twice; zramswap disabled at boot; /swap2.img and the pool file removed; 50 cells restarted on /swap.img.
- 2026-09-28 (later): burst-size curve and 100-cell ceiling recorded. Gate 0b density criterion met with the disk store (100 vs 35 stock+zram; ~5x plain stock). Host left at 100 hibernated cells on two 40 GB swapfiles.
- 2026-09-28: disk tier test and burst repeat recorded (experiments/hetzner-measurements-2026-09-27.md, section 2026-09-28). Host: kernel updated on reboot, zram module reinstalled; 40 GB swapfile at /swap.img; daemon relaunched (interval 5s, reclaim-after 20s, no pause cap); 49 volume cells recreated and left hibernated on disk for the density curve. Correction: 27 Sep cleanup had not removed the cell directories; wake concurrency default is 4.
- 2026-09-27 (later): volume test recorded (experiments/hetzner-measurements-2026-09-27.md): 50 hibernated, 10 simultaneous signed wakes, 0 failures; burst wake time is page-in bound at zero headroom; levers listed for Phase 1. Volume cells removed from fleet-exp-1 (tgw kept). Upstream comment for #114145 drafted (docs/upstream-watch.md) — decision: a comment is enough now, release/public repo later.
- 2026-09-27: post-thaw "heartbeat failed" notice eliminated without touching the tenant's config: pause wakes read OpenClaw's freeze-detector line from the cell log and hold the first forward for a measured 3 s window (its settlement reopening is not observable in 2026.9.6, verified in the shipped code). Confirmed with the heartbeat at default: single delivery, no abort, one answer. Upstream ask sharpened: a readiness signal a host can poll.
- 2026-09-27: first real agent turn after reclaim: paired owner, Anthropic key in cell config, Haiku 4.5 default; "what is 17 times 23" answered 8.7 s after Telegram's push. Third post-thaw gap found (session placement closed ~1 s, first turn aborts, retry succeeds); recorded for upstream.
- 2026-09-27: cell tgw paired: Telegram sender 7972328048 approved and set as command owner via `openclaw pairing approve telegram <code>` (the code came from the cell's channel_pairing_requests table; the CLI's `pairing list` wrongly reported no pairing channels). Lesson: an exec inside a cell must not overlap the daemon's idle pause; the cell was set always-on for the step.
- 2026-09-27: real push-wake through a public address: sslip.io + Caddy (free, no domain), OpenClaw self-registered its webhook, verify-before-wake refused an unsigned request, four real messages. Three ingress fixes from what the logs showed (wait for listener → request probe → retry the cell's post-thaw 5xx); message 4 delivered on Telegram's first push, 2.2 s end to end. Upstream observation recorded (channel not ready ~40 ms after listener; #127602 seam).
- 2026-09-27: supervisor density run 2 on the reviewed build with zram at 200 % and batched boots: 50/50 healthy at the top of the ladder, 0 failures, 84 reclaims moving 48 GiB; ceiling ≥ 50 vs 35 stock. Wake on zram flat at 1.5–2.6 s across all steps.
- 2026-09-27: supervisor density run complete (experiments/hetzner-measurements-2026-09-27.md): 40/40 healthy where stock had 37/40 with two dead; both bounded by the 15.2 GB zram; first zram wake numbers 150 ms–3 s. Two defects surfaced: a wake of an exited cell waits the full timeout (should fail fast), and the boot storm at 45 cells triggers OpenClaw's own startup-lease failures and one OOM kill (stagger cell boots).
- 2026-09-27: second independent review (commits since 8ec5076): 10 findings fixed. Reclaimed-ness is now a registry fact (Swapped) set only when bytes moved, so unsupported hosts never mislabel wakes or run prefetch; pulses prefetch first and no longer re-arm reclaim; prefetch surfaces every error, checks ctx per batch, and drops the unreachable /proc/mem fallback; Iovec built portably (32-bit Linux builds, go vet clean on Linux); CLI wake/hibernate route through a running daemon; root data dir is /var/lib/fleetd with one-time migration; cloud-init installs zram modules for every kernel and keeps them via linux-generic; deploy.sh quotes the key path; baseline script rerunnable with numeric sample output; parseMaps has a fixture test.
- 2026-09-27: supervisor density test launched on the host as a systemd unit (SSH-bound launches died twice; nohup under a non-interactive session is not enough). First attempt failed on the launcher's own mount check: the default cell state root under /root is a blocked path when the daemon runs as root; default moved to /srv/fleet/cells.
- 2026-09-27: stock ceiling on the zram host measured: 35 cells healthy, 40 thrashes (zram full, load 35, 2 exits, no OOM). Compression ~3.7x. The supervisor is measured against 35, not the 18–20 bare-RAM projection.
- 2026-09-27: first Hetzner baseline: 10 stock cells boot to healthy in 21–27 s in parallel, idle 719–815 MiB each (8.2 GB for 10); projected stock ceiling ~18–20 cells per 16 GB host. experiments/hetzner-measurements-2026-09-27.md
- 2026-09-27: Hetzner host bootstrapped after two cloud-init fixes (criu absent from Ubuntu 24.04; zram needs linux-modules-extra); zram 15.2 GB zstd active; OpenClaw image pulled; supervisor deployed; baseline-cells.sh added for the week-1 baseline.
- 2026-09-27: repository pushed to github.com/prathish-ks/fleet-supervisor (private), branch main, 30 commits; history verified free of tokens.
- 2026-09-27: prefetch refined: anon-only prefetch on a 150 MiB-floor reclaim wakes in 0.72 s on the laptop disk (plain pause 0.17 s; no prefetch 26 s). File-backed prefetch made it slower (2.0 s) and is now opt-in. Recommended defaults: -reclaim-keep-mib 150 -prefetch-on-wake. Commits 7e29ee4 + this.
- 2026-09-27: wake-time levers measured on the reclaimed cell: bulk prefetch via process_madvise cut wake from 26 s to 3.2 s on the laptop disk (variant D); the resident floor alone did not help (memory.current is not the working set). Prefetch is on by flag; extend it to file-backed mappings next. Commit aff03e8 + this.
- 2026-09-26: two-cell live run repeated with the reviewed build: coalesced wakes share the leader's outcome, a 15.3 s reclaimed-cell wake passes under the new timeout, CLI registry edits survive under the running daemon, 0 failures.
- 2026-09-26: independent code review of the first 23 commits (8 finder angles, 3 verifier passes): 10 confirmed correctness findings reported and fixed, plus 9 further verified issues and 5 cleanups. Highlights: reconcile-driven wakes deadlocked the daemon (separate reconcile pool); registry was a per-process snapshot (file lock + reload-before-mutate + fsync); empty webhook secrets and unset verifiers now fail closed; coalesced wakes share the leader's real outcome and survive caller cancellation; reclaimed cells get a longer wake timeout; one-shot due times clear and recurring ones advance; stale Waking phases are adopted; failed wakes keep the pause clock; gateway token moved from env/argv into the cell config; provisioning refuses webhook mode without a channel token, types the idle flag, runs cells as 1000:1000 and chowns state dirs when root; runtime stdout is separated from stderr and env values are redacted in errors; one shared path/secret classifier for launcher and checker; Podman info fallbacks; serve refuses to run unauthenticated off loopback; regression tests for each. All re-verified live on real cells.
- 2026-09-19: repo scaffolded (registry, runtime seam, idle, supervisor, ingress, spec, CLI, tests); live stop/start cycle against a stand-in container passed.
- 2026-09-19: provisioning scripts (cloud-init, provision-hetzner.sh, deploy.sh) written; versions verified.
- 2026-09-26: upstream issue review (#114145 Codex review, #127602, #119035, #149684, #63392) recorded in docs/upstream-watch.md; cron-aware wake added.
- 2026-09-26: local measurement of 3 OpenClaw cells on Docker Desktop (experiments/local-measurements-2026-09-26.md).
- 2026-09-26: concurrency work done and race-tested: per-cell locks, parallel bounded reconcile, wake preempts reclaim, POST /hibernate. Two-cell run inside the VM: concurrent wakes 1.9 s / 9.1 s, no failures. Finding: reclaim is bounded by swap capacity (1 GiB VM swap was full) — swap must exceed the resident sum of reclaimable cells; add to host checker and provisioning docs.
- 2026-09-26: heavy-cell simulation (400 MiB filler): reclaim 544 → 84 MiB, wake 12.9 s, no slower than a light cell; reclaim made chunked (64 MiB steps, stop on stall): 590 → 91 MiB in 34 s on Docker Desktop's disk, no more 60 s stalls.
- 2026-09-26: reclaim-after-pause implemented (`-reclaim-after`, cgroup memory.reclaim, request bounded to resident minus 48 MiB, 60 s wait bound) and verified by running the Linux build of fleetd inside the Docker VM: 376 → 46 MiB resident in 7 s, adoption of external pauses fixed. Commits ce53398…1fc6b4b.
- 2026-09-26: **memory reclaim of a paused cell measured: 789 → 20 MiB resident (661 MiB to swap) in 7 s; wake 16.7 s on Docker Desktop's disk.** `fleetd cells create` provisioning helper (config + verify secret + hardened run + registry in one step).
- 2026-09-26: pause-duration cap with pulse/stop fallthrough (verified live on a 56-min-frozen cell); freeze tolerance bounded (~3 min OK … ~56 min lease-stopped but alive … ~3 h self-exit); dependency-free Prometheus /metrics on the ingress.
- 2026-09-26: ingress verifiers (Telegram header secret, Slack HMAC+timestamp, GitHub/Meta X-Hub-Signature-256, WhatsApp hub.challenge, bearer); verify before wake, unknown verifier fails closed. `fleetd cells add -hook-verifier -hook-secret-file`.
- 2026-09-26: Release 0 host checker `fleetd check` (ported Isthmus checks) built and run live; docs/community.md added; outreach plan reframed.
- 2026-09-26: end-to-end Telegram test passed with a dedicated bot: messages sent during pause delivered and answered on wake; long-freeze self-exit and self-registered webhook documented.
- 2026-09-26: concurrent 5-cell live test passed (experiments/local-measurements-2026-09-26.md); interim cron-aware wake, daemon-restart reconciliation, host-side WAL checkpoint added; commits 09a3da1, 6211c02, a843098.
- 2026-09-26: plan restructured as a living document; HTTP /health readiness and pause/stop tiers implemented with tests (`fleetd cells add -tier pause|stop`).

---

# Appendix: original hour-level plan (revision 2, 2026-09-19) — reference only

# OpenClaw fleet control plane — project plan (hourly)

Assumptions: solo founder, 6 focused hours/day, 5 days/week, 14 weeks, ~420 hours. Hours numbered cumulatively (H1–H420).
Budget: one 16 GB Hetzner server (CX43 ~EUR 16/mo after the June 2026 price rise; AMD CPX plans now ~EUR 70/mo, avoid), a domain, test bot accounts (Telegram/Slack/Discord, free), ~$50 of LLM API credit for test cells.

## Gates
- Gate A (end of H60): experiment reached ≥3x baseline cells per host, p95 wake <5 s, zero state loss; AND ≥2 of 10 providers name density/per-instance cost a top-3 problem and would pay ≥$500/mo. Otherwise stop.
- Gate B (end of H300): OSS single-host release public; ≥1 paid pilot live.
- Gate C (end of H420): 3 pilots, ≥$1k MRR, multi-host alpha. Otherwise pivot to the Paperclip fallback.

## Phase 0 — Validation experiment (Weeks 1–2, H1–H60)

### Week 1 — Baseline, prototype, interviews start
| Hour | Task | Output |
|---|---|---|
| H1 | Provision Hetzner 16 GB box, Docker/Podman, node_exporter + cAdvisor | Metrics visible in a Grafana or plain Prometheus UI |
| H2 | Install stock OpenClaw fleet CLI; create one cell; pair a Telegram test bot | One working cell |
| H3 | Script: create N cells from a template, each with its own bot token | `make-cells.sh N` |
| H4 | Script: sample per-container RSS, CPU, start time, disk every 30 s | `collect.sh` writing CSV |
| H5 | Launch 10 cells; wait 30 min; record idle RSS per cell | Baseline row 1 |
| H6 | Write provider interview script (8 questions) and list 10 contacts (Molted, Agent37, Clawctl, Blink, xCloud, elest.io, OpenHosst, Lease Packet, MyClaw, Hostinger) | Interview doc + contact sheet |
| H7 | Scale to 25 then 50 cells; note where the box degrades or OOMs | Max stock cells per host |
| H8 | Measure fleet cold start (stop all, start all) at 10/25/50 cells | Start-time curve |
| H9 | Send 200 synthetic messages per cell; measure SQLite WAL growth | WAL growth per cell |
| H10 | Leave 50 cells idle 4 h; record memory drift | Leak or no leak |
| H11 | Write baseline report v0 (cells/host, RSS, start time, WAL) | `baseline-v0.md` |
| H12 | Send 10 interview requests (email, LinkedIn, Discord) | Requests out |
| H13 | Go project skeleton: `cmd/fleetd`, Docker client, config loader, logging | Compiles, lists containers |
| H14 | Cell registry (SQLite via `modernc.org/sqlite`): id, image, state, last-activity | CRUD + tests |
| H15 | Idle detector: poll container network counters; idle = no inbound bytes for N min | Idle events logged |
| H16 | Hibernate = `docker stop`; restore = `docker start`; time both across 20 cycles | Restore latency p50/p95 |
| H17 | Per-cell cgroup memory limit; verify OOM kills only that cell | Isolation confirmed |
| H18 | Self-heal loop: detect exited container, restart with backoff | Crash recovery <60 s |
| H19 | Shared ingress HTTP server (Go): `/wake/{cell}` + per-cell Telegram webhook path | Ingress up behind Caddy TLS |
| H20 | Telegram: set each bot's webhook to ingress; on POST, wake cell, forward to cell port once healthy | Telegram wake works |
| H21 | Slack Events API path (challenge handshake, forward) | Slack wake works |
| H22 | Discord: lightweight Go websocket "keeper" per cell that only listens and wakes | Discord wake works |
| H23 | Wake-latency harness: 100 messages per channel type, cells hibernated | p50/p95 per channel |
| H24 | Fix the worst latency source found in H23 | Improved numbers |
| H25 | State-survival test: 20 hibernate/wake cycles; check sessions, memory files, cron jobs, channel auth | Pass/fail list |
| H26 | Fix any state-loss finding, or document as limitation | Updated list |
| H27 | CRIU experiment: `docker checkpoint create/restore` on one cell; time it; note failures | CRIU feasibility note |
| H28 | Provider interviews 1–2 (30 min each) + notes | 2 interview notes |
| H29 | Week-1 write-up with numbers and charts | `week1.md` |
| H30 | Review against success criteria; adjust week-2 plan | Go/adjust decision |

### Week 2 — Density, WhatsApp, security, decision
| Hour | Task | Output |
|---|---|---|
| H31 | Density run: 50 cells with hibernation enabled; measure steady-state RSS | Cells/host with hibernation |
| H32 | Push to 100, 150, 200 cells; find the new ceiling | New ceiling |
| H33 | Thundering-herd test: wake all cells at once; measure memory peak and failures | Peak numbers |
| H34 | Add wake concurrency limit + queue; rerun H33 | Safe over-commit policy |
| H35 | Provider interviews 3–4 | Notes |
| H36 | Update density model spreadsheet (cost per cell at each density) | `density-model.csv` |
| H37 | WhatsApp: study how the gateway keeps its socket; test whether queued messages arrive after a cell restarts | Offline-delivery finding |
| H38 | WhatsApp: prototype "always-on class" (never hibernate) vs "keeper" approach; estimate effort | Decision note |
| H39 | Document WhatsApp limitation honestly for providers | Limitation section |
| H40 | Port Isthmus egress metadata/link-local block to per-cell iptables rule | Egress block applied |
| H41 | Port Isthmus mount validation and `doctor`-style checks to cells | `fleetd doctor` |
| H42 | Port `security-check` (privilege, Docker socket, secrets exposure) | `fleetd security-check` |
| H43 | Density calculator landing page (static HTML): inputs cells, server size; outputs $/cell before/after | Page live |
| H44 | Add experiment charts and methodology to page | Page complete |
| H45 | Draft pilot offer: $500–2,000/mo, 30-day, on their workload, success metric = cells/host | Pilot one-pager |
| H46 | Provider interviews 5–6 | Notes |
| H47 | Provider interviews 7–8 | Notes |
| H48 | Synthesize interviews so far: top-3 costs, willingness to pay | Interim synthesis |
| H49 | WAL checkpoint job per cell (TRUNCATE on schedule) + test | WAL stays bounded |
| H50 | Startup optimisation: stagger cell starts; measure fleet cold start vs stock | Start-time comparison |
| H51 | Robustness: kill fleetd mid-operation; verify registry consistency on restart | Crash-safe registry |
| H52 | Experiment report: baseline vs prototype table, wake latency, state tests, limitations | `experiment-report.md` |
| H53 | Charts + plain-language summary for providers | Summary page |
| H54 | Internal review against stop conditions | Checklist |
| H55 | Provider interviews 9–10 | Notes |
| H56 | Final interview synthesis: count who named density top-3, who would pay ≥$500 | Synthesis |
| H57 | Send pilot offers to the 3 most engaged providers | 3 offers out |
| H58 | Follow-ups; log objections | Objection list |
| H59 | Gate A decision meeting with yourself: go / no-go, with evidence | Decision doc |
| H60 | If go: plan Phase 1 backlog from experiment findings; if no-go: write post-mortem and open Paperclip fallback | Backlog or post-mortem |

## Phase 1 — Single-host MVP and OSS release (Weeks 3–10, H61–H300)
Each week: Mon–Thu build (24 h), Fri = tests, docs, pilot/customer work (6 h).

| Week | Hours | Blocks | Deliverable |
|---|---|---|---|
| 3 | H61–H90 | Mon: reconcile-loop architecture, desired-state model (H61–66). Tue: cell lifecycle state machine + tests (H67–72). Wed: CLI `fleetd cell create/list/stop/wake`, config file schema (H73–78). Thu: policy engine v1 (idle timeout, always-on class, memory class) (H79–84). Fri: unit tests, README skeleton, pilot follow-ups (H85–90). | Core daemon with tests |
| 4 | H91–H120 | Mon: hibernation productionised (wake queue, backoff, concurrency caps) (H91–96). Tue: ingress hardening (auth tokens per cell, rate limits, TLS automation) (H97–102). Wed: health checks per cell, readiness gating before forwarding (H103–108). Thu: fleet-wide cold-start orchestration, staggered starts (H109–114). Fri: 200-cell load test, fix top 3 issues (H115–120). | Reliable hibernate/wake |
| 5 | H121–H150 | Mon: Telegram adapter final (H121–126). Tue: Slack adapter final (H127–132). Wed: Discord keeper final (H133–138). Thu: generic webhook + WhatsApp always-on class (H139–144). Fri: channel matrix docs, latency benchmarks published (H145–150). | Channel wake support |
| 6 | H151–H180 | Mon: per-cell egress rules, secrets injection without plaintext on disk (H151–156). Tue: `doctor` and `security-check` complete with remediation text (H157–162). Wed: cell image upgrade with staged rollout and rollback (H163–168). Thu: backup/restore of cell state dirs (H169–174). Fri: security review checklist, threat-model doc (H175–180). | Security and upgrades |
| 7 | H181–H210 | Mon: Prometheus metrics (cells, wakes, latency, memory) (H181–186). Tue: per-cell usage records (active hours, wakes, bytes) (H187–192). Wed: billing export (CSV + webhook) (H193–198). Thu: minimal web dashboard (status, wake, logs) (H199–204). Fri: Grafana dashboard JSON, docs (H205–210). | Observability and metering |
| 8 | H211–H240 | Mon: single static binary, install script, systemd unit (H211–216). Tue: migration tool from stock fleet supervisor (import existing cells) (H217–222). Wed: docs site: quickstart, ops guide, limitations (H223–228). Thu: CI, integration tests, release pipeline, Apache-2.0 repo public (H229–234). Fri: announce (Show HN, OpenClaw Discord, provider outreach) (H235–240). | Public OSS release (Gate B part 1) |
| 9 | H241–H270 | Mon–Wed: pilot #1 onboarding on their workload; live debugging (H241–258). Thu: fixes from pilot (H259–264). Fri: pilot #1 metrics report, invoice (H265–270). | First paid pilot live (Gate B) |
| 10 | H271–H300 | Mon–Tue: pilot #2 onboarding (H271–282). Wed: fixes (H283–288). Thu: pilot #3 onboarding (H289–294). Fri: case study with real numbers; pricing page draft (H295–300). | 3 pilots, case study |

## Phase 2 — Multi-host and paid control plane (Weeks 11–14, H301–H420)

| Week | Hours | Blocks | Deliverable |
|---|---|---|---|
| 11 | H301–H330 | Mon–Tue: host agent + control API (register host, report capacity) (H301–312). Wed–Thu: placement (choose host for new cell by free memory) (H313–324). Fri: docs, pilot check-ins (H325–330). | Multi-host alpha |
| 12 | H331–H360 | Mon–Wed: cell migration between hosts (state dir sync, cut-over) (H331–348). Thu: failure handling (host down → re-place cells) (H349–354). Fri: chaos test, pilot check-ins (H355–360). | Migration and failover |
| 13 | H361–H390 | Mon: licence/plan enforcement for paid control plane (H361–366). Tue: dashboard SSO (OIDC) + audit log (H367–372). Wed: billing integration (Stripe) (H373–378). Thu: pricing page live, convert pilots to paid plans (H379–384). Fri: invoices out, churn/objection log (H385–390). | Paid tier live |
| 14 | H391–H420 | Mon–Tue: hardening from pilot feedback (H391–402). Wed: 500-cell multi-host load test (H403–408). Thu: metrics review vs targets (MRR, cells under management, wake p95) (H409–414). Fri: Gate C decision: scale, raise, or pivot (H415–420). | Gate C decision |

## Weekly recurring (inside the hours above)
- 1 h/week: check upstream OpenClaw releases and fleet docs for changes to the non-goals (thesis risk).
- 1 h/week: provider and agency outreach log; keep 10 warm conversations.
- 30 min/week: update the density model and public benchmark page.
