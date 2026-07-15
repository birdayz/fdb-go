package executor

import (
	"context"
	"strings"
	"testing"

	"google.golang.org/protobuf/proto"

	"fdb.dev/gen"
	"fdb.dev/pkg/recordlayer"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
)

// This file pins the FlatMap executor binder for a birth-disabled join
// (identity result value, no ordinal state of its own): newFlatMapCursor
// probes the inner plan for FrontierPinned baked references over the OUTER
// alias and, on a hit, binds the outer row positionally (adaptLegPositional,
// LOUD on adaptation failure — never a silent Datum fallback). It also pins
// the identity-FlatMap positional pass-through (the outer's positional row
// survives the WHERE-EXISTS FlatMap boundary instead of dying there), and the
// downstreamLegWindows unwrap that keeps consumers above the identity FlatMap
// resolving against their leg windows.

// existsCustomerType is the Customer record's positional shape — one field per
// descriptor field in declaration order, UPPER names (protoToPositional's
// contract) — the type the planner's seed-constructed QOV over a Customer
// quantifier carries.
func existsCustomerType() *values.RecordType {
	return values.NewRecordType("", false, []values.Field{
		{Name: "CUSTOMER_ID", FieldType: values.NotNullLong, Ordinal: 0},
		{Name: "NAME", FieldType: values.NotNullString, Ordinal: 1},
		{Name: "EMAIL", FieldType: values.NotNullString, Ordinal: 2},
		{Name: "PRICE", FieldType: values.NotNullInt, Ordinal: 3},
	})
}

// buildCorrelatedExistsShape builds a WHERE-EXISTS shape: an identity-RV
// FlatMap (pass-through) over a Customer scan whose INNER plan filters Orders
// by PRICE = <outer reference to the customer's PRICE, ordinal 3>. The outer
// reference is either BAKED (FrontierPinned ofOrdinal) or LAZY (name-model),
// per the baked flag. PRICE is the join column (not the ids) so the two
// record types keep DISJOINT primary keys — record types share one
// primary-key space, and a colliding id would silently overwrite the customer
// with the order.
func buildCorrelatedExistsShape(t *testing.T, baked bool) (plans.RecordQueryPlan, values.CorrelationIdentifier) {
	t.Helper()
	custCorr := values.NamedCorrelationIdentifier("CUST")
	qovCust := values.NewQuantifiedObjectValueOfType(custCorr, existsCustomerType())

	var outerRef values.Value
	if baked {
		b, err := values.NewFieldValueOfOrdinal(qovCust, 3)
		if err != nil {
			t.Fatalf("NewFieldValueOfOrdinal: %v", err)
		}
		outerRef = b
	} else {
		outerRef = values.NewCorrelatedFieldValueWithResolvedOrdinal(qovCust, "PRICE", 3, values.NotNullInt)
	}

	innerPlan := plans.NewRecordQueryPredicatesFilterPlan(
		plans.NewRecordQueryScanPlan([]string{"Order"}, nil, false),
		[]predicates.QueryPredicate{ojEqPred(values.NewFieldValueWithResolvedOrdinal("PRICE", 2, values.NotNullInt), outerRef)},
	)
	outerScan := plans.NewRecordQueryScanPlan([]string{"Customer"}, nil, false)
	fm := plans.NewRecordQueryFlatMapPlan(
		outerScan, innerPlan,
		custCorr, values.NamedCorrelationIdentifier("ORD"),
		qovCust, false,
	)
	return fm, custCorr
}

// assertCustomerPositional asserts the identity pass-through: the row carries
// the Customer-shaped positional row with the expected id and name (the PRICE
// slot's Go type follows the proto conversion pipeline and is asserted
// non-nil only).
func assertCustomerPositional(t *testing.T, qr QueryResult, wantID int64, wantName string) {
	t.Helper()
	pos := qr.Positional
	if pos == nil {
		t.Fatal("row carries no positional row — the identity pass-through must flow the outer's row across the FlatMap boundary")
	}
	if len(pos.Slots) != 4 {
		t.Fatalf("positional row has %d slots, want 4 (the Customer shape)", len(pos.Slots))
	}
	if v, ok := pos.Get(0); !ok || v != wantID {
		t.Fatalf("slot 0 = (%v, %v), want (%v, true)", v, ok, wantID)
	}
	if v, ok := pos.Get(1); !ok || v != wantName {
		t.Fatalf("slot 1 = (%v, %v), want (%q, true)", v, ok, wantName)
	}
	if v, ok := pos.Get(3); !ok || v == nil {
		t.Fatalf("slot 3 (PRICE) = (%v, %v), want a non-nil value", v, ok)
	}
}

