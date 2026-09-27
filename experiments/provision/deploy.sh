#!/usr/bin/env bash
# Cross-compile fleetd and copy it plus the experiment scripts to the host.
set -euo pipefail
HOST=${1:?usage: deploy.sh ops@IP}
# a passphrase-less key dedicated to the fleet host lets the supervisor's own tooling reach it non-interactively
SSHOPT=()
[ -f "$HOME/.ssh/fleet_ed25519" ] && SSHOPT=(-i "$HOME/.ssh/fleet_ed25519")
cd "$(dirname "$0")/../.."
case "$(ssh ${SSHOPT[@]+"${SSHOPT[@]}"} "$HOST" uname -m)" in
  aarch64|arm64) ARCH=arm64 ;;
  *) ARCH=amd64 ;;
esac
echo "target arch: $ARCH"
GOOS=linux GOARCH=$ARCH CGO_ENABLED=0 go build -o bin/fleetd-linux ./cmd/fleetd
ssh ${SSHOPT[@]+"${SSHOPT[@]}"} "$HOST" mkdir -p fleet/bin fleet/experiments
scp ${SSHOPT[@]+"${SSHOPT[@]}"} bin/fleetd-linux "$HOST":fleet/bin/fleetd
scp ${SSHOPT[@]+"${SSHOPT[@]}"} experiments/*.sh experiments/README.md "$HOST":fleet/experiments/
ssh ${SSHOPT[@]+"${SSHOPT[@]}"} "$HOST" 'chmod +x fleet/bin/fleetd fleet/experiments/*.sh && echo deployed && fleet/bin/fleetd cells list'
