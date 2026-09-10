package cascades

// RFC-248: the aggregate data-access rule partitions the GroupBy's inner
// filter into scan bounds, residuals over the aggregate row, or a decline.
// These arms drive the partition directly, one per hazard the design named.

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
)

// residualFixture is a COUNT(*) candidate over (region, status, year) of the
// Orders row, with the base row's QOV to build query-side predicates from.
type residualFixture struct {
	cand *AggregateIndexMatchCandidate
	row  values.QuantifiedObjectValue
	// outer is a second quantifier over the SAME row type — a correlated
	// query's outer row, whose fields share the grouping columns' names.
	outer values.QuantifiedObjectValue
}

func newResidualFixture(t *testing.T) residualFixture {
	t.Helper()
	cand := aggregateDataCandidate("cnt_region_status_year", "Orders",
		[]string{"region", "status", "year"}, expressions.AggCount, "",
		[]values.Type{values.NullableString, values.NullableString, values.NullableString})
	scan := mustAggregateDataConstruct(expressions.NewFullUnorderedScanExpression(
		[]string{"Orders"}, aggregateDataRowType("Orders")))
	scanQ := expressions.ForEachQuantifier(expressions.InitialOf(scan))
	row := mustAggregateDataConstruct(scanQ.RequireFlowedObjectValue())
	outerScan := mustAggregateDataConstruct(expressions.NewFullUnorderedScanExpression(
		[]string{"Orders"}, aggregateDataRowType("Orders")))
	outerQ := expressions.ForEachQuantifier(expressions.InitialOf(outerScan))
	outer := mustAggregateDataConstruct(outerQ.RequireFlowedObjectValue())
	return residualFixture{cand: cand, row: row, outer: outer}
}

// preds tags query-side predicates with the fixture's input alias, the way
// extractInnerFilterPredicates tags a filter's conjuncts.
func (f residualFixture) preds(ps ...predicates.QueryPredicate) []aggregateFilterPredicate {
	out := make([]aggregateFilterPredicate, len(ps))
	for i, p := range ps {
		out[i] = aggregateFilterPredicate{pred: p, input: f.row.Correlation()}
	}
	return out
}

func (f residualFixture) field(t *testing.T, name string) values.Value {
	t.Helper()
	return fieldOf(t, f.row, name)
}

func (f residualFixture) outerField(t *testing.T, name string) values.Value {
	t.Helper()
	return fieldOf(t, f.outer, name)
}

func fieldOf(t *testing.T, row values.QuantifiedObjectValue, name string) values.Value {
	t.Helper()
	rt := aggregateDataRowType("Orders")
	for i, fld := range rt.Fields {
		if fld.Name == name {
			return mustAggregateDataConstruct(values.ResolveFieldOrdinals(row, []int{i}))
		}
	}
	t.Fatalf("no field %s", name)
	return nil
}

func (f residualFixture) inputToOuter(t *testing.T, left, right string) *predicates.ComparisonPredicate {
	t.Helper()
	return &predicates.ComparisonPredicate{
		Operand:    f.field(t, left),
		Comparison: predicates.Comparison{Type: predicates.ComparisonEquals, Operand: f.outerField(t, right)},
	}
}

func (f residualFixture) cmp(t *testing.T, col string, typ predicates.ComparisonType, lit any) *predicates.ComparisonPredicate {
	t.Helper()
	c := predicates.NewLiteralComparison(typ, lit)
	return &predicates.ComparisonPredicate{Operand: f.field(t, col), Comparison: c}
}

func (f residualFixture) colToCol(t *testing.T, left, right string) *predicates.ComparisonPredicate {
	t.Helper()
	return &predicates.ComparisonPredicate{
		Operand:    f.field(t, left),
		Comparison: predicates.Comparison{Type: predicates.ComparisonEquals, Operand: f.field(t, right)},
	}
}

// boundColumns names the grouping columns the partition bound, in candidate
// order, with each bound's range type.
func boundColumns(p *aggregatePredicatePartition) []string {
	var out []string
	for i, alias := range p.cand.aliases {
		if cr, ok := p.scanPrefix[alias]; ok {
			kind := "="
			if !cr.IsEquality() {
				kind = "range"
			}
			out = append(out, p.cand.groupCols[i]+":"+kind)
		}
	}
	return out
}

