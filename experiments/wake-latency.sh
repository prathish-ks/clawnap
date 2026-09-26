#!/usr/bin/env bash
# Hibernate/wake a cell N times through the supervisor and print p50/p95 ready latency.
set -euo pipefail
CELL=${1:?cell}; N=${2:-20}; INGRESS=${INGRESS:-http://127.0.0.1:8080}; TOKEN=${FLEETD_TOKEN:-}
out=$(mktemp)
for i in $(seq 1 "$N"); do
  fleetd hibernate -name "$CELL" >/dev/null
  sleep 2
  ms=$(curl -s -o /dev/null -w '%header{X-Wake-Ready-Ms}' -X POST ${TOKEN:+-H "Authorization: Bearer $TOKEN"} "$INGRESS/wake/$CELL")
  echo "$ms" >> "$out"
done
sort -n "$out" | awk '{a[NR]=$1} END{print "n="NR" p50="a[int(NR*0.5+0.5)]"ms p95="a[int(NR*0.95+0.5)]"ms max="a[NR]"ms"}'
