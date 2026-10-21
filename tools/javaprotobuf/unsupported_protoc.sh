#!/usr/bin/env bash
printf '%s\n' 'Java conformance requires the pinned protoc 3.25.9 executable; add its execution-platform artifact instead of falling back to the native protobuf toolchain.' >&2
exit 1
