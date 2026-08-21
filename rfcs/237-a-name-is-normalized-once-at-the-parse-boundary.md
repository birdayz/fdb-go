# RFC-237: a name is normalized once, at the parse boundary

**Status:** IMPLEMENTED
**Scope:** identifier PRESENTATION at every authority that mints a column name, and identifier LOOKUP at every scope that resolves one.
**Relates to:** RFC-229 (a column states its own name), RFC-232 (field values are resolved field accesses), RFC-226 (a projection states the row it produces).

## 0. The query

Java answers this. Go refused it.

```sql
SELECT q1."id" FROM q1 JOIN (SELECT "id","k" FROM q2) d ON q1."k" = d."k"
```

Not with a 42703 — with an internal layout error:

```
edge lookup D: read as RECORD(id,k), declared RECORD(ID,K)
```

Two authorities disagreeing about one column's name, one letter-case apart. That
error is the shape of the whole defect: nothing is *missing*, two things are
*spelled differently*, and only the runtime edge check is positioned to notice.

## 1. Java

Java folds a COLUMN OR TABLE identifier in exactly one place, and it is the
parser. (Not *every* identifier: `SqlFunctionCatalogImpl.java:77,93,99` folds
FUNCTION names to lower, unconditionally and ignoring the case-sensitivity
option. That is a separate namespace with its own rule and is untouched here —
worth stating because "Java folds in one place" is the kind of sentence that
gets read as a repo-wide fact.)

```java
// SemanticAnalyzer.normalizeString (SemanticAnalyzer.java:146-153)
quoted   -> strip the quotes, keep the case
unquoted -> toUpperCase(Locale.ROOT)
```

Everything downstream compares EXACTLY.
`RecordLayerSchemaTemplate.findTableByName` is `table.getName().equals(tableName)`.
A column's name comes off the descriptor unfolded:

```java
Type.java:2875                     Optional.of(ProtoUtils.toUserIdentifier(fieldDescriptor.getName()))
serde/RecordMetadataDeserializer.java:92   ProtoUtils.toUserIdentifier(storageName)
```

and `ProtoUtils.toUserIdentifier` (util/ProtoUtils.java:79-81) only unescapes
`__2` / `__1` / `__0` (dot, dollar, double-underscore — ProtoUtils.java:39-41).
It never touches case.

So Java's model is one sentence: **normalize at the parse boundary, match
exactly everywhere else.** The DDL wrote the already-normalized spelling into
the descriptor, so for Java the descriptor IS the SQL name.

## 2. What Go did instead

Go folded again, at fifteen further places, in three different roles — and the
roles are what make this a defect class rather than fifteen typos.

**Presentation** — minting the name a column carries:
`values.FieldNameForProtoField`, `rlcatalog.columnForField`,
`values.OutputColumnName` / `DisplayColumnName` / `ProjectionColumnName`,
`expressions.AggregateKeyColumnName`, `logical_result_type.go`'s projection and
label authorities, `cascades_translator.go`'s `legColumns` /
`derivedOutputColumns` / `arrayFieldFromDescriptor`, `ordinal_seed.go`'s
`ordinalLegType`, `logical_predicate.go`'s ON-only CTE schema.

