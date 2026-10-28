#!/usr/bin/env bash
set -Eeuo pipefail
# Read-only GET observations only; the static probe rejects COW-unsafe/unknown data.
readonly root=/var/fdb/data probe=/etc/ingest20k/inode-flags
first=$(find "$root" -type l -print -quit)
[[ -z "$first" ]] || exit 1
uid=$(id -u fdb)
dev=$(stat -c %d "$root")
readonly uid dev
check() {
  local path=$1
  [[ ! -L "$path" && $(stat -c %u "$path") == "$uid" && $(stat -c %d "$path") == "$dev" ]] || exit 1
  "$probe" "$path"
}
check "$root"
check "$root/processes"
check "$root/traces"
for port in 14883 14884 14885 14886; do
  check "$root/processes/$port"
  check "$root/traces/$port"
done
listing=$(find "$root" -type f -print)
[[ -n "$listing" ]] || exit 1
files=0
while IFS= read -r path; do
  [[ "$path" =~ ^/var/fdb/data/((processes|traces)/[0-9]+/[A-Za-z0-9_.-]+|cluster/fdb.cluster|\.otel283-owner)$ ]] || exit 1
  check "$path"
  files=$((files + 1))
done <<< "$listing"
(( files >= 6 )) || exit 1
for port in 14883 14886; do
  listing=$(find "$root/processes/$port" -maxdepth 1 -type f -name 'storage-*.redwood-v1')
  mapfile -t primary <<< "$listing"
  (( ${#primary[@]} == 1 )) || exit 1
  [[ ${primary[0]##*/} =~ ^storage-[0-9a-f]{32}\.redwood-v1$ ]] || exit 1
done
listing=$(find "$root/processes/14884" -maxdepth 1 -type f -name 'log2-V_6-*.sqlite')
mapfile -t primary <<< "$listing"
(( ${#primary[@]} == 1 )) || exit 1
base=${primary[0]##*/}
[[ "$base" =~ ^log2-V_6-([0-9a-f]{32})\.sqlite$ ]] || exit 1
uuid=${BASH_REMATCH[1]}
for name in "log2-V_6-$uuid.sqlite-wal" "logqueue-V_6-$uuid-0.fdq" "logqueue-V_6-$uuid-1.fdq"; do
  [[ -f "$root/processes/14884/$name" && ! -L "$root/processes/14884/$name" ]] || exit 1
done
printf '{"schema":"ingest20k_inode_proof_v1","regular_files_verified":%s,"primary_sets_verified":3}\n' "$files"
