# Revision 2 (19 Sep 2026) — changes from the original plan

Applied after the ecosystem, Isthmus-reuse and density discussions. The original hourly plan follows unchanged below; these rows override it.

| Where | Change | Why |
|---|---|---|
| H13–H24 | **Done as a scaffold on day one** (registry, runtime seam, idle detector, hibernate/restore, bounded coalesced wake, ingress wake-then-proxy, spec validation, unit tests). Real-Docker verification remains. | Started 19 Sep with no server available; unit tests run against a fake runtime. |
| H31–H34 | Measure the **whole density stack**, one CSV each: (a) trimmed gateway config, (b) memory overcommit with per-cell limits, (c) + zram, (d) + KSM, (e) + hibernation. Report each layer's contribution. | Providers will ask what overcommit alone buys before trusting hibernation. |
| H25 | Elevate: the 20-cycle state-survival test is the Mode A gate (upstream labels abrupt stop `impact:data-loss`). Also verify SIGTERM handling is cooperative and whether the RFC 0013 snapshot CLI ships. See docs/upstream-watch.md. |
| H37–H39 | Add: check whether OpenClaw's cooperative-suspension and SQLite-snapshot primitives (upstream issue #114145, "scale-to-zero recovery contract") can make hibernation application-consistent. Align our stop/start with that contract's six states where cheap. | Upstream explicitly leaves ingress, wake, placement and lifecycle to hosts; adopting their contract lowers state-loss risk. |
| H40–H42 | Replace "port Isthmus checks" with **extract Isthmus packages** (`egress`, `doctor`, `securitycheck`, `mount`, `containerdefaults`, `capability`, `credentialbroker`, `trace`) into an importable module with a neutral `ContainerSpec` input; `nanogo` and `fleetd` both import it. `internal/spec` becomes an adapter. | Reuse ~a third of Isthmus's non-vendor Go without duplication; Isthmus gains a public module. |
| New H60a (end of week 2, 6 h) | **Release 0**: a standalone host checker for OpenClaw fleet hosts built from the extracted module (runtime class, metadata-egress block, Docker socket and dangerous mounts, root execution, secret-shaped env). Publish regardless of Gate A. | Useful alone to self-hosters and every provider; opens provider conversations; the "security badge" providers can show customers. |
| H16–H27 (revisit on Linux) | Local Docker Desktop measurement (experiments/local-measurements-2026-09-26.md): gateway ready 73–90 s after `docker start`, so stop/start cannot meet p95 wake < 5 s. Add **pause/unpause + zram** as the primary hibernation tier and CRIU as the secondary; stop/start becomes the cold tier with a longer SLA. Readiness = HTTP /health, never TCP (Docker Desktop proxy false positive). |
| Week 4 | Add **cron-aware wake**: parse cell cron schedules, wake before due; interim rule: no hibernation when a job is due within the idle window (upstream #119035). |
| Week 5 | Discord keeper stays; **WhatsApp cells default to the always-on class** in phase 1. | Persistent-socket channels resist hibernation; be honest in the channel matrix. |
| Week 6 | Drop the sibling-container API from phase 1. | OpenClaw's inner sandbox is off by default; the cell container is the boundary. Revisit when the NanoClaw/Isthmus driver lands. |
| Week 7 | Add one **customer-visible** feature to the dashboard: per-fleet public status page or security-badge endpoint. | Gives providers something their customers ask for, so providers pull adoption. |
| Weekly recurring | Track upstream issues #114145 and #127602 and the fleet docs' non-goals list; note the AWS `sample-multi-tenant-openclaw-on-firecracker` repo as the density reference design. | Thesis risk and design reference. |
| Phase 2 | Driver interface modelled as a **cell tree** (OpenClaw = one node; NanoClaw/Isthmus = host process + child containers). Sequence: OpenClaw driver → Isthmus driver → stock NanoClaw. | Keeps the three deliverables independent while sharing the core. |

---

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