func TestAggregatePredicatePartition(t *testing.T) {
	t.Parallel()
	f := newResidualFixture(t)
	eq := func(col string, lit any) *predicates.ComparisonPredicate {
		return f.cmp(t, col, predicates.ComparisonEquals, lit)
	}
	gt := func(col string, lit any) *predicates.ComparisonPredicate {
		return f.cmp(t, col, predicates.ComparisonGreaterThan, lit)
	}
	lt := func(col string, lit any) *predicates.ComparisonPredicate {
		return f.cmp(t, col, predicates.ComparisonLessThan, lit)
	}

	for _, tc := range []struct {
		name      string
		preds     []predicates.QueryPredicate
		wantOK    bool
		wantBound []string
		wantResid int
	}{
		{
			"input column compared to a same-named OUTER column is a residual that keeps the outer read",
			[]predicates.QueryPredicate{f.inputToOuter(t, "region", "region")},
			true, nil, 1,
		},
		{
			"an outer column alone is not a grouping read and declines",
			[]predicates.QueryPredicate{&predicates.ComparisonPredicate{
				Operand:    f.outerField(t, "region"),
				Comparison: predicates.NewLiteralComparison(predicates.ComparisonEquals, "us"),
			}},
			false, nil, 0,
		},
		{"leading equality is bound", []predicates.QueryPredicate{eq("region", "us")}, true, []string{"region:="}, 0},
		{
			"leading inequality is bound as a range and ends the run",
			[]predicates.QueryPredicate{gt("region", "m"), eq("status", "open")},
			true,
			[]string{"region:range"},
			1,
		},
		{
			"two inequalities on one column fold to one range",
			[]predicates.QueryPredicate{gt("region", "m"), lt("region", "z")},
			true,
			[]string{"region:range"},
			0,
		},
		{
			"contradictory equalities bind the first and re-apply the second",
			[]predicates.QueryPredicate{eq("region", "us"), eq("region", "eu")},
			true,
			[]string{"region:="},
			1,
		},
		{
			"equality beside an inequality on one column binds the equality, the inequality is residual",
			[]predicates.QueryPredicate{eq("region", "us"), gt("region", "m")},
			true,
			[]string{"region:="},
			1,
		},
		{
			"inequality then equality on one column: the equality still wins",
			[]predicates.QueryPredicate{gt("region", "m"), eq("region", "us")},
			true,
			[]string{"region:="},
			1,
		},
		{
			"duplicate equality binds once and re-applies nothing",
			[]predicates.QueryPredicate{eq("region", "us"), eq("region", "us")},
			true,
			[]string{"region:="},
			0,
		},
		{"non-leading equality is residual", []predicates.QueryPredicate{eq("status", "open")}, true, nil, 1},
		{
			"gap: leading bound, later residual",
			[]predicates.QueryPredicate{eq("region", "us"), eq("year", "2024")},
			true,
			[]string{"region:="},
			1,
		},
		{
			"IS NULL on a non-leading column is residual",
			[]predicates.QueryPredicate{f.cmp(t, "status", predicates.ComparisonIsNull, nil)},
			true, nil, 1,
		},
		{
			"OR of grouping predicates is residual",
			[]predicates.QueryPredicate{&predicates.OrPredicate{SubPredicates: []predicates.QueryPredicate{eq("status", "a"), eq("year", "b")}}},
			true, nil, 1,
		},
		{
			"NOT of a grouping predicate is residual",
			[]predicates.QueryPredicate{&predicates.NotPredicate{Child: eq("status", "a")}},
			true, nil, 1,
		},
		{
			"column to column over two grouping columns is residual",
			[]predicates.QueryPredicate{f.colToCol(t, "region", "status")},
			true, nil, 1,
		},
		{
			"a leaf on a non-grouping column declines",
			[]predicates.QueryPredicate{eq("region", "us"), gt("amount", int64(0))},
			false, nil, 0,
		},
		{
			"column to column with a non-grouping column declines",
			[]predicates.QueryPredicate{f.colToCol(t, "region", "amount")},
			false, nil, 0,
		},
		{
			"a predicate with no field leaf declines",
			[]predicates.QueryPredicate{&predicates.ValuePredicate{Value: &values.ConstantValue{Value: true}}},
			false, nil, 0,
		},
		{
			"a predicate kind outside the allow-list declines",
			[]predicates.QueryPredicate{&predicates.ConstantPredicate{Value: predicates.TriTrue}},
			false, nil, 0,
		},
		{
			"a whole-row leaf declines",
			[]predicates.QueryPredicate{&predicates.ValuePredicate{Value: f.row}},
			false, nil, 0,
		},
		{
			"a grouping leaf under a function wrapper is residual (walk descends, wrapper kept)",
			[]predicates.QueryPredicate{&predicates.ComparisonPredicate{
				Operand:    values.NewScalarFunctionValue("UPPER", values.NullableString, f.field(t, "status")),
				Comparison: predicates.NewLiteralComparison(predicates.ComparisonEquals, "OPEN"),
			}},
			true, nil, 1,
		},
		{
			"a non-grouping leaf under a function wrapper declines",
			[]predicates.QueryPredicate{&predicates.ComparisonPredicate{
				Operand:    values.NewScalarFunctionValue("ABS", values.NullableLong, f.field(t, "amount")),
				Comparison: predicates.NewLiteralComparison(predicates.ComparisonEquals, int64(1)),
			}},
			false, nil, 0,
		},
		{
			"leading IS NULL binds as an equality on the null key",
			[]predicates.QueryPredicate{f.cmp(t, "region", predicates.ComparisonIsNull, nil)},
			true,
			[]string{"region:="},
			0,
		},
		{
			"a vector distance-rank bound on a grouping column declines",
			[]predicates.QueryPredicate{f.cmp(t, "region", predicates.ComparisonDistanceRankLessThan, int64(3))},
			false, nil, 0,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			p, ok := partitionAggregatePredicates(f.cand, f.preds(tc.preds...))
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tc.wantOK)
			}
			if !ok {
				return
			}
			if got := boundColumns(p); !sameStrings(got, tc.wantBound) {
				t.Fatalf("bound = %v, want %v", got, tc.wantBound)
			}
			if len(p.residuals) != tc.wantResid {
				t.Fatalf("%d residuals, want %d", len(p.residuals), tc.wantResid)
			}
			if tc.name == "two inequalities on one column fold to one range" {
				// Both comparisons must be IN the range: first-comparison-wins
				// would bind `> m` and silently drop `< z`, the residual-drop
				// bug the old guard existed to prevent.
				cr := p.scanPrefix[f.cand.aliases[0]]
				if n := len(cr.GetInequalityComparisons()); n != 2 {
					t.Fatalf("the folded range carries %d inequality comparison(s), want 2: a comparison the fold "+
						"dropped is neither bound nor residual", n)
				}
			}
		})
	}
}

