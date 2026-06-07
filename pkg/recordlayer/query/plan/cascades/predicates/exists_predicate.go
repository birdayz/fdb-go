package predicates

import (
	"hash/fnv"

	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/values"
)

// ExistsPredicate is the SQL EXISTS predicate at the QueryPredicate
// layer: tests whether a subquery's result set is non-empty.
// Mirrors Java's `com.apple.foundationdb.record.query.plan.cascades.
// predicates.ExistsPredicate`.
//
//	EXISTS (SELECT ... FROM t WHERE ...)
//	  ↔  ExistsPredicate{ExistentialAlias: αsubq}
//
// The predicate is a LEAF — the subquery's plan tree is referenced
// indirectly via the existential alias (a CorrelationIdentifier
// that ranges over the subquery's Reference). The predicate
// evaluates to TRUE iff the subquery yields at least one row.
//
// Rules:
//   - The subquery's plan is materialized as a separate Reference
//     somewhere in the planner's DAG; the alias binds them.
//   - A NOT-EXISTS predicate is just NotPredicate(ExistsPredicate(α)).
//
// Evaluate is NOT supported by the seed — EXISTS evaluation
// requires actually running the subquery's plan, which the seed's
// per-row Eval contract doesn't support. Returns UNKNOWN (nil)
// at runtime; the planner's specialized EXISTS-handling rules
// short-circuit before evaluation.
type ExistsPredicate struct {
	ExistentialAlias values.CorrelationIdentifier
}

// NewExistsPredicate constructs the predicate.
func NewExistsPredicate(alias values.CorrelationIdentifier) *ExistsPredicate {
	return &ExistsPredicate{ExistentialAlias: alias}
}

// GetExistentialAlias returns the alias bound to the subquery.
func (p *ExistsPredicate) GetExistentialAlias() values.CorrelationIdentifier {
	return p.ExistentialAlias
}

// Eval returns TriUnknown — EXISTS evaluation isn't done at the
// per-row predicate level; specialized planner rules / executor
// handling does the row-level test.
func (p *ExistsPredicate) Eval(_ any) (TriBool, error) { return TriUnknown, nil }

// Children returns the empty slice — leaf.
func (*ExistsPredicate) Children() []QueryPredicate { return []QueryPredicate{} }

// GetCorrelatedTo returns the singleton set containing the
// existential alias — the subquery reference this predicate binds.
func (p *ExistsPredicate) GetCorrelatedTo() map[values.CorrelationIdentifier]struct{} {
	return map[values.CorrelationIdentifier]struct{}{
		p.ExistentialAlias: {},
	}
}

// HashCodeWithoutChildren hashes the predicate kind + alias.
func (p *ExistsPredicate) HashCodeWithoutChildren() uint64 {
	h := fnv.New64a()
	h.Write([]byte("exists|"))
	h.Write([]byte(p.ExistentialAlias.Name()))
	return h.Sum64()
}

// Explain renders the SQL-ish form.
func (p *ExistsPredicate) Explain() string {
	return "EXISTS(" + p.ExistentialAlias.Name() + ")"
}
