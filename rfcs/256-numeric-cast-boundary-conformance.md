# RFC-256: Numeric CAST boundary conformance

Status: Implementation under review. CI race failures and their repairs are
recorded in the evidence ledger below, superseding earlier local release gates.
PR review and CI approval are still required before merge.

## Finding and reference

QSC numeric-consumer authoring found that the existing CAST corpus asserts Go-only
floating overflow rejection as Java parity. Read Java 4.12.11.0 `CastValue.java` in
full, especially physical operators at lines 137–165; read Go `castEvaluated`,
`javaMathRound`, and the separate system-table `functions.CastValue` path.

`NumericCastBoundaryConformance` runs the existing plandiff setup runners against
real Java and Go, asserting hand-derived answers independently for each engine.
Integer-to-STRING projection inside SQL preserves all bits across the existing
JSON runner, which otherwise normalizes numbers through float64. The original six DOUBLE probe families:
Java agrees with their explicit answers; old Go fails literal/stored saturation and
INTEGER narrowing, while rounding/zero and half-tie controls pass. Examples:

- `CAST(1.0E20 AS BIGINT)` → Java Long.MAX; Go 22F3H.
- `CAST(-1.0E20 AS BIGINT)` → Java Long.MIN; Go 22F3H.
- `CAST(1.0E20 AS INTEGER)` → Java -1; Go 22F3H.
- `CAST(2147483647.5 AS INTEGER)` → Java -2147483648; Go 22F3H.

The root is Go's deliberate unsaturated float-returning `javaMathRound` plus range
rejection in DOUBLE-to-integer cast arms. Java instead returns an integer from
Math.round, saturating to signed-64 bounds, then narrows to int for DOUBLE_TO_INT.
The map-path duplicate additionally uses naive `floor(x+0.5)`, which misrounds the
largest double below 0.5 and odd integers between 2^52 and 2^53.

## Decision

1. Make the existing double round helper return int64, exactly implementing Java's
   integer bit-shift rounding and saturating fallback. Reject nonfinite values at
   the CAST boundary as Java does; retain distinct INT/LONG messages. Do not use
   float64 to carry Long.MAX_VALUE (it rounds to 2^63).
2. DOUBLE_TO_LONG returns that integer. DOUBLE_TO_INT narrows the integer to int32,
   then transports it as int64. LONG_TO_INT range rejection remains unchanged.
3. Dispatch floating casts using the declared source type. Java FLOAT uses
   Math.round(float), whose result is int32 and saturates at 32-bit bounds even in
   FLOAT_TO_LONG. Match the numerical result and the declared SQL target, not JVM
   boxing accidents. Pin FLOAT/DOUBLE distinctions and float32/float64 carriers;
   preserve existing nonfinite rejection. Confirm this width rule against live Java
   before relying on it. No changes to implicit coercion, scalar integer arguments,
   index bounds, or arithmetic overflow policy.
