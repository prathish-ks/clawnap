#!/usr/bin/env bash
# Create the experiment host on Hetzner Cloud with cloud-init. Idempotent by name.
# Prereqs: `hcloud context create fleet` (paste an API token from the Hetzner Cloud console).
set -euo pipefail
NAME=${NAME:-fleet-exp-1}
TYPE=${TYPE:-cx43}             # 8 vCPU / 16 GB Intel shared, ~EUR 16/mo, EU only. Fallback: cax31 (ARM, ~EUR 21/mo). US: cpx41 (~EUR 70/mo, avoid)
LOCATION=${LOCATION:-hel1}    # hel1 Helsinki / fsn1 Falkenstein / nbg1 Nuremberg (CX and CAX are EU-only)
IMAGE=${IMAGE:-ubuntu-24.04}
PUB=$(cat "${PUBKEY_FILE:-$HOME/.ssh/id_ed25519.pub}")
EXTRA=""; [ -n "${EXTRA_PUBKEY_FILE:-}" ] && EXTRA=$(cat "$EXTRA_PUBKEY_FILE")   # a second key for the ops user (e.g. a passphrase-less automation key)
HERE=$(cd "$(dirname "$0")" && pwd)

hcloud ssh-key describe fleet-key >/dev/null 2>&1 || hcloud ssh-key create --name fleet-key --public-key "$PUB"
SWAP_GB=${SWAP_GB:-80}         # ~0.8 GB per hibernated cell; 80 GB holds ~100 cells
python3 - "$HERE/cloud-init.yaml" "$PUB" "$EXTRA" "$SWAP_GB" > /tmp/fleet-cloud-init.yaml <<'PY'
import sys
src, pub, extra, swap = sys.argv[1], sys.argv[2], sys.argv[3], sys.argv[4]
s = open(src).read()
keys = '      - "%s"' % pub
if extra:
    keys += '\n      - "%s"' % extra
s = s.replace('      - "__SSH_PUBKEY__"', keys).replace("__SWAP_GB__", swap)
sys.stdout.write(s)
PY

if hcloud server describe "$NAME" >/dev/null 2>&1; then
  echo "server $NAME already exists"
else
  hcloud server create --name "$NAME" --type "$TYPE" --location "$LOCATION" --image "$IMAGE" \
    --ssh-key fleet-key --user-data-from-file /tmp/fleet-cloud-init.yaml --label project=clawnap
fi
IP=$(hcloud server ip "$NAME")
echo "host: $IP  (ssh ops@$IP)  — cloud-init takes ~3–5 min; check: ssh ops@$IP cat BOOTSTRAP_OK"
