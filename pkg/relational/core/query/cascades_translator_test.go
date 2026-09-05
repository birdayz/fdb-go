package query

import (
	"errors"
	"strings"
	"testing"

	"fdb.dev/gen"
	"fdb.dev/pkg/recordlayer"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/relational/api"
	"fdb.dev/pkg/relational/core/query/logical"
)

func TestPartitionCorrelatedScalarWherePredicate_OnlyTopLevelAndMoves(t *testing.T) {
	t.Parallel()

	outer := predicates.NewComparisonPredicate(
		exactTestNamedField(t, "P", "ID", values.NotNullLong),
		predicates.Comparison{
			Type:    predicates.ComparisonEquals,
			Operand: &values.ConstantValue{Value: int64(2)},
		},
	)
	scalar := predicates.NewComparisonPredicate(
		values.NewScalarSubqueryValue(values.NamedCorrelationIdentifier("SQ"), values.NullableLong),
		predicates.Comparison{
			Type:    predicates.ComparisonEquals,
			Operand: &values.ConstantValue{Value: int64(30)},
		},
	)

	pre, post := partitionCorrelatedScalarWherePredicate(
		predicates.NewAnd(outer, scalar),
		"P",
	)
	if !predicates.PredicateEquals(pre, outer) {
		t.Fatalf("top-level outer conjunct was not isolated before the scalar box: %v", pre)
	}
	if !predicates.PredicateEquals(post, scalar) {
		t.Fatalf("scalar conjunct did not remain above the scalar box: %v", post)
	}

	for name, wrapped := range map[string]predicates.QueryPredicate{
		"or":  predicates.NewOr(outer, scalar),
		"not": predicates.NewNot(predicates.NewAnd(outer, scalar)),
	} {
		t.Run(name, func(t *testing.T) {
			gotPre, gotPost := partitionCorrelatedScalarWherePredicate(wrapped, "P")
			if gotPre != nil {
				t.Fatalf("wrapped predicate produced a pre-scalar conjunct: %v", gotPre)
			}
			if !predicates.PredicateEquals(gotPost, wrapped) {
				t.Fatalf("wrapped predicate was distributed across the scalar box: %v", gotPost)
			}
		})
	}
}

// TestTableColumns_FromMetadata pins the md→columns derivation (tableColumns +
// FieldTypeForFD) that sources both exact scan rows and source-anchored join-leg
// columns. Columns are upper-cased and nested/repeated fields retain their exact
// proto-derived types.
func TestTableColumns_FromMetadata(t *testing.T) {
	t.Parallel()

	builder := recordlayer.NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
	builder.GetRecordType("Order").SetPrimaryKey(recordlayer.Field("order_id"))
	builder.GetRecordType("Customer").SetPrimaryKey(recordlayer.Field("customer_id"))
	builder.GetRecordType("TypedRecord").SetPrimaryKey(recordlayer.Field("id"))
	md, err := builder.Build()
	if err != nil {
		t.Fatalf("build metadata: %v", err)
	}

	tr := &cascadesTranslator{md: md}
	cols := tr.tableColumns("Order")
	if cols == nil {
		t.Fatal("tableColumns(Order) returned nil")
	}

	byName := make(map[string]values.Type, len(cols))
	for _, c := range cols {
		byName[c.Name] = c.FieldType
	}
	// Scalar Kind mapping (Order proto: order_id int64, price int32, vector_data bytes).
	primitive := func(col string, want values.TypeCode) {
		t.Helper()
		ft, ok := byName[col]
		if !ok {
			t.Fatalf("column %q missing; got columns %v", col, byName)
		}
		pt, ok := ft.(*values.PrimitiveType)
		if !ok {
			t.Fatalf("column %q: type %T, want *PrimitiveType", col, ft)
		}
		if pt.TypeCode != want {
			t.Errorf("column %q: TypeCode %v, want %v", col, pt.TypeCode, want)
		}
	}
	primitive("order_id", values.TypeCodeLong)
	primitive("price", values.TypeCodeInt)
	primitive("vector_data", values.TypeCodeBytes)
	flower, flowerOK := byName["flower"].(*values.RecordType)
	if !flowerOK || !flower.IsNullable() || len(flower.Fields) != 2 ||
		flower.Fields[0].Name != "type" || flower.Fields[0].FieldType.Code() != values.TypeCodeString ||
		flower.Fields[1].Name != "color" || flower.Fields[1].FieldType.Code() != values.TypeCodeEnum {
		t.Errorf("flower (message): got %v, want exact nullable type/color record", byName["flower"])
	}
	tags, tagsOK := byName["tags"].(*values.ArrayType)
	if !tagsOK || tags.IsNullable() || tags.ElementType == nil ||
		!tags.ElementType.Equals(values.NotNullString) {
		t.Errorf("tags (repeated): got %v, want ARRAY<STRING NOT NULL> NOT NULL", byName["tags"])
	}

	// nil md and unknown table fall back to nil (no typing source).
	if (&cascadesTranslator{}).tableColumns("Order") != nil {
		t.Error("nil-md tableColumns must be nil")
	}
	if tr.tableColumns("NoSuchTable") != nil {
		t.Error("unknown-table tableColumns must be nil")
	}
}

// demoMetaData builds the record_layer_demo metadata used by the leg-column
// derivation tests (Order, Customer — both carry a PRICE column, a duplicate bare
// name across legs).
func demoMetaData(t *testing.T) *recordlayer.RecordMetaData {
	t.Helper()
	builder := recordlayer.NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
	builder.GetRecordType("Order").SetPrimaryKey(recordlayer.Field("order_id"))
	builder.GetRecordType("Customer").SetPrimaryKey(recordlayer.Field("customer_id"))
	builder.GetRecordType("TypedRecord").SetPrimaryKey(recordlayer.Field("id"))
	md, err := builder.Build()
	if err != nil {
		t.Fatalf("build metadata: %v", err)
	}
	return md
}

// demoTableQOV mirrors the exact row authority translateScan obtains from the
// same demo metadata. Direct logical-tree fixtures use it when they must state
// a sort/group/aggregate Value before the translator has minted its quantifier.
func demoTableQOV(t *testing.T, table, alias string) values.QuantifiedObjectValue {
	t.Helper()
	cols := (&cascadesTranslator{md: demoMetaData(t)}).tableColumns(table)
	if len(cols) == 0 {
		t.Fatalf("demo table %q has no exact columns", table)
	}
	if alias == "" {
		alias = table
	}
	return exactTestQOV(t, alias, values.NewRecordType("", false, cols))
}

// TestLegColumns_NamingConsistentWithAnchoredRecord pins, IN ISOLATION (RFC-077
// 7.6 step 1), that legColumns produces names CONSISTENT with NewAnchoredJoinRecord
// for every supported leg shape, so a parent join's anchored RC composes its legs:
//
//   - scan → the table's bare metadata columns;
//   - filter / limit → the inner's columns (row-shape-preserving);
//   - join → EXACTLY the anchored RC's field names over its legs (qualified
//     ALIAS.COL + bare-unique, dotted-propagated), so a 3-way join's outer leg
//     (itself a join) contributes already-qualified names the parent propagates
//     verbatim;
//   - unsupported shape (aggregate, distinct, …) → nil.
func TestLegColumns_NamingConsistentWithAnchoredRecord(t *testing.T) {
	t.Parallel()
	md := demoMetaData(t)
	tr := &cascadesTranslator{md: md}

	names := func(fs []values.Field) map[string]bool {
		m := map[string]bool{}
		for _, f := range fs {
			m[f.Name] = true
		}
		return m
	}

	// (1) Scan → bare metadata columns, spelled as the descriptor declares them.
	scanCols := names(tr.legColumns(logical.NewScan("Order", "O")))
	for _, c := range []string{"order_id", "price", "quantity"} {
		if !scanCols[c] {
			t.Errorf("scan leg missing bare column %q; got %v", c, scanCols)
		}
	}

	// (2) Filter / limit preserve the inner scan's columns.
	filterCols := names(tr.legColumns(logical.NewFilter(logical.NewScan("Order", "O"), "price > 1")))
	if len(filterCols) != len(scanCols) {
		t.Errorf("filter leg columns %v != scan leg columns %v (filter must preserve shape)", filterCols, scanCols)
	}
	limitCols := names(tr.legColumns(logical.NewLimit(logical.NewScan("Order", "O"), 10, 0)))
	if len(limitCols) != len(scanCols) {
		t.Errorf("limit leg columns %v != scan leg columns %v (limit must preserve shape)", limitCols, scanCols)
	}

	// (3) Join → ONLY the DOTTED (source-accurate) per-table columns: a bare leg
	// column qualifies as UPPER(ALIAS).UPPER(COL), an already-dotted one
	// propagates verbatim — NOT the bare-last-wins names (those were the retired
	// name-model RC's own resolution convenience; propagating them would make a
	// parent re-qualify them into spurious "_2" keys — the nested-parity bug).
	join := logical.NewJoin(logical.NewScan("Order", "O"), logical.NewScan("Customer", "C"), logical.JoinInner, "")
	joinLegCols := tr.legColumns(join)
	gotNames := names(joinLegCols)
	// Expected = each leg's columns qualified by its alias (dotted verbatim).
	wantDotted := map[string]bool{}
	for _, leg := range []struct {
		alias string
		cols  []values.Field
	}{
		{"O", tr.legColumns(logical.NewScan("Order", "O"))},
		{"C", tr.legColumns(logical.NewScan("Customer", "C"))},
	} {
		for _, c := range leg.cols {
			name := strings.ToUpper(c.Name)
			if !strings.Contains(c.Name, ".") {
				name = strings.ToUpper(leg.alias) + "." + name
			}
			wantDotted[name] = true
		}
	}
	for k := range wantDotted {
		if !gotNames[k] {
			t.Errorf("join legColumns missing dotted RC field %q; got %v", k, gotNames)
		}
	}
	for k := range gotNames {
		if !wantDotted[k] {
			t.Errorf("join legColumns has %q which is not a dotted RC field; got %v want %v", k, gotNames, wantDotted)
		}
	}
	// The shared duplicate PRICE is exposed ONLY qualified (O.PRICE, C.PRICE) — NO
	// bare PRICE propagates (the bare-last-wins lives in the join's own result value,
	// not in what it exposes to a parent).
	if gotNames["PRICE"] {
		t.Errorf("join legColumns must NOT propagate bare PRICE (dotted-only); got %v", gotNames)
	}
	if !gotNames["O.PRICE"] || !gotNames["C.PRICE"] {
		t.Errorf("join legColumns must expose qualified O.PRICE and C.PRICE; got %v", gotNames)
	}

	// (3b) NESTED join — the outer leg is itself a join, so its already-qualified
	// (dotted) names propagate VERBATIM into the parent's leg columns.
	nested := logical.NewJoin(join, logical.NewScan("TypedRecord", "TR"), logical.JoinInner, "")
	nestedCols := names(tr.legColumns(nested))
	if !nestedCols["O.PRICE"] || !nestedCols["C.PRICE"] {
		t.Errorf("nested join must propagate the inner join's qualified O.PRICE/C.PRICE verbatim; got %v", nestedCols)
	}
	if nestedCols["TR.O.PRICE"] {
		t.Error("nested join must NOT re-qualify a dotted column to TR.O.PRICE")
	}

	// (4) Row-shape-preserving shapes (sort / distinct) now ANCHOR via their inner
	// (RFC-077 7.6 step 2), preserving the inner scan's column set.
	distinctCols := names(tr.legColumns(logical.NewDistinct(logical.NewScan("Order", "O"))))
	if len(distinctCols) != len(scanCols) {
		t.Errorf("distinct leg columns %v != inner scan columns %v (distinct is row-shape-preserving)", distinctCols, scanCols)
	}
	sortCols := names(tr.legColumns(logical.NewSort(logical.NewScan("Order", "O"), nil)))
	if len(sortCols) != len(scanCols) {
		t.Errorf("sort leg columns %v != inner scan columns %v (sort is row-shape-preserving)", sortCols, scanCols)
	}

	// (5) Genuinely-unsupported shapes (DML / nil) → nil.
	if tr.legColumns(logical.NewInsert("Order", nil, nil)) != nil {
		t.Error("insert leg must derive nil columns (not a row-producing join leg)")
	}
	if tr.legColumns(nil) != nil {
		t.Error("nil leg must derive nil columns")
	}

	// (5) nil-md translator derives nil for a scan (no typing source) → a join
	// over it is untranslatable (the opaque-merge fallback was retired, RFC-077 7.6).
	if (&cascadesTranslator{}).legColumns(logical.NewScan("Order", "O")) != nil {
		t.Error("nil-md leg columns must be nil (no typing source)")
	}
}

