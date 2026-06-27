package plans

import (
	"fmt"
	"hash/fnv"
	"strings"

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
	inner RecordQueryPlan
	ranks []ScoreForRank
}

// NewRecordQueryScoreForRankPlan constructs a score-for-rank plan.
func NewRecordQueryScoreForRankPlan(inner RecordQueryPlan, ranks []ScoreForRank) *RecordQueryScoreForRankPlan {
	copied := make([]ScoreForRank, len(ranks))
	copy(copied, ranks)
	return &RecordQueryScoreForRankPlan{
		inner: inner,
		ranks: copied,
	}
}

// GetInner returns the wrapped inner plan.
func (p *RecordQueryScoreForRankPlan) GetInner() RecordQueryPlan { return p.inner }

// GetRanks returns the list of ScoreForRank entries.
func (p *RecordQueryScoreForRankPlan) GetRanks() []ScoreForRank { return p.ranks }

// IsReverse delegates to the inner plan.
func (p *RecordQueryScoreForRankPlan) IsReverse() bool {
	if c, ok := p.inner.(interface{ IsReverse() bool }); ok {
		return c.IsReverse()
	}
	return false
}

// GetResultType returns the inner plan's result type (score-for-rank
// doesn't reshape rows — it binds scores into the evaluation context,
// then delegates row production to the inner plan).
func (p *RecordQueryScoreForRankPlan) GetResultType() values.Type {
	if p.inner == nil {
		return values.UnknownType
	}
	return p.inner.GetResultType()
}

// GetChildren returns the inner plan as the only child.
func (p *RecordQueryScoreForRankPlan) GetChildren() []RecordQueryPlan {
	if p.inner == nil {
		return nil
	}
	return []RecordQueryPlan{p.inner}
}

// EqualsWithoutChildren compares the ranks list.
func (p *RecordQueryScoreForRankPlan) EqualsWithoutChildren(other RecordQueryPlan) bool {
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
	if p.inner != nil {
		innerLabel = p.inner.Explain()
	}
	parts := make([]string, len(p.ranks))
	for i, r := range p.ranks {
		parts[i] = r.String()
	}
	return fmt.Sprintf("ScoreForRank([%s], %s)", strings.Join(parts, "; "), innerLabel)
}

var _ RecordQueryPlan = (*RecordQueryScoreForRankPlan)(nil)
