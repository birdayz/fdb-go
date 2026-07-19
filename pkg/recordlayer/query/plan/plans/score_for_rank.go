package plans

import (
	"fmt"
	"hash/fnv"
	"strings"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// ScoreForRank is a single conversion of a rank to a score to be
// bound to some name. Mirrors Java's
// RecordQueryScoreForRankPlan.ScoreForRank inner class.
//
// Fields:
//   - BindingName: the parameter name the converted score is bound to.
//   - FunctionName: the aggregate function name (e.g. "rank").
//   - IndexName: the index the rank function operates over.
//   - Comparisons: human-readable comparison descriptions (structure
//     only — no execution logic in this port).
type ScoreForRank struct {
	BindingName  string
	FunctionName string
	IndexName    string
	Comparisons  []string // typeless comparison strings
}

// String renders "bindingName = indexName.functionName(comp1, comp2)".
func (s *ScoreForRank) String() string {
	return s.BindingName + " = " + s.CallString()
}

// CallString renders "indexName.functionName(comp1, comp2)".
func (s *ScoreForRank) CallString() string {
	return s.IndexName + "." + s.FunctionName + "(" + strings.Join(s.Comparisons, ", ") + ")"
}

// RecordQueryScoreForRankPlan wraps an inner plan and evaluates
// rank/score functions, binding the results into the evaluation
// context so the inner plan can use them as parameters. Mirrors
// Java's RecordQueryScoreForRankPlan.
//
// This is a STRUCTURE-ONLY port — no execution logic.
type RecordQueryScoreForRankPlan struct {
	PlanExprBase
	innerQ expressions.Quantifier
	ranks  []ScoreForRank
}

// NewRecordQueryScoreForRankPlan constructs a score-for-rank plan.
func NewRecordQueryScoreForRankPlan(inner RecordQueryPlan, ranks []ScoreForRank) *RecordQueryScoreForRankPlan {
	copied := make([]ScoreForRank, len(ranks))
	copy(copied, ranks)
	return &RecordQueryScoreForRankPlan{
		innerQ: QuantifierOverPlan(inner),
		ranks:  copied,
	}
}

// GetInner returns the wrapped inner plan, dereferenced through the quantifier.
func (p *RecordQueryScoreForRankPlan) GetInner() RecordQueryPlan {
	return planFromQuantifier(p.innerQ)
}

// GetQuantifiers reports the real child quantifier, overriding
// PlanExprBase's none.
func (p *RecordQueryScoreForRankPlan) GetQuantifiers() []expressions.Quantifier {
	if p.innerQ.GetRangesOver() == nil {
		return nil
	}
	return []expressions.Quantifier{p.innerQ}
}

// GetRanks returns the list of ScoreForRank entries.
func (p *RecordQueryScoreForRankPlan) GetRanks() []ScoreForRank { return p.ranks }

// IsReverse delegates to the inner plan.
func (p *RecordQueryScoreForRankPlan) IsReverse() bool {
	if c, ok := p.GetInner().(interface{ IsReverse() bool }); ok {
		return c.IsReverse()
	}
	return false
}

// GetResultType returns the inner plan's result type (score-for-rank
// doesn't reshape rows — it binds scores into the evaluation context,
// then delegates row production to the inner plan).
func (p *RecordQueryScoreForRankPlan) GetResultType() values.Type {
	inner := p.GetInner()
	if inner == nil {
		return values.UnknownType
	}
	return inner.GetResultType()
}

// GetChildren returns the inner plan as the only child.
func (p *RecordQueryScoreForRankPlan) GetChildren() []RecordQueryPlan {
	inner := p.GetInner()
	if inner == nil {
		return nil
	}
	return []RecordQueryPlan{inner}
}

// EqualsWithoutChildren compares the ranks list.
func (p *RecordQueryScoreForRankPlan) EqualsPlanWithoutChildren(other RecordQueryPlan) bool {
	o, ok := other.(*RecordQueryScoreForRankPlan)
	if !ok {
		return false
	}
	if len(p.ranks) != len(o.ranks) {
		return false
	}
	for i := range p.ranks {
		if p.ranks[i].BindingName != o.ranks[i].BindingName {
			return false
		}
		if p.ranks[i].FunctionName != o.ranks[i].FunctionName {
			return false
		}
		if p.ranks[i].IndexName != o.ranks[i].IndexName {
			return false
		}
		if len(p.ranks[i].Comparisons) != len(o.ranks[i].Comparisons) {
			return false
		}
		for j := range p.ranks[i].Comparisons {
			if p.ranks[i].Comparisons[j] != o.ranks[i].Comparisons[j] {
				return false
			}
		}
	}
	return true
}

// HashCodeWithoutChildren mixes the class discriminator + ranks.
func (p *RecordQueryScoreForRankPlan) HashCodeWithoutChildren() uint64 {
	h := fnv.New64a()
	h.Write([]byte("scoreforrank|"))
	for _, r := range p.ranks {
		h.Write([]byte(r.BindingName))
		h.Write([]byte{0})
		h.Write([]byte(r.FunctionName))
		h.Write([]byte{0})
		h.Write([]byte(r.IndexName))
		h.Write([]byte{0})
		for _, c := range r.Comparisons {
			h.Write([]byte(c))
			h.Write([]byte{0})
		}
	}
	return h.Sum64()
}

// Explain renders ScoreForRank([rank1, rank2], inner).
func (p *RecordQueryScoreForRankPlan) Explain() string {
	innerLabel := "<nil>"
	if inner := p.GetInner(); inner != nil {
		innerLabel = inner.Explain()
	}
	parts := make([]string, len(p.ranks))
	for i, r := range p.ranks {
		parts[i] = r.String()
	}
	return fmt.Sprintf("ScoreForRank([%s], %s)", strings.Join(parts, "; "), innerLabel)
}

var (
	_ RecordQueryPlan                  = (*RecordQueryScoreForRankPlan)(nil)
	_ expressions.RelationalExpression = (*RecordQueryScoreForRankPlan)(nil)
)

// EqualsWithoutChildren is the RelationalExpression-shaped comparison; see
// planEqualsAsExpression.
func (p *RecordQueryScoreForRankPlan) EqualsWithoutChildren(other expressions.RelationalExpression, _ *expressions.AliasMap) bool {
	return planEqualsAsExpression(p, other)
}

// WithQuantifiers returns a copy ranging over the given child quantifier —
// Java's copy-on-write withChild(Reference).
func (p *RecordQueryScoreForRankPlan) WithQuantifiers(qs []expressions.Quantifier) expressions.RelationalExpression {
	if len(qs) != 1 {
		return p
	}
	cp := *p
	cp.innerQ = qs[0]
	return &cp
}

// GetRecordQueryPlan returns the plan itself.
func (p *RecordQueryScoreForRankPlan) GetRecordQueryPlan() RecordQueryPlan { return p }