// TestLegColumns_NestedNoSpuriousKeys pins the nested-parity property
// (RFC-077 7.6): a 3-way nested join's exposed leg schema must carry only
// SOURCE-ACCURATE dotted keys and NO dedup-suffixed "_2" garbage. Before the
// dotted-only legColumns fix, a sub-join leg's bare columns were re-qualified
// under sourceAlias(inner)=right-leg, colliding with the inner's verbatim dotted
// keys — spurious keys the runtime merge never produces.
func TestLegColumns_NestedNoSpuriousKeys(t *testing.T) {
	t.Parallel()
	tr := &cascadesTranslator{md: demoMetaData(t)}

	// 3-way: (Order O ⋈ Customer C) ⋈ TypedRecord TR. O and C share PRICE.
	inner := logical.NewJoin(logical.NewScan("Order", "O"), logical.NewScan("Customer", "C"), logical.JoinInner, "")
	outer := logical.NewJoin(inner, logical.NewScan("TypedRecord", "TR"), logical.JoinInner, "")

	cols := tr.legColumns(outer)
	if cols == nil {
		t.Fatal("3-way nested join leg schema must be derivable")
	}
	got := map[string]bool{}
	for _, c := range cols {
		if strings.Contains(c.Name, "_2") || strings.HasSuffix(c.Name, "_3") {
			t.Errorf("spurious dedup-suffixed key %q (nested-parity bug — a bare leg name was re-qualified into an existing dotted key)", c.Name)
		}
		got[c.Name] = true
	}
	// The inner join's source-accurate dotted keys propagate verbatim.
	for _, want := range []string{"O.PRICE", "C.PRICE", "O.ORDER_ID", "C.CUSTOMER_ID"} {
		if !got[want] {
			t.Errorf("3-way nested leg schema missing source-accurate dotted key %q; got %v", want, got)
		}
	}
}

// Positional machinery projections intentionally carry nil ProjectedValues:
// AggregateOutputOrdinals is their exact source-layout contract.  legColumns
// feeds executable ordinal seeds, so it must preserve the projected aggregate
// type rather than reinterpret the nil Value as UNKNOWN.
func TestLegColumns_PositionalProjectionPreservesExactType(t *testing.T) {
	t.Parallel()
	md := demoMetaData(t)
	tr := &cascadesTranslator{md: md}
	scan := logical.NewScan("Order", "O")
	orderRow := demoTableQOV(t, "Order", "O")
	price := exactTestField(t, orderRow, 2)
	agg := logical.NewAggregate(
		scan,
		nil,
		[]logical.AggregateCall{{Func: "SUM", Operand: "PRICE", BareColumn: true}},
		[]string{"SUM(O.PRICE)"},
		false,
	)
	agg.AggregateOperands = []values.Value{price}
	project := logical.NewProject(agg, []string{"SUM(O.PRICE)"}, nil)
	project.ProjectedValues = []values.Value{nil}
	project.AggregateOutputOrdinals = []int{0}

	cols := tr.legColumns(project)
	if len(cols) != 1 {
		t.Fatalf("positional projection columns = %v, want one exact aggregate slot", cols)
	}
	if cols[0].FieldType == nil || cols[0].FieldType.Code() != price.Type().Code() || !cols[0].FieldType.IsNullable() {
		t.Fatalf("positional projection type = %v, want nullable %v", cols[0].FieldType, price.Type().Code())
	}
}

func TestTranslateScan(t *testing.T) {
	t.Parallel()
	scan := logical.NewScan("Order", "")
	ref, _ := TranslateToCascadesWithSubqueries(scan, demoMetaData(t))
	if ref == nil {
		t.Fatal("expected non-nil reference")
	}
	members := ref.Members()
	if len(members) != 1 {
		t.Fatalf("expected 1 member, got %d", len(members))
	}
	if _, ok := members[0].(*expressions.FullUnorderedScanExpression); !ok {
		t.Fatalf("expected FullUnorderedScanExpression, got %T", members[0])
	}
}

func TestTranslateFilterOverScan(t *testing.T) {
	t.Parallel()
	scan := logical.NewScan("orders", "")
	filter := logical.NewFilter(scan, "price > 10")
	ref := TranslateToCascades(filter)
	if ref != nil {
		t.Fatal("expected nil: text-only predicate must not translate")
	}
}

func TestTranslateLimit(t *testing.T) {
	t.Parallel()
	// RFC-128: every LogicalLimit is translated to a LogicalLimitExpression
	// (→ RecordQueryLimitPlan), applied at its pipeline position — NOT skipped
	// and post-execution-hoisted. The top member is the limit expression; its
	// inner ranges over the scan.
	scan := logical.NewScan("Order", "")
	limit := logical.NewLimit(scan, 10, 5)
	ref, _ := TranslateToCascadesWithSubqueries(limit, demoMetaData(t))
	if ref == nil {
		t.Fatal("expected non-nil reference")
	}
	members := ref.Members()
	limExpr, ok := members[0].(*expressions.LogicalLimitExpression)
	if !ok {
		t.Fatalf("expected LogicalLimitExpression, got %T", members[0])
	}
	if limExpr.GetLimit() != 10 || limExpr.GetOffset() != 5 {
		t.Fatalf("limit/offset = %d/%d, want 10/5", limExpr.GetLimit(), limExpr.GetOffset())
	}
	innerRef := limExpr.GetInner().GetRangesOver()
	if innerRef == nil {
		t.Fatal("limit inner ranges over nil")
	}
	if _, ok := innerRef.Members()[0].(*expressions.FullUnorderedScanExpression); !ok {
		t.Fatalf("limit inner = %T, want FullUnorderedScanExpression", innerRef.Members()[0])
	}
}

func TestTranslateUnion(t *testing.T) {
	t.Parallel()
	scanA := logical.NewScan("Order", "A")
	scanB := logical.NewScan("Order", "B")
	union := logical.NewUnion([]logical.LogicalOperator{scanA, scanB}, false)
	ref, _ := TranslateToCascadesWithSubqueries(union, demoMetaData(t))
	if ref == nil {
		t.Fatal("expected non-nil reference")
	}
	if _, ok := ref.Members()[0].(*expressions.LogicalUnionExpression); !ok {
		t.Fatalf("expected LogicalUnionExpression, got %T", ref.Members()[0])
	}
}

func TestTranslateDistinctUnion(t *testing.T) {
	t.Parallel()
	scanA := logical.NewScan("A", "")
	scanB := logical.NewScan("B", "")
	union := logical.NewUnion([]logical.LogicalOperator{scanA, scanB}, true)
	ref := TranslateToCascades(union)
	if ref != nil {
		t.Fatal("expected nil: UNION DISTINCT is rejected (Java alignment)")
	}
}

