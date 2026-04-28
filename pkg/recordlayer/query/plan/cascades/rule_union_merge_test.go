package cascades

import (
	"testing"

	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/expressions"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/values"
)

// scanQuant returns a fresh ForEachQuantifier ranging over a fresh
// LogicalSomething(name) scan. Convenience for the union-merge
// fixtures that need different leaf identities.
func scanQuant(name string) expressions.Quantifier {
	scan := expressions.NewFullUnorderedScanExpression([]string{name}, values.UnknownType)
	return expressions.ForEachQuantifier(expressions.InitialOf(scan))
}

func TestUnionMergeRule_FlattensSingleNested(t *testing.T) {
	t.Parallel()
	// Union(Scan(A), Union(Scan(B), Scan(C)))
	a := scanQuant("A")
	innerU := expressions.NewLogicalUnionExpression([]expressions.Quantifier{
		scanQuant("B"),
		scanQuant("C"),
	})
	innerUQ := expressions.ForEachQuantifier(expressions.InitialOf(innerU))
	outerU := expressions.NewLogicalUnionExpression([]expressions.Quantifier{a, innerUQ})
	ref := expressions.InitialOf(outerU)

	rule := NewUnionMergeRule()
	yielded := FireExpressionRule(rule, ref)
	if len(yielded) != 1 {
		t.Fatalf("yielded=%d, want 1", len(yielded))
	}
	merged := yielded[0].(*expressions.LogicalUnionExpression)
	if got := len(merged.GetQuantifiers()); got != 3 {
		t.Fatalf("flattened child count=%d, want 3 (A + B + C)", got)
	}
}

func TestUnionMergeRule_FlattensMultipleNested(t *testing.T) {
	t.Parallel()
	// Union(Union(A, B), Union(C, D)) → Union(A, B, C, D)
	innerL := expressions.NewLogicalUnionExpression([]expressions.Quantifier{scanQuant("A"), scanQuant("B")})
	innerR := expressions.NewLogicalUnionExpression([]expressions.Quantifier{scanQuant("C"), scanQuant("D")})
	outer := expressions.NewLogicalUnionExpression([]expressions.Quantifier{
		expressions.ForEachQuantifier(expressions.InitialOf(innerL)),
		expressions.ForEachQuantifier(expressions.InitialOf(innerR)),
	})
	ref := expressions.InitialOf(outer)
	rule := NewUnionMergeRule()
	yielded := FireExpressionRule(rule, ref)
	if len(yielded) != 1 {
		t.Fatalf("yielded=%d, want 1", len(yielded))
	}
	merged := yielded[0].(*expressions.LogicalUnionExpression)
	if got := len(merged.GetQuantifiers()); got != 4 {
		t.Fatalf("flattened child count=%d, want 4", got)
	}
}

func TestUnionMergeRule_DeclinesOnNonNested(t *testing.T) {
	t.Parallel()
	outer := expressions.NewLogicalUnionExpression([]expressions.Quantifier{
		scanQuant("A"),
		scanQuant("B"),
	})
	ref := expressions.InitialOf(outer)
	rule := NewUnionMergeRule()
	yielded := FireExpressionRule(rule, ref)
	if len(yielded) != 0 {
		t.Fatalf("rule fired on a flat Union — yielded %d, want 0", len(yielded))
	}
}

func TestUnionMergeRule_PreservesOrderAcrossFlatten(t *testing.T) {
	t.Parallel()
	// Union(Scan(A), Union(Scan(B), Scan(C)), Scan(D))
	// Flatten preserves textual order: [A, B, C, D].
	a := scanQuant("A")
	innerU := expressions.NewLogicalUnionExpression([]expressions.Quantifier{scanQuant("B"), scanQuant("C")})
	innerUQ := expressions.ForEachQuantifier(expressions.InitialOf(innerU))
	d := scanQuant("D")
	outer := expressions.NewLogicalUnionExpression([]expressions.Quantifier{a, innerUQ, d})
	ref := expressions.InitialOf(outer)
	yielded := FireExpressionRule(NewUnionMergeRule(), ref)
	merged := yielded[0].(*expressions.LogicalUnionExpression)
	want := []string{"A", "B", "C", "D"}
	for i, q := range merged.GetQuantifiers() {
		inner := q.GetRangesOver().Get().(*expressions.FullUnorderedScanExpression)
		if got := inner.GetRecordTypes()[0]; got != want[i] {
			t.Fatalf("position %d: got %q, want %q", i, got, want[i])
		}
	}
}
