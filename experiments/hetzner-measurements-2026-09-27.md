# Hetzner measurements — fleet-exp-1 (CX43, 8 vCPU / 15.6 GB, fsn1, kernel 6.8, Docker 29.8 cgroup v2, zram 15.2 GB zstd)

## Week-1 baseline, step 1: 10 stock cells, no supervisor (2026-09-27, 03:35 UTC)
Cells: official image 2026-09-23, `--user 1000:1000`, own config (gateway local, token auth), no channels, `--memory 1g --pids-limit 512 --cap-drop ALL --security-opt no-new-privileges`. All 10 started at once.

| Metric | Value |
|---|---|
| Start → /health 200, all 10 in parallel | 21–27 s each (vs 40–150 s on the laptop) |
| Idle RSS per cell after 60 s settle | 719–815 MiB (mean ~748 MiB) |
| Idle CPU per cell | 0.7–3.3 % |
| PIDs per cell | 59–62 |
| Host after 10 cells | 8,174 MB used, 876 MB free, 6.9 GB cache; swap 0; load 6.7 |

Implication: stock cells at ~750 MiB idle fill this 15.6 GB host at roughly **18–20 cells** before the kernel starts swapping — the number the density stack is measured against.

## Week-1 baseline, step 2: stock cells past the RAM ceiling, zram present, no supervisor (03:40–03:55 UTC)
| Cells | Host used | Free | zram used (compressed) | Boot to healthy (new cells) | Mean idle RSS | Healthy |
|---|---|---|---|---|---|---|
| 10 | 8.2 GB | 876 MB | 0 | 21–27 s | 748 MiB | 10/10 |
| 20 | 12.8 GB | 717 MB | 2.5 GB | 23–27 s | 590 MiB | 20/20 |
| 25 | 13.8 GB | 151 MB | 5.6 GB → 1.4 GB compressed (3.9x) | 27 s | — | 25/25 |

Finding: **with zram present, stock OpenClaw with no supervisor already exceeds the 18–20 cell RAM projection.** Under memory pressure the kernel swaps idle pages into zram on its own; zstd compresses OpenClaw's idle pages ~3.9x, so 5.6 GB of swapped pages cost 1.4 GB of RAM. At 25 cells all remained healthy, boot time did not degrade, and load settled to 0.6.
This is the real provider baseline (zram is a one-line host setting) and what the supervisor must beat: reclaiming deliberately per cell before pressure, and prefetching on wake, versus the kernel's reactive swapping and fault-by-fault page-in. Ceiling not yet reached; the SSH session dropped after cell 25 (keepalive), not the host.

## Week-1 baseline, step 3: the stock ceiling on a zram host (04:00–04:30 UTC)
Cells added in steps of five; each step waited for the new cells' /health, settled 45 s, then re-checked every cell.

| Cells | New cells booted | Slowest boot | Healthy | Host used | zram data → compressed | Load | Exited |
|---|---|---|---|---|---|---|---|
| 25 | 5/5 | 27 s | 25/25 | 13.8 GB | 5.6 → 1.4 GB | 0.6 | 0 |
| 30 | 4/5 (one timed out at 300 s) | 321 s | 29/30 | 13.8 GB | 9.1 → 2.4 GB | 0.9 | 0 |
| 35 | 6/6 | 42 s | 34/35 | 13.9 GB | 14.2 → 3.8 GB | 4.6 | 0 |
| 40 | 6/6 | 63 s | 37/40 | 14.4 GB | **15.2 → 4.1 GB (zram full)** | **35.5** | 2 |

**Stock ceiling on this host with zram: ~35 cells.** At 40 the 15.2 GB zram device is full, swap is 99.8 % used, load is 35, two cells have died and three fail health. The failure mode is not out-of-memory kills (none) but thrashing: once compressed swap is exhausted the kernel has nowhere to put cold pages and every cell fights for the same RAM.

Compression stayed near 3.7x throughout (zstd on OpenClaw's idle heap), so zram sized at 100 % of RAM holds ~3.7 × 15 GB ≈ 56 GB of pages in 15 GB; the binding constraint at 40 cells was the zram device size, not compressibility. A larger zram (or NVMe swap behind it) would lift the stock ceiling further, at the cost of the fault-by-fault page-in the supervisor's prefetch exists to avoid.

Provider takeaway: the honest comparison for the product is not "stock on bare RAM" (18–20 cells) but "stock + zram" (35 cells). The supervisor's claim is measured against 35.

## Supervisor density test on the same host (04:07–04:55 UTC)
Daemon as a systemd unit: `-interval 15s -reclaim-after 60s -reclaim-keep-mib 150 -prefetch-on-wake -max-pause -1s`. Cells created through `fleetd cells create` (same image and hardened flags as the stock baseline), steps to 50, each step: boot all new cells → settle 150 s (idle 45 s → pause; +60 s → reclaim into zram) → five random hibernated cells woken one at a time through the ingress → health count. Stop rule: more than two exited cells.

| Cells | Healthy (paused counts) | Exited | Host used | zram data → compressed | Load | Wake p50 | Wake p95 |
|---|---|---|---|---|---|---|---|
| 10 | 10/10 | 0 | 5.7 GB | 2.9 → 0.8 GB | 0.8 | 108 ms | 129 ms |
| 20 | 20/20 | 0 | 8.5 GB | 9.6 → 2.5 GB | 0.5 | 142 ms | 2.18 s |
| 30 | 30/30 | 0 | 10.8 GB | 14.9 → 3.9 GB | 0.3 | 2.10 s | 2.74 s |
| 35 | 35/35 | 0 | 14.4 GB | 15.2 → 4.0 GB (full) | 0.2 | 2.12 s | 2.32 s |
| 40 | 40/40 | 0 | 15.1 GB | 15.2 → 4.0 GB (full) | 10.7 | 1.23 s | 3.18 s |
| 45 | 42/45 | 3 | 14.4 GB | 15.2 → 4.0 GB (full) | 0.01 | 1.27 s | 120 s (a dead cell, see below) |

**Against stock on the same host:** stock held 34/35 at load 4.6 and collapsed at 40 (37/40, load 35, two dead); the supervisor held 40/40 at load 10.7 and reached 45 before its stop rule fired. Both hit the same wall, the 15.2 GB zram device being full from 35 cells on; the supervisor's edge under that wall is that it reclaims deliberately before pressure and pages back in bulk, so the host stays responsive (load 0.2–0.3 through 35 cells vs stock's 4.6) instead of thrashing.

**Wake latency on zram, first real numbers.** Two regimes, both visible in the daemon log:
- Cells with little in swap (0–360 MiB): prefetch 3–150 ms, wake 150–300 ms end to end.
- Cells with 450–620 MiB in swap: prefetch 1.6–2.9 s, wake 2–3 s. Prefetch time tracks swapped bytes at ~250 MiB/s, i.e. single-threaded zstd decompression, regardless of the ~4.3 GiB advised. The lever here is a smaller swapped set per cell (or parallel decompression), not filtering the advise list.

**The 45-cell failures, all at boot, none of them wakes:**
- s45: SIGKILL (exit 137) during the boot storm; dmesg shows 5 OOM events. The step's boot took 997 s because 45 gateways started on a host whose zram was already full.
- s43 and s41: OpenClaw's own startup failed with `StateDatabaseCoordinatorContentionError` / "startup migration lease was lost" — its per-install lease logic timing out under that load. s41 was later picked for the wake sample: the supervisor unpaused an already-dead gateway and waited the full 2-minute reclaimed-wake timeout before returning 502. That is the 120 s p95; it is a dead-cell detection gap (an exited container should fail a wake immediately), not a slow wake.

**Ceiling verdict:** ~40 supervised cells vs ~35 stock on 16 GB with a 15.2 GB zram, both bounded by the zram size. To move the ceiling for either, size zram above RAM (compression held at 3.7x) or add NVMe swap behind it; the supervisor's advantage would then show as wake latency and host responsiveness under that larger regime.

## Supervisor density run 2: reviewed binary, zram 200 % of RAM, batched boots (05:48–06:24 UTC)
Same daemon settings as run 1; differences: the reviewed and fail-fast build, zram resized to 30.5 GB, cells booted five at a time with each batch healthy before the next.

| Cells | Healthy | Exited | Host used | zram data → compressed | Load | Wake p50 | Wake p95 |
|---|---|---|---|---|---|---|---|
| 10 | 10/10 | 0 | 6.0 GB | 4.6 → 1.2 GB | 0.8 | 2.25 s | 2.48 s |
| 20 | 20/20 | 0 | 9.0 GB | 10.7 → 2.8 GB | 0.5 | 1.72 s | 1.95 s |
| 30 | 30/30 | 0 | 10.8 GB | 17.3 → 4.6 GB | 0.3 | 2.31 s | 2.60 s |
| 35 | 35/35 | 0 | 12.5 GB | 19.5 → 5.2 GB | 7.0 | 2.09 s | 2.24 s |
| 40 | 40/40 | 0 | 11.8 GB | 23.3 → 6.2 GB | 10.2 | 2.01 s | 2.49 s |
| 45 | 45/45 | 0 | 11.8 GB | 27.6 → 7.4 GB | 9.3 | 2.07 s | 2.47 s |
| 50 | 50/50 | 0 | 13.1 GB | 30.5 → 8.2 GB (full) | 5.1 | 2.29 s | 2.38 s |

Totals: 85 hibernations, 84 reclaims moving 48 GiB out of RAM in aggregate, 35 wakes (34 of reclaimed cells), 0 failures, 0 exits. Boot steps took 15–41 s (batched) with no OOM and no OpenClaw startup-lease failure.

**Three runs on one host, side by side**

| Run | Swap device | Ceiling | State at 40 cells |
|---|---|---|---|
| Stock, no supervisor | zram 15.2 GB | ~35 | 37/40, load 35, 2 dead |
| Supervisor run 1 | zram 15.2 GB | 40 (45 with 3 boot failures) | 40/40, load 10.7 |
| Supervisor run 2 | zram 30.5 GB | **≥ 50, all healthy** | 40/40, load 10.2 |

