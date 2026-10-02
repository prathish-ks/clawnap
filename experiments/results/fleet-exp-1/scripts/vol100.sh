#!/bin/bash
# Grow to 100 cells on the swapfile store with the current build (pipelined page-in, headroom 3 GB, settle 1 s); burst 10 cold, then 10 warm.
R=/home/ops/results; LOG=$R/vol100.log; : > $LOG
export FLEETD_DATA=/var/lib/fleetd FLEETD_TOKEN=exp
log() { echo "$(date -u +%T) $*" | tee -a $LOG; }
A="Authorization: Bearer exp"; API=http://127.0.0.1:8080; INGRESS=https://167-233-118-221.sslip.io; TOKF=/root/bot.token
psi() { awk -v k=$2 '$1==k{for(i=2;i<=NF;i++) if($i ~ /^total=/){sub("total=","",$i); print $i}}' /proc/pressure/$1; }
npaused() { docker ps --filter label=fleet.cell --filter status=paused --format x | wc -l; }
ncells() { docker ps -a --filter label=fleet.cell --format x | wc -l; }
freemb() { free -m | awk 'NR==2{print $4}'; }
availmb() { free -m | awk 'NR==2{print $7}'; }
swapused() { awk '/SwapTotal/{t=$2}/SwapFree/{f=$2}END{print int((t-f)/1024)}' /proc/meminfo; }
settle_all() { local want=$1; for k in $(seq 1 120); do [ "$(npaused)" -ge "$want" ] && break; sleep 5; done; sleep 40
  log "settled: paused=$(npaused)/$want free=$(freemb)MB avail=$(availmb)MB swap_used=$(swapused)MB load=$(cut -d' ' -f1 /proc/loadavg)"; }
burst() { local n=$1; local tag=$2; local T0 m0 c0 i0 m1 c1 i1 pids=""
  m0=$(psi memory full); c0=$(psi cpu some); i0=$(psi io full); T0=$(date +%s.%N)
  for c in $(seq 1 $n); do ( s=$(date +%s.%N); code=$(curl -s -o /dev/null -m 300 -w "%{http_code}" -X POST -H "$A" $API/wake/v$c); e=$(date +%s.%N); echo "v$c $code $(python3 -c "print(round($e-$s,2))")" ) > $R/v100-$c.txt & pids="$pids $!"; done
  wait $pids; local T1=$(date +%s.%N); sleep 2
  m1=$(psi memory full); c1=$(psi cpu some); i1=$(psi io full)
  local times=$(for c in $(seq 1 $n); do awk '{print $3}' $R/v100-$c.txt; done | sort -n | tr '\n' ' '); local codes=$(for c in $(seq 1 $n); do awk '{print $2}' $R/v100-$c.txt; done | sort | uniq -c | tr -s ' ' | tr '\n' ';')
  local p50=$(echo $times | tr ' ' '\n' | awk '{a[NR]=$1}END{print a[int((NR+1)/2)]}'); local mx=$(echo $times | tr ' ' '\n' | tail -1); local mn=$(echo $times | tr ' ' '\n' | head -1)
  log "BURST $tag n=$n: codes=$codes wall_all=$(python3 -c "print(round($T1-$T0,1))")s min=${mn}s p50=${p50}s max=${mx}s | times: $times| psi(ms) mem_full=$(( (m1-m0)/1000 )) cpu_some=$(( (c1-c0)/1000 )) io_full=$(( (i1-i0)/1000 ))"; }