4. Expose `values.CastEvaluated(value, source, target)` around the existing
   `castEvaluated` implementation: no throwaway child ConstantValue or Evaluate
   recursion. Array elements and scalar evaluation use the same mechanism.
   `functions.CastValue` remains an untyped Go API: float64 means DOUBLE and
   float32 means FLOAT, explicitly documented; route those two integer-target
   arms through CastEvaluated and translate only typed InvalidCastError.
   Its SQL caller cannot use that carrier contract: MAP eval has ALREADY erased
   the type of nested FLOAT casts. Fix the loss at its source by replacing
   `filterSysRows`'s legacy interpreter with the existing typed expr.Resolver
   and Predicate.Eval. Give INFORMATION_SCHEMA rows an explicit semantic column
   schema (STRING except COLUMNS' BIGINT ordinal), bind qualified/aliased names
   in semantic.Scope, and evaluate against the same ordinal row layout. Construct
   the predicate once per query, not per row, including when the table is empty.
   Preserve Session's statement clock and map typed errors through the existing
   error mapper. This is a Go-only table source, not a second query framework.
   Delete the now-unreachable eval_map/eval_predicate_map and scalar dispatch
   interpreter; retain statement clock shims where used. No source type inference
   from runtime row values, no AST string matching, no fallback interpreter.
   Test system-table WHERE casts end-to-end (nested FLOAT, DOUBLE, INTEGER, NULL,
   rounding and saturation) plus ordinary qualified/aliased column predicates,
   three-valued logic and error paths. Existing unsupported relational system-table
   query shapes remain rejected. The shared expression compiler's additional
   scalar reach is an allowed read-only extension, not a reason to retain a
   divergent evaluator.
5. Correct proven-wrong expectations and parity comments everywhere they occur.
   Keep original dotless-exponent Go cases as Go syntax tests; use decimal-point
   spellings for live Java input. Existing integer/string overflow rejection and
   NaN/Infinity errors remain negative controls.

## Test campaign

Use the existing yamsql exact codec, metadata and driver-arg execution plus the
existing values/functions unit targets and live cross-engine conformance target.
No extension to the RFC-255 90-cell envelope: this is a separate finite cast slice.

- Hand-derived vectors at ±0, either side of ±0.5, ties, odd integral doubles above
  2^52, int32 wrap boundaries, ±2^63 and adjacent representable doubles, and ±1e20.
- Literal, driver-text-transport and stored-column producers; INTEGER and BIGINT
  consumers. Project input bits/DOUBLE metadata alongside exact int64 output and
  INTEGER/BIGINT metadata. Check first-level CAST and nested STRING separately.
- Include FLOAT width controls, SQL NULL, nonfinite rejection, integer-source
  narrowing rejection, and arrays whose element casts re-enter the scalar path.
- The independent oracle starts with big.Rat.SetFloat64 (exact IEEE value), adds
  rational 1/2, and takes mathematical floor via quotient/remainder (adjust negative
  remainder). Clamp the big.Int at source operator width: 64 for DOUBLE; 32 for
  FLOAT after binary32 quantization. Only then narrow DOUBLE-to-INT modulo 2^32.
  Do not use production constants, helper logic, math.Round, floating floor(x+0.5),
  or float-to-int conversion to establish expected results.
- Applied/built mutants for saturation, narrowing, source width, rounding and
  metadata/carrier loss must fail their intended assertions; restore and verify.
- New files wired with Gazelle/tidy and observed under uncached Bazel. Full
  `just test`; milestone implementation reviews after design ACK and implementation.

Performance: no planner/cost-rule change. Retain O(1) scalar conversion; reuse the
existing constant-time bit algorithm. Run million-row stress before/after on two
same-filesystem worktrees, record both revisions and load; do not attribute unrelated
workload noise or source-disk differences to this scalar correctness fix.

## Explicit exclusions

This slice does not claim all CAST pairs or all consumers: string/Boolean/temporal
casts, nested record coercion, literal lexical parity, plan diversity, cache/paging
and index-comparison rounding windows remain outside it. No claimed completion of
QSC-01 through QSC-07 follows from a bounded numeric-cast envelope.

## Design revision 2 evidence and acceptance details

The FLOAT width probe now independently confirms Java INTEGER results at ±1e20,
2^31 and the binary64 predecessor of 0.5 quantized to FLOAT. A separate retained
probe confirms an upstream FLOAT_TO_LONG boxing defect: Java returns Integer from
its physical operator, then fails constructing the LONG-typed result record with
`IllegalArgumentException: Wrong object type used with protocol message reflection.`
Go will preserve Java's numerical operation but emit its canonical LONG carrier.
The live test pins the Java failure specifically (not arbitrary errors) and Go's
explicit expected numeric answer independently. This is a deliberate boundary
repair, not claimed live FLOAT-to-LONG result equivalence.

`javaMathRoundFloat(float32)` may reuse exact double rounding after exact widening,
then clamp the integer at int32 bounds: every binary32 value is exactly representable
as binary64, so nearest-integer/ties-to-positive-infinity commute with that widening.
It must NOT wrap. DOUBLE's helper clamps at int64 bounds before Go conversion.
The source type selects the operation even for a mismatched-width Go carrier:
FLOAT/float64 quantizes first; DOUBLE/float32 widens first.

All driver, array, metadata, nonfinite and system-table cases above are **acceptance
criteria**, not completed coverage. Only the live probe outcomes explicitly stated
above have run. The source-specific errors remain negative controls; FLOAT-to-LONG
boxing is the named upstream exception. No SQL CAST source width will be inferred
from the untyped functions API's Go carrier after removing its legacy SQL caller.

## DFS continuation: computed derived-array UNNEST (design)

The retained real-FDB array tests found another live Java/Go divergence:

```sql
SELECT CAST(x AS STRING)
FROM (SELECT CAST([1.0E20, -1.0E20] AS BIGINT ARRAY) AS a
      FROM cast_values WHERE id = 1) q, q.a x
```

Java returns `9223372036854775807` and `-9223372036854775808`; Go raises
0AF00, `unnest over a computed/non-passthrough CTE/derived-table output is not yet
supported`. Evidence: `live-array-route.log` in `/var/tmp/query-grind-cast`, and the
retained `computed array unnest` live conformance probe. This is a valid shared SQL
shape, not a rejected candidate. It must be fixed before completing this campaign.

Read Java `LogicalOperator.java`, especially `generateCorrelatedFieldAccess`: the
resolved expression's declared ARRAY type determines the Explode element type;
there is no passthrough-projection or base-table whitelist. Go's
`classifyDerivedUnnestArray` still implements that obsolete whitelist, even though
`ExactLogicalResultType`, `derivedOutputColumns`, and the ordinal collection builder
now carry the projected array type. Its header's UNKNOWN-type justification no
longer describes the available machinery.

**Decision:** replace descriptor backtracking with classification of the bound
owner's actual output schema. Reuse `derivedOutputColumns` / existing CTE defining-
scope traversal and column-list renaming, and require a fully exact RecordType
before classification. An inline derived owner takes its own output columns; a
WITH reference resolves the visible scan to its CTE table name before consulting
CTE maps (a range alias must not accidentally select a same-named unused CTE).
Use the existing `seedFieldIndex` exact-first / unique-case-folded lookup to walk
semantic path segments through that type. Preserve quoted/dotted single segments.
The final ARRAY supplies the element type; a present scalar gives the existing
INVALID_COLUMN_REFERENCE error; missing or non-record intermediate paths give
UNDEFINED_COLUMN; genuinely untyped owners remain explicit unsupported errors.
Do not reconstruct a fake scalar expression or retain descriptor backtracking as
a second fallback. Execution stays on the existing ordinal Explode/FlatMap path.

This retires `recordDerivedUnnestSplit` and its passthrough-only helpers. Reconcile
the existing qualifier census, not a new instrument: remove that site's population
floor, retain its site identity, and add an always-on retired-call growth check
(including CARRIED) with unit pins for every class and the filtered-corpus case.
Remove the obsolete recorder gate subject; replace its positive wiring test with
an actual classifier test and a zero-traffic retirement assertion. No made-up
CARRIED calls may keep an obsolete population floor green.

Acceptance: the live minimal query, stored and expression-array FLOAT/DOUBLE casts
through this route, NULL and empty expression arrays, CTE and derived aliases,
column-list renaming, quoted/dotted fields, nested derived sources, ordinary scalar
and missing-field diagnostics, and existing passthrough/ordinality/owner-shadowing
regressions. Assert output values/types and an observed Explode/FlatMap plan witness
where applicable. The old computed-scalar 0AF00 expectations must become the Java
non-array diagnostic, never success. Any further valid failing shape is DFS work.
The existing two-run merge-base stress baseline precedes both changes; compare the
completed implementation on the same filesystem, twice, after the final edits.

The earlier array INSERT fixture contained NULL elements and correctly failed at
the protobuf write boundary (`MessageHelpers.coerceArray` rejects NULL elements).
The exact INSERT is retained as a negative test; expression-array NULLs are tested
separately from stored arrays. No stored-null-element coverage is claimed.

#### Array design revision 3 — bind the collection before translation

The initial `derivedOutputColumns` classifier plan above is **superseded**.
Both array-design reviewers NAKed it: neither a best-effort field slice nor a
runtime row's deduplicated keys is the SQL binding. Snapshotting a fabricated
record does not repair that loss. The implementation instead follows Java's
`resolveCorrelatedIdentifier(...).getUnderlying()` boundary:

1. Bind a lateral collection while its defining SELECT's semantic sources are
   available. Reuse `buildSelectScope` over the **preceding FROM prefix**, not the
   final SELECT scope: a later source must not participate, and a later RIGHT /
   FULL join must not prematurely null-extend the collection's input. Resolve
   the untouched normalized identifier segments through `expr.Resolver` in an
   inner scope over that prefix, as the existing correlated-primary UNNEST does.
   This deliberately obtains a checked source-relative correlation even when
   the prefix contains a single source. Existing scope resolution supplies CTE
   lexical visibility, duplicate-label diagnostics, declared nominal types,
   visible labels versus physical slot names, and minted owner binding IDs.
   Chained virtual sources retain the existing structural wrapper-path rule.
2. Attach the result to the actual `LogicalUnnest.CorrelatedCollection` on the
   logical join spine. Match parsed and logical FROM positions structurally,
   validate the node's segments/AS/AT/binding, and report a mismatch rather than
   selecting a node by alias. Extend the field's contract to ordinary lateral
   collections; correlated-primary EXISTS keeps its existing behavior. Invoke
   this binding at the existing catalog-aware logical-build boundary, while its
   lexical `cteScopes` still exists. Do not reconstruct that environment from
   the translator's ambient maps or walk into a derived definition as though
   it were visible in the outer FROM scope.
3. The ARRAY/element decision reads the bound collection's exact type. A resolved
   scalar gets the existing non-array diagnostic; unresolved identifiers retain
   semantic undefined/ambiguous errors. An unavailable exact semantic source
   stays an explicit unsupported error, not descriptor recovery or UNKNOWN.
   Existing semantic-column limitations (nullable elements and nested arrays)
   are not silently accepted as exact. The finite CAST population uses explicit
   FLOAT/DOUBLE/INTEGER/BIGINT ARRAY targets; expression NULL elements require
   their own live/runtime verdict and must not earn stored-NULL coverage.
4. Lower the same collection into the existing ordinal Explode/FlatMap path.
   Its checked correlation, root ordinal and nested ordinal path are the input;
   `Segments` remains diagnostic syntax, not a second runtime lookup. Reuse the
   existing source/element windows to map its bound root into the actual outer
   row, then use checked correlation/ordinal translation. A source-window or
   exact-type mismatch is loud. Preserve genuine whole-element bindings for
   chained unnests and the AS/AT two-slot shape; no name-model fallback and no
   synthetic record for classifier admission. Typed SQL-derived owners no
   longer need `derivedOwnerBody`, passthrough projection tracing, or
   `recordDerivedUnnestSplit`. Lower-level IR fixtures must provide their real
   resolved collection rather than manufacture a census event.
5. Retire that census site's traffic honestly: keep its stable identity, remove
   its old population floor, and enforce zero **all calls**, including CARRIED,
   independently of corpus filtering. Unit-drive every guard arm and vacuity
   boundary. Replace the recorder's obsolete AST wiring subject with assertions
   on the live typed binding/consumption and the retirement guard.

The acceptance population still includes the live minimized CAST query, stored
and expression producers, literal NULL/empty collections where admitted, CTE
column lists and definition chains, aliases with dots/quoted case, nested record
arrays, duplicate labels/owner shadowing, scalar/missing errors, existing
passthrough/ordinality shapes, and an observed Explode/FlatMap witness. New
mechanism implementation awaits the revision-3 design verdicts.

#### Mutation evidence (scalar/system slice)

`/var/tmp/query-grind-cast/mutation-results.log` records seven applied, compiled,
semantically killed and SHA256-restored mutants: positive long saturation,
DOUBLE-to-INT narrowing, FLOAT width, `floor(x + 0.5)` rounding, integer carrier,
statement clock and quoted system-table alias. Individual full outputs are
`mutation-<name>.log`. Five are killed by `TestNumericCastBoundaryAnswers`, one by
`TestSystemFilterStatementClock`, and the alias mutant by real-FDB
`TestSystemTableTypedCastFDB`. The restored focused values/functions/embedded
run passed all three selected targets (`mutation-restored-green.log`). This is
not an array completion claim and not a claim that the full corpus ran.

Revision-3 integration contract (resolves the design-placement/error findings):

```go
func bindLateralCollections(op logical.LogicalOperator, sq *selectQuery,
    md *recordlayer.RecordMetaData, schemaName string,
    cteScopes map[string]semantic.ScopeSource) error
```

This is the one attachment function. Call it in BOTH
`buildLogicalPlanForSelectWithCTECatalog_postBuildUnfolded` and the PlanVisitor's
catalog-upgrade path, immediately after their `needRebuild` blocks and before
projection/predicate binding. Earlier table-first demotion/classification remains
before it. No subsequent replacement may drop its values; apply it to the final
spine, never transfer bindings between trees. Peel transparent SELECT wrappers,
follow CTE `Main` (never `Body`), collect the left-deep logical join spine, reverse
it to FROM order, and require its right-source count to equal `len(sq.joins)`.
For each position classified as lateral, require that very right child to be a
`LogicalUnnest` with matching normalized segments, AS, AT and binding ID; a missing,
extra or different node is `ErrCodeInternalError`. Do not pair by alias.

For prefix `i`, use a local `prefix := *sq; prefix.joins = sq.joins[:i]` and the
same lexical CTE map. Factor the EXISTING scope builder into
`buildSelectScopeChecked(...) (*expr.Resolver, error)`, with the legacy
`buildSelectScope` delegating and retaining its current nil-return contract for
its existing callers. The checked form preserves `ResolveTable` and `AddSource`
errors. Its derived-source helper similarly gets an error-returning authority
with the existing bool API delegating: actual body-build errors survive; genuinely
unrepresentable exact source schemas produce explicit unsupported errors. A CTE
tombstone is unsupported, not permission to consult a same-named catalog table.
The binder maps semantic errors through the existing `mapPredicateWalkError` and
propagates others; lookup failures from `ResolveIdentifierPath` retain their
undefined/ambiguous-column classifications. Use the empty child scope over the
prefix for the collection resolution. Neither builder mutates the input query or
any caller-owned scope registry. This is a refactor of the existing builder, not
a second resolver or fallback.

`translateUnnestJoin`, `exactUnnestLegColumns`, `exactLogicalResultType` and chained
lowering consume the same carried collection. Every non-nil carried collection
is authoritative for its type, correlation and ordinal path; source syntax is
not an alternative lookup channel. Raw lower-level IR pins supply real bound
values. These concrete changes are part of revision 3, not a new option list.

#### Integration findings and bounded results

The revision-3 design delta reviews ACKed the checked-scope/final-spine contract
(`design-array-v3-{graefe,torvalds}-delta.log`). In addition to the two ordinary
SELECT final-rebuild sites, the custom correlated EXISTS builders (fast path and
predicate path) and correlated scalar builder reconstruct FROM themselves. The
same binder must run after each completed join loop, before any return or filter
construction. No scope reconstruction is added to translation. Existing correlated
inner-unnest plan pins and new binding assertions caught the missing EXISTS paths
and pass after these attachments (`binding-exists-{red,green}.log`).

CTE column aliases previously rebuilt a scalar-only Column and dropped IsArray
and nominal STRUCT metadata. Renaming now copies the entire column; its physical
projection slots use the record constructor's deduplicated names while SQL-visible
labels remain intact. Unnest virtual sources likewise preserve the complete
scalar/record element column instead of copying a subset of its type.

The original computed-NULL-element positive expectation is superseded, not
credited: SQL ARRAY target lookup forces non-nullable elements (Java
SemanticAnalyzer.lookupType and expr.walkSpecificFunction). Java CastValue retains
the NULL but the live derived query throws NPE; Go's exact layout rejects that
invalid element without a panic. `live-array-bound.log` pins that negative outcome
independently in both engines for DOUBLE→LONG. `TestNumericCastArrayFDB` retains
all four original NULL-element queries as Go typed LayoutNullabilityMismatch
checks using GoSQLSetupRunner, alongside 17 yamsql statements: four source/target
pairs over stored arrays, non-NULL expression arrays, empty arrays and NULL
containers, plus the original rejected stored-NULL INSERT. The focused run is in
`array-bound-results.log`; final-tree reruns and full array/unnest coverage remain
outstanding. This does not claim Java NULL-element result parity or stored NULL
elements. Nullable containers remain valid empty UNNEST inputs.

#### Quoted derived-owner path: source-token transport amendment

The real-FDB route matrix (`bound-array-routes-red.log`, index 0) finds an earlier
error than collection lowering: a derived alias `"q.q"` is correctly bound by the
semantic builder, but resolveQualifiedTableNames later splits its alias-carrier
LogicalScan.Table string and reports `Unknown database q`. A flattened name cannot
distinguish one quoted identifier from schema qualification. Java carries an
Identifier's qualifier list separately from its name (SemanticAnalyzer.tableExists).

Decision: carry the existing primary `sourceSegments` / join `segments` into
LogicalScan.TablePath. Extend NewScan with optional variadic path segments, copied
at construction; SQL builders pass captured tokens, and synthesized derived alias
scans pass exactly one literal alias segment. ResolveQualifiedTableNames uses
those segments through a shared ResolveQualifiedTablePath helper; the existing
string API delegates by splitting for its legacy string callers. This is token
transport to the existing validator, not a new resolver, alias-shape heuristic or
CTE membership exemption. A one-segment `"q.q"` stays literal; two segments `q.q`
still undergo schema validation even beside a same-spelled CTE. DML's existing
string entry points remain outside this amendment's claim.

Regression population: quoted dotted derived owner with dotted array output,
quoted dotted CTE reference, an actual schema-qualified table, wrong-schema error,
and same flattened spelling with one versus two captured tokens. The retained
real-FDB route supplies the execution witness; direct path tests cover malformed
and empty paths without interpreting SQL text.

Attachment census for this amendment (non-test `embedded` constructors):
`buildLogicalPlanForSelect` and `PlanVisitor.visitFrom` primary and joined scans;
`buildOuterPlanOnDerived`; `correlatedSubqueryJoinRight`,
`buildDerivedInnerCarrier`, `correlatedInnerPrimarySource`, and direct primary
scans in `buildCorrelatedExists` / `buildCorrelatedScalar`; and
`demoteSchemaQualifiedUnnest`. The two alias-inventory-only scans in the correlated
builder also receive their captured path. Demotion supplies the already validated
bare table as one segment. Index-on-source and DML scan constructors remain legacy.
Any scan reconstruction preserves TablePath. No SQL-originated SELECT scan may
leave it nil: completed-plan invariant tests walk children AND subquery side plans
for both frontends, including rebuild and correlated routes.

Nil TablePath explicitly denotes legacy/programmatic input. A non-nil empty path
is malformed, not legacy. Construction clones the slice while preserving that
nil/empty distinction; path validation rejects empty segments and arity zero or
three-plus with the existing internal-error code. Default-alias lockstep compares
Alias with the pre-resolution Table. A dedicated constructor test mutates the
caller slice and checks isolation. The legacy string helper retains its existing
empty-string behavior; the new typed path helper does not admit it.

The virtual Graefe/Torvalds design delta reviews approve this direction subject
to the explicit migration/invariant requirements above (`design-table-path-*.log`).

#### Lateral aliases: repair the carried-binding conversion

Live JVM evidence (`live-array-collision.log`, `live-ordinal-alias.log`) refutes
RFC-142's blanket FROM duplicate-alias claim. `FROM derived q, q.a q` is valid and
returns the element. `FROM derived q, q.a x AT x` is valid with an unused output;
referencing `x` reports 42702 `Ambiguous reference X`, not 42712. These exact probes
are retained in NumericCastBoundaryConformance and remain red on Go.

Decision: complete the existing per-source Binding transport, not add another
namespace. `unnestVirtualScopeSourceWithElement`, `unnestSourceCorrelation` and
`sourceBinding(LogicalUnnest)` consume the parser's existing Binding. Scope.AddSource
allows same SQL aliases with distinct binding correlations, including shadowing
unnest sources; actual correlation reuse remains a loud error. Remove the parsed
and logical FROM-level alias-name rejection passes. Raw logical inputs still need
unique correlation identities; the translator checks identities, never display
aliases. Existing per-attribute resolution adjudicates ambiguous references.

The parser mint authority must key implicit array aliases by their effective
AS/default name as well as explicit aliases, and reserve later effective names
against mint-shaped user aliases. A potential schema-qualified table with an
implicit alias has the same final bare alias as the array default; table-first
classification remains unchanged. Capture explicit alias presence where equality
with rendered table text currently loses it. No lowering-site mint or alias
re-derivation is permitted.

AS==AT is a duplicate SQL output label, not a duplicate quantifier. Preserve both
semantic columns (so a reference is 42702), but deduplicate the physical
WITH-ORDINALITY row field names with values.DedupFieldNames, matching the carried
virtual FlowedColumns. The element and ordinal remain distinct ordinal slots.
The raw AT-only `_0` input uses the same physical deduplication, not a special
alias rejection. Remove the obsolete alias guard after its consumers migrate.

Finite regression axes: explicit/default AS colliding with an earlier owner;
later source reusing an unnest alias; chained owner reuse; mint-shaped quoted
aliases; AS==AT with unused versus referenced output; WITH ORDINALITY and raw
AT-only physical-name collisions; and deliberately malformed raw duplicate
correlations. Cover both frontends, raw IR, real FDB and the retained live JVM
cases. Original negative tests must retain their semantic ambiguity/correlation
invariant at the correct boundary rather than being deleted wholesale.

#### Alias integration finding: ARRAY element metadata is not a STRUCT value

The original duplicate-alias and AS==AT probes now pass (`live-alias-transport.log`).
The expanded chained probes found two further gaps. The shared projection upgrade
was dropping AmbiguousColumnError inside CAST/arithmetic; it now preserves the
existing typed error mapping, pinned in both frontends and live Java. Reusing a
chained alias remains legal when unused; referencing both element outputs is 42702.

Active exact reproduction: `SELECT 1 FROM t AS items, items.items, items.n.vals x`,
with `items` an ARRAY<item> whose STRUCT element has `n.vals DOUBLE ARRAY`.
Java returns two rows; Go reports false 42702 (live-default-chained-owner.log).
`Column.StructFields` carries ARRAY element metadata for UNNEST. Both exact and
relaxed nested lookup currently treat those fields as if the container itself
were STRUCT. That admits an extra candidate from the table's ARRAY column and
collides with the genuine unnested STRUCT element.

Decision: enforce Java SemanticAnalyzer.lookupNestedField's STRUCT-only step at
both Column.LookupStructField and the relaxed lookup entry. A column with IsArray
or non-RECORD Type cannot be descended; direct access to the ARRAY column remains
valid and the element metadata remains available for UNNEST. No alias-specific
priority, schema recovery, or path rewrite is added. Unit pins cover exact/relaxed
lookup, ARRAY at the root and below STRUCT, direct ARRAY access, and the actual
same-label ARRAY-versus-STRUCT candidates. Retain the real-FDB and JVM reproductions.

### Integration finding: UNNEST names do not have blanket lookup precedence

Live Java rejects `SELECT v FROM t, t.a v, u v` as 42702 when `u` declares
`v`. Go returned the element because `Scope.ResolveColumn` discarded every
non-`Shadowing` match. `SemanticAnalyzer.lookup` instead counts all visible
matching attributes; its only direct/nested precedence removes an ephemeral
whole-STRUCT's duplicate route to the *same field*, not a different table's
same-named field. Changing alias uniqueness alone exposed this older defect.

Decision: remove that blanket preference. Retain the virtual-element marker
only for representing UNNEST's whole-value binding/AT wrapper; it must not
adjudicate ambiguity. Audit the SQL star-publication/physical-boundary helpers
that also drop same-named outer columns: a bare star preserves every visible
attribute (excluding genuinely ephemeral whole-STRUCT and row-version slots),
and a duplicate label becomes an error only when referenced. Java's
`generateCorrelatedFieldAccess` publishes scalar/AT attributes directly; a
record element without AT publishes its fields plus an ephemeral whole-object
alias. Keep physical owner windows exact while publishing those SQL attributes.
This uses the existing semantic resolver, projection, ordinal seed and star
helpers, not a second query path or a runtime name-lookup fallback.

Tests must cross same/different display aliases, source order, bare/qualified
reads, AS/AT, scalar/record elements, SELECT/WHERE/ORDER BY, and derived/CTE star
boundaries. Positive controls explicitly qualify the intended real table field;
unused duplicate aliases remain legal. Retain live Java outcomes, including
its ordered-plan limitation where Go's sanctioned in-memory sort answers.
The alleged multi-segment success `x.arrayOfStruct.field` was an invalid test
expectation: both engines reject 42703; an additional FROM leg is required to
cross that ARRAY. The original rejection shape remains a regression.

#### Qualified-star contract correction (Java source challenge)

`SemanticAnalyzer.expandStar` (lines 320–367) is not name resolution: Case 2
selects the **first** operator whose alias matches, requires its flowed type to
be RECORD, then publishes that operator's non-ephemeral visible attributes.
Thus `scalarAlias.*` without AT is 42F10; scalar+AT flows a record and is valid.
With duplicate display aliases, qualified star selects the first operator in
FROM order, not all matching operators. The earlier design-review requirement
to enumerate every matching operator for qualified star is superseded by this
source contract. Bare star still concatenates all visible attributes in order.
Case 3 resolves a non-operator qualifier as an expression and likewise requires
STRUCT before field-by-field ordinal expansion; ARRAY element metadata cannot
make an ARRAY a STRUCT here either.

Implementation stays in the existing star/resolver/projection machinery:
ScopeSource carries the exact whole-element Column (FlowedObject), while
ColumnOrdinals represents nonparallel SQL/physical slots such as AT without AS.
Star attributes carry already-bound Values into LogicalProject.ProjectedValues,
using the same exact source/ordinal mint as named resolution. Star publication
must not re-resolve duplicate labels. Existing bare-star and qualified-star
expanders consume the semantic scope; no second star classifier or runtime
name lookup is introduced. Ordinary named lookup still counts all matching
attributes, including across duplicated source aliases.

#### Integration finding: implicit SELECT aliases are not Java output names

The default-FROM-alias hypothesis is refuted. Java's
ExpressionVisitor.visitSelectExpressionElement (170–179) applies the parsed uid
ONLY when AS is present. CAST returns an unnamed expression (430–441). Therefore
`SELECT CAST(a AS BIGINT ARRAY) a` does not publish A, regardless of the derived
operator alias; `... AS a` does. The FROM collection lookup correctly fails
42703 before Explode construction. Direct-column expressions may retain their
inherited names even when an implicit trailing uid is ignored.

Decision: read the typed SELECT element's AS token at the shared alias extraction
boundary, matching Java, for direct, computed, aggregate, and COUNT projections.
Do not change FROM aliases (Java accepts those without AS). Preserve the existing
anonymous output-name/physical-slot distinction; no query-text inspection or
alias-collision rejection may simulate this rule. Carry this through both SQL
frontends and derived/CTE schema publication. Retain paired explicit/implicit AS
probes, direct inherited-name controls, computed-array FROM consumers, output
metadata and ORDER BY/WHERE consumers. The same-name default-FROM-alias cells
must use explicit SELECT AS when they intend to test FROM identity; the original
implicit-SELECT-alias failures remain distinct retained regressions.

The authoritative boundary is `selectOutputAlias(*antlrgen.SelectExpressionElementContext) string`:
return normalized Uid only when both AS and Uid are present, otherwise empty.
Its conversion surface in embedded is `extractAggFunc` (SELECT-only overlay,
not `extractAwfFields`' physical accumulator key), `selectExprToColumnName`,
`classifySelectElements`' sole COUNT and computed arms, `collectSelectNames`,
and `PlanVisitor.visitFinalProjection`. GROUP BY, FROM table/subquery/TVF/
UNNEST, inline values and qualifier-star Uids are not SELECT output aliases.
No consumer may recover authoredness from canonical text or a generated label.

Required retained matrix (both frontends and the live JVM/FDB harness):
- `SELECT id x` returns ID=1, not X; `id AS x` returns X=1. ORDER BY ID
  remains legal in the implicit case; ORDER BY X rejects 42703.
- `SELECT CAST(id AS BIGINT) x` returns an anonymous `_0`=1; explicit AS x
  returns X=1. The internal computed slot stays nonempty for execution.
- SUM(id), sole COUNT(*), and mixed COUNT(*)/SUM(id): implicit trailing x/y
  are ignored; explicit AS x/y supplies X/Y. Compare default labels against
  the same Java expression without any trailing Uid, not Go's accumulator key.
- Derived/CTE `q.x` over implicit computed x rejects 42703, explicit AS x
  resolves. The anonymous display label `_0` and canonical `CAST(...)` slot
  are not authored semantic names and must not become addressable through q.
- Same-level WHERE/ORDER BY X over computed implicit x rejects 42703;
  derived/CTE WHERE X succeeds only when the producer explicitly uses AS x.
- Computed ARRAY FROM consumers q.a reject 42703 for implicit SELECT a and
  resolve with explicit AS a, independently of q versus a FROM aliases.
- Table, derived, TVF and UNNEST FROM aliases with/without AS are unchanged;
  pair them with qualified reads and ordinal/row assertions.

Publication must preserve expression-name absence separately from executor
keys: SQL scope labels cannot fall back to canonical computed text or `_N`.
Repair the existing projection/scope publication boundaries if the new pins
expose such a fallback; do not change value ownership or infer presence from
spelling. Metadata naming is the materialization boundary, as in Java
Expressions.getStructType; it must not feed back into SQL identifier lookup.

Implementation naming boundary: aggregate SELECT items retain authored-AS
provenance separately from their accumulator `outName` (generalize the existing
`groupColAliased` fact to `outputAliased`). `aggregateProjectionItem` consumes
that fact, never equality with the canonical call spelling. The public
post-aggregate projection materializes unnamed outputs as `_N`, while a parallel
`LogicalProject.SQLNames` vector preserves absent semantic names as empty slots.
That vector is carried across the deferred post-sort construction in both
frontends and read by ExactLogicalOutputLabels, not ExactLogicalResultType.
The physical type still names the actual emitted row. `aggOutputCols` publishes
only authored aliases or inherited grouping-column names, and ordinary computed
publication likewise never substitutes a canonical executor key for absence.

The explicit canonical-label controls (`AS "SUM(ID)"`, `AS "COUNT(*)"`,
`AS "CAST(ID AS BIGINT)"`) expose a second naming boundary: Java rejects these
with ProtoUtils.InvalidNameException, mapped to 42602 by PlanGenerator (260–261).
Type.Record.Field computes fieldStorageName from the optional authored name;
Go already ports that escaping/validation in protoname.ToProtoBufCompliantName.
Apply that existing routine to authored SELECT output names at the shared
projection upgrade boundary, after expression binding, not inside the alias
extractor or by validating canonical executor keys. Preserve the typed cause.
Both frontends already call upgradeProjectionValues for ordinary and post-
aggregate SELECT slots. Split its binding body into resolveProjectionValues,
then validate projAliases, countStarAlias, and outputAliased aggCols through
one validateSelectOutputNames helper. Explicit `_0`, dotted and dollar-containing
aliases remain legal where ProtoUtils permits them; implicit invalid-looking
trailing Uids are ignored. Pin the original invalid shapes and legal controls.

The retained live controls exposed two further boundary defects. The Java
conformance HTTP handler reports the deepest cause's class/message correctly,
but also reads SQLSTATE only from that deepest cause, dropping PlanGenerator's
42602 wrapper. Preserve the root diagnostics, search the original exception's
cause chain for the first structured SQLException/RelationalException SQLSTATE,
and do not infer codes from class names or messages. The retained invalid-name
queries are end-to-end pins of this actual wrapper path.

For `SELECT q."a.b" FROM (SELECT COUNT(*) AS "a.b" FROM t) q`, the scoped
identifier is exact and rows are correct, but the metadata layer splits its
physical key and reports B. At projection publication, a resolved ordinary
reference already has its inherited SQL name in projCol.bare. Carry that name
as the projection's output alias when no alias/key has already been installed,
for ALL resolved ordinary references, not only dotted names. This matches
Expression.fromColumn's inherited naming and must not change the parser's
SELECT-AS fact: sq.projAliases/outputAliased remain the authored-AS authority.
Logical/physical output aliases represent output names (authored or inherited);
AliasMinted distinguishes internal key mints from these SQL names, not AS syntax.
Preserve an existing machinery alias and its source provenance. Apply once at
the shared upgradeProjectionValues boundary after resolution and name validation;
empty/computed slots must remain anonymous. Pin derived dotted COUNT labels,
ordinary inherited names, explicit renames and duplicate outputs in live tests.

Computed grouping-key output-name correction (integration finding): the
`sqlnames-propagation-audit.log` audit identified that reclassifying an unnamed
SELECT expression as a grouping-key slot overwrites groupColBare with the
canonical group program. `select-as-live-fifth.log` confirms Go wrongly accepts
Q."ID+1" through derived grouping queries, while Java rejects it with 42703.
This occurs with no aggregate, a visible COUNT, ignored implicit SELECT alias,
and a hidden HAVING COUNT. The positive computed-group queries reach Java's
UnableToPlanException: its planner lacks Go's existing in-memory sorting
fallback. They remain separate engine-specific pins, not shared-success claims.
Java Expressions.getStructType/underlyingAsColumns establishes their semantic
name absence independently of whether a physical group plan can be produced.

Decision: preserve an `outputInheritedName` on aggSelectCol at the direct SELECT
reference's classification, alongside the existing authored-AS provenance.
Computed/aggregate outputs leave it empty. Group-key rebasing may change the
execution-side groupCol/groupColBare but never this inherited output identity.
`aggregateOutputSQLName` returns authored outName when outputAliased, otherwise
outputInheritedName. Do not reinterpret groupColBare's canonical key as a name,
or clear it and break correlated/native-group binding. Capture direct-reference
names at the mixed SELECT and ordinary→aggregate demotion constructors; in the
hidden-aggregate harvest constructor only a nil projExpr inherits a name. Keep
explicit GROUP BY aliases and direct grouping names as controls. No changes to
matching, ordering or grouping algorithms. The existing `_N` physical
materialization and SQLNames transport consume the corrected presence fact.

The retained provenance pin found one pre-capture mutation:
`SELECT g FROM t GROUP BY id AS g` rewrote ordinary projCols to ID and inserted
G into projAliases, counterfeiting SELECT AS before aggregate demotion. Move the
existing GROUP BY alias rebase after ordinary/grouped-star output classification,
where every grouped SELECT item has its captured name/provenance. Delete the
ordinary-projection alias-insertion loop; rebase only aggregate execution keys
and ORDER BY as before. The post-aggregate projection applies an inherited output
name when it differs from the execution key, without changing authored-AS facts.
This completes the approved capture-before-rebase invariant rather than adding
another per-slot marker or reconstructing provenance from rendered names.

CTE projection-row publication correction (integration finding): the retained
six-deep DISTINCT/EXISTS/gather aggregate fails at execution with a sort key
rooted on RECORD(AID,K,ARR,BID,K,X) while its child declares
RECORD(AID,K,ARR,BID,K_2,X). The exact runtime guard is correct. The registration
of a simple CTE body copies only semantic.Table columns from the preceding CTE,
dropping its separate flowed row. It also fabricates the wrong whole-row type
for direct repeated references and reordered outputs. `cte-scope-row-red.log`
pins those three source-publication cases independently of the physical plan.

Java LogicalOperator.generateSimpleSelect constructs the result quantifier
from the actual select result, then Expressions.rewireQov assigns each SQL
expression an ordinal read from that result; expression names remain separate.
canAvoidProjectingIndividualFields compares the complete result fields, not
just star syntax. Go's existing exactVirtualScopeSource already implements the
SQL-label/flowed-row distinction. Decision: delete buildCTEColumnSource's
parse/catalog-only simple projection reconstruction and use its existing
buildExactScopeSourceOrBodyError path for ordinary and computed simple bodies,
as for joined and aggregate bodies. Use projectionOutputNames for semantic
names, the built body for the flowed row, and retain body errors rather than
swallowing them. Do not copy the input flowed layout for a reshaping SELECT or
invent another de-duplication rule in the scope builder. Recursive WITH still
derives only the seed branch selected by the existing typed SetQuery arm.

Required pins: direct duplicate projections, chained DISTINCT star publication,
reordered/repeated projected fields, original deep-six real-FDB execution,
semantic 42702 on duplicate SQL names, and 42703 on physical suffix names.
Exact layout checks, physical projection de-duplication and grouping/sort
algorithms remain unchanged. Run embedded/query and the complete existing
UnnestExistsGather integration test, including repeated runs with unique
per-invocation subspaces.

The first exact-body rerun repaired the original runtime error but exposed a
second publication boundary: expandBareStarFromScope elides a star projection
unless the source has an ephemeral/UNNEST attribute. Over a CTE whose semantic
columns are K,K,N but whose flowed fields are K,K_2,N, it leaves a bare scan;
the output-label derivation then reads physical names as SQL names. The retained
chained-star source test and K_2-negative catch this (first focused run remains
RED). Complete Java's canAvoidProjectingIndividualFields invariant at the
existing star-expansion boundary: a non-identity SQL-attribute-to-flowed-row
layout also requires projection (different widths, field names, or attribute
ordinals). Keep the current exact source-column Value construction; emit SQL
labels separately from the projection's deduplicated physical schema. Ordinary
identically named/mapped scan stars remain projection-free. This is not a
physical optimizer change and does not permit physical labels in name lookup.

Anonymous star controls required two further parts of the same row invariant.
The shared inherited-name publisher now materializes a bound-but-unnamed star
attribute as _N while retaining an empty SQLNames entry; projectionOutputNames
must not substitute the bound attribute's rendered source key for that absence.
ExactLogicalResultType previously named a computed CAST field by its executor
expression string while Cascades emitted _0. It now reuses the projection's
existing slot-name derivation for resolved Values. Focused scope/error/gate
pins pass in cte-row-focused-3.log; the live anonymous controls failed before
that last shared-derivation correction (select-as-live-eighth.log). Full core verification was pending at that
checkpoint; the enum integration results below record the subsequent run.

Enum transport exposed by exact publication: the initial complete embedded run
failed TestBuildLogicalPlanWithCatalog_CTESelectStarSchemaDerivation because
Order.flower.color is a stored protobuf enum. The removed catalog copier called
it STRING; exactVirtualScopeSource refused that lossy conversion.
TestCTEScopeCarriesStoredEnum captured the unavailable source in cte-enum-red.log.
This reached the enum-as-STRING defect already booked in TODO.md (now "Exact enum
transport"); the implementation and verification below supersede that failure.

Decision: mirror Java's separate DataType.EnumType representation rather than
passing a planner Type through the semantic model. Add enum name plus ordered
(name, number) members to semantic.Column, analogous to its existing nominal
StructTypeName/StructFields. rlcatalog captures these from the existing stored
field type authority; semanticColumnFromExactType copies them in the reverse
bridge; expr.columnCascadesType rebuilds the exact EnumType and no longer maps
ENUM to STRING. Re-label/nullability/array wrapping preserves the declaration.
Missing or malformed enum declarations fail the existing exact-type admission,
never panic or silently become strings. Number-aliasing protobuf enums retain
the stored mapper's established LONG carrier rather than inventing a second
alias policy. Java reference: DataTypeUtils.toRelationalType/toRecordLayerType
ENUM arms and DataType.EnumType (4.12.11.0). This introduces no new enum operator;
existing typed comparison/promotion support must be exercised, and any failure
is fixed before this finding closes. Keep nested enum vs top-level STRING
homonym pins, replace the old STRING/decline sentinels with exact-enum assertions,
and exercise real-FDB CTE/derived/WHERE/ordering/NULL and metadata behavior.
The separate nullable-element/nested-array carrier limitation remains outside
this enum declaration change; it is not claimed closed.

### Enum transport implementation and integration evidence

The semantic carrier now holds an enum name and its ordered (name, number)
list, independently of planner types. The stored-field mapper is the authority
in rlcatalog, including its LONG treatment of number-aliased protobuf enums.
Member names are decoded with protoname.ToUserIdentifier as Java
Type.Enum.enumValuesFromProto does. The reverse bridge validates the type graph
once before recursive copying; missing declarations, duplicate members/numbers,
typed nils and cycles decline without construction panics. Planner enum equality
and MaximumType retain Java Type.Enum's structural declaration comparison
(nominal names need not agree), while publication preserves the nominal name.

The first real-FDB comparison/IN runs exposed absent string-to-enum promotion.
ResolveComparison and ResolveIn now insert element promotions to the declared
number, including unresolved runtime parameter values. CastValue's already
admitted STRING->ENUM and identity ENUM->ENUM pairs also lacked evaluator arms:
`enum-cast-red.log` records STRING->ENUM silently returning nil. Cast and promote
now share the member lookup and InvalidEnumValueError; invalid names remain
case-sensitive and map to SQLSTATE XX000 with their typed cause preserved,
matching Java ExceptionUtil. The existing Go-only ENUM->STRING decimal-number
cast is unchanged, explicitly pinned rather than mislabeled a name-string
identity. JDBC enum metadata is OTHER, independently of the int64 row carrier.

**Scoped Java IN divergence:** in Java 4.12.11.0, InOpValue injects a promotion
to ARRAY<ENUM>, but PromoteValue.eval (the descriptor selection at lines
264–269 in the reference checkout) selects ENUM or asserts RECORD; ARRAY trips
VerifyException. `EnumTransportReference` retains positive-string-list IN with
XXXXX and invalid-member IN with XX000 as separate Java-only failure contracts;
invalid equality retains SemanticException/XX000. Go's element-wise list
promotion avoids that upstream assertion and is checked against independently
authored row expectations, not described as shared IN conformance. The exact
queries, schema and exception assertions live in
`conformance/numeric_cast_boundary_conformance_test.go`; debug stack evidence
is in `enum-java-reference-debug.log`. No upstream issue has been filed.

Evidence in `/var/tmp/query-grind-cast`:

- `enum-core-full-2.log`: uncached complete embedded, query, expr, rlcatalog and
  values Bazel targets all passed (5/5); hash inventory
  `enum-core-final.md5` verified afterwards. The subsequent root-once validation
  refactor and malformed-graph/UNION controls passed the focused
  `enum-union-guard.log` run; they are not retroactively covered by that full run.
- `enum-deep-cte-repeat.log`: three invocations each of TestFDB_EnumTransport and
  the complete TestFDB_UnnestExistsGather, including the original deep-six
  DISTINCT/groupby case, passed. At that revision, enum execution emitted 18
  row witnesses per invocation (54 total). The temporary whole-plan diagnostic
  dump was removed; the unique per-invocation subspace is retained.
- `enum-union-guard.log`: the later expanded enum fixture passed 20 row witnesses,
  including derived UNION and the unchanged decimal ENUM->STRING extension,
  alongside malformed transported-graph checks. It also checks exact enum
  declarations, OTHER metadata, runtime errors and an actual STATE_IDX scan.
- `enum-numeric-java-final.log`: both requested live-Java specs passed;
  EnumTransportReference emitted its 10 explicit outcomes. The existing numeric,
  name-publication and lateral contracts were rerun, not inferred from old logs.
- `just-test-enum.log`: `just test` completed in 649.645s, 55/92 targets executed
  (37 cached), 83 passed and 9 failed. The full factory corpus passed. This is
  still a RED campaign, not a full verification or a merge-ready checkpoint.
  Remaining sqldriver/corpus/golden/checker failures must be reconciled against
  Java without blanket expectation rewrites. The 1018-line ordinality fixture
  diff was read in full: its preserved original negatives and positive twins
  need stale-comment cleanup, missing aliasless twins and remaining test failures
  addressed. Read-only follow-up triage is in `full-suite-readonly-triage.log`.

No hook, commit, draft PR, push or merge has occurred. Design ACKs are not
implementation ACKs. Final implementation reviews, mutants, same-filesystem
stress runs and CI remain required before merge; the promised normal-hook/draft
PR checkpoint follows resolution of the integration regressions.

### Slot-order checker integration decision

The full-suite docscheck failure is a stale oracle, not permission to weaken
slot ordering: selectItemName assumes an anonymous COUNT/SUM call is publicly
named by its call text. Java Expressions.getStructType and the live probes name
anonymous aggregate outputs by their SELECT ordinal. The current checker also
parses SQL by substring/parenthesis heuristics, violating the typed-tree rule.

Replace only selectOutputNames' SQL derivation with the existing parser's typed
root/statement/query/select-element nodes. Keep its source-AST query/row pairing,
row rendering parser, cross-row consistency check, permutation checks and
non-vacuity floor unchanged. Explicit AS uid determines a name; an unaliased
simple column reference inherits its final identifier; a bare aggregate call
has the physical ordinal label _N. A trailing uid without AS is not an alias.
The bounded checker still declines stars, WITH/set/parenthesized query bodies,
non-SELECT/multiple statements, no-FROM queries and unaliased computations
outside its supported aggregate-call case; this is not a full metadata oracle.
Identifier token decoding may read token text, but SQL structure must never be
inferred from GetText/string matching. Unit pins cover every admission/decline,
quoted identifiers and punctuation inside strings, AS inside CAST/subqueries,
ignored trailing identifiers, aggregate ordinals and a mixed-column permutation.
The existing failed full corpus is the regression witness for the stale call
labels; both the gate tests and corpus run must pass before banking this repair.

Checker design clarifications: “order” means the sequence of NAME tokens in the
committed NAME=value expectations, not a proof that an anonymous value belongs
to a particular expression. Swapping two anonymous NAME=value pairs must fail;
swapping their values while leaving positional labels fixed is outside this
static instrument, exactly as a named-value swap with unchanged names is. A
unit pin must demonstrate both observations explicitly; actual values remain
the real-FDB tests' independent oracle. Preserve the existing global floor and
add a named, distinguishable-column population floor so anonymous positional
slots cannot fill the denominator if every named-order witness disappears.
Carry label provenance from the typed derivation to that guard, not a pattern
match on names (an authored _0 is a genuine name). The guard decision takes
explicit counts and both empty-population arms receive unit tests.

The alias contract is not inferred from grammar acceptance: Java
ExpressionVisitor.visitSelectExpressionElement checks ctx.AS() before using
ctx.uid(). The live NumericCastBoundaryConformance spec pins SELECT id x as ID,
SELECT id AS x as X, and anonymous CAST/COUNT with ignored trailing identifiers.
The typed checker must mirror that proved naming contract, not reinterpret the
optional-AS grammar as granting a semantic alias.

### Typed slot-order integration evidence and correlated-derived finding

The typed docscheck derivation is implemented after both design ACKs
(`design-slot-checker-graefe.log`, `design-slot-checker-torvalds-delta.log`).
It independently derives AS/inherited/anonymous-aggregate labels, retaining
provenance so an authored `_0` is not counted as anonymous. The unchanged global
floor is 100; a separate width-matched named/distinguishable population must be
nonempty. Unit pins distinguish moving NAME=value pairs from swapping values
under fixed labels; only the former is this gate's claim. Both collapse guards,
provenance classes and declined syntax are explicitly exercised.

`slot-order-typed-red.log` observed the old heuristic fail the new typed cases.
`slot-order-typed-2.log` passed the gate and both entire affected FDB fixtures:
14 legacy COUNT(*) expectation labels changed to Java's absolute SELECT ordinal
`_1`, without changing their SQL or values. Five applied, compiled mutations
(implicit AS, aggregate ordinal, global floor, named floor, anonymous-population
substitution) were killed by retained tests; `slot-mutant-*.log`. Original bytes
were restored. `FuzzTypedSelectLabels` passed 1,531,211 executions in 15 seconds
(`slot-order-fuzz-2.log`), **without coverage guidance** as the Bazel binary warns.
The first fuzz attempt did not build because its sandbox-writable cache directory
was absent; it ran zero tests and earns no evidence.

`slot-order-full-integration.log` ran both full targets uncached: docscheck passed,
sqldriver remains red. The gate population at that snapshot was 967 derived rows
at 338 sites, including 458 width-matched named/distinguishable rows, with 19
fallback sites and 140 ambiguously paired rows. Source hashes were unchanged for
that run (`slot-order-final.md5`, with deleted paths recorded separately).
The subsequent ordinality comment audit corrected additional stale shadowing and
retired-wrap claims; scanner token comparison against the pre-comment snapshot
confirmed no non-comment token changes. The earlier full ordinality run passed
228 RUN lines; a final rerun after comment repair remains required.

The correlated-derived diagnostic review **refutes** the earlier read-only triage's
claim that all 42F01 failures are stale fixture expectations. Java supports the
EXISTS body-WHERE query through QueryVisitor.visitSubqueryTableItem and lexical
fragment resolution. Go's fallback still catalog-resolves a derived alias, and
its WHERE path also reconstructs a bare scan. Correlated scalar query syntax is
absent in Java; it is a Go extension with scalar cardinality semantics. Its derived
primary should return (1,10),(2,20); the original unfiltered derived comma leg
produces two inner rows per outer row and must raise 21000, not 42F01/0A000.

This is within the approved carried-source design's correlated consumers:
reuse the existing checked derived semantic source and body carrier for the
primary and each join leg, preserve full source metadata/body errors, mint private
single-catalog/derived runtime identities, and retain left-to-current ON visibility. Do not add a
new schema copier or translate unknown derived aliases to a different error.
The Java reference probes are retained in `DerivedSourceReference`; Go regressions
must retain the exact originals plus empty-body, ON and scalar-cardinality twins.
Implementation/whole-campaign review, full green, normal hook/commit and draft PR
are still open; stress/mutations/final ACKs/CI remain merge gates.

### Correlated-derived lexical identity and clustered pull-up completion

The initial exact-source wiring passes the original scalar/EXISTS queries plus
empty, computed, WITH-reading, ON and 21000 twins (`derived-scope-green-2.log`).
Java's retained `DerivedSourceReference` passed all five outcomes: scalar syntax
42601, and the three EXISTS result sets (`derived-source-java.log`).

The extended alias-reuse dimension in `derived-scope-boundaries.log` is RED:
`derived_inner_alias_shadows_outer_leg` returns two rows, both true, instead of
four rows with true for order_id=1 and false for order_id=2. Its scalar twin
hits the clustered-outer ordinal dispatch decline. These tests are retained.
A single derived inner needs a private binding just as a single catalog inner
does. Its SQL alias remains the semantic qualifier, while the carrier's runtime
name and emitted correlation use the same collision-free mint. Do not rebase an
already-built predicate by the shared SQL alias; that would capture outer refs.

The clustered scalar walker currently declines LogicalCTE outright. Complete
that existing walker, not a second execution path: copy Body and Main, preserve
all wrapper attributes, recurse into both, and mask the body's locally bound
correlations while applying an outer pull-up. A definition's locally bound name
is not an outer reference merely because the outer query spells a leg the same
way. Free body correlations still get collected/rewritten. Unsupported/recursive
body carriers retain the existing exhaustive=false behavior; do not silently
claim them handled. The external alias collectors must treat a CTE definition as
a lexical boundary: collect the carrier/Main's outward bindings, not the hidden
Body's local bindings. This fixes the overinclusive inner-own set that otherwise
makes an ordinary unaliased outer table collide with a hidden body scan.

Unit pins must independently exercise Body free refs, Body locally bound refs,
Main refs, wrapper copy/no mutation, nested CTE scope, recursive/unsupported
declines, and hidden-body versus outward binding collections. Real-FDB pins
cover public alias reuse, the unaliased outer/body-table homonym, disjoint
clustered outer sources, empty/scalar-cardinality outcomes and nested WITH in a
derived body. Build the carrier from the full derived query (including its WITH),
not just QueryExpressionBody. This is a scoped completion of Java's lexical
fragment and fresh-quantifier contract, not a spelling-based tolerance gate.

Lexical-mask clarification after design review: Body and Main require **separate**
binding environments. Body masks its own source binding identities; Main masks
its source identities plus the CTE export identity (Binding, otherwise Name).
Use actual bindings, not display aliases or the old recursive alias union.
Nested CTE traversal pushes these environments. The collector must receive that
same scope context; a fixed callback capturing the surrounding Main's skip set
would incorrectly suppress a Body-free correlation whose spelling equals the
CTE export. Retain a unit pin for precisely that shape, as well as a Main-local
reference colliding with the outer query. Existing non-CTE carrier policies and
aggregate-local rewrite restrictions remain unchanged. A scope-only envelope
exposes Main; a bound derived carrier exposes its outward binding. Private derived
carrier Name/Binding use the same fresh identity; the SQL alias stays in the
semantic qualifier. No post-resolution alias substitution is introduced.

Derived-identity implementation exposes a second boundary (`derived-identity-1.log`):
query/embedded lexical units pass, but same-alias scalar/EXISTS FDB twins remain red.
The scalar descriptor still publishes its SQL alias in `InnerAlias`; use the
fresh inner correlation there as well. The EXISTS flatten creates its existential
quantifier under `esq.Alias` while its extracted predicate still reads the private
derived carrier. Complete the existing `existsInnerCorrelation` rebase for an
explicitly bound, nonrecursive LogicalCTE: it exports ONE binding regardless of
its definition's internal sources. Use `sourceBinding`, not its display alias,
as the rebase source. Leave unbound CTE envelopes and multi-source joins on their
existing conservative path. Only the extracted JoinPredicate changes; never
rewrite Body by an exported name. Java ExpressionVisitor.visitExistsExpressionAtom
uses an existential quantifier over the subquery reference; LogicalOperator's
withNewSharedReferenceAndAlias separates SQL names from the actual QOV identity.
Unit pins must verify the exact exported binding is rebased, outer and definition
references stay unchanged, and recursive/unbound/multi-source boundaries decline.

### Projected-existential continuation must retain its row-producing block

`derived-cluster-projection-red.log` reproduces the remaining 0AF00 with both
star and single-column derived bodies and with a disjoint second outer alias.
Read-only research (`derived-exists-planner-research.log`) identifies the
PartitionSelectRule disconnected-lower pruning, not a further alias fault.
The select has ForEach O, ForEach OTHER, and projected Existential E; its only
predicate connects O to E. The live-existential guard cannot put E below the
projection with a ForEach sibling. The necessary lower {O,OTHER} / upper {E}
is disconnected and the independent-component exception rejects it because
{O,E} straddles the split. No binary alternative survives.

Retain that necessary row-producing block by making connectivity pruning's
scope explicit: it cannot reject the whole ordinary ForEach block underneath a
single projected existential continuation. Admission is a property of the
quantifiers/result, never SQL text or CTE shape: all lowers are ordinary
ForEach (not null-on-empty or strict-single), there are at least two; the sole
upper is existential and referenced by the result; no quantifier in that lower
has a hard correlation dependency within the select. Existing cycle, dependency,
live-existential and exact-row checks continue to run. Other disconnected
lowers retain the existing pruning. This admits the missing canonical binary
construction without admitting the known lateral-component positional-merge
faults or multiplying unrestricted disconnected ordinary-join alternatives.
Java PartitionSelectRule.java (read completely, 347 lines) admits this partition
without any disconnected-lower guard. Its existing Case 1/2/positional-merge
construction remains the mechanism; do not add a translator-only workaround or
a new implementation path. Broad deletion of the Go pruning is not this fix:
its connected-join budget and lateral-component constraints require separate
proof before their whole search class can be admitted.

Pins must drive both result-live lower dimensions (one or both ForEach rows),
exact lower membership, projected versus predicate-only existential, empty/false
and true rows, duplicate-preserving outer cross products, hard dependencies,
null-on-empty and strict-single exclusions, and ordinary connected/disconnected
join controls. The current exact SQL reproducers stay in FDB tests. This remains
an open DFS until these pins, full integration, stress and implementation reviews
are green. The old stress baseline's checkout was on a different filesystem
(`/var/tmp` versus `/home`), so it is not comparable evidence: repeat both samples
at the unchanged merge-base on `/home` before quoting any ratio.

The first canonical-block implementation (`exists-block-green-1.log`) is still
RED: qualification unit arms pass, but rule/SQL witnesses yield no alternative.
There are TWO search preferences rejecting the partition: default-enabled Java
cross-product deferral rejects it before Go's disconnected-lower pruning runs.
The earlier diagnosis of disconnected-lower pruning alone was incomplete. The
same canonical-block property must exempt this one partition from BOTH search
preferences, leaving all semantic/dependency/liveness/exact-row checks intact.
Java can honor deferral by flowing live existential output through the lower;
Go's documented live-existential restrictions exclude those alternatives. A
search preference cannot forbid the only implementable partition. This is a
bounded Go admissibility difference, not a claim that Java bypasses its setting.
The tests must run with deferral both enabled and disabled. Do not alter the
configuration globally or relax the live-existential safety checks.

### Existing scan constraints survive existential access substitution

`exists-block-green-2.log` passes the qualified canonical-partition rule under
both deferral settings and the star/projected planner witnesses. The following
FDB run exposed a separate silent result defect: `derived-star-red.log` returns
true even for the empty star body (`WHERE order_id = 100`), with both single and
clustered outers. The catalog two-equality control and projected derived body
pass. `tryExistsFlatMap` replaces an existing bounded RecordQueryScanPlan with a
new PK comparison via WithScanComparisons, discarding the body constraint. Its
secondary-index arm similarly substitutes an index without retaining PK bounds.
Read-only research is `/var/tmp/query-grind-cast/derived-star-sarg-research.log`.

Limit this existing access-substitution shortcut to comparison-free base scans.
Any nonempty ScanComparisons vector routes through the existing generic
existential residual-filter implementation, preserving the chosen child's
bounds. Do not append same-position equalities as separate key components or
invent runtime range intersection. Java ImplementNestedLoopJoinRule (read in
full) retains its selected inner plans and residualizes join predicates; it has
no opportunistic bound replacement. This does not change unbounded shortcuts.
Pins: existing equality, inequality and composite bound vectors survive; the
PK and secondary-index shortcut candidates cannot erase them; unbounded controls
still optimize; FDB star/projected, empty/nonempty, single/clustered and catalog
controls preserve truth and multiplicity.

### Correlated-derived implementation verification and binding review correction

`derived-identity-green-4.log` passed four focused Bazel targets including the
complete scalar-derived and derived-EXISTS FDB fixtures. `derived-core-full.log`
ran the full Cascades, query and embedded targets successfully; docscheck's sole
failure was the moved `clustered_outer_scalar.go` RFC-238 renderer citation.
Both renderer citations now point to the actual expressions; the weak-cite ledger
and its gate were unchanged. Both affected citation tests passed in
`derived-cite-repair.log`. The core Go-source hash inventory was verified after
that run and after the first mutation batch.

The expanded live Java reference (`derived-identity-java-2.log`) contains ten
observed outcomes: the two existing scalar-syntax 42601 negatives; the three
existing derived-EXISTS outcomes; same-alias, star and empty-star clustered
EXISTS; the exact unordered both-live-outer-row query; and its computed ORDER BY
variant. That last *additional* probe could not plan in Java (0AF00), while the
unordered original returned the expected duplicate-preserving multiset. Both
probes remain tests; no sorted Go query was silently substituted for the original
witness, and no computed-ordering parity is claimed. Go's in-memory sort fallback
is a separate documented read-side extension.

Ten uncached invocations of the two complete FDB fixtures passed with the same
46 RUN lines (two parents and 44 subtests), recorded in
`derived-determinism-{1..10}.log` and their sorted name manifest. The first
invocation passed its tests but the surrounding loop initially expected 31 RUN
lines; the actual 46-name population was inspected before the remaining nine
runs were executed and compared against it. `FuzzSQLPlan` with retained
correlated-derived seeds passed 247,656 executions in 15 seconds, **without
coverage guidance** (`derived-sql-fuzz.log`); this is panic exploration, not an
independent correct-answer oracle.

The milestone implementation review ACKed the scoped CTE walker, exported EXISTS
identity, canonical partition and bounds preservation, but caught an incomplete
leaf conversion: `innerSourceAliases` still preferred display names on bound
scans and unnests. Their actual translators use `Binding`, then alias/table for
scans and `logical.UnnestBindingName` for unnests. The classifier now uses those
same authorities, retaining the conservative/exact distinction from
`outerSubtreeAliases`. Five bound scan/unnest AS/AT unit arms were observed red
before the correction (`inner-binding-red.log`); the new pins and related FDB
fixtures passed (`inner-binding-green.log`). These direct units demonstrate the
latent collector defect; they do not establish that a particular SQL producer
emitted a bound scan or unnest into that classifier before this correction.

The complete repeated mutation batch now has eleven applied, compiled, killed
mutants: Body-local masking, Body-free/export homonym, Main masking, exported
binding versus display alias, both partition search preferences independently,
prior PK/index bounds, scalar export identity, derived local WITH, and the scan
and unnest binding arms. Each source file was restored byte-for-byte and checked
by SHA-256; see `derived-mutant-results.txt` and `derived-mutant-*.log`.
Final delta confirmations and full integration remain required. This bounded
milestone does not close the remaining RFC-256 numeric/type/transport campaign.

### Full-suite follow-through: local WITH lost in correlated scalar fallback

`derived-just-test.log` finished with 8 failing targets (46 executed, 46 cached;
84 reported passing). All 6,158 inventoried source paths were unchanged. The
failed-target logs are preserved under `just-failures/`; this is not a full green.
Both correlated-derived implementation delta reviews ACKed their bounded slice
(`impl-derived-{graefe,torvalds}-delta.log`), not this newly exposed finding.

The canonical outer block also repairs CQ-70's predicate-free three-source EXISTS.
The original one-row query and an amplified 2×2×2 cross product match live Java
(`outer-block-java.log`); its old failure sentinel is replaced with exact original,
mixed-truth multiplicity and empty-inner assertions, green in
`outer-block-old-declines.log`. The adjacent Q54 shadow-scalar sentinel was NOT
safe to replace with only its original zero: a nonzero control revealed wrong
rows. `outer-block-shadow-red.log` retains three controls: V.B=outer LB.K returns
0 instead of 2; unfiltered count and outer-value MAX fail 42703. The original
B=100 zero is nondiscriminating because neither stale V.B={1,3} nor the intended
outer LB.K={5,6} matches 100.

Diagnosis (`research-shadow-body.log`): BuildScalar first builds the full query
but its CTE body has no enclosing semantic scope. On 42703, buildCorrelatedScalar
extracts only QueryExpressionBody, discards the local WITH, and resolves V from
the enclosing registry. wrapWithOuterCTEs then reinstalls the old BID projection,
exactly as the failed control's physical plan shows. This is a real wrong-row
bug, not a fixture-label refresh. Java rejects scalar-subquery syntax (retained
42601 reference); the extension's contract is ordinary lexical CTE scope and
SQL scalar cardinality. Java QueryVisitor.visitQuery/visitCtes/visitNamedQuery
and SemanticAnalyzer.resolveAcrossFragments supply the lexical mechanism.

Chosen correction: the ordinary catalog-aware query build must accept an actual
parent semantic.Scope and retain the full IQueryContext for correlated scalar
queries. Thread that scope through SELECT, local CTE definition publication/body
builds, derived bodies and nested subquery planner creation. Do not merge outer
row sources into the CTE registry. Every SELECT gets a child scope; WITH defines
sources in its own lexical environment, with the prior binding visible to its
definition and the new binding visible to Main. Keep body errors, column alias
arity, recursive restrictions, hidden-column publication and type metadata.

Replace the correlated scalar's separate SQL reconstruction with the same full
query build under its enclosing scope. Successful full builds after an initial
undefined-column speculation are correlated carriers, never prebound independent
scalars. Validate exactly one output field, carry its exact projected type/name,
mint the outward scalar identity and use the existing correlated-scalar lowering
with post-pagination strict-single enforcement. A genuine invalid column remains
an error; no failed full build may retry a query with WITH removed. Existing
cardinality, LIMIT/OFFSET, grouped/non-grouped aggregate, derived-source and
nested-correlation fixtures are mandatory controls. This removes the losing
alternative—teaching the old fallback a second WITH registry—and uses the
existing semantic parent-chain API, not a second planner or SQL text matching.

Design review is required before this correction is implemented. Current exact
Q54 controls stay red; no merge/commit/hook readiness is claimed.

Parent-scope design reviews both returned conditional ACKs
(`design-scalar-scope-{graefe,torvalds}.log`). Implementation is in progress:
BuildScalar now uses VisitQuery under the actual enclosing Scope, preserves the
full local WITH, and uses the translated reference's correlation property rather
than an undefined-column retry to classify the scalar. The body-only builder is
removed. Ordinary WITH definitions are built before publication, with body errors
propagated. This is not yet a passing implementation or an implementation ACK.

The first Q54 run on this path (`scalar-parent-3.log`) reaches the intended
`Project(LB.K)` shadowing body, but fails while assembling the scalar join's row:
`BakedNameContextError` for `_0#0`. The full visitor legitimately projects a
one-column RECORD titled `_0`; `ordinalJoinBuild.bindLeg` treats that title as a
scalar-envelope discriminator and unwraps it. Java QuantifiedObjectValue.eval
uses the result's type (record versus datum), never the field's title. The Go
executor already has explicit `PositionalRow.transportKind`, stamped by
scalarPositionalRowOfType, plus the selected OrdinalLayout's carrier kind.

Chosen correction to this newly reached boundary: make the existing
isBareScalarRow predicate consult the selected layout's carrier kind when
present, otherwise the explicit transport provenance, not the `_0` spelling.
Keep the scalar/record distinctions in the existing producers; do not rename the
scalar projection to dodge the detector, remove the ordinal guard, or invent a
second row tag. Extend the existing equal-shaped record/scalar unit and the
FlatMap leg binding tests to assert genuine `_0` records stay whole on BOTH
sides, including null-valued records, while scalar envelopes still unwrap.
Retain Q54 as the real-FDB witness. The scalar/record binding amendment needs its
design ACKs before production changes; broad scalar/CTE verification is still
outstanding. Temporary diagnostic instrumentation was restored byte-for-byte;
`scalar-parent-trace-3.log` ran but did not hit values.frontierContractGuard:
the live error constructor is the separate join-boundary assertion in
executor/ordinal_join.go, not that values evaluator guard.

The carrier amendment received both design ACKs
(`design-q54-carrier-{graefe,torvalds}.log`). The equal-shaped record/scalar and
both-side non-null/null-field record tests compiled red before the discriminator
fix (`q54-carrier-red.log`). The checked discriminator now uses layout/provenance
and rejects contradictions or malformed scalar slots. The focused real-FDB Q54
suite and binding tests passed (`scalar-parent-4.log`). A subsequent full executor
run passed (`q54-executor-full.log`), including ten explicit discriminator cases.

Parent-scope integration is still RED. `scalar-parent-core.log` ran the complete
query, embedded and executor targets; query passed, while embedded surfaced old
scalar reconstruction/barrier assumptions, unresolved-pagination loss, and the
new early underivable-CTE refusal; the executor failure was the new test's missing
physical tile declaration, since corrected and rerun in the full executor green.
The implementation now preserves underivable CTE tombstones, rejects unresolved
nested pagination, and uses existing ProvenCardinalitiesOf to avoid an unnecessary
strict barrier where the complete query proves at-most-one. These embedded
changes have not yet received a full green.

`scalar-parent-fdb-wide.log` ran the scalar/CTE/derived FDB-name population and is
also RED. Some old decline sentinels now admit full-query operators; they need
independent result/cardinality oracles, not a blind expectation refresh. Concrete
remaining defects include: the fresh scalar carrier ID leaks into a projected
field TITLE, making otherwise identical EXPLAINs differ by process history;
`ScalarDerivedSources/derived_primary_alias_shadows_outer_leg` fails to plan
because the new ordinary derived source path no longer mints the separate source
binding the retired scalar builder supplied; and NOT NULL selected fields lacked
the scalar boundary's nullable lift (`FieldValuedComputedScalar` and
`CorrelatedScalarJoinInner`). The last issue has just been corrected by deriving
the sole full-query field and applying nullable at the scalar boundary, pending
rerun. Other failures remain in that complete log and the earlier full-run
archive. No implementation ACK, full-green, mutation-complete, stress, hook or
commit claim is made for this work-in-progress.

The scalar carrier's EXPLAIN instability is a label/identity conflation: both
scalarSubqueryOrdinalSeed and clusteredOuterOrdinalSeed concatenate the private
inner identity into the final field's display name. The physical inner quantifier
already has its own fresh identity, and replaceScalarSubqueryRef now reads the
materialized scalar by exact ordinal, not this joined label. Chosen correction:
name that final field from the scalar's exact output label alone, on both paths;
retain the fresh quantifier identities and all ordinal/type guards. Do not reset
the global identity generator or regex-normalize EXPLAIN in tests. Audit remaining
name readers before deleting the joined-label contract; any live consumer must
use the already-known scalar slot, not recreate a text lookup. Existing five-plan
EXPLAIN determinism and scalar row/metadata tests are the witnesses. Design ACKs
and bounded implementation evidence for this correction are recorded below.

The scalar-label amendment received both design ACKs and is implemented. Exact
mixed-case/dotted labels, nullable BIGINT metadata and five raw EXPLAIN repeats
passed on single and clustered scalar routes (`scalar-label-metadata.log`). The
complete query target passed uncached (`scalar-label-query-full-2.log`), with its
source inventory unchanged (`scalar-label-query-hashcheck.log`). Full executor
also passed after the non-build outer scalar-NULL binding correction
(`q54-executor-full-2.log`). These are bounded greens, not integration approval.

### Parent-scope derived source identity amendment

Witness: `TestFDB_ScalarDerivedSources/derived_primary_alias_shadows_outer_leg`.
Before this amendment, its inner derived D and outer real D both emitted QOV(D),
despite distinct lexical scopes, causing 0AF00. Expected rows preserve the outer cross product:
1=10 twice, 2=20 twice. Java QueryVisitor.visitSubqueryTableItem builds the complete
query and applies LogicalOperator.withName; that method changes SQL qualification
without changing its distinct quantifier. SemanticAnalyzer.resolveAcrossFragments
walks local before parent. The Go bridge must preserve that same separation.

Carry a primary bindingID in fromSource and selectQuery alongside the authored
alias, matching the existing joinClause binding field. Under an enclosing scope,
assign private derived-source identities BEFORE semantic resolution. Reserve
aliases and actual CorrelationName values from every parent frame, local source
names/bindings, and CTE registry names. Compose with the existing same-level
duplicate mint; never regenerate an identity during a rebuild. Use the existing
collision-checked typed identifier mint, not SQL rewriting or global counter reset.

A shared derived carrier constructor uses the runtime binding for CTE Name,
Binding, and Main's scan key; the body is untouched. Authored aliases remain in
semantic ScopeSource.Alias and SQL metadata; CorrelationName uses the carried
binding. Thread this through primary/join visitor arms, plain/catalog rebuilds,
qualified/hidden star paths, SELECT/WHERE/ON/order resolvers and nested scope
sources. Exact derived schema remains based on the body and SQL publication rules;
reuse the catalog-built body rather than rebuild correlated bodies rootlessly.
No alias-based post-resolution rewrite can distinguish references already conflated.

Pins: bridge copying; parent alias versus private correlation collisions; primary
and joined carrier Name/Binding/Main agreement with untouched Body; resolver values
rooted at the same private binding; qualified/hidden star rebuild preservation;
actual-parent nested resolution; quoted mint-shaped identifiers. Retain all scalar
derived FDB cases (including empty and 21000), correlated-derived EXISTS controls,
local WITH, ON visibility, exact metadata and repeated EXPLAIN. This amendment is
implemented following both conditional design ACKs
(`design-derived-parent-{graefe,torvalds}.log`); it has not received implementation
approval. The mint uses the existing collision-check helper with deterministic
local candidates, not the process-global sequence, so carrier names are stable.
It reserves parent aliases/correlations/additional qualifiers, all three CTE
registries, local bindings and effective aliases (including captured final
segments of default schema-qualified sources). Bodies are built before star
classification and reused through carrier, scope, ON/USING and catalog rebuilds.

`derived-parent-red.log` observed four planning failures: the original primary
shadow plus joined-shadow, qualified-star and USING controls. After correction,
`derived-binding-combined-1.log` passed two structural units and all 17 scalar
source subtests (16 row cases plus the original 21000 cardinality case). Every row
case checks nullable BIGINT metadata and five exact raw EXPLAIN repeats. Added
controls cover a body reading the actual parent, same-level duplicate derived
aliases and quoted mint-shaped aliases. Structural units pin source bridge,
collision namespace, deterministic/idempotent mint, carrier Name/Binding/Main,
Body pointer identity across visitor/plain/catalog rebuilds, exact INTEGER field
ownership and scope parent identity. Earlier intermediate runs caught three
compile/nogo mistakes and a nil-schema unit fixture; none counted as test greens.
A final added default schema-qualified alias reservation is pending its rerun.
The full integration safety net still needs repair; no hook or commit yet.

### Full integration replay and autocommit ambiguity amendment

`derived-parent-just-test.log` completed in 910s: 25 of 92 targets executed,
67 served cached results, 81 total passed and 11 failed. All 6,149 inventoried
tracked/untracked non-ignored files matched `derived-parent-full.md5` afterwards
(`derived-parent-full-hashcheck.log`). Both derived structural units, including
the final implicit-qualified alias reservation, and the full scalar-derived FDB
function passed inside this run. Failed logs are archived individually in
`/var/tmp/query-grind-cast/derived-parent-failures/`. The additional red targets
include embedded's superseded scalar declines, docscheck's deleted-helper debt
and citations, and a factory setup failure. No green integration claim follows.

**Immediate DFS: factory applied-1021 replay.** Full corpus scenario
`fc_0000000745_q6_p0`, `join3_comma__cmp__none.yamsql` line 16695, failed its first
T_RD INSERT with 23505 after a logged connection failure and FDB 1021 retry.
This was NOT a timeout. The existing deterministic
`TestSQLFault_InsertDurablyCommitted_Spurious23505` reproduces the applied branch;
`TestSQLFault_DiscardedCommitUnknownAppliesExactlyOnce` pins the control branch.
Both ran uncached in `factory-1021-existing-pins.log`. No rerun can close this
finding while the replay behavior remains.

The old assertion that this is Java-matching SQL behavior is false for the
pinned Java source. `AbstractEmbeddedStatement.executeInternal` opens a context
through `EmbeddedRelationalConnection.ensureTransactionActive` and
`RecordLayerTransactionManager.createTransaction` (`fdbDb.openContext`, not
`FDBDatabase.run`). DML drains `countUpdates` and closes `RecordLayerResultSet`,
whose close calls `commitInternal` once. `RecordContextTransaction.commit` calls
`FDBRecordContext.commit` once. The generic FDB runner's retryability rule is
correct but the Go SQL application is not: `paginatingRows.fetchPage` routes an
autocommit write page through `runInCapturedTx(nil)` → `FDBDatabase.Run`, replaying
non-idempotent SQL. Java does not take that loop. Independent read-only source
research: `research-factory-1021.log`; this report's fixture-only recommendation
does not waive the now-established Go SQL divergence.

**Chosen correction, design gate required before implementation:**

1. A Cascades DML Execute with no active transaction owns one internal writable
   transaction for the entire statement, all internal pages included. Reuse the
   explicit transaction's store, budget, commit hooks, cancellation and termination
   mechanisms. Drain/count, then commit exactly once before reporting success.
   Roll back on execution error. Never re-execute SQL after an ambiguous commit.
   Existing explicit transactions remain application-owned. Read-only autocommit
   pagination keeps its existing sanctioned cross-transaction behavior. DDL is
   not conflated with Cascades DML by a generic `IsUpdate` check at ExecContext.
   Preserve caller/statement cancellation through commit, and preserve metrics.
   SQLSTATE 40003 remains the already-approved Go error-surface distinction for
   1021; neither 23505 nor an invented successful result is a valid substitute.
2. Factory fixture loading is an application-owned idempotent operation, unlike
   an arbitrary user INSERT. Extend the existing javacorpus runner with an
   explicitly selected reset-and-load setup mode, only for the factory loader's
   validated single schema/setup/test triple and scenario-private schema. Parse
   the schema's table identifiers through typed DDL nodes, not SQL text patterns.
   In one explicit transaction clear all those tables, execute the unchanged
   complete setup, then commit. Only a typed ambiguous COMMIT may replay this
   whole reset/load unit; a genuine error inside setup (including identical
   duplicate INSERTs) fails without replay. Reset plus load makes applied and
   discarded ambiguous commits equivalent. Bound attempts by context and a finite
   cap, retain attempt diagnostics, and never retry a test/assertion block or
   change its oracle. The default vendored-Java setup semantics remain unchanged.
3. Pin applied/discarded 1021 for INSERT and relative UPDATE, explicit-transaction
   ownership, multi-page DML atomic rollback, genuine duplicate 23505, cancel/commit
   failure and affected-count handling. Pin fixture applied/discarded commits,
   genuine duplicate errors, exhausted ambiguity and non-empty retry witnesses
   using the existing SimFDB injection seam, plus the exact FDB factory scenario
   and full factory corpus. Reconcile every stale Java-matching hazard claim in
   tests/docs with the measured source path; do not delete its regression axes.

Automatic idempotency is not a prerequisite and no client/wire change is needed:
that option is correctly rejected as unsupported by the pure-Go client today.
A blind INSERT retry, treating 23505 as success, wholesale corpus reblessing,
or a whole-scenario retry would all conceal a real outcome and are rejected.

### Autocommit/fixture amendment implementation checkpoint (integration open)

Both design reviews returned conditional ACKs (`design-autocommit-graefe.log`,
`design-autocommit-torvalds.log`). Their conditions cover explicit ownership,
commit/cancellation ordering, cumulative transaction limits, hooks/metrics,
private fixture topology and typed commit-only replay. This is not an
implementation ACK.

The implementation now gives autocommit Cascades DML one owned `embeddedTx`,
loads metadata into its transaction binding, drains the result and commits once.
Its pre-commit context-cancellation hook is detached/drained before commit;
commit outcome is not overwritten by a racing caller cancellation. Close aborts
only an unfinished owned transaction, never a borrowed application transaction.
Successful affected-row metrics are published only after commit success. No
client/wire option or retryability classification changed.

The four original required-behavior SimFDB witnesses went red→green
(`autocommit-dml-red.log`, `autocommit-dml-green-2.log`). The broader SQL fault and
page-retry slice passed uncached with 47 RUN lines in
`autocommit-faults-wide-3.log`; the four-file inventory `autocommit-wide.md5`
verified unchanged across that run. The later count-drain cancellation mapping
edit is not covered by that inventory. Pins include genuine duplicate rollback,
affected count, borrowed transaction ownership, applied/discarded ambiguity and
101 separate caller invocations consuming one fault each. The 101-fault test's
first revisions failed because its inspection QueryContext initialized the
catalog through a separate idempotent transaction and consumed the remaining
fault schedule. `autocommit-101-trace-2.log` captures that stack. The initial
store-header-only hypothesis was incomplete: rolling back the inspection alone
did not stop catalog initialization. Initialization is now done before arming
the schedule; the durable test also rolls back inspections. Temporary SimFDB
stack instrumentation was restored byte-for-byte; no simulator change remains.

Factory-only reset/load now lives in the existing javacorpus runner. RunParsed
validates the complete triple before DDL; the private namespace must be newly
created (no pre-existing database/template drop). Typed DDL supplies every table;
closed INSERT VALUES setup is validated structurally. Every load attempt resets
all declared tables and executes unchanged setup in one explicit transaction.
Only the typed 40003+underlying FDB1021 result of Commit may retry, at most three
attempts, with every ambiguity retained in FileResult. The factory caller uses
a fresh namespace token; ordinary vendored-Java Run/RunParsed setup is unchanged.
No assertions or oracle rows have been moved into the retry unit.

`fixture-focused-3.log` passed the new sandboxed validator/classifier/fault and
RunParsed tests after gazelle+mod tidy. These cover both ambiguity branches,
exhaustion, definite conflict, genuine duplicate, cancellation before begin,
quoted/qualified identifiers, foreign/unsupported setup rejection, private
namespace collision refusal, both setup modes, exact asserted-query count and
resetting a declared table absent from setup. A newly written test initially
misnamed COUNT(*)'s anonymous result; it now uses the independent explicit
`AS N` contract instead of matching an observed implicit label. The original
real-FDB failure `fc_0000000745_q6_p0` passed uncached in
`factory745-reset-load.log` (two RUN lines: suite plus that scenario; not the
8,150-scenario population advertised by the parent before filtering).

Still required on this same DFS path: full factory replay; remaining lifecycle,
cancellation-during-execution/commit, hook/metrics and transaction-limit pins;
mutation checks; review of the planning-versus-owned-execution metadata boundary;
reconciliation of superseded autocommit claims throughout docs/source; final
implementation reviews. Then return to the archived 11-target integration red
and the nullable/nested carrier, scalar-cardinality and docscheck failures above.
No hook, commit, PR, push or integration-green claim.

### SQL hunt application retry boundary — amendment to the same 1021 correction

The stale-claim census (`autocommit-claims-census.log`) found executable consumers
of the old policy in `pkg/simfdb/hunt/sqlhunt/{sqlhunt,indexhunt}.go`. These two
workloads deliberately issue only absolute constant UPDATEs and DELETEs against
their own isolated schema under injected 1007/1020/1021; they currently require
SQL itself to retry. Their independent row/index models and fingerprints must
stay intact. The corrected SQL contract must not be weakened to satisfy them.

Chosen design: move that operation retry responsibility into these existing
harness workloads, never into the driver. A bounded, unexported helper is called
only from the four existing absolute-UPDATE/DELETE workload sites, with the
original SQL and bound constants unchanged on every attempt. It retries only a
SQLSTATE 40001 with a typed FDB1007/1020 cause or 40003 with typed FDB1021 cause,
not arbitrary SQLSTATEs, any data error, INSERT, relative UPDATE, or verification
queries. Caller context and a finite cap terminate it; the final error remains
a failure. The helper returns only successful-operation status (not an affected
count, which replay cannot preserve). Each model advances once after the helper
succeeds; state and index verification remain unchanged and independent.

Required pins: applied/discarded ambiguity, definite conflict/too-old, exact
stable bound values through retry, zero retry on genuine domain errors or a
bare/mislabeled SQLSTATE, exhaustion/cancellation and a nonempty replay witness.
Run the existing deterministic SQL/index workload suites with faults enabled;
the seed schedule/fingerprint may change because transaction ownership changed,
but repeated runs must agree and no frozen expected data is re-derived from Go.
This is a design delta awaiting the existing reviewers' ACK, not an implemented
or approved additional retry mechanism. No production caller gains automatic
statement replay.

### Cancellation DFS: backend defects exposed by owned SQL DML

Full factory replay completed: `factory-full-reset-load.log` has 8,150 scenario
RUN lines and 8,150 scenario PASS lines, uncached, 335.7 seconds. All 4,126 Go /
BUILD / yamsql files inventoried under pkg/gen in `fixture-full-source.md5`
stayed unchanged across that run. This is a bounded factory green, not whole
integration. The SQL-hunt design delta subsequently received conditional ACKs
from both reviewers (`design-sqlhunt-retry-{graefe,torvalds}.log`); it is not yet
implemented because cancellation remains the deeper open path.

The new SQL cancellation hook makes Cancel concurrent with the owning read.
`TestCancelConcurrentWithRead` in simfdb was red under the race detector on
`simTxn.cancelled` (`cancel-sim-race-red.log`). Its flag is now atomic; normal
SimFDB data operations remain single-owner. The existing cancellation-entry and
new concurrent-cancel pins passed under `-race` (`cancel-sim-race-green.log`).
C++ ThreadSafeTransaction::cancel dispatches to the network thread; modeling its
observable flag with an unsynchronized Go bool was not safe for this use.

More importantly, the real pure-Go client does not interrupt an ordinary
in-flight read on Transaction.Cancel. New retained
`TestGet_CancelUnblocksHeldReply` in `pkg/fdbgo/client/watch_ctx_fault_test.go`
uses the existing real-FDB simDialer reply gate, pins a read version and holds
the storage reply. With the read RPC timeout raised to one hour, Cancel fails
to return within the 3-second assertion window (`client-cancel-held-red.log`).
The reply is only released by cleanup, so this is not a timing race against a
successful reply. The existing analogous WatchSetup test does unblock: it has
a private watch context canceled by cancelWatches. Ordinary opContext has only
a parent context / optional timeout, not the transaction's cancellation signal.

The C++ 7.3.77 reference is the pinned Bazel extraction at
`/var/tmp/query-hunt-254/bazel/external/foundationdb+` (MODULE.bazel names tag
7.3.77). `ThreadSafeTransaction.cpp:408` dispatches cancellation;
`ReadYourWrites.actor.cpp:2730` resolves resetPromise with 1025, and
`RYWImpl::getReadVersion` at :1537 races that promise. `resetRyow` at :2699
replaces the promise and fails the old incarnation's reads. The same future
boundary applies to ordinary RYW reads, not just watch setup. This supersedes
the earlier blanket statement that no client correction is needed: one-shot
commit itself requires no client change, but its cancellation condition exposed
this separate, now-reproduced client defect. No production client edit yet.

**Chosen client correction, design gate pending:** integrate a cancellation
context/cause with the existing read-incarnation machinery (`readGen`,
`readErrMu`, pendingReads). Every read operation captures its incarnation once
and retains it through GRV, locate/dial, retry, storage wait, range continuation,
metrics reads and deferred PendingGet resolution. Nested internal calls retain
the same capture rather than binding a fresh incarnation. Cancel fails the
current incarnation with typed FDB1025 and does NOT replace it. User Reset swaps
in a new incarnation, then fails the old one before acquiring any lock an old
blocked read may hold (C++ resetRyow swaps resetPromise before failing oldReset).
Only a SUCCESSFUL, nil-returning OnError retry replaces the incarnation;
every failed/aborted OnError returns its governing error AND fails the existing
incarnation with 1025 unless a cause is already terminal, as C++
ReadYourWrites.actor.cpp:1499-1533 requires. Timeout fails the current incarnation
with 1031 and never replaces it. C++ API >=410 does NOT auto-reset after commit
(:1383-1387, :1405-1410): rotation on Go's existing auto-reuse path is that existing
Go divergence, and occurs only after CONFIRMED successful commit and its read
completion barrier, never on commit dispatch. PendingGet retains its
context until terminal resolution, releasing its callback/timer/reply handle
once; the send-phase defer cannot cancel the deferred reply scope. Late old
outcomes cannot poison the next read generation.

Map interruption to the captured cause: Cancel or unsuccessful OnError means
1025; the incarnation's timebomb means 1031. Preserve actual data/wire errors
already obtained and caller context errors; never map all interruption to 1025.
No-timeout defaults remain unbounded. Dispatched commit and commit-unknown barrier
remain detached; do not alter wire encoding, conflict construction or FDB retry
classification. This is a transaction-future lifecycle correction, not a SQL
retry or invented short timeout. Volatile current-state checks alone cannot
interrupt a held RPC and cannot identify an old operation after Reset.

Required proof: held ordinary and pipelined reads, GRV and range waits; reset /
OnError old-vs-new incarnation isolation; no callback/reply-handle leak; unchanged
watch behavior, caller cancellation, 1031 and detached commit; race runs and
C-client differential for the shared observable cancellation/reset contract.
Client engineering/review gates now apply in addition to the SQL milestone gates.
No client production implementation until the design is ACKed.

Implementation refinement for the same signal: timeout is an incarnation-owned
cancellable timer, like C++ timebomb. Updating/clearing SetTimeout retires the
previous timer; an already-fired terminal cause cannot be cleared except by the
appropriate reset boundary. A timer callback must check its captured incarnation
and timer generation so a retired callback cannot fail a newer configuration.
The operation context's Done/cause, not a stale per-operation deadline snapshot,
is the interruption mechanism. Caller deadlines still propagate normally and
transaction timeout zero remains unbounded. Internal tests that specifically
asserted a transaction-added Context.Deadline need behavioral replacements that
prove timeout interrupts held operations, reconfiguration clears/rearms it,
caller deadlines survive, and no-timeout creates no deadline. This is not an
exported Context.Deadline contract; opContext is private. Record cancellation /
timeout cause by captured incarnation, never infer it from mutable timeoutNs or
current transaction state after reset.

Lock and publication protocol: `readErrMu` owns the current incarnation pointer,
readGen/readErr/pendingReads, recorded terminal cause and timer generation. It
is a LEAF client lock: code holding it must not acquire readVersionMu,
conflictMu, ryw.mu, watchMu or PendingGet.mu, wait for I/O, or invoke cancellation /
completion callbacks. Existing Resolve may hold p.mu then briefly take
readErrMu; lifecycle retirement must therefore NEVER resolve a pending read or
acquire p.mu while holding readErrMu. Record terminal cause / detach old timer
under readErrMu; release it before canceling context or stopping/draining any
callback. Timer callbacks validate incarnation and timer generation and RECORD
terminal cause while holding readErrMu, then deliver cancellation after
unlocking. This closes the validation-to-delivery reconfiguration race: a new
SetTimeout sees the already-recorded terminal cause and cannot undo it.

Reset atomically swaps the current incarnation, advances readGen, clears the
old ledger, and records the old terminal cause under readErrMu. Then unlock and
signal old cancellation BEFORE acquiring any readVersion/RYW/watch lock. The
new incarnation cannot inherit old registrations: pending-read registration
compares captured incarnation under that SAME readErrMu. A stale send is NOT
registered into the new ledger; cancel/release its RPC/timer and return the old
captured terminal cause. Completion publication is likewise generation-checked.
Pin reset specifically BETWEEN send and registration, and prove a new commit
does not wait for the old pending operation. Include repeated/concurrent Cancel,
retired timer callbacks, pending cleanup once, and all non-nil OnError classes.

Completion ordering: at entry, an already terminal incarnation precedes ordinary
read key validation, retaining C++'s separate earlier argument-construction
checks (e.g. inverted metrics ranges). A published/memoized outcome never changes.
At RPC resolution, an already queued response has priority over cancellation;
if a cancellation wake wins a blocking select, re-check for a queued response
before reporting interruption. Preserve that response's success or real
protocol/transport error, never overwrite it by current txn state. With no
completed response, a done caller context returns its original error; otherwise
return the captured incarnation's typed cause. Pin both response-ready and
response-held sides, caller versus incarnation simultaneous cancellation, and
timeout versus response. Do not apply this read-wait change to dispatched commit.
The caller census also includes GetAddressesForKey, GetLocations, mapped range,
storage metrics, split points, and versionstamp terminal-cause checks.

### Client cancellation implementation checkpoint (uncommitted)

Design gates are ACKed: `design-client-cancel-cpp-final.log`,
`design-client-cancel-torvalds-delta.log`, and
`design-client-cancel-review-delta.log`. The prior C++/Torvalds/independent NAKs
were folded before production implementation. No implementation ACK yet.

Implemented `read_incarnation.go`: captured cancellation/timeout cause, leaf
readErrMu publication, timer-generation retirement, successful-reset rotation,
terminal unsuccessful OnError, retained PendingGet context and generation-checked
registration. Public regular/snapshot/range/mapped/GRV/auxiliary reads capture
before execution. Read waiters prioritize queued replies; commit dispatch's
waiter is unchanged. SimFDB's Cancel flag is atomic. No wire encoding changed.

Observed bounded greens: original held-Get red→green; 118 RUN lines in
`client-incarnation-focused-2.log`; lifecycle unit arms in
`client-lifecycle-unit-1.log`; real-FDB pipelined/key/range/snapshot held replies,
GRV Cancel/Reset isolation, and reset-between-send-and-registration in
`client-incarnation-fdb-1.log`. New files were Gazelle-registered and run sandboxed.
The real-FDB test author's isolated draft was integrated only after strengthening
its GRV witness: skip cache, hold all replies, observe the held read-version lock,
and bound Reset independently. No test/assertion is retried.

Full client race run `client-full-race-1.log` executed 1,626 RUN entries (including
fuzz seed/subtest entries), with unchanged client-source inventory. It failed
TestSetRetryLimit_Unlimited and TestGetReadVersion_ConcurrentWithCommit_RaceFree;
this is NOT a full green. The retry test directly set private tx.state active
after terminal OnError. Retained C-client differential
TestDifferential_RetryLimitDoesNotReviveTerminalTransaction independently proved
both clients return 1020 at exhaustion, 1025 after merely removing the limit,
and success after explicit Reset (`client-retry-differential-1.log`, also the
existing CancelLifecycle differential). The unit now uses the public Reset
boundary and additionally pins non-revival rather than mutating internal state.

The race test's retirement error is required by Go's existing auto-reuse boundary,
which is already documented as a binding-tester extension in
`pkg/fdbgo/bench/differential_cancel_test.go:25-29`, not C++ >=410 behavior.
Its old "zero is legal" comment also concealed a real result-lifetime defect:
GetReadVersion ensured a GRV, then reread mutable tx.readVersion after Reset.
TestReadIncarnation_ReadVersionResultSurvivesReuse deterministically returned
33,051,373 instead of the captured 33,051,273 in
`client-grv-stable-result-red.log`. C++ RYWImpl::getReadVersion (:1537-1546)
returns the completed future's value. Go now returns the GRV from the common
readVersionForOperation implementation instead of recapturing mutable state.
The race test requires strictly positive successful versions, allows only typed
1025 for a retired incarnation, and starts the writer after a successful read.
`client-grv-retry-green-1.log` passes these pins under race.

Remaining in THIS client DFS: finish read-version acquisition/request-capture
interleaving audit, callback/timer/handle and watch lifetime checks, mutation
proofs, C-client lifecycle coverage, fuzz, full client/race and affected layers,
after-performance comparison, and completed-milestone implementation reviews.
Baseline getter benchmark before client implementation: two samples,
1,201,268 / 1,205,863 ns/op, 3,315 / 3,443 B/op, 32 allocs/op, recorded source
inventory unchanged (`client-cancel-bench-before.{log,md5}`). Both are the
uncommitted pre-client-fix tree above HEAD 8fbba9f701; no after ratio yet.
Then return to SQL DML lifecycle proofs / hunt-owned replay and the archived
integration red. No hooks, commits, PR or push.

### Cancellation DFS amendment — retire readers before reusing mutable state

Further retained interleavings found two more concrete holes, not flaky tests:
`client-grv-acquire-replacement-red.log` returned success for an acquisition paused
before its read-version lock, after Reset installed another version. A recheck
under that lock fixes it (`client-grv-acquire-replacement-green.log`).
`client-replacement-ryw-red.log` remains RED: after a completed GRV, Reset plus a
new Set caused the old Get to return the replacement transaction's uncommitted
value. The captured cause alone does not protect mutable RYW/conflict/options
state. C++ resetRyow installs a fresh promise, arena/cache/write maps/conflicts,
options and reading set before old actors can resume on its single network
thread (ReadYourWrites.actor.cpp:2699-2727). Go cannot mutate the shared state
while an old goroutine is still using it.

**Decision: drain active read execution at incarnation turnover, not a growing
collection of downstream value/error patches.** Keep the existing cause/timer/
registration design and add a per-incarnation execution lease and a replacement
ready barrier. The metadata belongs to the existing leaf readErrMu; no callback
or wait runs under it. A reset serialization mutex orders complete Reset /
successful OnError reset / Go's existing confirmed-commit reuse transition.

1. Every public operation accessing reset-owned state acquires one active
   execution lease under readErrMu, atomically validating readiness/identity,
   and releases it after its LAST such access. This includes reads, buffered
   mutations, conflict methods, option setters, read-version/size/committed-
   version getters, watch setup and Commit's pre-dispatch phase. Nested calls
   borrow the token or call private unleased implementations, never re-enter
   public gates while holding a token. Constants and pure handle construction,
   lifecycle methods themselves, and immutable detached commit work are the
   explicitly audited exemptions. Cancellation/error precedence remains in
   each API's existing validator; the data lease itself does not reject an
   option change merely because that incarnation already has a terminal cause.
   Terminal old contexts do not acquire a replacement lease. A new operation
   arriving during turnover waits on the replacement's ready barrier (or its
   caller/captured cancellation), not on partially reset transaction fields.
2. Reset swaps/marks the old incarnation under readErrMu, signals old contexts
   outside it, waits for its active execution leases outside ALL transaction /
   RYW / pending / watch locks, then resets mutable state. Finally it configures
   the replacement timer from the completed state and publishes ready. A new
   incarnation must not arm a timebomb from the previous state's timeout while
   turnover is in progress. Mutations use the SAME identity-checked lease, not
   a readiness check followed by an unprotected mutation (which is a TOCTOU).
3. GetPipelined holds a lease only for its send phase, NOT until someone calls
   Resolve. It retains its context/handle separately. Resolve acquires an old
   execution lease if that incarnation is still active. When it is retired,
   Resolve may consume an already-ready response using captured immutable
   request data and the generation-checked completion ledger, but cannot retry
   or inspect/mutate replacement RYW/conflict state. If no response exists it
   returns the captured cause (caller precedence retained). Cancellation must
   release each timer/reply/context ownership exactly once, including pending
   futures whose caller never resolves them.
4. OnError captures the incarnation it is handling. Its unsuccessful defer
   terminalizes THAT incarnation, never a newly installed one. It releases its
   execution lease before successful reset and requests reset only if its
   captured generation is still current; an intervening explicit Reset already
   retired that retry. Backoff/caller error stays the governing return value.
5. Commit holds an execution lease through its existing reading barrier and
   construction/transfer of a fully immutable dispatch package: mutations,
   conflicts, read version, options, tenant data, watch activation handles,
   generation and publication token. Release before the detached wire wait;
   no changed RPC/retry/1021 decision. Return the actual outcome as locals.
   Every completion-side state write is generation-checked, not only auto-reset:
   state, committedVersion, hasCommitted, txnBatchId, commitEpoch, watch and
   versionstamp completion, and transaction span/latency anchors. Successful
   completion publishes as part of serialized turnover, after draining state
   users. Database-global metrics may record the captured old operation without
   touching replacement transaction metadata. A stale completion returns its
   own actual outcome but publishes nothing into the replacement handle.
6. Cooperative draining requires cancellation through the SEND path too. Existing
   transport SendFrame/SendFrameDeferred/Flush wait only on the connection context.
   Add context-aware variants for READ enqueue/flush waits while preserving the
   existing detached commit call sites. The new variants copy request bytes
   into queue-owned storage and report whether enqueue transferred ownership.
   On caller cancellation, do not close/penalize the shared connection. Return
   cancellation without inventing a delivery assertion. A queued read reply
   still wins; unsuccessful deferred-send registration still cleans up. The
   completion/queue-ownership algorithm below handles abandoned writes.

Rejected: adding only an after-GRV guard (the same hole reappears inside a RYW
cache lock or after a reply), and scattering generation tests around mutable
caches (misses borrowed byteBuf/atomic operands and conflict publication). A
full replacement of the public Transaction representation is unnecessary here:
turnover serialization gives Go the isolation of C++'s non-interleaved reset
without duplicating every transaction option or changing the wire protocol.

Proof additions: keep the GRV/result/cache reproducers, but use an actual
concurrent Reset in phase seams (wait for old retirement, release old execution,
then assert Reset finishes before new use), rather than recursively invoking
Reset on the same active read stack. Require both pending-never-resolved and
queued-response retirement arms; send/flush backpressure cancellation over real
FDB connections; stale OnError and dispatched-commit completion with new writes;
new read/write blocked until ready; exactly-once leases/handles/timers; caller /
incarnation cause precedence; full race + differential + before/after cost.
This amendment needs design ACK before implementation. Current RYW regression
is deliberately retained RED, not waived or hidden behind a test expectation.

#### Drain protocol closure (folds the three design NAKs)

The current-method census is
`rg -n '^func \(tx \*Transaction\) [A-Z]' pkg/fdbgo/client --glob '*.go'`,
69 matching declaration lines at this uncommitted checkpoint, archived as
`client-public-transaction-census.txt`. Gate coverage includes their nested
call edges and Snapshot entry points, not just the declarations.

**Turnover ordering:** resetMu → brief readErrMu to validate expected identity,
publish the replacement-not-ready and retire/detach the old incarnation/set →
unlock readErrMu → signal cancellation and claim pending cleanup → wait active
leases with NO read-version/conflict/RYW/watch/PendingGet lock held → finish
pending cleanup → reset fields / configure the replacement timer → brief
readErrMu ready publication → unlock resetMu. No operation takes resetMu while
holding a state/PendingGet lock. Internal successful OnError/commit hand their
released execution token to turnover; a non-released token is a checked internal
error, not a WaitGroup self-wait. Public Reset holds no token. Tests must not
recursively invoke public Reset on a read's own instrumented stack: they run
Reset concurrently, observe old retirement, let execution return, then join it.

**Pending ownership algorithm:** the published future has `issued`, `resolving`,
`completed` states, serialized under p.mu. Resolve first tries to acquire a
lease for its CAPTURED incarnation (without p.mu, and without waiting for a
replacement). It then acquires p.mu: completed returns memo; otherwise this
caller owns response consumption and ALL cleanup. A valid lease permits the
existing full resolver; absent a lease only the retirement finalizer is legal.
Both paths memoize once, stop/return the timer, retire/release the reply handle,
release the retained context and remove the matching-generation ledger entry.
Release p.mu before dropping an acquired execution lease.

Retirement detaches the pending set under readErrMu. Outside it, after cancelling
contexts, TryLock(p.mu) claims each unresolved future for the non-I/O retirement
finalizer. If it fails, the holder already owns resolution/finalization and all
cleanup; retirement never waits on that lock while cancelling. Reset then drains
execution leases and calls the same finalizer under p.mu as a completion join.
Thus never-resolved futures have an owner; an active Resolve cannot be deadlocked
by reset; late Resolve only sees a memo; no pending field is mutated under
readErrMu and no lifecycle lock is taken from a held p.mu except the leaf ledger
removal. Timer failure / Cancel use the same detached-set TryLock cleanup even
when no replacement is created.

**Reply linearization:** add a single-owner ReplyHandle take-ready-or-cancel
operation, used only under pending ownership. Under connection pendingMu it
consumes an already-published response, or deletes the still-pending token and
claims cancellation. Move readLoop's buffered response publication into that
same pendingMu critical section with deletion; failAllPending already publishes
there. The buffer is size one with one producer. No delete-before-delivery gap
remains, no waiting for wire I/O occurs under that lock, and the winner is copied
to PendingGet-owned memo before ready publication. A successful or terminal-error
reply remains immutable; a retriable intermediate response from a retired
operation cannot re-drive into replacement state. Cancel/Release cannot consume
or pool the channel a second time.

**Read-write queue ownership:** new context-aware variants copy body bytes before
enqueue (legacy detached SendFrame behavior stays unchanged). Before enqueue,
the caller owns everything and cancellation releases it. Successful enqueue
transfers the immutable copy to writeLoop. Synchronous read-send/flush completion
uses a pooled completion with two ownership references (caller + queued writer):
caller consumes the buffered acknowledgment or abandons its reference; writeLoop
publishes acknowledgment then releases its reference. Only the last reference
drains/resets/pools the completion channel/object. Deferred sends transfer body
ownership without a waiting caller. Teardown/recovered writer failure completes
all owned requests in its current batch and drains queued owned requests, so
no waiter or completion is stranded even when a request is never written.

No cleanup goroutine per abandoned write is needed, and no caller-owned encoded
buffer remains referenced after a new context-aware send returns. Connection
health handling distinguishes caller/incarnation cancellation from actual I/O
failure. Existing commit send/wait/delivery classification is untouched. Pins
must force both enqueue sides, canceled-after-enqueue buffer reuse, eventual ack,
queued-never-written teardown, delivery/cancel lock winner, unresolved-future
retirement, nested-token self-wait guard and gate admission across successive
turnovers—not only a successful rerun of the original Get reproducer.

### Nested-WITH star alias publication follow-through

A full yamsql replay exposed C2(A,B) publishing the inner C1(X,Y) physical row.
`translateCTE`'s extractOutputColumns follows a nested WITH's Main but stops at
its scan; starBodyColumns stops at the WITH itself. Neither sees the actual
known row, so the rename disappears. Java 4.12.11.0 QueryVisitor.visitNamedQuery
applies Expression.withName to logicalOperator.getOutput().expanded() after
visitQuery, and SemanticAnalyzer.validateCteColumnAliases compares that same
expanded width. Go's existing ExactLogicalResultType already follows Body/Main
lexical bindings. Refine an unmodeled structural width with that exact record,
then use the existing InputOrdinals projection and exact arity check. No guessed
type, result-set tolerance or new naming mechanism is involved.

Local design challenge (no subagent tooling): the refinement must use the whole
body query's Main, not the nested definition's Body; it must preserve exact types
and reject both short and long alias lists. Keep the existing fallback only when
exact derivation is unavailable; a failed derivation earns no exact-width claim.
The existing cross-product replay plus translator rename/arity and narrowed-Main
controls are the executable contract. Review of the entire milestone remains open.

### Integration label-scope correction

The current replay/evidence ledger is TODO.md's "integration replay and label/type
separation" block. The label derivation's CTE fallback manufactured an UNKNOWN
RecordType solely to transport labels across a still-unpromoted UNION. It never
became an executable type, but conflates the two authorities and trips the mint
ratchet. Replace that synthetic row with a private label-binding map alongside
real exact CTE rows; do not extend the ratchet allowance. Preserve first-branch
UNION labels, duplicate names, explicit alias arity and lexical shadowing. The
exact row derivation remains independent and must still fail until promotion has
settled. This follows Java Expression.withName/QueryVisitor.visitNamedQuery's
separation of names and values. Local design challenge accepts this bounded
representation correction; full milestone implementation review remains pending.

### Final integration delta: application retry classification

The one-shot SQL repair and clean DISTINCT setup boundary passed the full
sqldriver/embedded/sqlhunt suites (`dml-lifecycle-three-full.log`, 8,696 RUN entries,
no skips). Restoring generic SQL replay fails the retained four ambiguity cases.
The SQL-hunt amendment above is now implemented at its four workload sites.
Local review caught and corrected an overbroad first classifier: matching 40001 or
40003 alone did not meet the design. The final helper accepts only 40001 with a
typed FDB1007/1020 cause or 40003 with typed FDB1021, and refuses joined errors.
The API's driver-budget marker without FDB cause is not eligible. One initial
attempt plus at most 100 retries remains bounded, caller cancellation terminates,
and exhaustion remains a finding. Models advance only after helper success.

The strict classifier has pre-fix red evidence (`sqlhunt-typed-policy-red.log`).
Row and secondary-index workloads each pin conflict/too-old/unknown-applied/
unknown-discarded through direct one-shot controls and application-owned bound
UPDATE/DELETE calls. Disabling retry fails all eight subcases; source restoration
passes SHA256. Seed determinism tests require a nonzero fault population, not an
impossible guarantee that every per-site activation seed fires. Full sqlhunt passes
in `sqlhunt-typed-full-green-2.log`; final integration and review remain open.

### Physical-plan integration audit

Compared `plan-shape-before-integration.golden` (2,967 entries) to
`plan-shape-integration-new.golden` (2,969) in the campaign artifact directory.
The complete 657-line `explain-diff-integration.txt` has now been read locally;
the earlier unsupported claim of this audit was withdrawn in TODO.md.
The report's six STOPPED-PLANNING entries are intentional error corrections:

| Entry | Contract and independent evidence |
|---|---|
| `aggregate_order_by_java.yaml#11` | Authored protobuf-invalid aggregate alias: Java 42602, pinned in ProjectionLabelIntegrationReference. |
| `derived_star_visibility.yaml#0` | Derived star exposes both stored X and unnested X: Java 42702. |
| `derived_star_visibility.yaml#1` | The same ambiguity through a CTE: Java 42702. |
| `derived_star_visibility.yaml#2` | Adding WHERE does not resolve that ambiguity: Java 42702. |
| `nested_derived_table.yaml#5` | Physical COUNT(*) is not a SQL-visible name: Java 42703, retained semantic-name conformance pins. |
| `quoted_identifier_aggregate_labels.yaml#13` | Authored Z' fails protobuf-name validation: Java 42602. |

The two recovered `cte.yaml#30,#35` shapes publish the derived alias's own row,
including repeated ID; ordered WITH is a Go extension (Java 0A000), not a shared
row-equality claim. The unordered control supplies affirmative Java evidence.
The four unpinned errors are exactly `information_schema.yaml#0..3`, byte-identical
before/after. `explaindiff_test.go` already requires precisely this set: production
routes these Go-extension queries through the catalog-backed system-table handler,
not the offline physical-plan harness. No allowance or expected rows changed.

Ten mismatched positions are the insertion of quoted/bare `_0` negative controls
at nested_derived_table #6/#7. Matching that file by exact SQL preserves all 16 old
queries; 11 plans are unchanged, four remove a redundant name-only projection,
and the COUNT(*) reference becomes the justified 42703 above. No old SQL vanished.
Other shape changes remove name-only CTE/derived/UNION projections, introduce
positional publication for duplicate field names, or align recursive branches to
the common row. The nested-WITH cross product additionally changes join child
order while preserving the six-row W,Z,A,B contract. Scalar and projected-EXISTS
changes rename internal keys without changing access paths/slot ordinals; qualified
star errors change 42F01 to Java's 42703. Recursive DFS/BFS retain traversal operators
and receive a common-row projection before consumption. These are characterization
baselines, not performance measurements or independent row oracles.

The eight changed SQL-hunt golden files were also read: aggregate metadata becomes
ordinal labels, scalar keys stop leaking qualifiers, CTE name-only projections
merge, and repeated-name star rows gain positional publication. Row values and
multiplicities are unchanged. The determinism test's XPROCPLAN stdout is retained:
it is the parent/child protocol, not an accidental debug print.

### Final local review and release measurements

This entry supersedes the earlier open integration/review/performance entries;
it does not close the broader QSC-04/07 campaigns. Local author/challenger review
was used during final integration because no subagent tools were available.
The completed Cascades/code-quality review covers the diffs inventoried in
`/var/tmp/query-grind-cast/{final-query,logical-predicate-additions,logical-predicate-removals,remaining-query-integration,select-parser-final,remaining-lowering-final}-review.diff`,
the separately reviewed executor/rules/semantic/value changes, and the physical
plan adjudication above. No remaining architectural or code-quality finding in
this bounded implementation. This is a local review, not an independent PR LGTM.
The earlier C++/Torvalds implementation ACKs in
`impl-client-drain-{review,torvalds}-delta-2.log` remain applicable to the unchanged
client sources; the fresh two-run client/transport race passed with 3,442 RUN
lines and zero skips. The source manifest still verifies after integration.

The first final full suite exposed a real inherited-name regression, not a stale
fixture: four original QuotedIdentifierCaseJavaProbe arms reported reference
spelling instead of declared spelling. `publishInheritedProjectionNames` only
consulted prebound star attributes; ordinary references were already resolved in
`ProjectedValues`. It now inherits unqualified field names from that resolved
slot. Java Expression.fromColumn:326-331 and SemanticAnalyzer.lookup:464-480
justify attribute inheritance; Java's 42703 on folded spellings is NOT a row
oracle for Go's existing relaxed-lookup extension.

The initial repair also exposed a distinct boundary: a scalar UNNEST QOV's
correlation is not its SQL name. The final rule inherits field names, but preserves
the structural SQL leaf for ordinary scalar-QOV references. Existing real-FDB
quoted-lowercase and duplicate-owner-alias tests caught V/Q$DUP1 leakage; both
remain unchanged and pass. New unit pins separately fail for the field-name loss
(four cases) and QOV-name leakage (two cases), then pass after repair. Final
focused runs: 11 unit RUN lines, 228 array-ordinality RUN lines, and all 40 queries
in quoted_identifier_labels. The original eight-case Java probe passed without
expectation changes. Local design/implementation delta review accepts this
attribute-versus-runtime-binding distinction. No new naming fallback or SQL replay.

#### Stress test 1M baseline — release comparison

Baseline: `ed3504f7e410d8e2a4f4c46fd7b4c72fd0484869`, the merge-base at this
measurement. Branch: working tree above `8fbba9f701bee185c9995f3922ecd0d44ce1cd77`;
source inventory `/var/tmp/query-grind-cast/release-source.sha256`, inventory digest
`8632c865ca03f370ee12aae63c6e9489461256909ad8b4f71373a3621355fe15`.
Both checkouts used the same Go module/SDK and /home filesystem (93% full).
Two fresh baseline runs preceded two branch runs, never concurrent with each other
or the full suite. One-minute load at starts: base 1.79/2.40, branch 2.40/2.09.
Command on each checkout (baseline used its own Bazel output base):

```
bazelisk test //pkg/relational/sqldriver/stress:stress_test \
  --nocache_test_results --test_output=all \
  --test_arg='--test.run=^TestFDB_Stress_1M$' --test_arg=--test.v
```

All four runs passed; each executed 24 RUN lines (root plus 23 query cases).
All 22 logged result-row counts agree across runs; the remaining full COUNT case
separately asserted exactly 1,000,000. Logs: `release-stress-{baseline,current}-{1,2}.log`.
The table is the arithmetic mean of two observations per side, not a latency SLA:

| Query | Rows | Base mean ms | Branch mean ms | Branch/base |
|---|---:|---:|---:|---:|
| PK lookup id=0 | 1 | 16.001 | 12.721 | 0.795x |
| PK lookup id=N/2 | 1 | 24.997 | 15.542 | 0.622x |
| PK lookup id=N-1 | 1 | 20.054 | 22.943 | 1.144x |
| idx_customer eq | 8 | 19.521 | 12.463 | 0.638x |
| idx_amount range >9000 | 100017 | 306.085 | 233.072 | 0.761x |
| idx_status count pending | 1 | 322.010 | 374.826 | 1.164x |
| full scan filter amount>5000 | 1 | 648.458 | 727.569 | 1.122x |
| GROUP BY status | 4 | 10.003 | 23.796 | 2.379x |
| GROUP BY status COUNT only | 4 | 15.484 | 12.939 | 0.836x |
| SUM by status (aggregate index) | 4 | 12.453 | 18.079 | 1.452x |
| GROUP BY customer HAVING | 47271 | 769.137 | 624.757 | 0.812x |
| JOIN 10 orders x customers | 10 | 21.140 | 20.784 | 0.983x |
| ORDER BY PK (full) | 1000000 | 3781.578 | 3919.444 | 1.036x |
| ORDER BY PK + index filter | 8 | 8.813 | 9.170 | 1.040x |
| scan all rows ordered | 1000000 | 3635.056 | 3784.116 | 1.041x |
| scan all rows wide | 1000000 | 3881.793 | 3989.700 | 1.028x |
| IN-list 5 values | 46 | 19.041 | 21.472 | 1.128x |
| PK needle id=999999 | 1 | 5.854 | 5.878 | 1.004x |
| PK+filter needle id=500000 | 1 | 7.429 | 7.923 | 1.067x |
| full scan sparse filter | 97 | 3276.149 | 3378.110 | 1.031x |
| UPDATE by index | 8 | 8.917 | 9.924 | 1.113x |
| DELETE single row | 1 | 6.462 | 7.202 | 1.115x |

Small-query timing is not asserted as parity: GROUP BY status was 5.87/14.14 ms
on base and 24.55/23.04 ms on branch despite byte-identical physical EXPLAINs.
The existing `BenchmarkPlanStressShape_` suite was therefore run separately on
both trees (`--test.run=^$ --test.bench=^BenchmarkPlanStressShape_`, benchtime 1s,
count 3, benchmem; 18 samples per side). GROUP BY planning was 1.705/1.669/1.686 ms
versus 1.708/1.659/1.683 ms; SUM planning 1.587/1.594/1.601 versus
1.640/1.574/1.596 ms. Those measurements rule out the observed multi-millisecond
increase being planner work; they do not claim all end-to-end timings are equal.
Full ordered scans were about 3–4% slower in this four-run sample. No timing bound
or plan expectation was loosened. Logs: `release-planner-bench-{baseline,current}.log`.

The existing client getter benchmark also ran twice after the repair:
1,212,556/1,208,546 ns/op, 5,635/5,674 B/op, 67 allocs/op versus the recorded
pre-client-fix 1,201,268/1,205,863 ns/op, 3,315/3,443 B/op, 32 allocs/op.
Mean latency is 1.006x, but allocation cost increased materially (+35 allocs/op,
about +2.3 KB/op). Incarnation cancellation contexts/callbacks and leased operation
ownership are now live on this path; this is a correctness-cost tradeoff, not an
allocation-neutral or faster-client claim. These are the existing real-FDB getter
benchmark's scope, not a broad throughput claim. The two before samples used the
pre-client-fix source inventory already recorded above, not the merge-base.

Generation, Gazelle, module tidy, formatting and diff whitespace checks passed.
Generation preserved the release source inventory. The owner's three skill
front-matter edits are included with permission and their original bytes retained.
One incidental whitespace-only hunk was restored; the SQL-hunt retry comment was
reflowed without changing the typed retry policy. Final full-suite/hook outcome
is recorded in the next entry once completed; no commit or push is claimed here.

Final release gate completed: the installed pre-commit hook passed generation,
lint, build and `just test` (92/92 targets green; 22 executed, 70 reused from the
Bazel cache). The complete staged source inventory remained unchanged across the
run (`release-hook-result.txt`: HOOK_EXIT=0, HASH_EXIT=0). No hook was bypassed.
The normal commit runs that hook again; PR and final-head CI remain merge gates.

### PR review: replacement timeout publication

PR #785 was reviewed at `ed1f41359ec7209d2781fe770e6abadc99748cc8`
against `ed3504f7e410d8e2a4f4c46fd7b4c72fd0484869` by separate
`gpt-6-astra` / `xhigh` virtual review sessions. The independent and code-quality
reviews completed the full 328-file diff; the client-maintainer review completed
its entire client/wire scope. All returned NAK. The architectural review reported
INCOMPLETE and is continuing; none of these verdicts is an approval. All seven CI
checks passed on that reviewed SHA, not on the subsequent finding repairs.

The first reproduced finding concerns reads that capture a replacement incarnation
while turnover's publication barrier is still closed. Construction deliberately
suppresses its timeout until reset-owned options/deadlines are final, but publication
previously armed only an incarnation that had not already been constructed. A read
could therefore enter the replacement and wait on GRV indefinitely past TIMEOUT.
C++ 7.3.77 `ReadYourWrites.actor.cpp:2699-2727` reapplies persistent timeout options
on `resetRyow`; `resetTimeout` at 1576-1579 attaches the timebomb to the replacement
resetPromise. Publication now explicitly arms that same replacement after finalizing
its options, regardless of whether it was captured early.

Retained regressions use the actual `opContext` early-capture path for retry and
user reset, and a real-FDB GRV with its response held behind the existing transport
interceptor. The real-FDB test expires the actual registered timer only after the
GRV is held, so response scheduling cannot beat the injected timeout. It checks
1031 and an independent successful read after releasing the reply. The two unit
cases and held-GRV case fail without the publication fix, then pass with it.
An initial test-constructor nil-database panic was corrected and is not counted
as semantic red evidence.

Evidence in `/var/tmp/query-grind-cast/pr785-review/`: `early-timeout-fdb-red.log`,
`early-timeout-green.log`, and a literal revert/restoration in
`early-timeout-revert-red.log` (all three cases detect the absent timer; restoration
SHA256 checked). `early-timeout-race-10.log` passed ten repetitions under `-race`,
150 RUN lines including the neighboring execution-lease tests. Full `just test`
passed 92/92 targets (43 executed, 49 cached) in `early-timeout-just-test.log`;
`early-timeout-tested.sha256` remained unchanged. Gazelle and module tidy passed.

Other review findings remain unclosed: terminal PendingGet registration cleanup,
deferred-error/timeout precedence, cancellation at retry retirement, CTE-carried
scalar correlation, recursive derived-source rebuilding, and UNNEST regression
assertions that bypass lowering. Their exact reports and reproduction state are
in `pr785-review/findings.md`. Final implementation delta confirmations, the final
PR reviewer and final-head CI remain mandatory before merge.

### PR review: terminal pipelined registration owns cleanup

The architecture review continuation completed all 47,295 lines / 328 files of
`ed1f41359ec7` and returned NAK; it is no longer an incomplete review. It additionally
identified lost enclosing CTE bodies during derived-source rebuilding. These remain
reports to reproduce, not evidence that an implementation has been approved.

The next reproduced client finding is Cancel/TIMEOUT between a successful pipelined
send and its registration. Cancellation detaches the current registry, but the late
send previously checked only incarnation identity and then registered itself into
the already-terminal transaction. Without Resolve, its timer, reply handle, retained
context and pending-map entry survived. C++ `ReadYourWrites.actor.cpp:391-402` races
each read actor against resetPromise independently of whether its caller consumes
the future. Registration now checks terminal cause atomically with identity, then
uses the existing PendingGet retirement/cleanup path outside the leaf mutex.

The already-published reply remains immutable: retirement returns its completed
value directly rather than replacing it with a late cancellation. Six real-FDB
cases cross Cancel/TIMEOUT/Reset with held/published responses. They never call
Resolve and assert terminal result, empty registry, completed memo, and nil timer,
reply handle and retained cancel function. The existing Reset regression now holds
its response explicitly: its 1025 assertion applies to an incomplete read, not a
race against a successful already-published response. New ready-Reset coverage pins
the complementary value-preservation outcome without weakening that assertion.

The initial Cancel/TIMEOUT cases were red (`pending-registration-red.log`). The
first ready-response test attempt incorrectly assumed the test helper encoded nil
as zero; the assertion now checks nil explicitly. This was a harness error, not
an engine mismatch. Final focused green: `pending-registration-green-2.log`.
Removing the terminal-cause guard kills all four Cancel/TIMEOUT resource cases;
replacing retirement with unconditional cancellation kills all three ready-value
cases. Both mutations compiled and restoration passed explicit SHA256 checks:
`pending-registration-{cause,ready}-mutant.log` and
`pending-registration-tested.sha256`. Ten race repetitions passed 80 RUN lines
(`pending-registration-race-10.log`). Full `just test` passed 92/92 targets
(43 executed, 49 cached), source hashes unchanged; Gazelle/module tidy passed.

Focused replay (equivalent to the grouped filter used for the ten-run race test):

```sh
bazelisk test //pkg/fdbgo/client:client_test --@rules_go//go/config:race \
  --test_arg='--test.run=^TestReadIncarnation_TerminationBeforePipelinedRegistrationRetiresResources$|^TestReadIncarnation_ResetBetweenPipelinedSendAndRegistration$' \
  --test_arg=--test.v --test_arg=--test.count=10 --nocache_test_results --test_output=all
```

This closes the pending-registration reproduction locally, not the PR's remaining
review findings. Next is deferred-error precedence over an expired timeout;
retry-retirement atomicity, derived rebuilding/CTE ownership, scalar correlation
and lowering-regression quality are also pending. Final-head delta reviews,
final PR reviewer and final-head CI are still required before merge.

### Proposed review repair: deferred failure at the operation entry boundary

**Design status: Accepted (revision four); implementation in progress, no implementation approval.** The current production
base is `feacb809443d579db7dc15a58dc1d71b329d6645`. Against that base, two retained regression
additions were red: the client unit test's twelve entry points yield ten incorrect
1031s instead of the pre-existing deferred 2018 (Commit and size are positive
controls). The real libfdb_c differential first OBSERVES timeout on both clean
transactions, then poisons both using Set followed by RYW-disable. Size witnesses
2000 on both, after which eight Go read paths return 1031 while C++ returns 2000.
The Go facade's versionstamp future blocks indefinitely before checking its
underlying error; the bounded regression now reports that failure explicitly.
Commit remains a 2000 positive control. Logs:
`pr785-review/deferred-timeout-{unit-red,differential-red-2}.log`.
The first differential attempt exceeded its shell timeout at versionstamp, not a
completed test run; the later bounded run preserves that failing dimension.

C++ `ThreadSafeTransaction.cpp:421-465,654,669,715-729` checks deferredError at
operation entry, before the underlying RYW resetPromise. `ThreadHelper.actor.h:51`
also explains why setting TIMEOUT AFTER poisoning cannot establish this test:
the already-failed deferred slot suppresses later void operations. RYW set and
RYW-disable do not reject an already-timed-out, uncommitted transaction, so the
observed-timeout-then-poison differential establishes both conditions in order.

Accepted revision-four implementation contract:

1. Capture the deferred entry error when a NEW `opContext` gains its execution
   lease; keep it in `readOperation` and preserve it when nested operations borrow
   or reacquire the captured operation. A nil/stale lease must never read a
   replacement's deferred slot. `readEntryError` prioritizes this captured error
   over the captured incarnation cause, then caller cancellation. Do NOT add a
   live deferred-slot lookup to every existing `readEntryError` call: some are
   completion/revalidation checks, and a later invalid operation must not rewrite
   an already-started read's entry outcome.
2. Route first-entry gates through that captured authority instead of the later
   duplicate deferred checks in readVersion/metrics/watch. Retain genuine
   API argument validation that occurs before C++ dispatch (e.g. inverted ranges),
   existing immutable reply selection, and Commit's separately justified mutation
   snapshot recheck. At context-free versionstamp entry, check deferredError before
   cancellation/timeout while the state lease protects the incarnation.
3. **Completion ownership belongs in the client incarnation, not in a facade
   after-the-fact publisher.** Replace the facade's commitDone/commitErr mechanism
   with client-owned pending-versionstamp and pending-commit handles, analogous to
   the existing PendingGet boundary. A versionstamp handle captures its entry
   verdict, inner transaction/incarnation and completion record synchronously;
   it never later reads a mutable facade transaction pointer. Use explicit
   pending/selected state, not 2015 as a readiness discriminator. The synchronous
   low-level GetVersionstamp API keeps its existing not-yet-committed result.
4. A completion record owns an immutable stamp/error and a done signal; all
   selection and ownership fields are protected by the existing readErrMu leaf
   lock. An incarnation tracks its uncompleted records, allowing its first recorded
   terminal cause to retire them all. No completion path rereads deferredErr:
   late poison cannot replace the entry verdict, and an earlier timeout survives
   Cancel/Reset. Selected records are never overwritten. Close ready signals only
   after result fields are final; context cancellation delivery and other locks
   remain outside readErrMu. Completed records can be removed from the pending
   set while handles retain their immutable result.
5. **Each commit producer has an owner before asynchronous execution.** A common
   client preparation routine captures the operation and claims its completion
   record **only after its deferred/terminal admission gate succeeds**. Rejected
   Commit admission completes that Commit handle, without claiming or failing
   an already-admitted versionstamp promise. The first admitted producer claims
   an initial unclaimed record to which pre-commit versionstamp handles are attached. A subsequent producer always gets
   a distinct record, even if the previous producer is still pending; new
   versionstamp calls bind to the most recently admitted producer. The facade
   invokes this admission synchronously before spawning commit work; direct and
   managed low-level Commit use the same preparation/execution path synchronously,
   with no additional goroutine on the synchronous path. Protect current-record
   replacement with readErrMu. This is explicit ownership for Go's existing
   auto-reuse/concurrent-call surface, not a change to mutation dispatch/replay.
6. **Select at the client result boundary.** A successful stamp comes directly
   from that invocation's immutable commitOutcome (version/batchID), never a getter
   on the mutable handle. Select it at successful reply processing, before metrics,
   facade scheduling or successful auto-turnover can intervene. The same locked
   selection point orders an already-recorded incarnation failure against the
   result. A later retirement cannot replace an already-selected result; a prior
   retirement keeps its first cause, even if the detached Commit still returns
   success to its own caller. **Commit completion and versionstamp completion
   are distinct outputs of the captured owner**, not one shared error result.
   Commit's own handle always returns execution's actual error after normalization
   and the uncertainty barrier. A deferred admission failure never changes an
   existing versionstamp promise; neither does a failed RYW reading barrier or
   pre-native write validation. Those previously admitted versionstamps stay
   pending until their native promise completes or their incarnation retires.
   Only the native-equivalent phase selects a versionstamp outcome: success uses
   CommitID; native commit failure produces transaction_invalid_version (2020),
   not the Commit error (NativeAPI.actor.cpp:6910-6940); no-write produces 2021.
   Classify by the C++ phase, not by a broad error-number switch. In Go, admission,
   the failed-read drain and mutation validation precede that phase; coalesced
   transaction-size validation, no-write handling, commit GRV and dispatch belong
   to native commit. Pin the split with live differentials. Detached Commit return
   semantics and stale-handle publication guards remain unchanged. The no-write
   path must explicitly distinguish its result
   from a real CommitID: C++ resolves versionstamp with no_commit_version (2021),
   NativeAPI.actor.cpp:6800-6805; pin this with the live oracle instead of treating
   a zero-filled stamp as a committed version.
7. **Every attempt is connected.** Database/Tenant managed retries already pass
   through client Commit and OnError/turnover, so their old versionstamp handles
   select that attempt's result or retirement cause directly; delete final-lastTx
   facade signaling rather than add a second completion authority. Callback errors
   retire through the existing OnError path. Caller-context exits must release
   admitted versionstamp waiters through their captured context even when no
   Commit/OnError runs. Preserve caller-first interruption mapping. User Reset
   swaps the inner transaction only after old handles have captured their owner;
   cancellation/retirement acts on that old owner, not the replacement. Facade
   ReadTransact's existing unsupported-versionstamp policy remains explicit.
8. A getter after a successful Go auto-reuse boundary may still expose the retained
   prior completed result only when no new commit producer has been admitted in
   the new incarnation. Once a new producer is admitted, its pending/error/result
   record is authoritative; failure can never reveal the prior successful stamp.
   Handles already holding the old result remain immutable. Preserve the existing
   committed-version metadata API separately from pending-versionstamp ownership.
9. **Scope amendment:** using the same captured incarnation failure authority
   necessarily also makes a healthy-created pending versionstamp observe later
   timeout, Cancel, and Reset, matching RYW::getVersionstamp's live resetPromise
   race (:2520). Include this dimension and its controlled differential rather than
   retain a second, deliberately incomplete facade wait mechanism. This is the
   same completion-ownership repair, not a claim that every other versionstamp
   or transaction behavior now matches C++.

**Scope ruling:** deferred-before-Cancel is included in this one entry-order
correction, including Commit's cancellation-first branch, as all three virtual
reviewers required. A 1031-only exception would violate the shared entry invariant.
OnError is explicitly exempt: C++ ThreadSafeTransaction::onError has no deferred
entry gate (:758); its existing retry/caller/terminal ordering must not change.
Context-free size retains its own C++ exception: deferred failure gates it, but
ordinary cancellation/timeout alone does not. Metrics inversion validation remains
before dispatch. No claim extends this repair to every adjacent API divergence.

Required proof: both poison/timeout and poison/Cancel orders, old-captured-context
versus replacement poison, preserved nil capture across nested lease reacquisition,
late poison not rewriting an admitted read, caller-first interruption and immutable
completed replies, both sides of Commit's mutation snapshot, metrics inversion and
size exceptions, versionstamp ready-error/pending/success/failure/Reset controls,
prior-success then failed reuse (including terminal 2015) without stale-stamp
success, old ready/pending futures isolated from replacement completion, retained
literal-2018 unit and real 2000 libfdb_c differential, applied/compiled
revert/mutation evidence, race repetitions, full suite, and final milestone delta
confirmations. Implementation is now present in the worktree; current evidence
is recorded below. No implementation or final-HEAD approval is claimed.

**Design review evidence:** the first direct and resumed reads failed sandbox
startup on a stale autofs mount; they produced no verdict. The same three tracked
`gpt-6-astra`/`xhigh` sessions then reviewed a numbered, hashed source packet supplied
as input, without relaxing sandbox permissions. All returned DESIGN NAK on the
original facade shortcut while supporting leased entry capture. The second
proposal was also NAKed: a facade getter/publisher cannot establish
source-result ordering, pending is not unclaimed, and lastTx abandons earlier
managed attempts. The client-owned completion design and explicit scope above
are revision four. Revision three received a Torvalds design ACK
but C++ and independent review NAKed forwarding early Commit failure into an
already-admitted versionstamp; the separate outputs/phase distinction above repairs
that design error. Revision four (`605ed116f83d…`) received DESIGN ACK from all
three same-model/same-session virtual reviewers in
`precedence-design-{cpp,torvalds,codex}-revision-4-verdict.md`. Native failure
publication must follow uncertainty-barrier completion and preserve C++'s
actor_cancelled exclusion. These are supplied-byte design approvals only.
All implementation and final-HEAD gates remain open.
Artifacts: `pr785-review/precedence-design-{cpp,torvalds,codex}-packet-verdict.md`;
these are virtual reviews of supplied bytes, not human approvals or independent
live-filesystem verification.

Two further retained admission tests compiled and ran against unchanged production:
borrowed and reacquired clean entry both incorrectly observe late 2018; a captured
poisoned entry loses its 2018 to retirement 1025; the clean-old/replacement-poison
control passes. Four subcases, three semantic failures, six total RUN lines.
`pr785-review/deferred-entry-capture-red.log` records the run (before a diagnostic-only
wording correction). They are additional RED evidence, not implementation proof.

Further retained RED proof at the same production SHA:
- `deferred-cancel-unit-red.log`: both Cancel/poison orders, twelve entries each;
  eleven failures and size positive control per order (27 RUN lines).
- `versionstamp-completion-differential-red-2.log`: four cases; C++ confirms 2021
  after no-write Commit, 2020 after native size failure (Commit itself 2101), and
  1031 after timeout configured on an already-pending healthy future. Go returns
  success, 2101, and no completion within five seconds respectively. The Cancel
  control passes on both. The earlier three-case log is a smaller historical run.
- `versionstamp-pre-native-differential-red.log`: late deferred 2000 and retained
  failed read 1036 both fail Commit without completing C++'s earlier versionstamp;
  subsequent Cancel gives that old future 1025. Go instead forwards 2000/1036.
  A new post-poison versionstamp correctly gives 2000 on both. These two cases
  establish the pre-native/native distinction independently of Go's current output.

### Deferred entry / versionstamp implementation evidence (initial ef1b0532)

The accepted revision-four design is now implemented above production
`feacb809443d579db7dc15a58dc1d71b329d6645`. Client `PendingCommit` captures admission
before facade goroutine startup; synchronous Commit uses the same admission and
execution without an additional goroutine. `PendingVersionstamp` holds a captured
incarnation and explicitly selected completion. First terminal cause seals all
pending records under readErrMu; native outcomes are immutable and no completion
rereads deferredErr. The facade has no lastTx signaling or shared Commit-error
arbiter. ReadTransact explicitly retains its unsupported 2015 policy. Go's
concurrent producer and auto-reuse behavior is an extension, not C++ parity.

The first restored libfdb_c run passes all four selected top-level tests (20 RUN
lines), including retained REDs for deferred/timeout, 2020 versus Commit's 2101,
2021 for no-write, live timeout of healthy pending stamps, and pre-native failures
remaining pending until retirement: `versionstamp-first-green.log` (55s).

The completion unit suite exposed a provisional omission: C++ actor_cancelled
is operation_cancelled 1101 (`flow/Error.h:109`, `error_definitions.h:114`), not
only Go context cancellation. `versionstamp-selection-unit-red.log` records its
executed failure; that exclusion is now implemented. Client/facade targeted green
in `versionstamp-lifetime-green.log` passes both targets with 90 RUN lines (89s).
Real-FDB pins hold native dispatch and the uncertainty barrier, and hold the
metrics mutex after success to prove stamp selection precedes metrics and Reset.
They compare successful stamps with persisted versionstamped bytes. Database AND
Tenant coverage includes successful retry, terminal callback 2015, caller exit
without Commit/OnError, and no-write completion. A further real-FDB overlapping
producer case passes in `versionstamp-overlapping-green.log` (one RUN line).

Compiled negative controls (not build-failure credit):
- `versionstamp-native-error-mutant.log`: passing Commit errors to stamps fails
  literal 2101 and 2015 cases that must select 2020 (12 RUN lines, five FAIL lines
  including parent tests).
- `versionstamp-producer-mutant.log`: removing the claimed-owner distinction
  fails producer identity (one RUN and one FAIL).
- `versionstamp-retirement-mutant.log`: context delivery without sealing pending
  records fails all three Cancel/Reset/timeout siblings consumed after successful
  native reply (six RUN lines, four FAIL lines including the parent).
- `versionstamp-facade-revert.log`: restoring the old transaction/database/tenant
  facades fails managed attempt/Reset waiter ownership (six RUN lines, four FAIL
  lines including the parent).

Each `.source.txt` records applied source hashes and verified exact restoration;
`versionstamp-mutants.py` retains the exact mutations and commands. Artifacts live
under `/var/tmp/query-grind-cast/pr785-review`. Ten race repetitions pass with
910 RUN lines and zero FAIL/SKIP lines across both targets (214s), and every
`versionstamp-race-tested.sha256` hash remained unchanged during that run. A later
fuzz-only append to `versionstamp_completion_test.go` adds an independent first-
effective-event model. The initial 25-second attempt completed zero seeds during
worker FDB-fixture startup; it is not fuzz evidence. The 90-second, one-worker
rerun completes all seven seeds and 2,837,314 executions without failure
(`versionstamp-fuzz-2.log`). Bazel explicitly warns that this binary lacks
coverage instrumentation: this is unguided lifecycle fuzzing, not wire coverage.
The following full-gate follow-up records restored suite results. Milestone
implementation/exact-final-HEAD PR approvals remain open; these tests do not close
the other review findings.

Full-suite follow-up: the first `just test` (`versionstamp-full.log`, 978s) was
RED, 91/92 targets. `TestMetricOps_EarlyReturnPrecedence` called the private metric
Impl functions with a bare context, bypassing the new captured-admission boundary.
Its two poison/timeout cases therefore lacked a deferred entry verdict and got
1031 instead of 2000. Production callers already pass through the public leased
wrappers. The retained test now exercises those public wrappers, with ALL original
expected codes unchanged and two added inverted+poisoned+timed-out 2005 assertions.
This is a test-entry repair, not an error-expectation relaxation or a new live
revalidation gate. A compiled mutant discarding readEntryError's captured verdict
still fails both 2000 assertions (`metrics-entry-mutant.log`); source restoration
is SHA-checked. Public metrics plus all four libfdb_c differential targets pass
`versionstamp-restored-differential-metrics-green.log` (21 RUN lines, 45s).
The final ten-run race pass also includes the repaired metric boundary and fuzz
seeds (`versionstamp-race-final-10.log`, 1,000 RUN lines, zero FAIL/SKIP lines,
213s). Full `just test` rerun passes 92/92 targets (43 executed, 49 cached,
947s), with all `versionstamp-full-2-tested.sha256` hashes unchanged during the
run (`versionstamp-full-2.log`). Implementation and final-HEAD review gates remain
open.


### Versionstamp implementation-review corrections (after ef1b0532)

The three tracked virtual implementation reviews all NAKed ef1b0532. Their
supplied-packet verdicts are `precedence-implementation-{cpp,torvalds,codex}-verdict.md`.
They identified current-head rediscovery by the synchronous getter, retention of
an older successful producer instead of the latest admitted producer, and a
native-result defer manufacturing success during panic unwinding. None of the
prior race/differential/full-suite greens exercised these schedules.

The repair passes the getter's lease incarnation into lookup, with deferred entry
before the leaf-locked captured cause/completion. Successful turnover retains the
retired incarnation's latest producer independently of committed-version metadata.
A retained retirement carries its sealed cause even when a new caller is admitted
on the replacement; caller-first interruption remains local. Native outcome
publication now follows a normal wire/uncertainty-barrier return, not a defer
that runs during panic. Go panic propagation is unchanged, with no fabricated
success/error code; later retirement still completes the pending stamp. These
implement the existing accepted ownership/result-boundary contract.

Retained real-FDB controls cover four getter turnover schedules, four opposite-
producer schedules, and two panic phases (before dispatch and during the dummy
uncertainty barrier after literal wire 1100). The first nine-cell run was RED
(11 RUN lines including parents). The first repair exposed another wrong-owner
retirement mapping (bare context cancellation instead of 1025), which is repaired
without changing expected codes. A caller-first unit control pins the local/shared
split. Four independent compiled mutants are killed at the intended semantic
assertions: replacement-head 2015, older-success nil, panic-created stamp and
successor-context cancellation. Every changed file is restored byte-for-byte and
SHA-checked by `versionstamp-review-mutants.py`; the summary records RUN counts
3/5/3/3. The nil-head deadlock regression is not counted as a semantic mutant kill.
Artifacts live under `/var/tmp/query-grind-cast/pr785-review`.

Restored gates pass on unchanged Go bytes: ten client/facade race repetitions
(1,140 RUN lines, zero FAIL/SKIP, 214s), entire client/facade race suites (2,000 RUN
lines, zero FAIL/SKIP, 253s), and the four libfdb_c differentials (20 RUN lines,
zero FAIL/SKIP, 24s). `just test` passes 92/92 targets (43 executed, 49 cached,
932s); its output is target-level, not per-test RUN evidence. The seven Go hashes
match across both race runs and all nine changed-file hashes match across the
full run. The two tracking documents receive this evidence update afterwards;
the normal commit hook must verify those updated bytes as well. Logs and hash
manifests share the `versionstamp-review-` artifact prefix.

This correction subsequently passed the normal hook and was committed/pushed as
cc2e21d14, with three scoped same-session delta ACKs. Those approvals do not close
the other PR findings or give human approval. The next block resumes conditional
turnover; TODO.md's latest QSC-04/08 block tracks CI/remaining-finding status.

### Conditional-turnover validation/retirement correction

The preceding versionstamp correction is committed/pushed as cc2e21d14 and has
three scoped, supplied-byte virtual implementation ACKs on that exact HEAD.
Those ACKs explicitly excluded the already-disclosed beginTurnover gap.

The accepted Drain protocol closure requires one readErrMu critical section for
expected-owner validation, replacement-not-ready publication and old-owner
retirement. Implementation split validation and retirement with an unlock/relock.
A completed Cancel/timeout in that interval could therefore be followed by
OnError returning nil and resetting away the terminal cause. This is a missed
implementation obligation, not a new lifetime mechanism or design change.
C++ 7.3.77 ReadYourWrites.actor.cpp:1499–1537 checks/races resetPromise in onError;
after the retry wait resumes it reaches resetRyow without another actor yield.

`TestFDBOnErrorCannotEraseTerminalCauseAtTurnover` pins both terminal sources
through public OnError over a real FDB-backed transaction. A boundary hook pauses
the conditional lifetime claim; Cancel or the deterministic existing timer callback
finishes first. Expected OnError errors are literal 1025 and 1031; the old owner
must remain terminal. Explicit Reset then discards its mutations and permits only
new writes to commit. Both cases are RED before repair (three RUN/FAIL lines,
including the parent, `turnover-terminal-red.log`).

The repair transfers the already-held leaf lock and captured incarnation into
turnoverLocked. Expected-owner validation and retirement cannot interleave with
terminal publication. Unconditional user Reset acquires the same leaf lock before
calling that helper. The pre-claim hook, cancellation delivery, timer stopping,
resource cleanup, drain waits and reset fields all stay outside readErrMu. No new
wire format, error mapping, retry policy or deferred gate is involved.

Restored targeted execution passes 27 RUN lines with zero FAIL/SKIP. A compiled
literal reversion to the split claim (retaining the same boundary hook) gives nil
instead of both expected errors. Full source and SHA-checked restoration are in
`turnover-terminal-revert-source.txt`; logs use `turnover-terminal-` under
`/var/tmp/query-grind-cast/pr785-review`.

Restored ten-run race passes 1,320 RUN lines, zero FAIL/SKIP (306s), including ten
executions of the new regression. Entire client/facade race suites pass 2,003 RUN
lines, zero FAIL/SKIP (258s). All three changed Go hashes match across both runs.
`just test` passes 92/92 targets (43 executed, 49 cached, 968s); all five changed-
file hashes match across execution. These evidence-only TODO/RFC updates follow
the full run and remain subject to the normal commit hook. Logs/hash inventories
use the `turnover-terminal-` prefix. The normal hook subsequently passed and the
correction was committed/pushed as 3f15b0877. C++ and Torvalds gave scoped
atomicity ACKs, but the independent review NAKed rejected-turnover watch cleanup,
addressed in the following block. Other PR findings remain open.

### Rejected-turnover terminal watch cleanup correction

The independent implementation review of 3f15b0877 accepted the atomic claim but
found a cleanup obligation missed after OnError releases its lease. Timeout
rejection skips resetFields, and the terminal defer previously canceled watches
only with the original lease still active. The accepted incarnation protocol
already requires terminal cleanup without affecting successor state.

C++ 7.3.77 ReadYourWrites.actor.cpp:1499–1533 races OnError against resetPromise;
its watch actor at 1284–1335 observes transaction failure. NativeAPI.actor.cpp:
5637–5684 releases the outstanding-watch counter on both completion and failure.
The Go correction reuses enterReadState with the captured incarnation and
wait=false when the original lease was released. A successful cleanup lease
prevents replacement publication until cancelWatches finishes; if replacement
has already begun, identity rejection leaves its watches untouched. Watch mutex
acquisition and cancellation delivery remain outside readErrMu. No new lifetime
mechanism, wire bytes, retry/error policy or C++ concurrency guarantee is added.

The retained real-FDB turnover regression now requires pending-watch termination
and slot release before any explicit Reset/deferred Cancel. Timeout is RED before
the fix while the public Cancel control passes. A second retained test parks
released-owner cleanup, resets and creates a replacement watch, then verifies
old OnError still returns literal 1031 without canceling the replacement. An
independent writer changes the watched key with a server-minted versionstamp
(also different across -test.count iterations); the replacement watch must fire
successfully and release its slot. This checks functionality, not just an unset
cancellation bit. Controlled Go schedule tests are not live C++ differential
coverage.

The first focused repaired run passes 23 RUN lines, no FAIL/SKIP. Compiled omission
and successor-reclaim mutants fail the old-watch leak and replacement-watch
cancellation assertions, respectively (three RUN/two FAIL including parent; one
RUN/FAIL). Both were rerun against the final repeat-safe test bytes; original
source restoration is byte-for-byte/SHA-checked. Logs and source artifacts use
`turnover-watch-` under `/var/tmp/query-grind-cast/pr785-review`.

Restored ten-run race passes 1,510 RUN lines, both turnover tests ten times, zero
FAIL/SKIP (360s). Entire client/facade race suites pass 2,004 RUN lines, zero
FAIL/SKIP (273s). Both changed Go hashes match across both runs. `just test` passes
92/92 targets (43 executed, 49 cached, 961s), all four changed-file hashes
unchanged; target-level output is not per-test no-skip evidence. These evidence-
only TODO/RFC updates followed the full run and passed the normal hook (92/92,
one executed). The correction is committed/pushed as 2212c0643; all three tracked
virtual reviewers gave scoped IMPLEMENTATION ACK on that exact HEAD, explicitly
closing the independent watch-cleanup NAK. These supplied-byte reviews are not
human/full-PR approvals. The following block resumes the timestamp-fixture repair;
other PR blockers remain open.

### CURRENT_TIMESTAMP fixture expiry correction

The feacb809 Build/Lint/Test CI failure was seed INSERT transaction-too-old in
Where batch1000 and CrossPage batch6000, before any timestamp assertion. The
client lifecycle repair and subsequent green CI do not resolve this separate
fixture assumption. Its saved patch is restored after the client cleanup's
scoped exact-HEAD ACKs, with both fixture-file hashes verified.

Keep all three 10,000-row test populations, all timestamp expectations and the
cross-page execution options. Factor only seed setup through the existing
retryTx helper: batch100, full explicit transaction per batch, three attempts on
typed transaction-time-limit failure only. Each body commits and the helper
rolls back failed attempts. Other errors, including conflict and unknown commit,
still fail immediately. No production SQL retry, clock or pagination code changes.

CURRENT_TIMESTAMP is a Go extension here, not Java row-conformance evidence:
Java4.12.11.0 BaseVisitor:1376–1380 delegates its visitor to children, and
QueryExecutionContext:28–65 supplies no per-statement instant. The independent
Go contract remains the retained statement/predicate/pagination assertions.

The retained SimFDB regression injects canonical1007 AFTER warming the query
connection/catalog, requires exactly one helper retry, and checks ordered rows
against the independent population0…999. Initial single-shot setup is RED;
initial repaired execution passes that regression, all three real-FDB timestamp
tests and five SQL1007/1021 controls (nine RUN lines). The compiled single-shot
reversion with SMALLER batch100 also fails at the injected1007 assertion (one
RUN/FAIL); restoration is byte-for-byte/SHA-checked. This distinguishes the retry
fix from merely reducing work. SimFDB is not wire-fidelity evidence, and no
Java rejection is counted as row evidence. Artifacts use `timestamp-seed-` under
`/var/tmp/query-grind-cast/pr785-review`.

Ten serial uncached race processes pass 240 RUN/PASS lines, zero FAIL/SKIP;
each touched test runs ten times (250s), source hashes unchanged. The first
unfiltered SQL-driver race run hit an ad-hoc 1800s timeout (6673 RUN/6672 PASS):
only MetamorphicPagingAtScale remained, runnable in LIMIT/sort continuation
serialization. The published eternal budget is 3600s in .bazelrc, not that
override. The unchanged full scope at its published budget passes 6688 RUN/PASS,
zero FAIL/SKIP (1849s), all four changed-file hashes unchanged. The paging test
finishes 140 checks in 1789s; the additional 15 RUN lines are the subsequently
reached FuzzSQL_QueryContext function and 14 seeds. Retained CPU profiling shows sort
continuation encoding consuming 757.75/6027.41 sampled CPU seconds cumulatively;
no baseline/performance-improvement claim is made. No repository budget, test
population or expectation changed. Logs/profile use `timestamp-seed-race-full`
and `timestamp-seed-race-full-2` prefixes. Full non-race `just test` passes
92/92 targets (three executed, 89 cached, 249s), all four changed-file hashes
unchanged (`timestamp-seed-full.log` and hash records). Target-level output does
not establish per-test no-skip evidence. Only these TODO/RFC evidence paragraphs
change afterward and still require the normal hook. Implementation/final-HEAD
review gates remain required; the latest QSC-04/08 block tracks other PR blockers.

### Retained bound CTE/derived bodies — review-repair design (DRAFT)

This amendment resumes the open ed1f413 architectural findings, not a new hunt
family. Implementation is gated on the existing tracked design reviewers.
Current production HEAD is 90b026077cb08a028cf567e423d39bca86eb2ae4; only tests and
tracking docs change during diagnosis. TODO's final QSC-04/07/08 block records
execution evidence; artifacts are `/var/tmp/query-grind-cast/pr785-review`.

**Observed lifecycle.** `PlanVisitor.prepareDerivedSourceBodies` calls
`buildCTEBodyQuery` before the parent classifier or scope consumer. This uses the
same visitor's lexical CTE scopes AND bodies, builds the complete query, and
finishes `upgradeProjectionValues`/`publishInheritedProjectionNames` before
returning. The prepared operator survives in `catalogAwareInnerPlan` and the
alias carrier. `boundDerivedSource` nevertheless unconditionally rebuilds it
using a new visitor with only cteScopes, losing cteBodies and repeating every
nested body. The comment claiming prepared output names are premature is
contradicted by the retained K/K BIGINT/DOUBLE control at nesting depths0–3.
The query-body-access regression measures 5/21/85/341 accesses at depths1–4,
not elapsed-time or parser-invocation counts.

`BuildScalar` already translates the complete operator and obtains its free
correlations. A CTE capturing O.ID yields exactly {O}; a local O shadow inside
the definition yields {}. Both the direct parse-tree veto and the scalar Main's
local-source blacklist can wrongly erase captured O. The definition's lexical
binding does not change when its caller aliases the CTE scan O. Registration
must not override the bound expression property with either heuristic.

**Design revision 2 (still DRAFT).** All three tracked reviewers NAKed the first
amendment's unchanged UNION fallback: it retains the same ownership defect,
not merely extra traversal cost. The retained real-FDB crossing regression adds
a DOUBLE branch to the CTE-backed BIGINT scalar and fails42F01 for C instead of
float64 rows7,9.5 (`cte-promoted-union-red.log`, one RUN/FAIL). That counterexample
supersedes the proposed fallback exclusion. No production edits have begun.

Retain the prepared body for ALL derived metadata, including promotion. Move
any necessary body construction to its owning visitor/builder boundary, where
the complete parent/CTE-schema/CTE-body environment and binding identity exist.
The bound-source conversion receives a completed operator, not permission to
reconstruct it from syntax. A missing body is an explicit construction error;
legacy parse-only adapters must prepare once through their owning visitor
before conversion, never invent a partial environment in the scope consumer.
Do not cache by SQL text or add mutable global caches.

Concretely, factor the existing `exactUnionResultRow` positional type fold into
one pure common-record-row helper used by both that translator and metadata
derivation. Preserve its branch-width/incompatible-column SQL diagnostics,
first-leg physical names, per-slot MaximumType/nullability, and exact validation.
Add a distinct `LogicalResultTypeAfterUnionPromotionWithCTEs` property entry
point over the retained logical tree. Share the existing recursive type walker;
its UNION policy is explicit: the strict API requires branch agreement, while
the new prospective-output API uses the SAME common-row helper the translator
will normalize to. This is a derived output contract, not a claim that promotion
has already rewritten the operator. Other operator rules and bound Values are
unchanged. Local CTE Body is visited in its defining environment; only Main sees
its new binding. Do not let an inference error fall through to a shadowed outer
binding. Keep `ExactLogicalResultType`, its strict entry points and the existing
negative UNION test unchanged.

At the embedded boundary, try strict type derivation first, then derive the
post-promotion row from the SAME body if needed. Factor the existing exact
record-to-semantic-source conversion rather than add a second mapper. Derive
SQL labels separately from that retained body's existing label property;
never use promoted physical names as SQL labels. A promotion/type error remains
its typed error, while a row that semantic.Column cannot represent remains a
loud unsupported-schema error. Neither triggers parse-tree reconstruction or
UNKNOWN synthesis. Keep cteSourceAs/private derived identity unchanged.

Classify scalar correlation by intersecting the translated Reference's free
identifier set with the parent's actual named binding identifiers (the same
CorrelationName/Alias identity convention expr.resolvedSourceColumnAt uses).
Use CorrelationIdentifier equality, not EqualFold on rendered names; remove
both the source-name and syntax vetoes from BuildScalar. Keep cardinality,
output arity/nullability, pagination and scalar attachment unchanged. Preserve
`!col.qualified && (col.bound != nil || isField)` and AsFieldValue in projection
publication. This revision does not authorize unrelated executor work.

