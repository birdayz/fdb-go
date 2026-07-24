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
	flowedType  values.Type
}

// NewFullUnorderedScanExpression builds a scan over the given record-
// type names with the given flowed Type. recordTypes is normalised
// (sorted + deduped); empty slice → scan over all types (caller's
// responsibility to attach the right type metadata for that case).
func NewFullUnorderedScanExpression(recordTypes []string, flowedType values.Type) *FullUnorderedScanExpression {
	if flowedType == nil {
		flowedType = values.UnknownType
	}
	return &FullUnorderedScanExpression{
		recordTypes: dedupSortedStrings(recordTypes),
		flowedType:  flowedType,
	}
}

// GetRecordTypes returns the canonical record-type-name list.
func (e *FullUnorderedScanExpression) GetRecordTypes() []string {
	return e.recordTypes
}

// GetFlowedType returns the rich Type of rows flowing out of the scan.
func (e *FullUnorderedScanExpression) GetFlowedType() values.Type {
	return e.flowedType
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
	return values.NewQueriedValue(e.recordTypes, e.flowedType)
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

// EqualsWithoutChildren compares record-type sets + flowed Type.
//
// The flowed type is NON-DISCRIMINATING when either side is UnknownType. Java
// holds this invariant structurally: both the query scan
// (RelationalExpression.fromRecordQuery) and the candidate scan
// (ExpansionVisitor.createBaseRef) flow Type.AnyRecord — a constant TOP type —
// so its flowedType.equals term is always AnyRecord==AnyRecord and never
// discriminates; the concrete record type rides a TypeFilter ABOVE the scan,
// never on the leaf. recordTypes NAMES are the sole discriminator.
//
// Go's UnknownType is the analog of Java's AnyRecord. The QUERY scan leaf is
// typed directly (so FieldValue.resolveOrdinal can resolve a column
// against it) while candidate scans keep UnknownType. Wildcarding UnknownType
// here restores Java's names-only match — top subsumes concrete, the direction
// scan-leaf subsumption (rule_match_leaf.go) needs. Two CONCRETE types still
// compare structurally, so query-side memo dedup of two scans over one table is
// preserved. HashCodeWithoutChildren stays names-only (below) so typed and
// untyped scans over the same types share a bucket and can meet here.
func (e *FullUnorderedScanExpression) EqualsWithoutChildren(other RelationalExpression, _ *AliasMap) bool {
	o, ok := other.(*FullUnorderedScanExpression)
	if !ok {
		return false
	}
	if e.flowedType != values.UnknownType && o.flowedType != values.UnknownType &&
		!typeEquals(e.flowedType, o.flowedType) {
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

// HashCodeWithoutChildren mixes a class-discriminating constant with
// the canonical record-type list. It MUST NOT mix flowedType — matching
// Java's names-only scan hash. EqualsWithoutChildren treats a UnknownType
// flowedType as a wildcard, so a typed query scan and an
// UnknownType candidate scan over the same record types must hash IDENTICALLY
// or they land in different memo buckets and the wildcard match never fires.
func (e *FullUnorderedScanExpression) HashCodeWithoutChildren() uint64 {
	h := fnv.New64a()
	h.Write([]byte("scan|"))
	for _, name := range e.recordTypes {
		h.Write([]byte(name))
		h.Write([]byte{0})
	}
	return h.Sum64()
}

func (e *FullUnorderedScanExpression) WithQuantifiers(_ []Quantifier) RelationalExpression {
	return e
}

var _ RelationalExpression = (*FullUnorderedScanExpression)(nil)