**Consumption** — folding a name *before* asking for it by exact match:
`plan_context_builder.go` (index and primary-key column names),
`index_expansion.go` (the placeholder path and its `FieldByName` requests),
`unnest_seed.go` / `unnest_gather.go` (path segments),
`rowdiff/ordering.go` (the differential harness's ordering check).

**Lookup** — relaxing a match: `recordTypeTable.LookupColumn`'s folded
fallback.

Every one of those was invisible for a `CREATE TABLE`-fed schema, because DDL
already folded the name into the descriptor. Every one of them was wrong for a
QUOTED identifier and for a hand-written `.proto`.

### 2.1 The consumption half was silently disabling index matching

This is the finding that changes the severity, and it was caught by review
rather than by any test.

An index candidate names its key columns PHYSICALLY — the index definition
names descriptor fields — and `expandValueIndex` resolves each one by EXACT
name against a base type whose slots come from `FieldNameForProtoField`.
`NewPlanContextFromIndexDefs` upper-folded those names on the way in. For any
`RecordMetaData` whose descriptor names are not already upper — i.e. every
hand-written `.proto` — the field request missed, `expandValueIndex` returned
`nil`, the candidate declined, and the planner full-scanned.

**Rows stayed correct. Nothing went red.** A 354-scenario yamsql corpus proves
nothing about it, because that corpus is DDL-fed and its descriptors are
already upper.

### 2.2 The one that hid the longest was a function whose name promises a no-op

`functions.StripIdentifierQuotes` is not a quote stripper. It is THE
normalizer — an unquoted identifier comes back UPPER-FOLDED, which is Java's
`normalizeString` — so it is not idempotent and must be applied exactly once.

All three CTE column-alias captures called
`StripIdentifierQuotes(FullIdToName(fid))`, and `FullIdToName` already applies
it per segment. So `"x"` became `x` became **`X`**, and `WITH c("x")` published
a column no reference could name.

It survived a full review lap, and the way it survived is the lesson: the four
sites that CONSUME a CTE's alias list were each audited and each found
faithful, because they were. The corruption was upstream of all of them. A
sweep for `strings.ToUpper` cannot see it — there is no `ToUpper` at the call
site — and a comment was added directly above the surviving fold correctly
describing the bug it sat on.

The class-level check, with its positive control:

```
$ grep -rnE "NormalizeIdentifier\((functions\.)?(FullIdToName|NormalizeIdentifier)\(" --include='*.go' pkg/
logical_predicate.go:8170:  // the parse capture (`NormalizeIdentifier(FullIdToName(fid))`,
                     # ONE hit, and it is a COMMENT naming the retired shape.
                     # Zero call sites — but the count is 1, not 0, and the
                     # difference is the whole point of printing the output.
$ grep -rl 'NormalizeIdentifier' --include='*.go' pkg/ | wc -l
20                   # the control: the symbol is reachable and the pattern
                     # well-formed. It was 19 when first measured; §8's operand
                     # mint is the twentieth file, which is what a population
                     # written into the claim is for.
```

**And the function is renamed.** `StripIdentifierQuotes` → `NormalizeIdentifier`,
81 references. A comment guarding a name that lies is a weaker fix than a name
that does not: `NormalizeIdentifier(FullIdToName(fid))` reads as obviously
wrong at the call site, where `StripIdentifierQuotes(FullIdToName(fid))` read
as a defensive no-op — which is exactly how it survived.

Removing that capture fold then ARMED two more: the recursive-CTE output-row
builders folded too, and had been agreeing with the capture rather than being
correct. A guard whose expected value inverts when a neighbouring fold is
removed — pinned now with a nullable-seeded recursive shape AND its unquoted
control, because a literal-seeded one fails identically for an unrelated
nullability reason and would be mistaken for it in either direction.

## 3. The design

**One presentation rule, one lookup rule, each implemented once.**

### 3.1 Presentation: verbatim, everywhere

A name minted from a descriptor is `ToUserIdentifier(field.Name())` — unescaped,
unfolded, exactly Java's `Type.java:2875`. A name minted from a SQL alias is the
alias as the parse capture produced it: `functions.StripIdentifierQuotes` has
already folded the unquoted case and preserved the quoted one, so there is
nothing left for a later layer to normalize and everything for it to destroy.

`AS "x"` now emits a column named `x`, which is a name a reference can spell.

### 3.2 Lookup: exact at a level, then unambiguous fold at that same level

`Table.LookupColumn` is EXACT — Java's rule. Case-insensitivity moved to the
SCOPE, as a second pass run at each level before falling to the parent
(`resolutionPass` in `semantic/scope.go`).

**THE SHAPE IS JAVA'S. THE DIMENSION IT RELAXES IS NOT.** Both halves of that
sentence are load-bearing, and an earlier revision of this RFC ran them
together and claimed the whole thing as a port. It is not.

The shape: `SemanticAnalyzer.resolveIdentifierMaybe` (SemanticAnalyzer.java:427-438)
runs `lookup(id, operators, true)` over ALL operators of a fragment and, only
when that yields nothing, the same lookup with `false` over the SAME operators,
before walking to the parent. Two passes, whole-level, strict first. That
structure is what is implemented here.

The dimension: Java's flag is `matchQualifiedOnly` (SemanticAnalyzer.java:444-446),
which relaxes whether the reference must be QUALIFIED. Both of its passes
compare through `Identifier.equals` → `String.equals` on the normalized name
(Identifier.java:155-157). **Java never relaxes case, under any option.** So
the relaxed pass here has no Java analogue; see §3.4, which argues it on its
own terms rather than on a precedent.

The per-level placement stands independently of the citation: an inner scope's
relaxed match beats an outer scope's exact match, which is ordinary SQL
shadowing; the alternative silently converts a local reference into a
correlated one.

**The relaxed pass COUNTS, it does not decline** — also a Go decision, not a
port. Two columns differing only by case are two candidates for a folded
reference, so the reference is AMBIGUOUS (42702) and says so. The old per-table
fallback declined on a collision, which reported 42703 — that the column does
not exist — about a name that exists twice. Java, having no relaxed pass, has
no analogous case: its own `case-sensitivity.yamsql` resolves a folded
reference against the single exact match and never reports ambiguity.

The fold did NOT stay on the table for a reason that matters: `Scope` collects
candidates from every source and then adjudicates. A table that relaxed on its
own would let a *relaxed* match at source A compete with an *exact* match at
source B, and a reference with exactly one right answer would come back
ambiguous. That defect was live in the record-catalog path (two raw-proto
tables with columns `aB` and `Ab`, reference `AB`) and replicating the fallback
into `StaticTable` would have multiplied the class instead of fixing it.

### 3.3 What the relaxed pass does NOT cover

Written first, from shapes actually run:

- **qualifiers and source aliases.** They originate in SQL text or in the
  catalog's own already-folded table index, never in a descriptor, so there is
  nothing for a fold to repair. They stay folded.
- **the quoting FLAG — and this is far stronger than "not covered".** The flag
  does not EXIST downstream of the parse capture: `FromNormalized` hard-codes
  `wasQuoted: false` and its own doc says the flag "is not recoverable from the
  captured string", and it is used on the reference path in ~59 places. So the
  engine structurally cannot tell `"K"` from `K` at resolution time, and the
  relaxed pass therefore applies to a QUOTED reference exactly as it does to an
  unquoted one. This was probed rather than reasoned: gating
  `resolutionPass.matches` on `!want.WasQuoted()` is INERT — all four of
  `SELECT "KEEPCASE"` / `"keepcase"` / `KeepCase` / `"Id"` still answer, and
  the corpus stays green — because there is no flag left to gate on.

  It matters because it names the wrong remediation everywhere else: making Go
  conformant here is **preserving the quoting bit through
  `StripIdentifierQuotes`/`FromNormalized`**, not plumbing
  `CASE_SENSITIVE_IDENTIFIERS` (which, per §3.4, would not help — Java keeps a
  quoted name verbatim in both modes).
- **`__2` / `__1` / `__0` escaping** (dot, dollar, double-underscore). Unescaped once, at
  the catalog boundary, by `ToUserIdentifier`.
- **Unicode case folding** beyond `strings.EqualFold`'s simple folding.
- **`values.AccessorNamePath`** (`accessor_name_path.go:67,84,302`), which folds
  BOTH sides of a plan-rule identity comparison. It is consistent with itself,
  so it is not a disagreement — but it can equate two genuinely distinct quoted
  columns `"a"` and `"A"` inside a rule like `PushFilterThroughGroupBy`. That is
  a real latent defect of the same family, in a different mechanism (identity
  comparison, not naming), with its own census and ratchet. It is NOT closed
  here and is called out for its own change.
- **the lateral-unnest AS/AT binding's internal keys.** Two dozen sites still
  upper-fold `u.Alias` / `u.AtAlias` (`unnest_seed.go`, `ordinal_seed.go`,
  `box_conjunct.go`, `clustered_outer_scalar.go`, the translator's leg
  columns). They key the binding's SLOT and QUALIFIER roles and are consistent
  with one another. This was written expecting the divergence to reach the
  user and MEASURED as not doing so: `SELECT "v" FROM QARR, QARR.arr AS "v"`
  reports the label `v`, because the label comes from the projection's naming
  authority, which no longer folds. Pinned in `quoted_identifier_labels.yaml`.
- **`strings.ToUpper(csq.ScalarCol)`** (`clustered_outer_scalar.go`,
  `cascades_translator.go`). `csq.ScalarCol` is now minted verbatim, so this
  fold went from inert to load-bearing — but the two authorities it sits
  between already disagree independently (`exactLogicalResultType` names the
  inner's output from the parse-folded projection text, while the runtime slot
  is keyed by `OutputColumnName`), so removing it picks one of two disagreeing
  sides rather than reconciling them. Flipping both sites verbatim was tried:
  24 of 24 non-Docker targets stayed identical, so no test in reach
  distinguishes the two, and shipping an unmeasurable flip is exactly what the
  mutation discipline forbids. `exactRowShapesAgree` compares field names
  exactly, so it IS decidable — by an FDB/executor test, which is the work.

- **the POSITIONAL EXECUTOR ROW LAYOUT**, which still folds hard enough that
  DDL has to guard against it: `ddl.go` refuses case-colliding quoted columns
  with *"the positional row layout folds identifiers, so case-colliding quoted
  columns are not supported."* That guard predates this change and survives it.

COVERED: column names and struct-field names below them, **as minted from a
descriptor or from a projection's own output naming.** Not every name in the
engine — the four bullets above are names too, and the covered set is the one
that reaches the two authorities §0's error is about.

### 3.4 Why the extension exists at all

Java ships `CASE_SENSITIVE_IDENTIFIERS` (`Options.java:211`, default `false` at
:298, threaded to the analyzer via `PlanContext.java:263`), and its SQL surface
is always DDL-fed — so a descriptor whose field names are not already the
normalized SQL spelling is a corner case there.

Here, `rlcatalog.Wrap(md)` over a user's own hand-written `.proto` is a
first-class entry point and `caseSensitive` is hardcoded `false` at every
production call site with no option to flip. Deleting the extension would make
every such user quote every identifier; plumbing the option instead would break
`SELECT ORDER_ID`. The extension is read-side only, never touches the wire, and
exists **because Go lacks the case-sensitivity option, not because Java's rule
is wrong.**

## 4. What this fixes, measured

- The reproducer in §0 answers `[[1]]`, matching Java
  (`quoted_identifier_columns.yaml`).
- `SELECT *` over a mixed-case quoted DDL column now reports
  `[ID KeepCase PLAIN]` — **byte-identical to Java's answer**, where Go
  previously reported `[ID KEEPCASE PLAIN]`
  (`QuotedIdentifierCaseJavaProbe`, measured against a live fdb-relational).
- A quoted-lowercase nested path (`n."sk"`) resolves, and SELECT / ORDER BY /
  GROUP BY agree on it (`groupby_nested_path_shapes_fdb_test.go`).
- `AS "a.b"` reaches the sort key as `a.b`, not `A.B`
  (`order_by_exact_metadata_test.go`).
- The ON-only CTE schema's case-sensitivity obstruction — which declined an
  entire derived source because `AS "x"` was unnameable — is retired, along
  with `cteBodyAllAliasesCaseSafe` and `cteBodyAliasQuoted`.
- Index matching over a non-upper descriptor works instead of silently
  declining (`quoted_identifier_index_bridge.yaml`, asserted on the PLAN).

## 5. Blast radius

Over the yamsql plan-shape corpus, **not one existing plan changed shape.**
Both numbers below are the commands' output, not a recollection — an earlier
revision of this section quoted "2570 queries" and "35 lines differ" and
neither was reproducible from either artifact:

```
$ git diff f50e87947 HEAD -- …/plan_shape.golden | grep -c '^-shape:'
0                     # zero shape lines removed: the structural claim

$ git diff f50e87947 HEAD -- …/plan_shape.golden | grep -c '^-plan:'
10                    # ten existing plan lines changed, all names

$ head -2 …/plan_shape.golden                    # before → after
# files=354 queries=2566 …
# files=358 queries=2591 …
```

The 25 added queries are this change's own four new scenarios. The 10 changed
`plan:` lines are computed grouping keys (`CASE(WHEN(predicate, TRUE),
['low','high'])` instead of the folded rendering) and the quoted-identifier
scenario's `_current.id` / `_current.k`.

The `-shape:` count is the load-bearing half and the one to re-run: it is zero,
and a fold anywhere in a naming authority cannot move it, so it is a clean
statement that the planner's DECISIONS did not change.

## 6. Guards whose direction inverted

Three guards were watching for the fold and had to be reconciled rather than
deleted, because their expected value changed sign:

- `TestLogicalProjection_AliasesAreSemanticIdentity` and its plan-side twin
  required `output_alias` and `OUTPUT_ALIAS` to be ONE identity, because both
  minted `OUTPUT_ALIAS`. They are now two identities: an alias arrives already
  normalized, so a surviving case difference means two different QUOTED
  aliases naming two different columns. The arm now asserts INEQUALITY.
- `TestPushFilterThroughGroupBy_CaseInsensitive` asserted that accessor
  identity bridged a mixed-case key request to a folded `REGION` output. That
  bridge is no longer on the path at all, so the assertion would have been
  vacuous; it is now
  `TestPushFilterThroughGroupBy_GroupingKeyPublishedVerbatim`, pinning the
  property that made the bridge unnecessary.
- `cte_box_unnest_on_resolution_probe_fdb_test.go` Q55/Q56 required `C."x"` to
  42703 and `C."X"` to answer. Both now answer — `"x"` exactly, `"X"` through
  the relaxed pass — and the arm that still holds is a spelling matching
  NEITHER. That test predicted its own inversion in place, and this is it.

## 7. Tests added

- `quoted_identifier_index_bridge.yaml` — the index bridge, asserted on the
  PLAN (`plan_contains: IndexScan(QIDX_CAT`), because a declining candidate
  returns exactly the right rows and only the plan can see it.
- `quoted_identifier_columns.yaml` — the ON reproducer, plus the folded
  COLLISION (`"kA"` / `"Ka"`, reference `"ka"` → 42702). An earlier draft of
  that collision used `"k"` / `"K"` with reference `"k"`, which proves nothing:
  `"k"` matches one of them EXACTLY, so the relaxed pass is never consulted.
- `rlcatalog_test.go` — the catalog presents the descriptor spelling and NOT
  also a folded twin; `LookupColumn` is exact; `LookupColumnRelaxed` reaches a
  raw-proto name; and the NESTED struct field obeys the same rule (a separate
  test, because nothing in the corpus resolves a nested field over raw-proto
  metadata, so every existing nested test passes either way).
- `plan_context_builder_test.go` — index and primary-key column names reach the
  candidate with their case intact, pinned on the NAME because the failure is a
  silent decline.

## 8. The instrument, and the sixth site it found

§2 found its five sites by grepping for `strings.ToUpper`. That census cleared
files that had live defects and it could not, even in principle, see the CTE
double-normalize — a fold with no `ToUpper` at the call site, spelled as a
function whose name promised a no-op. "Did I find every fold?" is not a
question a sweep can answer.

The replacement asks a question that is checkable. Under the rule in §1 an
unquoted identifier and its upper-folded quoted twin are the SAME NAME, so
rewriting every unquoted identifier in a query to `"UPPER"` is
semantics-preserving BY CONSTRUCTION. Any behavioural difference is a Go
defect — no oracle, no golden, no second engine.

`TestIdentifierAgreementOverCorpus` does exactly that over the yamsql corpus.
It works off `UidContext` nodes rather than the SQL text (`uid : simpleId |
DOUBLE_QUOTE_ID`, so `SimpleId() != nil` IS the unquoted case), plans both
spellings, and requires identical plan text.

Run once, before any further fix, it reported:

```
identifier agreement: 2443 perturbed of 2447 plannable
                      (4 did not plan at baseline)
31 of 2443 statements plan differently
```

**31 disagreements, one structurally distinct class.** Every one was an
aggregate over a COMPOUND argument, whose public output-column name was built
from the raw SQL source slice:

```
SELECT SUM(qty * price)      →  SUM(QTY*PRICE)
SELECT SUM("QTY" * "PRICE")  →  SUM("QTY"*"PRICE")     -- same name, two labels
```

Two repairs downstream — strip the whitespace, upper-fold the composed name —
made the unquoted spelling look right and destroyed the quoted one. On a table
whose columns are genuinely `qty` and `price` (quoted DDL) the damage is
visible without any perturbation at all, and in a single result row:

```
SELECT "KeepCase", SUM(plain) FROM QCASE GROUP BY "KeepCase"
  group key → KeepCase        (verbatim, fixed in §3.1)
  aggregate → SUM(PLAIN)      (folded — the column is named plain)
```

Two naming authorities over one table, disagreeing about what its columns are
called. The group-key half was already fixed here; the aggregate half was the
straggler the census had missed.

### 8.1 The fix

`embedded.aggOperandCanonicalText` becomes the SOLE mint for the operand
segment. It walks the argument's tokens and upper-cases every one — which is
what the old blanket fold did, and is why no unquoted spelling moves — EXCEPT a
delimited identifier, which contributes its inner text verbatim and without its
quotes. Both properties are needed and neither suffices: upper-casing alone
keeps the quotes, stripping the quotes alone still folds `"qty"` to `QTY`.

The two repairs then come out of `expressions.AggregateResultColumnName`,
because a name that arrives canonical must be carried, not edited. The one
`ToUpper` that stays is on a CONSTANT's rendered placeholder, which is this
package's own word for "a literal sat here" and not a user identifier.

Blast radius over the corpus: **not one existing plan changed.**

```
$ go run ./cmd/explain-differ dump -out /tmp/after.txt
$ diff -u …/plan_shape.golden /tmp/after.txt | grep -cE '^[-+]'
0
```

That zero is the load-bearing claim: the rewrite reproduces the old spelling
everywhere the old spelling was right, and differs only where it was wrong.
Re-run after the fix, the gate reports `0 of 2443`.

### 8.2 What the gate cannot see

Written from shapes actually run, because a scope sentence written by
describing the code is the failure that survives every other check.

- **A fold applied to a name that is genuinely lower-case.** The perturbation
  runs unquoted → quoted-UPPER, and those two are precisely the pair a fold
  CANNOT tell apart. Verified by mutation: re-folding the AVG arm of the naming
  authority left this gate green. That axis belongs to the yamsql `columns:`
  arms and to the unit pins in
  `expressions/group_by_naming_verbatim_test.go`; the two instruments are not
  substitutes and neither subsumes the other.
- **Result-set labels.** This compares planned PLAN TEXT. An authority that
  only reaches `executor.ColumnDef` is invisible to it.
- **Rows.** Nothing executes.
- **Already-quoted identifiers, and DDL.** Neither is perturbed — `"KeepCase"`
  has no second spelling, and the schema template is compiled once.
- **Anything that is not a `uid`**: function names and type names are separate
  grammar productions and are never rewritten.
- **Statements whose UNPERTURBED form does not plan.** This one is ASSERTED
  rather than logged, and the reason is a mutation that got away: reverting the
  operand mint to raw parse text made every compound aggregate stop planning on
  BOTH sides, each landed in this bucket, and the gate reported green over what
  was left. The count went 4 → 31 while the verdict did not move. A difference
  test is blind to a defect that breaks both spellings equally, so the size of
  that bucket is the assertion (`identifierAgreementBaseFailCeiling`, alarm
  direction GROWTH).

The floor and that ceiling are both driven by
`TestIdentifierAgreementVerdict`, because the corpus run reaches only the pass
arm — an alarm that has never fired is indistinguishable from one that cannot.

### 8.3 Guards whose expected value inverted, again

Thirty-five `OperandName` / `Alias` literals across 8 test files fed a
non-canonical spelling (`"price"`, `"total"`, `"cnt"`, `"sum"`, `"order_id"`)
and asserted the upper-folded result — the count is
`git diff -U0 -- '*_test.go' | grep -E '^\+' | grep -cE 'OperandName: *"|Alias: *"'`
over this change, excluding the two files it adds. They were
pinning the repair, not the contract: production has always minted the
canonical spelling, and the fold is what let a fixture's `sum` meet a
predicate's `SUM`. Each is now fed the spelling production produces. The one
that inverted most cleanly is
`TestAggregateResultColumnName_OperandNameDataWinsOverTheValue`: its input
(`"PRICE * QTY"`) is unchanged and its expectation moved from `SUM(PRICE*QTY)`
to `SUM(PRICE * QTY)`, which is the stronger form of the claim it always made —
the carried text is not consulted, not repaired, and not re-derived.

## 9. The label axis found a second silently-declining index

§8.2 says the agreement gate is blind to a FOLD, because its perturbation runs
unquoted → quoted-UPPER and those are exactly the two spellings a fold cannot
tell apart. That is not a hedge — the blind spot cashed out immediately, and
what found the defect was the other instrument.

Six scenarios in the whole corpus carry a `columns:` assertion and NONE of them
was on an aggregate-index query, so
`quoted_identifier_aggregate_labels.yaml` was written to cover the two plan
shapes that put no projection above the aggregate. Four of its five arms passed
on the first run. The fifth did not:

```
plan does not contain "AggregateIndex":
  Project([_current.Region#0, _current.SUM(Amount)#1],
    StreamingAgg(keys=[_current.Region#1],
      InMemorySort([_current.Region#1 ASC], Scan(SALES))))
```

An aggregate index declared `AS SELECT SUM("Amount") FROM sales GROUP BY
"Region"` was never being chosen. Right rows, a full scan plus an in-memory
sort, nothing red anywhere. The unquoted twin of the same query picks the
index, which is what makes it a case defect rather than a matching one — the
same §2.1 shape, one layer down, and armed by the same removal.

One fold, in `cascades_generator.tryAggregateIndexCandidate`: it upper-folded
the index's group and aggregate column names, which arrive from the key
expression carrying the DESCRIPTOR's spelling. The consumer compares them
exactly, so a quoted column offered `REGION` where the query held `Region`.
Same §2.1 shape — a fold on the CONSUMPTION side of an exact match, invisible
while the other side folded too.

Blast radius, same command as §8.1: **not one existing plan moved.** The corpus
is all-unquoted DDL, where the fold was a no-op; the change is visible only on
the shapes that had no coverage at all.

**A second fold was removed and then PUT BACK, and the reason is worth
keeping.** `values.AccessorNamePath` also upper-folds, and the obvious reading
is that it is one more site of this class. Removing it did fix the probe — but
so did the candidate fix alone, which is how the redundancy was discovered.
The suite then said what the corpus could not: five tests pin case-insensitive
accessor identity as DESIGNED (`lazyFlat("city")` equals `lazyFlat("CITY")`;
`bakedFused("ADDR","City")` equals `bakedFused("addr","city")`), so that fold
is a deliberate match-domain rule, not a stray normalization.

Whether it should SURVIVE RFC-237 is a real question and this RFC does not
answer it. What it does record is why the corpus cannot answer it either: every
corpus descriptor is DDL-fed and already upper, so removing the fold moved zero
plans — a green that establishes nothing about the property. Answering it needs
a fold-dependence count (how many accessor comparisons succeed ONLY under
folding), which is the instrument the leg-identity censuses already implement
for their own sites and report `foldOnly=0` on. Until that count exists for
this site, the fold stays, and the smaller fix is the one that ships.

The generalisable part is the ordering. The gate is what makes the CLASS
tractable — 31 sites in one run, with no oracle. The `columns:` arms are what
reach the half the gate cannot see. Neither found what the other did, and the
census that preceded both would have found neither: there is no `strings.ToUpper`
census that flags `strings.ToUpper(allCols[i])` as wrong, because it is only
wrong relative to what the other side now carries. That last point cuts both
ways, which is the paragraph above — the same census would flag
`AccessorNamePath`'s fold as wrong, and there it is right.

### 9.1 Two folds nearby that are NOT defects, and how that was established

Both were checked rather than assumed, because "it looks like the same class"
is how the accessor-path detour started.

`cascades_generator`'s three `executor.ColumnDef.Name` folds (`buildAggColumns`
twice, `deriveColumnsFromAggregateIndex` once) are the obvious next suspects —
they fold a name on the result-set path. **Mutation says they are observed by
nothing.** Replacing all three with `strings.ToLower` — a mutation that
survives nogo, unlike the `"ZZMUT"+` first attempt, which failed the LINT and
would have been read as a passing run — left the entire 359-scenario yamsql
corpus green AND 161 aggregate/group/scalar/count real-FDB `sqldriver` tests
green. Every aggregate plan carries a projection above it, and that projection
owns the output names. What pins this going forward is not a comment: the five
`quoted_identifier_aggregate_labels.yaml` arms assert the labels for both
aggregate shapes, so a change that ever routes them through these folds
reddens.

