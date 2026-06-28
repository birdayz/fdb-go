package properties

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
)

func TestEvaluateReferencesAndDependencies_Nil(t *testing.T) {
	t.Parallel()
	got := EvaluateReferencesAndDependencies(nil)
	if len(got.Refs) != 0 {
		t.Fatalf("refs count = %d, want 0", len(got.Refs))
	}
}

func TestEvaluateReferencesAndDependencies_SingleRef(t *testing.T) {
	t.Parallel()
	scan := expressions.NewFullUnorderedScanExpression([]string{"T"}, nil)
	ref := expressions.InitialOf(scan)
	inner := expressions.ForEachQuantifier(ref)
	filter := expressions.NewLogicalFilterExpression(nil, inner)

	got := EvaluateReferencesAndDependencies(filter)
	// One ref (between scan and filter).
	if len(got.Refs) != 1 {
		t.Fatalf("refs count = %d, want 1", len(got.Refs))
	}
	if _, ok := got.Refs[ref]; !ok {
		t.Fatal("ref should be in the set")
	}
}

func TestEvaluateReferencesAndDependenciesForRef(t *testing.T) {
	t.Parallel()
	scan := expressions.NewFullUnorderedScanExpression([]string{"T"}, nil)
	ref := expressions.InitialOf(scan)

	got := EvaluateReferencesAndDependenciesForRef(ref)
	if len(got.Refs) != 1 {
		t.Fatalf("refs count = %d, want 1", len(got.Refs))
	}
	if _, ok := got.Refs[ref]; !ok {
		t.Fatal("ref should be in the set")
	}
}

func TestEvaluateReferencesAndDependencies_Chain(t *testing.T) {
	t.Parallel()
	scan := expressions.NewFullUnorderedScanExpression([]string{"T"}, nil)
	ref1 := expressions.InitialOf(scan)
	q1 := expressions.ForEachQuantifier(ref1)
	filter := expressions.NewLogicalFilterExpression(nil, q1)
	ref2 := expressions.InitialOf(filter)
	q2 := expressions.ForEachQuantifier(ref2)
	tf := expressions.NewLogicalTypeFilterExpression([]string{"T"}, q2)

	got := EvaluateReferencesAndDependencies(tf)
	// Two refs: ref1 and ref2.
	if len(got.Refs) != 2 {
		t.Fatalf("refs count = %d, want 2", len(got.Refs))
	}
	// ref2 should depend on ref1 (filter quantifies over ref1).
	deps, ok := got.Dependencies[ref2]
	if !ok {
		t.Fatal("ref2 should have dependencies")
	}
	if _, ok := deps[ref1]; !ok {
		t.Fatal("ref2 should depend on ref1")
	}
}
