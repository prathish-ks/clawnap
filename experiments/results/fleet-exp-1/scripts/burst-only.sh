#!/bin/bash
# usage: burst-only.sh <tag> <n> [nowait]  — settle (all 50 paused), burst n wakes of v1..vn via the daemon API, settle again
TAG=$1; N=${2:-10}; R=/home/ops/results; LOG=$R/burst-$TAG.log; : > $LOG
export FLEETD_DATA=/var/lib/fleetd FLEETD_TOKEN=exp
log() { echo "$(date -u +%T) $*" | tee -a $LOG; }
A="Authorization: Bearer exp"; API=http://127.0.0.1:8080
psi() { awk -v k=$2 '$1==k{for(i=2;i<=NF;i++) if($i ~ /^total=/){sub("total=","",$i); print $i}}' /proc/pressure/$1; }
npaused() { docker ps --filter label=fleet.cell --filter status=paused --format x | wc -l; }
nres() { curl -s -H "$A" $API/metrics | awk '/^fleetd_cells\{phase="hibernated"\}/{print $2}'; }
settle_all() { for k in $(seq 1 120); do [ "$(npaused)" -ge 50 ] && break; sleep 5; done; sleep 40
  log "settled: paused=$(npaused) free=$(free -m | awk 'NR==2{print $4}')MB avail=$(free -m | awk 'NR==2{print $7}')MB swap_used=$(awk '/SwapTotal/{t=$2}/SwapFree/{f=$2}END{print int((t-f)/1024)}' /proc/meminfo)MB load=$(cut -d' ' -f1 /proc/loadavg)"; }
burst() { local n=$1; local T0 m0 c0 i0 m1 c1 i1 pids=""
  m0=$(psi memory full); c0=$(psi cpu some); i0=$(psi io full); T0=$(date +%s.%N)
  for c in $(seq 1 $n); do ( s=$(date +%s.%N); code=$(curl -s -o /dev/null -m 300 -w "%{http_code}" -X POST -H "$A" $API/wake/v$c); e=$(date +%s.%N); echo "v$c $code $(python3 -c "print(round($e-$s,2))")" ) > $R/bo-$c.txt & pids="$pids $!"; done
  wait $pids; local T1=$(date +%s.%N); sleep 2
  m1=$(psi memory full); c1=$(psi cpu some); i1=$(psi io full)
  local times=$(for c in $(seq 1 $n); do awk '{print $3}' $R/bo-$c.txt; done | sort -n | tr '\n' ' '); local codes=$(for c in $(seq 1 $n); do awk '{print $2}' $R/bo-$c.txt; done | sort | uniq -c | tr -s ' ' | tr '\n' ';')
  local p50=$(echo $times | tr ' ' '\n' | awk '{a[NR]=$1}END{print a[int((NR+1)/2)]}'); local mx=$(echo $times | tr ' ' '\n' | tail -1); local mn=$(echo $times | tr ' ' '\n' | head -1)
  log "BURST $TAG n=$n: codes=$codes wall_all=$(python3 -c "print(round($T1-$T0,1))")s min=${mn}s p50=${p50}s max=${mx}s | times: $times| psi(ms) mem_full=$(( (m1-m0)/1000 )) cpu_some=$(( (c1-c0)/1000 )) io_full=$(( (i1-i0)/1000 ))"
  sudo journalctl -u fleetd-exp --no-pager --since "-2min" | grep -E "woke cell=v[0-9]+ " | sed -E 's/.*woke //' | tr '\n' ';' | tee -a $LOG; echo | tee -a $LOG; }
[ "$3" = "nowait" ] || settle_all
log "daemon: $(pgrep -a -x fleetd | cut -d' ' -f3- | tr -s ' ' | cut -c1-150) binary=$(md5sum /usr/local/bin/fleetd | cut -c1-8)"
burst $N; settle_all; echo BURST-DONE | tee -a $LOG
