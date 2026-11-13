#!/usr/bin/env bash
set -Eeuo pipefail

ingest20k_check_inode() {
  local root=$1 probe=$2 uid=$3 dev=$4 path=$5
  [[ ! -L "$path" ]] || return 1
  [[ $(stat -c %u "$path") == "$uid" ]] || return 1
  [[ $(stat -c %d "$path") == "$dev" ]] || return 1
  "$probe" "$path"
}

# The production main supplies fixed paths and the image's fdb UID.
ingest20k_inventory() {
  local root=$1 probe=$2 uid=$3 dev first listing path relative port base uuid name
  local files=0
  local -a primary
  [[ $uid =~ ^[0-9]+$ && -d "$root" && ! -L "$root" ]] || return 1
  dev=$(stat -c %d "$root") || return
  [[ $dev =~ ^[0-9]+$ ]] || return 1
  first=$(find "$root" ! -type d ! -type f -print -quit) || return
  [[ -z "$first" ]] || return 1
  listing=$(find "$root" -type d -print) || return
  while IFS= read -r path; do
    [[ $path == "$root" ]] && continue
    relative=${path#"$root/"}
    [[ $relative =~ ^(cluster|processes|traces|processes/1488[3-6]|traces/1488[3-6]|processes/1488[36]/(serverCheckpoints|fetchedCheckpoints))$ ]] || return 1
  done <<< "$listing"
  for path in "$root" "$root/cluster" "$root/processes" "$root/traces"; do
    [[ -d "$path" ]] || return 1
    ingest20k_check_inode "$root" "$probe" "$uid" "$dev" "$path" || return
  done
  for port in 14883 14884 14885 14886; do
    for path in "$root/processes/$port" "$root/traces/$port"; do
      [[ -d "$path" ]] || return 1
      ingest20k_check_inode "$root" "$probe" "$uid" "$dev" "$path" || return
    done
  done
  # FDB 3ea44ce1d9003ad095e408039e1f755c319c4dfb storageserver.actor.cpp:210,212,13981-14034 creates both before SSI acceptance/durability.
  # Restore:14096-14105 recreates missing dirs; this fresh profile permits only these four, empty.
  for port in 14883 14886; do
    for name in serverCheckpoints fetchedCheckpoints; do
      path="$root/processes/$port/$name"
      [[ -d "$path" && ! -L "$path" ]] || return 1
      first=$(find "$path" -mindepth 1 -maxdepth 1 -print -quit) || return
      [[ -z "$first" ]] || return 1
      ingest20k_check_inode "$root" "$probe" "$uid" "$dev" "$path" || return
    done
  done
  [[ -f "$root/cluster/fdb.cluster" && -f "$root/.otel283-owner" ]] || return 1
  listing=$(find "$root" -type f -print) || return
  [[ -n "$listing" ]] || return 1
  while IFS= read -r path; do
    relative=${path#"$root/"}
    [[ $relative =~ ^((processes|traces)/1488[3-6]/[A-Za-z0-9_.-]+|cluster/fdb\.cluster|\.otel283-owner)$ ]] || return 1
    [[ ${path##*/} != serverCheckpoints && ${path##*/} != fetchedCheckpoints ]] || return 1
    ingest20k_check_inode "$root" "$probe" "$uid" "$dev" "$path" || return
    files=$((files + 1))
  done <<< "$listing"
  (( files >= 6 )) || return 1
  for port in 14883 14886; do
    listing=$(find "$root/processes/$port" -maxdepth 1 -type f -name 'storage-*.redwood-v1') || return
    mapfile -t primary <<< "$listing"
    (( ${#primary[@]} == 1 )) || return 1
    [[ ${primary[0]##*/} =~ ^storage-[0-9a-f]{32}\.redwood-v1$ ]] || return 1
  done
  listing=$(find "$root/processes/14884" -maxdepth 1 -type f -name 'log2-V_6-*.sqlite') || return
  mapfile -t primary <<< "$listing"
  (( ${#primary[@]} == 1 )) || return 1
  base=${primary[0]##*/}
  [[ $base =~ ^log2-V_6-([0-9a-f]{32})\.sqlite$ ]] || return 1
  uuid=${BASH_REMATCH[1]}
  for name in "log2-V_6-$uuid.sqlite-wal" "logqueue-V_6-$uuid-0.fdq" "logqueue-V_6-$uuid-1.fdq"; do
    [[ -f "$root/processes/14884/$name" && ! -L "$root/processes/14884/$name" ]] || return 1
  done
  printf '{"schema":"ingest20k_inode_proof_v1","regular_files_verified":%s,"primary_sets_verified":3}\n' "$files"
}

if [[ ${BASH_SOURCE[0]} == "$0" ]]; then
  uid=$(id -u fdb)
  ingest20k_inventory /var/fdb/data /etc/ingest20k/inode-flags "$uid"
fi