// runCorrelatedExistsShape executes the shape against a live store and
// returns the emitted rows.
func runCorrelatedExistsShape(t *testing.T, store *recordlayer.FDBRecordStore, fm plans.RecordQueryPlan) []QueryResult {
	t.Helper()
	ctx := context.Background()
	var results []QueryResult
	_, err := testDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
		s, err := recordlayer.NewStoreBuilder().
			SetContext(rtx).SetMetaDataProvider(store.GetMetaData()).
			SetSubspace(testSubspace(t)).Open()
		if err != nil {
			return nil, err
		}
		cursor, err := ExecutePlan(ctx, fm, s, EmptyEvaluationContext(), nil, recordlayer.DefaultExecuteProperties())
		if err != nil {
			return nil, err
		}
		defer cursor.Close()
		results, err = CollectAll(ctx, cursor)
		return nil, err
	})
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	return results
}

// TestIntegration_DisabledBirthBinder_BakedInner pins the executor binder: a
// birth-disabled FlatMap (identity RV — no ordinal state of its own) whose
// inner plan carries a BAKED FrontierPinned reference to the outer alias must
// bind the outer row POSITIONALLY, so the baked reference resolves by
// ordinal — binding it as a name-keyed Datum map would leave the baked
// reference unable to resolve and fail with a loud BakedNameContextError on
// every row. The emitted rows also pin the identity pass-through: the
// identity output carries the outer's positional row across the FlatMap
// boundary.
func TestIntegration_DisabledBirthBinder_BakedInner(t *testing.T) {
	t.Parallel()
	store := setupStore(t)
	insertCustomers(t, store,
		&gen.Customer{CustomerId: proto.Int64(1), Name: proto.String("Alice"), Price: proto.Int32(10)},
		&gen.Customer{CustomerId: proto.Int64(2), Name: proto.String("Bob"), Price: proto.Int32(20)},
	)
	insertOrders(t, store, &gen.Order{OrderId: proto.Int64(9001), Price: proto.Int32(10)})

	fm, _ := buildCorrelatedExistsShape(t, true)
	results := runCorrelatedExistsShape(t, store, fm)

	if len(results) != 1 {
		t.Fatalf("got %d rows, want 1 (only Alice's price matches an order)", len(results))
	}
	datum, isMap := rowMapOK(results[0])
	if !isMap || datum["CUSTOMER_ID"] != int64(1) {
		t.Fatalf("row = %v, want the outer row for customer 1", results[0].Positional)
	}
	// The identity output IS the bare Customer row (no leg windows), so the
	// column reads by its own name. (The alias-qualified "CUST.NAME" spelling
	// this pin previously used resolved only through the deleted
	// unique-leaf-strip heuristic; a qualified read over a window-less row is
	// not a real runtime form.)
	if v := rowVal(results[0], "NAME"); v != "Alice" {
		t.Fatalf("NAME = %v, want Alice (the bare Customer row)", v)
	}
	// The identity pass-through: the outer's positional row survives the boundary.
	assertCustomerPositional(t, results[0], int64(1), "Alice")
}

