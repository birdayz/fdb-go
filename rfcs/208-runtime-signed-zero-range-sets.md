# RFC-208 — Runtime signed-zero equality is an ordered physical range set

Status: **ACCEPTED AND IMPLEMENTED — 2026-08-01.**

Target: close the measured correlated/parameterized signed-zero gap deliberately
left by RFC-196 and watch-list entry 3 in road-to-prod.md.

Supersedes: RFC-196's compile-time-zero prefix termination. Its terminal
single-range widening remains a valid optimization; dropping suffix constraints
to make that optimization applicable does not.

## Decision in one sentence

Keep Go's numeric predicate equality, keep the shared FDB tuple wire encoding,
and map one logical floating-point equality to a typed, ordered, disjoint,
lazily enumerated physical range set whenever its evaluated non-NaN value is
zero.

NaN is part of the decision, not hidden under the signed-zero proof: a known NaN
does not select this access path, and a dynamic NaN reaching an already chosen
float scan returns a typed error before opening storage. The exact raw-NaN
payload mapping remains separately booked until it can be implemented for
non-terminal composite keys without a false exact-range claim.

## The measured defect

Against index (v, w), this correlated probe returns no row today:

~~~sql
SELECT t.id
FROM t, o
WHERE t.v = o.k AND t.w = 5 AND o.id = 10;
~~~

with o.k = +0.0 and stored key t.(v,w) = (-0.0,5).

The plan is precise-looking but false:

~~~text
FlatMap(Scan(O), Fetch(IndexScan(T_VW, [=, =])))
~~~

At runtime the inner comparison tuple is packed as (+0.0,5), so it misses
(-0.0,5). The reverse sign direction fails the same way. A terminal equality
works only because the range binder already widens a terminal zero
to the contiguous subtree [-0.0,+0.0]. (At the time of writing that was
scanComparisonsToTupleRange; it has since been retired as a dead twin — RFC-217 —
and the widening now lives only in bindScanComparisonsToRangeSet.)

The non-terminal answer is not contiguous. The interval from (-0.0,5) to
(+0.0,5) also contains keys such as (-0.0,9) and (+0.0,1). Returning that
interval without a residual is wrong; returning one packed sign is incomplete.
The exact answer is two disjoint ranges:

~~~text
allOf((-0.0, 5))
allOf((+0.0, 5))
~~~

For (v1,v2,w) with both equality comparands zero and w=5, the exact answer is
the four-sign Cartesian product. This is not a two-probe special case.

## Java and CockroachDB, read before designing the Go fix

The Java Record Layer 4.12.11.0 does not contain a multi-range solution to
port. `ScanComparisons.toTupleRange` evaluates every equality comparand, puts
the resulting objects into one `Tuple`, and returns one `TupleRange` (apart
from its ordinary single trailing-range handling). A correlated
`ValueComparison` evaluates its comparand from the runtime context, but the
physical result is still one packed tuple.

Java does not provide one stable signed-zero or mixed-numeric predicate
semantics that could justify that physical shape:

- the legacy/query-predicate `Comparisons.compareEquals` path uses boxed
  equality. `Double.equals` distinguishes -0.0 from +0.0 and canonicalizes NaN
  payloads;
- direct `RelOpValue.eval` selects typed physical operators. Its numeric EQ
  operators use Java primitive `==`, so the zero signs compare equal, NaN does
  not equal NaN, and mixed numeric operands use Java's primitive promotions;
- `RelOpValue.toQueryPredicate`, the lowering used to make a SARGable
  predicate, first promotes both operands to their maximum common type, then
  constructs a `SimpleComparison` or `ValueComparison`. Evaluation of that
  lowered comparison comes back through the boxed comparison helper. Thus an
  INT/DOUBLE pair is not accurately described as permanently unequal merely
  because the original boxes differed: both operands may first become DOUBLE.

Consequently Java's one physical probe is context-dependent behavior, not a
proof that one probe represents the logical predicate domain. In particular,
the direct relational evaluator and the lowered query-predicate evaluator can
disagree on signed zero and NaN. `ScanComparisons.toTupleRange` also performs no
inverse numeric projection for a comparison equivalence class. Go's `cmpAny`
compares integer and floating values in a shared float64 domain, so a one-key
rule would additionally be incomplete at the 2^53 precision cliff.

CockroachDB demonstrates the other principled solution: make the physical
encoding congruent with the logical comparator. Its ascending float encoder
maps every NaN to one `floatNaN` marker and, because `f == 0`, maps both zero
signs to one `floatZero` marker. Its SQL `DFloat.Compare` likewise treats the
zero signs equal and all NaNs equal. The encoded equality classes and the SQL
equality classes therefore agree by construction. CockroachDB owns that key
encoding; this project does not own FDB's shared tuple format, so it cannot use
the same normalization without changing existing and Java-written keys.

The tuple encoding cannot change either. Java and Go share FDB's tuple wire
format, existing data contains both encodings, Java-written index entries
retain the sign, and UNIQUE/DISTINCT/aggregate-index behavior already depends
on that physical identity. This RFC is a Go-only mapping from logical equality
to physical reads, with no stored or serialized key change.

The lowered Java comparison helper also canonicalizes NaNs for equality while
the FDB tuple encoding retains NaN sign and payload bits. That is a separate,
larger physical equivalence class. Negative NaNs and positive NaNs occupy two
physical regions, but for a non-terminal composite key, selecting an exact
suffix across every payload cannot be represented by a finite small set of
exact tuple ranges. This RFC therefore scopes its exhaustive range theorem to
non-NaN values and makes a dynamic NaN correct-or-loud. It does not repeat the
latent one-payload probe.

## Principles

1. **Make logical and physical equivalence congruent.** An engine that owns its
   wire format may canonicalize on write, as CockroachDB does. With an immutable
   shared wire format, one logical equality may map to several physical keys and
   the reader must represent that projection honestly.
