#!/bin/bash
R=/home/ops/results; LOG=$R/disk-tier-rest.log; : > $LOG
log(){ echo "$(date -u +%T) $*" | tee -a $LOG; }
# --- zram only (same service config as the density runs: zstd, 200 %)
swapoff /swap.img; systemctl start zramswap; sleep 2; log "zram: $(zramctl --noheadings | tr -s ' ')"
/home/ops/disk-tier.sh zram 3
# --- zram with writeback to a disk-backed loop device
swapoff /dev/zram0; echo 1 > /sys/block/zram0/reset
[ -f /var/zram-back.img ] || { fallocate -l 40G /var/zram-back.img; chmod 600 /var/zram-back.img; }
LD=$(losetup -f --show /var/zram-back.img); echo $LD > /sys/block/zram0/backing_dev || log "backing_dev failed"
echo zstd > /sys/block/zram0/comp_algorithm; echo 30G > /sys/block/zram0/disksize; mkswap -q /dev/zram0; swapon -p 100 /dev/zram0
log "zram+wb: backing=$(cat /sys/block/zram0/backing_dev) $(zramctl --noheadings | tr -s ' ')"
/home/ops/disk-tier.sh wb 3 wb
log "RUN-DONE rest"