// TestIntegration_ProbeNegative_LazyInner pins the probe-negative side: a LAZY
// (non-FrontierPinned) outer reference in the inner plan leaves the baked
// probe cold (outerBakedType nil), so the outer is bound POSITIONALLY as the
// plain-scan correlated-EXISTS shape (outerIdentityPassthrough) rather than by
// the baked-probe path. The identity output PROPAGATES the outer's positional
// row, stamped with the outer alias as a leg window (qualifyOuterPositional).
// That row resolves both a bare column (FieldIndex) and an alias-qualified
// reference (the Legs path), so a downstream projection reading "CUST.NAME"
// no longer loud-misses a bare-named row (the former
// TestFDB_CorrelatedExistsCrossJoin catch that once forced this edge to stay
// Datum-only).
func TestIntegration_ProbeNegative_LazyInner(t *testing.T) {
	t.Parallel()
	store := setupStore(t)
	insertCustomers(t, store,
		&gen.Customer{CustomerId: proto.Int64(1), Name: proto.String("Alice"), Price: proto.Int32(10)},
		&gen.Customer{CustomerId: proto.Int64(2), Name: proto.String("Bob"), Price: proto.Int32(20)},
	)
	insertOrders(t, store, &gen.Order{OrderId: proto.Int64(9001), Price: proto.Int32(10)})

	fm, _ := buildCorrelatedExistsShape(t, false)
	results := runCorrelatedExistsShape(t, store, fm)

	if len(results) != 1 {
		t.Fatalf("got %d rows, want 1 (only Alice's price matches an order)", len(results))
	}
	datum, isMap := rowMapOK(results[0])
	if !isMap || datum["CUSTOMER_ID"] != int64(1) {
		t.Fatalf("row = %v, want the outer row for customer 1", results[0].Positional)
	}
	// The outer's positional row survives, and its outer-alias leg window
	// resolves the alias-qualified read the same as the bare column.
	assertCustomerPositional(t, results[0], int64(1), "Alice")
	if v, ok := legRead(results[0].Positional, "CUST", "NAME"); !ok || v != "Alice" {
		t.Fatalf("legRead(CUST.NAME) = (%v, %v), want (Alice, true) — the outer-alias leg window", v, ok)
	}
}

// TestLoudAdaptationFailure pins that a probe-POSITIVE cursor whose outer row
// cannot adapt positionally (a merge-shaped name-model row — dotted keys, no
// positional) errors LOUDLY at the binding site (adaptLegPositional's
// zero-match tripwire), never a silent Datum fallback that would feed the
// baked references a name-keyed context.
func TestLoudAdaptationFailure(t *testing.T) {
	t.Parallel()
	legA, _, qovA, _, _ := ojWiringLegs(t)

	bakedAID, err := values.NewFieldValueOfOrdinal(qovA, 0)
	if err != nil {
		t.Fatalf("NewFieldValueOfOrdinal: %v", err)
	}
	innerPlan := plans.NewRecordQueryPredicatesFilterPlan(
		plans.NewRecordQueryScanPlan(nil, values.UnknownType, false),
		[]predicates.QueryPredicate{ojEqPred(bakedAID, &values.ConstantValue{Value: int64(1), Typ: values.NotNullLong})},
	)
	// The outer row is merge-shaped: dotted keys only, no positional row —
	// none of leg A's bare column names match.
	outer := recordlayer.FromList([]QueryResult{
		dmap(map[string]any{"A.ID": int64(1), "A.V": int64(10)}),
	})
	c, err := newFlatMapCursor(outer, nil, innerPlan, nil, EmptyEvaluationContext(),
		qovA.Correlation, values.NamedCorrelationIdentifier("B"),
		values.NewQuantifiedObjectValue(qovA.Correlation), false, recordlayer.ExecuteProperties{})
	if err != nil {
		t.Fatalf("newFlatMapCursor: %v", err)
	}
	defer c.Close()
	if c.outerBakedType == nil {
		t.Fatal("probe must be POSITIVE — the inner plan carries a baked FrontierPinned reference over the outer alias")
	}
	if got := len(c.outerBakedType.Fields); got != len(legA.Fields) {
		t.Fatalf("probed outer type has %d fields, want %d", got, len(legA.Fields))
	}
	_, onErr := c.OnNext(context.Background())
	if onErr == nil || !strings.Contains(onErr.Error(), "leg adapter") {
		t.Fatalf("OnNext = %v, want the loud leg-adapter zero-match error, never a silent Datum fallback", onErr)
	}
}

