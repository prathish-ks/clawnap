#!/bin/bash
# Compressed swap (ZFS zvol, lz4): single-cell cycles on tgw, migrate all cells' cold pages onto the zvol, drop the raw swapfiles, burst 10 at 100.
R=/home/ops/results; LOG=$R/zfs-run.log; : > $LOG
export FLEETD_DATA=/var/lib/fleetd FLEETD_TOKEN=exp
log() { echo "$(date -u +%T) $*" | tee -a $LOG; }
A="Authorization: Bearer exp"; API=http://127.0.0.1:8080
psi() { awk -v k=$2 '$1==k{for(i=2;i<=NF;i++) if($i ~ /^total=/){sub("total=","",$i); print $i}}' /proc/pressure/$1; }
npaused() { docker ps --filter label=fleet.cell --filter status=paused --format x | wc -l; }
swapused() { awk '/SwapTotal/{t=$2}/SwapFree/{f=$2}END{print int((t-f)/1024)}' /proc/meminfo; }
zst() { echo "zvol_used=$(zfs get -H -o value used swappool/swap) ratio=$(zfs get -H -o value compressratio swappool/swap) pool_alloc=$(zpool list -H -o alloc swappool) files: $(swapon --show --noheadings | awk '/img/{printf "%s=%s ",$1,$4}')"; }
settle_all() { local want=$1; for k in $(seq 1 120); do [ "$(npaused)" -ge "$want" ] && break; sleep 5; done; sleep 40
  log "settled: paused=$(npaused)/$want free=$(free -m | awk 'NR==2{print $4}')MB load=$(cut -d' ' -f1 /proc/loadavg) $(zst)"; }
burst() { local n=$1; local tag=$2; local T0 m0 c0 i0 m1 c1 i1 pids=""
  m0=$(psi memory full); c0=$(psi cpu some); i0=$(psi io full); vmstat 1 > $R/zfs-vmstat-$tag.txt & local VP=$!; T0=$(date +%s.%N)
  for c in $(seq 1 $n); do ( s=$(date +%s.%N); code=$(curl -s -o /dev/null -m 300 -w "%{http_code}" -X POST -H "$A" $API/wake/v$c); e=$(date +%s.%N); echo "v$c $code $(python3 -c "print(round($e-$s,2))")" ) > $R/zfs-b-$c.txt & pids="$pids $!"; done
  wait $pids; local T1=$(date +%s.%N); sleep 2; kill $VP 2>/dev/null
  m1=$(psi memory full); c1=$(psi cpu some); i1=$(psi io full)
  local times=$(for c in $(seq 1 $n); do awk '{print $3}' $R/zfs-b-$c.txt; done | sort -n | tr '\n' ' '); local codes=$(for c in $(seq 1 $n); do awk '{print $2}' $R/zfs-b-$c.txt; done | sort | uniq -c | tr -s ' ' | tr '\n' ';')
  local p50=$(echo $times | tr ' ' '\n' | awk '{a[NR]=$1}END{print a[int((NR+1)/2)]}'); local mx=$(echo $times | tr ' ' '\n' | tail -1); local mn=$(echo $times | tr ' ' '\n' | head -1)
  local sip=$(awk 'NR>2{if($7>m)m=$7}END{print int(m/1024)}' $R/zfs-vmstat-$tag.txt); local bip=$(awk 'NR>2{if($9>m)m=$9}END{print int(m/1024)}' $R/zfs-vmstat-$tag.txt); local idl=$(awk 'NR>2{if(m==""||$15<m)m=$15}END{print m+0}' $R/zfs-vmstat-$tag.txt); local wa=$(awk 'NR>2{if($16>m)m=$16}END{print m+0}' $R/zfs-vmstat-$tag.txt)
  log "BURST $tag n=$n: codes=$codes wall_all=$(python3 -c "print(round($T1-$T0,1))")s min=${mn}s p50=${p50}s max=${mx}s | times: $times| si_peak=${sip}MB/s bi_peak=${bip}MB/s min_idle=${idl}% peak_wa=${wa}% | psi(ms) mem_full=$(( (m1-m0)/1000 )) cpu_some=$(( (c1-c0)/1000 )) io_full=$(( (i1-i0)/1000 ))"
  # per-cell daemon timings
  sudo journalctl -u fleetd-exp --no-pager --since "-2min" | grep -E "woke cell=v[0-9]+ " | sed -E 's/.*woke //' | tr '\n' ';' | tee -a $LOG; echo | tee -a $LOG; }
# ---- 1. single-cell cycles on tgw (pages land on the zvol: highest priority)
log "swap: $(swapon --show --noheadings | tr -s ' ' | tr '\n' ';') arc_max=$(cat /sys/module/zfs/parameters/zfs_arc_max)"
/home/ops/disk-tier.sh zfs 3; log "after tgw cycles: $(zst)"
# ---- 2. migrate v1..v99 onto the zvol in batches of 8 (wake → idle → pause → reclaim)
i=1; while [ $i -le 99 ]; do j=$((i+7)); [ $j -gt 99 ] && j=99
  for c in $(seq $i $j); do curl -s -o /dev/null -m 300 -X POST -H "$A" $API/wake/v$c & done; wait
  settle_all 100; log "migrated v$i-v$j"
  for f in /swap2.img /swap.img; do u=$(swapon --show --noheadings --bytes | awk -v f=$f '$1==f{print $4}'); if [ -n "$u" ] && [ "$u" -lt 300000000 ]; then swapoff $f && rm -f $f && log "dropped $f (had $((u/1048576)) MiB left); root free=$(df -h / | awk 'NR==2{print $4}')"; fi; done
  i=$((j+1))
done
log "migration done: $(zst) root free=$(df -h / | awk 'NR==2{print $4}') free_ram=$(free -m | awk 'NR==2{print $4}')MB"
# ---- 3. burst 10 at 100 cells, twice
burst 10 z100a; settle_all 100; burst 10 z100b; settle_all 100
curl -s -H "$A" $API/metrics | grep -E "^fleetd_(wake_failures_total|cells)" | tee -a $LOG
log "final: paused=$(npaused) exited=$(docker ps -a --filter label=fleet.cell --filter status=exited --format x | wc -l) $(zst) arc=$(awk '/^size/{print int($3/1048576)"MiB"}' /proc/spl/kstat/zfs/arcstats)"; echo RUN-DONE | tee -a $LOG