**Reference and oracle.** Java4.12.11.0 QueryVisitor:170–181,688–691 builds a
query once and renames the resulting operator. AbstractRelationalExpression-
WithChildren:57–77 derives free references from values/quantifiers. This is the
architectural reference, not row evidence for nested scalar/CTE SQL: those
queries are Go extensions. The independent real-FDB expected results are row7
for the enclosing-CTE/derived query and (7,7),(9,9) for two outer identities.
Java rejection does not validate either answer.

**Required verification.** Retain the three original semantic RED regressions,
the captured-outer/local-shadow controls, the nested-body access bound, and
exact duplicate-label/type controls. Green must execute through the actual
BuildScalar and real-FDB driver, not a substitute planner. Existing alias,
CTE/derived, UNION, projection/metadata and scalar-cardinality controls remain
mandatory. Compile mutants that restore rebuilding and each correlation veto;
require the corresponding semantic/count failures, restore hashes, rerun green.
Run affected uncached targets, ten determinism/race repetitions, full just test,
and applicable published planner performance/stress gates without relaxing
limits. Complete milestone reviews and final-HEAD CI after findings are folded.
No wire/client, autocommit or executor-performance change is part of this design.
UNNEST lowering and full-PR review gates remain open; no merge/new hunt authorized.

### Retained-body implementation and EXISTS ownership follow-through

