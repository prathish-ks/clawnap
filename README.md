# clawnap

A small Go daemon that lets a host run many more stock [OpenClaw](https://github.com/openclaw/openclaw) cells than it has RAM for, by hibernating the idle ones and waking them on their first inbound message. Nothing changes inside the cells: same image, same config, same bot token, which the host never holds.

Measured on one Hetzner CX43 (8 shared vCPU, 16 GB, throttled NVMe), OpenClaw **2026.9.6**, a real Telegram bot. Reproduced from zero on a second host from this repository alone (cloud-init, binary, the quickstart below). OpenClaw 2026.9.7 was about half a second to a second slower per wake in an A/B on the same host; pin the image tag you measure.

| | Stock | With clawnap |
|---|---|---|
| Cells per 16 GB host | ~35 with zram, cells dying at 40 | **100, all healthy, 9 GB RAM still free** |
| RAM per idle cell | ~750 MiB | ~50 MiB cold, 300 MiB warm |
| Wake of a recently active cell | always on | ~1 s |
| Wake of a cold cell on its first message | always on | ~3 s, answer delivered on the platform's first push |
| Ten cold cells waking at once | – | last one ready to serve in 3–8 s (readiness; a reply adds admission and the model round trip, measured 5 s end to end for a cold cell without a model call) |
| User-visible artefacts of the freeze | – | none: the host covers OpenClaw's post-thaw window from the cell's own log |

Every number, including the ones that did not work (zram, zswap, ZFS, dm-vdo, KSM, memory caps), is in [experiments/hetzner-measurements-2026-09-27.md](experiments/hetzner-measurements-2026-09-27.md). The plan and decisions are in [docs/plan.md](docs/plan.md).

## How it works

A cell is a stock OpenClaw container with a label. clawnap watches its traffic and CPU; when it has been idle long enough it is **paused** (sub-second to resume) and taken through three memory tiers:

| Tier | What is in RAM | Wake |
|---|---|---|
| Warm | the cell's working set, ~300 MiB, trimmed with cgroup `memory.reclaim`; the pages the kernel kept are recorded as the cell's hot set | unpause, ~1 s |
| Cold | ~50 MiB; everything else on the host's swapfile | prefetch the hot set (~270 MiB) with `process_madvise`, unpause, ~3 s |
| Active | unlimited | running |

Cells go warm as soon as they pause and cold only when the host's available memory falls below a target, longest-paused first, or after a timed limit. A woken cell stays awake at least three minutes so OpenClaw can finish its post-thaw housekeeping.

The **ingress** is a small HTTP front door. Each cell's webhook URL points at `/hook/<cell>/...`; clawnap verifies the platform's signature (Telegram header secret, Slack and GitHub/Meta HMAC, bearer) with a verify-only secret, wakes the cell, waits until its gateway answers, and proxies the request. Bot tokens stay inside the cell. Unsigned requests are refused without a wake.

## Quickstart on a Linux host (Docker, cgroup v2)

The reference setup is Ubuntu 24.04 with Docker, a swapfile, and clawnap as a systemd service. [experiments/provision/cloud-init.yaml](experiments/provision/cloud-init.yaml) does all of it for a Hetzner cloud server; by hand it is:

```bash
# 1. a swapfile for the cold tier, ~0.8 GB per cell you plan to hold (no zram)
fallocate -l 80G /swap.img && chmod 600 /swap.img && mkswap /swap.img
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

Three numbers describe a host. The daemon flags that set them, and the rule that ties them together (every term measured on the reference host):

```
RAM ≈ 1 GB (OS) + 50 MiB × cold cells + 300 MiB × warm cells + 0.8 GB × cold wakes you want at full speed at once
```

| Flag (service default) | What it sets |
|---|---|
| `-warm-keep-mib 300` | the warm floor: a paused cell's resident set and the size of its hot set |
| `-reclaim-keep-mib 150` | the cold floor |
| `-headroom-mib 4096` | available memory the host keeps; below it, the longest-paused warm cells go cold. Size it for the burst of cold wakes you want to absorb |
| `-reclaim-after 30m` | timed limit after which a warm cell goes cold anyway |
| `-wake-concurrency 4` | page-ins in flight (a hot-set wake reads ~270 MiB) |
| `-max-recovering 8` | cells between unpause and ready at once (CPU-bound: OpenClaw's recovery; tuned for 8 vCPUs) |
| `-min-awake 3m`, `-idle-cpu-pct 10` | a woken cell is not paused again inside its post-thaw housekeeping |
| `-thaw-settle 1s` | hold on the first forwarded message after a thaw, keyed on the cell's own log |

Worked examples, 100 cells:

| Host | Warm cells | Cold wakes at once | Fits |
|---|---|---|---|
| 16 GB | 5–10 (self-adjusting) | 10 in 3–8 s | yes, measured |
| 32 GB | 20 | 10 | yes, by the rule |
| 64 GB, local NVMe | 50 | 20, faster | yes, by the rule; unmeasured |

## What clawnap never does

- Hold a channel credential or read a message. The ingress verifies signatures with a verify-only secret and proxies bytes.
- Change anything inside a cell: config, model, heartbeat, channels are the tenant's.
- Put a compressing or deduplicating layer under swap, or use zram as the store. All were measured; all lost to a plain swapfile plus knowing which pages to bring back.

## Limits

- Linux with cgroup v2 and Docker (Podman layouts are recognised; less tested). Pause tier needs the container runtime's freezer.
- Channels that hold a long-lived socket (WhatsApp Web, Discord gateway) cannot be woken by webhook; run those cells in the `always-on` class.
- One host. Placement across hosts, migration and metering are the next phase.
- OpenClaw exposes no readiness signal after a thaw; clawnap covers the window with a bounded hold read from the cell's log. The upstream ask is in [openclaw/openclaw#114145](https://github.com/openclaw/openclaw/issues/114145).

## Development

```bash
go test -race ./...
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -o clawnap-linux-amd64 ./cmd/clawnap
```

Packages: `runtime` (Docker/Podman CLI seam), `registry` (durable cell records), `idle`, `supervisor` (tiers, wakes, pressure), `reclaim` (cgroup reclaim, hot sets, prefetch), `ingress` (verify, wake, proxy), `provision` (cell creation), `spec` and `hostcheck` (refusal rules and `clawnap check`), `walcheck`.

## Licence

Apache-2.0. See [LICENSE](LICENSE).
