#!/bin/bash
# Recreate v1..v20 with the normal hardened launch (no KSM wrapper), paced 5 at a time.
IMG=ghcr.io/openclaw/openclaw:latest; LOG=/home/ops/results/prep.log; : > $LOG
log() { echo "$(date -u +%T) $*" | tee -a $LOG; }
avail() { free -m | awk 'NR==2{print $7}'; }
i=1; while [ $i -le 20 ]; do j=$((i+4)); [ $j -gt 20 ] && j=20
  for n in $(seq $i $j); do c=v$n; docker rm -f $c >/dev/null 2>&1
    docker run -d --name $c --label fleet.cell=$c --user 1000:1000 --cap-drop ALL --security-opt no-new-privileges --memory 1g --memory-swap 2g --pids-limit 512 --ulimit nofile=65536:65536 --log-opt max-size=10m --log-opt max-file=3 \
      -p 127.0.0.1:$((22000+n)):18789 -p 127.0.0.1:$((22900+n)):8787 -v /var/lib/fleetd/cells/$c/state:/home/node/.openclaw -v /var/lib/fleetd/cells/$c/auth:/home/node/.config/openclaw $IMG >/dev/null 2>&1 || log "recreate $c failed"
  done
  for n in $(seq $i $j); do for k in $(seq 1 60); do curl -s -o /dev/null -m 2 -w "%{http_code}" http://127.0.0.1:$((22000+n))/health | grep -q 200 && break; docker inspect -f "{{.State.Paused}}" v$n | grep -q true && break; sleep 2; done; done
  for k in $(seq 1 12); do [ "$(avail)" -gt 3000 ] && break; sleep 5; done
  log "recreated v$i-v$j avail=$(avail)MB"; i=$((j+1))
done
log "entrypoint now: $(docker inspect v1 -f '{{.Config.Entrypoint}} user={{.Config.User}} caps={{.HostConfig.CapAdd}}')"; echo PREP-DONE | tee -a $LOG
