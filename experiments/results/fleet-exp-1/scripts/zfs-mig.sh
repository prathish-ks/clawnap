#!/bin/bash
R=/home/ops/results; LOG=$R/zfs-mig.log; : > $LOG
export FLEETD_DATA=/var/lib/fleetd FLEETD_TOKEN=exp
log() { echo "$(date -u +%T) $*" | tee -a $LOG; }
st() { echo "free=$(free -m | awk 'NR==2{print $4}')MB avail=$(free -m | awk 'NR==2{print $7}')MB load=$(cut -d' ' -f1 /proc/loadavg) zvol=$(zfs get -H -o value used swappool/swap) ratio=$(zfs get -H -o value compressratio swappool/swap) $(swapon --show --noheadings | awk '{printf "%s=%s ",$1,$4}')"; }
# 1. trim to 50 cells (tgw + v1..v49): remove v50..v99
for b in $(seq 50 99); do fleetd cells rm -name v$b >/dev/null 2>&1; done
docker rm -f $(for b in $(seq 50 99); do echo v$b; done) >/dev/null 2>&1
rm -rf /var/lib/fleetd/cells/v[5-9][0-9]
log "trimmed: cells=$(docker ps -a --filter label=fleet.cell --format x | wc -l) $(st)"
# 2. force the cold pages onto the zvol: swapoff the raw files one at a time (kernel re-swaps evicted pages to the highest-priority device)
for f in /swap2.img /swap.img; do
  log "swapoff $f start: $(st)"
  ( while sleep 15; do echo "$(date -u +%T)   … $(st)" >> $LOG; done ) & MON=$!
  s=$(date +%s); swapoff $f; rc=$?; e=$(date +%s); kill $MON 2>/dev/null
  log "swapoff $f rc=$rc took=$((e-s))s: $(st)"
  [ $rc -eq 0 ] && rm -f $f && log "removed $f root_free=$(df -h / | awk 'NR==2{print $4}')"
done
log "paused=$(docker ps --filter label=fleet.cell --filter status=paused --format x | wc -l) exited=$(docker ps -a --filter label=fleet.cell --filter status=exited --format x | wc -l) $(st)"
echo MIG-DONE | tee -a $LOG
