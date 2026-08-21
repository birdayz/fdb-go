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

- **CTE COLUMN LISTS.** `WITH c("x") AS (…)` still publishes `X` on one of the
  five authorities that apply the list — four are reconciled here, the fifth is
  unidentified and needs instrumentation. Pre-existing (three mutations prove
  it), reproducer and the ruled-out routes booked in `TODO.md` under "A CTE
  COLUMN LIST still folds a quoted alias", cross-referenced from the comment at
  `exactCTEDefinitionRecordType`. The corpus is blind to it for exactly the
  reason §2.1 describes: it drives CTE column lists and it drives quoted
  identifiers, and never both at once.
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
# files=357 queries=2585 …
```

The 19 added queries are this change's own three new scenarios. The 10 changed
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
