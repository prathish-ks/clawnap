#!/bin/bash
# After a reboot: start tgw + v1..v99 in paced batches (health, then available RAM > 3.2 GB), let the daemon hibernate them.
R=/home/ops/results; LOG=$R/boot100.log; : > $LOG
log() { echo "$(date -u +%T) $*" | tee -a $LOG; }
availmb() { free -m | awk 'NR==2{print $7}'; }
docker start tgw >/dev/null 2>&1
i=1; while [ $i -le 99 ]; do j=$((i+4)); [ $j -gt 99 ] && j=99
  for b in $(seq $i $j); do docker start v$b >/dev/null 2>&1; done
  for b in $(seq $i $j); do for k in $(seq 1 90); do curl -s -o /dev/null -m 2 -w "%{http_code}" http://127.0.0.1:$((22000+b))/health | grep -q 200 && break; docker inspect -f "{{.State.Paused}}" v$b | grep -q true && break; sleep 2; done; done
  for k in $(seq 1 12); do [ "$(availmb)" -gt 3200 ] && break; sleep 5; done
  log "batch $i-$j up avail=$(availmb)MB load=$(cut -d' ' -f1 /proc/loadavg)"; i=$((j+1))
done
for k in $(seq 1 60); do [ "$(docker ps --filter label=fleet.cell --filter status=paused --format x | wc -l)" -ge 100 ] && break; sleep 5; done; sleep 30
log "booted: paused=$(docker ps --filter label=fleet.cell --filter status=paused --format x | wc -l) exited=$(docker ps -a --filter label=fleet.cell --filter status=exited --format x | wc -l) avail=$(availmb)MB swap_used=$(awk '/SwapTotal/{t=$2}/SwapFree/{f=$2}END{print int((t-f)/1024)}' /proc/meminfo)MB"; echo BOOT-DONE | tee -a $LOG