2. **One classification owns all deductions.** Execution, ordering,
   uniqueness, cardinality, and cost consume the same physical-equality shape.
3. **Exact or loud.** Never broaden a disjoint range without a residual,
   silently discard a branch, cap a Cartesian product, or probe one NaN payload
   while claiming all NaNs.
4. **Runtime values choose the current branches.** Literals, parameters, and
   correlated values use the same binder.
5. **Physical key types choose tuple representation.** The aligned index/PK
   component type overrides the RHS declared or runtime type. Unknown is
   conservative; it is never permission to pack the convenient type.
6. **A range set is one leaf.** Skip, row limit, scan budgets, continuation,
   covering/fetch, errors, and close semantics belong to the whole set.
7. **Logical equality is not physical fixedness.** A SARGed equality that may
   fan out must not let ordering or point-probe proofs skip that key.

## Design

### 1. Carry authoritative physical key-component types

Every physical plan that evaluates tuple-key comparisons carries a type slice
aligned one-for-one with those comparisons:

- RecordQueryScanPlan carries primary-key component types;
- RecordQueryIndexPlan carries value-index grouping-key component types;
- aggregate index plans carry their grouping-key component types and, for
  permuted MIN/MAX, the physical grouping-prefix boundary;
- RecordQueryVectorIndexPlan carries partition-key component types.

The candidate stamps these types from the record descriptor and expanded key
expression when it constructs the plan. Every With method, structural identity,
and plan copy preserves them. A type that genuinely cannot be established is
UnknownType; no consumer reconstructs a conflicting type from the comparand.

A secondary index has a second physical coordinate sequence after its root:
`TrimPrimaryKey(primaryKey)`. Go carries a separate authoritative type vector
aligned with the visible PK coverage names and trims names and types in lockstep.
That flat vector is admitted only when the physical PK topology is exactly
top-level scalar fields, optionally after one leading RecordTypeKey. The leading
type key is a fixed coordinate only for a single-record-type index; a shared
index stops visible suffix ordering at that partition. Literal, version,
function, nested, mid-hidden, and suffix-hidden PK shapes are not flattened.
They retain no guessed coverage/order metadata until Go carries structural PK
Values for the complete entry key. The executor consumes only this validated
plan metadata; it never recomputes `KeyExpression.FieldNames` and tail-aligns a
hidden value into a logical field slot.

This is deliberately more conservative than Java's representation. Java's
`ValueIndexExpansionVisitor` normalizes and structurally trims the actual PK
KeyExpression, expands every remaining coordinate into the candidate full key,
and derives ordering from that full expression. Go's current name vector cannot
state the same proof for arbitrary expressions, so it abstains instead of
pretending the visible leaf names are tuple coordinates.

The executor applies this authority matrix:

| physical key type | evaluated comparand | binder result |
| --- | --- | --- |
| FLOAT | compatible numeric equality, including Unknown-declared | exact float32 projection and signed-zero alternatives |
| DOUBLE | compatible numeric equality, including Unknown-declared | exact float64 projection and signed-zero alternatives |
| FLOAT or DOUBLE | ordered non-NULL predicate | `UnsupportedPhysicalFloatOrderingError`; raw stored NaNs make one-range tuple order unsound |
| STRING | string STARTS_WITH | exact prefix-string range |
| non-STRING | STARTS_WITH | `UnsupportedPhysicalStartsWithError` before storage |
| INT or LONG | integer carrier | exact int64 key |
| INT or LONG | runtime float, equality | inverse `cmpAny` class: empty or one int64 key; a plural class fails typed and loud |
| INT or LONG | runtime float, ordered | exact inclusive int64 truth interval |
| known scalar | compatible carrier | authoritative physical tuple carrier |
| known scalar | incompatible carrier | `IncompatiblePhysicalComparandError` before storage |
| Unknown or missing | non-NULL | `UnknownPhysicalKeyTypeError` before storage; never infer from RHS |
| Unknown or missing | type-independent NULL case | exact NULL/empty/null-boundary result |

Thus an integer key compared with a dynamically typed zero remains one integer
key, while a FLOAT key compared with an integer literal zero emits float32 sign
alternatives. FLOAT never accidentally becomes DOUBLE because the RHS happened
to evaluate as float64. A planner candidate with an Unknown physical type does
not claim the component as SARGed. The runtime error is the safety net for a
hand-built or stale plan; it is not an invitation to guess from a known operand
declaration.

INT and LONG both use the complete signed int64 tuple-carrier domain for
runtime float inversion. The physical TypeCode no longer retains the protobuf
field kind: uint32/fixed32 fields are classified as INT but values through
4,294,967,295 are packed as int64, while uint64/fixed64 fields are classified
as LONG and extraction casts their raw bits to int64 (so values above MaxInt64
occupy the negative half of the signed carrier). An INT32-only inverse would
silently miss valid uint32 keys. A narrower schema-specific domain could reduce
loud plural-class failures in the future, but may be used only if that source
kind is carried authoritatively in the plan.

For integer-key equality, `cmpAny` promotes the stored integer to float64.
Consequently one runtime float can equal several adjacent int64 keys at and
above 2^53. The current fixed-key plan contract accepts only empty or singleton
inverse classes and raises `UnsupportedPhysicalNumericProjectionError` for a
plural class. Ordered comparisons are monotone even across that cliff, so the
binder computes their complete inverse interval by overflow-safe binary search
over the carrier domain.

One shared planner helper returns a PhysicalEqualityShape containing:

- mayFanOut: the equality can select both signed-zero encodings;
- physicallyFixed: exactly one physical key is proven;
- successfulSeekUpperBound: the maximum number of signed-zero branches for
  non-NaN execution, with checked multiplication;
- provenRowMultiplicity: a finite value only when every float comparand is a
  known non-NaN constant; otherwise unknown because a dynamic value may be NaN;
