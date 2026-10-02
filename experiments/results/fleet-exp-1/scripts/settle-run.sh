#!/bin/bash
# For each settle bound: N real wakes of tgw via a signed Telegram update from the paired owner; pass = no settlement abort / no notice in the cell log.
R=/home/ops/results; LOG=$R/settle-run.log; : > $LOG
export FLEETD_DATA=/var/lib/fleetd FLEETD_TOKEN=exp
log() { echo "$(date -u +%T) $*" | tee -a $LOG; }
A="Authorization: Bearer exp"; API=http://127.0.0.1:8080; INGRESS=https://167-233-118-221.sslip.io
OWNER=7972328048; SEC=$(cat /var/lib/fleetd/cells/tgw/secrets/telegram-webhook-secret)
relaunch() { systemctl stop fleetd 2>/dev/null; systemctl stop fleetd-exp 2>/dev/null; pkill -x fleetd; sleep 1
  systemd-run --unit=fleetd-exp --setenv=FLEETD_DATA=/var/lib/fleetd --setenv=FLEETD_TOKEN=exp /usr/local/bin/fleetd serve -listen 127.0.0.1:8080 -interval 5s -reclaim-after 20s -reclaim-keep-mib 150 -prefetch-on-wake -max-pause -1s -wake-concurrency 2 "$@" >/dev/null 2>&1; sleep 2; }
n=0
for bound in 1500ms 1s 500ms; do
  relaunch -thaw-settle $bound; log "== settle bound $bound"
  for i in 1 2 3 4 5; do
    n=$((n+1))
    curl -s -o /dev/null -X POST -H "$A" $API/wake/tgw; sleep 3
    curl -s -o /dev/null -X POST -H "$A" $API/hibernate/tgw
    sleep 75   # reclaim at 20 s; long enough for the freeze detector to fire on thaw
    T0=$(date -u +%Y-%m-%dT%H:%M:%S); s=$(date +%s.%N)
    code=$(curl -s -o /dev/null -m 120 -w "%{http_code}" -X POST -H "Content-Type: application/json" -H "X-Telegram-Bot-Api-Secret-Token: $SEC" \
      -d "{\"update_id\":$((100000+n)),\"message\":{\"message_id\":$((5000+n)),\"date\":$(date +%s),\"from\":{\"id\":$OWNER,\"is_bot\":false,\"first_name\":\"owner\"},\"chat\":{\"id\":$OWNER,\"type\":\"private\"},\"text\":\"settle test $n: reply with the single word OK\"}}" $INGRESS/hook/tgw/telegram-webhook)
    e=$(date +%s.%N); sleep 25
    lg=$(docker logs tgw --since $T0 2>&1)
    det=$(echo "$lg" | grep -c "host timing gap detected"); abort=$(echo "$lg" | grep -c -i "settlement is closed"); notice=$(echo "$lg" | grep -c -i -E "heartbeat failed|embedded agent failed"); sent=$(echo "$lg" | grep -c -i -E "sendMessage|outbound|delivered")
    held=$(journalctl -u fleetd-exp --no-pager --since "$T0" | grep -o -E "hold=[0-9.]+m?s" | head -1)
    log "bound=$bound wake=$i http=$code delivered_in=$(python3 -c "print(round($e-$s,2))")s detector=$det $held abort=$abort notice=$notice outbound_lines=$sent"
  done
done
systemctl stop fleetd-exp; systemctl start fleetd; log "restored fleetd service: $(systemctl is-active fleetd)"; echo SETTLE-DONE | tee -a $LOG
