#!/bin/bash
# Compressed swap as the only swap device; cells started in paced batches; burst 10 at 50 cells, twice.
R=/home/ops/results; LOG=$R/zfs-run2.log; : > $LOG
export FLEETD_DATA=/var/lib/fleetd FLEETD_TOKEN=exp
log() { echo "$(date -u +%T) $*" | tee -a $LOG; }
A="Authorization: Bearer exp"; API=http://127.0.0.1:8080
psi() { awk -v k=$2 '$1==k{for(i=2;i<=NF;i++) if($i ~ /^total=/){sub("total=","",$i); print $i}}' /proc/pressure/$1; }
npaused() { docker ps --filter label=fleet.cell --filter status=paused --format x | wc -l; }
freemb() { free -m | awk 'NR==2{print $4}'; }
zst() { echo "zvol_used=$(zfs get -H -o value used swappool/swap) ratio=$(zfs get -H -o value compressratio swappool/swap) swap_used=$(awk '/SwapTotal/{t=$2}/SwapFree/{f=$2}END{print int((t-f)/1024)}' /proc/meminfo)MB arc=$(awk '/^size/{print int($3/1048576)}' /proc/spl/kstat/zfs/arcstats)MiB"; }
settle_all() { local want=$1; for k in $(seq 1 120); do [ "$(npaused)" -ge "$want" ] && break; sleep 5; done; sleep 40
  log "settled: paused=$(npaused)/$want free=$(freemb)MB load=$(cut -d' ' -f1 /proc/loadavg) $(zst)"; }
burst() { local n=$1; local tag=$2; local T0 m0 c0 i0 m1 c1 i1 pids=""
  m0=$(psi memory full); c0=$(psi cpu some); i0=$(psi io full); vmstat 1 > $R/zfs-vmstat-$tag.txt & local VP=$!; T0=$(date +%s.%N)
  for c in $(seq 1 $n); do ( s=$(date +%s.%N); code=$(curl -s -o /dev/null -m 300 -w "%{http_code}" -X POST -H "$A" $API/wake/v$c); e=$(date +%s.%N); echo "v$c $code $(python3 -c "print(round($e-$s,2))")" ) > $R/zfs-b-$c.txt & pids="$pids $!"; done
  wait $pids; local T1=$(date +%s.%N); sleep 2; kill $VP 2>/dev/null
  m1=$(psi memory full); c1=$(psi cpu some); i1=$(psi io full)
  local times=$(for c in $(seq 1 $n); do awk '{print $3}' $R/zfs-b-$c.txt; done | sort -n | tr '\n' ' '); local codes=$(for c in $(seq 1 $n); do awk '{print $2}' $R/zfs-b-$c.txt; done | sort | uniq -c | tr -s ' ' | tr '\n' ';')
  local p50=$(echo $times | tr ' ' '\n' | awk '{a[NR]=$1}END{print a[int((NR+1)/2)]}'); local mx=$(echo $times | tr ' ' '\n' | tail -1); local mn=$(echo $times | tr ' ' '\n' | head -1)
  local sip=$(awk 'NR>2{if($7>m)m=$7}END{print int(m/1024)}' $R/zfs-vmstat-$tag.txt); local bip=$(awk 'NR>2{if($9>m)m=$9}END{print int(m/1024)}' $R/zfs-vmstat-$tag.txt); local idl=$(awk 'NR>2{if(m==""||$15<m)m=$15}END{print m+0}' $R/zfs-vmstat-$tag.txt); local wa=$(awk 'NR>2{if($16>m)m=$16}END{print m+0}' $R/zfs-vmstat-$tag.txt)
  log "BURST $tag n=$n: codes=$codes wall_all=$(python3 -c "print(round($T1-$T0,1))")s min=${mn}s p50=${p50}s max=${mx}s | times: $times| si_peak=${sip}MB/s bi_peak=${bip}MB/s min_idle=${idl}% peak_wa=${wa}% | psi(ms) mem_full=$(( (m1-m0)/1000 )) cpu_some=$(( (c1-c0)/1000 )) io_full=$(( (i1-i0)/1000 ))"
  sudo journalctl -u fleetd-exp --no-pager --since "-2min" | grep -E "woke cell=v[0-9]+ " | sed -E 's/.*woke //' | tr '\n' ';' | tee -a $LOG; echo | tee -a $LOG; }
# ---- swap: zvol only
LD=$(losetup -j /zpool.img | cut -d: -f1); [ -z "$LD" ] && LD=$(losetup --direct-io=on -f --show /zpool.img)
zpool list swappool >/dev/null 2>&1 || zpool import -d $LD swappool
echo 536870912 > /sys/module/zfs/parameters/zfs_arc_max
swapon -p 200 /dev/zvol/swappool/swap 2>/dev/null; log "swap: $(swapon --show --noheadings | tr -s ' ' | tr '\n' ';') $(zst)"
# ---- daemon
systemctl stop fleetd-exp 2>/dev/null; pkill -x fleetd; sleep 1
systemd-run --unit=fleetd-exp --setenv=FLEETD_DATA=/var/lib/fleetd --setenv=FLEETD_TOKEN=exp /usr/local/bin/fleetd serve -listen 127.0.0.1:8080 -interval 5s -reclaim-after 20s -reclaim-keep-mib 150 -prefetch-on-wake -max-pause -1s >/dev/null 2>&1; sleep 2
# ---- start cells in batches of 5; wait for health, then wait for free RAM > 3 GB (daemon reclaim) before the next batch
docker start tgw >/dev/null 2>&1
i=1; while [ $i -le 49 ]; do j=$((i+4)); [ $j -gt 49 ] && j=49
  for b in $(seq $i $j); do docker start v$b >/dev/null 2>&1; done
  for b in $(seq $i $j); do for k in $(seq 1 90); do curl -s -o /dev/null -m 2 -w "%{http_code}" http://127.0.0.1:$((22000+b))/health | grep -q 200 && break; sleep 2; done; done
  for k in $(seq 1 60); do [ "$(freemb)" -gt 3000 ] && break; sleep 5; done
  log "batch $i-$j up free=$(freemb)MB load=$(cut -d' ' -f1 /proc/loadavg) $(zst)"; i=$((j+1))
done
settle_all 50; log "STEP 50 cells on compressed swap: used=$(free -m | awk 'NR==2{print $3}')MB free=$(freemb)MB $(zst) root_free=$(df -h / | awk 'NR==2{print $4}')"
burst 10 z50a; settle_all 50; burst 10 z50b; settle_all 50
curl -s -H "$A" $API/metrics | grep -E "^fleetd_(wake_failures_total|cells)" | tee -a $LOG
log "final: paused=$(npaused) exited=$(docker ps -a --filter label=fleet.cell --filter status=exited --format x | wc -l) $(zst)"; echo RUN-DONE | tee -a $LOG
