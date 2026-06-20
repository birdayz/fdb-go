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

**No production-code change** (unless §5 finds a live panic). Two deliverables:

1. **Refresh `docs/panic-audit.md`** to current reality: the 155/22 counts; the §2 boundary table as
   the authoritative recover-site allowlist (each with its role); and replace the obsolete "remove all
   `recover()`" goal with the actual policy — *the backstop layer IS the boundary discipline; new
   boundaries must be deliberate and listed here.* Keep the user-reachable-panic worklist (the eval
   `Value.Evaluate`/`QueryPredicate.Eval` error-channel conversion) as **explicitly deferred** (it is
   the big signature-change refactor, tracked, not part of this gate).

2. **`pkg/docscheck/panic_boundary_test.go`** — the release gate (two pure-unit guards, no FDB):
   - **Recover allowlist** — enumerate every non-test `recover()` site under `pkg/` and `cmd/` (regex
     over the source, the docscheck walk-up pattern) and assert the result matches the allowlist keyed
     by **file → expected count** (NOT `file:line` — line numbers shift on every edit and would make
     the gate flap). A `recover()` in a new file, or an extra one in an allowlisted file, fails until
     the author adds/justifies it in panic-audit.md; a removed one that leaves a stale doc entry also
     fails (drift both ways). The table's `file:line` in §2 stays human documentation; the executable
     key is `file → count`. This is the ratchet.
   - **Boundary fuzz registry** — a small in-test list of the four required boundaries → their fuzz
     target name + file; assert each fuzz function still exists (regex over its file). If a boundary
     loses its no-panic fuzz (renamed/deleted), → **red**. Names a boundary, not all 122 fuzzers
     (Torvalds: pin the load-bearing four, not padding).

   Both are name/site-presence guards (the RFC-133 completeness-guard shape), not behavioural — they
   keep the *map and the net* honest, cheaply, on every build.

## 4. Executable spec (tests)

- Add an unclassified `recover()` to a non-test file → gate red (revert-proven).
- Rename/delete a required boundary fuzz target → gate red (revert-proven).
- A stale allowlist entry whose `recover()` was removed → the test also flags drift the other way
  (an allowlisted site that no longer exists), so the doc can't keep dead entries.

## 5. Pre-flight verification (part of impl, not just claim)

Before declaring the boundaries safe, **run the four boundary fuzz targets** (parser, planner, wire,
tuple) for a bounded `-fuzztime` and confirm **zero panics / new crashers**. If any boundary panics on
malformed input, that is a real bug — fix it (convert the panic to an error at that boundary, with its
area's reviewer) and pin the crasher in the seed corpus, **in this PR** (DFS, not deferred).

## 6. Wire/behaviour impact

**None.** A doc refresh + a doc/coverage meta-test. No persisted bytes, no option/SQL/plan semantics.

## 7. Scope

One PR: the `docs/panic-audit.md` refresh + `panic_boundary_test.go` + its BUILD wiring. The
24-site convert-panics-to-errors worklist (eval error-channel) stays a **separate, deferred** item —
this RFC gates the discipline and makes the map honest; it does not undertake that refactor.
