package plangen_test

import (
	"errors"
	"testing"

	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/expressions"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/predicates"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/values"
	"github.com/birdayz/fdb-record-layer-go/pkg/relational/core/query/logical"
	"github.com/birdayz/fdb-record-layer-go/pkg/relational/core/query/plangen"
)

func TestConvert_Nil(t *testing.T) {
	t.Parallel()
	_, err := plangen.Convert(nil)
	if err == nil {
		t.Fatal("expected error on nil input")
	}
}

func TestConvert_Scan(t *testing.T) {
	t.Parallel()
	src := logical.NewScan("Order", "")
	got, err := plangen.Convert(src)
	if err != nil {
		t.Fatalf("Convert: %v", err)
	}
	scan, ok := got.(*expressions.FullUnorderedScanExpression)
	if !ok {
		t.Fatalf("got %T, want *FullUnorderedScanExpression", got)
	}
	if names := scan.GetRecordTypes(); len(names) != 1 || names[0] != "Order" {
		t.Fatalf("record types = %v, want [Order]", names)
	}
}

func TestConvert_FilterOverScan(t *testing.T) {
	t.Parallel()
	pT := predicates.NewConstantPredicate(predicates.TriTrue)
	src := logical.NewFilterWithPredicate(logical.NewScan("Order", ""), pT, "TRUE")
	got, err := plangen.Convert(src)
	if err != nil {
		t.Fatalf("Convert: %v", err)
	}
	f, ok := got.(*expressions.LogicalFilterExpression)
	if !ok {
		t.Fatalf("got %T, want *LogicalFilterExpression", got)
	}
	if got := f.GetPredicates(); len(got) != 1 {
		t.Fatalf("predicate count = %d, want 1", len(got))
	}
	// Inner should be the converted Scan.
	innerExpr := f.GetInner().GetRangesOver().Get()
	if _, ok := innerExpr.(*expressions.FullUnorderedScanExpression); !ok {
		t.Fatalf("filter inner = %T, want *FullUnorderedScanExpression", innerExpr)
	}
}

func TestConvert_FilterTextOnly_Unsupported(t *testing.T) {
	t.Parallel()
	// Text-only filter (no QueryPredicate) is the legacy non-catalog path.
	src := logical.NewFilter(logical.NewScan("Order", ""), "x > 5")
	_, err := plangen.Convert(src)
	if !errors.Is(err, plangen.ErrUnsupported) {
		t.Fatalf("got %v, want ErrUnsupported", err)
	}
}

func TestConvert_Union(t *testing.T) {
	t.Parallel()
	a := logical.NewScan("A", "")
	b := logical.NewScan("B", "")
	src := logical.NewUnion([]logical.LogicalOperator{a, b}, false)
	got, err := plangen.Convert(src)
	if err != nil {
		t.Fatalf("Convert: %v", err)
	}
	u, ok := got.(*expressions.LogicalUnionExpression)
	if !ok {
		t.Fatalf("got %T, want *LogicalUnionExpression", got)
	}
	if len(u.GetQuantifiers()) != 2 {
		t.Fatalf("union has %d children, want 2", len(u.GetQuantifiers()))
	}
}

func TestConvert_UnionDistinct(t *testing.T) {
	t.Parallel()
	a := logical.NewScan("A", "")
	b := logical.NewScan("B", "")
	src := logical.NewUnion([]logical.LogicalOperator{a, b}, true) // distinct = true
	got, err := plangen.Convert(src)
	if err != nil {
		t.Fatalf("Convert: %v", err)
	}
	d, ok := got.(*expressions.LogicalDistinctExpression)
	if !ok {
		t.Fatalf("got %T, want *LogicalDistinctExpression (Distinct wrapper)", got)
	}
	innerExpr := d.GetInner().GetRangesOver().Get()
	if _, ok := innerExpr.(*expressions.LogicalUnionExpression); !ok {
		t.Fatalf("distinct inner = %T, want *LogicalUnionExpression", innerExpr)
	}
}

func TestConvert_Delete(t *testing.T) {
	t.Parallel()
	src := logical.NewDelete("Order", logical.NewScan("Order", ""))
	got, err := plangen.Convert(src)
	if err != nil {
		t.Fatalf("Convert: %v", err)
	}
	d, ok := got.(*expressions.DeleteExpression)
	if !ok {
		t.Fatalf("got %T, want *DeleteExpression", got)
	}
	if d.GetTargetRecordType() != "Order" {
		t.Fatalf("target = %q, want Order", d.GetTargetRecordType())
	}
}

