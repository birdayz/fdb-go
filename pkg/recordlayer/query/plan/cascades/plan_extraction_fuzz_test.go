package cascades

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
		e := buildFuzzExpr(t, b, 0, 0)
		ref := expressions.InitialOf(e)
		// Insert a few alternatives so there's something to choose
		// between — without alternatives the GetBest is trivial.
		for i := 1; i < 4 && len(b) > i; i++ {
			alt := buildFuzzExpr(t, b, i, 0)
			if e.GetResultValue().Type().Equals(alt.GetResultValue().Type()) {
				// A Reference is one equivalence class and therefore one exact
				// result shape. Arbitrary operator bytes can produce a projection
				// or set operation with a different shape; that is not an
				// alternative to this root.
				ref.Insert(alt)
			}
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

func extractionFuzzProjectedFields(
	t testing.TB,
	quantifier expressions.Quantifier,
) []values.Value {
	t.Helper()
	rootValue, rootErr := quantifier.RequireFlowedObjectValue()
	root := mustConstruct(t, rootValue, rootErr)
	recordType, ok := root.FlowedType().(*values.RecordType)
	if !ok {
		t.Fatalf("projection input type = %T, want exact record", root.FlowedType())
		return nil
	}
	projected := make([]values.Value, len(recordType.Fields))
	for i := range recordType.Fields {
		field, err := values.ResolveFieldOrdinals(root, []int{i})
		projected[i] = mustConstruct(t, field, err)
	}
	return projected
}

func extractionFuzzSameResultType(
	t testing.TB,
	left, right expressions.Quantifier,
) bool {
	t.Helper()
	leftTypeValue, leftTypeErr := left.GetFlowedObjectType()
	leftType := mustConstruct(t, leftTypeValue, leftTypeErr)
	rightTypeValue, rightTypeErr := right.GetFlowedObjectType()
	rightType := mustConstruct(t, rightTypeValue, rightTypeErr)
	return leftType.Equals(rightType)
}

// buildFuzzExpr is a self-contained tree builder for the properties
// fuzz tests. Mirrors cascades/fixpoint_fuzz_test.buildFuzzExpression
// but does NOT depend on the cascades package (which would create an
// import cycle: cascades → properties → cascades).
func buildFuzzExpr(
	t testing.TB,
	b []byte,
	start, depth int,
) expressions.RelationalExpression {
	t.Helper()
	if depth >= 3 || len(b) == 0 {
		return mustFullUnorderedScan(t, []string{"T"}, values.NewRecordType(
			"T", false, []values.Field{
				{Name: "k", FieldType: values.NotNullLong, Ordinal: 0},
			}))
	}
	op := b[start%len(b)] % 8
	switch op {
	case 0:
		return buildFuzzExpr(t, nil, 0, depth+1)
	case 1:
		inner := buildFuzzExpr(t, b, (start+1)%len(b), depth+1)
		q := expressions.ForEachQuantifier(expressions.InitialOf(inner))
		pT := predicates.NewConstantPredicate(predicates.TriTrue)
		filterValue, filterErr := expressions.NewLogicalFilterExpression(
			[]predicates.QueryPredicate{pT}, q)
		return mustConstruct(t, filterValue, filterErr)
	case 2:
		inner := buildFuzzExpr(t, b, (start+1)%len(b), depth+1)
		q := expressions.ForEachQuantifier(expressions.InitialOf(inner))
		distinctValue, distinctErr := expressions.NewLogicalDistinctExpression(q)
		return mustConstruct(t, distinctValue, distinctErr)
	case 3:
		inner := buildFuzzExpr(t, b, (start+1)%len(b), depth+1)
		q := expressions.ForEachQuantifier(expressions.InitialOf(inner))
		projectionValue, projectionErr := expressions.NewLogicalProjectionExpression(
			extractionFuzzProjectedFields(t, q), q)
		return mustConstruct(t, projectionValue, projectionErr)
	case 4:
		inner := buildFuzzExpr(t, b, (start+1)%len(b), depth+1)
		q := expressions.ForEachQuantifier(expressions.InitialOf(inner))
		filterValue, filterErr := expressions.NewLogicalTypeFilterExpression([]string{"X"}, q)
		return mustConstruct(t, filterValue, filterErr)
	case 5:
		left := buildFuzzExpr(t, b, (start+1)%len(b), depth+1)
		right := buildFuzzExpr(t, b, (start+2)%len(b), depth+1)
		ql := expressions.ForEachQuantifier(expressions.InitialOf(left))
		qr := expressions.ForEachQuantifier(expressions.InitialOf(right))
		if !extractionFuzzSameResultType(t, ql, qr) {
			// SQL has already coerced set-operation legs to one exact row by
			// this boundary. Arbitrary bytes have not, so retain a valid leg.
			return left
		}
		unionValue, unionErr := expressions.NewLogicalUnionExpression(
			[]expressions.Quantifier{ql, qr})
		return mustConstruct(t, unionValue, unionErr)
	case 6:
		left := buildFuzzExpr(t, b, (start+1)%len(b), depth+1)
		right := buildFuzzExpr(t, b, (start+2)%len(b), depth+1)
		ql := expressions.ForEachQuantifier(expressions.InitialOf(left))
		qr := expressions.ForEachQuantifier(expressions.InitialOf(right))
		if !extractionFuzzSameResultType(t, ql, qr) {
			return left
		}
		leftRootValue, leftRootErr := ql.RequireFlowedObjectValue()
		leftRoot := mustConstruct(t, leftRootValue, leftRootErr)
		keyValue, keyErr := values.ResolveFieldOrdinals(leftRoot, []int{0})
		key := mustConstruct(t, keyValue, keyErr)
		intersectionValue, intersectionErr := expressions.NewLogicalIntersectionExpression(
			[]expressions.Quantifier{ql, qr}, []values.Value{key})
		return mustConstruct(t, intersectionValue, intersectionErr)
	default:
		inner := buildFuzzExpr(t, b, (start+1)%len(b), depth+1)
		q := expressions.ForEachQuantifier(expressions.InitialOf(inner))
		sortValue, sortErr := expressions.UnsortedLogicalSortExpression(q)
		return mustConstruct(t, sortValue, sortErr)
	}
}
