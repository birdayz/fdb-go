#!/usr/bin/env bash
set -Eeuo pipefail
readonly data=/var/fdb/data probe=/etc/ingest20k/inode-flags
# Seal an absolute session deadline around setup AND the whole server group.
if [[ $# == 1 && $1 =~ ^[0-9]+$ ]]; then
  remaining=$(( $1 - $(date +%s) - 1 ))
  (( remaining > 5 && remaining <= 1200 )) || exit 1
  # No --foreground: timeout must terminate the complete monitor/server group.
  exec /usr/bin/timeout --signal=TERM --kill-after=5s "$((remaining - 5))s" /bin/bash "$0" --inside "$1"
fi
[[ $# == 2 && $1 == --inside && $2 =~ ^[0-9]+$ ]] || exit 1
[[ ${FDB4_OWNER:-} == otel283-vxnar && ${FDB4_SESSION:-} == "${FDB4_RUN:-}" ]] || exit 1
[[ ${FDB4_RUN:-} =~ ^prof20k[0-9]{4}$ ]] || exit 1
[[ -d "$data" && ! -L "$data" ]] || exit 1
first=$(find "$data" -mindepth 1 -maxdepth 1 -print -quit)
[[ -z "$first" ]] || { echo 'Refusing populated data; no adoption or repair' >&2; exit 1; }
# The mounted static program only GETs flags. Original metadata is printed; any
# unsupported filesystem/GET, unsafe path/type/mount, or NOCOW observation fails.
"$probe" "$data"
mkdir "$data/cluster" "$data/processes" "$data/traces"
for port in 14883 14884 14885 14886; do
  mkdir "$data/processes/$port" "$data/traces/$port"
done
cp /etc/fdb4/fdb.cluster.seed "$data/cluster/fdb.cluster"
printf '%s %s %s\n' "$FDB4_OWNER" "$FDB4_SESSION" "$FDB4_RUN" > "$data/.otel283-owner"
chown -R fdb:fdb "$data"
exec /usr/bin/fdbmonitor --conffile /etc/fdb4/foundationdb.conf --lockfile /var/run/otel283-ingest20k-monitor.pid