func TestTranslateSort(t *testing.T) {
	t.Parallel()
	scan := logical.NewScan("Order", "O")
	row := demoTableQOV(t, "Order", "O")
	sort := logical.NewSort(scan, []logical.SortKey{
		{Expr: "PRICE", Dir: logical.SortAsc, Value: exactTestField(t, row, 2)},
		{Expr: "ORDER_ID", Dir: logical.SortDesc, Value: exactTestField(t, row, 0)},
	})
	ref, _ := TranslateToCascadesWithSubqueries(sort, demoMetaData(t))
	if ref == nil {
		t.Fatal("expected non-nil reference")
	}
	if _, ok := ref.Members()[0].(*expressions.LogicalSortExpression); !ok {
		t.Fatalf("expected LogicalSortExpression, got %T", ref.Members()[0])
	}
}

func TestTranslateDerivedSortKeyToPhysicalInputOnlyNormalizesTopLevelNames(t *testing.T) {
	t.Parallel()
	alias := values.NamedCorrelationIdentifier("S")
	logicalType := values.NewRecordType("", false, []values.Field{
		{Name: "a.b", Ordinal: 0, FieldType: values.NullableLong},
	})
	physicalType := values.NewRecordType("", false, []values.Field{
		{Name: "A.B", Ordinal: 0, FieldType: values.NullableLong},
	})
	logicalOwner := exactTestQOV(t, alias.Name(), logicalType)
	physicalOwner := exactTestQOV(t, alias.Name(), physicalType)
	logicalField := exactTestField(t, logicalOwner, 0)

	normalized, err := translateDerivedSortKeyToPhysicalInput(logicalField, physicalOwner)
	if err != nil {
		t.Fatalf("normalize derived sort key: %v", err)
	}
	normalizedField, ok := values.AsFieldValue(normalized)
	if !ok || normalizedField.ChildValue() != physicalOwner {
		t.Fatalf("normalized key = %T/%v, want exact physical owner %p",
			normalized, normalizedField, physicalOwner)
	}
	if got := normalizedField.Path().Ordinals(); len(got) != 1 || got[0] != 0 {
		t.Fatalf("normalized key path = %v, want [0]", got)
	}
	if !normalizedField.ResultType().Equals(values.NullableLong) {
		t.Fatalf("normalized key type = %s, want nullable LONG", normalizedField.ResultType())
	}
	if field, fieldOK := values.AsFieldValue(logicalField); !fieldOK || field.ChildValue() != logicalOwner {
		t.Fatal("normalization mutated the authored derived key")
	}

	// An ordinary derived output whose logical and physical declarations already
	// agree is outside the names-only bridge. Keep its Value pointer stable; the
	// later declared-edge translation owns that phase change.
	samePhysical := exactTestQOV(t, alias.Name(), logicalType)
	unchanged, err := translateDerivedSortKeyToPhysicalInput(logicalField, samePhysical)
	if err != nil || unchanged != logicalField {
		t.Fatalf("same-name key = (%v, %v), want unchanged", unchanged, err)
	}

	foreignOwner := exactTestQOV(t, "FOREIGN", logicalType)
	foreignField := exactTestField(t, foreignOwner, 0)
	unchanged, err = translateDerivedSortKeyToPhysicalInput(foreignField, physicalOwner)
	if err != nil || unchanged != foreignField {
		t.Fatalf("foreign key = (%v, %v), want unchanged", unchanged, err)
	}

	computed := &values.ArithmeticValue{
		Op:    values.OpAdd,
		Left:  logicalField,
		Right: values.LiteralValue(int64(1)),
	}
	unchanged, err = translateDerivedSortKeyToPhysicalInput(computed, physicalOwner)
	if err != nil || unchanged != computed {
		t.Fatalf("computed key = (%v, %v), want unchanged", unchanged, err)
	}

	for _, test := range []struct {
		name string
		typ  values.Type
	}{
		{
			name: "width",
			typ: values.NewRecordType("", false, []values.Field{
				{Name: "A.B", Ordinal: 0, FieldType: values.NullableLong},
				{Name: "EXTRA", Ordinal: 1, FieldType: values.NullableLong},
			}),
		},
		{
			name: "leaf type",
			typ: values.NewRecordType("", false, []values.Field{
				{Name: "A.B", Ordinal: 0, FieldType: values.NullableString},
			}),
		},
		{
			name: "leaf nullability",
			typ: values.NewRecordType("", false, []values.Field{
				{Name: "A.B", Ordinal: 0, FieldType: values.NotNullLong},
			}),
		},
		{
			name: "record nullability",
			typ: values.NewRecordType("", true, []values.Field{
				{Name: "A.B", Ordinal: 0, FieldType: values.NullableLong},
			}),
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			target := exactTestQOV(t, alias.Name(), test.typ)
			if rewritten, rewriteErr := translateDerivedSortKeyToPhysicalInput(logicalField, target); rewriteErr == nil || rewritten != nil {
				t.Fatalf("structural drift = (%v, %v), want nil,error", rewritten, rewriteErr)
			}
		})
	}
}

func TestTranslateProject(t *testing.T) {
	t.Parallel()
	scan := logical.NewScan("Order", "O")
	proj := logical.NewProject(scan, []string{"ORDER_ID", "PRICE"}, []string{"", "cost"})
	proj.InputOrdinals = []int{0, 2}
	ref, _ := TranslateToCascadesWithSubqueries(proj, demoMetaData(t))
	if ref == nil {
		t.Fatal("expected non-nil reference")
	}
	if _, ok := ref.Members()[0].(*expressions.LogicalProjectionExpression); !ok {
		t.Fatalf("expected LogicalProjectionExpression, got %T", ref.Members()[0])
	}
}

func TestExactLogicalProjectionOutputNamesPreserveQuotedScalarReferenceCase(t *testing.T) {
	t.Parallel()
	scalar := exactTestQOV(t, "VAL", values.NotNullInt)
	project := logical.NewProject(logical.NewScan("Order", "O"), []string{"val"}, nil)
	project.ProjectionRefs = []logical.ColumnRef{{Present: true, Bare: "val"}}

	names, err := exactLogicalProjectionOutputNames(project, []values.Value{scalar})
	if err != nil {
		t.Fatal(err)
	}
	if len(names) != 1 || names[0] != "val" {
		t.Fatalf("quoted scalar output names = %v, want [val]", names)
	}

	// splitColumnRef folds an unquoted spelling before it reaches this helper.
	// The output authority therefore preserves that already-folded form too.
	project.ProjectionRefs[0].Bare = "VAL"
	names, err = exactLogicalProjectionOutputNames(project, []values.Value{scalar})
	if err != nil {
		t.Fatal(err)
	}
	if len(names) != 1 || names[0] != "VAL" {
		t.Fatalf("unquoted scalar output names = %v, want [VAL]", names)
	}
}

func TestExactProjectionForLogicalProjectDoesNotLeakActiveCTEQualifier(t *testing.T) {
	t.Parallel()

	rowType := values.NewRecordType("", false, []values.Field{{
		Name: "ID", Ordinal: 0, FieldType: values.NotNullLong,
	}})
	row := exactTestQOV(t, "S", rowType)
	id, err := values.ResolveFieldOrdinals(row, []int{0})
	if err != nil {
		t.Fatal(err)
	}
	scanExpr, err := expressions.NewFullUnorderedScanExpression([]string{"S"}, rowType)
	if err != nil {
		t.Fatal(err)
	}
	inner := expressions.ForEachQuantifier(expressions.InitialOf(scanExpr))

	project := logical.NewProject(logical.NewScan("S", ""), []string{"S.ID"}, nil)
	project.ProjectionRefs = []logical.ColumnRef{{
		Present: true, Bare: "ID", Qualifier: "S", Qualified: true,
	}}
	translator := &cascadesTranslator{cteScope: map[string]logical.LogicalOperator{
		"S": logical.NewScan("T", ""),
	}}
	expr := translator.exactProjectionForLogicalProject([]values.Value{id}, project, inner)
	proj, ok := expr.(*expressions.LogicalProjectionExpression)
	if !ok {
		t.Fatalf("projection = %T, want LogicalProjectionExpression", expr)
	}
	if got := proj.GetOutputNames(); len(got) != 1 || got[0] != "ID" {
		t.Fatalf("SQL-boundary output names = %v, want [ID]", got)
	}
	if got := proj.GetAliases(); len(got) != 0 {
		t.Fatalf("projection aliases = %v, want none", got)
	}
	// cteScope controls resolution of the child source, not the result label.
	// Re-introducing a source-qualified output override here leaks the internal
	// CTE key into SELECT metadata and into the final positional row.
}

