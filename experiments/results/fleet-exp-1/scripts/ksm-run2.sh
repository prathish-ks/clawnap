#!/bin/bash
# Resident tier at a 350 MiB cap, then KSM dedup on it, then bursts against each state; every state logs RESIDENT / HEADROOM / BURST.
R=/home/ops/results; LOG=$R/ksm-run2.log; : > $LOG
export FLEETD_DATA=/var/lib/fleetd FLEETD_TOKEN=exp
log() { echo "$(date -u +%T) $*" | tee -a $LOG; }
A="Authorization: Bearer exp"; API=http://127.0.0.1:8080; IMG=ghcr.io/openclaw/openclaw:latest
cg() { id=$(docker inspect -f '{{.Id}}' $1); find /sys/fs/cgroup -maxdepth 4 -type d -name "*$id*" | head -1; }
psi() { awk -v k=$2 '$1==k{for(i=2;i<=NF;i++) if($i ~ /^total=/){sub("total=","",$i); print $i}}' /proc/pressure/$1; }
npaused() { docker ps --filter label=fleet.cell --filter status=paused --format x | wc -l; }
avail() { free -m | awk 'NR==2{print $7}'; }
ksm() { echo "ksm(run=$(cat /sys/kernel/mm/ksm/run) shared=$(( $(cat /sys/kernel/mm/ksm/pages_shared)*4/1024 ))MB sharing=$(( $(cat /sys/kernel/mm/ksm/pages_sharing)*4/1024 ))MB saved≈$(( ($(cat /sys/kernel/mm/ksm/pages_sharing))*4/1024 ))MB unshared=$(( $(cat /sys/kernel/mm/ksm/pages_unshared)*4/1024 ))MB full_scans=$(cat /sys/kernel/mm/ksm/full_scans) profit=$(( $(cat /sys/kernel/mm/ksm/general_profit)/1048576 ))MB)"; }
resident() { # count cells that are hibernated but not swapped-out, plus the sum of their resident bytes
  fleetd cells list | python3 -c "
import json,sys; cs=json.load(sys.stdin); r=[c['name'] for c in cs if c.get('phase')=='hibernated' and not c.get('swapped')]; print(len(r), ' '.join(r[:25]))"; }
sumres() { local t=0; for c in "$@"; do t=$((t + $(cat $(cg $c)/memory.current)/1048576)); done; echo "${t}M (per cell $((t/$#))M)"; }
state() { local tag=$1; shift; read -r n names <<<"$(resident)"; log "STATE $tag: RESIDENT=$n [$names] resident_sum=$( [ $n -gt 0 ] && sumres $names || echo 0 ) | HEADROOM avail=$(avail)MB used=$(free -m | awk 'NR==2{print $3}')MB free=$(free -m | awk 'NR==2{print $4}')MB swap_used=$(awk '/SwapTotal/{t=$2}/SwapFree/{f=$2}END{print int((t-f)/1024)}' /proc/meminfo)MB | $(ksm) | paused=$(npaused)"; }
burst() { local tag=$1; shift; local cells="$@"; local m0 c0 i0 m1 c1 i1 T0 T1 pids="" a0=$(avail)
  m0=$(psi memory full); c0=$(psi cpu some); i0=$(psi io full); T0=$(date +%s.%N)
  for c in $cells; do ( s=$(date +%s.%N); code=$(curl -s -o /dev/null -m 300 -w "%{http_code}" -X POST -H "$A" $API/wake/$c); e=$(date +%s.%N); echo "$c $code $(python3 -c "print(round($e-$s,2))")" ) > $R/kb-$c.txt & pids="$pids $!"; done
  wait $pids; T1=$(date +%s.%N); sleep 2; m1=$(psi memory full); c1=$(psi cpu some); i1=$(psi io full)
  local times=$(for c in $cells; do awk '{print $3}' $R/kb-$c.txt; done | sort -n | tr '\n' ' '); local codes=$(for c in $cells; do awk '{print $2}' $R/kb-$c.txt; done | sort | uniq -c | tr -s ' ' | tr '\n' ';')
  local p50=$(echo $times | tr ' ' '\n' | awk '{a[NR]=$1}END{print a[int((NR+1)/2)]}')
  log "BURST $tag: codes=$codes min=$(echo $times | cut -d' ' -f1)s p50=${p50}s max=$(echo $times | awk '{print $NF}')s | times: $times| psi(ms) mem_full=$(( (m1-m0)/1000 )) cpu_some=$(( (c1-c0)/1000 )) io_full=$(( (i1-i0)/1000 )) | avail before=${a0}MB after=$(avail)MB"; }
setpolicy() { mkdir -p /etc/systemd/system/fleetd.service.d
  printf '[Service]\nExecStart=\nExecStart=/usr/local/bin/fleetd serve -listen 127.0.0.1:8080 -interval 5s -reclaim-after 30m -reclaim-keep-mib 150 -wake-concurrency 2 -max-pause -1s %s\n' "$1" > /etc/systemd/system/fleetd.service.d/override.conf
  systemctl daemon-reload; systemctl restart fleetd; sleep 3; log "fleetd policy: $1"; }
waitpaused() { for k in $(seq 1 60); do [ "$(npaused)" -ge 100 ] && break; sleep 5; done; sleep 20; }
cap() { for c in "$@"; do echo ${CAP:-350M} > $(cg $c)/memory.high; done; }
uncap() { for c in "$@"; do echo max > $(cg $c)/memory.high; done; }
R20="$(seq -s ' ' -f v%g 1 20)"; W10="$(seq -s ' ' -f v%g 1 10)"; C1="$(seq -s ' ' -f v%g 21 30)"; C2="$(seq -s ' ' -f v%g 31 40)"; C3="$(seq -s ' ' -f v%g 41 50)"
echo 0 > /sys/kernel/mm/ksm/run
setpolicy "-headroom-mib 2048 -prefetch-on-wake"
# ---------- C. KSM: recreate the 20 resident cells with the mergeable wrapper, cap, let KSM merge, then the same bursts
for c in $R20; do docker rm -f $c >/dev/null 2>&1
  n=${c#v}; docker run -d --name $c --label fleet.cell=$c --user 0:0 -e HOME=/home/node --cap-drop ALL --cap-add SYS_RESOURCE --cap-add SETUID --cap-add SETGID --security-opt no-new-privileges \
    --memory 1g --memory-swap 2g --pids-limit 512 --ulimit nofile=65536:65536 --log-opt max-size=10m --log-opt max-file=3 \
    -p 127.0.0.1:$((22000+n)):18789 -p 127.0.0.1:$((22900+n)):8787 -v /var/lib/fleetd/cells/$c/state:/home/node/.openclaw -v /var/lib/fleetd/cells/$c/auth:/home/node/.config/openclaw \
    -v /home/ops/ksmexec:/ksmexec:ro --entrypoint /ksmexec $IMG tini -s -- node openclaw.mjs gateway >/dev/null 2>&1 || log "recreate $c failed"
done
for c in $R20; do n=${c#v}; for k in $(seq 1 60); do curl -s -o /dev/null -m 2 -w "%{http_code}" http://127.0.0.1:$((22000+n))/health | grep -q 200 && break; sleep 2; done; done
merge=$(for c in $R20; do pid=$(docker inspect -f '{{.State.Pid}}' $c); grep -c "VmFlags:.*mm" /proc/$pid/smaps 2>/dev/null | head -1; done | awk '{s+=$1}END{print s}')
log "recreated 20 cells with ksmexec; mappings flagged mergeable across them: $merge"
cap $R20; waitpaused; sleep 30; state "C-20-recreated-capped (KSM off)"
echo 1 > /sys/kernel/mm/ksm/run; echo 0 > /sys/kernel/mm/ksm/sleep_millisecs; echo 20000 > /sys/kernel/mm/ksm/pages_to_scan
for k in $(seq 1 40); do [ "$(cat /sys/kernel/mm/ksm/full_scans)" -ge 3 ] && break; sleep 10; done
log "KSM scanned: full_scans=$(cat /sys/kernel/mm/ksm/full_scans) after ~$((k*10))s"; state "C-20-resident-KSM"
burst "C-warm-10 (resident, capped, KSM)" $W10; waitpaused; state "C-after-warm"
burst "C-cold-10 (disk, prefetch)" $C2; waitpaused; state "C-after-cold"
# ---------- D. lazy cold wake under a cap: no prefetch, memory.high 400 MiB on ten cold cells
setpolicy "-headroom-mib 2048"; CAP=400M cap $C3; sleep 5; state "D-before-lazy"
burst "D-cold-10 (disk, NO prefetch, cap 400M)" $C3; waitpaused; state "D-after-lazy"
# ---------- restore
uncap $R20 $C1 $C2 $C3; echo 2 > /sys/kernel/mm/ksm/run; echo 0 > /sys/kernel/mm/ksm/run; rm -rf /etc/systemd/system/fleetd.service.d; systemctl daemon-reload; systemctl restart fleetd; sleep 3
log "restored: fleetd $(systemctl is-active fleetd) shipped policy; KSM off; caps cleared; wake_failures=$(curl -s -H "$A" $API/metrics | awk '/^fleetd_wake_failures_total/{print $2}') exited=$(docker ps -a --filter label=fleet.cell --filter status=exited --format x | wc -l)"; echo KSM-DONE | tee -a $LOG
