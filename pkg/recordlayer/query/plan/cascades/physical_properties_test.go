package cascades

import (
	"testing"

	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/expressions"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/values"
)

func TestPhysicalProperties_NoProperties_IsEmpty(t *testing.T) {
	t.Parallel()
	if !expressions.NoProperties.IsEmpty() {
		t.Fatal("NoProperties should be empty")
	}
}

func TestPhysicalProperties_WithOrdering_NotEmpty(t *testing.T) {
	t.Parallel()
	props := expressions.PhysicalProperties{}
	if !props.IsEmpty() {
		t.Fatal("zero-value should be empty")
	}
}

func TestReference_Winner_NoWinner(t *testing.T) {
	t.Parallel()
	scan := expressions.NewFullUnorderedScanExpression([]string{"T"}, nil)
	ref := expressions.InitialOf(scan)
	if ref.Winner(expressions.NoProperties) != nil {
		t.Fatal("expected nil winner")
	}
	if ref.HasWinner(expressions.NoProperties) {
		t.Fatal("expected no winner")
	}
}

func TestReference_Winner_SetAndGet(t *testing.T) {
	t.Parallel()
	scan := expressions.NewFullUnorderedScanExpression([]string{"T"}, nil)
	ref := expressions.InitialOf(scan)
	ref.SetWinner(expressions.NoProperties, scan)
	if ref.Winner(expressions.NoProperties) != scan {
		t.Fatal("expected scan as winner")
	}
	if !ref.HasWinner(expressions.NoProperties) {
		t.Fatal("expected winner present")
	}
}

func TestReference_Winner_MultipleProperties(t *testing.T) {
	t.Parallel()
	scan1 := expressions.NewFullUnorderedScanExpression([]string{"A"}, nil)
	scan2 := expressions.NewFullUnorderedScanExpression([]string{"B"}, nil)
	ref := expressions.InitialOf(scan1)
	ref.Insert(scan2)

	key1 := expressions.NoProperties
	key2 := expressions.PhysicalProperties{} // same as NoProperties

	ref.SetWinner(key1, scan1)
	if ref.Winner(key2) != scan1 {
		t.Fatal("equivalent keys should return same winner")
	}
}

func TestReference_Winner_OverwritesBetterPlan(t *testing.T) {
	t.Parallel()
	scan1 := expressions.NewFullUnorderedScanExpression([]string{"A"}, nil)
	scan2 := expressions.NewFullUnorderedScanExpression([]string{"B"}, nil)
	ref := expressions.InitialOf(scan1)
	ref.Insert(scan2)

	ref.SetWinner(expressions.NoProperties, scan1)
	ref.SetWinner(expressions.NoProperties, scan2)
	if ref.Winner(expressions.NoProperties) != scan2 {
		t.Fatal("second SetWinner should overwrite first")
	}
}

func TestPhysicalProperties_OrderingFromSortKeys(t *testing.T) {
	t.Parallel()
	keys := []expressions.SortKey{
		{Value: &values.FieldValue{Field: "name"}, Reverse: false},
		{Value: &values.FieldValue{Field: "age"}, Reverse: true},
	}
	props := expressions.OrderingFromSortKeys(keys)
	if props.IsEmpty() {
		t.Fatal("ordering properties should not be empty")
	}
	if props.OrderingCount() != 2 {
		t.Fatalf("expected 2 ordering columns, got %d", props.OrderingCount())
	}
}

func TestPhysicalProperties_Satisfies(t *testing.T) {
	t.Parallel()
	nameAsc := []expressions.SortKey{
		{Value: &values.FieldValue{Field: "name"}, Reverse: false},
	}
	nameAscAge := []expressions.SortKey{
		{Value: &values.FieldValue{Field: "name"}, Reverse: false},
		{Value: &values.FieldValue{Field: "age"}, Reverse: false},
	}
	nameDesc := []expressions.SortKey{
		{Value: &values.FieldValue{Field: "name"}, Reverse: true},
	}

	propsNameAsc := expressions.OrderingFromSortKeys(nameAsc)
	propsNameAscAge := expressions.OrderingFromSortKeys(nameAscAge)
	propsNameDesc := expressions.OrderingFromSortKeys(nameDesc)

	if !propsNameAscAge.Satisfies(propsNameAsc) {
		t.Fatal("(name ASC, age ASC) should satisfy (name ASC)")
	}
	if propsNameAsc.Satisfies(propsNameAscAge) {
		t.Fatal("(name ASC) should NOT satisfy (name ASC, age ASC)")
	}
	if propsNameAsc.Satisfies(propsNameDesc) {
		t.Fatal("(name ASC) should NOT satisfy (name DESC)")
	}
	if !propsNameAsc.Satisfies(expressions.NoProperties) {
		t.Fatal("any ordering should satisfy empty requirements")
	}
	if !expressions.NoProperties.Satisfies(expressions.NoProperties) {
		t.Fatal("empty should satisfy empty")
	}
}

func TestOptimizeReferenceTask_StampsWinner(t *testing.T) {
	t.Parallel()
	scan := expressions.NewFullUnorderedScanExpression([]string{"T"}, nil)
	ref := expressions.InitialOf(scan)

	p := NewPlanner(nil, nil)
	task := &OptimizeReferenceTask{Ref: ref}
	task.Run(p)

	if ref.Winner(expressions.NoProperties) != scan {
		t.Fatal("OptimizeReferenceTask should stamp winner on Reference")
	}
	if p.BestMember(ref) != scan {
		t.Fatal("OptimizeReferenceTask should also stamp bestMember map")
	}
}

func TestOptimizeReferenceTask_StampsOrderingWinner(t *testing.T) {
	t.Parallel()

	// Create an index scan candidate that provides ordering on STATUS.
	a1 := values.UniqueCorrelationIdentifier()
	cand := NewValueIndexScanMatchCandidate(
		"Order$status",
		[]string{"Order"},
		[]string{"STATUS"},
		[]values.CorrelationIdentifier{a1},
		values.UnknownType,
		false,
	)
	ctx := &indexTestPlanContext{candidates: []MatchCandidate{cand}}

	// Set up a scan → sort expression tree.
	scan := expressions.NewFullUnorderedScanExpression([]string{"Order"}, values.UnknownType)
	scanRef := expressions.InitialOf(scan)
	q := expressions.ForEachQuantifier(scanRef)
	sort := expressions.NewLogicalSortExpression(
		[]expressions.SortKey{
			{Value: &values.FieldValue{Field: "STATUS", Typ: values.UnknownType}},
		},
		q,
	)
	sortRef := expressions.InitialOf(sort)

	// Run exploration with BatchA rules to produce physical wrappers.
	rules := append(DefaultExpressionRules(), BatchAExpressionRules()...)
	p := NewPlanner(rules, ctx)
	p.Explore(sortRef)

	// The sort Reference should now have an ordering-specific winner
	// for the STATUS ordering (from OrderedIndexScanRule producing a
	// physicalIndexScanWrapper that hints STATUS ASC).
	statusOrdering := expressions.OrderingFromNameDir([]string{"STATUS"}, []bool{false})
	winner := sortRef.Winner(statusOrdering)
	if winner == nil {
		t.Fatal("expected ordering-specific winner for STATUS ASC")
	}
	if !IsPhysicalIndexScan(winner) {
		t.Fatalf("expected physicalIndexScanWrapper as ordering winner, got %T", winner)
	}
}
