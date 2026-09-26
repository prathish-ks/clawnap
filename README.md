# fleet-supervisor

A small Go host-side supervisor for fleets of OpenClaw cells (and later
NanoClaw/Isthmus hosts): hibernate idle cells, wake them on inbound message
through a shared ingress, keep always-on cells alive, and refuse unsafe
cell specs. Stock OpenClaw images run unchanged.

**Status:** Phase 0a (local correctness) essentially complete: pause and stop hibernation tiers, HTTP readiness, ingress wake-then-proxy, cron-aware wake, WAL checkpoint and the host checker are unit-tested and have been run against real OpenClaw cells with a real Telegram bot on Docker Desktop. Density numbers await a Linux host. See `docs/plan.md` for the plan and
`experiments/` for the week-1 measurement protocol.

## What exists

| Package | Role |
|---|---|
| `internal/runtime` | Docker/Podman CLI seam: inspect, stop, start, stats, list |
| `internal/registry` | Durable cell records (atomic JSON file) |
| `internal/idle` | Idle decision from network counters, with noise floor |
| `internal/supervisor` | Reconcile loop: hibernate idle cells, self-heal always-on cells, bounded coalesced wakes with readiness probe |
| `internal/ingress` | HTTP front door: `POST /wake/{cell}` and `/hook/{cell}/...` wake-then-proxy |
| `internal/spec` | Guest-neutral container spec and refusal rules (non-root, pids limit, memory ceiling, no Docker socket or dangerous mounts, no secrets in env) |
| `internal/hostcheck` | `fleetd check`: read-only host and cell security inspection (runtime class, metadata-egress block, privileged/root/caps/limits, socket and dangerous mounts, secrets in env, public ports). Ported from Isthmus. |
| `internal/walcheck` | Host-side SQLite WAL truncation after a stop-tier hibernate |
| `cmd/fleetd` | CLI and daemon |

## Quick try (needs Docker)

```bash
go build ./cmd/fleetd
./fleetd cells add -name a -container openclaw-a -port 18801 -idle 5m
./fleetd reconcile            # one pass
./fleetd check                # inspect every container labelled fleet.cell
./fleetd serve -listen 127.0.0.1:8080 -token dev
curl -X POST -H 'Authorization: Bearer dev' http://127.0.0.1:8080/wake/a
```

## Design rules

- The supervisor never modifies the guest; it only owns container lifecycle, ingress, limits and validation.
- One process on the host issues runtime commands for managed cells.
- Every non-goal of upstream OpenClaw's fleet feature (lightweight tenancy, multi-host, metering) is in scope here; everything upstream owns (the gateway itself) is not.
- Platform gaps are reported, never silently no-op'd.

License: Apache-2.0 (intended; LICENSE file to be added before first release).
