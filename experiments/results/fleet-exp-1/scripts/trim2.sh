#!/bin/bash
# Trimmed warm cells: paused, resident, but with their cold pages pushed into the zswap zstd pool (memory.reclaim "300M"), then woken five at once.
R=/home/ops/results; LOG=$R/trim2.log; : > $LOG
export FLEETD_DATA=/var/lib/fleetd FLEETD_TOKEN=exp
log() { echo "$(date -u +%T) $*" | tee -a $LOG; }
A="Authorization: Bearer exp"; API=http://127.0.0.1:8080; Z=/sys/module/zswap/parameters
cg() { id=$(docker inspect -f '{{.Id}}' $1); find /sys/fs/cgroup -maxdepth 4 -type d -name "*$id*" | head -1; }
st() { for c in "$@"; do d=$(cg $c); printf "%s:res=%dM/z=%dM/s=%dM " $c $(( $(cat $d/memory.current)/1048576 )) $(( $(cat $d/memory.zswap.current)/1048576 )) $(( $(cat $d/memory.swap.current)/1048576 )); done; }
echo Y > $Z/enabled; log "zswap $(cat $Z/enabled)/$(cat $Z/compressor) pool=$(awk '/^Zswap:/{print int($2/1024)}' /proc/meminfo)MB avail=$(free -m | awk 'NR==2{print $7}')MB"
# five cells awake, then paused by the API (resident: headroom is met after the shrinker freed RAM)
for c in 1 2 3 4 5; do curl -s -o /dev/null -m 300 -X POST -H "$A" $API/wake/v$c & done; wait; sleep 20
for c in 1 2 3 4 5; do curl -s -o /dev/null -X POST -H "$A" $API/hibernate/v$c; done; sleep 5
log "paused resident: $(st v1 v2 v3 v4 v5)"
# trim: ask the kernel to reclaim ~300 MiB from each while paused (pages go to zswap first)
T0=$(date +%s.%N); for c in 1 2 3 4 5; do echo 300M > $(cg v$c)/memory.reclaim 2>/dev/null; done; T1=$(date +%s.%N)
log "trimmed in $(python3 -c "print(round($T1-$T0,2))")s: $(st v1 v2 v3 v4 v5) pool=$(awk '/^Zswap:/{print int($2/1024)}' /proc/meminfo)MB avail=$(free -m | awk 'NR==2{print $7}')MB"
sleep 60
# wake all five at once (the daemon sees them as resident-paused: no prefetch, pages fault back from the pool on demand)
pids=""; T0=$(date +%s.%N)
for c in 1 2 3 4 5; do ( s=$(date +%s.%N); code=$(curl -s -o /dev/null -m 300 -w "%{http_code}" -X POST -H "$A" $API/wake/v$c); e=$(date +%s.%N); echo "v$c $code $(python3 -c "print(round($e-$s,2))")" ) > $R/trim-$c.txt & pids="$pids $!"; done
wait $pids; T1=$(date +%s.%N); sleep 3
times=$(for c in 1 2 3 4 5; do awk '{print $3}' $R/trim-$c.txt; done | sort -n | tr '\n' ' ')
log "WAKE trimmed x5: wall=$(python3 -c "print(round($T1-$T0,1))")s times: $times| after: $(st v1 v2 v3 v4 v5)"
# does the cell actually work after the trim? one health + a second later resident size
sleep 20; log "20 s later: $(st v1 v2 v3 v4 v5) health=$(for c in 1 2 3 4 5; do curl -s -o /dev/null -m 2 -w "%{http_code}," http://127.0.0.1:$((22000+c))/health; done)"
echo N > $Z/enabled; echo TRIM-DONE | tee -a $LOG
