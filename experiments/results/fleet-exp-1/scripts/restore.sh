#!/bin/bash
R=/home/ops/results; LOG=$R/restore.log; : > $LOG
log() { echo "$(date -u +%T) $*" | tee -a $LOG; }
freemb() { free -m | awk 'NR==2{print $4}'; }
rm -f /zpool.img; swapon -p 100 /swap.img 2>/dev/null; log "swap: $(swapon --show --noheadings | tr -s ' ') root_free=$(df -h / | awk 'NR==2{print $4}')"
systemctl stop fleetd-exp 2>/dev/null; pkill -x fleetd; sleep 1
systemd-run --unit=fleetd-exp --setenv=FLEETD_DATA=/var/lib/fleetd --setenv=FLEETD_TOKEN=exp /usr/local/bin/fleetd serve -listen 127.0.0.1:8080 -interval 5s -reclaim-after 20s -reclaim-keep-mib 150 -prefetch-on-wake -max-pause -1s >/dev/null 2>&1; sleep 2
docker start tgw >/dev/null 2>&1
i=1; while [ $i -le 49 ]; do j=$((i+4)); [ $j -gt 49 ] && j=49
  for b in $(seq $i $j); do docker start v$b >/dev/null 2>&1; done
  for b in $(seq $i $j); do for k in $(seq 1 90); do curl -s -o /dev/null -m 2 -w "%{http_code}" http://127.0.0.1:$((22000+b))/health | grep -q 200 && break; sleep 2; done; done
  for k in $(seq 1 60); do [ "$(freemb)" -gt 3000 ] && break; sleep 5; done
  log "batch $i-$j up free=$(freemb)MB load=$(cut -d' ' -f1 /proc/loadavg)"; i=$((j+1))
done
sleep 90; log "restored: paused=$(docker ps --filter label=fleet.cell --filter status=paused --format x | wc -l) exited=$(docker ps -a --filter label=fleet.cell --filter status=exited --format x | wc -l) free=$(freemb)MB swap_used=$(awk '/SwapTotal/{t=$2}/SwapFree/{f=$2}END{print int((t-f)/1024)}' /proc/meminfo)MB"; echo RESTORE-DONE | tee -a $LOG
