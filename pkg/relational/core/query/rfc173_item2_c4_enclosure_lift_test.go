package query

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/relational/core/query/logical"
)

// RFC-173 QP-REF-BIND item 2, commit 4 — the enclosure lift. The generic
// filter arm forced enclosure (t.inInnerCluster = true) under EXISTS
// UNCONDITIONALLY, poisoning every join class beneath: a gated LEFT/RIGHT
// box or gated inner cluster under a WHERE-EXISTS routed to
// buildExistentialSelect with enclosure forced, so its wedge-gate decision
// came back arityPoison (name model) and the W4-left ordinal existential
// rebase machinery (rfc173_w4left_existential.go) stayed DEAD on live SQL —
// the LIVE-CORRECTION recorded in the item-2 substrate.
//
// Commit 4 routes the EXISTS enclosure through the gate (design ruling
// condition 4 — one authority, ordinalWedgeGateDecide): a gate-eligible
// input is NOT enclosed, so it gates ordinal and the rebase machinery goes
// live. These pins prove the decision at the translator layer (the e2e
// EXPLAIN summary renders the classes identically, as in commit 3).

// c4ExistsFilterOver wraps input in a WHERE-EXISTS filter (one existential
// subquery over TypedRecord), the shape translateFilter routes to
// buildExistentialSelect for a non-INNER-join / clustered input.
func c4ExistsFilterOver(input logical.LogicalOperator, alias string) *logical.LogicalFilter {
	return &logical.LogicalFilter{
		Input: input,
		ExistsSubqueries: []logical.ExistsSubquery{{
			Alias: values.NamedCorrelationIdentifier(alias),
			Plan:  scan("TypedRecord", "g"),
		}},
	}
}

func TestRFC173Item2C4_EnclosureLift(t *testing.T) {
	t.Parallel()

	t.Run("single-source LEFT box under EXISTS gates ordinal", func(t *testing.T) {
		t.Parallel()
		// A single-source LEFT box is the W4-left gated class (RewriteOuterJoinRule
		// dissolves it to INNER + null-on-empty, the shape the ordinal machinery
		// implements). Under EXISTS it was ENCLOSED → arityPoison; the lift lets it
		// gate at arity 2 so the ordinal existential rebase fires.
		box := logical.NewJoin(scan("Order", "o"), scan("Customer", "c"), logical.JoinLeft, "")
		tr := newGateTranslator(t)
		if ref := tr.translateRef(c4ExistsFilterOver(box, "q$e")); ref == nil {
			t.Fatalf("translation failed: %v", tr.translateErr)
		}
		d, ok := tr.wedgeGate[box]
		if !ok || !d.Gated || d.Arity != 2 {
			t.Fatalf("LEFT box under EXISTS = %+v (recorded=%v), want gated arity 2 (enclosure lifted)", d, ok)
		}
	})

	t.Run("clustered-leg LEFT box under EXISTS stays name-model", func(t *testing.T) {
		t.Parallel()
		// A LEFT box whose preserved leg is itself a CLUSTER is the RFC-153
		// joined-preserved class — name-model until QP-REF-BIND item 3, gated or
		// not. The lift must NOT over-reach it: the gate declines on its own
		// merits (clusterArity > 1 per leg), independent of enclosure.
		cluster := logical.NewJoin(scan("Order", "o"), scan("Customer", "c"), logical.JoinInner, "")
		box := logical.NewJoin(cluster, scan("TypedRecord", "t"), logical.JoinLeft, "")
		tr := newGateTranslator(t)
		if ref := tr.translateRef(c4ExistsFilterOver(box, "q$e")); ref == nil {
			t.Fatalf("translation failed: %v", tr.translateErr)
		}
		if d, ok := tr.wedgeGate[box]; !ok || d.Gated {
			t.Fatalf("clustered LEFT box under EXISTS = %+v (recorded=%v), want recorded NOT gated (joined-preserved class, item 3)", d, ok)
		}
	})

	t.Run("non-existential LEFT box still gates (regression guard)", func(t *testing.T) {
		t.Parallel()
		// The same single-source LEFT box with NO EXISTS gates as it did before
		// commit 4 — the lift changes only the EXISTS-enclosure path.
		box := logical.NewJoin(scan("Order", "o"), scan("Customer", "c"), logical.JoinLeft, "")
		tr := newGateTranslator(t)
		if ref := tr.translateRef(box); ref == nil {
			t.Fatalf("translation failed: %v", tr.translateErr)
		}
		if d, ok := tr.wedgeGate[box]; !ok || !d.Gated || d.Arity != 2 {
			t.Fatalf("plain LEFT box = %+v (recorded=%v), want gated arity 2 (unchanged by the lift)", d, ok)
		}
	})
}