// TestProbeOuterBakedType pins the probe itself: positive on a baked
// FrontierPinned reference over the outer alias (PredicatesFilter surface),
// negative for other aliases, for lazy references, and for a nil plan; and
// the width-divergence panic on conflicting baked types (the same one-seed
// invariant widenLegTypesFromPlan asserts).
func TestProbeOuterBakedType(t *testing.T) {
	t.Parallel()
	legA, _, qovA, qovB, _ := ojWiringLegs(t)

	bakedAID, err := values.NewFieldValueOfOrdinal(qovA, 0)
	if err != nil {
		t.Fatalf("NewFieldValueOfOrdinal: %v", err)
	}
	inner := plans.NewRecordQueryPredicatesFilterPlan(
		plans.NewRecordQueryScanPlan(nil, values.UnknownType, false),
		[]predicates.QueryPredicate{ojEqPred(bakedAID, &values.ConstantValue{Value: int64(1), Typ: values.NotNullLong})},
	)

	t.Run("positive over the outer alias", func(t *testing.T) {
		t.Parallel()
		rt := probeOuterBakedType(inner, qovA.Correlation)
		if rt == nil || len(rt.Fields) != len(legA.Fields) {
			t.Fatalf("probe = %v, want leg A's %d-field type", rt, len(legA.Fields))
		}
	})
	t.Run("negative for another alias", func(t *testing.T) {
		t.Parallel()
		if rt := probeOuterBakedType(inner, qovB.Correlation); rt != nil {
			t.Fatalf("probe over B = %v, want nil (the baked reference is over A)", rt)
		}
	})
	t.Run("negative for lazy references", func(t *testing.T) {
		t.Parallel()
		lazyInner := plans.NewRecordQueryPredicatesFilterPlan(
			plans.NewRecordQueryScanPlan(nil, values.UnknownType, false),
			[]predicates.QueryPredicate{ojEqPred(
				values.NewFieldValue(qovA, "ID", values.NotNullLong),
				&values.ConstantValue{Value: int64(1), Typ: values.NotNullLong},
			)},
		)
		if rt := probeOuterBakedType(lazyInner, qovA.Correlation); rt != nil {
			t.Fatalf("probe over lazy references = %v, want nil (FrontierPinned only)", rt)
		}
	})
	t.Run("negative for a nil plan", func(t *testing.T) {
		t.Parallel()
		if rt := probeOuterBakedType(nil, qovA.Correlation); rt != nil {
			t.Fatalf("probe over nil plan = %v, want nil", rt)
		}
	})
	t.Run("width divergence panics", func(t *testing.T) {
		t.Parallel()
		wideA := values.NewQuantifiedObjectValueOfType(qovA.Correlation, values.NewRecordType("", false, []values.Field{
			{Name: "ID", FieldType: values.NotNullLong, Ordinal: 0},
			{Name: "V", FieldType: values.NotNullLong, Ordinal: 1},
			{Name: "X", FieldType: values.NotNullLong, Ordinal: 2},
		}))
		bakedWide, werr := values.NewFieldValueOfOrdinal(wideA, 0)
		if werr != nil {
			t.Fatalf("NewFieldValueOfOrdinal: %v", werr)
		}
		divergent := plans.NewRecordQueryPredicatesFilterPlan(
			plans.NewRecordQueryScanPlan(nil, values.UnknownType, false),
			[]predicates.QueryPredicate{
				ojEqPred(bakedAID, &values.ConstantValue{Value: int64(1), Typ: values.NotNullLong}),
				ojEqPred(bakedWide, &values.ConstantValue{Value: int64(2), Typ: values.NotNullLong}),
			},
		)
		defer func() {
			if r := recover(); r == nil {
				t.Fatal("divergent baked widths over one alias must panic (planner bug — one seed-constructed QOV)")
			}
		}()
		probeOuterBakedType(divergent, qovA.Correlation)
	})
}

