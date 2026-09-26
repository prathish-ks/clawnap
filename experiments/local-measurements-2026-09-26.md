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

## Concurrent test: 5 cells on the 3.9 GB Docker Desktop VM (2026-09-26, 15:41–15:53)

Cells started 20 s apart; all five reached /health 200 within 10–55 s of their own start. Host load average peaked at 52 on a 4-thread CPU.

| Measurement | Value |
|---|---|
| Live memory, 5 cells idle | 528–639 MiB each; VM 3,193 MB used, 144 MB free |
| After pausing 3 of 5 | 3,142 MB used — **pause frees no RAM by itself** (zram/swap needed; Hetzner question) |
| Pause tier, 20 cycles × 3 cells, under load | 60/60 healthy; wake p50 0.67–0.95 s, p95 1.30–2.23 s, max 6.4 s |
| Live cells during the 60 cycles | both stayed /health 200 |
| Stop tier, 5 cycles on one cell, under load | 5/5 healthy; wake 39.5–155.5 s (first cycle 155 s while other cells were booting) |
| State dir growth | live cell 1.4 MB → 59 MB in 12 min idle; hibernated cell 4.1 MB — investigate what grows (logs/cache) before Hetzner |

Local gate status: pause wake < 1 s at p50 but p95 up to 2.2 s under a load average of 52; on an idle host it was 0.2–0.4 s. Stop tier is a minutes-class SLA. Not yet done for the local gate: real Telegram bot end-to-end wake, and the state-survival check after cycles (needs the bot).

## Real Telegram bot cell (2026-09-26, evening)

Config that works (docs were wrong about `startup.auth`): `channels.telegram.botToken` in `openclaw.json`, `gateway.mode: "local"`, `gateway.auth.mode: "token"`. Validate with `openclaw config validate` before starting.

Findings:
- **Webhook mode is self-registering.** With `channels.telegram.webhookUrl` set, the channel starts a separate listener on `webhookHost:webhookPort` (default 127.0.0.1:8787, path `/telegram-webhook`) and calls Telegram `setWebhook` itself on every channel start, retrying 10 times. Telegram rejects unresolvable hosts, so webhook mode needs the ingress's real public HTTPS URL per cell. Consequence for the supervisor: the cell's `webhookUrl` must be `https://<ingress>/hook/<cell>/telegram-webhook` and `-hook-port` must map to the cell's 8787, not the gateway port.
- **Polling mode has a spool.** Log: `isolated polling ingress started spool=<state>/telegram/ingress-spool-default`. Messages sent while a cell is paused are held by Telegram (24 h) and fetched on wake.
- **Stale owner lease after an unclean stop.** After `docker rm -f` of a running cell, the next start failed: `Another Gateway owner lease is still active for this state directory` (exit 1). A later start succeeded, consistent with a lease timeout of roughly one to two minutes. The supervisor's stop tier uses a graceful stop, but OOM kills and host crashes will hit this; the pause tier does not. Needs a proper look on Linux: where the lease lives and whether `gateway status --deep` can clear it.
- **Wake through the ingress against the real cell:** paused → `GET /hook/tg/health` → cell unpaused and answered `{"ok":true,"status":"live"}` in 0.43 s end to end (wake 155 ms).
- Test note: a `find -iname '*lease*' -delete` intended for the lease also removed cached control-UI asset files under `state/cache/`; they are a cache and the gateway rebuilt them, but the real lease was not among them.

### Long pause (3 h) and the getUpdates conflict
- The cell was paused for ~3 h 05 min while waiting for the user's messages. On unpause OpenClaw logged `host timing gap detected: process was frozen ~11078795ms; restarting channels when idle`, `liveness heartbeat delayed … deferring recovery decisions`, `owner lease heartbeat stopped; process identity remains recorded`, then the process exited with code 135 about 7 s after thaw. `fleetd wake` then took the start path and the gateway was ready in 38.8 s. **Pause-tier consequence:** OpenClaw tolerates short freezes (the 20-cycle tests, seconds each, were clean) but treats a multi-hour freeze as a lost lease and restarts itself. The supervisor should either cap pause duration below the lease TTL and fall through to the stop tier, or treat exit-after-thaw as expected and self-heal (already does). Measure the threshold on Linux.
- `memory pressure: level=critical rss=780 MiB threshold=768 MiB`: OpenClaw self-monitors RSS with a 768 MiB critical threshold (512 MiB warning). A 1 GiB cell limit sits just above it.
- **Test contamination:** the bot token is also configured in the user's `~/dev/isthmus-fresh-clone-test/.env`, whose NanoClaw v2 host runs under launchd (PID 523) and long-polls the same bot. Telegram allows one poller per bot, so the cell logged `getUpdates conflict: terminated by other getUpdates request` and the user's three messages were consumed by the other install. The "message survives hibernation" check is therefore not yet done; it needs a dedicated bot or that service paused.

### Dedicated bot (@Ocfleet_bot): message survival across a pause — PASSED
- Cell paused; user sent two messages; `fleetd wake` → unpause 132 ms, /health ready 822 ms.
- 48 s after thaw the gateway logged `host timing gap detected: process was frozen ~48202ms` and `liveness heartbeat delayed … deferring recovery decisions` but kept running (contrast: the ~3 h freeze led to exit 135). Freeze tolerance threshold lies between ~48 s and ~3 h; lease constants in the code are 125 s and 30 min.
- Within 3 s of wake the polling worker received both held updates (`updateId=271848954`, `271848955`, `queued=2`), spooled them, and the user received a reply from the cell. Channel auth, polling offset and delivery all survived the pause.
- Not yet exercised: session/memory/cron state (no model provider configured in the cell).