**Revision-2 design accepted, implementation not approved.** The three tracked
virtual reviewers returned scoped DESIGN ACKs for packet
`3a79d427302f0a2a5a0c13c3bdd05e914d8702dc4fc5de1c705160d536bf1272`, against
committed `90b026077cb08a028cf567e423d39bca86eb2ae4`. Those packet-only verdicts
supersede the preceding historical “no production edits” checkpoint; they are
not human, implementation, full-PR or merge approval. All seven CI checks on
that committed HEAD are now successful (`cte-bound-pr-state-current.json`),
not evidence about the uncommitted implementation.

The worktree now retains derived bodies, shares strict/prospective UNION type
inference and removes both scalar-correlation vetoes. Scalar ordinal seeding
uses the translated inner's one-column flowed row, not a second `legColumns`
walk that can read a WITH definition instead of Main. Initial affected focused
execution passed; the pre-supplement full query/embedded run executed 2,462
RUN/PASS, zero FAIL/SKIP. Post-packet supplements independently specify promoted
slots `[X nullable DOUBLE, X_2 non-null BIGINT]`, duplicate SQL labels `[X,X]`,
strict rejection after prospective inference, unchanged branch identities,
local-shadow typed width/incompatibility errors and Main-versus-definition
scalar typing. Their focused execution has seven RUN/PASS, zero FAIL/SKIP
(`cte-bound-type-supplements-first.log`). These tests were not in the approved
revision-2 packet.

