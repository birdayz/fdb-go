# FoundationDB Record Layer — Go Port

## ABSOLUTE PRIME DIRECTIVE: NO SKIPS

**NEVER use t.Skip() to defer a failing test.** If a test fails, FIX IT. Immediately. No matter how long it takes, no matter how deep the rabbit hole goes. Skipping is forbidden. The only acceptable t.Skip is the runtime Docker check (`FDB not available (no Docker)`). Every other skip is a bug you're hiding. Hunt it down. Fix it. Principles first.

## THERE ARE NO UNRELATED FLAKES — EVERY FLAKE GETS INVESTIGATED NOW

**A flaky or intermittently-failing test is a REAL BUG, never noise to wave away.** "Unrelated flake," "just rerun CI," "not caused by my change," "known-flaky" are all FORBIDDEN rationalizations. A red CI is red — full stop. The moment any test fails or flakes (in CI, in `just test`, in a stress/fuzz run, anywhere), STOP and root-cause it immediately, even if it lives in a package your change never touched. Flakes are how real concurrency bugs, conflict-handling gaps, ordering assumptions, and resource leaks hide in plain sight — a transaction-conflict (`not_committed 1020`), a timeout, a watch that doesn't fire, a nondeterministic plan: each is a latent defect, not bad luck. Reproduce it (run it in a loop / under `--runs_per_test` / with `-race`), find the actual race or assumption, fix the code (or the test if the test's timing assumption is genuinely wrong — but prove that first), and pin it with a deterministic regression. Never merge or hand off with CI red or a flake outstanding. "It passed the second time" is not a fix; it's the bug still there.

## NO EXCUSES — DO THE WORK

**Nothing is "gated," "blocked," or "multi-shift."** If a TODO exists, DO IT. Now. No matter how long it takes. Don't rationalize deferral with "needs Java conformance server," "blocked on upstream," or "multi-shift effort." Those are excuses. Read the Java source, understand the algorithm, port it, test it, ship it. If it takes 826 lines, write 826 lines. If it takes all night, work all night. The TODO list is a list of things TO DO, not a list of things to punt.

**100% Java alignment or it's a bug.** Every component must match Java's behavior exactly. Go-only extensions are fine, but they need DEEP test coverage. If Java has it and Go doesn't, that's not "low priority" — it's missing functionality. Port it.

**Wire compat is the hard line; query reach is not.** Two distinct axes, don't conflate them:
1. **Anything touching the wire** — key encoding, record/index/version format, continuations, what gets written to FDB — MUST match Java exactly. Divergence there is a bug, full stop. This is the whole point: Go and Java apps share a cluster and read/write each other's records.
2. **The read-side query surface MAY go beyond Java.** Net-new query capabilities Java lacks *entirely* (new join flavours, operators, syntax) are welcome — provided (a) wire compat is never sacrificed (Java still reads/writes the exact same records; the extension only lets Go *express* more), and (b) the extension has deep test coverage. "Doing better than Java" on the read path is encouraged, not suspect.

Before treating a TODO "vs Java gap" as parity work, **verify Java actually supports it.** A TODO line can be stale (the feature may already work — e.g. via a normalization the item's author missed) or mis-framed (Java may not support it *at all*, in which case you're adding an allowed extension, not closing a divergence — and the conformance principle below does not forbid it).

## DFS, NOT BFS — GO ALL IN ON EVERY PROBLEM

**When you discover a problem, go ALL IN.** Dig into the rabbit hole. Fix it completely. No matter how long it takes, no matter how deep it goes. Don't "skip this and look for quick wins" — that's BFS thinking and it produces shallow, fragile work. DFS: pick the problem, understand it fully by reading Java first, then fix it properly in Go. One problem at a time, fixed to completion.

