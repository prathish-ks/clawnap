#!/bin/bash
# Hypothesis: our 45 s network-idle pause cuts OpenClaw's post-thaw maintenance short, and the next thaw resumes it ("gateway still has active work").
# Test: give v90..v99 a 6-minute idle timer, wake them, let maintenance finish, let them pause and go cold, then burst them.
R=/home/ops/results; LOG=$R/idle5.log; : > $LOG
export FLEETD_DATA=/var/lib/fleetd FLEETD_TOKEN=exp
log() { echo "$(date -u +%T) $*" | tee -a $LOG; }
A="Authorization: Bearer exp"; API=http://127.0.0.1:8080
psi() { awk -v k=$2 '$1==k{for(i=2;i<=NF;i++) if($i ~ /^total=/){sub("total=","",$i); print $i}}' /proc/pressure/$1; }
npaused() { docker ps --filter label=fleet.cell --filter status=paused --format x | wc -l; }
avail() { free -m | awk 'NR==2{print $7}'; }
cpu() { for c in "$@"; do id=$(docker inspect -f '{{.Id}}' $c); d=$(find /sys/fs/cgroup -maxdepth 4 -type d -name "*$id*" | head -1); awk '/^usage_usec/{printf "%d ", $2/1000000}' $d/cpu.stat; done; }
python3 - <<'PY'
import json, fcntl
p="/var/lib/fleetd/cells.json"; lk=open(p+".lock","w"); fcntl.flock(lk, fcntl.LOCK_EX)
cs=json.load(open(p))
for c in cs:
    if c["name"] in ["v%d"%i for i in range(90,100)]: c["idle_after"]=360_000_000_000
json.dump(cs, open(p,"w")); fcntl.flock(lk, fcntl.LOCK_UN)
PY
log "idle timer for v90..v99 set to 6 min; cpu_s before: $(cpu $(seq -s ' ' -f v%g 90 99))"
for c in $(seq -f v%g 90 99); do curl -s -o /dev/null -m 300 -X POST -H "$A" $API/wake/$c & done; wait
log "woken; letting maintenance run. cpu_s at +30s: $(sleep 30; cpu $(seq -s ' ' -f v%g 90 99))"
sleep 60; log "cpu_s at +90s: $(cpu $(seq -s ' ' -f v%g 90 99))"; sleep 60; log "cpu_s at +150s: $(cpu $(seq -s ' ' -f v%g 90 99))"; sleep 60; log "cpu_s at +210s: $(cpu $(seq -s ' ' -f v%g 90 99))"
for k in $(seq 1 60); do [ "$(npaused)" -ge 100 ] && break; sleep 5; done; sleep 30
for k in $(seq 1 60); do n=$(fleetd cells list | python3 -c "
import json,sys; cs=json.load(sys.stdin); print(sum(1 for c in cs if c['name'] in ['v%d'%i for i in range(90,100)] and c.get('reclaimed_at','')>c.get('warm_at','')))"); [ "$n" -ge 10 ] && break; sleep 5; done
log "v90..v99 paused and cold again ($n/10 cold) avail=$(avail)MB"; sleep 30
m0=$(psi memory full); c0=$(psi cpu some); i0=$(psi io full); T0=$(date +%s.%N); pids=""
for c in $(seq -f v%g 90 99); do ( s=$(date +%s.%N); code=$(curl -s -o /dev/null -m 300 -w "%{http_code}" -X POST -H "$A" $API/wake/$c); e=$(date +%s.%N); echo "$c $code $(python3 -c "print(round($e-$s,2))")" ) > $R/i5-$c.txt & pids="$pids $!"; done
wait $pids; T1=$(date +%s.%N); sleep 2; m1=$(psi memory full); c1=$(psi cpu some); i1=$(psi io full)
times=$(for c in $(seq -f v%g 90 99); do awk '{print $3}' $R/i5-$c.txt; done | sort -n | tr '\n' ' '); p50=$(echo $times | tr ' ' '\n' | awk '{a[NR]=$1}END{print a[int((NR+1)/2)]}')
log "BURST cold-10 after maintenance completed: min=$(echo $times | cut -d' ' -f1)s p50=${p50}s max=$(echo $times | awk '{print $NF}')s | times: $times| psi(ms) mem_full=$(( (m1-m0)/1000 )) cpu_some=$(( (c1-c0)/1000 )) io_full=$(( (i1-i0)/1000 ))"
docker logs v95 --since 3m 2>&1 | grep -E "timing gap|active work|admission" | sed -E "s/^2026-09-30T//" | cut -c1-120 | head -4 | tee -a $LOG
python3 - <<'PY'
import json, fcntl
p="/var/lib/fleetd/cells.json"; lk=open(p+".lock","w"); fcntl.flock(lk, fcntl.LOCK_EX)
cs=json.load(open(p))
for c in cs:
    if c["name"] in ["v%d"%i for i in range(90,100)]: c["idle_after"]=45_000_000_000
json.dump(cs, open(p,"w")); fcntl.flock(lk, fcntl.LOCK_UN)
PY
log "idle timers restored"; echo I5-DONE | tee -a $LOG