The required adapter audit found the same ownership defect in EXISTS. Two
retained embedded regressions failed: the standalone UNION metadata adapter
accessed its body twice, and the correlated-EXISTS source builder lost enclosing
C's body and returned 42F01 (`cte-bound-legacy-supplements-red.log`). The
standalone adapter now prepares once through its owner and calls the bound
converter; the old derived-term/UNION/manual joined-schema fallback chain is
removed. Its existing four-way join-nullability regression now exercises the
retained logical join, with unchanged expected nullabilities. Correlated source
carrier and metadata consumers now share a prepared body. Their direct builder
pins pass. An initial supplementary assertion wrongly expected O in the returned
FROM body's free set: this API carries the comparison on `lastJoinPredicate`
and the body's independent scalar on the returned scalar-plan list. The pin now
checks both exact attachment identities and the retained scalar binding. This
was a probe-boundary mistake, not an engine regression or mutant kill.

**Open end-to-end counterexample:** with `t(id)={(7),(9)}`,

```sql
WITH c AS (SELECT id AS v FROM t)
SELECT o.id FROM t o WHERE EXISTS (
  SELECT o.id FROM (SELECT (SELECT MAX(v) FROM c) AS x FROM t) d
  WHERE d.x = o.id
) ORDER BY o.id
```

Independent answer: `[9]`; replacing EXISTS by NOT EXISTS answers `[7]`.
Both retained real-FDB arms still fail42F01 before the repaired correlated
builder: `buildExists` first enters a schema-only query constructor. This nested
composition is a Go extension, not a Java row comparison.

A direct change to call the general visitor with its complete parent scope
made those two arms pass but bypassed the current EXISTS correlation attachment
and cardinality lowering. The focused EXISTS population executed **723 RUN,
594 PASS, 129 FAIL, zero SKIP** (`cte-bound-exists-owner-first.log`, two targets).
Failures include absent runtime outer bindings, lost indexed-probe shapes,
N-way/projected EXISTS planning and changed deliberate pagination/ON rejections.
The direct-switch experiment was reverted; no assertion or golden was relaxed.
This is a phase-boundary defect in the attempted migration, not a reason to keep
an old API for compatibility. The owner explicitly authorizes pre-release hard
changes for correctness.

**Supplementary design — DRAFT, implementation requires scoped design ACK.**
Use the normal visitor as the sole query constructor in the complete defining
scope, but preserve EXISTS lowering as an explicit consumer of the bound
query, not an error-driven reconstruction of its syntax:

1. Return an owner-produced bound-query result internally, carrying the final
   operator and, for a SELECT block, its prepared source graph, exact semantic
   scope/binding identities and already-resolved WHERE/ON Values plus nested
   subquery attachments. Thread this result through the query/SELECT visitor's
   return values; do not add mutable “last SELECT” state or cache by syntax/text.
   Ordinary callers can consume its operator. CTE Body and Main retain their
   different defining environments; nested construction cannot replace the
   parent's result. This is a construction artifact, not a second planner.
2. Classify dependence algebraically from the bound relational expression's
   free identifiers intersected with actual parent binding identities, sharing
   the scalar convention. A translation/typing failure remains its real error,
   never evidence of correlation. No undefined-column retry, syntax reference
   scan or source-name blacklist. Do not erase independent scalar attachments
   from the free-set accounting.
3. Lower a correlated EXISTS from that bound result into the existing
   `logical.ExistsSubquery` contract: retain the prepared FROM bodies and private
   identities; carry mixed inner/outer predicates on the attachment; keep
   outer-only predicates under the existential in BOTH polarities; retain
   nested EXISTS/scalar plans. Apply existing ON placement and unsupported-shape
   rules to resolved predicates and the bound join graph, including current
   RIGHT/FULL and multi-source rejection contracts. Never rebuild a derived
   body or re-resolve a Value against a different frame during lowering.
4. Projection elision is existence-specific and must preserve cardinality:
   aggregate/HAVING/DISTINCT/LIMIT/OFFSET cannot be dropped indiscriminately.
   Preserve current known-truth folding and typed unsupported contracts; the
   retained typed pagination/source clauses may supply the existing acceptance
   checks, but not correlation classification or body reconstruction. The
   existing projected-EXISTS, indexed-probe, gather/UNNEST and negative plan
   tests stay unchanged. No executor-performance repair is authorized.
5. Remove superseded query construction/retry helpers once callers use the
   owner result; do not leave a compatibility façade or competing schema walk.
   If the bound result cannot express a required current attachment, complete
   that representation before removing its old producer—never silently decline
   a formerly supported query.

Acceptance adds: the two real-FDB polarity rows above; actual entry-point
preparation counts and retained-body identity in correlated primary/JOIN-leg
sources; nested CTE capture/local shadow controls; original EXISTS population
including its 129 failed outcomes restored without weaker assertions; typed
invalid-body diagnostics; then separate compiled mutants, affected full suites,
race/determinism, unchanged-limit stress/performance and milestone/final-HEAD
reviews. Prior revision-2 implementation work is not being re-approved by this
supplement. UNNEST lowering and full-PR gates remain open. No merge/new hunt.


### EXISTS binding/lowering handoff — supplemental revision 2 (DRAFT)

The supplemental virtual Graefe review NAKed packet
`68a97641b83e9ef5cb3f1d56e3bacc96368454bc420e019d41a505a04f8cbc43`.
The preceding five requirements did not specify when identities are assigned,
where ON provenance survives folding, or how classification precedes lowering.
This amendment replaces that underspecified handoff, not the accepted retained
body/type work. **No phase-boundary implementation is authorized until the
tracked supplemental design reviews ACK it.** Reviewer verdicts are scoped,
virtual and packet-only, never human or full-PR approval.

#### Two return values, one constructor

The internal query visitor returns a `boundQuery`, not an operator plus mutable
visitor scratch state. Its operator is the existing logical tree, constructed
once. Additional fields record binding facts that the current tree loses:

- SELECT frame: prepared FROM graph with original body pointers, each source's
  SQL name, private binding identifier and exact output type; immutable parent
  frame and CTE-definition handles; resolved projection/group/HAVING/order Values
  already present on the operator, resolved WHERE predicate and ordered ON
  records. Each ON record contains its join node, join kind, left-to-current
  visible binding identifiers, resolved predicate and owned attachment edges.
- Attachment edge: generated result identifier, EXISTS/scalar kind, owning
  clause/frame, bound child query (or already-retained child Reference), and
  the actual binding/evaluation boundary. A scalar's arity/cardinality contract
  is separate from an existential's row type. Every generated alias used by a
  Value has an edge or an explicitly inherited binding; missing ownership is
  a typed construction error, not grounds to treat it as independent.
- WITH/UNION/parenthesized nodes retain their child results, not the last SELECT
  visited. Each WITH definition owns its defining frame; Main sees the completed
  new definition. A CTE source points to that definition and its retained body,
  not a future name lookup through a mutable registry. Recursive definitions
  retain their existing explicit self-binding rather than creating a recursive
  property walk through an unscoped registry.

These are annotations/edges on the constructed logical expression, not another
SQL planner or a second schema derivation. Internal query, SELECT, UNION and
parenthesized return paths thread the result. Ordinary public callers unwrap
its operator. No parse-node/text cache and no `lastBoundSelect` field.

`lowerBoundExists(boundQuery, parentBindings)` returns a separate value:
`{Plan, JoinPredicate, KnownTruth, FlowedType, owned attachment registrations}`.
It receives no parser or resolver. Its outcome is complete or an error; it
cannot append to the caller's planner lists. `BuildExists` validates this value,
then publishes the existential and all required scalar/nested registrations in
one success-only step. This removes `lastJoinPredicate` and its outer-only
scratch flag, including partial append/rollback as a construction protocol.
A failed bind, admission check, type derivation or lowering leaves the parent's
three registration lists unchanged. Nested construction uses private results,
not the parent's accumulating slices.

#### Ordering and identities

1. The owner allocates each source's runtime identifier before any Value is
   resolved against it. Extend current `assignDerivedSourceBindings` ownership
   to catalog/CTE sources and relevant JOIN/UNNEST legs in nested frames; the
   correlated fallback's late single-source mint disappears. SQL qualifiers and
   additional qualifiers remain lexical names. Scope `CorrelationName`, scan/
   CTE/UNNEST `Binding`, and emitted named QOV identifiers use the SAME assigned
   identity, including primary catalog scans (whose current visitFrom arm does
   not carry `fs.bindingID`). No post-binding rename or qualifier stripping.
   One deterministic construction-owned allocator is shared with child query
   owners. It reserves supplied parent identifiers, lexical names and previous
   generated identifiers; child/CTE preparation cannot restart its namespace.
   Nested/returned artifacts keep their assignments. Do not depend on the global
   planning counter or use a SQL alias as a substitute for private identity.
2. Allocate identity before registration, but register sources incrementally:
   ON(i) resolves against parent + primary + legs through i only. Preallocating
   a later leg must not make it visible early. WHERE sees the completed local
   frame. Save each resolved ON record and attachment ownership BEFORE
   `foldInnerOnExistsIntoWhere`; the eventual fold may change the executable
   tree but cannot erase the bound record's origin. WHERE and projection
   attachments have their own origins. Nested visitors cannot overwrite them.
3. Complete query binding and its attachment graph before dependency
   classification or extraction of correlation conjuncts. Snapshot maps/frames
   on return; restoring the visitor's WITH registries must not mutate a returned
   definition's environment. Keep body-before-Main typing and propagate local
   body errors; never fall back to an enclosing same-name CTE.

#### Classification without a lowering cycle

Dependency is a pure property of this bound graph, not a flag inferred from an
error, syntax occurrence, missing source name, or a physical winner. Use the
Java quantifier rule (`AbstractRelationalExpressionWithChildren.java:57–77`):
local Values contribute their referenced identifiers minus identifiers bound
at that node; child edges contribute their free dependencies, subtracting local
binders only at boundaries that actually satisfy child correlation. The graph
records those binders and edge boundaries while binding. A source/attachment
alias is not globally subtracted merely because it appears somewhere below.

For a SELECT's correlating frame this means unioning its resolved clause/output
references with dependencies of its source and attachment edges, then removing
its own satisfied bindings. At non-correlating boundaries, child dependencies
remain free, as in Java's `canCorrelate` condition. CTE source edges contribute
the referenced definition's free dependencies in its defining environment;
unused definitions do not become runtime dependencies of Main. UNION contributes
each bound branch's dependencies. Existing recursive feedback bindings remain
local; they are not parent dependencies. No Cascades translation is needed to
obtain this property, so classification cannot require successful correlated
EXISTS lowering first. Unsupported bound operator/property cases fail typed;
they do not retry another constructor.

In particular, a generated scalar alias is accounted for through its actual
attachment edge. An independent scalar contributes no parent dependency but
its plan/registration is retained wherever its alias is read. A correlated
scalar contributes its child's free identifiers. An attachment evaluated in an
ancestor remains an inherited binding at inner nodes, rather than being erased
there. Intersect the resulting free set with actual parent binding identifiers
using identifier equality and the resolver's named-identifier convention
(`expr.go:536–548`), not SQL source spellings. This preserves a CTE body's outer
capture even when Main aliases the CTE with that same SQL name.

Capture the pre-normalization property before extracting predicates. After
existence-specific projection elision, derive the residual property again from
the retained plan AND attachment predicates/edges. A discarded output-only
outer reference cannot demand a runtime outer binding. Drop a projection-owned
attachment only when no retained Value or cardinality operator requires it;
never indiscriminately discard an independent scalar used by WHERE/ON/HAVING.
Nested EXISTS children use the same bind/property/lower sequence recursively;
classification of each child is available before its own lowering, not inferred
from whether that lowering happened to succeed.

#### Explicit normalization and admission

The lowerer consumes resolved predicates and retained source identities. INNER
ON's inner-only conjuncts stay at their original join node. A permitted mixed
correlation becomes the attachment join predicate. Outer-only predicates stay
under the existential in both polarities. A nested existential's middle FROM
and non-EXISTS predicate cannot be dropped just because a child edge exists.
If an existing composition has no valid attachment placement, preserve its
current typed unsupported result instead of outer-routing it under negation.
No executor or performance workaround is authorized by this amendment.

Admission and dependency are separate. Preserve current typed rejections for
nested subqueries in correlated ON, correlated OUTER ON, an earlier correlated
ON before a later RIGHT/FULL join, the existing multi-source/shadow-collision
controls, correlated scalar in unsupported EXISTS WHERE positions, and current
projected-known-truth/pagination restrictions. Identity repair does not
implicitly authorize expanding these shapes. Evaluate these contracts from the
bound join/source/clause-origin records, never by detecting correlation from
SQL names or re-walking query syntax. A source's lexical name may be retained
for an explicit admission contract, but cannot override its bound free set.

Retain aggregate/HAVING/DISTINCT/pagination operators unless the existing
existence/cardinality rule proves their removal or known truth. Capture typed
pagination facts once in owner construction; unresolved parameter, positive
OFFSET and non-grouped aggregate contracts remain unchanged. Derive FlowedType
from the FINAL attachment Plan after normalization and CTE wrapping, before
publication; prospective UNION metadata is not automatically an exact final
row. Preserve scalar one-column/nullability/cardinality barriers separately.
No final type error may leave an admitted alias without a plan.

The Java anchors were reread: `LogicalOperator.java:263–293` constructs source
quantifiers before their FieldValues; `:404–420` binds local quantifiers with
resolved predicates; `QueryVisitor.java:257–275,670–691` keeps constructed
operators through clause construction/derived renaming; and
`ExpressionVisitor.java:558–574` constructs the query before attaching an
existential. Go's typed pagination and the reproducer's nested WITH composition
remain explicitly Go extensions, not Java row evidence.

#### Acceptance additions and current evidence

Add entry-point checks for catalog/derived/CTE/JOIN source identity BEFORE
resolution, left-to-current ON visibility with a later alias shadow, and
unchanged retained-body pointers/preparation counts. Assert complete attachment
free sets for independent and outer-dependent scalars, nested EXISTS, CTE
capture/local shadow and unused definitions. Assert projection-only outer
references leave no residual runtime binding. Force final-type and unsupported
ON failures and verify all parent registration lists are unchanged, then build
a valid sibling through the same planner. Existing negative cases/goldens stay
unchanged. Separate compiled mutants must remove early identity assignment,
ON provenance, a scalar attachment edge, and atomic publication; these are in
addition to the still-open rebuild and each-veto mutants, not substitutes.

Current restored-source verification (no boundary implementation): full query
and embedded targets ran uncached with **2,476 RUN/PASS, zero FAIL/SKIP**
(`cte-bound-query-embedded-supplement-revision2.log`, 12.786s). This includes the
new failure-class parent plus four cases (missing body, UNION width, UNION
incompatibility, unrepresentable type), whose earlier focused run had five
RUN/PASS. SHA256 checks of every dirty Go file matched before/after both runs
in this continuation. The real-FDB counterexample rerun remains RED: parent
and both polarities, **3 RUN/FAIL, zero PASS/SKIP**, each SQL arm reporting
`42F01: table "C" does not exist`
(`cte-bound-exists-owner-restored-red.log`). The reverted experiment's new rows
were genuinely green but its 129 failed outcomes remain a migration acceptance
population, not current implementation approval. Race/determinism, mutations,
full driver/just test, unchanged-limit performance/stress, UNNEST and final
reviews/CI remain open. No commit, push, merge, new hunt or broad QSC closure.


#### Retained-body mutation proof after supplemental packet creation

Supplemental revision2 packet
`ef8291fe3c881a97ae0f3e4942cdbf68543e5fd501e525ca87ec60299a7acb12`
(71,768bytes/1,107lines) received a scoped virtual Graefe DESIGN ACK (task74).
It explicitly covers the design, not implementation, and retains the identity,
ON/definition lifetime, complete dependency, atomic registration, residual
FlowedType and unchanged-semantic acceptance requirements. The other two
tracked requests (tasks75/76) are pending. Preceding tasks70/71 returned no
verdict amid repeated websocket idle retries and were cancelled; they are not
NAKs or ACKs. No boundary implementation started.

After that packet was fixed, the existing real-FDB outer-capture regression
was extended with `FROM c o` alongside `FROM c`: a scalar Main's local SQL
alias O must not capture the already-bound O in C's defining environment.
Both arms independently require rows(7,7),(9,9). This is the execution twin of
the existing BuildScalar registration/free-set control, not a new family.
Baseline focused embedded+driver execution:13RUN/PASS,0FAIL/SKIP,2/2targets
(`cte-bound-mutation-baseline.log`,13.804s). The packet predates this test delta.

Three separate source mutations then compiled and executed that SAME13-outcome
population; none was a build failure:

| Mutant | RUN/PASS/FAIL/SKIP | Observed detector |
|---|---|---|
| Restore direct-syntax correlation veto plus its old helper | 13/7/6/0 | Both capture registration arms incorrectly become0correlated/1independent; both real-FDB arms report unbound scalar-result aliases. Local-shadow/local-column controls remain green. |
| Restore local-FROM-name veto only | 13/9/4/0 | Same-name capture registration and real-FDB `c o` fail; unaliased capture remains green. |
| Remove prepared-body reuse guard | 13/9/4/0 | Query-body reads at depths1–4 become3/7/15/31; depths2–4 exceed6/8/10. This is traversal detection, not runtime/row performance proof. |

Counts include failing parents. Exact commands, applied diffs, nonempty RUNs,
build results and hashes are retained in `cte-bound-mutants-report.json` and
`cte-mutant-{syntax-veto,local-name-veto,repeat-body-preparation}.{diff,log}`.
Each production file was restored immediately and its SHA256 verified:
`logical_predicate.go`73164d71c269cc310b3b0ce13e43ca8804f50b1e8c5d4780df9af76665b5b0e7;
`plan_visitor.go`a8d85fe8b619b2b5c4c88c45da650aa01884bacc4b0712d41a2a735726d2c0cc.
Restored focused execution:13RUN/PASS,0FAIL/SKIP
(`cte-bound-mutants-restored-green.log`). These close those three bounded
mutation obligations only; they do not prove the unimplemented EXISTS handoff.
Its early-identity/ON-origin/attachment-edge/publication mutants remain open.

`just gazelle` and `bazelisk mod tidy` completed. Gazelle added the direct ANTLR
dependency used by the counted-query embedded tests; no module changes. The
new BUILD dependency and driver test delta require current-tree execution;
prior2476/full and13/focused logs predate the BUILD change. Full driver/just test,
race/determinism, performance/stress, UNNEST, implementation and final-HEAD
reviews remain open, as does the real-FDB correlated-EXISTS42F01 failure.


### EXISTS consumer admission — supplemental revision 3 (DRAFT)

Tasks74/75 returned scoped virtual Graefe/Torvalds DESIGN ACKs on packet
ef8291fe…; task76 Codex returned a scoped DESIGN NAK. Its concrete objection is
valid: removing `lastJoinPredicateOuterOnly` without retaining consumer
eligibility loses the different outcomes of positive predicate, negated
predicate and projected uses of the SAME child. Dependency is not eligibility.
This amendment adds that missing mechanism; all revision2 ownership, identity,
attachment, cardinality, typing and proof requirements remain. No boundary
implementation before the scoped design gate closes.

**This supersedes revision2's claim that BuildExists itself validates and
publishes the returned attachment.** An expression callback sees only the child
query; the completed parent clause supplies polarity and value-use context.
The chosen contract is delayed, success-only parent publication, not passing
parse syntax or an imperative polarity flag down into query construction.

1. The lowerer's return adds `ConsumerConstraints` to
   `{Plan, JoinPredicate, KnownTruth, FlowedType, owned attachments}`. Constraints
   are semantic admission requirements, distinct from dependency and predicate
   placement. For the existing nested-middle/non-inner-conjunct restriction,
   the requirement is **positive predicate consumption only**: WHERE/ON positive
   is admitted; normalized negated predicate and projected Value uses of either
   polarity retain their current typed0A000 rejection. A reference-free
   filterable conjunct participates; a statically-true tautology does not.
   Existing known-truth/unsupported-consumer rules remain separate requirements.
   Neither better predicate placement nor fresh identities removes these
   declared admission restrictions in this repair.
2. Every prepared attachment edge retains its constraints, original owner
   frame/clause and exact result identifier. The expression callback returns
   its provisional alias/final row type and records that edge in the owning
   clause's PRIVATE construction result, not in the parent's admitted
   `subqueries`/`scalarSubqueries`/`correlatedScalarSubqueries` lists. This is a
   clause-local result builder: it cannot escape on failure and is not a
   mutable last-query slot or a competing registration authority. Source and
   scalar dependencies remain represented on these edges for graph-property
   derivation, regardless of whether consumer validation has occurred yet.
3. After the parent resolves and normalizes the entire clause predicate or
   projection Value, the owner computes each edge's use context from those
   bound nodes and its saved clause origin. Predicate polarity is taken from
   normalized existential/not-existential predicates; projected use is read
   from the Value tree, including `NotValue(ExistsValue)`. Parentheses or nested
   NOT do not get guessed from the child SQL. If an identifier has multiple
   retained uses, EVERY use must satisfy its constraints. Unknown/unhandled
   use shapes retain the existing typed rejection, not default permission.
   Capture ON use before ON-to-WHERE folding; any later transformation must
   keep the edge's consumer context/provenance. Admission facts cannot be
   discarded by predicate extraction or middle-level rewriting.
4. Validate consumer constraints at that parent completion boundary, with all
   nested edges and final attachment types available, BEFORE publishing any
   parent registrations. A failed consumer, unresolved type, source error or
   nested attachment failure discards the private clause result. A successful
   complete result is published once by the parent owner. `BuildExists` is
   therefore construction-only; adjust its interface/callers rather than
   preserving eager publication behind a rollback facade. The current
   expression callbacks at `expr/walk.go:2633,2661` are the shared construction
   sites; their owning predicate/projection/ON builders finalize admission.
   Direct programmatic construction must supply an explicit resolved consumer
   context or keep the result private until one exists; there is no implicit
   positive-consumer default to bypass validation.
5. The legacy `OuterOnlyJoinConjuncts` placement boolean and its mutable scratch
   transfer retire only after the positive/negative/projected matrix has moved
   to this edge-owned constraint and parent validation. The two translator
   declines (`cascades_translator.go:8697–8764`) are not simply deleted; their
   admission contract is migrated to the single owning validation boundary.
   No duplicate frontend/translator policy, syntax correlation veto, silent
   acceptance expansion or unconditional child rejection. The algebraic free
   set remains identical for an identical bound child across all consumers.

The proof adds one retained child/attachment graph used in positive WHERE/ON,
negated WHERE/ON and positive/negated projection contexts: distinct current
admission outcomes, identical child identity/free set/FlowedType, and no parent
registrations after each rejected use. Construct a valid sibling with the same
planner after each failure. Nested NOT, reference-free false and tautology
controls remain unchanged. Mutation must separately drop the constraint,
invert its polarity/use test, and publish before parent validation; each must
compile and fail the corresponding test. These augment, not replace, the
revision2 identity/provenance/attachment/publication proof requirements.

**Current evidence, not implementation approval:** post-Gazelle full query+
embedded2476RUN/PASS,0FAIL/SKIP,15.664s; focused retained-body driver controls
5RUN/PASS,0FAIL/SKIP,6.527s. The13-outcome retained-body mutation population ran
with race10:130RUN/PASS,0FAIL/SKIP,89.614s; source/BUILD SHA256 checks matched.
These logs predate the new consumer-control test below, and do not cover the
unimplemented boundary. Artifacts:cte-bound-post-mutants-{full-query-embedded,
driver-controls,race10}.log and current/race10 hash checks.

The existing real-FDB `TestFDB_ExistsInnerShadow` passes unchanged (one RUN/PASS,
cte-bound-consumer-existing-control.log), including its current positive and
negative/projected controls. The newly retained
`TestFDB_NestedExistsConsumerAdmission` holds one child SQL fixed across four
consumers. Its positive-row oracle is independent: t={7,9}, flags={50}, seed={1};
the middle and its nested EXISTS are nonempty, so positive consumption returns
7,9. The new test is RED, parent+4 arms, because the positive execution (also
rechecked after each declined use) fails exact attachment row typing:
`read as RECORD(S.ID:LONG?,M.ID:LONG?), declared RECORD(ID:LONG?,ID:LONG?)`.
This is an additional final-attachment type/duplicate-slot counterexample within
the active ownership repair, not permission to weaken its expected rows or
executor type checks. `exactJoinResultType`/`logicalLegFields` publish qualified
fields (`logical_result_type.go:612–662`); the runtime edge declares unqualified
fields. The producer of that runtime declaration still needs tracing; this is
not yet a complete root-cause claim. Retain and resolve it before closure.
No new hunt, executor-performance repair, full-PR approval, commit, push or merge.


### Supplemental revision 3 design gate — accepted; implementation open

All three SAME tracked virtual sessions returned scoped DESIGN ACKs for packet
`063df42aebba11a6bb8cefe6684b66173eb2e25688e977f7060cea1888b46531`
(54,832bytes/770lines), tasks78Graefe/79Torvalds/80Codex. Revision3 supersedes
eager BuildExists publication in revision2: private typed attachment edges,
edge-owned consumer constraints, parent use validation, then complete atomic
publication. The full revision2 identity/frame/property/type obligations remain.
These are packet-only design ACKs, not human/implementation/full-PR approval.
The new phase boundary may now be implemented; none is implemented yet.

The row-type counterexample is now localized before execution, not merely at
the executor check. Retained unit
`TestExistsFinalAttachmentTypeMatchesTranslatedRow` visits the actual owner-
constructed attachments: middle FlowedType is RECORD(S.ID:LONG?,M.ID:LONG?) but
its translated Reference's result is RECORD(ID:LONG?,ID:LONG?). The nested
one-column attachment agrees at RECORD(1:INT). SQL-driver control logs separately
confirm all three restricted consumers reject0A000; each then fails its fresh
positive row check, like the positive-only arm. Combined run:6RUN/6FAIL,0PASS/SKIP
(`cte-bound-consumer-attachment-types-red.log`). A fresh driver query does NOT
prove same-planner publication isolation; that remains explicit work.

Producer trace: `BuildExists` captures `ExactLogicalResultType` before returning
the ExistsValue. `logical_result_type.go:612–662` qualifies merged logical fields
via source aliases. In contrast `cascades_translator.go:8512` builds the joined
EXISTS output through `ordinal_seed.go:627–704`: ordinalJoinSeedFields uses each
resolved field's DisplayName and NewRawRecordConstructorValue preserves the
bare duplicate ID names. The declared runtime edge agrees with that translated
producer. Reconcile final attachment typing at its producer/owner boundary;
do not rename SQL labels into physical slots, relax row equality, add executor
fallbacks or falsely attribute the mismatch to missing data. The unit and7/9
row oracle remain retained. This test was added after the design packet, not
reviewed implementation proof. Original derived-EXISTS42F01 remains open too.


### Primary binding-carrier prerequisite — implemented, EXISTS boundary still open

Resumed on committed90b026077; task80 had already been collected. Reread all
three revision3 verdicts and verified packet SHA256063df42a… unchanged. Their
scope remains DESIGN only. Also read the prior race10 hash-check log: all ten
listed Go/BUILD files were unchanged for that historical run.

`TestPrimarySourceRetainsAssignedBinding` first reproduced loss of an already
assigned PRIVATE_X for primary catalog and CTE sources: both FROM constructors
emitted Binding="" while SELECT/projection/subquery scopes resolved X instead
of PRIVATE_X. The visitor and legacy adapter now carry fs/sq.bindingID into
LogicalScan and inline VALUES; existing scope builders use the same identity.
SQL alias X is preserved. The test also covers inline VALUES and checks the
resolved column's exact singleton correlation set. This does NOT implement
all-source allocation, shared deterministic identity ownership, boundQuery,
consumer validation/publication, or final EXISTS attachment typing.

Artifacts under `/var/tmp/query-grind-cast/pr785-review`:
- Initial catalog/CTE regression:3RUN/3FAIL,0PASS/SKIP in
  `cte-primary-binding-carriers-red.log`.
- Expanded regression + two retained derived-binding controls:6RUN/PASS,
  0FAIL/SKIP (`cte-primary-binding-carriers-restored.log`).
- Two independent compiled mutants, `drop-primary-carriers` and
  `drop-primary-scope-bindings`: each4RUN/4FAIL,0PASS/SKIP for the intended
  binding-loss assertion. Both applied diffs/logs and restored SHA256s are in
  `cte-primary-binding-mutants-report.json`. An initial script inventory check
  stopped on a wrong expected occurrence count, restored all source bytes, and
  was corrected from observed counts before rerunning both mutants. That
  aborted inventory check is not a semantic mutant kill.
- Focused race10:60RUN/PASS,0FAIL/SKIP (`cte-primary-binding-race10.log`).
- Full query+embedded:2481RUN/2480PASS/1FAIL/0SKIP in
  `cte-primary-binding-full-query-embedded.log`. Sole failure remains
  TestExistsFinalAttachmentTypeMatchesTranslatedRow: declared S.ID/M.ID versus
  translated ID/ID. This is NOT a green suite.
- Driver `Exists|EXISTS|ScalarCTE|InlineValues`:641RUN/633PASS/8FAIL/0SKIP
  (`cte-primary-binding-driver-controls.log`). Failures comprise the original
  derived EXISTS42F01 parent/two polarities and consumer-admission parent/four
  arms. All four positive controls still hit layout11; three restricted
  consumers still explicitly reject0A000. This is not full driver coverage.

Next action remains the accepted boundQuery-to-EXISTS migration, preserving
ON provenance, private exact typed edges, all-use consumer constraints and
success-only publication. Retain both active REDs and129 earlier migration
regressions. These transport mutants do not satisfy the required early-mint,
ON/scalar-edge or consumer/publication mutants. No assertions/goldens weakened;
no executor changes, new hunt, commit, push or merge. Full implementation,
just test, performance/stress, UNNEST and exact-HEAD review/CI gates remain open.


### Owned EXISTS producer/type — implemented sub-step; binding/admission migration open

The active row-type RED is fixed at the producer/attachment boundary, not by
renaming fields or weakening executor.layout. Read Java's retained query
Reference/existential construction (ExpressionVisitor:558–574) and retained
quantifier result construction (LogicalOperator:404–420). New logical.ExistsInput
owns the lowered Reference, a snapshot of its actual result type, and independent
scalar edges. LowerExistsInput consumes an already-bound logical plan; it does
not publish parent registrations. BuildExists now gets FlowedType from that
producer. SQL attachment consumers reuse the very same Reference; programmatic
logical-only callers still have their first lowering at translator entry.

The retained logical Plan still supplies source/placement information. Gathered
and UNNEST outer-reference rebases now update the owned relational predicate
node as well, over the SAME child References, retaining its row and scalar
edges. They do not clear the owned input or retranslate its plan. A declaration
that disagrees with an owned producer fails typed before scalar registration.
No SQL label substitution, executor/layout fallback, unknown type, or relaxed
row comparison. This is not a complete boundQuery or consumer-admission repair:
the legacy constructor/retry, late source mint, scratch JoinPredicate/constraint
flag and eager clause-publication paths still need the accepted migration.

