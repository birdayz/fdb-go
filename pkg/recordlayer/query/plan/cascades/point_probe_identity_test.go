package cascades

import (
	"fmt"
	"math"
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
)

func mustPointProbeConstruct[T any](value T, err error) T {
	if err != nil {
		panic("construct point-probe fixture: " + err.Error())
	}
	return value
}

type pointProbePKCtx struct {
	indexTestPlanContext
	pk []string
}

func (c *pointProbePKCtx) GetPrimaryKeyColumns(string) []string { return c.pk }

func pointProbeLiteral(lit any) values.Value {
	var typ values.Type
	switch lit.(type) {
	case int64:
		typ = values.NotNullLong
	case float32:
		typ = values.NotNullFloat
	case float64:
		typ = values.NotNullDouble
	default:
		panic(fmt.Sprintf("unsupported point-probe literal type %T", lit))
	}
	return &values.ConstantValue{Value: lit, Typ: typ}
}

// predicatesFilterIsFullPKPointProbe answers a cardinality question — "does
// this filter over a full scan touch at most one record?" — and it used to
// answer it by comparing the scanned type's primary-key column NAMES against
// the leaf names of the filter's equality operands.
//
// That conflated a correlated OUTER reference with the scanned record's own
// primary key whenever two tables share a column name, which for `ID` is very
// nearly always. A correlated EXISTS puts `p.ID` into the predicate list of a
// filter sitting over an ORDERS scan; `ORDERS.ID` found `ID` in the set and a
// full scan was declared a one-record point probe. Criterion #2 then ranked
// that plan as if it read a single record.
//
// These layouts are the minimum needed to express that: two tables whose
// primary key is spelled the same and sits at the same ordinal, so the leaf
// name AND the bare ordinal both fail to tell them apart. Only the full triple
// does.
var pointProbeIdentityLayouts = map[string]*values.RecordType{
	"ORDERS":   pointProbeLayout("ORDERS", "ID", "PRODUCT_ID", "QTY"),
	"PRODUCTS": pointProbeLayout("PRODUCTS", "ID", "CATEGORY", "PRICE"),
	// Composite-PK table, for the partial-bind direction.
	"LINES": pointProbeLayout("LINES", "TENANT", "LINE_NO", "SKU"),
}

var pointProbeIdentityPKTypes = map[string][]values.Type{
	"ORDERS":   {values.NotNullLong},
	"PRODUCTS": {values.NotNullLong},
	"LINES":    {values.NotNullLong, values.NotNullLong},
}

func pointProbeLayout(name string, cols ...string) *values.RecordType {
	fields := make([]values.Field, len(cols))
	for i, c := range cols {
		fields[i] = values.Field{Name: c, FieldType: values.NotNullLong, Ordinal: i}
	}
	return values.NewRecordType(name, false, fields)
}

// pointProbeRef builds a resolved column reference the way the SQL resolver
// does: the ordinal the column has in rt's declared order, stamped with rt's
// domain. corr is the quantifier whose row it reads; the zero identifier makes
// it CHILDLESS, which by the ColumnIdentity contract means "this value's own
// source" — the shape most filter operands really have.
func pointProbeRef(t *testing.T, rt string, corr values.CorrelationIdentifier, field string) values.Value {
	t.Helper()
	layout, ok := pointProbeIdentityLayouts[rt]
	if !ok {
		t.Fatalf("setup: no layout registered for %q", rt)
	}
	ord, found := layout.FieldIndexUnique(field)
	if !found {
		t.Fatalf("setup: %s does not declare %q", rt, field)
	}
	root := mustPointProbeConstruct(values.NewQuantifiedObjectValue(corr, layout))
	return mustPointProbeConstruct(values.ResolveFieldOrdinals(root, []int{ord}))
}

