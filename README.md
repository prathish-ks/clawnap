# fleet-supervisor

A small Go host-side supervisor for fleets of OpenClaw cells (and later
NanoClaw/Isthmus hosts): hibernate idle cells, wake them on inbound message
through a shared ingress, keep always-on cells alive, and refuse unsafe
cell specs. Stock OpenClaw images run unchanged.

**Status:** day-one scaffold. Unit-tested against a fake container runtime;
not yet run against a real fleet. See `docs/plan.md` for the plan and
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
| `cmd/fleetd` | CLI and daemon |

## Quick try (needs Docker)

```bash
go build ./cmd/fleetd
./fleetd cells add -name a -container openclaw-a -port 18801 -idle 5m
./fleetd reconcile            # one pass
./fleetd serve -listen 127.0.0.1:8080 -token dev
curl -X POST -H 'Authorization: Bearer dev' http://127.0.0.1:8080/wake/a
```

## Design rules

- The supervisor never modifies the guest; it only owns container lifecycle, ingress, limits and validation.
- One process on the host issues runtime commands for managed cells.
- Every non-goal of upstream OpenClaw's fleet feature (lightweight tenancy, multi-host, metering) is in scope here; everything upstream owns (the gateway itself) is not.
- Platform gaps are reported, never silently no-op'd.

License: Apache-2.0 (intended; LICENSE file to be added before first release).
