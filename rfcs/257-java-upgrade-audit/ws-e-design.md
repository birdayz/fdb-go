# RFC-257 WS-E — shared scalar/SQL semantics and statement execution

Status: design v12, awaiting actual milestone gates (Graefe, Torvalds, storage/wire);
implementation has not started. v12 answers the three v11 NAKs (`ws-e-design-review-v11/`),
each change marked v12 at its site. Most answers are MEASURED, on both engines where the
question was about the target:
- The compensation guard (4.1(b)). v11's "the probe feeds ANY alias" admitted the shape
  RFC-150 refuses, and its only pin, PIN 1, is shown not to reach the widened branch. v12
  admits an EXPLODE alias only, registered where InComparisonToExplodeRule makes it. On
  the prototype it builds #7 and REFUSES the RFC-150 shape GUARD admitted.
- The planning cost (4.1(b)). v11's 54F02 on the and-idempotent pair is root-caused:
  Go's `ImplementInJoinRule` adds a Preserve ordering and repeats its memoization once per
  inner plan, where the target does neither. Aligned, four IN lists cost 44,099 tasks
  where they cost 460,629. The target's own task counts are now measured (a new
  TASK-COUNT mode of `planRuleTrace`): 3,835, 19,366 and 117,131 for two to four lists.
  Five lists pass Go's 150,000-task tripwire and are declared.
- NaN (4.1(b)). The producer census is complete, and MIN and MAX return a NaN operand's
  own bits in the target (measured). The CAST's grammar is measured over 22 spellings
  (11 disagree) and ported whole. The binder's four callers each get an arm (the
  aggregate and vector scans keep the refusal, declared). Section 8's stale NaN
  declaration is rewritten. A Go-written NaN in a target store with a UNIQUE index is
  now pinned to today's outcome.
- The DATE repair is scoped to midnight rows, batched, and preceded by a duplicate
  check (4.3).
- The type-merging operators (CASE, IF, IFNULL and the variadics) get the DATE-to-
  TIMESTAMP promotion.
- The remaining `time.Time` sites are each decided.
- The multi-binding product. It is measured on the target (5-by-5 refused), computed
  saturating, its signed-zero and resume boundaries stated.
- The covering fix is restated in F-7's terms, and step (5) re-measures on F-7.
- The ARRAY<RECORD> refusal is kept.
- The tie pins are split by deciding phase.
- The RFC-182 crossing gates step (4) by name.
- The step dependencies are stated.
- Section 0 and the evidence are regenerated for the fourteen WS-E Describes.
The v11 paragraph follows. Its guard change, its "carries whatever bits its producer gave
it", its budget framing, its repair and its "holds of the code" are superseded as above.

v11 answered the three v10 NAKs
(`ws-e-design-review-v10/`). #7, the two-source IN-union, is DERIVED, not a hypothesis:
the compensation of the partition-built lower is refused by Go's own
`compensationResidualCorrelationSafe` (RFC-150's defense-in-depth guard), because its
residual references an explode alias its probe does not feed; narrowed to refuse only an
uncorrelated probe's residual, the hazard it was written for, the prototype plans the
target's `InUnion(PredicatesFilter(IndexScan(IDX_VAL, [=])), bindings=2)`, changing that
one corpus entry and no test, and removing the guard outright is measured not to be the
change (a nine-conjunct OR exhausts the planner budget). Every prototype number is from a
named record run by one script whose configurations and source hashes are recorded
(`wse12-proto.sh`), seed 17's counts included; v10's were untraceable. The multi-binding
IN-union executor merges only, checks the product against the plan's maximum first (24
run, 25 refused), and lands after WS-F's F-7, whose merging executor it extends. The
partition arm and the guard change are declared in DIVERGENCES.md (PENDING) and named at
their rule sites. NaN: Go's CAST writes `math.NaN()`'s bits, a write divergence fixed in
step (6) (PENDING in DIVERGENCES.md), MEASURED by a new oracle round with the target's
stored bits for the CAST and for a division; a NaN known only at run time is handled by a
binder KEY FILTER below the continuation (two NaN ranges, a per-entry filter for later
components and tails, filtered entries counted toward limits). Temporal: the promotion's
sites are named (the comparison constructor, the IN items' promotion, the explode's
inheritance, a `compilePromotion` arm), the `time.Time` arms of the value CAST and the
date-part functions are deleted, and the DATE-column consequences of storing a bound
time.Time's TIMESTAMP text are declared with the repair `SET d = CAST(d AS DATE)`.
5.4: the RFC-182 union leg is handed to WS-F as an explicit item, the negative control
ranks the members with the two comparators directly, the tie text names REWRITING's
deciding rungs, and WS-F's dense level map is recorded as touching the level rung. The
v10 paragraph follows; its #7 hypothesis, its "concatenated without them" and its
single-bit-pattern NaN premise are superseded as above.

v10 answered the three v9 NAKs
(`ws-e-design-review-v9/`). WHO DECIDES a fold is now the target's own mechanism as
WS-F ports it: the physical REWRITING prune to one member per group (ws-f-design.md 2.3,
D7 to D12), so WS-E adds no prune; v9's provenance-scoped boundary prune is withdrawn,
since provenance is lost when a yield deduplicates against a member already in the
group (5.4(c)). The simplification rule therefore lands after WS-F step 7's option is
the default, which also supplies the conditional chain's task and progress (D1, D4),
the staleness gate (D5), the deletion of the standalone DecorrelateValuesRule
registrations, and the move of NormalizePredicatesRule WITH PredicateToLogicalUnionRule
to PLANNING (D12). The nested IN lists (4.1(b)) are derived from a prototype memo dump:
the target's member is never BUILT in Go, because Go's PartitionSelectRule puts a
spanning predicate in the upper; the port scopes Java's "can do in lower" arm to
partitions whose every upper is an explode (the full arm broke 158 tests; the explode-
only-predicate variant took a rowdiff seed to 382,371 tasks) and adds the executor's
multi-binding IN-union; the two-source IN-union (#7) is still not built with both, and
its derivation is labelled a HYPOTHESIS. The NaN equality over an index cites and
revises RFC-208: two NaN blocks from the existing builder, the NaN component the last
bound one with later comparisons residual, never physically fixed, no UNIQUE proof,
and declared as a Go extension. The temporal item (4.3) is narrowed to VALUES: a new
oracle round (Describe "WS-E target oracle v10", 12 pins) measures that the target has
no TIMESTAMP type either and no temporal CAST, and that a Go DATE or TIMESTAMP column
reads back as STRING, so v9's column claims (canonicalization by column type, the
`SET d = d` repair, the comparison lane, the stored-text table) are withdrawn; one
temporal parser with a year domain replaces three, and a bound time.Time stores its
canonical TIMESTAMP text. Smaller fixes: the census matcher covers `Eval`, the dedup's
`=` covers composites, the cost of a comparand IN source and the signed-zero IN-union's
continuation are stated, and every unmeasured claim is labelled SOURCE. The v9
paragraph follows; its prune and its temporal column claims are superseded as above.

v9 answered the three v8 NAKs
(`ws-e-design-review-v8/`). The boundary prune is scoped by PROVENANCE, a source member
and the ported rule's yields over it, so a Go-only REWRITING rule's variant (the CNF
NormalizePredicatesRule yields, which Go runs in REWRITING and the target in PLANNING
only) is never pruned for it, and NormalizePredicatesRule moves to PLANNING as the
target has it; the prune runs at both REWRITING-to-PLANNING crossings, the RFC-182 union
leg's included; and only a primary-key or UNIQUE conjunct is a rescue (measured: a
non-unique index is not). The rule is registered as the target schedules it, inside the
decorrelate-then-simplify conditional chain WS-F ports, as two instances of one type
under the target's name. A new oracle round (Describe "WS-E target oracle v9", 27 pins)
measures: two IN lists over a two-column index are NESTED IN-joins over one probe in the
target, as in Go today, so the explode port must keep that shape instead of the
prototype's FlatMap (v8 had moved the pin), and two IN lists under an ordering are one
two-source IN-union; the fold beside a UNIQUE index and inside a UNION ALL leg annuls in
the target where Go raises 22012; IN lists over signed zeros and NaN, per row and
indexed, which fixes the dedup's equality to the comparison's `=` (Go's declared IEEE
zeros, and NaN equal to NaN) and finds an indexed NaN equality that Go refuses and the
target answers, now mapped to the two NaN key ranges; and that the target has no DATE
type at all, so the temporal lane is Go's own extension. The census covers predicate
`Eval(nil)` too. The temporal section completes the assignment: `compilePromotion`'s
DATE and TIMESTAMP arms, canonicalization in the write converters by column type (so
`SET d = d` repairs), ONE parser with a stated domain, the TIMESTAMP-to-DATE edge
assignment-only, a NULL comparand NULL-strict, GREATEST and LEAST compared as instants,
and the lane admitted by the comparand's ROLE, an IN only when every item is
uncorrelated. The IN-union plan folds its source, not a pre-evaluated slice, into its
identity. Smaller fixes: the explode rule's two instances under one name, the
VerifyException's harness SQLSTATE, the teardown test asserting context.Canceled, the
store-open comment, and the stale labels and citations.