// TestAggregatePredicatePartition_ScanReceivesOnlyTheTruncatedRun: ToScanPlan
// breaks only on an ABSENT column, never after an inequality, so the map it
// receives must already be the leading run — an equality after a range must
// not appear in the scan's comparisons.
func TestAggregatePredicatePartition_ScanReceivesOnlyTheTruncatedRun(t *testing.T) {
	t.Parallel()
	f := newResidualFixture(t)
	p, ok := partitionAggregatePredicates(f.cand, f.preds(
		f.cmp(t, "region", predicates.ComparisonGreaterThan, "m"),
		f.cmp(t, "status", predicates.ComparisonEquals, "open"),
		f.cmp(t, "year", predicates.ComparisonEquals, "2024"),
	))
	if !ok {
		t.Fatal("partition declined")
	}
	idx := extractIndexPlan(f.cand.ToScanPlan(p.scanPrefix, false))
	if idx == nil {
		t.Fatal("no scan plan")
	}
	if n := len(idx.GetScanComparisons()); n != 1 || idx.GetScanComparisons()[0].IsEquality() {
		t.Fatalf("scan comparisons = %d (first equality=%v), want exactly the one range on region: an equality "+
			"after a range is not applied by the scan and must be a residual", n, n > 0 && idx.GetScanComparisons()[0].IsEquality())
	}
	if len(p.residuals) != 2 {
		t.Fatalf("%d residuals, want 2 (status, year)", len(p.residuals))
	}
}

