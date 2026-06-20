# RFC-134 — Panic-boundary discipline as a release gate

**Status:** Draft
**Item:** prod-readiness-audit-2026-06-19.md **P2** — "Panic Boundary Discipline Needs To Stay A
Release Gate."
**Reviewers:** Torvalds (code/test quality) + codex + @claude. This is a *meta-test* (a discipline
guard over existing code + a doc refresh), **not** a Cascades or wire-semantics change — so no Graefe
gate and no FDB-C-dev gate, *unless* refreshing the classification turns up a boundary that actually
panics on malformed input (a real bug); that fix would then carry its area's reviewer.

---

## 1. Problem (verified)

`SECURITY.md` scopes crash/DoS: untrusted input must produce **errors, never process crashes**. The
audit's concern is not that panics exist (most are legitimate fail-stop invariant asserts or `Must*`
APIs) but that **the boundary discipline has no gate**, so it silently drifts:

- The classification doc `docs/panic-audit.md` is **stale**. It records (2026-06-07) **158 panics /
  11 recovers**; the tree now has **155 panics / 22 recovers** (non-test). The recover count has
  **doubled**, and its "remove all `recover()`" goal was in practice **superseded** by a deliberate
  panic→error *backstop layer* (`pkg/fdbgo/client/panicbackstop.go`, `pkg/fdbgo/fdb/panic.go`) that
  did not exist when the doc was written. The doc no longer describes reality.
- **Nothing fails CI** when a new `recover()` is added (an undisciplined panic-swallow that could hide
  a real error path, or drop rows via a `keep=false` default arm — the exact hazard panic-audit.md
  §"Remove-all-recovers" flagged), nor when a public input boundary **loses** its no-panic fuzz.

The boundary *coverage* is actually strong already — 122 fuzz targets, including the four boundaries
the audit names: SQL parser (`FuzzParse`/`FuzzParseFunction`/`FuzzParseView`), planner
(`Planner_*_NoPanic`, `FuzzTranslateToCascades`), tuple/continuation decode (`tuple_malformed_test.go`,
`Unpack` returns error not panic, the `*Continuation` fuzzers), and wire decode (`reader_fuzz_test.go`,
`marshal_fuzz_test.go`). So the gap is **not missing tests** — it is (a) a stale map and (b) no
ratchet keeping the map and the coverage honest.

## 2. The boundary layer (the 22 recovers, classified)

Every non-test `recover()` is a deliberate panic→error translation at a public/goroutine boundary —
this IS the discipline the audit wants, it just isn't pinned:

| Site(s) | Boundary | Role |
|---|---|---|
| `fdbgo/client/panicbackstop.go:21,56,82` | client callback / goroutine | backstop: panic → error/log, never crash the host |
| `fdbgo/fdb/panic.go:21`, `fdb/transaction.go:524` | `Transact` closure | `panicToError` — a panicking user closure becomes a tx error |
| `fdbgo/client/database.go:451,645` | `Run`/retry callback | same backstop on the client-level transact loop |
| `fdbgo/client/grv.go:430,434`, `transport/conn.go:647,702` | background goroutines | a panic in a GRV/IO goroutine fails the conn, never the process |
| `fdbgo/libfdbc/backend.go:337,352,354` | cgo libfdb_c backend | translate cgo/callback panics to Go errors |
| `relational/core/parser/parser.go:39,99,121` | SQL parse | ANTLR bailout panic → syntax error |
| `relational/core/embedded/connection.go:417,453`, `cascades_generator.go:1028` | SQL conn / executor | public DB API + eval bridge: panic → SQL error |
| `recordlayer/merge_cursor.go:24` | comparison-key encode | `tuple.Pack()` panic on user-derived keys → cursor error |
| `cmd/fdb-stacktester/directory_ops.go:166` | binding-tester harness | test binary, not library |

None silently swallow: each maps to a returned error / failed future / logged-and-failed conn.

## 3. Change

**No production-code change** (unless §5 finds a live panic). Three deliverables.

