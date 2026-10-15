#!/bin/sh
# A failed build leaves no ./frl to run.
[ -e "$QUICKSTART_BUILT" ] || { echo './frl: not found' >&2; exit 127; }
printf 'frl|%s|%s\n' "$*" "$FDB_CLUSTER_FILE" >> "$QUICKSTART_LOG"
[ "$*" = 'fdb up' ] || exit 99
[ "$QUICKSTART_UP_STATUS" -eq 0 ] || exit "$QUICKSTART_UP_STATUS"
printf '%s\n' "$QUICKSTART_CLUSTER"
