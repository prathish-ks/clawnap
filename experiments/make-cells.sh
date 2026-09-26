#!/usr/bin/env bash
# Create N stock OpenClaw fleet cells named cell-1..cell-N with the supervisor label.
# Requires: openclaw CLI with fleet support, bots.txt with one Telegram bot token per line.
set -euo pipefail
N=${1:?usage: make-cells.sh N}
BASE_PORT=${BASE_PORT:-18800}
for i in $(seq 1 "$N"); do
  token=$(sed -n "${i}p" bots.txt || true)
  name="cell-$i"; port=$((BASE_PORT + i))
  # NOTE: adapt the create flags to the installed OpenClaw fleet CLI version.
  openclaw fleet create "$name" --port "$port" --label fleet.cell="$name" ${token:+--telegram-token "$token"}
  fleetd cells add -name "$name" -container "$name" -port "$port" -idle "${IDLE:-10m}"
done
echo "created $N cells starting at port $((BASE_PORT + 1))"
