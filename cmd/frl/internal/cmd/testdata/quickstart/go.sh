#!/bin/sh
printf 'go|%s|%s\n' "$*" "$FDB_CLUSTER_FILE" >> "$QUICKSTART_LOG"
case "$*" in
  'build -o frl ./cmd/frl')
    [ "$QUICKSTART_BUILD_STATUS" -ne 0 ] || : > "$QUICKSTART_BUILT"
    exit "$QUICKSTART_BUILD_STATUS" ;;
  'run ./example/sql'|'run ./example')
    # Both examples refuse an empty FDB_CLUSTER_FILE (errNoClusterFile, tested in
    # each example package) instead of falling back to FDB's default file.
    [ -n "$FDB_CLUSTER_FILE" ] || exit 1
    [ "$*" != 'run ./example/sql' ] || exit "$QUICKSTART_SQL_STATUS"
    exit 0 ;;
  *) exit 99 ;;
esac
