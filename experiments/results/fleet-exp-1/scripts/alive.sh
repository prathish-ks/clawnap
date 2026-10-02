#!/bin/bash
# Alive cells under memory.high: does an idle-running gateway stay quiet at 450 MiB (and at 350 MiB), with zswap zstd vs the disk alone?
R=/home/ops/results; LOG=$R/alive.log; : > $LOG
log() { echo "$(date -u +%T) $*" | tee -a $LOG; }
Z=/sys/module/zswap/parameters
cg() { id=$(docker inspect -f '{{.Id}}' $1); find /sys/fs/cgroup -maxdepth 4 -type d -name "*$id*" | head -1; }
psi() { awk -v k=$2 '$1==k{for(i=2;i<=NF;i++) if($i ~ /^total=/){sub("total=","",$i); print $i}}' /proc/pressure/$1; }
mem() { local tot=0 z=0 s=0; for c in "$@"; do d=$(cg $c); tot=$((tot + $(cat $d/memory.current)/1048576)); z=$((z + $(cat $d/memory.zswap.current)/1048576)); s=$((s + $(cat $d/memory.swap.current)/1048576)); done; echo "resident_sum=${tot}M zswap_sum=${z}M swap_sum=${s}M per_cell=$((tot/$#))M"; }
diag() { local T=$1; shift; local lv=0 el=0 mp=0 hb=0; for c in "$@"; do lg=$(docker logs $c --since $T 2>&1); lv=$((lv + $(echo "$lg" | grep -c "liveness"))); el=$((el + $(echo "$lg" | grep -c -i "event_loop\|event loop"))); mp=$((mp + $(echo "$lg" | grep -c "memory pressure"))); hb=$((hb + $(echo "$lg" | grep -c "heartbeat delayed"))); done; echo "diag(liveness=$lv event_loop=$el mem_pressure=$mp hb_delayed=$hb)"; }
hl() { local worst=0 sum=0 n=0; for c in "$@"; do p=$((22000+${c#v})); for k in 1 2 3 4 5; do t=$(curl -s -o /dev/null -m 5 -w "%{time_total}" http://127.0.0.1:$p/health); ms=$(python3 -c "print(int($t*1000))"); sum=$((sum+ms)); n=$((n+1)); [ $ms -gt $worst ] && worst=$ms; done; done; echo "health_ms(avg=$((sum/n)) worst=$worst)"; }
phase() { # phase <label> <cells...>
  local label=$1; shift; local cells="$@"
  for c in $cells; do docker unpause $c >/dev/null 2>&1; done
  for c in $cells; do p=$((22000+${c#v})); for k in $(seq 1 30); do curl -s -o /dev/null -m 2 -w "%{http_code}" http://127.0.0.1:$p/health | grep -q 200 && break; sleep 2; done; done; sleep 45
  log "== $label: alive, no limit: $(mem $cells) $(hl $cells) load=$(cut -d' ' -f1 /proc/loadavg)"
  for lim in 450M 350M; do
    T=$(date -u +%Y-%m-%dT%H:%M:%S); c0=$(psi cpu some); m0=$(psi memory some)
    for c in $cells; do echo $lim > $(cg $c)/memory.high; done; sleep 90
    log "$label high=$lim +90s: $(mem $cells) $(diag $T $cells) $(hl $cells) psi(ms) cpu_some=$(( ($(psi cpu some)-c0)/1000 )) mem_some=$(( ($(psi memory some)-m0)/1000 )) load=$(cut -d' ' -f1 /proc/loadavg) pool=$(awk '/^Zswap:/{print int($2/1024)}' /proc/meminfo)MB"
    T=$(date -u +%Y-%m-%dT%H:%M:%S); sleep 120
    log "$label high=$lim +210s: $(mem $cells) $(diag $T $cells) $(hl $cells) load=$(cut -d' ' -f1 /proc/loadavg)"
  done
  for c in $cells; do echo max > $(cg $c)/memory.high; done
}
systemctl stop fleetd; log "fleetd stopped for the test; paused=$(docker ps --filter label=fleet.cell --filter status=paused --format x | wc -l)"
echo N > $Z/enabled; echo zstd > $Z/compressor; echo 30 > $Z/max_pool_percent; echo Y > $Z/enabled
phase "zswap-zstd" v1 v2 v3 v4 v5 v6 v7 v8 v9 v10
echo N > $Z/enabled
phase "disk-only" v11 v12 v13 v14 v15 v16 v17 v18 v19 v20
for c in $(seq -f v%g 1 20); do docker pause $c >/dev/null 2>&1; done
systemctl start fleetd; sleep 3; log "fleetd restarted ($(systemctl is-active fleetd)); zswap=$(cat $Z/enabled)"; echo ALIVE-DONE | tee -a $LOG
