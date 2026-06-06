BUF_VERSION := "1.67.0"

default:
    @just --list

# Ensure buf is installed at pinned version
[private]
ensure-buf:
    #!/usr/bin/env bash
    set -euo pipefail
    BUF=.tools/buf
    if [ -x "$BUF" ] && "$BUF" --version 2>/dev/null | grep -q "{{BUF_VERSION}}"; then
        exit 0
    fi
    mkdir -p .tools
    curl -fsSL -o "$BUF" "https://github.com/bufbuild/buf/releases/download/v{{BUF_VERSION}}/buf-$(uname -s)-$(uname -m)"
    chmod +x "$BUF"

# Regenerate protobuf code + gomock mocks + ANTLR parser. All three are
# committed to git (clean + regenerate, same pattern). CI re-runs
# `just generate` and fails on any diff, so CI is the source of truth.
generate: ensure-buf generate-mocks generate-parser generate-frl
    rm -rf gen/
    .tools/buf generate
    bazelisk run //:gazelle

# Regenerate protobuf code for the `frl` CLI module (separate go.mod, separate
# buf.yaml under cmd/frl/). Output goes to cmd/frl/gen/, consumed by the CLI
# only — never by the library module.
generate-frl: ensure-buf
    rm -rf cmd/frl/gen/
    cd cmd/frl && ../../.tools/buf generate

# Regenerate gomock mocks for api.* interfaces. Cleans mocks_*.go
# first so removed interfaces / renamed files don't leave stale
# mocks behind. Same-package output so tests elsewhere write
# `api.NewMockSchema(ctrl)` with no extra import.
generate-mocks:
    find pkg/relational/api -name 'mocks_*.go' -delete
    go generate ./pkg/relational/api/...
    # go.uber.org/mock is a direct dep of the generated mocks; keep
    # go.mod's direct/indirect tags accurate after each regen.
    go mod tidy

# Smoke-test Relational SQL grammar coverage against the vendored Java
# yamsql corpus (fdb-record-layer/yaml-tests/...). Runs directly via `go
# test` — Bazel's test sandbox can't see the submodule tree. Only the
# files tagged with `//go:build yamsql` are picked up.
smoke-yamsql:
    go test -tags=yamsql -count=1 -v -run TestYamsqlCorpus ./pkg/relational/core/parser

# Regenerate the Relational SQL parser from grammar/*.g4 via Bazel. The
# ANTLR4 complete jar is fetched + cached as a Bazel external repo
# (@antlr4_tool_jar in MODULE.bazel). Version is pinned there — match the
# antlr4-go runtime declared in go.mod. Requires `java` on PATH at run time.
# Runs gazelle after, since generated .go files may have been added/renamed.
generate-parser:
    bazelisk run //pkg/relational/core/parser/grammar:generate_parser
    bazelisk run //:gazelle

# Build all targets (includes nogo lint)
build:
    bazelisk build //...

# Test all targets (includes Go↔Java conformance via the RFC-082 regression
# lock; excludes only the heavy 1M stress tier).
test:
    bazelisk test //... --test_tag_filters=-stress

# Run stress tests (10K/100K rows — exercises FDB transaction limits).
stress:
    bazelisk test //pkg/relational/sqldriver/stress:stress_test --test_timeout=600 --test_output=streamed

# Run conformance server
run-conformance-server:
    bazelisk run //conformance/java:conformance_server

# Run SQL conformance scenarios against the Go fdbsql driver.
# Each .yaml file under pkg/relational/conformance/yamsql/testdata/ pins a
# Java-authoritative correctness property (NULL semantics, CAST rules,
# integer arithmetic, etc.). Drift between Go and Java surfaces here first.
conformance-sql:
    bazelisk test //pkg/relational/conformance/yamsql:yamsql_test --test_output=streamed --test_arg=-test.v

# Regenerate BUILD files after adding/removing Go files or deps
gazelle:
    bazelisk run //:gazelle

# Clean bazel outputs
clean:
    bazelisk clean

# Go mod tidy
tidy:
    go mod tidy

