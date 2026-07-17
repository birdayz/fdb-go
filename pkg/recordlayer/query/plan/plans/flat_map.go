package plans

import (
	"fmt"
	"hash/fnv"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// RecordQueryFlatMapPlan represents a correlated nested-loop join where
// for each outer row, the inner plan is re-executed with the outer row
// bound as a correlation. Mirrors Java's RecordQueryFlatMapPlan which
// uses FlatMapPipelinedCursor for execution.
//
// The key difference from RecordQueryNestedLoopJoinPlan: the inner plan
// is parameterized by the outer row via correlation bindings. This
// enables targeted index probes on the inner side (O(N×logM) vs O(N×M)).
// LEFT-OUTER note: the plan carries NO leftOuter flag. LEFT-OUTER semantics are
// emergent from the inner being wrapped in DefaultOnEmpty (whose OrElse
// continuation makes the null-extension resume-safe), exactly like Java's
// RecordQueryFlatMapPlan — see rule_implement_nested_loop_join.go's lowering. An
// earlier in-memory leftOuter/innerHadMatch flag pair re-decided the extension per
// page and was the F2 spurious-null resume bug; it was removed as dead code.
type RecordQueryFlatMapPlan struct {
	outer                        RecordQueryPlan
	inner                        RecordQueryPlan
	outerAlias                   values.CorrelationIdentifier
	innerAlias                   values.CorrelationIdentifier
	resultValue                  values.Value
	inheritOuterRecordProperties bool
}

func NewRecordQueryFlatMapPlan(
	outer, inner RecordQueryPlan,
	outerAlias, innerAlias values.CorrelationIdentifier,
	resultValue values.Value,
	inheritOuterRecordProperties bool,
) *RecordQueryFlatMapPlan {
	return &RecordQueryFlatMapPlan{
		outer:                        outer,
		inner:                        inner,
		outerAlias:                   outerAlias,
		innerAlias:                   innerAlias,
		resultValue:                  resultValue,
		inheritOuterRecordProperties: inheritOuterRecordProperties,
	}
}

func (p *RecordQueryFlatMapPlan) GetResultType() values.Type { return values.UnknownType }

func (p *RecordQueryFlatMapPlan) GetChildren() []RecordQueryPlan {
	return []RecordQueryPlan{p.outer, p.inner}
}

func (p *RecordQueryFlatMapPlan) GetOuter() RecordQueryPlan                   { return p.outer }
func (p *RecordQueryFlatMapPlan) GetInner() RecordQueryPlan                   { return p.inner }
func (p *RecordQueryFlatMapPlan) GetOuterAlias() values.CorrelationIdentifier { return p.outerAlias }
func (p *RecordQueryFlatMapPlan) GetInnerAlias() values.CorrelationIdentifier { return p.innerAlias }
func (p *RecordQueryFlatMapPlan) GetResultValue() values.Value                { return p.resultValue }
func (p *RecordQueryFlatMapPlan) InheritOuterRecordProperties() bool {
	return p.inheritOuterRecordProperties
}

// EqualsWithoutChildren compares aliases, the resultValue (semantic Value
// identity — Java RecordQueryFlatMapPlan.equalsWithoutChildren is
// semanticEqualsForResults), and inheritOuterRecordProperties. Java folds
// inheritOuterRecordProperties into computeHashCodeWithoutChildren but omits
// it from equals — Go compares it in BOTH so the equal⟹same-hash memo
// invariant holds (two plans differing only in that flag null-extend
// differently; they are not interchangeable).
func (p *RecordQueryFlatMapPlan) EqualsWithoutChildren(other RecordQueryPlan) bool {
	o, ok := other.(*RecordQueryFlatMapPlan)
	if !ok {
		return false
	}
	if p.outerAlias != o.outerAlias || p.innerAlias != o.innerAlias {
		return false
	}
	if p.inheritOuterRecordProperties != o.inheritOuterRecordProperties {
		return false
	}
	return semanticValueEquals(p.resultValue, o.resultValue)
}

func (p *RecordQueryFlatMapPlan) HashCodeWithoutChildren() uint64 {
	h := fnv.New64a()
	h.Write([]byte("flatmap|"))
	h.Write([]byte(p.outerAlias.Name()))
	h.Write([]byte{0})
	h.Write([]byte(p.innerAlias.Name()))
	h.Write([]byte{0})
	if p.inheritOuterRecordProperties {
		h.Write([]byte{1})
	}
	writeValueHash(h, p.resultValue)
	return h.Sum64()
}

func (p *RecordQueryFlatMapPlan) Explain() string {
	return fmt.Sprintf("FlatMap(outer=%s, inner=%s)", p.outer.Explain(), p.inner.Explain())
}

var _ RecordQueryPlan = (*RecordQueryFlatMapPlan)(nil)
