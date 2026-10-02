#!/bin/bash
# Fair trial of a compressed swap store under the supervisor's own sequencing.
# usage: store-trial.sh <label>   (the store must already be the highest-priority swap device; see setup below)
L=$1; R=/home/ops/results; LOG=$R/store-$L.log; : > $LOG
export FLEETD_DATA=/var/lib/fleetd FLEETD_TOKEN=exp
log() { echo "$(date -u +%T) $*" | tee -a $LOG; }
A="Authorization: Bearer exp"; API=http://127.0.0.1:8080
cg() { id=$(docker inspect -f '{{.Id}}' $1); find /sys/fs/cgroup -maxdepth 4 -type d -name "*$id*" | head -1; }
psi() { awk -v k=$2 '$1==k{for(i=2;i<=NF;i++) if($i ~ /^total=/){sub("total=","",$i); print $i}}' /proc/pressure/$1; }
npaused() { docker ps --filter label=fleet.cell --filter status=paused --format x | wc -l; }
avail() { free -m | awk 'NR==2{print $7}'; }
store() { if zpool list swappool >/dev/null 2>&1; then echo "zvol_used=$(zfs get -H -o value used swappool/swap) ratio=$(zfs get -H -o value compressratio swappool/swap) arc=$(awk '/^size/{print int($3/1048576)}' /proc/spl/kstat/zfs/arcstats)MiB"; elif [ -e /dev/mapper/vdo-swap ] 2>/dev/null || command -v vdostats >/dev/null; then vdostats --human-readable 2>/dev/null | tail -1 | awk '{print "vdo_used="$3" saving="$6}'; fi; echo "swap: $(swapon --show --noheadings | awk '{printf "%s=%s ", $1, $4}')"; }
state() { log "STATE $1: paused=$(npaused) avail=$(avail)MB used=$(free -m | awk 'NR==2{print $3}')MB $(store)"; }
burst() { local tag=$1; shift; local cells="$@"; local m0 c0 i0 m1 c1 i1 T0 T1 pids="" a0=$(avail)
  m0=$(psi memory full); c0=$(psi cpu some); i0=$(psi io full); T0=$(date +%s.%N)
  for c in $cells; do ( s=$(date +%s.%N); code=$(curl -s -o /dev/null -m 300 -w "%{http_code}" -X POST -H "$A" $API/wake/$c); e=$(date +%s.%N); echo "$c $code $(python3 -c "print(round($e-$s,2))")" ) > $R/st-$c.txt & pids="$pids $!"; done
  wait $pids; T1=$(date +%s.%N); sleep 2; m1=$(psi memory full); c1=$(psi cpu some); i1=$(psi io full)
  local times=$(for c in $cells; do awk '{print $3}' $R/st-$c.txt; done | sort -n | tr '\n' ' '); local codes=$(for c in $cells; do awk '{print $2}' $R/st-$c.txt; done | sort | uniq -c | tr -s ' ' | tr '\n' ';')
  local p50=$(echo $times | tr ' ' '\n' | awk '{a[NR]=$1}END{print a[int((NR+1)/2)]}')
  log "BURST $tag: codes=$codes min=$(echo $times | cut -d' ' -f1)s p50=${p50}s max=$(echo $times | awk '{print $NF}')s | times: $times| psi(ms) mem_full=$(( (m1-m0)/1000 )) cpu_some=$(( (c1-c0)/1000 )) io_full=$(( (i1-i0)/1000 )) | avail before=${a0}MB after=$(avail)MB"; }
setpolicy() { mkdir -p /etc/systemd/system/fleetd.service.d
  printf '[Service]\nExecStart=\nExecStart=/usr/local/bin/fleetd serve -listen 127.0.0.1:8080 -interval 5s -reclaim-after 30m -reclaim-keep-mib 150 -wake-concurrency 2 -max-pause -1s -prefetch-on-wake %s\n' "$1" > /etc/systemd/system/fleetd.service.d/override.conf
  systemctl daemon-reload; systemctl restart fleetd; sleep 3; log "fleetd policy: $1"; }
