#!/bin/bash
# Burst-size curve at 50 cells (1,2,4,8,10 simultaneous wakes), then grow to 100 cells and burst 10 again.
R=/home/ops/results; LOG=$R/curve-run.log; : > $LOG
export FLEETD_DATA=/var/lib/fleetd FLEETD_TOKEN=exp
log() { echo "$(date -u +%T) $*" | tee -a $LOG; }
A="Authorization: Bearer exp"; API=http://127.0.0.1:8080
psi() { awk -v k=$2 '$1==k{for(i=2;i<=NF;i++) if($i ~ /^total=/){sub("total=","",$i); print $i}}' /proc/pressure/$1; }
INGRESS=https://167-233-118-221.sslip.io; TOKF=/root/bot.token
ncells() { docker ps -a --filter label=fleet.cell --format x | wc -l; }
npaused() { docker ps --filter label=fleet.cell --filter status=paused --format x | wc -l; }
swapused() { awk '/SwapTotal/{t=$2}/SwapFree/{f=$2}END{print int((t-f)/1024)}' /proc/meminfo; }
settle_all() { # wait until every cell is paused again and reclaim has moved their pages out
  local want=$1; for k in $(seq 1 120); do [ "$(npaused)" -ge "$want" ] && break; sleep 5; done; sleep 40
  log "settled: paused=$(npaused)/$want swap_used=$(swapused)MB free=$(free -m | awk 'NR==2{print $4}')MB load=$(cut -d' ' -f1 /proc/loadavg)"
}
burst() { # burst <n> : wake v1..vn at once through the daemon API; wall time per cell = wake ready incl. thaw settle
  local n=$1; local tag=$2; local T0 m0 s0 c0 i0 m1 s1 c1 i1 pids=""
  m0=$(psi memory full); s0=$(psi memory some); c0=$(psi cpu some); i0=$(psi io full)
  vmstat 1 > $R/curve-vmstat-$tag-$n.txt & local VP=$!
  T0=$(date +%s.%N)
  for c in $(seq 1 $n); do
    ( s=$(date +%s.%N); code=$(curl -s -o /dev/null -m 300 -w "%{http_code}" -X POST -H "$A" $API/wake/v$c); e=$(date +%s.%N)
      echo "v$c $code $(python3 -c "print(round($e-$s,2))")" ) > $R/curve-b-$c.txt &
    pids="$pids $!"
  done
  wait $pids; local T1=$(date +%s.%N); sleep 2; kill $VP 2>/dev/null
  m1=$(psi memory full); s1=$(psi memory some); c1=$(psi cpu some); i1=$(psi io full)
  local times=$(for c in $(seq 1 $n); do awk '{print $3}' $R/curve-b-$c.txt; done | sort -n | tr '\n' ' ')
  local codes=$(for c in $(seq 1 $n); do awk '{print $2}' $R/curve-b-$c.txt; done | sort | uniq -c | tr -s ' ' | tr '\n' ';')
  local p50=$(echo $times | tr ' ' '\n' | awk '{a[NR]=$1}END{print a[int((NR+1)/2)]}')
  local mx=$(echo $times | tr ' ' '\n' | tail -1); local mn=$(echo $times | tr ' ' '\n' | head -1)
  local sip=$(awk 'NR>2{if($7>m)m=$7}END{print int(m/1024)}' $R/curve-vmstat-$tag-$n.txt); local sop=$(awk 'NR>2{s+=$8}END{print int(s/1024)}' $R/curve-vmstat-$tag-$n.txt)
  local wa=$(awk 'NR>2{if($16>m)m=$16}END{print m+0}' $R/curve-vmstat-$tag-$n.txt); local idl=$(awk 'NR>2{if(m==""||$15<m)m=$15}END{print m+0}' $R/curve-vmstat-$tag-$n.txt)
  log "BURST $tag n=$n: codes=$codes wall_all=$(python3 -c "print(round($T1-$T0,1))")s min=${mn}s p50=${p50}s max=${mx}s | times: $times| si_peak=${sip}MB/s so_total=${sop}MB min_idle=${idl}% peak_wa=${wa}% | psi(ms) mem_full=$(( (m1-m0)/1000 )) cpu_some=$(( (c1-c0)/1000 )) io_full=$(( (i1-i0)/1000 ))"
}
# ---------- phase 1: curve at 50
log "phase 1: burst-size curve at $(ncells) cells; swap: $(swapon --show --noheadings | tr -s ' ' | tr '\n' ';')"
settle_all 50
for n in 1 2 4 8 10; do burst $n c50; settle_all 50; done
# ---------- phase 2: grow to 100
[ -f /swap2.img ] || { fallocate -l 40G /swap2.img; chmod 600 /swap2.img; mkswap -q /swap2.img; }; swapon /swap2.img 2>/dev/null
log "phase 2: growing to 100 cells; swap: $(swapon --show --noheadings | tr -s ' ' | tr '\n' ';')"
i=50; while [ $i -le 99 ]; do j=$((i+4)); [ $j -gt 99 ] && j=99
  for b in $(seq $i $j); do
    fleetd cells create -name v$b -port $((22000+b)) -hook-port $((22900+b)) -ingress-url $INGRESS -telegram-token-file $TOKF -idle 45s -tier pause >/dev/null 2>&1 || log "create v$b failed"
    python3 - <<PY
import json; p="/var/lib/fleetd/cells/v$b/state/openclaw.json"; d=json.load(open(p)); d["channels"]["telegram"]["enabled"]=False; json.dump(d,open(p,"w"),indent=2)
PY
    docker restart v$b >/dev/null 2>&1
  done
  for b in $(seq $i $j); do for k in $(seq 1 90); do curl -s -o /dev/null -m 2 -w "%{http_code}" http://127.0.0.1:$((22000+b))/health | grep -q 200 && break; sleep 2; done; done
  log "batch $i-$j up cells=$(ncells) free=$(free -m | awk 'NR==2{print $4}')MB swap_used=$(swapused)MB load=$(cut -d' ' -f1 /proc/loadavg) exited=$(docker ps -a --filter label=fleet.cell --filter status=exited --format x | wc -l)"
  if [ $j -eq 74 ] || [ $j -eq 99 ]; then settle_all $((j+1)); log "STEP $((j+1)) cells: used=$(free -m | awk 'NR==2{print $3}')MB free=$(free -m | awk 'NR==2{print $4}')MB swap_used=$(swapused)MB exited=$(docker ps -a --filter label=fleet.cell --filter status=exited --format x | wc -l) oom=$(dmesg 2>/dev/null | grep -c -i "out of memory")"; burst 10 c$((j+1)); settle_all $((j+1)); fi
  i=$((j+1))
done
curl -s -H "$A" $API/metrics | grep -E "^fleetd_(wakes_total|wake_failures_total|cells)" | tee -a $LOG
log "final: cells=$(ncells) paused=$(npaused) exited=$(docker ps -a --filter label=fleet.cell --filter status=exited --format x | wc -l) free=$(free -m | awk 'NR==2{print $4}')MB swap_used=$(swapused)MB"; echo RUN-DONE | tee -a $LOG
