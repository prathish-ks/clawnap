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
