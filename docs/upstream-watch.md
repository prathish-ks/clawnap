# Upstream watch (weekly, 1 h)

## openclaw/openclaw issue #114145 — scale-to-zero recovery contract

Status as of 2026-09-26: open, P2, `needs-product-decision`, labels `impact:data-loss`, `impact:session-state`.
Codex review 2026-09-22: keep open; candidate PRs need an owner decision on the ownership boundary
(does OpenClaw own aggregate recovery points under RFC 0013, or does host lifecycle need a separate contract).
All candidates by one contributor (giodl73-repo), no maintainer comment since July, no hosting provider comment.

Candidate stack (do not depend on any of it merging):
- #112385 compose recovery points — capture/verify only, no caller, blocked
- #112865 capture final recovery points — closed-state capture, blocked
- #112896 admit restored recovery points — `gateway run --restore-admission-descriptor <path>`,
  `gateway.restore.status({restoreOperationId})` → not-restored | held | ready; 3 P1 issues open, blocked
- #127602 restored-admission status seam (observation only)

Ownership line stated in #112896 (our product is the host side):
- OpenClaw owns: component restore, scheduler/owner readiness, durable ready evidence, admission gating.
- Host owns: ingress delivery, cron wake events, compute placement, generation management,
  delivery acknowledgement, source destruction.

Supervisor design consequence:
- Mode A (now): crash-consistent hibernate = WAL checkpoint + graceful stop; readiness = TCP probe.
  Week-1 hour 25 state-survival test is the gate for this mode.
- Mode B (if merged): capture-final → stop → start with restore descriptor → readiness =
  `gateway.restore.status == ready`. Adapter on the existing readiness function.
- Week-1 checks to add: is SIGTERM handling cooperative (writers closed) or plain exit; does the
  RFC 0013 snapshot CLI exist in the shipped version.
- Engage on the issue only after experiment numbers exist, as a host-side implementer.

## Also track
- Fleet docs non-goals list (docs.openclaw.ai/gateway/multi-tenant-hosting).
- aws-samples/sample-multi-tenant-openclaw-on-firecracker (density reference design).

## Related issues found 2026-09-26 (search "114145")
- #119035 wake-only cron payload (omarshahine, Aug 4, P3, PR #119040): second independent host-side
  scheduler author. Confirms cron wake is host-owned. **Supervisor requirement:** read each cell's cron
  schedule and wake before due time; until then, never hibernate a cell with cron due inside the idle window.
- #149684 restore points (taekop, Sep 16, P2, needs-security-review): update system already produces
  immutable "retained state captures" (config, shared + per-agent SQLite, Skill Workshop, plugins).
  **Week-1 check:** can that capture be invoked outside an update transaction? If yes, Mode A can snapshot before stop.
- #63392 per-agent backup/restore/clone (Apr 8, stale): map of per-agent state paths
  (workspace-<agent>/, agents/<agent>/agent/, agents/<agent>/sessions/, openclaw.json entry) — use in the
  state-survival test and for future cross-host migration.

## Found by thesis-watch run 2026-09-26 (verified)
- aws-samples/sample-openclaw-multi-tenant-platform (42 stars, MIT-0, "experimental, not for production"):
  per-user OpenClaw pods on EKS, KEDA scales idle pods to zero on HTTP traffic, cold start 15–30 s,
  ~100 tenants at $331–418/mo, ceiling from ALB target-group quotas; agent-sandbox mode stays always-on
  ("native suspend/resume is tracked upstream"). Closest prior art for scale-to-zero. Needs Kubernetes + AWS;
  15–30 s cold start is the number to beat publicly.
- mlamplugh-max/clawhire-openclaw-gateway (Elastic License 2.0, Fly Machines): multi-tenant worker with
  per-agent OS sandbox, tenant-scoped state, brokered short-lived credentials, enforced monthly cost cap,
  frozen REST contract. No hibernation. Overlaps our security/metering layer; not our density thesis. WATCH.
- PR #112896 links "non-normative host follow-on evidence (Microsoft access)" — the contract author appears
  to have a Microsoft-internal host consumer. No public product named. Note only.
- Cloud routine cannot fetch docs.openclaw.ai, dev.to, myclaw.ai, releasebot.io (egress blocked); prompt now
  points it at the docs source in the openclaw GitHub repo instead.