v8 answered the three v7 NAKs
(`ws-e-design-review-v7/`). WHO DECIDES a fold is now where the target decides it: at
the REWRITING-to-PLANNING boundary Go prunes each class of PREDICATE VARIANTS (members
of one group that differ only in their predicates) to its REWRITING winner, so an
access path can no longer rescue the unfolded member (5.4(c)). A new oracle round
(Describe "WS-E target oracle v8", 31 pins) measures the target on primary-key,
IN-list and indexed conjuncts beside annulling, reducing and tie folds: with the
target's own type folds it answers no rows (annulment) and the bare probe's rows
(reduction) where Go today raises 22012, and the reviewers' `id = 5 AND 1 / 0 = 1 AND 1
= 2` is not a fold of the target at all (its literals are constant object values), so
both engines raise the division there. The mechanism is measured on a prototype with
the stand-in's folds (the unfolded probe wins without the prune, the annulment with
it), and the whole suite runs with the prune (89 of 94 targets, the 5 reds
accounted). The ported rule is ONE rule under the
target's name over both the select and the one-source filter, so
DISABLE_PLANNER_REWRITING turns off both arms. The partition guard's deletion is
measured on ROWS, not only plans: TestFDB_GroupByWithWherePush passes and the nine
scenarios holding the 26 changed corpus entries return their pinned rows (4.1(b)). The
IN-union takes the IN-join's sources, evaluated when the plan opens; a SQL list takes
the target's deduplicating explode arm; the rebased InUnion keys are field paths and
need no simplification (4.1). The temporal lane (4.3(2)) is rewritten on half-open
bounds that are exact complements under NOT, `<>` included, NULL-strict in every arm,
one lane per comparison admitted by its constructor and inherited by the IN-join and
the explode, one parser of stored text with its failure outcome and SQLSTATE, and the
runtime coercer restricted by target type. The constant-evaluation census covers every
evaluation with no context, not only `EvaluateConstant`: it finds two more Go-only
folds (PROMOTE and CAST over a constant) and a comparison evaluator of the deleted
driver, all deleted; every census states how it reads its population under Bazel and
carries a positive control (5.4(a)). Citations and counts are bound to the reviewed
tree (the UUID census, the `readIndexState` census, now 35 calls in 14 files since
WS-C deletes one); the store-open conflicts cite `loadRecordStoreState` and the cached
path; the harness teardown cleans up in `t.Cleanup` and asserts the not-found codes.
The v7 paragraph follows.

v7 answered the three v6 NAKs
(`ws-e-design-review-v6/`). Measured on prototypes of Go's planner: the fold port
reaches a one-source WHERE by a rule over the filter that yields a filter, and Go's
PLANNING decides what the target's REWRITING decides (section 5.4(c)); the whole suite,
the Java-parity net included, runs on the fold-free prototype and every red is
accounted for (5.4(a)); the explode rule is registered in PLANNING only, without the
idempotency scan, and reaches the IN-join through Go's partition rule once that rule's
stale uncorrelated-explode guard is deleted; the single-element collapse goes, and
with it Go's InUnion is made to cover as the target's does (two causes, both measured);
and a comparand correlated to the select's own row is a tie row in the target, which Go
decides by its EstimateCost rung (section 4.1). The oracle gains the UUID component
boundary rows (387 pinned target outcomes). Section 4.3 now writes temporal values
canonically from every source, compares DATE with TIMESTAMP through one projected text
comparison with the lane carried statically and saturating at year 9999, and deletes the
`time.Time` comparison arms; the UUID lane names all six parser sites; the harness drops
survive a canceled run. The v1 to v6
designs and their verdicts are in `ws-e-design-review-v1/` to `-v6/`; nothing below
depends on them. This document records the implementation decision, not a request to
choose an option. Java is tag 4.14.2.0 at
`fdacd162a9c8acfadc49082b89185c823ab8ae4a`. The umbrella scope is RFC-257 WS-E
(`rfcs/257-java-4.14.2.0-upgrade.md`, "WS-E"); the audit sources are build-docs
W4-W6, cascades W1-W2/W11 and relational W1-W3/W10; the umbrella's build-docs W7
(ARRAY_AGG) belongs to WS-G. WS-E1 (FROM-less queries) is
already accepted with WS-A and is not repeated here. Preserved boundaries: the
owner-approved Go read/raw nullable-array, scalar and sorting extensions; the
Go-only IN-subquery read extension, which is not present at the base (Go refuses every
shape with 0AF00, as the target does; the umbrella RFC's WS-E section); C++ 7.3.77; no
second query pipeline.

## 0. Measured target behaviour

Every contract below that names a target outcome is MEASURED, not inferred, and
cites its pinned row; claims taken from source alone are labelled SOURCE. The
live-JVM oracle is `conformance/ws_e_probe_conformance_test.go` in
`//conformance:rfc257_oracle_test`, fourteen Describes: the ten of rounds v1 to v10, with
457 pinned target outcomes; round v11's (10 pins, both engines' NaN bits); and round v12's
three (92 pins of both engines' NaN producers and CAST spellings, 9 of the target's
planner task counts, and 6 of a Go-written NaN in a target store). They are:
`WS-E target oracle` (74: 59 plain, one over an enum-typed table, 15 prepared) over
one table `T(id BIGINT, s STRING, n BIGINT)` with rows `(1,'abc',NULL)`,
`(2,'a<LF>b',5)`, `(3,'<U+1D11E>x',7)`, `(4,'a%b',1)`, `(5,'a_b',2)`, `(6,'ab<LF>',3)`,
`(7,'a\b',4)`; `WS-E target oracle v2` (37: LIKE error timing, B64 and decorated
literals, EXPLAIN with SNAPSHOT, variadic admission, eager evaluation, result
nullability, several, named and typed parameters); `WS-E target oracle v2 extended`
(12: ORDER BY a constant, SNAPSHOT on the CONNECTION, prepared DML into an INTEGER
column, mixed and repeated named parameters, `IN ?`, a UUID parameter); `WS-E target
oracle v3` (46: nullability of all-NOT-NULL calls, COALESCE folding in WHERE and SELECT,
IN and array NULL timing, GREATEST/LEAST over negative values, array comparison
operands, DRY_RUN and SNAPSHOT connection options over DML, DDL, SHOW and EXPLAIN
INSERT, parameter order, parameter counts, FLOAT/DOUBLE/INT bindings and literals, and
an untyped bound NULL); and `WS-E target oracle v4` (89: COALESCE heads of every
folding class in WHERE and SELECT with their EXPLAINs, the null-strict collapse,
inline and typed NULL in arithmetic, the STRING and BOOLEAN arithmetic lanes, IN-list
NULL timing separated from planning by EXPLAIN, a correlated list and filtered-out
rows, GREATEST/LEAST at zero, NaN, infinity and the LONG lane, an overflowing
literal, DESCRIBE/DESC/HELP on a snapshot connection, DRY_RUN over DELETE and DDL,
assignment of expressions and column sources per lane, ARRAY parameters in INSERT and
UPDATE, the HAVING row's controls, array comparisons with a NULL element, and the
read scope of the snapshot option in an explicit transaction with a concurrent writer,
over a covering scan, an index scan with record fetches and a record scan, Java step
`snapshotReadScopeProbe`); and `WS-E target oracle v5` (75: the REWRITING prune of
eight predicate simplifications traced with Java step `planRuleTrace`, the same
null-strict comparison over a two-table and a seven-table schema and in both planning
orders, an OR over a division with a `NOT FALSE` disjunct, IS NULL over a NOT NULL
COALESCE and over the null-strict collapse, a COALESCE under AND and under NOT, a
collapse nested in a COALESCE head, folded-boolean nullability in a projection, a
typed-NULL IN item under OR, NOT, NOT IN, a projection, a CASE and on the inner side
of a join, over an empty and a populated table, the STRING lanes with FLOAT, DOUBLE
and LONG operands, assignment into array elements and struct fields and of strings
into ENUM and UUID columns, index-state read conflicts for the scanned index, an
unused index and a record scan under both isolation levels (Java step
`indexStateReadScopeProbe`), and the read-scope controls v4 lacked); and `WS-E target
oracle v6` (54: a constant expression as a comparand and its trace, folds to `false`
and `true` beside another conjunct and their traces, IS [NOT] NULL over values whose
nullability decides it and over nullable values that fail, CAST nullability over a
literal and a column, the CASE rows that locate a target defect, a correlated IN item
of the select's own row, and the STRING to UUID lane on eleven spellings as a literal,
a bound string and a CAST, and to ENUM on three, plus the component boundary: a
component of 2^63 refused, `7fffffffffffffff` accepted, and a fifth dash refused);
and `WS-E target oracle v8` (31, over T with an index T_N on n: annulling and reducing
folds beside a primary-key, a primary-key IN, an index range and an index equality
conjunct, first with comparisons of literals, which the target does not fold, then
with IS NULL over a NOT NULL COALESCE and over the null-strict collapse, which it
does, each with its EXPLAIN; and tie folds, a NOT over a comparison and a duplicated
OR, alone and beside a primary-key and an indexed conjunct, with their EXPLAINs);
and `WS-E target oracle v9` (27: two IN lists over a two-column index and two under
a requested ordering, with their EXPLAINs; IN lists and equalities over signed zeros
and NaN, over an indexed and an unindexed DOUBLE column; an annulling fold beside a
UNIQUE index's equality and inside a UNION ALL leg, with their EXPLAINs; and DATE rows,
which the target refuses, having no DATE type); and `WS-E target oracle v10` (12: a
TIMESTAMP column, refused as the DATE column is; `CAST(... AS DATE)` and `CAST(... AS
TIMESTAMP)` as values and over a column, syntax errors in the target, and with Go's
GO line showing its TIMESTAMP column read back as STRING; a NaN equality at the FIRST
of two index components beside an equality, an IN and an ORDER BY over the second,
with their EXPLAINs; and the second NaN insert into a UNIQUE index and a lookup there);
and `WS-E target oracle v11` (10: the target's stored bits of a NaN made by CAST to
DOUBLE and FLOAT and by a division, read from the raw entries of an index on each column
of a kept store, beside Go's evaluator bits for the same expressions, which this round
PINS as well, since the bits are the finding; and the target's refusal of a DOUBLE
division into a FLOAT column); `WS-E target oracle v12` (92: MIN and MAX over a column
holding the division's NaN, written by INSERT ... SELECT and read back as stored bits in
both engines, and 22 spellings of a string CAST to DOUBLE, each stored through an index,
with both engines' outcome and bits); and `WS-E target oracle v12 planning cost` (9: the
target's planner task count per phase, counted from its ExecutingTaskPlannerEvents by
Java step `planRuleTrace`'s TASK-COUNT mode, with its EXPLAIN, for one to four IN lists,
repeated conjuncts and #7; the per-rule breakdown is printed, not pinned); and `WS-E target
oracle v12 cross-engine NaN` (6: the NaN Go's CAST makes, written by Go's record layer into a
store the target created with a UNIQUE index on the column, then the target's covering probe
for its own NaN, its EXPLAIN, its insert of that NaN beside Go's row, and the index's stored
bits). Rounds v11
and v12 pin a NaN made by a division, which is the hardware's, so they fail with that
reason on an architecture other than amd64, where they were measured
(`wseRequireNaNPinArch`), rather than compare bits across machines (v11 Torvalds L4).

Every Describe up to v10 runs through one set of helpers (`wseRender`, `wseGoArg`,
`wseOracle.plain`/`prepared`/`check`): a row renders as result types, JDBC
nullability, exact values (integers with all their digits, NULL as NULL) and, for a
DML statement, `COUNT n`, the update count it reported before its follow-up query
(`plandiff.RowSet.UpdateCount`, from executeUpdate on the Java side and RowsAffected
on the Go side); an EXPLAIN or DESCRIBE of a query renders its textual plan only; an
error renders SQLSTATE, class and message. Every Java prepared statement runs through
the step `runPreparedExtended`. Each Describe checks that its probe set equals its pin
set, then each probe against its pin. Every JAVA outcome is pinned verbatim; the GO
outcome is printed, not pinned, because it is the gap this design closes. The run
record is `ws-e-oracle/evidence-run.txt`, regenerated for v12 (v11's text here still
described the ten-Describe run, v11 Graefe L1): two uncached runs of all fourteen WS-E
Describes, whose probe blocks and pin lines are equal as sorted sets. The file states
the counts it found, blocks and pin lines separately (v11's "458 probe blocks" counted
the teardown line as a block). The sha256 of the oracle's source files is verified
unchanged after the runs, each check printing its ": OK" count beside its zero. `ws-e-oracle/evidence-mutations.txt` records the v5 round's
three mutation runs over the first six Describes (and a fourth, M4, of the harness
cleanup below), each with its presence check: one pin VALUE mutated per
Describe (a re-rendered type, two re-rendered nullabilities, two update counts, a v4
row) reddens each Describe on exactly that row; one pin DELETED per Describe reddens
each on the probe-set = pin-set check; and a probe renamed to an existing probe's name
reddens only its own Describe, on the probe-name uniqueness check; the v6 round's
two runs over the seventh (two pin values, one pin deleted), each reddening it on
exactly the mutated rows; v7's run over one of its new UUID boundary pins, which
reddens the seventh on exactly that row; and v8's run over one of the eighth's
type-annulment pins (M8-annul), which reddens the eighth on exactly that row; and v9's
run over one of the ninth's nested-IN pins (M9-nested), which reddens the ninth on exactly
that row; and v10's run over one of the tenth's NaN pins (M10-nan), which reddens the
tenth on exactly that row (its first attempt did not build and is recorded as such);
M11, the target pin `java_t_d_id1` of round v11 mutated, which reddens exactly the v11
spec on that row (v11 ran it and left it out of this list); and M12, round v12's pin
`java_u_d_id10` mutated to Go's bits, which reddens exactly the v12 spec on that row
(`/var/tmp/fdb-upgrade-recovery/wse12-mut.log`; its first attempt put the marker before
the comma, did not build, exit 1, and is recorded as such in `wse12-mut-nobuild.log`). The Go side binds
an `int` kind as int32 and a `long` kind as int64, so both integer lanes are asked. A
target upgrade that changes a row reddens the spec and forces the matching section
here to be revisited. Every target run the design rests on is one of those two files'
runs, named with its log; the Go-side measurements of section 5.4 (the suite run with
the translator folds removed, and the REWRITING-model variants) each name their tree,
command and log where they are cited. Rows are cited below as `[probe_name]`.

The harness drops what it creates and reports a drop that fails. The Java steps
create and drop through one catalog URL (`SYS_CATALOG_URL`, sql_plan_steps.java), and
a failed drop is the step's error, or a suppressed exception behind the step's own;
the Go runner's teardown does the same through `withTeardown`
(pkg/relational/conformance/plandiff/go_runner.go), whose every arm
`TestWithTeardown` drives. The spec "Conformance server ephemeral schema teardown"
pins that neither the ephemeral database nor its template is listed afterwards, over
listings that are checked to be non-empty (the Java step reports how many rows each
listing returned and the Go spec fails on zero, fault_inject_retry_conformance_test.go:
220-221; the template name comes from the harness that created it, not from string
surgery);
the Java teardown catches every exception of a drop, so a runtime failure of the
database drop still lets the template drop run; the Go runner's deferred drops reach
the error `runEphemeralFollowUp` returns (`TestGoSQLRunner_TeardownFailureIsReported`
drives the wiring through the runner's `teardownExec` seam, which `TestWithTeardown`
alone cannot see, and cleans up what it made fail in a `t.Cleanup` registered before
the run, so a failed assertion no longer leaks the two objects); the drops run detached
from the run's cancellation, under their own timeout, so a canceled run still removes
what it created (`TestGoSQLRunner_CanceledRunStillTearsDown`, whose strict re-drop of
each object must fail with the not-found code of what it drops, 42F63 for the
database and 42F55 for the template, where v7 accepted any error; the v6 drops
inherited the run's context and leaked both objects of a canceled run); and the template that
`dry_run_connection_create_template` really creates (the target ignores DRY_RUN on
DDL) is dropped after its probe, the drop asserted and the template's absence asserted
as the not-found SQLSTATE 42F55. Each of those checks has a recorded mutation that
reddens it (evidence-mutations.txt: M4, "Harness teardown, v5", "Harness teardown,
v6" and "Harness teardown, v7": both listing guards, the Go wiring and the detached
drops).

## 1. LIKE (build-docs W4, cascades W1, relational W3)

### Target

Grammar: `expression NOT? LIKE pattern=constant (ESCAPE escape=STRING_LITERAL)?`
(RelationalParser.g4:1255). `constant` admits string (adjacent-token) literals,
decimals, bytes, booleans and NULL, not parameters and not columns.
ExpressionVisitor.visitLikePredicate (:695-710) builds
`__pattern_for_like(pattern, escape)` with the escape decoded from its single token
(`SemanticAnalyzer.normalizeStringLiteral(ctx.escape.getText())`) or a NULL literal
when absent, then `like(operand, pattern)`, then NOT.

`PatternForLikeValue` (values/PatternForLikeValue.java) has a RECORD result type:
field 1 pattern and field 2 escape, both nullable STRING (:92-95). Its eval (:115-131)
evaluates the pattern, then the escape; a non-NULL escape is validated even when
the pattern is NULL (`validateEscapeChar`, :133-139: exactly one UTF-16 unit, not a
surrogate, else ESCAPE_CHAR_OF_LIKE_OPERATOR_IS_NOT_SINGLE_CHAR; `%` or `_`, else
ESCAPE_CHARACTER_CONFLICT), and the pattern is validated only when both are
non-NULL (`validatePattern`, :141-153: an escape must be followed by `_`, `%` or
itself, else INVALID_ESCAPE_SEQUENCE). A NULL pattern leaves field 1 absent; a NULL
escape leaves field 2 absent. Typing (`encapsulate`, :231-239): pattern and escape
must be NULL-typed or STRING, else OPERAND_OF_LIKE_OPERATOR_IS_NOT_STRING.

`LikeOperatorValue` (values/LikeOperatorValue.java): typing (:320-327) requires the
operand NULL-typed or STRING and the pattern exactly PatternForLikeValue.TYPE, else
OPERAND_OF_LIKE_OPERATOR_IS_NOT_STRING. `likeOperation` (:93-105): NULL operand,
NULL pattern record or absent pattern field -> NULL. `matchLike` (:145-241) works on
UTF-16 code units over the WHOLE text: `%` backtracks from the last wildcard,
`_` consumes one unit or one surrogate pair (:207-215), an escaped character matches
that unit literally, trailing `%` match the empty string, and nothing treats line
terminators specially. It re-checks escape sequences as it meets them (:184-190).
`toQueryPredicate` (:245-248) turns it into `ValuePredicate(operand,
ValueComparison(LIKE, patternValue))`, and `Comparisons.evalComparison` LIKE calls
`compareLike`, which delegates to the same `likeOperation` (Comparisons.java:
761-762, 284-295). LIKE never bounds an index scan
(RangeConstraints.canBeUsedInScanPrefix returns false for it; Go already ports that,
predicates/range_constraints.go:166-182).

SQLSTATEs (ExceptionUtil.java:95-102, ErrorCode.java:83-88, messages
SemanticException.java:47-57): OPERAND_OF_LIKE_OPERATOR_IS_NOT_STRING -> 22F00 "The
like operator expects string operands but was invoked with an operand of another
type."; ESCAPE_CHAR_OF_LIKE_OPERATOR_IS_NOT_SINGLE_CHAR -> 22019 "The like operator
expects an escape character of length 1."; ESCAPE_CHARACTER_CONFLICT -> 2200B "The
like operator rejects wildcards as the escape character."; INVALID_ESCAPE_SEQUENCE
-> 22025 "The like operator pattern requires all escape characters to be followed
by a special character."

Measured: `%` and `_` cross LF [like_percent_crosses_newline,
like_underscore_matches_newline]; no trailing-newline tolerance
[like_no_trailing_newline_tolerance]; `_` consumes a surrogate pair
[like_underscore_surrogate_pair]; escaped `%`, `_` and escape
[like_escape_percent, like_escape_underscore, like_escaped_escape]; 2200B for `%`/`_`
escapes; 22019 for two characters, the empty string and a supplementary character
[like_escape_two_chars, like_escape_empty, like_escape_supplementary]; 22025 for a
dangling escape and an escape before an ordinary character; 22019 for a bad escape
with a NULL pattern [like_null_pattern_bad_escape]; a NULL pattern filters every
row [like_null_pattern]; 22F00 for a numeric operand and for a numeric pattern
[like_numeric_operand, like_numeric_pattern]; the empty pattern matches only the
empty string [like_empty_pattern]; an adjacent-literal pattern `'a' '%'` is `'a%'`
[like_adjacent_literal_pattern]; projection returns the boolean [like_projection].

### Go today

`values/like_match.go` deliberately implements the pre-4.14 regex semantics
(newline exclusions, a retry after stripping one final line terminator :90-100,
permissive escapes :203); `values/value_pattern_for_like.go:88-129` returns a regex
string and nil for a malformed escape; `values/value_like.go:104-117` ignores the
escape and returns nil for non-string operands; `predicates/comparisons.go` carries
a Go-only `Comparison.Escape rune` and a literal-string operand for ComparisonLike;
`core/query/expr/expr.go:1557-1597` requires a constant string pattern and maps a
non-string operand to 42804; `core/query/expr/walk.go:2007-2045` measures ESCAPE in
Go runes (a supplementary character passes) and turns every length error into an
UnsupportedExpressionShapeError, i.e. 0AF00 at planning [like_escape_two_chars];
the Go grammar spells the pattern `STRING_LITERAL` (RelationalParser.g4:1230), so a
NULL, numeric or adjacent-literal pattern is a 42601 [like_null_pattern,
like_numeric_pattern, like_adjacent_literal_pattern]. `api/errcode.go` has 22F00 but
not 22019, 2200B or 22025.

### Design

1. Grammar: the Go predicate rule becomes Java's `NOT? LIKE pattern=constant
   (ESCAPE escape=STRING_LITERAL)?`. The walker visits the pattern through the
   ordinary constant path (so section 2's per-token decoding applies) and decodes
   the escape from its single token.
2. `PatternForLikeValue` is rebuilt as Java's value: children (pattern, escape),
   result type the two-field record (field numbers 1 and 2, both nullable STRING),
   and a runtime result `LikePattern{Pattern, Escape *string}` whose nil pointers
   are Java's absent fields (explicit absence, distinct from any character value).
   Evaluate follows :115-131 exactly, including the order (pattern evaluated first,
   escape validated whenever non-nil, pattern validated only when both are
   non-nil). The escape's UTF-16 rule is computed once from the escape string: it is
   valid iff `utf16.Encode([]rune(s))` has length 1 and that unit is not a
   surrogate. Every string reaching it is valid UTF-8: the lexer produces none
   other, and section 4 rejects a bound parameter that is not.
3. `LikeOperatorValue(operand, pattern)` evaluates `likeOperation` and `matchLike`
   ported line for line, including the in-match escape re-check, over RUNES: the
   target's `_` consumes one code point, a surrogate pair included (measured
   [like_underscore_surrogate_pair]), and for valid UTF-8 every rune is one code
   point, so rune-level matching is the target's matching without a per-row
   `[]uint16` conversion. The existing matcher (`values/like_match.go`) is replaced
   rather than kept, because it is a regex translation: it cannot raise the
   target's in-match escape error (22025) at the position the target does, and its
   newline handling is the reason for [like_percent_crosses_newline,
   like_underscore_matches_newline, like_no_trailing_newline_tolerance]'s Go lines.
   Its fuzz targets are retargeted to the new matcher with a differential against a
   brute-force code-point reference.
4. Predicates: `ComparisonLike`'s operand becomes the PatternForLikeValue (a Value,
   as Java's ValueComparison), and `Comparison.Escape` is removed; every consumer
   is converted in the same change, so no second LIKE representation survives:
   predicates/comparisons.go:222,513,997; rule_simplify.go:484;
   null_rejecting_conjuncts.go:76; the identity and hash sites semantic_hash.go:42,
   semantic_equals.go:41, predicates.go:174, comparison_identity.go:58,104,
   plans/semantic_identity.go:167,216 and planning_cost_model.go:1401; and the
   factory/rowdiff generators. The escape-distinctness pins
   (predicates_test.go:783, semantic_hash_invariant_test.go:22-23) are carried over:
   two LIKEs differing only in the escape must stay distinct in hash, equality and
   plan identity. Explain renders
   Java's tokens: `operand LIKE pattern ESCAPE escape`
   (PatternForLikeValue.java:182-188, LikeOperatorValue.java:277-283). The
   INFORMATION_SCHEMA/system-table filters evaluate the same value.
5. Typing and errors: the operand and pattern type checks are Java's (NULL or
   STRING, else 22F00 with Java's message), replacing Go's 42804 gate. Go's current
   admission of ENUM, DATE and TIMESTAMP operands (expr.go:1581-1590) is removed for
   ENUM, a type the target has and rejects with 22F00 (measured
   [like_enum_operand]), and kept for DATE and TIMESTAMP, which the target does not
   have at all (relational DataType.Code has no date or timestamp,
   DataType.java:1600-1616), so it is an allowed read-side extension with its own
   tests. Go SQL DDL cannot declare an enum type today (ddl.go:300-302 rejects
   CREATE TYPE AS ENUM until RFC-257 WS-J F6 lands), so the Go ENUM rejection is
   tested through record-layer metadata carrying a proto enum field. New typed
   errors carry the three new SQLSTATEs (22019, 2200B, 22025) with Java's messages.
   TIMING: they are raised only when a row evaluates the predicate. The target's
   pattern is a ConstantObjectValue validated inside ValueComparison.eval per row
   (Comparisons.java:1656-1662, PatternForLikeValue.java:115-131), so a bad escape
   over an empty table, over rows another conjunct filters out, and under EXPLAIN
   all succeed (measured [like_bad_escape_empty_table,
   like_dangling_escape_empty_table, like_bad_escape_filtered_out,
   like_bad_escape_explain]), while a NULL operand still raises 22019 because the
   pattern is evaluated before the operand's NULL test
   [like_bad_escape_null_operand]. Go today fails four of the five at planning with
   0AF00; the dangling escape over the empty table already answers (their GO lines).
   So the simplifier never folds a PatternForLikeValue, valid pattern or not: the
   target's EXPLAIN keeps it as `LIKE @c7 ESCAPE 'ab'` [like_bad_escape_explain]. The
   predicate regime does visit the pattern (ValuePredicateSimplificationRule
   simplifies the comparand of every non-unary value comparison, :62-74), but its
   dereference rule turns only BOOLEAN and NULL constant objects into literals, a
   STRING constant object stays a constant object, and no rule in either set evaluates
   a PatternForLikeValue (section 5.4), so the pattern survives to run time. Go's
   generic fold, which could have evaluated one, is deleted (section 5.4 (b)), and a
   test runs both value sets and the predicate set over a PatternForLikeValue and
   asserts it is returned unchanged. The
   predicate is evaluated in the target's order: pattern, escape validation, then the
   operand.
5b. The Go-only `Comparison.Escape` is removed together with every reader of it:
   predicates/comparisons.go, the field registries at comparison_identity.go:27 and
   plans/semantic_identity.go:114, and rule_match_intermediate.go:1232. The matcher's
   rune-equivalence claim is TESTED, not assumed: a differential runs `matchLike`
   against a UTF-16 transliteration of Java's `matchLike` (LikeOperatorValue.java:
   145-241) over random patterns, escapes and subjects including supplementary
   characters. Patterns and escapes are always valid UTF-8 (the lexer produces
   nothing else, and section 4.3 refuses an invalid bound string). A STORED operand
   need not be: no Go write path validates UTF-8 today (`git grep -n 'utf8.Valid'`
   over non-test `pkg/recordlayer` and `pkg/relational` finds only conformance
   helpers), and proto2 string fields do not enforce it, so a Go library writer can
   store invalid bytes. The target reads such a field leniently: protobuf-java's
   `ByteString.toStringUtf8` decodes with the JDK's UTF-8 decoder in REPLACE mode, one
   U+FFFD per maximal invalid subsequence. The matcher therefore decodes an operand
   that fails `utf8.ValidString` with that same rule (a Go helper implementing the
   maximal-subpart substitution, tested byte-for-byte against the JDK decoder on the
   Unicode "U+FFFD substitution" table and on fuzzed input), so a LIKE over an invalid
   stored string answers exactly what the target answers over the same bytes; Go's
   own `for range` decoding (one U+FFFD per invalid BYTE) would differ on a truncated
   multi-byte sequence. Valid strings take the fast path unchanged. Writing such
   strings stops too: section 4.3's Values rule makes the record-layer save path
   refuse a string field that is not valid UTF-8, the only writer that can produce
   one.
6. No LIKE-to-STARTS_WITH or prefix-bound rewrite exists or is added; LIKE stays a
   residual (the RFC's "prefix bounds require sound residual evaluation" is
   satisfied by having none).
7. Wire: Go does not serialize Values into PValue protos (no Go writer of
   `PPatternForLikeValue`/`PLikeOperatorValue`; `gen/record_query_plan.pb.go` is
   generated only), and Go continuations carry no plan, so the record-typed pattern
   is a planner-internal change with no persisted bytes.

Obsolete tests revised individually, each with its causal upstream reference
(#4430): `values/like_match_test.go:51-67,106,117,176,329`,
`values/value_pattern_for_like_test.go:91,110,136`,
`predicates/comparisons_test.go:1057,1258`, `core/embedded/embedded_test.go:477`,
`sqldriver/like_escape_parity_fdb_test.go:35,56,85,108`,
`core/embedded/like_prefix_not_sargable_test.go:77`.

## 2. Adjacent string literals (relational W1, #4524)

Target: `stringLiteral` is `STRING_CHARSET_NAME? STRING_LITERAL STRING_LITERAL+ |
...` (RelationalParser.g4:808-816), and SemanticAnalyzer.normalizeStringLiteral
(:197-213) decodes EACH `STRING_LITERAL` token (strip the quotes, `''` -> `'`) and
concatenates. The decorated forms never reach a value: ExpressionVisitor rejects a
charset-prefixed, national or COLLATE literal with 0AF00 (:849-851), measured
[literal_charset_prefix] "charset not is supported" (the target's own wording),
[literal_national] "national string literal is not supported" and
[literal_collate] "collation is not supported"; Go today returns the raw token text
as the value (`_utf8'abc'`, `N'abc'`, `'abc'COLLATEutf8_bin`, their GO lines). AstNormalizer extracts cached
literals with the same decoder (:151-155). Single-token sites (LIKE ESCAPE, enum
values) use the token decoder (:223-228). Measured: `'a' 'b'` is `ab`, `'a''b'` is
`a'b`, `'a''' 'b'` is `a'b`, and a predicate compares the concatenation
[literal_adjacent, literal_doubled_quote, literal_adjacent_with_doubled,
literal_adjacent_predicate].

Go today decodes `k.GetText()` of the whole context (walk.go:2239-2246), which
concatenates tokens without spacing and cannot tell `'a' 'b'` from `'a''b'`
(measured: Go returns `a'b` and `a''b`). Design: the walker decodes
`StringLiteralContext.AllSTRING_LITERAL()` token by token exactly as :197-213, with
the three decorated forms rejected at the same point with the target's 0AF00
messages verbatim (a typo in a shared message is kept, as the conformance
principle requires where the message can be shared); the LIKE escape and every
other single-token site use one token decoder. `functions.StripStringLiteralQuotes`
(cast.go:244) has no non-test caller and is deleted with its two test files'
uses converted to the token decoder; `walk.go`'s `stripStringLiteral` becomes
that single token decoder. Tests cover projection, predicates, INSERT and UPDATE
values, LIKE patterns and ESCAPE.

## 3. SQL comments and cache normalization (build-docs W5, relational W1, #4392)

Target (RelationalLexer.g4:29-67,1425-1429): comments are SKIPPED (no token, no
token-index contribution); `--` starts a line comment with no following whitespace
and CR, LF, CRLF or EOF ends it; block comments nest through a lexer mode stack;
EOF inside a block comment is a lexer error raised from `emitEOF`; `#` is not a
comment; `/*! ... */` is an ordinary block comment; quoted markers are data.
QueryParser reports lexer errors as 42601. Measured: `1--1` is `1` followed by a
comment [comment_dash_no_space]; nesting [comment_nested_block]; unterminated and
unterminated-nested blocks are 42601 [comment_unterminated_block,
comment_unterminated_nested]; `#` is a 42601 token error [comment_hash_not_comment];
CR and CRLF end a line comment [comment_cr_terminates, comment_crlf_terminates];
`/*! ... */` is ignored [comment_mysql_executable_form]; `'--x'` is data
[comment_marker_in_string].

Go today: RelationalLexer.g4:29-39 keeps a MYSQLCOMMENT channel, non-nesting hidden
block comments, `#` comments and whitespace-dependent `--`; the plan-cache key text
is re-normalized by a handwritten scanner (`embedded/query_hash.go:118-248`:
stripComments, collapseWhitespace, upperOutsideStrings) with its own comment rules,
and `embedded/utilities.go:94-164` (parameter substitution) has a third.

Design:
1. The Go lexer grammar takes Java's comment rules verbatim. Java's `@lexer::members`
   block is Java code and cannot compile for the Go target, so the Go grammar
   replaces it with Go `@lexer::structmembers { openBlockComments int }` and
   `@lexer::members` overriding `PushMode`/`PopMode` (counting the
   IN_BLOCK_COMMENT depth; the ATN's LexerPushModeAction and LexerPopModeAction
   call them through the Lexer interface on the generated lexer) and `NextToken`
   (on the EOF token with a non-zero depth, report one LexerNoViableAlt-style
   syntax error through `GetErrorListenerDispatch()`, the exported dispatch;
   antlr4-go's `notifyListeners` is unexported), resetting the depth counter in
   `Reset` and `SetInputStream`. This reproduces `emitEOF`; together with the
   already Go-specific ERROR_RECOGNITION action (RelationalLexer.g4:1382-1389) it is
   the Go-only text in the lexer grammar. The generated parser is regenerated
   (`just generate-parser`) and the drift check stays green.
2. The plan-cache key text is rebuilt from TOKENS, not raw text, by the target's
   rule (AstNormalizer.visitTerminal, :190-206; visitUid, :216-219): walking the
   statement's parse tree, a terminal whose token type has a LITERAL name in the
   lexer vocabulary (keywords and punctuation) is uppercased; every other terminal
   (identifiers, string, B64, hex and national literals, numbers) is kept byte for
   byte; an identifier (`uid`) is replaced by its analyzer-normalized form in
   quotes, through the same `semantic.NormalizeString` the resolver uses, so
   exactly the names the analyzer already merges share a key. Tokens are joined by
   one space; comments are never tokens. The fold is fail-closed: a new lexer rule
   is kept verbatim unless it is a literal-named keyword. MEASURED why it matters:
   `B64'YWJj'` and `B64'ywjj'` are different values in the target
   [b64_literal_upper, b64_literal_lower], and Go inlines constants into cached
   plans, so a case-folded key would return the first statement's bytes for the
   second. The statement-options subtree is NOT part of the text (the target
   visits it only to fill query options, :282-312): a planning option enters the
   key through a planner-configuration component, the target's
   `getQuerySpecificPlannerConfig` (:696, :713) — today only PLAN RIGHT DEEP, which
   sets the planner's existing `ShouldJoinRightDeep` (rule_partition_select.go:338,
   Java PartitionSelectRule.java:93); the execution options (SNAPSHOT, DRY RUN, LOG
   QUERY) never enter the key; NOCACHE bypasses the cache. `stripComments`,
   `collapseWhitespace` and `upperOutsideStrings` are deleted. Injectivity: a token
   never begins or ends with a space and whitespace occurs inside a token only
   within quotes, so distinct token sequences render distinctly. Tests pin
   `SELECT AB`/`SELECT A B`, `'a b'`/`'a' 'b'`, `1.5`/`1 .5`, the two B64 spellings
   and a national literal pair (each a separate entry and each returning its own
   value warm), `f3`/`F3` for a case-sensitive function name, quoted versus
   unquoted identifiers, comment/no-comment equivalence, a statement with and
   without SNAPSHOT sharing one entry while each execution reads at its own
   isolation, OPTIONS (NOCACHE) never populating or reading the cache, and PLAN
   RIGHT DEEP with and without it NOT sharing one.
3. The third scanner disappears with section 4's typed binding. A fourth
   canonicaliser exists: `aggOperandCanonicalText` (logical_builder.go:839-872)
   upper-cases every terminal except DOUBLE_QUOTE_ID and STRING_LITERAL, and
   aggregate result slots are keyed by that name, so two `COUNT(CASE WHEN b = B64'...'
   ...)` aggregates differing only in the literal's case would plausibly share a slot
   (SOURCE). It is rebuilt on the SAME fold predicate as the cache text (one function
   deciding, per terminal, keep or upper-case), with a test of two aggregates that
   differ only in a B64 literal's case. The DRY RUN key-text invariant, that a DML
   plan is never cached (cascades_generator.go:1198), is pinned by a test so the
   option can stay out of the key text safely. SQL text that is not valid UTF-8 is
   refused with 22021 before lexing, as a bound string is (section 4.3), instead of
   today's silent U+FFFD replacement (case_insensitive.go:22): the target cannot
   receive such text, and a replaced character would change a literal the caller
   wrote. The IN list's bare-NULL check (section 4.1) runs in this normalization pass,
   before cache lookup, as the target runs it (AstNormalizer.java:497-503).
4. Malformed SQL never reaches the cache: `query_hash_test.go:30` (hash comments
   removable) and `:32` (malformed block normalization to `SELECT * FROM FOO D`) are
   replaced by lexer-error tests. Syntax-error MESSAGE text stays Go's caret format;
   the parity point is SQLSTATE 42601, as for every other Go syntax error.

## 4. Arrays, IN lists and typed parameter binding (relational W2, #4171/#4453/#4480/#4469/#4595)

Target, measured: a bare NULL IN item is 42809 RelationalException "NULL values are
not allowed in the IN list" before planning, alone, beside a literal or under NOT
[in_bare_null, in_bare_null_beside_literal, not_in_bare_null]; a PARENTHESIZED
NULL is not bare: `IN ((NULL))` is 0A000 SemanticException "The action is currently
unsupported An ARRAY value cannot have NULL elements" [in_wrapped_null], as is a
typed NULL `CAST(NULL AS BIGINT)` [in_typed_null] and a column that is NULL on some
row, at run time [in_column_value_null]; an array literal with a bare NULL element
is 0A000 RelationalException "An ARRAY value cannot have NULL elements"
[array_bare_null_element]; a typed-NULL element and a nullable column element that
is NULL are the SemanticException form [array_typed_null_element,
array_nullable_column_element]. Target tests `InListNullParameterTest.java:53,65`
bind `setNull(p, BIGINT)` into `IN (?p)` and `IN (1, ?p)` and expect 0A000 with the
same message.

Go today: bare NULL is 42809 (walk.go:1983-2003, logical_predicate.go:1002-1004), but
the check keys on the item's VALUE (walk.go:1991-1996), so it also catches
`((NULL))`, which the target sends to 0A000; typed and column NULL elements succeed. Parameters are SUBSTITUTED INTO THE SQL TEXT
before parsing (`embedded/utilities.go:94-164`, called from connection.go:636,671),
so a bound nil becomes the bare token NULL and the target's bound-NULL contract
(0A000, an untyped NULL constant) cannot be expressed at all; the walker already has `ParameterValue` and a
`ParameterBinder` eval capability (values.go:2379-2395) that nothing binds.

Design:
1. IN list: the bare-NULL check is Java's AstNormalizer rule exactly
   (AstNormalizer.java:493-528): only the list items that are spelled out are
   checked, and an item is a bare NULL iff descending through SINGLE-CHILD parse
   nodes reaches a NullLiteral (`isNullLiteral`, :522-528). So the ordinary
   expression/predicate/atom/constant chain above a NULL token is seen through,
   while `(NULL)` (a three-child nested-expression node) and `CAST(NULL AS
   BIGINT)` are not bare [in_wrapped_null, in_typed_null]. The Go check walks the
   same parse-tree rule rather than today's value-based match. Every other
   NULL element fails with 0A000 and the SemanticException message.
   WHEN is measured, and it is when the ARRAY is evaluated, never when the statement
   is planned. The target puts an all-literal list through its literal pipeline as one
   constant array, `promote(@c7 AS ARRAY(LONG))` [in_single_literal_explain], and
   anything else, `CAST(NULL AS BIGINT)` and a column item included, through
   `__internal_array` (ParseHelpers.java:129-144, ExpressionVisitor.java:646-656), an
   array constructor whose nullable elements are promoted to the non-null element type
   (item 2), which fails with the SemanticException 0A000 when it EVALUATES a NULL.
   Where that happens depends only on where the plan evaluates the array:
   - A positive IN conjunct at the top of a SELECT's predicates is exploded by
     InComparisonToExplodeRule (:126-131), which takes the comparand as it is and
     explodes `ArrayDistinctValue(comparand)` (:164-176), with no constancy or
     correlation check. A row-independent explode is the OUTER side of its join and is
     evaluated when the plan opens: the statement fails over an empty table
     [in_cast_null_empty_table, in_cast_null_explain: `EXPLODE arrayDistinct(array(NULL))
     | FLATMAP ...`], over a populated one [in_cast_null_nonempty_table], when another
     conjunct filters out every row [in_cast_null_filtered_out], a literal beside the
     typed NULL included [in_literal_and_cast_null_explain: `[IN arrayDistinct(array(
     promote(@c8 AS LONG), NULL))] | INJOIN ...`, a runtime array, not the literal
     pipeline's constant], and as an IN-union source, opened the same way [in_cast_null_or_nonempty_table, in_cast_null_or_explain:
     `[IN arrayDistinct(array(NULL)) SORTED] | INJOIN ... ∪ SCAN(...)`].
   - An explode on the INNER side of a join is evaluated only when that side opens, so
     over an empty outer it never is [in_cast_null_join_inner_empty,
     in_cast_null_join_inner_explain: `SCAN([IS E]) | FLATMAP q0 -> { EXPLODE
     arrayDistinct(array(NULL)) ... }`]. WHICH side it lands on is a join-nesting
     choice, and that choice is decided by an identifier-sensitive cost tie in both
     engines (TODO.md, "An identifier-sensitive cost tie decides join nesting (RFC-235
     §17)"), so the empty-outer row is a TIE row, not parity: Go matches it today only
     because its plan-time fold yields `[nil]` (its GO line), and after (b) Go's nesting
     decides it. It is compared with the tie rows of section 5.4 (g): Go's EXPLAIN and
     answer are pinned per schema.
   - Everywhere else the comparison is a per-row ValueComparison (InOpValue.java:
     125-127): under an OR over a column no index serves [in_cast_null_or_empty_table],
     under NOT and as NOT IN [not_in_paren_cast_null_empty_table,
     not_in_paren_cast_null_nonempty_table, not_in_cast_null_empty_table,
     not_in_cast_null_nonempty_table, not_in_cast_null_explain: `FILTER NOT _.ID IN
     array(NULL)`], in a projection and a CASE [in_cast_null_projection_empty_table,
     in_cast_null_projection_nonempty_table, in_cast_null_case_nonempty_table], and a
     list with a column item [in_cast_null_mixed_empty_table,
     in_cast_null_mixed_nonempty_table]: no rows over an empty table, 0A000 on the first
     row that evaluates it. An array constructor in a projection behaves the same
     [array_cast_null_empty_table, array_cast_null_nonempty_table], and a column that
     is NULL on some row fails on that row [in_column_value_null].
   Go today evaluates a row-independent list during PLANNING wherever it meets one:
   the constant fork of `ResolveIn` (expr.go:1734-1795), and the explode rule, which
   calls `Evaluate(nil)` on the comparand and declines on an error
   (rule_in_to_explode.go:129-138), as `extractInValues` does for the IN-join source
   (rule_implement_in_join.go); the executor's IN join iterates only that plan-time
   list (`executeInJoin`, executor_new_plans.go:1784). Design, the target's mechanism
   at each of those places:
   (a) `ResolveIn`'s constant fork admits exactly the target's literal pipeline, a list
       whose every item is a literal or a bound parameter; every other list, a
       `CAST(NULL AS T)` item included, takes the runtime fork, an
       ArrayConstructorValue with item 2's non-null promotion, and nothing evaluates it
       at planning. A literal-pipeline list cannot hold a NULL (a bare NULL item is
       42809 before planning, and a bound NULL is a NullType constant whose array
       element is refused as item 2 says), so that fork needs no NULL check of its own.
   (b) InComparisonToExplodeRule is the target's rule (InComparisonToExplodeRule.java:
       120-176), matcher and output. It matches a SelectExpression whose quantifiers are
       all ForEach and replaces EVERY top-level IN conjunct (Go's rule stops at the
       first): each becomes a sibling ForEach quantifier over
       `ExplodeExpression(ArrayDistinctValue(comparand))` for a value comparand, over the
       literal list for a list comparison and over the parameter for a parameter
       comparison, and the conjunct becomes `value = QOV(explode)` AT THE SELECT, beside
       the other conjuncts, with the original quantifiers after the explodes. Nothing is
       evaluated at planning: the `IsConstantValue` guard and the `Evaluate(nil)`
       extraction go with the rule's Go-only shape, which matched a
       LogicalFilterExpression and pushed the equality into a new filter over the inner
       (rule_in_to_explode.go:57, 140-200). A record-typed IN operand never reaches the
       rule in either engine, so the target's record arm (:147-148, 204-224) has no Go
       counterpart to port: Go refuses it at the walk (expr.go:1639-1648, "a comparison
       operand of complex type (record) is not supported").
       ONE-SOURCE WHERE. Go's translator emits a WHERE over one source as a
       LogicalFilterExpression (cascades_translator.go:283-293) where the target emits a
       SelectExpression. The filter is exactly the one-ForEach-quantifier select
       `Select(QOV(q), [q], conjuncts)`, and its predicate list already IS the
       conjunction: `NewLogicalFilterExpression` lifts every top-level AND
       (logical_filter.go:32-55), as the target's SelectExpression constructor does, so
       the translator's single WHERE predicate is split before any rule reads it. The
       ported rule therefore has two matchers, a SelectExpression and a
       LogicalFilterExpression read as that select, and yields the target's
       SelectExpression for both, reusing the bound quantifiers as the target does
       (`transformedQuantifiers.addAll(bindings.getAll(innerQuantifierMatcher))`). The
       two matchers are two INSTANCES of the one rule type `InComparisonToExplodeRule`,
       the target's name, as 5.4(c)'s rule is (the prototype registered the filter arm
       under a second, Go-only name, `InComparisonToExplodeSelectRule`,
       wse8-explode-findings.txt:4, which disabling the rule by the target's name would
       have left on); a test pins that disabling `InComparisonToExplodeRule` turns off
       both.
       Yielding a select for a filter is safe HERE, unlike the predicate rule of 5.4(c),
       because of the next point.
       PHASE. The target registers the rule in PLANNING only (PlanningRuleSet.java:108).
       Go registers it in both phases today (DefaultExpressionRules, default_rules.go:118,
       which REWRITING runs, and PlanningExplorationRules, :179); the REWRITING
       registration is DELETED, so the REWRITING cost model, whose first rung counts
       SelectExpressions, never ranks an exploded select, and the REWRITING prune ranks
       only members the target's REWRITING also builds for this rule (none).
       TERMINATION. The target has no idempotency guard: its yield keeps no IN conjunct
       the rule admits, so the rule re-firing on its own yield yields nothing, and
       re-firing on the original yields a select the memo finds equal. Go's scan of the
       reference for an existing explode select (rule_in_to_explode.go:66-80) is deleted
       for the same reason. Measured: a prototype with no guard (below) planned all 2995
       corpus entries with the same 304 plan errors, 4 of them unpinned, as the tree
       without it, none a task-cap failure; a unit test asserts that PLANNING a group
       holding two IN conjuncts ends with exactly the members the target builds (the
       filter, the exploded select and its partitions; the rule runs in PLANNING only, so
       nothing of it exists before) and that planning it twice gives the same member
       count. The measurement covered Go's constant-list admission only, the prototype's;
       the port's admission (comparand, parameter and correlated sources) is run through
       the same corpus and the same unit test in the implementation gate, and a
       task-cap failure there is a defect of the port.
       THE IN-JOIN ROUTE, MEASURED. In the target the equality reaches the inner side
       through the planner's own partition rules, as a join predicate does, and
       ImplementInJoinRule (Go's is already the target's, rule_implement_in_join.go:
       18-80) implements what they produce. Go's PartitionBinarySelectRule declines any
       select with an UNCORRELATED explode leg (rule_partition_binary_select.go:90-106),
       a guard the target does not have, so with the target-shaped rule and that guard
       nothing partitions and every IN-list plan is lost. A prototype measured it
       (detached worktree of the tree `2964d0e40092d14b1689132b90d02ea844ccbf9f`, the
       rule above over both matchers, PLANNING only, admission kept to Go's constant
       lists so that only the route changes; findings in
       `/var/tmp/fdb-upgrade-recovery/wse8-explode-findings.txt`, dumps
       `wse8-explode-*.txt`, diffs `wse8-explode-*.diff`; the unchanged tree's dump equals
       `plan_shape.golden`): with the guard, 73 corpus plans change and every IN-list
       plan becomes `PredicatesFilter(Scan(...))`; without it, 5 change, and the IN
       lists keep their IN-join and IN-union plans. The guard's stated reason, that the
       table column of the equality is a flat FieldValue whose correlation names only the
       explode, is stale: the filter's IN operand reports the table's alias (a probe
       printed `corr=map[T:{}]` for `id IN (100, 200, 300)`). The guard is DELETED.
       ROWS, MEASURED (v8; v7 had plan dumps only). On the same prototype with the design's
       configuration (the guard off, the collapse deleted, the IN-union carrying every
       satisfying member and the rebased keys: `PROBE_PARTITION=1 PROBE_INUNION_ALL=1
       PROBE_REBASE=1`): the wrong-answer case the guard names, TestFDB_GroupByWithWherePush
       (its five subtests, `region IN ('east', 'west') ... GROUP BY ... HAVING` among
       them), passes (`/var/tmp/fdb-upgrade-recovery/wse8-explode-suites.log`, the run
       whose plan shapes show the PROBE_* flags reached the sandbox, the cq75 and
       nested_ins shapes; v8 cited `wse8-explode-groupby.log`, which shows no such
       witness); the nine
       yamsql scenarios holding the 26 changed corpus entries (in_with_join,
       in_plan_winner_stability, in_list_pushdown, delete_complex_where,
       in_expression_types, in_list_advanced, in_list_comprehensive, in_list_index_plan,
       in_over_primary_scan_sarg; 112 tests) return their pinned rows in every test, the
       runner checking rows before any plan assertion (`yamsql/runner.go`), and the 6
       tests that fail do so only on their `plan_contains`/`plan_not_contains` pins of the
       old shapes, the plan changes listed below (`wse8-explode-rows-yamsql.log`); and the
       whole `yamsql_test` and `sqldriver_test` targets under the same flags
       (`wse8-explode-suites.log`) fail only on plan pins: the same 6 yamsql tests, and
       three sqldriver subtests whose shape assertions predate the change,
       TestFDB_CompositeIndexZeroWidening `cq75_exact_in_negative_then_positive` and
       `_positive_then_negative` (`v IN (-0.0, 0.0)`, an all-duplicate list, now an
       IN-join over the covering probe, the collapse's deletion) and
       TestFDB_ConjunctionBinding `nested_ins` (the second IN exploded under a FlatMap,
       `in_plan_winner_stability.yaml#7`'s shape), whose rows were then run past the shape
       check and match its oracle (`wse8-explode-nested-rows.log`). The two
       zero-widening pins move with the change (the IN-join over the covering probe is
       the target's shape for a SQL list, and the dedup's equality below keeps the rows
       once). The `nested_ins` pin does NOT move: v8 moved it as a pin that "predates
       the change", but the prototype's FlatMap loses a sargable comparison (two IN
       lists over `idx_ab(a, b)` became an IN-join over `idx_a` with the second list a
       residual explode), and MEASURED in the target (round v9) the shape is two NESTED
       IN-joins over one two-column probe, `[IN arrayDistinct(...)] | INJOIN q0 -> { [IN
       arrayDistinct(...)] | INJOIN q1 -> { COVERING(IDX_AB [EQUALS q0, EQUALS q1]) ...
       } }` [nested_ins_explain], which is Go's plan today (`InJoin(InJoin(IndexScan(
       IDX_AB, [=, =] COVERING)))`, its GO line). Likewise two IN lists under an ordering
       the first's index provides: the target makes BOTH lists sources of one IN-union,
       `[IN ... ⋈ IN ...] INUNION q0, q1 -> { ISCAN(IDX_VAL [EQUALS q0]) | FILTER
       _.CAT EQUALS q1 } COMPARE BY (_.VAL, _.ID)` [two_in_ordered_explain], where Go
       today unions over the first list only with the second as a residual and the
       prototype explodes the second under a FlatMap.
       HOW THE TARGET BUILDS THEM, and why Go does not (v10, MEASURED on the prototype
       with a memo dump, `/var/tmp/fdb-upgrade-recovery/wse11-nested-memo*.log`; v9 named
       the NestedLoopJoin sibling guard, which cannot decide it: the guard refuses only
       an explode leg uncorrelated to the other leg, rule_implement_nested_loop_join.go:
       128-137, and the FlatMap's inner, a filter over the explode, is correlated). The
       target's ImplementInJoinRule and ImplementInUnionRule each match ONE
       predicate-free select holding every explode quantifier and one inner quantifier,
       and nest or union all of them in one firing over the inner's plans
       (ImplementInJoinRule.java:114-164, ImplementInUnionRule.java:99-163). That select
       comes from PartitionSelectRule's lower={T} partition: a predicate correlated to
       both the lower and an upper that does not depend on the lower goes to the LOWER
       ("we can do it in lower", PartitionSelectRule.java:201-205; v11 cited :197-200), so the lower is
       `T | a = q0, b = q1`, correlated to both explodes, and the data access matches
       IDX_AB with both equalities. Go's port puts every such spanning predicate in the
       UPPER (rule_partition_select.go, the classifier's spanning arm, RFC-043's
       placement), so the memo holds no select over T with both equalities and no
       IDX_AB access with two comparisons: the target's member is never BUILT, it does
       not lose on cost (the prototype's memo holds only `IDX_AB [=, *]` accesses; its
       two-comparison member appears nowhere). Porting Java's arm as it stands was
       measured and is not the port: every spanning predicate moved lower broke 158
       tests of the three suites (EXISTS over joins, positional merges and the ordinal
       join build rely on RFC-043's upper placement), and moving the predicates whose
       upper aliases are all explodes blew the rowdiff lane's seed 17 (a three-way join
       with an IN on one leg) from 42,723 planner tasks to 382,371, past the 150,000
       budget (54F02), for the same final plan (v11: every count here is from
       `/var/tmp/fdb-upgrade-recovery/wse12-seed17-design-config.log`, the statement 54F02
       named at `wse11-explode-E-suites.log:23342`, planned with the budget raised to
       2,000,000 so each configuration's own count is read; the configurations are
       named in `wse12-proto-configs.txt`, run by `wse12-proto.sh`, whose source md5s
       were checked after the run: the explode port as designed 42,723, the variant
       above 382,371, the port below 43,919, and with the guard change below 44,932; v10
       quoted the first three with no record behind them). THE PORT (Go's scoping of
       Java's arm, declared in DIVERGENCES.md "A spanning predicate goes to the lower only
       in an explode partition (RFC-257 WS-E)" and at the rule's arm in
       `rule_partition_select.go`): a spanning predicate goes to the lower when EVERY
       upper quantifier of the partition is an explode, the IN-join and IN-union shape,
       and its upper aliases do not depend on the lower, as in Java
       (PartitionSelectRule.java:201-205); every other partition keeps RFC-043's
       placement. The RFC-043 comment at the arm (rule_partition_select.go:503-506, "its
       'can do in lower' branch would push a predicate referencing an absent upper alias
       into the lower") is rewritten with the arm: it names the explode partition as the
       case where the branch applies, and why the others keep the upper placement. Measured with it (the ARM configuration): `nested_ins` plans
       `InJoin(InJoin(IndexScan(IDX_AB, [=, =] COVERING)))`, the target's shape; seed 17
       takes 43,919 tasks (+2.8%) and plans as before; the plan-shape corpus changes in
       two entries against the explode port without it, `in_plan_winner_stability.yaml#7`
       and `in_with_join.yaml#2` (`wse11-explode-F.diff`; re-measured, `wse12-corpus-
       F-vs-ARM.diff` finds the ARM corpus identical to F's, all 2995 entries); and the
       three suites fail only on the plan pins the explode port already moves (the two
       zero-widening pins and the yamsql pins of the collapse deletion and the covering
       in-union, `wse12-suites-ARM.log`), once the executor runs a MULTI-BINDING
       IN-union. It did not: Go's `executeInUnion` refuses more than one binding
       ("multi-binding IN union (%d bindings) not yet implemented",
       executor_new_plans.go:1930), and with the partition arm a two-binding IN-union wins
       for `a IN (...) AND b IN (...) ORDER BY c` (TestFDB_OrderingLimitOracle's three
       `a_in_b_in` subtests and TestFDB_MetamorphicCompositePrimaryKey failed with it,
       `wse11-explode-F-suites.log`). The port executes it as the target does
       (RecordQueryInUnionPlan.java:150-175, :331-353): the product of the sources' sizes
       checked FIRST against the plan's maximum, `RecordCoreException` "too many IN
       values" above it (the target plans with `attemptFailedInJoinAsUnionMaxSize` 24,
       PlannerConfiguration.java:161, so 24 values run and 25 are refused, both pinned).
       For SEVERAL sources the check is on the PRODUCT (RecordQueryInUnionPlan.java:
       331-337). That is MEASURED, not only read (v12; v11 cited it from source): WS-F's
       oracle runs `col1 IN (5 values) AND col2 IN (5 values) ORDER BY col1, col2` over a
       two-column index, and the target plans the two-source in-union and fails with
       XXXXX "too many IN values" [w8_in5x5_rows], while a 4-by-6 product answers
       [w8_in4x6_rows]. Go answers the 5-by-5 query today [its GO pin, `OK [[1] [3]]`]. So
       step (5) makes Go refuse a query it answers today, as the target refuses it.
       DECLARED in CHANGELOG ("Changed: an ordered query over several IN lists whose
       value counts multiply past 24 fails with 'too many IN values'"). The two WS-F rows
       move to SAME at step (5), their owner there. The target computes the product in
       an `int` that wraps: 65536 × 65536 is 0, which it then treats as an empty
       product and answers with no rows. Go computes it saturating and refuses anything
       above 24, overflow included. That is a declared divergence, the target's defect not
       ported, pinned by a unit test of the product with a wrapping pair of sizes, which
       step (6)'s bound arrays make reachable from SQL. Two more boundary facts (v11 Graefe
       L6):
       - The size each source contributes is its size after deduplication, and Go's
         deduplication is `=` (above), which merges -0.0 and 0.0 where the target's
         `Object.equals` keeps both. So a list holding both zeros counts one less in Go.
         25 values including both zeros run in Go (24) and fail in the target (25). That
         is the declared signed-zero divergence reaching the size check, not a new one; the
         DIVERGENCES.md "Signed zero" entry names it, and a unit test pins the 25-with-both-
         zeros list running.
       - A resume under a differently bound array. The target's continuation records each
         child's position, and a resume re-evaluates the sources (`getValues(context)`)
         and does not check that they are the ones the positions were taken over, so it
         resumes a different list at the old positions. Go's multi-binding continuation
         records the product's size and each source's size. A resume whose re-evaluated
         sizes differ is refused with the invalid-continuation error that a continuation
         over another plan already raises, where the target would resume the other list.
         That is a Go-only refusal, declared in section 8, which never returns rows of a
         different list. A test resumes a two-source in-union with the second array
         rebound to another size and asserts the refusal.
       No IN-SUBQUERY explode reaches this
       check (v11 Graefe L8). Both engines refuse `col1 IN (SELECT …)` with 0AF00
       (ws-f-design.md 4.3 item 8, measured), so the in-join rule's QOV arm
       (rule_implement_in_join.go:627-628) is reached only by a bound array parameter's
       quantifier. That source's size is known only at execution, and an in-union over it is
       checked against 24 at open as the target's `InParameterSource` is. A bound array
       above 24 values fails with "too many IN values" in both engines, and a unit test binds
       25. The executor then runs
       nothing for a product of 0 (a genuinely empty source), the sole child with the
       continuation for a product of 1, and otherwise the Cartesian product of the sources' values, the FIRST source
       outermost, one child execution per combination bound with every alias, merged by
       the comparison keys through the union cursor. There is no concatenation arm: the
       target always merges (v10 said "or concatenated without them", which the cited
       method never does), and WS-F's F-7 deletes Go's unordered in-union arm and ports
       the size check (ws-f-design.md 4.3), so the multi-binding executor LANDS WITH OR
       AFTER F-7 and extends F-7's merging executor rather than the arm F-7 deletes; until
       then the product is unbounded, which is why it cannot land first. Measured on the
       prototype, the four tests then pass with their oracle rows.
       THE TWO-SOURCE IN-UNION (#7), MEASURED (v11; v10 called it a HYPOTHESIS whose first
       suspect was WS-F's D5 re-queue, and the control it cited ruled that out, as the
       Torvalds lens said: `cat = 20` is implemented and `cat = q1` is not, and the
       difference is the correlation to an explode alias, not admission timing). The
       compensation of the partition-built lower is the logical filter `cat = q1` over
       IDX_VAL's `[EQUALS q0]` access. `compensationSafeForYield` (planner.go) refuses it
       through its correlation half, `compensationResidualCorrelationSafe`, a Go-only
       guard (RFC-150 section 8, "defense-in-depth") that refuses a residual correlated
       to an outer alias the compensation's own probe does not feed; `q1` is such an
       alias, so the compensation is inserted final with no exploration and never
       implemented (the memo dump of every reference, `wse12-7all-in2.log`; with the guard
       bypassed by a probe flag the target's plan appears at once). The guard exists
       for one hazard, stated at it: a leg whose correlation lives only in its RESIDUAL is
       classified by the bound-prefix signal (`matchBoundPrefixIsCorrelated`) as
       uncorrelated, and realizing it as a standalone leg severs the join's correlation
       feed (0 rows). That hazard needs a probe with NO correlation; a compensation whose
       probe is already correlated is classified as a correlated inner, and its
       residual's correlations are its own predicates', visible on the filter to every
       placement decision. THE CHANGE (v12; v11 generalized the exception to "the probe feeds ANY alias", which admits
       the shape RFC-150 section 8 refuses): the guard admits an outer-correlated residual
       when the compensation's probe is correlated AND every outer alias of the residual is
       either fed by the probe (the existing per-alias exception, unchanged) or the alias of
       an EXPLODE quantifier, the iteration variable of an IN list or array that
       InComparisonToExplodeRule made. An explode alias is not a table row. The planner binds
       it through the IN-join or FlatMap over its explode, the same way it binds the probe's
       own explode aliases. A residual correlated to a TABLE alias the probe does not feed
       stays refused, which is RFC-150's per-alias property. The implementation carries
       the explode aliases on the planner, registered where the rule makes each explode
       quantifier, and not by searching the matched select, because the partition arm
       places the explodes in the UPPER select, and the matched lower does not hold them.
       The prototype measured that search finding none of them (CORRSAFE3 over the select
       alone, `wse13-superseded/wse13-7.log`, the #7 plan not built).
       MEASURED (v12 driver `wse13-proto2.sh`, records `wse13b-*`, configurations in
       `wse13b-configs.txt`; the prototype's source md5s checked after the run, 17 of 17 OK;
       its diff against `71ccd8cf8` is `wse13b-prototype.diff`), in the configuration of this
       design (ARM2: the explode port with the comparand arm of 4.1 "WHICH EXPLODE ARM", the
       partition arm, and the in-join rule aligned below; CS3b adds this guard; GUARD2 adds
       v11's instead):
       - `in_plan_winner_stability.yaml#7` plans the target's two-source IN-union,
         `InUnion(PredicatesFilter(IndexScan(IDX_VAL, [=]), [1 preds]), bindings=2, ASC)`,
         under CS3b and GUARD2 alike, with 22 admissions, each of the explode alias `q$14`
         (`wse13b-7.log`). Without either guard change it is the in-memory sort.
       - CS3b and GUARD2 give the same plans and the same task counts on every probe
         measured (#7, seed 17, the IN family below, and the four guard shapes).
       - The guard shapes, planning only, with every admission logged (`wse13b-guard.log`):
         - PIN 1 (`SELECT o.id FROM o, t, bb WHERE t.fk = o.id AND t.xb = bb.v`) and its
           control `TestFDB_CompositeJoinDrivesProbeSide` reach the widened branch under
           NEITHER variant (0 admissions). So PIN 1 does not test this change, and v11's
           reliance on its pass was vacuous (the v11 Torvalds lens suspected as much).
         - Storage's RFC-150 shape, `SELECT o.id, t.id FROM o JOIN t ON t.fk = o.id WHERE
           t.k IN (1, 2)` with an index on k and none on fk: GUARD2 ADMITS the table alias
           O onto the explode-probed leg, and CS3b REFUSES it (2 refusals each for two
           probe correlations). This is the shape the change must not admit.
         - The three-table form (probe tied to t's IN list, residual to p): both admit
           only explode aliases.

       The pins:
       - that RFC-150 shape and its three-table form as FDB row tests against a full-scan
         oracle (both nestings, with and without ORDER BY, resumed from a continuation);
       - a unit test per guard case: probe uncorrelated, residual correlated: refused;
         probe feeding the residual's alias: admitted; probe correlated, residual on an
         explode alias: admitted; probe correlated, residual on a TABLE alias it does not
         feed: refused;
       - an instrumented pin that the RFC-150 shape reaches the refusal (a counter the test
         reads, so it cannot pass vacuously as PIN 1 does);
       - the plan of #7 by name.

       RFC-150 section 8, the PIN 1 comment and the DIVERGENCES.md entry at
       `DIVERGENCES.md:3131-3143` are amended with the explode arm, and they keep "a table
       alias the probe does not feed is refused".
       PLANNING COST, MEASURED ON BOTH ENGINES (v12; v11 quoted seed 17 against the explode
       port's own count and not against today's tree, and its prototype explode used the
       constant arm, not this design's comparand arm, so its counts were of a source kind
       this design does not build). The target's planner task counts come from round v12's
       "planning cost" spec (`wsE12CostPins`, counted from its ExecutingTaskPlannerEvents,
       per phase); Go's are the prototype's `tasksRun`. The two engines' tasks are not the
       same units, so what compares is growth.

       | query (sweep schema) | target | Go today (NONE) | ARM1 (v11 prototype) | ARM2 | CS3b (this design) |
       |---|---|---|---|---|---|
       | 2 IN lists | 3,835 | 506 | 5,745 | 3,231 | 5,569 |
       | 3 IN lists | 19,366 | 2,170 | 39,533 | 10,435 | 28,337 |
       | 4 IN lists | 117,131 | 14,262 | 460,629 | 44,099 | 73,986 |
       | 5 IN lists | not finished in 2 min | 118,374 | not measured | 235,412 | 326,752 |
       | `id IN (…) AND id IN (…)` | 1,443 | 518 | 4,343 | 2,713 | 2,911 |
       | the and-idempotent pair | refused by the target | 22,848 | 127,421 | 20,281 | 26,548 |
       | #7 | 2,912 | not measured | 6,529 | 3,114 | 3,648 |
       | seed 17 | not measured | 23,459 | 43,919 | 43,283 | 44,136 |

       Notes on the table:
       - ARM1's 4-list count, 460,629 against the target's 117,131, is where v11's 54F02
         came from (v11 Torvalds H2).
       - Two divergences in Go's `ImplementInJoinRule` cause it, and both are on the
         production tree, not only in the prototype. Per-rule counts on both engines at
         four lists (`wse13-in4-rulestats.log` and, for each alignment alone,
         `wse13-in4-injoin.log`; the target's `WS-E12-COST-KIND` lines):
         - Go appends a PRESERVE requested ordering when the call carries none
           (`rule_implement_in_join.go:140-145`). The target yields only for the orderings
           the planner requested (`ImplementInJoinRule.java:112-160`).
         - Go repeats the whole memoization once per inner plan (`for range innerPlans`,
           `:187`), with a comment that it deduplicates. It does not: one pass builds 7,097
           in-join plans at four lists where the repetition builds 39,321. The target
           memoizes a partition's plans once per source ordering
           (`call.memoizeMemberPlansBuilder(innerReference, planPartition.getPlans())`).
         Aligning the first alone gives 84,691 tasks, the second alone 113,333, and both
         (ARM2) 44,099, from 460,629. They are part
         of step (5), ported line for line from the target's rule, with a unit test that
         counts the in-join plans of a four-list query.
       - The target runs no `PushInJoinThroughFetchRule` on any of these queries: it
         registers the rule for in-VALUES and in-PARAMETER joins only, and a SQL list's
         `arrayDistinct(promote(…))` source is a COMPARAND join. Go's rule already excludes
         the comparand source (DIVERGENCES.md, the InSourceKind entry). The comparand arm
         changes no count here, because the rule's task is scheduled and returns.
       - With this design, Go's growth per added list is ×5.1, ×2.6 and ×4.4 under CS3b
         (the third, fourth and fifth list), against the target's ×5.0 and ×6.0 for the
         third and fourth. So it is within 2% of the target's at the third list and below
         it at the fourth, and the four-list query costs Go 63% of the target's tasks.
       - Seed 17 (one IN list on a three-way join) costs +88% over today's tree, which v11
         did not report. It is the partition arm on a join, not the in-join rule (ARM1 and
         ARM2 agree), and it plans under the budget.

       THE BUDGET. Go's planner has a Go-only complexity tripwire, 150,000 tasks
       (`embeddedPlannerMaxTasks`, planner_options.go:232). The target's relational layer
       sets no task limit (no `setMaxTotalTaskCount` in fdb-relational-core). With this
       design a query with FIVE IN lists costs 326,752 tasks and trips it (54F02). Go plans
       it today in 118,374 tasks, 2.2 s, and the design's plan would take 6.2 s
       (`wse13b-walltime.log`, two runs each, load recorded). The target did not finish
       planning it within the invoker's two-minute timeout (`wse12-cost-cap2.log`, one
       run). DECIDED: the tripwire stays at 150,000.
       - Its purpose is to fail closed before a plan takes seconds, and the target, which
         has none, did not plan the query in two minutes.
       - Raising it to admit five lists would raise the worst case of every query in the
         family.
       - DECLARED in section 8's list and in CHANGELOG: a query with five or more IN lists
         over one table, which Go plans today, fails with 54F02 after step (5). This is an
         owner report item.
       - The metamorphic sweep fails on a 54F02 where it logged and returned
         (metamorphic_sweep_rewrites_fdb_test.go:85-91). Its both-error counts are pinned
         per rewrite, so a planning-budget failure can no longer read as equivalence (v11
         Torvalds H2).
       - The pins: the table's Go counts, as a plan-harness test per row asserting the
         count within ±5% and under the budget, and seed 17 at its measured count ±5%,
         not "under the budget" (v11's pin left 3.3x headroom). The NestedLoopJoin rule's sibling guard
       (rule_implement_nested_loop_join.go:117-138) stays, for what it decides: an
       uncorrelated explode is not joined as a plain leg. The 5 changes of the explode port
       without the partition arm (v8's measurement): `in_with_join.yaml#0`, `#1` and `#2` put the IN
       list on the join's probe (an IN-join driving `Scan(CUSTOMERS, [=])` or
       `Scan(ORDERS, [=])`, or the IN as a semi-join over the explode above the join,
       where it was a residual on one leg; with the arm, `#2` becomes `FlatMap(outer=
       Scan(ORDERS), inner=PredicatesFilter(InJoin(Scan(CUSTOMERS, [=]))))`, the IN-join
       inside the join's probe), `in_plan_winner_stability.yaml#7` explodes its
       second IN list too (`FlatMap(outer=InUnion(IndexScan(IDX_VAL, [=])),
       inner=PredicatesFilter(Explode(constant), [1 preds]))`; with the arm,
       `InMemorySort(InJoin(FlatMap(outer=IndexScan(IDX_VAL, [=]), inner=...)))`; neither
       is the target's two-source IN-union, whose derivation v11 measured: the
       compensation guard, below), and
       `in_list_pushdown.yaml#7` drops a `Distinct` over an InUnion of a primary-key
       probe per distinct value, which yields each key once; the rows of all five are
       asserted by their scenarios in the implementation gate.
       THE SINGLE-ELEMENT COLLAPSE of `x IN (v)` to `x = v` is DELETED: it is Go-only and
       the target keeps an IN-join for it [in_single_literal_explain: `[IN
       arrayDistinct(promote(@c7 AS ARRAY(LONG)))] | INJOIN q0 -> { SCAN([IS T, EQUALS
       q0]) ...`]. Measured on the same prototype: 10 corpus plans change, each a
       single-element or all-duplicate list moving from a probe to an IN-join or
       IN-union over the same probe (`delete_complex_where.yaml#4`,
       `in_expression_types.yaml#3`, `in_list_advanced.yaml#4`,
       `in_list_comprehensive.yaml#6`, `in_list_index_plan.yaml#1`,
       `in_list_pushdown.yaml#1`, `#2`, `#37`, `#38`, `in_over_primary_scan_sarg.yaml#4`).
       Two of them lost a COVERING scan, which leads to the next point.
       THE IN-UNION DOES NOT COVER IN GO, and deleting the collapse would spread that from
       multi-element to single-element lists, so it is fixed with it. Across the corpus
       at this tree no InUnion or MergeSortUnion runs over a covering scan (the unchanged
       dump has none), where the target plans `[IN ...] INUNION q0 -> { COVERING(...) }
       ... | FETCH` (join-with-order-by-tests.yamsql:707). Two causes, both measured with
       probes: ImplementInUnionRule bakes ONE cheapest spine-pinned member of the inner
       partition (rule_implement_in_union.go, the `FinalOf(pinned)` quantifier), where
       the target memoizes the partition's plans (ImplementInUnionRule.java:205,
       `memoizeMemberPlansFromOther(innerReference, planPartition.getPlans())`), so the
       `Fetch(covering)` member never reaches the InUnion; and once it does,
       PushInUnionThroughFetchRule hands the comparison keys to the fetch's translation
       over `_current` (rule_push_set_operation_through_fetch.go:204, and :163 for the
       merge-sort union), where the target rebases them onto the source alias first
       (RecordQueryInUnionOnValuesPlan.java:97-103, `rebase(AliasMap.ofAliases(
       Quantifier.current(), newBaseAlias))`), so the covering translation, which admits
       only a field of the source alias, refuses every key (the probe printed `corr=
       map[_current:{}] ... ok=false`). The fix is both, and its first half is WS-F's F-7,
       stated in F-7's terms (v12; v11 stated it as "every member that satisfies the
       leg's ordering, spine-pinned", which is the admission F-7 replaces,
       ws-f-design.md 4.3):
       - The InUnion's inner is the partition's plans, `planPartition.getPlans()`, memoized
         by F-7's memoizer (`ImplementationRuleCall.MemoizeFinalExpressionsFromOther`),
         exactly as ImplementInUnionRule.java:205 does. WS-E adds nothing to it and pins no
         spine.
       - The comparison keys are rebased
       from `_current` onto the source alias with `values.TranslatePhaseRoot` (Go's
       AliasMap refuses current-to-named by design, alias_map.go:62) before translation,
       for the InUnion and the merge-sort union alike. That second half is WS-E's own.
       Measured with both on the prototype, over the PRE-F-7 admission the prototype
       has (`PROBE_INUNION_ALL`, every satisfying member): 14 further corpus plans change, every one an IN-union that now runs
       over `IndexScan(..., [=] COVERING)`, bare where the projection is covered and
       under a `Fetch` where it is not (`in_list_index_plan.yaml#0`, `#1`, `#2`,
       `in_list_pushdown.yaml#34`, `#35`, `#36`, `#37`, `#38`, `#41`, `#42`, `#44`,
       `in_over_primary_scan_sarg.yaml#14`, `#15`, `in_plan_winner_stability.yaml#6`),
       the two collapse losses among them. These counts, #7's plan, the 14 covering
       changes and `a_in_b_in` were all measured on that pre-F-7 admission. So step (5),
       which lands after F-7, RE-MEASURES each of them on F-7's admission before its gate:
       the corpus diff against F-7's tree, and the named plans by name. A difference from
       the numbers here is reported with its cause, not absorbed.
       A COMPARAND CORRELATED TO THE SELECT'S OWN ROW is exploded like any other, as the
       target does: `n IN (id + 4, 999)` plans in the target `SCAN([IS T]) | FLATMAP q0 ->
       { EXPLODE arrayDistinct(array(q0.ID + @c10, promote(@c12 AS LONG))) | FILTER q0.N
       EQUALS _ ... }` and answers [[3]] [in_own_column_item_where,
       in_own_column_item_explain]. The explode depends on the scanned row, so it can
       only be the inner side of a dependent join, never an IN-join source (c). WHICH
       PLAN WINS IS A TIE IN THE TARGET (SOURCE: read from the target's cost model, not
       measured; the target's answer is measured, its plan choice is not): the dependent
       explode and the residual filter
       over the scan have one residual conjunct, one data access and one
       PredicatesFilter each, and the FlatMap is not among the plan classes the model
       counts (PlanningCostModel.java:80-89, 147-283), so the target decides by its
       planHash rung (:334-339). Go, measured on the prototype with the correlated arm
       added (`ExplodeExpression(ArrayDistinctValue(comparand))`), builds the same
       dependent explode (`FlatMap(outer=Scan(T), inner=PredicatesFilter(Explode(
       array_distinct), [1 preds]))`, seen with the filter's implementation disabled)
       and picks the residual filter over the scan on its EstimateCost rung, the Go
       extension before its hash (planning_cost_model.go:397-410): with that rung
       neutralized the hash picks the dependent explode, and with the hash then
       inverted, the filter. So v6's "after the port it plans the dependent explode" was
       wrong: the row is a tie row, compared with those of section 5.4(g), its Go plan
       (the residual filter) and answer pinned per schema, and the deciding Go rung named
       in the pin.
   (c) The IN-join and IN-union sources are the target's, admitted by its rule and no
       other (ImplementInJoinRule.java:385-432, `isSupportedExplodeValue` and
       `computeInSource`): a LiteralValue list is `InSourceValues` (extracted at
       planning, sorted there for a SORTED source); a QuantifiedObjectValue, the
       explode of an outer row's value, is `InSourceParameter` over its alias; a
       ParameterObjectValue is `InSourceParameter` over its name; and a value that
       `isConstant()` (Value.java:165-168: NO correlation at all and no nondeterministic
       value in its tree) is `InSourceComparand`, the target's InComparandSource and
       SortedInComparandSource. Anything else is not an IN source and the explode is
       planned as the inner side of a dependent join: `classifyInSourceKind`'s default
       (rule_implement_in_join.go:575-584), which sends every non-constant value to
       `InSourceValues` and so to `extractInValues`, where a FieldValue evaluates to NULL
       with no error, is replaced by that admission, and `isSupportedExplodeValue`
       (:611-635) becomes the target's four-arm test, KEEPING its first arm (v12; v11 said
       the four-arm test replaces the function, which would have deleted it): an
       ARRAY<RECORD> explode is refused as an IN source. That refusal's comment records the
       silent 0-row defect it closes (a record-valued explode bound as a name-keyed map, whose
       correlated child reads NULL). The target admits such a source, but Go's record binding
       does not read it the target's way, so the refusal stays, declared as a Go-only arm at
       the function. A unit test feeds an ARRAY<RECORD> explode and asserts it is not an IN
       source, and the FDB test of inline VALUES keeps its rows. Go's `InSourceComparand` evaluates
       its comparand when the plan OPENS, under the evaluation context's outer bindings,
       deduplicated (ArrayDistinctValue) and, for a sorted source, sorted by the same
       `sortInJoinValues` order the values source uses; a resume re-evaluates it, as the
       target's `getValues(context)` does, and the continuation's position check is
       unchanged. The comparand is evaluated against the plan's non-nil evaluation
       context, never a nil one, so a correlation the context does not bind is the
       existing `UnboundEvalContextError` (values.go:660-678), never a NULL: the
       admission makes that unreachable, and evaluating under a real context makes a
       regression loud instead of a `b IN (NULL, 999)` answer; a unit test feeds the
       source a comparand with a foreign correlation and asserts that error. `extractInValues` serves `InSourceValues` only.
       THE IN-UNION TAKES THE SAME SOURCES (v8; v7 named only the IN-join). Go's
       ImplementInUnionRule extracts every source at PLANNING, calling `Evaluate(nil)`
       on each explode's collection value and dropping one that errors
       (rule_implement_in_union.go:217-235), and `executeInUnion` iterates only those
       plan-time lists (executor_new_plans.go:1837-1890). Both are converted with the
       IN-join: the IN-union's sources are the four admitted kinds above (Java's
       ImplementInUnionRule takes them from the same `computeInSource`), a values source
       is the literal list, and a comparand source is evaluated when the plan opens,
       under the evaluation context, deduplicated and, for the union's sorted merge,
       sorted by `sortInJoinValues`; the plan carries the source, not a pre-evaluated
       slice, and `executeInUnion` evaluates it at open as `executeInJoin` does. The
       PLAN side moves with it: the in-union plan's identity, semantic hash and copy fold
       the pre-evaluated slice today (in_union.go:39, 225-242, 304-326), and they fold the
       source instead (its kind and its literal list or comparand Value, as the IN-join
       plan's do), so two plans over the same comparand hash alike whatever it will
       evaluate to. The target's size check at open (RecordQueryInUnionPlan.java:151-153,
       "too many IN values"), which Go has never enforced (`GetMaxSize` has no executor
       reader), is WS-F's (its section 4.3 ports the check and its configuration); this
       conversion evaluates the source where that check will read it. THE COST of a
       comparand source: its size is unknown at planning, and `LiteralFanout` reports it
       as it reports a source unavailable at planning, a nil dimension, unknown
       (in_union.go:247-287); that is the target's reading, which gives an
       InComparandSource or InParameterSource `unknownMaxCardinality`
       (CardinalitiesProperty.java:439-461), and Go's ProvenCardinalities gives the same
       unknown maximum (cardinality_bounds.go:344-347), while the cost estimate
       substitutes ten for the dimension (cost.go:1121-1140, a Go term the target's cost
       model does not have). Today the same source is dropped at planning and read as a
       known-EMPTY dimension, an exact zero, the cheapest plan with a false zero-row
       proof; after the change it ranks as unknown. The executor's one-combination fast
       path (executor_new_plans.go:1866) reads `LiteralFanout`; it reads the evaluated
       sources' sizes at open instead, so a comparand that evaluates to one item takes
       it. Tests: the IN-union
       rows of (b) with a runtime (CAST) item, which today's plan-time extraction drops
       to an empty source, the resume of a sorted IN-union over a comparand source, and
       two in-union plans over one comparand hashing equal.
       WHICH EXPLODE ARM A SQL LIST TAKES (v8). The target's rule has two: a
       ValueComparison explodes `ArrayDistinctValue(comparand)`, deduplicated, and a
       ListComparison explodes the literal list as it is, NOT deduplicated
       (InComparisonToExplodeRule.java:162-178). A SQL IN list reaches the target's rule
       as a ValueComparison over an array (the literal pipeline's `promote(@c7 AS
       ARRAY(LONG))`, or `__internal_array`; [in_single_literal_explain] shows
       `arrayDistinct(...)`), and only a record-layer API query builds a ListComparison.
       Go's constant fork emits `ConstantValue{list, TypeUnknown}` (expr.go:1790-1793),
       which names neither; it is given its array type, ARRAY of the list's element
       type, so both of `ResolveIn`'s forks reach the ported rule as a value comparison
       and take the ArrayDistinct arm, as the target's SQL lists do, and the list arm
       serves only a record-layer `ListComparison`. Without that a duplicate item would
       multiply rows wherever the explode is not the IN-join source, as the inner of a
       FlatMap (`in_plan_winner_stability.yaml#7`'s shape): v7's prototype deduplicated at
       planning (`Evaluate(nil)` and `distinctInListValues`,
       rule_in_to_explode.go:145-155), which (b) deletes. THE DEDUP'S EQUALITY is the
       predicate comparison's `=` as `ComparisonEquals` evaluates it
       (predicates/comparisons.go:531-557): `deepValueEqual` when either item is a
       composite, an array or a record (:524-544, element by element and field by field,
       :803-840), and `cmpAny` returning 0 otherwise (:611-740); two NULL items are one
       item, since each matches nothing (v9 named `cmpAny` alone, which answers ok=false
       for a composite, :739, so an array list would never have deduplicated); a
       temporal item is a DATE or TIMESTAMP value holding canonical text (4.3), so its
       `=` is the text's and that is the instant's. It is not Go's `==` as
       `ArrayDistinctValue` has it today
       (value_array_distinct.go:61-110) and not the target's `Object.equals`
       (ArrayDistinctValue.java:102): an item the list keeps is a probe, and each probe
       answers the rows its `=` matches, so two items that `=` calls equal must be one
       item or a row comes back twice. Go's `=` treats -0.0 and 0.0 as equal (IEEE, the
       declared signed-zero divergence, DIVERGENCES.md "Signed zero") and, like the
       target, NaN as equal to NaN (the total order, DIVERGENCES.md "NaN comparison
       follows Java's total order"); `==` split the NaNs (two probes) and merged the
       zeros, and `Object.equals` would split the zeros (two probes, each widened to
       both zeros by the index binder, TestFDB_CompositeIndexZeroWidening). MEASURED in
       the target (round v9, F with an index on f and G without, rows 0.0, -0.0, 1.5 and
       NaN): `f IN (-0.0, 0.0)` answers [[1] [2]] over both, as Go does; `f IN (0.0,
       0.0)` answers [[1]] where Go answers both zeros, and `f = 0.0` [[1]] and `f = -0.0`
       [[2]] where Go answers both, which is the declared signed-zero divergence and not
       new; `f IN (NaN, NaN)` and `f = NaN` answer [[4]] over both tables
       [f_in_signed_zeros_index_where, f_in_signed_zeros_rows_where,
       f_in_zero_twice_index_where, f_eq_zero_index_where, f_eq_negative_zero_index_where,
       f_eq_zero_rows_where, f_eq_negative_zero_rows_where, f_in_nan_twice_index_where,
       f_in_nan_twice_rows_where, f_eq_nan_index_where, f_eq_nan_rows_where]. Go answers
       each of those as the target does except the signed-zero rows, and except
       `f IN (NaN, NaN)` over the index, which fails ("scan comparison 0 ... evaluates
       to NaN; exact indexed NaN equivalence is unsupported", scan_range_binding.go:35):
       the IN-join's probe is a NaN, and the binder refuses a NaN equality because NaN
       has many bit patterns, packed apart (DIVERGENCES.md, "every NaN packs to the
       same key" is false). That refusal is a failure where the target answers, found
       by this round, and it is fixed with the dedup, since the IN-join is the path that
       reaches it. v10 states the fix against the decision it revises: RFC-208 scoped
       exact ranges to non-NaN values because "selecting an exact suffix across every
       payload cannot be represented" (rfcs/208-*.md:111-117), required that an equality
       that can fan out not be treated as physically fixed for ordering (:512-527), and
       made the binder correct-or-loud (scan_range_binding.go:17-23); v9 reversed it for
       one case without citing it. MEASURED in the target (round v10, FG with an index on
       (f, g) and rows (NaN, 2), (NaN, 1), (1.5, 1), (NaN, 3)): `f = NaN AND g = 1`
       answers [[2]] through `FG_FG [EQUALS CAST(@c9 AS DOUBLE), EQUALS promote(@c16 AS
       LONG)]`, `f IN (NaN) AND g IN (1, 2)` answers [[1] [2]] through two nested
       IN-joins over `[EQUALS q0, EQUALS q1]`, and `f = NaN ORDER BY g` answers [[2] [1]
       [4]] through `FG_FG [EQUALS ...]` with no sort [nan_then_eq_*, nan_in_then_in_*,
       nan_order_by_suffix_*]: the target probes the packed bit pattern of its NaN and
       takes the probe as one key, fixed for ordering, so its index answer misses a
       stored NaN of another payload. Go today answers the same rows through a full scan
       and a filter, and sorts for the ordering (their GO lines), and fails the IN-join
       over the index (round v9).
       WHICH NaN BITS EACH ENGINE WRITES (v11; v10 said every NaN a statement produces is
       `CAST('NaN' AS DOUBLE)`'s one pattern, which is false across the engines): the
       target parses with `Double.parseDouble`, whose NaN is `Double.NaN`,
       `0x7ff8000000000000`, and Go parses with `strconv.ParseFloat`, whose NaN is Go's
       `math.NaN()`, `0x7ff8000000000001` (values.go, the CAST's DOUBLE string arm;
       streaming_cursors.go:275-276 says so, and embedded/utilities.go:19-27 keeps the
       parameter text transport's two spellings on it), and neither the write converter
       nor the tuple packer canonicalizes, so the same INSERT writes different record
       bytes and index keys in the two engines: the target's index probe misses a NaN
       row Go wrote, and a UNIQUE index admits one NaN from each engine. That is a WRITE
       divergence, and it is fixed, not declared: Go's CAST of a string to DOUBLE yields
       `0x7ff8000000000000` for a NaN, and to FLOAT `0x7fc00000` (`Float.parseFloat`'s
       `Float.NaN`), the target's bits. It lands in section 8's step (6) WITH the
       parameter binding, because the text transport renders a bound `math.NaN()` as
       `CAST('NaN' AS DOUBLE)` today and would change that value's bits if the CAST moved
       first; with binding, a bound NaN is a DOUBLE constant carrying its own bits, and
       the transport and its two-spelling table are deleted. Until the step lands,
       DIVERGENCES.md records the write divergence as PENDING ("A NaN made by CAST from a
       string has Go's bits, not Java's").
       EVERY NaN PRODUCER on the shared SQL surface (v12; v11 said "a NaN from arithmetic or
       a bound value carries whatever bits its producer gave it in both engines", which is
       false for Go's MIN and MAX). The census is `git grep -n 'math.NaN()'` over the
       non-test files of pkg/recordlayer/query/executor, pkg/recordlayer/query/plan/cascades/
       values and pkg/relational. At the v12 tree it gives three code sites, and `math.Inf(`
       over the first two directories gives two lines as the positive control:
       - `javaMinF64` and `javaMaxF64` (streaming_cursors.go:2157-2168), the MIN and MAX
         aggregates over DOUBLE, return `math.NaN()` for any NaN operand. The target's
         MIN_D and MAX_D are `Math.min` and `Math.max` (NumericAggregationValue.java:688,
         693), which return the NaN OPERAND (`if (a != a) return a`, and `a <= b` false
         returns b). MEASURED by round v12 (Describe "WS-E target oracle v12"): over a
         column holding the division's NaN, the target's `INSERT INTO U SELECT 10, MIN(d)`
         and `MAX(d)` store `fff8000000000000` [java_u_d_id10, java_u_d_id11], and Go's
         store `7ff8000000000001` [go_u_id10, go_u_id11]. Fixed in step (6): both become
         Java's `Math.min`/`Math.max` line for line, the signed-zero arms included, and
         the round's pins flip to the target's bits.
       - `toFloat64`'s default arm (executor.go:3831) is reached only for a carrier that is
         not numeric. Its one caller (streaming_cursors.go:731) runs after `isNumeric`,
         which admits exactly the numeric carriers, so the arm is unreachable. It becomes
         an error return in step (6), so a future caller cannot make a NaN from a type
         error.
       - The CAST of a string (values.go, the DOUBLE and FLOAT string arms) is the parser
         below.
       THE CAST'S GRAMMAR is `Double.parseDouble`'s and `Float.parseFloat`'s, not only its NaN
       bits. MEASURED by round v12 over 22 spellings: the two engines disagree on 11 of them
       [cast_00 to cast_21 and their stored bits]:
       - The target ACCEPTS a signed NaN (`-NaN`, `+NaN`, each stored as
         `7ff8000000000000`), `+Infinity`, the `d`, `D`, `f` and `F` suffixes, and `1e400`
         as +Infinity. Go refuses each: `strconv.ParseFloat` rejects the sign and the
         suffixes, and reports `1e400` as a range error.
       - The target REFUSES `nan`, `NAN`, `inf`, `infinity` and `1_000` (22F3H, "For input
         string"). Go accepts each; `strconv.ParseFloat` accepts the lower-case spellings
         and Go's digit separators.
       - The two agree on `NaN`, `Infinity`, `-Infinity`, a padded ` 1.5 `, the hex
         floats `0x1p3` and `0x1.8p1`, `1e-400` (as 0), `.5` and `5.`.
       Step (6) ports `FloatingDecimal.readJavaFormatString` as the one parser behind both
       CASTs: Java's whitespace trim; an optional sign; `NaN` or `Infinity`, exactly those
       spellings; the decimal and hex forms with an optional `f`, `F`, `d` or `D` suffix; an
       out-of-range magnitude rounded to ±Infinity or ±0 rather than refused. Every NaN it
       returns is the canonical `0x7ff8000000000000` (FLOAT: `0x7fc00000`). The round's
       CAST pins flip to the target's outcomes. A unit test drives every row of the round,
       plus boundary cases (`Infinityx`, `NaNd`, an empty string, a lone sign), each first
       added to round v12 so its expected outcome is the target's measured one.
       MEASURED by a new
       round (Describe "WS-E target oracle v11", section 0, 10 pins, in the
       `rfc257_oracle_test` target): the target inserts `CAST('NaN' AS DOUBLE)` with
       `CAST('NaN' AS FLOAT)`, and `0.0 / 0.0` with `0.0f / 0.0f`, into a kept store with
       an index on each column, and the raw index entries give the stored bits: the CAST
       stores the canonical quiet NaNs, `7ff8000000000000` and `7fc00000`; the division
       stores the hardware's NaN, the NEGATIVE quiet NaN `fff8000000000000` and
       `ffc00000` on the x86 machine measured (so a NaN from arithmetic sits in the
       negative NaN key range, before every other value, which the two NaN ranges below
       cover). Go's evaluator, read through its driver on a Go-created schema, gives
       `7ff8000000000001` for the CAST to DOUBLE (the divergence), the canonical bits for
       the CAST to FLOAT (read back widened, `7ff8000000000000`), and the target's bits
       for the division. The round also measured that the target refuses `0.0 / 0.0`
       into the FLOAT column (22000, "cannot be promoted"), where Go stores it: the
       assignment lattice below, fixed in the same step (6). After step (6) the CAST pin
       flips to the target's bits, and round v12's cross-engine spec flips with it (v12:
       that row exists now, pinned to today's outcome, Describe "WS-E target oracle v12
       cross-engine NaN"): Go's record layer writes the NaN Go's CAST made into a store
       the target created with a UNIQUE index, and today the target's covering probe of
       that index for its own NaN answers no rows [x_java_probe_rows] and the target's
       insert of its NaN beside it succeeds [x_java_insert_nan_beside_go_row], two NaN
       entries under UNIQUE [x_t_d_id1, x_t_d_id2]. After step (6) the Go-written row is
       found by the target's probe, and the target's insert fails with 23505.
       THE PORT: an equality at float component k whose comparand EVALUATES to NaN when
       the scan is bound (a literal is not folded at planning, 5.4(b), so every NaN is a
       binding-time value, whether a CAST constant, a parameter, an explode's item or a
       correlated probe) maps to the two key ranges that hold every NaN, built by the
       binder's existing NaN-block builder (scan_range_binding.go:832-852): the
       negative-sign NaNs below -Inf and the positive-sign NaNs above +Inf in the TUPLE
       order (v9 named `values.CompareFloat64`'s order, which is not it: that order
       collapses every NaN above +Inf, coercion.go:33-40, and the tuple order differs,
       scan_range_binding.go:820-824). The PLAN is the target's: the scan keeps every
       comparison the planner gave it, `[EQUALS q0, EQUALS q1]` for `nan_in_then_in`,
       because the planner cannot know a NaN is coming (v10 said the later comparisons
       become a plan-level residual, which exists only for a value known at planning,
       and there is none). A comparison after component k cannot follow two ranges of
       arbitrary payloads, so the BINDER turns it into a KEY FILTER: the bound scan is the
       two NaN ranges over the prefix before k, plus a predicate on each index entry that
       every later component satisfies its comparison, equalities and the range tail alike,
       evaluated on the decoded key column with the comparison's own `=` and order (the
       predicate comparison's `Eval`, NaN-equal and zero-widened as everywhere else,
       recursively, so a second NaN component is filtered by the same rule); the refusals
       at scan_range_binding.go:988 and :1045 of a tail after a non-exact component
       become that filter. The index cursor applies it BELOW the continuation: the
       continuation is the key of the last entry READ, filtered or not, so a resume never
       re-reads a rejected entry and never skips an unread one; every entry read counts
       toward the scan's limits (the scanned-record and byte limits), as a filtered read
       does anywhere; a reverse scan reads the positive-NaN block first.
       THE BINDER'S FOUR CALLERS (v12; v11 specified the value-index cursor only, while the
       refusal it replaces lives in the shared binder, `bindScanComparisonsToRangeSet`,
       which four scans call). Each gets its own arm, and each arm is tested:
       - A VALUE-INDEX scan (executor.go:444): the two NaN ranges and the key filter above.
       - A PRIMARY-KEY scan (executor.go:320): the same two ranges over the records
         subspace. The key filter runs on each RECORD's primary key, decoded from the
         first key of the record's key-value group before any split chunk is assembled or
         the value is parsed. The version suffix and split suffixes are skipped with the
         group, as the record cursor groups them today. The continuation is the primary
         key of the last record READ, filtered or not, so a resume never re-reads a
         rejected record. A float primary-key component takes this arm (v11 Torvalds L8).
       - An AGGREGATE-INDEX scan (executor_new_plans.go:104) KEEPS the loud refusal. Each
         NaN bit pattern is its own group entry of an aggregate index: round v11 measured
         two patterns written by SQL, and round v12 a third, MIN's. A NaN range would
         return one "NaN" group per pattern, while Go's streaming GROUP BY puts every NaN
         in one group (streaming_cursors.go:268-280), so the answer would depend on the
         plan. Merging the pattern entries would re-aggregate inside a scan that returns
         stored aggregates. The target answers from its probe's one entry. DECLARED in
         section 8: Go refuses an aggregate-index read bound to a NaN group key where the
         target answers that one entry.
       - A VECTOR-PARTITION prefix (executor.go:765) KEEPS the refusal it already has for a
         terminal widening, because each partition prefix is its own graph and a NaN range
         cannot be opened as exact partitions. DECLARED likewise.
       Tests: one NaN equality per caller, the first two answering the NaN rows and the
       last two refusing with the binder's error, and a primary-key NaN scan resumed from
       a continuation inside each NaN range over split records. The scan-range
       fingerprint the continuation carries folds the bound comparands, a NaN folded as
       the NaN class with its payload ignored, so a resume under another NaN payload is
       the same scan. Ordering: the equality is NOT physically fixed, as RFC-208 requires
       of an equality that can fan out, and as Go's plan already treats every dynamic
       float equality (ordering.go under plans/, the non-constant arm, :491-493, and
       `EqualityPinsSinglePhysicalKeyOnColumn`, plans/ordering.go:534-537, whose `n != 0`
       test answers true for a NaN constant today and answers false for one after this
       change), so `ORDER BY g` over `f = ?` sorts in Go where the target, which fixes its
       one-key probe, does not: a declared plan difference, with equal rows. The seek
       bound: `PhysicalEqualityShape` documents a dynamic terminal equality as one seek
       "for a non-NaN execution" (properties/physical_equality_shape.go:20-24, :416-429);
       it becomes at most two range seeks for a float component, and `UnsupportedKnownNaN`
       (:389-392) becomes the two-block shape; the component's multiplicity stays
       unknown. A UNIQUE index gives no one-row proof for a float equality, since it can
       hold NaNs of two payloads (MEASURED: two NaNs of one engine's bits are one key,
       23505 on the second insert [unique_nan_second_insert], and the lookup answers [[1]]
       [unique_nan_eq_where]). DECLARED (section 8's list): Go's indexed NaN equality
       returns every stored NaN, as its per-row `=` does, where the target's index returns
       the NaNs of its probe's bits; after step (6) both engines write the same bits for
       every NaN a CAST produces, so rows differ only for NaNs of other payloads (from
       arithmetic, a bound value or the record-layer API), and plans differ where the
       target claims order. Tests: `id IN (1, 1, 2)` and `n IN (3, 3)`, and the round-v9 and
       round-v10 float rows (±0 and NaN lists, one item and two), through the per-row IN,
       the IN-join, the IN-union (resumed from a continuation mid-list, and resumed
       inside each NaN range) and a FlatMap-inner explode, each row returned once, the
       EXPLAIN showing `arrayDistinct`; the composite and nested NaN probes of round v10
       with the NaN arriving at run time (a parameter and an explode item), their rows,
       the plan keeping `[EQUALS, EQUALS]`, and the sort for `ORDER BY g`; the key filter
       with a range tail after the NaN component, with a second float component that is
       also NaN, resumed inside each block, forward and reverse, and with a scan limit
       that expires on a filtered entry (the continuation past it, nothing lost); an
       indexed NaN equality over stored NaNs of two bit patterns (Java's and Go's, written
       through the record layer), both returned, and the same over a UNIQUE index, both
       returned and no one-row plan; and a unit test of the binder's filter per comparison
       type. THE SIGNED ZEROS AND THE CONTINUATION (the half of
       the v8 storage finding v9 left open): a list holding both zeros is one IN-union
       child in Go, whose probe the binder widens to both zeros, and two in the target,
       whose `Object.equals` keeps them apart, so the continuation's shape differs too:
       Go's one child receives the continuation as it is (the size-one path,
       RecordQueryInUnionPlan.java:150-175 ported), the target's two children a
       UnionContinuation of two. No token crosses the engines: SQL continuations are
       engine-private and an externally supplied one is refused
       (DIVERGENCES.md, "Go SQL tokens are ENGINE-PRIVATE",
       TestOptContinuation_RejectsLoudly), so a resume is always Go's plan over Go's
       token; this is part of the declared signed-zero divergence, and the test resumes
       Go's IN-union over `f IN (-0.0, 0.0)` inside its child and asserts each row once.
       THE REBASED KEYS (v8). The target rebases the IN-union's comparison keys from
       `_current` onto the source alias and then SIMPLIFIES them
       (`DefaultValueSimplificationRuleSet`, RecordQueryInUnionOnValuesPlan.java:98-101),
       which folds a field taken from a record constructed over the base back to a field
       path. Go's keys at this site are the ordering's key values, which Go builds as
       field paths over `_current` (`FieldValue` chains, never a field of a record
       constructor), and `values.TranslatePhaseRoot` rebases a field path to the same
       path over the source alias, already in the form the simplification would produce;
       so Go does not port that simplification here. A unit test asserts, for every
       IN-union and merge-sort union of the corpus, that each rebased key is a field path
       over the source alias; a key that is not is refused by the covering translation
       (`ok=false`, the plan then fetching), the fail-closed outcome it has today, never a
       wrong translation.
   Every row above is then answered by the same evaluation in both engines, EXPLAIN
   included: v4's declaration that Go refuses to explain such a statement is
   withdrawn. A bound NULL parameter in a list is a constant item (section 4.3), so
   `IN (?)` bound NULL fails the same way [prepared_in_typed_null,
   prepared_in_object_null]. Tests: every row above as a Go assertion (the EXPLAIN rows
   asserting the explode and IN-union shapes), plus a list whose NULL item is the only
   element, the last element, and a parameter; a correlated list (`b IN (a, 999)` and
   the oracle's `n IN (id + 4, 999)`) with NO IN-join plan anywhere in the tree
   (asserted on the typed plan, not its text), with its rows, its Go plan pinned as the
   tie row of (b) with the deciding rung named, and the dependent explode asserted to be
   in the group (the plan with the filter's implementation disabled, through
   `PlanQueryForTestWithDisabledRules`), executed against FDB with its rows so the
   dependent explode's execution is covered even though the tie does not pick it; a list
   correlated to an outer row (a correlated subquery's `x IN (o.a, 1)`) likewise; each
   arm of the IN-source admission, the refusal of a correlated value included, as a
   unit test of the rule; two IN conjuncts exploded together, and the member-count
   termination test of (b); the rule's absence from REWRITING's rule list and presence
   in PLANNING's; the partition of an uncorrelated explode leg, both orderings, as a
   unit test of PartitionBinarySelectRule, and TestFDB_GroupByWithWherePush with the
   guard gone; the 26 distinct corpus entries of (b) (5 + 10 + 14 = 29 listed changes,
   of which in_list_index_plan.yaml#1 and in_list_pushdown.yaml#37 and #38 are in both
   the collapse and the covering lists), each with its scenario's rows, the goldens
   and pins revised and named with the row that justifies them; an InUnion over a
   covering index and an InUnion under a Fetch, each with its rows and its comparison
   keys resolved against the covering layout, one of them resumed from a continuation;
   a mutation of each of the two InUnion fixes (the single pinned member restored, the
   rebase removed) reddening those tests; a comparand IN join over an index, with its
   probes counted, resumed from a continuation mid-list; and `x IN (1 + 1, 3)` (a
   runtime list of constants) answering through the index as the literal list `x IN
   (2, 3)` does.
2. Array construction: SQL array elements are non-nullable (DataTypeUtils.java:76,
   114; SemanticAnalyzer.java:863; ExpressionVisitor.java:1023,1187): the target's
   check is type-based and context-free (ExpressionVisitor.java:1186-1203): an
   element of NullType is rejected with the RelationalException message, a nullable
   element expression is promoted to the non-nullable element type and fails with
   the SemanticException message when it evaluates to NULL; one-field records stay
   records; a direct nested array stays unsupported while a record containing an
   array is supported. MEASURED: the target rejects a NULL element in every
   consumer, a projection [array_bare_null_element], a function argument
   [null_array_function_argument] and a comparison operand
   [null_array_comparison_operand], all 0A000.
   Array COMPARISON admission. The target's comparison check DOES compare element
   nullability: RelOpValue removes only the outer nullability (RelOpValue.java:
   365-370), and array type equality includes the element's (Type.java:1106,
   3313-3314). It answers `arr = [m]` [array_eq_column_element] because `handleArray`
   has already promoted the constructor's nullable element `m` to the non-null element
   type (ExpressionVisitor.java:1186-1203), so both operands are ARRAY<LONG NOT NULL>;
   `[5]` stays 42804 because ARRAY<INT> and ARRAY<LONG> have no promotion
   [array_eq_literal, array_eq_parenthesised_operand]. Go ports that promotion into
   array construction (above) and KEEPS its existing comparison check, which compares
   element types with element nullability (expr.go:977-981); with the constructor typed
   as the target types it, `arr = [m]` is admitted, and nothing else about admission
   changes. A NULL literal element is still NullType and is still the target's
   RelationalException 0A000, in a WHERE as in a projection, with a column on the other
   side [array_column_eq_column_and_null] or not [array_null_element_eq_literal,
   null_array_comparison_operand].
   The owner-approved Go nullable-array READ extension keeps exactly its approved pins
   and nothing wider (`conformance/array_comparison_java_probe_test.go:53-55`:
   `[1,NULL] = [1,NULL]` and `[NULL] = [NULL]` are TRUE, `[1,NULL] = [1,2]` is 42804).
   Stated not-covered list first: a column operand on either side, a column-valued,
   typed-NULL (`CAST(NULL AS ...)`) or bound-NULL element, a parenthesised, CAST or
   CASE-wrapped constructor, and every consumer other than a comparison operand follow
   the target with its exact error. Covered, and only this: a comparison with =, <>,
   IS [NOT] DISTINCT FROM or their negations whose BOTH operands are array constructors
   consumed directly (no wrapper), whose elements are all LITERALS, and at least one of
   which contains the bare NULL token. For exactly that shape the walker builds the
   constructors with today's nullable element typing instead of the NullType
   rejection, and Go's unchanged check then gives the approved answers (TRUE for equal
   element nullability, 42804 otherwise). The decision is made where the walker builds
   the comparison, from both constructors' own element nodes, never from SQL text. The
   target answers 0A000 for all three approved shapes [null_array_comparison_operand,
   array_null_element_eq_literal]; those rows are the extension's declared rows.
   Both serialization guards stay as unconditional write backstops, retargeted to the
   target's 0A000: `functions/proto_value.go:198-205` and `executor.go:4363` (a NULL
   element can never reach a stored array whatever the admission path). Tests (cold and
   warm, static and runtime NULL): empty array, one-field record array, a record
   containing an array versus a directly nested array, a NULL array versus a NULL
   element, each consumer above on both sides of the boundary, `arr = [m]` admitted and
   `arr = [m, NULL]` refused, and the extension's pins unchanged.
3. Parameters are bound, not substituted. MEASURED (prepared rows run the statement
   through Java's JDBC setters and Go's database/sql arguments): a setNull(BIGINT)
   and a setObject(null) are indistinguishable in every consumer
   [prepared_in_typed_null = prepared_in_object_null, both 0A000;
   prepared_array_typed_null_element takes the RelationalException (NullType) form],
   because the target throws the SQL type away (EmbeddedRelationalPreparedStatement.
   java:207-216) and types the parameter from its value, `Type.fromObject(null)` =
   NullType (MutablePlanGenerationContext.java:473-495); a bound NULL as a predicate
   or function operand behaves as an inline NULL [prepared_eq_typed_null,
   prepared_is_null_param, prepared_coalesce_typed_null], an INSERT stores it as NULL
   [insert_untyped_null_param], and it has no arithmetic lane: `? + 1` bound to NULL
   is XX000 "unable to encapsulate arithmetic operation due to type mismatch(es)"
   [arith_untyped_null_param], as are an inline `NULL + 1` in a projection and in a
   predicate [inline_null_plus_one_select, inline_null_plus_one_where] and `(1 / 0) +
   NULL` [null_strict_null_beside_div0_select], while a typed NULL has a lane
   [cast_null_plus_one_select: `CAST(NULL AS BIGINT) + 1` is a BIGINT NULL]; Go takes
   all of them by section 5.6's lane table (Go today answers NULL for the inline
   forms, their GO lines); a bound NULL
   PROJECTED alone is an internal target failure, XXXXX RecordCoreException "should
   not be called" [select_untyped_null_param], because NullType has no result-set
   type there, and Go keeps answering a NULL of UNKNOWN type (a target defect,
   recorded in DIVERGENCES.md with the probe); setLong is LONG and setInt
   INT [prepared_long_no_overflow, prepared_int_overflow, prepared_long_vs_int_column,
   prepared_int_vs_int_column]; setDouble is DOUBLE and setBoolean BOOLEAN
   [prepared_double, prepared_boolean]; setUUID is UUID [uuid_parameter, result type
   OTHER]; several positional parameters bind in order [prepared_two_where,
   prepared_in_two]; named parameters `?x` and `$x` bind by name, may be repeated
   and may be mixed with positional ones [prepared_named, prepared_named_dollar,
   named_parameter_twice, mixed_named_and_positional]; `IN ?` takes an ARRAY
   parameter [in_array_parameter]; `LIKE ?` is 42601 because the pattern is
   `constant` [prepared_like_param_pattern, prepared_like_null_pattern]; `ORDER BY ?`
   is 0AF00 [prepared_order_by_param]. Go today substitutes the arguments into the
   SQL text (`embedded/utilities.go:94-164`, called from connection.go:636,671), and
   every one of those rows' GO lines shows the consequence: a nil becomes the bare
   NULL token (42809), every integer an INT-if-it-fits literal, a UUID a STRING, named
   parameters and array parameters are unsupported.
   Design: the SQL text is parsed with its `?` and NAMED_PARAMETER tokens intact
   (RelationalLexer.g4:1359) and the walker binds each parameter where the grammar
   admits `preparedStatementParameter` (expression atoms, IN lists including `IN ?`,
   LIMIT/OFFSET atoms) to a CONSTANT typed from the bound driver value, the target's
   `Type.fromObject`. The typing happens in `CheckNamedValue` (connection.go:958), which
   database/sql calls with the caller's ORIGINAL value before any conversion of its own,
   so its int64 widening never erases a lane. The order of the checks is fixed, and where database/sql has an
   order it is database/sql's (`driver.callValuerValue` asks for a Valuer BEFORE it
   dereferences anything, and answers NULL for a nil pointer whose Value method has a
   value receiver): (1) a nil interface -> an UNTYPED NULL (NullType) constant; (2) the
   known concrete types, before any interface: uuid.UUID -> UUID (it is also a Valuer
   whose Value() is a string, which is why it comes first); time.Time -> the instant
   rule below; the database/sql null wrappers by their payload (sql.NullInt32, NullInt16
   and NullByte -> INT, NullInt64 -> LONG, NullFloat64 -> DOUBLE, NullBool -> BOOLEAN,
   NullString -> STRING, NullTime -> as time.Time, each an untyped NULL when not Valid);
   (3) a pointer whose pointee TYPE is one of those known types is dereferenced and
   typed by (2), a nil one being an untyped NULL, so `*uuid.UUID` and `*time.Time` are
   never taken by their Valuer's text; (4) any other driver.Valuer, the value AS PASSED
   (a pointer included, so a Value method with a pointer receiver is found) -> its
   Value() typed by this list once more, except that an int64 from a Valuer takes the
   unsuffixed-literal typing of step 6's plain `int` (driver.Value has no int32, so a
   Valuer's int64 carries no lane, exactly like a Go `int`, and a custom Valuer keeps
   storing into an INTEGER column); a nil pointer whose Value method has a value
   receiver is an untyped NULL, as database/sql answers; (5) any other pointer is
   dereferenced ONE level and the value restarts at (2), and a nil pointer of any other
   type is an untyped NULL; (6) the value's `reflect.Kind`, so named types (`type
   Status int32`) are typed by what they are: Int32 -> INT and Int64 -> LONG (Java's
   setInt and setLong); Int, Int8, Int16, Uint8, Uint16 and Uint32 -> the type an
   unsuffixed integer LITERAL of that value gets (INT when it fits in int32, else LONG,
   ParseHelpers.java:96-98), so the idiomatic `Exec("INSERT INTO I VALUES (?, ?)", 9,
   7)` still stores into an INTEGER column; Uint and Uint64 -> LONG when the value fits
   in int64, else 22003; Float32 -> FLOAT and Float64 -> DOUBLE; String -> STRING; Bool
   -> BOOLEAN; a slice of Uint8 and an ARRAY of Uint8 (`[N]byte`) -> BYTES, both checked
   before the general rule (a UUID is bound as uuid.UUID, which (2) takes first; a bare
   `[16]byte` is BYTES, because the engine's internal `[16]byte` UUID representation,
   values.go:4521-4526, is not a binding rule); any other slice or array -> an ARRAY
   typed ONCE, from its STATIC element type, never element by element: an element kind
   with a fixed type (Int32, Int64, Float32, Float64, String, Bool, uuid.UUID,
   time.Time, and a `[]byte` or `[N]byte`, which is BYTES) gives that element type, a
   time.Time element taking the canonical rule below element by element, its year
   domain included; a pointer element kind (`[]*int32`, `[]*time.Time`) is typed by
   its pointee's kind by this same rule, and a nil element is a NULL element, refused
   as item 2 says; any other slice or array element kind (`[][]int`) is a directly
   nested array, which item 2 keeps unsupported, and is 22023 naming the parameter and
   its Go type; an element
   kind whose scalar typing depends on the value (Int, Int8, Int16, Uint16, Uint32)
   gives INT when EVERY element fits in int32 and LONG otherwise, the type the target's
   array constructor gives those integers (its elements promote to their maximum type),
   so `[]int{1, 3000000000}` is ARRAY<LONG>, never a mixed array; Uint and Uint64
   elements give LONG, 22003 for one beyond int64; an EMPTY slice of a value-typed kind,
   having no value to type from, is the target's empty-array type ARRAY<NONE>
   (PromoteValue.java:90), which NONE_TO_ARRAY assigns to any array column (:374-380);
   and an element of interface type (`[]any`) is typed by this list per element, the
   array's element type being the maximum type of its elements as in the target's array
   constructor, 22023 when they have none, while a NULL element is refused as item 2
   says; (7) anything else -> 22023 naming the parameter and its Go type. The `fmt.Stringer` rule of
   today's CheckNamedValue (every Stringer bound as its String() text) is DELETED: it
   is Go-only, database/sql's own default converter never applies it, and it silently
   stored a stringer-generated integer enum as its name text; after this change such a
   value binds by its kind and `time.Duration` binds as LONG nanoseconds (declared in
   CHANGELOG). A nil slice (any element type, `[]byte(nil)` included) is an EMPTY value
   of its type, as `[]byte(nil)` is today, and only a nil interface or nil pointer is
   NULL; the tests compare the stored record bytes for the empty array, the NULL array
   and the NULL element. There is no typed-NULL driver value: the target has none.
   Slices are accepted wherever a parameter is: MEASURED, the target binds a
   `setArray` parameter into an ARRAY column in INSERT and UPDATE, stores an empty
   array as `[]`, and `setNull(ARRAY)` as NULL [insert_array_parameter,
   insert_empty_array_parameter, update_array_parameter, insert_null_array_parameter];
   Go today refuses every slice in its driver (their GO lines).
   time.Time is typed by its INSTANT, never by its wall clock, and binds as the
   CANONICAL TIMESTAMP TEXT of that instant: a TIMESTAMP-typed constant whose value is
   the string `FormatTimestamp(t)` (functions/time.go:13-15: UTC, to the second), the
   same carrier every other TIMESTAMP value in the engine has, a CAST's included
   (values.go:4549-4560). No time.Time ever reaches the planner, the executor or a key:
   the index binder's carrier check admits only a string for STRING, DATE and TIMESTAMP
   keys (scan_range_binding.go:586-591, pinned at scan_range_binding_test.go:834-838),
   and that check stays exactly as it is. The target has no date or time type at all,
   MEASURED: a DATE column is 42F18 "could not find type 'DATE'" (round v9,
   [date_lt_null_timestamp_where and the four after it]), a TIMESTAMP column 42F18
   "could not find type 'TIMESTAMP'" [timestamp_column_rows], and `CAST(... AS DATE)` and
   `CAST(... AS TIMESTAMP)` are syntax errors, 42601 [cast_date_value_select,
   cast_timestamp_value_select, cast_column_to_date_where] (round v10; v9 had measured
   DATE only and called TIMESTAMP's absence measured); so everything temporal below is
   the approved Go extension's alone, held to the design's own consistency rather than
   to a target outcome, and DIVERGENCES.md declares it so when it lands. The text's
   year must be in 0000-9999: outside that domain `FormatTimestamp` writes a text that
   no layout of the parser below reads and that does not sort by instant, so such a
   value is refused at bind with 22008 (DATETIME_FIELD_OVERFLOW) naming the parameter,
   an element of a bound slice included. Sub-second precision is dropped at bind, as
   the text channel drops it today when it renders; CHANGELOG states it.
   THE SCOPE (v10): DATE and TIMESTAMP are types of VALUES, never of a stored column.
   MEASURED (round v10): a column declared TIMESTAMP reads back as STRING in Go
   [timestamp_column_rows, GO `[BIGINT STRING]`], as round v9's GREATEST over a DATE and
   a TIMESTAMP column is STRING [greatest_date_timestamp_select, GO `[STRING]`], where
   Go's `MaximumType(DATE, TIMESTAMP)` is TIMESTAMP (type.go:1410). SOURCE for why: the
   DDL writes `TYPE_STRING` and no marker (builder.go:1019-1022), every template
   rebuilds its columns from the proto descriptor (builder.go:702,
   fdb_template_catalog.go:297), and the SQL type "is not recoverable from the proto
   descriptor alone" (proto_types.go:73-78; `FieldTypeForProtoField`,
   proto_field_type.go:50, and rlcatalog.go:398-409 both answer STRING). v9's COLUMN
   claims are WITHDRAWN, each of which rested on a column type no stored column has:
   canonicalization in the write converters by column type (which dispatch on the
   proto kind, proto_value.go:317-323, executor.go:4459-4463), the `SET d = d` repair,
   22007 for a STRING written into a DATE column, the lane admitting a column,
   `GREATEST(d, t)` typed TIMESTAMP, and the stored-text table per column type.
   Persisting the type (a field option in the stored metadata) is rejected: it changes
   catalog bytes the target reads, for a type the target does not have, and templates
   written before and after the change would differ. A test creates a template with a
   DATE and a TIMESTAMP column, opens it through the catalog in a new connection, and
   asserts each column's type is STRING, so a change that starts persisting the type
   fails there and must revisit this item.
   THE PRODUCERS of DATE and TIMESTAMP values, enumerated at the tree: `CAST(x AS
   DATE)` and `CAST(x AS TIMESTAMP)` (expr/walk.go:1389-1392, values.go:4534-4560), the
   statement-clock functions CURRENT_DATE, CURRENT_TIMESTAMP and their synonyms
   (scalar_function_catalog.go:423-429, evaluated at values.go:2743-2746), a bound
   time.Time (above), a NULL of either type, and the DATE-to-TIMESTAMP promotion
   (type.go:1410, below); any other DATE or TIMESTAMP value carries one of these
   through a projection, a subquery column or a constructed record (a planner-captured
   field type of DATE or TIMESTAMP over a STRING carrier, field_value.go:1065-1069).
   Each writes the canonical text of its type in UTC (`dateLayout`, `timestampLayout`,
   values.go; `FormatTimestamp` at bind), and with the parser's domain below every
   non-NULL DATE or TIMESTAMP value holds canonical text in the years 0000-9999.
   THE TYPE-MERGING OPERATORS are producers too (v12; v11 said their typing "does not
   change here", and the property failed through them). CASE is a `PickValue` over its
   branches as built, typed by `CommonValueType`, which is `MaximumTypeOfMany`
   (walk.go:649-661, scalar_function_catalog.go:504-545). IF and IFNULL are typed the same
   way, and so are COALESCE, GREATEST and LEAST (section 5). A branch of type DATE under a
   result of type TIMESTAMP then carries DATE text under a TIMESTAMP type:
   `CASE WHEN c THEN CAST('2024-01-01' AS DATE) ELSE CAST('2024-01-01 00:00:00' AS
   TIMESTAMP) END = CAST('2024-01-01 00:00:00' AS TIMESTAMP)` is FALSE for the first
   branch, and `<` is TRUE, since the two texts differ. So the promotion below is ALSO
   injected into every branch of those operators whose type differs from the result type
   (the same `compilePromotion` arm the comparison uses). A DATE branch becomes its
   midnight TIMESTAMP text before the branches are picked, which is what the target's
   `PickValue` does by demanding equal alternative types after promotion.
   The rest of this item rests on that property. A unit test per producer asserts its
   text against the canonical layout, the domain edges included. Each merging operator
   gets a test with a DATE branch under a TIMESTAMP result, asserting the branch's text
   and its `=` and `<` against the midnight TIMESTAMP.
   ONE PARSER, with a stated domain: `ParseTemporalText(s)`, in the values package
   beside `timestampParseLayouts` (values.go:2732), trims surrounding whitespace, reads
   exactly those four layouts (`2006-01-02 15:04:05`, RFC 3339 with a zone,
   `2006-01-02T15:04:05`, `2006-01-02`), converts to UTC, and REFUSES a result whose UTC
   year is outside 0000-9999: the RFC 3339 layout takes an offset, so
   `'9999-12-31T23:00:00-05:00'` is year 10000 in UTC and `'0000-01-01T00:30:00+01:00'`
   year -1, whose `FormatTimestamp` text no layout reads and which sorts out of instant
   order (SOURCE). Its callers: the CAST to DATE and the CAST to TIMESTAMP
   (values.go:4534-4560; the CAST to DATE reads only the date-only and space layouts
   today, so RFC 3339 text becomes castable to DATE, declared in CHANGELOG), the CAST of
   an int64 (epoch milliseconds) to TIMESTAMP, which takes the same domain check, and
   the date-part functions (values.go:3348-3353), which try the parser first and then
   their one extra form, a time of day `15:04:05` (they read and trim as the CASTs do
   and keep that form, so `HOUR('15:04:05')` keeps its answer; the trim is new for them,
   so `YEAR(' 2024-01-01')` answers 2024 where it raised 22023, declared in CHANGELOG; a date-part function
   produces an integer, never a DATE or TIMESTAMP value, so the form does not reach the
   property above). A CAST's failure, the out-of-domain case included, keeps its own
   `InvalidCastError` (22F3H), naming the domain for that case. The other parser,
   `functions.ParseTimestamp` (functions/time.go:24-37: five layouts, one with
   fractional seconds, no trim), is DELETED with every caller, and so is what only it
   served: the residual comparison's `time.Time` arms (predicates/comparisons.go:
   644-707, its calls at :645, :687 and :701), deleted below; `functions.CompareValues`'
   `time.Time` arms (compare.go:89-109, its calls at :95 and :103), whose one non-test
   caller orders system-table rows (select_parser.go:338, reached from
   system_rows.go:233), which carry no time.Time: the rows are produced by
   `system_tables.go` (:92 builds them), and at the tree `git grep -n -E
   'time\.(Time|Now|Unix)' <TREE> -- pkg/relational/core/embedded/system_tables.go
   pkg/relational/core/embedded/system_rows.go` finds no line, with the control `git
   grep -c 'func ' <TREE> -- pkg/relational/core/embedded/system_tables.go` non-zero (v10
   grepped the orderer's file only, in the working tree); and `functions.CastValue` (functions/cast.go:28-221,
   its calls at :204 and :216), which has no non-test caller (its callers are
   functions_test.go, cast_float_test.go and cast_numeric_boundary_test.go in package
   functions, and FuzzCastValue, embedded_test.go:705-732), deleted with those tests,
   each of whose shapes is checked against the values package's CAST tests and moved
   there where missing. v9 said "a text is readable everywhere or nowhere" while three
   parsers existed; the claim now made is narrower and holds: a text is a DATE by CAST
   exactly when it is a TIMESTAMP by CAST, and the date-part functions read exactly
   those texts and a time of day.
   ASSIGNMENT. A DATE or TIMESTAMP value assigned to a column, a STRING column
   whatever its DDL spelling, takes Go's DATE-to-STRING or TIMESTAMP-to-STRING edge
   (type.go:1411-1412) and stores its text, which is canonical by the property above;
   into any other column it is 22000 by the lattice below. THE RUNTIME HALF of that
   assignment (v8, corrected in v9, narrowed in v10): the planning-time lattice has a
   runtime coercer, `promotionNode.coerce` (promote_prepared.go:379-391), which today
   sends EVERY non-ENUM STRING target to `uuid.Parse`; that branch is restricted to UUID
   targets, so v7's swap to `ParseJavaUUID` there reaches UUIDs only. `compilePromotion`
   (promote_prepared.go:118-201) has no DATE or TIMESTAMP arm, its NULL arm at :136
   included, so DATE to TIMESTAMP fails at PREPARE today; it gains DATE to TIMESTAMP (the
   date's canonical midnight text, `2024-01-01` to `2024-01-01 00:00:00`), DATE and
   TIMESTAMP to STRING (the text as it is), and the NULL arms of both types. No
   STRING-to-DATE, STRING-to-TIMESTAMP or TIMESTAMP-to-DATE promotion is added:
   `promotionMap` (type.go:1385-1412) is unchanged, a STRING becomes a DATE or TIMESTAMP
   only by CAST and a TIMESTAMP a DATE only by CAST; v9's edges existed for the
   withdrawn DATE column. Tests: each coercer edge from each source (STRING, DATE,
   TIMESTAMP and UUID targets), DATE to TIMESTAMP through Prepare, a STRING into a UUID
   column still parsed as a UUID, and a DATE and a TIMESTAMP value into a STRING column
   and into an INTEGER column (stored text; 22000).
   A bound time.Time is a TIMESTAMP value and is assigned like one:

   | column                                  | stored text          | today (text channel)                                   |
   |-----------------------------------------|----------------------|--------------------------------------------------------|
   | STRING, DATE or TIMESTAMP (all STRING)  | `FormatTimestamp(I)` | `FormatDate(I)` when the value is at midnight in its own zone, `FormatTimestamp(I)` otherwise |
   | any other                               | 22000                | a conversion error or the rendered text                |

   Today's text channel decides DATE versus TIMESTAMP by midnight in the value's OWN
   zone and then renders in UTC (utilities.go:256-263, functions/time.go:13-20), so
   `2024-01-01T00:00+02:00` binds as `'2023-12-31'`, a day that is the instant's day in
   neither zone and loses 22 hours; that corruption is fixed and pinned by a regression
   test per zone case. DECLARED in CHANGELOG, with the round trip it changes: a
   time.Time at midnight in its own zone written into a DATE-spelled column was stored
   as date text and is now stored as timestamp text, and `WHERE d = ?` with such a
   value over rows the old channel wrote as date text found them and now compares
   `'2024-01-01 00:00:00'` with `'2024-01-01'` as text and finds nothing. The statement
   that stores and finds a day is `CAST(? AS DATE)`: the bound value is canonical
   TIMESTAMP text, which the CAST's string arm (values.go:4538-4546, through the one
   parser) reads and renders as its UTC day, a DATE stored and compared as date text
   (v10 cited the `time.Time` arm, which this item deletes); the CHANGELOG entry gives
   it. The change has consequences beyond `WHERE d = ?` over DATE-spelled columns whose
   old rows hold date text, each declared in the CHANGELOG with its repair and pinned by
   a test over mixed rows (old date text beside new timestamp text): a day RANGE bound by
   midnight values, `d >= ? AND d < ?`, drops the first day's date-text rows and admits
   the next day's, since `'2024-01-01' < '2024-01-01 00:00:00'`; a UNIQUE index admits a
   second row for a day, one per spelling; and GROUP BY and DISTINCT split one day into
   two groups. THE REPAIR (v12; v11's `UPDATE ... SET d = CAST(d AS DATE)` over the whole
   table was wrong four ways: one unreadable row fails the whole atomic statement with
   22F3H, where v11 said the row is "left as it is"; it truncated the time of day of every
   non-midnight timestamp text, which this change never touched; under the UNIQUE index
   declared above it collides with the date-text row of the same day, 23505; and it is one
   unbatched statement against FDB's 5 s and 10 MB limits). The CHANGELOG gives this
   procedure, and a test pins each step:
   1. Only rows whose text is a MIDNIGHT timestamp in the canonical layout are touched,
      selected by `WHERE d LIKE '____-__-__ 00:00:00'` (the canonical TIMESTAMP text the
      channel wrote, 4.3's layout). Every other row, date text and non-midnight timestamp
      text alike, is left as stored.
   2. Duplicates first. Where a UNIQUE index holds both spellings of one day, the
      procedure lists the pairs (`SELECT` of each midnight row whose date text also exists)
      and stops. The operator deletes or merges one of each pair before step 3, since
      only they know which row is the survivor.
   3. The rewrite, `UPDATE t SET d = CAST(d AS DATE) WHERE d LIKE '____-__-__ 00:00:00'
      AND pk >= ? AND pk < ?`, runs in primary-key batches small enough for one
      transaction each (the CHANGELOG states the batch as a row count the operator picks
      under the 10 MB write limit). A batch that fails leaves every row outside it as it
      was, and it can be re-run, since a rewritten row no longer matches step 1's filter.
   Tests over mixed rows (date text, midnight timestamp text, non-midnight timestamp
   text, and a duplicate day under UNIQUE): step 2 lists exactly the duplicate; after its
   removal, step 3 in two batches rewrites exactly the midnight rows and no other row; a
   re-run changes nothing; and a non-midnight row keeps its time of day. Binding is
   `CAST(? AS DATE)` from then on. Rows the old channel wrote are read back as they were stored and compare
   as text, as today; a test writes each "today" cell with the old channel's exact
   text, reads it back, and compares it with a bound time.Time and with `CAST(? AS
   DATE)` in a residual filter and through an index equality, asserting the textual
   answers.
   THE COMPARISON. An operand of type DATE and one of type TIMESTAMP, both VALUES by the
   scope above, compare under their common type TIMESTAMP (`MaximumType`, type.go:1410):
   the DATE promotes to its canonical midnight text, and the two texts compare as
   strings, which for canonical text in the four-digit domain IS the instant order (the
   layout is fixed-width, most significant field first). THE PROMOTION IS INJECTED, at
   the sites that build a comparison, as the target's `RelOpValue.promoteOperands` and
   `InOpValue` inject theirs (v10 named the common type and no site, and Go injects none
   today: the comparison constructor promotes only to an ENUM common type,
   expr.go:911-925, IN items only to UUID or ENUM, expr.go:1830-1843, and `cmpAny`
   compares string with string as text, so `CAST('2024-01-01' AS DATE) = CAST('2024-01-01
   00:00:00' AS TIMESTAMP)` is FALSE and `<` TRUE without it): (i) the comparison
   constructor wraps the operand whose type is DATE in `PromoteValue(operand,
   TIMESTAMP)` when the other is TIMESTAMP, beside its ENUM arm; (ii)
   `promoteInListItemsToDeclaredType` promotes each DATE item to TIMESTAMP when the
   probe is TIMESTAMP, and a DATE probe against a list whose common type is TIMESTAMP is
   wrapped as in (i); (iii) the explode's equality inherits its IN's operands, already
   promoted, and so does every comparison built from another (`Negate`, `Commute`, a
   rebase); and the runtime is `compilePromotion`'s new DATE-to-TIMESTAMP arm (below),
   whose output is the canonical midnight text. A STRING operand is never promoted to a
   temporal type (no such edge). Tests: `CAST('2024-01-01' AS DATE) = CAST('2024-01-01
   00:00:00' AS TIMESTAMP)` TRUE, `<` FALSE and `<=` TRUE, a DATE against a TIMESTAMP
   ten hours later `<` TRUE, a DATE probe against a TIMESTAMP IN list and a TIMESTAMP probe
   against a DATE list, each through the per-row path and the explode, and the EXPLAIN
   showing the promotion. The same holds for GREATEST
   and LEAST (section 5's port promotes their arguments to the common type, and the
   result is TIMESTAMP-typed) and for an IN list whose items mix the two types (its
   common type TIMESTAMP); DATE against DATE and TIMESTAMP against TIMESTAMP compare
   canonical texts of one layout, the instant order again. v9's LANE is WITHDRAWN,
   with its half-open mapping, its saturation at the domain edge, its role rule, its
   binder change and its 22007 arm: it existed to make an index over a DATE COLUMN agree
   with a residual over the same stored text, no index key has either type (every
   stored column is STRING), and a DATE or TIMESTAMP value never holds text the parser
   cannot read, so the 22007 had no producer (the review's point). A DATE or TIMESTAMP
   value compared with a STRING column (any spelling) takes the common type STRING
   (the edges above) and compares text, as today and alike through every plan:
   `TestFDB_TemporalComparandDateColumn` keeps its answers (`D = CAST('2024-01-01
   00:00:00' AS TIMESTAMP)` answers no rows, `D >=` it only 2024-01-02, through the
   index and the residual), is re-labelled as pinning the STRING column's textual
   comparison, and gains the rows this change moves: a bound time.Time at midnight
   answers what its CAST constant answers (today row 1, through the old channel's date
   text) and `D = CAST(? AS DATE)` answers row 1. `GREATEST(d, t)` over two columns
   stays STRING and textual (round v9's GO lines, `2024-01-01 10:00:00` and `LEAST`
   `2024-01-01`, unchanged); over values, `GREATEST(CAST('2024-01-01' AS DATE),
   CAST('2024-01-01 10:00:00' AS TIMESTAMP))` is TIMESTAMP `2024-01-01 10:00:00` and
   `LEAST` TIMESTAMP `2024-01-01 00:00:00` (Go today on values is unmeasured; a test pins
   both). A property test drives generated DATE and TIMESTAMP values from every producer
   (across 0000-01-01 and 9999-12-31 23:59:59, and NULL) through `=`, `<>`, `<`, `<=`,
   `>`, `>=`, their NOT forms and `Commute`, and through GREATEST and LEAST, asserting the
   instant order and UNKNOWN for a NULL. The `time.Time` arms of the residual comparison
   (predicates/comparisons.go:644-707) have no producer once a bound time.Time is text:
   CURRENT_TIMESTAMP and CURRENT_DATE evaluate to text (values.go:2743-2746), CAST
   produces text, and the executor's clock (evaluation_context.go:87) feeds only those;
   the arms are deleted, and so are the other `time.Time` arms that no producer reaches
   once a bound time.Time is text: the value CAST's DATE and TIMESTAMP arms
   (values.go:4535-4537, :4550-4552) and the date-part functions' (:3343-3347). The one
   place a time.Time could still enter an evaluated row is checked instead: the residual
   comparison's operand conversion refuses a `time.Time` with an internal error naming
   the Value, pinned by a unit test that feeds one. v11 said this makes "no time.Time
   reaches the executor" hold "of the code"; the v11 gates found four sites it did not
   list, and each is decided here (the census is `git grep -n 'time\.Time'` over the
   non-test files of pkg/recordlayer/query/executor and pkg/relational, restated at the
   implementation's tree with each hit classified):
   - The continuation codec's tag 12 (continuation.go:103, the encoder at :153, the
     decoder at :327). The ENCODER arm is deleted with the other unreachable arms. The
     DECODER keeps reading tag 12, since a continuation a pre-upgrade Go build issued may
     carry it and a resume must not misread it. It decodes the time to its canonical text,
     the value that key now has, and a unit test decodes a tag-12 continuation.
   - The write converter's STRING arm for a `time.Time` (functions/proto_value.go:
     321-323) is deleted: after binding, no row value is a time.Time, and a stored STRING
     field receives text.
   - The hash-join comment at streaming_cursors.go:1775-1781, which relies on the deleted
     `ParseTimestamp`, is rewritten with the change: a probe key is never a time.Time.
   - The driver's scan type for a DATE or TIMESTAMP result column
     (cascades_generator.go:1743-1744) declares `time.Time` for values that are text. It
     is a DRIVER-API surface, not an evaluated row, and it changes to `string`, the type
     the driver returns. CHANGELOG states it, and `ColumnTypeScanType` is pinned per SQL
     type by a unit test.
   Ordinals. Positional parameters bind in TEXTUAL order over the whole statement
   (and, for a multi-statement ExecContext batch, over the whole batch, as the
   substitution channel does today), which is the JDBC contract. The target binds in
   VISITOR order instead (PreparedParams.java:66-76 numbers parameters as
   QueryVisitor reaches them, FROM, then WHERE, then SELECT, :258-296), so
   `SELECT ? FROM T WHERE id = ?` bound (100, 1) returns no rows in the target
   [prepared_select_then_where] while Go returns [[100]]. MEASURED on writes too:
   `UPDATE L SET v = ? WHERE id = ?` bound (100, 1) updates nothing in the target,
   which binds 100 to the WHERE [update_set_then_where_order], and Go updates row 1;
   `INSERT INTO L SELECT ?, ? FROM T WHERE id = ?` bound (20, 7, 1) inserts nothing in
   the target and (20, 7) in Go [insert_select_then_where_order]; `SELECT id, n + ?
   AS k FROM T WHERE id = ?` bound (10, 2) is empty in the target and [[2 15]] in Go
   [select_expr_then_where_order]. The update counts show the DML rows are not a
   read-back artefact: the UPDATE and the INSERT ... SELECT report COUNT 0 in the
   target. An IN list and a trailing conjunct bind in textual order in both
   [in_list_order: bound (2, 1, 0), textual order is `id IN (2, 1) AND n > 0` and
   answers [[2]], the other order would be `n > 2 AND id IN (1, 0)` and answer
   nothing]. That is a target defect, not a contract, and on DML it changes stored
   data: DIVERGENCES.md records it as a WRITE divergence with its four probes
   (prepared_select_then_where, update_set_then_where_order,
   insert_select_then_where_order, select_expr_then_where_order), Go keeps textual
   order, and Go FDB tests pin the read-back and the RowsAffected of the UPDATE and the
   INSERT ... SELECT (a Java application porting a statement whose parameters straddle
   WHERE and SET/SELECT must reorder its arguments). The measured population is exactly
   those shapes. A HAVING shape cannot be measured because the target cannot plan the
   GROUP BY under it at all: the same statement with literals and the GROUP BY alone
   are both 0AF00 in the target [where_then_having_order, having_literal_control,
   group_by_literal_control], so the cause is GROUP BY without an ordering index, the
   documented Go extension (DIVERGENCES.md, "GROUP BY"), not HAVING; the IN-subquery
   shape is refused by both [subquery_order]; ORDER BY and multi-subquery orders are
   unmeasured. The upstream report is unpublished (publication is not authorized).
   Named parameters bind by name, a name may appear several times, and named and
   positional parameters may be mixed. Positional ORDINALS count only the `?`
   tokens: a named argument passed to database/sql carries NamedValue.Ordinal like
   every argument, and the binder assigns positional values to `?` tokens in the
   order the positional ARGUMENTS appear, skipping named ones, so `Exec(q,
   sql.Named("x", 1), 5)` binds 5 to the first `?`; a test passes a named argument
   before positional ones. Counts, as the target (PreparedParams.java:71-82): a `?`
   or name with no argument is 42F02 "No value found for parameter <n or name>"
   [too_few_parameters], and extra positional or unused named arguments are
   ignored [too_many_parameters]. Go's 22023 for both goes with substituteParams;
   declared in CHANGELOG, because a caller who passed one argument too many and
   relied on the error now has the extra silently ignored, as in the target.
   Values. The bound value is copied at bind time, recursively ([]byte and every
   slice bound to `IN ?`, element by element: database/sql callers may reuse their
   buffers, and bound constants live in the shared plan cache and in lazily fetched
   pages); a test mutates a reused slice after the call and takes a warm hit. A string
   that is not valid UTF-8 is rejected at bind time with 22021 naming the parameter (a
   Go-only code: the target has none, its JDBC strings being UTF-16), and the check
   recurses into every element of a bound slice. EVERY binding error of a
   multi-statement batch is raised before any statement of it runs, not only this one:
   a missing parameter (42F02), a uint64 that does not fit (22003), an unsupported Go
   type (22023) and invalid UTF-8 (22021) are all found by one pass over the whole
   batch's parameters, so an auto-commit batch never applies partially because of a
   binding error (today's `substituteParams` has the same all-first property,
   utilities.go:156-159, and it is kept). The target cannot receive such a
   string at all (a JDBC String is UTF-16), and letting its raw bytes reach proto2
   string fields and tuple/index keys would store bytes Java reads back as U+FFFD,
   so a Java update or delete would compute different index keys and orphan the
   entries; replacing them silently (today's lexer path does so) would change the
   caller's data. For the same reason the record layer's union serialization
   (`serializeUnion`, store.go:2137), the one step every Go writer passes through,
   refuses a record with a string (at any depth: a string field, a repeated string, and
   a map's string KEYS as well as its string values) that is not valid UTF-8, with a typed
   `InvalidUTF8StringError{Field}`: the target can never write such bytes (its strings
   are UTF-16 and protobuf-java encodes them as valid UTF-8), so a Go-written record
   holding them is a record whose bytes and index keys the target would read back
   differently. The save path is not that one step: `SaveRecordBatch` on a store with
   the standard layout serializes through `serializeUnion` and writes with
   `tx.SetBytes` or `saveWithSplit` itself (store_batch.go:188), never calling
   `saveRecordInternal`, and the dry-run save serializes at store_api.go:391; all three
   call `serializeUnion` (store.go:658, store_api.go:391, store_batch.go:188, the only
   non-test callers, `git grep -n 'serializeUnion(' -- 'pkg/recordlayer/*.go'`), so the
   check sits there, and a test per caller, the batch path on a standard-layout store
   among them, pins the refusal with nothing written. `SaveRecordBatch` today writes
   record i before it serializes record i + 1 (store_batch.go:187-205), so a bad record
   in a later position would leave the earlier ones written in the transaction; it
   serializes (and so validates) EVERY record of the batch before its first write, as
   the binding errors are all raised before any statement runs, and its test puts the
   bad record last and asserts the transaction holds none of the batch. The check is
   `utf8.ValidString` per string field; its cost is
   measured by the 1M stress comparison section 8 already requires. CHANGELOG declares
   the new error. A record ALREADY stored with such bytes (by a Go library writer
   before this change) stays readable, but a Go UPDATE of ANY column of it now fails
   with `InvalidUTF8StringError`, because the save validates the whole new record;
   CHANGELOG declares that with its two repairs: an UPDATE that also sets the offending
   field to valid text succeeds (the check sees the new value), and a DELETE of the
   record succeeds (a delete saves nothing). An FDB test writes such a record's bytes
   with a raw transaction at its record key (the record layer refuses to serialize it)
   and pins the read, the refused UPDATE of another column and both repairs. The plan-cache key is section 3's token text (parameter tokens
   included) plus, per parameter in ordinal order and then per name in sorted order,
   its bound type and an exact rendering of its value, length-prefixed like the
   existing scope components, with a tag byte distinguishing NULL from an empty
   BYTES or STRING: floats by `math.Float64bits`/`Float32bits` (NaN payloads and
   -0.0 distinct), time.Time by the TIMESTAMP text of its canonical constant (the
   bound value itself, so an entry is shared exactly by the values that ARE the same
   constant, whatever their zones or sub-second parts), arrays by their element count and then element by element; a warm hit
   needs the same values, as today.
   Assignment. EVERY value assigned to a column, bound or not, is assigned by the
   target's promotion lattice, which has INT_TO_LONG, INT/LONG_TO_FLOAT/DOUBLE and
   FLOAT_TO_DOUBLE but no narrowing and no DOUBLE_TO_FLOAT (PromoteValue.java:76-97),
   and the check is by the assigned value's TYPE at planning for every source: an
   INSERT VALUES expression, an UPDATE SET expression and an INSERT ... SELECT column
   (ExpressionVisitor.java:1090, 1118-1123 inject a PromoteValue per target column,
   which refuses with INCOMPATIBLE_TYPE at PromoteValue.java:370). MEASURED beyond
   bound values: `VALUES (5, 1.5 + 0.0)` into FLOAT, `SET f = f + 0.5` on a FLOAT
   column, INSERT ... SELECT of a DOUBLE column into FLOAT and of a BIGINT column into
   INTEGER, `VALUES (21, 3000000000 - 2999999999)` (a LONG expression that fits) into
   INTEGER and `SET i = id` (BIGINT into INTEGER) are all 22000
   [insert_double_expr_into_float_column, update_float_column_plus_double,
   insert_select_double_column_into_float, insert_select_bigint_column_into_int,
   insert_long_expr_into_int_column, update_int_column_from_bigint_column], and Go
   stores the narrowed value for every one of them today (their GO lines); `SET f = f +
   0.5f`, an INT expression into INTEGER, `SET i = i + 1`, INT into FLOAT and DOUBLE,
   FLOAT into DOUBLE, INTEGER into BIGINT by INSERT ... SELECT, and an INT parameter
   into FLOAT all store in both [update_float_column_plus_float,
   insert_int_expr_into_int_column, update_int_column_plus_literal,
   insert_int_literal_into_float_column, insert_int_literal_into_double_column,
   insert_float_literal_into_double_column, insert_select_int_column_into_bigint,
   insert_int_param_into_float_column]; a DOUBLE literal into BIGINT is 22000 in both
   [insert_double_literal_into_bigint_column]. So the executor's value converters
   (functions/proto_value.go:263-297 and the executor's UPDATE/INSERT ... SELECT
   converter) stop NARROWING: they are reached only with a value whose type the
   planning check admitted, and they keep their range checks as backstops. The comment
   "Plain-column narrowing ... NOT checked" (logical_predicate.go:5668-5670) goes with
   the narrowing. For bound values: a LONG parameter stored into an INTEGER column is
   22000 "A value cannot be assigned ..." even when it fits
   [insert_long_into_int_column, insert_long_overflow_into_int_column], an INT
   parameter is stored [insert_int_into_int_column] and promotes into a BIGINT
   column [insert_int_into_bigint_column], a DOUBLE parameter into a FLOAT column is
   22000 [insert_double_into_float_column] and a FLOAT one is stored
   [insert_float_into_float_column]. The check is TYPE-based and made at planning
   from the bound constant's type (at run time the converter sees int64 for INT and
   LONG alike). Declared consequence for Go callers: a float64 argument into a FLOAT
   column changes from stored to 22000 (pass a float32), and an int64 argument into
   an INTEGER column changes likewise (a plain `int` keeps working, by the typing
   above); no Go-only write conversion applies to a bound value (int64 into BOOLEAN,
   string into BYTES and UUID into STRING are 22000, as the lattice says), while the
   approved Go extension types keep their own rules: a time.Time is a TIMESTAMP value,
   and Go's TIMESTAMP-to-STRING edge stores its canonical text (the table above; v9
   listed time.Time into STRING as 22000, which contradicted its own table, since every
   DATE or TIMESTAMP column is a STRING column). Go today narrows a fitting LONG and reports an
   overflow as 22003 (their GO lines); the INSERT/UPDATE coercion takes the target's
   lattice for bound values, and the same lattice for a LONG-typed literal (a
   literal is INT whenever it fits, so only an out-of-range literal changes, from
   22003 to 22000) and for a DOUBLE-typed literal into a FLOAT column (a decimal
   literal is DOUBLE, ParseHelpers.java:84, so Go's literal narrowing into FLOAT,
   proto_value.go's FloatKind arm, changes from stored to 22000, MEASURED
   [insert_double_literal_into_float_column]; `1.5f` keeps storing
   [insert_float_literal_into_float_column]) and for an out-of-range LONG literal into
   INTEGER, MEASURED 22000 in the target and 22003 in Go today
   [insert_long_literal_into_int_column]. The obsolete pins are revised with them, each
   citing the probe: `float_integer_types_probe_test.go:34-56` (stores `0.1` and `1.5`
   literals into FLOAT and expects success) and `:67-76` (22003), plus every test that
   stores a DOUBLE expression, a DOUBLE or BIGINT column, or a LONG literal or
   expression into a narrower column, found by making the converter's narrowing arms
   panic in a scratch run of the full suite and listing every test that trips it.
   The lattice is RECURSIVE, as the target's is (PromoteValue.java:364-429, 462-505):
   an ARRAY source is promoted element-wise to the column's element type, a RECORD
   source field-wise, after a field-count check, to the struct's field types, an
   empty array (ARRAY<NONE>) takes NONE_TO_ARRAY into any array type, and a STRING
   takes STRING_TO_ENUM or STRING_TO_UUID into an ENUM or UUID column (:93-94); a pair
   with no lane at any depth is 22000 with the INCOMPATIBLE_TYPE message. (SOURCE: two
   nested types whose type codes differ fail the target's VerifyException check before
   a lane is sought, :394-399; no SQL source reaches that, because the planning check
   refuses the assignment first, and Go's recursive check gives the same 22000.)
   MEASURED: `[1.5]` and `[1.5 + 0.0]` into a FLOAT ARRAY, `[1.5]` into a BIGINT ARRAY
   and a DOUBLE struct field `(1.5)` into a FLOAT field are 22000
   [insert_double_array_into_float_array, insert_double_expr_array_into_float_array,
   insert_double_array_into_bigint_array, insert_double_struct_field_into_float]; `[1]`
   into a FLOAT ARRAY, `[3000000000]` into a BIGINT ARRAY, `[]` into a FLOAT ARRAY and an
   INT field `(1)` into a FLOAT field store [insert_int_array_into_float_array,
   insert_long_array_into_bigint_array, insert_empty_array_into_float_array,
   insert_int_struct_field_into_float]; a string literal and a bound string store into
   an ENUM and a UUID column [insert_string_literal_into_enum,
   insert_bound_string_into_enum, insert_string_literal_into_uuid,
   insert_bound_string_into_uuid]; and a string that names no enum value, or is not a
   UUID, is XX000 SemanticException "Invalid enum value for the enum type BLUE" /
   "Invalid UUID value for the UUID type not-a-uuid" [insert_bad_string_into_enum,
   insert_bad_string_into_uuid]. Go today stores the narrowed array element and struct
   field and answers the bad UUID with 22F3H "cannot CAST ..." (their GO lines). Go's
   assignment check becomes ONE recursive function over the column's type, element and
   field lanes included, applied at planning to every source as above; the two string
   lanes convert at run time and fail with the target's SQLSTATE and message, which Go
   can share verbatim.
   The STRING to UUID conversion is the JDK's `UUID.fromString` (PromoteValue.java:
   163-170 calls it and maps its IllegalArgumentException to that SemanticException;
   CastValue's STRING_TO_UUID, CastValue.java:249, calls the same function), ported
   from the JDK 21 source the conformance JVM runs (java/util/UUID.java:238-293), and it
   accepts different strings from google/uuid's `uuid.Parse`, which every Go site uses
   today, in both directions. MEASURED as an INSERT literal, a bound string and a CAST
   (`WS-E target oracle v6`): the target accepts short components (`1-2-3-4-5` is
   `00000001-0002-0003-0004-000000000005`), a `+` sign on a component, upper case, a
   first component wider than eight digits, masked to its low 32 bits (`123456789-1-1-1-1`
   is `23456789-0001-0001-0001-000000000001`), a Unicode decimal digit and a fullwidth
   hex letter (`\u0661-2-3-4-5`, `\uFF21-2-3-4-5`: `Long.parseLong` takes
   `Character.digit`'s alphabet), and a component at `Long.MAX_VALUE`, masked
   (`1-2-3-4-7fffffffffffffff` is `00000001-0002-0003-0004-ffffffffffff`); it refuses
   braces, the `urn:uuid:` prefix, 32 digits without dashes, a leading space, an empty
   component, a component of 2^63 or more (`1-2-3-4-8000000000000000`, beyond
   `Long.parseLong`'s range), a fifth dash (`1-2-3-4--5`, although `parseLong` would read
   `-5`) and a string past 36 UTF-16 units, each XX000 "Invalid UUID value for the UUID type <s>"
   [uuid_literal_*, uuid_bound_*, cast_uuid_*]; Go refuses every accepted form and
   accepts braces, urn and no dashes (its GO lines). The port is one function,
   `values.ParseJavaUUID`: the length check in UTF-16 units, the four dash positions
   and a fifth refused (`indexOf`; `dash4 < 0 || dash5 >= 0`, UUID.java:269-283), each
   component parsed as `Long.parseLong(s, begin, end, 16)` parses it (an optional sign,
   at least one digit, a value outside the signed 64-bit range refused, each
   UTF-16 unit's value from `Character.digit(char, 16)`) and masked as the JDK masks it
   (32, 16, 16, 16 and 48 bits); the digit function is a table of the BMP's radix-16
   digits generated from the JDK and checked by a conformance spec that asks the JVM for
   `Character.digit(c, 16)` over all 65536 chars and compares Go's table. It replaces
   `uuid.Parse` at every string-to-UUID site, which are exactly the six non-test calls
   outside pkg/fdbgo at the reviewed tree (`git grep -n -E
   'uuid\.(Parse|MustParse|ParseBytes|Validate)\(' <TREE> -- 'pkg/**/*.go' 'cmd/**/*.go'
   ':!pkg/fdbgo/**' ':!**/*_test.go'`, six lines on this tree as on 9265beef and
   66663d5d; the positive control `git grep -c 'package values' <TREE> --
   'pkg/recordlayer/query/plan/cascades/values/promote_prepared.go'` is 1). v6's five
   came from `git grep` over the WORKING tree without `--untracked`, which skips
   promote_prepared.go: the file is new to this uncommitted migration, absent from HEAD
   and from the repository's index, and present in every tree object the gates review,
   because each is written from a temporary index after `git add -A`. v7 said "git does
   not track it", true of the index and false of the reviewed trees, and gave a
   working-tree command; a census binds to the tree: the
   PromoteValue port's STRING arm (values/promote_prepared.go:388, `promotionNode.coerce`,
   the lane itself, reached by every PromoteValue the lattice injects and by nested
   array and record promotions, which fails today with an untyped error), the write
   converter (pkg/relational/core/functions/proto_value.go:42), the executor's
   assignment (executor.go:4488), the IN-list and comparison constant fork
   (expr/expr.go:1775), the value CAST (values.go:4661) and the function CAST
   (functions/cast.go:172, inside `functions.CastValue`, which 4.3 deletes as dead: it has
   no non-test caller, so after this change the sites are FIVE; v10 counted the sixth and
   routed a per-row CAST through it, and the measured per-row CAST goes through the
   value CAST, `cast_column_to_date_where`'s GO line carrying values.go's message; the
   value CAST's UUID arm keeps the target's refusal of a surrounding space
   [cast_uuid_surrounding_space]). The rows reach them as follows: an INSERT literal and
   a bound string into a UUID column go through the lattice's PromoteValue
   (promote_prepared.go), whose run-time conversion is the executor's assignment and, on
   the proto write, the write converter; a CAST goes through the value CAST, evaluated
   when the plan runs, whether its operand is constant or not (5.4 deletes the planning-
   time `tryCastConstant`; v11 said "at planning when its operand is constant"); an IN list of
   string literals against a UUID column goes through the constant fork. Every one
   calls `ParseJavaUUID` and fails with the lane's XX000, and a test per path drives
   one accepted and one refused spelling through it. A test fails on a call of
   google/uuid's parsers outside pkg/fdbgo, with an EMPTY allowlist. A CAST that fails takes the
   same XX000 and message, because the target's CAST fails in the same function
   (Go's 22F3H today). Other CAST failures keep 22F3H, which the target shares, and
   take its message prefix: `CAST('x' AS BIGINT)` is 22F3H "Invalid cast operation
   Cannot cast string 'x' to LONG: For input string: \"x\"" in the target and lacks
   the "Invalid cast operation " prefix in Go [cast_string_to_bigint_select]; Go's
   `InvalidCastError` renders the prefix, and the pins that quote the old message are
   revised with the row.
   The STRING to ENUM lane matches the string EXACTLY, case and surrounding spaces
   included, against each value's user identifier (`protoname.ToUserIdentifier` of
   its proto name, as PromoteValue.java:151-160 compares against
   `ProtoUtils.toUserIdentifier`): `'red'`, `'RED '` and a bound `'green'` into
   `CLR('RED', 'GREEN')` are XX000 "Invalid enum value for the enum type <s>"
   [enum_lowercase_literal, enum_padded_literal, enum_lowercase_bound]. The ENUM rows
   are compared once RFC-257 WS-J F6 gives Go enum DDL (section 8); until then their
   Go assertion runs through record-layer metadata.
   Tests: every row above as a Go assertion, cold and warm, plus a nested array of
   records and a record holding an array, each with a narrowing and a promoting lane.
   Consequences, each declared: `LIKE ?` becomes a syntax error, as in the target
   (Go accepts it today; CHANGELOG and the audits' "dynamic pattern restriction is a
   Go gap" claim, cascades W1 and relational W3, are corrected). `ORDER BY ?` binds
   a constant, not a position; ordering by a constant is 0AF00 in the target for a
   literal or an expression too [order_by_string_literal,
   order_by_constant_expression, prepared_order_by_param], because no access path
   provides an ordering by a constant and the target has no physical sort, while
   Go's sanctioned in-memory sort fallback answers it (rows in scan order): an
   allowed read-side reach beyond the target, recorded in DIVERGENCES.md with the
   three probes, not a rejection bolted on. LIMIT and OFFSET parameters keep
   working as a LONG constant: LIMIT is an approved Go extension (the target rejects
   LIMIT itself, 0AF00 [prepared_limit_param]). Parameters in DDL and view bodies
   are not admitted by the target's grammar positions and stay rejected (the
   DIVERGENCES.md:1575 consequence is rewritten). EXECUTE CONTINUATION's
   continuation atom and COPY ... FROM take parameters in the target grammar; Go has
   neither statement (section 6), so no binding exists for them.
   The constants are ordinary literal Values, exactly as a literal written in the
   SQL, so a bound value plans exactly as the literal of the same TYPE would; where
   the bound type differs from what the text channel produced (a LONG compared with
   an indexed INTEGER column, whose promotion now sits on the column side) the plan
   is measured, not assumed: a test runs `WHERE i = ?` over an index on an INTEGER
   column with an int32 and with an int64 argument and asserts both plans'
   EXPLAIN (index use or its loss is then a stated fact of the change). Nothing here
   introduces planner ParameterValues (those remain an unused capability, values.go:2379-2395,
   and Java's literal-parameterized plan sharing stays the documented optimization
   gap in query_hash.go). The text channel's workarounds are deleted with
   `substituteParams`: the NaN-payload refusal (the renderable-NaN table) and the
   integer-literal reinterpretation (the time.Time rendering goes with the transport:
   4.3 binds a time.Time as canonical TIMESTAMP text, as the typing above states; v10
   said it was kept); DIVERGENCES.md's
   "Bound parameters stay Unknown-typed at the plan gates" entry is rewritten:
   bound parameters are typed from their values. Tests: every prepared oracle row as
   a Go assertion, cold and warm; ordinals across subqueries, several IN lists and a
   multi-statement batch; named, repeated and mixed parameters; each Go value type;
   NaN payload and -0.0 round trips; a reused []byte buffer mutated after the call
   followed by a warm hit; an invalid UTF-8 string refused with nothing stored.
4. Obsolete pins revised individually: `yamsql/cast_boundary_fdb_test.go:110`,
   `sqldriver/in_list_array_column_fdb_test.go:193`, every `substituteParams`
   test (each becomes a binding test), and every test pinning `ORDER BY ?` as a
   position or a NaN-payload refusal.

## 5. Variadic COALESCE/GREATEST/LEAST (cascades W2, #4171/#4440/#4453)

Target: `VariadicFunctionValue.encapsulate` (:239-272) computes the maximum type
of the arguments (22F00 FUNCTION_UNDEFINED_FOR_GIVEN_ARGUMENT_TYPES when no
physical operator exists for it, which includes every argument NULL), sets the
result nullability by operator (COALESCE: nullable iff ALL arguments are nullable;
GREATEST/LEAST: nullable iff ANY is, :339-376), and promotes each argument to the
common type WHILE KEEPING ITS OWN nullability; `computeResultType` (:131-142)
re-derives the result type from the children on reconstruction. `eval` (:115-118)
evaluates EVERY child before applying the operator. Arity below two is a
VerifyException (:241). Measured: all-NULL COALESCE and GREATEST are 22F00 "The
function is not defined for the given argument types" [coalesce_all_null,
greatest_all_null]; single-argument COALESCE and GREATEST are XX000
VerifyException [coalesce_single_argument, greatest_single_argument];
`COALESCE(1, 1 / 0)` fails with the division error because every child is
evaluated [coalesce_evaluates_every_argument]; mixed numeric promotion
[greatest_mixed_numeric] and nullable-column results
[least_nullable_column, coalesce_nullable_column] agree today.

MEASURED (v2 rounds): COALESCE over BYTES and GREATEST over BYTES are 22F00 [coalesce_bytes,
greatest_bytes]: the target's operator map covers only INT, LONG, BOOLEAN, STRING,
FLOAT, DOUBLE, RECORD and ARRAY (VariadicFunctionValue.java:483-490); arguments with no
maximum type are 22000 with the target's INCOMPATIBLE_TYPE message, "A value cannot be
assigned to a variable because the type of the value does not match ..."
[coalesce_incompatible] (INCOMPATIBLE_TYPE at :247, mapped by ExceptionUtil.java:92-93);
`COALESCE(1, 1 / 0)` and `COALESCE(id, 1 / 0)` raise the division error
[coalesce_eager_div0_literal, coalesce_eager_div0_column]; and result NULLABILITY,
read from the result-set metadata: `COALESCE(n, 0)` is NOT NULL, `COALESCE(n, n)` and
`COALESCE(id, n)` NULL (every column is nullable in the target, the primary key
included), GREATEST and LEAST nullable whenever any argument is
[nullability_coalesce_nullable_literal, nullability_coalesce_nullable_nullable,
nullability_coalesce_notnull_nullable, nullability_greatest_notnull,
nullability_greatest_nullable, nullability_least_mixed].

Go today: `values/scalar_function_catalog.go:380-389` types all three through
`CommonValueType` (:500-542), returns NullType for all-NULL arguments (so they succeed),
accepts one argument and admits the types CommonValueType unifies (BYTES among them,
though `greatest_bytes` already fails there with 22F00 on its GO line); on top of that
`ScalarFunctionValue.Type()` (values.go:2516-2521) forces EVERY scalar function result
nullable (the blanket override the umbrella and cascades W2 name), which is why
`COALESCE(n, 0)` is NULL on its GO line; `simplifyCoalesce` (simplifier_value.go:333-360,
493-503) folds a COALESCE whose first argument is a non-nil ConstantValue, which is why
both division probes return 1 on their GO lines; COALESCE over BYTES succeeds, and the
incompatible case fails with an internal 0AF00.

Design: the three become one Go port of VariadicFunctionValue with Java's encapsulate,
computeResultType and eager eval, and:
1. Admission is the target's operator map. BYTES, UUID and ENUM arguments, types the
   target has and rejects, fail with 22F00 and the target's message. DATE and TIMESTAMP,
   the approved Go-only types the target does not have, keep being admitted by an
   explicit extension entry in the map (not by a generic fallback), with their own
   tests; no other type is added.
2. No maximum type: 22000 with the target's INCOMPATIBLE_TYPE message (Go's
   CANNOT_CONVERT_TYPE code). All-NULL arguments: 22F00 [coalesce_all_null,
   greatest_all_null]. Fewer than two arguments: XX000 (Go's internal-error code, with a
   message naming the arity where Java's VerifyException has none: the SQLSTATE is
   shared, the message cannot be).
3. Nullability is a property of each catalog ENTRY, derived from the Java class the
   entry ports, never from a generic rule. MEASURED, all-NOT-NULL arguments:
   GREATEST(1, 5) and COALESCE(1, 2) are NOT NULL [nn_greatest_literals,
   nn_coalesce_literals]; 5 % 2, 5 & 3 and 1 + 2 are NULL [nn_mod_literals,
   nn_bitand_literals, nn_add_literals], because the target's ArithmeticValue is
   always nullable (ArithmeticValue.java:126-127, Type.java:404-405), `id + 1` included
   [nn_pk_plus_literal]; a literal is NOT NULL [nn_literal]; and every column, the
   primary key included, is NULL, which every `SELECT id` row of the oracle shows (for
   example [like_escape_percent, prepared_named]). The census, over
   `scalar_function_catalog.go:336-429` as it stands AFTER this workstream and WS-J
   F4 (the MOD, bit and bitmap operators have left the catalog for the ArithmeticValue
   lane table of item 6, where every lane is nullable as ArithmeticValue.java:126-127
   says, so they are not catalog entries any more and are not listed here):
   - AnyArgument (nullable iff any argument is): GREATEST, LEAST
     (VariadicFunctionValue.java:339-376);
   - AllArguments (nullable iff every argument is): COALESCE (same);
   - Always: every Go-only function of the approved extension, because several return
     NULL on non-NULL input (POWER on NaN/Inf, values.go:2902-2913; EXP on overflow,
     :3145; LN and LOG out of domain, :3160-3186; SQRT on Inf/NaN; NULLIF, :2927-2937;
     a type-mismatch decline) and none has a target class to derive anything tighter
     from [nullability_scalar_function].
   So removing the blanket override in `ScalarFunctionValue.Type()` changes exactly
   three entries, and every other entry keeps returning a nullable type. (Until WS-J
   F4 lands, the bit and bitmap entries are still in the catalog and carry the Always
   rule, which is the nullability their lanes will have.) Each entry
   carries its rule as a field, and a test drives every catalog entry through it. The
   type is rederived, never copied, wherever a ScalarFunctionValue is rebuilt: the
   rebuild sites that copy `Typ` verbatim (replace.go:313, map_field_values.go:83,
   simplifier_value.go:158,391, logical_predicate.go:4558,
   rule_aggregate_data_access.go:506) call the constructor that applies the entry's
   rule to the new children. The readers of the nullability that a NOT NULL
   COALESCE/GREATEST/LEAST now reaches are each given a test with such a value:
   uniqueness enforcement (physical_equality_shape.go:166-303), promotion of a
   prepared value into a NOT NULL field (promote_prepared.go:341), the AND/OR value
   (value_andor.go:101), IN-to-explode (rule_in_to_explode.go:346), OF TYPE
   (value_oftype.go:89), and the simplifier's `coalesceReplacementFor` and
   `sameDeclaredType`.
   Every Value's result nullability becomes OBSERVABLE through item 4's
   EffectiveConstant port, whose NOT_NULL arm decides IS NULL and IS NOT NULL over a
   NOT NULL value without evaluating it, so a value Go types NOT NULL where the target
   types it nullable answers where the target raises, and the converse raises where
   the target answers. MEASURED: `CAST('x' AS BIGINT) IS NULL` answers no rows while
   the CAST alone is 22F3H [is_null_cast_string_to_bigint_where,
   cast_string_to_bigint_select], because a CAST takes its operand's nullability
   (ExpressionVisitor.java:532, `targetType.withNullability(underlyingType.
   isNullable())`; `CAST(1 AS BIGINT)` is NOT NULL, `CAST(n AS INTEGER)` NULL
   [cast_literal_nullability_select, cast_column_nullability_select]), while Go's
   `CastValue.Type()` forces every CAST nullable (values.go:4235-4240) and its GO line
   raises; `CAST(1 / 0 AS BIGINT) IS NULL` fails in both, its operand being a nullable
   ArithmeticValue [is_null_cast_div0_where]; a CASE is nullable whatever its branches
   (PickValue.java:200, `withNullability(true)`), so its IS NULL evaluates
   [is_null_case_literals_where, is_null_case_div0_condition_where]. So the census is
   not the catalog's alone: EVERY Value class of the values package states its
   nullability rule and the target class it derives it from, or the reason there is
   none, in one table the implementation writes beside the classes (60 classes define
   `Type()` today, `git grep -n 'func (.*) Type() Type' --
   'pkg/recordlayer/query/plan/cascades/values/*.go'`, tests excluded); CastValue
   takes its operand's nullability, PromoteValue its target type's as built,
   PickValue and the CASE it lowers to are always nullable, and field access,
   subscripts and constructors are listed with their target rules. A test enumerates
   the classes that implement Type() and fails on one missing from the table, and each
   class whose type can be NOT NULL is driven under IS NULL and IS NOT NULL with an
   erroring child, asserting the target's outcome.
   The arm has a second, worse failure than the erroring child: a value Go types NOT
   NULL that can still EVALUATE to NULL. IS NULL over it folds to FALSE without
   evaluating it and drops a row the target keeps. So the table carries, per class, a
   SOUNDNESS rule, and every NOT NULL typing is made sound or loosened before the arm
   lands:
   - CastValue. Go's `castEvaluated` falls through to `return nil, nil` for a source it
     cannot convert (values.go:4685) and `CastValue.Type()`'s comment admits a NULL "on
     out-of-range / unsupported source" (:4234-4240). Taking the operand's nullability
     (above) is sound only if a CAST never produces NULL from a non-NULL operand, as
     Java's CastValue never does (each of its operator arms converts or throws). So
     every arm of `castEvaluated` either converts or returns the CAST error the target
     raises (22F3H, the invalid-cast message), and the fall-through becomes an error; a
     test drives every (source, target) pair of the cast table with a non-NULL operand
     and asserts a non-NULL value or an error, never NULL.
   - Scan leaves. Go types a proto3 scalar, a proto2 required field and a flat
     repeated array NOT NULL at the scan (proto_field_type.go:28-31, 58-65), and a
     FieldValue inherits its root's nullability (field_value.go:456-490), which is
     widened to nullable only on a null-on-empty edge (quantifier.go:393-404) or a
     gated-path seed (ordinal_seed.go:241-249). A NOT NULL column read from the
     null-supplying side of an outer join IS NULL on an unmatched row, so every Go
     outer-join path must widen it: the materialized outer-join NLJ (RFC-152), the
     null-on-empty quantifier, the gated path and the clustered path. The test is IS
     NULL and IS NOT NULL over a NOT NULL column of the null-supplying side, on each of
     those paths, with an unmatched row, asserting the row is kept (IS NULL) and
     dropped (IS NOT NULL) as the target answers; a path that does not widen is fixed
     in this workstream, since it is the arm that makes it observable.
   - Every other class in the table states its rule (derived from its target class)
     and the test that shows it cannot evaluate to NULL when typed NOT NULL.
4. Eager evaluation, and simplification exactly as the target simplifies. The target
   has no generic constant folding. Its literals and bound parameters are constant
   objects (COVs, MutablePlanGenerationContext.java:229-247); an inline NULL is a
   NullValue, not a COV (ExpressionVisitor.java:890-891); and `CAST(NULL AS T)` is a
   typed NullValue at translation, never a CastValue (CastValue.inject, CastValue.java:
   454-458). Two rule sets simplify values:
   - DEFAULT (DefaultValueSimplificationRuleSet.java:50-54), used for result values:
     CollapseNullStrictValueOverNullValueRule (an ArithmeticValue, CastValue,
     FieldValue, NotValue, PromoteValue or SubscriptValue with a NullValue child
     becomes a NullValue of its type, CollapseNullStrictValueOverNullValueRule.java:
     53-59), the two field-over-constructor/field compositions, and the record-
     constructor-to-star collapse.
   - PREDICATE (DereferenceConstantObjectValueRuleSet.java:50-56), applied by
     ValuePredicateSimplificationRule (:62-74) to a predicate's value and to the
     comparand of every non-unary value comparison: the default rules plus
     DereferenceConstantObjectValueRule (a BOOLEAN COV becomes a literal and a NULL COV
     a NullValue; every other COV stays a COV, DereferenceConstantObjectValueRule.java:
     70-83), EvaluateConstantPromotionRule, and EvaluateConstantCoalesceRule, which
     folds only over heads that are a NullValue or a NOT NULL LiteralValue (`cannotFold`,
     EvaluateConstantCoalesceRule.java:105-108): leading NullValues are skipped, the
     first such literal replaces the COALESCE, and any other head keeps it.
   A QUERY-PREDICATE rule set, ConstantFoldingRuleSet (ConstantFoldingRuleSet.java:
   36-51), simplifies predicates: the nine default predicate rules
   (DefaultQueryPredicateRuleSet.java:41-59: identity and annulment for AND and for
   OR, absorption for each, NOT over a comparison, and De Morgan over AND and over
   OR), ValuePredicateSimplificationRule applying the PREDICATE value set above, and
   three predicate-level folds, ConstantFoldingValuePredicateRule,
   ConstantFoldingPredicateWithRangesRule and ConstantFoldingMultiConstraintPredicate-
   Rule. They fold a comparison only over EFFECTIVE constants. `EffectiveConstant.from`
   has two overloads (ConstantPredicateFoldingUtil.java:262-301). Over a Value it
   recognises exactly three shapes: a NullValue (NULL), a BOOLEAN LiteralValue (TRUE,
   FALSE or NULL), and a value whose result type is NOT NULL (NOT_NULL, which decides
   IS NULL and IS NOT NULL whatever the value evaluates to); anything else, a NOT, AND,
   OR or comparison over literals included, is UNKNOWN and is not folded. Over a plain
   Object, a comparison's literal comparand, it answers NULL for null, TRUE or FALSE
   for a Boolean, a Value's answer for a Value, and NOT_NULL for anything else. The set
   runs over the WHOLE conjunction: the rule builds `AndPredicate.and(predicates)`
   (which drops tautologies, AndPredicate.java:188-205), optimizes it, yields nothing
   when the result is semantically equal to the input, and otherwise yields a select
   whose predicates are the result's conjuncts, or the result itself when it is not an
   AND (QueryPredicateSimplificationRule.java:105-131); `rejectsNull` runs the same set
   over a predicate whose correlations to the null-on-empty alias are translated to
   typed NullValues (ConstantPredicateFoldingUtil.java:177-203).
   The simplified predicate does not REPLACE the original. QueryPredicateSimplification-
   Rule is an exploration rule (RewritingRuleSet.java:53, QueryPredicateSimplification-
   Rule.java:105-131) whose result joins the reference as an alternative, and the
   REWRITING phase's prune (CascadesPlanner.OptimizeGroup, :649-690) keeps one final
   member by RewritingCostModel.compare (RewritingCostModel.java:58-115): fewest outer
   joins, then selects, then table functions, then fewest normalized conjuncts, then
   more predicates at deeper levels, and last the smaller `semanticHashCode`.
   MEASURED with that prune traced at the moment it runs (Java step `planRuleTrace`
   records every REWRITING group left with more than one final member, each member's
   criteria and semantic hash, and the model's verdict on every pair):
   - A fold to `true` removes its conjunct (0 conjuncts against 1) and wins on a
     DETERMINISTIC criterion: `(1 / 0) + CAST(NULL AS INTEGER) IS NULL` collapses to
     `true` and answers every row with no filter [null_strict_div0_cast_null_is_null_where,
     null_strict_div0_cast_null_is_null_where_explain,
     trace_null_strict_div0_cast_null_is_null], and `COALESCE(TRUE, 1 / 0 = 1)` does the
     same [trace_coalesce_true_div0]; a NOT over a typed NULL in a COALESCE head
     collapses to NULL, is skipped, and the TRUE after it folds, so the collapse is
     reached through the COALESCE's argument, the set applying recursively
     [coalesce_not_cast_null_head_where, coalesce_not_cast_null_head_where_explain].
   - A fold to `null` or `false` keeps one conjunct and TIES with the unfolded predicate
     on every criterion but the semantic hash, which then decides.
     `(1 / 0) + CAST(NULL AS INTEGER) = 1` plans `FILTER null` and answers no rows over
     this round's two-table schema, and keeps `promote(@c6 AS INT) EQUALS @c6 / @c8 +
     NULL` and raises the division over the seven-table v4 schema, in the same JVM, the
     two traces showing the verdict follow the hashes
     [trace_null_strict_div0_cast_null_eq_one, trace_v4_schema_div0_cast_null_eq_one,
     null_strict_div0_cast_null_eq_one_where, null_strict_div0_cast_null_eq_one_explain,
     v4_schema_div0_cast_null_eq_one_where, v4_schema_div0_cast_null_eq_one_explain,
     v5_schema_div0_cast_null_eq_one_where_first, and v4's
     null_strict_cast_null_beside_div0_where, _where_empty_table and _where_explain];
     which statement of a shape the JVM plans first does not change it
     [first_planning_a_exec, first_planning_a_explain, first_planning_b_explain,
     first_planning_b_exec]. A column beside a typed NULL is the same tie
     [trace_null_strict_column_cast_null_eq_one: `FILTER null` here; v4's
     null_strict_column_beside_cast_null_where_explain kept `_.N + NULL`], and so is
     `COALESCE(1 / 0, 5) IS NULL`, whose NOT NULL COALESCE (item 3) makes the IS NULL an
     effective-constant `false`: `FILTER false`, no rows [coalesce_div0_five_is_null_where,
     coalesce_div0_five_is_null_where_explain, trace_coalesce_div0_five_is_null].
   - Where the simplified alternative folds nothing that is evaluated, both members
     answer alike. `(1 / 0 = 1) OR ((NOT FALSE) = TRUE)` simplifies only to
     dereferenced constants, `'true' EQUALS NOT 'false'` staying a comparison because a
     NOT over a literal is UNKNOWN, and raises the division whichever member wins
     [fold_div0_or_not_false_where, fold_div0_or_not_false_where_explain,
     trace_fold_div0_or_not_false]; a COALESCE with a NOT head is kept by both members
     [trace_coalesce_not_head].
   - A COALESCE directly under AND or NOT in a WHERE is XX000 VerifyException in the
     target before any simplification runs (measured for AND and NOT; OR takes the same
     `AndOrValue` code, SOURCE): translating AND/OR and NOT to predicates
     verifies that each child is a BooleanValue (AndOrValue.java:217-218, NotValue.java:
     85), and a COALESCE is a VariadicFunctionValue. The failing frame,
     `AndOrValue.toQueryPredicate` called from `LogicalOperator.generateSimpleSelect`,
     was read from the oracle run with CONFORMANCE_DEBUG=1
     [coalesce_true_div0_and_column_where, not_coalesce_false_div0_where, and their
     EXPLAINs].
   - The rule simplifies the WHOLE conjunction, so a fold beside another conjunct is
     decided on the conjunct count too (v6). `n > 0 AND COALESCE(1 / 0, 5) IS NULL`
     annuls to `false`, one conjunct against two, and answers no rows [conj_false_where,
     conj_false_explain: `FILTER false`, trace_conj_false: members `conjuncts=1
     predicates=[false]` and `conjuncts=2`, verdict -1]; `n > 0 AND (1 / 0) + CAST(NULL
     AS INTEGER) IS NULL` drops its folded `true` by identity and answers the rows with
     `n > 0` [conj_true_where, conj_true_explain: `FILTER _.N GREATER_THAN promote(@c7 AS
     LONG)`, trace_conj_true: one conjunct against two].
   - A constant EXPRESSION as a comparand is not folded and stays sargable: `id = 1 + 2`
     scans `[IS T, EQUALS promote(@c7 + @c9 AS LONG)]`, `id > 3 - 2` the range
     `[GREATER_THAN promote(@c7 - @c9 AS LONG)]`, and the rule yields no alternative for
     it (`calls=1 ... exploratory=0`) [constant_expression_comparand_where,
     constant_expression_comparand_explain, constant_expression_range_explain,
     trace_constant_expression_comparand].
   - IS [NOT] NULL over a NOT NULL value is decided by the NOT_NULL arm without
     evaluating the value: `COALESCE(1 / 0, 5) IS NOT NULL` answers every row and
     `CAST('x' AS BIGINT) IS NULL` none [is_not_null_coalesce_div0_where,
     is_null_cast_string_to_bigint_where]; over a NULLABLE value it evaluates it:
     `CAST(1 / 0 AS BIGINT)`, a CASE with a dividing condition and `GREATEST(1, 1 / 0)`
     under IS NULL raise the division [is_null_cast_div0_where,
     is_null_case_div0_condition_where, is_null_greatest_div0_where] (item 3's census
     types each).
   - One CASE row is a target defect: `CASE WHEN id > 0 THEN 1 ELSE 1 / 0 END IS NULL`
     is 22000 INCOMPATIBLE_TYPE [is_null_case_div0_branch_where], while the CASE itself
     answers 1 [case_div0_branch_select], the same IS NULL over two literal branches
     and over two arithmetic branches answers no rows [is_null_case_literals_where,
     is_null_case_both_arithmetic_where], and a SELECT of the CASE over literals answers
     1 [case_literals_select]. The frame, read with CONFORMANCE_DEBUG=1
     (`/var/tmp/fdb-upgrade-recovery/wse7-debug.log`), is `PickValue.resolveTypesFrom-
     Alternatives` (PickValue.java:200, which demands EQUAL alternative types,
     nullability included; the result's `withNullability(true)` is :203) called from
     `PickValue.withChildren` under `Simplification.computeCurrent`, inside
     ValuePredicateSimplificationRule. The trigger (SOURCE, from the rule set and the
     frame): the CASE's literal branch is `promote(1 AS INT NULL)`, promoted to the
     nullable type of its arithmetic sibling; EvaluateConstantPromotionRule's case 3
     (EvaluateConstantPromotionRule.java:75-85) drops a promotion that only adds
     nullability to a NOT NULL value, the branch becomes `1` typed `INT NOT NULL`, and
     the rebuild compares it with the arithmetic branch's `INT NULL` and fails. With two
     literal branches both promotions go and the types stay equal, and with two
     arithmetic branches neither exists [the two controls]. Go ports case 3 (it is part
     of the PREDICATE set) and does not port the defect: Go's PickValue carries the
     result type it was built with (`NewPickValue`, value_pick.go:32-43, which takes any
     type; a CASE's is nullable because the walker builds it with `CommonValueType`,
     walk.go:650, 748 through `caseResultType` :660-662, which returns the widened type
     WITH nullability, scalar_function_catalog.go:543, the nullable type Java's :203
     gives) and a rebuild keeps it, with no equality check
     over the rebuilt alternatives, so the same statement answers what
     the target's rules define (no rows); a unit test rebuilds a CASE through the set
     with exactly this pair of branches and asserts the rebuilt type.
   MEASURED, every class of COALESCE head: in a WHERE, `COALESCE(TRUE, 1 / 0 = 1)`
   folds and its EXPLAIN has no filter left [coalesce_true_erroring_tail_where,
   coalesce_true_head_where_explain]; a NULL or `CAST(NULL AS BOOLEAN)` head is skipped
   and the TRUE after it folds, even with an erroring third argument
   [coalesce_null_head_where, coalesce_cast_null_head_where,
   coalesce_null_head_erroring_tail_where, coalesce_cast_null_head_erroring_tail_where];
   a bound TRUE and a bound NULL behave as the literals [coalesce_bound_true_head_where,
   coalesce_bound_null_head_where]; a NOT, AND, OR or arithmetic head, and an INT
   literal head (an INT COV is not dereferenced), keep the COALESCE and raise the
   division error [coalesce_not_head_where, coalesce_and_head_where,
   coalesce_or_head_where, coalesce_arith_head_where, coalesce_int_literal_head_where];
   the kept COALESCE is visible in the plan as `coalesce_boolean(NOT 'false', @c10 /
   @c12 equals @c10)` [coalesce_not_head_where_explain], NOT unevaluated over a
   dereferenced `'false'`. In a SELECT nothing folds a COALESCE, a TRUE head, a bound
   one and a NOT head included [coalesce_true_erroring_tail_select,
   coalesce_bound_true_head_select, coalesce_not_head_select,
   coalesce_evaluates_every_argument]. The null-strict collapse is visible in a
   projection: `SELECT (1 / 0) + CAST(NULL AS INTEGER)` answers NULL and its plan is
   `MAP (NULL AS _0)` [null_strict_cast_null_beside_div0_select,
   null_strict_cast_null_beside_div0_select_explain], and folded booleans in a
   projection stay nullable: `SELECT NOT FALSE`, `TRUE AND TRUE` and `NOT (n > 1)` are
   BOOLEAN NULL [not_false_select, true_and_true_select, not_column_comparison_select].
   Go today folds in four places, and only one of them is the target's.
   (1) The TRANSLATOR folds every WHERE, ON, QUALIFY, EXISTS and subquery predicate
   BEFORE Cascades sees it: `predicates.SimplifyPredicateValues` REPLACES the walked
   predicate at 19 sites (`git grep -c 'predicates.SimplifyPredicateValues(' --
   'pkg/relational/core/embedded/*.go'`, tests excluded: logical_predicate.go 14 at
   :163, 192, 238, 1345, 1381, 1427, 1668, 1724, 1788, 2712, 2754, 4479, 5350 and 7887;
   plan_visitor.go 2 at :1048 and 1086; bound_query.go:419; logical_qualify.go:47;
   subquery_clause.go:40). The target's translator has no such pass: it calls only
   `Value.simplify` with the DEFAULT set, inside pull-ups (Expression.java:243-245,
   303; Expressions.java:104-108; OrderByExpression.java:80, 92), and predicates fold
   only in the planner. (2) QueryPredicateSimplificationRule runs the same
   `SimplifyPredicateValues` on each predicate SEPARATELY
   (rule_query_predicate_simplification.go:47-55), not on the conjunction. (3)
   `rejectsNull` substitutes NULL with its own null-strict collapse
   (`substituteNullAtAlias`), then runs `SimplifyPredicateValues` and then the Go-only
   driver `Simplify(..., DefaultSimplifyRules())` (rule_eliminate_null_on_empty.go:
   149-153), whose twelve rules (simplifier.go:145-159) include two that fold composite
   subtrees through `EvaluateConstant` (`ComparisonConstantSimplifyRule`,
   rule_simplify.go:398-417, and `ValuePredicateConstantFoldRule`,
   rule_value_predicate_fold.go:54); that call is the driver's ONLY non-test caller.
   (4) `values.SimplifyValue` (simplifier_value.go:32-80) is one function for both
   value sets: it folds every whitelisted composite whose inputs are constant (NOT,
   AND/OR, arithmetic, CAST, PROMOTE, scalar functions, CASE, PICK and EvaluatesTo,
   :80-104) into a literal and COALESCE over any constant head; its predicate folds
   treat any `BooleanValue` and any non-nil `ConstantValue` as known
   (`effectiveConstant`, predicates/simplifier_predicate_values.go) and ignore NOT NULL
   types. Its non-test callers outside the simplifier are pullup.go:83,
   match_info_merge.go:588 and simplifier_predicate_values.go:27, 30, 47; the generic
   `DefaultFolder` (values/folder.go:31-40) and `SimplifyAll` (values_helpers.go:76-79)
   have no non-test caller, and `embedded.foldConstantProjections`, which v5 said they
   served, does not exist (its only mention is the comment at folder.go:10). Every one
   of the four boolean-head WHERE rows and the SELECT NOT row is 0AF00 in Go today,
   `coalesce_arith_head_where` and `coalesce_int_literal_head_where` return rows, the IS
   NULL collapse, `COALESCE(1 / 0, 5) IS NULL` and both conjunction rows raise 22012,
   `CAST('x' AS BIGINT) IS NULL` raises 22F3H, and the nested collapse and the COALESCE
   under AND or NOT are 0AF00 (their GO lines). Design:
   a. The translator folds are DELETED, all 19: a SQL predicate reaches Cascades as it
      was walked, so the REWRITING prune ranks the unfolded member against the
      simplified one exactly as the target's does. Without this the prune tests of (j)
      would be vacuous for SQL, the fold having already replaced the predicate. MEASURED
      on this tree with the 19 calls made the identity (a detached worktree of the tree
      `3e7ea9aa4b29a2d3f9c41e1e860be337bbc0ca47`, `bazelisk test //...
      --test_tag_filters=-stress,-conformance_java --keep_going`,
      `/var/tmp/fdb-upgrade-recovery/wse7-nofold-bazel.log`): 90 of 92 targets pass,
      and the three failing tests are exactly these. Two embedded unit tests pin the
      fold itself (TestBuildLogicalPlanWithCatalog_RHSScalarFunctionFolded and
      _RHSArithmeticFolded: `got "ORDER.price#2 = (1 + 2)", want ORDER.price#2 = 3`,
      `/var/tmp/fdb-upgrade-recovery/wse7-nofold-embedded-test.log`); they are
      rewritten to pin that the predicate reaches Cascades unfolded. And
      TestFDB_ExistsInnerShadow's `case1_notexists_colliding_foldable` declines on the
      scope-ambiguity arm instead of the anti-join guard's "outer-only conjunct" arm,
      both 0A000 (the target answers {11,12};
      `/var/tmp/fdb-upgrade-recovery/wse7-nofold-shadow.log`, the same fold-free tree,
      that test alone): the shape reached the guard only because
      the translator folded `COALESCE(1, MA."C")` to 1 and so erased the colliding
      reference, a fold the target never makes (an INT literal head is not foldable
      [coalesce_int_literal_head_where]). Its pin becomes the ambiguity decline with
      that reason, and the guard keeps its coverage through the same test's
      non-colliding Case-1 pins (`case1_notexists_noncolliding` and the positive
      twins); the multi-source inner-shadow gap that makes it a decline at all is
      TODO.md's mint-per-leg entry. Sargability does not depend on the fold, in either
      engine: the target keeps `promote(@c7 + @c9 AS LONG)` unfolded as a scan
      comparand (above), and Go plans `id = 1 + 2`, `customer_id = 40 + 2` and `amount
      > 3 - 2` as the same primary, index and covering range scans with and without the
      fold (TestPlanHarness_ConstantExpressionComparandIsSargable, added, passing on the
      current tree and in the fold-free run above). That test's three rows compare a
      BIGINT column with an INT or LONG constant, which none of the walk-time
      coercions below touches (they fire only for a DOUBLE column, a FLOAT column or a
      floating constant against an integer column), so its green in the fold-free run is
      a statement about the Cascades path alone.
      EVERY PLACE GO EVALUATES A CONSTANT AT PLANNING, not only the Simplify entry points.
      v7's population was the callers of `values.EvaluateConstant` only, which misses
      every direct `Evaluate(nil)`; v8's population is every non-test call, in pkg/ and
      cmd/, that evaluates a Value or a predicate with no evaluation context: a call of
      `EvaluateConstant`, and a call of a method `Evaluate` or `Eval` whose one argument
      is the literal `nil` (v8's first population named `Evaluate` only and missed the
      predicate evaluation below). Measured at the reviewed tree (SCOPE.md names it), comment lines
      dropped: `git grep -n -E 'EvaluateConstant\(' <TREE> -- 'pkg/**/*.go'
      'cmd/**/*.go' ':!**/*_test.go'` gives 18 code lines, the definition (values.go:1571)
      and 17 calls, and `git grep -n -E '\.Evaluate\(nil\)' <TREE> -- ...` the same
      pathspecs gives 16 code lines (and four comment lines), two of them in the rowdiff
      oracle (pkg/relational/conformance/rowdiff/oracle.go:936, 963), a conformance
      harness evaluating its own expressions, outside the engine and excluded BY NAME, and
      one the body of `EvaluateConstant` (values.go:1575), and `git grep -n -E
      '\.Eval\(nil\)' <TREE> -- ...` the same pathspecs gives 1 code line,
      logical_predicate.go:8072. Every other site is assigned below. NOT covered, first: an evaluation under a context built for the purpose (at
      the tree the one context-bearing evaluation outside the executor and the Value
      implementations is insert_cascades.go:204's `cell.Evaluate(clock)`, which runs when
      an INSERT executes, not at planning), and a helper that evaluates under another name
      except through its own `Evaluate(nil)`. DELETED with the generic fold and the Go-only
      driver: simplifier_value.go:52, folder.go:40, rule_simplify.go:417, rule_value_predicate_
      fold.go:54; and, found by the widened population, simplifier_value.go:136 (a PROMOTE
      over a constant evaluated into a literal: the target's EvaluateConstantPromotionRule,
      EvaluateConstantPromotionRule.java:55-92, rewrites only a NULL, an untyped empty
      array and a nullability-only promotion, and never evaluates one, so a numeric or
      STRING→UUID promotion stays a `promote(...)` comparand as the target keeps it, and
      the ported rule replaces the arm), simplifier_value.go:277 (`tryCastConstant`, a
      CAST over a constant: the target's simplification package has no rule over
      CastValue, `git -C fdb-record-layer grep -l CastValue -- '*/values/simplification/*'`
      names only CollapseNullStrictValueOverNullValueRule, which collapses a CAST of NULL
      and never evaluates), and predicates/comparisons.go:382 (`Comparison.Eval`, whose
      only non-test caller is ComparisonConstantSimplifyRule, rule_simplify.go:384, a rule
      of the deleted driver; `EvalAgainst` stays). Neither of the two new deletions was in
      the fold-free prototype, which kept Go's simplifier (5.4(c), "THE MECHANISM"), so the
      implementation gate re-runs the whole-suite accounting on the port and lists every
      pin they move. ANALYSIS ONLY, reading a constant without replacing anything a plan
      evaluates: physical_equality_shape.go:291, 344, 485, 818 (cardinality and equality
      shape), plans/ordering.go:510, 657 (constant ordering keys) and predicates/
      comparisons.go:1028 (EXPLAIN text of a literal comparand); and, of the direct
      evaluations, value_range.go:136, 144 and 152 (`RangeValue.Cardinality`, constant
      bounds sizing a range table function), plans/cost.go:1253 and
      rule_sink_limit_into_vector_scan.go:119 (a vector scan's literal K, for its cost and
      for folding the limit into the scan) and logical_qualify.go:351 (a distance-rank
      QUALIFY's literal K, the vector-search extension: a K that does not evaluate
      becomes the runtime cap, evaluated at execution, so an error surfaces there), and
      logical_predicate.go:8072 (`cp.Eval(nil)` in the EXISTS guard's hazard test: a
      comparison of two constants that is statically TRUE is not a filtering conjunct;
      anything else, an error included, keeps the conjunct flagged); each declines on an
      evaluation error, so none can hide one. OWNED BY ANOTHER SECTION:
      expr.go:1571 and 1605 (a LIKE pattern and a STARTS_WITH prefix must be constant,
      section 1), expr.go:1744 and 1784 (`ResolveIn`'s constant fork and its ENUM
      promotion of a literal item, section 4.1(a)), rule_in_to_explode.go:134 (the
      explode's comparand, replaced by the admission of 4.1(b)),
      rule_implement_in_join.go:601 (`extractInValues`, which serves the literal values
      source only, 4.1) and rule_implement_in_union.go:228 (the IN-union's plan-time
      sources, deleted by 4.1's IN-union conversion). And three WALK-TIME
      NUMERIC COERCIONS that do replace a constant comparand by a literal of the column's
      type: `widenConstAgainstDoubleColumn` (expr.go:1070-1130: an INT, LONG or FLOAT
      constant against a DOUBLE column), `narrowFloatConstAgainstInt` (:1160-1230: a
      FLOAT or DOUBLE constant against an INT or LONG column, rewriting `>`/`<` over a
      non-integral bound to the equivalent integer predicate) and the FLOAT-column
      narrowing (:1350-1400). The target has none of them: it types the comparison by
      promoting the narrower side (a `promote(... AS DOUBLE)` over the column or the
      constant, visible in the WS-J non-integer plans, `FILTER promote(_.F + @c7 AS
      DOUBLE) EQUALS promote(@c9 AS DOUBLE)`). They are KEPT, as the Go read-side
      extension they are (they make a mixed-type comparison sargable where the target's
      promoted column is not), under three stated properties, each tested: the constant
      is evaluated once and the rewrite declines when evaluation fails, so an error the
      target raises (a division by zero in the comparand) is raised by Go per row too;
      the rewritten comparison selects exactly the rows the promoted comparison selects
      (a property test drives each rewrite over every operator, integral and non-integral
      bounds, both zero signs, NaN, the infinities and the int32/int64 edges, against
      the residual comparison of the unrewritten predicate, the same oracle the index
      binder's projectFloatComparisonToIntegerDomain is tested against); and they apply to the
      comparand only, never to a predicate's truth value, so no fold decision of 5.4
      depends on them. DIVERGENCES.md records them as plan-only differences (answers
      equal, EXPLAIN and cache identity Go's own). A census test enumerates that
      population and fails on a site not in this assignment. HOW EVERY CENSUS OF THIS
      DESIGN GETS ITS POPULATION UNDER BAZEL (this one, the entry-point censuses of (b),
      the `readIndexState`/`PeekIndexStates` census of 6.4, and the UUID-parser census
      of 4.3): each is a test in pkg/docscheck, which reads the real source tree from
      its runfiles through `sourceTreeRoot` (MODULE.bazel is a declared input; its
      symlink resolves to the workspace, source_hygiene_test.go:141-149) and enumerates
      tracked plus untracked non-ignored Go files through `trackedGoFiles` (:151-171,
      falling back to the named-exclusion walk without git), so a file new to an
      uncommitted change is in the population. Each parses the files with go/parser and
      matches CALL EXPRESSIONS (the callee's selector name, and for `Evaluate` and `Eval`
      an argument that is the identifier `nil`; v9 widened the population to `Eval` and
      left the matcher naming `Evaluate` only, and a unit test of the matcher feeds it
      `x.Evaluate(nil)`, `x.Eval(nil)`, `x.Eval(ctx)` and a comment holding
      `Eval(nil)`, asserting the first two are sites and the others are not), never text, so a comment is not a site; each keys its
      assignment by file, enclosing function and callee with a count, not by line, so an
      unrelated edit does not move it. Each has three guards: a VACUITY floor (fewer Go
      files parsed than the tree's pkg/recordlayer/query holds fails as "not the real
      tree", the shape of build_membership_test.go:439); a POSITIVE CONTROL, every entry of
      its assignment table must be found, so a census that reads nothing fails on its
      first entry rather than passing; and an ARM TEST that drives the decision function
      over synthetic sources, an unassigned call (fails), an assigned one (passes), a
      comment naming the callee and a call with a non-nil context (not sites), a
      `_test.go` file (excluded) and an empty population (the vacuity arm). The
      implementation gate runs pkg/docscheck under Bazel and reads each census's
      `=== RUN` line and its reported population count.
   b. Go's generic fold is DELETED outright, not confined: the target has none.
      `DefaultFolder`, `ExpressionFolder`, folder.go and `SimplifyAll` go with their
      tests, and `SimplifyValue` loses its `EvaluateConstant` arms. Three value entry
      points remain, each a rule SET passed down its traversal, never a global:
      `SimplifyValue(v)` is the DEFAULT set, `SimplifyPredicateValue(v)` the PREDICATE
      set, and `predicates.SimplifyPredicate(p)` runs ConstantFoldingRuleSet (c). The set
      is a parameter of the recursion: `simplifyChildren` (simplifier_value.go:111-204)
      calls back into the SAME set it was entered with, so a COALESCE nested inside a
      NOT, an AND or a function argument of a predicate is simplified by the PREDICATE
      set [coalesce_not_cast_null_head_where]. The DEFAULT set runs where the target's
      does, which is where it translates a value "with simplification": PullUp.java:154
      (Go's pullup.go:83, its port), and the value translations that ask for it: the
      `translateCorrelations(..., true)` calls on 37 lines of the target's cascades
      package
      (`git -C fdb-record-layer grep -n 'translateCorrelations([^;]*, *true)' --
      'fdb-record-layer-core/src/main/java/com/apple/foundationdb/record/query/plan/cascades'`:
      DerivationsProperty.java 18, SelectMergeRule.java 3, GroupByExpression.java 3,
      DecorrelateValuesRule.java 2, LogicalTypeFilterExpression.java 2,
      SelectExpression.java 2, and one each in RelationalExpression.java:716,
      ConstantPredicateFoldingUtil.java:184, AbstractDataAccessRule.java:734,
      AggregateDataAccessRule.java:177, PredicatePushDownRule.java:345,
      RewriteOuterJoinRule.java:134 and WithPrimaryKeyDataAccessRule.java:155), the
      compensation functions built with simplification on (PredicateMultiMap.java:203,
      416; SelectMergeRule.java:154, 170), `Values.simplify` in
      ImplementStreamingAggregationRule.java:112 and PushRequestedOrderingThroughGroupBy-
      Rule.java:143, and GroupByExpression's primitive-value simplifications (:324, 334,
      435, 464, 794). The implementation writes the census as a table from each target
      site to its Go port (match_info_merge.go:588 is GroupByExpression's), or to the
      reason Go has no counterpart; a test enumerates the callers of each entry point and
      fails on an unassigned new one, a second drives a nested COALESCE through each set
      and asserts which set simplified it, and the projection rows
      [null_strict_cast_null_beside_div0_select and its EXPLAIN: `MAP (NULL AS _0)`] are
      the end-to-end test that the DEFAULT set with the null-strict collapse is reached
      on a result value.
   c. ConstantFoldingRuleSet is ported as ONE rule set and serves its two consumers.
      QueryPredicateSimplificationRule becomes the target's (QueryPredicateSimplification-
      Rule.java:105-131): it builds the conjunction of the select's predicates with the
      target's `AndPredicate.and` (tautologies dropped, one conjunct returned bare, none
      as TRUE), runs the set over it to its fixpoint, yields nothing when the result is
      semantically equal to the conjunction, and otherwise yields a select with the
      result's conjuncts, or the result alone. So `[n > 0, <fold to false>]` annuls to
      `[false]` and `[n > 0, <fold to true>]` reduces to `[n > 0]`, one conjunct against
      two, as the target measures [conj_false_*, conj_true_*]; per-predicate
      simplification would leave `[n > 0, false]`, a two-against-two hash tie that can
      raise the division. `rejectsNull` becomes the target's `foldPredicateAtNull`: the
      null-on-empty alias's correlations are translated to typed NullValues and the
      SAME set runs, the result being FALSE or NULL to reject; the bespoke collapse of
      `substituteNullAtAlias` goes, because the PREDICATE value set carries the
      collapse (e). The Go-only driver is DELETED with its only caller: `Simplify`,
      `DefaultSimplifyRules`, `NormalizationRules` and the rules only they register
      (rule_simplify.go, rule_value_predicate_fold.go, rule_demorgan.go), their tests
      rewritten as tests of the ported set's rules (identity, annulment, absorption,
      NOT over a comparison and De Morgan, each with the target's rule as its spec).
      `rejectsNull` decides whether an outer join may become an inner join, which
      interacts with RFC-152's materialized outer-join NLJ; every RFC-152 test and the
      outer-join rows of the factory corpus run in the implementation gate, and any
      conversion that flips is listed with the target's answer for its shape.
      WHERE THE RULE MEETS A ONE-SOURCE WHERE. The target translates every WHERE to a
      SelectExpression; Go translates a WHERE over one source to a LogicalFilterExpression
      over one ForEach quantifier (cascades_translator.go:2993-3010, `exactFilter`), which
      is exactly the target's one-quantifier select with the quantifier's own row as its
      result, and no Go rule turns one into the other. The ported rule therefore matches
      BOTH: a SelectExpression, whose alternative is a SelectExpression, and a
      LogicalFilterExpression, whose alternative is a LogicalFilterExpression over the SAME
      quantifier. Yielding a select for a filter would be wrong, not merely different:
      "fewest SelectExpressions" is the model's first rung (designated_final.go:146-150,
      RewritingCostModel.java:70-77), so every fold would lose to its own unfolded
      filter. The translator already splits a WHERE into its conjuncts: MEASURED on the
      prototype below, `n > 0 AND (1 / 0 = 1 OR 1 = 1)` is a filter of 2 predicates and
      `n > 0 AND 1 / 0 = 1 AND 1 = 2` one of 3, so the rule's conjunction is built from
      the filter's predicate list as the target builds it from the select's. The Go-only
      filter rules that run beside it (FilterDropTruePredicatesRule, FilterDedup-
      PredicatesRule, FilterMergeRule, NoOpFilterRule; default_rules.go:42-44, 70) derive
      from a yielded filter only forms EQUAL to it (a TRUE conjunct dropped, a duplicate
      dropped, two filters merged, an empty filter removed), never the unfolded
      predicate, so they cannot undo a fold; they are Go-only and stay out of this
      workstream, and the tests below assert the member SET the group reaches PLANNING
      with, not only the survivor.
      ONE RULE, UNDER THE TARGET'S NAME. The two arms are one rule TYPE,
      `QueryPredicateSimplificationRule`, registered as two instances, one whose matcher
      is a SelectExpression and one whose matcher is a LogicalFilterExpression (v8 said
      one instance over `matching.NewAnyOf`; the schedule below is why it is two); the
      registered name is the type's (default_rules.go `shortTypeName`), so both
      instances carry the target's name. The prototype registered the filter arm as a second rule,
      `QueryPredicateSimplificationFilterRule`, and that is why its run reddened
      TestPlannerOptions_DisablePlannerRewriting: DISABLE_PLANNER_REWRITING disables
      rules by the target's names (RewritingRuleSet.OPTIONAL_RULES, which the test
      holds as a literal list: DecorrelateValuesRule, PredicatePushDownRule,
      QueryPredicateSimplificationRule, RewriteOuterJoinRule, SelectMergeRule), and a
      Go-only name is not among them, so the option would have left the filter arm on.
      Under one name, disabling rewriting by the target's name turns off both arms, the
      test's literal list stays the target's, and the test gains a row: with the option
      set, a one-source WHERE holding a fold reaches PLANNING unfolded, and so does a
      two-source one. ITS SCHEDULE IS THE TARGET'S: the target never fires the rule on its
      own; it is the second rule of the conditional chain `decorrelateThenSimplification`
      (RewritingRuleSet.java:50-53, ConditionalCascadesRule.java:42-47), tried on a
      select only when DecorrelateValuesRule yields nothing there, so the target never
      builds `simplify(original)` beside a decorrelated alternative. Go has no
      conditional rule today (v8 registered the rule on its own, which would build that
      member). WS-F section 2 ports the conditional types (its D3,
      `ConditionalExplorationCascadesRule` under Java's names), but D3 provides only the
      types, whose `OnMatch` fails: the chain EXECUTES through D4's
      `ConditionalTransformTask` with D1's progress, where a deduplicated yield is not
      progress (ws-f-design.md 2.1), and it behaves as the target's only once D5's
      staleness gate stops every rule from re-firing each round (2.2) and D12 deletes both
      standalone DecorrelateValuesRule registrations (default_rules.go:159 and :400, both
      composed into REWRITING, planner_options.go:242-243), and this design's step (4)
      replaces the old simplification rule registered beside the second (:398, D12 does
      not touch it; v10 read as though D12 deleted it too): while a standalone
      DecorrelateValuesRule fires first, the chain's own
      decorrelate call deduplicates, which is no progress, and the simplification fires
      beside the decorrelated member. D12 also makes `OptionalRewritingRuleNames` expand
      the chains, so DISABLE_PLANNER_REWRITING names both inner rules. v9 said "lands
      after D3"; this port registers the SELECT instance INSIDE the chain, after
      DecorrelateValuesRule, and lands after WS-F step 7's option is the default (D1 to
      D12 with the physical prune, section 8), so there is no interim in which Go fires
      it unconditionally on a select or without the prune deciding. The name registry
      keeps one of two instances of one name (it skips the second, default_rules.go:
      486-494): the SELECT instance is a member of the chain wrapper, which the registry
      holds under the wrapper's name, and only the FILTER instance is registered under
      the rule's own name, so no instance is skipped; a test pins that both instances
      are reachable. The chain's inner rules must
      agree on their root operator (D3's check, as Java's), and both match a
      SelectExpression, which is why the arms are two instances: one AnyOf instance
      declares no root operator (combinators.go:91-109) and would fail that check. The
      FILTER instance is registered on its own: the target has no one-source filter, and
      DecorrelateValuesRule never matches a filter, so the chain's condition ("the first
      rule yielded nothing") holds for every filter, and registering the instance alone
      is the chain's behaviour there. A test pins that on a select where
      DecorrelateValuesRule yields, the simplification does not fire, and another that
      DISABLE_PLANNER_REWRITING turns off both instances.
      WHO DECIDES (v8). The target decides a fold in REWRITING: the rule yields its
      alternative into the same group, and the group's single surviving final is chosen
      by the REWRITING cost model before any access path exists. v7 let both members
      cross into PLANNING and claimed PLANNING's cost reached the same decision on the
      same conjunct count. It does not: PLANNING ranks max data-access cardinality
      first (planning_cost_model.go:193-219) and a PredicatesFilter takes its child's
      maximum (cardinality_bounds.go:131-136), so beside a primary-key, primary-key IN
      or indexed conjunct the unfolded member, a probe proving few rows with the
      erroring residual, beats `[FALSE]` over a full scan before any predicate is
      counted. MEASURED in the target (Describe "WS-E target oracle v8", 31 pins): with
      a fold the target's predicate set makes, IS NULL over the NOT NULL `COALESCE(1 /
      0, 5)`, the conjunction annuls beside `id = 5`, `id IN (1, 2)`, `n > 0` and `n = 5`
      (n indexed) and every row answers no rows with the plan `COVERING(T_N <,> ...) |
      FILTER false` [pk_type_annulling_fold_where and _explain, and the pk_in_,
      index_range_ and index_eq_ rows], and the null-strict collapse `(1 / 0) +
      CAST(NULL AS INTEGER) IS NULL` reduces beside `id = 5` and `n = 5` to the bare
      probe, answering [[5]] and [[2]] [pk_type_reducing_fold_*, index_eq_type_reducing_
      fold_*]; Go today raises 22012 on all six (their GO lines). The reviewers'
      counterexample `id = 5 AND 1 / 0 = 1 AND 1 = 2` is NOT a fold of the target: a
      comparison of literals compares constant object values, which are not effective
      constants (below), so the target keeps `@c17 EQUALS @c9` in its filter and raises
      the division beside every access path [pk_annulling_fold_where and _explain,
      index_range_annulling_fold_*, index_eq_annulling_fold_*, or_annulling_fold_where];
      the port makes no such fold either, so Go raises 22012 there as it does today, and
      nothing is left to decide. Beside an IN list the same statement fails in the
      target with an internal VerifyException (SQLSTATE XXXXX, the harness's rendering of
      an exception that carries none) instead of the division
      [pk_in_annulling_fold_where]; Go raises the division's 22012, the target's own
      answer to the statement without the IN list, a declared divergence (section 8's
      list of rows that stay different) that is not ported. (v7's population could not show the rescue: every prototype
      row compared an unindexed column, so both members always had the same access
      path.) Nor is "Go does not prune in REWRITING" exact: the REWRITING
      OptimizeInputs routing prunes a group whose finals existed when it ran, and crosses
      one whose finals were promoted in its last round unpruned (unified_tasks.go:
      187-199), so which phase decided a fold was timing-dependent.
      v10: THE TARGET'S DECISION IS THE TARGET'S MECHANISM, WS-F's port of it, and
      WS-E adds no prune of its own. The target decides a fold because its REWRITING
      prunes every group to ONE member, the RewritingCostModel's winner, before PLANNING
      sees it (OptimizeInputs over fresh child copies, FinalizeExpressionsRule, and
      `Verify(finalMembers.size() == 1)`); WS-F ports exactly that (ws-f-design.md 2.3,
      D7 to D12, the physical REWRITING prune with the comparator `RewritingCostModelLess`,
      behind one option until its census is clean, then the default). v8 and v9 built a
      Go-only boundary prune in its place, first by structure and then by the
      simplification rule's PROVENANCE, and each version had a hole the physical prune
      does not have: a provenance field on a yield is lost when the yield deduplicates
      against a member already in the group (`Reference.Insert` returns false,
      reference.go:643-690; yields are deduplicated at commit, expression_rule_call.go:
      141-153), so from `[id = 5, X, X]` (X = `COALESCE(1 / 0, 5) IS NULL`)
      FilterDedupPredicatesRule's `[id = 5, X]` simplifies to the same `[FALSE]` filter
      as the original, forms no class, crosses into PLANNING, and its primary-key probe
      raises 22012 where the target answers no rows (the three v9 gates, SOURCE); the
      same with FilterDropTruePredicatesRule, FilterMergeRule and SelectMergeRule over
      the source. The physical prune has no classes: it keeps one member of the group,
      whichever rule yielded the others, so S0, its deduplicated variant and the fold
      are ranked together and the fold, with one conjunct, wins on the conjunct rung.
      It also answers the other v9 findings of this subsection without a Go-only rule:
      the SECOND crossing is WS-F's too, as a named item of its design (v10 said "WS-F's
      census covers the RFC-182 leg", which ws-f-design.md did not say):
      `AdvanceStagePreservingMembers` carries a group with NO finals into PLANNING with
      all its exploratory members (unified_tasks.go:98-108), the RFC-182 union leg's
      shape, and WS-F's D7 keeps such groups ("any child with none, no yield"), so
      without a WS-F change the cardinality rung could still rescue an unfolded member
      there. ws-f-design.md v4 (section 2.3, "The invariant covers EVERY crossing") makes
      a crossing with no final the same checked error as one with two, deletes
      `AdvanceStagePreservingMembers` when the prune becomes the default, and names WS-E's
      union-leg row [union_leg_type_annulling_fold_where] in its census as a query that
      must cross as the one fold final (v10 and v11 cited a v3 statement that did not
      exist). The dependency is explicit in section 8; no duplicate mechanism for WS-F to
      retire (v9 had none planned), and no rule placement WS-E must change:
      NormalizePredicatesRule is placed by WS-F D12's per-phase parity, which moves each
      Go REWRITING rule whose target counterpart is in PlanningRuleSet to PLANNING, and
      that moves PredicateToLogicalUnionRule WITH it (the target runs both in PLANNING,
      PlanningRuleSet.java:106-113, :107, :184-188; Go's PredicateToLogicalUnionRule
      assumes Normalize's CNF input, rule_predicate_to_logical_union.go:16-17, 106-110,
      which v9's move of Normalize alone would have broken). The ported
      QueryPredicateSimplificationRule therefore lands AFTER WS-F step 7's option is the
      default (section 8): the rule yields its fold into the group in REWRITING, as the
      target's does, and the physical prune decides, as the target's does. The
      RewritingCostModel's first three rungs (outer joins, SELECT boxes, table
      functions; Go omits the first by RFC-152's declared divergence) are equal for an
      original and its fold, which share their children, so the deciding rungs are the
      target's last three: normalized residual conjuncts (`countNormalizedConjuncts`,
      the CNF full size of the combined predicates, tautologies counting 0), predicates
      by level, and the semantic hash. WS-F's comparator is `RewritingCostModelLess` with
      the two changes its 2.3 names, and the second, the DENSE level producer, IS the
      level rung (v10 said neither touched these rungs); WS-E adds the tautology filter to
      the conjunct rung ((g)). For an original and its fold the two changes are
      harmless (their trees and heights are equal, (g)), but the physical prune ranks
      EVERY member of a group, not only such pairs (v10 said it ranks "the same members"
      as v8's class prune), so WS-F's census of lost plans is re-run with the tautology
      filter in the comparator before this step lands, and its result is recorded with
      the step. MEASURED in the target (round v9): `SELECT id FROM T WHERE id =
      5 AND COALESCE(1 / 0, 5) IS NULL UNION ALL SELECT id FROM T WHERE id = 1` answers
      [[1]], the annulled leg planned `... | FILTER false` [union_leg_type_annulling_fold_
      where, _explain], where Go today raises 22012. WHICH CONJUNCTS A PROBE RESCUES: only
      one whose access path proves few rows, a primary key or a UNIQUE index's equality
      (and an IN list over one); a non-unique index's equality or range proves no bound,
      so PLANNING already ranks the two members alike and the prune changes nothing there
      (v8's text named "an indexed conjunct" among the rescues; measured on the
      prototype, without the prune only the primary-key and primary-key IN rows raised
      22012, the `n > 0` and `n = 5` rows over the non-unique index passed). MEASURED in
      the target over a UNIQUE index: `n = 5 AND COALESCE(1 / 0, 5) IS NULL` answers no
      rows, `COVERING(T_UN <,> ...) | FILTER false` [unique_eq_type_annulling_fold_where,
      _explain], where Go today raises 22012. That a UNIQUE equality rescues the unfolded
      member in Go without a prune is SOURCE (`indexProvableMaxCard`,
      planning_cost_model.go:497-510; the prototype measured the primary key, the
      primary-key IN and the non-unique index only), and so is the union leg's reaching
      PLANNING with both members today (Go plans it as a union of finals, its GO line);
      under the physical prune neither question arises, and the implementation gate
      asserts both rows' target answers.
      THE MECHANISM, MEASURED on a prototype whose folds are NOT the target's, and whose
      prune is v8's Go-only one, not WS-F's (below, the prototype's "prune" is that one:
      on each row it ranks the same members with the same comparator, so its decision is
      the rung the physical prune decides by, and its suite and golden counts are v8's
      prototype's; the implementation re-measures both over WS-F's prune, section 8)
      (worktree `/home/birdy/projects/fdb-nofold-probe`, v7's stand-in rule, which runs
      Go's current simplifier and so folds `1 = 2`, a fold the target does not make, and
      cannot make the target's type folds above: Go types COALESCE nullable,
      `CommonValueType`, and its simplifier ignores NOT NULL types, so the six type-fold
      rows run through the prototype raise 22012 with the prune and without it,
      MEASURED, `/var/tmp/fdb-upgrade-recovery/wse8-typefold-{prune,noprune}.log`,
      scenario `zz_wse8_typefold`, "6/6 tests failed" on both; plus the prune in
      `boundary_predicate_prune.go`; its control,
      `PROBE_NO_BOUNDARY_PRUNE=1`, is v7's behaviour; plans in
      `/var/tmp/fdb-upgrade-recovery/wse8-prune-rungs.txt`, rows in
      `wse8-prune-rows-{prune,noprune}.log`, a yamsql scenario over T(id PRIMARY KEY, s,
      n) with an index on n and rows 1, 2 and 5). With the prune every one of the
      stand-in's annulments returns no rows, the answer the TYPE-fold rows above give in
      the target; these rows are the mechanism's measurement, not target answers: `id = 5 AND 1 / 0 = 1 AND 1 = 2`, `id IN (1, 2) AND
      1 / 0 = 1 AND 1 = 2`, `n > 0 AND ...` and `n = 5 AND ...` (the indexed column) and
      `(1 / 0 = 1 OR 1 = 2) AND 1 = 2`, each deciding `[FALSE]` (one conjunct) against
      the unfolded member (two or three) on the CONJUNCT rung; without it the primary-key
      and primary-key IN rows raise 22012 ("5 passed" against "2/5 tests failed", both
      with `22012: / by zero`). A fold that keeps the conjunct count is decided by the
      HASH rung, as in the target: `NOT (n > 3)` and its fold `n <= 3` (NOT over a
      comparison, a rule of the ported set) tie on conjuncts and levels (1 at level 1
      each), and so do `id = 5 AND NOT (n > 3)` against `id = 5 AND n <= 3` and `n = 5
      AND NOT (id > 3)` against `n = 5 AND id <= 3` (2 against 2), so the hash picks the
      member and with it the access path. MEASURED in the target, the hash lands on both
      sides: alone, `NOT (n > 3)` keeps the NOT and scans the whole index, `COVERING(T_N
      <,> ...) | FILTER NOT _.N GREATER_THAN ...` [index_tie_not_over_comparison_explain];
      beside `id = 5` the rewritten `_.N LESS_THAN_OR_EQUALS` is the residual of the
      primary-key probe [pk_beside_tie_fold_explain]; beside `n = 5` the rewritten `id <=
      3` wins and becomes a primary-key RANGE scan with `n = 5` as its residual, not the
      index equality [index_beside_tie_fold_explain]; and `n = 5 OR n = 5` keeps the OR
      over the whole index [index_tie_duplicate_or_explain]. Every one answers the same
      rows in Go today (their GO lines), whose plans differ (`PredicatesFilter(Scan(T))`
      for the lone NOT and the OR, and the index equality beside `NOT (id > 3)`); these are
      tie rows, pinned per engine with the deciding rung named, like the own-row tie of
      4.1(b). (The first v8 draft used `COALESCE(1, 1 / 0) = 2`, a fold of the
      prototype's stand-in simplifier that the target does not make
      [coalesce_int_literal_head_where].) Beside the index the tie matters: `col1 = 20 OR
      col1 = 20` against `col1 = 20` is a hash tie in both engines (the CNF full size of `a OR a` is 1, as the
      target's `getMetricsForMinor` multiplies 1 by 1), the target's hash keeps `col1 =
      20` and plans the index scan (standard-tests.yamsql:169-170), and Go's keeps the OR
      for the corpus schema and plans `PredicatesFilter(Scan(T1))`; the row is a tie row,
      its Go plan pinned with the hash named as the deciding rung (the same rows either
      way), like the own-row tie of 4.1(b); on this round's schema the target's hash
      kept the OR [index_tie_duplicate_or_explain], so the hash's pick is a property of
      the query and schema, not of the fold. v7's text said the port closes that row;
      that was an artefact of letting PLANNING decide, which picked the sargable member,
      and is withdrawn.
      THE WHOLE SUITE WITH THE PRUNE: `bazelisk test //... --test_tag_filters=-stress
      --keep_going --nocache_test_results` on the prototype
      (`/var/tmp/fdb-upgrade-recovery/wse8-prune-full.log`, per-target logs of the reds in
      `wse8-prune-full-logs/`) executed 94 of 94 targets and passed 89, the 1429 specs of
      `//conformance:conformance_test` and the oracle among them. The 5 red targets, each
      read from its log: (1) `cascades_test`, the two rule-registration censuses
      (TestRewritingRules_ContainsExpectedRules,
      TestRuleTypes_EveryProductionRuleHasDirectBehavioralTest), which the new rule's
      registration and its direct test update; (2) `explaindiff_test`, the plan-shape
      golden, and (3) `factory_test`, TestFactoryDeterminism's dedup-key headers, both
      regenerated with the plan changes below; (4) `embedded_test`, the two fold pins
      named in (a) and TestPlannerOptions_DisablePlannerRewriting, red because the
      prototype registered the filter arm under a second, Go-only name (the port is one
      rule under the target's name, above, so the test's literal list does not change);
      and (5) `sqldriver_test`,
      TestFDB_ExistsInnerShadow `case1_notexists_colliding_foldable`, the decline arm (a)
      describes. v7's run had two more: `recordlayer_test` (a WS-C defect since fixed)
      and `yamsql_test`'s `index_range_predicates_java` test[3], the tie row above, whose
      pin the prune now meets. None is a changed answer.
      THE PLAN CHANGES, COUNTED BY ENTRY (v7 quoted the golden test's LINE count, "16800
      line(s) differ", which counts shifted lines, not plans): `explain-differ diff` of
      the golden against the prototype's dump reports 13 of 2995 entries differing, 10 of
      them shape flips (`wse8-prune-vs-golden.diff`), and against the same prototype
      without the prune 2 entries (`wse8-prune-vs-noprune.diff`): `exists_with_aggregate.
      yaml#6`, whose UNION ALL leg's `[id = (SELECT MAX(id) ...), 1 = 0]` now annuls to
      `[FALSE]` on the conjunct rung (1 against 2), where PLANNING had kept the
      primary-key probe (a fold of the STAND-IN: the target does not fold `1 = 0`, a
      comparison of constant object values [pk_annulling_fold_explain], and the scenario
      is QUALIFY, Go-only; v8 said "as the target's annulment does", which was wrong), and `index_range_predicates_java.yaml#3`,
      the tie row. The other 11 are the stand-in's own folds, present without the prune:
      the constant BETWEEN, IS NOT DISTINCT FROM and `1 = 1` filters of `between.yaml#14`,
      `#15`, `between_java.yaml#11` to `#14`, `covering_index_pushdown.yaml#18`,
      `datetime_functions.yaml#6`, `distinct_from_java.yaml#8`, `#9`,
      `is_distinct_from.yaml#8` and `where_literal_on_left.yaml#6`. THE STAND-IN IS NOT
      THE PORT, and its plan changes are not the port's: it runs Go's current simplifier,
      the Go-only driver this section deletes, which folds more than the target (it folds
      `COALESCE(1, 1 / 0) = 2`, which the target keeps [coalesce_int_literal_head_where])
      and may fold less. So these 13 entries measure the MECHANISM (the prune decides
      exactly the variant pairs and nothing else moves), not the port's plan set; the
      port's own plan changes are regenerated and listed, entry by entry with each
      target answer, when its rules land (section 8), and its two tie rows above were
      read with the stand-in's folds only where the target's fold is the same (the
      annulment and the absorption of a duplicate disjunct are the target's rules).
      THE REJECTSNULL DUMP. The port of `rejectsNull` (foldPredicateAtNull over the
      ported set) decides outer-to-inner conversions, and v6 asked for a plan-corpus dump
      of it. It cannot be read off the stand-in, whose null folds are Go's; it lands with
      the port's value sets in the same step, as a corpus census of every outer join
      whose conversion the ported check flips, each with the target's answer for its
      shape, beside the plan-change list above (section 8's gate). That is where it
      belongs rather than a deferral: the dump measures the ported rules, so it cannot
      exist before them, and the step cannot merge without it.
   d. The COALESCE rule is Java's algorithm verbatim (leading NULL heads skipped, the
      first NOT NULL literal head returned, NULLs after a non-foldable argument dropped,
      all-NULL to a typed NullValue), in the PREDICATE value set only. A foldable head is
      a NullValue or a NOT NULL BOOLEAN literal after dereference, which in Go means a
      BOOLEAN written in the SQL or a bound BOOLEAN parameter alike: Go's SQL literals
      are literal Values, not constant objects, so the dereference step is the identity
      for them and the BOOLEAN condition applies to them exactly as to a bound value.
      An INT literal head is not foldable [coalesce_int_literal_head_where], and no
      composite head is.
   e. The NULL mapping is the target's: an inline NULL is a NullValue, and `CAST(NULL
      AS T)` becomes a typed NullValue when the walker builds it (Java's inject
      shortcut), so `coalesce_cast_null_head_*` behave as the NULL rows.
      CollapseNullStrictValueOverNullValueRule is ported for the six classes Java lists
      (Go's ArithmeticValue, CastValue, FieldValue, NotValue, PromoteValue and subscript
      value) into BOTH value sets, as the target has it (DefaultValueSimplification-
      RuleSet.java:50-54, DereferenceConstantObjectValueRuleSet.java:50-56).
   f. `effectiveConstant` is replaced by a port of BOTH `EffectiveConstant.from`
      overloads (the Value one with its three shapes, and the Object one for a
      comparison's literal comparand, which Go's `NewLiteralComparison` comparands
      need), and the three predicate-level folds are ported over it; a BooleanValue or
      ConstantValue outside its shapes is UNKNOWN to them.
   g. The REWRITING cost model is the target's on every rung that decides a fold, and
      the rows the target decides by `semanticHashCode` are pinned to Go's answer.
      Go's rule already yields its select as an alternative in the same reference and the
      prune ranks it with `RewritingCostModelLess` (planning_cost_model.go), the port of
      RewritingCostModel.java:58-115 but for its outer-join rung (omitted for RFC-152's
      materialized join, the reason stated at the function). Two rungs differ from the
      target today; the first decides folds and is ported, the second decides none and
      is measured and left to its own entry. The conjunct count: the target drops
      tautologies before counting, at every expression and over the child results, and
      counts 0 for a residual that is a tautology (NormalizedResidualPredicateProperty.
      java:81-90, 105-121); Go's `residualConjuncts` (designated_final.go:246-262) sums
      `normalFormSize` over every predicate, and a ConstantPredicate takes the default
      arm and counts 1 (normal_form.go:196-217), so `[true]` against the original is 1
      against 1 and a hash decides what the target decides on the count. Go filters
      `predicates.IsTautology` exactly where the target does, which moves
      `null_strict_div0_cast_null_is_null_where`, the COALESCE rows with an erroring
      tail and `coalesce_not_cast_null_head_where` from a Go hash tie to the target's
      count decision. The predicate level map:
      the target's producer is DENSE, an entry for every level of the tree, 0 where no
      predicate sits (PredicateCountByLevelProperty.java:196-216), so its final tiebreak
      compares tree heights; Go's `predCountByLevel` is sparse and compares the highest
      PREDICATE levels, a difference its own comment records
      (planning_cost_model.go:121-135, designated_final.go:278-287). A dense producer
      would give the target's verdict through `comparePredicateCountByLevel`'s single
      pass over the union of levels (on dense maps, a level past the shorter tree
      compares 0 against the longer tree's count, which has the sign of the target's
      height comparison, and equal counts reach the height comparison itself). WHAT EACH CHANGES outside this workstream was measured on a detached
      worktree of the tree `34c824e272259d45f538d459dbddfc8b1170bc42`, the full suite
      uncached (`bazelisk test //... --test_tag_filters=-stress,-conformance_java
      --keep_going --nocache_test_results`) and the plan-shape corpus dumped
      (`go run ./cmd/explain-differ dump`, 2823 queries and 172 DML statements) and
      compared with `testdata/plan_shape.golden`:
      - Both together (`/var/tmp/fdb-upgrade-recovery/wse7-runB-bazel.log`): 89 of 92
        targets pass; six corpus plans change (cte_published_row_names.yaml#51,
        derived_star_visibility.yaml#4, limit_offset_bounds.yaml#3,
        quoted_identifier_labels.yaml#8, repeated_output_names.yaml#3 and #4), and
        every one of them keeps a TALLER survivor: a redundant Project over a Project,
        or `Limit(1, offset=1, Limit(3, offset=1, ...))` for `Limit(1, offset=2, ...)`;
        the simfdb hunt golden `repnames` changes with the same redundant Project; and
        four cascades unit tests fail, each asserting that a shallower plain scan beats
        a Sort over it at a full tie (rewriting_final_invariant_test.go:96, 151, 290;
        planning_cost_model_rungs_test.go:988).
      - The tautology filter alone (the four added lines of
        `/var/tmp/fdb-upgrade-recovery/wse7-runB-taut.go` against the tree): the corpus
        dump is identical to the golden (0 differing lines), and the full suite passes,
        92 of 92 targets executed uncached
        (`/var/tmp/fdb-upgrade-recovery/wse7-runTaut-bazel.log`).
      So every flip is the dense map's. Its mechanism is the target's own tiebreak:
      at a full tie on per-level counts the target keeps the TALLER tree (its compare
      is `compare(b, a)` and ends on `Integer.compare(b.highest, a.highest)`,
      PredicateCountByLevelProperty.java:183-194), and in the target a redundant
      projection does not reach that rung, because a SQL projection there IS a
      SelectExpression and the select-count rung, earlier in the model, keeps the
      member with fewer; Go's projections are LogicalProjectionExpressions, which Go's
      select count (`isSelectExpression`, planning_cost_model.go:160-163) does not
      count, and LIMIT is the approved Go extension with no target expression at all.
      WS-E takes the tautology filter; the dense map is WS-F's (its 2.3 makes the
      producer dense for the physical prune, which lands before this step), and this
      measurement is WS-F's input: a dense map WITHOUT Go's select-count rung counting
      what the target's counts flips the six plans above to taller survivors, so WS-F's
      dense-map change carries the TODO.md entry "Finding 6-followup — dense
      predicate-count producer for Java tiebreak parity" (RFC-189 E2) with it, and
      ws-f-design.md v3 says so (v10 had WS-E decline the dense map and named a comment
      at designated_final.go:278-287, a file WS-F deletes). No WS-E row reaches the level
      rung's height tiebreak: a predicate simplification's two members differ only in
      their predicates, so their trees and heights are identical and the per-level
      counts are compared on the same levels in both producers.
      The hash rung stays Go's: Go has no port of `semanticHashCode`, and matching it
      bit for bit would reproduce an accident of naming (the target's answer for the
      same statement changes with the schema [trace_null_strict_div0_cast_null_eq_one,
      trace_v4_schema_div0_cast_null_eq_one]). Go's tiebreak is `deepHash`
      (designated_final.go:294-334), which folds `HashCodeWithoutChildren`, and a
      SelectExpression's hashes its result value and predicates
      (expressions/select.go:257-272), so Go's answer ALSO depends on the schema and
      the statement. Under the physical prune every tie between a fold and its original is
      decided HERE, by REWRITING's hash rung, since one member of the group crosses into
      PLANNING and PLANNING's `costExprHash` (planning_cost_model.go:412-419) never sees
      the pair (v10 still said PLANNING decided the one-source ties, from the v8
      prototype, which let both members cross). The tie rows are therefore pinned, not
      accepted as a set, and split by the PHASE that decides them (v12; v11 predicted one
      outcome for all of them, which cannot hold for the join-nesting tie, decided in
      PLANNING, 4.1 and TODO.md:4571):
      - REWRITING's fold ties [null_strict_div0_cast_null_eq_one_where, v4_schema_div0_
        cast_null_eq_one_where, v5_schema_div0_cast_null_eq_one_where_first,
        first_planning_a_exec, first_planning_b_exec, null_strict_cast_null_beside_div0_
        where, null_strict_cast_null_beside_div0_where_empty_table,
        coalesce_div0_five_is_null_where]: their pair never reaches PLANNING.
        - Inverting REWRITING's `deepHash` compare is predicted to redden each row whose
          two members answer differently. Each row's prediction is written beside it in
          the test table (the rows where both members give the same answer stay green, and
          are named).
        - Inverting PLANNING's `costExprHash` is predicted to redden none of them.
      - The join-nesting tie [in_cast_null_join_inner_empty] is decided in PLANNING by
        `costExprHash`. Inverting `costExprHash` is predicted to redden it, and inverting
        `deepHash` to leave it green.
      For each row, planned over the two-table schema of the v5 round AND the seven-table
      schema of the v4 round, a Go test pins the answer (rows or error) and the EXPLAIN
      that Go gives, cold, 20 plans each, and names the rung that decided it. Both
      mutation runs are recorded with the implementation, and each row's actual red or
      green is compared with its written prediction; a mismatch is a finding about the
      phase that decides it, not a pin to update. A change to either hash that flips a row
      is thereby a red test, and each flip is a reviewed pin change, never a silent
      error-to-rows swap. DIVERGENCES.md records the rows with their traces under the
      TODO.md entry "An identifier-sensitive cost tie decides join nesting (RFC-235
      §17)".
   h. The COALESCE-under-AND/NOT VerifyException and the CASE-branch INCOMPATIBLE_TYPE
      (above) are target defects, internal failures on well-typed predicates, and Go
      does not reproduce them. The CASE row answers no rows in Go (the CASE is
      nullable, its IS NULL is evaluated per row, and every row takes the literal
      branch), as its GO line already does. The two COALESCE rows are 0AF00 in Go today,
      a planning gap of Go's own, which the port closes: each plans, and what it
      answers depends on which member the prune keeps, because the simplified member
      has folded the COALESCE (to `true` under the AND, to `false` under the NOT) and
      the unfolded one evaluates it and raises the division. So each row's Go answer and
      EXPLAIN are pinned exactly as (g) pins the tie rows, the test naming the rung that
      decided it: for the AND row the identity rule leaves one conjunct against two, a
      count decision, and the expected answer is the rows with `n > 0`; for the NOT row
      the rung depends on the predicate the walker builds for NOT over a boolean value,
      which the implementation reads off the plan and states in the pin.
      DIVERGENCES.md records each defect with its probes and frame.
   i. A PatternForLikeValue is never evaluated by any set (section 1.5).
   j. Tests: every row above as a Go assertion, with EXPLAIN assertions for the plan
      rows; a simplifier unit test per head class and per set; the `EffectiveConstant`
      port per recognised and unrecognised shape, both overloads (a BOOLEAN literal of
      each value, a NullValue, a NOT NULL COALESCE, a nullable NOT over a literal, a
      nil, Boolean and non-Boolean comparand); a fixpoint test that runs each set
      repeatedly over a COALESCE with a `NOT FALSE` head and asserts it never folds; a
      test per deterministic row asserting the surviving member, the conjunction rows
      among them, each with the REWRITING rung that decided it (under the physical prune
      PLANNING decides no fold); the rule over a filter yielding a
      filter and over a select yielding a select; the tie pins of
      (g); `rejectsNull` over every null-rejecting and null-accepting shape its tests
      cover today, re-derived from the ported set; and the census tests of (b). The
      folds the physical prune decides (c): yamsql scenarios over round v8's schema (T
      with T_N on n) and round v9's (T with the UNIQUE T_UN) carrying their rows, each answer asserted
      as the target's (the type annulments no rows, the type reductions the probe's row,
      the union leg [[1]], the literal comparisons 22012, the IN-list row 22012 as
      declared, the tie rows' rows) and each EXPLAIN asserted with `plan_contains` on the
      surviving member (the annulled `[FALSE]` filter, the bare probe of a reduction),
      not only on its rows; the deduplicated-variant rows `id = 5 AND X AND X` and `id =
      5 AND TRUE AND X` (X = `COALESCE(1 / 0, 5) IS NULL`) answering no rows, with the
      SURVIVOR of their group asserted (the fold; v10 asserted the member set, which
      `Verify(==1)` makes trivially one); the bare `WHERE X AND X` row, a tie between the
      deduplicated `[X]` and the fold `[FALSE]` (one conjunct each), pinned with its
      deciding rung. The target collapses duplicates when it builds the select
      (SelectExpression.java:708-719, SOURCE), so its query is `WHERE X`, whose MEASURED
      answer is no rows [coalesce_div0_five_is_null_where]. Go's answer is decided by the
      same tie as that row's, so the row is a tie row like it: its Go answer and EXPLAIN
      are pinned per schema with the deciding rung, and it joins section 8's tie list
      (v11 said "what Go must give", which contradicts the tie). And the NEGATIVE CONTROL, built so that it outlives WS-F's
      prune-off path, which WS-F deletes before WS-E lands (v10 ran it "with the physical
      prune off"): a unit test ranks the original and the fold of the primary-key,
      primary-key IN, UNIQUE-index and union-leg groups directly, once with the REWRITING
      comparator, which must keep the fold, and once with PLANNING's
      (`PlanningCostModelLess`) over their implemented plans, which must prefer the
      unfolded probe on the cardinality rung, so the pair shows both that the prune
      decides and that without it PLANNING would decide the other way (the non-unique
      `n = 5` and `n > 0` rows are NOT in it: measured, PLANNING's choice there is the
      fold's too).
5. `CommonValueType` keeps serving only its other callers (IF, CASE and IFNULL), whose
   typing does not change here; MOD leaves it for item 6.
6. Arithmetic lanes. The target types `+ - * / %` by its operator map: a physical
   operator per (logical operator, left type code, right type code), ArithmeticValue.
   java:406-498, looked up at construction, and no entry is XX000 VerifyException
   "unable to encapsulate arithmetic operation due to type mismatch(es)" (:226-229).
   The map has no NULL, BOOLEAN or BYTES lane anywhere, and STRING lanes for ADD only
   (ADD_IS, _LS, _FS, _DS, _SI, _SL, _SF, _SD, _SS: concatenation, result STRING).
   MEASURED: `'a' + 1` is STRING 'a1', `s + s` STRING [add_string_int_select,
   add_string_string_select], and the non-integer operands render as Java's
   `String.valueOf` renders them: `'a' + 1.5` and `1.5 + 'a'` are `a1.5` and `1.5a`,
   `'a' + 1.5f` is `a1.5`, `'a' + 10000000000.0` is `a1.0E10`, `'a' + 0.0001` is
   `a1.0E-4` and `'a' + 3000000000` is `a3000000000`, all STRING
   [add_string_double_select, add_double_string_select, add_string_float_select,
   add_string_large_double_select, add_string_small_double_select,
   add_string_long_select]; Go's STRING lanes render a DOUBLE with
   `values.JavaDoubleToString` and a FLOAT with `values.JavaFloatToString`
   (values.go:3942-3945), whose shortest-digit and exponent rules are the JDK's, and a
   test pins a FLOAT whose `Float.toString` and `Double.toString` differ (`0.1f` is
   `a0.1`, not the widened double's digits). Go today gets the values right and types
   them DOUBLE (their GO lines). `'a' - 1`, `1 + TRUE`, an inline `NULL + 1` (SELECT and
   WHERE), `(1 / 0) + NULL` and a bound NULL `? + 1` are XX000
   [sub_string_int_select, add_int_boolean_select, inline_null_plus_one_select,
   inline_null_plus_one_where, null_strict_null_beside_div0_select,
   arith_untyped_null_param]; a typed NULL has its type's lane
   [cast_null_plus_one_select]. Go today promotes through `ArithmeticValue.Type()`
   (values.go:3642-3659), types `'a' + 1` INTEGER, answers NULL for the NULL forms and
   refuses `'a' - 1` and `1 + TRUE` at run time with 22000 (their GO lines). Design:
   Go's ArithmeticValue ops OpAdd, OpSub, OpMul, OpDiv and OpMod resolve their physical
   lane ONCE, at construction, from Java's map ported verbatim as a table; the lane
   fixes the result type and the arithmetic (`addExact`-style INT and LONG overflow,
   Java's float and double semantics, concatenation for the STRING lanes); a pair with
   no lane is XX000 with the target's message, which Go can share verbatim. The `MOD`
   function spelling becomes the same OpMod ArithmeticValue (Java's `mod` is
   ArithmeticValue's MOD too), so it leaves the scalar-function catalog and
   CommonValueType. This is the ONE lane table: RFC-257 WS-J F4 adds the bit and bitmap
   operators' rows to it (ws-j-design.md section 6 already names this sharing), and no
   second resolution path exists. The approved Go extension types (DATE, TIMESTAMP)
   keep the arithmetic Go gives them today as explicit extension rows of the table,
   each with its tests, and there is no generic promotion fallback. Explain, plan hash
   and cache identity carry the lane as ArithmeticValue already carries its op.
   Declared behaviour changes (CHANGELOG): `'a' + 1` is STRING; NULL-typed operands,
   BOOLEAN operands and non-ADD STRING operands are refused at planning with XX000
   instead of answering NULL or failing per row. Tests: one per lane class and per
   refused class, cold and warm, and every row above as a Go assertion.
GREATEST and LEAST over DOUBLE and FLOAT carry a target defect. GREATEST_DOUBLE and
GREATEST_FLOAT start their fold from Double.MIN_VALUE and Float.MIN_VALUE, the smallest
POSITIVE values, and compare with a strict `>`; LEAST_DOUBLE and LEAST_FLOAT start from
MAX_VALUE with a strict `<` (VariadicFunctionValue.java:417-432, 466-481). So every
GREATEST whose arguments are all at most zero returns MIN_VALUE [greatest_negative_doubles,
greatest_negative_floats, greatest_zero_doubles, greatest_zero_floats]; a NaN argument
never wins a strict comparison and is skipped, so `GREATEST(NaN, -1.0)` is MIN_VALUE
[greatest_nan_double] and `LEAST(NaN, 1.0)` is 1 [least_nan_double]; and `LEAST(+Inf,
+Inf)` is MAX_VALUE [least_infinite_doubles]. LEAST over finite values and the INT and
LONG lanes are right [least_negative_doubles, greatest_negative_longs,
greatest_negative_long_literals, least_long_literals, greatest_negative_long_columns].
Go folds GREATEST and LEAST from the first argument with the engine's total order for
numbers, `CompareOrdered` (values/compare_ordered.go:17-18: java.lang.Double.compare's
order, NaN above every other value, -0.0 below 0.0), the order its sorts already use,
instead of today's `compareScalar`, under which a NaN compares equal to everything and
`LEAST(NaN, 1.0)` answers NaN. Then `least_nan_double` equals the target, and the rows
that stay different are exactly the defect's: the four GREATEST rows at or below zero,
`greatest_nan_double` (Go: NaN) and `least_infinite_doubles` (Go: Infinity).
DIVERGENCES.md records the defect with those six probes (upstream report unpublished).
The operator map cited above is COALESCE's; GREATEST and LEAST have no RECORD or ARRAY
entries, so they reject both with 22F00, and Go's already-failing `greatest_bytes`
(22F00) is the same rule.
The arithmetic error SQLSTATEs differ: the target leaves ArithmeticException unmapped,
so division by zero and INT overflow render XXXXX [coalesce_evaluates_every_argument,
prepared_int_overflow, coalesce_eager_div0_literal], and a decimal literal beyond the
double range is XXXXX NumberFormatException [least_overflowing_literal], while Go raises
22012, 22003 and 22003. Go's codes are the SQL-standard ones and Java's XXXXX is an
unmapped internal error, so Go keeps them; this pre-existing difference is recorded in
DIVERGENCES.md by this workstream with the probes as evidence. The Go-only IF/IIF
short-circuit contracts do not change. Tests: every variadic oracle row as a Go
assertion including result nullability through the driver's column metadata (the Go
runner now reports it), the BYTES/UUID/ENUM rejections and the DATE/TIMESTAMP
extension, and a simplifier test per arm that the rule refuses to fold.

## 6. Statement options and snapshot reads (build-docs W6, relational W10, cascades W11, #4362/#4364/#4603)

Target grammar: `statementOptions` (RelationalParser.g4:591-601: NOCACHE, LOG QUERY,
DRY RUN, PLAN RIGHT DEEP, ISOLATION LEVEL SNAPSHOT) attaches only to
`selectStatement : query statementOptions?` (:401), INSERT/UPDATE/DELETE (:385,
:392, :462), EXECUTE CONTINUATION (:690) and DESCRIBE's `query statementOptions?`
(:744); INSERT...SELECT uses `queryExpressionBody` (:441-442), so an INSERT's options
sit only at its end. Statement EF_SEARCH is gone; the window-clause
`EF_SEARCH '=' n` (:1177) remains. AstNormalizer.visitStatementOption (:291-312)
maps the options. PlanGenerator.validateIsolationLevelSnapshotOption (:507-517)
admits SNAPSHOT only for SELECT and EXECUTE CONTINUATION (0A000 "OPTIONS (ISOLATION
LEVEL SNAPSHOT) is only supported on SELECT queries"), and a continuation carrying a
COPY plan is rejected (:295-305, 0A000 "... only supported when continuing a SELECT
query"). QueryPlan (:500-507) sets the execution's ExecuteProperties isolation to
SNAPSHOT; it is an execution choice, never persisted in a continuation, so a resume
must pass it again. Connection isolation stays SERIALIZABLE. The DML plans refuse
snapshot execution before touching their child, dry runs included (SOURCE:
QueryPlanUtils.java:43, RecordQueryAbstractDataModificationPlan.java:192,
RecordQueryDeletePlan.java:96; statement admission refuses every such statement first,
so SQL cannot reach the check). Measured: SNAPSHOT on a SELECT runs
[options_snapshot_select]; options after ORDER BY [options_after_order_by] and LOG
QUERY [options_log_query] run; options inside a subquery [options_inside_subquery]
and statement EF_SEARCH [options_ef_search_statement] are 42601.

Go today: `queryOptions` hangs off `#simpleTable` (RelationalParser.g4:531,587-596)
with statement EF_SEARCH and no SNAPSHOT; `dmlHasDryRunOption`
(cascades_generator.go:779-810) walks the whole DML subtree because the old grammar
attaches INSERT...SELECT's options to the inner SELECT.

MEASURED (v2 rounds): EXPLAIN with SNAPSHOT is admitted and explains
[explain_snapshot] (Go today: 42601, its grammar has no SNAPSHOT); the option set on
the CONNECTION (Options.Name.ISOLATION_LEVEL_SNAPSHOT, "Scope: Connection, Query",
Options.java:224-232, merged with the statement's options at PlanGenerator.java:170
before validation) admits a SELECT and an EXPLAIN [snapshot_connection_select,
snapshot_connection_explain] and rejects an INSERT with the same 0A000
[snapshot_connection_insert]. Go has no such connection option (`api/options.go`
copies the older option list; its OptDryRun and OptLogQuery are read nowhere outside
options.go).

Design:
1. Grammar sync with the target's statementOptions placement and alternatives;
   `#simpleTable` loses its options; statement EF_SEARCH is removed and the window
   EF_SEARCH kept. `dmlHasDryRunOption`'s tree walk is replaced by reading the
   statement's own options node, which the new grammar makes the only position.
2. Options merge exactly as the target merges them, with no scope filter: the
   "Scope:" lines of Options.java:179-232 are Javadoc the target does not enforce
   (the only checks are type checks, :592-593), and PlanGenerator.java:170 merges the
   connection's options into the statement's with `withChild` before anything reads
   them. MEASURED: DRY_RUN set on the CONNECTION makes an INSERT, an UPDATE and a
   DELETE write nothing while each reports the count it WOULD have affected, COUNT 1
   [dry_run_connection_insert, dry_run_connection_update, dry_run_connection_delete],
   and leaves a SELECT alone [dry_run_connection_select] (the target's own
   OptionScopeTest.optionTakenFromConnection, :63-75, pins the INSERT). DDL IGNORES it:
   CREATE SCHEMA TEMPLATE on a DRY_RUN connection creates the template (the follow-up
   DESCRIBE finds it) and reports COUNT 0 [dry_run_connection_create_template],
   because DRY_RUN's only reader is QueryPlan.applyOptions (QueryPlan.java:434,
   500-503), which DDL never reaches. `api.OptIsolationLevelSnapshot` is added, and
   Go's OptDryRun, today read nowhere, is read from the merged set by the DML
   executor only: a Go INSERT, UPDATE or DELETE on a DRY_RUN connection stores nothing
   and its RowsAffected is the count it would have affected, as the target's update
   count; Go DDL ignores DRY_RUN, as the target's does.
   Setting a connection option. Go has no working way today:
   `EmbeddedConnection.SetOptions` REPLACES the whole set (connection.go:157-158), which
   would also drop the DSN's `restrict_ddl_to_session_database` security option;
   `api.Connection.SetOption` is declared (api/connection.go:56) and unimplemented; the
   DSN reads an allowlist and silently ignores anything else (sqldriver/dsn.go:93-122).
   Design: `SetOption(name, value)` is implemented on EmbeddedConnection and MERGES one
   option into the connection's set, type-checked as the target's `Options` is
   (Options.java:592-593); `SetOptions` is kept for its existing callers and documented
   as replacing; the DSN gains `dry_run` and `isolation_level_snapshot` boolean
   parameters, parsed like its other booleans, and an UNKNOWN DSN parameter becomes an
   error naming it instead of being ignored (declared in CHANGELOG), so a misspelled
   option cannot silently leave a connection writable. The error lists every parameter
   the driver accepts, in sorted order: `cluster_file`, `dry_run`,
   `isolation_level_snapshot`, `planner_statistics`, `restrict_ddl_to_session_database`,
   `schema` and `transaction_tags`. `schema` and `cluster_file` sit in the same query
   map as the options (dsn.go:205-226; driver.go:238 reads `cluster_file` from it), so
   they are accepted names like the rest and never reported as unknown. The check runs
   where the connector turns the DSN into options (`ConnectionOptions`, dsn.go:99),
   not in `ParseDSN`, which stays a parser (its tests that carry arbitrary keys,
   driver_test.go:46-162, are unaffected); a connector test pins the error's text, and
   one opening test per accepted name pins that it is not refused. database/sql callers reach
   `SetOption` through `Conn.Raw`. Pool safety: a connection option set through `Raw`
   must not outlive the borrow, or the next borrower's DML silently writes nothing,
   which is the data-loss hazard the current code guards against by keeping DRY RUN
   statement-scoped (cascades_generator.go:910-921, 1293-1294, 1964-1966;
   dml_dry_run_fdb_test.go:7-12). So `ResetSession` (connection.go:828-853) restores
   the option set the connector created the connection with (the DSN's), and those
   comments are rewritten to say the connection scope exists and is reset on return.
   FDB tests: with `SetMaxOpenConns(1)`, set DRY_RUN through `Raw`, return the
   connection, then `db.Exec` an INSERT on the same physical connection and read the row
   back stored (the no-sticky sentinel of dml_dry_run_fdb_test.go retargeted at pool
   return); DRY_RUN from the DSN persists across borrows (it is the connector's set);
   `SetOption` leaves `restrict_ddl_to_session_database` in force; a DRY_RUN DELETE and
   a DRY_RUN CREATE SCHEMA TEMPLATE with their RowsAffected and stored state. LOG_QUERY logs the
   statement through the connection's logger on either scope; PLAN RIGHT DEEP, from
   either scope, sets `ShouldJoinRightDeep` for that statement's planning and enters
   the cache key's planner-configuration component (section 3), which keeps Go's
   existing connection-level read (planner_options.go:106). The merged options are
   captured once per statement EXECUTION, before planning; `executeProps`
   (cascades_generator.go:1970) reads that captured set for every page instead of
   re-reading the connection, so a connection option changed between two pages of
   one result does not change the rest of it. An FDB test runs a DRY_RUN connection
   INSERT and reads back nothing stored, with RowsAffected 1.
3. `ISOLATION LEVEL SNAPSHOT` is an execution option. It is validated after the
   merge and before cache lookup, and the target admits only a SELECT and EXECUTE
   CONTINUATION (PlanGenerator.java:170-171, 507-517): MEASURED, on a snapshot
   CONNECTION an UPDATE, a DELETE, an EXPLAIN INSERT (the explained statement is
   classified), CREATE DATABASE, DROP DATABASE, SHOW DATABASES and SHOW SCHEMA
   TEMPLATES are all 0A000 "OPTIONS (ISOLATION LEVEL SNAPSHOT) is only supported on
   SELECT queries" [snapshot_connection_update, snapshot_connection_delete,
   snapshot_connection_explain_insert, snapshot_connection_create_database,
   snapshot_connection_drop_database, snapshot_connection_show_databases,
   snapshot_connection_show_templates], and a SELECT and an EXPLAIN of a SELECT run
   [snapshot_connection_select, snapshot_connection_explain]. The classification is
   by GRAMMAR RULE, not by keyword: `fullDescribeStatement` (EXPLAIN, DESCRIBE or DESC
   over a query, a DML statement or EXECUTE CONTINUATION, RelationalParser.g4:726-747)
   is unwrapped and its inner statement classified (ParseTreeInfoImpl.java:133-137,
   AstNormalizer.java:271-274), while `simpleDescribeStatement` (DESCRIBE SCHEMA,
   DESCRIBE SCHEMA TEMPLATE) and `helpStatement` are utility statements
   (AstNormalizer.java:345-347). MEASURED: DESCRIBE and DESC of a SELECT run on a
   snapshot connection and explain [snapshot_connection_describe_select,
   snapshot_connection_desc_select, and describe_select without the option], while
   DESCRIBE SCHEMA, DESCRIBE SCHEMA TEMPLATE and HELP are 0A000
   [snapshot_connection_describe_schema, snapshot_connection_describe_template,
   snapshot_connection_help]. Go classifies the same way, on the same grammar rules,
   which its grammar has (grammar/RelationalParser.g4:710-738), before a statement
   touches the catalog or a store: on a merged snapshot option a query, and an
   EXPLAIN/DESCRIBE/DESC of a query, pass; DML, DDL, SHOW, `simpleDescribeStatement`,
   HELP and admin statements are 0A000 with the target's message, whichever scope set
   it. Go's START TRANSACTION, COMMIT and ROLLBACK statements are Go-only SQL
   (connection.go:916-950) standing for the JDBC calls the target makes outside SQL,
   which its classification never sees, so they stay ADMITTED on a snapshot
   connection (SOURCE: no target statement to measure; refusing ROLLBACK would leave a
   snapshot connection unable to abandon its transaction). An FDB test runs CREATE
   DATABASE and CREATE SCHEMA TEMPLATE on a snapshot connection and asserts no catalog
   row was written, and one statement per class above asserts its admission. It never
   enters a cached plan or the cache key: a hit re-reads it from the merged options.
   Go has no EXECUTE CONTINUATION (the planner rejects caller continuations,
   cascades_generator.go:1344-1354) and no COPY, so the target's continuation arms
   (admission on EXECUTE CONTINUATION, the COPY-continuation 0A000, "pass it again on
   resume") have no Go route. Their owner is TODO.md CQ-78 / RFC-203 (compiled-
   statement continuations), not WS-G, whose section is ARRAY_AGG state; this
   workstream books the snapshot contract there (the option is never persisted in a
   continuation, it must be supplied again on resume, a COPY continuation is
   rejected), with a pointer back here, as an appended TODO.md block.
4. Read scope is the target's, which is narrower than "every read". MEASURED in
   explicit transactions (Java step `snapshotReadScopeProbe`: connection A reads,
   connection B commits a write, A writes and commits): a SERIALIZABLE read conflicts
   with a concurrent insert into its range over a covering scan, an index scan with
   fetches and a record scan alike [read_scope_covering_serializable_insert,
   read_scope_fetch_serializable_insert, read_scope_scan_serializable_insert], all
   40001, and so do a concurrent update of the INDEXED column under a covering scan and
   a concurrent update of a scanned record under a record scan
   [read_scope_covering_serializable_update_indexed, read_scope_scan_serializable_update];
   under the SNAPSHOT option a concurrent insert into the scanned range, and a
   concurrent update of a scanned record, commit cleanly over a covering scan and a
   record scan [read_scope_covering_snapshot_insert,
   read_scope_covering_snapshot_update_indexed, read_scope_scan_snapshot_insert,
   read_scope_scan_snapshot_update] and over the index range of a fetching scan
   [read_scope_fetch_snapshot_insert]. (v4's read_scope_covering_snapshot_update updated
   the unindexed `w`, which leaves the index entry unchanged and so proves nothing; it
   stays pinned, and the `_indexed` pair above, which updates `v`, is the evidence.)
   A concurrent
   update of a record the index scan FETCHED still conflicts under SNAPSHOT
   [read_scope_fetch_snapshot_update_fetched, and its serializable control]: the
   fetch after an index scan stays serializable. The plans are the ones named
   [read_scope_covering_explain: `COVERING(S_V ...)`, read_scope_fetch_explain:
   `ISCAN(S_V ...)`, read_scope_scan_explain: `SCAN([IS P])`]. SOURCE for the kinds
   the probe does not cover: the option sets the execution's ExecuteProperties
   isolation (QueryPlan.java:500-507), which record and index range scans
   (KeyValueCursorBase.java:358), rank reads (RankIndexMaintainer.java:318), text scans
   (TextIndexMaintainer.java:539), vector scans (VectorIndexMaintainer.java:225) and
   version loads (FDBRecordStore.java:1458-1460) follow; FDBRecordStore.java:1256 is
   recordExistsAsync, which takes the isolation it is passed. Go's census has three kinds of site, and they are listed
   separately because they are tested differently.
   (i) Reads that FOLLOW the execution's isolation, each taking it from its
   ExecuteProperties at the call named, each reachable from SQL and each given an FDB
   snapshot test with a serializable control: value and primary scans
   (key_value_cursor.go, index_scan.go, record_key_cursor.go); rank scans
   (rank_index_maintainer.go, ranked_set.go); vector (hnsw.go,
   spfresh_index_maintainer.go and the spfresh_* read helpers it calls); bitmap
   (bitmap_value_index_maintainer.go); aggregate and count index scans
   (aggregate_function.go, count_index_maintainer.go); permuted min/max
   (permuted_min_max_index_maintainer.go); and the legacy-format record version load
   (key_value_cursor.go:489-490, `LoadRecordVersion(pk, c.isSnapshot())`,
   store_version.go:130-157), tested on a store below the version-inline format.
   (ii) Reads that follow the isolation but that NO SQL statement reaches: the TEXT
   index scan (text_index_maintainer.go:398-406 picks its read transaction by the
   isolation, as Java's does) and the time-window leaderboard scans
   (time_window_leaderboard_maintainer.go). Go's SQL DDL has no syntax that creates
   either index type and no SQL predicate plans either scan (checked by a grep of the
   DDL walker and of the planner's index-type dispatch, recorded with the tests);
   their existing record-layer tests cover the isolation arm, and a unit test pins
   that each takes the snapshot transaction when the isolation says so.
   (iii) Reads that are ALWAYS snapshot, whatever the option, as in the target, and
   which therefore get no serializable control: the record count
   (record_count.go:170-172, Java's getSnapshotRecordCount); rank `InitNeeded` and
   `PreloadForLookup` (rank_index_maintainer.go:248, 361, 372; aggregate_function.go:
   565, 576); and the index-state load at store open, a snapshot range read over the
   index-state subspace (`loadRecordStoreState`, store_state_cache.go:263-275; v7 cited
   index_state.go:688-696, which is `LoadIndexStates`, the planner's own read, snapshot
   too), as the target loads its store state at SNAPSHOT (FDBRecordStore.java:450-455;
   its conflicts are the per-index keys below).
   (iv) Reads that are ALWAYS serializable, whatever the option, as in the target: the
   store HEADER read at store open, which every SQL statement's store open makes. The
   target reads the header with SERIALIZABLE isolation while it loads the index states
   at SNAPSHOT (SOURCE: the open path, FDBRecordStore.java:2622-2631, loads the state
   through `loadRecordStoreStateAsync(existenceCheck)` directly when the store lock is
   bypassed and through the store-state cache otherwise, whose default pass-through
   cache calls the same method; that method is `loadRecordStoreStateAsync(
   existenceCheck, SERIALIZABLE, SNAPSHOT)`, :4116-4117; fdb-relational-core configures
   no other cache); Go's open reads the store info key with a serializable point Get
   in the same function (`loadRecordStoreState`, store_state_cache.go:263-275; the
   target's `getRange(subspace.range(), 1)` conflicts from the subspace's start through
   the info key, and no record-layer key sorts before the info key's 0, so the two
   ranges cover the same written keys), and an open served from the store-state cache
   adds a read conflict on the info key (`handleCachedState`, :71-80), as the target's
   FDBRecordStoreStateCacheEntry.handleCachedState does. v7 cited `checkStoreExists`
   (store_builder.go:1050), which serves only `ReloadRecordStoreState` (store.go:1913)
   and the builder's existence check (store_builder.go:1379). An FDB test with the
   snapshot option and a concurrent write of the store header (`SetHeaderUserField`,
   store.go:1789, committed between the read and the reader's own write) pins the
   conflict under both isolations, on an uncached open and on an open through a
   store-state cache hit.
   The record FETCH after an index scan stays serializable (measured above;
   FDBRecordStoreBase.java:1413-1417, loadRecordInternal(pk, state, false)); Go's fetches (`LoadRecord`,
   executor.go:1417, 2143; executor_new_plans.go:921, a load-by-keys fetch) already
   match and stay so.
   Index-state read conflicts are the target's, MEASURED with Java step
   `indexStateReadScopeProbe` (connection A reads in an explicit transaction; a bare
   FDB transaction then writes the index's raw STATE KEY, the DISABLED value at the
   store's index-state subspace key for that index, and commits, touching nothing else;
   A writes a row of another table, `WR`, and commits): a state change of the index the
   read SCANNED conflicts, under
   SERIALIZABLE and under the SNAPSHOT option alike [index_state_scanned_serializable,
   index_state_scanned_snapshot, index_state_index_read_explain: `COVERING(X_V ...)`];
   a state change of an index the read did not use commits, under both
   [index_state_unused_serializable, index_state_unused_snapshot]; and so does a state
   change of X_V under a record scan [index_state_record_scan_serializable,
   index_state_record_read_explain: `SCAN([IS X, ...])`]. That is the target's code:
   planning reads the store state loaded at open and adds no conflict (PlanContext.java:
   237-260); the plan-constraint check adds none (DatabaseObjectDependenciesPredicate.
   java:98); `scanIndex` adds one key per index it scans, whatever the isolation
   (FDBRecordStore.java:1500 -> 4241-4242 -> 4187-4198); and index maintenance on save
   adds one key per maintained index of the saved type (`sanitizeIndexes`, :4519-4531).
   The whole-subspace range (`addStoreStateReadConflict`, :4205-4213) is taken only by
   the record-layer API `getAllIndexStates` (:4610-4617), which the SQL path never calls.
   Go today adds a key for EVERY index of the metadata, twice: at planning
   (`fetchIndexStateSnapshot`, cascades_generator.go:2711-2757) and on every page (the
   revalidation, :2245-2246), both through `GetAllIndexStates` (store_api.go:103-108),
   whose per-index `GetIndexState` goes through `transactionIndexStateView.read`, which
   adds the key (index_state.go:478-485). So a Go reader in an explicit transaction
   aborts with 1020 on a state change of an index it never touched, where the target
   commits. Design: a conflict-free read of the loaded states, `PeekIndexStates()` (the
   view's states without `read`'s key, the target's `getRecordStoreState().getState`),
   serves both planning's snapshot and the per-page revalidation, which keep their
   one-function invariant by both calling it. The per-index key that a scan takes stays
   at every site that takes it today, and those sites are named because the SQL path
   does not go through `ScanIndex` alone: `requireReadableQueryIndex`
   (executor.go:823-839, via `ReadIndexState`) in `openIndexEntryCursor`
   (executor.go:436, the value and covering index scans, which then call
   `maintainer.Scan` directly, :471), `executeVectorIndexScan` (:626) and
   `executeAggregateIndexScan` (executor_new_plans.go:40); `ScanIndex` itself
   (index_scan.go:266, `readIndexState`); `scanIndexByType` (:339, the BY_GROUP rank
   and aggregate scans); and `scanTimeWindowLeaderboard` (:446). Those are the scan
   sites; the full census of state-key reads is every non-test call of
   `readIndexState`/`ReadIndexState` at the reviewed tree (`git grep -n -E
   '(r|R)eadIndexState\(' <TREE> -- 'pkg/**/*.go' ':!**/*_test.go'`, 38 code lines in 14
   files, less the two definitions, index_state.go:492 and 498, and the interface
   declaration at index_maintainer.go:163): 35 calls in 14 files, and each keeps its
   key. (v7 counted 36 in 15 on 9265beef; the difference is indexing_mutual.go's call in
   `mutualBuildAlreadyReadable`, which WS-C's section 7 deletes, ws-c-design.md:467.) By role: the SQL and record-layer scans above (executor.go
   1, index_scan.go 3) and rank_scan.go:51; the record-function and aggregate-function
   lookups that pick a readable index to answer from (record_function.go:71, 88;
   aggregate_function.go:219, 237), as the target's `getIndexState` in the same lookups
   does; index maintenance on save (index_maintainer.go:646, bitmap_value_index_
   maintainer.go:134, vector_index_maintainer.go:1296, 1343); the state transitions
   themselves (index_state.go, 7); the online indexer and its queue (online_indexer.go
   4, online_indexer_queue.go 7); delete-where
   (store_delete_where.go 2); and the record-layer API's `GetAllIndexStates`
   (store_api.go:117) and the SPFresh verifier (spfresh_verify_api.go:41). `PeekIndexStates`
   serves planning and the revalidation ONLY: a census test enumerates the callers of
   `PeekIndexStates` and of `readIndexState` and fails on a `PeekIndexStates` caller
   outside those two or on a new state read not in this list, and an FDB test per
   executor path (value, covering, vector, aggregate, BY_GROUP, rank) writes the scanned
   index's raw state key concurrently, as the probe does, and asserts the reader's
   1020, so a path that stops taking its key reddens. The save path's maintained-index
   keys stay.
   `GetAllIndexStates` itself takes the target's shape for its remaining caller
   (fleet/build.go:134): one read-conflict range over the index-state subspace. FDB
   tests, one per probe shape and a unit test of the range, each making the state
   change the probe makes, a raw write of the state key in its own transaction, and
   never through `MarkIndexDisabled`, which also clears the index's data
   (index_state.go:321) and would conflict on the data range instead of the key: a state
   change of the
   scanned index conflicts under both isolations; one of an unused index and one under
   a record scan commit (today's 1020 pinned first, then flipped by the change); and a
   DML statement conflicts on its maintained index and not on an index of another
   table. The flag is carried as a bool on the statement's execution
   and mapped to the record layer's isolation at the scan boundary, never as a field
   of the isolation enum type, whose zero value is SNAPSHOT (scan_properties.go:63),
   so no new field can default a read to snapshot.
5. Paging. Go pages a result internally; in auto-commit mode each page is its own
   read-only `DB.Run` transaction (cascades_generator.go:2200-2206), which cannot
   conflict, so the flag is re-applied to every page from the captured options and
   "one read version" holds only across the pages of an explicit transaction. The
   observable test is therefore an EXPLICIT transaction whose SELECT spans at least
   two pages, a concurrent write into page 2's key range committed between them, and
   the transaction's own commit succeeding under SNAPSHOT and failing with 1020
   under the serializable control.
6. The executor's INSERT, UPDATE and DELETE (executor.go:3865, 3934, 4181) check the
   isolation before opening their child, dry runs included (SOURCE: the target's DML
   plans do so, QueryPlanUtils.java:43, RecordQueryAbstractDataModificationPlan.java:
   192, RecordQueryDeletePlan.java:96; no admitted statement reaches the check, so it
   cannot be measured through SQL), and fail with Go's existing
   `RecordCoreArgumentError` (rank_scan.go:19-27, the Go form of the same Java class)
   carrying the target's message "Cannot execute plan at SNAPSHOT isolation level" and
   its new `Plan string` field set to the plan's type name, the counterpart of the
   target's PLAN log info (QueryPlanUtils.java:52-58). The field and its rendering are
   specified once, in ws-f-design.md section 7, and implemented here; the guard
   compares against SERIALIZABLE exactly, because the isolation enum's zero value is
   SNAPSHOT. A direct unit test drives each of the three plans at SNAPSHOT with a DRY
   RUN and without one.
7. JDBC field 35 (`jdbc.proto:174-181`) belongs to the Java JDBC/gRPC server, which Go
   does not implement; nothing to port.
8. Tests on real FDB, each with a reader-side WRITE so a conflict is observable, and
   a serializable control for each (the backstop of 6.6 and the two NULL-element
   write guards of 4.2 also get direct unit tests, since no admitted statement
   reaches them): a snapshot SELECT sees its transaction's own
   writes; a concurrent write into the scanned key range commits and the reader's
   commit still succeeds (the serializable control conflicts); an INDEX-backed
   snapshot SELECT with a concurrent UPDATE of a fetched record conflicts (the fetch
   is serializable) while a concurrent insert into the index range does not; count,
   MAX_EVER, join and union reads (the target's SnapshotIsolationConcurrencyTest
   shapes); a connection-scoped option across two statements, whose reader-side write
   cannot be DML on that connection (the connection scope refuses it, 6.3), so the
   test's explicit transaction runs its SELECT with the connection option set, clears
   the option with `SetOption` inside the same transaction (options are captured per
   statement execution, 6.2), then INSERTs and commits, with a concurrent write into
   the SELECT's range committed in between: the commit succeeds under the option and
   fails with 1020 in the control that never sets it; the statement and the
   connection scope each rejecting DML with no child read and no mutation; the same
   SQL run with and without SNAPSHOT sharing one cache entry while each execution
   reads at its own isolation; the two-page explicit-transaction test of 6.5; the
   per-index-kind census tests of 6.4; the per-scanned-index state conflicts and the
   header conflict of 6.4; and
   every statement class of 6.3 refused on a snapshot connection with no catalog or
   store write.

## 7. Function argument errors (relational W3)

Shared non-string function-argument failures use 22F00 (INVALID_ARGUMENT_FOR_
FUNCTION), LIKE included (section 1; ExceptionUtil.java:94-96 maps both
FUNCTION_UNDEFINED_FOR_GIVEN_ARGUMENT_TYPES and OPERAND_OF_LIKE_OPERATOR_IS_NOT_
STRING to it). Go already maps FUNCTION_UNDEFINED_FOR_GIVEN_ARGUMENT_TYPES to 22F00
(scalar_function_catalog.go:128-143); section 1 adds the LIKE operand rule, and
section 5 the all-NULL variadic case, which reaches the same code.

## 8. Order, tests and acceptance

Implementation order, green tests per increment, one joint gate for the milestone:
(1) grammar and lexer sync (sections 3 and 6 grammar, section 1 grammar) with the
token-based cache text and the planner-configuration key component; (2) per-token
literal decoding and the decorated-literal rejections; (3) LIKE value, predicate,
matcher (with the lenient operand decode) and error port with per-row timing;
(4) the arithmetic lane table (5.6), the variadic port, the nullability override
removal with the nullability table of 5.3, and the simplification of 5.4: the
translator folds deleted, the generic fold and the Go-only predicate driver deleted, the
two value rule sets and one ConstantFoldingRuleSet with both `EffectiveConstant`
overloads, threaded through the recursion, serving the whole-conjunction rule and
`rejectsNull`, the null-strict collapse in both value sets, and the REWRITING model's
tautology filter with the tie pins, the rule registered inside the
decorrelate-then-simplify conditional chain, so this step lands AFTER RFC-257 WS-F's
step 7 option is the default (the conditional types and task, D3 and D4, D5's staleness
gate, and D12's rule sets and placements, which delete the standalone
DecorrelateValuesRule registrations and move NormalizePredicatesRule and
PredicateToLogicalUnionRule to PLANNING, with the physical REWRITING prune that decides
the fold; WS-F section 2). Step (4) is GATED on the RFC-182 second crossing by name (v12;
v11 said this dependency was explicit here, and it was not): WS-F's crossing census
(ws-f-design.md 2.3, 2.6) names [union_leg_type_annulling_fold_where]. At WS-F's F-5 that
census can check only that the leg crosses with one final, because the fold rule is
WS-E's. So step (4) re-runs WS-F's crossing census with the fold rule in place, and the
gate requires that the union leg reach PLANNING as ONE final, that final the fold, and
that the row answer as the target does; (5) IN/array rules, the
array-constructor promotion and the consumer boundary, the literal-pipeline test of
`ResolveIn`, the target's explode rule over a select for every top-level IN conjunct,
the single-element collapse deleted, the target's IN-source admission, and the
comparand IN source evaluated at open (4.1), with `isSupportedExplodeValue`'s ARRAY<RECORD>
refusal kept; with it, and not without it, the partition arm (a spanning predicate lower
in an explode partition), the in-join rule aligned with the target's (no Go-added
Preserve ordering, one memoization per partition and source ordering), the compensation
correlation guard's explode arm (#7; a table alias the probe does not feed stays
refused), the multi-binding IN-union executor (the product, saturating, checked against
the plan's maximum, merged, no concatenation), the NaN binder arms for each of the
binder's four callers (4.1(b)), and the planning-cost pins of 4.1(b), with the
metamorphic sweep failing on 54F02; the executor extends WS-F F-7's
merging in-union executor, so this step lands AFTER F-7 (ws-f-design.md 4.3 and its
phase list; F-5, the physical prune as default, precedes it already through step (4)),
and the gate
asserts that no ordered two-IN query gains an `InMemorySort` (4.1(b)) and re-measures
on F-7's admission every plan and count 4.1 took on the pre-F-7 prototype; (6) parameter
binding (the CAST of a string to DOUBLE and FLOAT made to yield the target's NaN bits
with it, 4.1(b), retiring the PENDING DIVERGENCES.md entry; the CAST parser of 4.1(b)
and MIN and MAX with Java's `Math.min`/`Math.max`; typing order with the Valuer
check before each dereference, untyped NULL,
named and array parameters typed by static element kind, slices in DML, time.Time
bound as canonical TIMESTAMP text with its year domain, the one temporal parser with
its domain and the deletion of `functions.ParseTimestamp`, `CastValue` and the
`time.Time` comparison arms, the coercer's DATE and TIMESTAMP arms, the
catalog-reload type test, values, batch-first errors, key rendering, UTF-8
refusal at bind and in `serializeUnion`, the JDK UUID parser at every site, the recursive
assignment lattice for every source); (7) statement and
connection options (SetOption, the DSN parameters and their error, the pool reset),
statement classification and snapshot execution, and the index-state conflicts
(`PeekIndexStates` for planning and revalidation only, the named per-index key sites,
the subspace range of `GetAllIndexStates`).
Dependencies between the steps (v12; v11 left it unstated whether the wire fix waits on
F-7, v11 Graefe L9 and Torvalds L9). Step (6) depends on step (5) only where a bound array
reaches an IN, through the IN-source admission of 4.1. Its other parts depend on neither
(5) nor any WS-F phase: the scalar binding, the CAST's parser and bits, MIN and MAX, and
the temporal channel. The order above is the order of the WORK. Nothing of RFC-257 reaches
master before the whole tree merges, so no user runs a build between the steps, and
landing (6)'s write fix before (5) would change no release. The implementation may take
(6)'s independent parts first; the gate is the milestone's.
Every changed expectation cites its upstream change or
its probe. The oracle stays pinned; each Go outcome it prints becomes a Go assertion
in the owning section's tests.

Acceptance of rounds v11 and v12, whose pins record BOTH engines (v11 Graefe L1 asked how
they are accepted): their GO pins are Go's current outcome, and each flips at the step
that fixes it. Step (6) flips the CAST bits and spellings and MIN/MAX [go_insert_cast_nan,
go_cast_*, go_t_id10*, go_u_id10, go_u_id11], each to the target's value in the same
round. The planning-cost pins are the target's alone; Go's counts are the plan-harness
pins of 4.1(b). A GO pin that does not move with its step, or moves at another step, is a
red test, so the flip is reviewed, not absorbed.
Acceptance comparison, per oracle row of rounds v1 to v10 (457 rows, each rendered through `wseRender`;
the Go side binds an `int` kind as int32 and a `long` kind as int64 in every
Describe): the Go outcome equals the Java outcome in CLASS (rows or error), SQLSTATE,
row values, JDBC type name, nullability and, for DML, the update count; the error
MESSAGE is compared where the target's message is shared (sections 1, 2, 4 and 5
adopt the target's texts, the lane refusal's included) and not for syntax errors (Go's
caret format) or other internal errors; an EXPLAIN or DESCRIBE row is compared on
admission only (each engine renders its own plans). The connection-option rows
(DRY_RUN and SNAPSHOT) are compared through the Go runner once section 6.2 gives Go
`SetOption`: the plandiff Go runner's prepared paths take the same
`connectionOptions` map the Java step takes and apply it through `Conn.Raw` before the
statement, so the rows' GO lines stop reading NO-GO-CONNECTION-OPTION;
`dry_run_connection_create_template` compares class, COUNT and the template name in
its follow-up's first column (the DESCRIBE's metadata columns are each engine's own).

The rows that stay different are exactly these, each listed in DIVERGENCES.md with its
probes, and each class states why:
- Unmapped target exceptions: every row whose target outcome is XXXXX
  ArithmeticException (division by zero, INT overflow) or XXXXX NumberFormatException
  compares in class, with Go's 22012 or 22003 (section 5), e.g.
  [coalesce_evaluates_every_argument, prepared_int_overflow,
  coalesce_not_head_where, fold_div0_or_not_false_where, least_overflowing_literal].
- The arity message [coalesce_single_argument, greatest_single_argument]: XX000 in
  both, Go's message names the arity.
- The approved LIMIT extension [prepared_limit_param].
- Ordering by a constant under Go's in-memory sort fallback [order_by_string_literal,
  order_by_constant_expression, prepared_order_by_param].
- GROUP BY without an ordering index, the documented Go extension
  [where_then_having_order, having_literal_control, group_by_literal_control].
- The target's parameter-ordinal defect, a WRITE divergence on DML
  [prepared_select_then_where, update_set_then_where_order,
  insert_select_then_where_order, select_expr_then_where_order].
- The target's GREATEST/LEAST start-value defect [greatest_negative_doubles,
  greatest_negative_floats, greatest_zero_doubles, greatest_zero_floats,
  greatest_nan_double, least_infinite_doubles].
- The target's internal failure on a projected untyped bound NULL
  [select_untyped_null_param].
- The approved nullable-array comparison extension [null_array_comparison_operand,
  array_null_element_eq_literal].
- The Go-only scalar functions [nullability_scalar_function].
- The REWRITING prune's semantic-hash ties (5.4g) and the join-nesting tie (4.1),
  whose Go answer and EXPLAIN are PINNED per schema, not accepted as a set: the answer
  may differ from the target's, and a change to it is a reviewed pin change
  [null_strict_div0_cast_null_eq_one_where, v4_schema_div0_cast_null_eq_one_where,
  v5_schema_div0_cast_null_eq_one_where_first, first_planning_a_exec,
  first_planning_b_exec, null_strict_cast_null_beside_div0_where,
  null_strict_cast_null_beside_div0_where_empty_table, coalesce_div0_five_is_null_where,
  in_cast_null_join_inner_empty, the bare `WHERE X AND X` of 5.4j, and the EXPLAIN and
  trace rows of the same statements,
  whose plan text is each engine's own]. The same fold beside another conjunct is NOT a
  tie [conj_false_where]: annulment leaves one conjunct against two, which the count
  decides in both engines.
- The target's internal failures on a COALESCE under AND or NOT and on a CASE whose
  rebuilt branches' types differ (5.4h) [coalesce_true_div0_and_column_where,
  coalesce_true_div0_and_column_where_explain, not_coalesce_false_div0_where,
  not_coalesce_false_div0_where_explain, is_null_case_div0_branch_where]; Go's answers
  are pinned as 5.4h says.
- The trace rows [trace_*]: they record the target's planner internals and have no Go
  counterpart; their Go side is the REWRITING-prune tests of 5.4.
- The IN-subquery row, rejected by both with different messages [subquery_order].
- The target's internal failure beside an IN list: `id IN (1, 2) AND 1 / 0 = 1 AND 1 =
  2` is an internal VerifyException in the target (XXXXX: no SQLSTATE), where the same conjuncts beside `id = 5`,
  an index range or an index equality raise the division [pk_in_annulling_fold_where,
  against pk_annulling_fold_where, index_range_annulling_fold_where,
  index_eq_annulling_fold_where]; Go raises the division's 22012 on all four, the
  target's answer without the IN list, and the defect is not ported.
- The tie folds of round v8, whose rows are equal and whose plans are each engine's
  hash pick, pinned per engine [index_tie_not_over_comparison_explain,
  index_tie_duplicate_or_explain, pk_beside_tie_fold_explain,
  index_beside_tie_fold_explain].
- The declared signed-zero equality (DIVERGENCES.md, "Signed zero"): Go's `=` treats
  -0.0 and 0.0 as equal where the target's does not [f_in_zero_twice_index_where,
  f_eq_zero_index_where, f_eq_negative_zero_index_where, f_eq_zero_rows_where,
  f_eq_negative_zero_rows_where].
- The temporal rows of rounds v9 and v10: the target has no DATE or TIMESTAMP type and
  no temporal CAST, and refuses each with 42F18 or 42601, while DATE and TIMESTAMP are
  Go's own extension [date_lt_null_timestamp_where, date_not_lt_null_timestamp_where,
  greatest_date_timestamp_select, least_date_timestamp_select,
  date_lt_bound_null_timestamp, timestamp_column_rows, cast_date_value_select,
  cast_timestamp_value_select, cast_column_to_date_where]; their Go answers are 4.3's,
  pinned by its tests.
- The declared NaN index extension (4.1(b)). Go's indexed NaN equality scans the two NaN
  blocks, with a binder KEY FILTER for the comparisons after the NaN component, and it
  claims no order over a later column. The target probes the one packed key of its NaN's
  bits and claims that key fixed. The ROWS differ wherever a stored NaN has other bits
  than the probe's: the target's probe misses such a row, and Go returns it. MEASURED:
  a SQL division stores `fff8000000000000` (`java_t_d_id3`), the target's `CAST('NaN' AS
  DOUBLE)` probes `7ff8000000000000`, and MIN and MAX over the division's NaN store its
  bits (round v12). So after step (6) the rows agree for NaNs of the CAST's bits and
  differ for the rest (v11 said "the rows are equal for every NaN a statement can write",
  refuted by its own round). The plans differ where the target claims order
  [nan_then_eq_where, nan_then_eq_explain, nan_in_then_in_where, nan_in_then_in_explain,
  nan_order_by_suffix_where, nan_order_by_suffix_explain; the EXPLAIN rows are each
  engine's own, the row answers compared].
- The NaN refusals kept per scan kind (4.1(b), "THE BINDER'S FOUR CALLERS"): an
  aggregate-index scan and a vector-partition prefix bound to a NaN keep Go's loud
  refusal where the target answers from its NaN's one key.
- The planning budget (4.1(b), "THE BUDGET"): an ordered query with five or more IN
  lists over one table, which Go plans today in 118,374 tasks, costs 326,752 after step
  (5) and fails with 54F02 at Go's 150,000-task tripwire; the target sets no budget and
  did not plan it within two minutes. CHANGELOG states it.
- The IN-union product (4.1(b)): the target's `int` product wraps (65536 × 65536 is 0,
  answered as empty); Go's saturates and refuses. And Go refuses the 5-by-5 product it
  answers today, as the target does [w8_in5x5_rows, a WS-F row].
- A multi-binding IN-union continuation resumed under a bound array of another size is
  refused as an invalid continuation; the target resumes the other list at the old
  positions (4.1(b)).
- `isSupportedExplodeValue` keeps refusing an ARRAY<RECORD> explode as an IN source,
  where the target admits it (4.1): Go's record binding reads such a source as NULL.
- The two-source IN-union of round v9 (4.1(b)): its rows are compared
  [two_in_ordered_where], and its plan [two_in_ordered_explain] is asserted as the
  target's shape by name at the implementation gate; the derivation is measured on the
  prototype (v11: the narrowed compensation guard builds it), so this entry is deleted
  when step (5) lands with that assertion holding, and the gate is not passed while it
  is here.
Rows that depend on another workstream, compared when it lands and declared here with
their owner: `like_enum_operand` on RFC-257 WS-J F6 (enum DDL; until then its Go
assertion runs through record-layer metadata, section 1.5), and the ENUM assignment
rows [insert_string_literal_into_enum, insert_bound_string_into_enum,
insert_bad_string_into_enum] on the same F6; `nn_bitand_literals` on
WS-J F4 (the bit operators' rows of the lane table, section 5.6); and every row whose
target result column is ARRAY [array_cast_null_empty_table, insert_array_parameter,
insert_empty_array_parameter, insert_null_array_parameter, update_array_parameter,
insert_int_array_into_float_array, insert_empty_array_into_float_array,
insert_long_array_into_bigint_array]
on TODO.md CQ-74, the result-metadata item under which Go reports an ARRAY column by
its element type (the ARRAY arm of `cascadesTypeName`, cascades_generator.go); their
values and classes are compared now, their type names when CQ-74 lands. The
index-state and read-scope rows [index_state_*, read_scope_*] are two-connection
probes the Go runner does not drive (their GO lines read NO-GO); their Go side is the
FDB tests of 6.4, one per row, asserting the same READ and COMMIT outcome.
Full `just test`, race, the LIKE, lexer and lenient-decode fuzz targets, the grammar
drift check, and the 1M stress comparison protocol (the cache-key, binding, arithmetic
and save-path changes touch the executor path) on a frozen tree precede the
implementation gate. No commit, publication, CI trigger or PR change is authorized by
this design.
