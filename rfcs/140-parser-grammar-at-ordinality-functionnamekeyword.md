# RFC-140 — Parser grammar: `AT ordinality` source + `functionNameKeyword` (Java 4.12, RFC-135 §4 R3)

**Status:** Draft
**Item:** RFC-135 §4 **R3** — port Java 4.12's two `RelationalParser.g4` changes: the PartiQL `AT`
unnest-with-ordinality table source (commit `3474171d9` #4112) and the `LEFT`/`RIGHT` identifier
disambiguation (commit `986694af1` #4272).
**Reviewers:** **Graefe** + Torvalds (query-engine grammar/parse-surface item).

---

## 1. Problem (both verified real against 4.12.11.0)

Go's `RelationalParser.g4` predates two 4.12 grammar changes:

**(a) `AT atAlias` table source (#4112).** Java's `atomTableItem` is
`tableName (AS? alias=uid)? (AT atAlias=uid)? (indexHint ...)?`; Go's lacks `(AT atAlias=uid)?`
(`RelationalParser.g4:483`). This is the parse half of PartiQL `FROM t, t.arr AS elem AT pos` — bind an
array element *and* its 1-based ordinal. (The execution — `ExplodeExpression.withOrdinality` /
`RecordQueryExplodePlan.with_ordinality` — is **R5**; this RFC is the grammar only.)

**(b) `LEFT`/`RIGHT` disambiguation (#4272).** Java moved `LEFT`/`RIGHT` **out of `functionNameBase`**
into a new `functionNameKeyword : LEFT | RIGHT ;`, referenced only from `scalarFunctionName`. Go still
has `LEFT`/`RIGHT` **in `functionNameBase`** (`:1396,1413`), and `functionNameBase` feeds `simpleId`
(`:778-783`) → `uid` → table aliases. So Go currently accepts `LEFT`/`RIGHT` as bare identifiers/aliases,
which is ambiguous with the `{LEFT|RIGHT} [OUTER] JOIN` clause — exactly what #4272 removed. After the
fix they remain usable as scalar **function** names (`LEFT(s, n)`) but not as identifiers — matching
Java, and a cross-engine parse-surface parity item.

## 2. Investigation (Java ↔ Go)

| | Java 4.12 (`RelationalParser.g4`) | Go (`pkg/relational/core/parser/grammar/RelationalParser.g4`) |
|---|---|---|
| `atomTableItem` | `… (AS? alias=uid)? (AT atAlias=uid)? (indexHint …)?` | missing `(AT atAlias=uid)?` (`:483`) |
| `functionNameBase` | no `LEFT`/`RIGHT` | has `LEFT` (`:1396`), `RIGHT` (`:1413`) |
| `functionNameKeyword` | `: LEFT | RIGHT ;` (new) | absent |
| `scalarFunctionName` | `: functionNameBase | functionNameKeyword | …` | `: functionNameBase | …` (no `functionNameKeyword`) |

Tokens already in Go's lexer: `AT` (`:407`), `LEFT` (`:139`), `RIGHT` (`:190`) — no lexer change.
`AT atAlias=uid` uses the `AT` token + a uid; there is **no** `ORDINALITY` token in Java's syntax, so
none is needed. Parser regen: `just generate` → `bazelisk run //…/grammar:generate_parser` (ANTLR, needs
`java` on PATH); the generated Go parser is checked in.

## 3. Fix (grammar only)

1. `atomTableItem`: insert `(AT atAlias=uid)?` between the `AS? alias` and `indexHint` groups (byte-for-
   byte Java).
2. `functionNameBase`: remove `LEFT` and `RIGHT`.
3. Add `functionNameKeyword : LEFT | RIGHT ;`.
4. `scalarFunctionName`: add `| functionNameKeyword` as the second alternative (Java's position).
5. `just generate` to regenerate the parser; `just gazelle` if file set changes.
6. **functionNameKeyword — no visitor code (Graefe + Torvalds confirmed).** Every consumer reads the
   scalar-function name via `ScalarFunctionName().GetText()` (e.g. `walk.go:615`, `cascades_generator.go:2838`);
   `GetText()` concatenates the matched subtree regardless of *which* alternative fired, so `LEFT` reached
   via `functionNameKeyword` yields `"LEFT"` identically to the old `functionNameBase` path — `LEFT(...)`/
   `RIGHT(...)` keep resolving as function calls with zero visitor changes.
7. **AT — reject at EVERY consumer until R5 (codex P2, two rounds).** The grammar parses `AT atAlias`,
   but consumers must **not** merely ignore the alias: "parse-and-ignore" has a silent-wrong-result hole —
   for a table that *has* a column named the same as the ordinal alias (e.g. `SELECT tier FROM t AS e AT
   tier` where `t.tier` exists), the ignored AT clause lets `tier` resolve to the real column and returns
   the wrong value, no error. `select_parser.go` gains `rejectAtOrdinality(atomItem)` returning
   `ErrCodeUnsupportedQuery` whenever `GetAtAlias() != nil`, called at **every** `AtomTableItemContext`
   consumer that would otherwise drop the clause: the four query-planner lowering sites (comma-FROM, the
   main atom, both JOIN paths), the **aggregate-index DDL** path (`ddl.go parseAggregateIndexDefinition` —
   codex round 2: `CREATE INDEX … AS SELECT … FROM t AT p GROUP BY p` would otherwise group by a real
   column `p`; and round 3: that path also silently *dropped* any JOIN — `… FROM a JOIN b AT p ON …` —
   leaving AT on the joined source unchecked, so JOINs are now rejected outright there, which aggregate
   indexes never supported anyway), and the semantic `scope_build.go` path (a local `UnsupportedFromShapeError{"AT ordinality"}`
   — currently test-only, guarded for defense-in-depth). The only unguarded `AtomTableItemContext` site is
   `cascades_generator.go`'s `referencesInformationSchema` tree-walker, which inspects table names for
   INFORMATION_SCHEMA and consumes no AT semantics. This is the honest R3 boundary: grammar accepts, every
   consumer explicitly rejects; R5 removes the guards and binds the ordinal. (Not a "for now" stub.)

## 4. Wire / behaviour impact

Parse-surface only; no persisted bytes, no plan/continuation change. Two observable deltas, both toward
Java parity: (a) `FROM … AS x AT y` now parses (previously a syntax error); (b) `LEFT`/`RIGHT` are no
longer accepted as bare identifiers/aliases (previously accepted via `functionNameBase`), but remain
valid scalar-function names. The conformance principle holds: where both engines run the same query they
now agree at the parse layer.

**Suite survey (run, not asserted — Torvalds):** `rg` over `pkg/relational/**/*.{go,yamsql}` shows the
LEFT/RIGHT usages split into exactly two safe buckets and an empty risky one:
- **As scalar functions** (survive via `functionNameKeyword`): `SELECT LEFT(name, 3)` /
  `SELECT RIGHT(name, 3)` (`embedded_fdb_test.go:6025-6026`), `LEFT(s, 2.5)` / `RIGHT(s, 2.5)` (`:7178-7179`).
- **As JOIN clauses** (untouched — `joinPart`, not `functionNameBase`): 99 occurrences.
- **As bare identifiers / aliases** (would break): **0 hits** (`AS LEFT|RIGHT`, bare `LEFT|RIGHT` column/
  table) — so removing them from `functionNameBase` reconciles a latent divergence with **no** existing
  test to weaken.

- **functionNameKeyword (fully e2e in R3):** the four existing `LEFT(...)`/`RIGHT(...)` function tests
  above must stay green after regen (they now resolve via `functionNameKeyword`; `GetText()` is
  alternative-agnostic). **Pin the negative:** a new test that `LEFT`/`RIGHT` used as a table alias
  (`… T AS LEFT`) / identifier is now a **parse error** specifically (not a planner error), matching Java.
- **AT grammar parses + alias captured:** `SELECT e FROM orders AS e AT p` parses with no syntax error
  and the `atomTableItem` tree exposes `atAlias = p`; without `AT`, `atAlias` is nil (real optionality).
- **AT rejected at planning — the R3→R5 sentinel (codex P2):** planning `… AS e AT p` returns
  `ErrCodeUnsupportedQuery` (not a panic, not wrong rows), **including the column-collision case**
  `SELECT tier FROM orders AS e AT tier` where `orders.tier` exists (proving the alias can't silently
  resolve to the real column) and the **JOIN** path (`… A a JOIN B b AT p ON …`). R5 removes the guard
  and flips these from reject to real ordinal rows.
- **No regression:** the relational parser + planner suites stay green after regen.
- Determinism: grammar change, no planner nondeterminism; run the affected parser test 10× to confirm.

## 6. Scope

One commit on the RFC-135 branch (PR #336): the four grammar edits + regenerated parser + tests. **No
visitor change** — `ScalarFunctionName().GetText()` (`scalar_functions.go:70`, `cascades_generator.go:2838`)
is alternative-agnostic over the unlabeled `scalarFunctionName` rule, so `LEFT`/`RIGHT` reached via
`functionNameKeyword` extract identically (Graefe + Torvalds confirmed — the earlier "visitor hook" was a
phantom task). The AT-ordinality **execution** (`withOrdinality` through expression/plan/rule/EXPLAIN/
cursor) is **R5**, the next commit. `functionNameKeyword`'s deeper tie to LEFT/RIGHT **JOIN** semantics is
**R7**; R3 only makes the grammar accept/reject correctly.
