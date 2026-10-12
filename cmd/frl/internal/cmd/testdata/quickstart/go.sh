#!/bin/sh
printf 'go|%s|%s\n' "$*" "$FDB_CLUSTER_FILE" >> "$QUICKSTART_LOG"
case "$*" in
  'build -o frl ./cmd/frl') exit "$QUICKSTART_BUILD_STATUS" ;;
  'run ./example/sql') exit "$QUICKSTART_SQL_STATUS" ;;
  'run ./example') exit 0 ;;
  *) exit 99 ;;
esac
