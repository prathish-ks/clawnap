#!/usr/bin/env bash
# Volume test: 50 hibernated cells, 10 woken at once by signed webhook updates through the public ingress.
set -u
R=/home/ops/results; LOG=$R/volume-run.log; : > $LOG
export FLEETD_DATA=/var/lib/fleetd FLEETD_TOKEN=exp
log() { echo "$(date -u +%T) $*" | tee -a $LOG; }
INGRESS=https://167-233-118-221.sslip.io
TOKF=/root/bot.token
# --- create 49 more webhook-mode cells (tgw exists) in batches of 5; each gets the same bot token in its config
# (a bot token can only be registered with one webhook at a time, so only tgw advertises; the others get -ingress-url so the
#  provisioner writes a verifier+secret, but their Telegram channel is disabled to avoid setWebhook fights)
i=1; while [ $i -le 49 ]; do j=$((i+4)); [ $j -gt 49 ] && j=49
  for b in $(seq $i $j); do
    fleetd cells create -name v$b -port $((22000+b)) -hook-port $((22900+b)) -ingress-url $INGRESS -telegram-token-file $TOKF -idle 45s -tier pause >/dev/null 2>&1 || log "create v$b failed"
    # disable the telegram channel in this cell so it never calls setWebhook for the shared bot
    python3 - <<PY
import json; p="/var/lib/fleetd/cells/v$b/state/openclaw.json"; d=json.load(open(p)); d["channels"]["telegram"]["enabled"]=False; json.dump(d,open(p,"w"),indent=2)
PY
    docker restart v$b >/dev/null 2>&1
  done
  for b in $(seq $i $j); do for k in $(seq 1 90); do curl -s -o /dev/null -m 2 -w "%{http_code}" http://127.0.0.1:$((22000+b))/health | grep -q 200 && break; sleep 2; done; done
  log "batch $i-$j up"; i=$((j+1))
done
log "all cells up; waiting for pause + reclaim of all 50"
for k in $(seq 1 60); do n=$(docker ps --filter label=fleet.cell --filter status=paused --format x | wc -l); r=$(curl -s -H "Authorization: Bearer exp" http://127.0.0.1:8080/metrics | awk '/^fleetd_reclaims_total/{print $2}'); [ "$n" -ge 50 ] && [ "${r:-0}" -ge 50 ] && break; sleep 10; done
log "paused=$(docker ps --filter label=fleet.cell --filter status=paused --format x | wc -l) reclaims=$(curl -s -H "Authorization: Bearer exp" http://127.0.0.1:8080/metrics | awk '/^fleetd_reclaims_total/{print $2}') used=$(free -m | awk 'NR==2{print $3}')MB zram=$(zramctl --noheadings -o DATA | head -1 | tr -d ' ')"
# --- fire 10 signed updates at once (v1..v9 + tgw) through the public ingress
log "USER: send a message to @Ocfleet_bot NOW (the real tenth wake)"
sleep 20
targets="tgw v1 v2 v3 v4 v5 v6 v7 v8 v9"
T0=$(date +%s.%N)
for c in $targets; do
  ( sec=$(cat /var/lib/fleetd/cells/$c/secrets/telegram-webhook-secret); s=$(date +%s.%N)
    code=$(curl -s -o /dev/null -m 120 -w "%{http_code}" -X POST -H "Content-Type: application/json" -H "X-Telegram-Bot-Api-Secret-Token: $sec" -d "{\"update_id\":$RANDOM,\"message\":{\"message_id\":1,\"date\":$(date +%s),\"chat\":{\"id\":1,\"type\":\"private\"},\"text\":\"volume\"}}" $INGRESS/hook/$c/telegram-webhook)
    e=$(date +%s.%N); echo "$c $code $(python3 -c "print(round(($e-$s)*1000))") ms (started +$(python3 -c "print(round(($s-$T0)*1000))") ms)" ) &
done; wait
log "--- results"; sleep 5
sudo journalctl -u fleetd-exp --no-pager --since "-3min" | grep -E "woke|thaw recovery|WARN|wake failed" | cut -c40-160 | tee -a $LOG | tail -30
log "still paused: $(docker ps --filter label=fleet.cell --filter status=paused --format x | wc -l)  running: $(docker ps --filter label=fleet.cell --filter status=running --format x | wc -l)  exited: $(docker ps -a --filter label=fleet.cell --filter status=exited --format x | wc -l)"
curl -s -H "Authorization: Bearer exp" http://127.0.0.1:8080/metrics | grep -E "^fleetd_(wakes_total|wake_failures_total|cells)" | tee -a $LOG
log "load: $(cut -d' ' -f1-3 /proc/loadavg)"; echo RUN-DONE | tee -a $LOG
