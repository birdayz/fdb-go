package query

import (
	"strings"
	"testing"

	"github.com/birdayz/fdb-record-layer-go/gen"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/expressions"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/values"
	"github.com/birdayz/fdb-record-layer-go/pkg/relational/core/query/logical"
)

// TestTableColumns_FromMetadata pins the md→columns derivation (tableColumns +
// fieldTypeForFD) that 7.6 uses to source source-anchored join-leg columns. It
// does NOT type the scan leaf (that was NAK'd — the scan stays AnyRecord; see
// RFC-077 v3). Columns are upper-cased; proto Kind maps to values.Type; repeated
// and message (non-UUID) fields collapse to UnknownType.
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
	primitive("ORDER_ID", values.TypeCodeLong)
	primitive("PRICE", values.TypeCodeInt)
	primitive("VECTOR_DATA", values.TypeCodeBytes)
	// Message (non-UUID) field FLOWER and repeated field TAGS collapse to Unknown.
	if byName["FLOWER"] != values.UnknownType {
		t.Errorf("FLOWER (message): got %v, want UnknownType", byName["FLOWER"])
	}
	if byName["TAGS"] != values.UnknownType {
		t.Errorf("TAGS (repeated): got %v, want UnknownType", byName["TAGS"])
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

	// (1) Scan → bare metadata columns (upper-cased).
	scanCols := names(tr.legColumns(logical.NewScan("Order", "O")))
	for _, c := range []string{"ORDER_ID", "PRICE", "QUANTITY"} {
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

	// (3) Join → the DOTTED (source-accurate) subset of NewAnchoredJoinRecord's
	// fields. A join leg propagates ONLY its already-qualified per-table columns
	// (O.ID, C.PRICE, …) to a parent — NOT the bare-last-wins names (those are the
	// join's own result-value resolution convenience; propagating them would make a
	// parent re-qualify them into spurious "_2" keys, Torvalds' nested-parity catch).
	join := logical.NewJoin(logical.NewScan("Order", "O"), logical.NewScan("Customer", "C"), logical.JoinInner, "")
	joinLegCols := tr.legColumns(join)
	wantRC := values.NewAnchoredJoinRecord([]values.AnchoredJoinLeg{
		{Alias: values.NamedCorrelationIdentifier("O"), Columns: tr.legColumns(logical.NewScan("Order", "O"))},
		{Alias: values.NamedCorrelationIdentifier("C"), Columns: tr.legColumns(logical.NewScan("Customer", "C"))},
	})
	gotNames := names(joinLegCols)
	// Expected = exactly the DOTTED fields of the RC.
	wantDotted := map[string]bool{}
	for _, f := range wantRC.Fields {
		if strings.Contains(f.Name, ".") {
			wantDotted[f.Name] = true
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

// TestBuildJoinResultValue_NestedNoSpuriousKeys pins Torvalds' nested-parity catch
// (RFC-077 7.6): a 3-way nested-join seed's anchored RC must carry only
// SOURCE-ACCURATE keys and NO dedup-suffixed "_2" garbage. Before the
// dotted-only legColumns fix, a sub-join leg's bare columns were re-qualified
// under sourceAlias(inner)=right-leg, colliding with the inner's verbatim dotted
// keys, so NewRecordConstructorValue suffixed "C.PRICE_2" etc. — spurious keys the
// opaque merge never produces.
func TestBuildJoinResultValue_NestedNoSpuriousKeys(t *testing.T) {
	t.Parallel()
	tr := &cascadesTranslator{md: demoMetaData(t)}

	// 3-way: (Order O ⋈ Customer C) ⋈ TypedRecord TR. O and C share PRICE.
	inner := logical.NewJoin(logical.NewScan("Order", "O"), logical.NewScan("Customer", "C"), logical.JoinInner, "")
	outer := logical.NewJoin(inner, logical.NewScan("TypedRecord", "TR"), logical.JoinInner, "")

	rv := tr.buildJoinResultValue(outer.Left, outer.Right, sourceAlias(outer.Left), sourceAlias(outer.Right))
	rc, ok := rv.(*values.RecordConstructorValue)
	if !ok {
		t.Fatalf("3-way seed result value = %T, want anchored *RecordConstructorValue", rv)
	}

	for _, f := range rc.Fields {
		if strings.Contains(f.Name, "_2") || strings.HasSuffix(f.Name, "_3") {
			t.Errorf("spurious dedup-suffixed key %q (nested-parity bug — a bare leg name was re-qualified into an existing dotted key)", f.Name)
		}
	}
	// The inner join's source-accurate dotted keys propagate verbatim.
	got := map[string]bool{}
	for _, f := range rc.Fields {
		got[f.Name] = true
	}
	for _, want := range []string{"O.PRICE", "C.PRICE", "O.ORDER_ID", "C.CUSTOMER_ID"} {
		if !got[want] {
			t.Errorf("3-way seed missing source-accurate dotted key %q; got %v", want, got)
		}
	}
}

func TestTranslateScan(t *testing.T) {
	t.Parallel()
	scan := logical.NewScan("orders", "")
	ref := TranslateToCascades(scan)
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
	// LogicalLimit is skipped by the translator — LIMIT/OFFSET is
	// applied post-execution by paginatingRows. The Cascades pipeline
	// sees only the inner (scan) expression.
	scan := logical.NewScan("orders", "")
	limit := logical.NewLimit(scan, 10, 5)
	ref := TranslateToCascades(limit)
	if ref == nil {
		t.Fatal("expected non-nil reference")
	}
	members := ref.Members()
	if _, ok := members[0].(*expressions.FullUnorderedScanExpression); !ok {
		t.Fatalf("expected FullUnorderedScanExpression (limit skipped), got %T", members[0])
	}
}

func TestTranslateUnion(t *testing.T) {
	t.Parallel()
	scanA := logical.NewScan("A", "")
	scanB := logical.NewScan("B", "")
	union := logical.NewUnion([]logical.LogicalOperator{scanA, scanB}, false)
	ref := TranslateToCascades(union)
	if ref == nil {
		t.Fatal("expected non-nil reference")
	}
	if _, ok := ref.Members()[0].(*expressions.LogicalUnionExpression); !ok {
		t.Fatalf("expected LogicalUnionExpression, got %T", ref.Members()[0])
	}
}

// TestUnionBranchNormalizable_AggregateArity pins the RFC-081 gate boundary. UNGROUPED bare
// aggregate branches are unchanged from RFC-080 (always StreamingAgg) — always normalizable,
// NOT re-gated (no regression — codex). A GROUPED bare aggregate is normalizable IFF every
// aggregate's output name is STABLE between the logical leg schema and the physical row key —
// i.e. COUNT(*) or FUNC(<bare column>); a qualified operand (SUM(T.C)), a constant
// (COUNT(1)/COUNT(NULL)), an expression, or DISTINCT canonicalizes differently → gated (clean
// error, never wrong rows). 0-aggregate (group-only) is also gated.
func TestUnionBranchNormalizable_AggregateArity(t *testing.T) {
	t.Parallel()
	tr := &cascadesTranslator{}
	scan := logical.NewScan("A", "")

	// Stable forms → normalizable (grouped and ungrouped).
	for _, tc := range []struct {
		name string
		agg  *logical.LogicalAggregate
	}{
		{"ungrouped COUNT(*)", logical.NewAggregate(scan, nil, []string{"COUNT(*)"}, []string{"X"}, "")},
		{"ungrouped SUM(V),COUNT(*)", logical.NewAggregate(scan, nil, []string{"SUM(V)", "COUNT(*)"}, []string{"S", "C"}, "")},
		// Ungrouped is unchanged from RFC-080 (always StreamingAgg) — the stable-name gate applies
		// only to GROUPED branches, so ungrouped constants/qualified stay normalizable (no
		// regression of previously-working ungrouped union join legs — codex).
		{"ungrouped COUNT(1) [not re-gated]", logical.NewAggregate(scan, nil, []string{"COUNT(1)"}, []string{""}, "")},
		{"ungrouped SUM(T.C) [not re-gated]", logical.NewAggregate(scan, nil, []string{"SUM(T.C)"}, []string{""}, "")},
		{"grouped COUNT(*)", logical.NewAggregate(scan, []string{"G"}, []string{"COUNT(*)"}, []string{""}, "")},
		{"grouped SUM(V),COUNT(*)", logical.NewAggregate(scan, []string{"G"}, []string{"SUM(V)", "COUNT(*)"}, []string{"", ""}, "")},
		{"grouped COUNT(X) bare col", logical.NewAggregate(scan, []string{"G"}, []string{"COUNT(X)"}, []string{""}, "")},
	} {
		if !tr.unionBranchNormalizable(tc.agg) {
			t.Errorf("%s: must be normalizable", tc.name)
		}
	}

	// Divergent forms → gated.
	qualified := logical.NewAggregate(scan, []string{"G"}, []string{"SUM(T.C)"}, []string{""}, "")
	if tr.unionBranchNormalizable(qualified) {
		t.Error("qualified aggregate SUM(T.C) must NOT be normalizable (physical strips qualifier → SUM(C))")
	}

	// COUNT(<numeric constant>) — gated by both the text (leading digit) and the ConstantValue operand.
	constNum := logical.NewAggregate(scan, []string{"G"}, []string{"COUNT(1)"}, []string{""}, "")
	constNum.AggregateOperands = []values.Value{&values.ConstantValue{Value: int64(1)}}
	if tr.unionBranchNormalizable(constNum) {
		t.Error("COUNT(1) must NOT be normalizable (count-star name mismatch)")
	}

	// COUNT(NULL) — text arg "NULL" LOOKS like an identifier, so only the ConstantValue operand
	// catches it. This is why the gate combines text + operand (Torvalds/codex class).
	constNull := logical.NewAggregate(scan, []string{"G"}, []string{"COUNT(NULL)"}, []string{""}, "")
	constNull.AggregateOperands = []values.Value{&values.ConstantValue{Value: nil}}
	if tr.unionBranchNormalizable(constNull) {
		t.Error("COUNT(NULL) must NOT be normalizable (constant folds to count-star; text alone misses it)")
	}

	// DISTINCT → gated via the branch flag.
	distinct := logical.NewAggregate(scan, []string{"G"}, []string{"COUNT(X)"}, []string{""}, "")
	distinct.HasDistinctAggregate = true
	if tr.unionBranchNormalizable(distinct) {
		t.Error("DISTINCT aggregate must NOT be normalizable")
	}

	// 0-aggregate (group-only and ungrouped) → gated.
	if tr.unionBranchNormalizable(logical.NewAggregate(scan, []string{"G"}, nil, nil, "")) {
		t.Error("0-aggregate group-only must NOT be normalizable")
	}
	if tr.unionBranchNormalizable(logical.NewAggregate(scan, nil, nil, nil, "")) {
		t.Error("0-aggregate ungrouped must NOT be normalizable")
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
	scan := logical.NewScan("orders", "")
	sort := logical.NewSort(scan, []logical.SortKey{
		{Expr: "price", Dir: logical.SortAsc},
		{Expr: "id", Dir: logical.SortDesc},
	})
	ref := TranslateToCascades(sort)
	if ref == nil {
		t.Fatal("expected non-nil reference")
	}
	if _, ok := ref.Members()[0].(*expressions.LogicalSortExpression); !ok {
		t.Fatalf("expected LogicalSortExpression, got %T", ref.Members()[0])
	}
}

func TestTranslateProject(t *testing.T) {
	t.Parallel()
	scan := logical.NewScan("orders", "")
	proj := logical.NewProject(scan, []string{"id", "price"}, []string{"", "cost"})
	ref := TranslateToCascades(proj)
	if ref == nil {
		t.Fatal("expected non-nil reference")
	}
	if _, ok := ref.Members()[0].(*expressions.LogicalProjectionExpression); !ok {
		t.Fatalf("expected LogicalProjectionExpression, got %T", ref.Members()[0])
	}
}

func TestTranslateJoin(t *testing.T) {
	t.Parallel()
	// md is REQUIRED to translate a join: the seed result value is the
	// source-anchored RecordConstructorValue, whose leg columns come from
	// metadata (RFC-077 7.6). The opaque-seed fallback for the catalog-free path
	// was retired, so a nil-md join is untranslatable (TestTranslateJoinNilMd).
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
	if !ok || !rc.AnchoredJoin {
		t.Fatalf("expected source-anchored RecordConstructorValue result, got %T (anchored=%v)", sel.GetResultValue(), ok && rc.AnchoredJoin)
	}
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

// TestTranslateJoinWithExists_NilMdUntranslatable is the Torvalds-review regression:
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

func TestTranslateNil(t *testing.T) {
	t.Parallel()
	ref := TranslateToCascades(nil)
	if ref != nil {
		t.Fatal("expected nil for nil input")
	}
}

func TestTranslateAggregate(t *testing.T) {
	t.Parallel()
	scan := logical.NewScan("orders", "")
	agg := logical.NewAggregate(scan, []string{"CATEGORY"}, []string{"SUM(PRICE)", "COUNT(*)"}, []string{"total", "cnt"}, "")
	ref := TranslateToCascades(agg)
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
	scan := logical.NewScan("orders", "")
	agg := logical.NewAggregate(scan, nil, []string{"COUNT(*)"}, []string{"cnt"}, "")
	ref := TranslateToCascades(agg)
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

func TestParseAggregateText(t *testing.T) {
	t.Parallel()
	tests := []struct {
		input string
		fn    expressions.AggregateFunction
		ok    bool
	}{
		{"COUNT(*)", expressions.AggCount, true},
		{"SUM(PRICE)", expressions.AggSum, true},
		{"AVG(X)", expressions.AggAvg, true},
		{"MIN(Y)", expressions.AggMin, true},
		{"MAX(Z)", expressions.AggMax, true},
		{"count(*)", expressions.AggCount, true},
		{"UNKNOWN(X)", 0, false},
		{"noparen", 0, false},
	}
	for _, tc := range tests {
		spec, ok := parseAggregateText(tc.input)
		if ok != tc.ok {
			t.Errorf("parseAggregateText(%q): ok=%v, want %v", tc.input, ok, tc.ok)
			continue
		}
		if ok && spec.Function != tc.fn {
			t.Errorf("parseAggregateText(%q): fn=%d, want %d", tc.input, spec.Function, tc.fn)
		}
	}
}

func TestTranslateDistinct(t *testing.T) {
	t.Parallel()
	scan := logical.NewScan("orders", "")
	dist := logical.NewDistinct(scan)
	ref := TranslateToCascades(dist)
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
	body := logical.NewScan("Product", "")
	main := logical.NewScan("expensive", "")
	cte := logical.NewCTE("expensive", body, main, false)

	ref := TranslateToCascades(cte)
	if ref == nil {
		t.Fatal("expected non-nil reference for non-recursive CTE")
	}
	scan, ok := ref.Members()[0].(*expressions.FullUnorderedScanExpression)
	if !ok {
		t.Fatalf("expected inlined FullUnorderedScanExpression, got %T", ref.Members()[0])
	}
	if scan.GetRecordTypes()[0] != "Product" {
		t.Fatalf("expected scan of Product, got %s", scan.GetRecordTypes()[0])
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
	bodyA := logical.NewScan("Product", "")
	mainA := logical.NewScan("B", "")
	bodyB := logical.NewScan("A", "")
	cteA := logical.NewCTE("A", bodyA, mainA, false)
	cteB := logical.NewCTE("B", bodyB, cteA, false)

	ref := TranslateToCascades(cteB)
	if ref == nil {
		t.Fatal("expected non-nil reference for chained CTEs")
	}
	scan, ok := ref.Members()[0].(*expressions.FullUnorderedScanExpression)
	if !ok {
		t.Fatalf("expected FullUnorderedScanExpression, got %T", ref.Members()[0])
	}
	if scan.GetRecordTypes()[0] != "Product" {
		t.Fatalf("expected scan of Product (A inlined into B's body), got %s", scan.GetRecordTypes()[0])
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
	body := logical.NewProject(logical.NewScan("T", ""), []string{"id"}, []string{""})
	main := logical.NewProject(logical.NewScan("T", ""), []string{"id"}, []string{""})
	cte := logical.NewCTE("T", body, main, false)

	ref := TranslateToCascades(cte)
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
	// CTE referenced twice in the main query (via join). md is REQUIRED so the
	// join's CTE-reference legs anchor (the leg columns derive from the CTE body's
	// real table — RFC-077 7.6); the opaque-seed fallback was retired.
	body := logical.NewScan("Order", "")
	left := logical.NewScan("p", "")
	right := logical.NewScan("p", "")
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
}

func TestTranslateAggregateWithHavingReturnsNil(t *testing.T) {
	t.Parallel()
	scan := logical.NewScan("orders", "")
	agg := logical.NewAggregate(scan, []string{"REGION"}, []string{"SUM(PRICE)"}, []string{"total"}, "SUM(PRICE) > 100")
	ref := TranslateToCascades(agg)
	if ref != nil {
		t.Fatal("expected nil — aggregate with HAVING should bail to naive")
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
		{"projection with ABS in Value tree", func() logical.LogicalOperator {
			p := logical.NewProject(logical.NewScan("T", ""), []string{"x"}, nil)
			p.ProjectedValues = []values.Value{
				values.NewScalarFunctionValue("ABS", values.UnknownType,
					&values.FieldValue{Field: "x", Typ: values.UnknownType}),
			}
			return p
		}(), "ABS"},
		{"projection with SQRT in Value tree", func() logical.LogicalOperator {
			p := logical.NewProject(logical.NewScan("T", ""), []string{"x"}, nil)
			p.ProjectedValues = []values.Value{
				values.NewScalarFunctionValue("SQRT", values.UnknownType,
					&values.FieldValue{Field: "x", Typ: values.UnknownType}),
			}
			return p
		}(), "SQRT"},
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
				values.NewScalarFunctionValue("COALESCE", values.UnknownType,
					&values.FieldValue{Field: "a", Typ: values.UnknownType}),
			}
			return p
		}(), ""},
		{"unsafe func in value", func() logical.LogicalOperator {
			p := logical.NewProject(logical.NewScan("T", ""), []string{"x"}, nil)
			p.ProjectedValues = []values.Value{
				values.NewScalarFunctionValue("ABS", values.UnknownType,
					&values.FieldValue{Field: "a", Typ: values.UnknownType}),
			}
			return p
		}(), "ABS"},
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
		inner := logical.NewScan("real_table", "")
		body := logical.NewScan("real_table", "")
		cte := logical.NewCTE("my_cte", body, inner, false)
		got := sourceAlias(cte)
		if got != "MY_CTE" {
			t.Errorf("want MY_CTE, got %s", got)
		}
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
