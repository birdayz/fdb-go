package expressions

import (
	"hash/fnv"

	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/values"
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
// preferences to the executor. Hints land when a rule actually
// consults them; the seed accepts the record-types set + flowed Type
// only.
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

// GetResultValue is a fresh QuantifiedObjectValue. The scan is a
// source — it allocates its own CorrelationIdentifier-equivalent. We
// approximate by re-using a unique CorrelationIdentifier per call,
// which means every read of GetResultValue produces a distinct Value
// (Java caches in a Suppliers.memoize). For the seed this is fine —
// callers that need stable identity should bind via a Quantifier
// (which ranges over the Reference holding this expression).
func (e *FullUnorderedScanExpression) GetResultValue() values.Value {
	return values.NewQuantifiedObjectValue(values.UniqueCorrelationIdentifier())
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
func (e *FullUnorderedScanExpression) EqualsWithoutChildren(other RelationalExpression, _ *AliasMap) bool {
	o, ok := other.(*FullUnorderedScanExpression)
	if !ok {
		return false
	}
	if !typeEquals(e.flowedType, o.flowedType) {
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
// the canonical record-type list.
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