New/strengthened retained tests assert final row equality, pointer identity of
both consumed existential References, scalar-edge ownership, private scalar
slice copies, unchanged children/result type through filter/select predicate
rebases, exact old/new free sets, and failure without input replacement. The
original positive7/9 real-FDB checks now pass; all three restricted consumers
still explicitly reject0A000. They still do not prove same-planner admission
atomicity. The original derived-EXISTS/NOT EXISTS42F01 remains RED.

Current artifacts under `/var/tmp/query-grind-cast/pr785-review`:
- `cte-owned-input-ownership-tests.log`, `cte-owned-input-pre-mutants.log` and
  `cte-owned-input-restored.log`:10RUN/PASS,0FAIL/SKIP over query, embedded and
  driver targets (four ownership/rebase outcomes, one attachment graph test,
  five consumer-matrix outcomes including its parent).
- `cte-owned-input-mutants-report.json`, `cte-owned-mutant-*.{diff,log}`:
  separately compiled logical-row-not-producer10RUN/4PASS/6FAIL;
  retranslate-owned-producer10/8/2; drop-owned-predicate-rebase10/7/3;
  drop-owned-scalar-edges10/9/1; all0SKIP. Source hashes restored after each;
  the same10 outcomes reran green. An initial unconditional-return rebase
  mutant was rejected by nogo as unreachable code, not killed semantically;
  its `*-unreachable.{diff,log}` is retained separately. The corrected compiled
  mutant is the reported10/7/3 run.
- `cte-owned-input-race10.log`: initial100RUN/70PASS/30FAIL with a fixture race.
  The new filter/select parallel cases shared one memo Reference, racing its
  lazy GetCorrelatedTo property at reference.go:1195/1240. Both accesses were
  those two cases, not SQL execution. Each case now constructs its own graph;
  no test serialization, production lock, assertion or population change.
  `cte-owned-input-race10-isolated.log`:100RUN/PASS,0FAIL/SKIP, all three targets.
- `cte-owned-input-full-current.log`: unfiltered query+embedded+logical+driver
  9235RUN/9232PASS/3FAIL/0SKIP. Only failures are the existing CTE-scope regression
  parent and its EXISTS/NOT EXISTS arms, both42F01 at driver line359. Four targets
  executed; this is NOT an all-green suite or an implementation approval.
- `cte-owned-input-full-race.log`: my ad-hoc900s override expired with9220RUN,
  9216PASS/3FAIL/0SKIP and ONE unfinished test, MetamorphicPagingAtScale. Timeout
  stack is runnable protobuf sort-continuation serialization through LIMIT,
  matching the already-recorded full-race budget issue, not a new deadlock.
  Source confirms this paging population has no EXISTS. Published eternal
  budget is3600s (.bazelrc), not900s; no repository gate changed. Task81 has
  completed/been collected: the SAME four-target full race scope at the published
  budget yields9235RUN/9232PASS/3FAIL/0SKIP with no missing outcomes or DATA RACE.
  The only failures are the CTE-scope regression parent/two arms. Paging completes
  all140checks in1878.21s; total1990.384s. All18 changed Go/BUILD hashes match
  (`cte-owned-input-full-race-published{.log,.sha256,-hashes.log}`). This is still
  RED from42F01, not a full green gate, and not performance-parity evidence.

These supersede the earlier checkpoint's open row-type counterexample ONLY.
BoundQuery/immutable defining frames, pure dependency derivation, shared early
identities, ON provenance, consumer constraints and success-only private
publication remain open, with their own mutants and same-planner proofs. No
new hunt, executor-performance work, commit, push, merge or approval claim.


### Bound-query/admission migration — implementation and restored boundary proofs

This supersedes the earlier open-migration/42F01 status (not the final review,
race, full-suite, stress or merge gates). EXISTS now binds once against the real
parent and retained CTE/derived bodies, classifies the complete logical graph,
then lowers privately. Scalar classification uses the same pure bound property;
translation supplies scalar cardinality, not correlation classification. Missing
attachment owners fail typed. Residual dependencies are derived again from the
normalized input, attachment predicate and surviving scalar edges. Independent
scalars bind surviving input predicates; projection-only dependencies disappear.
The original derived EXISTS/NOT EXISTS rows 9/7 and exact producer-row controls
are green. The middle FROM survives nested existential composition, including
an empty-middle EXISTS/NOT EXISTS real-FDB control. Child Reference identities
are retained across composition and attachment; no executor/type-check bypass.

The query-owned allocator carries early bindings through catalog/CTE/derived,
JOIN and primary UNNEST construction, keeping SQL aliases separate. ON records
retain the original predicate, existential edges and left-to-current bindings.
All-use consumer constraints are discharged in a private clause result before
success-only publication. HAVING now owns its own result rather than stealing
projection/WHERE registrations. Rejected admission/ON/resolution is followed by
a valid sibling on the same owner in retained tests. Existing restricted shapes,
projection-label guard, fan-out/gather checks and negative goldens are unchanged.

Boundary regressions exposed and fixed during migration:
- Unresolved LIMIT/OFFSET disappeared in UNION's right-branch and root/derived
  constructors. The shared atom parser now rejects remaining parameters; driver
  literal substitution is unchanged. Twelve root/nested cases retain this Go
  extension contract. Initial test run:23 RUN/14 PASS/9 FAIL, including parent;
  restored bounded pagination population:78 PASS,0 FAIL/SKIP.
- Primary UNNEST used its lexical alias while binding predicate Values. It now
  allocates its private source identity first; the regression checks two builds,
  deliberate Q$BOUND1 lexical collision, exact predicate and owner free sets.
- An early ON referring to a later-shadowed alias keeps the actual parent ID;
  its admission rule reads preserved provenance, not the generated ID's text.
- HAVING moved prior projection registrations into its aggregate; the new
  isolation regression was red before the clause-owner fix.

One investigation hypothesis was rejected: ordinary private parents must NOT be
called scope-ambiguous merely for repeating a lexical name. The unchanged driver
minted-middle control requires its existing0AF00 outcome, not0A000. The overly
broad guard was removed; the new control distinguishes that ordinary private
case from the separate UNNEST-frame lexical restriction. No existing assertion
was relaxed or golden changed.

Verification artifacts in /var/tmp/query-grind-cast/pr785-review:
- cte-clause-full-restored.log: six affected targets,11869 distinct test names,
  all green (historical pre-boundary additions).
- cte-bound-final-full.log: all six targets uncached,11908 package-scoped Go
  test/subtest outcomes,11908 PASS,0 FAIL/SKIP,218.927s; no missing outcomes.
  Counting excludes ten indented subprocess diagnostic PASS lines from the
  allocation-isolation tests, not actual tests. TestLikeMatch occurs in two
  packages. The earlier cte-migration-clause-full.log is reconciled as11546 RUN,
  11544 PASS,2 FAIL (not11554 PASS); same ten diagnostic lines explained its
  discrepancy. Scripts/details:count_go_log.py,cte-reconciled-counts.jsons.
- cte-clause-mutants-report.json: five compiled admission/publication/product
  mutants killed; original31 source/BUILD hashes restored,19 focused PASS.
- cte-boundary-mutants-report.json: seven separately compiled mutants killed:
  unresolved-pagination76 RUN/63 PASS/13 FAIL; late-primary-ID76/75/1;
  drop-early-source-ID74/72/2; drop-ON-provenance76/69/7; drop-scalar-edge76/73/3;
  stale residual-free-set76/72/4; HAVING-steals-projection76/75/1; all0 SKIP.
  Each applied diff/log and original/mutant hash is retained. All33 changed
  Go/BUILD hashes restored, followed by76 RUN/PASS,0 FAIL/SKIP. An initial
  pre-write script assertion expected a replacement string to be globally
  unique; it aborted without editing/running tests, was corrected to check the
  occurrence delta, and is NOT a mutant kill.

The six-target full-race command (task82, cte-bound-final-race.log) ended with
Bazel INTERRUPTED, exit8. Recovered logs contain11908 RUN/PASS over the six
targets, but that is NOT a successful command gate. Its cte-boundary-freeze.json
snapshot also predates the subsequent admission/helper repairs below. No
repository limit was raised. Current-source normal/race/determinism, unchanged-
limit stress/performance, implementation review and exact-HEAD review/CI remain
required; no commit/push/merge or final implementation approval is claimed here.

### Legacy migration gate repairs — live coverage and retained clause provenance

This continues the approved bound-query/admission migration, not QSC expansion
or executor/performance work. The worktree is still based on
`90b026077cb08a028cf567e423d39bca86eb2ae4`; prior reviewed freezes do not include
these repairs. The companion current-status block is at the end of `TODO.md`.
Artifacts below are in `/var/tmp/query-grind-cast/pr785-review`.

The first `just test` after migration failed five targets. The source-resolution
repair preserves the real missing-table error rather than restoring the legacy
text fallback: retained `BoundExistsSourceConformance` asserts Java and Go
42F01 for SELECT/DELETE/UPDATE EXISTS over a missing source, with valid-source
controls (12 observations in `cte-missing-source-java-final.log`). The changed
DML yamsql error expectation is based on that cross-engine proof, not on Go's
new output. Correlated UNION bodies retain their typed admission restriction;
independent UNION and a correlated query over a derived UNION remain controls.
The full javacorpus target passed in `cte-setop-javacorpus-green.log`.

The docscheck failure exposed retired compatibility functions. Nine definitions
were removed from `logical_predicate.go` and `select_parser.go`, including the
newly orphaned pre-pagination/window classifiers. Their tests now use live
binding allocation, bound-query lowering, bound scope ambiguity, error mapping,
and resolved nested FieldValue paths. No dead-helper ledger exception was added.
The migrated cardinality tests initially failed: the bound entry could erase
OVER before classifying an aggregate, and it no longer retained QUALIFY's
admission provenance. The bound constructor now runs the existing typed window
validation. Both SELECT builders preserve QUALIFY provenance on its owning
filter; correlated EXISTS rejects it before publication instead of folding a
global aggregate to TRUE. The owning-filter helper cannot cross a derived-source
boundary and fails typed if no owner exists.

The UNION right-branch coverage also exposed a live migration defect: its
post-builder threw away a successful predicate walk when no subquery attachment
was present, rebuilt under SQL aliases, and produced an orphan `T` rather than
the retained query-local source identity. It now keeps the resolved predicate.
The subquery-bearing path also combines QUALIFY rather than returning early and
losing it. `exists_with_aggregate.yaml` retains the nonempty/empty UNION controls
and the right-branch scalar/false-QUALIFY exact reproducer. All12 cases passed
against real FDB in `cte-qualify-shared-green.log`. This is the existing Go
admission policy, not a claim that Java lacks correlated aggregate support:
Java QueryVisitor.java:318-321 combines QUALIFY with the resolved predicate,
ExpressionVisitor.java:569 wraps the retained child Reference existentially,
and LogicalOperator.java:604-646 retains both UNION producers and first-leg
output labels. No C++ client behavior or wire format changed in these repairs.

The restored live-helper baseline ran107 tests/subtests under Bazel,107 PASS,
0 FAIL/SKIP (`cte-helper-mutant-baseline.log`). Six separately applied, compiled
mutants were killed (`cte-helper-mutants-report.json`): window validation,
QUALIFY provenance, QUALIFY admission, right-WHERE rebinding, scalar-QUALIFY loss,
and right-branch provenance loss. Five used the107-outcome embedded population;
the scalar-QUALIFY mutant used the real-FDB12-case scenario and returned three
rows where zero were required. All45 files in `cte-helper-freeze.json` were
restored byte-for-byte. The first script run stopped because its expected error
wording did not match the runner's actual `row set mismatch` wording; it restored
the tree, retained that log, and the corrected full six-mutant run completed.
Post-mutation full-suite evidence is recorded separately, not inferred here.

The golden changes were inspected in full before refreshing:
- `correlated_subquery_probes.yaml#21` retains the middle FROM producer in the
  nested existential product. Removing that producer violates empty-middle
  semantics; the retained driver/owned-producer regressions pin it.
- `union_join_leg_aggregate_forms.yaml#0` loses an intermediate identity
  projection in the left branch now built by PlanVisitor. Public labels and
  ordinals remain unchanged. The corresponding SimFDB `unionjoinleg` capture
  has four query/ROWS blocks and16 datum lines: one PLAN line changes, and every
  non-PLAN byte is identical (`cte-golden-shape-verdict.json`).
- The measured missing-source42F01 and six new yamsql cases are recorded without
  changing any existing successful-row expectations. Generated feature/coverage
  ledgers are regenerated from the corpus, not hand-adjusted.

Review remained open at this snapshot. The existing virtual Graefe session
subsequently ACKed all 18 supplied source/test/BUILD/golden deltas in
`cte-helper-graefe-verdict.md`; four documentation files were not byte-reviewed.
That scoped ACK predates the final three test-file repairs recorded below and is
not final-HEAD or full-PR approval. The mandated existing Torvalds and Codex
sessions both return `context window`
exhaustion (`cte-bound-implementation-{torvalds,codex}-recovered.jsonl`). No
replacement session, history reset, model substitution or approval bypass had
been performed at that snapshot. On 2026-09-17 the owner explicitly authorized
whatever review-session management is needed, including fresh sessions. The
missing implementation reviews remain gates; retaining those session IDs does
not. No merge approval is claimed.

### Legacy migration — final assertion repairs and fresh local gates

This is the same PR785 migration, based on committed
`90b026077cb08a028cf567e423d39bca86eb2ae4`, not a new hunt or an executor/performance
repair. The companion block at the end of `TODO.md` records remaining gates.
Artifacts named below are under `/var/tmp/query-grind-cast/pr785-review`.

The 47-file uncached full run (`cte-repair-full.log`) executed all 92 targets:
90 passed and two failed. Its reconciled Go population was 39,937 RUN,
39,919 PASS, 13 FAIL, five SKIP, with no missing/extra outcomes. Those failures
were repaired rather than retried away:

- `TestExplainOnlyMode_KeepsCrossDerivedPredicate` now requires the resolved
  `Filter(T1.AID#0 = T2.CID#0)`, not raw SQL spelling. Each derived key occupies
  slot zero; the owner/ordinal assertion detects a lost semantic walk.
- No-argument Cobra command tests set an explicit empty argument slice instead
  of inheriting Bazel's `--test.v` through `os.Args`. Commands under test and
  tests supplying real arguments are unchanged; no production CLI code changed.

Both repairs are revert-proven (`cte-final-assertion-mutants.json`). Removing
resolved-WHERE retention compiles and fails the EXPLAIN assertion; removing
explicit arguments compiles and fails `TestVersionCmd_Text` with
`unknown flag: --test.v`. Each narrowed mutant executes one test and fails it.
Original hashes were restored and both tests then passed. The complete affected
CLI/embedded/docscheck targets passed 3,388 Go tests/subtests, no FAIL/SKIP
(`cte-final-tests-green.log`, reconciled counts alongside it).

Fresh checks on the unchanged 50-file `cte-final-tests-freeze.json` snapshot:

| Check | Executed population | Result |
|---|---|---|
| `just test` | 92 nonstress targets, zero cached; 39,937 Go tests/subtests | Command exit 0; 39,932 PASS, five SKIP, zero FAIL |
| Seven affected targets with race detection | Seven targets, zero cached; 12,323 Go tests/subtests | Command exit 0; 12,323 PASS, zero FAIL/SKIP |
| Ten process runs of migration regressions | 22 embedded + eight driver root tests in every run; 1,430 total tests/subtests | Command exit 0; 1,430 PASS, zero FAIL/SKIP |

The normal run took 1,005.256s; race took 1,953.745s. Race uses the six earlier
migration targets (`values`, `query/logical`, `query/expr`, `query`, `embedded`,
`sqldriver`) plus `cmd/frl/internal/cmd`, with
`--@rules_go//go/config:race --test_arg=--test.v --nocache_test_results
--test_output=all` and the dedicated `/var/tmp/query-hunt-254/race` output base.
The eternal timeout remains 3,600s. The narrowed determinism command uses
`--runs_per_test=10 --nocache_test_results`; its exact filter and root-name
inventory are in `cte-final-determinism.log` and
`cte-final-determinism-selected.json`. BEP independently verifies every target's
run count and cache status; each of the 30 selected root names appears in all ten
runs. These repeated assertions do not replace the full-suite census floors.
The `cte-final-{just-test,race,determinism}-verdict.json` files verify all 50
hashes stayed unchanged through each run. Documentation updates follow that
freeze and require their own docscheck/hook verification.

The normal run is **not a clean no-skip gate**. The five skips are existing
factory opt-in sweeps: ExcludedShapeHunt, FullShapeHunt, NestedShapeHunt,
PlanDiversityHunt and PredicateEquivalenceHunt. Their reason is `sweep not
requested`, not unavailable Docker. No skip was introduced or counted as a pass;
no new hunt was started. Their non-execution remains explicit rather than being
hidden by Bazel's green target summary.

At this snapshot, the existing Torvalds and Codex sessions returned `Codex ran
out of room in the model's context window. Start a new thread or clear earlier
history before retrying.` Their implementation approvals were absent. The owner
subsequently authorized fresh sessions on 2026-09-17; session reuse is not a gate.
Graefe's scoped 18-file ACK still required the final test/documentation delta and
committed-HEAD reconfirmation at this snapshot. Fresh stress/performance, final-HEAD reviews and CI remain
separate gates; the historical release comparison is not evidence for this
migration snapshot. No merge approval is claimed.

### Legacy migration — committed stress comparison

The implementation is committed as `9b0b1042fe67975f58b9c5b1747158118d38d2c9`;
the normal pre-commit generate/lint/build/test hooks passed, with the previously
recorded source hashes intact and a clean worktree. The fresh four-run stress
comparison, complete 22-row timing table, both SHAs and follow-up planner
benchmark samples are recorded at the end of `TODO.md` under **Stress test 1M
baseline — RFC-256 bound-query migration (2026-09-17)**. It compares that
committed tree to merge-base `ed3504f7e410d8e2a4f4c46fd7b4c72fd0484869`, not to
the earlier release snapshot.

Both checkouts used the same non-full filesystem and identical Go module/SDK;
two baseline runs preceded two current runs. All four passed the existing 1M
suite, each with 24 RUN/PASS and no FAIL/SKIP/cache hit. All logged row counts
agree. Small aggregate queries remain slower in the observed end-to-end sample
(3.050x/2.917x/3.664x mean ratios for GROUP BY status/COUNT-only/SUM); printed
plans match, but that is not performance parity. The independent existing
planner benchmarks have 18 samples per side and do not establish the cause of
the end-to-end difference. No limits or executor/performance code changed.

The companion TODO block retains the exact measurements and remaining review,
no-skip and final-HEAD CI gates. These measurements do not authorize merge.

### Legacy migration — witnessed CI correlation-read race (2026-09-17)

**Status: Graefe and Torvalds design ACKs; repair implemented, verification and
implementation reviews in progress.** This is
triage of CI run `35198992145` at
`3905677ad4480c7593bdf96fdd4b8bc2158cafa7`, not a new query hunt. The companion
block at the end of `TODO.md` identifies the same open gate.
`TestPartitionSelect_ProjectedExistentialKeepsOuterCrossProduct` shares a stable
expression graph between parallel `enumerate_products`/`defer_products` cases.
Their independent rule calls race between `Reference.getCorrelatedToGuarded`'s
plain-map cache read and publication (`reference.go:1195/1240` at that SHA).
The existing `Reference.flowedType` contract and concurrent-read test explicitly
permit property reads over a shared stable reference; the fixture must not be
serialized or copied merely to suppress this failure. Memo mutation remains
single-threaded.

Java tag `4.12.11.0` supplies the reference semantics: `Reference.java:474–479`
unions member correlations into a fresh `ImmutableSet`;
`AbstractRelationalExpressionWithChildren.java:45–75` memoizes the immutable
set and excludes locally bound aliases; `Quantifier.java:102,711–712` delegates
through its memoizing supplier. Go's reference-level cache is retained because
its cross-group merging invalidation lives at that level (RFC-037 §3), not moved
onto immutable-looking quantifiers whose ranged-over group can change.

The chosen repair publishes the complete, thereafter read-only correlation map
through `atomic.Pointer`, like the existing flowed-type memo. Concurrent cold
readers may derive equivalent maps; none may observe a partially built map.
Convert each existing invalidation to an atomic nil store without changing the
invalidation sites or correlation algorithm. Resolve forwarding with the existing
`canonicalReferenceReadOnly` helper: path compression would itself write shared
topology during a getter. This does **not** authorize concurrent member insertion,
merge, pruning, or arbitrary mutation of a returned correlation set. A coarse
lock or `sync.Once` is wrong here: recursive graph traversal should not hold a
reference lock, and caches must remain invalidatable after sequential memo edits.

The regression belongs in the existing sandboxed expressions target and covers
empty/nonempty sets, transitive bound-alias exclusion, a shared DAG child, and a
forwarded receiver. It starts eight readers over up to 32 fresh graphs per
shape (stopping on failure), checks literal expected sets on cold and warm reads,
and asserts that the forwarding chain is not compressed. Retain the original parallel Cascades test
unchanged and repeat it under `-race`; also run the full expressions/Cascades
race targets, existing invalidation/merge tests, and `just test`. A race-free run
cannot certify concurrent mutation or general planner thread safety.

Local reproduction of the original CI failure: `cte-correlation-race-red.log`
and its BEP under `/var/tmp/query-grind-cast/pr785-review`; the uncached Bazel
race target compiles, runs the named test 20 times, and exits 3 with a data race.
This supersedes any reading of the earlier seven-target local race green as a
full Cascades race gate. The earlier scoped implementation approvals do not cover
this repair.

The new regression compiled and executed under Bazel with `-race`: five RUN,
five FAIL, no missing or extra outcomes (`cte-correlation-regression-red.log`,
`-counts.json`, `-bep.jsonl`). It catches both cache publication and path-compression
races. Plain `just test` also exits 3: the expressions target fails the explicit
`correlation reads compressed shared forwarding topology` assertion without
requiring the race detector. Its printed population is 357 expressions
RUN = 355 PASS + two FAIL (the parent and forwarded subtest). BEP reports 91
passing targets and one failing target; 90 results were cached, only expressions
and docscheck executed freshly. The source/test hashes remained unchanged during
both runs. An earlier `just test --test_arg=...` invocation did not run tests
because that recipe accepts no arguments; its diagnostic is retained separately
in `cte-correlation-just-test-bad-args.log`.

The existing Graefe session returned a scoped design ACK
(`cte-correlation-design-graefe-verdict.md`), requiring atomic publication of
completed non-nil maps, unchanged invalidation sites, read-only forwarding,
explicit borrowed/read-only results, and no concurrent memo mutation. The
existing Torvalds session, using `gpt-6-astra/xhigh` even with its advertised
872000-token context setting, still exits 1 before review with `Codex ran out of
room in the model's context window` (`cte-correlation-design-torvalds.jsonl`).
The owner has explicitly authorized fresh review sessions and any necessary
session management. Review prompts must be short: describe scope, repository,
SHAs, evidence paths, and the specific ask; reviewers inspect source and diffs
through their own tools rather than receiving pasted code packets. Old sessions
and findings remain available as evidence, not mandatory conversation IDs. The
missing approvals remain mandatory; session replacement needs no further owner
decision. A fresh, tool-enabled Torvalds review inspected the code and Java
source and returned DESIGN ACK (`cte-correlation-design-torvalds-fresh-verdict.md`).
The approved repair is now implemented: atomic complete-map publication, atomic
stores at the existing invalidation sites, read-only getter forwarding, and
borrowed/read-only contracts on both public getters. The original Cascades
fixture is unchanged.

`TestReferenceCorrelationCacheSequentialInvalidation` adds seven operation arms:
exploratory/final insertion, both prepared-admission lanes, absorb with growth,
absorb of a duplicate, and invalidation through a forwarded receiver. It warms
the cache before each operation, checks invalidation/recomputation and literal
expected sets, and requires previously returned maps to remain unchanged. Its
eight Go outcomes passed on the original implementation before converting the
cache representation. The first repaired-code race run then passed 440 outcomes:
20 repetitions of the focused correlation-read, sequential-invalidation,
prepared-equality, original projected-existential and merge-invariant tests
across the expressions and Cascades targets, with no missing/extra outcomes
(`cte-correlation-focused-green.log`, `-counts.json`, `-bep.jsonl`). Full targets,
mutation verification and final implementation reviews remain separate gates.

Mutation verification covers seven deliberate regressions: plain-map publication,
getter path compression, and each of the five invalidation sites. All seven edits
were observed in source, compiled, and failed the intended test/assertion; exact
source/test bytes were restored afterward (`cte-correlation-mutants.json` and
per-mutant logs/BEP/diffs). The first explicit-invalidation mutation failed nogo
SA4006 before testing because deleting the store left its canonicalized receiver
unused. That attempt is not counted as a killed mutation and is preserved in
`cte-correlation-mutants-first-attempt/`. Its corrected mutant stores the existing
cache rather than nil; it compiles and fails the forwarded-invalidation assertion.
No test expectations, race checks, parallelism, or limits were weakened.


### Legacy migration — full review findings and captured timeout repair (2026-09-17)

**Status: review repairs in progress; no full-PR implementation ACK or merge
approval.** Companion: TODO.md **RFC-256 PR785 full-review repair queue**.
The correlation repair's six hashes in `cte-correlation-final-freeze.json`
remained unchanged through the uncached full suite: 92/92 targets passed,
39,950 Go RUN = 39,945 PASS + five non-Docker opt-in SKIP, zero missing/extra
outcomes. This is not a no-skip pass. Full expressions/Cascades race targets
executed 4,026 RUN/PASS without skips; the focused 20-repeat run executed
440 RUN/PASS. Existing `FuzzSemanticEquals_Properties` ran for 15s under race:
664,376 executions and exit 0, explicitly without coverage guidance. The seven
compiled/killed semantic/race mutants and first failed-build attempt remain
recorded above. All artifacts are in `/var/tmp/query-grind-cast/pr785-review`.

Fresh gpt-6-astra/xhigh implementation reviews returned:
- Torvalds: scoped correlation-repair ACK; full PR incomplete. Retained P2:
  mapped special-key reads bypass the captured deferred error (2018 loses to
  2000), `mappedrange.go:317` at committed HEAD `3905677ad4480c7593bdf96fdd4b8bc2158cafa7`.
- C++ maintainer: NAK, full PR incomplete. Retained P1: synchronous timeout
  publication can fail the replacement incarnation while Reset drains the
  admitted old operation, `transaction.go:2834` at that HEAD.
- Independent Codex: all 367 committed changed files / 59,984 diff lines plus
  the six-file repair reviewed; NAK on lost production-lowering assertions in
  `unnest_seed_test.go:187,249` and `explode_collection_ordinal_test.go:284`.
  Fixture-supplied collection inspection must again exercise physical rebinding
  with a nonzero owner offset and assert correlation, full nested ordinal path,
  suffix spelling and frontier pin; retain a compiled mutant proving detection.
Graefe's full review is still running at this entry. These are repairs to the
retained migration review, not authorization for a new hunt or performance work.
The SQL/client CI-equivalent race gate, final committed-HEAD reviews, @claude,
CI, five opt-in omissions and measured aggregate slowdowns remain unwaived.

#### Captured synchronous timeout publication — design under review

The P1 is a lifetime-ownership gap in the existing client migration. A read has
an execution lease and passes its initial entry check. Reset retires that owner,
sets `readLife=nil`, and waits for the lease. `checkTimeout()` still reads the
old deadline, but its `failReadIncarnation()` looks up/creates the current owner;
this poisons the unpublished successor with 1031. Its `checkCancelled()` return
also samples the successor rather than the operation that detected expiry.

C++ tag **7.3.77**, not the local checkout's 7.3.75, is authoritative. Pinned
raw sources are retained in `pr785-review/fdb-7.3.77/`, fetched from
`https://raw.githubusercontent.com/apple/foundationdb/7.3.77/fdbclient/`.
`ReadYourWrites.actor.cpp:1567–1578` captures `resetPromise` by value in the
timebomb; `:2699–2727` replaces it and completes the old promise, preserving its
first terminal cause. Deadline publication and returned cause must have the
same captured owner in Go too. No wire encoding or conflict-range change.

Change `checkTimeout` to accept the operation context and acquire/borrow its
existing `opContext` lease. Inspect the captured incarnation's cause, publish an
expired deadline only with `failCapturedIncarnation`, and return that same
incarnation's first cause. Never select/create another incarnation for a
captured read; remove the now-unused current-owner failure helper. Convert its
six production callers (GRV, commit entry, OnError, two metrics paths, watch
setup) to pass their captured context. Existing direct timeout unit/port tests
pass a fresh caller context and retain their assertions. Reset still waits for
lease drain; cancellation delivery remains outside the leaf lock.

A per-transaction, lock-free test seam immediately before synchronous timeout
publication permits a deterministic schedule: stop the real timer, expire the
atomic deadline, admit a public Get, park publication, start Reset and observe
old-owner retirement while the lease is held, then release publication. The
old Get must return literal 1025 (Reset won), not 1031; the reset successor must
perform a real FDB read/commit successfully. A no-turnover control must return
1031. Cover each synchronous timeout entry path without weakening existing
error ordering, and retain a compiled revert/mutation showing the old
current-owner lookup poisons the successor. Run repeated race regression,
full client race and normal suites. Implementation starts only after the C++
maintainer and Torvalds design ACKs.


Captured-timeout design review: both fresh gpt-6-astra/xhigh C++ maintainer and
Torvalds sessions ACKed the design (`cte-timeout-design-{cpp,torvalds}-verdict.md`).
They additionally require stale/unadmitted context handling, borrowed lease
preservation, prepared-commit coverage, and timeout-first preservation. The
instrumented, otherwise unfixed implementation compiled and ran against real
FDB under race: 22 RUN = seven PASS + 15 FAIL, no missing/extra outcomes. All
seven `reset-first` route leaves return 1031 instead of 1025 and poison the
successor's real read; all seven timeout-first controls pass. The aggregate
fail count also includes their parents. Evidence: `cte-timeout-regression-red.*`
and the unchanged two-file red freeze. The captured-context implementation and
additional prepared-commit/stale-context pins are now present; repeated race,
compiled mutation and full-target verification are running/pending.

The completed Graefe full-PR review read all 367 committed files and the frozen
correlation repair, excluding later client/docs edits. It found no additional
correlation-repair defect, but returned a retained migration **P1** at
`logical_predicate.go:9003,9529`: EXISTS handoff copies directly referenced CTEs
without their defining environment. A chained `b AS (SELECT id FROM a)` inside
EXISTS can lose CTE `a`, yielding 42F01 or resolving to a same-named physical
table. This source-derived finding still requires a witnessed regression.
Required pins: chained EXISTS/NOT EXISTS, catalog-name collision and lexical
shadowing; Java preserves bound producer identity via
`LogicalOperator.withNewSharedReferenceAndAlias`. This finding follows the
active client repair in the same existing-review queue, not a new hunt.


Captured-timeout implementation verification: all eight changed client-file
hashes in `cte-timeout-fix-freeze.json` survived the focused race run (1,720
RUN/PASS over 20 repetitions) and full uncached client race target (1,790
RUN/PASS, no skip/failure/missing/extra outcomes, 213.819s). The expanded real-FDB
regression has eight routes, including prepared commit; the original red
population had seven routes. Reintroducing current-owner selection at timeout
publication compiled and killed the expanded regression: 25 RUN, 17 FAIL
(including eight reset-first leaves), eight timeout-first PASS; every failing
leaf witnesses successor poisoning. The exact fixed bytes were restored and
the full client race above ran after restoration. Artifact:
`cte-timeout-mutant-current-owner.{json,log,diff,bep.jsonl}`.

C++ maintainer and Torvalds each inspected the entire eight-file delta and
verified its hashes before/after review. Both returned **scoped implementation
ACK** (`cte-timeout-impl-{cpp,torvalds}-verdict.md`), excluding the still-open
mapped-read and SQL findings and all final-HEAD/full-PR/CI/performance gates.
Fresh normal-suite verification and independent review remain in progress.


Captured-timeout local normal gate completed on the unchanged 14-file working
snapshot (`cte-timeout-just-test-freeze.json`): 92/92 uncached targets passed,
39,979 Go RUN = 39,974 PASS + 5 existing opt-in SKIP, zero
missing/extra outcomes, 969.344s. Independent Codex also ACKed the eight-file
client repair after verifying its hashes and recounting red/green/mutation
logs (`cte-timeout-impl-codex-verdict.md`). No skips earn pass credit.

Artifact correction: this normal run reused the correlation run's BEP filename
because the old local wrapper hard-coded it. The earlier correlation BEP was
overwritten and is **not retained**. Its full stdout, exit, reconciled counts
and unchanged-file verdict remain. The new complete BEP is correctly named
`cte-timeout-just-test.bep.jsonl`; the misleading old filename was removed.
`cte-correlation-just-test-bep-status.json` records this loss. Subsequent
uncached wrappers require a unique BEP path and refuse existing destinations.
Do not use the later event stream as evidence about the earlier tree.

### Legacy migration — mapped-read deferred-entry repair (2026-09-17)

**Status: retained Torvalds P2; all three scoped implementation ACKs and local
verification recorded below; publication and no-skip gates remain open.** Companion: TODO.md
**RFC-256 PR785 mapped-read deferred-entry repair**. This is not a new hunt.
The existing `GetMappedRange` captures `opContext` but rejects special keys
before checking `op.entryErr`, so deferred invalid-atomic 2018 incorrectly loses
to special-key 2000. Pinned C++ 7.3.77 `ThreadSafeTransaction.cpp:497–511` checks
`deferredError` before calling RYW; `ReadYourWrites.actor.cpp:1785–1802` then
checks special keys before `resetPromise`. The exact precedence is **captured
deferred error → special-key rejection → lifetime/remaining guards**.

Add the captured operation's `entryErr` check immediately after `opContext` in
`GetMappedRange`. Do not move the combined `readEntryError` before special keys,
and do not load the live deferred slot: either change would violate existing
special-key/lifetime ordering or the admitted operation's ownership. No wire,
conflict-range, or mapper-processing change. The existing deferred-entry test
matrix gains regular and special-key mapped calls; focused pins retain
special-key-over-cancel/timeout, nested clean-entry preservation, and stale
entry versus replacement poison. Extend the existing tag-gated raw-libfdb_c
mapped differential with literal invalid atomic type 1, reverse/forward and
ordinary/special-key controls; its reference handle needs only a thin raw
`fdb_transaction_atomic_op` wrapper. That differential is intentionally run by
`go test -tags libfdbc`, as its existing per-PR CI lane and BUILD file specify,
not silently credited to Bazel's stub target. Retain red→green and compiled
revert proof, race repetitions and full client/normal gates.


#### Mapped-read fixture correction and first green evidence

The first focused run after adding the captured-entry guard was **not green**:
1,780 RUN = 1,720 PASS + 60 FAIL across 20 repetitions. Both replacement arms
failed their setup (and their parent failed): oversized `Set` buffers its
mutation and does not immediately populate `deferredErr`, so the intended
replacement-2103 precondition was absent. This did not exercise replacement
isolation and is not a regression kill. The original raw-C differential did
witness the actual bug independently: both poisoned/special-key directions
returned Go 2000 versus C++ 2018; the other six matrix arms passed.

The corrected replacement setup calls invalid atomic type **1** on an ordinary
system key, producing literal **2004** before op-code validation
(`ReadYourWrites.actor.cpp:2226–2235`). The fixture asserts this precondition.
2004 is distinct from both the old 2018 and special-key 2000, preserving the
replacement-isolation detector rather than weakening its expected outcome.
Production `Set` validation was not changed by this mapped-entry repair.

After correction, the focused uncached client race run passed **1,780/1,780**
outcomes across 20 repetitions, with no skips or unmatched outcomes. The raw-C
2×2×2 differential passed **27/27** outcomes (eight arms plus their parent,
three repetitions). Its oracle was the downloaded/checksummed official
**libfdb_c 7.3.77**, API 730 selected, client version
`7.3.77,3ea44ce1d9003ad095e408039e1f755c319c4dfb,fdb00b073000000`;
the installed 7.3.69 library was not used. Headers, shared-library search path
and server version were pinned together. Artifacts under the existing review
root: `cte-mapped-focused-{green,fixed}.*` (the former is the failed first
attempt), `cte-mapped-differential-{red,green}.*`,
`cte-mapped-reference-version.json`, and `cte-mapped-fix-freeze.json`.
Compiled mutants, full client race, normal suite and implementation reviews
remain separate obligations; these results are not merge approval.

#### Repair publication sequencing — one correctness fix per PR

The earlier separate-PR sequencing below is superseded by the owner's explicit
instruction to commit and push the existing PR785 branch. Keep logical commits
and normal hooks; that publication instruction does not authorize merge.
The correlation repair was recorded as local commit
`e3a03821bcb61cb551e2741e8c0f6bc0c0bc9690` (parent
`3905677ad4480c7593bdf96fdd4b8bc2158cafa7`), containing only its four Go files.
Its commit ran the ordinary generate/lint/build/test hook successfully. Main
worktree bytes were preserved when adopting it; the exact adoption inventory
is `cte-correlation-commit-adoption.json` under the review root.

Local branch `fix/pr785-correlation-cache-race` was prepared for a separate
repair PR, but was never pushed and no second PR was opened. The owner clarified
that these repairs belong on existing PR785, whose branch remains
`rfc256-cast-array-binding`. Publish the verified correlation, timeout and mapped
repairs there, in logical commits, rather than using the unused local branch.
PR785 targets master, so its normal pull-request workflows apply; the earlier
manual-dispatch plan for a hypothetical non-master repair PR is not needed.
Final-HEAD reviews, the exact CI race scopes and published merge gates still
apply. `cte-repair-sequencing.json` records the superseded split-PR plan.

The five existing non-Docker opt-in omissions and the prohibition on starting
new hunts remain a release-policy conflict, not a waived gate. Neither these
local greens nor the separate-branch preparation authorizes a merge.


#### Mapped-read compiled mutants and complete client race

The corrected 55-outcome deferred/mapped race population killed three compiled
mutations: missing captured-entry guard (42 PASS / 13 FAIL), combined
lifetime/deferred guard moved before special keys (50 PASS / five FAIL), and
live deferred-slot lookup (49 PASS / six FAIL). Each edit was observed in source,
each target built and ran, and each expected diagnostic appeared. All four
mapped-repair source hashes were restored and checked before the next run.
`cte-mapped-mutants.json`, per-mutant logs/BEP/counts, and
`cte-mapped-mutants-freeze-verdict.json` retain the evidence.