// TestAggregatePredicatePartition_ApplyResidualsRewritesOntoTheAggregateRow:
// the residual is re-hung on the yielded plan's row at the grouping column's
// ordinal, reads that row's alias and nothing else, and a leaf the rewrite
// cannot place (bridge) declines rather than filtering on the wrong row.
func TestAggregatePredicatePartition_ApplyResidualsRewritesOntoTheAggregateRow(t *testing.T) {
	t.Parallel()
	f := newResidualFixture(t)
	p, ok := partitionAggregatePredicates(f.cand, f.preds(
		f.cmp(t, "status", predicates.ComparisonEquals, "open"),
	))
	if !ok {
		t.Fatal("partition declined a non-leading grouping equality")
	}
	if len(p.residuals) != 1 {
		t.Fatalf("partition residuals=%d, want 1", len(p.residuals))
	}
	scan := extractIndexPlan(f.cand.ToScanPlan(p.scanPrefix, false))
	agg, err := plans.NewRecordQueryAggregateIndexPlan(scan, "Orders", aggregateDataRowType("Orders"), "COUNT")
	if err != nil {
		t.Fatal(err)
	}
	agg = agg.WithGroupColumns(f.cand.groupCols, "").WithGroupColumnLayout(f.cand.GetBaseRowType())
	filtered, ok := p.applyResiduals(agg)
	if !ok {
		t.Fatal("applyResiduals declined a residual over a grouping column")
	}
	fp, isFilter := filtered.(*plans.RecordQueryPredicatesFilterPlan)
	if !isFilter {
		t.Fatalf("applyResiduals returned %T, want a PredicatesFilter over the aggregate plan", filtered)
	}
	preds := fp.GetPredicates()
	if len(preds) != 1 {
		t.Fatalf("%d predicates on the filter, want 1", len(preds))
	}
	cp, isCmp := preds[0].(*predicates.ComparisonPredicate)
	if !isCmp {
		t.Fatalf("residual is %T, want a comparison", preds[0])
	}
	fv, isField := values.AsFieldValue(cp.Operand)
	if !isField || fv.Path() == nil || fv.Path().Len() != 1 || fv.Path().Ordinals()[0] != 1 {
		t.Fatalf("residual reads %s, want ordinal 1 (status) of the aggregate row", values.ExplainValue(cp.Operand))
	}
	innerAlias := fp.GetChildren()[0]
	_ = innerAlias
	if corr := cp.GetCorrelatedTo(); len(corr) != 1 {
		t.Fatalf("residual correlated to %v, want exactly the aggregate row's alias", corr)
	}
	for alias := range cp.GetCorrelatedTo() {
		if alias == f.row.Correlation() {
			t.Fatal("residual still reads the base quantifier: the rewrite did not move it")
		}
	}

	// Bridge: a residual whose leaf the rewrite cannot place declines. Built
	// by hand past the admission (a whole-row leaf is refused there), so this
	// exercises applyResiduals' own assertion.
	p.residuals = f.preds(&predicates.ValuePredicate{Value: f.row})
	if _, ok := p.applyResiduals(agg); ok {
		t.Fatal("applyResiduals accepted a residual still correlated to the base quantifier")
	}
}

