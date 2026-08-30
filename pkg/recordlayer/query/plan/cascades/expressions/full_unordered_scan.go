package expressions

import (
	"hash/fnv"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// FullUnorderedScanExpression is the leaf RelationalExpression used as
// the base of every query tree — the planner inserts one over the
// queried record types before the SQL parser builds anything else on
// top of it. Zero Quantifiers (it's a source, not a transformer).
//
// Ports the structural surface of Java's
// `com.apple.foundationdb.record.query.plan.cascades.expressions.FullUnorderedScanExpression`.
// Java's full implementation includes an `AccessHints` set — a
// hint-plumbing struct used by rules to communicate ordering
// preferences to the executor. No Go rule consults access hints, so
// Go carries the record-types set + flowed Type only.
type FullUnorderedScanExpression struct {
	recordTypes []string // sorted, deduped — canonical form for equality + hash
	flowedType  values.ExactTypeHandle
	// resultValue is derived once at construction rather than per call. It is a
	// pure function of the two fields above, both fixed here, and a scan.s
	// result value is read on nearly every visit to the leaf: rebuilding it
	// thawed the whole flowed type graph each time, 1GB over the pure-planner
	// sweep. Sharing one immutable QueriedValue is also strictly stronger than
	// the equal-but-distinct values it replaces.
	resultValue values.Value
}

// NewFullUnorderedScanExpression builds a scan over the given record-
// type names with the given flowed Type. recordTypes is normalised
// (sorted + deduped); empty slice → scan over all types (caller's
// responsibility to attach the right type metadata for that case).
func NewFullUnorderedScanExpression(recordTypes []string, flowedType values.Type) (*FullUnorderedScanExpression, error) {
	exactType, err := snapshotExpressionResultType("FullUnorderedScanExpression", flowedType)
	if err != nil {
		return nil, err
	}
	canonicalTypes := dedupSortedStrings(recordTypes)
	return &FullUnorderedScanExpression{
		recordTypes: canonicalTypes,
		flowedType:  exactType,
		resultValue: values.NewQueriedValue(canonicalTypes, exactType.Type()),
	}, nil
}

// GetRecordTypes returns the canonical record-type-name list.
func (e *FullUnorderedScanExpression) GetRecordTypes() []string {
	return e.recordTypes
}

// GetFlowedType returns the rich Type of rows flowing out of the scan.
func (e *FullUnorderedScanExpression) GetFlowedType() values.Type {
	return e.flowedType.Type()
}

// GetResultValue is a QueriedValue carrying the scan's flowed record Type,
// matching Java's `new QueriedValue(flowedType)`.
//
// A scan is a source: what it flows is "the queried record", which is not
// correlated to anything. QueriedValue says exactly that, and because it
// compares structurally, building a fresh one per call is free of
// consequence — two reads of the same scan are equal values.
//
// A QuantifiedObjectValue over a freshly minted correlation identifier, which
// is what this used to return, says something different and false: that the
// scan's output is correlated to a quantifier nobody introduced. Two reads
// then produce unequal values, which silently breaks every consumer that
// reads the result value more than once. It did: a leaf match stored
// MaxMatchMap(candidateResultValue) at match time, the pull-up later re-read
// GetResultValue for the same expression and got a different alias, and the
// two could no longer be bridged — compensation went impossible and the match
// was discarded. That is why the leaf matcher had to model the identity by
// hand instead of nesting the pull-up the way Java does.
//
// The value flows e.flowedType: Java's scan quantifier result type is always
// the record type, and FieldValue.resolveOrdinal resolves a column name to
// its ordinal against this child Type. Passing UnknownType here would
// silently discard the flowed type (resolveOrdinal's *RecordType assertion
// fails → (0,false)); with no name fallback, that failure is a loud
// unbaked-ref error, so carrying the real flowedType is load-bearing. A
// nil/UnknownType flowedType still degrades cleanly (NewQueriedValue falls
// back to UnknownType) rather than panicking.
func (e *FullUnorderedScanExpression) GetResultValue() values.Value {
	return e.resultValue
}

// GetQuantifiers returns the empty list — leaf.
func (e *FullUnorderedScanExpression) GetQuantifiers() []Quantifier { return nil }

// CanCorrelate is false — leaf has no children, no inter-child
// correlation possible.
func (e *FullUnorderedScanExpression) CanCorrelate() bool { return false }

// ChildrenAsSet is false — leaf has no children.
func (e *FullUnorderedScanExpression) ChildrenAsSet() bool { return false }

// GetCorrelatedToWithoutChildren returns the empty set — scans are
// closed over no upstream Quantifiers.
func (e *FullUnorderedScanExpression) GetCorrelatedToWithoutChildren() map[values.CorrelationIdentifier]struct{} {
	return map[values.CorrelationIdentifier]struct{}{}
}

// EqualsWithoutChildren compares record-type sets + flowed Type, and it is what
// rule_match_leaf.go's matchLeafWithCandidate calls to decide whether a query
// scan is subsumed by a candidate scan. Every index access path in the planner
// depends on that comparison returning true for the right pairs.
//
// WHAT MAKES IT RETURN TRUE is the exact channel's identity excluding the
// record NAME. The query scan is built over values.NewRecordType("", cols) —
// unnamed, from the SQL columns (cascades_translator.go) — while the candidate
// scan is built "over the candidate's exact row" (index_expansion.go), whose
// record type carries the table's name. Those two only compare equal because
// exactType's canonical form omits the name: it is "provenance, not shape, and
// Java's Type.Record/Type.Enum equals+hashCode exclude it too" (exact_type.go).
// Adding the name back there would make every leaf match fail and remove index
// access wholesale, which is why the dependency is stated here rather than left
// to be rediscovered from the far end.
// TestFullUnorderedScan_MatchesAcrossRecordNames pins it.
//
// There is NO UnknownType wildcard, and an earlier version of this comment
// described one at length: that the flowed type is "non-discriminating when
// either side is UnknownType", that candidate scans "keep UnknownType", and
// that wildcarding it is what scan-leaf subsumption needs. All three are false.
// values.ExactTypesEqual is a strict canonical-bytes comparison with no
// wildcard arm; candidate scans carry the candidate's exact base type; and an
// UnknownType scan cannot be constructed at all, because
// NewFullUnorderedScanExpression snapshots through snapshotExpressionResultType,
// which refuses a placeholder. Structural typing on both sides replaced the
// wildcard; the prose describing it stayed.
func (e *FullUnorderedScanExpression) EqualsWithoutChildren(other RelationalExpression, _ *AliasMap) bool {
	o, ok := other.(*FullUnorderedScanExpression)
	if !ok {
		return false
	}
	if !values.ExactTypesEqual(e.flowedType, o.flowedType) {
		return false
	}
	if len(e.recordTypes) != len(o.recordTypes) {
		return false
	}
	for i := range e.recordTypes {
		if e.recordTypes[i] != o.recordTypes[i] {
			return false
		}
	}
	return true
}

// HashCodeWithoutChildren mixes a class-discriminating constant with the
// canonical record-type list, and deliberately does NOT mix flowedType. This
// DIVERGES FROM JAVA, which hashes Objects.hash(recordTypes, flowedType)
// (FullUnorderedScanExpression.java:150). Do not "align" it; that was tried.
//
// An earlier version of this comment claimed the omission was "matching Java's
// names-only scan hash". Java's scan hash is not names-only, so that was a false
// Java citation introduced by the very commit that removed a different one —
// worth recording, because an unchecked claim about the reference implementation
// reads exactly like a checked one.
//
// Folding flowedType in, to close that divergence, REGRESSES THE PLANNER.
// Measured: it reddens TestPlanShapeGolden by 13731 lines and breaks three memo
// tests (TestDesignatedFinal_GenerationInvalidation,
// TestDesignatedFinal_NoCacheInUnfinalizedWindow,
// TestOptimizeGroup_RewritingCoherence), all of them selecting a
// LogicalSortExpression where a plain scan should win. Scan identity is the base
// of every query tree, so changing which scans share a memo bucket changes group
// membership and therefore winner selection.
//
// The omission is SAFE on its own terms, which is why it is kept rather than
// merely tolerated: a hash folding FEWER fields than equality only ever collides
// unequal expressions and never scatters equal ones, so the equal-implies-
// same-hash invariant the memo needs is untouched. EqualsWithoutChildren still
// compares the flowed type, so nothing is conflated — the two just bucket
// together.
//
// An earlier version of this comment justified the exclusion differently — that
// EqualsWithoutChildren treats an UnknownType flowedType as a wildcard, so a
// typed query scan and an UnknownType candidate scan had to share a bucket or
// the wildcard match would never fire. Both halves of that are false.
// ExactTypesEqual is a strict canonical-bytes comparison with no wildcard arm,
// and an UnknownType scan cannot be built at all: NewFullUnorderedScanExpression
// snapshots through snapshotExpressionResultType, which refuses a placeholder
// type ("placeholder type is not exact"), and that constructor is the only
// writer of the field. TestFullUnorderedScan_RefusesAPlaceholderFlowedType pins
// the refusal, so if it is ever relaxed this reasoning gets revisited rather
// than silently inherited.
func (e *FullUnorderedScanExpression) HashCodeWithoutChildren() uint64 {
	h := fnv.New64a()
	h.Write([]byte("scan|"))
	for _, name := range e.recordTypes {
		h.Write([]byte(name))
		h.Write([]byte{0})
	}
	return h.Sum64()
}

func (e *FullUnorderedScanExpression) WithQuantifiers(quantifiers []Quantifier) (RelationalExpression, error) {
	if err := requireQuantifierArity("FullUnorderedScanExpression", len(quantifiers), 0); err != nil {
		return nil, err
	}
	return e, nil
}

var _ RelationalExpression = (*FullUnorderedScanExpression)(nil)
