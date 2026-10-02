#!/bin/bash
# zswap as the compressed tier in front of the swapfile: single-cell cycles (off / lz4 / zstd), then a 10-wake burst at 100 cells.
R=/home/ops/results; LOG=$R/zswap-run.log; : > $LOG
export FLEETD_DATA=/var/lib/fleetd FLEETD_TOKEN=exp
log() { echo "$(date -u +%T) $*" | tee -a $LOG; }
A="Authorization: Bearer exp"; API=http://127.0.0.1:8080
Z=/sys/module/zswap/parameters
zs() { echo "zswap=$(cat $Z/enabled)/$(cat $Z/compressor)/$(cat $Z/zpool) pool=$(awk '/^Zswap:/{print int($2/1024)}' /proc/meminfo)MB zswapped=$(awk '/^Zswapped:/{print int($2/1024)}' /proc/meminfo)MB written_back=$(cat /sys/kernel/debug/zswap/written_back_pages 2>/dev/null) reject_alloc=$(cat /sys/kernel/debug/zswap/reject_alloc_fail 2>/dev/null) avail=$(free -m | awk 'NR==2{print $7}')MB"; }
psi() { awk -v k=$2 '$1==k{for(i=2;i<=NF;i++) if($i ~ /^total=/){sub("total=","",$i); print $i}}' /proc/pressure/$1; }
npaused() { docker ps --filter label=fleet.cell --filter status=paused --format x | wc -l; }
setpolicy() { mkdir -p /etc/systemd/system/fleetd.service.d
  printf '[Service]\nExecStart=\nExecStart=/usr/local/bin/fleetd serve -listen 127.0.0.1:8080 -interval 5s %s -reclaim-keep-mib 150 -prefetch-on-wake -wake-concurrency 2 -max-pause -1s\n' "$1" > /etc/systemd/system/fleetd.service.d/override.conf
  systemctl daemon-reload; systemctl restart fleetd; sleep 3; log "fleetd policy: $1 ($(systemctl is-active fleetd))"; }
cycles() { /home/ops/disk-tier.sh "$1" 3 >/dev/null 2>&1; grep cycle $R/disk-tier-$1.log | sed -E 's/\| psi.*//' | cut -c1-200 | tee -a $LOG; }
# --- 1. single-cell cycles, zswap off (today's disk baseline); timed reclaim so every cycle really reclaims
setpolicy "-reclaim-after 20s"
echo N > $Z/enabled; log "== cycles: zswap off $(zs)"; cycles zoff
# --- 2. lz4
echo zsmalloc > $Z/zpool; echo lz4 > $Z/compressor; echo 20 > $Z/max_pool_percent; echo Y > $Z/shrinker_enabled; echo Y > $Z/enabled
log "== cycles: zswap lz4 $(zs)"; cycles zlz4; log "after lz4 cycles: $(zs)"
# --- 3. zstd
echo N > $Z/enabled; echo zstd > $Z/compressor; echo Y > $Z/enabled
log "== cycles: zswap zstd $(zs)"; cycles zzstd; log "after zstd cycles: $(zs)"
# --- 4. burst at 100 with lz4 under the shipped policy:
setpolicy "-reclaim-after 30m -headroom-mib 8192"
#  wake v1..v10, let them pause, headroom reclaim puts them in the pool
echo N > $Z/enabled; echo lz4 > $Z/compressor; echo Y > $Z/enabled
for c in $(seq 1 10); do curl -s -o /dev/null -m 300 -X POST -H "$A" $API/wake/v$c & done; wait
for k in $(seq 1 60); do [ "$(npaused)" -ge 100 ] && break; sleep 5; done; sleep 60
sw=$(fleetd cells list | python3 -c "import json,sys; print(sum(1 for c in json.load(sys.stdin) if c['name'] in ['v%d'%i for i in range(1,11)] and c.get('swapped')))")
log "before burst: paused=$(npaused) v1-v10 swapped=$sw/10 $(zs)"
m0=$(psi memory full); c0=$(psi cpu some); i0=$(psi io full); T0=$(date +%s.%N); pids=""
vmstat 1 > $R/zswap-vmstat.txt & VP=$!
for c in $(seq 1 10); do ( s=$(date +%s.%N); code=$(curl -s -o /dev/null -m 300 -w "%{http_code}" -X POST -H "$A" $API/wake/v$c); e=$(date +%s.%N); echo "v$c $code $(python3 -c "print(round($e-$s,2))")" ) > $R/zsw-$c.txt & pids="$pids $!"; done
wait $pids; T1=$(date +%s.%N); sleep 2; kill $VP 2>/dev/null; m1=$(psi memory full); c1=$(psi cpu some); i1=$(psi io full)
times=$(for c in $(seq 1 10); do awk '{print $3}' $R/zsw-$c.txt; done | sort -n | tr '\n' ' '); p50=$(echo $times | tr ' ' '\n' | awk '{a[NR]=$1}END{print a[int((NR+1)/2)]}')
log "BURST zswap-lz4 n=10: wall=$(python3 -c "print(round($T1-$T0,1))")s min=$(echo $times | cut -d' ' -f1)s p50=${p50}s max=$(echo $times | awk '{print $NF}')s | times: $times| psi(ms) mem_full=$(( (m1-m0)/1000 )) cpu_some=$(( (c1-c0)/1000 )) io_full=$(( (i1-i0)/1000 )) | si_peak=$(awk 'NR>2{if($7>m)m=$7}END{print int(m/1024)}' $R/zswap-vmstat.txt)MB/s bi_peak=$(awk 'NR>2{if($9>m)m=$9}END{print int(m/1024)}' $R/zswap-vmstat.txt)MB/s"
log "after burst: $(zs)"
sudo journalctl -u fleetd --no-pager --since "-2min" | grep -E "prefetched cell=v([1-9]|10) " | grep -o -E "cell=v[0-9]+ .*landed_mib=[0-9]+ advise_took=[0-9.]+m?s pagein_took=[0-9.]+m?s" | sed -E "s/mechanism=[a-z_]+ procs=[0-9]+ mappings=[0-9]+ advised_mib=[0-9]+ //" | tr '\n' ';' | tee -a $LOG; echo | tee -a $LOG
for k in $(seq 1 60); do [ "$(npaused)" -ge 100 ] && break; sleep 5; done; sleep 30
log "final: paused=$(npaused) exited=$(docker ps -a --filter label=fleet.cell --filter status=exited --format x | wc -l) wake_failures=$(curl -s -H "$A" $API/metrics | awk '/^fleetd_wake_failures_total/{print $2}') $(zs)"; rm -rf /etc/systemd/system/fleetd.service.d; systemctl daemon-reload; systemctl restart fleetd; echo ZSWAP-DONE | tee -a $LOG
