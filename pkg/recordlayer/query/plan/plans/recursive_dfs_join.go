package plans

import (
	"fmt"
	"hash/fnv"
	"strings"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// DfsTraversalStrategy selects pre-order vs post-order traversal for
// the recursive DFS join.
type DfsTraversalStrategy int

const (
	DfsPreorder DfsTraversalStrategy = iota
	DfsPostorder
)

func (s DfsTraversalStrategy) String() string {
	switch s {
	case DfsPreorder:
		return "PREORDER"
	case DfsPostorder:
		return "POSTORDER"
	}
	return "UNKNOWN"
}

// RecordQueryRecursiveDfsJoinPlan implements a recursive depth-first
// join: the root plan seeds the traversal, and the child plan is
// re-evaluated for each row using priorCorrelation to bind the
// "prior" row. Mirrors Java's
// `com.apple.foundationdb.record.query.plan.plans.RecordQueryRecursiveDfsJoinPlan`.
//
// The two legs are stored ONCE, as Quantifiers over References — Java's shape
// (`Quantifier.Physical` for the root and the recursive child). The raw
// `root`/`child` pointers they replace were a second storage location for the
// same edges. They stay two separately-named fields rather than a slice
// because the legs are not interchangeable: the root seeds the traversal and
// the child is re-evaluated per row against priorCorrelation. RFC-183 P5
// step 2.
type RecordQueryRecursiveDfsJoinPlan struct {
	PlanExprBase
	rootQ             expressions.Quantifier
	childQ            expressions.Quantifier
	priorCorrelation  values.CorrelationIdentifier
	traversalStrategy DfsTraversalStrategy
	distinct          bool // UNION DISTINCT deduplication for cycle detection
}

func NewRecordQueryRecursiveDfsJoinPlan(
	root, child RecordQueryPlan,
	priorCorrelation values.CorrelationIdentifier,
	strategy DfsTraversalStrategy,
) *RecordQueryRecursiveDfsJoinPlan {
	return &RecordQueryRecursiveDfsJoinPlan{
		rootQ:             QuantifierOverPlan(root),
		childQ:            QuantifierOverPlan(child),
		priorCorrelation:  priorCorrelation,
		traversalStrategy: strategy,
	}
}

// NewRecordQueryRecursiveDfsJoinPlanDistinct creates a DFS plan with
// UNION DISTINCT deduplication.
func NewRecordQueryRecursiveDfsJoinPlanDistinct(
	root, child RecordQueryPlan,
	priorCorrelation values.CorrelationIdentifier,
	strategy DfsTraversalStrategy,
) *RecordQueryRecursiveDfsJoinPlan {
	return &RecordQueryRecursiveDfsJoinPlan{
		rootQ:             QuantifierOverPlan(root),
		childQ:            QuantifierOverPlan(child),
		priorCorrelation:  priorCorrelation,
		traversalStrategy: strategy,
		distinct:          true,
	}
}

func (p *RecordQueryRecursiveDfsJoinPlan) IsDistinct() bool { return p.distinct }

func (p *RecordQueryRecursiveDfsJoinPlan) GetRoot() RecordQueryPlan {
	return planFromQuantifier(p.rootQ)
}

func (p *RecordQueryRecursiveDfsJoinPlan) GetChild() RecordQueryPlan {
	return planFromQuantifier(p.childQ)
}

func (p *RecordQueryRecursiveDfsJoinPlan) GetPriorCorrelation() values.CorrelationIdentifier {
	return p.priorCorrelation
}

func (p *RecordQueryRecursiveDfsJoinPlan) GetTraversalStrategy() DfsTraversalStrategy {
	return p.traversalStrategy
}

func (p *RecordQueryRecursiveDfsJoinPlan) GetResultType() values.Type { return values.UnknownType }

// GetChildren returns the root leg then the recursive child leg, dereferenced
// through the quantifiers. The pair is always two entries wide — a nil leg
// stays a nil entry rather than shrinking the arity.
func (p *RecordQueryRecursiveDfsJoinPlan) GetChildren() []RecordQueryPlan {
	return []RecordQueryPlan{p.GetRoot(), p.GetChild()}
}

// GetQuantifiers reports the real leg quantifiers in GetChildren order
// (root, child), overriding PlanExprBase's none. That order is what
// WithQuantifiers indexes into.
func (p *RecordQueryRecursiveDfsJoinPlan) GetQuantifiers() []expressions.Quantifier {
	if p.rootQ.GetRangesOver() == nil || p.childQ.GetRangesOver() == nil {
		return nil
	}
	return []expressions.Quantifier{p.rootQ, p.childQ}
}

// WithQuantifiers returns a copy ranging over the given leg quantifiers, in
// GetQuantifiers order. The receiver is never mutated, which is what keeps a
// memoized plan safe to share.
func (p *RecordQueryRecursiveDfsJoinPlan) WithQuantifiers(qs []expressions.Quantifier) expressions.RelationalExpression {
	if len(qs) != 2 {
		return p
	}
	cp := *p
	cp.rootQ = qs[0]
	cp.childQ = qs[1]
	return &cp
}

func (p *RecordQueryRecursiveDfsJoinPlan) EqualsPlanWithoutChildren(other RecordQueryPlan) bool {
	o, ok := other.(*RecordQueryRecursiveDfsJoinPlan)
	if !ok {
		return false
	}
	return p.priorCorrelation == o.priorCorrelation && p.traversalStrategy == o.traversalStrategy && p.distinct == o.distinct
}

func (p *RecordQueryRecursiveDfsJoinPlan) HashCodeWithoutChildren() uint64 {
	h := fnv.New64a()
	h.Write([]byte("recursivedfs|"))
	h.Write([]byte(p.priorCorrelation.Name()))
	h.Write([]byte{byte(p.traversalStrategy)})
	if p.distinct {
		h.Write([]byte{1})
	} else {
		h.Write([]byte{0})
	}
	return h.Sum64()
}

func (p *RecordQueryRecursiveDfsJoinPlan) Explain() string {
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("RecursiveDfsJoin(%s, ", p.traversalStrategy))
	if root := p.GetRoot(); root != nil {
		sb.WriteString(root.Explain())
	}
	sb.WriteString(", ")
	if child := p.GetChild(); child != nil {
		sb.WriteString(child.Explain())
	}
	sb.WriteString(")")
	return sb.String()
}

var (
	_ RecordQueryPlan                  = (*RecordQueryRecursiveDfsJoinPlan)(nil)
	_ expressions.RelationalExpression = (*RecordQueryRecursiveDfsJoinPlan)(nil)
)

// EqualsWithoutChildren is the RelationalExpression-shaped comparison; see
// planEqualsAsExpression.
func (p *RecordQueryRecursiveDfsJoinPlan) EqualsWithoutChildren(other expressions.RelationalExpression, _ *expressions.AliasMap) bool {
	return planEqualsAsExpression(p, other)
}

// CanCorrelate reports that this operator anchors a correlation between its
// children (the seed leg binds what the recursive leg reads), mirroring physicalRecursiveDfsJoinWrapper.
func (p *RecordQueryRecursiveDfsJoinPlan) CanCorrelate() bool { return true }
