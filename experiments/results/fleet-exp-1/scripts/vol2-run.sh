#!/bin/bash
# Burst test repeat: 50 cells hibernated with the cold set on an NVMe swapfile (no zram), 10 signed wakes at once.
R=/home/ops/results; LOG=$R/volume2-run.log; : > $LOG
export FLEETD_DATA=/var/lib/fleetd FLEETD_TOKEN=exp
log() { echo "$(date -u +%T) $*" | tee -a $LOG; }
A="Authorization: Bearer exp"; API=http://127.0.0.1:8080
psi() { awk -v k=$2 '$1==k{for(i=2;i<=NF;i++) if($i ~ /^total=/){sub("total=","",$i); print $i}}' /proc/pressure/$1; }
INGRESS=https://167-233-118-221.sslip.io; TOKF=/root/bot.token
# --- swap: NVMe file only
swapoff /dev/zram0 2>/dev/null; echo 1 > /sys/block/zram0/reset 2>/dev/null; losetup -D 2>/dev/null; swapon /swap.img 2>/dev/null
log "swap: $(swapon --show --noheadings | tr -s ' ' | tr '\n' ';')"
# --- create v1..v49 (tgw exists) in batches of 5, telegram channel disabled in the 49
i=1; while [ $i -le 49 ]; do j=$((i+4)); [ $j -gt 49 ] && j=49
  for b in $(seq $i $j); do
    fleetd cells create -name v$b -port $((22000+b)) -hook-port $((22900+b)) -ingress-url $INGRESS -telegram-token-file $TOKF -idle 45s -tier pause >/dev/null 2>&1 || log "create v$b failed"
    python3 - <<PY
import json; p="/var/lib/fleetd/cells/v$b/state/openclaw.json"; d=json.load(open(p)); d["channels"]["telegram"]["enabled"]=False; json.dump(d,open(p,"w"),indent=2)
PY
    docker restart v$b >/dev/null 2>&1
  done
  for b in $(seq $i $j); do for k in $(seq 1 90); do curl -s -o /dev/null -m 2 -w "%{http_code}" http://127.0.0.1:$((22000+b))/health | grep -q 200 && break; sleep 2; done; done
  log "batch $i-$j up free=$(free -m | awk 'NR==2{print $4}')MB"; i=$((j+1))
done
log "all cells up; waiting for pause + reclaim of all 50"
rc() { curl -s -H "$A" $API/metrics | awk '/^fleetd_reclaims_total/{print $2}'; }
for k in $(seq 1 90); do n=$(docker ps --filter label=fleet.cell --filter status=paused --format x | wc -l); sw=$(awk '/SwapTotal/{t=$2}/SwapFree/{f=$2}END{print int((t-f)/1024)}' /proc/meminfo); [ "$n" -ge 50 ] && [ "$sw" -ge 25000 ] && break; sleep 10; done
sleep 30
log "paused=$(docker ps --filter label=fleet.cell --filter status=paused --format x | wc -l) swap_used=$(awk '/SwapTotal/{t=$2}/SwapFree/{f=$2}END{print int((t-f)/1024)}' /proc/meminfo)MB used=$(free -m | awk 'NR==2{print $3}')MB free=$(free -m | awk 'NR==2{print $4}')MB avail=$(free -m | awk 'NR==2{print $7}')MB load=$(cut -d' ' -f1-3 /proc/loadavg)"
# --- burst: 10 signed updates at once
m0=$(psi memory full); s0=$(psi memory some); c0=$(psi cpu some); i0=$(psi io full)
vmstat 1 > $R/volume2-vmstat.txt & VP=$!
targets="tgw v1 v2 v3 v4 v5 v6 v7 v8 v9"; T0=$(date +%s.%N)
for c in $targets; do
  ( sec=$(cat /var/lib/fleetd/cells/$c/secrets/telegram-webhook-secret); s=$(date +%s.%N)
    code=$(curl -s -o /dev/null -m 120 -w "%{http_code}" -X POST -H "Content-Type: application/json" -H "X-Telegram-Bot-Api-Secret-Token: $sec" -d "{\"update_id\":$RANDOM,\"message\":{\"message_id\":1,\"date\":$(date +%s),\"chat\":{\"id\":1,\"type\":\"private\"},\"text\":\"volume\"}}" $INGRESS/hook/$c/telegram-webhook)
    e=$(date +%s.%N); echo "$(date -u +%T) $c $code $(python3 -c "print(round(($e-$s)*1000))") ms (started +$(python3 -c "print(round(($s-$T0)*1000))") ms)" | tee -a $LOG ) &
done; wait
sleep 3; kill $VP 2>/dev/null
m1=$(psi memory full); s1=$(psi memory some); c1=$(psi cpu some); i1=$(psi io full)
log "psi(ms) mem_full=$(( (m1-m0)/1000 )) mem_some=$(( (s1-s0)/1000 )) cpu_some=$(( (c1-c0)/1000 )) io_full=$(( (i1-i0)/1000 ))"
log "vmstat: si peak=$(awk 'NR>2{if($7>m)m=$7}END{print m+0}' $R/volume2-vmstat.txt)K/s sum=$(awk 'NR>2{s+=$7}END{print s+0}' $R/volume2-vmstat.txt)K so sum=$(awk 'NR>2{s+=$8}END{print s+0}' $R/volume2-vmstat.txt)K min_idle=$(awk 'NR>2{if(m==""||$15<m)m=$15}END{print m+0}' $R/volume2-vmstat.txt)% peak_wa=$(awk 'NR>2{if($16>m)m=$16}END{print m+0}' $R/volume2-vmstat.txt)%"
log "--- daemon"
sudo journalctl -u fleetd-exp --no-pager --since "-3min" | grep -E "woke|prefetched|WARN|wake failed" | cut -c40-170 | tee -a $LOG | tail -3 >/dev/null
log "still paused: $(docker ps --filter label=fleet.cell --filter status=paused --format x | wc -l)  running: $(docker ps --filter label=fleet.cell --filter status=running --format x | wc -l)  exited: $(docker ps -a --filter label=fleet.cell --filter status=exited --format x | wc -l)"
curl -s -H "$A" $API/metrics | grep -E "^fleetd_(wakes_total|wake_failures_total|cells)" | tee -a $LOG
log "free=$(free -m | awk 'NR==2{print $4}')MB load=$(cut -d' ' -f1-3 /proc/loadavg)"; echo RUN-DONE | tee -a $LOG