# Format Go source files using Bazel-managed gofumpt (same version as nogo linter)
fmt:
    #!/usr/bin/env bash
    set -euo pipefail
    GOFUMPT=$(bazelisk run --run_under="echo" @cc_mvdan_gofumpt//:gofumpt 2>/dev/null)
    find . -name '*.go' -not -path './fdb-record-layer/*' -not -path './bazel-*' -not -path './gen/*' -not -path './cmd/frl/gen/*' -not -path './pkg/relational/core/parser/gen/*' -not -path './.claude/*' -exec "$GOFUMPT" -w {} +

# Check Go formatting (fails if any file needs formatting)
# Uses Bazel-managed gofumpt — same version as nogo linter, no drift.
lint:
    #!/usr/bin/env bash
    set -euo pipefail
    GOFUMPT=$(bazelisk run --run_under="echo" @cc_mvdan_gofumpt//:gofumpt 2>/dev/null)
    unformatted=$("$GOFUMPT" -l $(find . -name '*.go' -not -path './fdb-record-layer/*' -not -path './bazel-*' -not -path './gen/*' -not -path './cmd/frl/gen/*' -not -path './pkg/relational/core/parser/gen/*' -not -path './.claude/*'))
    if [ -n "$unformatted" ]; then
        echo "Unformatted files:"
        echo "$unformatted"
        echo "Run 'just fmt' to fix."
        exit 1
    fi

# Run all benchmarks (skips Ginkgo specs, runs only Go benchmarks).
# Run all record layer benchmarks (benchtime=3s)
bench:
    #!/usr/bin/env bash
    set -euo pipefail
    bazelisk build //pkg/recordlayer:recordlayer_test
    BAZEL_BIN=$(bazelisk info bazel-bin)
    timeout 300 "$BAZEL_BIN/pkg/recordlayer/recordlayer_test_/recordlayer_test" \
        -test.run='^$' -test.bench=. -test.benchtime=3s -test.benchmem --ginkgo.skip='.*'

# Run a specific benchmark by name regex
bench-one NAME:
    #!/usr/bin/env bash
    set -euo pipefail
    bazelisk build //pkg/recordlayer:recordlayer_test
    BAZEL_BIN=$(bazelisk info bazel-bin)
    timeout 300 "$BAZEL_BIN/pkg/recordlayer/recordlayer_test_/recordlayer_test" \
        -test.run='^$' -test.bench='{{NAME}}' -test.benchtime=3s -test.benchmem --ginkgo.skip='.*'

# Run benchmarks for CI — record layer + wire types, benchtime=1s, results to bench-results.txt.
#
# Individual BenchmarkXxx failures (FDB context-deadline timeouts etc.)
# are surfaced via the GitHub step summary + bench-raw.txt, but don't
# turn CI red — they're frequently FDB-load-dependent flakes and
# blocking on them would make every PR a dice roll. A bench BINARY
# crash (exit != 0 / timeout) IS reported in the run log via the
# `!!! ...` sentinel so it's still visible.
bench-ci:
    #!/usr/bin/env bash
    # No -e: we WANT to keep running when a bench binary fails so all
    # benches produce output. Track exit codes manually.
    set -uo pipefail
    rm -f bench-raw.txt bench-results.txt
    bazelisk build //pkg/recordlayer:recordlayer_test //pkg/fdbgo/wire/types:types_test //pkg/recordlayer/query/plan/cascades:cascades_test //pkg/recordlayer/query/plan/cascades/expressions:expressions_test //pkg/fdbgo/fdb:fdb_test //pkg/recordlayer/keyspace:keyspace_test //pkg/relational/api:api_test //pkg/relational/core/query/plangen:plangen_test
    BAZEL_BIN=$(bazelisk info bazel-bin)
    fail_count=0
    {
        echo "=== Running benchmarks: //pkg/recordlayer:recordlayer_test ==="
        timeout 300 "$BAZEL_BIN/pkg/recordlayer/recordlayer_test_/recordlayer_test" \
            -test.run='^$' \
            -test.bench=. \
            -test.benchmem \
            -test.benchtime=1s \
            --ginkgo.skip='.*' 2>&1
        rc=$?
        if [ "$rc" -ne 0 ]; then
            echo "!!! recordlayer_test bench binary exited with $rc (124 = timeout)"
            fail_count=$((fail_count + 1))
        fi
        echo "=== Running benchmarks: //pkg/fdbgo/wire/types:types_test ==="
        timeout 60 "$BAZEL_BIN/pkg/fdbgo/wire/types/types_test_/types_test" \
            -test.run='^$' \
            -test.bench=. \
            -test.benchmem \
            -test.benchtime=1s 2>&1
        rc=$?
        if [ "$rc" -ne 0 ]; then
            echo "!!! wire/types bench binary exited with $rc (124 = timeout)"
            fail_count=$((fail_count + 1))
        fi
        echo "=== Running benchmarks: //pkg/recordlayer/query/plan/cascades:cascades_test ==="
        timeout 60 "$BAZEL_BIN/pkg/recordlayer/query/plan/cascades/cascades_test_/cascades_test" \
            -test.run='^$' \
            -test.bench=. \
            -test.benchmem \
            -test.benchtime=1s 2>&1
        rc=$?
        if [ "$rc" -ne 0 ]; then
            echo "!!! cascades_test bench binary exited with $rc (124 = timeout)"
            fail_count=$((fail_count + 1))
        fi
        echo "=== Running benchmarks: //pkg/recordlayer/query/plan/cascades/expressions:expressions_test ==="
        timeout 60 "$BAZEL_BIN/pkg/recordlayer/query/plan/cascades/expressions/expressions_test_/expressions_test" \
            -test.run='^$' \
            -test.bench=. \
            -test.benchmem \
            -test.benchtime=1s 2>&1
        rc=$?
        if [ "$rc" -ne 0 ]; then
            echo "!!! expressions_test bench binary exited with $rc (124 = timeout)"
            fail_count=$((fail_count + 1))
        fi
        echo "=== Running benchmarks: //pkg/fdbgo/fdb:fdb_test ==="
        timeout 60 "$BAZEL_BIN/pkg/fdbgo/fdb/fdb_test_/fdb_test" \
            -test.run='^$' \
            -test.bench=. \
            -test.benchmem \
            -test.benchtime=1s 2>&1
        rc=$?
        if [ "$rc" -ne 0 ]; then
            echo "!!! fdb_test bench binary exited with $rc (124 = timeout)"
            fail_count=$((fail_count + 1))
        fi
        echo "=== Running benchmarks: //pkg/recordlayer/keyspace:keyspace_test ==="
        timeout 60 "$BAZEL_BIN/pkg/recordlayer/keyspace/keyspace_test_/keyspace_test" \
            -test.run='^$' \
            -test.bench=. \
            -test.benchmem \
            -test.benchtime=1s 2>&1
        rc=$?
        if [ "$rc" -ne 0 ]; then
            echo "!!! keyspace_test bench binary exited with $rc (124 = timeout)"
            fail_count=$((fail_count + 1))
        fi
        echo "=== Running benchmarks: //pkg/relational/api:api_test ==="
        timeout 60 "$BAZEL_BIN/pkg/relational/api/api_test_/api_test" \
            -test.run='^$' \
            -test.bench=. \
            -test.benchmem \
            -test.benchtime=1s 2>&1
        rc=$?
        if [ "$rc" -ne 0 ]; then
            echo "!!! api_test bench binary exited with $rc (124 = timeout)"
            fail_count=$((fail_count + 1))
        fi
        echo "=== Running benchmarks: //pkg/relational/core/query/plangen:plangen_test ==="
        timeout 60 "$BAZEL_BIN/pkg/relational/core/query/plangen/plangen_test_/plangen_test" \
            -test.run='^$' \
            -test.bench=. \
            -test.benchmem \
            -test.benchtime=1s 2>&1
        rc=$?
        if [ "$rc" -ne 0 ]; then
            echo "!!! plangen_test bench binary exited with $rc (124 = timeout)"
            fail_count=$((fail_count + 1))
        fi
        echo "=== bench-ci summary: $fail_count bench binary failure(s) ==="
    } | tee bench-raw.txt
    # Extract benchmark lines for bench-report.
    grep -E '^(Benchmark|goos:|goarch:|pkg:|cpu:)' bench-raw.txt > bench-results.txt || true
    # grep -c prints "0" AND exits 1 on no-match, so `... || echo 0`
    # appends a second "0" — hence the awkward `; true` pattern to
    # keep the count string clean.
    NRESULTS=$(grep -c '^Benchmark' bench-results.txt 2>/dev/null; true)
    # Surface individual BenchmarkXxx failures — the bench binary may
    # exit 0 even if one Benchmark inside it failed. Previously
    # invisible: `--- FAIL: BenchmarkXxx` lines got silenced by the
    # original `|| true`.
    FAILED_BENCHES=$(grep -cE '^--- FAIL: Benchmark' bench-raw.txt 2>/dev/null; true)
    echo "Benchmarks: $NRESULTS results → bench-results.txt"
    echo "Failing benchmarks: $FAILED_BENCHES (see bench-raw.txt for details)"
    if [ -n "${GITHUB_STEP_SUMMARY:-}" ] && [ "$FAILED_BENCHES" -gt 0 ]; then
        {
            echo "### Benchmark failures (${FAILED_BENCHES})"
            echo ""
            echo '```'
            grep -E '^--- FAIL: Benchmark' bench-raw.txt
            echo '```'
        } >> "$GITHUB_STEP_SUMMARY"
    fi

# Compare benchmark results: just bench-report old.txt new.txt
bench-report old new:
    bazelisk run //cmd/bench-report -- -old {{old}} -new {{new}}

# Run Go vs Java performance comparison benchmark.
# Run Go vs Java performance comparison benchmark
bench-compare:
    #!/usr/bin/env bash
    set -euo pipefail
    bazelisk build //conformance:conformance_test
    BAZEL_BIN=$(bazelisk info bazel-bin)
    # CONFORMANCE_RUN_BENCHMARK=1 un-skips the benchmark (it is Skip'd in the
    # normal merge gate — load-sensitive, doesn't belong there).
    CONFORMANCE_RUN_BENCHMARK=1 timeout 600 "$BAZEL_BIN/conformance/conformance_test_/conformance_test" \
        --ginkgo.focus='Performance Comparison' --ginkgo.v

# Regenerate Go wire types from FDB C++ headers (v5 composable-primitives generator).
# Two Bazel genrules: fdb_cmake_build (cached on FDB version) → generate_wire_types (cached on generator code).
generate-wire-types:
    #!/usr/bin/env bash
    set -euo pipefail
    bazelisk build //cmd/fdb-schema-extract:generate_wire_types
    BAZEL_BIN=$(bazelisk info bazel-bin)
    rm -f pkg/fdbgo/wire/types/*_generated.go
    tar xf "$BAZEL_BIN/cmd/fdb-schema-extract/wire_types.tar" -C pkg/fdbgo/wire/types/ --strip-components=1
    just gazelle
    echo "Generated: $(ls pkg/fdbgo/wire/types/*_generated.go | wc -l) files"

# Regenerate FDB wire schema JSON files (Bazel, sandboxed)
wire-schema:
    bazelisk build //cmd/fdb-wire-schema-generator:gen_schema
    rm -rf pkg/fdbgo/wire/schema
    cp -r bazel-bin/cmd/fdb-wire-schema-generator/schema pkg/fdbgo/wire/schema
    @echo "Updated: $$(ls pkg/fdbgo/wire/schema/*.json | wc -l) schemas"

# Regenerate FDB test vectors (Docker, real FDB ObjectWriter)
wire-testvecs:
    bazelisk build //cmd/fdb-wire-schema-generator:gen_schema
    bash cmd/fdb-wire-schema-generator/docker_build.sh \
        $$(bazelisk info output_base)/external/foundationdb+ \
        bazel-bin/cmd/fdb-wire-schema-generator/generated_messages.cpp \
        /tmp/testvecs_docker
    rm -f pkg/fdbgo/wire/testdata/[A-Z]*.json
    cp /tmp/testvecs_docker/*.json pkg/fdbgo/wire/testdata/
    @echo "Updated: $$(ls pkg/fdbgo/wire/testdata/[A-Z]*.json | wc -l) test vectors"

# Run FDB binding tester (stack machine conformance via Bazel + testcontainers).
binding-test:
    bazelisk test //cmd/fdb-stacktester/bindingtester:bindingtester_test --test_output=streamed

# Binding tester stress: N seeds × M ops (report + logs → binding-stress-out/)
binding-stress runs="100" ops="1000":
    bazelisk run //cmd/fdb-binding-stress -- -seeds {{runs}} -ops {{ops}}

# Binding tester stress for a duration (e.g. 2h, 30m)
binding-stress-duration duration ops="1000":
    bazelisk run //cmd/fdb-binding-stress -- -duration {{duration}} -ops {{ops}}

# Directory layer binding stress: N seeds × M ops
binding-stress-directory runs="50" ops="500":
    bazelisk run //cmd/fdb-binding-stress -- -seeds {{runs}} -ops {{ops}} -test-name directory

# Generate HTML test report from the latest Bazel test run.
# Reads .bazel-bep.jsonl (produced automatically by every `just test` via .bazelrc).
# Builds the tool then runs it outside the sandbox (needs access to BEP + test.xml files).
report:
    #!/usr/bin/env bash
    set -euo pipefail
    if [ ! -f .bazel-bep.jsonl ]; then
        echo "error: .bazel-bep.jsonl not found — run 'just test' first" >&2
        exit 1
    fi
    bazelisk build //cmd/test-report
    BAZEL_BIN=$(bazelisk info bazel-bin)
    "$BAZEL_BIN/cmd/test-report/test-report_/test-report" .bazel-bep.jsonl > test-report.html
    TOTAL=$(grep 'stat-total' test-report.html | grep -oP '>\K\d+(?=</span>)' || echo '?')
    echo "Report: test-report.html ($TOTAL tests)"

# Run tests with coverage and generate HTML report
coverage:
    #!/usr/bin/env bash
    set -euo pipefail
    bazelisk coverage //... --combined_report=lcov
    LCOV=$(bazelisk info output_path)/_coverage/_coverage_report.dat
    bazelisk build //cmd/test-report
    BAZEL_BIN=$(bazelisk info bazel-bin)
    "$BAZEL_BIN/cmd/test-report/test-report_/test-report" -coverage "$LCOV" .bazel-bep.jsonl > test-report.html
    TOTAL=$(grep 'stat-total' test-report.html | grep -oP '>\K\d+(?=</span>)' || echo '?')
    COV=$(grep -oP 'Coverage</span></div>' test-report.html || true)
    COV_PCT=$(grep -oP 'style="color: #[0-9a-f]+">\K[0-9.]+%' test-report.html | head -1 || echo '?')
    echo "Report: test-report.html ($TOTAL tests, $COV_PCT coverage)"

# Run client tests with race detector (slower — recompiles with instrumentation)
race:
    bazelisk test //pkg/fdbgo/client:client_test --@rules_go//go/config:race --test_timeout=300

# Run all tests with race detector (~3 min, full recompile + instrumentation)
race-all:
    bazelisk test //pkg/fdbgo/client:client_test //pkg/recordlayer:recordlayer_test //pkg/fdbgo/fdb:fdb_test //pkg/recordlayer/chaos:chaos_test //conformance:conformance_test --@rules_go//go/config:race --test_timeout=900

# Full pre-merge verification: build + test + race detector + fuzz smoke test.
# Run this before requesting PR merge. Takes ~3 minutes on a warm cache.
verify:
    #!/usr/bin/env bash
    set -euo pipefail
    echo "=== Build + lint + test ==="
    just test
    echo "=== Race detector (5 targets) ==="
    just race-all
    echo "=== Fuzz smoke (3 targets, 10s each) ==="
    bazelisk run //pkg/recordlayer:recordlayer_test -- \
        -test.run='^$' -test.fuzz='^FuzzFastUnpack$' \
        -test.fuzzcachedir=/tmp/fuzz_verify -test.fuzztime=10s 2>&1 | tail -1
    bazelisk run //pkg/recordlayer:recordlayer_test -- \
        -test.run='^$' -test.fuzz='^FuzzDeserializeBunch$' \
        -test.fuzzcachedir=/tmp/fuzz_verify -test.fuzztime=10s 2>&1 | tail -1
    bazelisk run //pkg/fdbgo/client:client_test -- \
        -test.run='^$' -test.fuzz='^FuzzRYWCache$' \
        -test.fuzzcachedir=/tmp/fuzz_verify -test.fuzztime=10s 2>&1 | tail -1
    echo "=== All verification passed ==="

# Install pre-commit hook (lint + gazelle + build + test)
install-hooks:
    #!/usr/bin/env bash
    set -euo pipefail
    cat > .git/hooks/pre-commit << 'HOOK'
    #!/usr/bin/env bash
    set -euo pipefail
    echo "Running pre-commit: just lint && just gazelle && just build && just test"
    just lint && just gazelle && just build && just test
    HOOK
    chmod +x .git/hooks/pre-commit
    echo "Pre-commit hook installed."

# Run a specific test with forced rebuild (no stale binary)
test-fresh target *args:
    bazelisk test {{target}} --cache_test_results=no {{args}}

# Convenience wrapper for the frl CLI (Phase A skeleton — see cmd/frl/).
# Example: `just frl version` or `just frl config schema`.
frl *args:
    bazelisk run //cmd/frl -- {{args}}
