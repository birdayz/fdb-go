# Repository instructions

Read `CLAUDE.md` for the project rules; it is the shared engineering guide.

## Efficient validation

**Never run `go test`.** Not for a package, not for a single test, not for a
quick check, not with `-run`. Every test run goes through Bazel: `just test`,
`just test-full`, or `bazelisk test` on a target. This holds for focused
debugging too; use Bazel's `--test_arg`/`--test_filter` when you must narrow.

**Run tests with `just test`. Do not pick targets yourself** with `bazelisk test
//pkg/...:foo_test`, `--test_filter`, or `go test`. Bazel's cache already
reruns exactly the targets your change affects, dependents included, and
answers the rest from cache; a hand-picked target list misses affected
dependents and is not the check the hook and CI make.

- `just test`: the standard fast edit/commit loop. Bazel runs nogo, unit tests,
  and bounded integration tests. Targets tagged `test-full`, `conformance_java`,
  or `stress` are excluded; `manual` targets are excluded by Bazel's wildcard.
- `just test-full`: all Bazel test targets, including manual targets. This adds
  the heavyweight client, Java conformance, chaos, SQL integration/rowdiff,
  planner-sweep, full-corpus, and stress/scale suites. Requires Docker and the
  external data/toolchain dependencies declared by those targets.
- Full means all targets at their default budgets, not every scheduled execution
  mode. CI/nightlies retain extended seed, race, and active-fuzz configurations;
  `just verify` adds the existing race and fuzz-smoke checks to the full lane.
- Complete a coherent workstream before running the suite. Do not rerun it after
  every small edit. If a touched regression target is outside the fast lane
  (tagged `test-full`/`conformance_java`/`stress`/`manual`), run `just test-full`,
  which is cached the same way; use it at workstream/PR boundaries too.
- The pre-commit hook retains secret scanning and the dirty-tree guard, then runs
  `just test` without redundant generation/lint/build passes. Codegen drift stays
  in CI; run `just generate` for codegen-input changes and `just install-hooks`
  to update an existing clone's hook.
- The fast lane uses `--build_tests_only` to avoid unrelated wildcard builds.
  Keep Bazel caching enabled. Do not substitute standalone `go test`, `go vet`,
  or `go get` for this module's just/Bazel validation. Nogo already runs during
  compilation. Standalone nested modules have their own documented CI commands.
- Classify heavyweight targets using the `test-full` BUILD tag, not test skips
  or global build-tag changes. New ordinary targets enter the fast lane by default.
- Report which lane ran, failures, and actual versus cached execution. A fast pass
  must never be described as full-suite verification.
