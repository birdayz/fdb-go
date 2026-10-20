set -euo pipefail
umask 077
R=/srv/fdb-bench
systemctl stop foundationdb.service
systemctl is-active foundationdb.service && exit 1
if pgrep -x fdbserver; then echo 'REFUSED: distribution fdbserver still running' >&2; exit 1; fi
lscpu -J > "$R/provenance/lscpu.json"
lscpu -e=CPU,CORE,SOCKET,NODE > "$R/provenance/cpu-map.txt"
uname -a > "$R/provenance/uname.txt"
ip -j address > "$R/provenance/ip-address.json"
lsblk -J -o NAME,SIZE,TYPE,MODEL,ROTA,MOUNTPOINT > "$R/provenance/disks.json"
chronyc tracking > "$R/provenance/chronyc-tracking.txt"
for f in /sys/devices/system/cpu/cpu[0-9]*/topology/thread_siblings_list; do printf '%s ' "$f";cat "$f";done > "$R/provenance/thread-siblings.txt"
/usr/sbin/fdbserver --version > "$R/provenance/fdbserver-version.txt"
/usr/bin/fdbcli --version > "$R/provenance/fdbcli-version.txt"
sha256sum /usr/sbin/fdbserver /usr/bin/fdbcli /usr/lib/libfdb_c.so > "$R/provenance/runtime.sha256"
printf '%s  %s\n' 179df8ec7292ad060fb7bd60a771ff4a8dd3989a8115638356c73feea4fc2c5f /usr/sbin/fdbserver 6dfe3328389fe8ff8175ddc080e278c5b07d1d2601f97079cba1194bfb2e41fb /usr/bin/fdbcli e4ad7fed7b7580bf5f7b38f0a4279d37e698f69d534182e5de0b8beeef6cd1b1 /usr/lib/libfdb_c.so | sha256sum -c -
python3 - <<'PY'
from pathlib import Path
assert len(list(Path('/sys/devices/system/cpu').glob('cpu[0-9]*'))) == 16
for i in range(16):
    text=Path(f'/sys/devices/system/cpu/cpu{i}/topology/thread_siblings_list').read_text().strip()
    want=f'{i//2*2}-{i//2*2+1}'
    want2=f'{i//2*2},{i//2*2+1}'
    assert text in (want,want2),(i,text,want)
print('PASS: all 16 CPUs expose the frozen disjoint (2k,2k+1) sibling map')
PY
ip -j route get 10.89.20.10 > "$R/provenance/private-route-client.json"
ip -j route get 10.89.20.11 > "$R/provenance/private-route-node1.json"
printf 'BOOTSTRAP COMPLETE\n'