func TestTranslateJoin(t *testing.T) {
	t.Parallel()
	// md is REQUIRED to translate a join (leg columns derive from metadata,
	// RFC-077 7.6; nil-md is untranslatable — TestTranslateJoinNilMd).
	//
	// A maximal 2-way inner join gates ordinal: its seed result value is the
	// ordinal RC (every field a BAKED ofOrdinalNumber leg reference, dup
	// names legal), never the name-model anchored RC.
	left := logical.NewScan("Order", "")
	right := logical.NewScan("Customer", "")
	join := logical.NewJoin(left, right, logical.JoinInner, "")
	ref, _ := TranslateToCascadesWithSubqueries(join, demoMetaData(t))
	if ref == nil {
		t.Fatal("expected non-nil reference")
	}
	sel, ok := ref.Members()[0].(*expressions.SelectExpression)
	if !ok {
		t.Fatalf("expected SelectExpression for join, got %T", ref.Members()[0])
	}
	rc, ok := sel.GetResultValue().(*values.RecordConstructorValue)
	if !ok {
		t.Fatalf("expected RecordConstructorValue result, got %T", sel.GetResultValue())
	}
	// The name-model anchored RC no longer exists for a gated join; the seed
	// assert below is the sole shape authority.
	values.AssertOrdinalJoinSeed(rc) // panics on a malformed seed
	for i, f := range rc.Fields {
		fv, isFV := values.AsFieldValue(f.Value)
		if !isFV || fv.Path() == nil || fv.Path().Len() == 0 {
			t.Fatalf("ordinal seed field %d (%q) is not a baked leg reference: %T", i, f.Name, f.Value)
		}
	}

	// A 3-way inner cluster translates FLAT — one select, three quantifiers,
	// the N-leg ordinal seed (Java flattens inner joins at translation;
	// nested binaries are never seeded).
	three := logical.NewJoin(join, logical.NewScan("TypedRecord", ""), logical.JoinInner, "")
	ref3, _ := TranslateToCascadesWithSubqueries(three, demoMetaData(t))
	if ref3 == nil {
		t.Fatal("expected non-nil reference for the 3-way")
	}
	sel3, ok := ref3.Members()[0].(*expressions.SelectExpression)
	if !ok {
		t.Fatalf("expected SelectExpression for the 3-way, got %T", ref3.Members()[0])
	}
	if got := len(sel3.GetQuantifiers()); got != 3 {
		t.Fatalf("the 3-way cluster must translate FLAT with 3 quantifiers, got %d", got)
	}
	rc3, ok := sel3.GetResultValue().(*values.RecordConstructorValue)
	if !ok {
		t.Fatalf("a 3-way inner cluster must seed the ORDINAL flat RC, got %T", sel3.GetResultValue())
	}
	values.AssertOrdinalJoinSeed(rc3) // three consecutive full-leg runs
}

// TestTranslateJoinNilMd pins the RFC-077 7.6 contract: without metadata a join's
// leg columns are not derivable, so the join is untranslatable (the opaque-seed
// fallback was retired). Production always passes md; only the catalog-free
// TranslateToCascades wrapper (tests) can hit this.
func TestTranslateJoinNilMd(t *testing.T) {
	t.Parallel()
	join := logical.NewJoin(logical.NewScan("Order", ""), logical.NewScan("Customer", ""), logical.JoinInner, "")
	if ref := TranslateToCascades(join); ref != nil {
		t.Fatalf("expected nil reference for a nil-md join (no derivable leg columns), got %T", ref.Members()[0])
	}
}

// TestTranslateJoinWithExists_NilMdUntranslatable is the nil-guard regression:
// translateJoinWithExists must guard a nil result value the SAME as translateJoin.
// Without md the join's leg columns don't derive, so buildJoinResultValue returns
// nil (the opaque-seed fallback was retired, RFC-077 7.6). A nil result value must
// NOT flow into the SelectExpression — downstream GetCorrelatedToOfValue(nil) would
// nil-deref. The join+EXISTS shape must be untranslatable (nil), not a select with
// a nil result. (Without the guard this returns a non-nil SelectExpression and the
// assertion fails — the latent crash this test pins shut.)
func TestTranslateJoinWithExists_NilMdUntranslatable(t *testing.T) {
	t.Parallel()
	tr := &cascadesTranslator{} // nil md — leg columns never derive
	join := logical.NewJoin(logical.NewScan("Order", "O"), logical.NewScan("Customer", "C"), logical.JoinInner, "")
	filter := &logical.LogicalFilter{
		Input: join,
		ExistsSubqueries: []logical.ExistsSubquery{
			{Alias: values.NamedCorrelationIdentifier("E"), Plan: logical.NewScan("TypedRecord", "TR")},
		},
	}
	if got := tr.translateJoinWithExists(join, filter); got != nil {
		t.Fatalf("nil-md join+EXISTS must be untranslatable (nil), got %T", got)
	}
}

// The join+EXISTS flatten is INNER-only BY CONTRACT — translateFilter routes
// every OUTER kind to the generic arm (W4-left pin g: merging a
// preserved-side WHERE conjunct into the flat select turns it into ON
// semantics, null-padding rows that must drop). A non-INNER join reaching the
// flatten is a caller bug and must DECLINE, never mistranslate to INNER.
func TestTranslateJoinWithExists_NonInnerDeclines(t *testing.T) {
	t.Parallel()
	tr := &cascadesTranslator{}
	for _, kind := range []logical.JoinKind{logical.JoinLeft, logical.JoinRight, logical.JoinFull} {
		join := logical.NewJoin(logical.NewScan("Order", "O"), logical.NewScan("Customer", "C"), kind, "")
		filter := &logical.LogicalFilter{
			Input: join,
			ExistsSubqueries: []logical.ExistsSubquery{
				{Alias: values.NamedCorrelationIdentifier("E"), Plan: logical.NewScan("TypedRecord", "TR")},
			},
		}
		if got := tr.translateJoinWithExists(join, filter); got != nil {
			t.Fatalf("kind %v must decline the INNER-only flatten, got %T", kind, got)
		}
	}
}

func TestTranslateNil(t *testing.T) {
	t.Parallel()
	ref := TranslateToCascades(nil)
	if ref != nil {
		t.Fatal("expected nil for nil input")
	}
}

func TestTranslateAggregate(t *testing.T) {
	t.Parallel()
	scan := logical.NewScan("Order", "O")
	row := demoTableQOV(t, "Order", "O")
	agg := logical.NewAggregate(scan, []logical.GroupKey{{Display: "ORDER_ID", Bare: "ORDER_ID", Value: exactTestField(t, row, 0)}}, []logical.AggregateCall{
		{Func: "SUM", Operand: "PRICE", BareColumn: true, Bare: "PRICE"},
		{Func: "COUNT", Operand: "*", Star: true},
	}, []string{"total", "cnt"}, false)
	agg.AggregateOperands = []values.Value{exactTestField(t, row, 2), nil}
	ref, _ := TranslateToCascadesWithSubqueries(agg, demoMetaData(t))
	if ref == nil {
		t.Fatal("expected non-nil reference for aggregate")
	}
	gb, ok := ref.Members()[0].(*expressions.GroupByExpression)
	if !ok {
		t.Fatalf("expected GroupByExpression, got %T", ref.Members()[0])
	}
	if len(gb.GetGroupingKeys()) != 1 {
		t.Fatalf("expected 1 grouping key, got %d", len(gb.GetGroupingKeys()))
	}
	if len(gb.GetAggregates()) != 2 {
		t.Fatalf("expected 2 aggregates, got %d", len(gb.GetAggregates()))
	}
	if gb.GetAggregates()[0].Function != expressions.AggSum {
		t.Fatalf("expected AggSum, got %d", gb.GetAggregates()[0].Function)
	}
	if gb.GetAggregates()[1].Function != expressions.AggCount {
		t.Fatalf("expected AggCount, got %d", gb.GetAggregates()[1].Function)
	}
}

func TestTranslateAggregateNoGroup(t *testing.T) {
	t.Parallel()
	scan := logical.NewScan("Order", "")
	agg := logical.NewAggregate(scan, nil, []logical.AggregateCall{{Func: "COUNT", Operand: "*", Star: true}}, []string{"cnt"}, false)
	ref, _ := TranslateToCascadesWithSubqueries(agg, demoMetaData(t))
	if ref == nil {
		t.Fatal("expected non-nil reference for scalar aggregate")
	}
	gb, ok := ref.Members()[0].(*expressions.GroupByExpression)
	if !ok {
		t.Fatalf("expected GroupByExpression, got %T", ref.Members()[0])
	}
	if len(gb.GetGroupingKeys()) != 0 {
		t.Fatalf("expected 0 grouping keys, got %d", len(gb.GetGroupingKeys()))
	}
}

func TestAggregateFunctionByName(t *testing.T) {
	t.Parallel()
	tests := []struct {
		input string
		fn    expressions.AggregateFunction
		ok    bool
	}{
		{"COUNT", expressions.AggCount, true},
		{"SUM", expressions.AggSum, true},
		{"AVG", expressions.AggAvg, true},
		{"MIN", expressions.AggMin, true},
		{"MAX", expressions.AggMax, true},
		{"UNKNOWN", 0, false},
	}
	for _, tc := range tests {
		fn, ok := aggregateFunctionByName(tc.input)
		if ok != tc.ok || (ok && fn != tc.fn) {
			t.Errorf("aggregateFunctionByName(%q) = (%d, %v), want (%d, %v)", tc.input, fn, ok, tc.fn, tc.ok)
		}
	}
}

// TestTranslateAggregate_StructuredCallsOnly pins RFC-180 F-1: the translator
// consumes LogicalAggregate.Calls (parse-tree-derived) and never re-parses
// aggregate SQL text. Missing call info and unresolved COMPUTED operands are
// TYPED declines; a parse-tree-classified bare column keeps its lazy read.
func TestTranslateAggregate_StructuredCallsOnly(t *testing.T) {
	t.Parallel()

	build := func(calls []logical.AggregateCall) *logical.LogicalAggregate {
		scan := logical.NewScan("Order", "O")
		row := demoTableQOV(t, "Order", "O")
		agg := logical.NewAggregate(scan,
			[]logical.GroupKey{{Display: "ORDER_ID", Bare: "ORDER_ID", Value: exactTestField(t, row, 0)}},
			calls, make([]string, len(calls)), false)
		agg.AggregateOperands = make([]values.Value, len(calls))
		for i, call := range calls {
			if call.BareColumn && strings.EqualFold(call.Operand, "PRICE") {
				agg.AggregateOperands[i] = exactTestField(t, row, 2)
			}
		}
		return agg
	}

	// A parse-tree-classified bare column carries its exact resolved Value.
	ref, _, err := TranslateToCascadesWithError(build(
		[]logical.AggregateCall{{Func: "SUM", Operand: "PRICE", BareColumn: true, Bare: "PRICE"}}), demoMetaData(t))
	if err != nil || ref == nil {
		t.Fatalf("bare-column aggregate must translate: ref=%v err=%v", ref, err)
	}

	// A text-only aggregate with no structured call is UNREPRESENTABLE since
	// RFC-180 F-3 deleted the display-text slice — the missing-Calls decline
	// this test used to pin is now enforced by the type system.

	// Computed operand with no resolved Value: typed decline.
	ref, _, err = TranslateToCascadesWithError(build(
		[]logical.AggregateCall{{Func: "SUM", Operand: "(AMOUNT+10)*2"}}), demoMetaData(t))
	if ref != nil || err == nil {
		t.Fatalf("unresolved computed operand must decline typed: ref=%v err=%v", ref, err)
	}
}