The complete uncached client race target then passed **1,805 RUN/PASS**, no
skips or missing/extra outcomes, in 210.277s. The four mapped-repair hashes
matched again after the run (`cte-mapped-full-client-race.*` and its freeze
verdict). This population also contains the uncommitted timeout repair; it is
integrated local evidence, not a claim that either repair has independently
passed published-PR gates. C++/Torvalds/independent implementation reviews and
fresh normal-suite verification remain in progress.


#### Mapped-read implementation ACKs, normal gate and publication STOP

Fresh read-only `gpt-6-astra/xhigh` C++ maintainer, Torvalds and independent
Codex sessions each returned **scoped implementation ACK** on the complete
four-file mapped delta at HEAD `e3a03821bcb61cb551e2741e8c0f6bc0c0bc9690`.
Each independently checked the frozen source hashes, C++ ordering, corrected
fixture, raw-C differential and compiled mutations. Verdicts are retained as
`cte-mapped-impl-{cpp,torvalds,codex}-verdict.md` under the review root. These
ACKs exclude the other repairs and do not approve a whole PR or merge.

Fresh `just test` completed in **986.546s**: **92/92 uncached Bazel targets**,
**39,994 RUN = 39,989 PASS + five SKIP**, no missing/extra outcomes. The thirteen
embedded subprocess diagnostic outcomes are excluded from that Go population.
The BEP's 92 unique summaries and 92 fresh passing results match the log's 92
targets. `cte-mapped-just-test.{log,exit,bep.jsonl}` and the count/target/freeze
verdicts record the run. All 18 frozen paths matched after this suite and after
the subsequent tagged lane (`cte-mapped-all-gates-freeze-verdict.json`). This
closing documentation amendment follows that freeze; it does not rewrite the
frozen run as having tested later documentation bytes.

The complete per-PR libfdbc command then ran against the pinned official 7.3.77
library/headers/server:
`go test -tags libfdbc -count=1 -timeout=30m -v ./pkg/fdbgo/libfdbc/... ./pkg/internal/fdbclient/...`.
It passed **85/85** outcomes, with no skips or unmatched outcomes, including
both the existing mapped differential and the new deferred-entry matrix.
Artifacts: `cte-mapped-full-libfdbc.{log,exit}` and `-counts.json`.

**Earlier publication STOP, superseded for commit/push only:** the owner has
explicitly requested publication on existing PR785. Merge remains unauthorized.
The normal suite's five omissions are
`TestFDB_{PlanDiversityHunt,NestedShapeHunt,PredicateEquivalenceHunt,ExcludedShapeHunt,FullShapeHunt}`,
not unavailable Docker. They call `requireSweepOptIn` in
`pkg/relational/conformance/factory/excluded_shape_hunt_fdb_test.go:37–43`,
requiring `HUNT_SEEDS`, `NEST_SEEDS`, `EQUIV_SEEDS`, `EXCL_SEEDS` and `FULL_SEEDS`
respectively. Starting those sweeps conflicts with the owner's no-new-hunt
scope; counting the skips as passes conflicts with the no-skip release rule.
Owner authorization to execute these existing opt-in sweeps for verification
is needed. No skip, expectation, golden, timeout or workload limit was changed.
The publication instruction does not waive the omissions or other retained
findings, and does not authorize starting new hunts.

The unused local correlation branch remains unpublished; the verified repairs
are to be committed and pushed through `rfc256-cast-array-binding` to PR785.
The missing production-lowering assertions and source-derived transitive-CTE
finding in the full-review queue have not been repaired by this work. There is
no final full-PR ACK, clean no-skip result or performance approval.


#### PR785 publication preparation — owner clarification

The owner explicitly requested **commit and push to existing PR785**, not a
new repair PR. The branch is `rfc256-cast-array-binding`; no second PR was
opened. Correlation commit `e3a03821bcb61cb551e2741e8c0f6bc0c0bc9690` is followed
by timeout commit `79f411aae438c481e6843d5269c8ba96e9ef330e` and mapped-read commit
`41d40f3127334715d7930cf1157fee35ec1aeb65`. The latter two were made in the clean comparison
worktree with ordinary generate/lint/build/test hooks and exact reviewed source
hashes; 92 target results passed on each commit (43 freshly executed for timeout,
39 for mapped, with the remaining target results cached). These hook results
are separate from the uncached verification ledger above. Publication does not
authorize merge or waive the remaining full-PR findings, final-HEAD review/CI,
opt-in omissions or measured performance concerns. No new hunt was started.


### PR785 CI pooled-buffer lifetime repair (2026-09-17)

CI `35232274269` at `c861d20bef44418b6f963798fe6fc11f3182b4b7` fails the client
race target. `TestBuildCommitRequest_TenantNoAlias` returns its marshal buffer
before comparing `req.Transaction.Mutations[0].Param1`. The generated decoder
uses `Reader.ReadBytes`, which returns a slice of the original body; a parallel
commit reuses/clears that pool buffer while the assertion still reads it. The
reported writer is `CommitTransactionRequest.MarshalFDBPooled`; the reader is
`transaction_concurrent_unit_test.go:242`. This is a test-owned lifetime error,
not a reason to change production marshaling or serialize parallel tests.

C++ 7.3.77 `NativeAPI.actor.cpp:6523–6563` allocates prefixed mutations/ranges
in the request arena. `ObjectSerializer.h:41–51,131–158` permits zero-copy reads
only while retaining the backing arena; `Arena.h:734–747` constructs StringRef
from that memory. Go's decoded byte slices likewise need their buffer retained
until all reads finish. Move the test's pool return into a defer immediately
after each build, retaining both buffers through the existing two-attempt
assertions, including fatal paths. Preserve every tenant-prefix/non-aliasing
assertion, loop bound and `t.Parallel`; no production/generated code changes.

Local uncached Bazel `-race` reproduction runs
`^(TestBuildCommitRequest_TenantNoAlias|TestConcurrent_ConflictReaders_NoRace)$`
with `-test.count=50`: 100 RUN, 99 PASS / one FAIL, exit 3, with the same reader
and pooled-marshal writer. Artifacts: `/var/tmp/query-grind-cast/pr785-review/ci-c861d20be/`
(`race-job.log`, `tenant-buffer-red.*`). Keep this existing regression pair;
a rerun without a fix is not remediation. Design review, repeated green,
full client/CI race scopes, normal hooks and published CI remain required.


Implementation verification for this test-only repair: the same two-function
race filter/count50 passes **100 RUN/PASS**; each named test executes 50 times.
No assertion, loop count, test parallelism or workload limit changed. Fresh
C++/Torvalds/independent `gpt-6-astra/xhigh` design and scoped implementation
reviews ACKed the one-file delta. The reviewed test-file SHA-256 is
`137b5a5db46981779d21a7e7ad7beee247787274a0ea043a97bfb83f9fa2d485`.

All three complete CI-equivalent race scopes then passed uncached, serially:

| Scope | Executed targets | RUN/PASS | Skips |
|---|---:|---:|---:|
| Cascades | 8 | 7,442 | 0 |
| Relational, excluding conformance and stress | 18 | 10,570 | 0 |
| Client, transport and facade | 5 | 2,208 | 0 |

The queried relational population is 19 targets; its sole filtered target is
`//pkg/relational/sqldriver/stress:stress_test`, excluded by the existing CI
`--test_tag_filters=-stress`, not a test skip. BEP and per-test log populations
reconcile with no missing/extra outcomes or cached results. The ten embedded
Cascades subprocess diagnostics are excluded from its Go outcome population.
The source hash remained unchanged throughout. Artifacts in the same directory:
`implementation-*-verdict.md`, `tenant-buffer-green.*`, `race-{cascades,rel,client}.*`,
`race-target-verdict.json` and `race-freeze-verdict.json`. Ordinary commit hooks
and published CI remain publication/merge gates, not implied by local ACKs.


### PR785 retained CTE producer repair — defining environments

Status: design accepted by the scoped Graefe and Torvalds reviews; the owner
requested the remaining ACKs and authorized merge only once all required ACKs exist. This is the retained Graefe P1, not a
new hunt. At `3e1b120e6322bdb23a7c8f4cc7a2932a37b2c075`, the new real-FDB
`cte_defining_environment.yaml` reproduces 13 failures among 14 SQL queries.
The exact reported catalog-collision EXISTS returns a row instead of none;
its negation loses a row. No-catalog chains fail, and scalar, lexical-shadow,
column-list and later-declaration cases expose the same missing ownership.
The one passing control does not establish that the broader route is correct.
Uncached Bazel log/BEP: `pr785-review/remaining-acks/cte-red.*` under the
existing `/var/tmp/query-grind-cast` artifact root.

Java 4.12.11.0 `QueryVisitor.java:161–181` prepares each declaration and its
column list before publishing it. `SemanticAnalyzer.java:173–190` chooses
from lexical fragments; `LogicalOperator.java:171–193` creates a new consumer
quantifier over that producer's existing Reference. An existing producer cannot
be rebound by a declaration in a later consumer. Go's raw body/name registry,
direct-name wrappers and single-name translator shadow pop do not retain that
contract. Sorting wrappers or collecting transitive names is rejected: neither
protects a physical-table lookup or an earlier producer from a later shadow.

**Chosen implementation:** retain a declaration/producer object containing its
prepared body, exact column aliases, recursive/traversal metadata, identity and
immutable defining CTE registry. Publish it only after successful preparation.
Bind SQL scan sources while their lexical owner exists: a CTE scan retains the
selected producer; a physical-table result is explicit, so it cannot later
fall through to a same-named CTE. Logical declaration envelopes and consumer
references share that producer rather than duplicating metadata authorities.
Preserve the existing programmatic constructor interface through the same
resolver, not a second planner. No parser replay, SQL-text matching, new
execution strategy or query-admission expansion.

Replace `wrapWithOuterCTEs` with those retained edges. Translation, exact
row/label typing, validation and logical free-correlation derivation must consume
the same source identity. Any lazy work runs against the producer's complete
defining registry (replacement, never an overlay of the consumer environment).
Unused declarations contribute no free bindings. Recursive self references
retain the existing seed/temp-scan machinery with producer-owned scoped bindings;
column renames remain positional. FROM reconstruction and lowering copies must
preserve resolved source identity and existing correlation aliases.

Proof: retain all 14 real-FDB cases (both EXISTS polarities, correlated and
independent, missing/colliding catalog names, lexical shadowing, captured physical
lookups, column lists and scalar handoffs). Add focused producer-identity,
defining-scope and no-repreparation assertions; revert the binding/retention
mechanism, compile and observe the same failures, then restore exact bytes.
Existing CTE/recursive/derived/scalar/EXISTS suites, uncached affected race targets,
normal hooks and before/after 1M stress checks remain required. No expectation,
assertion, golden, workload limit or skip is weakened. Full-PR reviewers evaluate
the completed repair together with the retained UNNEST test repair afterwards;
this design gate is not a full-PR or merge ACK.

Design reviews ACKed at the unchanged implementation HEAD. The promised focused
coverage also includes producer-originating correlation (referenced versus
unused declarations) and nested recursive ownership/restoration; the 14 SQL
cases alone do not establish those dimensions. Review artifacts:
`remaining-acks/cte-design-{graefe,torvalds}.{md,jsonl}`. No implementation ACK
has yet been granted.

Expanded regression population before implementation: **20 queries, 13 fail and
seven pass** against the same published `3e1b120e6` in the isolated comparison
worktree. The original 14 remain unchanged; added producer-origin correlation,
unused correlated declaration, two-consumer and nested-recursive polarity cases
pass before repair and must remain green afterward. Their rows complement, not
replace, the promised structural ownership/free-dependency assertions. Evidence:
`remaining-acks/cte-expanded-red.*`, its fixture hash and verdict JSON.

#### Retained repairs: implementation and regression evidence (2026-09-18)

The retained CTE implementation now stores a prepared producer and persistent
lexical registry, with explicit physical or producer ownership on scans. Shared
producer references remain separate from fresh consumer bindings. The full-suite
failure investigation also restored Java's scoped `RforScan`/`RforInsert` names
(`QueryVisitor.handleRecursiveNamedQuery`) and preserves typed missing-column
errors from computed projections, including their normalized reference paths.
The exact recursive computed-column reproducer continues to report `D.CC`, not a
later `D.DD`. The existential identity assertion now observes the actual logical
attachment and translated quantifier before checking the selected FlatMap;
its selected-carrier pointer, nullable type and ordinal assertions remain.

The retained UNNEST tests again invoke production lowering. They assert nonzero
root positions `[1,0]` and `[3,0]`, exact suffix spelling, correlation, frontier
pin/domain, and the supplied carrier's identity where the lowering API accepts
that carrier. These are not reads of the fixture's pre-bound collection.

Uncached real-FDB checks pass **20/20** defining-environment queries and **40/40**
quoted-identifier queries. The new plan dump preserves all **2,975** existing
entries (2,803 SELECT plus 172 DML) byte-for-byte; only the 20 new queries and
population header are added. Full affected embedded/semantic/logical/explaindiff/
docscheck targets and the full query target pass. These are local working-tree
results, not final published-head approval.

Four compiled semantic mutants were applied, executed and restored byte-exact:
loss of the UNNEST owner-window offset fails the second-window assertion (two
FAIL outcomes including its parent, among five RUN outcomes); re-resolving
already-retained scan sources fails **19/20** SQL cases; unique recursive temp
names fail both focused binding tests; swallowing computed-column failures fails
all four focused RUN outcomes. The scan-rebinding mutant is distinct from the
pre-implementation **13/20** red comparison and is not claimed to have the same
failure population. Mutation script, changed-byte hashes, full logs and verdict:
`remaining-acks/retained-mutations*`, `unnest-owner-offset-red.*`,
`cte-rebind-retained-red.*`, `recursive-bindings-red.*`, and
`computed-error-propagation-red.*`. Green evidence and golden preservation:
`cte-second-green.*`, `cte-affected-second.*`, `cte-query-second.*`,
`cte-plan-second-preservation.json` under the same artifact root.

Full-suite post-restoration execution, affected race/determinism, final stress,
normal commit hooks and exact published-head full-PR reviews remain required.
The five opt-in sweep omissions remain an explicit permission question under
the earlier no-hunts instruction; no skip or workload bound was altered.

#### Retained scan lookup race and non-publishing resolution

The first affected race run was **red**: parallel production-default and
under-EXISTS UNNEST lowering shared the same boxed scan nodes, and
`ResolveScan` wrote their captured source during a read-side gate. The ongoing
normal full-suite run was cancelled, not banked as a final green.
Java `SemanticAnalyzer.findCteMaybe` only reads the lexical fragments;
`LogicalOperator.generateAccess` creates the consumer at construction. Go now
likewise separates read-only resolution from `BindCTESources`, which seals
ownership during construction. The SQL FROM construction sites explicitly bind
before retaining/rebuilding their scans. Neither fixture serialization nor a
lock around the test masks the write.

`TestResolveScanDoesNotPublishConsumerScope` deterministically failed before
repair and now checks independent unbound lookups, retained producer identity,
and captured physical ownership. The complete four affected race targets passed
**2,804/2,804** RUN/PASS; ten repetitions of the selected ownership/CTE/UNNEST/
EXISTS tests passed **430/430**, with no skips or unmatched outcomes. Evidence:
`cte-affected-race.log` (red), `cte-resolution-red.*`,
`cte-affected-race-second.*`, `cte-race-repeat.*`, `cte-race-counts.json`.

The updated five-mutant campaign compiled and failed all five mutants, then
restored exact bytes (`retained-mutations-v3*`, `v3-*-red.*`). Ignoring retained
ownership again fails 19/20 SQL cases; reintroducing the publishing lookup fails
the deterministic test. An intermediate `v2` ownership mutant was rejected by
nogo for a nil dereference and earns **no semantic mutation credit**. The final
mutant removes the retained-source branch and compiles. The other three final
mutants retain the populations stated above. Existing translator fuzz ran for
15 seconds, **3,938,125 executions**, no failure; Bazel explicitly reported no
coverage instrumentation, so this is unguided mutation fuzzing, not a coverage
gain claim (`cte-translator-fuzz.*`). Final full suite, stress, hooks and review
still follow the restored source, not the cancelled or mutant executions.

#### Restored-source full verification (2026-09-18)

The fresh normal suite completed in **1,039.949s**: **92/92 uncached Bazel
targets passed**, with **40,022 Go RUN = 40,017 PASS + five SKIP**, no missing
or extra outcomes. Thirteen embedded subprocess diagnostic outcomes are not
counted as test results. The five omissions are the previously identified
opt-in factory sweeps, not unavailable Docker; permission remains unanswered,
so this is **not a no-skip gate**. All 51 frozen paths matched after execution.
The compiled v3 mutation campaign restored its inputs, and the full suite
subsequently exercised the restored implementation, including the new lookup
regression. Artifacts: `remaining-acks/cte-final-just-test*` and
`cte-readonly-final-freeze.json`.

Gazelle and module tidy leave those bytes unchanged. The restored union builder
also matches its entire original method body through the `unionLiftedClauses`
comment at parent `3e1b120e6`; removed legacy CTE replay/leg-enumeration helpers
are audited separately (`cte-current-deletion-audit.json`). Existing golden
entries and retained lowering assertions were not weakened.

Fresh 1M stress comparison ran **two merge-base samples, then two repaired
samples**, serialized on the same `/var/tmp` filesystem, with identical
`go.mod`. Each ran **24 RUN/PASS** (root plus 23 query cases), no skips; all
22 logged row counts match, and the remaining COUNT(*) asserts 1,000,000.
The exact trees and all timings are in TODO's **Stress test 1M baseline —
RFC-256 retained producer repair (2026-09-18)** block. Timing variation and
slower observations are retained, not averaged away into a parity claim.
No performance fix or workload relaxation was made. Evidence:
`remaining-acks/cte-readonly-stress*`.

This closing documentation follows the frozen measurements; it does not
pretend those executions read later documentation bytes. Normal hooks and
exact published-head full-PR Graefe/Torvalds/C++/Codex approvals, published
@claude LGTM, CI and the outstanding sweep-permission decision still precede
merge. The earlier scoped design ACKs do not approve implementation.

#### Published binding-boundary findings at `eea214b7d`

The full-PR sessions returned NAK and explicitly incomplete coverage, not ACKs.
The metadata entry points and cluster gates still called the mutating binder;
metadata-free derived reconstruction also sealed a child before its enclosing
WITH existed. New permanent regressions reproduced both defects: **26/26 Go
RUN outcomes failed**, with compiled tests, at published `eea214b7d` in the
isolated `cte-property-boundary` worktree. The initial attempt failed during
Gazelle tool compilation because `/tmp`'s per-user quota was exhausted and
receives no regression credit. Re-running with repository-tool TMPDIR under
`/var/tmp` compiled and exposed the actual failures. Evidence:
`remaining-acks/cte-property-boundary-{red,built-red}.*`.

The repair preserves the accepted construction/read boundary: property readers
carry a private lexical registry and row/label caches. A prepared producer uses
its retained defining registry; an unprepared constructor obtains its defining
frame from that reader's persistent registry, without publishing into the graph.
Recursive reference classification uses the same read-only source lookup.
Metadata-free nested construction must retain an unbound complete graph until
its enclosing query owns binding. No clone-based parallel planner or locking
of readers is introduced.

The same review found flattened CTE-name lookup confusing a quoted dot with a
schema qualifier, and an obsolete `unnestFallbackOrReject` helper. Declarations
now retain normalized identifier segments; lookup compares those segments when
the scan carries a parsed path. Legacy string-only constructors retain their
existing interface. Captured producer and physical ownership still take priority.
The unused helper is removed, its source descriptions corrected, and the old
RFC-142/RFC-173 accounts explicitly identified as historical.

The distinguishing FDB regression was compiled red: physical and joined `s.LA`
returned one CTE row instead of two physical rows; derived/star reconstruction
failed binding. The quoted CTE control passed. All five initial FDB cells then
passed, including an already-working qualified declaration control. Follow-up
projection coverage found the same information loss in SELECT and JOIN ON scope
construction: a quoted-dot CTE's `OWN_ID` was resolved against physical LA.
Those scope builders and the catalog normalizer now consume captured segments
and retained scan ownership, including explicit physical absence. A retained
producer lacking matching schema metadata is a tombstone, never a physical-table
fallback. The final ten-cell FDB population passes, including CTE-only projection,
qualified star, JOIN ON, and physical access beneath an unqualified CTE shadow.
Evidence: `remaining-acks/cte-qualified-path-red.*`,
`cte-qualified-declaration-before.*`, `cte-literal-projection-boundary.*`,
`cte-qualified-scope-fdb*`, and `cte-qualified-scope-fdb-green.*`.

Completing metadata-free construction also exposed dropped nested WITH bodies,
column aliases, and recursive traversal metadata. Compiled red pins now cover
those omissions; the builder retains the complete query and declaration options.
Evidence: `cte-nested-metadata-free-red.*`,
`cte-nested-alias-metadata-free-red.*`, and `cte-recursive-metadata-free-red.*`.
The Java basis is QueryVisitor.visitNamedQuery/handleRecursiveNamedQuery and
Identifier.equals/SemanticAnalyzer.findCteMaybe at tag 4.12.11.0.

The first full-suite attempt (`cte-boundaries-just-test.*`) was cancelled after
the projection-scope finding and earns no final-tree credit. Eight compiled
mutants of that earlier frozen binding repair failed and restored exact bytes
(`cte-boundaries-mutation-verdict.json`); that population predates the SELECT/ON
scope repair and does not validate those later edits. A transient case-sensitive
physical-catalog lookup introduced during that repair was caught by the existing
mixed-case/DML/correlation tests; restoring their established lookup policy made
the three affected targets pass again (`cte-scope-boundaries-affected-green.*`).
No expectations, admission guards, or goldens were weakened.

Published `eea214b7d` has seven successful CI checks, but that is not verification
of these later edits. Final-tree suite/race/mutation/hook evidence and exact
published-head full-PR approvals remain required; the five opt-in sweeps still
await permission. Companion: TODO **RFC-256 published binding-boundary findings**.

#### Binding-boundary verification before publication

The next full suite (`cte-scope-boundaries-just-test.*`) completed with
**91/92 passing targets**: docscheck alone failed because deleting the obsolete
helper shifted an RFC-238 numeric citation away from its intended mint.
All three occurrences now cite that mint's current line; the uncached docscheck
rerun passes without changing the gate or weak-cite floor. The full-run Go
population was 40,114 RUN = 40,108 PASS + one FAIL + five opt-in SKIP, with no
unmatched outcomes; 13 captured subprocess diagnostics are not test outcomes.
This red run plus a docs-only pass is not a final full-tree green.

The repaired executable files subsequently passed the affected four-target
race population (**2,886 RUN/PASS**) and focused ten-repeat race population
(**830 RUN/PASS**), with balanced outcomes. **Eleven v4 semantic mutants**
compiled, failed the intended tests, and restored exact bytes; the restored
three-target run passed **2,729 RUN/PASS**. Earlier compile/nogo-rejected mutant
attempts receive no semantic credit. Existing `FuzzTranslateToCascades` executed
**3,758,546 cases in 15 seconds**, with **no coverage guidance**. The fresh
real-FDB qualified-source rerun passed its root and all ten cases after the
case-normalization correction. Evidence under `remaining-acks/`:
`cte-scope-boundaries-{race,repeat,restored,fuzz,docs-green}.*`,
`cte-scope-boundaries-v4-mutation-verdict.json`, and `cte-boundary-final-fdb.*`.

Two baseline and two repaired 1M stress runs were serialized on the same
filesystem, all **24 RUN/PASS** with matching row populations and no skips.
Baseline was commit `ed3504f7e410d8e2a4f4c46fd7b4c72fd0484869`; repaired input
was parent `eea214b7dfc5a49a96af05d32a37f1d53afeea2c` plus the frozen 20-path
repair, Git tree `50c8315b280564fd313995d3c45828b466038913` (before this
closing documentation). TODO **RFC-256 binding-boundary verification and
stress (2026-09-18)** records both samples of every measured row, command wall
times, load and artifact paths. Slower observations are retained; this does
not establish performance parity or acceptance. No performance fix was made.

Final normal hooks/full-suite execution and published-head full-PR approvals,
@claude LGTM and CI remain required. The five restricted sweeps still need owner
permission; these measurements do not waive the no-skip or merge gates.

#### Remaining reader and USING ownership findings

At published `7483327ce1d91c14c2256740c19fbd97a3d67345`, all seven CI checks
passed. Graefe, Torvalds and C++ completed the full 389-file / 68,299-line read
and withheld approval: projection/aggregate/sort and outer-source readers still
select schema through flattened names, and USING-star expansion still emits
ambiguous name references instead of preserving duplicate output-slot identity.
Independent Codex timed out after two hours with no verdict/approval.

The repair follows the existing bound-source design: reuse checked SELECT scope
construction for expression readers and outer-source capture, then reuse the
checked visible-attribute star expander for USING. Java 4.12.11.0's
Identifier.equals and SemanticAnalyzer.findCteMaybe/expandStar are the reference;
there is no new planner, source-selection fallback or name-derived slot mapping.
The exact reproducer and active verification state live in TODO **RFC-256
remaining reader/USING ownership findings at `7483327ce`**. Source-traced findings
receive regression credit only after the retained cases compile and fail.

#### FirstOrDefault result-type contract uncovered by the reader regressions

**Design revision 2 accepted; implementation and verification in progress.**
Graefe and Torvalds ACKed the v2 design only (`remaining-acks/first-default-design-v2-{graefe,torvalds}.md`). The admitted correlated
array query `SELECT p.AID FROM s.LA p WHERE EXISTS (SELECT x FROM p.ARR x
WHERE x = 7)` fails both with and without the colliding CTE. Both compiled runs
reach `FlatMap(Scan(LA), PredicatesFilter(FirstOrDefault(PredicatesFilter(Explode))))`
and fail with `edge 0 non-nullable scalar is SQL NULL` on the empty inner arm.
Evidence: `remaining-acks/cte-reader-array-runtime-red.{log,bep.jsonl,exit}`.

Java 4.12.11.0 `RecordQueryFirstOrDefaultPlan` constructor verifies equal child
and default types after making their roots nullable, then derives its result
from both alternatives with **the default's root nullability**. Go instead
publishes the child's unmodified type and exact layout. A typed NULL default
therefore violates the scalar output contract. Do not relax the runtime binder's
non-null check or invent a scalar absence exemption to hide the incorrect type.

The repair will use the existing `DefaultOnEmpty` result-contract architecture:
validate both exact types, require the selected child's exact input layout,
publish a fresh identity output layout of the derived type, and revalidate when
rebuilding the child edge. Java's nullable-child/non-null-default quadrant is
unsound: execution returns a nonempty NULL child unchanged despite declaring
NOT NULL. Preserve that actual value semantics and the existing Go acceptance:
use the nullable union of both alternatives for the physical result contract,
exactly as DefaultOnEmpty already does. This deliberately differs from Java's
understated root-nullability metadata in that quadrant only; it changes no
stored bytes or SQL admission. Do not reject a formerly valid NULL-valued child,
replace it with the default, or weaken the non-null runtime guard. The exact physical QOV
remains the stable output carrier, as for DefaultOnEmpty; this is not an
attempt to evaluate Java's non-evaluable DerivedValue at runtime. Only proven
child lineage may cross the root-nullability boundary; no source-name fallback
or retained child windows are exported from the default alternative.

Both returned alternatives must carry the plan's declared type. Reuse and extend
the existing default-result normalization for record and one-slot scalar
transport, with exact shape checks and non-mutating rebasing of whole-record
absence. Keep matched all-NULL records distinct from empty/default records.
Do not change cardinality, continuations, skip/limit ordering or out-of-band
checkpoint/restart semantics. A nil Go default is invalid construction, not a
second untyped SQL-NULL representation; callers must supply a typed default.

Regression obligations: constructor/type/default compatibility and rebuild
checks for scalar and record inputs; default/nonempty scalar output and parent
IS NULL/IS NOT NULL evaluation; empty vs matched all-NULL record presence;
strict cardinality and consumed/checkpoint/restart continuations; the exact
real-FDB correlated-array query with and without the quoted-dot CTE; rejection
of the already out-of-envelope scalar correlated-array SUM remains unchanged.
Existing tests that construct nil defaults must use typed defaults or explicitly
assert constructor rejection, without reducing their original assertions.

Both initial design reviews withheld approval over the nullable-child/non-null
default quadrant. Revision 2 resolves it with the conservative physical type
above and adds nonempty SQL-NULL scalar/record controls; constructor/rebuild
checks assert that NULL remains admissible. A new compiled parent-predicate
control also exposed scalar filter layout binding being conditional on the
optional extra alias (`scalar-filter-alias-red.*`): both no-alias cases fail
UnboundCorrelation while the aliased controls pass. Select the scalar binder by
its declared carrier kind, independently of that optional alias. The same exact
layout and declared-edge validation still apply; no ambient fallback is added.

##### Null-extension field lineage clarification (design accepted; verification in progress)

The new retained `TestDefaultResultLineageUsesOnlyExactChildAndOutputCarriers`
exposes a second boundary defect: `TranslateNullExtendedPhaseRoot` rejects a
NOT NULL field read when the owning record becomes nullable. Its generic
`RebuildFieldValue` call requires an unchanged read-result type, and its comment
incorrectly asserts that the read cannot become nullable. Java
`FieldValue.java:145-148` instead derives read nullability from both the child
record and every field in the path; `withNewChild` at 159-161 recomputes it.
The stored field descriptor does not change, but reading that field from a
possibly absent record necessarily has nullable result type. The compiled red
is `remaining-acks/default-lineage.log` (non-null record-child cells); the
already-nullable record controls must explicitly expect nullable field reads.

Within this exact null-extension bridge only, resolve the original complete
ordinal path against the proven target carrier, retaining frontier provenance.
Require the rebuilt result to equal the original result with root nullability
widened to true. Keep the existing exact source-handle, same-row-shape and
one-way root-widening preconditions; foreign handles remain pointer-stable,
shape/nested-type drift and narrowing remain errors. Do not loosen generic
`RebuildFieldValue` or exact runtime binding. Rebuild enclosing Values
copy-on-write so their types derive from their children. Tests must cover
scalar and record outputs, nested field paths, non-null and nullable child
roots, mixed already-output/input programs, foreign same-shaped carriers,
unchanged source metadata and invalid shape/narrowing. This is a clarification
of the approved root-nullability boundary, not permission to alter stored
field nullability, ordinal identity, source windows, SQL admission or wire data.

Separately, the reader FDB cells are green after preserving the filter's
immutable admitted carrier during extraction relinking, rather than reading
its mutable child reference for the old owner. The compiled live-reference
red and restored green are `default-filter-live-relink-{red,green}.*`.
Four affected Bazel suites passed in `first-default-affected-v5.*` before the
new field-lineage test; that green does not cover this still-open boundary.

Graefe and Torvalds ACKed this design delta, not the implementation, in
`remaining-acks/default-field-lineage-design-{graefe,torvalds}.md`. The targeted
value/plan/executor regressions now pass (`default-field-lineage-green.*`),
including both empty and matched records. The ordinary field rebuild remains
strict. Full-suite/race/mutation verification still precedes implementation
approval; no earlier whole-PR approval is inferred.

#### Default-result metadata and owner-approved plan-shape refresh

The first complete `just test` run of the uncommitted default-result repair
failed three targets (`remaining-acks/default-result-full.*`), not a green
verification. The docs census/dead-helper and hand-built CTE fixture failures
were repaired and their focused tests passed (`default-full-docs-fixture-green.*`).
The remaining `TestPlanShapeGolden` difference is exactly these three entries
in the complete generated corpus dump (`default-result-plan-shapes.diff`):

- `projected_exists_over_a_derived_source.yaml#2` (CTE / WHERE EXISTS);
- `projected_exists_over_a_derived_source.yaml#5` (derived / WHERE EXISTS);
- `subquery_in.yaml#11` (correlated plus uncorrelated NOT EXISTS).

Each changes only the orientation of an INNER NestedLoopJoin and its ordered
shape children. Both corresponding yamsql scenarios passed in the full run;
this is not a new query-admission failure. Instrumenting the existing final
hash rung showed the compared orientations had identical preceding criteria
and scalar costs: cardinality `2.5e11`, CPU `1.30621313655e11` for the first
pair, and cardinality `62500`, CPU `7.233521890595822e10` for the NOT EXISTS
pair. These are **cost-model estimates**, not latency measurements. The lower
hash now selects the other orientation. The trace is
`remaining-acks/default-result-cost-trace.log`; the cost-model source was
restored byte-for-byte and is unchanged from published HEAD.

The mechanism is `stablePlanNodeHash` folding predicate/result semantic hashes,
which include the QOV's exact flowed type in `values/semantic_hash.go`.
Correctly widening FirstOrDefault's output and existential flowed types
therefore changes a cost-tied plan's final hash without changing its cost.
Java 4.12.11.0 `PlanningCostModel.java:320–326` likewise uses plan hash only
after its cost criteria tie; Go's semantic hash is not Java's numeric hash
(`QuantifiedObjectValue.java:115–123` hashes only the base tag in Java).
No cost formula, selection criterion, or semantic-hash implementation was
changed to recover a previous arbitrary orientation.

For causal isolation, the bounded replay contains all 19 queries from those
two unchanged YAML files. Restoring **only** `first_or_default.go` and
`expressions/quantifier.go` to published `7483327ce` in the otherwise-current
working tree restores every one of those 19 checked-in shapes; restoring the
repair produces precisely the three differences above. This mixed-tree
experiment is not a full published-HEAD replay. Evidence:
`default-result-published-types-shapes.*` and `default-result-cost-trace.plans.txt`.
A constructor-only rollback (`default-result-legacy-carrier-shapes.*`) left
new-type relinking in place and failed planning; it earns no isolation credit.

The permanent SQL pin is
`TestPlanHarness_ExistsDefaultTypesAndSymmetricJoinCosts`: all three exact SQL
shapes require non-null children, nullable defaults and nullable output types,
plus the nonzero, equal estimated cost of the two join orientations. It passed
four Go RUN/PASS (parent plus three cells). Restoring the two published files
compiled and failed all three cells on the incorrect non-null output, then
both files were restored byte-for-byte (`default-cost-premise-{green,red}.*`,
`default-cost-premise-restored.json`). This pin does not replace or relax the
existing exact-orientation golden sentinel, nor prove runtime equivalence of
an unexecuted constructed reverse plan.

**Owner approved refreshing these three exact shape snapshots.** Reverting
truthful metadata merely to recover the old tie-break is not a correctness
fix, and retuning the cost/hash selection to preserve those orientations is
outside the authorized work. A fresh complete dump changed exactly the three
named entries out of 2,995, preserving the header, SQL and each entry's node
population (`remaining-acks/default-result-approved-shapes.*`). The full
EXPLAIN target then passed three uncached runs, **159/159 Go RUN/PASS**,
including three executions each of `TestPlanShapeGolden` and
`TestBaselineIsDeterministic` (`default-result-approved-shapes-test.*`).
Full-suite verification follows; no broader expectation, admission, skip,
performance, push or merge waiver follows from the approval. The reader/USING code repairs now have the focused and mutation coverage
recorded below; final verification and reviews remain incomplete. See TODO
**RFC-256 default-result plan-shape refresh (owner-approved)**.

The field-lineage fuzz run completed three seeds and **7,303,410 executions in
15 seconds without coverage guidance**, with no failure
(`remaining-acks/default-field-fuzz.*`). That is bounded fuzz evidence, not a
substitute for full-suite/race/mutation verification of the final tree.

A subsequent complete uncached `just test` run against the frozen working tree
completed in **954.831 seconds: 91/92 targets passed**, with only
`TestPlanShapeGolden` failing. The nonempty Go population reconciles as
**40,213 RUN = 40,207 PASS + one FAIL + five SKIP**, with 13 captured subprocess
diagnostics excluded and no missing/extra outcomes. BEP records 92 summaries,
91 PASSED / one FAILED, zero cached results. All tracked regular-file hashes
were unchanged across the run. Both affected yamsql scenarios passed unchanged
(`projected_exists_over_a_derived_source` 6/6, `subquery_in` 13/13), as did all
four outcomes of the retained SQL/type/cost pin. The five skips are the still
restricted opt-in hunts; no waiver is inferred. Evidence:
`remaining-acks/default-cost-full.{log,exit,bep.jsonl,freeze.json,changed.json,counts.json}`.
This paragraph and the corresponding TODO result were recorded after that run;
the full-run hash claim refers to the preceding frozen tree, not these later
documentation bytes.

#### Bound-attribute USING expansion and reader failure contract

The existing accepted source-ownership design now covers the final separate
bare-star path: both `PlanVisitor` and the catalog SELECT constructor call the
same `expandBareStarFromScope` / `starColumnsFromScopeChecked` machinery.
`expandBareStarOverUsingJoins` and its two calls were removed, not patched to
invent unique lookup names. Java 4.12.11.0
`SemanticAnalyzer.java:321–368` returns visible output attributes;
`Expressions.java:164–166` filters visibility without deduplicating names;
`visitors/QueryVisitor.java:397–420` hides the particular right-hand USING
attribute. Go's checked expander already carries the corresponding resolved
ordinal Values and hidden-attribute filtering.

The driver regression retains left/right derived and CTE legs, quoted-dot
duplicate labels, unnamed literals, nested derived/CTE bodies, qualified and
mixed-star controls, and an outer-join arm. The data distinguishes slots
(`10` versus `20`), not merely duplicate labels. Explicit `d.x` remains an
ambiguous named reference (42702). The initial compiled run failed nine new
SQL cells plus the parent (25 RUN = 15 PASS + 10 FAIL); after removing the
legacy path, the full root passed 25/25.

Restoring only the catalog-entry call initially survived these driver tests.
This is recorded as missing coverage, not a killed mutant. The permanent
`TestCatalogUsingStarPublishesAttributeOrdinals` therefore drives that actual
constructor, asserting the retained owner and ordinal 0/1/2 explicitly for
duplicate, quoted and unnamed outputs. The first fixture revision used a
nonexistent Order column and assumed a binding was minted for an unambiguous
alias; those failures are not engine evidence. With a real `quantity` field
and an explicitly carried binding, the test passes, and restoring the legacy
catalog call fails all four outcomes on 42702. Independently restoring the
visitor call fails 14 of 16 focused driver outcomes; the two qualified-star
controls remain green. Both mutations compiled and restored exact source
bytes (`using-attribute-slots-*-mutant-v2.*`, `verify-using-slot-mutations.py`).

