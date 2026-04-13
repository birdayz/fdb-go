# RFC 018: Benchmark Reports in CI

## Status: Implemented

## Problem

We have 63 benchmarks across 6 packages but no CI pipeline to track performance over time. Regressions can silently land. The only defense is manual `just bench` runs and eyeballing numbers against the table in CLAUDE.md.

We want:
1. Every CI run produces machine-readable benchmark results
2. PRs get an automatic comparison against master baseline
3. Results are archived alongside test reports

## Benchmark Inventory

| Package | Benchmarks | Needs FDB? | ~Time |
|---|---|---|---|
| `pkg/recordlayer/benchmark_test.go` | 15 | Yes | ~50s |
| `pkg/recordlayer/store_test.go` | 14 (proto marshal) | No | <1s |
| `pkg/recordlayer/store_generic_test.go` | 1 (typed overhead) | No | <1s |
| `pkg/fdbgo/bench/bench_test.go` | 12 (client ops) | Yes | ~30s |
| `pkg/fdbgo/client/ryw_test.go` | 6 (RYW merge) | No | <1s |
| `pkg/fdbgo/client/bench_test.go` | 2 (PureGo vs CGo) | Yes | ~10s |
| `pkg/fdbgo/wire/types/bench_test.go` | 12 (wire serde) | No | <1s |
| `pkg/recordlayer/bench/vector_benchmark_test.go` | 5 (HNSW) | Yes | ~120s |
| **Total** | **67** | | |

FDB-dependent benchmarks need a testcontainer (~5s startup each package). Vector benchmarks are slow (large dataset inserts). Everything else is sub-second CPU work.

## Proposed Design

### 1. `just bench-ci` recipe

New justfile recipe that:
- Runs all benchmarks across all packages (except vector — too slow, nightly only)
- Captures output in Go's standard benchmark format
- Writes to `bench-results.txt`

```bash
just bench-ci  # ~90s total
```

Implementation:
```
bench-ci:
    #!/usr/bin/env bash
    set -euo pipefail
    TARGETS=(
        //pkg/recordlayer:recordlayer_test
        //pkg/fdbgo/bench:bench_test
        //pkg/fdbgo/client:client_test
        //pkg/fdbgo/wire/types:types_test
    )
    for target in "${TARGETS[@]}"; do
        bazelisk test "$target" \
            --test_arg="-test.bench=." \
            --test_arg="-test.benchmem" \
            --test_arg="-test.benchtime=3s" \
            --test_arg="--ginkgo.skip=.*" \
            --test_output=all \
            --nocache_test_results \
            --test_timeout=300 2>&1
    done | tee bench-raw.txt
    # Extract benchmark lines + headers for benchstat
    grep -E '^(Benchmark|goos:|goarch:|pkg:|cpu:)' bench-raw.txt > bench-results.txt
    echo "Benchmarks: $(grep -c '^Benchmark' bench-results.txt) results"
```

### 2. CI workflow additions

Add a benchmark step to `ci.yml` after the test step:

```yaml
- name: Run benchmarks
  run: just bench-ci

- name: Upload benchmark results
  if: always()
  uses: actions/upload-artifact@v4
  with:
    name: bench-results
    path: bench-results.txt
    retention-days: 90
```

For master pushes, also upload to S3 as baseline:
```yaml
- name: Upload master baseline
  if: github.ref == 'refs/heads/master'
  run: mc cp bench-results.txt "hetzner/$S3_BUCKET/bench/master-latest.txt"
```

### 3. PR comparison with benchstat

On PRs, download the master baseline and run `benchstat`:

```yaml
- name: Compare benchmarks vs master
  if: github.event_name == 'pull_request'
  run: |
    # Download master baseline
    mc cp "hetzner/$S3_BUCKET/bench/master-latest.txt" master-bench.txt || true
    if [ -f master-bench.txt ]; then
      go install golang.org/x/perf/cmd/benchstat@latest
      benchstat master-bench.txt bench-results.txt > bench-comparison.txt 2>&1 || true
      echo "## Benchmark Comparison" >> $GITHUB_STEP_SUMMARY
      echo '```' >> $GITHUB_STEP_SUMMARY
      cat bench-comparison.txt >> $GITHUB_STEP_SUMMARY
      echo '```' >> $GITHUB_STEP_SUMMARY
    fi
```

This posts the comparison to the GitHub Actions step summary. No PR comment machinery needed — the summary is linked from the checks tab.

### 4. benchstat output format

benchstat gives a clear regression/improvement table:

```
                          │ master.txt │           branch.txt           │
                          │   sec/op   │   sec/op    vs base            │
SaveRecord-24               2.176m ± 3%   2.190m ± 2%  ~ (p=0.421 n=5)
LoadRecord-24               410.9µ ± 4%   398.2µ ± 3%  -3.09% (p=0.032 n=5)
ScanRecords/100-24          624.9µ ± 5%   680.1µ ± 4%  +8.83% (p=0.008 n=5)
```

Statistically significant regressions show `+X%`. Improvements show `-X%`. Noise shows `~`.

### 5. Vector benchmarks (nightly)

HNSW vector benchmarks are slow (~2 min) and noisy due to dataset construction. Run them in the existing nightly-fuzz workflow or a separate nightly-bench workflow.

### 6. What we explicitly DON'T do

- **No HTML benchmark report.** benchstat text output is sufficient. Adding an HTML generator is wasted effort when the text is machine-readable and human-readable.
- **No PR comments.** GitHub step summary is discoverable enough. PR comment bots are noisy and need token management.
- **No historical graphing.** The S3 archive gives us point-in-time snapshots. If we need time-series later, we can build a tool that reads them.
- **No benchtime tuning per benchmark.** 3s across the board. Some benchmarks may only get 1-2 iterations — that's fine for regression detection. benchstat handles low sample counts gracefully.

## Time Budget

| Step | Time |
|---|---|
| recordlayer benchmarks (15) | ~50s |
| client bench (12) | ~30s |
| wire types (12) | <1s |
| ryw merge (6) | <1s |
| client bench_test (2) | ~10s |
| proto marshal (14+1) | <1s |
| benchstat comparison | <1s |
| **Total** | **~95s** |

Well within the 10-minute budget. Container startup is amortized across benchmarks in the same package.

## Migration Path

1. Add `just bench-ci` recipe
2. Add benchmark step to CI (runs on every push/PR, ~90s overhead)
3. First master push establishes baseline
4. PRs get comparison automatically from then on

No changes to existing benchmark code or test infrastructure needed.

## Open Questions

- **benchtime=3s vs default?** 3s gives 1-5 iterations for FDB benchmarks, ~1000+ for CPU-only. Good enough for regression detection. Could increase to 5s for more stable FDB results (adds ~30s).
- **Separate CI job?** Could run benchmarks in parallel with tests (separate job). Saves wall time but uses more runner resources. Current proposal: sequential in the same job.
- **PR comment vs step summary?** Step summary is simpler. PR comments are more visible. Starting with step summary, can add comments later if needed.