**Java is the reference. Always.** Before writing ANY fix, read the corresponding Java code. Understand how Java handles the exact same case. Then port that approach to Go. No invented shortcuts, no "pragmatic alternatives," no "we'll do this differently." If Java uses SemanticAnalyzer.resolveIdentifier, Go uses the semantic scope. If Java walks the ANTLR tree with typed visitors, Go walks the ANTLR tree with typed visitors. 1:1.

**Never paper over a problem.** If a test fails, the fix is in the code, not in the test expectations. If an error code is wrong, trace it to the root cause — don't add a string check at the surface. If a column doesn't resolve, fix the resolution infrastructure — don't strip qualifiers with string hacks.

**"For now" is a red flag.** If you're about to write "for now" or "pragmatic approach" or "we'll fix this later" — STOP. That means you're about to create technical debt. Either do it properly or document it as the FIRST priority in TODO.md so it gets done next. No deferred hacks.

## NO FAKE CHECKBOXES — E2E OR IT'S NOT DONE

**A TODO item is done when a SQL query exercises it end-to-end and a test pins the behavior.** "Plan type exists" is not done. "Rule ported but can't fire" is not done. "Infrastructure exists but SQL can't trigger it" is not done. If a user can't write a SQL query that hits the code path and gets the right answer, the checkbox stays unchecked.

The proof is a **yamsql scenario** (for SQL-visible features) or an **FDB integration test** (for record-layer internals). The test must demonstrate the OPTIMIZATION actually fires — not just that the query returns correct results via a slower fallback. For planner optimizations, use `EXPLAIN` assertions to verify the expected plan shape:

```yaml
# GOOD: proves aggregate index scan fires
- query: SELECT status, COUNT(*) FROM orders GROUP BY status
  plan_contains: AggregateIndexScan
  rows:
    - [delivered, 2500]
    - [pending, 2500]

# BAD: just proves GROUP BY returns correct results (could be full scan)
- query: SELECT status, COUNT(*) FROM orders GROUP BY status
  rows:
    - [delivered, 2500]
    - [pending, 2500]
```

If you can't write the e2e test, the feature isn't done. Period.

## NO TEXT MATCHING ON SQL / PARSE TREES

**NEVER detect SQL features by string-matching on SQL text or GetText() output.** The ANTLR parse tree has typed nodes — use them. `strings.Contains(sql, "CROSS JOIN")` is forbidden. `GetText()` concatenates tokens without whitespace and produces garbage like `labelISDISTINCTFROMnull`. Magic length limits (`lparen > 12`) are fragile trash that breaks on `CHARACTER_LENGTH`. Walk the parse tree or Value tree. If you need to detect a function call, find `FunctionCallExpressionAtomContext` / `ScalarFunctionValue` in the tree — don't regex the text.

## QUERY-ENGINE CHANGES REQUIRE A GRAEFE ACK — RFC AND IMPL

**Any change to the Cascades query engine (planner, optimizer, cost model, matching/data-access infra, physical wrappers, executor) needs a Graefe ACK on BOTH the RFC and the implementation before merge. Never merge a query-engine change Graefe hasn't reviewed.** Torvalds + @claude + codex are the other gates — don't ship with a NAK from any; re-request after every commit (an ACK only covers the HEAD it reviewed). Holds always-on, not just when a skill is loaded; mechanics in `.claude/skills/query-engine/` (impl) and `.claude/skills/todo-worker/` (RFC). PR #201 shipped a latent 0-row planner bug because it skipped Graefe.

---

Port `fdb-record-layer-core` from Java to Go with full wire compatibility — Go and Java apps must read/write each other's records and share the same FDB cluster. SQL/relational layer features (UDFs, views, synthetic record types, fdb-relational-*) are out of scope unless a TODO entry calls for them; protobuf round-trips them via unknown-field preservation.

## Keep this file general

This file is for **general instructions only** — project goals, testing rules, design principles, working rhythm. Specific bug findings, gotcha lists, resolved/retracted history, per-shift state belong in:
- TODO.md (open issues)
- `shifts/*.md` handovers (history)
- Code comments at the relevant fix site

