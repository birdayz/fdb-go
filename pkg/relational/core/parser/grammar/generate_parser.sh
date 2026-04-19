#!/usr/bin/env bash
# Regenerate the Go ANTLR parser from the .g4 grammar files.
#
# Invoked via `bazelisk run //pkg/relational/core/parser/grammar:generate_parser`
# (usually through `just generate-parser`). Writes into the source tree via
# $BUILD_WORKSPACE_DIRECTORY, so it is NOT hermetic — that's intentional:
# the generated files live in git, and CI enforces drift-freeness via
# `just generate && git diff --exit-code`.
#
# Requires `java` on PATH at run time (the ANTLR complete jar is a standalone
# executable we just `java -jar` against; we don't bring our own JDK).

set -euo pipefail

# --- Bazel runfiles boilerplate ----------------------------------------------
# https://github.com/bazelbuild/bazel/blob/master/tools/bash/runfiles/runfiles.bash
# shellcheck disable=SC1090,SC1091
if [[ -z "${RUNFILES_DIR:-}" && -z "${RUNFILES_MANIFEST_FILE:-}" ]]; then
    if [[ -f "${BASH_SOURCE[0]}.runfiles_manifest" ]]; then
        export RUNFILES_MANIFEST_FILE="${BASH_SOURCE[0]}.runfiles_manifest"
    elif [[ -d "${BASH_SOURCE[0]}.runfiles" ]]; then
        export RUNFILES_DIR="${BASH_SOURCE[0]}.runfiles"
    fi
fi
if [[ -f "${RUNFILES_DIR:-/dev/null}/bazel_tools/tools/bash/runfiles/runfiles.bash" ]]; then
    source "${RUNFILES_DIR}/bazel_tools/tools/bash/runfiles/runfiles.bash"
elif [[ -f "${RUNFILES_MANIFEST_FILE:-/dev/null}" ]]; then
    source "$(grep -m1 "^bazel_tools/tools/bash/runfiles/runfiles.bash " \
        "${RUNFILES_MANIFEST_FILE}" | cut -d' ' -f2-)"
else
    echo "ERROR: cannot locate runfiles helpers" >&2
    exit 1
fi
# -----------------------------------------------------------------------------

if [[ -z "${BUILD_WORKSPACE_DIRECTORY:-}" ]]; then
    echo "ERROR: BUILD_WORKSPACE_DIRECTORY is not set." >&2
    echo "This target must be invoked via 'bazelisk run', not 'bazelisk test' or 'build'." >&2
    exit 1
fi

if ! command -v java >/dev/null 2>&1; then
    echo "ERROR: 'java' not found on PATH. Install a JDK (17+) and retry." >&2
    exit 1
fi

JAR="$(rlocation antlr4_tool_jar/file/antlr-4.13.1-complete.jar)"
LEXER_G4="$(rlocation _main/pkg/relational/core/parser/grammar/RelationalLexer.g4)"
PARSER_G4="$(rlocation _main/pkg/relational/core/parser/grammar/RelationalParser.g4)"

for f in "$JAR" "$LEXER_G4" "$PARSER_G4"; do
    if [[ ! -f "$f" ]]; then
        echo "ERROR: runfile missing: $f" >&2
        exit 1
    fi
done

OUT_DIR="${BUILD_WORKSPACE_DIRECTORY}/pkg/relational/core/parser/gen"

# ANTLR resolves the parser grammar's `options { tokenVocab=RelationalLexer; }`
# by reading RelationalLexer.tokens from the current working directory (or from
# -lib). It also writes its outputs relative to the -o dir from the CWD. Doing
# everything in a staging dir keeps the invocation identical to how the
# justfile historically ran it (cd into grammar dir, run ANTLR twice) while
# keeping the source tree untouched until the final move.
STAGE="$(mktemp -d)"
trap 'rm -rf "$STAGE"' EXIT

cp "$LEXER_G4"  "$STAGE/RelationalLexer.g4"
cp "$PARSER_G4" "$STAGE/RelationalParser.g4"

STAGE_OUT="$STAGE/gen"
mkdir -p "$STAGE_OUT"

cd "$STAGE"
# Lexer first; produces RelationalLexer.tokens in $STAGE_OUT.
java -jar "$JAR" -Dlanguage=Go -package antlrgen -o "$STAGE_OUT" RelationalLexer.g4
# Parser reads the lexer's .tokens via `-lib $STAGE_OUT`.
java -jar "$JAR" -Dlanguage=Go -package antlrgen -visitor -lib "$STAGE_OUT" -o "$STAGE_OUT" RelationalParser.g4

# Preserve the committed BUILD.bazel (gazelle-managed) across regen.
BUILD_BAZEL=""
if [[ -f "$OUT_DIR/BUILD.bazel" ]]; then
    BUILD_BAZEL="$(mktemp)"
    cp "$OUT_DIR/BUILD.bazel" "$BUILD_BAZEL"
fi

rm -rf "$OUT_DIR"
mkdir -p "$OUT_DIR"
# Use `cp -a` plus an explicit glob so the STAGE_OUT itself isn't nested.
cp -a "$STAGE_OUT"/. "$OUT_DIR"/

if [[ -n "$BUILD_BAZEL" ]]; then
    cp "$BUILD_BAZEL" "$OUT_DIR/BUILD.bazel"
    rm -f "$BUILD_BAZEL"
fi

echo "Generated parser files in $OUT_DIR"
echo "Run 'bazelisk run //:gazelle' if any Go files were added/removed."
