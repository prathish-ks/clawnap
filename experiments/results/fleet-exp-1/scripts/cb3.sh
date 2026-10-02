#!/bin/bash
# Design 1 acceptance: warm floor 300 MiB + hot-set prefetch. 100 cells; v1..v5 warm (resident tier), v21..v40 cold with hot sets; warm burst of five, two cold bursts of ten.
R=/home/ops/results; LOG=$R/cb3-$1.log; : > $LOG
export FLEETD_DATA=/var/lib/fleetd FLEETD_TOKEN=exp
log() { echo "$(date -u +%T) $*" | tee -a $LOG; }
A="Authorization: Bearer exp"; API=http://127.0.0.1:8080
psi() { awk -v k=$2 '$1==k{for(i=2;i<=NF;i++) if($i ~ /^total=/){sub("total=","",$i); print $i}}' /proc/pressure/$1; }
npaused() { docker ps --filter label=fleet.cell --filter status=paused --format x | wc -l; }
avail() { free -m | awk 'NR==2{print $7}'; }
tiers() { fleetd cells list | python3 -c "
import json,sys
cs=json.load(sys.stdin); warm=[];cold=[];res=[]
for c in cs:
    if c.get('phase')!='hibernated': continue
    w=c.get('warm_at',''); r=c.get('reclaimed_at','')
    if w and (not r or w>r): warm.append(c['name'])
    elif r: cold.append(c['name'])
    else: res.append(c['name'])
print('warm=%d cold=%d untrimmed=%d warm:[%s]'%(len(warm),len(cold),len(res),' '.join(sorted(warm))))"; }
state() { log "STATE $1: paused=$(npaused) $(tiers) avail=$(avail)MB used=$(free -m | awk 'NR==2{print $3}')MB hotsets=$(ls /var/lib/fleetd/hotsets 2>/dev/null | wc -l)"; }
burst() { local tag=$1; shift; local cells="$@"; local m0 c0 i0 m1 c1 i1 T0 T1 pids="" a0=$(avail)
  vmstat 1 > $R/cb3-vmstat-$(echo $tag | tr -c "a-z0-9" "_").txt & local VP=$!; ( sleep 6; top -b -n1 -o %CPU | sed -n "7,14p" | awk "{print \$9, \$12}" | tr "\n" ";" > $R/cb3-top.txt ) &
  m0=$(psi memory full); c0=$(psi cpu some); i0=$(psi io full); T0=$(date +%s.%N)
  for c in $cells; do ( s=$(date +%s.%N); code=$(curl -s -o /dev/null -m 300 -w "%{http_code}" -X POST -H "$A" $API/wake/$c); e=$(date +%s.%N); echo "$c $code $(python3 -c "print(round($e-$s,2))")" ) > $R/d1-$c.txt & pids="$pids $!"; done
  wait $pids; T1=$(date +%s.%N); sleep 2; kill $VP 2>/dev/null; m1=$(psi memory full); c1=$(psi cpu some); i1=$(psi io full)
  log "  vmstat: si_peak=$(awk "NR>2{if(\$7>m)m=\$7}END{print int(m/1024)}" $R/cb3-vmstat-$(echo $tag | tr -c "a-z0-9" "_").txt)MB/s so_total=$(awk "NR>2{s+=\$8}END{print int(s/1024)}" $R/cb3-vmstat-$(echo $tag | tr -c "a-z0-9" "_").txt)MB min_idle=$(awk "NR>2{if(m==\"\"||\$15<m)m=\$15}END{print m+0}" $R/cb3-vmstat-$(echo $tag | tr -c "a-z0-9" "_").txt)% peak_wa=$(awk "NR>2{if(\$16>m)m=\$16}END{print m+0}" $R/cb3-vmstat-$(echo $tag | tr -c "a-z0-9" "_").txt)% peak_sy=$(awk "NR>2{if(\$14>m)m=\$14}END{print m+0}" $R/cb3-vmstat-$(echo $tag | tr -c "a-z0-9" "_").txt)% top@6s: $(cat $R/cb3-top.txt)"
  local times=$(for c in $cells; do awk '{print $3}' $R/d1-$c.txt; done | sort -n | tr '\n' ' '); local codes=$(for c in $cells; do awk '{print $2}' $R/d1-$c.txt; done | sort | uniq -c | tr -s ' ' | tr '\n' ';')
  local p50=$(echo $times | tr ' ' '\n' | awk '{a[NR]=$1}END{print a[int((NR+1)/2)]}')
  log "BURST $tag: codes=$codes min=$(echo $times | cut -d' ' -f1)s p50=${p50}s max=$(echo $times | awk '{print $NF}')s | times: $times| psi(ms) mem_full=$(( (m1-m0)/1000 )) cpu_some=$(( (c1-c0)/1000 )) io_full=$(( (i1-i0)/1000 )) | avail before=${a0}MB after=$(avail)MB"
  journalctl -u fleetd --no-pager --since "-3min" | grep -E "prefetched cell=" | grep -o -E "cell=v[0-9]+ scope=[a-z ()]+ .*advised_mib=[0-9]+ swap_before_mib=[0-9]+ landed_mib=[0-9]+ advise_took=[0-9.]+m?s pagein_took=[0-9.]+m?s" | sed -E "s/mechanism=[a-z_]+ procs=[0-9]+ mappings=[0-9]+ //" | tr '\n' ';' | cut -c1-900 | tee -a $LOG; echo | tee -a $LOG
  journalctl -u fleetd --no-pager --since "-3min" | grep -E "woke cell=" | grep -o -E "cell=v[0-9]+ kind=[a-z]+|cell=v[0-9]+ start=[0-9.]+m?s ready=[0-9.]+s" | tr '\n' ';' | cut -c1-500 | tee -a $LOG; echo | tee -a $LOG; }
setpolicy() { mkdir -p /etc/systemd/system/fleetd.service.d
  printf '[Service]\nExecStart=\nExecStart=/usr/local/bin/fleetd serve -listen 127.0.0.1:8080 -interval 5s -reclaim-keep-mib 150 -wake-concurrency 2 -max-pause -1s -prefetch-on-wake -warm-keep-mib 300 %s\n' "$1" > /etc/systemd/system/fleetd.service.d/override.conf
  systemctl daemon-reload; systemctl restart fleetd; sleep 3; log "fleetd policy: -warm-keep-mib 300 $1 ($(systemctl is-active fleetd), binary $(md5sum /usr/local/bin/fleetd | cut -c1-8))"; }
waitpaused() { for k in $(seq 1 60); do [ "$(npaused)" -ge 100 ] && break; sleep 5; done; sleep 15; }
R10="$(seq -s ' ' -f v%g 1 10)"; C1="$(seq -s ' ' -f v%g 21 30)"; C2="$(seq -s ' ' -f v%g 31 40)"
state "start"; sleep 30
burst "cold-10 long-paused (v71..v80, ~45 min)" "$(seq -s ' ' -f v%g 71 80)"; waitpaused; sleep 45; state "after-1"
burst "cold-10 recently active (v51..v60, paused ~10 min)" "$(seq -s ' ' -f v%g 51 60)"; waitpaused; sleep 45; state "after-2"
for c in $(seq -f v%g 71 80); do docker logs $c --since 15m 2>&1 | grep -E "timing gap|tool-search: cataloged|webhook advertised|listening" | sed -E "s/^([0-9T:.-]+).*(timing gap|cataloged|advertised|listening).*/\1 \2/" | head -4 | tr "\n" " "; echo " <- $c"; done 2>/dev/null | head -4 | tee -a $LOG
log "wake_failures=$(curl -s -H "$A" $API/metrics | awk '/^fleetd_wake_failures_total/{print $2}')"; echo CB-DONE | tee -a $LOG
