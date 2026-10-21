#!/usr/bin/env bash
set -euo pipefail
set +e
output=$("$1" 2>&1)
status=$?
set -e
if [[ $status != 1 || $output != 'Java conformance requires the pinned protoc 3.25.9 executable; add its execution-platform artifact instead of falling back to the native protobuf toolchain.' ]]; then
    printf 'unexpected fallback compiler result: status=%s output=%s\n' "$status" "$output" >&2
    exit 1
fi