func pointProbeEqFilter(
	t *testing.T,
	rt string,
	innerAlias values.CorrelationIdentifier,
	operands ...values.Value,
) (*plans.RecordQueryPredicatesFilterPlan, *plans.RecordQueryScanPlan) {
	t.Helper()
	layout, ok := pointProbeIdentityLayouts[rt]
	if !ok {
		t.Fatalf("setup: no layout registered for %q", rt)
	}
	scan := mustPointProbeConstruct(plans.NewRecordQueryScanPlan([]string{rt}, layout, false)).
		WithKeyComponentTypes(pointProbeIdentityPKTypes[rt])
	preds := make([]predicates.QueryPredicate, 0, len(operands))
	for _, op := range operands {
		preds = append(preds, predicates.NewComparisonPredicate(op,
			predicates.NewLiteralComparison(predicates.ComparisonEquals, int64(1))))
	}
	return mustPointProbeConstruct(plans.NewRecordQueryPredicatesFilterPlanWithAlias(
		scan, preds, innerAlias)), scan
}

// TestPredicatesFilterIsFullPKPointProbe_Identity probes each element of the
// identity triple SEPARATELY.
//
// The separation is not stylistic. Measured over the explaindiff corpus, the
// domain check and the correlation check reject exactly the same 7 predicates,
// so a corpus run cannot tell which is load-bearing and a mutation that drops
// either one still measures clean. The misdomained case below therefore holds
// the correlation EQUAL to the inner quantifier's, and the mis-correlated case
// holds the domain-bearing ordinal equal — each isolates one element.
func TestPredicatesFilterIsFullPKPointProbe_Identity(t *testing.T) {
	t.Parallel()

	ordersAlias := values.NamedCorrelationIdentifier("O")
	ctx := &pointProbePKCtx{pk: []string{"ID"}}

	t.Run("a correlated OUTER reference is not this scan's primary key", func(t *testing.T) {
		t.Parallel()
		// The live defect. `p.ID` reads the PRODUCTS row; ORDERS' primary key
		// is unbound, and a full ORDERS scan is not a point probe.
		outerID := pointProbeRef(t, "PRODUCTS", values.NamedCorrelationIdentifier("P"), "ID")
		filter, scan := pointProbeEqFilter(t, "ORDERS", ordersAlias, outerID)
		if predicatesFilterIsFullPKPointProbe(filter, scan, ctx) {
			t.Fatal("a full ORDERS scan filtered only by a correlated PRODUCTS.ID reference " +
				"was declared a one-record point probe — the cost model bounds an " +
				"unbounded access at 1 because two tables spell their key the same")
		}
	})

	t.Run("a same-ordinal reference in another DOMAIN, correlation held equal", func(t *testing.T) {
		t.Parallel()
		// PRODUCTS.ID is ordinal 0 exactly as ORDERS.ID is, and this one is
		// stamped with the ORDERS filter's OWN inner alias. Correlation agrees,
		// ordinal agrees; only the domain says no. Dropping the domain check
		// accepts it and nothing else in the suite notices.
		misdomained := pointProbeRef(t, "PRODUCTS", ordersAlias, "ID")
		filter, scan := pointProbeEqFilter(t, "ORDERS", ordersAlias, misdomained)
		if predicatesFilterIsFullPKPointProbe(filter, scan, ctx) {
			t.Fatal("an ordinal baked against PRODUCTS' layout satisfied ORDERS' primary key — " +
				"an ordinal compared across layouts is the same conflation as a name, " +
				"with a type that reads as authoritative")
		}
	})

	t.Run("a same-domain reference under another CORRELATION", func(t *testing.T) {
		t.Parallel()
		// A self-join: the reference is baked against ORDERS' own layout, so
		// the ordinal and the domain both agree with the primary key. Only the
		// correlation says it reads a DIFFERENT ORDERS row. Dropping the
		// correlation check accepts it and nothing else notices.
		otherLeg := pointProbeRef(t, "ORDERS", values.NamedCorrelationIdentifier("O2"), "ID")
		filter, scan := pointProbeEqFilter(t, "ORDERS", ordersAlias, otherLeg)
		if predicatesFilterIsFullPKPointProbe(filter, scan, ctx) {
			t.Fatal("a reference to ANOTHER ORDERS row satisfied this scan's primary key — " +
				"ordinal 0 of two quantifiers are different columns")
		}
	})

	t.Run("the scan's own key column binds it", func(t *testing.T) {
		t.Parallel()
		ownID := pointProbeRef(t, "ORDERS", ordersAlias, "ID")
		filter, scan := pointProbeEqFilter(t, "ORDERS", ordersAlias, ownID)
		if !predicatesFilterIsFullPKPointProbe(filter, scan, ctx) {
			t.Fatal("a genuine `o.ID = ?` equality over a full ORDERS scan was NOT recognised " +
				"as a point probe — over-declining here un-bounds a PK probe and is " +
				"the mis-ranking this bound exists to prevent, pointed the other way")
		}
	})

	t.Run("an exact own-row reference binds it", func(t *testing.T) {
		t.Parallel()
		// RFC-232 admits field reads only through an exact quantified row. The
		// filter's own alias is therefore explicit rather than encoded as an
		// ambiguous childless field.
		ownID := pointProbeRef(t, "ORDERS", ordersAlias, "ID")
		filter, scan := pointProbeEqFilter(t, "ORDERS", ordersAlias, ownID)
		if !predicatesFilterIsFullPKPointProbe(filter, scan, ctx) {
			t.Fatal("an exact own-row `ID = ?` equality was not recognised as a point probe")
		}
	})

	t.Run("a non-key column does not bind the key", func(t *testing.T) {
		t.Parallel()
		qty := pointProbeRef(t, "ORDERS", ordersAlias, "QTY")
		filter, scan := pointProbeEqFilter(t, "ORDERS", ordersAlias, qty)
		if predicatesFilterIsFullPKPointProbe(filter, scan, ctx) {
			t.Fatal("`o.QTY = ?` was accepted as a full primary-key bind")
		}
	})

	t.Run("a partial prefix of a composite key is still a range", func(t *testing.T) {
		t.Parallel()
		compositeCtx := &pointProbePKCtx{pk: []string{"TENANT", "LINE_NO"}}
		tenant := pointProbeRef(t, "LINES", ordersAlias, "TENANT")
		filter, scan := pointProbeEqFilter(t, "LINES", ordersAlias, tenant)
		if predicatesFilterIsFullPKPointProbe(filter, scan, compositeCtx) {
			t.Fatal("an equality on only the first column of a composite primary key " +
				"was declared a point probe")
		}
		lineNo := pointProbeRef(t, "LINES", ordersAlias, "LINE_NO")
		full, fullScan := pointProbeEqFilter(t, "LINES", ordersAlias, tenant, lineNo)
		if !predicatesFilterIsFullPKPointProbe(full, fullScan, compositeCtx) {
			t.Fatal("equalities covering the FULL composite primary key were not " +
				"recognised as a point probe")
		}
	})

	t.Run("a scan with no declared column order is rejected", func(t *testing.T) {
		t.Parallel()
		// No layout means no domain in which a primary-key proof can even be
		// stated. RFC-232 rejects that scan at construction instead of allowing
		// it to reach the cost proof and fall back to display names.
		if _, err := plans.NewRecordQueryScanPlan(
			[]string{"ORDERS"}, nil, false); err == nil {
			t.Fatal("a scan with no declared column order was admitted")
		}
	})
}