The accessor-path fold is the other, and §9's note above is the record of why
it stays. Neither is filed as future work; both are answered.

## 10. Review found three more, and two of them were claims this RFC made

§8.1 called `aggOperandCanonicalText` "the SOLE mint" for the operand segment.
That was false when written, and it is the exact failure §8.2's preamble
legislates against: a scope sentence produced by describing the code just added
rather than by probing what it covers. `AggregateSpec.OperandName` is set from
`logical.AggregateCall.Operand`, and that field has **three** producers in
non-test sources, not one.

**The correlated scalar subquery had its own fold, and it was live.**
`canonicalAggName` (`logical_predicate.go`) did
`strings.ToUpper(ColumnNameValue(operand))` — verbatim the repair §8.1 removed
from `AggregateResultColumnName`, still standing one route over. Measured:

```
SELECT o.id, (SELECT SUM(i."Amount") FROM inner_t i WHERE i.k = o.k) …
  -> _current.SUM(I.AMOUNT)        -- correlated-scalar route
SELECT k, SUM("Amount") FROM inner_t GROUP BY k HAVING SUM("Amount") > 1
  -> _current.SUM(Amount)          -- GROUP BY / HAVING route
```

Two routes to one name, disagreeing, on a column declared `Amount`. The
whitespace strip stays: it normalizes the RENDERING's spacing, both sides of
every comparison there derive from `ColumnNameValue`, and it touches no
identifier. The second producer (`Operand: strings.ToUpper(bareArg)`, with its
`name` twin) is now verbatim too — the two must move together, because
`CanonicalName()` recomposes `Func + "(" + Operand + ")"` and `name` is the
alias the slot publishes under.

