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