- unsupportedKnownNaN: a constant NaN that prevents this access path.

IS NULL is one physical key. Ordinary = NULL remains unsatisfiable. A known
nonzero finite value, infinity, or non-float value is physically fixed.
A known float zero may fan out and contributes two physical key possibilities
unless the terminal contiguous-range optimization reduces the number of seeks.
A typed dynamic float or Unknown dynamic component may fan out, but its row
multiplicity is unknown because its domain includes NaN. Checked overflow makes
a proof unknown and saturates cost at the repository's largest finite heuristic
value; it never changes execution.

The same helper is the only source used by candidate ordering, plan ordering,
cardinality, and cost. Tests pin the authority table so the planner and executor
cannot drift back to RHS-only coercion.

### 2. Evaluate comparisons into a compact range specification

`bindScanComparisonsToRangeSet` is the plural range binder. It produces an
evaluated `boundScanRangeSet`, evaluates each operand once, and coerces it using
the aligned physical type. It (with its `...WithTerminalWidening` sibling, which
shares the same body) is now the only range-binding implementation in the executor:
the legacy singular `scanComparisonsToTupleRange`, which this RFC left in place for
its isolated compatibility unit tests, has since been deleted and those tests
re-pointed at the plural binder (RFC-217).

The range set stores:

- one compact evaluated component per equality position;
- one or two scalar alternatives per component;
- an optional evaluated trailing inequality or STARTS_WITH descriptor;
- direction and the terminal-widening decision.

It stores no copied tuple prefix and no materialized Cartesian product.

For the contiguous equality prefix:

- an ordinary value contributes one tuple element;
- a FLOAT zero contributes float32(-0) and float32(+0);
- a DOUBLE zero contributes float64(-0) and float64(+0);
- IS NULL contributes nil once;
- ordinary = NULL returns the empty range set;
- null-safe equality with NULL retains the exact-null probe;
- a NaN returns UnsupportedPhysicalFloatEquivalenceError before any cursor or
  FDB range is opened.

The comparison slice is validated before operand evaluation: it must contain a
contiguous equality prefix, at most one inequality component, and then only
nil/Empty components. A gap followed by a constraint, an equality after an
inequality, or a second constrained tail returns
`InvalidScanComparisonShapeError`; no later predicate is silently discarded.

Unknown physical metadata is admitted only when the result is independent of
the non-null tuple carrier: IS NULL is one nil key, ordinary = NULL and binary
ordered/STARTS_WITH NULL are empty, null-safe equality with NULL is one nil key,
and IS NOT NULL is the exclusive nil boundary. Every evaluated non-null bound
requires authoritative physical metadata.

STARTS_WITH additionally requires an authoritative STRING key. The comparison
truth table is string/string-only; a tuple prefix over an integer, BYTES, or a
string-backed DATE/TIMESTAMP carrier would be a physical operation with no
matching logical predicate. Such a hand-built plan fails with
`UnsupportedPhysicalStartsWithError` before storage.

The optional trailing comparison is evaluated once and applied to every active
equality alternative. Multiple supported inequalities on that component are
intersected in packed one-element FDB tuple order, choosing the stricter bound
independent of insertion order and choosing exclusive over inclusive at an
equal endpoint. The primitive is tested against FLOAT/DOUBLE signed-zero tuple
endpoints as well as ordinary carriers, and turns contradictory bounds into an
empty range.

An ordered FLOAT/DOUBLE SARG is not currently a supported use of that
primitive. FDB tuple order preserves raw NaN sign and payload: negative NaNs
sort below negative infinity, whereas `cmpAny` canonicalizes every NaN as one
logical greatest value. Thus `< finite` can physically include false negative
NaNs and `> finite` can miss logically true negative NaNs. The planner retains
the predicate as a residual or declines the candidate, and a hand-built plan's
runtime binder raises `UnsupportedPhysicalFloatOrderingError` before storage
for every non-NULL ordered threshold. Ordered comparisons whose RHS evaluates
to NULL remain the type-independent empty result, and IS NOT NULL remains the
exact exclusive-NULL range. The signed-zero endpoint projection helper remains
specified and tested, but cannot justify an indexed FLOAT/DOUBLE ordered scan
until raw NaN normalization or a compensating physical access shape exists.

A terminal zero with no later constraining
comparison may retain the exact inclusive subtree range
prefix+[-0.0,+0.0], reducing two seeks to one.

The odometer is a choice vector over the evaluated components. Forward scans
visit -0 before +0 at each float-zero decision; reverse scans visit +0 before
-0. To open one leaf, the binder materializes one tuple workspace and one
TupleRange in O(k) time, where k is the comparison-prefix length. Advancing to
the next leaf mutates the choice vector and rebuilds that one workspace; it is
O(k) worst case. Live evaluated state, choice state, and tuple workspace are
all O(k), independent of the 2^z possible leaves.

The physical ranges are pairwise disjoint and are enumerated in full FDB tuple
order. No dedup cursor is added: dedup would be unnecessary for ordinary keys
and wrong for duplicate-producing indexes.

### 3. Use a flat range-set cursor and flat continuation

Do not build nested ConcatCursors. Their recursively captured prefixes and
nested continuation marshaling do not establish the required linear bound.
Introduce one executor-internal scanRangeSetCursor[T] with a lazy leaf factory:

~~~text
open(TupleRange, innerContinuation, childProperties) -> RecordCursor[T]
~~~

Only the active child exists. The cursor owns the evaluated components, choice
vector, current inner continuation, and whole-range execution state.

Add a Go-only relational continuation message:

~~~proto
message ScanRangeSetContinuation {
  required bytes fingerprint = 1;
  repeated uint32 choices = 2 [packed = true];
  optional bytes inner_continuation = 3;
  optional bool child_started = 4;
}
~~~

