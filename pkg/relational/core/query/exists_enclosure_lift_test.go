package query

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/relational/core/query/logical"
)

// The EXISTS enclosure lift. A naive filter arm would force enclosure
// (t.inInnerCluster = true) under EXISTS UNCONDITIONALLY, poisoning every
// join class beneath: a gated LEFT/RIGHT box or gated inner cluster under a
// WHERE-EXISTS would route to buildExistentialSelect with enclosure forced,
// so its wedge-gate decision would come back arityPoison (name model) and
// the ordinal existential rebase machinery would stay dead on live SQL.
//
// Instead the EXISTS enclosure routes through the gate (one authority,
// ordinalWedgeGateDecide): a gate-eligible input is NOT enclosed, so it
// gates ordinal and the rebase machinery goes live. These pins prove the
// decision at the translator layer (the e2e EXPLAIN summary renders the
// classes identically).

// existsFilterOver wraps input in a WHERE-EXISTS filter (one existential
// subquery over TypedRecord), the shape translateFilter routes to
// buildExistentialSelect for a non-INNER-join / clustered input.
func existsFilterOver(input logical.LogicalOperator, alias string) *logical.LogicalFilter {
	return &logical.LogicalFilter{
		Input: input,
		ExistsSubqueries: []logical.ExistsSubquery{{
			Alias: values.NamedCorrelationIdentifier(alias),
			Plan:  scan("TypedRecord", "g"),
		}},
	}
}

func TestExistsEnclosureLift(t *testing.T) {
	t.Parallel()

	t.Run("single-source LEFT box under EXISTS gates ordinal", func(t *testing.T) {
		t.Parallel()
		// A single-source LEFT box gates (RewriteOuterJoinRule
		// dissolves it to INNER + null-on-empty, the shape the ordinal machinery
		// implements). Under EXISTS it was ENCLOSED → arityPoison; the lift lets it
		// gate at arity 2 so the ordinal existential rebase fires.
		box := logical.NewJoin(scan("Order", "o"), scan("Customer", "c"), logical.JoinLeft, "")
		tr := newGateTranslator(t)
		if ref := tr.translateRef(existsFilterOver(box, "q$e")); ref == nil {
			t.Fatalf("translation failed: %v", tr.translateErr)
		}
		d, ok := tr.wedgeGate[box]
		if !ok || !d.Gated || d.Arity != 2 {
			t.Fatalf("LEFT box under EXISTS = %+v (recorded=%v), want gated arity 2 (enclosure lifted)", d, ok)
		}
	})

	t.Run("single-source RIGHT box under EXISTS gates ordinal", func(t *testing.T) {
		t.Parallel()
		// RIGHT is the mirror of LEFT in the gated class (normalized to
		// LEFT with swapped operands at execution); the lift covers both.
		box := logical.NewJoin(scan("Order", "o"), scan("Customer", "c"), logical.JoinRight, "")
		tr := newGateTranslator(t)
		if ref := tr.translateRef(existsFilterOver(box, "q$e")); ref == nil {
			t.Fatalf("translation failed: %v", tr.translateErr)
		}
		d, ok := tr.wedgeGate[box]
		if !ok || !d.Gated || d.Arity != 2 {
			t.Fatalf("RIGHT box under EXISTS = %+v (recorded=%v), want gated arity 2", d, ok)
		}
	})

	t.Run("FULL box under EXISTS declines loud", func(t *testing.T) {
		t.Parallel()
		// FULL genuinely GATES as a root (opaque both ways), but existsOuterGatesFresh
		// deliberately EXCLUDES it: its drain-build composition with the existential
		// semi-join is unvalidated, so the lift keeps it out (a dedicated FULL-outer
		// extension would need to widen it) — and with no remaining name-model
		// fallback the excluded shape declines LOUDLY instead of translating.
		box := logical.NewJoin(scan("Order", "o"), scan("Customer", "c"), logical.JoinFull, "")
		tr := newGateTranslator(t)
		if ref := tr.translateRef(existsFilterOver(box, "q$e")); ref != nil {
			t.Fatalf("FULL box under EXISTS translated (%T) — must decline loudly (lift excludes FULL; no name-model fallback)", ref.Members()[0])
		}
		if tr.translateErr == nil {
			t.Fatal("FULL box under EXISTS decline must be LOUD (a translate error), got nil")
		}
	})

	t.Run("clustered-leg LEFT box under EXISTS gates", func(t *testing.T) {
		t.Parallel()
		// The joined-preserved LEFT box GATES at the
		// root, and existsOuterGatesFresh (routing through the one gate
		// authority) widens with it automatically — the box under an EXISTS
		// flatten translates fresh and takes the ordinal seed.
		cluster := logical.NewJoin(scan("Order", "o"), scan("Customer", "c"), logical.JoinInner, "")
		box := logical.NewJoin(cluster, scan("TypedRecord", "t"), logical.JoinLeft, "")
		tr := newGateTranslator(t)
		if ref := tr.translateRef(existsFilterOver(box, "q$e")); ref == nil {
			t.Fatalf("translation failed: %v", tr.translateErr)
		}
		if d, ok := tr.wedgeGate[box]; !ok || !d.Gated {
			t.Fatalf("clustered LEFT box under EXISTS = %+v (recorded=%v), want recorded and GATED", d, ok)
		}
	})

	t.Run("EXISTS box already enclosed declines loud", func(t *testing.T) {
		t.Parallel()
		// When the WHERE-EXISTS filter is ITSELF translated inside an enclosing
		// context (prevEnclosure true), the lift must NOT clear the enclosure —
		// existsOuterGatesFresh probes with a FRESH position, so lifting on that
		// alone would seed an ordinal positional row the parent then mis-binds.
		// With no remaining name-model fallback the
		// non-lifted enclosed shape declines LOUDLY.
		box := logical.NewJoin(scan("Order", "o"), scan("Customer", "c"), logical.JoinLeft, "")
		tr := newGateTranslator(t)
		tr.inInnerCluster = true // the enclosing context
		if ref := tr.translateRef(existsFilterOver(box, "q$e")); ref != nil {
			t.Fatalf("already-enclosed EXISTS box translated (%T) — must decline loudly (the lift must not clear the enclosure)", ref.Members()[0])
		}
		if tr.translateErr == nil {
			t.Fatal("already-enclosed EXISTS box decline must be LOUD (a translate error), got nil")
		}
	})

	t.Run("non-existential LEFT box still gates (regression guard)", func(t *testing.T) {
		t.Parallel()
		// The same single-source LEFT box with NO EXISTS gates as it did with
		// EXISTS above — the lift changes only the EXISTS-enclosure path.
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