func TestResidualFieldReadsScanCarrierRequiresExactCurrentOwner(t *testing.T) {
	t.Parallel()

	rowType := pointProbeIdentityLayouts["ORDERS"]
	first := mustPointProbeConstruct(plans.NewRecordQueryScanPlan(
		[]string{"ORDERS"}, rowType, false))
	second := mustPointProbeConstruct(plans.NewRecordQueryScanPlan(
		[]string{"ORDERS"}, rowType, false))
	firstLayout := mustPointProbeConstruct(first.ProvidedOutputLayout())
	secondLayout := mustPointProbeConstruct(second.ProvidedOutputLayout())
	if firstLayout.Carrier() == secondLayout.Carrier() {
		t.Fatal("independent scan fixtures unexpectedly share a current carrier")
	}
	firstID := mustPointProbeConstruct(values.ResolveFieldOrdinals(
		firstLayout.Carrier(), []int{0}))
	secondID := mustPointProbeConstruct(values.ResolveFieldOrdinals(
		secondLayout.Carrier(), []int{0}))
	firstField, ok := values.AsFieldValue(firstID)
	if !ok {
		t.Fatal("first scan ID is not an exact FieldValue")
	}
	secondField, ok := values.AsFieldValue(secondID)
	if !ok {
		t.Fatal("second scan ID is not an exact FieldValue")
	}
	frontier := values.OrdinalDomainOfType(rowType)
	firstIdentity, ok := values.CorrelatedFieldIdentityIn(firstField, frontier)
	if !ok {
		t.Fatal("first scan ID has no identity in ORDERS")
	}
	secondIdentity, ok := values.CorrelatedFieldIdentityIn(secondField, frontier)
	if !ok {
		t.Fatal("second scan ID has no identity in ORDERS")
	}
	innerAlias := values.NamedCorrelationIdentifier("O")
	if !residualFieldReadsScanCarrier(
		firstField, firstIdentity, innerAlias, firstLayout.Carrier()) {
		t.Fatal("the scan's exact current carrier was rejected")
	}
	if residualFieldReadsScanCarrier(
		secondField, secondIdentity, innerAlias, firstLayout.Carrier()) {
		t.Fatal("a same-shaped current carrier from another scan was accepted")
	}
}

