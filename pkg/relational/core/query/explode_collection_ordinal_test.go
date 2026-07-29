package query

import (
	"strings"
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/relational/core/query/logical"
)

// A lateral UNNEST's Explode must read its array through an ORDINAL bake over
// the quantifier that owns it. The alternative the lowering used to keep — a
// lazy `FieldValue{Field: "SEG0.COL"}` over the merged outer row — is the
// qualified-name channel (RFC-197 item 6): the owning leg packed into a string
// because a bare read over a multi-namespace row is ambiguous (last-leg-wins,
// and the winner follows an operand order the planner may swap).
//
// A string qualifier is not a fix for that ambiguity, it relocates it. Nothing
// downstream can tell the packed qualifier from a leaf name that legitimately
// contains a dot, so the dependency on the owning leg had to be recovered by
// slicing the prefix back off and treating it as a correlation identifier — a
// quantifier's identity decided by text, which loses the edge when the leg is
// addressed by a minted binding and invents one when a sibling shares the alias.
//
// The bake states the same fact as the leg's own window offset, so no qualifier
// needs encoding and no recovery needs to run. TestGatheredExplodeCorrelatesToOwner
// (pkg/recordlayer/query/plan/cascades) is the consumer-side twin: it pins that
// the baked collection's correlation carries the dependency the recovery used to
// reconstruct.
//
// This test walks the lowering's OWN output, so it fails the moment a
// name-model collection is reintroduced — a plan-level row assertion cannot,
// because both forms yield the same rows until the operand order swaps.
func TestExplodeCollectionsAreOrdinalBaked(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name  string
		outer logical.LogicalOperator
	}{
		// Single namespace: a bare read WOULD have been unambiguous here, so
		// this arm pins that the ordinal bake is unconditional rather than a
		// multi-source special case.
		{"single source", scan("Order", "o")},
		// MULTI-namespace outer — the shape that used to mint `O.TAGS`. The
		// outer row carries Order's, Customer's and TypedRecord's columns, so
		// the qualifier was the only thing distinguishing the array.
		{"clustered FULL box (multi-namespace outer)", logical.NewJoin(
			logical.NewJoin(scan("Order", "o"), scan("Customer", "c"), logical.JoinInner, ""),
			scan("TypedRecord", "d"), logical.JoinFull, "",
		)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			tr := newGateTranslator(t)
			tr.unnestUnderExistential = true
			j := logical.NewJoin(tc.outer,
				&logical.LogicalUnnest{Segments: []string{"o", "TAGS"}, Alias: "X"},
				logical.JoinInner, "")
			expr := tr.translateUnnestJoin(j, j.Right.(*logical.LogicalUnnest)) //nolint:errcheck // fixture
			if expr == nil {
				t.Fatalf("translation failed: %v", tr.translateErr)
			}
			sel, ok := expr.(*expressions.SelectExpression)
			if !ok {
				t.Fatalf("unnest expr = %T, want a SelectExpression", expr)
			}

			collections := explodeCollectionsOf(sel)
			if len(collections) == 0 {
				t.Fatal("no Explode in the lowered unnest — the fixture stopped exercising the collection producer")
			}
			for _, coll := range collections {
				fv, isFV := coll.(*values.FieldValue)
				if !isFV {
					t.Fatalf("Explode collection = %T, want a *FieldValue read of the owner's column", coll)
				}
				if fv.Resolved == nil {
					t.Fatalf("Explode collection %q is LAZY (Resolved == nil) — a name-keyed read of a merged row, the qualified-name channel this lowering must not reopen", fv.Field)
				}
				if strings.Contains(fv.Field, ".") {
					t.Fatalf("Explode collection field %q packs its owning leg into the name — qualification must be the bake's ordinal, not a string prefix", fv.Field)
				}
				// The bake is only meaningful if it names an owner: a
				// collection correlating to nothing has no leg for a
				// bipartition to keep it with.
				if corr := values.GetCorrelatedToOfValue(coll); len(corr) == 0 {
					t.Fatalf("Explode collection %q correlates to nothing — no owner edge for the bipartition rules to respect", fv.Field)
				}
			}
		})
	}
}

// explodeCollectionsOf returns the collection Value of every Explode reachable
// through sel's quantifiers.
func explodeCollectionsOf(sel *expressions.SelectExpression) []values.Value {
	var out []values.Value
	for _, q := range sel.GetQuantifiers() {
		ref := q.GetRangesOver()
		if ref == nil {
			continue
		}
		for _, m := range ref.Members() {
			if ex, isExplode := m.(*expressions.ExplodeExpression); isExplode {
				out = append(out, ex.GetCollectionValue())
			}
		}
	}
	return out
}