func TestConvert_Insert(t *testing.T) {
	t.Parallel()
	src := logical.NewInsert("Order", []string{"id"}, logical.NewScan("OrderSource", ""))
	got, err := plangen.Convert(src)
	if err != nil {
		t.Fatalf("Convert: %v", err)
	}
	ins, ok := got.(*expressions.InsertExpression)
	if !ok {
		t.Fatalf("got %T, want *InsertExpression", got)
	}
	if ins.GetTargetRecordType() != "Order" {
		t.Fatalf("target = %q, want Order", ins.GetTargetRecordType())
	}
}

func TestConvert_Insert_NoSource_Unsupported(t *testing.T) {
	t.Parallel()
	src := logical.NewInsert("Order", []string{"id"}, nil)
	_, err := plangen.Convert(src)
	if !errors.Is(err, plangen.ErrUnsupported) {
		t.Fatalf("got %v, want ErrUnsupported (no Source)", err)
	}
}

func TestConvert_Project_BareColumns(t *testing.T) {
	t.Parallel()
	src := logical.NewProject(
		logical.NewScan("Order", ""),
		[]string{"id", "name"},
		[]string{"", ""},
	)
	got, err := plangen.Convert(src)
	if err != nil {
		t.Fatalf("Convert: %v", err)
	}
	p, ok := got.(*expressions.LogicalProjectionExpression)
	if !ok {
		t.Fatalf("got %T, want *LogicalProjectionExpression", got)
	}
	pv := p.GetProjectedValues()
	if len(pv) != 2 {
		t.Fatalf("projected values len=%d, want 2", len(pv))
	}
	for i, want := range []string{"id", "name"} {
		fv, ok := pv[i].(*values.FieldValue)
		if !ok {
			t.Fatalf("projected[%d] = %T, want *values.FieldValue", i, pv[i])
		}
		if fv.Field != want {
			t.Fatalf("projected[%d].Field = %q, want %q", i, fv.Field, want)
		}
	}
	innerExpr := p.GetInner().GetRangesOver().Get()
	if _, ok := innerExpr.(*expressions.FullUnorderedScanExpression); !ok {
		t.Fatalf("project inner = %T, want *FullUnorderedScanExpression", innerExpr)
	}
}

func TestConvert_Project_ExpressionUnsupported(t *testing.T) {
	t.Parallel()
	// "id + 10" is not a bare column → unsupported.
	src := logical.NewProject(
		logical.NewScan("Order", ""),
		[]string{"id", "id + 10"},
		[]string{"", ""},
	)
	_, err := plangen.Convert(src)
	if !errors.Is(err, plangen.ErrUnsupported) {
		t.Fatalf("got %v, want ErrUnsupported (expression projection not yet wired)", err)
	}
}

func TestConvert_Project_EmptyList_Succeeds(t *testing.T) {
	t.Parallel()
	// LogicalProject with no projections is structurally weird but
	// should not crash. The converter produces an empty
	// LogicalProjectionExpression — the optimiser's ProjectionElim
	// rule won't fire (it needs a single QOV; empty has zero), but
	// downstream callers can still walk the tree.
	src := logical.NewProject(
		logical.NewScan("Order", ""),
		[]string{}, []string{},
	)
	got, err := plangen.Convert(src)
	if err != nil {
		t.Fatalf("Convert: %v", err)
	}
	p, ok := got.(*expressions.LogicalProjectionExpression)
	if !ok {
		t.Fatalf("got %T, want *LogicalProjectionExpression", got)
	}
	if len(p.GetProjectedValues()) != 0 {
		t.Fatalf("projected values len=%d, want 0 (empty list)", len(p.GetProjectedValues()))
	}
}

func TestConvert_Project_EmptyStringEntry_Unsupported(t *testing.T) {
	t.Parallel()
	// Empty-string projection entry isn't a valid bare column.
	src := logical.NewProject(
		logical.NewScan("Order", ""),
		[]string{""},
		[]string{""},
	)
	_, err := plangen.Convert(src)
	if !errors.Is(err, plangen.ErrUnsupported) {
		t.Fatalf("got %v, want ErrUnsupported (empty projection name)", err)
	}
}

