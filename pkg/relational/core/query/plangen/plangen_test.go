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

func TestConvert_Scan_AliasIgnoredInSeed(t *testing.T) {
	t.Parallel()
	// LogicalScan.Alias is dropped in the seed converter — the
	// Quantifier wrapping the scan in the parent operator gets a
	// freshly-generated alias. This test pins the current behaviour
	// (no errors on non-empty alias). When proper alias propagation
	// lands (gated on parent-context-aware Convert), update this
	// test to assert the alias is preserved.
	src := logical.NewScan("Order", "o")
	got, err := plangen.Convert(src)
	if err != nil {
		t.Fatalf("Convert: %v", err)
	}
	scan, ok := got.(*expressions.FullUnorderedScanExpression)
	if !ok {
		t.Fatalf("got %T, want *FullUnorderedScanExpression", got)
	}
	// Record types preserved — only the alias is dropped.
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

func TestConvert_FilterTextSimple(t *testing.T) {
	t.Parallel()
	src := logical.NewFilter(logical.NewScan("Order", ""), "x > 5")
	got, err := plangen.Convert(src)
	if err != nil {
		t.Fatalf("Convert: %v", err)
	}
	f, ok := got.(*expressions.LogicalFilterExpression)
	if !ok {
		t.Fatalf("got %T, want *LogicalFilterExpression", got)
	}
	if len(f.GetPredicates()) != 1 {
		t.Fatalf("predicate count = %d, want 1", len(f.GetPredicates()))
	}
}

func TestConvert_FilterTextComplex_Unsupported(t *testing.T) {
	t.Parallel()
	// Complex expression with function call can't be lowered
	src := logical.NewFilter(logical.NewScan("Order", ""), "UPPER(name) = 'FOO'")
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

func TestConvert_Project_LiteralProjections(t *testing.T) {
	t.Parallel()
	// Mix of bare-column + literals exercises lowerSimpleScalarText
	// across all simple forms.
	src := logical.NewProject(
		logical.NewScan("Order", ""),
		[]string{"id", "42", "-7", "1.5", "TRUE", "false", "NULL", "'hello'"},
		[]string{"", "", "", "", "", "", "", ""},
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
	if len(pv) != 8 {
		t.Fatalf("projected values len=%d, want 8", len(pv))
	}
	// 0: id → FieldValue
	if fv, ok := pv[0].(*values.FieldValue); !ok || fv.Field != "id" {
		t.Errorf("pv[0] = %v, want FieldValue(id)", pv[0])
	}
	// 1: 42 → ConstantValue(int64(42))
	if cv, ok := pv[1].(*values.ConstantValue); !ok || cv.Value != int64(42) {
		t.Errorf("pv[1] = %v, want ConstantValue(int64(42))", pv[1])
	}
	// 2: -7 → ConstantValue(int64(-7))
	if cv, ok := pv[2].(*values.ConstantValue); !ok || cv.Value != int64(-7) {
		t.Errorf("pv[2] = %v, want ConstantValue(int64(-7))", pv[2])
	}
	// 3: 1.5 → ConstantValue(float64(1.5))
	if cv, ok := pv[3].(*values.ConstantValue); !ok || cv.Value != float64(1.5) {
		t.Errorf("pv[3] = %v, want ConstantValue(float64(1.5))", pv[3])
	}
	// 4: TRUE → ConstantValue(true)
	if cv, ok := pv[4].(*values.ConstantValue); !ok || cv.Value != true {
		t.Errorf("pv[4] = %v, want ConstantValue(true)", pv[4])
	}
	// 5: false → ConstantValue(false)
	if cv, ok := pv[5].(*values.ConstantValue); !ok || cv.Value != false {
		t.Errorf("pv[5] = %v, want ConstantValue(false)", pv[5])
	}
	// 6: NULL → NullValue
	if _, ok := pv[6].(*values.NullValue); !ok {
		t.Errorf("pv[6] = %T, want *NullValue", pv[6])
	}
	// 7: 'hello' → ConstantValue("hello")
	if cv, ok := pv[7].(*values.ConstantValue); !ok || cv.Value != "hello" {
		t.Errorf("pv[7] = %v, want ConstantValue(\"hello\")", pv[7])
	}
}

func TestConvert_Project_StringLiteralWithApostropheUnsupported(t *testing.T) {
	t.Parallel()
	// 'it''s' has an apostrophe inside the body — we don't handle ''
	// escapes. ErrUnsupported.
	src := logical.NewProject(
		logical.NewScan("Order", ""),
		[]string{"'it''s'"},
		[]string{""},
	)
	_, err := plangen.Convert(src)
	if !errors.Is(err, plangen.ErrUnsupported) {
		t.Fatalf("got %v, want ErrUnsupported (escape in string literal)", err)
	}
}

func TestConvert_Project_FloatExponentUnsupported(t *testing.T) {
	t.Parallel()
	// 1e10 / 1.5E10 not handled by the simple lowering (cross-engine
	// alignment with fdb-relational's strict-uppercase-E rule needs
	// dedicated handling).
	src := logical.NewProject(
		logical.NewScan("Order", ""),
		[]string{"1.5E10"},
		[]string{""},
	)
	_, err := plangen.Convert(src)
	if !errors.Is(err, plangen.ErrUnsupported) {
		t.Fatalf("got %v, want ErrUnsupported (exponent literal)", err)
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

func TestConvert_Sort_LiteralKey(t *testing.T) {
	t.Parallel()
	// `ORDER BY 1` (sort-by-constant — every row equal, preserves
	// natural order). Lowers to LogicalSortExpression with a
	// ConstantValue key. SQL standard treats `ORDER BY 1` as
	// "ordinal column reference" (first projection column), but the
	// lowering layer doesn't know about that — it just records the
	// literal.
	src := logical.NewSort(
		logical.NewScan("Order", ""),
		[]logical.SortKey{{Expr: "1", Dir: logical.SortAsc}},
	)
	got, err := plangen.Convert(src)
	if err != nil {
		t.Fatalf("Convert: %v", err)
	}
	s, ok := got.(*expressions.LogicalSortExpression)
	if !ok {
		t.Fatalf("got %T, want *LogicalSortExpression", got)
	}
	keys := s.GetSortKeys()
	if len(keys) != 1 {
		t.Fatalf("keys len=%d, want 1", len(keys))
	}
	cv, ok := keys[0].Value.(*values.ConstantValue)
	if !ok {
		t.Fatalf("key[0].Value = %T, want *ConstantValue", keys[0].Value)
	}
	if cv.Value != int64(1) {
		t.Fatalf("key[0].Value.Value = %v, want int64(1)", cv.Value)
	}
}

func TestConvert_Sort_MixedKeys(t *testing.T) {
	t.Parallel()
	// Sort with mixed key shapes: bare column, literal, NULL.
	// Each is lowered independently via lowerSimpleScalarText.
	src := logical.NewSort(
		logical.NewScan("Order", ""),
		[]logical.SortKey{
			{Expr: "id", Dir: logical.SortAsc},
			{Expr: "1", Dir: logical.SortDesc},
			{Expr: "NULL", Dir: logical.SortAsc},
		},
	)
	got, err := plangen.Convert(src)
	if err != nil {
		t.Fatalf("Convert: %v", err)
	}
	s := got.(*expressions.LogicalSortExpression)
	keys := s.GetSortKeys()
	if len(keys) != 3 {
		t.Fatalf("keys len=%d, want 3", len(keys))
	}
	// 0: FieldValue("id") ASC
	if _, ok := keys[0].Value.(*values.FieldValue); !ok {
		t.Errorf("keys[0].Value = %T, want *FieldValue", keys[0].Value)
	}
	if keys[0].Reverse {
		t.Errorf("keys[0].Reverse = true, want false")
	}
	// 1: ConstantValue(int64(1)) DESC
	if cv, ok := keys[1].Value.(*values.ConstantValue); !ok || cv.Value != int64(1) {
		t.Errorf("keys[1].Value = %v, want ConstantValue(int64(1))", keys[1].Value)
	}
	if !keys[1].Reverse {
		t.Errorf("keys[1].Reverse = false, want true")
	}
	// 2: NullValue ASC
	if _, ok := keys[2].Value.(*values.NullValue); !ok {
		t.Errorf("keys[2].Value = %T, want *NullValue", keys[2].Value)
	}
}

func TestConvert_Update_LiteralRHS(t *testing.T) {
	t.Parallel()
	// UPDATE Order SET active = TRUE, count = 0 — both RHSes are
	// simple literals → lowers cleanly.
	src := logical.NewUpdate(
		"Order",
		[]logical.Assignment{
			{Column: "active", Expr: "TRUE"},
			{Column: "count", Expr: "0"},
		},
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
	tx := u.GetTransforms()
	if len(tx) != 2 {
		t.Fatalf("transforms len=%d, want 2", len(tx))
	}
	// Canonical sort: "active" before "count".
	if tx[0].FieldPath != "active" {
		t.Fatalf("tx[0].FieldPath = %q, want active", tx[0].FieldPath)
	}
	cv, ok := tx[0].NewValue.(*values.ConstantValue)
	if !ok || cv.Value != true {
		t.Fatalf("tx[0].NewValue = %v, want ConstantValue(true)", tx[0].NewValue)
	}
	cv2, ok := tx[1].NewValue.(*values.ConstantValue)
	if !ok || cv2.Value != int64(0) {
		t.Fatalf("tx[1].NewValue = %v, want ConstantValue(int64(0))", tx[1].NewValue)
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

func TestConvert_Update_MultipleSetsBareColumn(t *testing.T) {
	t.Parallel()
	// UPDATE Order SET name = altname, status = altstatus
	// All RHSes are bare-column → all transform.
	// The UpdateExpression canonicalises by sorting by FieldPath.
	src := logical.NewUpdate(
		"Order",
		[]logical.Assignment{
			{Column: "name", Expr: "altname"},
			{Column: "status", Expr: "altstatus"},
		},
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
	tx := u.GetTransforms()
	if len(tx) != 2 {
		t.Fatalf("transforms len=%d, want 2", len(tx))
	}
	// Canonical (sorted-by-FieldPath) order: "name" before "status".
	if tx[0].FieldPath != "name" || tx[1].FieldPath != "status" {
		t.Fatalf("transforms not in canonical order: got [%q, %q], want [name, status]",
			tx[0].FieldPath, tx[1].FieldPath)
	}
}

func TestConvert_Update_OneRHSExpression_Unsupported(t *testing.T) {
	t.Parallel()
	// UPDATE Order SET name = altname, status = altstatus + 1 — second RHS
	// has arithmetic → ErrUnsupported (no partial conversion).
	src := logical.NewUpdate(
		"Order",
		[]logical.Assignment{
			{Column: "name", Expr: "altname"},
			{Column: "status", Expr: "altstatus + 1"},
		},
		logical.NewScan("Order", ""),
	)
	_, err := plangen.Convert(src)
	if !errors.Is(err, plangen.ErrUnsupported) {
		t.Fatalf("got %v, want ErrUnsupported (one RHS is non-bare)", err)
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
	// LEFT JOIN with text-only ON is unsupported.
	inner := logical.NewJoin(logical.NewScan("A", ""), logical.NewScan("B", ""), logical.JoinLeft, "a.id = b.aid")
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
	// Literal seeds — exercise lowerSimpleScalarText branches that
	// were previously dead in Project / Sort / Update cases.
	f.Add(uint64(4), "42", "true", uint8(4))
	f.Add(uint64(5), "1.5", "NULL", uint8(5))
	f.Add(uint64(6), "'hello'", "FALSE", uint8(8))
	f.Add(uint64(7), "-7", "true", uint8(4))
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

func TestConvert_Aggregate_Basic(t *testing.T) {
	t.Parallel()
	src := logical.NewAggregate(
		logical.NewScan("Orders", ""),
		[]string{"customer_id"},
		[]string{"COUNT(id)", "SUM(amount)"},
		[]string{"cnt", "total"},
		"",
	)
	got, err := plangen.Convert(src)
	if err != nil {
		t.Fatalf("Convert: %v", err)
	}
	gb, ok := got.(*expressions.GroupByExpression)
	if !ok {
		t.Fatalf("got %T, want *GroupByExpression", got)
	}
	if len(gb.GetGroupingKeys()) != 1 {
		t.Fatalf("grouping keys = %d, want 1", len(gb.GetGroupingKeys()))
	}
	if values.ExplainValue(gb.GetGroupingKeys()[0]) != "customer_id" {
		t.Fatalf("grouping key = %q, want customer_id", values.ExplainValue(gb.GetGroupingKeys()[0]))
	}
	aggs := gb.GetAggregates()
	if len(aggs) != 2 {
		t.Fatalf("aggregates = %d, want 2", len(aggs))
	}
	if aggs[0].Function != expressions.AggCount {
		t.Fatalf("agg[0].Function = %d, want AggCount", aggs[0].Function)
	}
	if values.ExplainValue(aggs[0].Operand) != "id" {
		t.Fatalf("agg[0].Operand = %q, want id", values.ExplainValue(aggs[0].Operand))
	}
	if aggs[1].Function != expressions.AggSum {
		t.Fatalf("agg[1].Function = %d, want AggSum", aggs[1].Function)
	}
}

func TestConvert_Aggregate_CountStar(t *testing.T) {
	t.Parallel()
	src := logical.NewAggregate(
		logical.NewScan("T", ""),
		nil, // no grouping keys = global aggregate
		[]string{"COUNT(*)"},
		[]string{"total"},
		"",
	)
	got, err := plangen.Convert(src)
	if err != nil {
		t.Fatalf("Convert: %v", err)
	}
	gb := got.(*expressions.GroupByExpression)
	if len(gb.GetGroupingKeys()) != 0 {
		t.Fatalf("grouping keys = %d, want 0", len(gb.GetGroupingKeys()))
	}
	if len(gb.GetAggregates()) != 1 {
		t.Fatalf("aggregates = %d, want 1", len(gb.GetAggregates()))
	}
	if gb.GetAggregates()[0].Function != expressions.AggCount {
		t.Fatal("expected COUNT function")
	}
	if values.ExplainValue(gb.GetAggregates()[0].Operand) != "*" {
		t.Fatalf("expected * operand, got %q", values.ExplainValue(gb.GetAggregates()[0].Operand))
	}
}

func TestConvert_Aggregate_AllFunctions(t *testing.T) {
	t.Parallel()
	src := logical.NewAggregate(
		logical.NewScan("T", ""),
		[]string{"g"},
		[]string{"COUNT(a)", "SUM(b)", "MIN(c)", "MAX(d)", "AVG(e)"},
		nil,
		"",
	)
	got, err := plangen.Convert(src)
	if err != nil {
		t.Fatalf("Convert: %v", err)
	}
	gb := got.(*expressions.GroupByExpression)
	aggs := gb.GetAggregates()
	if len(aggs) != 5 {
		t.Fatalf("aggregates = %d, want 5", len(aggs))
	}
	expected := []expressions.AggregateFunction{
		expressions.AggCount, expressions.AggSum,
		expressions.AggMin, expressions.AggMax, expressions.AggAvg,
	}
	for i, exp := range expected {
		if aggs[i].Function != exp {
			t.Fatalf("aggs[%d].Function = %d, want %d", i, aggs[i].Function, exp)
		}
	}
}

func TestConvert_Aggregate_UnsupportedFunction(t *testing.T) {
	t.Parallel()
	src := logical.NewAggregate(
		logical.NewScan("T", ""),
		nil,
		[]string{"MEDIAN(x)"},
		nil,
		"",
	)
	_, err := plangen.Convert(src)
	if !errors.Is(err, plangen.ErrUnsupported) {
		t.Fatalf("expected ErrUnsupported, got %v", err)
	}
}

func TestConvert_Aggregate_ComplexOperand(t *testing.T) {
	t.Parallel()
	// SUM(a + b) is too complex for the simple text parser.
	src := logical.NewAggregate(
		logical.NewScan("T", ""),
		nil,
		[]string{"SUM(a + b)"},
		nil,
		"",
	)
	_, err := plangen.Convert(src)
	if !errors.Is(err, plangen.ErrUnsupported) {
		t.Fatalf("expected ErrUnsupported for complex operand, got %v", err)
	}
}

func TestConvert_Aggregate_ComplexGroupKey(t *testing.T) {
	t.Parallel()
	// GROUP BY (a + b) is too complex for the simple text lowering.
	src := logical.NewAggregate(
		logical.NewScan("T", ""),
		[]string{"a + b"},
		[]string{"COUNT(x)"},
		nil,
		"",
	)
	_, err := plangen.Convert(src)
	if !errors.Is(err, plangen.ErrUnsupported) {
		t.Fatalf("expected ErrUnsupported for complex group key, got %v", err)
	}
}

func TestConvert_Limit(t *testing.T) {
	t.Parallel()
	src := logical.NewLimit(logical.NewScan("T", ""), 10, 0)
	expr, err := plangen.Convert(src)
	if err != nil {
		t.Fatalf("Convert: %v", err)
	}
	lim, ok := expr.(*expressions.LogicalLimitExpression)
	if !ok {
		t.Fatalf("expected *LogicalLimitExpression, got %T", expr)
	}
	if lim.GetLimit() != 10 {
		t.Fatalf("limit = %d, want 10", lim.GetLimit())
	}
	if lim.GetOffset() != 0 {
		t.Fatalf("offset = %d, want 0", lim.GetOffset())
	}
}

func TestConvert_LimitWithOffset(t *testing.T) {
	t.Parallel()
	src := logical.NewLimit(logical.NewScan("T", ""), 5, 20)
	expr, err := plangen.Convert(src)
	if err != nil {
		t.Fatalf("Convert: %v", err)
	}
	lim, ok := expr.(*expressions.LogicalLimitExpression)
	if !ok {
		t.Fatalf("expected *LogicalLimitExpression, got %T", expr)
	}
	if lim.GetLimit() != 5 {
		t.Fatalf("limit = %d, want 5", lim.GetLimit())
	}
	if lim.GetOffset() != 20 {
		t.Fatalf("offset = %d, want 20", lim.GetOffset())
	}
}

func TestConvert_LimitOverSort(t *testing.T) {
	t.Parallel()
	sorted := logical.NewSort(logical.NewScan("T", ""), []logical.SortKey{{Expr: "name", Dir: logical.SortAsc}})
	src := logical.NewLimit(sorted, 10, 0)
	expr, err := plangen.Convert(src)
	if err != nil {
		t.Fatalf("Convert: %v", err)
	}
	lim, ok := expr.(*expressions.LogicalLimitExpression)
	if !ok {
		t.Fatalf("expected *LogicalLimitExpression, got %T", expr)
	}
	_ = lim
}

func TestConvert_Join_CrossJoin(t *testing.T) {
	t.Parallel()
	src := logical.NewJoin(logical.NewScan("A", ""), logical.NewScan("B", ""), logical.JoinInner, "")
	expr, err := plangen.Convert(src)
	if err != nil {
		t.Fatalf("Convert: %v", err)
	}
	sel, ok := expr.(*expressions.SelectExpression)
	if !ok {
		t.Fatalf("expected *SelectExpression, got %T", expr)
	}
	if len(sel.GetQuantifiers()) != 2 {
		t.Fatalf("expected 2 quantifiers, got %d", len(sel.GetQuantifiers()))
	}
	if len(sel.GetPredicates()) != 0 {
		t.Fatalf("expected 0 predicates (cross join), got %d", len(sel.GetPredicates()))
	}
}

func TestConvert_Join_InnerWithPredicate(t *testing.T) {
	t.Parallel()
	pred := predicates.NewComparisonPredicate(
		&values.FieldValue{Field: "a_id", Typ: values.TypeInt},
		predicates.NewLiteralComparison(predicates.ComparisonEquals, int64(1)),
	)
	src := logical.NewJoinWithPredicate(
		logical.NewScan("A", ""),
		logical.NewScan("B", ""),
		logical.JoinInner,
		pred,
	)
	expr, err := plangen.Convert(src)
	if err != nil {
		t.Fatalf("Convert: %v", err)
	}
	sel, ok := expr.(*expressions.SelectExpression)
	if !ok {
		t.Fatalf("expected *SelectExpression, got %T", expr)
	}
	if len(sel.GetQuantifiers()) != 2 {
		t.Fatalf("expected 2 quantifiers, got %d", len(sel.GetQuantifiers()))
	}
	if len(sel.GetPredicates()) != 1 {
		t.Fatalf("expected 1 predicate, got %d", len(sel.GetPredicates()))
	}
}

func TestConvert_Join_InnerWithTextPredicate(t *testing.T) {
	t.Parallel()
	src := logical.NewJoin(logical.NewScan("A", ""), logical.NewScan("B", ""), logical.JoinInner, "id = aid")
	expr, err := plangen.Convert(src)
	if err != nil {
		t.Fatalf("Convert: %v", err)
	}
	sel, ok := expr.(*expressions.SelectExpression)
	if !ok {
		t.Fatalf("expected *SelectExpression, got %T", expr)
	}
	if len(sel.GetPredicates()) != 1 {
		t.Fatalf("expected 1 predicate, got %d", len(sel.GetPredicates()))
	}
}

func TestConvert_Join_LeftJoinNoStructuredPred_Unsupported(t *testing.T) {
	t.Parallel()
	src := logical.NewJoin(logical.NewScan("A", ""), logical.NewScan("B", ""), logical.JoinLeft, "a.id = b.aid")
	_, err := plangen.Convert(src)
	if !errors.Is(err, plangen.ErrUnsupported) {
		t.Fatalf("expected ErrUnsupported, got %v", err)
	}
}

func TestConvert_Join_ComplexTextPredicate_Unsupported(t *testing.T) {
	t.Parallel()
	// Dotted reference in ON clause is too complex for the simple parser
	src := logical.NewJoin(logical.NewScan("A", ""), logical.NewScan("B", ""), logical.JoinInner, "a.id = b.aid")
	_, err := plangen.Convert(src)
	if !errors.Is(err, plangen.ErrUnsupported) {
		t.Fatalf("expected ErrUnsupported for dotted ref, got %v", err)
	}
}
