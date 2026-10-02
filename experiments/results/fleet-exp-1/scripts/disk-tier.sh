#!/bin/bash
# usage: disk-tier.sh <label> <cycles> [wb]   — cycles hibernate→reclaim→(writeback)→wake on cell tgw via the daemon API
L=$1; N=${2:-3}; WB=${3:-}
R=/home/ops/results; mkdir -p $R; OUT=$R/disk-tier-$L.log
A="Authorization: Bearer exp"; API=http://127.0.0.1:8080
ID=$(docker inspect -f '{{.Id}}' tgw); CG=$(find /sys/fs/cgroup -maxdepth 4 -type d -name "*$ID*" | head -1)
psi() { awk -v k=$2 '$1==k{for(i=2;i<=NF;i++) if($i ~ /^total=/){sub("total=","",$i); print $i}}' /proc/pressure/$1; }
cur() { echo $(( $(cat $CG/memory.current) / 1048576 )); }
swp() { echo $(( $(cat $CG/memory.swap.current 2>/dev/null || echo 0) / 1048576 )); }
echo "== $L cycles=$N wb=$WB $(date -u +%T) swap: $(swapon --show --noheadings | tr '\n' ';')" | tee -a $OUT
for c in $(seq 1 $N); do
  T0=$(date -u +"%Y-%m-%d %H:%M:%S")
  # ensure running + healthy, settle
  curl -s -o /dev/null -X POST -H "$A" $API/wake/tgw; for k in $(seq 1 60); do curl -s -m 2 -o /dev/null -w "%{http_code}" http://127.0.0.1:21001/health | grep -q 200 && break; sleep 1; done; sleep 15
  before="cur=$(cur) swap=$(swp)"
  r0=$(curl -s -H "$A" $API/metrics | awk '/^fleetd_reclaims_total/{print $2}')
  curl -s -o /dev/null -X POST -H "$A" $API/hibernate/tgw
  for k in $(seq 1 90); do r1=$(curl -s -H "$A" $API/metrics | awk '/^fleetd_reclaims_total/{print $2}'); [ "${r1:-0}" -gt "${r0:-0}" ] && break; sleep 1; done; sleep 5
  after="cur=$(cur) swap=$(swp)"
  wbinfo=""
  if [ -n "$WB" ]; then
    mm0=$(awk '{print $3}' /sys/block/zram0/mm_stat); echo all > /sys/block/zram0/idle; sleep 2; echo idle > /sys/block/zram0/writeback; sleep 3
    mm1=$(awk '{print $3}' /sys/block/zram0/mm_stat); bd=$(cat /sys/block/zram0/bd_stat)
    wbinfo="zram_mem_before=$((mm0/1048576))MiB after=$((mm1/1048576))MiB bd_stat(pages: count reads writes)=$bd"
  fi
  sleep 10
  m0=$(psi memory full); s0=$(psi memory some); c0=$(psi cpu some); i0=$(psi io full)
  vmstat 1 > /tmp/vm.$c & VP=$!
  ws=$(date +%s.%N); curl -s -o /dev/null -m 180 -X POST -H "$A" $API/wake/tgw; we=$(date +%s.%N)
  for k in $(seq 1 120); do curl -s -m 2 -o /dev/null -w "%{http_code}" http://127.0.0.1:21001/health | grep -q 200 && break; sleep 0.2; done; he=$(date +%s.%N)
  sleep 2; kill $VP 2>/dev/null
  m1=$(psi memory full); s1=$(psi memory some); c1=$(psi cpu some); i1=$(psi io full)
  woke=$(sudo journalctl -u fleetd --no-pager --since "$T0" | grep -E "woke cell=tgw" | tail -1 | sed 's/.*woke //')
  pf=$(sudo journalctl -u fleetd --no-pager --since "$T0" | grep -E "prefetched cell=tgw" | tail -1 | grep -o -E "swap_before_mib=[0-9]+|took=[0-9.]+m?s" | tr '\n' ' ')
  peak_si=$(awk 'NR>2{if($7>m)m=$7}END{print m+0}' /tmp/vm.$c); sum_si=$(awk 'NR>2{s+=$7}END{print s+0}' /tmp/vm.$c); peak_wa=$(awk 'NR>2{if($16>m)m=$16}END{print m+0}' /tmp/vm.$c); min_id=$(awk 'NR>2{if(m==""||$15<m)m=$15}END{print m+0}' /tmp/vm.$c)
  printf "%s cycle %d: hib %s -> %s | %s | wake_api=%.2fs health=%.2fs | %s | prefetch %s| psi(ms) mem_full=%d mem_some=%d cpu_some=%d io_full=%d | vmstat si peak=%dK/s sum=%dK min_idle=%d%% peak_wa=%d%%\n" \
    "$(date -u +%T)" $c "$before" "$after" "$wbinfo" $(echo "$we-$ws" | bc) $(echo "$he-$ws" | bc) "$woke" "$pf" $(( (m1-m0)/1000 )) $(( (s1-s0)/1000 )) $(( (c1-c0)/1000 )) $(( (i1-i0)/1000 )) $peak_si $sum_si $min_id $peak_wa | tee -a $OUT
done
echo "RUN-DONE $L" | tee -a $OUT