waitpaused() { for k in $(seq 1 60); do [ "$(npaused)" -ge 100 ] && break; sleep 5; done; sleep 20; }
cap() { for c in "$@"; do echo 350M > $(cg $c)/memory.high; done; }
uncap() { for c in "$@"; do echo max > $(cg $c)/memory.high; done; }
R5="$(seq -s ' ' -f v%g 1 5)"; C1="$(seq -s ' ' -f v%g 21 30)"; C2="$(seq -s ' ' -f v%g 31 40)"
log "== store trial: $L"; state "start"
# ---- 1. bring tgw + v1..v40 into the store the way production would: restart 5 at a time, let the daemon reclaim under its 8 GB target before the next batch
setpolicy "-headroom-mib 8192"
m0=$(psi memory full); T0=$(date +%s)
for batch in "tgw v1 v2 v3 v4" "v5 v6 v7 v8 v9" "v10 v11 v12 v13 v14" "v15 v16 v17 v18 v19" "v20 v21 v22 v23 v24" "v25 v26 v27 v28 v29" "v30 v31 v32 v33 v34" "v35 v36 v37 v38 v39" "v40"; do
  for c in $batch; do docker restart -t 10 $c >/dev/null 2>&1; done
  for c in $batch; do p=21001; [ "$c" != tgw ] && p=$((22000+${c#v})); for k in $(seq 1 60); do curl -s -o /dev/null -m 2 -w "%{http_code}" http://127.0.0.1:$p/health | grep -q 200 && break; docker inspect -f "{{.State.Paused}}" $c | grep -q true && break; sleep 2; done; done
  for k in $(seq 1 36); do [ "$(avail)" -gt 7500 ] && break; sleep 5; done
  log "batch [$batch] in store: avail=$(avail)MB load=$(cut -d' ' -f1 /proc/loadavg) $(store)"
done
log "fill done in $(( $(date +%s)-T0 ))s, memory full stall over the fill: $(( ($(psi memory full)-m0)/1000 ))ms"; waitpaused; state "41-cells-in-store"
# ---- 2. single cold wakes of tgw (wake, idle -> pause, daemon reclaims under the target, wake again)
for i in 1 2 3; do curl -s -o /dev/null -X POST -H "$A" $API/wake/tgw; sleep 5; curl -s -o /dev/null -X POST -H "$A" $API/hibernate/tgw
  for k in $(seq 1 40); do sw=$(fleetd cells list | python3 -c "import json,sys; print([c.get('swapped') for c in json.load(sys.stdin) if c['name']=='tgw'][0])"); [ "$sw" = "True" ] && break; sleep 3; done; sleep 30
  T=$(date -u +"%Y-%m-%d %H:%M:%S"); s=$(date +%s.%N); curl -s -o /dev/null -m 120 -X POST -H "$A" $API/wake/tgw; e=$(date +%s.%N)
  log "single cold wake $i: api=$(python3 -c "print(round($e-$s,2))")s $(journalctl -u fleetd --no-pager --since "$T" | grep -o -E "cell=tgw .*pagein_took=[0-9.]+m?s" | grep -o -E "swap_before_mib=[0-9]+|landed_mib=[0-9]+|advise_took=[0-9.]+m?s|pagein_took=[0-9.]+m?s" | tr '\n' ' ')"; done
curl -s -o /dev/null -X POST -H "$A" $API/hibernate/tgw
# ---- 3. adopted scenario: 5 resident capped, warm burst (cap lifted), two cold bursts of ten from the store
setpolicy "-headroom-mib 6144"
for c in $R5; do curl -s -o /dev/null -m 300 -X POST -H "$A" $API/wake/$c & done; wait; cap $R5; waitpaused; sleep 30; state "5-resident"
uncap $R5; burst "warm-5 (cap lifted at wake)" $R5; cap $R5; waitpaused; sleep 20; state "after-warm"
burst "cold-10 (v21..v30 from the store)" $C1; waitpaused; state "after-cold-1"
burst "cold-10 (v31..v40 from the store)" $C2; waitpaused; state "after-cold-2"
uncap $R5 $C1 $C2; setpolicy "-headroom-mib 8192"; log "wake_failures=$(curl -s -H "$A" $API/metrics | awk '/^fleetd_wake_failures_total/{print $2}') exited=$(docker ps -a --filter label=fleet.cell --filter status=exited --format x | wc -l)"; echo TRIAL-DONE | tee -a $LOG
