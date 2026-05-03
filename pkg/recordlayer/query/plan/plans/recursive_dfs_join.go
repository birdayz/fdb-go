package plans

import (
	"fmt"
	"hash/fnv"
	"strings"

	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/values"
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
type RecordQueryRecursiveDfsJoinPlan struct {
	root              RecordQueryPlan
	child             RecordQueryPlan
	priorCorrelation  values.CorrelationIdentifier
	traversalStrategy DfsTraversalStrategy
}

func NewRecordQueryRecursiveDfsJoinPlan(
	root, child RecordQueryPlan,
	priorCorrelation values.CorrelationIdentifier,
	strategy DfsTraversalStrategy,
) *RecordQueryRecursiveDfsJoinPlan {
	return &RecordQueryRecursiveDfsJoinPlan{
		root:              root,
		child:             child,
		priorCorrelation:  priorCorrelation,
		traversalStrategy: strategy,
	}
}

func (p *RecordQueryRecursiveDfsJoinPlan) GetRoot() RecordQueryPlan  { return p.root }
func (p *RecordQueryRecursiveDfsJoinPlan) GetChild() RecordQueryPlan { return p.child }

func (p *RecordQueryRecursiveDfsJoinPlan) GetPriorCorrelation() values.CorrelationIdentifier {
	return p.priorCorrelation
}

func (p *RecordQueryRecursiveDfsJoinPlan) GetTraversalStrategy() DfsTraversalStrategy {
	return p.traversalStrategy
}

func (p *RecordQueryRecursiveDfsJoinPlan) GetResultType() values.Type { return values.UnknownType }

func (p *RecordQueryRecursiveDfsJoinPlan) GetChildren() []RecordQueryPlan {
	return []RecordQueryPlan{p.root, p.child}
}

func (p *RecordQueryRecursiveDfsJoinPlan) EqualsWithoutChildren(other RecordQueryPlan) bool {
	o, ok := other.(*RecordQueryRecursiveDfsJoinPlan)
	if !ok {
		return false
	}
	return p.priorCorrelation == o.priorCorrelation && p.traversalStrategy == o.traversalStrategy
}

func (p *RecordQueryRecursiveDfsJoinPlan) HashCodeWithoutChildren() uint64 {
	h := fnv.New64a()
	h.Write([]byte("recursivedfs|"))
	h.Write([]byte(p.priorCorrelation.Name()))
	h.Write([]byte{byte(p.traversalStrategy)})
	return h.Sum64()
}

func (p *RecordQueryRecursiveDfsJoinPlan) Explain() string {
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("RecursiveDfsJoin(%s, ", p.traversalStrategy))
	if p.root != nil {
		sb.WriteString(p.root.Explain())
	}
	sb.WriteString(", ")
	if p.child != nil {
		sb.WriteString(p.child.Explain())
	}
	sb.WriteString(")")
	return sb.String()
}

var _ RecordQueryPlan = (*RecordQueryRecursiveDfsJoinPlan)(nil)