The reader adapters deliberately retain their existing nil-on-source-failure
signature; this is not a new promise to return the checked constructor's
error. They now obtain source identity from that single constructor and never
publish an incomplete prefix or substitute catalog source. Four negative unit
cells pin missing primary/join metadata, a CTE tombstone and missing catalog.
Redundant retries of the same scope builder were deleted. Four FDB diagnostic
cells pin undefined computed projection/aggregate/sort columns as 42703 and
ambiguous projection as 42702 beneath the original quoted-dot collision;
existing scalar-array rejection remains 42F00. This bounds the error-compatibility
claim rather than claiming every conceivable invalid source was exercised.

The final restored focused three-target run passed **77/77 RUN/PASS**, including
both exact-golden and determinism tests; no additional golden entry changed.
Evidence: `remaining-acks/reader-using-focused*` and the explicit artifacts in
TODO **RFC-256 reader/USING repair continuation after the approved golden
refresh**. Full-suite/race/default-result mutation/stress and implementation /
exact-published-head review remain required; the prior full-PR NAKs are not
converted into approvals by these local runs.

#### Array-constructor reconstruction after null extension

The local implementation review found an enclosing-Value contract hole:
`ARRAY[child.ID]`, declared with NOT NULL LONG elements, retained that declared
element type after the exact default-plan lineage crossing widened `child.ID`
to nullable. An absent record could therefore produce `[NULL]` under nonnullable
element metadata. The existing array-field pins do not construct an array and
do not cover this case. The correction and its regression evidence are recorded
below; the earlier full-suite green did not detect the defect.

The correction is Java's constructor reconstruction contract, not propagation
of arbitrary new types through a parent. In Java 4.12.11.0,
`AbstractArrayConstructorValue.java:155–177,213–228`, nonempty, non-ANY children
resolve their common type with `Type.maximumType`; it must equal the retained
element type, including nullability and nested shape. Only then are promotions
injected for children whose types differ modulo root nullability. Empty rebuilds
retain the original constructor; ANY retains its explicit heterogeneous
contract. The raw explicit-type constructor remains unchanged, just as Java's
`of(children, elementType)` at lines 290–295 does not validate its arguments.

Implement one checked array rebuilder under the existing atomic checked Value
reconstruction authority. The array's Value-only compatibility method and the
generic Value-only wrapper fail closed on reconstruction errors, without
returning a typed-nil interface. Expose the existing checked authority for the
mixed input/output lineage branch, so that branch propagates the same typed
error instead of discarding it. Do not relax exact field rebuilding, silently
widen the array's declared element type, add a runtime name fallback, or change
costs, hashes, admission expectations or the three approved snapshots.

Retain regressions for empty and matched FOD, strict FOD and DOE, both simple
input-only and mixed input/output expressions. Invalid NOT NULL-element arrays
must fail planning; arrays explicitly declared with nullable elements must
retain nonnull array metadata and evaluate to `[NULL]` or `[7]` (two elements in
the mixed case), then END. Both outcomes must leave source descriptors, field
reads and the original constructor unchanged. Unit coverage also pins the Java
empty/ANY/exact/numeric-promotion branches, nullability narrowing and widening,
incompatible primitives, nested-array drift, nil children and defensive copies.
Mutation evidence must restore the old element-preserving reconstruction and
observe semantic failures, not merely compilation failures.

This is the implementation-review correction within the accepted default-result
workstream, not new SQL feature admission. Companion: TODO **RFC-256 array
constructor rebuild correction**. The new design delta and final implementation
confirmation do not approve the full PR or waive its remaining gates.

Implementation of the array reconstruction correction now uses the single
checked array method from both compatibility reconstruction and the checked
Value dispatcher. The mixed default-lineage path retains its typed diagnostic.
The field mapper's array arm also returns an actual nil on failure and its
parents propagate that failure rather than publishing a partly rebuilt record.
The general Value-only wrapper retains its pre-existing unchanged-leaf behavior;
routing every leaf through the checked FieldValue arity gate was caught by
`TestWithChildren_LeafField_EmptySlice` and corrected without changing that test.

The numeric reconstruction control exposed a second real defect:
`PromoteValue` inserted for INT→LONG did not widen an `int32` carrier. It now
reuses `promoteConstant`, already used by `ConstantObjectValue`, before the
existing FLOAT row-domain normalization. Java `PromoteValue.java:76`
(`INT_TO_LONG`) is the reference. No new numeric conversion lattice, change to
`coerceNumericResult`, or changes to scalar-function/simplifier typing were made.
The retained pins require int32 minimum/maximum and Go int to produce int64,
NULL to remain NULL, and the constructed numeric array to contain int64 values.
Initial numeric fixtures used `LiteralValue`, which deliberately assigns UNKNOWN;
they now declare their intended INT/LONG types explicitly. A field-map fixture
also initially miscalled a variadic constructor; its compile failure is not
semantic-red evidence. Raw constructor behavior and its heterogeneous/nil-child
tests remain unchanged; comments no longer attribute nil-child tolerance to Java.

Array correction verification: all three values/plans/executor targets passed
uncached (5,036 RUN/PASS, 11 embedded diagnostics excluded). Five independently
applied/compiled semantic mutants, each over 56 RUN outcomes, failed as follows:
unchecked array elements 32 FAIL; field-mapper typed-nil return 3; partially
rebuilt record parent 2; omitted INT→LONG promotion 6; mixed-lineage diagnostic
loss 7. All source bytes were restored, followed by 56/56 restored RUN/PASS.
The expanded field-lineage fuzzer completed four seeds and 6,648,152 executions
in 15 seconds without coverage guidance, with no failure. Its additional axis
constructs nested arrays around widened field reads and preserves foreign-root
identity rather than asserting only array-field access.

Wider default-result mutation verification also compiled semantic reds for both
FOD construction/rebuild contracts (17 RUN each, 17 and 5 FAIL respectively),
child-only/default-only union nullability (255 RUN each, 55 and 15 FAIL), missing
matched-FOD normalization (23 FAIL), alias-gated scalar filtering (20 FAIL),
mutable-reference filter relinking (4 FAIL), missing existential widening and
shared-cache contamination (1 FAIL each), and lost absent-record presence
(4 FAIL); the last six also ran 255 outcomes each. The first harness revision
stopped before cache/presence mutations because its edit-presence assertion did
not allow an insertion retaining the old line; the complete v2 run is the
credited run. Earlier broad first-contract mutants could abort execution; v2
uses the complete finite 17-outcome contract test instead. All v2 outcomes
reconcile without missing results and every production file was restored.

A permanent direct normalization pin additionally covers FOD/strict-FOD/DOE
with present nonnull fields, present all-NULL fields and whole-record absence.
It requires a separate output row/slot slice and unchanged source type, layout
and presence. Both borrowed-row and borrowed-slot mutants compiled and failed
all ten outcomes; the unmutated test passed 10/10. This pins the nonmutation
property separately from the existing end-to-end value/presence assertions.

Evidence: `remaining-acks/default-array-*`, `verify-default-array-mutations.py`,
`default-result-mutant-v2-*`, `default-result-mutations-v2.*`,
`verify-default-result-mutations.py`, `default-row-*` and
`verify-default-row-mutations.py`. Design-delta ACKs are in
`default-array-design-{graefe,torvalds}.*`; they are not implementation approvals.
Final frozen-tree full/race/stress verification and local delta review follow;
none of these bounded runs replace the exact-published-head full-PR gates.

#### Structured promotion reproducer and expectation-approval STOP

Historical checkpoint: the owner-decision stop below is superseded by **Owner-ordered
pinned-version parent and stacked parity upgrade** at the end of this RFC. The
reproduced defect remains real; it is not repaired by accepting the split.

The array reconstruction implementation-delta reviews both returned **NAK**
for the frozen virtual tree `472bff1b40276a3f7738331c7470deb593b44de3`
(published HEAD `7483327ce1d91c14c2256740c19fbd97a3d67345` plus diff
SHA256 `dd52a2e1fd50aab77395f1ac881baa03033470c1690671f6a568d2671d5132dc`).
Both completed the 11-file / 748-line delta. The original array-nullability
finding is repaired, but a newly injected structured `PromoteValue` still
only changes metadata: its evaluator's primitive helpers do not recurse.
The independent full-local-diff Codex review did **not** complete: on resumption
there was no tracked task or matching review process, no verdict or exit
artifact, and its log ended during source inspection. The termination cause
is unknown; no approval is credited. The newer race run was cancelled after
the NAK and receives no completion credit. No stress run was started.

Before the following regression additions, that frozen tree completed uncached
`just test`: 92/92 targets, 40,299 RUN = 40,294 PASS + five restricted-hunt SKIP,
13 embedded diagnostics excluded, zero cached results, all 6,185 tracked
regular-file hashes unchanged. That is a real run over an incomplete semantic
population, not evidence against the newly reproduced bug and not a no-skips
pass. Evidence remains `remaining-acks/default-array-full.*`.

Permanent new regressions now drive the missing dimension:

- `TestArrayConstructorValue_CheckedRebuildNestedNumericCarriers`: the checked
  constructor must retain its declared element type, insert `PromoteValue`,
  and actually widen ARRAY<INT> to ARRAY<LONG>, ARRAY<LONG> to ARRAY<DOUBLE>,
  and another nested array level. Nullable elements must retain NULL and the
  empty-array control must remain empty. Source arrays are copied independently
  for the nonmutation assertion; comparing two aliases would not prove it.
- `TestPromoteValue_EvaluateRecordNumericCarriers`: protobuf record fields
  require target descriptor kinds and actual values for INT→LONG, LONG→DOUBLE,
  NULL fields, array fields and empty repeated fields. Source message bytes
  and descriptor identity must remain unchanged.

The uncached sandboxed values target compiled and ran **12 outcomes: 11 FAIL
(nine cases plus both parents), one PASS (the empty-array control)**, with no
missing or extra outcomes. Failure diffs show retained int32/int64 array
leaves and old Int32Kind/Int64Kind message fields under promoted metadata.
`just gazelle` and `bazelisk mod tidy` completed; no new test file was needed.
There was no implementation of recursive conversion at this checkpoint. The
working tree then retained these failing regression pins and was not ready to
commit or publish. The owner-ordered split below carries these exact success
assertions into the immediate successor rather than weakening them.

Java 4.12.11.0 `PromoteValue.java:353–429` constructs array-element and
record-field coercions; `MessageHelpers.java:488–529,546–599` applies them
recursively, builds a target-descriptor message, preserves absent optional
fields and treats repeated fields as present even when empty. The repair must
use those recursive promotion semantics and existing primitive operators,
not route implicit promotions through the broader explicit CAST lattice.
Go's existing nullable array-element behavior must remain admitted; Java's
null-element rejection is not permission to shrink Go's current contract.

**Historical STOP for an owner decision, subsequently resolved by the split below.**
This repair also reaches existing SQL refusal expectations outside the three
owner-approved plan-shape refreshes. The unchanged
`TestFDB_ArrayOfRecordLiteralsDescriptorOutcomes` table in
`pkg/relational/sqldriver/wrapper_hidden_child_fdb_test.go` requires the
following queries to fail with `but double in the target`:

```sql
SELECT ([(1 AS A), (2.5 AS A)] AS CH) FROM t;
SELECT ([(1 AS A), (2.5 AS B)] AS CH) FROM t;
```

The table's uncached real-FDB test ran and passed (one Go RUN/PASS; its SQL
cases are a plain loop, not Go subtests). Its checks require those error
substrings and would fail if either query answered. The failure is the same
missing record-field conversion: the stamped parent receives an unpromoted
INT message where its common type requires DOUBLE. A recursive record conversion would remove that Go refusal. Subsequent live
Java verification below shows the exact SQL also fails in Java 4.12.11.0:
this proposed SQL outcome change would be an upstream-bug workaround, not
restoration of Java's observed SQL behavior. The Go impact remains source-traced,
not a claim that an unimplemented repair has already been run. Other
refusal/representation cases in that table have not been approved for change.

The initial request to replace these two numeric-width refusal expectations
with successful rows must be read with the live-Java correction below: such a
change would deliberately go beyond Java 4.12.11.0's observed behavior. It is
not justified merely by the presence of recursive coercion helpers in Java.
The recursive ARRAY/RECORD promotion repair must assert rows, DOUBLE
metadata/carriers and source immutability. It must not introduce a record/array
rejection merely to hide an accepted promotion. The owner subsequently chose
the immediate Java-upgrade/parity successor for that repair, rather than an
upstream-bug workaround in this pinned-version PR; see the split below. That
instruction authorizes finishing and publishing this PR, not new QSC work,
performance repair, unrelated expectation changes or merge.

Artifacts under `/var/tmp/query-grind-cast/pr785-review/remaining-acks`:
`default-array-impl-{graefe,torvalds}.*`,
`default-array-impl-codex.incomplete.json`,
`structured-promotion-red.{log,exit,counts.json}`,
`structured-promotion-admission-controls.{log,exit,counts.json,bep.jsonl}`,
and `structured-promotion-{gazelle,tidy}.log`.
Companion: TODO **RFC-256 structured promotion — owner expectation decision**.

#### Live Java outcome correction for numeric record arrays

The owner's request to distinguish FRL Java from ANSI semantics exposed an
incorrect inference in the preceding repair discussion. **Java 4.12.11.0 also
fails both exact mixed-width SQL queries.** The existence of the correct
recursive coercion helper is not evidence that the SQL path calls it.

The permanent `RecordConstructorJavaProbe` spec **records Java numeric array
record promotion outcomes** executes four queries against a nonempty real-FDB
table. Both original `(1 AS A)` / `(2.5 AS A|B)` cases raise this exact Java
`IllegalArgumentException`:

```
Wrong object type used with protocol message reflection.
Field number: 1, field java type: DOUBLE, value type: java.lang.Integer
```

Replacing only `1` with `1.0` makes both controls succeed. Their exact retained
rows are `{CH: [{A: 1.0}, {A: 2.5}]}` for agreeing field names, and
`{CH: [{_0: 1.0}, {_0: 2.5}]}` for differing field names. The HTTP runner exposes
outer STRUCT metadata and JSON values, not nested JDBC primitive metadata;
those row assertions do not independently prove nested numeric carrier widths.
The required DOUBLE kind in the failure and Java's common-type source establish
the mixed-width target separately.

Root cause in the pinned Java source: `ExpressionVisitor.java:1094–1110`
`handleArray` calls `LightArrayConstructorValue.of(children)`, bypassing the
encapsulator that injects promotions. Its own TODO at lines 1098–1107 explicitly
identifies that missing promotion step. `AbstractArrayConstructorValue.java:285–287`
constructs without injecting; by contrast, the encapsulator at lines 144–176
resolves the common type and injects element promotions. Java's debug stack
confirms this actual failure path: `MessageHelpers.deepCopyMessage:292` ←
`RecordConstructorValue.deepCopyIfNeeded:216,202` ← `eval:124`. It copies the
unpromoted Integer into the common DOUBLE descriptor. This is an upstream
execution defect, not a SQL typing rule forbidding compatible numeric fields.

The first new probe incorrectly expected success and failed on its first
query; it did not run the second query or the later controls. The retained
measurement now pins the observed Java failure explicitly alongside exact
successful control rows. Both the normal and stack-traced uncached focused
runs passed one Ginkgo spec with all four probe outputs present. The other
1,441 specs were excluded by the focus filter; these runs are not full-suite
or no-skips evidence. No existing Go error expectation was changed, and no
production repair or upstream report was made in this investigation.

ANSI caveat: `[ ... ]` and `(expression AS field)` are FRL-specific syntax,
not portable standard SQL. The analogous typed ARRAY/ROW construction uses
compatible common field types and converted values. It does not require a
protobuf reflection failure. Nor does ANSI require DOUBLE for bare `2.5`:
an unsuffixed decimal literal is exact numeric, while FRL chooses DOUBLE.
Java's anonymous-field spelling `_0` is likewise not an ANSI naming guarantee.
Do not collapse intended common-type semantics, Java helper behavior, and
observed Java SQL outcomes into one parity claim.

Artifacts: `remaining-acks/structured-promotion-java.log` (initial refuted
success expectation), `structured-promotion-java-v2.*` (four-case pin),
`structured-promotion-java-traced.*` (repeat with Java stack), and
`structured-promotion-java-final-{gazelle,tidy}.log`.
Companion: TODO **RFC-256 live Java numeric record-array correction**. This
measurement alone did not authorize a shared-surface behavior change. The
subsequent owner instruction below resolves the publication stop by retaining
the pinned-version failures here and ordering an immediate upgrade/parity PR;
review and verification requirements remain.

#### Owner-ordered pinned-version parent and stacked parity upgrade

The owner instructed: finish our Go PR, accept this as broken, then open the
next PR on top to bump the Java version we map against, including all parity
work. No upstream Java PR is requested: upstream already fixed SQL array
promotion in #4171 (`851712f14364cb99d7dfd2f410ce56d7bcba4fb8`, first released
in 4.12.13.0). PR785 stays pinned to **4.12.11.0**. Its existing mixed-numeric
record-array refusal expectations and the four-case live-Java regression stay
unchanged. This instruction supersedes the preceding publication STOP, not
unrelated review findings or verification requirements.

The accepted limitation includes the same pre-existing structured evaluator
incompleteness exposed by checked reconstruction. Java 4.12.11 already has
recursive library coercion, while Go still leaves nested numeric carriers and
record descriptors unchanged beneath promoted metadata. Java's SQL construction
bypasses its working coercion. Thus matching SQL errors is **not** proof of
library parity, and this PR does not claim that recursive promotion is repaired.
No compatibility flag, rejection guard, SQL-text match or new expectation is
introduced to preserve the error.

The split was checked against the actual merge-base
`ed3504f7e410d8e2a4f4c46fd7b4c72fd0484869`, not just inferred from old helpers.
The same ten carrier/descriptor cases executed under uncached Bazel there:
12 Go outcomes = 11 FAIL (nine cases and two parents) + one PASS (empty array).
The record test is identical; the array baseline uses the old `WithChildren`
entry point and omits only assertions about the newly inserted promotion node.
Its input values, target type, runtime carrier and nonmutation assertions are
unchanged. The old unchecked rebuild already returned those same incorrect
nested carriers under the retained outer metadata. This establishes that the
measured defect predates PR785; it does not prove arbitrary old/new equivalence.

Graefe and Torvalds both ACKed this bounded architectural split after inspecting
HEAD `7483327ce1d91c14c2256740c19fbd97a3d67345` plus the frozen 40-file diff
SHA256 `4bea7c6a60f432fae792e939953a02d7f7a7c19195e38823197efce1395c3393`.
Those are **design/scope ACKs only**, not implementation or full-PR approval.

The two newly added desired-success roots travel **verbatim** with the recursive
ARRAY/RECORD implementation into the immediate stacked successor:
`TestArrayConstructorValue_CheckedRebuildNestedNumericCarriers` and
`TestPromoteValue_EvaluateRecordNumericCarriers`. They are neither skipped nor
rewritten to assert incorrect carriers; their removal from the parent's pending
changes is a PR split, not a green claim for that contract. Preserve and apply
`/var/tmp/query-grind-cast/pr785-review/owner-split/successor-tests.patch`
(SHA256 `b7384c77cfba0885ccb321f1552bbce620d474d105b3ae96c629d8b2295d9724`),
whose manifest records function/file hashes and whose application was checked.
They must be committed as tests with the successor's implementation. The existing
checked-rebuild, primitive-promotion, null-extension and SQL-failure regressions
remain in this parent. No other SQL refusal/representation expectation changes.

Fresh focused real-FDB verification executed the unchanged Go refusal table
(one Go root, SQL cases in its plain loop) and the pinned Java spec (one Ginkgo
spec, all four outputs; 1,441 other specs excluded). Both targets executed
uncached. These are focused outcome proofs, not full-suite/no-skips evidence.
Artifacts: `owner-split/{before.*,design.txt,graefe.*,torvalds.*,baseline-*,pinned-*,successor-tests.*}`
under the existing PR785 review directory. Final parent full/race/stress runs and
reviews use the newly frozen parent tree; five restricted hunts, published-head
reviews, Claude LGTM and CI are not waived. Publication is authorized; merge is
not inferred. No new QSC hunt or performance fix is authorized.

After the parent is published, verify and confirm the highest common published
Java release before changing pins. The successor must include dependency pins,
reference checkout, proto/grammar/generated changes as applicable, the upstream
behavioral delta and all required Go parity—not merely a jar version change.
Companion: TODO **RFC-256 owner-ordered parent and Java-upgrade successor**.

#### Read-lifetime cancellation-observation ordering

The first parent verification run exposed the living-document version gate;
its historical release citation now points from TODO to this RFC. The next
uncached full run passed that guard but reproduced a real client flake:
`TestGetReadVersion_ConcurrentWithCommit_RaceFree` reported
`concurrent GetReadVersion: context canceled`. It completed 92 targets with
91 passed, and 40,299 Go outcomes = 40,293 PASS + one FAIL + five restricted
SKIP. This is not covered by acceptance of the structured-promotion limitation.
Evidence: `owner-split/v2/{parent-full.*,failures.txt,verification-records.json}`.
Neither stopped run reached the sequential race/stress stages.

Before the fix, `readLifetimeError` sampled the captured incarnation's cause,
unlocked, then sampled `ctx.Err()`. Retirement could record 1025 and deliver cancellation
between those observations, leaking the internal context cancellation instead
of its FDB cause. Timeout has the same ordering hole for 1031. C++ 7.3.77
`ReadYourWrites.actor.cpp:1537–1547` races the GRV future against resetPromise;
`resetRyow`/`cancel` at 2699–2732 publish transaction_cancelled through that
promise, while `timebomb` at 1567–1574 publishes transaction_timed_out.
`ThreadSafeTransaction.cpp:419–425` checks deferred failure before dispatch.

The implemented design captures the context error **before** reading the recorded
incarnation cause, retaining cause-first return precedence. Cause publication always
precedes internal cancellation delivery, so an observed cancellation cannot be
paired with an earlier, clean cause sample. If neither has happened yet, entry
may proceed; existing downstream lifetime/completion gates retain their roles.
Do not translate at GetReadVersion's surface, change caller/deferred precedence,
borrow the replacement incarnation, relax the existing race test, or add a new
production hook. There is no wire-format/conflict-range change.

The retained deterministic real-FDB test intercepts only the observation of the
real operation context's `Err`, triggering Cancel, timeout, Reset or successful
commit reuse there, then forwarding the actual context error. This pins the
missing ordering dimension through public GetReadVersion. Caller cancellation
is a control; replacement GRV and committed-data controls prove no poisoning or
lost commit. Keep the original concurrent stress test unchanged and loop both
under race. C++/Torvalds and independent design ACKs preceded the production
edit; final implementation delta reviews include this finding and the
documentation fix. These design ACKs do not approve implementation or merge.

Verification: the six-outcome deterministic test failed before the fix and on
explicit reversion with the same four retirement failures (1025/1031 replaced by
raw context cancellation), failed parent and passing caller control. Restoring
the fix passed 50 repetitions of that test plus the unchanged concurrent test,
both normally and under `-race`: 350 RUN/PASS per uncached target execution, no
skips/missing outcomes, and matching source hashes. Each case preserves real FDB
setup/GRV, live-caller and replacement checks; commit reuse additionally verifies
persisted data through an independent transaction. Artifacts under
`owner-split/read-cancel/` include full logs/BEPs, outcome counts, mutation presence
and restored hashes. An initial proof-log postprocessor guessed non-FDB code 0;
the helper actually returns -1. Rechecking the retained log confirmed the intended
semantic failures; that postprocessor error is not test or race evidence.
The completed local full/race/stress rerun and code ACKs are recorded below;
final published-head gates remain separate.
Companion: TODO **RFC-256 read-lifetime cancellation observation flake**.

#### Watch setup must retain captured cancellation classification

The client implementation review found a second bypass after the GRV observation
repair: `WatchSetup` calls `readEntryError` and `checkCancelled`, then directly
returns `ctx.Err()`. Cancellation delivered after the preceding gates therefore
escapes as a raw context error rather than the captured incarnation's 1025/1031.
The v3 broad verification was deliberately stopped on this finding after 87
completed passing target summaries; it is not a complete full/race/stress result.
Evidence: `owner-split/v3/{cpp-delta.md,interrupted.json,parent-full.*}`.

C++ `ReadYourWrites.actor.cpp:2445–2446` returns the reset-promise error before
watch options and key checks; its watch actor at 1301–1304 preserves that typed
failure during setup, and the timebomb at 1567–1574 delivers 1031. The selected
repair replaces the raw context gate with the existing `readLifetimeError`, not
a watch-specific error translation. This preserves captured ownership, typed
cause-first entry precedence, deferred admission and caller-only cancellation.
Watch option/key/cap order, synchronous timeout publication, asynchronous watch
lifetime and persisted wire/conflict behavior are unchanged.

Extend the real-FDB cancellation-observation fixture with watch entry and watch
follow-up gates across cancel, actual timeout callback, Reset, successful commit
reuse and caller cancellation. The test-only context interposes on the first or
second error observation, preserving the actual context binding and error. Pin
literal error codes, no acquired/leaked watch slot, positive replacement GRV,
successful replacement setup under a one-watch cap and independent commit data.
The original GRV assertions and concurrent GRV stress are retained. C++, Torvalds
and independent design ACKs preceded the one-gate production repair. The expanded
fixture compiled red before that repair: 19 outcomes = 13 PASS + six FAIL (four
follow-up retirement cases and their two parents). Entry and caller controls
passed. Explicitly restoring the raw watch gate reproduced those same failures;
restoring the old cause-before-context observation compiled and failed all twelve
retirement cases across GRV entry and both watch gates, plus four parents, while
all three caller controls passed (19 outcomes = 16 FAIL + three PASS).

Restoring exact fixed bytes then passed the GRV/watch regressions and unchanged
concurrent stress at 50 repetitions normally and under `-race`: 1,000 RUN/PASS
per uncached target execution, no skips/missing outcomes. Both mutations were
verified present before testing; all four client/test source hashes matched after
restoration. Artifacts: `owner-split/watch-cancel/{red.*,records.json,complete.json,source.freeze.json,*-present.json,green-repeat.*,watch-raw-gate.*,cause-first-observation.*,race-repeat.*}`.
Completed implementation delta ACKs and full/race-package/stress verification
are recorded below. Companion: TODO **RFC-256 watch-setup cancellation classification**.

#### Accepted-parent verification completed

The preceding client findings are repaired, with code ACKs from Graefe, Torvalds,
the C++ maintainer and independent Codex at virtual tree
`71501a0b216ee0a2a52a66bea8a9cfafbcaef587` (418 changed files / 72,460 diff lines,
retained complete full-PR coverage plus final deltas). The read/watch regressions
and compiled reversion evidence remain part of the parent; accepted structured
promotion remains unresolved and assigned to the immediate parity successor.

At that frozen tree, actual uncached `just test` passed all 92 targets: 40,318 Go
RUN = 40,313 PASS + five restricted opt-in SKIP. Eight complete affected race
targets passed 19,812 RUN/PASS, no skips. Counts exclude 13/11 embedded diagnostic
outcomes respectively and reconcile every executed name. Four serialized 1M
runs (two baseline, then two repaired) passed 24 RUN/PASS each with matching
22 timed row counts and independent COUNT(*)=1,000,000. Every one of the 104
target results across these six executions was uncached, verified using BEP
summary and individual-result cache fields. All 6,185 frozen source hashes matched
afterwards. Full logs, commands, identities, counts, loads and cache reconciliation
are under `owner-split/v4/`, notably `verified-results.json`.

Stress compares merge-base commit `ed3504f7e410d8e2a4f4c46fd7b4c72fd0484869`
against published parent `7483327ce1d91c14c2256740c19fbd97a3d67345` plus the
43-path repair at the exact virtual tree above, on one filesystem with identical
SDK input and more than 222 GB free at each run start. The full two-sample timing
table lives in TODO **RFC-256 accepted parent — complete repaired-tree verification**
and `owner-split/v4/stress-rows.json`; it includes slower observations and claims
no performance parity, attribution or acceptance. No performance fix was made.

This closes the local full/race/stress rerun requirement, not the five restricted
hunts, performance acceptance, final published-SHA review/CI, Claude or merge
authority. The reviewed/tested tree predates this closing documentation, so normal
hooks and final SHA confirmation still apply. The known numeric-record-array
failures and immediate stacked Java-upgrade/parity obligation remain unchanged.

### Published-review follow-up: quoted lexical aliases in EXISTS admission

At published `6b833ba3c28b8266c00670e778f7497d07a188b7`, the retained
`BoundExistsSourceConformance` reproduction demonstrates false rejection of
quoted lowercase `"a"` versus unquoted `A`. Java 4.12.11.0 returns `[[1]]` for
both outer/inner orientations and for an early correlated ON followed by a
later `A` source; Go reports the scope-ambiguous or later-source-collision error.
The distinct-letter control succeeds on both engines. This is not authorization
to remove the existing multi-source EXISTS admission boundaries.

Java reference: `SemanticAnalyzer.normalizeString` preserves quoted case;
`Identifier.equals` compares normalized names exactly; `SemanticAnalyzer.lookup`
compares identifiers, not a second uppercase rendering. Go's semantic scope
already has that contract. `bound_exists_on.go` folds the captured lexical names
again, conflating distinct identifiers after correct binding.

**Design:** preserve normalized lexical spellings in `boundSourceNames`,
`parentLexicalNames` and the inherited-name map in `lowerBoundOn`. Leave canonical
runtime correlation names and predicate identity comparisons unchanged. The
compatibility-only `boundScopeAmbiguous` check must require BOTH the same lexical
alias and the existing parent-binding collision from the SAME parent source;
private/minted parents must not start failing merely because their display name
repeats. Do not combine independently populated lexical/binding sets: two parents
could otherwise supply different halves of a false collision. Preserve identical
quoted aliases, identical unquoted aliases, lexical aliases equal after SQL
normalization, and the existing private-parent/no-predicate/single-source guards.

The UNNEST frame check also conflates the lexical names. Its independent inner
query should be accepted for `"a"` versus `A`. A correlated multi-source child
reading the outer element still reaches the separate translator restriction
(`EXISTS with a multi-table FROM referencing the unnest element is not supported`),
just like the distinct-letter control. Retain both original correlated probes
with that exact existing error; add independent inner controls that test the
lexical fix without claiming broader correlated-UNNEST support.

Executable scope: retained dual-engine SQL probes plus unit coverage of the
source-name shapes, quoted/unquoted pairs, minted parents and later-ON visibility.
Capture the red run, then compiled semantic reversions and fixed-source runs.
Design ACKs precede the production edit; implementation/final-head reviews and
full verification remain mandatory. Artifacts:
`/var/tmp/query-grind-cast/pr785-review/owner-split/claude-followup/`.

### Published-review follow-up: shared producer classification scope

The reported `rebuildInnerInScope` cache concern is in
`pkg/relational/core/query/clustered_outer_scalar.go`, not `embedded`.
`TestClusteredCTESharedProducerConsumerOrder` reproduces it on the public logical
representation: two joined scans retain the same producer; its definition reads
outer A, while one consumer is itself aliased A. The classifier passes the whole
consumer frame as its skip set. The first scan removes only its own name before
walking the definition, then caches that rewritten producer. Reversing A/X scan
order changes whether definition-time A is seen. Initial run: three Go outcomes,
one PASS and two FAIL (root plus colliding-consumer-last). This is an actual
classification defect, not a reproduced SQL wrong-row claim.

The retained SQL probe for both scan orders and bare/quoted aliases already
returns `[[1,1],[2,NULL]]` in Go at the unchanged implementation: construction
mints distinct consumer bindings and retains the defining envelope. Java
4.12.11.0 rejects WITH inside the scalar expression with 42601; retain that
observed grammar boundary, not a manufactured Java-success expectation. Go's
existing read-side shape has an independent seeded-row oracle. No new syntax or
admission is introduced by this repair.

Java's `QueryVisitor.visitNamedQuery` constructs the body before registering the
named operator; `LogicalOperator.withNewSharedReferenceAndAlias` creates a fresh
consumer quantifier over the shared definition. `Value.translateCorrelations`
uses bound correlation identities. A consumer's current FROM bindings therefore
cannot become the definition's lexical environment.

**Design:** in the retained-scan branch, build an uncached producer's body scope
from the enclosing frame (`scope.parent`, nil-safe), not the current consumer
frame minus only this occurrence's alias. Still mask the body's own bindings and
retain all ancestor frames. An explicit LogicalCTE envelope continues preparing
its definition before Main and supplies the cached snapshot. Keep one immutable
snapshot per producer within this traversal; keying by consumer spelling would
preserve the erroneous dependence rather than fix it. Preserve recursive and
unknown-carrier declines and never mutate the shared original.

Regression scope includes both scan orders, multiple definition-time free refs,
body-local/inherited masks, distinct private consumer bindings, shared rewritten
producer identity, and unchanged originals. Compiled reversion must lose the
free references again. The existing lexical-boundary/nested-scope tests and SQL
probe remain. Design ACKs are required before this production edit; full-suite,
race, implementation review and published-head gates remain outstanding.
Artifacts share `owner-split/claude-followup/` with the quoted-alias repair.

#### Follow-up implementation evidence before full verification

Both bounded designs received Graefe/Torvalds ACKs in their existing read-only
`gpt-6-astra/xhigh` sessions. The quoted-name fix is confined to lexical admission
comparisons; runtime canonicalization is unchanged. The CTE fix removes the whole
consumer frame only on an uncached retained-producer visit, preserving ancestors,
body locals and explicit-envelope cache reuse.

- Restored quoted-name scope: 50 Go RUN/PASS. Three selected Ginkgo specs pass,
  including 20 named quoted-alias engine outcomes. Unordered multi-row controls
  use exact multiset comparison; the separately retained ordered variants pin
  Java's existing UnableToPlanException and Go's in-memory-sort extension.
- Seven quoted-name semantic mutants compile and fail retained assertions:
  full revert, binding-only classification, parent/source/ON lexical folds,
  EqualFold in place of runtime uppercase identity, and independently populated
  parent sets. Fixed bytes were restored and rerun green. The dotless-i/Kelvin
  unit controls distinguish canonical uppercase from Unicode EqualFold.
- Restored clustered-CTE scope: 12 Go RUN/PASS, including both consumer orders,
  private bindings, two free refs, ancestor/body-local masks, nil entry and
  explicit-envelope cache reuse. Four final mutants compile and fail: original
  exclusion, dropped ancestors, dropped body masks and ignored cache. The first
  cache mutant was rejected by nogo and earns no semantic credit; corrected v2
  ran and failed. The four-case SQL probe passes its independent Go row oracle
  and explicitly pins Java's 42601 scalar-WITH grammar boundary.
- An early combined-target command passed Ginkgo-only flags to the plain Go
  target, so that target ran no tests. Its separate corrected run supplies the
  unit red evidence. An initial fix spelling triggered SA6005; the final code
  caches the exact uppercase identity instead of changing it to EqualFold.

RFC-142 now explicitly reconciles the superseded AS==AT rejection and UNNEST
shadowing contract; remaining deleted-guard references are marked historical.
Transport pooling and projection-boundary comments describe the actual current
mechanisms. No client code or wire behavior changes in this follow-up.

The PR body's three-snapshot statement now explicitly refers to the 43-file
`6b833ba3c` repair, not the entire PR. Against `ed3504f7e4`, published `6b833ba3c`
changes ten `.golden` files (the accumulated plan-shape work and nine simulation
output-label files). This follow-up changes none. The plan-shape digest remains
`8f8b13c16a055347c698230810cd22a2dcf05d73bdd661f68a937edf67538b14`.
Full verification, milestone delta reviews, publication and new-head CI/review
are not claimed by these focused results.

### Final follow-up verification and owner merge authorization

On 2026-09-18 the owner explicitly requested merging PR #785. This supersedes
all earlier publication-only/no-merge authorization checkpoints in this RFC and
TODO; it is not a claim that unrun tests passed. The accepted pinned-parent
structured-promotion limitation and immediate full-parity Java successor remain.
The common 4.14.2.0 candidate still needs the requested version confirmation;
there are no upgrade-pin, Java-checkout, golden or upstream-PR changes here.

Graefe, Torvalds, C++ and independent Codex ACKed the complete 17-path follow-up
at `285739a5f8d1837cee2523d57aa4426f03f51d82`, retaining prior full-PR coverage.
The full 92-target execution then found one stale-citation regression: 40,362 Go
RUN = 40,356 PASS + one FAIL + five restricted opt-in SKIP. RFC-238's moved
references now point at the field-name assignments; the existing test and census
floor are unchanged. Three focused uncached citation/census tests pass. The
failed full run is retained, not relabeled green. The first subsequent hook
rejected the closing TODO's future-version citation under the current-pin-only
living-doc contract. The candidate remains recorded in this RFC; TODO now links
here rather than naming a non-current pin. No test or census assertion changed;
that failed hook is retained under `claude-followup/publish/attempt1/`.

The corrected tree `33744cee59417c1d8bf341b3faf0534e7cce9b8b`, over published
`6b833ba3c28b8266c00670e778f7497d07a188b7`, differs from the reviewed tree only
by those documentation citations. Five complete affected race targets pass
9,600 RUN/PASS with no skips or unmatched outcomes. Two baseline and two current
million-row samples pass 24 RUN/PASS each; all 22 timed row populations agree and
COUNT(*) independently asserts 1,000,000. All nine race/stress target results are
uncached by both BEP summary and per-result cache fields, and all 6,186 source
hashes stayed fixed. Baseline `ed3504f7e410d8e2a4f4c46fd7b4c72fd0484869` is
the 2026-09-18 merge-base, not moving master. The checkouts share filesystem and
SDK input, with serialized samples and no concurrent local test jobs. Full
individual timings, loads and slower observations are in TODO **PR #785 final
follow-up verification and owner merge authorization** and the matching
`owner-split/claude-followup/v2/stress-rows.json`. No performance parity or
causal attribution is asserted and no performance work was added.

Closing evidence still precedes the unbypassed commit hook and exact published
SHA; those full-suite/CI/delta-review gates and completion of Claude's unread
scope are not pre-claimed. The five restricted factory hunts remain unrun and
unapproved; ordinary skips are not Docker checks or passing coverage. The owner
merge request does not manufacture new paging140/eternal3600 or transaction,
watch and retry evidence. Runtime records and hashes are in
`owner-split/claude-followup/v2/verified-runtime-results.json`; full failed-run,
mutation and focused-regression evidence remains in the parent directory.