If you're tempted to add a 5-line note explaining a divergence, write it as a code comment at the call site instead.

**Never put shift tags in code comments.** No `nightshift-65`, no `swingshift-64`, no `landed in shift X`. Shift refs rot the moment the codebase outlives the shift naming scheme and they leak ephemeral process state into permanent files. Code comments explain WHY the code is the way it is — not WHEN it got there or WHO did it. That belongs in `shifts/*.md` handovers and PR descriptions. Old shift-tag refs already in the codebase are cleanup fodder; don't add new ones.

## Testing

**EVERY bug you discover gets a regression test — no exceptions.** The moment you find a problem (a failing probe, a reviewer catch, a "huh, that's wrong"), the fix is incomplete until a test pins the corrected behavior. This is not optional polish; it is the difference between fixing a bug and fixing it *for good*. **A green CI with the bug still latent is the real danger** — it reads as "covered" when it isn't. Most bugs ship green precisely because no test exercises that *dimension*: the gap is dimensional, not volumetric (you can have 100 tests for a feature and still miss the one axis that's broken). When you fix something, ask "what dimension was unprobed that let this through?" and add the test on that axis. Hard-won examples from this codebase: non-correlated `EXISTS` was wrong on master with full CI green because every `NOT EXISTS` test was *correlated*; deleting a DML helper silently dropped the secondary-UNIQUE→23505 and `RecordDoesNotExistError` mappings — caught by review and a deliberate probe, not by the suite. If a reviewer (human or Graefe/Torvalds/@claude) finds a problem the tests didn't, that's a doubly-important signal: fix the bug **and** add the test that should have caught it. Quality is the point.