The range-set framing is flat and O(k), never a recursively nested
continuation. Total token size and serialization work are O(k + |inner|), where
|inner| is the active leaf cursor's token. In particular, a vector maintainer's
leaf token can itself be Theta(K * per-entry state) when it records a K-result
horizon; that cost is not recursively multiplied by the range set.

The fingerprint covers plan identity, aligned physical types, evaluated
coerced components and tail, terminal-widening shape, and direction. Built-in
type graphs are encoded structurally; a typed-nil type or an active-path cycle
returns a typed identity error before binding or opening storage, while a
shared acyclic type subgraph has the same identity as an independently
duplicated equal subgraph. A continuation whose fingerprint, choice arity, or
alternative bounds disagree returns ContinuationParseError before opening a
child. The wrapper format is used even when the evaluated set has one leaf, so
a dynamic nonzero page never switches between raw-leaf and range-set
continuation formats.

This Go-internal format intentionally fails closed on pre-range-set raw leaf
tokens. It does not attempt an ambiguous raw-versus-wrapper fallback: such a
token returns ContinuationParseError and the query must restart without it.
No continuation format shared with the Java Record Layer changes.

child_started distinguishes a continuation inside a leaf from a resumable stop
immediately before an unopened leaf. An absent inner continuation for a started
child means the child's start, not the range set's start.

OnNext obeys these rules:

1. lazily open the current child;
2. forward values and wrap every continuation with the current choices;
3. advance the odometer only when the child returns SourceExhausted;
4. on any other no-next reason, retain the current choices and inner
   continuation and return the same reason;
5. close an exhausted child before opening the next, and surface a close error;
6. if leaf construction fails, return that error without advancing;
7. return SourceExhausted with EndContinuation only after the final child.

Empty range sets return recordlayer.Empty without indexing an empty choice
slice. Index metadata, maintainer support, and permuted aggregate metadata are
prevalidated before constructing the range cursor where possible. A
construction-time error is deterministic and typed; no earlier range is
returned and then followed by a late structural surprise.

Close is idempotent. It closes the active child exactly once, never calls an
unopened factory, latches the range set closed even when child Close returns an
error, and returns that first close error. The repository has no shared
closed-cursor error today, so this cursor introduces and consistently returns a
typed scanRangeSetCursorClosedError from OnNext after Close. Tests cover empty,
malformed continuation, factory error, child error, close error, and every
no-next reason.

### 4. Make resource limits whole-range, not per child

Before binding or opening any child, normalize a nil props.ScanState once and
copy that one pointer to every child. Children receive
props.ClearSkipAndLimit(); skip and returned-row limit are applied once outside
the range-set cursor, after fetch/covering shaping at the same layer as today.
State, isolation, direction, scanned-row/byte/time limits, and
FailOnScanLimitReached are preserved.

Leaf cursors intentionally grant one initial attempt in paginating mode. If
every physical branch received that privilege, an already exhausted budget
could admit one row per branch. The range-set cursor therefore owns one
logical initial-pass flag:

- the first OnNext attempt of the first active child consumes the pass, even if
  the child is empty;
- before opening or first-pulling every later child, the range cursor checks
  the shared scanned-record, byte, and elapsed-time state itself;
- an exhausted budget returns the matching no-next reason with child_started
  false and the next choice vector, or ScanLimitReachedError under fail mode;
- a child out-of-band stop is propagated and never causes branch advancement;
- on continuation resume, a fresh page/transaction state receives exactly one
  new logical pass, regardless of which choice vector resumes.

This gate removes the per-branch free-pass hole and makes an empty first branch
observable to the logical gate. It does not pretend scanned-record limits price
empty FDB seeks; the planner's explicit seek cost covers that risk and
time/byte limits remain shared.

The wrapper order is deliberate:

~~~text
leaf range cursor(s)
  -> one range-set cursor
  -> one covering projection or record fetch
  -> one skip/returned-row-limit wrapper
~~~

For primary scans, record-type prefix transformation and endpoint clamps happen
per materialized range, then stored records are concatenated and mapped once.
For value scans, IndexEntry children are combined before covering or fetch.
For aggregate scans, every child produces the same already-validated row shape,
including permuted MIN/MAX.

### 5. Cover every singular physical consumer, including vector partitions

The plural contract applies to:

- primary record scans;
- ordinary value-index scans, covering and fetching;
- aggregate index scans, including permuted MIN/MAX;
- vector index partition-prefix scans.

PERMUTED_MIN/MAX requires an explicit physical-layout boundary. With
groupingCount g and permutedSize p, its BY_GROUP key is:

~~~text
[group[0:g-p], aggregateValue, group[g-p:g]]
~~~

Only group[0:g-p] is a contiguous leading SARG prefix, and positive p also
destroys the aggregate plan's claimed logical [groupPrefix,groupSuffix]
ordering. The current aggregate rule has no compensation node that can safely
residualize the permuted suffix, and multi-aggregate intersection assumes
logical group order. Therefore this RFC supports PERMUTED_MIN/MAX only when
permutedSize is zero. A candidate with positive permutedSize is explicitly
ineligible and falls back to base-record aggregation; it is not partially
SARGed. The executor still carries/prevalidates the boundary and rejects an
illegally hand-constructed cross-boundary plan before storage. It never applies
a logical [groupPrefix,groupSuffix] range to the physical
[groupPrefix,aggregateValue,groupSuffix] key.

For a partitioned vector plan, evaluate the equality prefix with the same
physical type authority and sign alternatives. Each exact physical partition
prefix opens one existing self-limiting per-partition top-k vector scan; the
range-set continuation tags the active physical partition and preserves each
maintainer continuation. This retains the current per-physical-partition rank
semantics and never treats two HNSW graphs as one globally ordered stream.
Forward/reverse tuple ordering is used only to choose deterministic partition
order; vector distance results do not acquire a generic ordering property.

The unpartitioned ordered-stream vector path has no partition equality prefix
and is unchanged. A future partitioned ordered-stream mode must explicitly
define cross-partition distance merge semantics before it can reuse this
mechanism.

