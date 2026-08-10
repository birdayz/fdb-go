# RFC-227 — The hidden sort column is identified by the value it reads, not by a rendered name

**Status:** rev 3 — Graefe ACK + Torvalds ACK on rev 2 (both with conditions, all folded).
IMPLEMENTED and mutation-verified per half; §5 carries the results, including one finding that
corrected this RFC's own §2b and one near-miss the per-half mutation caught.
Awaiting delta re-confirmation from both.
**Origin:** RFC-218 §0's first REFUTED claim, which RFC-218's own fix falsified and did not
revisit. The live bug behind it is this one.
**Scope:** `pkg/relational/core/query/cascades_translator.go` (`collectExtraSortColumns` and
the three hand-copied nested-key predicates), the characterization pin in
`pkg/relational/sqldriver/nested_sort_key_rows_fdb_test.go`, the plan-shape golden, one yamsql
scenario, and two RFC-197 registry entries.

> **Numbering.** 226 is the highest merged (checked after #699 landed as `374e654b9`).
> `gh pr list --state open` shows #579 and #486; their diffs claim `rfcs/205-…` and
> `rfcs/graph-db-on-fdb.md` only. 227 is free.

---

## 0. What changed in rev 2

Rev 1 proposed only renaming the appended column. Graefe **NAK'd** it: Java identifies the
remaining order-by columns by **derivation and ordinal**, never by a rendered name, and Go
already has the derivation test three lines above the defect (`sortKeyInOutput`). Keeping a
name-keyed `seen` leaves the failure class intact and merely rarer. That is correct, and rev 1's
own §6 conceded value-keying was "where this ends up" and then deferred it — inside the
function it was already editing. **Both changes are in scope now**, and they are independent:
the dedup KEY becomes the source value, and the appended column's NAME becomes the resolved
path. Rev 3 MEASURED how each half fails on its own — the answer corrected §2b; see §5.

Torvalds ACK'd rev 1's site choice and truncation claim, conditionally. Folded here: a shared
nested-key predicate instead of a fourth hand-copy (§2c), the `Value == nil` shape stated
(§2d), honest collision wording (§2e), honest measured-vs-pending labelling (§5), and a second
registry entry that this change also falsifies (§4).

---

## 1. The defect: two nested ORDER BY keys of one struct root return wrong rows

Schema `CREATE TYPE AS STRUCT nst (sk BIGINT, co BIGINT)`,
`CREATE TABLE t1 (id BIGINT, n nst, PRIMARY KEY (id))`, rows
`(1,(2,5)) (2,(1,5)) (3,(1,4))` — both members tied so a dropped key is visible.

**MEASURED** on `374e654b9`, `--nocache_test_results`, 15 `=== RUN` lines:

```
SELECT id, EXISTS (SELECT 1 FROM t2 WHERE t2.t1_id = t1.id) AS h FROM t1 ORDER BY n.co, n.sk
  got [3 1 2]   SQL requires [3 2 1]
SELECT id, EXISTS (SELECT 1 FROM t2 WHERE t2.t1_id = t1.id) AS h FROM t1 ORDER BY n.sk, n.co
  got [2 3 1]   SQL requires [3 2 1]
```

Silent wrong rows, no error. The two non-fold controls over the same data return `[3 2 1]`,
which localises the defect to the projected-EXISTS fold, not to nested multi-key sorting.

### 1a. The mechanism, corrected — the second KEY survives; its COLUMN does not

The brief this work started from, and the pin's own header comment, both say "the second of
them is dropped as a duplicate of the first". The **measured** plan says otherwise, and the
difference decides where the fix goes:

```
plan: Project([ID#0, H#1], InMemorySort([N ASC, N.SK ASC], FlatMap(outer=Scan(T1), …)))
```

Both sort keys are present. What is missing is the second key's **hidden column**.
`collectExtraSortColumns` (`:5198`) names each appended column with `sortKeyFieldRef(k)`, and
for a nested single-table key that renders the struct **root** (`:4966-4973`: `fv.Child == nil`
→ `ToUpper(fv.Field)` → `"N"`). Both keys therefore name `N`, that name keys the `seen` dedup,
and the second key's column is skipped. `pullUpSortKeyValue` then finds no output field whose
VALUE is the second key's source column, leaves the key unrewritten, and the sort's second key
reads a slot the folded record does not carry. The `N.SK` in the plan is that unrewritten key
printing its own resolved path — not a resolved column.

So the observable is "the second key does no work"; the defect is "the second column was never
appended". A fix aimed at the key would be aimed at the symptom.

### 1b. Why RFC-218 believed this was safe, and what changed underneath it

RFC-218 §0 lists as its first REFUTED mechanism:

> **"Two keys that render alike collapse to one appended column."** They do, and it is
> harmless. `sortKeySourceValue` depends on the key *only* through `sortKeyFieldRef(k)`, so
> alike-rendering keys carry an identical source value by construction.

That argument was **true when written and false when shipped**, and RFC-218's own fix
falsified it. Before RFC-218, `sortKeySourceValue` began at `field := sortKeyFieldRef(k)`, so
the dependency was total and the construction held. RFC-218 added the nested arms (`:5022`,
`:5044`) **above** that call — confirmed by `git log -L 5022,5031:…` →
`f599685d2 RFC-218: sort keys carry resolved paths, not rendered names (#676)`. Those arms
return a distinct per-member value for two keys that still render alike. The premise the safety
argument rested on was removed by the change the argument was justifying, and §0 was not
revisited.

This is the ordinary shape of the failure: a dedup keyed on a rendering is only as sound as the
claim that the rendering determines the value — which is exactly what a fix that stops
re-deriving values from renderings destroys.

---

## 2. The fix: derivation for identity, resolved path for the name

### 2a. The dedup key becomes the source VALUE (Java's `canBeDerivedFrom`)

**Java, read and cited directly** (`fdb-record-layer/`, tag 4.12.11.0):

- `LogicalOperator.java:390` — `orderByExpressions.difference(output, outerCorrelations)`;
  `:394` — `output.concat(remainingOrderByExpressions)`.
- `Expressions.java:124-146` — `difference` compares each order-by expression against `that`
  (the OUTPUT) only, **never against its own siblings**. There is no self-dedup in Java.
- Membership is `canBeDerivedFrom` (`Expression.java:254-264`) — a **value-derivation** test.
- The pull-up is positional: `Expressions.java:87-96`, `FieldValue.ofOrdinalNumber`. Java's
  remaining order-by columns carry **no name identity at any point**.

Go's `seen` map is a Go-only invention. Deleting it outright would be the most literal port,
and is rejected only because `ORDER BY n.co, n.co` would then append two identical columns and
widen the folded row for nothing. What replaces it is the test Java actually performs, which
Go already has three lines above in the same function — `sortKeyInOutput` (`:5142-5152`) is
`canBeDerivedFrom` over `SemanticEqualsUnderAliasMap`:

```go
val := src.sortKeySourceValue(k)
if val == nil {
	continue
}
// Identity is the VALUE the column reads, never its rendering (Java's
// Expressions.difference -> canBeDerivedFrom, Expressions.java:124-146). The
// same test sortKeyInOutput applies against the OUTPUT, applied here among the
// appended columns themselves.
if extraSortColOfValue(extra, val) >= 0 {
	continue
}
```

This orders the checks correctly too: today `seen` is consulted **before** `val` is computed,
so the dedup decides on a rendering it has not yet earned the right to trust.

#### 2a-0. Why the value comparison actually separates these two keys

`SemanticEqualsUnderAliasMap` → `EqualsWithoutChildren` on `*FieldValue`
(`map_field_values.go:345-355`) compares baked nodes by the **full ordinal path**
(`av.Resolved.Equals(bv.Resolved)`), never by `Field`. `n.co` and `n.sk` share the `Field`
`"N"` and differ in the leaf accessor, so they stay distinct. Baked-vs-lazy is unequal by
contract, so the worst case is a missed dedup — never a conflation.

**The children recursion is load-bearing on the JOIN nested arm, and that must not be
optimised away.** Two legs whose struct sits at the same ordinals produce *equal* resolved
paths; only the `QOV` correlation CHILD separates them. `Resolved.Equals` alone would merge
`t1.n.sk` with `t2.n.sk`. This is written down precisely because it is the sort of
safe-by-construction property whose mechanism a later reader deletes while "simplifying" the
comparison.

#### 2a-ii. One coarsening, in the safe direction, recorded rather than discovered later

The flat join arm's no-matching-leg fallback (`:5091-5092`) returns a lazy
`FieldValue{Field: BARE}` with name-only equality. So `ORDER BY x.id, y.id` where neither `x`
nor `y` names a known leg now appends **one** column where it appended two. Both already read
the same bare slot, so the result is narrower, not wronger — but it is a behaviour change and
it belongs here.

#### 2a-i. The equality must stay SYMMETRIC — binding condition

The extras-dedup uses `SemanticEqualsUnderAliasMap` and **must never be "upgraded" to
`canBeDerivedFrom`**, even though the surrounding prose cites `canBeDerivedFrom` as the Java
authority. Java's derivation test is **asymmetric** and is correct only in the direction it is
used there: order-by expression *against the OUTPUT*. Applied among the extras themselves it
inverts the meaning — for `ORDER BY n, n.sk`, `n.sk` **is** derivable from `n`, so an
asymmetric test drops `n.sk`'s column and reproduces **this exact defect, with a Java citation
attached to it**.

That is the most likely way this fix is undone, because the "improvement" looks like closer
Java alignment. Two things guard it, and both are deliverables, not prose: a comment at the
call site stating the asymmetry, and a dedup failure message that names it, so the next person
reaching for the asymmetric test is told why it is wrong *here* rather than discovering it as
wrong rows.

Note on how that guard is pinned: `ORDER BY n` over a whole struct currently fails at runtime
(`no ordering defined between *dynamicpb.Message and *dynamicpb.Message`), so the `n, n.sk`
shape is **not** reachable as a wrong-rows SQL test. The pin is therefore a unit assertion that
`SemanticEqualsUnderAliasMap` answers **false** for a root value and a member value of that
root — the fact the dedup rests on — with a failure message naming the asymmetric-upgrade
re-armer. Pinning the reachable proxy instead of the shape would be pinning a cousin.

### 2b. The appended column's NAME becomes the resolved path

**MEASURED, and it corrects rev 2's own claim.** Rev 2 said value-keying alone is "not
sufficient". Reverting the naming half with value-keying in place leaves the ROWS **correct** —
both fold arms still return `[3 2 1]`. What reds is the EXPLAIN test and the unit name
assertions, and the folded record carries two fields spelled `N` without mis-resolving, because
`pullUpToOutputField` bakes the ordinal.

So the naming half is a **display-and-hygiene** claim, not a correctness one, and §2b is
reworded to say exactly that rather than to imply rows depend on it. It still ships, for three
reasons that are worth the four lines: the hidden column's name is what EXPLAIN shows a user,
naming two different reads alike inside one `RecordConstructorValue` is a latent trap resting on
every future reader preserving the ordinal bake, and the flat spelling is the thing that made
the identity wrong in the first place. Keeping the name honest is what stops the next dedup from
being tempted by it.

A key carrying a multi-accessor resolved path therefore takes its name from that path:

```go
func sortKeyExtraColumnName(k logical.SortKey) string {
	if fv, ok := nestedResolvedSortKey(k); ok {
		return strings.ToUpper(values.ColumnNameValue(fv))
	}
	return sortKeyFieldRef(k)
}
```

`ColumnNameValue` on a multi-accessor path dot-joins the per-step names (`values.go:1884-1900`,
Java's `FieldPath.toString`, `FieldValue.java:428-433`), so `n.co` → `N.CO`, `n.sk` → `N.SK`.

### 2c. One nested-key predicate, not four hand-copies

`fv.Resolved != nil && len(fv.Resolved.Accessors) > 1` is currently written out three times
(`:4701` the fold's bail guard, `:5022` and `:5044` the two `sortKeySourceValue` arms). Adding a
fourth copy is how RFC-218 produced this bug in the first place — an arm added to one site that
the others did not learn about. All four call one predicate:

```go
// nestedResolvedSortKey reports whether a sort key carries a multi-accessor
// resolved path — the ONE definition of "this key is nested". Three sites read
// it and they must not drift: a key that is nested for the source-value arms and
// flat for the naming arm would be named by its struct root while reading a
// member.
func nestedResolvedSortKey(k logical.SortKey) (*values.FieldValue, bool)
```

### 2d. The shape that does not reach the new arm

A key whose `Value` is nil — the three-segment `t1.n.sk`, which `walkColumnRef` rejects and
`upgradeSortKeyValues` swallows — never reaches `nestedResolvedSortKey` and falls to
`sortKeyFieldRef`'s `k.Expr` text branch. It happens not to collide (`T1.N.SK` and `T1.N.CO`
already differ as text), so it is not a hole this RFC leaves open; it is a shape this RFC does
not reach, and it is stated rather than left to be discovered. It is RFC-218's separately-booked
gap and does not convert here.

### 2e. What the collision wording actually claims

Rev 1 said the change "removes" a name collision. That was too strong. `DOUBLE_QUOTE_ID`
accepts dots, so `SELECT id, "N.CO", EXISTS(…) FROM t1 ORDER BY n.co` can still land two fields
spelled `N.CO` in one `RecordConstructorValue`. The change removes the **unquoted-reachable**
collision — the one every ordinary nested query hits today, including
`SELECT id, n, EXISTS(…) ORDER BY n.sk`, which right now builds `[ID, N, H]` and appends a
hidden column **also named `N`**.

The actual guarantee against both is **not** the name: `pullUpToOutputField` matches by pointer
or `SemanticEqualsUnderAliasMap` and emits `NewFieldValueWithResolvedOrdinalInDomain(f.Name, i,
…)` (`:5444`), so the ordinal decides and the name never does. That is the invariant to pin, and
§4 pins it with a quoted-identifier arm rather than asserting it.

### 2f. Why here and not at `sortKeyFieldRef`

A sibling filed this defect against `sortKeyFieldRef` (`:4966`). That filing is **wrong**, on
two independent grounds:

1. **It has flat-key callers that legitimately want the flat name.** `sortKeyName` →
   `resolveKeyName` (`:4915`, `:5157`) and `sortKeySourceValue`'s flat join arm (`:5061+`) are
   both stated in `LEG.COL` two-segment terms. Changing the mint site changes every key shape to
   fix one.
2. **It is where the string is minted, not where it becomes wrong.** A rendering is a faithful
   answer to "what does this key spell". It becomes wrong when used as an **identity**, which
   happens in `collectExtraSortColumns` alone.

### 2g. What the change deliberately does not touch

Nested-over-**JOIN** already renders `T1.N.SK` (`fv.Child` is the leg QOV, so `sortKeyFieldRef`
takes the `ColumnNameValue` branch); the new naming arm recomputes the identical string, so that
arm is a no-op by construction. **Flat** keys keep `T1.ID` / `COL1`, preserving the RFC-141 R4
P2b collision-freeness argument and every flat golden.

---

## 3. Blast radius

**No wire bytes, no continuation — VERIFIED by reading, not assumed.** The cleanup projection
builds exactly `outputCount` columns from `fields[i].Name` (`:4794-4826`), and the extras are
appended *after* the original outputs, so they are dropped positionally. Nothing downstream of
the cleanup can observe the name: not a result row, not a stored record, not an index entry,
not a continuation. §5 adds the executed confirmation.

**EXPLAIN moves, and the name IS the channel.** Rev 1 wrongly implied the name has no
downstream observer. It has one: `pullUpToOutputField` carries `f.Name` into the pulled-up sort
key, so the hidden column's name is the DISPLAY name the re-applied sort renders. PR #698
measured this on master (`InMemorySort([N ASC]` moving with a rename) and the registry entry it
updated says so. Every moved golden record is enumerated and read individually in §5 —
"expected" is not "unreviewed".

**`OrdinalDomain` moves with it, benignly.** `outputFieldDomain` (`:5467`) derives its layout
signature from field NAMES. Producer and consumer both derive it from the same `fields` slice
within one fold, so no token crosses a plan boundary.

---

## 4. Deliverables

1. `collectExtraSortColumns` dedups on the source VALUE (§2a) and names by the resolved path
   (§2b); `nestedResolvedSortKey` extracted and used by all four sites (§2c).
2. Mutation-verified in **both** directions (`co, sk` and `sk, co` are separately falsifiable —
   a fix satisfying one only is not a fix), and **per half**: reverting the naming half alone
   and the value-keying half alone must each go red on something, or one half is unpinned.
   **What each half reds on must be NAMED, not just observed.** In particular: with
   value-keying in place, reverting the naming half leaves two fields spelled `N` in one
   `RecordConstructorValue` — the report must say whether that reds on ROWS (a duplicate datum
   key clobbering) or only on the EXPLAIN golden. If only the golden, then §2b's "not
   sufficient" is a HYGIENE claim, not a correctness one, and §2b must be reworded to say so.
   A two-half fix does not ship with one half's justification unmeasured.
3. The two-sided characterization pin flipped: `broken` becomes the correct order and the
   "now returns the correct rows" branch deleted, as that test's own failure text instructs.
4. `TestFDB_NestedSortKeyExplainRendersTheStructRoot` updated to the faithful rendering, which
   it explicitly anticipates.
5. ~~A quoted-identifier `"N.CO"` arm.~~ **NOT BUILT, and the reason is measured rather than
   waved:** that shape is not constructible as a rows test today, because a flat key spelled
   `N.CO` goes through `stripSortQualifier`, which takes everything after the LAST dot and
   yields the column `CO`. A quoted dotted identifier therefore never reaches the collision —
   it is mangled one layer earlier, by a separate pre-existing gap that this RFC does not
   touch. What IS pinned instead is the guarantee §2e actually rests on, at the level where it
   is expressible: `TestExtraSortColumnsDedupOnTheValueNotTheSpelling` drives two keys that
   spell differently and read the same slot, which is the same name-vs-value independence seen
   from its other side. Recording the non-construction here so the next reader does not
   re-derive it.
6. Plan-shape golden + yamsql scenario updated, each moved record read and justified.
7. **Two** registry entries corrected, refutations kept VISIBLE per repo convention:
   - the `sortKeyFieldRef` contract entry, whose `REFUTED (1)` (the dedup is safe because
     `sortKeySourceValue` depends on the key only through `sortKeyFieldRef`) is true for flat
     keys and false for nested ones since RFC-218 — and whose "`ORDER BY n.sk` sorts by the
     whole struct" is separately measured-false (rendering only; the merged pin proves the rows
     follow the member);
   - the `pullUpSortKeyValue # a EqualFold call # 1` entry (line ~674), which hard-codes that
     the hidden columns are "each named by sortKeyFieldRef's `strings.ToUpper(fv.Field)` at
     4741" — falsified by this change.
8. §2a-i's guard, as CODE not prose: a call-site comment stating why the extras-dedup equality
   is symmetric, a dedup failure message naming the asymmetric-upgrade re-armer, and a unit pin
   that `SemanticEqualsUnderAliasMap` is false for a struct root value versus a member value of
   that root.
9. The first test header's over-broad sentence ("yields [1 2 3] under any first-member-wins or
   serialized-prefix comparison") tightened to the measured truth recorded later in the same
   comment: there is no struct comparator at all; it errors.

---

## 5. Evidence — MEASURED so far vs. STILL OWED

**Measured before this RFC was written** (rev 1 §5 called these pending; that was dishonest and
Torvalds caught it):

- Baseline: three tests green on `db5ac6fd9` (identical to `374e654b9` in both files),
  `--nocache_test_results`, **15 `=== RUN`**, pin holding the broken order.
- Candidate applied (naming half only), patch confirmed applied by `git diff --stat`: both fold
  arms reported `got: [3 2 1] (correct)`, all 8 arms of the rows test still green, EXPLAIN test
  red exactly as it predicts.
- Java citations above read directly from the tree, not inherited from the registry.
- The truncation claim read at `:4794-4826`.

**Census note to measure, not predict:** `sortKeyInOutput` already calls `sortKeySourceValue`
unconditionally above the dedup, so hoisting `val` adds a second call only for keys that
previously short-circuited on `seen` (i.e. `ORDER BY n.co, n.co`), filing one extra
`NoteFieldValueMint` / `recordExistsSortSplit` record. Behaviour-neutral but census-visible —
the unfiltered `docscheck`/corpus floors must be READ, not assumed green.

**MUTATION RESULTS (rev 3), all `--nocache_test_results`:**

| mutation | rows | EXPLAIN | unit pins |
|---|---|---|---|
| whole fix reverted | RED both directions (`[3 1 2]`, `[2 3 1]`) | RED | RED (3 tests) |
| naming half only reverted | **GREEN** | RED | RED (names + duplicate-collapse) |
| value-keying half only reverted | GREEN | GREEN | RED (`DedupOnTheValueNotTheSpelling`) |

**The middle row is the finding** and it is why §2b was rewritten. **The bottom row is a
self-inflicted near-miss worth recording:** the first version of the unit file left the
value-keying half entirely UNPINNED — reverting it kept every test green — because the arm
written for it (`ORDER BY n, n.sk`) has *differing* names and so cannot tell a name-keyed dedup
from a value-keyed one. It discriminated symmetric-vs-asymmetric, which is a different axis.
`TestExtraSortColumnsDedupOnTheValueNotTheSpelling` was added to close it. Without the per-half
mutation this would have shipped as covered.

**Still owed at rev 3:** Plus: per-half mutation
reds; the flipped pin green; the quoted-identifier arm; full unfiltered `sqldriver`,
`explaindiff`, `yamsql`, `docscheck` targets (a narrowed filter withholds the census floors);
the golden diff enumerated record by record.

---

## 6. Rejected alternatives

- **Change `sortKeyFieldRef`** — §2f.
- **Delete the `seen` dedup entirely to match Java literally** — §2a: correct in spirit, but it
  widens the folded row on `ORDER BY n.co, n.co` for no benefit. Value-keying gets Java's
  semantics without that cost.
- **Name-key the dedup and stop there (rev 1)** — NAK'd: leaves the failure class intact and
  merely rarer.
- **Value-key the dedup and stop there** — insufficient: two distinct columns would still be
  spelled `N` (§2b).
- **Bail the fold when two nested keys share a root.** Trades silent wrong rows for a clean
  `0AF00` — better than today, worse than planning it, and Java plans it.