Findings:
- **Supervised cells per 16 GB host: at least 50, versus 35 stock.** The ladder ended at its top step with zram exactly full (30.5 of 30.5 GB) and every cell healthy, so the true ceiling is above 50 and, once more, the swap device is the wall, not RAM (2.2 GB free) and not CPU (load 5). Compression stayed at 3.7x: 30.5 GB of pages in 8.2 GB.
- **Batched boots removed the failure mode.** No OOM kill and no OpenClaw startup-lease failure across 50 boots, where run 1 had one of each at 45. The cost is a slower step (40 s for ten cells vs 26 s).
- **Wake on zram for a reclaimed cell is 1.5–2.6 s and flat across the ladder** (p95 2.0–2.6 s at every step from 10 to 50): it does not degrade with cell count, because it is bounded by single-threaded zstd decompression of that one cell's ~500 MiB, not by host load. The one 122 ms sample was a cell woken before its reclaim landed. The lever to get under 1 s is a smaller swapped set per cell (lower the working set that leaves RAM) or parallel swap-in, not filtering.
- Per-cell cost at 50 cells: 13.1 GB RAM + 8.2 GB zram for 50 cells ≈ 262 MB RAM and 164 MB compressed swap per cell, against ~750 MiB per stock cell resident.

## Real Telegram push-wake through a public address (08:50–09:16 UTC)
Setup, all free: `167-233-118-221.sslip.io` (public DNS that resolves any ip-with-dashes name to that IP, nothing to register) + Caddy with an automatic Let's Encrypt certificate (issued in seconds), exposing only `/hook/*` and `/healthz`; the daemon on loopback; a cell created with `fleetd cells create -ingress-url https://… -telegram-token-file …`, so the provisioner wrote the webhook URL and verify secret into the cell's config and OpenClaw registered the webhook with Telegram itself on start (confirmed by Telegram's getWebhookInfo). An unsigned POST to the public hook URL is refused 401 without a wake.

Three real messages sent to the bot while the cell was paused and reclaimed (~600 MiB in zram):

| Message | Wake | What Telegram saw at the ingress | Delivered on attempt | Cell replied |
|---|---|---|---|---|
| 1 (original build) | 2.32 s | 502, 502, then 200 | 3rd | yes (pairing notice for an unknown sender) |
| 2 (wait-for-TCP-accept fix) | 2.11 s | 502, 502, then 200 | 3rd | no reply by design: OpenClaw sends the pairing notice once per unpaired sender |
| 3 (request-probe fix) | 1.91 s | 500 after 2.1 s, then 200 at 51 ms | 2nd | no (same reason) |
| 4 (ingress retries the cell's post-thaw 5xx) | 1.81 s | **one 200 in 2.2 s, no retry** | **1st** | no (same reason) |

**Result: with the fourth build a real Telegram message wakes a reclaimed cell and is delivered on the platform's first push, 2.2 s end to end.** What the earlier failures were: on thaw OpenClaw restarts its Telegram channel (`starting provider` → `webhook local listener` → `webhook advertised`, ~1 s after /health answers). The container's port mapping accepts connections throughout, so a TCP-accept probe passed and the proxy got a reset (messages 1–2). The request probe waits for the listener to answer, which removed the resets (message 3), but the cell then answered the first real update with 500 while its channel was still finishing its restart, and Telegram's automatic retry delivered. Every message was delivered; the cost of the remaining gap is one platform retry (~2 s).

Wake times on zram for this ~600 MiB reclaimed cell: 1.9–2.3 s, consistent with the density run.

## First real agent turn after reclaim (10:14 UTC)
Cell tgw: owner paired (Telegram sender approved via `pairing approve telegram <code>`), Anthropic key in the cell's own config (`models.providers.anthropic.apiKey`, 0600, never in env or argv), default model `anthropic/claude-haiku-4-5-20251001` (the build's default is an OpenAI model, which produced a 401 until changed). Cell paused and reclaimed, 622 MiB in zram. User sent "what is 17 times 23".

| Event | Time from Telegram's push |
|---|---|
| Push arrives at Caddy; ingress verifies, prefetches (4,995 MiB advised, 622 MiB in swap), unpauses | 0 |
| Gateway healthy; request forwarded, 200 to Telegram | 2.36 s |
| Cell's first attempt at the turn aborts: `session placement turn settlement is closed`; it sends the user a "heartbeat failed, the main chat session remains available" notice | 3.3 s |
| Cell retries, model answers, **"17 x 23 = 391" delivered** | **8.7 s** |

Findings:
- **A real agent turn completes after page-in from zram, end to end in 8.7 s**, of which 2.3 s is the wake and ~5 s is the model round-trip plus OpenClaw's own retry. The turn itself did not fault noticeably: the reclaimed pages the model client and session store needed were prefetched.
- **Third post-thaw gap inside OpenClaw**, after the listener (fixed by the request probe) and the channel (fixed by the retry): its session placement is still "closed" for roughly a second after thaw, so the first turn aborts and a diagnostic notice reaches the user; the turn succeeds on OpenClaw's own retry. Nothing outside the cell can see this state either. For the upstream thread: the restored-admission "ready" signal in #127602 is exactly what would let a host hold the first message until placement has reopened.
- Operating rule learned twice today: any command run inside a cell (pairing, config set) must first move the cell out of the hibernate tier, or the daemon pauses it mid-command and freezes the exec.

### Is there a readiness signal after thaw? (11:45 UTC) — no, not in this build
Sampled `/health` and `/ready` every 50 ms across a real thaw after a 95 s pause (long enough to trip OpenClaw's freeze detector: `liveness heartbeat delayed: overdue=38315ms`). Both answered 200 from 92 ms after unpause and never changed for the next 11 s, while the gateway was still logging its recovery. `/ready` (`{"ready":true}`) is a liveness alias in 2026.9.6. The internal states that matter (`placementReady`/`placementNotReady`, `admissionOpen`/`admissionClosed`, `placementFactState`) exist in the shipped code but are never exposed on any HTTP or protocol surface; the only external hits are control-UI translation strings. So there is nothing for a host to poll today. The certain alternative is the gateway's own log, which records the recovery sequence with millisecond timestamps.

### Thaw settle: what certainty is available (12:00–12:15 UTC)
A pause wake now reads the cell's own log for OpenClaw's freeze-detector line and, if it fired, holds the first forwarded message for a measured window before proxying. Two facts from the shipped code and two thaws: the session placement's turn settlement is closed by an internal closure after thaw and its reopening is neither logged nor exposed (the `[admission] reopened` line belongs to another subsystem and arrived 30 s after a wake whose turn had already succeeded at 10 s); so the trigger is certain, the end is not observable. The hold is therefore keyed on the trigger with a bound of 3 s (measured window 22 ms–800 ms), and returns at once when no trigger was logged. With the settle on the first turn succeeded without the "heartbeat failed" notice (heartbeat then turned off; being re-verified with the heartbeat at default). A gateway-side ready signal would replace the bound; that is the upstream ask.

### Confirmation with the tenant's default configuration (12:18 UTC)
Heartbeat restored to default; cell paused and reclaimed; one question sent. Detector fired at +0.0 s (`host timing gap detected`), the supervisor read it from the cell's log and held the first forward for the remainder of the 3 s window, Telegram saw a single 200, the cell logged **no settlement abort and no "embedded agent failed"**, and exactly one outbound send followed: the answer, ~11 s after the push. **No "heartbeat failed" notice, with nothing changed in the tenant's cell.** The settle covers OpenClaw's post-thaw window from outside the cell, using only the cell's own evidence.

## Volume test: 50 hibernated cells, 10 signed wakes at once (12:30–12:50 UTC)
Setup: tgw plus 49 more webhook-mode cells (v1..v49, same image, same config, Telegram channel disabled in the 49 because one bot token can only advertise one webhook), booted in batches of 5, then left to the supervisor until all 50 were paused and reclaimed: `paused=50 reclaims=50 used=13.4 GB zram=30.5 GB`, 195 MB free. Ten signed Telegram updates were then POSTed to the public ingress within 12 ms of each other (tgw and v1..v9), each with its own cell's verify secret; the daemon's wake concurrency was left at its default of 3.

| Wave | Cells | Unpause + prefetch | Ready (/health) after the request | Note |
|---|---|---|---|---|
| 1 | v1, v5, v9 | 18.2–18.6 s | 24.5–24.7 s | first three page-ins share one saturated zram stream while the host has no free RAM; the earlier single-cell 1.5–2.6 s does not hold here |
| 2 | v2, v4, v7 | 0.07–0.12 s | 7.2–7.5 s | started after wave 1 released its slots |
| 3 | v3, v6, v8 | 0.09–0.10 s | 7.1–7.2 s | |
| 4 | tgw | 0.43 s | 51.7 s | tenth in the queue; its own thaw took about 12 s |

Per request at the ingress: tgw **200 in 54.7 s**, one delivery, no retry; v1..v9 502 after 45–60 s, expected because their Telegram channel was disabled so nothing listened on their hook port after the (successful) wake. Prefetch advised ~4.4 GiB per cell with ~700 MiB in swap and took 2.1–3.8 s each. Detector fired on every cell; each first forward was held 2.6–3.0 s.

After: still paused 40, running 10, exited 0, `wake_failures_total 0`, no OOM kill. Load 25–35 during the wakes, 1.7 four minutes later. Cleanup: the 49 cells removed; host back to 13.9 GB free with tgw paused.

Findings:
- **Ten simultaneous wakes on a fully reclaimed 50-cell host complete without a failure, but wall time is dominated by page-in contention, not by OpenClaw.** The first wave took 25 s where a lone reclaimed cell takes 2 s: ten cells' ~7 GB of pages had to come back into a host with 195 MB free, so every page-in also evicted someone else's pages to zram, on one zstd stream. Once the first wave was resident the next waves settled to ~7 s each.
- **The wake queue works as designed** (3 at a time, no wake lost, the tenth answered 200), but the tenth message waited 52 s. At this density the honest SLA for a burst of ten is "under a minute for the last one", and "2 s" only for the first cell woken into free RAM.
- **Levers, in order of expected effect:** keep RAM headroom of roughly `wake_concurrency × per-cell working set` (here 3 × 700 MiB ≈ 2 GB) rather than filling the host to the last 200 MB; raise wake concurrency only together with that headroom; multi-stream zram or NVMe swap for parallel page-in; and a lower per-cell swapped set (the reclaim floor of 150 MiB already keeps the gateway's hot pages resident, which is why the settle-and-first-turn path still worked). These are Phase 1 tuning items, not blockers.
- The test also reconfirms the three post-thaw gaps at scale: every cell logged the detector, every first forward was held, and the one cell with a live channel answered on the first delivery.

# 2026-09-28

Correction to the 27 Sep volume test: the daemon's default wake concurrency is 4, not 3 (the waves were v1/v5/v9 plus tgw, whose thaw took 51 s). And the 49 volume cells' state directories were not removed on 27 Sep (the glob ran in an unprivileged shell); they were removed today before the rerun.

## Disk tier: where the reclaimed pages live (10:43–10:56 UTC)
Same host after a reboot (kernel 6.8.0-142, `linux-modules-extra` reinstalled for it because zram would not load). Root disk is the Hetzner cloud volume (`fio`, direct, QD32: 4 KiB random read 35k IOPS ≈ 137 MB/s; 32 KiB random read 24.7k IOPS ≈ 770 MB/s). Cell tgw, three hibernate → reclaim (150 MiB floor) → wake cycles per configuration, driven through the daemon API, prefetch on; `vmstat 1` and `/proc/pressure` sampled across each wake.

| Where the ~600 MiB went | Wake to /health | Prefetch call | Page-in rate (si peak) | PSI during wake |
|---|---|---|---|---|
| NVMe swapfile only (`vm.page-cluster=3`) | **1.45 s, 1.81 s, 1.98 s** | 0.63–0.90 s | 500–640 MB/s | io full 114–477 ms, mem full ≤173 ms |
| zram only (zstd, same as the density runs) | 2.47 s, 2.34 s, 2.32 s | 1.47–1.65 s | 290–310 MB/s | io full 166–227 ms, mem full ≤48 ms |
| zram with writeback of idle pages to a disk-backed loop device | 8.37 s, 6.96 s, 6.58 s | 4.8–5.9 s | 86–110 MB/s | io full 1.5–1.9 s, cpu some ~280 ms |

Findings:
- **A plain NVMe swapfile wakes a reclaimed cell faster than zram on this host** (1.5–2.0 s vs 2.3–2.5 s) because swap-in from the disk runs at 500–640 MB/s with the kernel's 32 KiB read clustering, while zram is bounded by one zstd stream at ~300 MB/s. And it costs no RAM: zram held 30.5 GB of cell pages in 8.2 GB of memory; the swapfile holds them in none.
- **zram writeback is not the cold tier.** The driver reads written-back pages one page at a time through the backing device with no clustering, so wakes were 3–4x slower than the plain swapfile. Dropped.
- Decision: the hibernation store is a swapfile on the host's NVMe; zram is optional and off by default. Prefetch is unchanged (it asks the kernel for the pages and does not care where they are).

## Burst repeat with the cold set on disk: 50 hibernated, 10 signed wakes at once (10:57–11:07 UTC)
Same procedure as 27 Sep (tgw + v1..v49, batches of 5, Telegram disabled in the 49, 10 signed updates within 10 ms, wake concurrency 4) but with the swapfile instead of zram. Before the burst: 50 paused, **34.0 GB in swap, 10.3 GB free** (27 Sep: 195 MB free).

| Wave (4 at a time) | Cells | Ready after the request | 27 Sep (zram, no headroom) |
|---|---|---|---|
| 1 | v3, v6, v7, v8 | 9.1 s | 24.5–24.7 s |
| 2 | v2, v4, v9, tgw | 7.8–7.9 s (done at +17 s) | 7.2–7.5 s (done at +32 s) |
| 3 | v1, v5 | 6.4–6.6 s (done at +24 s) | 7.1–7.2 s (done at +39 s) |
| tgw (live channel) | | **200 in 19.6 s**, first delivery | 200 in 54.7 s |

`vmstat` during the burst: swap-in 440–690 MB/s sustained in four bursts; **swap-out 0** (no cell was evicted to make room); CPU idle 30–55 %, I/O wait 26–57 %. Zero wake failures, zero exits, 50 cells still healthy afterwards.

Findings:
- **Headroom removed the memory contention entirely**: nothing was paged out during the burst, and the last of ten cells was ready in 24 s instead of 52 s, the first four in 9 s instead of 25 s.
- **What remains is disk bandwidth.** Ten cells are ~7 GB of pages; at ~550 MB/s that is ~13 s of pure page-in, shared four ways per wave (each cell's ~700 MiB takes ~5 s at a quarter of the device), plus the 3 s thaw settle and health wait per wave. The single-cell 1.5–2 s only holds when the disk is otherwise idle.
- Levers now, in order: a faster swap device (a real local NVMe does 2–3 GB/s; this cloud volume tops out near 770 MB/s); a smaller swapped set per cell (a higher floor for cells likely to wake soon); zram and the swapfile used together so a burst draws on CPU and disk at once; and the settle window, which becomes a visible share of the wake once page-in is fast.
- Method note: the run script waited on its own `vmstat` sampler and hung after the burst; the timings above are from the daemon journal and the vmstat file, PSI deltas for the burst were lost.

## Burst-size curve at 50 cells, then the ceiling with the disk store: 75 and 100 cells (11:24–11:45 UTC)
Wakes through the daemon API (`POST /wake/<cell>`), so each cell's time is "request → cell healthy, including the 3 s thaw settle"; cells v1..v10, all paused ≥15 min and fully reclaimed to the swapfile (~700 MiB each) before every burst; wake concurrency 4; every cell re-hibernated and re-reclaimed between bursts.

| Cells on host | Simultaneous wakes | Fastest | p50 | Slowest | Failures |
|---|---|---|---|---|---|
| 50 | 1 | 4.65 s | 4.65 s | 4.65 s | 0 |
| 50 | 2 | 3.5 s | 3.5 s | 5.7 s | 0 |
| 50 | 4 | 5.3 s | 5.5 s | 8.2 s | 0 |
| 50 | 8 | 4.5 s | 8.4 s | 14.6 s | 0 |
| 50 | 10 | 7.6 s | 15.5 s | 21.2 s | 0 |
| 75 | 10 | 9.9 s | 19.5 s | 26.3 s | 0 |
| 100 | 10 | 9.6 s | 18.8 s | 24.7 s | 0 |

Host at each step (all cells paused and reclaimed):

| Cells | RAM used (incl. OS) | RAM free | In swap | Exited / OOM |
|---|---|---|---|---|
| 50 | 4.6 GB | 10.3 GB | 34 GB | 0 / 0 |
| 75 | 5.4 GB | 9.4 GB | 52 GB | 0 / 0 |
| 100 | **5.7 GB** | **9.1 GB** | **70 GB** (two 40 GB swapfiles) | **0 / 0** |

Boots to 100 were batched (5 at a time); each batch transiently drove free RAM to ~250 MB and load to 24 while the new cells' pages were still resident, and the supervisor reclaimed them within two minutes. Cumulative: 65 reclaimed-cell wakes today, 0 failures.

Findings:
- **100 stock OpenClaw cells on one 16 GB host, all healthy, 9 GB of RAM still free.** That is 2.9x the stock+zram ceiling of 35 and ~5x stock without zram (18–20 projected). The per-cell RAM cost of a cold cell is ~50 MiB (5.7 GB minus ~1 GB of OS and daemon, over 100), well under the 100 MiB estimate; the cost moved to disk at ~700 MiB per cell. On this box the next limit is disk space (150 GB), not memory.
- **Burst latency does not depend on how many cells are on the host.** Ten wakes at 50, 75 and 100 cells all took ~10 / ~19 / ~25 s per wave; the host had ~9 GB free at every step, so nothing was evicted (swap-out 17–553 MB per burst, noise). The curve that matters is burst size, not density.
- **Burst size curve on this cloud volume:** one wake 4.7 s, two 5.7 s, four 8.2 s, eight 14.6 s, ten 21 s for the last cell. Roughly 2 s per additional cell, which is 700 MiB at the disk's ~550 MB/s shared across the wave, plus the settle per wave. PSI confirms the stall is I/O: `io full` 2.1 s during the burst of four, 4.2–5.8 s during bursts of eight and ten, against `memory full` of at most 1.5 s.
- **The 3 s thaw settle is now the largest fixed share of a single wake** (4.65 s wall for one cell, of which ~1.6 s is page-in). A gateway-side ready signal, or a measured shorter bound, would cut single-wake latency by up to 60 %.
- Method note: the `vmstat` sampler stopped reporting for the later bursts (si_peak=0 rows); PSI and the per-cell times are complete.

## Compressed swap on disk: ZFS zvol (lz4) as the swap device — rejected (12:17–13:09 UTC)
Idea (user): store the cold pages compressed on disk so the same disk bandwidth carries 2–4x more pages, and decompress on CPU, which has headroom. The kernel has no compressing swap path on 6.8 (zram writeback and zswap both write raw pages; a btrfs swapfile cannot be compressed; dm-vdo is 6.9+), so the test used an lz4-compressed ZFS volume (16 KiB blocks, ashift 12, metadata-only cache, ARC capped at 512 MiB, sync=always, loop device with direct I/O over a file on the root disk) as the highest-priority swap device.

| Step | Result |
|---|---|
| Compression ratio on idle OpenClaw heaps, lz4 | **2.07–2.10x** (zram with zstd measured 3.7x) |
| Single-cell wake, tgw, 3 cycles, pages on the zvol | 6.4 s (first, mixed devices), 2.4 s, 2.2 s — no better than the raw swapfile (1.5–2.0 s); page-in 340–650 ms but ~0.6–1.0 s of CPU stall per wake |
| Migration by re-waking cells | does not move clean pages: a page read back from swap keeps its slot on the old device unless it is rewritten; only ~30 % of the set moved |
| Migration by `swapoff` of the raw file (22 GB) with the zvol as target, 50 cells | pages moved at ~60 MB/s with free RAM at 180–340 MB and load 6→22; **host hung after ~2.8 min**, SSH dead, CPU pegged; hard reset |
| Controlled refill after reset: zvol as the only swap, cells started 5 at a time, waiting for free RAM > 3 GB between batches | **host hung again at 15 cells with 1.8 GB on the zvol** during the daemon's own reclaim plus a boot batch (free 3 GB, load 28); hard reset. No kernel message survived either hang |

Findings:
- **Swap on a ZFS volume is not usable here**: two hard hangs in an hour, one under system-wide pressure (the documented risk) and one under ordinary supervisor reclaim with gigabytes free. ZFS needs memory to write, and the swap path is where memory is being taken away. Nothing in our process can make that safe for a provider's host.
- The ratio a compressed store would get on real heaps is ~2x with lz4, less than zram's 3.7x with zstd; the potential bandwidth gain is therefore ~2x, not 4x, and single wakes would not improve because the decompression shows up as CPU stall in the same place the disk stall was.
- The idea itself stays valid; the mechanism has to be a compressing block layer that does not allocate in the swap-out path: dm-vdo (kernel 6.9+, worth one test on an HWE kernel), or compression done by the supervisor itself in user space (a checkpoint-style dump of a paused cell's anonymous memory, restored before unpause), which is a larger build.
- Operational lesson: the host had zram re-enabled by the reboot (the service was only stopped, not disabled) and the root disk filled to 100 % when the pool file was created next to two swapfiles; both fixed. Provisioning must own swap layout explicitly.

# 2026-09-29

## Pipelined page-in and reclaim on pressure: ten-wake burst at 50 cells, before and after (11:47–12:05 UTC)
Same host and cells as 28 Sep (50 hibernated on the 40 GB swapfile, ~650 MiB per cell in swap, 9.5 GB available), wakes through the daemon API, each time is request → healthy including the thaw settle. Three daemon builds in sequence, then the headroom policy.

| Build | Concurrency rule | Fastest | p50 | Slowest | Wall for all ten |
|---|---|---|---|---|---|
| Before (28 Sep binary) | 4 slots held for the whole wake (page-in, unpause, health, settle) | 8.2 s | 16.1 s | 21.9 s | 22.0 s |
| Slot released after the advise + unpause | 2 slots, page-in only issued | 11.3 s | 11.5 s | 12.0 s | 12.0 s |
| **Slot held until the pages have landed** | 2 slots, released when the cgroup's resident bytes stop growing | **7.5 s** | **11.9 s** | **14.6 s** | 14.6 s |

Per cell in the final build: advise 1.4–1.8 s, landing wait 0.5 s (85 % of the swapped set back before the growth stalls), unpause 0.05–0.1 s, health ~2 s under a burst (0.1 s solo), settle 3 s. Pairs started page-in 2.2 s apart; the last pair began at ~9 s and finished at 14.6 s.

Findings:
- **Releasing the slot at "reads issued" collapses the burst to the bandwidth floor but makes every cell wait for every other** (all ten at 11.3–12.0 s): `process_madvise` returns once the reads are queued, so all ten page-ins overlapped and shared the disk. Holding the slot until the pages have actually landed (watching `memory.current`, since `memory.swap.current` does not fall for pages that keep their swap slot) restores FIFO: the first pair at 7.5 s, then one pair every ~2.3 s. Last cell 21.9 → 14.6 s, first 8.2 → 7.5 s, median 16.1 → 11.9 s.
- **What is left in a wake is now settle and health, not disk**: of a 7.5 s first-pair wake, 2.3 s is page-in, 3 s is the settle bound and ~2 s is the gateway answering /health while it faults its last ~15 % in. A shorter settle bound (measured window ≤ 0.8 s) is the next lever.

### Reclaim on pressure (headroom policy)
Daemon relaunched with `-reclaim-after 30m -headroom-mib 2048`: cells that pause stay resident while the host has ≥ 2 GB available. v1..v10 woken, left idle, paused by the supervisor; **none reclaimed** (available 4.0 GB), registry shows all ten resident.

| Burst of ten, cells resident (paused, not reclaimed) | Fastest | p50 | Slowest |
|---|---|---|---|
| | **0.71 s** | **0.94 s** | **1.17 s** |

No disk reads (`io full` 8 ms), no memory stall; the freeze detector does not fire on a pause this short, so the settle returns at once and the wake is unpause + health.

Then relaunched with `-headroom-mib 12000` (above what the host can have): the loop logged `memory headroom below target … cells=10` on its next pass and reclaimed all ten resident cells (each 675–714 → 149 MiB, 608–636 MiB to swap), available 4.0 → 9.4 GB. Timed reclaim untouched.

Findings:
- **Recently active tenants wake in under a second, cold ones in 2–15 s, and the host chooses which is which by memory pressure, longest-paused first.** With 9 GB free at 50 cells (or at 100), roughly a dozen tenants can be kept warm at no cost to density; a provider sizes the headroom to their active share.
- The policy needs no per-cell configuration and no change inside the cell: it is one flag plus the existing reclaim path.

## Thaw settle bound: how short can the hold be? (12:26–12:54 UTC)
Cell tgw (real Telegram channel, paired owner, Anthropic key), 15 cycles: wake → hibernate → 75 s (reclaim at 20 s; the freeze is long enough for OpenClaw's freeze detector) → one signed Telegram update from the paired owner through the public ingress ("reply with the single word OK") → 25 s → read the cell's own log. Pass = the detector fired, the first turn was **not** aborted (`settlement is closed` absent), no "heartbeat failed" / "embedded agent failed" notice, exactly one outbound send (the answer). Five wakes per bound.

| Settle bound | Hold applied | Detector fired | First turn aborted | Notice | Answers sent | Push → 200 at the ingress |
|---|---|---|---|---|---|---|
| 3 s (previous default) | ~2.9 s | 2/2 | 0 | 0 | 2/2 | ~11 s to the answer (28 Sep) |
| 1.5 s | 1.45–1.47 s | 5/5 | 0 | 0 | 5/5 | 3.9–4.9 s |
| 1 s | 0.95–0.97 s | 5/5 | 0 | 0 | 5/5 | 3.0–4.2 s |
| 0.5 s | 0.44–0.46 s | 5/5 | 0 | 0 | 5/5 | 3.1–3.6 s |

Findings:
- **15 of 15 real turns completed cleanly with the hold cut to as little as 0.5 s.** The measured settlement window (22–800 ms, 28 Sep) is the upper end of what the gateway needs; in practice the channel restart plus the ingress's own request probe already absorb most of it.
- **Default changed to 1 s** (from 3 s): five clean turns and 20 % margin over the longest window ever observed. Saves 2 s on every cold wake and 2 s per wave in a burst; a cold wake through the public ingress is now ~3–4 s from the platform's push to the 200, including page-in from the swapfile.
- The bound is a flag (`-thaw-settle`); a provider on a slower host can raise it. The right fix remains a gateway-side ready signal (#114145 / #127602), which would replace the bound with a fact.

## 100 cells with the final build: cold and warm bursts, and the headroom rule (13:09–13:51 UTC)
Cells v50..v99 recreated (two 40 GB swapfiles), booted 5 at a time paced on available memory, under the boot-service daemon (pipelined page-in ×2, settle 1 s, timed reclaim 30 m, headroom 3 GB). One cell (v72) exited at first boot with OpenClaw's doctor refusing a state migration ("store unavailable" for the maintenance lease): the test script restarts each new container seconds after creation, which interrupted its first-boot migration; a plain `docker start` brought it healthy in 18 s. A supervisor artefact of the test, not of the host.

At 100 cells, all paused: used 12.0 GB, available 3.6 GB, 64.7 GB in swap (~720 MiB per cold cell), 10 cells resident under the headroom policy, 0 exited, 0 OOM.

| Burst of ten at 100 cells | Headroom target | Available before | Fastest | p50 | Slowest | PSI memory full / io full |
|---|---|---|---|---|---|---|
| Cold (all ten reclaimed) | 3 GB | 3.6 GB | 19.9 s | 23.1 s | 25.6 s | 7.7 s / 12.8 s |
| Warm (all ten resident) | 3 GB | 3.8 GB | 0.72 s | 0.99 s | 1.39 s | 0 / 0 |
| Cold, 7 of 10 reclaimed | **8 GB** | 8.3 GB | **4.1 s** | **7.7 s** | **10.5 s** | 1.4 s / 2.3 s |
| For reference, 28 Sep, timed reclaim only, 9 GB free | – | 9.1 GB | 9.6 s | 18.8 s | 24.7 s | – |

Findings:
- **The headroom target must cover the burst, not just the page-in.** With 3 GB the ten woken cells (7 GB) had nowhere to land, the kernel evicted resident cells while the daemon's own pressure reclaim ran, and the burst was worse than with no warm tier at all (7.7 s of memory stall). With 8 GB the same burst ran at page-in speed: last cell 10.5 s against 24.7 s yesterday and 25.6 s an hour earlier. Rule for the unit file: **headroom ≈ expected simultaneous wakes × 0.75 GB**; 8 GB on a 16 GB host.
- **The warm tier is what is left after that headroom.** At 100 cells on 16 GB (5 GB of cold floors + 1 GB OS + 8 GB headroom) that is a couple of cells; at 50 cells it is about ten. Warm wakes are 0.7–1.4 s regardless of density.
- Final shape on this host, 100 cells: warm wake ~1 s, single cold wake ~3–4 s through the ingress, ten cold wakes at once 4–10.5 s, zero failures across 100 cells and every burst today.

# 2026-09-30

## zswap as a compressed tier in front of the swapfile — no warm tier, marginal burst gain (14:46–14:59 UTC)
Idea: the kernel's own compressed cache with LRU writeback to the swapfile and a pressure-driven shrinker (6.8), so recently reclaimed cells would sit compressed in RAM with the right aging and no ZFS. Runtime switch only: `zpool=zsmalloc`, `max_pool_percent=20`, `shrinker_enabled=Y`, compressor lz4 then zstd. Single-cell cycles on tgw under timed reclaim (so every cycle really reclaims), then the ten-wake burst at 100 cells under the shipped policy (headroom 8 GB, settle 1 s, page-in ×2).

| Configuration | Reclaimed per cycle | Wake to /health (3 cycles) | Bytes that reached the zswap pool |
|---|---|---|---|
| zswap off (disk baseline, same day) | 658–677 MiB | 2.46 s, 2.50 s, 2.55 s | – |
| zswap lz4 | 658–667 MiB | 2.40 s, 2.61 s, 2.43 s | pool 23 MB after three cycles |
| zswap zstd | 663–680 MiB | 2.52 s, 2.45 s, 2.52 s | pool 52 MB after three more |

| Ten-wake burst at 100 cells, headroom 8 GB | Cold cells | Fastest | p50 | Slowest | Pool before / after | PSI mem full / io full |
|---|---|---|---|---|---|---|
| zswap lz4 | 9 of 10 | 3.0 s | 5.4 s | 9.8 s | 319 MB / 202 MB zswapped (lz4 ratio 1.6x) | 48 ms / 2.2 s |
| 29 Sep, disk only | 7 of 10 | 4.1 s | 7.7 s | 10.5 s | – | 1.4 s / 2.3 s |

Findings:
- **The compressed tier never received the cells' pages.** A page read back from swap keeps its slot on the swapfile; when the cell is reclaimed again the kernel drops the clean page without writing it. Only pages the cell dirtied while awake are written, and an idle cell dirties ~30–60 MB between wake and re-pause. So after a cell's first reclaim, ~90 % of it lives on disk permanently and zswap (or any front tier) sees the remaining ~10 %. Single-cell wakes were identical to disk in all three configurations.
- **The burst improved modestly (p50 7.7 → 5.4 s, last 10.5 → 9.8 s) with about a tenth of each cell served from RAM**, but the two runs also differed in how many of the ten were cold (7 vs 9), so the attributable gain is small. No stall, no failures, pool shrank under the burst as designed.
- This is the same mechanism that made the 28 Sep "migration by re-waking" fail and it bounds every compressed-front-tier design on a host with persistent swap: the only way a cell's whole working set sits compressed in RAM is zram as the *only* swap device, which brings back the RAM cost and the 35-cell ceiling.
- Decision: zswap off by default. Harmless when on (no CPU or stall penalty measured, 1.6x on the dirty set), so a provider may enable it, but it is not a warm tier. The warm tier remains "resident under headroom" and the cold tier the swapfile.
- Side effect worth knowing: because clean pages keep their slots, hibernating a cell that was woken and stayed idle writes almost nothing to disk; swap usage grows to one full copy per cell (~700 MiB) and then stays flat.

## Compressed tier, best case: fresh cells fully inside a zswap zstd pool (01:35–02:13 UTC)
Setup: 100 cells hibernated on the swapfiles after a power-on (swapfiles and daemon came up from fstab and the boot unit; cells restarted in paced batches). zswap zstd, zsmalloc, pool up to 30 % of RAM, shrinker on. tgw and v1..v10 restarted so none of their pages had a disk slot; their first reclaim therefore went into the pool: **7.0 GB of pages in 1.97 GB of RAM, zstd ratio 3.6x**, 147–154 MiB of pool per cell. Daemon on the shipped policy (headroom 8 GB, settle 1 s, page-in ×2).

| Measurement | From the pool (zstd) | Disk only, same build | Resident (warm) |
|---|---|---|---|
| Single wake, tgw (~950 MiB set), 3 cycles | 3.37 s, 3.23 s, 3.68 s | 2.46–2.55 s (29 Sep, ~660 MiB set) | – |
| Burst of ten, first | 4.4 / 9.8 / 18.3 s (min / p50 / max) | 4.1 / 7.7 / 10.5 s | 0.7 / 1.0 / 1.4 s |
| Burst of ten, second (cells re-cycled once) | 3.6 / 8.3 / 14.5 s | – | – |
| PSI during the first burst | cpu some 6.7 s, memory full 2.5 s, io full 0.2 s | memory full 1.4 s, io full 2.3 s | none |

Findings:
- **Even in its best case the compressed tier is slower than the disk here.** Decompression is one zstd stream per cell at ~500 MB/s, the same order as the disk, and a burst adds CPU contention (6.7 s of CPU stall) plus zswap writeback: the pool is RAM the woken cells need, so the shrinker wrote 1.7 GB of pool to disk during the burst while the wakes were paging in. The stall moved from the disk to the CPU and the burst got worse (18 s vs 10.5 s).
- **The tier does hold cells densely** (3.6x: about five cells per GB, and the cells stayed in the pool across a wake cycle because zswap keeps the entry for a clean page), but at 100 cells on 16 GB that RAM is the burst's headroom. On a host with spare RAM and a slow disk the same numbers make it attractive; on this class it is a loss.
- Closes the compressed-tier line for cloud hosts with NVMe-class disks: zram (RAM cost, wrong aging), zram writeback (7 s wakes), ZFS (hangs), zswap on existing cells (only the dirty 10 %), zswap on fresh cells (slower than disk, competes with headroom).

## Trimmed warm cells: drop a resident cell's cold pages, keep the hot set (02:17–02:19 UTC)
Question: can a resident cell keep only its hot pages in RAM? Five cells woken, paused by the API, left resident (headroom target lowered to 3 GB for the test so the daemon would not reclaim them), then `memory.reclaim 300M` written to each cgroup, then all five woken at once.

| State | Resident per cell | Woken five at once | Health after |
|---|---|---|---|
| Fully resident (warm) | 661–684 MiB | 0.7–1.4 s (29 Sep) | 200 |
| **Trimmed** (300 MiB dropped in 1.0 s for all five) | **363–386 MiB** | **1.16–1.29 s** | 200; resident 374–396 MiB 20 s after the wake |
| Cold (floor 150 MiB, ~700 MiB in swap) | 149 MiB | 3–4 s alone, 4–10.5 s in a burst | 200 |

Findings:
- **A trimmed warm cell costs ~380 MiB of RAM and wakes in 1.2 s, and needs no compression.** The dropped pages were clean and already on the swapfile (their swap slots survive a wake), so the trim wrote nothing, took a second for five cells, and the woken cell simply faults back the few pages it touches; 20 s after the wake it was running at ~390 MiB, healthy.
- This is a third tier between warm (700 MiB, ~1 s) and cold (150 MiB, 3–4 s): **1.8x more warm cells per GB for a 0.3 s slower wake.** It is the existing reclaim path with a higher floor (`-reclaim-keep-mib` ~400 for the warm set, 150 for the cold set) rather than a new mechanism, and it answers the "idle pages of a resident cell" question: they can be dropped for free once the cell has been reclaimed once; compressing them buys nothing.
- To build: a warm floor policy in the daemon (reclaim resident-paused cells to the warm floor immediately, to the cold floor only under headroom pressure), one flag, and a rerun of the warm burst.

## Alive cells under memory.high: the idle gateway's real working set (02:39–02:56 UTC)
Ten running (unpaused, daemon stopped for the test) cells per variant, `memory.high` set on their cgroups while alive, 210 s per step; the cells' own logs scanned for `liveness`, `event_loop`, `memory pressure` and `heartbeat delayed`; /health latency sampled 50 times per step; PSI and load recorded.

| Variant | Step | Resident per alive cell | Cell diagnostics | /health avg / worst | Host |
|---|---|---|---|---|---|
| zswap zstd, v1..v10 (had been trimmed/reclaimed before) | no limit | 270 MiB (already lazy after the earlier trim) | none | 2 / 5 ms | load 0.8 |
| | high 450 MiB | 276–282 MiB | none | 1 / 9 ms | cpu some 2.4 s over 90 s |
| | high 350 MiB | 268–277 MiB | none | 1–4 / 22 ms | – |
| disk only, v11..v20 (fully resident at start) | no limit | 681 MiB | none | 2 / 7 ms | – |
| | high 450 MiB | 429 → 393 MiB | none | 6 / 58 ms during eviction, then 2 / 4 | memory some 7.7 s over the first 90 s |
| | high 350 MiB | 287 → 295 MiB | none | 1 / 3–4 ms | load 1.3 |

Findings:
- **An idle alive OpenClaw gateway runs quietly at ~280–300 MiB, not 700.** Under `memory.high` 350 MiB the disk-only cells settled at 287–295 MiB with zero liveness, event-loop or memory-pressure diagnostics in their own logs and sub-10 ms health latency; the cells that had been trimmed earlier were already at ~270 MiB without any limit. The other ~400 MiB is cold heap the idle gateway never touches.
- **Compression is not needed for idle-alive cells either.** The disk-only variant reached the same resident size with the same silence; the only visible cost was ~58 ms worst health latency during the first eviction wave. zswap would matter only for a cell that wakes up its cold heap often (an active conversation), where a decompression beats a disk read.
- What this changes: an "idle-alive" tier at ~300 MiB per cell (2.3x more alive tenants per GB than today's 700 MiB), and the warm/trimmed floors can sit at ~300 MiB rather than 400. The knob is a soft limit that must be lifted when a cell becomes active (a growing heap throttled at `memory.high` would slow a live turn), so it belongs in the supervisor: low `memory.high` while the cell is idle-alive or paused, `max` from the first inbound message until idle again.
- Not measured yet: an active conversation on a limited cell (turn latency with the heap re-faulting), and the RAM saving at scale (the 100-cell budget becomes ~30 alive + ~70 cold on 16 GB with an 8 GB burst headroom, versus ~3 alive today).

## Resident tier at a 350 MiB cap, KSM dedup, and the lazy cold wake (03:22–04:24 UTC)
100 cells on the host throughout. For every state: resident cells, headroom, burst. Daemon on timed reclaim 30 m; headroom target lowered to 2 GB while resident tiers were being built so the daemon would not reclaim them; prefetch on except in D. "Resident" = paused cells whose pages are in RAM (capped at 350 MiB via `memory.high`).

Caveat on absolute headroom: the host had not been rebooted since the zswap and alive experiments, and carried ~2 GB of leftovers (zswap pool entries, swap cache, page tables of cells that had been alive), so "all cold" showed 4.3 GB available against 9.1 GB on a clean host the day before. Differences between states are valid; absolute headroom is ~4 GB low.

| State | Resident cells | Resident RAM | Available | Burst of ten | Min / p50 / max |
|---|---|---|---|---|---|
| A: all cold | 0 | 0 | 4.3 GB (dirty baseline) | – | – |
| B: 20 resident, capped 350 MiB | 20 | 6.0 GB (302 MiB each) | 4.2 GB | warm (10 of the 20) | 1.4 / 4.0 / 5.3 s |
| B | 20 | | 4.2 GB | cold, disk, prefetch | 13.1 / 25.8 / 27.4 s; available fell to 0.8 GB, daemon then reclaimed 12 of the 20 |
| C: same 20 recreated with the KSM opt-in wrapper, capped, KSM scanning | 20 | ~6.0 GB, KSM saved 0.22–0.35 GB | 4.6–4.8 GB | warm (10 of the 20) | 1.3 / 1.6 / 1.9 s |
| C | 20 | | 4.8 GB | cold, disk, prefetch | 9.8 / 19.7 / 21.6 s; available fell to 0.8 GB |
| D: lazy cold wake, no prefetch, cap 400 MiB on the ten | 8 (leftovers) | 6.0 GB | 2.5 GB | cold, disk, faults only | 21.7 / 22.7 / 23.4 s |

KSM detail: 20 idle gateways, 3+ full scans in 50 s at 20k pages per pass: pages shared 52–58 MB, pages sharing 217–353 MB, general profit 133–266 MB. About 11–18 MiB saved per cell, ~5 % of a capped cell. The identical parts of a gateway (binary, libraries) are file-backed and already shared through the page cache; the anonymous heap is private per process and does not merge. Zero failures, zero exits in every burst; KSM cost ~1 s of CPU stall per burst.

Findings:
- **The 300 MiB resident tier works as a tier**: 20 paused cells kept at ~300 MiB each (6 GB) and woken ten at a time in 1.3–1.9 s (C), with the caveat that the first warm burst (B) showed 4–5 s for some cells because the 350 MiB cap throttled them while they re-faulted; the cap has to be lifted at wake, which is the supervisor's job.
- **KSM is not a lever for OpenClaw cells**: ~5 % of a capped cell, at the price of running the container entrypoint as root with three capabilities so the gateway can opt in. Dropped.
- **A cold burst of ten needs ~7 GB of headroom, and no tier trick changes that**: with 4.2–4.8 GB available the burst took 20–27 s in both B and C (page-in plus eviction, memory stall 2.9–4.7 s), against 4–10.5 s with 8 GB. The resident tier and the cold-burst headroom compete for the same RAM.
- **The lazy cold wake is not viable**: without prefetch the ten cells took 21.7–23.4 s each, faulting their pages one at a time through OpenClaw's recovery, reproducing the 27 Sep result. Prefetch stays, and with it the ~700 MiB per cold wake.
- **Budget on a 16 GB host at 100 cells** (clean host, ~9 GB after OS and cold floors): either ~7 resident cells at 300 MiB plus 7 GB for a burst of ten cold cells, or ~20 resident cells and cold bursts that take 20+ s. Both tiers fit together only with more RAM (a 32 GB host holds 20 resident and the burst headroom) or a smaller burst target (a burst of four needs 3 GB).
- Method notes: the daemon pauses a fresh cell 45 s after boot, so any script waiting on /health after that point must also accept "paused"; and 20 first boots at once (14 GB) thrash a 16 GB host, so recreation must be paced like the density ladder. The recreated v1..v20 still run with the KSM wrapper and its capabilities; recreate them normally before any security check.

## Same table at 10 resident cells, 100 cells, bursts of ten, on a rebooted host (04:51–05:05 UTC)
Rerun of the previous section at the intended scope (10 resident, not 20), after a reboot so the headroom numbers are clean: all cold = 8.76 GB available (6.85 GB used), matching the 29 Sep baseline. Cells 1..20 first put back on the normal hardened launch. Daemon: timed reclaim 30 m, prefetch on, headroom target 2 GB while the resident tier existed (8 GB for state A).

| State | Resident cells | Resident RAM | Available before the burst | Burst of ten | min / p50 / max | Available after |
|---|---|---|---|---|---|---|
| A: all cold, clean host | 0 | 0 | **8.76 GB** | – | – | – |
| B: 10 resident, capped 350 MiB | 10 | 2.99 GB (299 MiB each) | 6.09 GB | warm (the 10, still capped) | 1.8 / 3.3 / 3.9 s | 5.7 GB |
| B | 10 | 2.95 GB | 6.13 GB | cold (v21..v30, disk, prefetch) | 15.1 / 20.3 / 22.8 s | 1.8 GB; the daemon then reclaimed 9 of the 10 resident cells to restore its 2 GB target |
| C: the 10 recreated with the KSM wrapper, capped, KSM scanning (plus 4 leftover resident cells from B's cold burst) | 10 (+4) | ~3 GB (+2.2 GB) | 4.13 GB | warm (the 10, capped, KSM) | 1.2 / 1.5 / 1.8 s | 3.9 GB |
| C | 10 (+4) | | 4.13 GB | cold (v31..v40, disk, prefetch) | 7.5 / 18.9 / 20.8 s | 1.7 GB |

KSM at 10 cells: pages sharing 48–124 MB, profit 5–80 MB, i.e. 5–12 MiB per cell. Zero wake failures. PSI during the cold bursts: cpu some 6.8–8.6 s, io full 6.1–8.9 s, memory full 0.3–1.0 s.

Findings, at the 10-resident scope:
- **Ten resident cells at 300 MiB cost 3 GB and leave 6.1 GB available on a clean host.** Their warm burst is 1.2–1.9 s when the cap is not fighting the wake (C) and 1.8–3.9 s when it is (B): the cap must be lifted at wake.
- **6.1 GB is still not enough for a cold burst of ten**: 15–23 s, against 4–10.5 s with 8.3 GB (29 Sep). The burst's ~7 GB of page-in plus the kernel's own needs puts the real requirement at the 8 GB rule, so on 16 GB with 100 cells the resident tier at 10 cells and a cold burst of ten do not coexist at page-in speed; at 7 or fewer resident cells they do.
- KSM: 5–12 MiB per cell, same conclusion as at 20. Dropped.
- Two operational facts from the run: a freshly booted cell is paused by the daemon after 45 s, so scripts must accept "paused" as booted; and a cell recreated within 125 s of its previous instance being killed refuses to start ("another Gateway owner lease is still active") until the lease expires — the supervisor's cold tier never does this, but provisioning tools must wait out the lease on recreate.

## Single scenario, published: 10 resident cells capped at 350 MiB with KSM, 100 cells, bursts of ten (06:03–06:17 UTC)
Requested as one scenario. Host had been rebooted 90 min earlier; baseline available 6.75 GB (some page cache and two resident leftovers from the previous run). v1..v10 recreated with the KSM opt-in wrapper after a 135 s lease wait, paced five at a time, capped at 350 MiB as each batch came up; KSM at 20k pages per pass with no sleep (114 full scans by the first state); daemon: timed reclaim 30 m, prefetch on, headroom target 2 GB so the ten stayed resident.

| State | Resident cells | Resident RAM | Available | Burst of ten | Outcome | min / p50 / max |
|---|---|---|---|---|---|---|
| Baseline, all cold | 2 leftovers | 0.27 GB | 6.75 GB | – | – | – |
| Scenario: 10 resident, capped 350 MiB, KSM on | 10 (+2) | ~3.0 GB | 5.89 GB | warm, the ten, still capped | **4 woke in 1.4–1.9 s; 6 failed** (health reset/EOF for the full 15 s wake timeout) | 1.4 / 15.1 / 15.6 s |
| After the warm burst | 6 | 1.97 GB (328 MiB each) | 4.81 GB | cold, v21..v30 from disk | 10 of 10 | 6.6 / 15.5 / 16.5 s |
| After the cold burst | 10 (the woken cold cells, 625 MiB each) | 6.25 GB | 2.28 GB | – | – | – |

KSM over the scenario: pages shared 11–21 MB, pages sharing 72–105 MB, profit 49–71 MB, i.e. 5–10 MiB per cell; ksmd used 8 min 50 s of CPU during the 14-minute run at the aggressive scan settings, and CPU stall during the warm burst was 12.0 s.

Findings:
- **The scenario as specified fails its own warm burst**: six of the ten capped cells could not answer /health within the 15 s pause-tier timeout (connection reset, then EOF) and were marked failed; the four that made it woke in 1.4–1.9 s. The same ten cells woke 10/10 in 1.2–1.8 s an hour earlier with a lightly loaded scanner (3 full scans) and in 1.8–3.9 s the day before without KSM. The difference here was CPU: a scanner pinned at full speed plus ten gateways throttled by `memory.high` while re-faulting their recovery working set. A wake must lift the cap first; a resident tier that keeps the cap through the wake is not safe under load.
- The cold burst behaved as the headroom rule predicts: 4.8 GB available → 6.6–16.5 s (page-in plus eviction), against 4–10.5 s with 8 GB.
- KSM contributes 5–10 MiB per cell here as in every run, and at these scan settings it costs a core. Dropped for good; the wrapper stays in the repo as a record.
- Scenario verdict for a provider: 10 resident cells at 300 MiB are affordable on 16 GB with 100 cells (3 GB), but only with the cap lifted at wake and without KSM; the cold burst of ten then runs at 15–23 s unless the resident count drops to ~7 or the host has 8 GB of headroom.

## Scenario: 5 resident cells capped at 350 MiB, 100 cells, cold bursts of ten, rebooted host (07:53–08:02 UTC)
No KSM. Cap lifted at wake for the warm burst (the policy under build), re-applied after. Daemon: timed reclaim 30 m, prefetch on, headroom target 6 GB so the five stay resident. Clean baseline: 8.70 GB available, 6.9 GB used, 68 GB in swap (~680 MiB per cold cell).

| State | Resident | Resident RAM | Available before | Burst | Outcome | min / p50 / max | PSI (mem full / cpu some / io full) | Available after |
|---|---|---|---|---|---|---|---|---|
| All cold | 0 | 0 | 8.70 GB | – | – | – | – | – |
| 5 resident, capped | 5 | 1.62 GB (324 MiB each) | 7.16 GB | warm: the five, cap lifted at wake | 5 of 5 | 1.4 / 2.0 / 2.5 s | 0.4 / 1.1 / 0.5 s | 6.6 GB |
| 5 resident | 5 | 1.45 GB (289 MiB each) | 7.36 GB | cold: ten from the swapfile | 10 of 10 | 8.5 / 16.2 / 19.1 s | **0** / 7.8 / 7.1 s | 1.1 GB (daemon then reclaimed 4 of the 5 to restore its target) |
| 1 resident | 1 | 0.66 GB | 6.29 GB | cold: ten more from the swapfile | 10 of 10 | 16.2 / 19.0 / 21.0 s | 0.1 / 8.1 / 8.9 s | 1.0 GB |

Findings:
- **Five resident cells cost 1.6 GB and leave 7.2 GB; their warm wakes are 1.4–2.5 s, five of five, with the cap lifted at wake.**
- **A burst of ten fully cold cells at 7.4 GB available is disk-bound, not memory-bound**: zero memory stall, 7 s of I/O stall, 8.5 s for the first pair and 19 s for the last. That is ~7 GB read at the volume's ~550 MB/s in sequenced pairs. The 29 Sep figure of 4–10.5 s was a burst with three of the ten already resident; this is the honest all-cold number on this cloud volume, and it matches the day-one sequenced result (7.5 / 11.9 / 14.6 s at 50 cells with ~650 MiB per cell).
- So the trade the user named is settled: at 5 % resident the host keeps burst headroom, and the remaining burst latency is bandwidth. Cutting it further means a faster disk (a dedicated NVMe host would read the same 7 GB in ~3 s), fewer bytes per cold wake, or a smaller burst.
- Method: the boot script also needed the "paused counts as booted" fix; recorded.

## Compressed cold store, fair trial under the supervisor's own sequencing: ZFS zstd and dm-vdo (08:31–10:52 UTC)
Premise (user): the earlier ZFS hangs came from a stress fill; under production sequencing (cells entering the store five at a time, reclaimed by the daemon under its headroom rule, low concurrency) a compressing block layer under the swapfile might hold, and would halve or better the bytes a cold burst reads. Protocol per store: 100 cells on the host, tgw + v1..v40 brought into the store by restarting them five at a time and letting the daemon reclaim under the 8 GB target before the next batch; three single cold wakes; the adopted 5-resident scenario with a warm burst and two cold bursts of ten.

| Store | Kernel | Fill (41 cells, paced) | Compression | Single cold wake | Warm-5 | Cold burst of ten | Outcome |
|---|---|---|---|---|---|---|---|
| ZFS zvol, zstd, loop-backed, sync=always, ARC 512 MiB | 6.8 | batch 1 took 4.5 min at load 22, batch 2 four min at load 31; the daemon's reclaim of one cell moved 78 MiB in 15 s (≈5 MB/s) | 2.8x | – | – | – | **host hang during batch 3** (third ZFS hang; this one under the paced pattern) |
| Swapfiles only, same script, for a same-kernel baseline | 7.0 (HWE) | 36 min (script gates), no stall | – | 2.3–2.9 s | not resident (target not met at 4.1 GB): 6.5–8.9 s | 10.6 / 14.7 / 23.8 s and 12.0 / 17.1 / 18.5 s, memory stall 8.9 s and 3.4 s | ran; host tighter on 7.0 (see below) |
| dm-vdo, LZ4, dedup off, loop-backed, LVM | 7.0 | 36 min, load < 1 throughout, memory stall 2.6 s total, **no hang during the fill** | 58 % saving ≈ 2.4x | **4.6–5.4 s** (advise 3.1–3.3 s vs 1.0–1.4 s on the swapfile: ~250 MB/s read path) | not resident: 11.6–15.1 s for five | **host hang during the cold burst of ten** (prefetches of v22..v30 in flight, 4.6 GB available) | rejected |

Findings:
- **ZFS is out even under production sequencing.** The write path under zstd is so slow (about 5 MB/s per reclaim, load 22–31 with tasks blocked in the kernel) that the daemon cannot free memory faster than five booting cells consume it, and the host locks up. Sequencing changed nothing about the mechanism.
- **dm-vdo survives the fill but fails on the two things that matter**: its read path is slower than the raw swapfile (single cold wake 4.6–5.4 s against 2.3–2.9 s, ~250 MB/s despite reading half the bytes), and it hung under the first cold burst of ten, the same signature as ZFS: a compressing layer in the swap path with memory under pressure.
- **Closed: no compressing layer under swap on this host class.** Two implementations, four hangs, and the one that survived the fill reads slower than the disk it was meant to accelerate. The way to read fewer bytes on a cold wake, if it ever exists, is inside the supervisor (knowing which pages to bring back), not under the swapfile.
- **Kernel 7.0 note** (HWE, briefly installed for dm-vdo, removed afterwards): the same 100 cold cells left 6.2 GB available instead of 8.7 on 6.8, with the sum of container memory at 11.9 GB and page tables at 2.0 GB, and the disk-only cold bursts were correspondingly worse. The reclaim floor behaves differently on 7.0 (more anonymous memory left resident per cell). Not investigated further; the host is back on 6.8 and the sizing numbers stand for 6.8. Kernel upgrades need a re-measure of the floors before a provider adopts them.

## Design 1: warm floor + hot-set prefetch, acceptance at 100 cells (11:29–11:50 UTC)
Build `94ecff4`: a paused cell is first reclaimed to a 300 MiB warm floor, the pages the kernel kept are recorded from `/proc/<pid>/pagemap` as the cell's hot set (one JSON file per cell), and only later (timed, or memory pressure, longest-paused first) is it taken to the 150 MiB cold floor. A cold wake then prefetches the hot set only. No `memory.high`, no cap to lift: the warm tier is the warm floor itself. Host on 6.8 after the day's reboots with only 3.9–6.4 GB available at all-cold (used 9.2–11.7 GB; the two clean mornings had 8.7 GB, see the kernel/boot variance note), which makes this a harder test than the morning's.

Warm stage on real cells: 630–727 MiB → 298–299 MiB, hot set 267–273 MiB across the cell's 6 processes, recorded in < 1 s.

| State | Warm cells | Available before | Burst | Outcome | min / p50 / max | Bytes advised per cell | PSI mem full / cpu / io |
|---|---|---|---|---|---|---|---|
| 5 warm at the 300 MiB floor, target 3 GB | 5 | 3.17 GB | warm: the five, no prefetch | 5 of 5 | **0.66 / 0.73 / 1.34 s** | 0 | 10 ms / 1.0 s / 36 ms |
| same, second run | 5 | 3.30 GB | warm: the five | 5 of 5 | 0.64 / 1.16 / 2.27 s | 0 | 9 ms / 0.5 s / 0.1 s |
| 5 warm | 5 | 3.17 GB | cold: v21..v30 from the swapfile, hot-set prefetch | 10 of 10 | **2.76 / 5.16 / 6.79 s** | ~270 MiB (was ~700) | 0.8 / 1.6 / 1.9 s |
| earlier the same hour, target 6 GB (warm cells taken cold by pressure) | 0 | 3.88 GB | cold: v21..v30, hot-set prefetch | 10 of 10 | 4.19 / 6.65 / 7.53 s | ~270 MiB | 1.3 / 2.5 / 1.5 s |
| same | 0 | 3.85 GB | cold: v31..v40, hot-set prefetch | 10 of 10 | 5.93 / 6.63 / 7.25 s | ~270 MiB | 0.9 / 2.8 / 1.5 s |
| For reference, this morning: full prefetch, 7.4 GB available | 5 | 7.36 GB | cold: ten | 10 of 10 | 8.5 / 16.2 / 19.1 s | ~680 MiB | 0 / 7.8 / 7.1 s |

Per cold wake in the daemon log: advise 0.14–0.41 s (was 1.0–1.4 s), landed 128–150 MiB of the ~270 advised (the rest was still resident or shared), pagein wait 0.5 s, ready 2.3–5.7 s under the burst.

Findings:
- **Cold burst of ten: 19 s → 6.8 s for the last cell, 16 → 5.2 s median, on half the headroom.** Reading the hot set instead of the whole cell cut bytes per wake by ~60 % and, with it, the disk term that no storage layer could touch. Zero failures.
- **The warm tier costs nothing extra and wakes in under a second**: five cells at 299 MiB each woke in 0.66–1.34 s with no prefetch and no cap, which also retires the `memory.high` idea and its lift-at-wake requirement.
- **Headroom per cold wake drops from ~0.75 GB to ~0.3 GB**: the sizing rule becomes RAM ≈ 1 GB + 50 MiB × cold + 300 MiB × warm + 0.3 GB × burst. On 16 GB at 100 cells that is ten warm cells *and* a burst of ten at page-in speed.
- The hot set is invalidated by a restart (pids change) and the wake falls back to a full prefetch; a cell that was never warmed (old registry) also gets the full prefetch. Both paths exercised today.
- Variance note: available memory at all-cold on this host has ranged 3.9–8.7 GB across boots this week; the differences come from what stays charged after restarts (page cache, page tables, swap cache). The hot-set results above were taken at the low end, so they are conservative.

### Design 1, real turn on the live-channel cell (12:02 UTC)
tgw: warm stage 998 → 299 MiB, hot set 258 MiB in 4 processes; cold floor 149 MiB with 802 MiB in swap. One signed owner message through the public ingress: prefetch `scope=hot` advised 258 MiB (146 landed), wake to healthy 4.7 s (detector fired, 1 s settle), Telegram saw one 200 in 5.0 s, the cell logged no settlement abort and no heartbeat notice, and sent exactly one outbound message, the answer. Resident after the turn 787 MiB: what the turn needed came back from swap on demand. Confirms a hot-set wake is a fully operational cell, not a partially loaded one.

Note on the two cold bursts of the acceptance run (2.8/5.2/6.8 s with five warm cells vs 4.2/6.7/7.5 s with none): warm cells play no part in cold wakes; the 0-warm bursts ran minutes after a mass reclaim of twenty cells while swap-out was still draining to the disk. Treat 3–7.5 s as the range.

## Post-thaw maintenance, the CPU wall, and the fix: minimum awake time (12:46–15:00 UTC)
After the day's reboot every cold burst of ten came in at 8–25 s for the last cell (nine runs), against the 6.8 s of the design-1 acceptance, with CPU saturated (idle 0 %, gateways at 1.3–3.6 cores each) and 1.5 GB swapped out during the burst. Page-cache warming of the image (950 MB) changed nothing and was evicted again by the daemon's own reclaim. A single isolated cold wake told the story: hot set landed in 0.6 s, then the gateway ran `database integrity verification`, `memory-core dreaming promotion`, `slow SQLite reclamation`, cron catch-up, faulted 94 MiB more from disk and grew to 822 MiB within 20 s. Its log at the next thaw said `host thaw channel restart deferred: gateway still has active work`, with admission reopening 30 s after the thaw instead of at once.

Cause: the 45 s network-idle timer paused cells in the middle of that post-thaw housekeeping (~0.2 core for 30–60 s, then ~5 % steady, some of it I/O-bound), so every later thaw resumed the interrupted work before serving. Test: ten cells given a 6-minute idle timer, woken, left to finish, paused, taken cold, burst: **1.8 / 3.8 / 6.5 s**, admission reopened immediately, no deferral.

Fixes shipped (`3fc093f`, `c8f36c6`): idle detection also counts CPU (a cell above 10 % of a core is not idle, `-idle-cpu-pct`), and a minimum awake time after any wake (`-min-awake`, default 3 m). Also `-max-recovering` (default 4) bounds cells between unpause and ready, and pressure reclaim now yields while wakes are in flight (`a0d116f`), after a burst that overlapped a pressure reclaim ran 10–15 s per wake.

| Build | Cold burst of ten (v21..v30, hot-set prefetch) | min / p50 / max | PSI mem / cpu / io | Warm survivors under the 4 GB rule (this boot: ~4.2 GB available) |
|---|---|---|---|---|
| CPU gate only, round 1 (debt from earlier pauses) | | 8.0 / 14.1 / 21.8 s | 0.5 / 5.0 / 9.8 s | 5, woke in 1.3–2.5 s |
| CPU gate only, round 2 | | 3.7 / 8.5 / 13.7 s | 0.4 / 2.3 / 3.3 s | 4, 1.1–1.2 s |
| + minimum awake 3 m, round 1 | | 2.9 / 6.3 / 9.7 s | 0.01 / 1.7 / 1.9 s | 4, 1.1–1.2 s |
| **+ minimum awake 3 m, round 2 (steady state)** | | **2.8 / 5.4 / 8.3 s** | 0.2 / 1.8 / 1.7 s | 4, 1.2–1.3 s |

Findings:
- **The steady-state cold burst of ten on this host is 2.8 / 5.4 / 8.3 s**, with the memory stall at zero and the CPU and I/O stalls under 2 s: what remains is the gateways' own recovery running four at a time on 8 shared vCPUs.
- **Never pause a gateway inside its post-thaw minute.** This is the operating rule the whole day's slow bursts came down to, and it costs only RAM: a woken cell stays resident for three minutes instead of 45 s. It also matches reality: a tenant who just woke their assistant is likely to send another message.
- Four warm survivors on this boot because available memory sat at 4.2 GB against the 4 GB target; the resident count follows the host's state as designed. A clean boot gives 6–9 GB and eight to ten.
- The resident-count question ("can we have ten warm?") therefore has a measured answer: yes on a clean 16 GB host with the 4 GB target, self-adjusting downward when the host is tighter, and a burst of ten cold wakes still lands under 10 s either way.

# 2026-10-01, fresh host (clawnap-fresh, CX43 fsn1, 2.28.47.38)

## Zero-to-fleet from the repository
`provision-hetzner.sh` + the current cloud-init, `deploy.sh`, the README quickstart command. First attempt failed: a colon in one cloud-init `echo` made YAML parse that runcmd entry as a mapping and cloud-init dropped the whole command list (no Docker, no swap, no marker). Fixed (`286affa`), server deleted and recreated. Second attempt: swapfile 80 GB, Docker 29.8.2, clawnap unit enabled; Caddy with a sslip.io certificate answered in seconds; the first cell (`alice`, created with the README command) was healthy 158 s after the command including the image pull, registered its own Telegram webhook (getWebhookInfo: url set, pending 0, no error), and was paused at the warm floor on schedule. The image pulled was OpenClaw **2026.9.7** (the reference host runs 2026.9.6); 2026.9.7 prints that the Telegram webhook listener settings have moved to `channels.telegram.legacyWebhook` and the old location is legacy forwarding; delivery still works.

A real message from the paired-nowhere owner through the public ingress woke `alice` (warm wake, 0.9 s hold, ready 4.6 s) and Telegram recorded the push as accepted. Whether the cell's pairing notice reached the chat is not visible in the cell log; to be confirmed by the user.

## 100 cells on the fresh host: the first attempt was a harness defect, not a result
99 more cells created with the README command, five at a time, with a fill override (`-min-awake -1s -idle-cpu-pct -1`) and a health loop that accepted "paused" as booted. Consequence: cells that were still booting after their config restart went network-idle, the daemon paused them mid-boot, the warm stage recorded a half-started process as a hot set (170 MiB, 2 procs), and the "cold" cells had never finished starting. The scenario then "woke" ten half-booted gateways at once: each resumed a full boot (`loading configuration… starting HTTP server…`), CPU saturated for a minute (cpu some 61–67 s), cold bursts 22–90 s, seven wake timeouts. Also: with all 100 "cold" the host used 12.3 GB, because these fresh ~340 MiB cells sit below the 150 MiB floor (v60: 97 MiB resident, 0 in swap) and the floor never engaged.

Lessons, both recorded as follow-ups: (1) a fill must boot every cell to /health under the shipped policy before pausing it, which the CPU gate and the minimum awake time do by themselves (the override removed both); (2) the cold floor should be a fraction of the cell for small cells rather than a fixed 150 MiB, or `-reclaim-keep-mib` should default lower (cold cells on the reference host measured 35–71 MiB after reclaim). Rerun in progress with every cell booted to health under the shipped policy first.

# 2026-10-02, fresh host, clean run

## 100 cells booted to health under the shipped policy, then the scenario
After the power cycle every container was Exited. Started ten at a time, each waited to /health (no shortcuts), paced on available memory, under the shipped policy (warm floor 300 MiB, cold floor 150, headroom 4 GB, min awake 3 m, CPU gate 10 %). Boot pass 65 min, 0 exited. All booted and paused: 7.7 GB available, 7.9 GB used, 76 GB in swap; a cold cell 31 MiB resident with 802 MiB in swap. So once cells have really booted, the floors engage and the host looks like the reference host; the 12 GB / no-swap state of 1 Oct was the half-booted fill.

| Fresh host, OpenClaw 2026.9.7 cells | Warm burst of five | Cold burst of ten, v21..v30 | Cold burst of ten, v31..v40 | CPU stall in the cold bursts |
|---|---|---|---|---|
| Round 1 (first wake after first pause) | 2.7 / 3.9 / 5.2 s | 7.1 / 13.6 / 24.3 s | 7.9 / 14.2 / 25.9 s | 20 s |
| Round 2 | 3.8 / 6.8 / 7.8 s | 4.8 / 10.9 / 14.9 s | 6.4 / 11.0 / 13.9 s | 3.3–4.6 s |
| Reference host, 2026.9.6, steady state (1 Oct) | 1.1–1.3 s | 2.8 / 5.4 / 8.3 s | – | 1.7–1.8 s |

Zero wake failures in every round. The first-wake debt (post-thaw housekeeping) is visible again: round two halved the cold burst and cut the CPU stall from 20 s to under 5. Round two still runs about twice the reference, and the cell log shows admission reopening 25–30 s after a thaw on these cells where the reference cells reopen within a second.

## A/B on the same host: OpenClaw 2026.9.6 vs 2026.9.7
One fresh cell per version, same policy, hibernated through the API between wakes (warm), then a timed cold reclaim.

| | 2026.9.6 | 2026.9.7 |
|---|---|---|
| Warm wake 1 / 2 / 3 | 2.0 / 1.0 / 0.95 s | 4.2 / 2.6 / 1.6 s |
| Cold wake, hot-set prefetch | 2.9 s (365 MiB advised, 253 landed) | 3.5 s (377 MiB advised, 266 landed) |

Findings:
- **The reference numbers reproduce on a fresh host with the reference version**: a 2026.9.6 cell wakes warm in about a second and cold in under three, from zero, with the published cloud-init, binary and quickstart.
- **2026.9.7 is slower after a thaw and converges with use**: 4.2 → 2.6 → 1.6 s over three warm wakes, 0.6 s more on a cold wake. Most of the fresh host's burst gap to the reference is this version's heavier post-thaw path plus the first-wake debt of cells that have been woken only once or twice; a third round is running to show where it settles.
- Release consequence: the README states the measured version (2026.9.6) and that 2026.9.7 adds roughly half a second to a second per wake in this A/B; the upstream follow-up gains a concrete regression note (admission reopening 25–30 s after a thaw on 2026.9.7).
- Harness lessons kept: a fill must boot every cell to health under the shipped policy; "paused" is not "booted".

### Round three on the 2026.9.7 cells (05:47–05:57 UTC)
| | Cold burst v21..v30 | Cold burst v31..v40 | Warm burst of five | CPU stall (cold) |
|---|---|---|---|---|
| Round 3 | 3.4 / 9.7 / 16.4 s | 3.9 / 9.7 / 18.0 s | 4.2 / 5.8 / 7.7 s | 11.7–12.5 s |

It does not converge to the reference. The first cell of a burst is now fast (3.4–3.9 s, the hot-set prefetch doing its job) but the tail stays at 16–18 s with the CPU saturated for 12 s by ten gateways' post-thaw work, and warm wakes stay at 4–8 s. Combined with the A/B, the conclusion is a version effect: **on OpenClaw 2026.9.7 the post-thaw path costs several seconds of CPU per cell and reopens admission 25–30 s after a thaw**, where 2026.9.6 reopens within a second and the same host and policy give 1 s warm and 3–8 s cold bursts. Zero wake failures in every round. For the release: measured version pinned in the README; the 9.7 regression goes into the upstream follow-up with these numbers. For the supervisor: nothing to change; the cost is inside the gateway after the thaw, which is exactly the ready-signal and deferred-housekeeping ask already on the thread.
