#!/usr/bin/env bash
# Supervisor density test. Runs detached on the host; writes CSV + log as it goes.
set -u
R=/home/ops/results; mkdir -p $R; LOG=$R/supervisor-run.log; CSV=$R/supervisor-steps.csv
: > $LOG; echo "step,cells,healthy,running,paused,exited,used_mb,free_mb,zram_data,zram_compr,load,wake_p50_ms,wake_p95_ms,wake_n,wake_fail" > $CSV
export FLEETD_DATA=/root/.fleetd FLEETD_TOKEN=exp
systemctl stop fleetd-exp 2>/dev/null; pkill -f "fleetd serve" 2>/dev/null; sleep 1
systemd-run --unit=fleetd-exp --setenv=FLEETD_DATA=/root/.fleetd --setenv=FLEETD_TOKEN=exp \
  /usr/local/bin/fleetd serve -listen 127.0.0.1:8080 -interval 15s -reclaim-after 60s -reclaim-keep-mib 150 -prefetch-on-wake -max-pause -1s >/dev/null 2>&1
sleep 3; curl -s http://127.0.0.1:8080/healthz >/dev/null && echo "daemon-up $(date -u +%T)" | tee -a $LOG || { echo "daemon failed"; journalctl -u fleetd-exp --no-pager -n 5; exit 1; }
C() { curl -s -m 240 -H "Authorization: Bearer exp" "$@"; }
for target in 10 20 30 35 40 45 50; do
  from=$(fleetd cells list | grep -c '"name"')
  T0=$(date +%s)
  # boot in batches of 5: a batch must be healthy before the next starts, so a
  # step cannot exhaust memory or trip OpenClaw's startup lease under a boot storm
  i=$((from+1)); while [ $i -le $target ]; do
    j=$((i+4)); [ $j -gt $target ] && j=$target
    for b in $(seq $i $j); do fleetd cells create -name s$b -port $((20000+b)) -idle 45s -tier pause -state-root /srv/fleet/cells >/dev/null 2>&1 || echo "create s$b failed" | tee -a $LOG; done
    for b in $(seq $i $j); do for k in $(seq 1 90); do curl -s -o /dev/null -m 2 -w "%{http_code}" http://127.0.0.1:$((20000+b))/health | grep -q 200 && break; sleep 2; done; done
    i=$((j+1))
  done
  echo "step $target: $((target-from)) cells booted in $(( $(date +%s)-T0 ))s" | tee -a $LOG
  sleep 150
  ws=(); wf=0; n=0
  for i in $(shuf -i 1-$target -n 5); do
    [ "$(docker inspect -f '{{.State.Status}}' s$i 2>/dev/null)" = paused ] || continue
    out=$(C -o /dev/null -w "%{http_code} %{time_total}" -X POST http://127.0.0.1:8080/wake/s$i); code=${out%% *}; t=${out##* }
    ms=$(python3 -c "print(int(float('$t')*1000))"); ws+=($ms); n=$((n+1)); [ "$code" = 200 ] || wf=$((wf+1))
    echo "  wake s$i -> $code ${ms}ms" >> $LOG
  done
  p50=""; p95=""; if [ $n -gt 0 ]; then sorted=$(printf "%s\n" "${ws[@]}" | sort -n); p50=$(echo "$sorted" | awk '{a[NR]=$1} END {print a[int((NR+1)/2)]}'); p95=$(echo "$sorted" | tail -1); fi
  sleep 30
  ok=0; for i in $(seq 1 $target); do curl -s -o /dev/null -m 3 -w "%{http_code}" http://127.0.0.1:$((20000+i))/health | grep -q 200 && ok=$((ok+1)); done
  paused=$(docker ps --filter label=fleet.cell --filter status=paused --format x | wc -l); running=$(docker ps --filter label=fleet.cell --filter status=running --format x | wc -l); exited=$(docker ps -a --filter label=fleet.cell --filter status=exited --format x | wc -l)
  line="$target,$target,$((ok+paused)),$running,$paused,$exited,$(free -m | awk 'NR==2{print $3}'),$(free -m | awk 'NR==2{print $4}'),$(zramctl --noheadings -o DATA | head -1 | tr -d ' '),$(zramctl --noheadings -o COMPR | head -1 | tr -d ' '),$(cut -d' ' -f1 /proc/loadavg),$p50,$p95,$n,$wf"
  echo "$line" >> $CSV; echo "$line" | tee -a $LOG
  [ $exited -gt 2 ] && { echo "ceiling: exits at $target" | tee -a $LOG; break; }
done
echo "=== final $(date -u +%T)" | tee -a $LOG; free -m | sed -n 2,3p | tee -a $LOG; zramctl | tail -1 | tee -a $LOG
C http://127.0.0.1:8080/metrics | grep -E "^fleetd_(wakes_total|wake_failures_total|reclaims_total|reclaimed_bytes_total|hibernates_total|cells)" | tee -a $LOG
journalctl -u fleetd-exp --no-pager -n 8 | cut -c1-170 | tee -a $LOG
echo RUN-DONE | tee -a $LOG
