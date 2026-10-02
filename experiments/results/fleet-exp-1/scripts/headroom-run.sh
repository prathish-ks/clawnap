#!/bin/bash
R=/home/ops/results; LOG=$R/headroom-run.log; : > $LOG
export FLEETD_DATA=/var/lib/fleetd FLEETD_TOKEN=exp
log() { echo "$(date -u +%T) $*" | tee -a $LOG; }
A="Authorization: Bearer exp"; API=http://127.0.0.1:8080
relaunch() { systemctl stop fleetd-exp 2>/dev/null; pkill -x fleetd; sleep 1
  systemd-run --unit=fleetd-exp --setenv=FLEETD_DATA=/var/lib/fleetd --setenv=FLEETD_TOKEN=exp /usr/local/bin/fleetd serve -listen 127.0.0.1:8080 -interval 5s -reclaim-keep-mib 150 -prefetch-on-wake -max-pause -1s -wake-concurrency 2 "$@" >/dev/null 2>&1; sleep 2; log "daemon relaunched: $* ($(systemctl is-active fleetd-exp))"; }
npaused() { docker ps --filter label=fleet.cell --filter status=paused --format x | wc -l; }
avail() { free -m | awk 'NR==2{print $7}'; }
# --- A. headroom mode, generous: ten recently active cells stay resident after they pause
relaunch -reclaim-after 30m -headroom-mib 2048
T0=$(date -u +"%Y-%m-%d %H:%M:%S")
for c in $(seq 1 10); do curl -s -o /dev/null -m 300 -X POST -H "$A" $API/wake/v$c & done; wait
log "woke v1-v10: avail=$(avail)MB"
for k in $(seq 1 60); do [ "$(npaused)" -ge 50 ] && break; sleep 5; done; sleep 30
res=$(fleetd cells list | python3 -c "import json,sys; cs=json.load(sys.stdin); print(sum(1 for c in cs if c['name'] in ['v%d'%i for i in range(1,11)] and c.get('phase')=='hibernated' and not c.get('swapped')))")
log "after idle: paused=$(npaused) resident_among_v1-10=$res avail=$(avail)MB reclaims_since=$(sudo journalctl -u fleetd-exp --no-pager --since "$T0" | grep -c 'reclaimed cell=') headroom_lines=$(sudo journalctl -u fleetd-exp --no-pager --since "$T0" | grep -c 'headroom below')"
/home/ops/burst-only.sh resident 10 nowait >/dev/null 2>&1; grep -E "BURST|cell=v" $R/burst-resident.log | cut -c1-330 | tee -a $LOG
# --- B. force pressure: target above what the host has; the ten resident cells must be reclaimed oldest-paused first
T1=$(date -u +"%Y-%m-%d %H:%M:%S"); avail0=$(avail)
relaunch -reclaim-after 30m -headroom-mib 12000
sleep 75
log "pressure: avail_before=${avail0}MB avail_after=$(avail)MB"
sudo journalctl -u fleetd-exp --no-pager --since "$T1" | grep -E "headroom below|reclaimed cell=" | sed -E 's/.*(INFO|WARN) //' | cut -c1-140 | tee -a $LOG
relaunch -reclaim-after 20s
echo HR-DONE | tee -a $LOG
