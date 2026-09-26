#!/usr/bin/env bash
# Cross-compile fleetd and copy it plus the experiment scripts to the host.
set -euo pipefail
HOST=${1:?usage: deploy.sh ops@IP}
cd "$(dirname "$0")/../.."
case "$(ssh "$HOST" uname -m)" in
  aarch64|arm64) ARCH=arm64 ;;
  *) ARCH=amd64 ;;
esac
echo "target arch: $ARCH"
GOOS=linux GOARCH=$ARCH CGO_ENABLED=0 go build -o bin/fleetd-linux ./cmd/fleetd
ssh "$HOST" mkdir -p fleet/bin fleet/experiments
scp bin/fleetd-linux "$HOST":fleet/bin/fleetd
scp experiments/*.sh experiments/README.md "$HOST":fleet/experiments/
ssh "$HOST" 'chmod +x fleet/bin/fleetd fleet/experiments/*.sh && echo deployed && fleet/bin/fleetd cells list'
