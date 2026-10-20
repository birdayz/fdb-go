#!/usr/bin/env bash
set -Eeuo pipefail
cd /opt/fdbperf
p=artifacts/fullstack-r17-build
exec > >(tee "$p/setup-correction.log") 2>&1
# FDB connection-string IDs accept alphanumeric characters, not underscores.
printf 'fdbperfR17:fdbperfR17<action-id>@127.0.0.1:4500\n' > /etc/foundationdb/fdb.cluster
chown foundationdb:foundationdb /etc/foundationdb/fdb.cluster
systemctl restart foundationdb.service
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
