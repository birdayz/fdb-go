# FoundationDB Record Layer — Go Port

## Project scope and references

Port the FoundationDB Record Layer to Go so Go and Java applications can share
record stores. Java Record Layer / Relational **4.14.2.0** is the reference for
record-layer and shared SQL behavior; FoundationDB C++ **7.3.77** is the reference
for the pure-Go client's protocol and transaction semantics. Read the corresponding
upstream implementation before changing behavior. The Java checkout is at
`fdb-record-layer/` (gitignored); dependency versions are pinned in `MODULE.bazel`.

Persisted keys, records, indexes, versions, headers and supported continuations
must match the declared Java target. Read-side extensions that Java does not
implement are allowed when they preserve wire compatibility and have end-to-end
coverage. Do not call a feature a parity gap without checking Java supports it.
Document compatibility boundaries in `docs/compatibility.md` and `DIVERGENCES.md`.

## Working rules

- Work within the owner's assigned scope and respect explicit pauses and decisions.
- Investigate failures and flakes; do not dismiss them as unrelated or fix them by
  rerunning CI. Reproduce the defect, identify its cause and add a regression.
- Do not use `t.Skip()` to defer a failing test. The runtime Docker availability
  check (`FDB not available (no Docker)`) is the allowed skip.
- Finish a coherent change before moving to another. Surface genuine external
  blockers directly, with evidence and the decision or capability needed.
- Keep commit messages to ten lines or fewer.
- Stage explicit paths. Do not use `--no-verify` or `git stash` (stash state is
  shared across worktrees). Preserve other agents' work.
- Read both sides of every merge conflict. One side may correct the other;
  retaining both is not always a valid resolution.
- Delegate separable work with paths and context. Do not run two large
  implementation tasks concurrently.

`TODO.md` is the durable work list. Record genuinely deferred work there, not
only in conversation state; do not bury an open finding in a completed entry.
Append self-contained entries to shared sections to reduce concurrent conflicts.
A completed item needs implemented behavior and a regression that exercises it
end to end, not merely a type, rule or plan that exists.

## Validation

Read `AGENTS.md` for the validation lanes and PR rules. Use **`just test`**, not
`go test`, `go vet`, a hand-picked Bazel test list or a test filter; a single-target race
or repeat run to prove one test is allowed (`AGENTS.md`). Bazel's cache
is enabled: report executed and cached results separately. `just test` is the
fast lane, not full-suite verification. Heavyweight, Java conformance, stress and
manual targets belong to `just test-full`; run that lane in CI when local full
runs are prohibited by the owner. Do not force uncached tests to manufacture
verification.

- Use `bazelisk`, not a system `bazel`. Nogo runs during compilation.
- Run `just gazelle` after adding Go/test files and verify new tests are declared
  inputs of their Bazel targets. Run `bazelisk mod tidy` for dependency changes.
- Run `just generate` for codegen inputs. Proto generation uses buf.
- Freeze the tracked and untracked input tree around a run and check it afterward;
  a pass before the last edit does not verify the committed bytes.
- Confirm a gate has a nonempty population before reading its verdict. Empty
  checks, missing test inputs, a build failure or a run still in progress is not
  a passing test. Capture complete probe output when output is the evidence.
- A proof used to justify a behavior change becomes a committed test, including
  negative results and the exact shape that reproduced a bug.
- Census/guard helpers need unit tests driving every arm and the vacuity checks.
  Check values as well as relationships when both sides share a derivation.
- A mutation test needs evidence that the mutation applied, compiled and changed
  the expected result. Do not infer this from an empty failure search.

Use real FoundationDB via testcontainers for integration tests. Tests should
call `t.Parallel()` and isolate key prefixes/subspaces and mutable state; keep
exclusive tests exclusive where sharing is unsafe. Bound container setup with a
context timeout (normally two minutes). Do not run binding stress concurrently
with the fast lane. Respect the owner's shared-machine resource limits and locks.

For SQL-visible changes, pin end-to-end behavior in yamsql or the SQL integration
suite under `pkg/relational/sqltest/<area>` (helpers in `sqltest/testkit`). For
planner optimizations assert the plan shape with `EXPLAIN`, not just the rows a
fallback can also return. Record-layer internals need FDB integration coverage.

## Evidence and measurements

State the population behind every count and measurement. Quote search globs and
use a positive control before claiming a zero proves absence. Distinguish lines
from occurrences. When a claim is superseded, search for its other copies.

A profile measures cost; a census only locates work. Performance comparisons
must identify both trees, compiler, workload, sample sizes and measurement scope.
Use the merge-base as the baseline, the same filesystem and toolchain, and
sequential runs under comparable load. Re-measure rather than extrapolating from
a confound. A historical result is not a claim about current master. Benchmark
success does not establish correctness or authorize a merge.

## Code and design

- Preserve Java's architectural behavior on the shared surface, not only one
  observed output. Do not add surface-level string checks to hide a deeper bug.
- Use typed ANTLR/value nodes, not string matching on SQL or `GetText()` output.
- Library errors are explicit error types, not panics or bare sentinels. Match
  Java exception classes with Go structs carrying the relevant log context;
  use `errors.As` and preserve wrapping with `%w`.
- Keep code simple. Prefer direct implementations to premature abstractions.
- Preserve protobuf field presence, open enums and unknown fields. Use the
  complete upstream proto definitions; `proto/apple/` mirrors Java's protos.
- Comments explain a non-obvious reason in one or two lines. Do not add reviewer
  names, review rounds, shift tags or measurement narratives to code comments.
- This file contains general rules, not specific findings or resolved history.
  Findings belong in `TODO.md`, compatibility docs or the relevant fix site.

## Wire types and store boundaries

Do not hand-write FDB wire structs in `pkg/fdbgo/wire/types/`. Register types in
`cmd/fdb-schema-extract/extract.h` and `main.cpp`, then run
`just generate-wire-types && just gazelle`. `keyrangeref_custom.go` is the
existing documented exception.

Match Java's tuple/subspace keys, protobuf records, store headers, index entries,
split records (100 KB chunks at suffixes 1+, unsplit at 0), and inline versions
at `pk + -1` for format version 6 and later. FoundationDB constrains transaction
age (5 s), values (100 KB), transactions (10 MB) and keys (about 10 KB). Cursors
need scan/time limiters and continuations; large values need split records.

## Repository map and tooling

- `pkg/fdbgo/`: pure-Go FoundationDB client, transport and differentials.
- `pkg/recordlayer/`: records, indexes, cursors and query planning.
- `pkg/relational/`: embedded SQL engine, driver and cross-engine tests.
- `conformance/`: Java record-layer harness.
- `gen/`, `proto/apple/`: generated Go and upstream protobuf definitions.
- `.claude/`: retained agent skills; `AGENTS.md`: repository workflow rules.
- `.bruh/review/`: lenses and config for the `bruh-code-review` skill, installed with bruh
  (`just install` or `just install-skills` in the bruh repository puts it in `~/.bruh/skills`).
- `STATUS.md`: current readiness; `RELEASE.md`: release gates.

Development uses Go, Bazel via bazelisk, gazelle, nogo, buf, just and Docker.
Common commands are `just build`, `just test`, `just gazelle`, `just generate`,
`just bench` and `just binding-stress`. Consult the recipes and target inputs
rather than treating historical counts or shift notes as current evidence.
