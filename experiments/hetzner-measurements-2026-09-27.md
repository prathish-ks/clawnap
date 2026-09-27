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