## Observation for the #114145 thread (2026-09-27, real host)
On thaw after a pause, OpenClaw 2026.9.6 logs `host timing gap detected … restarting channels`, `[admission] closed: suspend phase` → `reopened`, then restarts the Telegram channel. Its webhook listener is serving ~40 ms before the channel is ready, and the first real update in that window is answered `Telegram webhook ingress is not ready.` (a 5xx to the platform), 5 ms before `webhook advertised`. Telegram's retry delivers ~2 s later. A host that wakes on push therefore needs either a channel-ready signal (the restored-admission status seam in #127602 would be exactly this) or its own short retry; we do the latter. This is concrete evidence for the host-side split the thread proposes.
- Third post-thaw gap (2026-09-27 10:14): after the listener and the channel, the session placement stays closed ~1 s after thaw; the first agent turn aborts with `session placement turn settlement is closed`, the user receives a "heartbeat failed" notice, and the turn succeeds on OpenClaw's retry. Same shape as the other two: recoverable, invisible from outside, and the case for a single restored-admission ready signal.
- Verified 2026-09-27 11:45: `/ready` is a liveness alias (true 92 ms after thaw while recovery is still running); placement/admission state never reaches an external surface. A host has no readiness signal to wait on. This is the concrete ask for #127602.
- Verified in the shipped code 2026-09-27 12:10: the session placement "turn settlement" is closed by an internal closure after thaw and its reopening is neither logged nor exposed. The only observable is the trigger line (`host timing gap detected`). A host can hold the first message for a measured window after the trigger but cannot know when the window ends. This is the precise gap a restored-admission ready signal (#127602) would close; the measured window on a CX43 is 22 ms to ~800 ms.

## Draft comment for #114145 (ready to post; 2026-09-27)
Decision 2026-09-27: post the comment now from the private-repo stage; publish the code later. Purpose of the comment: put measured host-side evidence on the thread, state the one concrete ask (an external post-thaw ready signal), and register as a host-side implementer so maintainers and providers know the split is real. Do not link the repo yet; say numbers are reproducible on request.

---
Host-side data point for this thread, from an external supervisor that hibernates stock OpenClaw containers (2026.9.6) and wakes them on inbound webhooks. Everything below was measured on one Hetzner CX43 (8 vCPU, 16 GB, zram swap), nothing changed inside the cells.

**Density.** Stock cells on the same host with zram degrade at ~35 (load 35, cells dying at 40). With idle cells paused via the container runtime and their memory reclaimed through cgroup v2 `memory.reclaim`, 50 cells ran healthy on the same box (13 GB RAM + 8 GB compressed swap, 0 OOM), and the ceiling is above 50. A reclaimed cell (~600 MiB paged out) wakes in 1.5–2.6 s on zram when the host has headroom. Concurrent wakes into a host packed to its limit are page-in bound rather than gateway bound (a burst of ten at 50 cells completed with no failures but queued for tens of seconds); we are measuring the density-to-latency curve next and will post it.

**What a host sees after thaw.** With a real Telegram bot on a paused cell, the gateway's own recovery has three windows that are invisible from outside:
1. The webhook listener answers ~40 ms before the channel has finished restarting; a real update in that window gets `Telegram webhook ingress is not ready` (5xx to the platform). Telegram's retry delivers ~2 s later.
2. After the channel is up, the session placement stays closed for a further 22 ms–800 ms: the first turn aborts with `session placement turn settlement is closed`, the user receives a "heartbeat failed" notice, and the turn succeeds on the gateway's own retry.
3. `/health` and `/ready` both return 200 from ~90 ms after unpause and never change during any of this; `/ready` is a liveness alias in this build. The internal placement/admission states exist but are not exposed on any HTTP or protocol surface.

A host can work around 1 with a request-level probe plus a short retry and around 2 by watching the cell's log for `host timing gap detected` and holding the first message for a bounded window, which is what we do. That covers it, but it is guesswork about the end of the window, because the settlement reopening is neither logged nor exposed.

**The ask.** The ownership split proposed here (OpenClaw owns component restore, scheduler/owner readiness and admission gating; host owns delivery, wake events and placement) matches what we needed. The one thing the host side cannot build for itself is the ready signal: a single external "admission/placement reopened" state, either on the existing `/ready` endpoint or the restored-admission status seam from #127602. With that, a host can hold inbound delivery until the gateway is actually able to take a turn, and the platform retry and the user-facing notice both disappear. Full timings and the method are reproducible; happy to share them here or test a build that exposes such a signal.
---
