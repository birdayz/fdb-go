// Portions derived from FoundationDB Record Layer (
// ValueIndexLikeMatchCandidate.java, OrderingProperty.java,
// AggregateIndexMatchCandidate.java, RecordQueryIndexPlan.java, and others),
// Copyright 2015-2018 Apple Inc. and the FoundationDB project authors
// Copyright 2015-2021 Apple Inc. and the FoundationDB project authors
// Copyright 2015-2022 Apple Inc. and the FoundationDB project authors
// Licensed under the Apache License, Version 2.0; translated to Go and modified.

package plans

import (
	"math"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// Output ordering, owned by the plans themselves (RFC-183 P5).
//
// Two shapes live here:
//
//   - PRODUCERS advertise an order derived from their own state (an index
//     scan's key columns, a sort's sort keys, a merge union's comparison
//     keys). They answer properties.OrderingHinter directly.
//
//   - DELEGATORS preserve their input's order rather than producing one
//     (filter, projection, limit, fetch, …). They expose OrderingSourceRef so
//     ordering satisfaction and extraction-time sort elision can resolve
//     through the SOURCE, and their HintOrdering inherits from it.
//
// A delegator follows its child reference. Constructors accepting a quantifier
// retain the live memo group; constructors using QuantifierOverPlan create a
// singleton reference over the concrete child. Ordering must follow the actual
// reference rather than assume either shape.
//
// Bare plans can be memo expressions themselves (RFC-184 W2), so both plain and
// rich hint methods are live dispatch surfaces. The parity tests in
// cascades/plan_rich_ordering_parity_test.go check the rich derivations.
//
// WHICH PRODUCERS ASK THE ORDERING-CLAIM PREDICATE
//
// A producer's claim is that the PHYSICAL order it hands rows back in equals
// the LOGICAL order the comparator imposes. For a FLOAT/DOUBLE coordinate those
// two differ (values/ordering_claim.go), so a producer whose order comes from
// FDB KEY layout has to ask:
//
//   - ASK, because their order IS the tuple-key order:
//     RecordQueryScanPlan.HintOrdering (via PKScanOrdering),
//     RecordQueryIndexPlan.HintOrdering, both of their HintRichOrdering forms,
//     RecordQueryStreamingAggregationPlan.HintOrdering, and both forms of
//     RecordQueryAggregateIndexPlan (HintOrdering and HintRichOrdering, through
//     splitKeyOrder). The last three claim GROUP order, which is the group
//     column's key order and therefore the same hazard one level up.
//
//   - DOES NOT ASK, and must not: RecordQueryInMemorySortPlan.HintOrdering. It
//     restates the keys it sorted BY, with the comparator. Its claim is true by
//     construction and truncating it would delete the one plan that repairs
//     the others.
//
//   - DOES NOT ASK: RecordQueryInJoinPlan. Its leading key is the IN value in
//     the order its source was sorted with the comparator (sortInJoinValues),
//     and the rest restates its inner's claim, which asked.
//
//   - DO NOT ASK, because a float cannot reach them:
//     RecordQueryMergeSortUnionPlan, RecordQueryIntersectionPlan,
//     RecordQueryInUnionPlan and RecordQueryMultiIntersectionOnValuesPlan all
//     restate a COMPARISON KEY they were built with, and a comparison key is
//     derived from the legs' provided orderings — which now terminate at the
//     float, so no common ordering containing one exists and the merge is never
//     built. That is an EMERGENT property, not a guard, so it is pinned rather
//     than asserted: embedded_test.TestFloatNeverReachesAMergeComparisonKey
//     shows the identical shape still building an Intersection over an INTEGER
//     coordinate and falling back to a materialized sort over a DOUBLE one.
//
//   - Produce no ordering at all: every body under "--- unordered ---" below,
//     and RecordQueryVectorIndexPlan.HintRichOrdering.

// equalityPrefixLen returns the length of the leading equality-bound prefix
// in comps, capped at n key positions, read from the operand alone. It is what
// the PRIMARY-KEY scan's two ordering derivations consult (PKScanOrdering
// drops this prefix from the ordering keys, RecordQueryScanPlan.
// HintRichOrdering retains it as FixedBinding entries), so the two cannot
// classify a column differently by one caller's loop breaking where the
// other's does not. The index and aggregate scans, whose coordinates carry
// physical key types, ask the column-aware per-coordinate form instead
// (splitKeyOrder's pins).
//
// Mirrors Java's ValueIndexLikeMatchCandidate.computeOrderingFromScanComparisons,
// whose equality prefix is scanComparisons.getEqualitySize(): a leading run
// of equality entries, ending at the first entry that is not an equality (an
// inequality, an unbound/empty range, or simply running out of comps).
//
// An equality comparison that reappears AFTER a non-equality gap does NOT
// resume the prefix — this is the conservative reading, chosen deliberately:
// a gap comparison (e.g. index (a, b, c) SARGed a = 1, b > 5, c = 3) already
// breaks the contiguous-range guarantee a scan provides. Rows are ordered by
// (a, b, c) as a whole; fixing c to a single value only holds true WITHIN
// each individual (a, b) sub-range the scan visits, not across the scan as a
// whole the way a genuine leading equality prefix does (every row shares the
// same a because a is bound before any range is opened). Classifying c as
// FIXED would tell a caller that any direction on c is safe everywhere in
// the stream, which breaks the moment b's bound range spans more than one
// distinct b value. So a resumed equality stays an ordinary ordering key —
// Sorted in the rich form, retained as a key in the plain form — despite
// testing equal at its position.
//
// This shape is unreachable through the sole production constructor today
// (ValueIndexScanMatchCandidate.ComputeBoundParameterPrefixMap always stops
// at the first inequality or unbound parameter, so it never emits an
// equality past a gap), but the helper defines it anyway rather than leaving
// it to whichever caller's loop happens to run first.
func equalityPrefixLen(comps []*predicates.ComparisonRange, n int) int {
	prefix := 0
	for i := 0; i < n && i < len(comps); i++ {
		if !EqualityPinsSinglePhysicalKey(comps[i]) {
			break
		}
		prefix = i + 1
	}
	return prefix
}

// ownOrderPrefixLen is equalityPrefixLen for the SELF question: how many
// leading coordinates are FIXED by an equality, counting one that spans both
// signed zeros. Always >= equalityPrefixLen.
//
// The gap between the two is load-bearing. When ownOrderPrefixLen runs past
// equalityPrefixLen, the extra coordinates still carry their OWN order — the
// range set enumerates their blocks in key order, so a sort on any of them is
// satisfied in the scan's direction (NOT because they admit a single logical
// value: a signed-zero equality admits two distinct sort values, see
// EqualityBoundCoordinateClaimsOwnOrder) — but one of them covers more than a
// single physical key, so the SORTED tail after that prefix is not globally
// ordered and must be dropped entirely. Callers
// express that by claiming the fixed prefix and emptying the tail, never by
// claiming a shorter fixed prefix: shortening it would throw away a sound
// claim, which is what cost `ORDER BY <signed-zero-bound float>` its
// index order.
func ownOrderPrefixLen(comps []*predicates.ComparisonRange, n int) int {
	prefix := 0
	for i := 0; i < n && i < len(comps); i++ {
		if !EqualityBoundCoordinateClaimsOwnOrder(comps[i]) {
			break
		}
		prefix = i + 1
	}
	return prefix
}

// IndexColumnCouldBeFloat resolves each index key column against the scan's
// flowed record layout and reports whether it can hold a signed zero. It is the
// per-position input splitKeyOrder's pins need, and the cascades-side match
// candidate asks the SAME function for its matched ordering parts
// (ValueIndexScanMatchCandidate.ComputeMatchedOrderingParts), so the two
// derivations cannot classify a coordinate differently. It delegates to
// values.ColumnCouldBeFloat so the float classification stays in the one file
// that owns it.
//
// It asks that predicate rather than NEGATING ColumnCanExtendOrderingClaim, and
// the difference is not cosmetic. The two disagree exactly when the layout
// cannot resolve the column, which happens on real plans — NewRecordQueryIndexPlan
// defaults a nil flowedType to UnknownType and AggregateIndexMatchCandidate
// passes UnknownType explicitly, neither of which is a *RecordType. Negating the
// permissive answer there yields "not a float" -> "pins" -> assume-sound, which
// makes this whole fix INERT on such a plan. ColumnCouldBeFloat fails closed
// instead; see its comment for why a burden-of-proof direction cannot be
// inverted.
func IndexColumnCouldBeFloat(
	keyTypes []values.Type, layout values.Type, columnNames []string, duplicateColumns []bool,
) func(int) bool {
	return func(i int) bool {
		if i < 0 || i >= len(columnNames) {
			// No such coordinate: there is nothing to widen.
			return false
		}
		// The PHYSICAL key component type is the authority and is asked first:
		// it is aligned with scanComparisons positionally, so it needs no name
		// resolution and it is present on plans whose flowed layout is not a
		// record at all. Those plans are the ones that made the layout-only
		// reading inert — NewRecordQueryIndexPlan defaults a nil flowedType to
		// UnknownType and AggregateIndexMatchCandidate passes UnknownType
		// explicitly — while WithKeyComponentTypes still carries the real
		// coordinate types.
		if i < len(keyTypes) && keyTypes[i] != nil &&
			keyTypes[i].Code() != values.TypeCodeUnknown {
			return values.TypeTerminatesOrderingClaim(keyTypes[i])
		}
		// An exploded element is not the root field with the same name.
		if i < len(duplicateColumns) && duplicateColumns[i] {
			return true
		}
		// No physical type for this coordinate: fall back to resolving the
		// column name against the flowed layout, which fails CLOSED when it
		// cannot answer. See values.ColumnCouldBeFloat for why this must not be
		// written as the negation of ColumnCanExtendOrderingClaim.
		return values.ColumnCouldBeFloat(layout, columnNames[i])
	}
}

// EqualityPinsSinglePhysicalKey reports whether cr binds its coordinate to ONE
// physical key — the property that makes the coordinate FIXED rather than
// sorted, so it claims no order of its own and the columns after it remain
// claimable.
//
// This is the SINGLE AUTHORITY on that question. Two callers derive an
// ordering claim from a key-column sequence and must agree column for column:
// equalityPrefixLen here on the plan side, and the sargable candidates'
// ComputeMatchedOrderingParts on the cascades side. A second hand-rolled copy
// of the rule is how the two derivations drift apart and classify the same
// column differently — which is exactly the defect this consolidation removes,
// where one side exempted an equality-bound float and the other terminated on
// it, costing every affected query an index intersection or a materialized
// sort it did not need.
//
// An equality is NOT enough on its own. A zero-valued FLOAT/DOUBLE equality
// pins no single key: the executor widens it to span both signed zeros (-0.0
// and +0.0 are IEEE-equal but pack to distinct adjacent keys), so the scan
// covers TWO physical prefixes and every column after it RESETS at the
// boundary.
//
// Counting that as an equality would claim the suffix is globally ordered and
// let the planner drop a required sort: over rows (-0.0, 9) and (+0.0, 1),
// `WHERE v = 0 ORDER BY w` would return [9 1] unsorted, and `... LIMIT 1` the
// wrong row entirely.
//
// Such a range is treated like an inequality — it does not pin, so the prefix
// stops there. A non-equality leading comparison already trims nothing, so `v`
// stays in the ordering (it IS ordered: -0.0 sorts immediately before +0.0)
// while the suffix no longer claims an order the scan does not provide.
// A nil range is an ABSENT binding, not an equality — it pins nothing. The
// check is here rather than at the call sites because one of them reads a
// parameter-binding map, where a miss yields nil and IsEquality would panic.
func EqualityPinsSinglePhysicalKey(cr *predicates.ComparisonRange) bool {
	return cr != nil && cr.IsEquality() && !isZeroFloatEqualityRange(cr)
}

// EqualityPinsSinglePhysicalKeyOnColumn is EqualityPinsSinglePhysicalKey for a
// caller that knows the INDEXED COORDINATE's type — the one fact the range
// alone cannot supply, and the one couldBeFloatOperand documents as the right
// discriminator it did not have.
//
// The operand's declared type is not a usable proxy for it. An operand carries
// FLOAT only when the literal was coerced to the column; an IN-list binding
// reaches here as an UNKNOWN-typed correlation, and couldBeFloatOperand answers
// "not a float" for it. On an INT column that answer is right and load-bearing
// (it is what keeps an untyped IN-join binding's ascending claim). On a FLOAT
// column it is wrong, and wrong in the unsound direction: the binding can be
// zero at runtime, the executor widens the probe across both signed-zero
// blocks, and the coordinate pins no single key.
//
// Splitting the question by COLUMN type keeps both answers: an int coordinate
// always pins (it has no signed zero to widen, whatever the operand), and a
// float coordinate pins only when the operand is a constant that is provably
// nonzero.
//
// MEASURED, this is the whole of the InUnion defect. `e IN (5, 7, 0)` over a
// FLOAT column plans InUnion over a per-binding leg that advertises PK order.
// The zero binding widens at runtime to the -0.0 and +0.0 blocks, so that leg
// emits its +0.0 rows only after all of its -0.0 rows; the ordered merge reads
// one row of lookahead per leg, so those rows land after the entire result.
// `ORDER BY id` returned [4 17 20 26 29 30 32 42 45 47 69 103 119 14 80] —
// sorted except for the two +0.0 rows, 14 and 80, appended at the end.
//
// This DIVERGES from Java, and the divergence is systematic rather than a patch
// over one Java slip. Java derives its ordering prefix from
// scanComparisons.getEqualitySize() with no signed-zero exemption anywhere, in
// four places built the identical way (4.12.11.0):
//
//	ValueIndexLikeMatchCandidate.java:166
//	WindowedIndexScanMatchCandidate.java:363
//	VectorIndexScanMatchCandidate.java:345
//	AggregateIndexMatchCandidate.java:340
//
// all of them `for (i = scanComparisons.getEqualitySize(); i < …; i++)`. So Java
// makes the same unsound claim on every one of those candidate kinds, and the
// query above reproduces against it. Citing a single site would read as a local
// bug worth patching around; four identical sites say Go is knowingly declining
// a Java-wide behaviour, which is the claim DIVERGENCES.md has to carry.
func EqualityPinsSinglePhysicalKeyOnColumn(cr *predicates.ComparisonRange, columnCouldBeFloat bool) bool {
	if cr == nil || !cr.IsEquality() {
		return false
	}
	if !columnCouldBeFloat {
		// No signed zero exists on this coordinate, so no widening can occur
		// whatever the operand's declared type is.
		return true
	}
	// A float coordinate pins only on a provably-nonzero CONSTANT. A
	// non-constant operand (parameter, correlation, or an IN binding) could be
	// zero at runtime and would widen, so it does not pin.
	cmp := cr.GetEqualityComparison()
	if cmp == nil || cmp.Operand == nil {
		// An equality range carrying no operand is how `IS NULL` arrives. NULL is
		// not a zero and has no signed twin: it packs to ONE distinct tuple key
		// below every number, so the coordinate is genuinely pinned and the
		// suffix after it stays ordered.
		//
		// Treating this as "not provably nonzero" would put a materialized sort
		// back on every `<float> IS NULL … ORDER BY <pk>`. The widening this
		// function guards is driven by a zero VALUE; a range with no value cannot
		// trigger it.
		return true
	}
	if !values.IsConstantValue(cmp.Operand) {
		return false
	}
	v, ok := values.EvaluateConstant(cmp.Operand)
	if !ok {
		// Not evaluable here, so it cannot be shown nonzero.
		return false
	}
	if v == nil {
		// A NULL-VALUED constant operand — the other spelling of `IS NULL`,
		// where the comparison carries a null Value rather than no comparison at
		// all. Same conclusion as the nil-comparison arm above and for the same
		// reason: NULL is not a zero, has no signed twin, and packs to ONE key.
		//
		// Kept as its own arm rather than folded into the "not provably nonzero"
		// return because that is precisely how the nil-comparison spelling
		// regressed once already, and the two spellings must not diverge. No
		// generated seed in the ordering sweep's range reaches this one, so a
		// sweep that stays green is not evidence it is unreachable.
		return true
	}
	// Any NUMERIC constant settles it, not just a float-typed one: an integer
	// literal against a float column is coerced before the probe is built, so
	// `e = 5` pins exactly as `e = 5.0` does, and `e = 0` widens exactly as
	// `e = 0.0` does. Judging on the VALUE rather than the declared type is the
	// same discipline isZeroFloatEqualityRange applies, and for the same
	// reason: the executor decides to widen from the runtime value.
	//
	// A NaN does not pin either: every NaN is one logical value, but the
	// executor probes the two NaN key blocks (negative NaNs below -Inf,
	// positive above +Inf), so the suffix after the coordinate is ordered
	// within each block and not across them (RFC-257 WS-E 5.3).
	switch n := v.(type) {
	case float64:
		return n != 0 && !math.IsNaN(n)
	case float32:
		return n != 0 && !math.IsNaN(float64(n))
	case int64:
		return n != 0
	case int32:
		return n != 0
	case int:
		return n != 0
	}
	// A constant this function cannot read as a number — it cannot be shown
	// nonzero, so it does not pin.
	//
	// This is also what makes the two NULL arms above safe against a THIRD
	// spelling nobody has found, and the argument is structural rather than a
	// hope that the enumeration is complete. Any further way to say NULL would
	// have to be a constant that IsConstantValue accepts and EvaluateConstant
	// returns with ok=true and a NON-nil value that nonetheless means NULL. Such
	// a value misses every numeric case and lands exactly here, on `return
	// false` — it does NOT pin, so the claim is refused and a sort is kept.
	//
	// The asymmetry is the point: an unrecognised operand costs a materialised
	// sort that was not needed, never a row in the wrong order. The unknown
	// degrades toward latency by construction, which is the direction this
	// predicate must fail in.
	return false
}

// EqualityBoundCoordinateClaimsOwnOrder answers a DIFFERENT question from
// EqualityPinsSinglePhysicalKey, and the two must never be substituted for one
// another — conflating them is precisely the defect this pair replaces.
//
//	EqualityPinsSinglePhysicalKey: may LATER coordinates claim order THROUGH
//	                               this one? (the SUFFIX question)
//	this predicate:                may THIS coordinate claim ITS OWN order?
//	                               (the SELF question)
//
// A signed-zero float equality answers NO to the first and YES to the second.
// It answers no to the first because it spans two physical keys, so a later
// coordinate restarts at the block boundary: over rows (-0.0, 9) and (+0.0, 1),
// `WHERE v = 0 ORDER BY w` returns [9 1]. That termination is inviolable and
// stays exactly as it was.
//
// It answers yes to the second on ONE ground, the PHYSICAL ENUMERATION, and
// only that: the executor's range set opens the two signed-zero blocks in KEY
// ORDER and reverses them wholesale for a reverse scan, so the coordinate is
// genuinely ordered in whichever direction the scan runs. DESC and mid-scan
// resume rest on the same fact. Because the ground is an enumeration order and
// not an absence of order, the claim it supports is SORTED and DIRECTIONAL.
//
// Do NOT reason from tie-class vacuity here. The tempting alternative — "the
// equality admits one logical value, so any permutation satisfies ORDER BY,
// which settles ASC by itself" — is FALSE at this coordinate. It presumes one
// comparator. There are TWO, and they
// disagree on signed zeros BY DESIGN: predicates.Comparison.Eval checks IEEE
// equality, so -0.0 == +0.0 and both rows are admitted; values.CompareFloat64
// (faithful to java.lang.Double.compare, and to FDB tuple order) ranks -0.0
// BELOW +0.0. The admitted rows are therefore TWO distinct ORDER BY values, not
// one tie class. (The NaN tie class elsewhere in this file IS genuine — there
// CompareFloat64 canonicalizes every payload to one value, so both comparators
// agree. The reasoning is sound for NaN and unsound for signed zeros; the
// difference is which comparators agree, so it must be checked, never assumed.)
//
// The consequence is that the claim is DIRECTIONAL, never FIXED. A caller that
// records this coordinate as FIXED — order-free, hence satisfying any requested
// direction — elides the sort on `WHERE z = 0.0 ORDER BY z DESC` and answers it
// from a FORWARD scan, returning the zero blocks ascending. HintRichOrdering
// binds it SORTED for that reason, and
// TestFDB_SignedZeroEqualityDoesNotOrderThePKSuffix/bound_column_descending
// pins it; its ascending sibling stays green either way and proves nothing.
//
// NaN does not intrude here the way it does for an unbound or range-bound float
// coordinate: an equality against zero admits no NaN at all, so the two
// disjoint NaN blocks are simply out of range.
func EqualityBoundCoordinateClaimsOwnOrder(cr *predicates.ComparisonRange) bool {
	return cr != nil && cr.IsEquality()
}

// isZeroFloatEqualityRange reports whether cr is an equality against a
// COMPILE-TIME-CONSTANT zero FLOAT/DOUBLE — the shape the executor widens
// across both signed zeros.
//
// Constness is checked BEFORE evaluating. Evaluating an arbitrary operand with
// a nil context silently treats unbound parameters as NULL, so
// `v = COALESCE(?, 0.0)` would fold to zero and be misreported as a constant
// zero even when the runtime binding is nonzero.
func isZeroFloatEqualityRange(cr *predicates.ComparisonRange) bool {
	if cr == nil || !cr.IsEquality() {
		return false
	}
	cmp := cr.GetEqualityComparison()
	if cmp == nil || cmp.Operand == nil {
		return false
	}
	// Deliberately NOT gated on the declared type. The executor decides to
	// widen from the runtime VALUE (isZeroFloatBound), so gating here on a
	// declared FLOAT/DOUBLE would let an untyped literal that evaluates to 0.0
	// widen at execution while match-time reasoning never noticed -- the
	// property and the behaviour would disagree, which is the whole defect
	// class this guards. Match on the value's Go type, exactly as the
	// executor does.
	if !values.IsConstantValue(cmp.Operand) {
		// NOT constant — a parameter, correlated operand or ConstantObjectValue.
		// Its runtime value is unknown here, and if it binds to zero the
		// executor WILL widen. For ORDERING the safe answer is therefore the
		// conservative one whenever the operand could be a float: report true
		// so the suffix keeps no ordering claim.
		//
		// `WHERE v = ? ORDER BY w LIMIT 1` with ? bound to 0.0 otherwise loses
		// its sort and returns the wrong row — and a bound parameter is the
		// COMMON shape, far more common than a literal zero.
		//
		// Asymmetric with the SARGABILITY decision in match_candidate_index on
		// purpose, and the asymmetry is the point: there, being conservative
		// de-sargs a probe into a scan of the whole leading-column group — a
		// performance cliff on every correlated composite join. Here it costs
		// one sort. Different price, different answer.
		return couldBeFloatOperand(cmp.Operand)
	}
	v, ok := values.EvaluateConstant(cmp.Operand)
	if !ok || v == nil {
		return false
	}
	switch n := v.(type) {
	case float64:
		return n == 0
	case float32:
		return n == 0
	}
	return false
}

// couldBeFloatOperand reports whether a non-constant operand is DECLARED float,
// so a zero binding could widen the scan. Integers, strings and booleans have
// no signed zero and cannot.
//
// UNKNOWN deliberately returns FALSE, and that leaves a known hole. Treating it
// as "could be" is the strictly safer reading — a bound parameter often reaches
// the plan gates untyped — but it also swallows every untyped IN-join binding
// over an INT column, which costs those plans their ordering claim and their
// InJoin sorted-order optimisation for a signed zero that cannot exist there.
//
// The right discriminator is the INDEXED COLUMN's type — an int column has no
// zero to widen regardless of what the comparand is — and that is not available
// here; equalityPrefixLen sees only ComparisonRanges. Threading column types
// into ordering computation is the real fix and is tracked separately. Until
// then this covers the typed case and the untyped hole is pinned by a sentinel
// rather than left silent.
func couldBeFloatOperand(v values.Value) bool {
	t := v.Type()
	if t == nil {
		return false
	}
	switch t.Code() {
	case values.TypeCodeFloat, values.TypeCodeDouble:
		return true
	default:
		return false
	}
}