func residualPointProbe(
	t *testing.T,
	physicalType values.Type,
	rhs values.Value,
) (*plans.RecordQueryPredicatesFilterPlan, *plans.RecordQueryScanPlan, PlanContext) {
	t.Helper()
	layout := values.NewRecordType("R", false, []values.Field{{
		Name: "K", FieldType: physicalType, Ordinal: 0,
	}})
	alias := values.NamedCorrelationIdentifier("r")
	root := mustPointProbeConstruct(values.NewQuantifiedObjectValue(alias, layout))
	key := mustPointProbeConstruct(values.ResolveFieldOrdinals(root, []int{0}))
	scan := mustPointProbeConstruct(plans.NewRecordQueryScanPlan([]string{"R"}, layout, false)).
		WithKeyComponentTypes([]values.Type{physicalType})
	filter := mustPointProbeConstruct(plans.NewRecordQueryPredicatesFilterPlanWithAlias(
		scan,
		[]predicates.QueryPredicate{predicates.NewComparisonPredicate(key, predicates.Comparison{
			Type: predicates.ComparisonEquals, Operand: rhs,
		})},
		alias,
	))
	return filter, scan, &pointProbePKCtx{pk: []string{"K"}}
}

// Residual predicates do not execute the exact-or-loud scan binder. Their
// at-most-one proof must therefore classify the complete cmpAny equality
// class over the raw UNIQUE/PK key, including both zero signs, NaN payloads,
// and integer precision-cliff aliases.
func TestPredicatesFilterIsFullPKPointProbe_LogicalEqualityClasses(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name         string
		physicalType values.Type
		rhs          values.Value
		want         bool
	}{
		{name: "DOUBLE positive zero", physicalType: values.NotNullDouble, rhs: pointProbeLiteral(float64(0))},
		{name: "DOUBLE negative zero", physicalType: values.NotNullDouble, rhs: pointProbeLiteral(math.Copysign(0, -1))},
		{name: "FLOAT zero", physicalType: values.NotNullFloat, rhs: pointProbeLiteral(float32(0))},
		{name: "DOUBLE NaN", physicalType: values.NotNullDouble, rhs: pointProbeLiteral(math.NaN())},
		{name: "dynamic DOUBLE", physicalType: values.NotNullDouble, rhs: &values.ParameterValue{Ordinal: 1, Typ: values.NotNullDouble}},
		{name: "LONG dynamic DOUBLE", physicalType: values.NotNullLong, rhs: &values.ParameterValue{Ordinal: 1, Typ: values.NotNullDouble}},
		{name: "LONG precision cliff", physicalType: values.NotNullLong, rhs: pointProbeLiteral(float64(1 << 53))},
		{name: "DOUBLE finite nonzero", physicalType: values.NotNullDouble, rhs: pointProbeLiteral(float64(5)), want: true},
		{name: "LONG integer", physicalType: values.NotNullLong, rhs: pointProbeLiteral(int64(5)), want: true},
		{name: "LONG exactly represented float", physicalType: values.NotNullLong, rhs: pointProbeLiteral(float64(5)), want: true},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			filter, scan, ctx := residualPointProbe(t, test.physicalType, test.rhs)
			got := predicatesFilterIsFullPKPointProbe(filter, scan, ctx)
			if got != test.want {
				t.Fatalf("predicatesFilterIsFullPKPointProbe = %v, want %v", got, test.want)
			}
			counts := findExpressionsByType(filter, nil, ctx)
			if test.want {
				if counts.unboundedDataAccess || counts.maxDataAccessCardinality != 1 {
					t.Fatalf("proven residual point counts = {unbounded:%v max:%v}, want {false 1}", counts.unboundedDataAccess, counts.maxDataAccessCardinality)
				}
			} else if !counts.unboundedDataAccess || counts.maxDataAccessCardinality != -1 {
				t.Fatalf("unproven residual access counts = {unbounded:%v max:%v}, want {true -1}", counts.unboundedDataAccess, counts.maxDataAccessCardinality)
			}
		})
	}
}

