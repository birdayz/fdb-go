#!/bin/bash
# enable-shared-cache.sh — turn on the fleet-shared Bazel cache (RFC-257) on a
# live runner. Idempotent. Run as root on the box after it is attached to the
# ci-fleet network; .github/workflows/fleet-shared-cache.yml does both steps.
#
# server: also runs bazel-remote, published on the private IP only.
# Both: point /etc/bazel.bazelrc at it. The local --disk_cache stays as the
# first tier; an unreachable remote cache only warns (Bazel falls back to
# local execution), and --remote_timeout bounds the wait.
#
# Rollback: delete the marked block from /etc/bazel.bazelrc (and, on the
# server, `docker rm -f bazel-remote`).
set -euo pipefail

ROLE="${1:?usage: enable-shared-cache.sh server|client}"
IP="10.77.0.2" # infra/shared_cache.tf local.shared_cache_ip
IMAGE="buchgr/bazel-remote-cache@sha256:8e17332e2ceb8b69f67bb08d6a1a51b3e11aaa5a25644c8a34f09d1ff1811a53"

# Bring up the ci-fleet interface. Attaching a network to a running server adds
# a NIC but configures no address, so take IP/MAC/gateway from Hetzner's
# metadata service, configure them now, and persist them in netplan for reboots
# (netplan is not re-applied now, so nothing else on the box is touched).
META=$(curl -fsS http://169.254.169.254/hetzner/v1/metadata/private-networks)
MY_IP=$(awk '/^- ip:/{print $3; exit}' <<<"$META")
MAC=$(awk '/mac_address:/{print $2; exit}' <<<"$META")
GW=$(awk '/gateway:/{print $2; exit}' <<<"$META")
if [ -z "$MY_IP" ] || [ -z "$MAC" ] || [ -z "$GW" ]; then
	echo "no private network in metadata: attach this box to ci-fleet first" >&2
	exit 1
fi
IFACE=$(ip -o link | awk -v m="$MAC" 'index(tolower($0), tolower(m)) {sub(":", "", $2); print $2; exit}')
[ -n "$IFACE" ] || { echo "no interface with MAC $MAC" >&2; exit 1; }
if ! ip -4 addr show dev "$IFACE" | grep -q "inet $MY_IP/"; then
	ip link set "$IFACE" up
	ip addr add "$MY_IP/32" dev "$IFACE"
fi
ip route replace 10.77.0.0/16 via "$GW" dev "$IFACE" onlink
printf '%s\n' \
	'network:' \
	'  version: 2' \
	'  ethernets:' \
	'    ci-fleet:' \
	"      match: {macaddress: \"$MAC\"}" \
	"      addresses: [\"$MY_IP/32\"]" \
	"      routes: [{to: 10.77.0.0/16, via: \"$GW\", on-link: true}]" \
	>/etc/netplan/60-ci-fleet.yaml
chmod 600 /etc/netplan/60-ci-fleet.yaml
echo "ci-fleet: $IFACE $MY_IP via $GW"

if [ "$ROLE" = server ]; then
	[ "$MY_IP" = "$IP" ] || { echo "this box is $MY_IP, the cache server must be $IP" >&2; exit 1; }
	mkdir -p /mnt/ci-data/bazel-remote
	if ! docker inspect bazel-remote >/dev/null 2>&1; then
		# 30 GiB of the 100 GiB data volume (it also holds Docker, the 20 GiB disk cache and the FDB build); LRU-evicted by bazel-remote.
		docker run -d --restart=always --name bazel-remote \
			-v /mnt/ci-data/bazel-remote:/data \
			-p "$IP:9092:9092" \
			"$IMAGE" --dir /data --max_size 30
	fi
fi

MARK="# RFC-257 shared cache"
if ! grep -q "$MARK" /etc/bazel.bazelrc; then
	printf '%s\n' "$MARK" "build --remote_cache=grpc://$IP:9092" "build --remote_timeout=10s" >>/etc/bazel.bazelrc
fi

for _ in $(seq 1 30); do
	if (exec 3<>"/dev/tcp/$IP/9092") 2>/dev/null; then
		echo "shared cache reachable at $IP:9092"
		exit 0
	fi
	sleep 2
done
echo "shared cache NOT reachable at $IP:9092" >&2
if [ "$ROLE" = server ]; then
	docker ps -a --filter name=bazel-remote >&2
	docker logs --tail 40 bazel-remote >&2 || true
	ss -ltnp >&2 || true
fi
exit 1