// TestComputeResult_PassThrough pins the identity pass-through at the
// computeResult level: a PROBE-POSITIVE identity-over-outer FlatMap emits the
// outer's positional row on the output verbatim (same row object —
// propagation, not a rebuild), including through a nil inner row. A
// mismatched (index-shaped) outer layout must instead be RE-ADAPTED to the
// baked type, never passed through as-is (a silent wrong-slot read
// downstream). A PROBE-NEGATIVE identity cursor still propagates the outer's
// positional row, stamped with the outer alias as a leg window, so both a
// bare and an alias-qualified read resolve off it.
func TestComputeResult_PassThrough(t *testing.T) {
	t.Parallel()
	legA, legB, qovA, qovB, _ := ojWiringLegs(t)
	// The probe-POSITIVE inner plan: a baked FrontierPinned reference over
	// the outer alias inside a residual filter.
	bakedAID, err := values.NewFieldValueOfOrdinal(qovA, 0)
	if err != nil {
		t.Fatalf("NewFieldValueOfOrdinal: %v", err)
	}
	bakedInner := plans.NewRecordQueryPredicatesFilterPlan(
		plans.NewRecordQueryScanPlan(nil, values.UnknownType, false),
		[]predicates.QueryPredicate{ojEqPred(bakedAID, &values.ConstantValue{Value: int64(1), Typ: values.NotNullLong})},
	)
	newIdentityCursor := func(t *testing.T, innerPlan plans.RecordQueryPlan) *flatMapCursor {
		t.Helper()
		c, err := newFlatMapCursor(nil, nil, innerPlan, nil, EmptyEvaluationContext(),
			qovA.Correlation, qovB.Correlation,
			values.NewQuantifiedObjectValue(qovA.Correlation), false, recordlayer.ExecuteProperties{})
		if err != nil {
			t.Fatalf("newFlatMapCursor: %v", err)
		}
		return c
	}
	outerQR := ojLegQR(t, legA, int64(1), int64(10))
	innerQR := ojNameQR(legB, int64(2), int64(20))

	t.Run("positional outer passes through", func(t *testing.T) {
		t.Parallel()
		c := newIdentityCursor(t, bakedInner)
		got, err := c.computeResult(outerQR, innerQR)
		if err != nil {
			t.Fatalf("computeResult: %v", err)
		}
		if got.Positional != outerQR.Positional {
			t.Fatalf("output Positional = %p, want the outer's own row %p (verbatim pass-through)", got.Positional, outerQR.Positional)
		}
	})
	t.Run("nil inner passes through", func(t *testing.T) {
		t.Parallel()
		c := newIdentityCursor(t, bakedInner)
		got, err := c.computeResultLegs(outerQR, nil)
		if err != nil {
			t.Fatalf("computeResultLegs: %v", err)
		}
		if got.Positional != outerQR.Positional {
			t.Fatal("nil-inner identity emission must pass the outer positional through")
		}
	})
	t.Run("mismatched outer layout re-adapts to the baked type", func(t *testing.T) {
		t.Parallel()
		// A probe-positive cursor whose outer positional row is INDEX-shaped
		// ([V, ID] — a covering row) while the baked QOV expects table order
		// ([ID, V]). The binding arm adapts correctly, but a pass-through
		// publishing the ORIGINAL mismatched row hands downstream baked
		// ordinals the wrong layout — a silent wrong-slot read (V where ID
		// was baked). The pass-through must publish the ADAPTED row.
		coveringType := values.NewRecordType("", false, []values.Field{
			{Name: "V", FieldType: values.NotNullLong, Ordinal: 0},
			{Name: "ID", FieldType: values.NotNullLong, Ordinal: 1},
		})
		covering := NewPositionalRow(coveringType)
		covering.Set(0, int64(10)) // V
		covering.Set(1, int64(1))  // ID
		outer := QueryResult{Positional: covering}
		c := newIdentityCursor(t, bakedInner)
		got, err := c.computeResult(outer, innerQR)
		if err != nil {
			t.Fatalf("computeResult: %v", err)
		}
		if got.Positional == nil {
			t.Fatal("mismatched-layout outer must still emit (adapted), not drop the positional")
		}
		if got.Positional == covering {
			t.Fatal("pass-through published the ORIGINAL index-shaped row — downstream baked ordinals would read the wrong slot")
		}
		// The adapted row is in the BAKED layout: slot 0 = ID, slot 1 = V.
		ojAssertSlots(t, got.Positional, int64(1), int64(10))
	})
	t.Run("probe-negative identity emits an alias-qualified positional", func(t *testing.T) {
		t.Parallel()
		// The PLAIN-SCAN correlated-EXISTS output edge: a probe-negative
		// identity pass-through (outerBakedType nil, outerMergedType nil →
		// outerIdentityPassthrough) over an outer that CARRIES a positional
		// row now PROPAGATES it, stamped with the outer alias as a leg window
		// (qualifyOuterPositional). This lets the downstream projection read
		// by ordinal instead of by name, while STILL resolving an
		// alias-qualified reference ("A.ID"), the read form that once forced
		// this edge to stay Datum-only (the TestFDB_CorrelatedExistsCrossJoin
		// catch).
		c := newIdentityCursor(t, nil)
		if c.outerBakedType != nil {
			t.Fatal("nil inner plan must probe negative")
		}
		if !c.outerIdentityPassthrough {
			t.Fatal("a probe-negative identity RV over a plain outer must be an identity pass-through")
		}
		got, err := c.computeResult(outerQR, innerQR)
		if err != nil {
			t.Fatalf("computeResult: %v", err)
		}
		if got.Positional == nil {
			t.Fatal("probe-negative identity output must propagate the outer's positional row")
		}
		// The row resolves BOTH a bare column (its plan-time slot) and an
		// alias-qualified reference (a baked QOV(A).col through the row's leg
		// windows) off the same slots.
		if v, ok := getByName(got.Positional, "ID"); !ok || v != int64(1) {
			t.Fatalf("ID = (%v, %v), want (1, true)", v, ok)
		}
		if v, ok := getByName(got.Positional, "V"); !ok || v != int64(10) {
			t.Fatalf("V = (%v, %v), want (10, true)", v, ok)
		}
		if v, ok := legRead(got.Positional, "A", "ID"); !ok || v != int64(1) {
			t.Fatalf("legRead(A.ID) = (%v, %v), want (1, true) — the outer-alias leg window", v, ok)
		}
		if v, ok := legRead(got.Positional, "A", "V"); !ok || v != int64(10) {
			t.Fatalf("legRead(A.V) = (%v, %v), want (10, true) — the outer-alias leg window", v, ok)
		}
	})
}

