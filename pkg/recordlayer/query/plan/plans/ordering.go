package plans

import (
	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/properties"
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
// WHY THE DELEGATOR BODIES DUPLICATE THE WRAPPERS' RATHER THAN DELEGATING
//
// Not mechanism: the two sides are byte-identical loops, both over
// AllMembers(). The difference is the PROVENANCE of the reference each one
// walks.
//
//   - A wrapper's quantifier ranges over a SHARED MEMO GROUP — the group the
//     wrapper was built over, holding every alternative exploration has
//     yielded into it. Walking its members asks "does any explored
//     alternative provide an order?"
//   - A plan's quantifier ranges over a FRESH SINGLETON: QuantifierOverPlan
//     mints a new FinalOfAtStage reference per child, so the set holds
//     exactly the one child plan that was put there. Walking its members asks
//     "what order does MY concrete child produce?"
//
// Same loop, different set, different question. Collapsing them would make
// one of the two questions unanswerable, which is why the memo keeps asking
// the wrapper.
//
// UNREACHABLE TODAY — DELIBERATELY KEPT
//
// The 9 delegator HintOrdering bodies, the 9 OrderingSourceRef methods, and
// the 4 HintRichOrdering bodies below are WRITE-ONLY: nothing in production
// calls them. Every ordering question the memo asks still goes to the
// physical wrapper. They are staging for the wrapper deletion that would flip
// the caller over — and that deletion is BLOCKED, by RFC-183 §11: four rules
// build compensating plans they never memoize, so a plan's quantifier and its
// plan pointer are two DIFFERENT facts (what the memo costs vs. what
// executes), and collapsing them drops DefaultOnEmpty wrappers and residual
// filters silently. Until those rules memoize, these bodies stay unreachable.
//
// They are kept rather than deleted because re-deriving them at deletion time
// is where a transcription slip would land, and the parity tests in
// cascades/plan_rich_ordering_parity_test.go are what hold them honest in the
// meantime.
//
// WHICH PRODUCERS ASK THE ORDERING-CLAIM PREDICATE
//
// A producer's claim is that the PHYSICAL order it hands rows back in equals
// the LOGICAL order the comparator imposes. For a FLOAT/DOUBLE coordinate those
// two differ (values/ordering_claim.go), so a producer whose order comes from
// FDB KEY layout has to ask. This is enumerated rather than counted, because a
// count is what rots first — and because the sentence this replaces ("every
// derivation routes through it") was false about two of them while they were
// returning wrong rows.
//
//   - ASK, because their order IS the tuple-key order:
//     RecordQueryScanPlan.HintOrdering (via PKScanOrdering),
//     RecordQueryIndexPlan.HintOrdering, both of their HintRichOrdering forms,
//     RecordQueryStreamingAggregationPlan.HintOrdering and
//     RecordQueryAggregateIndexPlan.HintOrdering. The last two claim GROUP
//     order, which is the group column's key order and therefore the same
//     hazard one level up.
//
//   - DOES NOT ASK, and must not: RecordQueryInMemorySortPlan.HintOrdering. It
//     restates the keys it sorted BY, with the comparator. Its claim is true by
//     construction and truncating it would delete the one plan that repairs
//     the others.
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

// orderingSourceOf returns the reference a single-child plan's ordering flows
// from — its one child quantifier's group.
func orderingSourceOf(p RecordQueryPlan) *expressions.Reference {
	qs := p.GetQuantifiers()
	if len(qs) == 0 {
		return nil
	}
	return qs[0].GetRangesOver()
}

// inheritOrdering returns the first known ordering among a reference's
// members. After exploration the ordering-providing alternative may not be
// the first member, so every member is consulted.
//
// FINAL members first, then exploratory — the same discipline RFC-183 §10
// applied to findPhysicalExpr, for the same reason: Java consults FINAL
// expressions only, and AllMembers() concatenates exploratory members BEFORE
// final ones, so a bare AllMembers() scan answers from a promoted-but-
// dominated alternative when both sets hold a member. The exploratory
// fallback stays because a reference can be consulted before finalization.
//
// On the singleton references a plan's quantifier actually ranges over
// (see this file's header) the two orders coincide, so this is a no-op today
// — it is correct for the day the provenance changes, which is the same day
// the wrapper's version stops being the one that runs.
func inheritOrdering(ref *expressions.Reference) properties.Ordering {
	if ref == nil {
		return properties.Ordering{}
	}
	for _, m := range ref.FinalMembers() {
		if o := properties.EstimateOrdering(m); o.IsKnown {
			return o
		}
	}
	for _, m := range ref.Members() {
		if o := properties.EstimateOrdering(m); o.IsKnown {
			return o
		}
	}
	return properties.Ordering{}
}

// richOrderingOf returns the rich ordering a reference's members provide,
// FINAL members first — see inheritOrdering for why the order matters and why
// the exploratory fallback stays.
func richOrderingOf(ref *expressions.Reference) *properties.RichOrdering {
	if ref == nil {
		return properties.EmptyOrdering()
	}
	for _, m := range ref.FinalMembers() {
		if rh, ok := m.(properties.RichOrderingHinter); ok {
			return rh.HintRichOrdering()
		}
	}
	for _, m := range ref.Members() {
		if rh, ok := m.(properties.RichOrderingHinter); ok {
			return rh.HintRichOrdering()
		}
	}
	return properties.EmptyOrdering()
}

// --- delegators -------------------------------------------------------------

// OrderingSourceRef reports the child group this plan's ordering flows from.
func (p *RecordQueryFilterPlan) OrderingSourceRef() *expressions.Reference {
	return orderingSourceOf(p)
}

// HintOrdering: a filter preserves its input's order.
func (p *RecordQueryFilterPlan) HintOrdering() properties.Ordering {
	return inheritOrdering(p.OrderingSourceRef())
}

// OrderingSourceRef reports the child group this plan's ordering flows from.
func (p *RecordQueryPredicatesFilterPlan) OrderingSourceRef() *expressions.Reference {
	return orderingSourceOf(p)
}

// HintOrdering: a filter preserves its input's order.
func (p *RecordQueryPredicatesFilterPlan) HintOrdering() properties.Ordering {
	return inheritOrdering(p.OrderingSourceRef())
}

// OrderingSourceRef reports the child group this plan's ordering flows from.
func (p *RecordQueryTypeFilterPlan) OrderingSourceRef() *expressions.Reference {
	return orderingSourceOf(p)
}

// HintOrdering: a type filter preserves its input's order.
func (p *RecordQueryTypeFilterPlan) HintOrdering() properties.Ordering {
	return inheritOrdering(p.OrderingSourceRef())
}

// OrderingSourceRef reports the child group this plan's ordering flows from.
func (p *RecordQueryDistinctPlan) OrderingSourceRef() *expressions.Reference {
	return orderingSourceOf(p)
}

// HintOrdering: duplicate elimination drops repeats without reordering the
// survivors.
func (p *RecordQueryDistinctPlan) HintOrdering() properties.Ordering {
	return inheritOrdering(p.OrderingSourceRef())
}

// OrderingSourceRef reports the child group this plan's ordering flows from.
func (p *RecordQueryUnorderedPrimaryKeyDistinctPlan) OrderingSourceRef() *expressions.Reference {
	return orderingSourceOf(p)
}

// HintOrdering: primary-key duplicate elimination drops rows without
// reordering the survivors.
func (p *RecordQueryUnorderedPrimaryKeyDistinctPlan) HintOrdering() properties.Ordering {
	return inheritOrdering(p.OrderingSourceRef())
}

// OrderingSourceRef reports the child group this plan's ordering flows from.
func (p *RecordQueryProjectionPlan) OrderingSourceRef() *expressions.Reference {
	return orderingSourceOf(p)
}

// HintOrdering: a projection reshapes rows without reordering them.
func (p *RecordQueryProjectionPlan) HintOrdering() properties.Ordering {
	return inheritOrdering(p.OrderingSourceRef())
}

// OrderingSourceRef reports the child group this plan's ordering flows from.
func (p *RecordQueryMapPlan) OrderingSourceRef() *expressions.Reference {
	return orderingSourceOf(p)
}

// HintOrdering: a map reshapes rows without reordering them.
func (p *RecordQueryMapPlan) HintOrdering() properties.Ordering {
	return inheritOrdering(p.OrderingSourceRef())
}

// OrderingSourceRef reports the child group this plan's ordering flows from.
func (p *RecordQueryLimitPlan) OrderingSourceRef() *expressions.Reference {
	return orderingSourceOf(p)
}

// HintOrdering: a limit truncates a stream without reordering it.
func (p *RecordQueryLimitPlan) HintOrdering() properties.Ordering {
	return inheritOrdering(p.OrderingSourceRef())
}

// OrderingSourceRef reports the child group this plan's ordering flows from.
func (p *RecordQueryDefaultOnEmptyPlan) OrderingSourceRef() *expressions.Reference {
	return orderingSourceOf(p)
}

// HintOrdering: passing the child through preserves its order.
func (p *RecordQueryDefaultOnEmptyPlan) HintOrdering() properties.Ordering {
	return inheritOrdering(p.OrderingSourceRef())
}

// OrderingSourceRef reports the child group this plan's ordering flows from.
func (p *RecordQueryFetchFromPartialRecordPlan) OrderingSourceRef() *expressions.Reference {
	return orderingSourceOf(p)
}

// HintOrdering: fetching the full record per index entry preserves the index
// scan's order.
func (p *RecordQueryFetchFromPartialRecordPlan) HintOrdering() properties.Ordering {
	return inheritOrdering(p.OrderingSourceRef())
}

// --- producers --------------------------------------------------------------

// HintOrdering: a primary scan produces rows in primary-key order.
func (p *RecordQueryScanPlan) HintOrdering() properties.Ordering {
	return PKScanOrdering(p)
}

// equalityPrefixLen returns the length of the leading equality-bound prefix
// in comps, capped at n key positions. It is the SINGLE SOURCE OF TRUTH both
// ordering derivations below consult — HintOrdering drops this prefix from
// the ordering keys, HintRichOrdering retains it as FixedBinding entries —
// so the two cannot classify a column differently by one caller's loop
// breaking where the other's does not.
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
	return equalityPrefixLenOnColumns(comps, n, nil)
}

