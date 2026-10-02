#!/bin/bash
R=/home/ops/results; LOG=$R/hr8.log; : > $LOG
export FLEETD_DATA=/var/lib/fleetd FLEETD_TOKEN=exp
log() { echo "$(date -u +%T) $*" | tee -a $LOG; }
A="Authorization: Bearer exp"; API=http://127.0.0.1:8080
psi() { awk -v k=$2 '$1==k{for(i=2;i<=NF;i++) if($i ~ /^total=/){sub("total=","",$i); print $i}}' /proc/pressure/$1; }
availmb() { free -m | awk 'NR==2{print $7}'; }
npaused() { docker ps --filter label=fleet.cell --filter status=paused --format x | wc -l; }
mkdir -p /etc/systemd/system/fleetd.service.d
printf '[Service]\nExecStart=\nExecStart=/usr/local/bin/fleetd serve -listen 127.0.0.1:8080 -interval 5s -reclaim-after 30m -headroom-mib %s -reclaim-keep-mib 150 -prefetch-on-wake -wake-concurrency 2 -max-pause -1s\n' "$1" > /etc/systemd/system/fleetd.service.d/override.conf
systemctl daemon-reload; systemctl restart fleetd; sleep 3; log "fleetd restarted with headroom $1 MiB ($(systemctl is-active fleetd))"
for k in $(seq 1 40); do [ "$(availmb)" -ge $(( $1 - 200 )) ] && break; sleep 5; done; sleep 20
cold=$(fleetd cells list | python3 -c "import json,sys; print(sum(1 for c in json.load(sys.stdin) if c['name'] in ['v%d'%i for i in range(1,11)] and c.get('swapped')))")
log "before burst: paused=$(npaused) avail=$(availmb)MB v1-v10 swapped=$cold/10 resident_paused=$(fleetd cells list | python3 -c "import json,sys; print(sum(1 for c in json.load(sys.stdin) if c.get('phase')=='hibernated' and not c.get('swapped')))")"
m0=$(psi memory full); i0=$(psi io full); T0=$(date +%s.%N); pids=""
for c in $(seq 1 10); do ( s=$(date +%s.%N); code=$(curl -s -o /dev/null -m 300 -w "%{http_code}" -X POST -H "$A" $API/wake/v$c); e=$(date +%s.%N); echo "v$c $code $(python3 -c "print(round($e-$s,2))")" ) > $R/hr8-$c.txt & pids="$pids $!"; done
wait $pids; T1=$(date +%s.%N); sleep 2; m1=$(psi memory full); i1=$(psi io full)
times=$(for c in $(seq 1 10); do awk '{print $3}' $R/hr8-$c.txt; done | sort -n | tr '\n' ' '); p50=$(echo $times | tr ' ' '\n' | awk '{a[NR]=$1}END{print a[int((NR+1)/2)]}')
log "BURST cold100 headroom=$1: wall=$(python3 -c "print(round($T1-$T0,1))")s min=$(echo $times | cut -d' ' -f1)s p50=${p50}s max=$(echo $times | awk '{print $NF}')s | times: $times| psi(ms) mem_full=$(( (m1-m0)/1000 )) io_full=$(( (i1-i0)/1000 ))"
for k in $(seq 1 60); do [ "$(npaused)" -ge 100 ] && break; sleep 5; done; sleep 30
log "after: paused=$(npaused) avail=$(availmb)MB wake_failures=$(curl -s -H "$A" $API/metrics | awk '/^fleetd_wake_failures_total/{print $2}')"; echo HR8-DONE | tee -a $LOG