func TestConvert_Project_DigitFirstEntry_Unsupported(t *testing.T) {
	t.Parallel()
	// Identifiers can't start with digits.
	src := logical.NewProject(
		logical.NewScan("Order", ""),
		[]string{"1col"},
		[]string{""},
	)
	_, err := plangen.Convert(src)
	if !errors.Is(err, plangen.ErrUnsupported) {
		t.Fatalf("got %v, want ErrUnsupported (digit-first identifier)", err)
	}
}

func TestConvert_Project_SpaceEntry_Unsupported(t *testing.T) {
	t.Parallel()
	// Whitespace makes it not a bare identifier.
	src := logical.NewProject(
		logical.NewScan("Order", ""),
		[]string{"col 1"},
		[]string{""},
	)
	_, err := plangen.Convert(src)
	if !errors.Is(err, plangen.ErrUnsupported) {
		t.Fatalf("got %v, want ErrUnsupported (whitespace in identifier)", err)
	}
}

func TestConvert_Project_QualifiedUnsupported(t *testing.T) {
	t.Parallel()
	// "Order.id" has a dot → unsupported (qualified-column needs scope).
	src := logical.NewProject(
		logical.NewScan("Order", ""),
		[]string{"Order.id"},
		[]string{""},
	)
	_, err := plangen.Convert(src)
	if !errors.Is(err, plangen.ErrUnsupported) {
		t.Fatalf("got %v, want ErrUnsupported (qualified-column not yet wired)", err)
	}
}

func TestConvert_Sort_BareColumns(t *testing.T) {
	t.Parallel()
	src := logical.NewSort(
		logical.NewScan("Order", ""),
		[]logical.SortKey{
			{Expr: "id", Dir: logical.SortAsc},
			{Expr: "name", Dir: logical.SortDesc},
		},
	)
	got, err := plangen.Convert(src)
	if err != nil {
		t.Fatalf("Convert: %v", err)
	}
	s, ok := got.(*expressions.LogicalSortExpression)
	if !ok {
		t.Fatalf("got %T, want *LogicalSortExpression", got)
	}
	if s.IsUnsorted() {
		t.Fatal("sort reported unsorted with 2 keys")
	}
	keys := s.GetSortKeys()
	if len(keys) != 2 {
		t.Fatalf("sort keys len=%d, want 2", len(keys))
	}
	for i, want := range []struct {
		field   string
		reverse bool
	}{
		{"id", false},
		{"name", true},
	} {
		fv, ok := keys[i].Value.(*values.FieldValue)
		if !ok {
			t.Fatalf("key[%d].Value = %T, want *values.FieldValue", i, keys[i].Value)
		}
		if fv.Field != want.field {
			t.Fatalf("key[%d].Field = %q, want %q", i, fv.Field, want.field)
		}
		if keys[i].Reverse != want.reverse {
			t.Fatalf("key[%d].Reverse = %v, want %v", i, keys[i].Reverse, want.reverse)
		}
	}
}

func TestConvert_Sort_Empty_Unsorted(t *testing.T) {
	t.Parallel()
	src := logical.NewSort(logical.NewScan("Order", ""), nil)
	got, err := plangen.Convert(src)
	if err != nil {
		t.Fatalf("Convert: %v", err)
	}
	s, ok := got.(*expressions.LogicalSortExpression)
	if !ok {
		t.Fatalf("got %T, want *LogicalSortExpression", got)
	}
	if !s.IsUnsorted() {
		t.Fatal("sort with empty Keys should be Unsorted")
	}
}

func TestConvert_Sort_ExpressionUnsupported(t *testing.T) {
	t.Parallel()
	src := logical.NewSort(
		logical.NewScan("Order", ""),
		[]logical.SortKey{{Expr: "id + 10", Dir: logical.SortAsc}},
	)
	_, err := plangen.Convert(src)
	if !errors.Is(err, plangen.ErrUnsupported) {
		t.Fatalf("got %v, want ErrUnsupported (expression sort key not yet wired)", err)
	}
}