A production path that cannot honor the plural contract fails explicitly; it
may not use the first range.

### 6. Keep all usable suffix constraints SARGed

Delete RFC-196's constant-only early return from
ValueIndexScanMatchCandidate.ComputeBoundParameterPrefixMap. A valid scan
prefix remains zero or more equalities and at most one trailing inequality.
The runtime range set expresses it exactly for literals, parameters, and
correlations.

This improves the constant case from a broad two-zero-group scan plus residual
filter to the same exact probes used by the correlated case. It also removes
the planner/executor asymmetry where a literal zero and the same value from an
outer row choose materially different access shapes.

This RFC does not implement general IN plus trailing equality planning. An
IN-derived equality leg does use this binder, so signed-zero element order
cannot choose which physical sign is read. CQ-75 is closed only after an
empirical plan/result test proves that overlap; CQ-76 remains separate.

### 7. Separate logical equality from physical fixedness in ordering

Extend MatchedOrderingPart (or the equivalent shared property) with explicit
physical fixedness derived from PhysicalEqualityShape. Retain the original
comparison as the logical SARG; do not encode this fact by replacing equality
with an empty or synthetic comparison.

For an ordinary non-fan-out key:

- physically fixed equality components may be skipped;
- the first possibly expanding equality remains a sorted physical key;
- later suffix keys remain ordered only as part of the full physical sequence,
  not globally after deleting the expanding key.

Consequently ORDER BY v,w can use a signed-zero range set, while
WHERE v = ? ORDER BY w LIMIT 1 keeps its Sort when v may expand.
SatisfiesRequestedOrdering, intersection ordering, candidate matched ordering,
plan HintOrdering, and HintRichOrdering all consume physical fixedness.

For a duplicate-producing/fan-out index component, a possibly expanding
equality is neither physically fixed nor a safely exposed scalar sorted key.
Candidate ordering stops at that component; it must not continue into the
suffix or expose the logical collection as ordered. This is pinned by a
sort-retention test. Coordinate-safe PK names remain available for covering
row reconstruction; the separate duplicate-producing signal suppresses their
use as an ordering suffix.

The same congruence rule applies to the complete secondary entry key. An
equality-fixed index root does not make an appended FLOAT/DOUBLE PK globally
logical-order-compatible: raw negative NaNs precede finite keys physically,
while the query comparator places every NaN greatest. Appended PK ordering
therefore stops at the first FLOAT, DOUBLE, or Unknown coordinate. A real-FDB
regression writes negative-NaN, finite, both signed-zero, and positive-NaN
DOUBLE primary keys under one STATUS value and requires an InMemorySort for
ASC, DESC, and LIMIT plans.

Multi-aggregate intersection is likewise a physical merge. It is eligible only
when every child streams the complete contiguous grouping key in comparator-
congruent order. A permuted layout, or an unbound FLOAT/DOUBLE/Unknown grouping
coordinate, declines the intersection and falls back to base aggregation.

Known physical key type overrides an Unknown RHS in this classification. A
known INT/LONG key remains fixed for every successful equality bind because a
plural float inverse fails before storage. A known FLOAT/DOUBLE key with a
dynamic or Unknown RHS is conservative. A genuinely Unknown physical key type
is not SARGed; execution never reconstructs it from the RHS.

### 8. Price every extra physical seek and weaken unsafe row proofs

Use two related but distinct quantities:

1. successfulSeekUpperBound: for executable non-NaN bindings, the checked
   product of at most two sign choices per maybe-expanding component, reduced
   when terminal widening is proven;
2. provenRowMultiplicity: finite only for a fully known non-NaN equality bind;
   dynamic float/Unknown binds are unknown because NaN is in their logical
   domain.

The static range-seek estimator is attached to every plural leaf, not only
fully bound UNIQUE/PK probes. Primary, non-unique value, partial value,
aggregate, permuted aggregate, and vector-partition plan-local costs all add:

~~~text
(successfulSeekUpperBound - 1) * PhysicalRangeSeekCost
~~~

to their existing scan cost. The first seek is already represented by the base
leaf cost. Arithmetic uses the same finite saturating helpers as existing
fan-out formulas: overflow clamps cardinality and CPU at the repository's
largest finite heuristic values, preserving FuzzCostSanity and the total cost
preorder. Saturation strongly disfavors the access path without introducing
NaN or infinity into comparison. Execution itself stays lazy and uncapped.
Empty probes still carry seek cost even though they return no row and charge no
scanned-record unit.

When every vector partition column is bound, signed-zero fan-out multiplies the
existing self-limiting scan's row cardinality and per-row CPU by
successfulSeekUpperBound before adding seek setup. A zero partition equality
can return up to two independent top-k result sets, so it is costed as at most
2*k rows plus two graph-search setups, not as one k-row search plus one cheap
seek. Multiple expanding bound components scale by their checked, finitely
saturated product. A partial partition prefix still fans out over an unknown
number of remaining partitions; its row cardinality stays unknown/conservative
and must not be advertised as N*k merely because the signed-choice multiplier
N is known. Its finite heuristic cost saturates or uses the existing
multi-partition estimate, then adds the signed-choice setup contribution.

For a full equality bind:

- a primary or UNIQUE index with only known non-NaN constants has a proven max
  equal to the product of its physical equality multiplicities;
- the same bind is a point probe only when the product is one;
- any dynamic FLOAT/DOUBLE or Unknown component has unknown max cardinality;
- non-unique indexes retain unknown max cardinality.

Plan cardinality, plan-local cost, logical cost, and concrete cost use the same
shape helper. Exact constant-zero unique/PK probes are costed as a small set of
seeks/rows rather than table-cardinality selectivity. No one-row shortcut is
allowed merely because the logical predicate is equality.

## NaN boundary and booked follow-up

The signed-zero theorem is:

~~~text
for every non-NaN physical key:
  key is in exactly one emitted range
    iff
  the logical comparison is TRUE
~~~

It is intentionally not stated for NaN. The tuple codec preserves many NaN
payloads while logical comparison canonicalizes them, so a one-payload exact
probe is false. This change implements both safeguards:

- ordinary value/primary matching does not SARG a visible constant NaN and
  retains the logical predicate as compensation; access paths such as aggregate
  or self-limiting vector scans that cannot compensate are ineligible;
- the runtime binder returns UnsupportedPhysicalFloatEquivalenceError for a
  parameter or correlation that evaluates to NaN, before any child opens.

NaN is also a property of stored keys, not only of the RHS. Therefore every
ordinary ordered FLOAT/DOUBLE SARG is declined/residualized even for a finite
constant threshold, and an already chosen physical plan fails typed with
`UnsupportedPhysicalFloatOrderingError`. Restricting the theorem to non-NaN
candidate rows would not be sufficient: a broad physical range that returns a
raw negative NaN is itself a false-positive result.

The no-index predicate path continues to evaluate NaN according to existing Go
semantics. road-to-prod.md records the remaining feature: exact indexed NaN
equivalence for composite keys, which requires either a residual-capable broad
NaN-prefix scan or a physical normalization decision. Until that work lands,
the index path is never silently incomplete.

## Alternatives rejected

**One broad signed-zero interval.** It admits unrelated suffix keys.
Adversarial flank rows make this fail.

**Rewrite equality to IN (-0,+0).** IN elements are a logical set under Go
numeric equality, and preserving both signs there can duplicate residual or
unnested execution. Physical branching belongs below logical equality.

**Terminate the prefix and residual-filter every possible float.** Correct but
turns every correlated composite float probe into a scan of both entire zero
groups. It is RFC-196's constant stopgap, not the desired access path.

**Canonicalize keys on write or in the tuple codec.** This breaks shared wire
compatibility, existing data, Java-written indexes, and settled physical
identity behavior.

**Executor-only branching.** This leaves ordering, uniqueness, cardinality, and
cost claiming facts the executor no longer satisfies.

**Nested ConcatCursors.** They compose behaviorally, but captured tuple prefixes
and nested continuation messages do not provide a linear space/serialization
proof.

**Materialize all ranges or impose a maximum.** The former permits exponential
memory before the first row; the latter turns a valid query into an arbitrary
failure. One flat odometer is exact with linear live state.

**Pretend NaN has two exact ranges.** Its two sign regions contain many payload
prefixes; adding a constrained suffix makes the wanted set non-contiguous.
Without an explicit residual layer, two broad ranges return false rows.

## Measured corpus reconciliation

**There is no corpus reconciliation. No committed plan point moves, so nothing
is retired, re-blessed or added, and this branch ships no retirement ledger.**

That is the second answer this section has carried, and the first one was wrong
in a way worth recording, because the mistake is reproducible by anyone holding
the same tool.

An earlier revision measured 156 of 5000 committed scenarios changing plan
shape, rewrote all 156 provenance headers to match whatever the new planner
produced, and recorded the change in a retirement ledger authorizing 36 file
replacements. Every gate went green. They went green because the expectations
had been moved to wherever the planner landed.

What the re-bless had absorbed was a regression. Dumping `explaindiff.ShapeOf`
plus `Explain()` for all 5000 scenarios on both sides showed the direction the
aggregate count concealed:

- 156 scenarios differed, in 40 distinct shape-transition classes;
- 148 of them LOST at least one `IndexScan`;
- 4 kept their index count but gained a blocking `InMemorySort`;
- scenarios that GAINED an access path: **zero**.

Every moved point was a strict access-path downgrade — `Fetch(IndexScan(IDX_E))`
becoming `Scan(T_RD)`, a float index serving ordering only being replaced by a
full scan under an in-memory sort, a `FlatMap` outer leg going from
O(selectivity·N) index entries to O(N) records. And 39 of the 156 bought no
correctness at all: a two-sided finite range on a float key is exactly sound as
one physical range, because no NaN is logically inside a finite interval.

The cause was two gates that keyed on the column's TYPE instead of on the
scan's CAPABILITY, described in §3. Once an ordered float comparison is
represented as the exact range set this RFC already builds — one range for
`<`/`<=`, two for `>`/`>=`, four blocks for an ordering-only coordinate — the
access paths come back, and the corpus agrees with the parent commit without
anything being rewritten.

**The lesson, stated plainly because it generalises beyond this RFC: any tool
that re-blesses expectations to match observed behaviour converts a regression
into a green test.** This repository now contains such a tool
(`cmd/factory-rebless-plan-shapes`). Its guard rails are therefore part of its
contract, and they are narrow by construction: it recomputes the candidate, all
four TLP renderings, the schema template, the setup and every frozen result row,
and treats ANY difference in them as an error rather than as something to write
down. Rows are oracle output. A planner change that alters them is a bug report,
not a re-bless. The tool is retained for the next legitimate plan-shape
transition; what is not retained is a ledger for a transition that did not
happen, which would be a durable false record in an audit trail that only has
value if every entry in it is true.

Java 4.12.11.0 was also exercised as a prospective second-engine oracle rather
than assumed authoritative. It could not plan the representative
`ORDER BY E NULLS FIRST, ID` shape; in the seed-19 probe it rejected 8 of 11
plans and the remaining 3 exposed genuine row-semantic divergences. Those
results reinforce the source-level finding above: Java provides neither this
multi-range implementation nor one stable signed-zero contract. No Java result
was therefore mislabeled as a cross-engine blessing in this reconciliation.

## Verification gates

Every new Go test calls t.Parallel where isolation permits. Storage behavior is
tested against real FDB; only the repository's Docker-availability guard may
skip.

### Range algebra and executor units