func TestTranslateDistinct(t *testing.T) {
	t.Parallel()
	scan := logical.NewScan("Order", "")
	dist := logical.NewDistinct(scan)
	ref, _ := TranslateToCascadesWithSubqueries(dist, demoMetaData(t))
	if ref == nil {
		t.Fatal("expected non-nil reference for DISTINCT")
	}
	if _, ok := ref.Members()[0].(*expressions.LogicalDistinctExpression); !ok {
		t.Fatalf("expected LogicalDistinctExpression, got %T", ref.Members()[0])
	}
}

func TestTranslateNestedSortFilterScan(t *testing.T) {
	t.Parallel()
	scan := logical.NewScan("orders", "")
	filter := logical.NewFilter(scan, "active = true")
	sort := logical.NewSort(filter, []logical.SortKey{{Expr: "id", Dir: logical.SortAsc}})
	limit := logical.NewLimit(sort, 20, 0)
	ref := TranslateToCascades(limit)
	if ref != nil {
		t.Fatal("expected nil: text-only predicate in nested tree must not translate")
	}
}

func TestTranslateCTEInlines(t *testing.T) {
	t.Parallel()
	body := logical.NewScan("Order", "")
	main := logical.NewScan("expensive", "")
	cte := logical.NewCTE("expensive", body, main, false)

	ref, _ := TranslateToCascadesWithSubqueries(cte, demoMetaData(t))
	if ref == nil {
		t.Fatal("expected non-nil reference for non-recursive CTE")
	}
	scan, ok := ref.Members()[0].(*expressions.FullUnorderedScanExpression)
	if !ok {
		t.Fatalf("expected inlined FullUnorderedScanExpression, got %T", ref.Members()[0])
	}
	if scan.GetRecordTypes()[0] != "Order" {
		t.Fatalf("expected scan of Order, got %s", scan.GetRecordTypes()[0])
	}
}

func TestTranslateCTEWithFilter(t *testing.T) {
	t.Parallel()
	body := logical.NewFilter(logical.NewScan("Product", ""), "price > 100")
	main := logical.NewProject(
		logical.NewScan("expensive", ""),
		[]string{"name"}, []string{""},
	)
	cte := logical.NewCTE("expensive", body, main, false)

	ref := TranslateToCascades(cte)
	if ref != nil {
		t.Fatal("expected nil: CTE body with text-only predicate must not translate")
	}
}

func TestTranslateCTEChained(t *testing.T) {
	t.Parallel()
	bodyA := logical.NewScan("Order", "")
	mainA := logical.NewScan("B", "")
	bodyB := logical.NewScan("A", "")
	cteA := logical.NewCTE("A", bodyA, mainA, false)
	cteB := logical.NewCTE("B", bodyB, cteA, false)

	ref, _ := TranslateToCascadesWithSubqueries(cteB, demoMetaData(t))
	if ref == nil {
		t.Fatal("expected non-nil reference for chained CTEs")
	}
	scan, ok := ref.Members()[0].(*expressions.FullUnorderedScanExpression)
	if !ok {
		t.Fatalf("expected FullUnorderedScanExpression, got %T", ref.Members()[0])
	}
	if scan.GetRecordTypes()[0] != "Order" {
		t.Fatalf("expected scan of Order (A inlined into B's body), got %s", scan.GetRecordTypes()[0])
	}
}

func TestTranslateCTEOuterTextFilterBailsToNaive(t *testing.T) {
	t.Parallel()
	// Main query has a text-only filter on the CTE reference.
	// This must bail (return nil) so the planner falls back to naive
	// rather than silently dropping the filter.
	body := logical.NewScan("Product", "")
	main := logical.NewFilter(logical.NewScan("expensive", ""), "id > 5")
	cte := logical.NewCTE("expensive", body, main, false)

	ref := TranslateToCascades(cte)
	if ref != nil {
		t.Fatal("expected nil — text-only filter on CTE reference should bail to naive")
	}
}

func TestTranslateCTEShadowsTableName(t *testing.T) {
	t.Parallel()
	// CTE name = table name in body — must not infinite-recurse.
	body := logical.NewProject(logical.NewScan("Order", ""), []string{"ORDER_ID"}, []string{""})
	body.InputOrdinals = []int{0}
	main := logical.NewProject(logical.NewScan("Order", ""), []string{"ORDER_ID"}, []string{""})
	main.InputOrdinals = []int{0}
	cte := logical.NewCTE("Order", body, main, false)

	ref, _ := TranslateToCascadesWithSubqueries(cte, demoMetaData(t))
	if ref == nil {
		t.Fatal("expected non-nil reference when CTE name shadows table name")
	}
	proj, ok := ref.Members()[0].(*expressions.LogicalProjectionExpression)
	if !ok {
		t.Fatalf("expected LogicalProjectionExpression, got %T", ref.Members()[0])
	}
	innerRef := proj.GetQuantifiers()[0].GetRangesOver()
	innerProj, ok := innerRef.Members()[0].(*expressions.LogicalProjectionExpression)
	if !ok {
		t.Fatalf("expected inlined projection from CTE body, got %T", innerRef.Members()[0])
	}
	innerScan := innerProj.GetQuantifiers()[0].GetRangesOver().Members()[0]
	if _, ok := innerScan.(*expressions.FullUnorderedScanExpression); !ok {
		t.Fatalf("expected FullUnorderedScanExpression at leaf, got %T", innerScan)
	}
}

func TestTranslateCTEMultipleReferences(t *testing.T) {
	t.Parallel()
	// CTE referenced twice in the main query (via join), under DISTINCT aliases
	// — the SQL-reachable double-reference class (`FROM p AS a, p AS b`; a bare
	// `FROM p, p` is a 42712 duplicate alias upstream). md is REQUIRED so the
	// join's CTE-reference legs derive their columns from the CTE body's real
	// table (RFC-077 7.6).
	body := logical.NewScan("Order", "")
	left := logical.NewScan("p", "a")
	right := logical.NewScan("p", "b")
	join := logical.NewJoin(left, right, logical.JoinInner, "")
	cte := logical.NewCTE("p", body, join, false)

	ref, _ := TranslateToCascadesWithSubqueries(cte, demoMetaData(t))
	if ref == nil {
		t.Fatal("expected non-nil reference for CTE with double reference")
	}
	sel, ok := ref.Members()[0].(*expressions.SelectExpression)
	if !ok {
		t.Fatalf("expected SelectExpression for join, got %T", ref.Members()[0])
	}
	quants := sel.GetQuantifiers()
	if len(quants) != 2 {
		t.Fatalf("expected 2 quantifiers, got %d", len(quants))
	}

	// The DUP-ALIAS double reference (`FROM p, p` — only constructible by
	// direct tree building; SQL rejects it upstream) declines LOUDLY: the gate
	// poisons indistinguishable leg correlations, and there is no name-model
	// fallback to fall back to.
	dupJoin := logical.NewJoin(logical.NewScan("p", ""), logical.NewScan("p", ""), logical.JoinInner, "")
	if dupRef, _ := TranslateToCascadesWithSubqueries(logical.NewCTE("p", body, dupJoin, false), demoMetaData(t)); dupRef != nil {
		t.Fatal("dup-aliased CTE double reference must decline loudly (gate dup poison, no name-model fallback)")
	}
}

func TestTranslateAggregateWithHavingReturnsNil(t *testing.T) {
	t.Parallel()
	scan := logical.NewScan("orders", "")
	agg := logical.NewAggregate(scan, []logical.GroupKey{{Display: "REGION", Bare: "REGION"}}, []logical.AggregateCall{{Func: "SUM", Operand: "PRICE", BareColumn: true}}, []string{"total"}, true)
	ref := TranslateToCascades(agg)
	if ref != nil {
		t.Fatal("expected nil — aggregate with HAVING should bail to naive")
	}
}