// equalityPrefixLenOnColumns is equalityPrefixLen for a caller that can resolve
// each coordinate's type. columnCouldBeFloat reports, per key position, whether
// that coordinate can hold a signed zero; a nil func means "unknown", which
// falls back to the operand-only reading.
//
// Threading the column type is what lets a FLOAT coordinate bound by an
// UNKNOWN-typed operand (an IN-list binding) stop the prefix while an INT
// coordinate bound by the same untyped operand keeps it. Deciding that from the
// operand alone cannot separate the two — see
// EqualityPinsSinglePhysicalKeyOnColumn.
func equalityPrefixLenOnColumns(comps []*predicates.ComparisonRange, n int, columnCouldBeFloat func(int) bool) int {
	prefix := 0
	for i := 0; i < n && i < len(comps); i++ {
		var pins bool
		if columnCouldBeFloat == nil {
			pins = EqualityPinsSinglePhysicalKey(comps[i])
		} else {
			pins = EqualityPinsSinglePhysicalKeyOnColumn(comps[i], columnCouldBeFloat(i))
		}
		if !pins {
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

// indexColumnCouldBeFloat resolves each index key column against the scan's
// flowed record layout and reports whether it can hold a signed zero. It is the
// per-position input equalityPrefixLenOnColumns needs, and it delegates to
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
func indexColumnCouldBeFloat(
	keyTypes []values.Type, layout values.Type, columnNames []string,
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
// Counting that as an equality claimed the suffix was globally ordered and let
// the planner drop a required sort: over rows (-0.0, 9) and (+0.0, 1),
// `WHERE v = 0 ORDER BY w` returned [9 1] unsorted, and `... LIMIT 1` returned
// the wrong row entirely.
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
		// Treating this as "not provably nonzero" put a materialized sort back on
		// every `<float> IS NULL … ORDER BY <pk>` — measured on rowdiff seed 224,
		// 4 plans, and caught by the committed pure-planner ordering sweep, not
		// by the targeted test. The widening this function guards is driven by a
		// zero VALUE; a range with no value cannot trigger it.
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
	switch n := v.(type) {
	case float64:
		return n != 0
	case float32:
		return n != 0
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
// which settles ASC by itself" — is FALSE at this coordinate, and believing it
// produced a wrong answer. It presumes one comparator. There are TWO, and they
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
// from a FORWARD scan. Measured, that returned the zero blocks ascending as
// [7 9 1 3] where the correct answer is [3 1 9 7]. HintRichOrdering binds it
// SORTED for that reason, and
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
		// COMMON shape, far more common than the literal zero this function was
		// originally written for.
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
// Measured: it turned four test targets red, including an InJoin that must
// claim ascending order.
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

// PKScanOrdering returns a primary scan's PK ordering. Shared with the
// data-access path's plan-backed leaf, which memoizes a SARGed PK scan.
//
// PK positions bound by an equality comparison do not consume a sort
// position: mirrors Java's
// ValueIndexLikeMatchCandidate.computeOrderingFromScanComparisons, whose
// equality-bound prefix (i < scanComparisons.getEqualitySize()) only
// populates the binding map with Binding.fixed entries and is never
// appended to orderingSequenceBuilder — the ordering sequence starts at the
// first non-equality-bound key. Without this, a per-binding equality scan
// (id Fixed) and an unbound scan over the same PK columns (id
// Sorted-ascending) report the identical Keys, so plan partitioning
// (expression_partition.go's orderingsEqual) cannot tell them apart and
// co-partitions them. Compare RecordQueryScanPlan.HintRichOrdering below,
// which already carries this distinction via FixedBinding/SortedBinding but
// is not consulted by plain-ordering partitioning.
func PKScanOrdering(plan *RecordQueryScanPlan) properties.Ordering {
	if plan == nil {
		return properties.Ordering{}
	}
	pk := plan.GetPrimaryKeyValues()
	if len(pk) == 0 {
		return properties.Ordering{}
	}
	comps := plan.GetScanComparisons()
	firstNonEq := equalityPrefixLen(comps, len(pk))
	keys := resolveOrderingColumns(pk[firstNonEq:])
	// Terminate the claim at the first coordinate whose physical key order is
	// not its logical order (a FLOAT/DOUBLE primary-key column — see
	// keyCanExtendOrderingClaim). A float PK is reachable: `PRIMARY KEY (e)`
	// with e DOUBLE makes the scan itself claim an order it does not deliver.
	//
	// This truncation is NOT what keeps the sort on such a query, and saying so
	// matters because it decides where the test has to live. MEASURED: deleting
	// this line leaves every plan-shape assertion in
	// embedded_test.TestFloatPrimaryKeyScanClaimsNoOrder green — SQL sort
	// elision on the primary-key path is decided by OrderedPrimaryScanRule's
	// decline. What this line protects is the OTHER consumer of PKScanOrdering:
	// plan partitioning (expression_partition.go's orderingsEqual) and the cost
	// model, which would otherwise co-partition scans that deliver different
	// orders. Its own coverage is therefore direct, at this level —
	// TestPKScanOrdering_FloatPKColumnTerminatesTheClaim — rather than borrowed
	// from a plan-shape test that would stay green without it.
	keys = keys[:claimableKeyLimit(plan.GetFlowedType(), keys)]
	if len(keys) == 0 {
		return properties.Ordering{}
	}
	desc := make([]bool, len(keys))
	if plan.IsReverse() {
		for i := range desc {
			desc[i] = true
		}
	}
	return properties.Ordering{IsKnown: true, Keys: keys, Descending: desc}
}

// HintOrdering: an index scan produces rows in index-key order for the
// non-equality-bound suffix columns, extended by the trimmed primary-key
// suffix (index entries are (index key, primary key), so the PK columns
// continue the sort order). E.g. index(a, b, c) with a = 1 over PK (id)
// produces output sorted by (b, c, id). Mirrors the full-key ordering of
// Java's ValueIndexLikeMatchCandidate.computeOrderingFromScanComparisons.
func (p *RecordQueryIndexPlan) HintOrdering() properties.Ordering {
	if p == nil || len(p.GetColumnNames()) == 0 {
		return properties.Ordering{}
	}
	if !p.orderingKeyNamesKnown || !p.orderingKeyNamesSafe {
		return properties.Ordering{}
	}
	// A fan-out index key contains exploded elements, while columnNames names
	// the logical record fields. Synthesizing an ordering from those names
	// would advertise the original array (and any later flat columns) as
	// ordered Values. The candidate-level match path can reason positionally
	// about an equality-fixed fan-out element; this plan-level hint lacks that
	// structural key-expression information, so it must abstain.
	if p.distinctRecordsKnown && p.createsDuplicates {
		return properties.Ordering{}
	}
	columnNames, pkColumnNames := p.GetColumnNames(), p.GetPKColumnNames()
	comps := p.GetScanComparisons()
	firstNonEq := equalityPrefixLenOnColumns(comps, len(columnNames),
		indexColumnCouldBeFloat(p.GetKeyComponentTypes(), p.GetFlowedType(), columnNames))
	rev := p.IsReverse()
	// The SORTED coordinates, in order: the non-equality-bound index columns
	// followed by the trimmed PK suffix. The claim is truncated at the first
	// FLOAT/DOUBLE among them — an index on a double column does NOT deliver
	// its own key order (NaN packs into two disjoint blocks), and because all
	// NaNs are one logical tie class split across those blocks, the PK suffix
	// after it is not ordered within the tie either.
	// A coordinate bound by an equality that spans BOTH signed zeros stays in
	// that prefix — the range set opens its two blocks in key order, so a sort on
	// it is satisfied in the scan's direction, NOT because it admits one logical
	// value (it admits two distinct sort values; see
	// EqualityBoundCoordinateClaimsOwnOrder) — but nothing after it is globally
	// ordered, so the sorted tail goes away entirely rather than being truncated
	// by type.
	fixedLen := ownOrderPrefixLen(comps, len(columnNames))
	sorted := make([]string, 0, len(columnNames)-fixedLen+len(pkColumnNames))
	if fixedLen == firstNonEq {
		sorted = append(sorted, columnNames[fixedLen:]...)
		sorted = append(sorted, TrimmedPKSuffix(columnNames, pkColumnNames)...)
		sorted = sorted[:claimableNameLimit(p.GetFlowedType(), sorted)]
	}
	if len(sorted) == 0 {
		return properties.Ordering{}
	}
	keys := make([]values.Value, 0, len(sorted))
	desc := make([]bool, 0, len(sorted))
	for _, col := range sorted {
		key := orderingColumnOfName(p.GetResultValue(), p.GetFlowedType(), col)
		if key == nil {
			return properties.Ordering{}
		}
		keys = append(keys, key)
		desc = append(desc, rev)
	}
	return properties.Ordering{IsKnown: true, Keys: keys, Descending: desc}
}

// columnCanExtendOrderingClaim / keyCanExtendOrderingClaim are the plans-side
// door to the ONE ordering-claim predicate (values.TypeTerminatesOrderingClaim),
// so no two derivations can classify the same column differently.
//
// An earlier revision said "EVERY derivation in this file that turns a
// key-column sequence into an ordering claim routes through them". It did not,
// and the gap was not academic: the two AGGREGATE producers never asked, and
// `SELECT d, SUM(a) FROM t GROUP BY d ORDER BY d` returned the negative-NaN
// group FIRST on a real cluster. Which producer asks, and why the rest need
// not, is enumerated in this file's header.
//
// The rule they enforce: an ordering claim TERMINATES at a coordinate whose
// physical FDB key order is not its logical order. For FLOAT/DOUBLE that is
// NaN — negative NaN payloads pack BEFORE -Inf and positive ones AFTER +Inf,
// while the comparator (values.CompareFloat64, faithful to
// java.lang.Double.compare) collapses every NaN to one value ranked GREATEST.
// So the column is misordered AND every NaN is one logical tie class split
// across two disjoint physical ranges, which leaves any LATER sort column
// unordered within the tie. Hence terminate, not reorder.
//
// Only the SORTED coordinates are subject to this. An equality-bound float
// prefix column is FIXED, not sorted: it pins one physical point, so it makes
// no order claim and the columns after it stay claimable. (The one equality
// that does NOT pin a point — a zero-valued float, which spans both signed
// zeros — is already excluded upstream by equalityPrefixLen.)
// keyCanExtendOrderingClaim is the Value-shaped form, for derivations that
// already hold ordering keys rather than metadata names. A flat field is
// resolved by name against the layout (its declared type is usually
// UnknownType at this point, so the layout is the authority); any other key
// shape is judged on its own type.
func keyCanExtendOrderingClaim(layout values.Type, k values.Value) bool {
	if k == nil {
		return true
	}
	return !values.TypeTerminatesOrderingClaim(k.Type())
}

// claimableKeyLimit returns how many leading keys may be claimed as an
// ordering against layout — the index of the first key that terminates the
// claim, or len(keys) when none does.
func claimableKeyLimit(layout values.Type, keys []values.Value) int {
	for i, k := range keys {
		if !keyCanExtendOrderingClaim(layout, k) {
			return i
		}
	}
	return len(keys)
}

// claimableNameLimit is claimableKeyLimit over metadata column names.
func claimableNameLimit(layout values.Type, names []string) int {
	return values.ClaimableOrderingPrefix(layout, names)
}

// claimableTypedKeyLimit is claimableKeyLimit for keys that carry their OWN
// declared type, with no flowed layout to resolve against.
//
// It cannot go through keyCanExtendOrderingClaim: that one resolves a flat
// FieldValue by NAME against the layout, because a scan's ordering keys are
// minted with UnknownType and the layout is the only authority. A grouping key
// arrives from the translator already typed, and there is no layout — so
// routing it through the name path with a nil layout would ask a question that
// always answers "not a float" and would silently claim every ordering.
//
// It is the plans-side door to values.ClaimableTypedKeyPrefix, which the
// planner's StreamingAggFromIndexRule also asks — for a different reason (input
// CLUSTERING, not advertised order) but off the same predicate, so the rule
// that BUILDS the plan and the derivation that DESCRIBES it cannot classify the
// same grouping key differently.
func claimableTypedKeyLimit(keys []values.Value) int {
	return values.ClaimableTypedKeyPrefix(keys)
}

// orderingColumnOfName mints an ordering key for a METADATA column name
// resolved against the layout the plan flows.
//
// This is RFC-197's one sanctioned direction for a name: the index's column
// list names its columns, the name is resolved ONCE against the row type the
// scan produces, and it dies there — every consumer downstream compares the
// ORDINAL and the layout token, never the spelling. The display name rides
// along for Explain only.
//
// A layout with no declared column order (a multi-record-type index's degraded
// row type, UnknownType) or a name the layout does not declare yields a LAZY
// key, which no identity-keyed consumer can address. That is the fail-closed
// direction: the ordering claim is dropped, costing a sort, rather than being
// stated in terms nothing can verify.
func orderingColumnOfName(root values.Value, layout values.Type, name string) values.Value {
	if ident, ok := values.OrdinalOfNameIn(layout, name); ok {
		request, err := values.FieldByNameAndOrdinal(name, ident.Ordinal)
		if err != nil {
			return nil
		}
		resolved, err := values.ResolveFieldAccess(root, []values.FieldRequest{request})
		if err != nil {
			return nil
		}
		return resolved
	}
	return nil
}

// resolveOrderingColumns re-mints an already-Value-shaped key list against the
// layout the plan flows, so a key that arrived as a LAZY flat field carries its
// ordinal and layout token like every other provided key.
//
// A key that is not a bare flat field (a RecordTypeValue constant, a nested or
// composite component) is passed through untouched: it has no single-ordinal
// identity to state, and inventing one is the ordinal conflation the domain
// token exists to prevent. An ALREADY-baked key is left alone — re-resolving it
// by name would undo whatever producer knew better.
func resolveOrderingColumns(keys []values.Value) []values.Value {
	out := make([]values.Value, len(keys))
	copy(out, keys)
	return out
}

// TrimmedPKSuffix returns the primary-key columns not already present in the
// index key columns, in PK order. Ports Java's Index.trimPrimaryKey semantics
// as used by ValueIndexExpansionVisitor.fullKey: PK components that appear in
// the index key are trimmed, the remainder is appended after the index key.
func TrimmedPKSuffix(columnNames, pkColumnNames []string) []string {
	if len(pkColumnNames) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(columnNames))
	for _, c := range columnNames {
		seen[c] = struct{}{}
	}
	suffix := make([]string, 0, len(pkColumnNames))
	for _, col := range pkColumnNames {
		if _, dup := seen[col]; dup {
			continue
		}
		seen[col] = struct{}{}
		suffix = append(suffix, col)
	}
	return suffix
}

// HintOrdering: an in-memory sort produces exactly its sort keys. NULL
// placement is carried so a parent sort does not elide against a counterflow
// (e.g. ASC NULLS LAST) stream as if it were natural order.
func (p *RecordQueryInMemorySortPlan) HintOrdering() properties.Ordering {
	if p == nil {
		return properties.Ordering{}
	}
	sks := p.GetSortKeys()
	keys := make([]values.Value, len(sks))
	desc := make([]bool, len(sks))
	nullsFirst := make([]bool, len(sks))
	for i, sk := range sks {
		if sk.ValueExpr == nil {
			// ValueExpr is REQUIRED of every SortKey (see in_memory_sort.go),
			// and the executor enforces that: a nil one is rejected as a
			// malformed plan, loud, never a name read. An ADVERTISER that
			// minted a lazy FieldValue from SortKey.Field instead was therefore
			// MORE PERMISSIVE THAN THE EXECUTOR OF THE SAME STRUCT — it stated
			// an ordering for a plan the cursor will refuse to run.
			//
			// It also stated it in a second vocabulary. SortKey.Field is a
			// DISPLAY rendering: for anything but a bare column it is
			// ExplainValue's output, correlation and `#ordinal` included. A
			// lazy FieldValue carrying that string is declined by the
			// match-domain identity (AccessorNamePath), because a rendered
			// label is indistinguishable as a string from a real nested path.
			// So the advertised ordering was not comparable with a baked one,
			// which is what made satisfaction producer-dependent rather than
			// merely untidy.
			//
			// UNKNOWN rather than a panic: HintOrdering is a property
			// advertiser with no error channel, and the contract already has
			// exactly one loud enforcement point — the executor, where an error
			// can be returned and where the plan is actually rejected. An
			// advertiser that under-claims costs a plan shape; one that
			// over-claims is the bug being fixed here.
			return properties.Ordering{}
		}
		// The key's OWN Value is the identity, and the sort re-orders rows
		// without reshaping them, so the Value the executor evaluates per row is
		// exactly the Value this plan provides an ordering ON.
		keys[i] = sk.ValueExpr
		desc[i] = sk.Desc
		nullsFirst[i] = sk.NullsFirst
	}
	return properties.Ordering{IsKnown: true, Keys: keys, Descending: desc, NullsFirst: nullsFirst}
}

// HintOrdering: a merge-sort union emits rows in its comparison-key order,
// in the direction it merges. See RecordQueryInUnionPlan.HintOrdering for why
// the single reverse flag is the whole truth about that direction.
func (p *RecordQueryMergeSortUnionPlan) HintOrdering() properties.Ordering {
	return mergeComparisonKeyOrdering(p.GetComparisonKeys(), p.IsReverse())
}

// HintOrdering: an intersection emits rows in its semantic comparison-key
// order. Use the ordering parts rather than the executable comparison Values:
// a future mixed/counterflow key may be physically encoded as ordered bytes,
// but the SQL-visible ordering remains over the original columns.
func (p *RecordQueryIntersectionPlan) HintOrdering() properties.Ordering {
	if p == nil {
		return properties.Ordering{}
	}
	parts := p.GetComparisonKeyOrderingParts()
	if len(parts) == 0 {
		return properties.Ordering{}
	}
	keys := make([]values.Value, len(parts))
	descending := make([]bool, len(parts))
	nullsFirst := make([]bool, len(parts))
	for i, part := range parts {
		if !part.SortOrder.IsDirectional() {
			return properties.Ordering{}
		}
		keys[i] = part.Value
		descending[i] = part.SortOrder.IsAnyDescending()
		nullsFirst[i] = part.SortOrder == properties.ProvidedSortOrderAscending ||
			part.SortOrder == properties.ProvidedSortOrderDescendingNullsFirst
	}
	return properties.Ordering{
		IsKnown:    true,
		Keys:       keys,
		Descending: descending,
		NullsFirst: nullsFirst,
	}
}

// HintOrdering: an InUnion emits rows in its comparison-key order, in the
// direction it merges its per-binding legs.
//
// The reverse flag IS the direction of every comparison key: the rules that
// build these merges refuse any candidate whose parts do not all agree with it
// (properties.NaturalComparisonKeyValues), because the executable comparison
// key is the raw Value and a key read forward cannot express a descending
// component. Reporting the keys without their direction advertised a
// descending merge as ascending, so a matching ORDER BY DESC saw its own
// access path as unsatisfying and kept an in-memory sort over it.
func (p *RecordQueryInUnionPlan) HintOrdering() properties.Ordering {
	return mergeComparisonKeyOrdering(p.GetComparisonKeys(), p.IsReverse())
}

// mergeComparisonKeyOrdering builds the provided ordering of a merge set
// operation: its comparison keys, every one of them in the merge direction.
// NULL placement stays natural (ASC → nulls first, DESC → nulls last) — the
// merge compares raw tuple-encoded values, which is exactly the natural
// placement.
func mergeComparisonKeyOrdering(keys []values.Value, reverse bool) properties.Ordering {
	if !reverse {
		return properties.Ordering{IsKnown: true, Keys: keys}
	}
	descending := make([]bool, len(keys))
	for i := range descending {
		descending[i] = true
	}
	return properties.Ordering{IsKnown: true, Keys: keys, Descending: descending}
}

// HintOrdering: a multi-way intersection emits rows in its comparison-key
// order, when it has one.
func (p *RecordQueryMultiIntersectionOnValuesPlan) HintOrdering() properties.Ordering {
	if p == nil {
		return properties.Ordering{}
	}
	compKey := p.GetComparisonKey()
	if len(compKey) == 0 {
		return properties.Ordering{}
	}
	// The comparison key is CHILD-row relative (the merge cursor evaluates it
	// against each stream's [groupCols..., FUNC(col)] row); the ordering this
	// plan ADVERTISES is over the row it EMITS. The two agree on the grouping
	// ordinals — group column i is slot i of both — but they are different
	// layouts, and only the output row is the one a requested ORDER BY key is
	// baked against. So the advertised key restates the same ordinal in the
	// OUTPUT row's domain instead of handing out the child-relative Value.
	//
	// No derivable output layout leaves the keys as they are: an
	// unaddressable ordering key costs an elision, one domained in a layout
	// nothing can name would be a claim rather than a proof.
	names := p.outputColumnNames()
	if len(names) == 0 {
		return properties.Ordering{IsKnown: true, Keys: compKey}
	}
	outputLayout, err := p.ProvidedOutputLayout()
	if err != nil || outputLayout == nil || outputLayout.Carrier() == nil {
		return properties.Ordering{IsKnown: true, Keys: compKey}
	}
	keys := make([]values.Value, len(compKey))
	for i, k := range compKey {
		fv, isField := values.AsFieldValue(k)
		path := values.FieldPathView(nil)
		if isField {
			path = fv.Path()
		}
		if !isField || fv.ChildValue() == nil || path == nil ||
			path.Len() != 1 || i >= len(names) {
			// Anything but "grouping column i, read at slot i" is not the
			// shape this restatement is proven for.
			return properties.Ordering{IsKnown: true, Keys: compKey}
		}
		accessor, ok := path.Accessor(0)
		if !ok || accessor.Ordinal() != i {
			return properties.Ordering{IsKnown: true, Keys: compKey}
		}
		request, err := values.FieldByNameAndOrdinal(names[i], i)
		if err != nil {
			return properties.Ordering{IsKnown: true, Keys: compKey}
		}
		resolved, err := values.ResolveFieldAccess(outputLayout.Carrier(), []values.FieldRequest{request})
		if err != nil {
			return properties.Ordering{IsKnown: true, Keys: compKey}
		}
		keys[i] = resolved
	}
	return properties.Ordering{IsKnown: true, Keys: keys}
}

// outputColumnNames returns the column names of the row this plan emits, taken
// from its result value's record-constructor fields, or nil when the result
// value is not a record constructor (no nameable output layout).
func (p *RecordQueryMultiIntersectionOnValuesPlan) outputColumnNames() []string {
	rc, isRecord := p.GetResultValue().(*values.RecordConstructorValue)
	if !isRecord {
		return nil
	}
	names := make([]string, len(rc.Fields))
	for i, f := range rc.Fields {
		names[i] = f.Name
	}
	return names
}

// HintOrdering: the advertised ordering is over the aggregate's OUTPUT row —
// group key i flows as output column i, NAMED by the canonical group-key
// output name (AggregateKeyColumnName, the same authority the runtime output
// row and the ORDER-BY-over-aggregate bake use). Advertising the raw
// grouping-key VALUES (input-relative bakes over the pre-aggregate row)
// mis-rendered the provided keys and made a satisfied ORDER BY look
// unsatisfied — a spurious second InMemorySort above the aggregate; an
// evaluating consumer (a merge comparison key) would also have read the
// aggregate's output row with a dead pre-aggregate ordinal.
// A grouping key that TERMINATES the ordering claim truncates it here exactly
// as it does on an index scan, and for the same reason: a streaming
// aggregation emits its groups in the order its input hands them over, so a
// FLOAT/DOUBLE grouping key means the groups come out in tuple-key order, with
// a negative-NaN group physically FIRST and logically LAST.
//
// The truncation is asked of the same authority the scan producers ask
// (values.TypeTerminatesOrderingClaim, via claimableKeyLimit) because the
// grouping keys are Values that already carry their declared type. This plan
// does NOT inherit the decision from its inner: StreamingAggFromIndexRule
// builds it by matching grouping keys against index column NAMES and never
// reads the inner's ordering claim at all, so terminating the inner scan's
// claim does not reach this producer.
func (p *RecordQueryStreamingAggregationPlan) HintOrdering() properties.Ordering {
	if p == nil || len(p.GetGroupingKeys()) == 0 {
		return properties.Ordering{IsKnown: false}
	}
	groupKeys := p.GetGroupingKeys()
	groupKeys = groupKeys[:claimableTypedKeyLimit(groupKeys)]
	if len(groupKeys) == 0 {
		return properties.Ordering{IsKnown: false}
	}
	outputLayout, err := p.ProvidedOutputLayout()
	if err != nil || outputLayout == nil || outputLayout.Carrier() == nil {
		return properties.Ordering{IsKnown: false}
	}
	keys := make([]values.Value, len(groupKeys))
	for i, k := range groupKeys {
		request, err := values.FieldByNameAndOrdinal(expressions.AggregateKeyColumnName(k), i)
		if err != nil {
			return properties.Ordering{IsKnown: false}
		}
		keys[i], err = values.ResolveFieldAccess(outputLayout.Carrier(), []values.FieldRequest{request})
		if err != nil {
			return properties.Ordering{IsKnown: false}
		}
	}
	desc := make([]bool, len(keys))
	if orderProducingScanIsReverse(p.GetInner()) {
		for i := range desc {
			desc[i] = true
		}
	}
	return properties.Ordering{IsKnown: true, Keys: keys, Descending: desc}
}

// orderProducingScanIsReverse reports whether the index scan that actually
// produces plan's rows runs in reverse.
//
// The DIRECTION of a delegating producer's ordering is a correctness claim, not
// decoration: a consumer that reads "ascending" elides its own sort, so an
// ascending claim over a descending scan returns wrongly-ordered rows with
// nothing failing. Matching only the bare *RecordQueryIndexPlan made that claim
// fail SILENTLY in the ascending direction — the type assertion misses, the
// direction slice stays all-false, and the miss is indistinguishable from a
// genuinely forward scan.
//
// Two wrappers stand between an operator and its scan, and neither changes row
// ORDER, so both are traversed:
//
//   - RecordQueryCoveringIndexPlan holds its scan as a FIELD, not a child, so
//     no child walk reaches it. Java answers the same way — by DELEGATING the
//     getter to the wrapped plan rather than by walking children
//     (RecordQueryCoveringIndexPlan.java:224 for getCorrelatedTo).
//   - RecordQueryFetchFromPartialRecordPlan resolves entries to base records in
//     the order its inner hands them over, which is why it delegates IsReverse
//     itself. It is the shape the access path now always builds beneath an
//     operator that needs the full record, so an ordered inner reaching this
//     function is normally Fetch(Covering(IndexScan)).
func orderProducingScanIsReverse(plan RecordQueryPlan) bool {
	switch p := plan.(type) {
	case *RecordQueryIndexPlan:
		return p.IsReverse()
	case *RecordQueryCoveringIndexPlan:
		return orderProducingScanIsReverse(p.GetIndexPlan())
	case *RecordQueryFetchFromPartialRecordPlan:
		return orderProducingScanIsReverse(p.GetInner())
	default:
		return false
	}
}

// HintOrdering: an aggregate index is stored grouped, so it emits one row per
// group in group-column KEY order — which is the group's VALUE order only for
// coordinates whose tuple encoding is order-preserving under the comparator.
// The claim is therefore truncated at the first FLOAT/DOUBLE grouping column,
// by the same authority every other producer here asks: a negative-NaN group
// is the physically first row and the logically last one, and the NaN groups
// form one tie class split across two disjoint physical ranges, so no later
// grouping column is ordered within it either.
//
// The grouping columns are NAMES, so answering this needs a layout — carried
// as groupColLayout from the match candidate's base record type. With none
// (a multi-record-type index), the predicate's fail-open direction applies and
// the claim stands, exactly as it does for an untyped index scan.
func (p *RecordQueryAggregateIndexPlan) HintOrdering() properties.Ordering {
	groupCols := p.GetGroupCols()
	if len(groupCols) == 0 {
		return properties.Ordering{IsKnown: true}
	}
	groupCols = groupCols[:claimableNameLimit(p.GetGroupColumnLayout(), groupCols)]
	if len(groupCols) == 0 {
		return properties.Ordering{IsKnown: false}
	}
	// Grouping column i IS slot i of the row this plan flows
	// ([groupCols..., FUNC(col)] — aggregateIndexCursor's layout, named by
	// OutputColumnNames), so the ordering key states that ordinal and the
	// layout it indexes. The streaming-aggregation provider states the same
	// thing about its own output row; both must, because a requested ORDER BY
	// key on an aggregate output is baked against that row and an ordinal with
	// no domain is one no consumer may compare.
	keys := make([]values.Value, len(groupCols))
	desc := make([]bool, len(groupCols))
	for i, col := range groupCols {
		request, err := values.FieldByNameAndOrdinal(col, i)
		if err != nil {
			return properties.Ordering{IsKnown: false}
		}
		keys[i], err = values.ResolveFieldAccess(p.GetResultValue(), []values.FieldRequest{request})
		if err != nil {
			return properties.Ordering{IsKnown: false}
		}
		desc[i] = p.IsReverse()
	}
	return properties.Ordering{IsKnown: true, Keys: keys, Descending: desc}
}

// --- unordered --------------------------------------------------------------

// HintOrdering: an InJoin iterates IN-values one at a time. Each batch
// preserves the inner scan's ordering, but the GLOBAL result ordering depends
// on the IN-source order, not the inner scan. Claiming the inner's ordering
// would let sort elimination remove a necessary ORDER BY.
func (p *RecordQueryInJoinPlan) HintOrdering() properties.Ordering {
	return properties.Ordering{}
}

// HintOrdering: an unordered union interleaves its legs arbitrarily.
func (p *RecordQueryUnorderedUnionPlan) HintOrdering() properties.Ordering {
	return properties.Ordering{}
}

// HintOrdering: a K-NN probe returns its neighbours in an order the ordering
// property does not model.
func (p *RecordQueryVectorIndexPlan) HintOrdering() properties.Ordering {
	return properties.Ordering{}
}

// HintOrdering: exploding a collection yields elements in no modeled order.
func (p *RecordQueryExplodePlan) HintOrdering() properties.Ordering {
	return properties.Ordering{IsKnown: false}
}

// HintOrdering: a dependent join's output order is not modeled.
func (p *RecordQueryFlatMapPlan) HintOrdering() properties.Ordering {
	return properties.Ordering{IsKnown: false}
}

// HintOrdering: a nested-loop join's output order is not modeled.
func (p *RecordQueryNestedLoopJoinPlan) HintOrdering() properties.Ordering {
	return properties.Ordering{IsKnown: false}
}

// HintOrdering: recursive traversal order is not modeled.
func (p *RecordQueryRecursiveDfsJoinPlan) HintOrdering() properties.Ordering {
	return properties.Ordering{IsKnown: false}
}

// HintOrdering: recursive level order is not modeled.
func (p *RecordQueryRecursiveLevelUnionPlan) HintOrdering() properties.Ordering {
	return properties.Ordering{IsKnown: false}
}

// HintOrdering: a table function's output order is opaque.
func (p *RecordQueryTableFunctionPlan) HintOrdering() properties.Ordering {
	return properties.Ordering{IsKnown: false}
}

// HintOrdering: a temp-table insert's output order is not modeled.
func (p *RecordQueryTempTableInsertPlan) HintOrdering() properties.Ordering {
	return properties.Ordering{IsKnown: false}
}

// HintOrdering: a temp-table scan reads an unordered buffer.
func (p *RecordQueryTempTableScanPlan) HintOrdering() properties.Ordering {
	return properties.Ordering{IsKnown: false}
}

// --- rich orderings ---------------------------------------------------------
//
// HintRichOrdering is the binding-carrying form of HintOrdering: it keeps the
// distinction between a key that is EQUALITY-BOUND (FixedBinding — every row
// holds the same value, so the key satisfies a request in EITHER direction)
// and one that is merely SORTED in a given direction. That distinction is what
// lets `WHERE a = 1 ORDER BY a DESC` elide its sort over a forward scan, and
// plain Ordering cannot express it.
//
// Only the plans whose ordering carries bindings implement it. Everything else
// is served by the caller's fallback, which synthesizes a sorted-only binding
// map from HintOrdering.

// HintRichOrdering returns a primary scan's PK ordering with bindings: PK
// positions bound by an equality comparison become FixedBinding entries, the
// rest SortedBinding. A primary scan is a value-index-like candidate in Java
// (PrimaryScanMatchCandidate implements ValueIndexLikeMatchCandidate), so its
// ordering comes from the same computeOrderingFromScanComparisons: the
// equality prefix is Binding.fixed, which is compatible with ANY requested
// direction.
func (p *RecordQueryScanPlan) HintRichOrdering() *properties.RichOrdering {
	if p == nil {
		return properties.EmptyOrdering()
	}
	pk := p.GetPrimaryKeyValues()
	if len(pk) == 0 {
		return properties.EmptyOrdering()
	}
	comps := p.GetScanComparisons()
	prefixLen := equalityPrefixLen(comps, len(pk))
	bm := make(map[values.Value][]properties.OrderingBinding, len(pk))
	keys := make([]values.Value, 0, len(pk))
	dir := properties.ProvidedSortOrderAscending
	if p.IsReverse() {
		dir = properties.ProvidedSortOrderDescending
	}
	resolved := resolveOrderingColumns(pk)
	// Truncate the SORTED tail at the first coordinate whose physical order is
	// not its logical order. The equality-bound prefix is exempt: a FixedBinding
	// pins one physical point and states no order, so a float there is harmless
	// and the columns after it stay claimable.
	limit := prefixLen + claimableKeyLimit(p.GetFlowedType(), resolved[prefixLen:])
	// The ordering covers the WHOLE storage key exactly when nothing was
	// truncated away above and the key's physical uniqueness is also logical
	// uniqueness. Both facts are known here and nowhere else, so both are
	// stamped here rather than re-derived by whoever consumes the ordering.
	storageComplete := limit == len(resolved) &&
		len(p.GetRecordTypes()) == 1 &&
		properties.TupleKeyUniquenessMatchesLogicalEquality(
			p.GetKeyComponentTypes(), len(pk))
	for i, key := range resolved[:limit] {
		keys = append(keys, key)
		if i < prefixLen {
			bm[key] = []properties.OrderingBinding{properties.FixedBinding(comps[i])}
		} else {
			bm[key] = []properties.OrderingBinding{properties.SortedBinding(dir)}
		}
	}
	if len(keys) == 0 {
		return properties.EmptyOrdering()
	}
	return properties.NewRichOrdering(bm, keys, properties.NotDistinct()).
		WithStorageKeyComplete(storageComplete)
}

// HintRichOrdering returns the index scan's full ordering with bindings:
// equality-bound prefix columns become FixedBinding entries (carrying the
// comparison), non-equality suffix columns become SortedBinding entries. The
// trimmed primary-key suffix continues the sorted keys — this is what lets an
// equality-prefixed scan (status = ?) satisfy ORDER BY pk, exactly as Java's
// ValueIndexLikeMatchCandidate.computeOrderingFromScanComparisons derives the
// ordering over getFullKeyExpression() (index key + trimmed PK) with
// Binding.fixed for the equality prefix and Binding.sorted for the rest.
//
// Note this differs from HintOrdering, which DROPS the equality prefix
// entirely; here the prefix is retained as fixed, which is strictly more
// information.
func (p *RecordQueryIndexPlan) HintRichOrdering() *properties.RichOrdering {
	if p == nil || len(p.GetColumnNames()) == 0 {
		return properties.EmptyOrdering()
	}
	if !p.orderingKeyNamesKnown || !p.orderingKeyNamesSafe {
		return properties.EmptyOrdering()
	}
	if p.distinctRecordsKnown && p.createsDuplicates {
		return properties.EmptyOrdering()
	}
	columnNames, pkColumnNames := p.GetColumnNames(), p.GetPKColumnNames()
	comps := p.GetScanComparisons()
	prefixLen := equalityPrefixLenOnColumns(comps, len(columnNames),
		indexColumnCouldBeFloat(p.GetKeyComponentTypes(), p.GetFlowedType(), columnNames))
	bm := make(map[values.Value][]properties.OrderingBinding)
	keys := make([]values.Value, 0, len(columnNames)+len(pkColumnNames))

	dir := properties.ProvidedSortOrderAscending
	if p.IsReverse() {
		dir = properties.ProvidedSortOrderDescending
	}
	// Same split as the plain form: the equality-bound prefix is FIXED (no order
	// claimed, so a float there is harmless), and the SORTED tail — the
	// remaining index columns plus the trimmed PK suffix — is truncated at the
	// first FLOAT/DOUBLE.
	// A coordinate bound by an equality spanning BOTH signed zeros belongs in
	// that leading prefix too — but as a SORTED entry, not a FIXED one, which is
	// why the prefix length comes from ownOrderPrefixLen rather than from the
	// pins-one-key question. Its ground is RFC-208's range set opening the two
	// zero blocks in KEY ORDER (reversed wholesale under a reverse scan), NOT
	// that it admits one logical value: it admits two distinct sort values, so
	// `ORDER BY` on it is satisfied only in the direction the scan runs. The
	// loop below binds it accordingly; do not restate the vacuity argument here,
	// it is refuted at EqualityBoundCoordinateClaimsOwnOrder.
	// What it cannot do is carry the tail: a later coordinate restarts at the
	// block boundary. So the tail is dropped outright rather than truncated by
	// type, and the leading prefix keeps its full length.
	fixedLen := ownOrderPrefixLen(comps, len(columnNames))
	tail := make([]string, 0, len(columnNames)-fixedLen+len(pkColumnNames))
	untruncated := 0
	if fixedLen == prefixLen {
		tail = append(tail, columnNames[fixedLen:]...)
		tail = append(tail, TrimmedPKSuffix(columnNames, pkColumnNames)...)
		untruncated = len(tail)
		tail = tail[:claimableNameLimit(p.GetFlowedType(), tail)]
	}
	// The coordinates below are the whole storage key exactly when the tail was
	// neither dropped wholesale (fixedLen != prefixLen, a signed-zero equality
	// that restarts the order at a block boundary) nor truncated at a FLOAT, AND
	// the index key and its primary-key suffix both make physical uniqueness
	// mean logical uniqueness. Every one of those facts is settled right here;
	// stamping it is what keeps a consumer from asking a second property to
	// agree with this one by hand.
	storageComplete := fixedLen == prefixLen && len(tail) == untruncated &&
		len(p.GetRecordTypes()) == 1 &&
		properties.TupleKeyUniquenessMatchesLogicalEquality(
			p.GetKeyComponentTypes(), len(columnNames)) &&
		properties.TupleKeyUniquenessMatchesLogicalEquality(
			p.GetPrimaryKeyComponentTypes(), len(pkColumnNames))
	prefixLen = fixedLen
	for i, col := range columnNames[:prefixLen] {
		key := orderingColumnOfName(p.GetResultValue(), p.GetFlowedType(), col)
		if key == nil {
			return properties.EmptyOrdering()
		}
		keys = append(keys, key)
		// FIXED means "states no order, so ANY requested direction is
		// satisfied". That is only true of a coordinate pinned to ONE physical
		// key, where every admitted row shares one SORT value.
		//
		// A signed-zero-widened equality is not such a coordinate. The
		// PREDICATE comparator makes -0.0 and +0.0 equal, which is why the
		// equality admits both; the SORT comparator (values.CompareFloat64,
		// faithful to java.lang.Double.compare) ranks -0.0 BELOW +0.0, which is
		// why they are two distinct ORDER BY values. The coordinate is ordered,
		// but only in the direction the scan runs — so it binds SORTED, not
		// FIXED. Calling it FIXED elides the sort on `WHERE z = 0.0 ORDER BY z
		// DESC` and answers it from a FORWARD scan, returning the two zero
		// blocks ascending.
		if EqualityPinsSinglePhysicalKey(comps[i]) {
			bm[key] = []properties.OrderingBinding{properties.FixedBinding(comps[i])}
		} else {
			bm[key] = []properties.OrderingBinding{properties.SortedBinding(dir)}
		}
	}
	for _, col := range tail {
		key := orderingColumnOfName(p.GetResultValue(), p.GetFlowedType(), col)
		if key == nil {
			return properties.EmptyOrdering()
		}
		keys = append(keys, key)
		bm[key] = []properties.OrderingBinding{properties.SortedBinding(dir)}
	}
	if len(keys) == 0 {
		return properties.EmptyOrdering()
	}
	// Java passes RecordQueryIndexPlan.isStrictlySorted(), not the index
	// metadata's UNIQUE bit. A unique index is only a distinct ordering once
	// the chosen ordering keys cover that uniqueness proof; RemoveSort marks
	// that exact plan strictly sorted after checking the coverage.
	return properties.NewRichOrdering(bm, keys,
		properties.DistinctOverAllKeysIf(p.IsStrictlySorted())).
		WithStorageKeyComplete(storageComplete)
}

// HintRichOrdering: an HNSW probe returns its neighbours in distance order,
// which is not a column ordering the planner models. Empty rather than a
// synthesized fallback, so no caller mistakes distance order for key order.
func (p *RecordQueryVectorIndexPlan) HintRichOrdering() *properties.RichOrdering {
	return properties.EmptyOrdering()
}

// HintRichOrdering: fetching the full record per index entry preserves the
// index scan's rich ordering, so inherit it from the source.
//
// This DUPLICATES the physical wrapper's body rather than delegating to it,
// for the reason this file's header gives: same loop, but the wrapper walks a
// shared memo group while the plan walks the fresh singleton its own child
// quantifier ranges over. Two questions, two answers.
func (p *RecordQueryFetchFromPartialRecordPlan) HintRichOrdering() *properties.RichOrdering {
	return richOrderingOf(p.OrderingSourceRef())
}