// TestAggregatePredicatePartition_OuterCorrelationIsCarriedNotRewritten: in a
// correlated shape, `o.region = c.region` — the outer row's field shares the
// grouping column's NAME — only the input-rooted read moves onto the
// aggregate row; the outer read stays correlated to the outer quantifier. A
// rewrite that matched by name alone turned this into
// `group.region = group.region` and passed every group.
func TestAggregatePredicatePartition_OuterCorrelationIsCarriedNotRewritten(t *testing.T) {
	t.Parallel()
	f := newResidualFixture(t)
	p, ok := partitionAggregatePredicates(f.cand, f.preds(f.inputToOuter(t, "region", "region")))
	if !ok || len(p.residuals) != 1 {
		t.Fatalf("partition ok=%v residuals=%d, want a single residual", ok, len(p.residuals))
	}
	scan := extractIndexPlan(f.cand.ToScanPlan(p.scanPrefix, false))
	agg, err := plans.NewRecordQueryAggregateIndexPlan(scan, "Orders", aggregateDataRowType("Orders"), "COUNT")
	if err != nil {
		t.Fatal(err)
	}
	agg = agg.WithGroupColumns(f.cand.groupCols, "").WithGroupColumnLayout(f.cand.GetBaseRowType())
	filtered, ok := p.applyResiduals(agg)
	if !ok {
		t.Fatal("applyResiduals declined a residual over a grouping column with an outer parameter")
	}
	fp := filtered.(*plans.RecordQueryPredicatesFilterPlan)
	cp := fp.GetPredicates()[0].(*predicates.ComparisonPredicate)
	corr := cp.GetCorrelatedTo()
	if _, outerKept := corr[f.outer.Correlation()]; !outerKept {
		t.Fatalf("the outer correlation was erased: residual now reads %v; `o.region = c.region` became a "+
			"comparison of the group with itself", corr)
	}
	if _, inputGone := corr[f.row.Correlation()]; inputGone {
		t.Fatal("the input read was not moved onto the aggregate row")
	}
	if len(corr) != 2 {
		t.Fatalf("residual correlated to %v, want exactly the aggregate row and the outer quantifier", corr)
	}
	if !values.ValuesStructurallyEqual(cp.Comparison.Operand, f.outerField(t, "region")) {
		t.Fatalf("the outer operand changed: %s", values.ExplainValue(cp.Comparison.Operand))
	}
}

// TestAggregatePredicatePartition_WrappedLeafKeepsItsWrapper: a residual
// whose grouping-column leaf sits under a function wrapper is rewritten at
// the leaf only — `UPPER(status) = 'OPEN'` becomes `UPPER(<row>.status) =
// 'OPEN'` over the aggregate row, the wrapper intact and the bridge holding.
func TestAggregatePredicatePartition_WrappedLeafKeepsItsWrapper(t *testing.T) {
	t.Parallel()
	f := newResidualFixture(t)
	wrapped := &predicates.ComparisonPredicate{
		Operand:    values.NewScalarFunctionValue("UPPER", values.NullableString, f.field(t, "status")),
		Comparison: predicates.NewLiteralComparison(predicates.ComparisonEquals, "OPEN"),
	}
	p, ok := partitionAggregatePredicates(f.cand, f.preds(wrapped))
	if !ok || len(p.residuals) != 1 || len(p.scanPrefix) != 0 {
		t.Fatalf("partition ok=%v residuals=%d bound=%d, want one residual and no bound", ok, len(p.residuals), len(p.scanPrefix))
	}
	scan := extractIndexPlan(f.cand.ToScanPlan(p.scanPrefix, false))
	agg, err := plans.NewRecordQueryAggregateIndexPlan(scan, "Orders", aggregateDataRowType("Orders"), "COUNT")
	if err != nil {
		t.Fatal(err)
	}
	agg = agg.WithGroupColumns(f.cand.groupCols, "").WithGroupColumnLayout(f.cand.GetBaseRowType())
	filtered, ok := p.applyResiduals(agg)
	if !ok {
		t.Fatal("applyResiduals declined a wrapped grouping leaf")
	}
	cp := filtered.(*plans.RecordQueryPredicatesFilterPlan).GetPredicates()[0].(*predicates.ComparisonPredicate)
	fn, isFn := cp.Operand.(*values.ScalarFunctionValue)
	if !isFn || fn.FuncName != "UPPER" || len(fn.Children()) != 1 {
		t.Fatalf("the wrapper did not survive the rewrite: %s", values.ExplainValue(cp.Operand))
	}
	leaf, isField := values.AsFieldValue(fn.Children()[0])
	if !isField || leaf.Path().Ordinals()[0] != 1 {
		t.Fatalf("the wrapped leaf reads %s, want ordinal 1 (status) of the aggregate row", values.ExplainValue(fn.Children()[0]))
	}
	if corr := cp.GetCorrelatedTo(); len(corr) != 1 {
		t.Fatalf("residual correlated to %v, want the aggregate row only", corr)
	}
}