**Adding the `columns:` arm the review asked for found a third defect that has
nothing to do with case.** The derived label for an unaliased correlated-scalar
aggregate was `AMOUNT)`. Not folded — *mangled*: `parseColRef` split
`I.SUM(I.AMOUNT)` on its LAST dot, giving table `I.SUM(I` and column `AMOUNT)`,
and that fragment was the result-set label. The split now ignores dots inside
parentheses, because a qualifier is never inside one. Three defects on a single
arm: `AMOUNT)` → `SUM(X.AMOUNT)` → `SUM(X.Amount)`.

**And a claim about the MECHANISM was wrong, which is worse than a wrong
number.** §9 said the aggregate-index candidate's folded names failed an exact
match in `AccessorNamePathMatchesNames`. They cannot have: that function folds
the candidate (`accessor_name_path.go`), so it matched fine. The real decliner
is `LookupFieldUnique` on the aggregate operand
(`rule_aggregate_data_access.go`), bottoming out in
`RecordType.fieldNameScan`'s `f.Name == name`. The fix was right and the
comment pointed the next reader at the wrong file; it now names the verified
consumer.

That correction also settles §9's open question in one direction. The
`ToUpper` on the candidate in `AccessorNamePathMatchesNames` is not a stray
fold to delete — it is one half of a PAIR with `AccessorNamePath`'s fold on the
value side. Delete either alone and a verbatim candidate can never meet an
upper-folded path; that is precisely what was measured when both were removed
and then when the value side was restored. They are one decision, and the site
now says so.

Blast radius of this round, same command as §8.1: **zero existing plan lines
removed.** Only the three new corpus arms appear.
