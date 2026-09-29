#!/usr/bin/env bash
# Create the experiment host on Hetzner Cloud with cloud-init. Idempotent by name.
# Prereqs: `hcloud context create fleet` (paste an API token from the Hetzner Cloud console).
set -euo pipefail
NAME=${NAME:-fleet-exp-1}
TYPE=${TYPE:-cx43}             # 8 vCPU / 16 GB Intel shared, ~EUR 16/mo, EU only. Fallback: cax31 (ARM, ~EUR 21/mo). US: cpx41 (~EUR 70/mo, avoid)
LOCATION=${LOCATION:-hel1}    # hel1 Helsinki / fsn1 Falkenstein / nbg1 Nuremberg (CX and CAX are EU-only)
IMAGE=${IMAGE:-ubuntu-24.04}
PUB=$(cat ~/.ssh/id_ed25519.pub)
HERE=$(cd "$(dirname "$0")" && pwd)

hcloud ssh-key describe fleet-key >/dev/null 2>&1 || hcloud ssh-key create --name fleet-key --public-key "$PUB"
SWAP_GB=${SWAP_GB:-80}         # ~0.8 GB per hibernated cell; 80 GB holds ~100 cells
sed "s|__SSH_PUBKEY__|$PUB|; s|__SWAP_GB__|$SWAP_GB|" "$HERE/cloud-init.yaml" > /tmp/fleet-cloud-init.yaml

if hcloud server describe "$NAME" >/dev/null 2>&1; then
  echo "server $NAME already exists"
else
  hcloud server create --name "$NAME" --type "$TYPE" --location "$LOCATION" --image "$IMAGE" \
    --ssh-key fleet-key --user-data-from-file /tmp/fleet-cloud-init.yaml --label project=fleet-supervisor
fi
IP=$(hcloud server ip "$NAME")
echo "host: $IP  (ssh ops@$IP)  — cloud-init takes ~3–5 min; check: ssh ops@$IP cat BOOTSTRAP_OK"
