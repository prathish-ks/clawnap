#!/usr/bin/env bash
# Week-1 baseline: N stock OpenClaw cells as plain hardened containers (no
# supervisor), each with its own config, gateway port and bot-less Telegram
# stub, so idle memory, cold start and the host ceiling can be measured.
#   baseline-cells.sh up N     create/start cells 1..N (idempotent)
#   baseline-cells.sh sample   one CSV line per running cell: name,rss_mib,cpu,pids
#   baseline-cells.sh ready    seconds until each cell answers /health
#   baseline-cells.sh down     remove all baseline cells
set -euo pipefail
ROOT=${ROOT:-$HOME/fleet/cells}
IMG=${IMG:-ghcr.io/openclaw/openclaw:latest}
BASE_PORT=${BASE_PORT:-19000}
cmd=${1:-}; n=${2:-0}
mk() { i=$1; d=$ROOT/b$i; mkdir -p $d/state $d/auth
  [ -f $d/state/openclaw.json ] || cat > $d/state/openclaw.json <<JSON
{ "gateway": { "mode": "local", "bind": "auto", "auth": { "mode": "token", "token": "$(openssl rand -hex 16)" } },
  "agents": { "defaults": { "workspace": "/home/node/.openclaw/workspace" } } }
JSON
  sudo chmod 600 $d/state/openclaw.json; sudo chown -R 1000:1000 $d
  docker inspect b$i >/dev/null 2>&1 && { docker start b$i >/dev/null; return; }
  docker run -d --name b$i --label fleet.cell=b$i --user 1000:1000 \
    -v $d/state:/home/node/.openclaw -v $d/auth:/home/node/.config/openclaw \
    -p 127.0.0.1:$((BASE_PORT+i)):18789 --pids-limit 512 --memory 1g \
    --cap-drop ALL --security-opt no-new-privileges --restart no $IMG >/dev/null; }
case $cmd in
  up) for i in $(seq 1 $n); do mk $i; done; echo "started $n cells";;
  sample) docker stats --no-stream --format '{{.Name}},{{.MemUsage}},{{.CPUPerc}},{{.PIDs}}' $(docker ps --filter label=fleet.cell --format '{{.Names}}' | sort -V) | awk -F, 'BEGIN{OFS=","} {split($2,m," "); v=m[1]; if (v ~ /GiB$/) v=v*1024; sub(/[A-Za-z]+$/,"",v); $2=v; print}';;
  ready) for c in $(docker ps -a --filter label=fleet.cell --format '{{.Names}}' | sort -V); do p=$(docker port $c 18789/tcp | cut -d: -f2); t0=$(docker inspect -f '{{.State.StartedAt}}' $c | python3 -c 'import sys,datetime;print(int(datetime.datetime.fromisoformat(sys.stdin.read().strip().replace("Z","+00:00")[:26]+"+00:00").timestamp()))' 2>/dev/null || date +%s); for k in $(seq 1 120); do curl -s -o /dev/null -m 2 -w '%{http_code}' http://127.0.0.1:$p/health | grep -q 200 && { echo "$c,$(( $(date +%s)-t0 ))"; break; }; sleep 2; done; done;;
  down) docker rm -f $(docker ps -aq --filter label=fleet.cell) >/dev/null 2>&1 || true; echo removed;;
  *) echo "usage: $0 up N | sample | ready | down"; exit 1;;
esac