**Why a nogo analyzer, not a docscheck walk.** The primary gate is `bazelisk test //...` + nogo
(`just test`, pre-commit). Bazel globs do **not** cross package boundaries, so a docscheck test cannot
`filepath.Walk` the whole tree under Bazel — it would need every `.go` file as a `data` dep (one per
package), which is impractical. nogo, by contrast, runs **per package on every build** and already
hosts two project-specific analyzers (`//pkg/linters/{gofumpt,noemptyiface}`). A `recover()` ratchet
belongs there: it sees every package (including a brand-new file), runs in the strongest gate, and
needs no data-dep plumbing. This also answers Torvalds's "wired into CI?" — nogo *is* the build.

1. **Refresh `docs/panic-audit.md`** to current reality: the 155/22 counts; the §2 boundary table as
   the authoritative recover-site allowlist (each with its role); replace the obsolete "remove all
   `recover()`" goal with the actual policy — *the backstop layer IS the boundary discipline; new
   boundaries must be deliberate and listed in the `norecover` allowlist.* Keep the user-reachable-
   panic worklist (the eval `Value.Evaluate`/`QueryPredicate.Eval` error-channel conversion) as
   **explicitly deferred** (the big signature-change refactor — tracked, not part of this gate).

2. **`pkg/linters/norecover`** — a nogo analyzer (the recover ratchet). It walks each package's AST
   for a call to the builtin `recover`, counts them **per file**, and compares to a baked-in
   `map[string]int` allowlist (repo-relative-suffix → permitted count — the §2 boundary layer). A
   `recover()` in a non-allowlisted file, or one *more* than the allowance in an allowlisted file,
   is a **nogo build error**. Per-file count (not file-level exclude) keeps it as tight as the
   originally-ACK'd file→count design. *Accepted hole* (Torvalds): an add-one/remove-one within the
   same allowlisted file nets zero and slips through — but that means editing an already-scrutinised
   boundary file, the narrowest possible gap. Registered in `//:nogo` deps + `nogo_config.json`.

3. **`pkg/docscheck/panic_boundary_test.go`** — the boundary-fuzz registry (the other half). A small
   in-test list of the four audit-named boundaries → `{fuzz fn, file}`. For each it asserts (a) the
   fuzz function exists in its file AND (b) **that file is in a `go_test` target's `srcs` in the same
   dir's `BUILD.bazel`** — i.e. it actually compiles + replays its seed corpus under `bazelisk test`,
   not merely "a function with that name exists" (Torvalds: name-presence is theater; wiring is the
   point). The four fuzz files + their BUILD.bazel are specific, so they are `data`-dep'd (the
   RFC-131 pattern) — no whole-tree problem. Losing/renaming a boundary fuzz, or unwiring it from its
   test target, → **red**.

## 4. Executable spec (tests)

- `norecover` analyzer unit test (`analysistest`, the `noemptyiface` pattern): a fixture with a
  `recover()` in a non-allowlisted path → diagnostic; an allowlisted path → none.
- Revert-prove the live gate: add a `recover()` to a non-allowlisted production file → `just build`
  goes red (nogo); remove → green.
- Boundary-fuzz registry: rename a required fuzz fn, or remove its file from the `go_test` `srcs` →
  `panic_boundary_test.go` red (revert-proven).

## 5. Pre-flight verification (quantified — part of impl, not a claim)

Before declaring the boundaries safe, **actively fuzz the four boundary targets** — parser
(`FuzzParse`), planner (`FuzzTranslateToCascades`), wire (the `reader_fuzz` target), tuple
(`tuple_malformed`) — at **`-fuzztime=60s` each** (seed corpus replayed + new inputs explored), via
the Bazel fuzz invocation. Confirm **zero panics / zero new crashers**. Any crasher is a real bug:
fix it (convert that boundary's panic to a returned error, with its area's reviewer — Graefe for
parser/planner, FDB-C-dev for wire/tuple) and commit the crasher into the seed corpus, **in this PR**
(DFS, not deferred). The exact commands + their clean output go in the PR description so the
verification is auditable, not asserted.

## 6. Wire/behaviour impact

**None.** A doc refresh + a doc/coverage meta-test. No persisted bytes, no option/SQL/plan semantics.

## 7. Scope

One PR: the `docs/panic-audit.md` refresh + `panic_boundary_test.go` + its BUILD wiring. The
24-site convert-panics-to-errors worklist (eval error-channel) stays a **separate, deferred** item —
this RFC gates the discipline and makes the map honest; it does not undertake that refactor.