- authoritative coercion for FLOAT, DOUBLE, known non-float, typed RHS, and
  Unknown RHS;
- missing/Unknown physical metadata rejects every non-null carrier before
  storage while retaining the explicitly enumerated NULL-only semantics;
- INT/LONG runtime-float inverse projection over the full signed int64 tuple
  carrier, including uint32 values above MaxInt32, both extrema, 2^53 plural
  equality classes, fractions, infinities, NaN, and every ordered operator;
- repeated FLOAT/DOUBLE lower and upper bounds in both insertion orders,
  inclusive/exclusive ties, signed-zero endpoints, and contradictions;
- every non-NULL ordered FLOAT/DOUBLE threshold fails typed before storage,
  while ordered NULL remains empty and IS NOT NULL retains all raw NaN keys;
- malformed comparison slices with a gap, equality after an inequality, or a
  second constrained component fail typed before binding;
- every unsupported range-tail operator, missing binary operand, malformed
  unary operand, and mixed STARTS_WITH tail fails typed before numeric
  projection; no malformed comparison can collapse into a silent empty scan;
- STARTS_WITH admits only an authoritative STRING carrier; LONG,
  DATE/TIMESTAMP, BYTES, floating, boolean, and UUID carriers fail typed before
  storage, while a NULL comparand remains the type-independent empty result;
- exact packed endpoints for both input zero signs;
- zero in leading, middle, and terminal positions, with fixed prefixes;
- suffix equality, inequality, STARTS_WITH, empty, IS NULL, and null-safe
  equality;
- two zero equalities and all four sign combinations;
- nonzero, negative nonzero, subnormal, infinities, ordinary NULL, binder
  errors, constant NaN rejection, and dynamic NaN typed error;
- forward -0,+0 and reverse +0,-0 range order;
- exhaustive small non-NaN tuple universe proving membership iff logical truth
  and exactly-one-range disjointness;
- large zero-component specs proving O(k) evaluated state, tuple workspace, and
  range-set continuation framing before any row is requested; total serialized
  size additionally includes the active leaf token.

### Range cursor state machine

- empty set does not panic and opens no factory;
- malformed, wrong-fingerprint, wrong-arity, and out-of-bounds continuations
  fail before storage;
- execution fingerprints include the complete structural scan identity,
  including the aligned primary-key coverage columns; a token minted for an
  otherwise-identical plan with different PK coverage fails before storage;
- SourceExhausted is the only branch-advance reason;
- row, scan, byte, and time stops inside the first branch, exactly between
  branches, inside the second branch, and after an empty first branch;
- nil ScanState is normalized once and all child properties share its pointer;
- fail and paginate modes cannot receive one free pass per branch;
- factory, child OnNext, and child Close errors do not advance;
- Close is idempotent, active-only, once, and latches after a close error;
- forward/reverse continuation sweeps compare paged output with unpaged output
  under a hard progress cap: no gap, duplicate, reorder, restart, or limit
  reset.

### Real access paths

- flip correlated_zero_composite_sentinel_test.go into a correctness test;
- both outer sign directions, plus nonzero, NULL, and NaN bindings through one
  correlated plan;
- adversarial (-0,9) and (+0,1) flank rows around suffix 5;
- FLOAT and DOUBLE; literal, parameter/COALESCE, and correlated values;
- (v,w), (g,v,w), (v1,v2,w), and composite float primary keys;
- suffix equality, inequality, and IS NULL;
- UNIQUE (v,w) containing both (-0,5) and (+0,5) returns two rows; a scalar
  subquery over it raises SQLSTATE 21000;
- covering and non-covering value scans and aggregate scans;
- permuted MIN/MAX with permutedSize zero; positive permutedSize declines to
  base aggregation, and an illegal cross-boundary physical plan fails before
  storage;
- partitioned vector indexes with FLOAT and DOUBLE prefixes, both signs,
  per-physical-partition top-k, branch continuation, and no false generic
  ordering;
- vector partition decoding uses the same structural PK proof as planning and
  is exercised with primary key (partition, id), so legal partition/PK overlap
  works in fresh and paged scans without guessing a flattened key topology;
- indexed output equals the no-index/reference path for supported non-NaN
  values, while EXPLAIN proves the composite index and suffix are used;
- known NaN chooses the reference path; dynamic NaN fails before storage;
- a real STATUS index over a DOUBLE PK containing negative/positive raw NaNs,
  finite values, and both zero signs retains Sort for ASC/DESC and before LIMIT,
  and matches the no-index logical order;
- IN (-0,+0), reversed, and repeated forms return each row exactly once.

### Planner agreement and mutation proof

- constant zero/nonzero/NaN, typed dynamic, Unknown RHS, and known physical key
  type combinations;
- physical fixedness is separate from logical equality in candidate, plan,
  intersection, and rich ordering;
- suffix ORDER BY LIMIT retains Sort after an expanding equality;
- duplicate-producing component ordering stops at an expanding equality;
- every plural leaf pays extra seek cost, including empty non-unique, partial,
  aggregate, and vector probes; overflow saturates finitely;
- fully bound vector multiplicity scales top-k row cardinality and per-row CPU
  as well as graph-search setup; partial partition prefixes remain unknown;
- full unique/PK constant binds get exact finite multiplicity; dynamic float
  binds remain unknown and decline one-row shortcuts;
- a partial equality prefix of a composite UNIQUE index remains a range cost,
  while only equality over every authoritative index column gets the point-
  probe cardinality/CPU shortcut;
- secondary appended-PK types remain coordinate-aligned through middle trims;
  leading RecordTypeKey is fixed only for single-type indexes; shared, hidden,
  nested, function, literal, version, and width-disagreeing shapes fail closed;
- unbound/partial aggregate candidates preserve the full physical grouping
  type vector, and multi-aggregate merge declines raw FLOAT/DOUBLE/Unknown
  grouping order;
