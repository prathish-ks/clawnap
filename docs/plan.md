# fleet-supervisor — living plan

**Status line (update every session):** 2026-09-26 · Phase 0a complete except Hetzner. **Density lever measured locally: paused+reclaimed cell 789 → 20 MiB (≈35x), wake 16.7 s on a slow disk.** Phase 0a (local correctness): 11 of 12 rows done; local gate met except session/memory/cron survival (needs a model provider). Release 0 host checker built. Remaining: provider access (9, in progress), Hetzner account (10, user). Pause cap, /metrics, `fleetd cells create` and reclaim-after-pause all done and verified (supervisor-driven reclaim in the VM: 376 → 46 MiB in 7 s). Code is ready for Phase 0b; Hetzner measures zram vs NVMe wake latency and cells per host · next gate: local go/no-go · Hetzner account: not yet created · thesis-watch routine: daily 21:00 UTC, last verdict NO CHANGE (2026-09-26).

How to maintain: edit the status line and the "Now" table every working session; move finished rows to the Changelog with the date; never delete a gate, only mark it passed or failed with evidence.

## Thesis and stop conditions (unchanged unless a gate fails)
Stock OpenClaw cells, unchanged, supervised by a small Go host-side control plane that hibernates idle cells and wakes them on inbound message, so a hosting provider runs 3–5x more cells per server.
Stop if: upstream ships hibernation or multi-host fleet; a credible open-source hibernation supervisor for OpenClaw on plain Docker hosts appears; two or more named providers exit; OpenClaw is abandoned or superseded; fewer than 2 of 10 providers name density a top-3 cost or will pay $500/mo; the Hetzner experiment cannot reach 3x baseline with an honest wake SLA.

## Phases and gates

| Phase | Where | Goal | Gate to leave |
|---|---|---|---|
| 0a Local correctness | This Mac, 2 live cells + 3–5 hibernated | Every lifecycle path works and state survives | Zero state loss over 20 hibernate/wake cycles with a real Telegram bot; pause-tier wake < 1 s on /health; stop-tier readiness measured and documented; ingress wake works end to end; daemon survives its own restart |
| 0b Hetzner numbers | CX43 16 GB, up to 50 cells | Publishable density and wake numbers: pause+reclaim with zram vs NVMe swap, wake latency per tier | ≥3x baseline cells/host; p95 wake within the tier's stated SLA (pause < 5 s); zero state loss; density-stack CSVs (trim, overcommit, zram, KSM, CRIU, pause, stop) |
| 0c Upstream engagement | GitHub | Validate the host-side split with OpenClaw | Comment posted on #114145 (and #119035 if cron wake is implemented) with numbers; any maintainer or Codex response recorded in docs/upstream-watch.md |
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
| 10 | Hetzner account + API token (user); hcloud CLI install (needs approval); provision CX43 | blocked on user | experiments/provision/ |
| 11 | Release 0 host checker (`fleetd check`): ported from Isthmus doctor/securitycheck/egress/mount rather than extracted as a shared module (Isthmus packages are internal/ and NanoClaw-typed; the port keeps their checks and remediation text). Isthmus can import this package back later | done 2026-09-26 (unit-tested; ran live: 23 pass / 13 warn / 0 fail across a hardened cell and two unhardened containers) | egress check is Linux-only by design |
| 12 | First git commit of the repo | done 2026-09-26 (09a3da1) | |

## Backlog (Phase 0b onward)
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
- 2026-09-26: no content-reading polling keeper; webhook-first with ingress secret verification, count-only keeper as opt-in only.
- 2026-09-19: OpenClaw fleet supervisor chosen over Paperclip (docs/opportunity-scout-2026-09-19.md).
- 2026-09-19: Hetzner CX43 (~EUR 16/mo) for the experiment host; AMD CPX plans avoided after June 2026 price rise.
- 2026-09-26: Hibernation is tiered (pause → CRIU → stop) after local measurement showed 70–90 s gateway start; readiness is HTTP /health.
- 2026-09-26: Phase 0 split into local correctness (0a) and Hetzner numbers (0b); upstream engagement (0c) scheduled after numbers exist.
- 2026-09-26: Daily thesis-watch routine created (trig_01BRPWHtqCuwJYP4MicWYTSq), run-log delivery.

## Changelog
- 2026-09-19: repo scaffolded (registry, runtime seam, idle, supervisor, ingress, spec, CLI, tests); live stop/start cycle against a stand-in container passed.
- 2026-09-19: provisioning scripts (cloud-init, provision-hetzner.sh, deploy.sh) written; versions verified.
- 2026-09-26: upstream issue review (#114145 Codex review, #127602, #119035, #149684, #63392) recorded in docs/upstream-watch.md; cron-aware wake added.
- 2026-09-26: local measurement of 3 OpenClaw cells on Docker Desktop (experiments/local-measurements-2026-09-26.md).
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