// TestUnwrapIdentityFlatMap pins the downstreamLegWindows companion of the
// identity pass-through: an identity-over-OUTER FlatMap is a row-preserving
// passthrough toward its outer's join (the cursor re-emits the outer row,
// positional included), so consumers above it keep the OUTER join's leg
// windows. An inner-identity RV, a FlatMap over a non-join outer, and the
// seed-RV FlatMap (its own authority) pin the arm's exact bounds.
func TestUnwrapIdentityFlatMap(t *testing.T) {
	t.Parallel()
	_, _, qovA, qovB, seed := ojWiringLegs(t)
	nlj := plans.NewRecordQueryNestedLoopJoinPlan(nil, nil, nil, plans.JoinInner, "A", "B", seed)
	corrM := values.NamedCorrelationIdentifier("M")
	corrX := values.NamedCorrelationIdentifier("X")

	t.Run("identity FlatMap unwraps to the outer join", func(t *testing.T) {
		t.Parallel()
		fm := plans.NewRecordQueryFlatMapPlan(nlj, nil, corrM, corrX, values.NewQuantifiedObjectValue(corrM), false)
		spans, ok := downstreamLegWindows(fm)
		if !ok {
			t.Fatal("identity FlatMap over a seed NLJ must unwrap to the join's windows")
		}
		if len(spans) != 2 || spans[0].Offset != 0 || spans[0].Width != 2 || spans[1].Offset != 2 || spans[1].Width != 2 {
			t.Fatalf("spans = %+v, want offsets 0/2 widths 2/2", spans)
		}
	})
	t.Run("wrapped identity FlatMap unwraps transitively", func(t *testing.T) {
		t.Parallel()
		fm := plans.NewRecordQueryFlatMapPlan(nlj, nil, corrM, corrX, values.NewQuantifiedObjectValue(corrM), false)
		if _, ok := downstreamLegWindows(plans.NewRecordQueryLimitPlan(fm, 5, 0)); !ok {
			t.Fatal("limit(identityFlatMap(nlj)) must unwrap to the join's windows")
		}
	})
	t.Run("inner-identity RV is not a passthrough", func(t *testing.T) {
		t.Parallel()
		fm := plans.NewRecordQueryFlatMapPlan(nlj, nil, corrM, corrX, values.NewQuantifiedObjectValue(corrX), false)
		if _, ok := downstreamLegWindows(fm); ok {
			t.Fatal("an INNER-identity FlatMap re-emits inner rows — it must not unwrap to the outer's windows")
		}
	})
	t.Run("identity FlatMap over a non-join declines", func(t *testing.T) {
		t.Parallel()
		fm := plans.NewRecordQueryFlatMapPlan(
			plans.NewRecordQueryScanPlan(nil, values.UnknownType, false), nil,
			corrM, corrX, values.NewQuantifiedObjectValue(corrM), false)
		if _, ok := downstreamLegWindows(fm); ok {
			t.Fatal("identity FlatMap over a scan has no join below — must decline")
		}
	})
	t.Run("seed-RV FlatMap stays its own authority", func(t *testing.T) {
		t.Parallel()
		fm := plans.NewRecordQueryFlatMapPlan(nil, nil, qovA.Correlation, qovB.Correlation, seed, false)
		spans, ok := downstreamLegWindows(fm)
		if !ok || len(spans) != 2 {
			t.Fatalf("seed-RV FlatMap = (%+v, %v), want its own 2 spans (unchanged by the unwrap arm)", spans, ok)
		}
	})
}
