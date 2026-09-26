# Local measurement — Docker Desktop on a 2015 MacBook Air (2026-09-26)

Host: Intel i5-5350U (2c/4t), 8 GB; Docker Desktop 27.3.1 VM: 4 vCPU, 3.9 GB, cgroup v2, runc.
Image: ghcr.io/openclaw/openclaw:latest (2026-09-23), 1.17 GB, entrypoint tini, cmd `node openclaw.mjs gateway`.
Cells run with `--allow-unconfigured`, no channels, no provider keys, `--memory 1g --pids-limit 512 --cap-drop ALL`.

| Metric | cell1 (bind mount) | cell2 (bind mount) | cell3 (named volume) |
|---|---|---|---|
| Idle RSS at 2 min (still starting) | 172 MiB | 153 MiB | — |
| Idle RSS settled (~6 min) | 641 MiB | 690 MiB | 611 MiB |
| PIDs settled | 30 | 30 | 30 |
| Idle CPU settled | 1.3% | 2.9% | 1.0% |
| Container start → `[gateway] ready` | 264 s (cold, image page-in, host thrashing) | 73 s | 92 s |
| "slow SQLite transaction" log lines at startup | 2 | 2 | 1 |
| State dir after start | 1.4 MB | — | — |

Lifecycle timings (cell2/cell3):
- `docker stop -t 10`: 1.37 s. `docker start`: 1.01 s. Gateway ready after start: ~73–90 s (same as cold; no warm path).
- TCP accept on the published port succeeded 1.58 s after start — **false positive**: Docker Desktop's port proxy accepts before the process listens. Readiness must be HTTP `GET /health` (200) or the image's `dist/docker-healthcheck.js` (took 8.4 s here). On native Linux a TCP probe is honest, but use HTTP anyway.
- `docker pause`: 0.11 s; `unpause`: 0.12 s; `/health` 200 within 0.13 s of unpause. Memory stays resident while paused (565 MiB).
- Host load average hit 28 on a 4-thread CPU with three idle cells starting; the Mac is CPU-bound, so absolute start times here are pessimistic.

Conclusions:
1. Idle footprint ~600–700 MiB per unconfigured cell confirms the 400–800 MB figure in hosting guides. With channels and Chromium it will be higher.
2. This Mac can run 3–4 live cells (3.9 GB VM, ~650 MiB each, plus OneCLI already using ~500 MB). Hibernated cells cost 0 RAM and ~2 MB disk, so the supervisor's benefit is real even here, but it is not a fleet test host.
3. **Wake-on-message p95 < 5 s is not achievable with stop/start on this image**: the gateway needs 70–90 s to be ready even on a warm restart. Three options, to be measured on the Linux host: (a) `pause/unpause` + zram/swap so paused pages compress (instant wake, memory partially reclaimed); (b) CRIU checkpoint/restore (Linux only, sub-second restore if sockets cooperate); (c) tiered pools: pause for "cool" cells, stop for "cold" cells with a longer wake SLA.
4. Cold start on Docker Desktop is dominated by SQLite lock waits (busyTimeout 5 s) plus plugin load; named volume vs bind mount made little difference here.

## Live cycles through fleetd (same day, one real cell, `--allow-unconfigured`)

Cell ready (/health 200) ~50 s after `docker run` on an otherwise idle host.

| Tier | Cycles | Hibernate | Wake to /health 200 | Result |
|---|---|---|---|---|
| pause | 5 | 0.11–0.19 s | 0.20–0.37 s | 5/5 healthy |
| stop | 2 | 6.8–10.4 s (graceful stop reached the 10 s grace once) | timed out at the 30 s default | 0/2 — supervisor bug, not the cell |

Re-run after the fix (fresh cell): stop tier 2/2 healthy; hibernate 0.28 s and 17.3 s (second stop exceeded the 10 s SIGTERM grace, so Docker killed it); wake to /health 200 in 71.5 s and 45.3 s.

Fix applied: wake timeout is now per tier (`WakeTimeout` 15 s for pause, `StopWakeTimeout` 3 min for stop). The stop tier also shows OpenClaw does not exit promptly on SIGTERM (6.8–10.4 s), which matters for the "cooperative suspension" question in docs/upstream-watch.md.
