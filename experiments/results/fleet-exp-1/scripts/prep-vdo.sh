set -x
export DEBIAN_FRONTEND=noninteractive
apt-get update -qq
apt-get install -y -qq linux-generic-hwe-24.04 build-essential git uuid-dev zlib1g-dev libblkid-dev libdevmapper-dev pkg-config python3-dev 2>&1 | tail -3
cd /home/ops && rm -rf vdo && git clone -q --depth 1 https://github.com/dm-vdo/vdo.git && cd vdo && (make -j4 2>&1 | tail -5) && ls -la utils/vdoformat/vdoformat utils/vdostats/vdostats 2>&1
install -m 0755 utils/vdoformat/vdoformat /usr/bin/vdoformat 2>/dev/null; install -m 0755 utils/vdostats/vdostats /usr/bin/vdostats 2>/dev/null
ls /boot/vmlinuz-* ; dpkg -l | grep -E "linux-modules-extra-7|linux-image-7" | awk "{print \$2}"
echo PREP-VDO-DONE
