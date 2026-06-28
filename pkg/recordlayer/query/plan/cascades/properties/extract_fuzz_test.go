package properties

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// FuzzExtractBestPlan_SingletonInvariant pins that ExtractBestPlan
// always returns a tree where every reachable Reference has exactly
// one member — the post-extraction "best plan" invariant.
//
// Also pins termination + non-panic + non-error on every shape
// the seed expression hierarchy supports.
//
// Tree generator mirrors fixpoint_fuzz_test.buildFuzzExpression but
// is duplicated here to keep the test cross-package self-contained
// (properties imports expressions but not cascades).
func FuzzExtractBestPlan_SingletonInvariant(f *testing.F) {
	f.Add([]byte{0, 1, 2, 3, 4, 5})
	f.Add(make([]byte, 8))
	f.Fuzz(func(t *testing.T, b []byte) {
		if len(b) < 4 {
			return
		}
		e := buildFuzzExpr(b, 0, 0)
		ref := expressions.InitialOf(e)
		// Insert a few alternatives so there's something to choose
		// between — without alternatives the GetBest is trivial.
		for i := 1; i < 4 && len(b) > i; i++ {
			alt := buildFuzzExpr(b, i, 0)
			ref.Insert(alt)
		}

		extracted, err := ExtractBestPlan(ref)
		if err != nil {
			t.Fatalf("ExtractBestPlan err=%v", err)
		}
		if extracted == nil {
			return
		}

		// Walk and assert singleton invariant.
		var visit func(e expressions.RelationalExpression, depth int)
		visit = func(e expressions.RelationalExpression, depth int) {
			if depth > 100 {
				t.Fatalf("extracted tree too deep — possible cycle")
			}
			for _, q := range e.GetQuantifiers() {
				r := q.GetRangesOver()
				if r == nil {
					continue
				}
				if got := len(r.Members()); got != 1 {
					t.Fatalf("Reference has %d members in extracted tree (want 1)", got)
				}
				visit(r.Get(), depth+1)
			}
		}
		visit(extracted, 0)
	})
}

// buildFuzzExpr is a self-contained tree builder for the properties
// fuzz tests. Mirrors cascades/fixpoint_fuzz_test.buildFuzzExpression
// but does NOT depend on the cascades package (which would create an
// import cycle: cascades → properties → cascades).
func buildFuzzExpr(b []byte, start, depth int) expressions.RelationalExpression {
	if depth >= 3 || len(b) == 0 {
		return expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType)
	}
	op := b[start%len(b)] % 8
	switch op {
	case 0:
		return expressions.NewFullUnorderedScanExpression([]string{"T"}, values.UnknownType)
	case 1:
		inner := buildFuzzExpr(b, (start+1)%len(b), depth+1)
		q := expressions.ForEachQuantifier(expressions.InitialOf(inner))
		pT := predicates.NewConstantPredicate(predicates.TriTrue)
		return expressions.NewLogicalFilterExpression([]predicates.QueryPredicate{pT}, q)
	case 2:
		inner := buildFuzzExpr(b, (start+1)%len(b), depth+1)
		q := expressions.ForEachQuantifier(expressions.InitialOf(inner))
		return expressions.NewLogicalDistinctExpression(q)
	case 3:
		inner := buildFuzzExpr(b, (start+1)%len(b), depth+1)
		q := expressions.ForEachQuantifier(expressions.InitialOf(inner))
		return expressions.NewLogicalProjectionExpression(
			[]values.Value{q.GetFlowedObjectValue()}, q)
	case 4:
		inner := buildFuzzExpr(b, (start+1)%len(b), depth+1)
		q := expressions.ForEachQuantifier(expressions.InitialOf(inner))
		return expressions.NewLogicalTypeFilterExpression([]string{"X"}, q)
	case 5:
		left := buildFuzzExpr(b, (start+1)%len(b), depth+1)
		right := buildFuzzExpr(b, (start+2)%len(b), depth+1)
		ql := expressions.ForEachQuantifier(expressions.InitialOf(left))
		qr := expressions.ForEachQuantifier(expressions.InitialOf(right))
		return expressions.NewLogicalUnionExpression([]expressions.Quantifier{ql, qr})
	case 6:
		left := buildFuzzExpr(b, (start+1)%len(b), depth+1)
		right := buildFuzzExpr(b, (start+2)%len(b), depth+1)
		ql := expressions.ForEachQuantifier(expressions.InitialOf(left))
		qr := expressions.ForEachQuantifier(expressions.InitialOf(right))
		keys := []values.Value{&values.FieldValue{Field: "k", Typ: values.UnknownType}}
		return expressions.NewLogicalIntersectionExpression([]expressions.Quantifier{ql, qr}, keys)
	default:
		inner := buildFuzzExpr(b, (start+1)%len(b), depth+1)
		q := expressions.ForEachQuantifier(expressions.InitialOf(inner))
		return expressions.UnsortedLogicalSortExpression(q)
	}
}