func TestTranslateAggregateOutputContractFailsClosedWhenMalformed(t *testing.T) {
	t.Parallel()
	scan := logical.NewScan("Order", "O")
	row := demoTableQOV(t, "Order", "O")
	build := func(
		slots []logical.AggregateOutputSlot,
		projectOrdinals []int,
		computed []bool,
		projected []values.Value,
	) logical.LogicalOperator {
		agg := logical.NewAggregate(scan,
			[]logical.GroupKey{{Display: "ORDER_ID", Bare: "ORDER_ID", Value: exactTestField(t, row, 0)}},
			[]logical.AggregateCall{{Func: "SUM", Operand: "PRICE", BareColumn: true, Bare: "PRICE"}},
			[]string{""}, false)
		agg.AggregateOperands = []values.Value{exactTestField(t, row, 2)}
		agg.OutputSlots = slots
		if projectOrdinals == nil {
			return agg
		}
		proj := logical.NewProject(agg, []string{"SUM(PRICE)", "REGION"}, nil)
		proj.AggregateOutputOrdinals = projectOrdinals
		proj.IsComputed = computed
		proj.ProjectedValues = projected
		return proj
	}
	tests := []struct {
		name string
		op   logical.LogicalOperator
	}{
		{
			name: "zero select ordinal",
			op: build([]logical.AggregateOutputSlot{
				{SelectOrdinal: 0, NativeOrdinal: 1},
			}, nil, nil, nil),
		},
		{
			name: "out of range native ordinal",
			op: build([]logical.AggregateOutputSlot{
				{SelectOrdinal: 1, NativeOrdinal: 2},
			}, nil, nil, nil),
		},
		{
			name: "project disagrees with producer slots",
			op: build([]logical.AggregateOutputSlot{
				{SelectOrdinal: 1, NativeOrdinal: 1},
				{SelectOrdinal: 2, NativeOrdinal: 0},
			}, []int{0, 1}, []bool{false, false}, nil),
		},
		{
			name: "computed marker disagrees with native ordinal",
			op: build([]logical.AggregateOutputSlot{
				{SelectOrdinal: 1, NativeOrdinal: 1},
				{SelectOrdinal: 2, NativeOrdinal: 0},
			}, []int{1, 0}, []bool{true, false}, nil),
		},
		{
			name: "computed value contains lazy source field",
			op: build([]logical.AggregateOutputSlot{
				{SelectOrdinal: 1, NativeOrdinal: -1},
				{SelectOrdinal: 2, NativeOrdinal: 0},
			}, []int{-1, 0}, []bool{true, false}, []values.Value{
				exactTestNamedField(t, "SOURCE", "PRICE", values.NullableLong),
				nil,
			}),
		},
		{
			name: "correlated scalar cannot bypass physical contract proof",
			op: func() logical.LogicalOperator {
				op := build([]logical.AggregateOutputSlot{
					{SelectOrdinal: 1, NativeOrdinal: 1},
					{SelectOrdinal: 2, NativeOrdinal: 0},
				}, []int{1, 0}, []bool{false, false}, nil)
				op.(*logical.LogicalProject).CorrelatedScalarSubqueries = []logical.CorrelatedScalarSubquery{{}}
				return op
			}(),
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ref, _, err := TranslateToCascadesWithError(tc.op, demoMetaData(t))
			if ref != nil {
				t.Fatalf("malformed aggregate output layout translated: %T", ref.Get())
			}
			var apiErr *api.Error
			if !errors.As(err, &apiErr) || apiErr.Code != api.ErrCodeUnsupportedQuery {
				t.Fatalf("want typed 0AF00 fail-closed error, got %v", err)
			}
		})
	}
}

func TestBindPostAggregateValueRejectsForeignExactField(t *testing.T) {
	t.Parallel()

	sourceType := &values.RecordType{Fields: []values.Field{
		{Name: "ID", Ordinal: 0, FieldType: values.NotNullLong},
		{Name: "PRICE", Ordinal: 1, FieldType: values.NullableLong},
	}}
	source := exactTestQOV(t, "SOURCE", sourceType)
	key := exactTestField(t, source, 0)
	foreign := exactTestField(t, source, 1)
	agg := &logical.LogicalAggregate{GroupKeys: []logical.GroupKey{{Display: "ID", Value: key}}}
	output := exactTestQOV(t, "AGG_OUT", &values.RecordType{Fields: []values.Field{
		{Name: "ID", Ordinal: 0, FieldType: values.NotNullLong},
	}})

	bound, err := bindPostAggregateValue(key, agg, output)
	if err != nil {
		t.Fatalf("bind exact group key: %v", err)
	}
	field := exactTestFieldView(t, bound)
	owner, ok := values.AsQuantifiedObjectValue(field.ChildValue())
	if !ok || owner.Correlation() != values.NamedCorrelationIdentifier("AGG_OUT") ||
		len(field.Path().Ordinals()) != 1 || field.Path().Ordinals()[0] != 0 {
		t.Fatalf("bound group key does not address AGG_OUT[0]: %T %v", bound, field.Path().Ordinals())
	}

	if _, err := bindPostAggregateValue(foreign, agg, output); err == nil {
		t.Fatal("foreign exact field bypassed the aggregate output contract")
	}
	wrongOutput := exactTestQOV(t, "AGG_OUT_BAD", &values.RecordType{Fields: []values.Field{
		{Name: "ID", Ordinal: 0, FieldType: values.NullableString},
	}})
	if _, err := bindPostAggregateValue(key, agg, wrongOutput); err == nil {
		t.Fatal("aggregate binding accepted a native slot whose exact type changed")
	}
}

func BenchmarkTranslateCTEInline(b *testing.B) {
	body := logical.NewFilter(
		logical.NewScan("Product", ""),
		"price > 100",
	)
	main := logical.NewProject(
		logical.NewScan("expensive", ""),
		[]string{"name"}, []string{""},
	)
	cte := logical.NewCTE("expensive", body, main, false)

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		ref := TranslateToCascades(cte)
		if ref == nil {
			b.Fatal("unexpected nil")
		}
	}
}

func BenchmarkTranslateSimpleScan(b *testing.B) {
	scan := logical.NewScan("Product", "")
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		ref := TranslateToCascades(scan)
		if ref == nil {
			b.Fatal("unexpected nil")
		}
	}
}

func TestTranslateRecursiveCTEReturnsNil(t *testing.T) {
	t.Parallel()
	body := logical.NewScan("Product", "")
	main := logical.NewScan("recursive_cte", "")
	cte := logical.NewCTE("recursive_cte", body, main, true)

	ref := TranslateToCascades(cte)
	if ref != nil {
		t.Fatal("expected nil for recursive CTE (not yet supported)")
	}
}

func TestFindUnsupportedFunction(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		op   logical.LogicalOperator
		want string
	}{
		{"nil op", nil, ""},
		{"plain scan", logical.NewScan("T", ""), ""},
		{"projection with SIN in Value tree", func() logical.LogicalOperator {
			p := logical.NewProject(logical.NewScan("T", ""), []string{"x"}, nil)
			p.ProjectedValues = []values.Value{
				values.NewScalarFunctionValue("SIN", values.NotNullLong,
					exactTestNamedField(t, "T", "x", values.NotNullLong)),
			}
			return p
		}(), "SIN"},
		{"projection with TAN in Value tree", func() logical.LogicalOperator {
			p := logical.NewProject(logical.NewScan("T", ""), []string{"x"}, nil)
			p.ProjectedValues = []values.Value{
				values.NewScalarFunctionValue("TAN", values.NotNullLong,
					exactTestNamedField(t, "T", "x", values.NotNullLong)),
			}
			return p
		}(), "TAN"},
		{"projection with COUNT (allowed)", func() logical.LogicalOperator {
			p := logical.NewProject(logical.NewScan("T", ""), []string{"COUNT(*)"}, nil)
			return p
		}(), ""},
		{"projection with COALESCE (allowed)", func() logical.LogicalOperator {
			p := logical.NewProject(logical.NewScan("T", ""), []string{"COALESCE(a,b)"}, nil)
			return p
		}(), ""},
		{"long expression (not detected)", func() logical.LogicalOperator {
			p := logical.NewProject(logical.NewScan("T", ""), []string{"CASEWHENEXISTS(SELECT1)"}, nil)
			return p
		}(), ""},
		{"plain column", func() logical.LogicalOperator {
			p := logical.NewProject(logical.NewScan("T", ""), []string{"name"}, nil)
			return p
		}(), ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := FindUnsupportedFunction(tc.op)
			if got != tc.want {
				t.Fatalf("FindUnsupportedFunction: got %q, want %q", got, tc.want)
			}
		})
	}
}

func TestFindUnsupportedFunction_ValueTree(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		op   logical.LogicalOperator
		want string
	}{
		{"nil", nil, ""},
		{"scan", logical.NewScan("T", ""), ""},
		{"safe func in value", func() logical.LogicalOperator {
			p := logical.NewProject(logical.NewScan("T", ""), []string{"x"}, nil)
			p.ProjectedValues = []values.Value{
				values.NewScalarFunctionValue("COALESCE", values.NotNullLong,
					exactTestNamedField(t, "T", "a", values.NotNullLong)),
			}
			return p
		}(), ""},
		{"unsafe func in value", func() logical.LogicalOperator {
			p := logical.NewProject(logical.NewScan("T", ""), []string{"x"}, nil)
			p.ProjectedValues = []values.Value{
				values.NewScalarFunctionValue("SIN", values.NotNullLong,
					exactTestNamedField(t, "T", "a", values.NotNullLong)),
			}
			return p
		}(), "SIN"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := FindUnsupportedFunction(tc.op)
			if got != tc.want {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
		})
	}
}

func FuzzTranslateToCascades(f *testing.F) {
	tables := []string{"Orders", "Items", "Customer", "Sales"}
	cols := []string{"id", "name", "price", "amount", "status"}

	f.Add(byte(0), byte(0), byte(0), byte(0), byte(0), byte(0))
	f.Add(byte(1), byte(2), byte(3), byte(1), byte(1), byte(0))
	f.Add(byte(3), byte(4), byte(1), byte(2), byte(2), byte(1))

	f.Fuzz(func(t *testing.T, opKind, tableIdx, colIdx, childKind, childCol, flags byte) {
		tbl := tables[int(tableIdx)%len(tables)]
		col := cols[int(colIdx)%len(cols)]
		childTbl := tables[int(childKind)%len(tables)]
		childField := cols[int(childCol)%len(cols)]

		var op logical.LogicalOperator
		scan := logical.NewScan(tbl, "")

		switch opKind % 8 {
		case 0:
			op = scan
		case 1:
			op = logical.NewFilter(scan, col+" > 10")
		case 2:
			op = logical.NewProject(scan, []string{col, childField}, nil)
		case 3:
			right := logical.NewScan(childTbl, "a")
			op = logical.NewJoin(scan, right, logical.JoinInner, "")
		case 4:
			op = logical.NewSort(scan, []logical.SortKey{{Expr: col, Dir: logical.SortAsc}})
		case 5:
			op = logical.NewDistinct(scan)
		case 6:
			body := logical.NewScan(tbl, "")
			main := logical.NewFilter(logical.NewScan(tbl, ""), col+" > 0")
			op = logical.NewCTE("cte1", body, main, false)
		case 7:
			left := logical.NewProject(scan, []string{col}, nil)
			right := logical.NewProject(logical.NewScan(childTbl, ""), []string{childField}, nil)
			op = logical.NewUnion([]logical.LogicalOperator{left, right}, true)
		}

		if flags&1 != 0 {
			op = logical.NewFilter(op, col+" = 'test'")
		}
		if flags&2 != 0 {
			op = logical.NewProject(op, []string{col}, nil)
		}

		TranslateToCascades(op)
	})
}

