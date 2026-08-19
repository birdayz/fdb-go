package cascades

import (
	"fmt"
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
)

func mustRebaseLayoutConstruct[T any](value T, err error) T {
	if err != nil {
		panic("construct rebase-layout fixture: " + err.Error())
	}
	return value
}

// legRowType is a join leg's own row layout — what its quantifier flows, and
// therefore the domain a leg-relative ordinal indexes.
func legRowType(cols ...string) *values.RecordType {
	fields := make([]values.Field, len(cols))
	for i, c := range cols {
		fields[i] = values.Field{Name: c, FieldType: values.NotNullLong, Ordinal: i}
	}
	return values.NewRecordType("RebaseLegRow", false, fields)
}

// legRead is a BARE column read off a join leg: the leg's quantifier flows the
// leg's row type, and the read is SOURCE-RELATIVE baked at the column's ordinal
// in that type — the resolver's construction bind (expr.go:278-284), which is
// what reaches this rebase. A FrontierPinned bake reaching it is a planner bug
// the rebase asserts on, so the fixture must not build one.
func legRead(leg string, rt *values.RecordType, ordinal int) values.FieldValue {
	qov := mustRebaseLayoutConstruct(values.NewQuantifiedObjectValue(
		values.NamedCorrelationIdentifier(leg), rt))
	resolved := mustRebaseLayoutConstruct(values.ResolveFieldOrdinals(qov, []int{ordinal}))
	field, ok := values.AsFieldValue(resolved)
	if !ok {
		panic(fmt.Sprintf("resolved leg read is %T, not FieldValue", resolved))
	}
	return field
}

// legRC builds the positional RecordConstructorValue concat a FlatMap outer
// carries: each slot is a bare read off one leg, names bare (duplicates across
// legs allowed — the layout is positional, and the whole point is that two
// legs' same-named columns are different columns).
func legRC(reads ...values.FieldValue) *values.RecordConstructorValue {
	fields := make([]values.RecordConstructorField, len(reads))
	for i, r := range reads {
		fields[i] = values.RecordConstructorField{Name: r.DisplayName(), Value: r}
	}
	return values.NewRawRecordConstructorValue(fields...)
}

func fusedLegRead(leg string) values.FieldValue {
	nested := values.NewRecordType("RebaseNested", false, []values.Field{
		{Name: "ID", FieldType: values.NotNullLong, Ordinal: 0},
	})
	rootType := values.NewRecordType("RebaseFusedLeg", false, []values.Field{
		{Name: "ADDRESS", FieldType: nested, Ordinal: 0},
	})
	root := mustRebaseLayoutConstruct(values.NewQuantifiedObjectValue(
		values.NamedCorrelationIdentifier(leg), rootType))
	resolved := mustRebaseLayoutConstruct(values.ResolveFieldOrdinals(root, []int{0, 0}))
	field, ok := values.AsFieldValue(resolved)
	if !ok {
		panic(fmt.Sprintf("resolved fused leg read is %T, not FieldValue", resolved))
	}
	return field
}

func rebaseMergedRowType(arity int) values.Type {
	fields := make([]values.Field, arity)
	for i := range fields {
		fields[i] = values.Field{
			Name: values.OrdinalFieldName(i), FieldType: values.NotNullLong, Ordinal: i,
		}
	}
	return values.NewRecordType("RebaseMergedRow", false, fields)
}

