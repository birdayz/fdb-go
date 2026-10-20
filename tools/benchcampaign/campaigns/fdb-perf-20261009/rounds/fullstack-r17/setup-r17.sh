#!/usr/bin/env bash
set -Eeuo pipefail
cd /opt/fdbperf
p=artifacts/fullstack-r17-build
mkdir "$p"
exec > >(tee "$p/setup.log") 2>&1
printf '%s  %s\n' \
  4701f00b86ce3313a60152e39b34ebc71506fac3f485de222ff9747f67e10957 upload/fullstack-r16.tar.gz \
  93b8d79f05a5b1620c9c51a1e5dd7515943adcda269333a4b2561f5db11e7f2f bin/clientbench-r16-new \
  4c89840d7e8d83d30534747dc0ae6ab2a1fc68b504db5699d9aa6418397cdb1c bin/clientbench-v7-cgo \
  e4ad7fed7b7580bf5f7b38f0a4279d37e698f69d534182e5de0b8beeef6cd1b1 /usr/lib/libfdb_c.so \
  179df8ec7292ad060fb7bd60a771ff4a8dd3989a8115638356c73feea4fc2c5f /usr/sbin/fdbserver \
  48b9a56e70aa3d6c43c3760a3c8e43e6f3d70e6cd1805757de7f74518f177203 artifacts/fullstack-r16-build/reused-v7-build/harness-v7-candidate.tar.gz \
  > "$p/frozen-inputs.sha256"
sha256sum -c "$p/frozen-inputs.sha256"
mkdir sources/r17-frozen-harness
tar -xzf artifacts/fullstack-r16-build/reused-v7-build/harness-v7-candidate.tar.gz -C sources/r17-frozen-harness
file bin/clientbench-r16-new bin/clientbench-v7-cgo > "$p/binary-types.txt"
ldd bin/clientbench-v7-cgo > "$p/native-ldd.txt"
readelf -V bin/clientbench-v7-cgo > "$p/native-symbol-versions.txt"
/usr/sbin/fdbserver --version > "$p/fdbserver-version.txt"
python3 --version > "$p/python-version.txt"
strace --version > "$p/strace-version.txt"
test ! -d /var/lib/foundationdb
getent passwd foundationdb || useradd --system --user-group --home-dir /var/lib/foundationdb --shell /usr/sbin/nologin foundationdb
install -d -o foundationdb -g foundationdb /var/lib/foundationdb/data/4500 /var/log/foundationdb
printf 'fdbperf_r17:fdbperf_r17_<action-id>@127.0.0.1:4500\n' > /etc/foundationdb/fdb.cluster
chown -R foundationdb:foundationdb /etc/foundationdb
cat > /etc/systemd/system/foundationdb.service <<'UNIT'
[Unit]
Description=FoundationDB R17 fresh benchmark cluster
After=network-online.target
Wants=network-online.target
[Service]
Type=simple
ExecStart=/usr/lib/foundationdb/fdbmonitor --conffile /etc/foundationdb/foundationdb.conf --lockfile /var/run/fdbmonitor.pid
CPUAffinity=2 3 4 5 6 7
Restart=on-failure
[Install]
WantedBy=multi-user.target
UNIT
systemctl list-timers --all > "$p/timers-before.txt"
# Keep automatic package jobs out of the registered dedicated timing interval.
systemctl stop apt-daily.timer apt-daily-upgrade.timer
systemctl stop apt-daily.service apt-daily-upgrade.service unattended-upgrades.service || true
systemctl daemon-reload
systemctl enable --now foundationdb.service
timeout 60 /usr/bin/fdbcli --exec 'configure new single ssd-2 tenant_mode=optional_experimental' > "$p/cluster-configure.txt"
timeout 60 /usr/bin/fdbcli --exec 'status json' > "$p/fresh-fdb-status.json"
python3 -B - <<'PY'
import json
from pathlib import Path
p=Path('/opt/fdbperf/artifacts/fullstack-r17-build')
s=json.loads((p/'fresh-fdb-status.json').read_text())
c=s['cluster']['configuration']
assert c['storage_engine']=='ssd-2' and c['redundancy_mode']=='single'
assert c['tenant_mode']=='optional_experimental'
print('Fresh cluster configured:',c)
PY
systemctl cat foundationdb.service > "$p/fdb-service.txt"
ps -eo pid,ppid,comm,args > "$p/processes-after-setup.txt"
lscpu -e > "$p/cpu-topology.txt"
lscpu > "$p/lscpu.txt"
uname -a > "$p/uname.txt"
cat /etc/os-release > "$p/os-release.txt"
cat /proc/sys/kernel/random/boot_id > "$p/boot-id.txt"
cat /proc/cmdline > "$p/kernel-command-line.txt"
systemctl list-timers --all > "$p/timers-after.txt"
sha256sum /usr/lib/libfdb_c.so /usr/sbin/fdbserver /usr/lib/foundationdb/fdbmonitor /etc/foundationdb/fdb.cluster /etc/foundationdb/foundationdb.conf /etc/systemd/system/foundationdb.service > "$p/runtime.sha256"
printf 'setup_exit_code=0\nfinished_utc=%s\n' "$(date -u +%FT%TZ)" > "$p/setup-status.txt"