- Real FDB via testcontainers, never mocks. High and thorough coverage required for every feature/fix/behavior change — edge cases, error paths, zero-value behavior.
- All tests MUST call `t.Parallel()` and be safe to run concurrently (unique key prefixes / subspaces, no shared mutable state).
- Container setup MUST have timeouts: `context.WithTimeout(context.Background(), 2*time.Minute)` around `foundationdbtc.Run()` / `container.InitializeDatabase()`. Bare `context.Background()` blocks forever when Docker is slow.
- Never run binding stress concurrently with `just test` — both spin Docker containers; pre-commit runs `just test`.
- `.bazelrc` has `--local_test_jobs=4` to cap concurrent FDB containers. If a test suite "hangs", check whether 200 tests are cascading through 30s timeouts (kill it; don't wait).

## Shift system

Vollkonti continuous 24/7 shifts via `/vollkonti`. Handovers in `shifts/`. One branch + one PR per shift, merged at end.

**Pacing is NEVER the model's call.** Don't autonomously slow down, "find a stopping point," or rationalize coasting. Stops are EXTERNAL: user intervention, mid-shift check-in, wind-down at T+7:30. Heuristics from human-paced practice ("keep PRs focused", "let big work rest") DO NOT apply — the system is designed for continuous output. Common rationalizations to ignore: trained-SWE-instinct, marginal-value-reasoning, reviewer-empathy projection, milestone pattern-matching, "shipped ahead of demand" guilt. If a "what I would have done differently" entry boils down to "less work would have been fine," delete it before merging.

## Work tracking

`TODO.md` is the authoritative execution order — numbered items in 6 sequential phases, items inside a phase run in parallel unless gated. **At shift start, pick the lowest-numbered unchecked item whose gates are satisfied.** Handover follow-ups are suggestions, not the priority list. Finish what you start before moving on.

**Working rhythm:** one thing at a time. Implement → `just test` → commit → push → next. One logical change per commit; don't batch unrelated features. Don't push unless asked.

**High-output patterns (proven in swingshift-70, 11k+ LOC/shift):**
- **Commit constantly.** Every green test = commit + push. Small commits (5-50 LOC each) maintain momentum and make rollback trivial. 80+ commits/shift is normal when you're flowing.
- **Read Java first, write Go second.** Read the Java source file completely before porting. Understand the algorithm, then translate idiomatically — don't transliterate line-by-line.
- **Tests find bugs.** Write the test BEFORE assuming the implementation is correct. swingshift-70 found 3 real bugs via tests (InJoin chain flat, UnorderedUnion early return, DistinctUnion ascending-only). Tests are not padding — they're debugging tools.
- **Fuzz is non-negotiable.** Run fuzz targets (`bazelisk test ... --test_arg="-test.fuzz=FuzzXxx" --test_arg="-test.fuzztime=15s" --test_arg="-test.fuzzcachedir=/tmp/fuzz-cache" --sandbox_writable_path=/tmp/fuzz-cache`) on new infrastructure. 200k+ execs should produce 0 panics.
- **Prove with FDB.** Integration tests against real FoundationDB (testcontainers) are the gold standard. A unit test proves the code compiles; an FDB test proves it works. `bazelisk test //pkg/relational/sqldriver:sqldriver_test --test_arg="--test.run=TestFDB_Xxx"` runs specific FDB tests.
- **Subagents for boilerplate.** Delegate test writing, wrapper creation, and mechanical porting to subagents. Keep the critical path (algorithms, rule logic, architectural decisions) in the main context.
- **Don't pad tests, do find gaps.** Use `bazelisk coverage //path:target --combined_report=lcov` to find actual coverage gaps. Only write tests that exercise uncovered code paths or prove new behavior.
- **100% Java alignment unless there's a good reason.** Never simplify "for now" — the simplified version rots and the next shift inherits technical debt. Port the full algorithm, handle all edge cases, match the error messages.
- **Java is the spec for the query planner.** Always read the Java source first, understand the architecture, then port. 1:1 port is king — same class structure, same algorithm, same semantics. No Go-only shortcuts, no "temporary" alternative paths. If Java uses Cascades for all queries, Go uses Cascades for all queries. If Java doesn't have a physical sort operator (RemoveSortRule eliminates the sort), Go doesn't have one either. Same goes for tests: port Java's test cases directly, don't invent Go-only test shapes that diverge from Java's expectations.
- **No parallel pipelines.** Java has one query path (Cascades). Go has one query path (Cascades). Don't maintain a "plangen" or "naive" fallback alongside Cascades — it creates divergence, doubles maintenance, and hides Cascades gaps instead of forcing them to be fixed.

**Fix bugs as you find them.** When a corpus probe (parallel-agent batch or otherwise) surfaces a real Go-side divergence, the default response is: investigate root cause → fix in the same shift → pin the corpus entry. Filing a TODO and dropping the entry is a failure mode — it ships nothing, removes the regression sentinel that would have caught the bug, and dumps the work on the next shift. A 30-line TODO writeup is more expensive than the 50-line fix it punts. **Only file a TODO when the fix is genuinely out of scope:** Java upstream bug, gated on a future Phase, or multi-shift effort. Tiny isolated bugs (one comparison op, one missing dedup-key projection, one error-message tweak) MUST be fixed inline. The corpus is the regression net for cross-engine parity; if you can't pin a shape because Go has a bug, fix the bug — don't grow the corpus around it. Nightshift-65 surfaced 23 bugs and fixed zero; that pattern is now explicitly forbidden.

**Divergence grinding:** `DIVERGENCES.md` is the authoritative list of Go vs Java architectural differences. **100% Java alignment is the default. ALWAYS read Java source FIRST before writing any fix, any port, any new code.** The workflow for closing divergences:
1. **Research phase:** spawn parallel read-only subagents to investigate the Java source code for each divergence. Each agent reads the Java class, notes fields/methods/behavioral differences, and reports whether the divergence is (a) fixable now, (b) blocked on execution layer, or (c) deep architectural.
2. **Port phase:** DFS through the fixable items. For each: read Java → understand the exact semantics → implement the identical semantics in Go → write tests → `just test` → update DIVERGENCES.md. One divergence at a time, fixed to completion.
3. **Document phase:** update DIVERGENCES.md with precise findings — what's fixed, what's remaining, what's blocked and why.

Never rationalize a divergence as "intentional" without first reading the Java code. If Java does X and Go does Y, the default is to port X. Only keep Y if there's a real architectural reason (Go has no sealed classes, Go's fixpoint rule architecture requires a different guard, etc.) — and document that reason in DIVERGENCES.md.

