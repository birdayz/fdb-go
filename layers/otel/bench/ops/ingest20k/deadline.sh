#!/usr/bin/env bash
set -Eeuo pipefail

# Absolute sealed epoch, never a renewed relative setup/server allowance.
ingest20k_remaining() {
  [[ $# == 2 && $1 =~ ^[0-9]{1,10}$ && $2 =~ ^[0-9]{1,10}$ ]] || return 1
  local remaining=$((10#$1 - 10#$2))
  (( remaining > 5 && remaining <= 1200 )) || return 1
  printf '%s\n' "$remaining"
}

ingest20k_deadline_main() {
  [[ $# -ge 2 ]] || return 1
  local epoch=$1 now remaining
  shift
  now=$(date +%s) || return
  remaining=$(ingest20k_remaining "$epoch" "$now") || return
  # Reserve the final five seconds for KILL. Do not use --foreground: descendants
  # must remain in timeout's process group through setup and server execution.
  exec /usr/bin/timeout --signal=TERM --kill-after=5s "$((remaining - 5))s" "$@"
}

if [[ ${BASH_SOURCE[0]} == "$0" ]]; then
  ingest20k_deadline_main "$@"
fi