func TestSourceAlias(t *testing.T) {
	t.Parallel()
	assertCTESourceQOV := func(t *testing.T, cte *logical.LogicalCTE, want values.CorrelationIdentifier) {
		t.Helper()
		ref, _, err := TranslateToCascadesWithError(cte, demoMetaData(t))
		if err != nil || ref == nil {
			t.Fatalf("translate CTE: ref=%v err=%v", ref, err)
		}
		q := expressions.NamedForEachQuantifier(
			values.NamedCorrelationIdentifier(sourceAlias(cte)), ref)
		qov, err := q.RequireFlowedObjectValue()
		if err != nil {
			t.Fatalf("exact CTE source QOV: %v", err)
		}
		if qov.Correlation() != want {
			t.Fatalf("CTE source correlation = %#v, want exact %#v", qov.Correlation(), want)
		}
		wantType := demoTableQOV(t, "Order", "EXPECTED").FlowedType()
		if !qov.FlowedType().Equals(wantType) {
			t.Fatalf("CTE source flowed type = %s, want exact %s", qov.FlowedType(), wantType)
		}
	}
	t.Run("scan_with_alias", func(t *testing.T) {
		t.Parallel()
		got := sourceAlias(logical.NewScan("orders", "o"))
		if got != "O" {
			t.Errorf("want O, got %s", got)
		}
	})
	t.Run("scan_no_alias", func(t *testing.T) {
		t.Parallel()
		got := sourceAlias(logical.NewScan("orders", ""))
		if got != "ORDERS" {
			t.Errorf("want ORDERS, got %s", got)
		}
	})
	t.Run("cte_returns_cte_name", func(t *testing.T) {
		t.Parallel()
		inner := logical.NewScan("my_cte", "")
		body := logical.NewScan("Order", "")
		cte := logical.NewCTE("my_cte", body, inner, false)
		got := sourceAlias(cte)
		if got != "MY_CTE" {
			t.Errorf("want MY_CTE, got %s", got)
		}
		assertCTESourceQOV(t, cte, values.NamedCorrelationIdentifier("MY_CTE"))
	})
	t.Run("scope_only_cte_preserves_main_alias", func(t *testing.T) {
		t.Parallel()
		body := logical.NewScan("Order", "")
		main := logical.NewScan("my_cte", "visible_alias")
		cte := logical.NewCTE("my_cte", body, main, false)
		cte.PreserveMainSource = true
		if got := sourceAlias(cte); got != "VISIBLE_ALIAS" {
			t.Errorf("want VISIBLE_ALIAS, got %s", got)
		}
		assertCTESourceQOV(t, cte, values.NamedCorrelationIdentifier("VISIBLE_ALIAS"))
	})
	t.Run("filter_wrapping_scan", func(t *testing.T) {
		t.Parallel()
		got := sourceAlias(logical.NewFilter(logical.NewScan("t", "a"), "x=1"))
		if got != "A" {
			t.Errorf("want A, got %s", got)
		}
	})
	t.Run("nil_returns_empty", func(t *testing.T) {
		t.Parallel()
		got := sourceAlias(nil)
		if got != "" {
			t.Errorf("want empty, got %s", got)
		}
	})
}

// TestLegColumns_CTEScopeResolvesBody pins the RFC-077 7.6 CTE/derived-table
// anchoring: a cteScope-shadowed scan derives its columns from the CTE BODY (not
// the real table's metadata, and not nil). The CTE is removed from scope while
// resolving the body, so a same-named scan inside the body resolves to the REAL
// table — and legColumns does NOT recurse forever (the CTE-shadow stack-overflow
// regression). A pre-translated recursive-CTE reference (cteExprScope) still falls
// back to nil (its body output columns are not readable from the logical tree).
func TestLegColumns_CTEScopeResolvesBody(t *testing.T) {
	t.Parallel()
	md := demoMetaData(t) // has a real "Order" table

	// Without a shadow, "Order" anchors from metadata (non-nil).
	plain := &cascadesTranslator{md: md, cteScope: map[string]logical.LogicalOperator{}}
	realCols := plain.legColumns(logical.NewScan("Order", ""))
	if realCols == nil {
		t.Fatal("setup: a real table must derive columns from metadata")
	}

	// A CTE named "order" whose body is a projection over the real table resolves
	// to the BODY's output columns (here renamed), NOT the real table's columns.
	body := logical.NewProject(logical.NewScan("Order", ""), []string{"ORDER_ID"}, []string{"OID"})
	shadowed := &cascadesTranslator{
		md:       md,
		cteScope: map[string]logical.LogicalOperator{"ORDER": body},
	}
	cols := shadowed.legColumns(logical.NewScan("Order", ""))
	if len(cols) != 1 || cols[0].Name != "OID" {
		t.Errorf("CTE-shadowed leg must derive the BODY's output columns [OID]; got %v", cols)
	}

	// A CTE whose body is a bare same-named scan: the body's scan resolves to the
	// REAL table (CTE removed from scope while resolving) — and no infinite
	// recursion. The leg derives the real table's columns via the body.
	selfBody := logical.NewScan("Order", "")
	selfShadowed := &cascadesTranslator{
		md:       md,
		cteScope: map[string]logical.LogicalOperator{"ORDER": selfBody},
	}
	if got := selfShadowed.legColumns(logical.NewScan("Order", "")); len(got) != len(realCols) {
		t.Errorf("self-referential CTE body must resolve to the real table's columns (no recursion); got %v want %d cols", got, len(realCols))
	}

	// cteExprScope (a pre-translated recursive-CTE reference) still falls back to nil.
	exprShadowed := &cascadesTranslator{
		md:           md,
		cteExprScope: map[string]expressions.RelationalExpression{"ORDER": nil},
	}
	if cols := exprShadowed.legColumns(logical.NewScan("Order", "")); cols != nil {
		t.Errorf("cteExprScope-shadowed name must NOT anchor (recursive-CTE body unreadable); got %v", cols)
	}
}

// TestPullUpToOutputField_PointerIdentityPreferred pins that pullUpToOutputField
// prefers an EXACT POINTER-identity output field over an earlier
// semantically-equal one. When two SELECT-list aliases share a
// semantically-equal value (`id AS a, id AS b ORDER BY b`),
// upgradeSortKeyValues copies the EXACT projected Value pointer into the sort
// key, so the key actually names the pointer-identical field (`b`). A single
// semantic-equality pass would return the first equal field (`a`) and pull up to
// the WRONG output column name. The two-pass design
// keeps the pulled-up name faithful to the aliased column.
func TestPullUpToOutputField_PointerIdentityPreferred(t *testing.T) {
	t.Parallel()

	// Two distinct FieldValue pointers that are SEMANTICALLY EQUAL (same field).
	sourceType := &values.RecordType{Fields: []values.Field{{Name: "ID", Ordinal: 0, FieldType: values.NotNullLong}}}
	source := exactTestQOV(t, "SOURCE", sourceType)
	valA := exactTestField(t, source, 0)
	valB := exactTestField(t, source, 0)
	if !values.SemanticEqualsUnderAliasMap(valA, valB, values.EmptyAliasMap()) {
		t.Fatalf("test setup: valA and valB must be semantically equal")
	}
	if valA == valB {
		t.Fatalf("test setup: valA and valB must be distinct pointers")
	}

	fields := []values.RecordConstructorField{
		{Name: "A", Value: valA},
		{Name: "B", Value: valB},
	}
	outputType := &values.RecordType{Fields: []values.Field{
		{Name: "A", Ordinal: 0, FieldType: values.NotNullLong},
		{Name: "B", Ordinal: 1, FieldType: values.NotNullLong},
	}}
	output := exactTestQOV(t, "OUTPUT", outputType)

	// Sort key Value is the EXACT pointer of output field B. The pull-up must
	// resolve to B (pointer identity), NOT A (earlier semantic match).
	got, ok := pullUpToOutputField(valB, fields, output)
	if !ok {
		t.Fatalf("pullUpToOutputField returned no match for a pointer-identical key")
	}
	fv := exactTestFieldView(t, got)
	if fv.DisplayName() != "B" || len(fv.Path().Ordinals()) != 1 || fv.Path().Ordinals()[0] != 1 {
		t.Errorf("pull-up resolved to output field %q at %v, want B at [1]", fv.DisplayName(), fv.Path().Ordinals())
	}

	// Symmetric: keying on valA must resolve to A.
	if got, ok := pullUpToOutputField(valA, fields, output); ok {
		fv := exactTestFieldView(t, got)
		if fv.DisplayName() != "A" || fv.Path().Ordinals()[0] != 0 {
			t.Errorf("pull-up on valA resolved to %q at %v, want A at [0]", fv.DisplayName(), fv.Path().Ordinals())
		}
	} else {
		t.Errorf("pullUpToOutputField returned no match for valA")
	}

	// A key that is only SEMANTICALLY equal (a third distinct pointer) falls to
	// pass 2 and resolves to the FIRST semantically-equal field (A) — the
	// documented fallback for rebuilt (non-pointer-copied) keys.
	valC := exactTestField(t, source, 0)
	if got, ok := pullUpToOutputField(valC, fields, output); ok {
		fv := exactTestFieldView(t, got)
		if fv.DisplayName() != "A" || fv.Path().Ordinals()[0] != 0 {
			t.Errorf("semantic-only pull-up resolved to %q at %v, want first equal field A at [0]", fv.DisplayName(), fv.Path().Ordinals())
		}
	} else {
		t.Errorf("pullUpToOutputField returned no match for a semantically-equal key")
	}

	narrowOutput := exactTestQOV(t, "NARROW_OUTPUT", &values.RecordType{Fields: outputType.Fields[:1]})
	if got, ok := pullUpToOutputField(valB, fields, narrowOutput); ok || got != nil {
		t.Fatalf("pull-up accepted output owner without ordinal 1: %T", got)
	}
}