### Freeze-tolerance measurement (same cell, evening)
| Freeze | Gateway reaction on thaw | Survived? |
|---|---|---|
| ~48 s (earlier) | timing-gap notice, liveness "deferring recovery" | yes |
| ~82 s | timing-gap; Telegram "polling stall detected … forcing restart" | yes, no lease line |
| ~192 s | timing-gap; memory-pressure warning | yes, no lease line |
| ~56 min, then 5 s pulse, then full wake | timing-gap; `owner lease heartbeat stopped; process identity remains recorded`; Telegram lease released and polling restarted | yes, still running 40 s later |
| ~3 h 05 min (morning) | same lease line, then exit 135 ~7 s after thaw | no |

Bounds: the "owner lease heartbeat stopped" state begins somewhere between ~3 min and ~56 min (the 30-min lease constant `LEASE_MS = 18e5` fits); self-exit begins somewhere between ~56 min and ~3 h. The pause cap default of 20 min keeps every freeze under the first bound. Each thaw over ~1–2 min costs a Telegram polling restart (a few seconds), which is why the pulse window is 5 s and not shorter.

Pulse path verified live: `fleetd serve -max-pause 1m` pulsed the 56-min-frozen cell (unpause → /health 200 → 5 s → re-pause in 352 ms).

## Memory reclaim of a paused cell (2026-09-26, late) — THE DENSITY LEVER, MEASURED

Docker Desktop VM: kernel 6.10 linuxkit, 1 GiB swap file (no zram), cgroup v2 with `memory.reclaim`.
Procedure: cell paused → `echo 900M > /sys/fs/cgroup/docker/<id>/memory.reclaim` → unpause → time /health.

| Point | Resident (memory.current) | In swap | anon | file cache |
|---|---|---|---|---|
| Paused, before reclaim | 789 MiB | 0 | 661 MiB | 83 MiB |
| Paused, after reclaim (7 s; rc=EAGAIN means "nothing left to reclaim") | **20 MiB** | 661 MiB | 0 | 0 |
| 8 s after wake, serving | 145 MiB | 504 MiB | | |

- Wake after reclaim: unpause 0.24 s, /health 200 after **16.65 s** (page-in from a swap file on a 2015 SSD through Docker Desktop's virtual disk). Without reclaim the same wake takes 0.2–0.8 s.
- The gateway served with only 145 MiB paged back; the rest pages in lazily on demand.

Conclusions:
1. **A paused-and-reclaimed OpenClaw cell costs ~20 MiB of RAM instead of ~700 MiB (≈35x)**. This is the lever hosting providers do not have today; overcommit alone cannot shrink an idle gateway.
2. The price is wake latency, set by the swap device: ~17 s on this disk. Expected on Hetzner: zram (compressed in RAM, no disk) → sub-second to low-seconds wake with ~2–3x space saving; NVMe swap → low-seconds wake with ~35x saving. Phase 0b measures both.
3. Tiers become: pause (resident, <1 s), pause+reclaim (20 MiB, seconds), stop (0, 40–150 s). The supervisor should reclaim on a schedule after pause (e.g. after N minutes paused), not immediately, so recently used cells stay sub-second.
4. `memory.reclaim` on the container cgroup is the mechanism; it is Linux-only and needs write access to /sys/fs/cgroup (the supervisor runs as root on a fleet host). On systemd hosts the cgroup path is /sys/fs/cgroup/system.slice/docker-<id>.scope; on cgroupfs hosts /sys/fs/cgroup/docker/<id>.

### Reclaim driven by the supervisor itself (Linux build of fleetd run inside the Docker VM)
- `fleetd reconcile` adopted the externally paused cell (PausedAt set on adoption — fixed today), then `-reclaim-after 1s` reclaimed it: **190 → 20 MiB resident, 663 MiB in swap**.
- First attempt requested the full resident amount and the kernel write only returned at the 60 s wait bound (it spins on the last unreclaimable pages); requests are now capped at memory.current minus a 48 MiB floor. Second timed pass with the floor: 376 → 46 MiB resident, 622 MiB in swap, write returned in 7 s.

### Heavy-cell simulation (no Chromium in the image; a 400 MiB resident filler process stands in for a browser/heavy heap; limit raised to 2 GiB)
| Point | Resident | In swap |
|---|---|---|
| light cell running (gateway mostly already swapped from the previous test) | 120 MiB | 517 MiB |
| + 400 MiB filler, running | 542 MiB | 488 MiB |
| paused, after supervisor reclaim (63 s: the 48 MiB floor was not enough at this size, kernel spun to the wait bound) | 84 MiB | 933 MiB |
| 8 s after wake, serving | 163 MiB | 864 MiB |

- Wake after reclaim: unpause 0.41 s, /health 200 after **12.9 s** — no slower than the light cell (16.7 s earlier on the same disk). The filler's 400 MiB never paged back in. **Wake time tracks the pages the gateway touches, not the cell's total size.** A heavy cell costs more swap space, not more wake time.
- Consequence: reclaim must be chunked (64 MiB steps, stop when a chunk frees < 16 MiB) instead of one request with a fixed floor; implemented after this test.
- Caveat: the filler is idle bytes. A real browser or a large live heap is touched by the gateway on some code paths (health does not touch them; a first agent turn might), so first-message latency after reclaim on a heavy cell is a Hetzner measurement with a real model.
