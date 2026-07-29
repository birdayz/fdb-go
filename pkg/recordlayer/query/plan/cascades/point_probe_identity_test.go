package cascades

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
)

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

func pointProbeLayout(name string, cols ...string) *values.RecordType {
	fields := make([]values.Field, len(cols))
	for i, c := range cols {
		fields[i] = values.Field{Name: c, FieldType: values.UnknownType, Ordinal: i}
	}
	return values.NewRecordType(name, false, fields)
}

// pointProbeRef builds a resolved column reference the way the SQL resolver
// does: the ordinal the column has in rt's declared order, stamped with rt's
// domain. corr is the quantifier whose row it reads; the zero identifier makes
// it CHILDLESS, which by the ColumnIdentity contract means "this value's own
// source" — the shape most filter operands really have.
func pointProbeRef(t *testing.T, rt string, corr values.CorrelationIdentifier, field string) *values.FieldValue {
	t.Helper()
	layout, ok := pointProbeIdentityLayouts[rt]
	if !ok {
		t.Fatalf("setup: no layout registered for %q", rt)
	}
	ord, found := layout.FieldIndex(field)
	if !found {
		t.Fatalf("setup: %s does not declare %q", rt, field)
	}
	domain := values.OrdinalDomainOfType(layout)
	if corr.IsZero() {
		return values.NewFieldValueWithResolvedOrdinalInDomain(field, ord, values.UnknownType, domain)
	}
	return values.NewCorrelatedFieldValueWithResolvedOrdinalInDomain(
		values.NewQuantifiedObjectValue(corr), field, ord, values.UnknownType, domain)
}

func pointProbeEqFilter(
	t *testing.T,
	rt string,
	innerAlias values.CorrelationIdentifier,
	operands ...*values.FieldValue,
) (*plans.RecordQueryPredicatesFilterPlan, *plans.RecordQueryScanPlan) {
	t.Helper()
	layout, ok := pointProbeIdentityLayouts[rt]
	if !ok {
		t.Fatalf("setup: no layout registered for %q", rt)
	}
	scan := plans.NewRecordQueryScanPlan([]string{rt}, layout, false)
	preds := make([]predicates.QueryPredicate, 0, len(operands))
	for _, op := range operands {
		preds = append(preds, predicates.NewComparisonPredicate(op,
			predicates.NewLiteralComparison(predicates.ComparisonEquals, int64(1))))
	}
	return plans.NewRecordQueryPredicatesFilterPlanWithAlias(scan, preds, innerAlias), scan
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
	ctx := &pkGateTestCtx{pk: []string{"ID"}}

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

	t.Run("a CHILDLESS own-row reference binds it", func(t *testing.T) {
		t.Parallel()
		// The majority shape in the corpus: no QOV child, so the zero
		// correlation, which the ColumnIdentity contract reads as "this value's
		// own source". It must still count, or the bound disappears for most
		// real plans.
		childless := pointProbeRef(t, "ORDERS", values.CorrelationIdentifier{}, "ID")
		filter, scan := pointProbeEqFilter(t, "ORDERS", ordersAlias, childless)
		if !predicatesFilterIsFullPKPointProbe(filter, scan, ctx) {
			t.Fatal("a childless own-row `ID = ?` equality was not recognised as a point probe — " +
				"the zero correlation means the value's own source, not `some other leg`")
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
		compositeCtx := &pkGateTestCtx{pk: []string{"TENANT", "LINE_NO"}}
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

	t.Run("a scan with no declared column order fails closed", func(t *testing.T) {
		t.Parallel()
		// No layout means no domain to state the proof in. The probe declines
		// rather than falling back to the names the operands still carry.
		untyped := plans.NewRecordQueryScanPlan([]string{"ORDERS"}, values.UnknownType, false)
		ownID := pointProbeRef(t, "ORDERS", ordersAlias, "ID")
		filter := plans.NewRecordQueryPredicatesFilterPlanWithAlias(untyped,
			[]predicates.QueryPredicate{predicates.NewComparisonPredicate(ownID,
				predicates.NewLiteralComparison(predicates.ComparisonEquals, int64(1)))},
			ordersAlias)
		if predicatesFilterIsFullPKPointProbe(filter, untyped, ctx) {
			t.Fatal("a scan with no declared column order was declared a point probe — " +
				"there was no layout to resolve the primary key's name against")
		}
	})
}
