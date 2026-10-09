#!/bin/bash
# enable-shared-cache.sh — turn on the fleet-shared Bazel cache (RFC-257) on a
# live runner. Idempotent. Run as root on the box, after `tofu apply` attached
# it to the ci-fleet network:
#
#   ssh root@<gh-runner-fdb>     'bash -s server' < infra/enable-shared-cache.sh
#   ssh root@<gh-runner-drain-0> 'bash -s client' < infra/enable-shared-cache.sh
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
IP="10.77.0.2"  # infra/shared_cache.tf local.shared_cache_ip
IMAGE="buchgr/bazel-remote-cache@sha256:8e17332e2ceb8b69f67bb08d6a1a51b3e11aaa5a25644c8a34f09d1ff1811a53"

if [ "$ROLE" = server ]; then
	ip -4 addr show | grep -q "inet $IP/" || { echo "this box does not hold $IP: attach it to ci-fleet first (tofu apply)"; exit 1; }
	mkdir -p /mnt/ci-data/bazel-remote
	if ! docker inspect bazel-remote >/dev/null 2>&1; then
		# 60 GiB of the 100 GiB data volume; LRU-evicted by bazel-remote.
		docker run -d --restart=always --name bazel-remote \
			-v /mnt/ci-data/bazel-remote:/data \
			-p "$IP:9092:9092" \
			"$IMAGE" --dir /data --max_size 60
	fi
fi

MARK="# RFC-257 shared cache"
if ! grep -q "$MARK" /etc/bazel.bazelrc; then
	cat >>/etc/bazel.bazelrc <<EOF
$MARK
build --remote_cache=grpc://$IP:9092
build --remote_timeout=10s
EOF
fi

for i in $(seq 1 30); do
	(exec 3<>"/dev/tcp/$IP/9092") 2>/dev/null && { echo "shared cache reachable at $IP:9092"; exit 0; }
	sleep 2
done
echo "shared cache NOT reachable at $IP:9092" >&2
exit 1
