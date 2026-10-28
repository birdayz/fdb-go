#!/usr/bin/env bash
set -Eeuo pipefail

# Parameters exist for isolated offline fixtures; the production main fixes them.
ingest20k_prepare() {
  local data=$1 probe=$2 seed=$3 owner=$4 session=$5 run=$6 first port
  [[ $owner == otel283-vxnar && $session == "$run" && $run =~ ^prof20k[0-9]{4}$ ]] || return 1
  [[ -d "$data" && ! -L "$data" ]] || return 1
  first=$(find "$data" -mindepth 1 -maxdepth 1 -print -quit) || return
  [[ -z "$first" ]] || { echo 'Refusing populated data; no adoption or repair' >&2; return 1; }
  # GET must succeed before the first write. Never adopt or repair existing data.
  "$probe" "$data" || return
  mkdir "$data/cluster" "$data/processes" "$data/traces" || return
  for port in 14883 14884 14885 14886; do
    mkdir "$data/processes/$port" "$data/traces/$port" || return
  done
  cp "$seed" "$data/cluster/fdb.cluster" || return
  printf '%s %s %s\n' "$owner" "$session" "$run" > "$data/.otel283-owner" || return
  chown -R fdb:fdb "$data" || return
}

ingest20k_entry_main() {
  if [[ $# == 1 && $1 =~ ^[0-9]+$ ]]; then
    # The independent absolute deadline encloses setup, not just fdbmonitor.
    exec /bin/bash /etc/fdb4/deadline.sh "$1" /bin/bash /etc/fdb4/container-entrypoint.sh --inside "$1"
  fi
  [[ $# == 2 && $1 == --inside && $2 =~ ^[0-9]+$ ]] || return 1
  ingest20k_prepare /var/fdb/data /etc/ingest20k/inode-flags /etc/fdb4/fdb.cluster.seed \
    "${FDB4_OWNER:-}" "${FDB4_SESSION:-}" "${FDB4_RUN:-}" || return
  exec /usr/bin/fdbmonitor --conffile /etc/fdb4/foundationdb.conf --lockfile /var/run/otel283-ingest20k-monitor.pid
}

if [[ ${BASH_SOURCE[0]} == "$0" ]]; then
  ingest20k_entry_main "$@"
fi