func TestConvert_Update_BareColumnRHS(t *testing.T) {
	t.Parallel()
	src := logical.NewUpdate(
		"Order",
		[]logical.Assignment{{Column: "name", Expr: "altname"}},
		logical.NewScan("Order", ""),
	)
	got, err := plangen.Convert(src)
	if err != nil {
		t.Fatalf("Convert: %v", err)
	}
	u, ok := got.(*expressions.UpdateExpression)
	if !ok {
		t.Fatalf("got %T, want *UpdateExpression", got)
	}
	if u.GetTargetRecordType() != "Order" {
		t.Fatalf("target = %q, want Order", u.GetTargetRecordType())
	}
	tx := u.GetTransforms()
	if len(tx) != 1 {
		t.Fatalf("transforms len=%d, want 1", len(tx))
	}
	if tx[0].FieldPath != "name" {
		t.Fatalf("transform[0].FieldPath = %q, want name", tx[0].FieldPath)
	}
	fv, ok := tx[0].NewValue.(*values.FieldValue)
	if !ok || fv.Field != "altname" {
		t.Fatalf("transform[0].NewValue = %v, want FieldValue{altname}", tx[0].NewValue)
	}
}

func TestConvert_Update_ExpressionRHS_Unsupported(t *testing.T) {
	t.Parallel()
	src := logical.NewUpdate(
		"Order",
		[]logical.Assignment{{Column: "n", Expr: "n + 1"}},
		logical.NewScan("Order", ""),
	)
	_, err := plangen.Convert(src)
	if !errors.Is(err, plangen.ErrUnsupported) {
		t.Fatalf("got %v, want ErrUnsupported", err)
	}
}

func TestConvert_Update_NoInput_Unsupported(t *testing.T) {
	t.Parallel()
	src := logical.NewUpdate(
		"Order",
		[]logical.Assignment{{Column: "n", Expr: "altn"}},
		nil,
	)
	_, err := plangen.Convert(src)
	if !errors.Is(err, plangen.ErrUnsupported) {
		t.Fatalf("got %v, want ErrUnsupported (no Input)", err)
	}
}

// TestConvert_NestedFilterOverFilter — proves recursion through
// the converter walks correctly.
func TestConvert_NestedFilterOverFilter(t *testing.T) {
	t.Parallel()
	pT := predicates.NewConstantPredicate(predicates.TriTrue)
	pF := predicates.NewConstantPredicate(predicates.TriFalse)
	inner := logical.NewFilterWithPredicate(logical.NewScan("Order", ""), pT, "TRUE")
	outer := logical.NewFilterWithPredicate(inner, pF, "FALSE")
	got, err := plangen.Convert(outer)
	if err != nil {
		t.Fatalf("Convert: %v", err)
	}
	outerF, ok := got.(*expressions.LogicalFilterExpression)
	if !ok {
		t.Fatalf("got %T, want *LogicalFilterExpression", got)
	}
	innerExpr := outerF.GetInner().GetRangesOver().Get()
	innerF, ok := innerExpr.(*expressions.LogicalFilterExpression)
	if !ok {
		t.Fatalf("inner = %T, want *LogicalFilterExpression", innerExpr)
	}
	scanExpr := innerF.GetInner().GetRangesOver().Get()
	if _, ok := scanExpr.(*expressions.FullUnorderedScanExpression); !ok {
		t.Fatalf("scan = %T, want *FullUnorderedScanExpression", scanExpr)
	}
}

// TestConvert_DeeplyNested_ProjectSortFilterScan exercises the
// recursive walk through every kind of supported operator, top-down:
//
//	Project([id], Sort([id], Filter(TRUE, Scan)))
//
// Lowers to:
//
//	Projection({FieldValue(id)}, Sort({SortKey(FieldValue(id))},
//	    Filter([TRUE], FullUnorderedScan)))
//
// The scan reachable through three GetInner().GetRangesOver().Get()
// hops proves the recursion is not silently truncated.
func TestConvert_DeeplyNested_ProjectSortFilterScan(t *testing.T) {
	t.Parallel()
	pT := predicates.NewConstantPredicate(predicates.TriTrue)
	src := logical.NewProject(
		logical.NewSort(
			logical.NewFilterWithPredicate(
				logical.NewScan("Order", ""),
				pT, "TRUE",
			),
			[]logical.SortKey{{Expr: "id", Dir: logical.SortAsc}},
		),
		[]string{"id"},
		[]string{""},
	)
	got, err := plangen.Convert(src)
	if err != nil {
		t.Fatalf("Convert: %v", err)
	}
	proj, ok := got.(*expressions.LogicalProjectionExpression)
	if !ok {
		t.Fatalf("top = %T, want *LogicalProjectionExpression", got)
	}
	sort, ok := proj.GetInner().GetRangesOver().Get().(*expressions.LogicalSortExpression)
	if !ok {
		t.Fatalf("under projection = %T, want *LogicalSortExpression", proj.GetInner().GetRangesOver().Get())
	}
	filter, ok := sort.GetInner().GetRangesOver().Get().(*expressions.LogicalFilterExpression)
	if !ok {
		t.Fatalf("under sort = %T, want *LogicalFilterExpression", sort.GetInner().GetRangesOver().Get())
	}
	if _, ok := filter.GetInner().GetRangesOver().Get().(*expressions.FullUnorderedScanExpression); !ok {
		t.Fatalf("under filter = %T, want *FullUnorderedScanExpression", filter.GetInner().GetRangesOver().Get())
	}
}