**Delegation:** principal-engineer mindset. Delegate mechanical/boilerplate work to subagents with full context (file paths, snippets, patterns). Critical/tricky pieces: do yourself. Never run two big implementation subagents in parallel.

**Build & verify:** always `just test`. Bazel cache makes incremental runs fast. After Go file/dep changes: `just gazelle` then `bazel mod tidy`. Proto codegen: `buf generate` (not in Bazel). **Always `bazelisk`, never `bazel`** when invoking directly. Never `--no-verify` — investigate hook failures.

Update TODO.md as work completes (`- [x]` with a short note).

## Wire types

Never hand-write FDB wire-type structs in `pkg/fdbgo/wire/types/`. Generate via the C++ schema extractor: register in `cmd/fdb-schema-extract/extract.h`, add `extractType<T>(...)` in `main.cpp`, run `just generate-wire-types && just gazelle`. The only exception is `keyrangeref_custom.go` (documented).

## Stack

Go (see `go.mod`); FoundationDB via pure Go client (`pkg/fdbgo/fdb`) or Apple CGo binding; protobuf (Apple's protos in `proto/apple/`); buf for proto codegen; Bazel 9 via bazelisk + gazelle; nogo lint as build error; just as task runner; testcontainers-go.

Top-level dirs (run `ls`/`tree` for detail):
```
pkg/recordlayer/        Record Layer impl + chaos + cascades + plans
pkg/relational/         SQL engine + cross-engine harnesses
pkg/fdbgo/              Pure Go FDB client
gen/                    Generated proto Go code
proto/apple/            Apple's proto defs
conformance/            Java conformance server + Go conformance tests
shifts/                 Per-shift handovers (YYYY-MM-DD-{shift}.md)
TODO.md                 Authoritative priority list
```

## Running

```sh
just build / test / bench / gazelle / generate / tidy / verify
just bench-one NAME
just binding-stress [N M]   # default 100 seeds × 1000 ops
```

Run a specific Ginkgo test: `bazelisk test //pkg/recordlayer:recordlayer_test --test_arg="--ginkgo.focus=NAME" --test_output=streamed`. Continuous fuzz: `bazelisk run //pkg/...:test -- -test.fuzz='^FuzzName$' -test.fuzztime=60s`. Reproduce a binding-stress seed: `bazelisk run //cmd/fdb-binding-stress -- -seeds 1 -seed-start NNN`. FDB crash debugging: see `pkg/fdbgo/client/CRASH_BUG.md`.

**Stress comparison workflow:** When changing the planner, cost model, or executor, run the 1M stress test before AND after to detect regressions. Use a git worktree for the baseline:
```sh
# Baseline (master):
git worktree add /tmp/fdb-master master
cd /tmp/fdb-master && bazelisk test //pkg/relational/sqldriver/stress:stress_test \
  --test_output=streamed --test_arg="--test.run=TestFDB_Stress_1M$" --test_arg="--test.v"
git worktree remove /tmp/fdb-master --force

# Current branch:
bazelisk test //pkg/relational/sqldriver/stress:stress_test \
  --test_output=streamed --test_arg="--test.run=TestFDB_Stress_1M$" --test_arg="--test.v"
```
Compare row counts + durations. Record results in `TODO.md` "Stress test 1M baseline" table. Key thresholds: point lookups <5ms, full scans ~3s/1M, index equality <10ms.

## Java compatibility — non-negotiable

Wire-level compatibility is the whole point. These match Java exactly: subspace constants, key construction (FDB tuple encoding), protobuf format, record store header, builder pattern, continuation tokens (proto-wrapped, magic `6773487359078157740`), index entry format, split record format (100KB chunks at suffixes 1+; unsplit at 0), record version storage (inline at `pk + -1` suffix, format version ≥ 6).

FDB constraints: 5s tx limit, 100KB value limit, 10MB tx limit, ~10KB key limit. Cursors need `TimeScanLimiter` + continuations; values use split records.

Java source at `fdb-record-layer/` (gitignored, tag **4.11.1.0**, matches MODULE.bazel pins).

## Design principles

1. **Compatibility first** — match Java wire format exactly.
2. **C++ is the spec for the FDB client** — Go divergence from C++ is a bug in Go. Never skip a divergence test; fix Go.
3. **No mocks.**
4. **Explicit errors** — never panic in library code.
5. **Simple code** — three similar lines beats a premature abstraction.
6. **Proto fidelity** — open enums, field presence, wire compat.
7. **Test hard** — `t.Parallel()`, edge cases, Java interop.
8. **Error types, not sentinels** — see below.
9. **Never paper over bugs** — early-return tolerance gates compound across shifts and hide real failures. Pin the actual expected behaviour.
10. **Emergent behaviour over special-case checks** — match the architectural property that produces the behaviour, not a downstream observable. Bolted-on `if X { throw }` checks diverge the moment Java's structure changes.

## Cross-engine SQL conformance

Conformance principle: **doesn't work in Java → doesn't work in Go**, in the same architectural way (visitor-doesn't-implement → fall-through-to-default), with identical error wording where the message can be cleanly shared.

This governs the **shared** query surface — inputs Java also attempts. It is NOT a ban on capabilities Java lacks entirely: net-new read-side query extensions are allowed when wire compat holds (see "Wire compat is the hard line; query reach is not" near the top). The rule means *don't silently diverge from Java where both engines run the same query*, not *never exceed Java*.

Don't enumerate Java's quirks in this file — find them via the cross-engine harness, document each at the relevant code site, capture open ones in TODO.md.

## Error handling

Java exception class = Go error struct, always. Use `errors.As()` to match (the Go equivalent of `catch (SpecificException e)`). Never use bare `var ErrFoo = errors.New("...")` sentinels — they can't carry the structured context Java's `addLogInfo()` provides.

```go
type RecordAlreadyExistsError struct { PrimaryKey tuple.Tuple }
func (e *RecordAlreadyExistsError) Error() string { ... }

return &RecordAlreadyExistsError{PrimaryKey: pk}

var e *RecordAlreadyExistsError
if errors.As(err, &e) { ... }
```

Carry the same context fields as the Java exception's `addLogInfo()` keys. Wrap with `fmt.Errorf("...: %w", err)` to add call-site context while preserving `errors.As()` unwrapping. For genuinely message-only Java exceptions, use `XxxError{Message string}`.

## Proto definitions

Use Java's protos directly. Canonical at `fdb-record-layer/fdb-record-layer-core/src/main/proto/`; `proto/apple/` mirrors them. Copy the full proto when adding new types — don't hand-maintain a subset.

## Chaos testing

Model-based, at `pkg/recordlayer/chaos/`. An in-memory `StoreModel` shadows the real store; `Verify()` compares them after each operation. `ChaosTransactor` injects faults at tx boundaries via the production-side `NewFDBDatabaseWithTransactor` hook. Targeted (`s.InjectOnce(FaultCommitUnknown)`) or random (`WithSeed(N), WithFaults(FaultsRetryHeavy)`); seed for reproducibility. Extend by adding a `FaultType` + `ChaosTransactor.Transact` arm, or a new `Verify()` invariant, or a new `StoreModel` state field.

## Status

For up-to-date counts and shift-by-shift state, read the most recent file in `shifts/` and run the relevant tooling: `just test` for spec count, `grep -rE "^func Fuzz" pkg/` for fuzz targets, `just coverage` for HTML coverage, `just bench` for benchmarks.
