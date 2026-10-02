#!/bin/bash
# Best case for a compressed RAM tier: fresh cells (no disk slots) reclaimed into zswap zstd; single wakes, a burst of ten from the pool, and a second burst after a re-cycle.
R=/home/ops/results; LOG=$R/zswap2.log; : > $LOG
export FLEETD_DATA=/var/lib/fleetd FLEETD_TOKEN=exp
log() { echo "$(date -u +%T) $*" | tee -a $LOG; }
A="Authorization: Bearer exp"; API=http://127.0.0.1:8080; Z=/sys/module/zswap/parameters
zs() { echo "pool=$(awk '/^Zswap:/{print int($2/1024)}' /proc/meminfo)MB zswapped=$(awk '/^Zswapped:/{print int($2/1024)}' /proc/meminfo)MB stored_pages=$(cat /sys/kernel/debug/zswap/stored_pages) written_back=$(cat /sys/kernel/debug/zswap/written_back_pages) avail=$(free -m | awk 'NR==2{print $7}')MB"; }
psi() { awk -v k=$2 '$1==k{for(i=2;i<=NF;i++) if($i ~ /^total=/){sub("total=","",$i); print $i}}' /proc/pressure/$1; }
npaused() { docker ps --filter label=fleet.cell --filter status=paused --format x | wc -l; }
cellz() { for c in "$@"; do id=$(docker inspect -f '{{.Id}}' $c); d=$(find /sys/fs/cgroup -maxdepth 4 -type d -name "*$id*" | head -1); printf "%s:z=%dM/s=%dM " $c $(( $(cat $d/memory.zswap.current) / 1048576 )) $(( $(cat $d/memory.swap.current) / 1048576 )); done; }
burst() { local tag=$1; local m0 c0 i0 m1 c1 i1 T0 T1 pids=""; m0=$(psi memory full); c0=$(psi cpu some); i0=$(psi io full); T0=$(date +%s.%N)
  for c in $(seq 1 10); do ( s=$(date +%s.%N); code=$(curl -s -o /dev/null -m 300 -w "%{http_code}" -X POST -H "$A" $API/wake/v$c); e=$(date +%s.%N); echo "v$c $code $(python3 -c "print(round($e-$s,2))")" ) > $R/z2-$c.txt & pids="$pids $!"; done
  wait $pids; T1=$(date +%s.%N); sleep 2; m1=$(psi memory full); c1=$(psi cpu some); i1=$(psi io full)
  local times=$(for c in $(seq 1 10); do awk '{print $3}' $R/z2-$c.txt; done | sort -n | tr '\n' ' '); local p50=$(echo $times | tr ' ' '\n' | awk '{a[NR]=$1}END{print a[int((NR+1)/2)]}')
  log "BURST $tag: wall=$(python3 -c "print(round($T1-$T0,1))")s min=$(echo $times | cut -d' ' -f1)s p50=${p50}s max=$(echo $times | awk '{print $NF}')s | times: $times| psi(ms) mem_full=$(( (m1-m0)/1000 )) cpu_some=$(( (c1-c0)/1000 )) io_full=$(( (i1-i0)/1000 ))"
  sudo journalctl -u fleetd --no-pager --since "-2min" | grep -E "prefetched cell=v([1-9]|10) " | grep -o -E "swap_before_mib=[0-9]+ landed_mib=[0-9]+ advise_took=[0-9.]+m?s pagein_took=[0-9.]+m?s" | tr '\n' ';' | cut -c1-400 | tee -a $LOG; echo | tee -a $LOG; }
settle() { for k in $(seq 1 60); do [ "$(npaused)" -ge 100 ] && break; sleep 5; done; sleep 45; }
# --- zswap zstd, pool up to 30 % of RAM
# keep current pool: echo N > $Z/enabled; echo zsmalloc > $Z/zpool; echo zstd > $Z/compressor; echo 30 > $Z/max_pool_percent; echo Y > $Z/shrinker_enabled; echo Y > $Z/enabled
log "zswap: $(cat $Z/enabled)/$(cat $Z/compressor) max_pool=$(cat $Z/max_pool_percent)% $(zs)"
# --- fresh pages: restart tgw and v1..v10 so nothing of theirs has a disk slot; let them pause; headroom reclaim writes them into the pool
# skipped: for c in tgw $(seq -f v%g 1 10); do docker restart $c >/dev/null 2>&1; done
# skipped: for c in tgw $(seq -f v%g 1 10); do p=21001; [ "$c" != tgw ] && p=$((22000+${c#v})); for k in $(seq 1 60); do curl -s -o /dev/null -m 2 -w "%{http_code}" http://127.0.0.1:$p/health | grep -q 200 && break; sleep 2; done; done
settle; log "after first reclaim of fresh cells: $(zs)"; log "cells: $(cellz tgw v1 v5 v10)"
# --- single wakes of tgw from the pool (3 cycles: wake, idle→pause, reclaim, wake)
for i in 1 2 3; do curl -s -o /dev/null -X POST -H "$A" $API/wake/tgw; sleep 5; curl -s -o /dev/null -X POST -H "$A" $API/hibernate/tgw; for k in $(seq 1 30); do sw=$(fleetd cells list | python3 -c "import json,sys; print([c.get('swapped') for c in json.load(sys.stdin) if c['name']=='tgw'][0])"); [ "$sw" = "True" ] && break; sleep 3; done; sleep 40
  T0=$(date -u +"%Y-%m-%d %H:%M:%S"); s=$(date +%s.%N); curl -s -o /dev/null -m 120 -X POST -H "$A" $API/wake/tgw; e=$(date +%s.%N)
  log "tgw wake $i: api=$(python3 -c "print(round($e-$s,2))")s $(sudo journalctl -u fleetd --no-pager --since "$T0" | grep -o -E "cell=tgw .*pagein_took=[0-9.]+m?s" | grep -o -E "swap_before_mib=[0-9]+|landed_mib=[0-9]+|advise_took=[0-9.]+m?s|pagein_took=[0-9.]+m?s" | tr '\n' ' ') $(cellz tgw)"; done
curl -s -o /dev/null -X POST -H "$A" $API/hibernate/tgw
# --- burst of ten from the pool
settle; log "before burst 1: paused=$(npaused) $(zs) $(cellz v1 v5 v10)"; burst pool-1
# --- re-cycle: do the cells stay in the pool after being woken once? then burst again
settle; log "before burst 2: paused=$(npaused) $(zs) $(cellz v1 v5 v10)"; burst pool-2
settle; log "final: paused=$(npaused) exited=$(docker ps -a --filter label=fleet.cell --filter status=exited --format x | wc -l) wake_failures=$(curl -s -H "$A" $API/metrics | awk '/^fleetd_wake_failures_total/{print $2}') $(zs)"
echo N > $Z/enabled; echo Z2-DONE | tee -a $LOG
