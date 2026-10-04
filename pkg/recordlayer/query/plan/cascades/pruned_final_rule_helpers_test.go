package cascades

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
)

// promoteToFinals stands in for REWRITING's FinalizeExpressionsRule and the
// child pruning OptimizeInputs performs: every exploratory member of every
// reference under ref also becomes a final member.
func promoteToFinals(ref *expressions.Reference, seen map[*expressions.Reference]bool) {
	if ref == nil || seen[ref.Canonical()] {
		return
	}
	seen[ref.Canonical()] = true
	for _, member := range ref.Members() {
		ref.InsertFinal(member)
		for _, q := range member.GetQuantifiers() {
			promoteToFinals(q.GetRangesOver(), seen)
		}
	}
}

// mustFirePrunedFinalRule fires a rule that runs only on a final expression
// with pruned inputs (SelectMergeRule, PredicatePushDownRule) once on ref's
// expression, as OptimizeInputs does, with every input promoted to final.
func mustFirePrunedFinalRule(t testing.TB, rule ImplementationRule, ref *expressions.Reference) []expressions.RelationalExpression {
	t.Helper()
	root := ref.Get()
	seen := map[*expressions.Reference]bool{}
	for _, q := range root.GetQuantifiers() {
		promoteToFinals(q.GetRangesOver(), seen)
	}
	return mustFireImplementationRule(t, rule, expressions.FinalOf(root))
}
