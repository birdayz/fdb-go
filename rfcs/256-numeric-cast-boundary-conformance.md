# RFC-256: Numeric CAST boundary conformance

Status: Implemented; local release gates passed. The amendments and evidence
ledger below supersede earlier in-progress entries. PR review and CI approval
are still required before merge.

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