- plan identity/determinism repeated ten times;
- mutations removing expansion, keeping one sign, broadening one interval,
  dropping the suffix, failing reverse order, resetting continuation, omitting
  FLOAT, reviving the non-terminal point proof, skipping vector partitions,
  granting per-child free passes, continuing fan-out ordering, omitting PK
  coverage from continuation identity, or letting malformed tails reach
  numeric projection each make a committed proof red.

Finally run focused tests, relevant fuzz/property tests, race coverage, Gazelle,
the repository's full just test, and the required 1M-row before/after stress
comparison for the planner/executor change. Record exact commands, timings,
disk conditions, and results in the PR; no flake is waived.

## Implementation verification

A development checkpoint before the final planner and history-gate hardening
passed all of the following on 2026-08-01. These are historical measurements,
not the final-tree claim; the source-frozen verification is recorded below:

- deterministic generation, Gazelle, gofumpt/nogo lint, docscheck, and an
  all-target Bazel build;
- the complete non-stress repository suite: 75/75 Bazel test targets in
  469.732s, including real FDB, full RFC-201 corpus, row differential, Java
  conformance, SimFDB, and factory canonicality;
- race instrumentation over executor, plans, cascades, and embedded;
- four 60-second continuation/range-set fuzzers with 1,250,688 aggregate
  executions: 262,945 branch advances, 235,640 malformed-token parses, 385,882
  continuation round trips, and 366,221 paged sweeps;
- the factory determinism/canonical audit over all 5,000 committed scenarios,
  plus the content-exact retirement-ledger Go and Bazel runfiles gates and the
  Git-history gate that verifies the ledger's old side at `base_commit`; and
- the 1M-row FDB stress scale. The pre-change run took 224.48s (100k customer
  ingest 12.398s; 1M three-index order ingest 191.297s). The final run took
  165.21s (9.294s; 138.105s respectively), and every point/range/aggregate/
  join/full-scan/IN/update/delete assertion passed. The observed improvement is
  not treated as a benchmark guarantee; it proves there is no measured
  regression on the required scale.

The host was already under unrelated multi-worktree Bazel load. One
unconstrained build server was killed with Bazel exit 37 and no compiler/test
diagnostic. The gate was rerun from start with four jobs and passed, followed by
the complete four-job test suite. This is recorded as an infrastructure retry,
not waived as a test flake.

### Source-frozen verification

The implementation source was frozen at
`79466077b6499e75ce36c2e68875e28280bcff95` on 2026-08-02. The only subsequent
tree edits were this verification record, a precision correction in
`DIVERGENCES.md`, and the stress recipe's aggregate timeout; no Go, protobuf,
planner, executor, or corpus bytes changed. That source passed:

- two consecutive `just generate` runs with byte-identical diff and status
  fingerprints, followed by `just lint`, `just build`, and pre-commit's complete
  generate/lint/build/test chain;
- `go run ./cmd/verify-corpus-retirement-history -trusted-ref origin/master`,
  authenticating BEFORE at `51d9e9701bbcb959ae09e472fa9e6bb2c9e84169`,
  first-add AFTER at `97a49228d019df009e33c7891e04853ab9d98625`,
  and raw proposed HEAD at the source-frozen commit;
- the focused executor/planner/embedded Bazel matrix (4/4 targets and 6,032
  reported cases), the focused signed-zero/NaN/index-state real-FDB matrix (73
  cases), and the governance/history/docs matrix (4/4 targets), all with zero
  failures;
- `just test`: 76/76 non-stress targets in 88.727s; and the exact CI-tagged
  command, including bindingtester and Docker-required SimFDB differential:
  77/77 targets in 64.699s with 11,470 reported passing and zero reported
  failing/skipped cases (some targets do not publish case metadata);
- race-mode Bazel scopes: cascades 7/7 in 36.906s, all relational targets 25/25
  in 488.528s (including the full committed corpus), and FDB
  client/transport/API targets 5/5 in 181.597s;
- four 60-second continuation/range-set fuzzers with 2,844,802 aggregate
  executions: 346,631 branch advances, 571,202 malformed-token parses, 952,133
  continuation round trips, and 974,836 paged sweeps; and
- the complete real-FDB stress target after its aggregate timeout was corrected
  from 600s to 1,800s: 12 suites / 114 cases, zero failures/skips, in 718.851s.
  The 1M SQL suite passed in 156.46s (100k customer ingest 7.385s; 1M
  three-index order ingest 131.470s), and both vector proofs passed (cold-start
  88.86s; streaming heap bound 237.95s).

The first stress invocation had already passed the raw/record-layer scaling and
all 10K/100K/1M SQL assertions when the old 600s **target-wide** budget expired
during the final vector proof. Because that target contains the entire stress
family rather than one scale point, the stale recipe was corrected to match its
existing Bazel `eternal` classification and the whole target was rerun from
start. The passing rerun, rather than the partial first run, is the evidence.
The host had 21GiB free on a 98%-used `/home` filesystem and 32GiB free in
`/dev/shm`; no failure was waived for disk pressure or concurrent load.

## Review disposition

The independent post-rebase Graefe review returned **ACK**: the scoped non-NaN
equivalence/loud-NaN boundary, suffix SARGability, physical fixedness,
constant-only finite row proofs, seek multiplicity, full-key UNIQUE/PK proof,
PK topology, aggregate ordering, and primary/value/aggregate/permuted/vector
consumer coverage all agree.

The independent post-rebase Torvalds review returned **ACK**: evaluated and
serialized state are O(k) and O(k + |inner|), one normalized ScanState owns the
whole range set, continuations validate before storage, only SourceExhausted
advances a branch, forward/reverse scans agree, and empty/error/close/overflow/
unopened-factory behavior is pinned.

The independent corpus review also returned **ACK**: both census hashes, both
complete corpus-tree hashes, all 54 changed-file entries, and all 27 replacement
semantics match; no unlisted corpus byte changed.
