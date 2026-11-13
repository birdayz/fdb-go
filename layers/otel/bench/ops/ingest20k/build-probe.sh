#!/usr/bin/env bash
# Build only. Never execute the probe or change the target filesystem.
set -Eeuo pipefail
if [[ $# != 1 || $1 != /* || $1 == */ || -e $1 || -L $1 ]]; then
    printf 'usage: %s NEW_ABSOLUTE_OUTPUT_DIRECTORY\n' "$0" >&2
    exit 2
fi
out=$1
mkdir -- "$out"
trap 'status=$?; printf "%s\n" "$status" > "$out/build.exit"' EXIT
source_dir=$(CDPATH= cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd -P)
source_file=$source_dir/inode_flags.c
compiler=$(command -v -- "${CC:-cc}")
compiler=$(readlink -f -- "$compiler")
printf '%s\n' "$compiler" > "$out/compiler.path"
sha256sum -- "$source_file" "$source_dir/build-probe.sh" > "$out/source.sha256"
sha256sum -- "$compiler" > "$out/compiler.sha256"
"$compiler" --version > "$out/compiler.version" 2>&1
command=("$compiler" -static -std=c11 -O2 -Wall -Wextra -Werror
         "$source_file" -o "$out/inode-flags")
printf '%q ' "${command[@]}" > "$out/command.sh"
printf '\n' >> "$out/command.sh"
set +e
"${command[@]}" > "$out/compiler.stdout" 2> "$out/compiler.stderr"
status=$?
set -e
printf '%s\n' "$status" > "$out/compiler.exit"
if (( status != 0 )); then
    cat -- "$out/compiler.stderr" >&2
    exit "$status"
fi
sha256sum -- "$out/inode-flags" > "$out/binary.sha256"
readelf -h -l -d -- "$out/inode-flags" > "$out/binary.elf.txt"
if ! grep -q 'Class:.*ELF' "$out/binary.elf.txt" ||
   grep -Eq 'INTERP|\(NEEDED\)' "$out/binary.elf.txt"; then
    printf 'FAIL: not a static ELF\n' > "$out/static-elf.receipt"
    printf 'build-probe: static ELF verification failed\n' >&2
    exit 1
fi
printf 'PASS: ELF; no INTERP program header or NEEDED dynamic entries\n' > "$out/static-elf.receipt"
printf 'probe built (not executed): %s/inode-flags\n' "$out"