// A residual equality only acts like a probe when its comparand is constant
// for the whole scan. In particular, `K = K` and `K = OTHER_COLUMN` can match
// every row even though K is a primary key; raw key uniqueness says nothing
// about a value chosen afresh from each row.
func TestPredicatesFilterIsFullPKPointProbe_ComparandMustBeRowInvariant(t *testing.T) {
	t.Parallel()
	base, scan, ctx := residualPointProbe(t, values.NotNullLong, pointProbeLiteral(int64(5)))
	key := base.GetPredicates()[0].(*predicates.ComparisonPredicate).Operand
	innerAlias := base.GetInnerAlias()
	layout := scan.GetResultType()
	innerRoot := mustPointProbeConstruct(values.NewQuantifiedObjectValue(innerAlias, layout))
	innerKey := mustPointProbeConstruct(values.ResolveFieldOrdinals(innerRoot, []int{0}))
	outerRoot := mustPointProbeConstruct(values.NewQuantifiedObjectValue(
		values.NamedCorrelationIdentifier("outer"), layout))
	outerKey := mustPointProbeConstruct(values.ResolveFieldOrdinals(outerRoot, []int{0}))

	for _, test := range []struct {
		name string
		rhs  values.Value
		want bool
	}{
		{name: "literal", rhs: pointProbeLiteral(int64(5)), want: true},
		{name: "parameter", rhs: &values.ParameterValue{Ordinal: 1, Typ: values.NotNullLong}, want: true},
		{name: "outer correlated field", rhs: outerKey, want: true},
		{name: "explicit current-row field", rhs: innerKey},
	} {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			filter := mustPointProbeConstruct(plans.NewRecordQueryPredicatesFilterPlanWithAlias(
				scan,
				[]predicates.QueryPredicate{predicates.NewComparisonPredicate(
					key, predicates.Comparison{Type: predicates.ComparisonEquals, Operand: test.rhs},
				)},
				innerAlias,
			))
			if got := predicatesFilterIsFullPKPointProbe(filter, scan, ctx); got != test.want {
				t.Fatalf("predicatesFilterIsFullPKPointProbe = %v, want %v", got, test.want)
			}
			counts := findExpressionsByType(filter, nil, ctx)
			if test.want {
				if counts.unboundedDataAccess || counts.maxDataAccessCardinality != 1 {
					t.Fatalf("proven residual point counts = {unbounded:%v max:%v}, want {false 1}", counts.unboundedDataAccess, counts.maxDataAccessCardinality)
				}
			} else if !counts.unboundedDataAccess || counts.maxDataAccessCardinality != -1 {
				t.Fatalf("unproven residual access counts = {unbounded:%v max:%v}, want {true -1}", counts.unboundedDataAccess, counts.maxDataAccessCardinality)
			}
		})
	}
}

