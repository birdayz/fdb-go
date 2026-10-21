#!/usr/bin/env bash
set -euo pipefail
cd "${TEST_SRCDIR}/${TEST_WORKSPACE}"
export PYTHONUTF8=1
gofumpt="$PWD/$1"
norm="$PWD/$2"
source_zip="$PWD/$3"
python3 tools/unicode15gen/properties.py \
  --data internal/unicode15/ucd --formatter "$gofumpt" \
  --output "${TEST_TMPDIR}/tables.go"
cmp internal/unicode15/tables.go "${TEST_TMPDIR}/tables.go"
"$norm" -data "$PWD/internal/unicode15/ucd" \
  -output "${TEST_TMPDIR}/norm_tables.go"
cmp internal/unicode15/text/unicode/norm/tables15.0.0.go "${TEST_TMPDIR}/norm_tables.go"
python3 internal/unicode15/text/snapshot.py --source "$source_zip" \
  --gofumpt "$gofumpt" --root internal/unicode15/text --check
printf 'Unicode 15 regeneration: properties, normalization and 48-file runtime snapshot unchanged\n'