// TestConvert_RecursionPropagatesErrUnsupported — when a deeply
// nested unsupported operator appears, the error wraps ErrUnsupported
// at every level so callers using errors.Is can detect it.
func TestConvert_RecursionPropagatesErrUnsupported(t *testing.T) {
	t.Parallel()
	// LogicalLimit is currently unsupported.
	inner := logical.NewLimit(logical.NewScan("Order", ""), 10, 0)
	pT := predicates.NewConstantPredicate(predicates.TriTrue)
	outer := logical.NewFilterWithPredicate(inner, pT, "TRUE")
	_, err := plangen.Convert(outer)
	if !errors.Is(err, plangen.ErrUnsupported) {
		t.Fatalf("got %v, want errors.Is(err, ErrUnsupported)", err)
	}
}

// FuzzConvert pins the no-panic invariant: Convert may return any
// (RelationalExpression, error) shape but MUST NOT panic, including
// on adversarial input shapes the SQL parser would never produce
// (deeply nested operators, empty columns/aliases, unicode quirks
// in identifiers, etc.). We don't assert on the result tree shape —
// only that the call returns cleanly.
func FuzzConvert(f *testing.F) {
	// Seed with shapes that exercise each top-level case.
	f.Add(uint64(0), "Order", "", uint8(0))
	f.Add(uint64(1), "T", "x", uint8(1))
	f.Add(uint64(2), "A", "B", uint8(2))
	f.Add(uint64(3), "x", "y", uint8(3))
	f.Add(uint64(0xff), "", "", uint8(255))
	f.Fuzz(func(t *testing.T, seed uint64, name1, name2 string, shape uint8) {
		op := buildFuzzOp(seed, name1, name2, shape)
		if op == nil {
			return
		}
		// Fail loudly on any panic.
		_, _ = plangen.Convert(op)
	})
}

// buildFuzzOp builds a small LogicalOperator tree based on the fuzz
// inputs. Keeps depth bounded (stops recursing past 4 levels) so
// fuzz iterations stay fast.
func buildFuzzOp(seed uint64, name1, name2 string, shape uint8) logical.LogicalOperator {
	const maxDepth = 4
	var build func(depth int, s uint64) logical.LogicalOperator
	build = func(depth int, s uint64) logical.LogicalOperator {
		if depth >= maxDepth {
			return logical.NewScan(name1, name2)
		}
		switch s % 10 {
		case 0:
			return logical.NewScan(name1, name2)
		case 1:
			return logical.NewFilter(build(depth+1, s>>3), name1)
		case 2:
			return logical.NewFilterWithPredicate(
				build(depth+1, s>>3),
				predicates.NewConstantPredicate(predicates.TriTrue),
				"TRUE",
			)
		case 3:
			return logical.NewUnion(
				[]logical.LogicalOperator{build(depth+1, s>>3), build(depth+1, s>>4)},
				(s&1) == 1,
			)
		case 4:
			return logical.NewProject(
				build(depth+1, s>>3),
				[]string{name1, name2},
				[]string{"", ""},
			)
		case 5:
			return logical.NewSort(
				build(depth+1, s>>3),
				[]logical.SortKey{{Expr: name1, Dir: logical.SortAsc}},
			)
		case 6:
			return logical.NewDelete(name1, build(depth+1, s>>3))
		case 7:
			return logical.NewInsert(name1, []string{name2}, build(depth+1, s>>3))
		case 8:
			return logical.NewUpdate(
				name1,
				[]logical.Assignment{{Column: name1, Expr: name2}},
				build(depth+1, s>>3),
			)
		case 9:
			// LogicalLimit is unsupported — exercises the default
			// ErrUnsupported branch + propagation through ancestors.
			return logical.NewLimit(build(depth+1, s>>3), int64(s%100), int64((s>>5)%50))
		}
		return nil
	}
	if shape&1 == 1 {
		return nil // exercise the nil-input path occasionally
	}
	return build(0, seed)
}
