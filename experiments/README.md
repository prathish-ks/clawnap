# Week-1/2 density experiment protocol

Goal: measure how many stock OpenClaw cells one host can carry, with and
without the supervisor, and what wake-on-message costs. Numbers from this
protocol are the product's first public benchmark.

Host: Hetzner CX43 (8 vCPU / 16 GB Intel shared, ~EUR 16/mo, EU only; fallback CAX31 ARM ~EUR 21/mo),
Ubuntu 24.04 bootstrapped by `provision/cloud-init.yaml` (Docker, Go, node_exporter, cAdvisor, zram, CRIU).
Provision: `provision/provision-hetzner.sh`; deploy: `provision/deploy.sh ops@IP`.

## Steps

1. `./make-cells.sh 10` — create N stock OpenClaw fleet cells (one Telegram test bot each; tokens in `bots.txt`, one per line).
2. `./collect.sh 30 > baseline-10.csv` — sample RSS/CPU/NetIO every 30 s for the whole run.
3. Repeat at 25 and 50 cells. Record the cell count at which the host swaps or OOM-kills.
4. Cold start: `docker stop $(docker ps -q --filter label=fleet.cell)` then start all; time to all ports ready.
5. WAL growth: send 200 messages per cell via the bot API; `du -sh` each state dir before/after.
6. Register the cells with the supervisor (`fleetd cells add ...` per cell), run `fleetd serve`, point each bot's webhook at `/hook/<cell>/...`, and repeat steps 2–4 with hibernation on.
7. Wake latency: `./wake-latency.sh <cell> 100` — 100 wake cycles, prints p50/p95 of ready time.
8. Density stack (week 2): repeat step 6 with (a) memory overcommit only, (b) + zram, (c) + KSM, (d) + hibernation. Each is one CSV.

## Success criteria (Gate A)

- ≥3x baseline cells per host with hibernation, p95 wake < 5 s, zero state loss across 20 hibernate/wake cycles (sessions, memory files, cron, channel auth).
