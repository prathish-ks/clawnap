<p align="center"><img src="assets/logo.svg" alt="clawnap" width="128"></p>

# clawnap

A small Go daemon that lets a host run many more stock [OpenClaw](https://github.com/openclaw/openclaw) cells than it has RAM for, by hibernating the idle ones and waking them on their first inbound message. Nothing changes inside the cells: same image, same config, same bot token, which the host never holds.

Measured on one Hetzner CX43 (8 shared vCPU, 16 GB, throttled NVMe), OpenClaw **2026.9.6**, a real Telegram bot. Reproduced from zero on a second host from this repository alone (cloud-init, binary, the quickstart below). Pin the image tag you measure: OpenClaw **2026.9.7** (the `latest` tag) does more work after a thaw, and the same host measured it slower, see the second table.

| | Stock | With clawnap (2026.9.6) |
|---|---|---|
| Cells per 16 GB host | ~35 with zram; at 40, two died and three failed health | **100, all healthy, 8–9 GB RAM still free on a fresh fleet; ~4 GB after a week of use (see below)** |
| RAM per idle cell | ~750 MiB | ~65 MiB cold (150 MiB floor, the rest is cache), 300 MiB warm |
| Wake of a recently active cell | always on | ~1 s (0.7–1.3 s) |
| Wake of a cold cell on its first message | always on | ~3 s, answer delivered on the platform's first push |
| Ten cold cells waking at once | – | last one ready to serve in 3–8 s (readiness, steady state: 2.8 / 5.4 / 8.3 s first / median / last; a reply adds the model round trip, measured 5 s end to end for a cold cell without a model call) |
| User-visible artefacts of the freeze | – | none in 15 real turns: the host covers OpenClaw's post-thaw window from the cell's own log |

The same host with 2026.9.7 cells, 100 cells, measured 2026-10-02:

| 2026.9.7 | Measured |
|---|---|
| Warm wake | 1.25–1.8 s single; 4.2 / 2.6 / 1.6 s on the first three wakes after a cold one (it converges with use) |
| Cold wake, single | 2.0–2.3 s on a cell woken before; 3.5 s on its first |
| Ten cold cells at once, nothing else active | 5.0 / 9.9 / 11.5 s |
| Ten cold cells at once with 5 uncapped residents active (host over-committed, see Tuning) | 7.4 / 8.3 / 11.3 s |
| Ten cold cells at once, first wake after a fleet boot | 17–28 s: each cell's post-thaw housekeeping is CPU-bound and runs once |

Measured over 15 hours at these settings with 10 % of cells taking traffic: the
fleet holds 1–3 cells awake, free memory sits near 4 GB, and a message to a
sleeping cell is answered in 2.7 s at the median (3.5 s at p90). The 8–9 GB
figure above was taken on a freshly created fleet; this one had been in use for
a week, and its cold cells hold ~98 MiB each rather than ~65. Which of the two
a host sees over months is not yet settled, so size for the lower one.

Every number, including the ones that did not work (zram, zswap, ZFS, dm-vdo, KSM, memory caps), is in [experiments/hetzner-measurements-2026-09-27.md](experiments/hetzner-measurements-2026-09-27.md). The plan and decisions are in [docs/plan.md](docs/plan.md).

## How it works

A cell is a stock OpenClaw container with a label. clawnap watches its traffic and CPU; when it has been idle long enough it is **paused** (sub-second to resume) and taken through three memory tiers:

| Tier | What is in RAM | Wake |
|---|---|---|
| Warm | the cell's working set, ~300 MiB, trimmed with cgroup `memory.reclaim`; the pages the kernel kept are recorded as the cell's hot set | unpause, ~1 s |
| Cold | ~65 MiB used (a 150 MiB floor, mostly cache); everything else on the host's swapfile | prefetch the hot set (~270 MiB on 2026.9.6, 330–420 MiB on 2026.9.7) with `process_madvise`, unpause, 2–3.5 s |
| Active | unlimited (an idle running gateway holds ~0.8 GB) | running |

Cells go warm as soon as they pause and cold only when the host's available memory falls below a target, longest-paused first, or after a timed limit; the target is kept under continuous traffic (measured: the pressure pass yields to wakes in flight for at most 30 s). Only idle cells can be reclaimed; what active cells hold is theirs. A woken cell stays awake at least three minutes so OpenClaw can finish its post-thaw housekeeping.

**Scheduled jobs** wake a sleeping cell too. Register the cell's next due time (`clawnap cells add ... -next-due <RFC3339> -next-due-every 1h`): the cell is woken two minutes before it, is not put to sleep when a job is due within its idle window, and a recurring schedule advances only after a successful wake, so a missed run is not skipped. With `-read-schedules` the host reads those due times out of the cell's own job store instead, so nothing has to be registered by hand. It wakes only for work a person is waiting on, one-shots and jobs that deliver somewhere, and leaves the cell's internal maintenance to catch up on its next wake. It reads scheduling fields only: the prompt text, the job's name and its description never leave the cell. A cell nobody ever messages still has internal work of its own (memory consolidation, a weekly review, its heartbeat), and `-maintain-every` wakes each cell at least that often so it runs: see [docs/design-scheduled-wakes.md](docs/design-scheduled-wakes.md) for what the host can see of a cell's schedule and what it should wake for.

The **ingress** is a small HTTP front door. Each cell's webhook URL points at `/hook/<cell>/...`; clawnap verifies the platform's signature (Telegram header secret, Slack and GitHub/Meta HMAC, bearer) with a verify-only secret, wakes the cell, waits until its gateway answers, and proxies the request. Bot tokens stay inside the cell. Unsigned requests are refused without a wake.

## What a sleeping cell gives up: standard and premium

OpenClaw's **heartbeat** is how an agent speaks first. Every 30 minutes by
default (1 hour under Anthropic OAuth or token auth) the gateway runs an agent
turn in the cell's main session so the model can raise anything that needs
attention. A sleeping cell cannot do that, so hibernation changes this one
behaviour and a host should say so rather than let a tenant discover it.

It changes less than it sounds. The stock heartbeat is deliberately quiet: its
prompt tells the agent to answer with a no-reply token when nothing needs
attention, and without a resolvable owner DM the poll skips entirely. Proactive
behaviour is opt-in. And the gateway coalesces what it missed, measured on this
host: a cell that had been down about three days ran **one** heartbeat turn on
return, not the ~144 it had missed.

So the honest offer is two tiers, and both already ship:

| | Standard (`-class hibernate`, the default) | Premium (`-class always-on`) |
|---|---|---|
| Wakes on a message | yes, ~1–3 s | always up |
| Timed jobs aimed at a person | woken before they are due | on time |
| Internal maintenance (memory consolidation, weekly review) | runs on the next wake; `-maintain-every` guarantees one | continuous |
| Proactive check-in | once per maintenance wake | native cadence, every 30 min |
| Cost to the host | ~65 MiB asleep | ~0.8 GB, permanently resident |

With `-maintain-every 12h` a standard cell is woken twice a day, so its
proactive check-in happens roughly twice a day instead of 48 times. That is the
trade: a tenant who wants an agent that pipes up on its own belongs on premium.

Sizing the mix on a 16 GB host at 100 cells, arithmetic from the measured
per-cell figures rather than a measured scenario: each premium cell costs
~0.8 GB against the ~4.5 GB left once the OS, the cold floors and a burst
headroom of ten wakes are covered, so roughly **six premium cells alongside 94
standard**. Beyond that, add RAM.

On the cadence: `-maintain-every 12h` holds on a 100-cell host, measured. A
maintenance wake costs 4–6 minutes (a boot, the cell's post-thaw work, then it
sleeps at the first quiet moment rather than waiting out an idle timeout meant
for an absent tenant), which is under the ~7 minute pace, so wakes land at the
configured interval and the host never holds more than one maintenance cell
awake, about 0.8 GB. If a fleet falls far behind, `-maintain-concurrent` caps
the cost and the interval stretches instead of cells piling up. A tenant can
shape their own side with
`heartbeat.activeHours`, `heartbeat.every` and `cron.skipMissedJobs`; those are
theirs to set, not the host's to change.

## Quickstart on a Linux host (Docker, cgroup v2)

The reference setup is Ubuntu 24.04 with Docker, a swapfile, and clawnap as a systemd service. [experiments/provision/cloud-init.yaml](experiments/provision/cloud-init.yaml) does all of it for a Hetzner cloud server; by hand it is:

```bash
# 1. a swapfile for the cold tier: 1 GB per cell you plan to hold, and do not undersize it (see below; no zram)
fallocate -l 100G /swap.img && chmod 600 /swap.img && mkswap /swap.img
echo "/swap.img none swap sw,pri=100 0 0" >> /etc/fstab && swapon -a

# 2. the daemon
install -m 0755 clawnap-linux-amd64 /usr/local/bin/clawnap
cp experiments/provision/clawnap.service /etc/systemd/system/
echo "CLAWNAP_TOKEN=$(openssl rand -hex 16)" > /etc/clawnap.env && chmod 600 /etc/clawnap.env
systemctl daemon-reload && systemctl enable --now clawnap

# 3. a cell with a Telegram bot, reached through a public HTTPS ingress (Caddy in front of 127.0.0.1:8080 works; a free hostname such as sslip.io is enough)
echo "<bot token from BotFather>" > /root/bot.token && chmod 600 /root/bot.token
clawnap cells create -name alice -port 22001 -hook-port 22901 -image ghcr.io/openclaw/openclaw:2026.9.6 \
  -ingress-url https://<your-host> -telegram-token-file /root/bot.token -idle 10m
```

The cell starts, registers its webhook with Telegram itself, pairs its owner the normal OpenClaw way, and from then on is paused when idle and woken by the next message. Watch it:

```bash
clawnap cells list                       # phases, tiers, last activity
curl -H "Authorization: Bearer $CLAWNAP_TOKEN" http://127.0.0.1:8080/metrics   # Prometheus text
clawnap check                            # read-only security inspection of host and cells
```

## Tuning

Four numbers describe a host. The daemon flags that set them, and the rule that ties them together (every term measured on the reference host):

```
RAM  ≈ 1 GB (OS) + 65 MiB × cold cells + 300 MiB × warm cells
       + 0.8 GB × active cells (uncapped residents: tenants in a conversation)
       + 0.8 GB × cold wakes you want at full speed at once (a woken cell holds ~0.8 GB through its post-thaw window; the page-in itself is ~0.3 GB)

swap ≈ 1 GB × cells
```

**Do not undersize the swapfile.** A cold cell's pages live there, and the
headroom policy frees RAM by moving more of them there, so a full swapfile does
not fail loudly: it quietly removes the host's ability to reclaim at all.
Measured on a 16 GB host after booting 100 cells with the policy running, usage
reached 784 MiB per cell, so an 80 GB file was 98.7 % full. Memory pressure read
near zero and nothing was OOM-killed, while cold wakes ran 4.6–7.7 s instead of
the usual 2–3.5 s and the headroom target could no longer be met. 1 GB per cell
leaves room for the steady state plus the reclaim a burst triggers. It buys
headroom, not speed: page-in is bound by the device, so a larger file does not
make a wake faster, it stops the tier from seizing up.

Disk is then the next constraint, and it is the cheap one. Measured on the
reference host, the OS, images and 100 cell state directories come to about
29 GB, so budget `disk ≈ 30 GB + 1 GB × cells`: 130 GB for 100 cells, which
fits that host's 150 GB with ~20 GB spare. Prefer paying for disk over RAM, it
is the resource this design trades into.

Active cells are the term people forget. clawnap can only make room from idle cells; a resident gateway in use holds its ~0.8 GB until it goes idle, and ten of them on 16 GB leave nothing for a burst. The measured over-commit: 95 cold + 5 uncapped residents + a burst of ten is ~20 GB on paper, and the burst ran memory-bound (7.4 / 8.3 / 11.3 s on 2026.9.7 against 5.0 / 9.9 / 11.5 s with no residents). Hosts that expect many simultaneous conversations need the RAM for them, or a memory cap on resident cells (an idle gateway runs at ~290 MiB under a 350 MiB cap; lifting the cap at wake is on the backlog).

| Flag (service default) | What it sets |
|---|---|
| `-warm-keep-mib 300` | the warm floor: a paused cell's resident set and the size of its hot set |
| `-reclaim-keep-mib 150` | the cold floor (memory.current; ~65 MiB of it is not cache) |
| `-headroom-mib 4096` (or `-burst-target N` = 0.8 GB × N) | available memory the host keeps; below it, the longest-paused warm cells go cold. Size it for the burst of cold wakes you want to absorb. Active cells count against it and cannot be reclaimed |
| `-reclaim-after 30m` | timed limit after which a warm cell goes cold anyway |
| `-wake-concurrency 4` | page-ins in flight (a hot-set wake reads ~270 MiB on 2026.9.6, 330–420 MiB on 2026.9.7; four in flight fill a cloud volume's ~0.5 GB/s) |
| `-max-recovering 8` | cells between unpause and ready at once (CPU-bound: OpenClaw's recovery; tuned for 8 vCPUs) |
| `-min-awake 3m`, `-idle-cpu-pct 10` | a woken cell is not paused again inside its post-thaw housekeeping |
| `-thaw-settle 1s` | hold on the first forwarded message after a thaw, keyed on the cell's own log |
| `-maintain-every 0` (off) | maintenance rotation: wake the longest-unwoken hibernated cell on a pace derived from the fleet, so a cell nobody messages still runs its own internal schedule. Stands aside for real wakes and for the headroom policy |
| `-maintain-concurrent 1` | cells the rotation may hold awake at once. A maintenance wake lasts until the cell's catch-up finishes and its idle timeout elapses, so this, not the pace, is what bounds its cost (~0.8 GB per cell held awake) |
| `-maintain-idle 30s` | idle timeout for a cell the rotation woke, instead of its own: a maintenance wake has no tenant to wait for, so the cell sleeps as soon as it goes quiet. `-min-awake` and the CPU gate still apply; a message through the ingress reverts the cell to its own timeout, as does the cell staying awake longer than that timeout. Measured: this cut a maintenance wake from 25 min to 4–6 min |
| `-read-schedules` (off) | take each cell's next due time from its own job store when it hibernates, rather than from `-next-due`. Reads scheduling metadata only; an upstream schema change falls back to the operator-set value |

Worked examples, 100 cells:

| Host | Active (uncapped) | Warm cells | Cold wakes at once | Fits |
|---|---|---|---|---|
| 16 GB | 0–2 | 5–10 (self-adjusting) | 10 in 3–8 s (2026.9.6) | yes, measured |
| 16 GB | 5 | ≤5 | 10 in 7–11 s (2026.9.7), memory-bound | over-committed, measured |
| 32 GB | 5 | 20 | 10 | yes, by the rule |
| 64 GB, local NVMe | 10 | 50 | 20, faster | yes, by the rule; unmeasured |

## What clawnap never does

- Hold a channel credential or read a message. The ingress verifies signatures with a verify-only secret and proxies bytes.
- Change anything inside a cell: config, model, heartbeat, channels are the tenant's.
- Put a compressing or deduplicating layer under swap, or use zram as the store. All were measured; all lost to a plain swapfile plus knowing which pages to bring back.

## Limits

- Linux with cgroup v2 and Docker (Podman layouts are recognised; less tested). Pause tier needs the container runtime's freezer.
- Channels that hold a long-lived socket (WhatsApp Web, Discord gateway) cannot be woken by webhook; run those cells in the `always-on` class.
- One host. Placement across hosts, migration and metering are the next phase.
- No admission control: clawnap frees memory held by idle cells, never by active ones. A host with more simultaneous conversations than its RAM allows degrades to kernel swapping like a stock host would.
- The daemon shells out to the container CLI: two calls per pass plus one `docker stats` per running cell (measured 0.02–0.05 cores at 100 cells on a 5 s interval).
- OpenClaw exposes no readiness signal after a thaw; clawnap covers the window with a bounded hold read from the cell's log. The upstream ask is in [openclaw/openclaw#114145](https://github.com/openclaw/openclaw/issues/114145).

## Development

```bash
go test -race ./...
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -o clawnap-linux-amd64 ./cmd/clawnap
```

Packages: `runtime` (Docker/Podman CLI seam), `registry` (durable cell records), `idle`, `supervisor` (tiers, wakes, pressure), `reclaim` (cgroup reclaim, hot sets, prefetch), `ingress` (verify, wake, proxy), `provision` (cell creation), `spec` and `hostcheck` (refusal rules and `clawnap check`), `walcheck`.

## Licence

Copyright 2026 Prathish KS. Apache-2.0: see [LICENSE](LICENSE) and [NOTICE](NOTICE).
