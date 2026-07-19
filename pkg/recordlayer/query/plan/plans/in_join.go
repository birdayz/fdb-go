package plans

import (
	"fmt"
	"hash/fnv"
	"reflect"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// InSourceKind distinguishes the three Java InJoin subclasses.
type InSourceKind int

const (
	InSourceValues    InSourceKind = iota // static value list (InValuesJoinPlan)
	InSourceParameter                     // runtime parameter binding (InParameterJoinPlan)
	InSourceComparand                     // comparand from correlated subquery (InComparandJoinPlan)
)

// RecordQueryInJoinPlan executes its inner plan once for each value
// from an IN-source, binding the value to a correlation variable.
// The result is the concatenation of all inner executions.
//
// Mirrors Java's RecordQueryInJoinPlan hierarchy
// (InValuesJoin, InParameterJoin, InComparandJoin).
type RecordQueryInJoinPlan struct {
	PlanExprBase
	innerQ      expressions.Quantifier
	bindingName string
	sorted      bool
	reverse     bool
	inValues    []any
	sourceKind  InSourceKind
}

func NewRecordQueryInJoinPlan(
	inner RecordQueryPlan,
	bindingName string,
	sorted bool,
	reverse bool,
) *RecordQueryInJoinPlan {
	return &RecordQueryInJoinPlan{
		innerQ:      QuantifierOverPlan(inner),
		bindingName: bindingName,
		sorted:      sorted,
		reverse:     reverse,
	}
}

func (p *RecordQueryInJoinPlan) GetInner() RecordQueryPlan { return planFromQuantifier(p.innerQ) }

// GetQuantifiers reports the real child quantifier, overriding
// PlanExprBase's none.
func (p *RecordQueryInJoinPlan) GetQuantifiers() []expressions.Quantifier {
	if p.innerQ.GetRangesOver() == nil {
		return nil
	}
	return []expressions.Quantifier{p.innerQ}
}

// WithInner returns a copy with the inner replaced and EVERY other field
// preserved — the extraction-relink rebuild path; reconstructing via the
// constructor risks silently dropping fields the setters carry.
func (p *RecordQueryInJoinPlan) WithInner(inner RecordQueryPlan) *RecordQueryInJoinPlan {
	cp := *p
	cp.innerQ = QuantifierOverPlan(inner)
	return &cp
}
func (p *RecordQueryInJoinPlan) GetBindingName() string       { return p.bindingName }
func (p *RecordQueryInJoinPlan) IsSorted() bool               { return p.sorted }
func (p *RecordQueryInJoinPlan) IsReverse() bool              { return p.reverse }
func (p *RecordQueryInJoinPlan) GetInValues() []any           { return p.inValues }
func (p *RecordQueryInJoinPlan) SetInValues(vals []any)       { p.inValues = vals }
func (p *RecordQueryInJoinPlan) GetSourceKind() InSourceKind  { return p.sourceKind }
func (p *RecordQueryInJoinPlan) SetSourceKind(k InSourceKind) { p.sourceKind = k }

func (p *RecordQueryInJoinPlan) GetResultType() values.Type {
	if inner := p.GetInner(); inner != nil {
		return inner.GetResultType()
	}
	return values.UnknownType
}

func (p *RecordQueryInJoinPlan) GetChildren() []RecordQueryPlan {
	inner := p.GetInner()
	if inner == nil {
		return nil
	}
	return []RecordQueryPlan{inner}
}

func (p *RecordQueryInJoinPlan) EqualsPlanWithoutChildren(other RecordQueryPlan) bool {
	o, ok := other.(*RecordQueryInJoinPlan)
	if !ok {
		return false
	}
	// bindingName is DELIBERATELY excluded: it is an internal correlation alias
	// minted by UniqueCorrelationIdentifier (a process-global counter), so two
	// structurally-identical InJoins that differ only in the arbitrary alias are
	// the SAME plan. Including it made every replanned IN-query non-equal and
	// differently-hashed → plan-cache churn + nondeterministic Explain (RFC-164
	// WS-4). Identity is alias-invariant; the real alias is retained on the field
	// for execution (GetBindingName), which is unaffected.
	//
	// inValues IS included: the static IN-list is the join's comparand — an
	// InJoin over (1,2,3) binds different values than one over (4,5,6) and is a
	// DIFFERENT plan. Excluding it let the memo collapse the two (an InJoin is a
	// non-leaf node deduped by HashCodeWithoutChildren + EqualsWithoutChildren),
	// mirroring the F21 comparand-blind index-scan defect. Java's
	// InValuesJoinPlan.equalsWithoutChildren compares Objects.equals(values, ...).
	return p.sorted == o.sorted && p.reverse == o.reverse && inValuesEqual(p.inValues, o.inValues)
}

func (p *RecordQueryInJoinPlan) HashCodeWithoutChildren() uint64 {
	h := fnv.New64a()
	h.Write([]byte("injoinplan|"))
	// bindingName excluded — see EqualsWithoutChildren (alias-invariant identity).
	if p.sorted {
		h.Write([]byte{1})
	}
	if p.reverse {
		h.Write([]byte{2})
	}
	// inValues folded — hash analog of EqualsWithoutChildren (see there). %#v
	// pins Go type + value, so inValuesEqual-equal lists (identical types +
	// values) fold identically; the leading length keeps distinct-length lists
	// apart cheaply.
	fmt.Fprintf(h, "|inv:%d:%#v", len(p.inValues), p.inValues)
	return h.Sum64()
}

// inValuesEqual compares two static IN-list comparands element-wise. The lists
// hold plan-time-evaluated literals ([]any of int64 / string / []byte / …);
// reflect.DeepEqual is the safe faithful equivalent of Java List.equals here —
// it never panics on the non-comparable slice carriers (e.g. []byte) that a raw
// `==` would, and treats a nil list and an empty list as distinct exactly as
// Java's null-vs-empty-list does.
func inValuesEqual(a, b []any) bool {
	if len(a) == 0 && len(b) == 0 {
		return (a == nil) == (b == nil)
	}
	return reflect.DeepEqual(a, b)
}

func (p *RecordQueryInJoinPlan) Explain() string {
	innerLabel := "<nil>"
	if inner := p.GetInner(); inner != nil {
		innerLabel = inner.Explain()
	}
	dir := ""
	if p.sorted {
		if p.reverse {
			dir = " DESC"
		} else {
			dir = " ASC"
		}
	}
	// The binding correlation alias (a process-global unique counter) is NOT
	// rendered — it varies per planning invocation and would make the Explain
	// nondeterministic (RFC-164 WS-4); "binding" marks its presence structurally.
	return fmt.Sprintf("InJoin(%s, binding%s)", innerLabel, dir)
}

var (
	_ RecordQueryPlan                  = (*RecordQueryInJoinPlan)(nil)
	_ expressions.RelationalExpression = (*RecordQueryInJoinPlan)(nil)
)

// EqualsWithoutChildren is the RelationalExpression-shaped comparison; see
// planEqualsAsExpression.
func (p *RecordQueryInJoinPlan) EqualsWithoutChildren(other expressions.RelationalExpression, _ *expressions.AliasMap) bool {
	return planEqualsAsExpression(p, other)
}

// WithQuantifiers returns a copy ranging over the given child quantifier —
// Java's copy-on-write withChild(Reference).
func (p *RecordQueryInJoinPlan) WithQuantifiers(qs []expressions.Quantifier) expressions.RelationalExpression {
	if len(qs) != 1 {
		return p
	}
	cp := *p
	cp.innerQ = qs[0]
	return &cp
}