# --- swap: second file
[ -f /swap2.img ] || { fallocate -l 40G /swap2.img && chmod 600 /swap2.img && mkswap -q /swap2.img; }; swapon -p 100 /swap2.img 2>/dev/null
log "swap: $(swapon --show --noheadings | tr -s ' ' | tr '\n' ';') root_free=$(df -h / | awk 'NR==2{print $4}') daemon: $(systemctl show -p ExecStart --value fleetd | grep -o -- '-headroom-mib [0-9]*\|-thaw-settle [0-9a-z]*\|-wake-concurrency [0-9]*' | tr '\n' ' ')"
# --- create v50..v99 in batches of 5; wait health then free RAM > 3 GB (headroom reclaim does the rest)
i=${START:-50}; while [ $i -le 99 ]; do j=$((i+4)); [ $j -gt 99 ] && j=99
  for b in $(seq $i $j); do
    fleetd cells create -name v$b -port $((22000+b)) -hook-port $((22900+b)) -ingress-url $INGRESS -telegram-token-file $TOKF -idle 45s -tier pause >/dev/null 2>&1 || log "create v$b failed"
    python3 - <<PY
import json; p="/var/lib/fleetd/cells/v$b/state/openclaw.json"; d=json.load(open(p)); d["channels"]["telegram"]["enabled"]=False; json.dump(d,open(p,"w"),indent=2)
PY
    docker restart v$b >/dev/null 2>&1
  done
  for b in $(seq $i $j); do for k in $(seq 1 90); do curl -s -o /dev/null -m 2 -w "%{http_code}" http://127.0.0.1:$((22000+b))/health | grep -q 200 && break; sleep 2; done; done
  for k in $(seq 1 12); do [ "$(availmb)" -gt 3200 ] && break; sleep 5; done
  log "batch $i-$j up cells=$(ncells) free=$(freemb)MB avail=$(availmb)MB swap_used=$(swapused)MB load=$(cut -d' ' -f1 /proc/loadavg) exited=$(docker ps -a --filter label=fleet.cell --filter status=exited --format x | wc -l)"
  i=$((j+1))
done
settle_all 100
log "STEP 100 cells: used=$(free -m | awk 'NR==2{print $3}')MB free=$(freemb)MB avail=$(availmb)MB swap_used=$(swapused)MB exited=$(docker ps -a --filter label=fleet.cell --filter status=exited --format x | wc -l) oom=$(dmesg 2>/dev/null | grep -c -i 'out of memory') resident_paused=$(fleetd cells list | python3 -c "import json,sys; print(sum(1 for c in json.load(sys.stdin) if c.get('phase')=='hibernated' and not c.get('swapped')))")"
# --- cold burst: make sure v1..v10 are reclaimed (headroom keeps recent ones resident otherwise)
for c in $(seq 1 10); do curl -s -o /dev/null -X POST -H "$A" $API/hibernate/v$c; done; sleep 5
cold=$(fleetd cells list | python3 -c "import json,sys; print(sum(1 for c in json.load(sys.stdin) if c['name'] in ['v%d'%i for i in range(1,11)] and c.get('swapped')))")
log "v1-v10 swapped=$cold/10 before the cold burst"
burst 10 cold100; sudo journalctl -u fleetd --no-pager --since "-2min" | grep -E "woke cell=v([1-9]|10) " | sed -E 's/.*woke //' | tr '\n' ';' | tee -a $LOG; echo | tee -a $LOG
# --- warm burst: the same ten pause again; with 3 GB headroom they should stay resident
settle_all 100
warm=$(fleetd cells list | python3 -c "import json,sys; print(sum(1 for c in json.load(sys.stdin) if c['name'] in ['v%d'%i for i in range(1,11)] and c.get('phase')=='hibernated' and not c.get('swapped')))")
log "v1-v10 resident=$warm/10 avail=$(availmb)MB before the warm burst"
burst 10 warm100; settle_all 100
curl -s -H "$A" $API/metrics | grep -E "^fleetd_(wake_failures_total|cells)" | tee -a $LOG
log "final: cells=$(ncells) paused=$(npaused) exited=$(docker ps -a --filter label=fleet.cell --filter status=exited --format x | wc -l) free=$(freemb)MB swap_used=$(swapused)MB"; echo RUN-DONE | tee -a $LOG
