#!/usr/bin/env bash
# Cross-compile fleetd and copy it plus the experiment scripts to the host.
set -euo pipefail
HOST=${1:?usage: deploy.sh ops@IP}
# a passphrase-less key dedicated to the fleet host lets the supervisor's own tooling reach it non-interactively
[ -f ~/.ssh/fleet_ed25519 ] && export GIT_SSH_COMMAND="ssh -i ~/.ssh/fleet_ed25519" && SSHOPT="-i $HOME/.ssh/fleet_ed25519" || SSHOPT=""
cd "$(dirname "$0")/../.."
case "$(ssh $SSHOPT "$HOST" uname -m)" in
  aarch64|arm64) ARCH=arm64 ;;
  *) ARCH=amd64 ;;
esac
echo "target arch: $ARCH"
GOOS=linux GOARCH=$ARCH CGO_ENABLED=0 go build -o bin/fleetd-linux ./cmd/fleetd
ssh $SSHOPT "$HOST" mkdir -p fleet/bin fleet/experiments
scp $SSHOPT bin/fleetd-linux "$HOST":fleet/bin/fleetd
scp $SSHOPT experiments/*.sh experiments/README.md "$HOST":fleet/experiments/
ssh $SSHOPT "$HOST" 'chmod +x fleet/bin/fleetd fleet/experiments/*.sh && echo deployed && fleet/bin/fleetd cells list'
