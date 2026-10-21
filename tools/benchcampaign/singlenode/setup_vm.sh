#!/usr/bin/env bash
# Prepare a fresh benchmark VM: pinned toolchain downloads, a masked FDB
# service, the private client namespace and veth link, the cluster file and the
# source trees (from $root/sources/*.tar.gz, copied there beforehand).
set -euo pipefail
: "${FDB_BENCH_HOST_VETH_ADDR:?host veth address with prefix, e.g. 192.0.2.1/30}"
: "${FDB_BENCH_PEER_VETH_ADDR:?namespace veth address with prefix, e.g. 192.0.2.2/30}"
: "${FDB_BENCH_SERVER_ADDR:?owned server host:port, e.g. 192.0.2.1:4500}"
: "${FDB_BENCH_CLUSTER_DESCRIPTION:?cluster file description (before the colon)}"
: "${FDB_BENCH_CLUSTER_ID:?cluster file ID (between the colon and @)}"
root=${FDB_BENCH_ROOT:-/srv/fdb-bench}
netns=${FDB_BENCH_NETNS:-fdb-bench-client}
host_dev=${FDB_BENCH_HOST_VETH:-fdb-bench-host}
peer_dev=${FDB_BENCH_PEER_VETH:-fdb-bench-peer}
server_host=${FDB_BENCH_SERVER_ADDR%:*}
mkdir -p "$root"/{bin,downloads,trees,cache,results,provenance,cluster}
fetch() {
  local url=$1 hash=$2 dest=$3
  curl --fail --location --retry 3 --connect-timeout 15 --max-time 600 "$url" -o "$dest"
  printf '%s  %s\n' "$hash" "$dest" | sha256sum -c -
}
fetch https://github.com/bazelbuild/bazelisk/releases/download/v1.30.0/bazelisk-linux-amd64 a248ae2b2b62b5baebb748f6fba7203bff1a27e5d01b55399a8406744cb539e8 "$root/downloads/bazelisk"
install -m755 "$root/downloads/bazelisk" /usr/local/bin/bazelisk
fetch https://github.com/apple/foundationdb/releases/download/7.3.77/foundationdb-clients_7.3.77-1_amd64.deb b6c263723009bddbc30b6ae8cf9854340bd35cef14152abb0da8fb5af2b91254 "$root/downloads/fdb-clients.deb"
fetch https://github.com/apple/foundationdb/releases/download/7.3.77/foundationdb-server_7.3.77-1_amd64.deb 789f1ed4cbacc0fd7043da2217c9e57516e4ec94fa7140853ec22a7e715785cc "$root/downloads/fdb-server.deb"
systemctl unmask foundationdb.service
DEBIAN_FRONTEND=noninteractive dpkg -i "$root/downloads/fdb-clients.deb" "$root/downloads/fdb-server.deb"
systemctl disable --now foundationdb.service
systemctl mask foundationdb.service
ldconfig
DEBIAN_FRONTEND=noninteractive apt-get install -y iputils-ping
systemctl disable --now apt-daily.timer apt-daily-upgrade.timer
if systemctl list-unit-files unattended-upgrades.service --no-legend | grep -q unattended; then systemctl stop unattended-upgrades.service; fi
ip netns add "$netns"
ip link add "$host_dev" type veth peer name "$peer_dev"
ip link set "$peer_dev" netns "$netns"
ip address add "$FDB_BENCH_HOST_VETH_ADDR" dev "$host_dev"
ip link set "$host_dev" up
ip netns exec "$netns" ip address add "$FDB_BENCH_PEER_VETH_ADDR" dev "$peer_dev"
ip netns exec "$netns" ip link set "$peer_dev" up
ip netns exec "$netns" ip link set lo up
printf '%s:%s@%s\n' "$FDB_BENCH_CLUSTER_DESCRIPTION" "$FDB_BENCH_CLUSTER_ID" "$FDB_BENCH_SERVER_ADDR" > "$root/cluster/fdb.cluster"
for side in legacy before pr2 final; do
  mkdir -p "$root/trees/$side"
  tar xzf "$root/sources/$side.tar.gz" -C "$root/trees/$side"
done
lscpu -J > "$root/provenance/lscpu.json"
lscpu -e=CPU,CORE,SOCKET,NODE > "$root/provenance/cpu-map.txt"
uname -a > "$root/provenance/uname.txt"
lsblk -J -o NAME,SIZE,TYPE,MODEL,ROTA,MOUNTPOINT > "$root/provenance/disks.json"
cat /proc/meminfo > "$root/provenance/meminfo.txt"
cat /proc/cpuinfo > "$root/provenance/cpuinfo.txt"
ip -j address > "$root/provenance/ip-address.json"
ip netns exec "$netns" ip -j address > "$root/provenance/client-ip-address.json"
ip netns exec "$netns" ping -c 10 -i 0.1 "$server_host" > "$root/provenance/zero-delay-ping.txt"
/usr/sbin/fdbserver --version > "$root/provenance/fdbserver-version.txt"
/usr/bin/fdbcli --version > "$root/provenance/fdbcli-version.txt"
sha256sum /usr/sbin/fdbserver /usr/bin/fdbcli /usr/lib/libfdb_c.so /usr/local/bin/bazelisk "$root/cluster/fdb.cluster" > "$root/provenance/runtime.sha256"
printf 'VM setup complete\n'