func TestPredicatesFilterIsFullPKPointProbe_PhysicalTypeAlignment(t *testing.T) {
	t.Parallel()
	filter, baseScan, ctx := residualPointProbe(
		t, values.NotNullDouble,
		pointProbeLiteral(int64(0)),
	)
	key := filter.GetPredicates()[0].(*predicates.ComparisonPredicate).Operand
	prefixedPK := []values.Value{
		values.NewRecordTypeValue(nil), key,
	}
	for _, test := range []struct {
		name      string
		types     []values.Type
		wantProof bool
	}{
		{name: "full-coordinate DOUBLE is not shifted onto prefix", types: []values.Type{values.NotNullLong, values.NotNullDouble}},
		{name: "visible-coordinate DOUBLE remains authoritative", types: []values.Type{values.NotNullDouble}},
		{name: "ambiguous arity declines", types: []values.Type{values.NotNullLong, values.NotNullLong, values.NotNullDouble}},
	} {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			scan := baseScan.WithPrimaryKey(prefixedPK).WithKeyComponentTypes(test.types)
			rebound := mustPointProbeConstruct(plans.NewRecordQueryPredicatesFilterPlanWithAlias(
				scan, filter.GetPredicates(), filter.GetInnerAlias(),
			))
			if got := predicatesFilterIsFullPKPointProbe(rebound, scan, ctx); got != test.wantProof {
				t.Fatalf("predicatesFilterIsFullPKPointProbe = %v, want %v", got, test.wantProof)
			}
		})
	}

	// A statically non-zero DOUBLE proves the positive direction even when the
	// structural RecordTypeValue occupies a full physical coordinate.
	nonzeroFilter, _, _ := residualPointProbe(t, values.NotNullDouble, pointProbeLiteral(float64(5)))
	nonzeroScan := baseScan.WithPrimaryKey(prefixedPK).
		WithKeyComponentTypes([]values.Type{values.NotNullLong, values.NotNullDouble})
	nonzero := mustPointProbeConstruct(plans.NewRecordQueryPredicatesFilterPlanWithAlias(
		nonzeroScan, nonzeroFilter.GetPredicates(), nonzeroFilter.GetInnerAlias(),
	))
	if !predicatesFilterIsFullPKPointProbe(nonzero, nonzeroScan, ctx) {
		t.Fatal("full-coordinate nonzero DOUBLE equality lost its valid at-most-one proof")
	}
}

// residualPrimaryKeyPhysicalTypes consumes two ordered translations of one
// primary-key expression. It must preserve their coordinates in primary-key
// component order. RFC-232 exact record layouts reject duplicate semantic
// names at their boundary, so the former duplicate-display-name adversary is
// no longer a constructible Value; reversed type order remains the positional
// discriminator.
func TestResidualPrimaryKeyPhysicalTypes_StayPositional(t *testing.T) {
	t.Parallel()
	layout := values.NewRecordType("PositionalPhysicalTypes", false, []values.Field{
		{Name: "DOUBLE_KEY", FieldType: values.NotNullDouble, Ordinal: 0},
		{Name: "FLOAT_KEY", FieldType: values.NotNullFloat, Ordinal: 1},
	})
	root := mustPointProbeConstruct(values.NewQuantifiedObjectValue(
		values.NamedCorrelationIdentifier("positional_physical"), layout))
	pk := []values.Value{
		mustPointProbeConstruct(values.ResolveFieldOrdinals(root, []int{1})),
		mustPointProbeConstruct(values.ResolveFieldOrdinals(root, []int{0})),
	}
	scan := mustPointProbeConstruct(plans.NewRecordQueryScanPlan([]string{"T"}, layout, false)).
		WithPrimaryKey(pk).
		WithKeyComponentTypes([]values.Type{values.NotNullFloat, values.NotNullDouble})
	got, ok := residualPrimaryKeyPhysicalTypes(scan, 2)
	if !ok {
		t.Fatal("coordinate-aligned primary-key fields were declined")
	}
	if len(got) != 2 || got[0] != values.NotNullFloat || got[1] != values.NotNullDouble {
		t.Fatalf("physical types = %v, want [FLOAT DOUBLE] in PK-component order", got)
	}

	prefixed := scan.WithPrimaryKey(append([]values.Value{values.NewRecordTypeValue(nil)}, pk...)).
		WithKeyComponentTypes([]values.Type{values.NotNullLong, values.NotNullFloat, values.NotNullDouble})
	got, ok = residualPrimaryKeyPhysicalTypes(prefixed, 2)
	if !ok || len(got) != 2 || got[0] != values.NotNullFloat || got[1] != values.NotNullDouble {
		t.Fatalf("record-type-prefixed physical types = %v ok=%v, want [FLOAT DOUBLE]", got, ok)
	}
}