// TestTranslateUnnest_NilMetadataIsCleanError pins that the
// metadata-less translation path (TranslateToCascades / the nil-md
// TranslateToCascadesWithSubqueries, used by scalar-subquery / DML translation
// and unit tests) must NOT panic when handed a LogicalJoin whose Right is a
// LogicalUnnest. Classifying a lateral array unnest requires the outer source's
// proto descriptor (resolveRecordType → t.md); with t.md == nil the translator
// declines CLEANLY with ErrCodeUnsupportedQuery rather than dereferencing nil
// metadata. RFC-142.
func TestTranslateUnnest_NilMetadataIsCleanError(t *testing.T) {
	// FROM T1, T1.ARR1 AS V — a lateral unnest whose array field lives on T1.
	unnest := &logical.LogicalUnnest{Segments: []string{"T1", "ARR1"}, Alias: "V"}
	join := logical.NewJoin(logical.NewScan("T1", ""), unnest, logical.JoinInner, "")

	// 1. TranslateToCascades (the bare nil-md entry) must not panic.
	if ref := TranslateToCascades(join); ref != nil {
		t.Fatalf("nil-md unnest: expected untranslatable (nil ref), got %v", ref)
	}

	// 2. TranslateToCascadesWithError surfaces the specific clean code.
	ref, _, err := TranslateToCascadesWithError(join, nil)
	if ref != nil {
		t.Fatalf("nil-md unnest: expected nil ref, got %v", ref)
	}
	if err == nil {
		t.Fatalf("nil-md unnest: expected a clean error, got nil")
	}
	var apiErr *api.Error
	if !errors.As(err, &apiErr) || apiErr.Code != api.ErrCodeUnsupportedQuery {
		t.Fatalf("nil-md unnest: err = %v (%T), want code %v", err, err, api.ErrCodeUnsupportedQuery)
	}
}

// TestTranslateUnnest_NilMetadataAtOrdinalityIsCleanError is the AT-ordinality
// variant of P2b: the AT-only form (no AS) also reaches translateUnnestJoin and
// must decline cleanly on the nil-md path, never panic. RFC-142.
func TestTranslateUnnest_NilMetadataAtOrdinalityIsCleanError(t *testing.T) {
	unnest := &logical.LogicalUnnest{Segments: []string{"T1", "ARR1"}, AtAlias: "ORD"}
	join := logical.NewJoin(logical.NewScan("T1", ""), unnest, logical.JoinInner, "")

	ref, _, err := TranslateToCascadesWithError(join, nil)
	if ref != nil {
		t.Fatalf("nil-md AT unnest: expected nil ref, got %v", ref)
	}
	var apiErr *api.Error
	if !errors.As(err, &apiErr) || apiErr.Code != api.ErrCodeUnsupportedQuery {
		t.Fatalf("nil-md AT unnest: err = %v (%T), want code %v", err, err, api.ErrCodeUnsupportedQuery)
	}
}

// TestExactLogicalResultType_LateralRightUnnest pins the only context in which
// a syntax-only LogicalUnnest has enough information to state an exact type:
// the right child of its lateral join. The owner is selected structurally from
// the exact left input, and its stored array slot supplies the element type.
// A standalone unnest and every malformed owner/path remain loud; this must
// never become an Unknown-producing text fallback.
func TestExactLogicalResultType_LateralRightUnnest(t *testing.T) {
	t.Parallel()
	md := demoMetaData(t)
	left := logical.NewScan("Order", "O")

	t.Run("scalar AS", func(t *testing.T) {
		unnest := &logical.LogicalUnnest{Segments: []string{"O", "TAGS"}, Alias: "TAG"}
		joined, err := ExactLogicalResultType(logical.NewJoin(left, unnest, logical.JoinInner, ""), md)
		if err != nil {
			t.Fatal(err)
		}
		record, ok := joined.(*values.RecordType)
		if !ok || len(record.Fields) == 0 {
			t.Fatalf("joined type = %T %v, want a non-empty exact record", joined, joined)
		}
		last := record.Fields[len(record.Fields)-1]
		if last.Name != "TAG" || last.Ordinal != len(record.Fields)-1 || !last.FieldType.Equals(values.NotNullString) {
			t.Fatalf("scalar unnest field = %+v, want TAG STRING NOT NULL at final ordinal", last)
		}
	})

	t.Run("AS plus AT", func(t *testing.T) {
		unnest := &logical.LogicalUnnest{Segments: []string{"O", "TAGS"}, Alias: "TAG", AtAlias: "POS"}
		right, err := exactLateralUnnestResultType(left, unnest, md)
		if err != nil {
			t.Fatal(err)
		}
		record, ok := right.(*values.RecordType)
		if !ok || len(record.Fields) != 2 {
			t.Fatalf("ordinal unnest type = %T %v, want exact two-field record", right, right)
		}
		if record.Fields[0].Name != "TAG" || record.Fields[0].Ordinal != 0 || !record.Fields[0].FieldType.Equals(values.NotNullString) {
			t.Fatalf("AS field = %+v, want TAG STRING NOT NULL at ordinal 0", record.Fields[0])
		}
		if record.Fields[1].Name != "POS" || record.Fields[1].Ordinal != 1 || !record.Fields[1].FieldType.Equals(values.NotNullInt) {
			t.Fatalf("AT field = %+v, want POS INT NOT NULL at ordinal 1", record.Fields[1])
		}
	})

	for _, tc := range []struct {
		name   string
		unnest *logical.LogicalUnnest
	}{
		{"foreign owner", &logical.LogicalUnnest{Segments: []string{"X", "TAGS"}, Alias: "TAG"}},
		{"non-array field", &logical.LogicalUnnest{Segments: []string{"O", "PRICE"}, Alias: "TAG"}},
		{"missing field", &logical.LogicalUnnest{Segments: []string{"O", "NO_SUCH_FIELD"}, Alias: "TAG"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if typ, err := ExactLogicalResultType(logical.NewJoin(left, tc.unnest, logical.JoinInner, ""), md); err == nil || typ != nil {
				t.Fatalf("malformed lateral unnest typed as (%v, %v), want nil,error", typ, err)
			}
		})
	}

	t.Run("standalone remains loud", func(t *testing.T) {
		unnest := &logical.LogicalUnnest{Segments: []string{"O", "TAGS"}, Alias: "TAG"}
		if typ, err := ExactLogicalResultType(unnest, md); err == nil || typ != nil {
			t.Fatalf("standalone syntax-only unnest typed as (%v, %v), want nil,error", typ, err)
		}
	})
}

// TestAggregateOutputColumns_DupNameConflictingTypes pins the typeOf
// ambiguity guard: a GROUP BY key whose bare name is carried by MULTIPLE
// input columns with CONFLICTING type codes (two legs both projecting `V`,
// one INT one STRING) must stay UnknownType — first-match would silently
// attach one leg's type to what may be the other leg's column (the N-F4
// wrong-metadata class). Same-typed duplicates keep the shared type.
func TestAggregateOutputColumns_DupNameConflictingTypes(t *testing.T) {
	t.Parallel()
	tr := &cascadesTranslator{}

	mkAgg := func(a, b values.Type) *logical.LogicalAggregate {
		return &logical.LogicalAggregate{
			Input: &logical.LogicalProject{
				Projections: []string{"V", "V"},
				Aliases:     []string{"", ""},
				ProjectedValues: []values.Value{
					&values.ConstantValue{Value: int64(1), Typ: a},
					&values.ConstantValue{Value: "s", Typ: b},
				},
			},
			GroupKeys: []logical.GroupKey{{Display: "V", Bare: "V"}},
			Calls:     []logical.AggregateCall{{Func: "COUNT", Operand: "*", Star: true}},
		}
	}

	// Conflicting codes → the key stays Unknown (correct-or-unknown).
	fields := tr.aggregateOutputColumns(mkAgg(values.NullableInt, values.TypeString))
	if len(fields) < 1 {
		t.Fatalf("no output fields")
	}
	if fields[0].FieldType == nil || fields[0].FieldType.Code() != values.TypeCodeUnknown {
		t.Errorf("conflicting dup-name key typed %v, want Unknown", fields[0].FieldType)
	}

	// Agreeing codes → the shared type flows.
	fields = tr.aggregateOutputColumns(mkAgg(values.NullableInt, values.NullableInt))
	if fields[0].FieldType == nil || fields[0].FieldType.Code() != values.TypeCodeInt {
		t.Errorf("same-typed dup-name key typed %v, want INT", fields[0].FieldType)
	}

	// A FIRST match that is itself UnknownType is still a MATCH: a later
	// typed duplicate must not overwrite it (the name is indeterminate,
	// not first-come-typed). Before the matched-bool split, UnknownType
	// doubled as the not-yet-seen sentinel and the INT duplicate won.
	fields = tr.aggregateOutputColumns(mkAgg(values.UnknownType, values.NullableInt))
	if fields[0].FieldType == nil || fields[0].FieldType.Code() != values.TypeCodeUnknown {
		t.Errorf("unknown-then-typed dup-name key typed %v, want Unknown", fields[0].FieldType)
	}
}
