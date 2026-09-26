#!/usr/bin/env bash
# Sample per-container memory, CPU and NetIO for all fleet cells every INTERVAL seconds. CSV to stdout.
set -euo pipefail
INTERVAL=${1:-30}
echo "ts,container,state,mem_bytes,cpu_pct,net_rx,net_tx"
while true; do
  ts=$(date -u +%FT%TZ)
  docker ps -a --filter label=fleet.cell --format '{{.Names}} {{.State}}' | while read -r name state; do
    if [ "$state" = "running" ]; then
      docker stats --no-stream --format '{{.Name}},{{.MemUsage}},{{.CPUPerc}},{{.NetIO}}' "$name" \
        | awk -F, -v ts="$ts" -v st="$state" '{gsub(/ /,"",$2); split($2,m,"/"); split($4,n,"/"); print ts","$1","st","m[1]","$3","n[1]","n[2]}'
    else
      echo "$ts,$name,$state,0,0,0,0"
    fi
  done
  sleep "$INTERVAL"
done
