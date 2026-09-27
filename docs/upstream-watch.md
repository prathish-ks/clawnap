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