// TestBuriedLegOrdinalLayout pins the COLUMN IDENTITY → global-ordinal
// derivation (WS-N slice 4, keyed by identity since RFC-197 item 3).
func TestBuriedLegOrdinalLayout(t *testing.T) {
	t.Parallel()

	// A(ID, FLAG) ++ B(ID, A_ID). Both legs declare an "ID", which is the whole
	// hazard: the retired key was "CORR.LEAF" built from the display name, and
	// only the alias prefix kept the two IDs apart. The identity keeps them
	// apart by the DOMAIN as well, so a leg that renamed its columns cannot
	// collide with a sibling either.
	aType := legRowType("ID", "FLAG")
	bType := legRowType("ID", "A_ID")
	aID, aFlag := legRead("A", aType, 0), legRead("A", aType, 1)
	bID, bAID := legRead("B", bType, 0), legRead("B", bType, 1)

	scan := mustRebaseLayoutConstruct(plans.NewRecordQueryScanPlan(
		[]string{"T"}, legRowType("SCAN_ID"), false))
	fm := mustRebaseLayoutConstruct(plans.NewRecordQueryFlatMapPlan(
		scan, scan,
		values.NamedCorrelationIdentifier("A"), values.NamedCorrelationIdentifier("B"),
		legRC(aID, aFlag, bID, bAID),
		false,
	))
	layout := buriedLegOrdinalLayout(fm)
	if layout == nil {
		t.Fatal("RC-concat FlatMap outer must derive a layout")
	}
	for _, tc := range []struct {
		read values.FieldValue
		want int
	}{{aID, 0}, {aFlag, 1}, {bID, 2}, {bAID, 3}} {
		id, ok := legSlotIdentity(tc.read)
		if !ok {
			t.Fatalf("a bare leg read must state an identity: %v", tc.read)
		}
		if got, hit := layout[id]; !hit || got != tc.want {
			t.Errorf("layout[%v] = %d (ok=%v), want %d", id, got, hit, tc.want)
		}
	}
	// The two legs' "ID" columns are DIFFERENT entries — same leaf name, same
	// leg-relative ordinal 0, separated only by the correlation. This is the
	// dimension the name-built key could express only through its alias prefix
	// and the reason the map is keyed by the triple.
	aid, _ := legSlotIdentity(aID)
	bid, _ := legSlotIdentity(bID)
	if aid == bid {
		t.Fatal("A.ID and B.ID share one identity — ordinal 0 of two legs are different columns")
	}

	// A fused nested slot states no BARE-leg identity and mints no key, so a
	// layout containing only such slots is no layout at all. RFC-232 no longer
	// admits the former unresolved/lazy FieldValue adversary.
	fusedSlot := fusedLegRead("A")
	fmFused := mustRebaseLayoutConstruct(plans.NewRecordQueryFlatMapPlan(
		scan, scan,
		values.NamedCorrelationIdentifier("A"), values.NamedCorrelationIdentifier("B"),
		values.NewRawRecordConstructorValue(values.RecordConstructorField{Name: "ID", Value: fusedSlot}),
		false,
	))
	if got := buriedLegOrdinalLayout(fmFused); got != nil {
		t.Fatalf("a constructor of fused nested slots states no bare-leg layout, got %v", got)
	}

	// Underivable: a FlatMap whose result value is not an RC concat.
	scalarResult := mustRebaseLayoutConstruct(values.NewQuantifiedObjectValue(
		values.NamedCorrelationIdentifier("A"), values.NotNullLong))
	fmNoRC := mustRebaseLayoutConstruct(plans.NewRecordQueryFlatMapPlan(
		scan, scan,
		values.NamedCorrelationIdentifier("A"), values.NamedCorrelationIdentifier("B"),
		scalarResult,
		false,
	))
	if buriedLegOrdinalLayout(fmNoRC) != nil {
		t.Fatal("non-RC FlatMap outer must not claim a layout")
	}

	// The scan/NLJ-chain shape is no longer derived: it has leg windows and column
	// NAMES, no per-slot value to take an identity from, so every key it could mint
	// would be a name. Declining costs the lazy
	// qualified mint, which is what an underivable outer already gets.
	rt := legRowType("X", "Y")
	typedScan := mustRebaseLayoutConstruct(plans.NewRecordQueryScanPlan([]string{"T"}, rt, false))
	nlj := mustRebaseLayoutConstruct(plans.NewRecordQueryNestedLoopJoinPlan(
		typedScan, typedScan, nil, plans.JoinInner, values.NamedCorrelationIdentifier("L"), values.NamedCorrelationIdentifier("R"),
		values.NewRecordConstructorValue(),
	))
	if got := buriedLegOrdinalLayout(nlj); got != nil {
		t.Fatalf("the NLJ-chain shape states only column NAMES and must decline, got %v", got)
	}
}
