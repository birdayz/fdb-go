package values

import (
	"errors"
	"testing"
)

type bindingTestRow struct {
	names []string
	slots []any
}

func (r *bindingTestRow) Get(ordinal int) (any, bool) {
	if r == nil || ordinal < 0 || ordinal >= len(r.slots) {
		return nil, false
	}
	return r.slots[ordinal], true
}

func (r *bindingTestRow) TypeNames() []string {
	if r == nil {
		return nil
	}
	return append([]string(nil), r.names...)
}

type hostileWindowPresence struct{ WindowMatchPresence }

type bindingErrorCode interface {
	Code() ResolutionErrorCode
}

func requireBindingError(t testing.TB, value any, present bool, err error, want ResolutionErrorCode) {
	t.Helper()
	if value != nil || present {
		t.Fatalf("failed binding returned (%T, %t), want (nil, false)", value, present)
	}
	var coded bindingErrorCode
	if !errors.As(err, &coded) || coded.Code() != want {
		t.Fatalf("error = %v, want code %d", err, want)
	}
}

func bindingFixtureRow() *bindingTestRow {
	return &bindingTestRow{
		names: []string{"OUTER_LONG", "NESTED", "OUTER_STRING", "NULLABLE_LONG"},
		slots: []any{
			int64(10),
			&bindingTestRow{names: []string{"INNER_LONG", "INNER_STRING"}, slots: []any{int64(20), "nested"}},
			"outer",
			nil,
		},
	}
}

func TestOrdinalObjectBinderMaterializesCurrentObjectAndWindows(t *testing.T) {
	t.Parallel()

	fixture := newLayoutFixture(t, "_binding")
	layout := mustOrdinalLayout(t, fixture)
	presence, err := NewWindowMatchPresence([]WindowMatch{{Source: fixture.nullableSource, Matched: false}})
	if err != nil {
		t.Fatalf("NewWindowMatchPresence: %v", err)
	}
	row := bindingFixtureRow()
	binder, err := NewOrdinalObjectBinder(layout, row, presence, nil)
	if err != nil {
		t.Fatalf("NewOrdinalObjectBinder: %v", err)
	}

	current, present, err := binder.GetQuantifiedBinding(layout.Carrier())
	if err != nil || !present || current != row {
		t.Fatalf("current binding = (%T, %t, %v), want exact row", current, present, err)
	}
	fieldObject, present, err := binder.GetQuantifiedBinding(fixture.fieldSource)
	if err != nil || !present {
		t.Fatalf("field source binding = (%T, %t, %v)", fieldObject, present, err)
	}
	fieldRow, ok := fieldObject.(OrdinalRow)
	if !ok {
		t.Fatalf("field source = %T, want OrdinalRow", fieldObject)
	}
	if got, ok := fieldRow.Get(0); !ok || got != int64(10) {
		t.Fatalf("field window[0] = (%v, %t), want (10, true)", got, ok)
	}
	if got, ok := fieldRow.Get(1); !ok || got != "nested" {
		t.Fatalf("field window[1] = (%v, %t), want (nested, true)", got, ok)
	}
	object, present, err := binder.GetQuantifiedBinding(fixture.objectSource)
	if err != nil || !present || object != row.slots[1] {
		t.Fatalf("object window = (%T, %t, %v), want nested row", object, present, err)
	}
	scalar, present, err := binder.GetQuantifiedBinding(fixture.scalarSource)
	if err != nil || !present || scalar != int64(20) {
		t.Fatalf("scalar window = (%v, %t, %v), want (20, true, nil)", scalar, present, err)
	}
	unmatched, present, err := binder.GetQuantifiedBinding(fixture.nullableSource)
	if err != nil || !present || unmatched != nil {
		t.Fatalf("unmatched window = (%v, %t, %v), want bound SQL NULL", unmatched, present, err)
	}

	fieldValue, err := ResolveFieldOrdinals(fixture.fieldSource, []int{1})
	if err != nil {
		t.Fatalf("ResolveFieldOrdinals: %v", err)
	}
	got, err := fieldValue.Evaluate(&RowEvalContext{Objects: binder})
	if err != nil || got != "nested" {
		t.Fatalf("source-bound FieldValue = (%v, %v), want (nested, nil)", got, err)
	}
}

func TestOrdinalObjectBinderDistinguishesAbsentNullAndMatchedAllNull(t *testing.T) {
	t.Parallel()

	fixture := newLayoutFixture(t, "_presence")
	layout := mustOrdinalLayout(t, fixture)
	row := bindingFixtureRow()

	unmatchedPresence, err := NewWindowMatchPresence([]WindowMatch{{Source: fixture.nullableSource, Matched: false}})
	if err != nil {
		t.Fatal(err)
	}
	unmatchedBinder, err := NewOrdinalObjectBinder(layout, row, unmatchedPresence, nil)
	if err != nil {
		t.Fatal(err)
	}
	got, present, err := unmatchedBinder.GetQuantifiedBinding(fixture.nullableSource)
	if err != nil || !present || got != nil {
		t.Fatalf("unmatched = (%v, %t, %v), want present SQL NULL", got, present, err)
	}

	matchedPresence, err := NewWindowMatchPresence([]WindowMatch{{Source: fixture.nullableSource, Matched: true}})
	if err != nil {
		t.Fatal(err)
	}
	matchedBinder, err := NewOrdinalObjectBinder(layout, row, matchedPresence, nil)
	if err != nil {
		t.Fatal(err)
	}
	got, present, err = matchedBinder.GetQuantifiedBinding(fixture.nullableSource)
	if err != nil || !present || got != nil {
		t.Fatalf("matched NULL datum = (%v, %t, %v), want present SQL NULL", got, present, err)
	}

	absent, err := NewQuantifiedObjectValue(NamedCorrelationIdentifier("absent"), NullableLong)
	if err != nil {
		t.Fatal(err)
	}
	got, present, err = matchedBinder.GetQuantifiedBinding(absent)
	if err != nil || present || got != nil {
		t.Fatalf("absent = (%v, %t, %v), want unbound", got, present, err)
	}
	got, err = absent.Evaluate(&RowEvalContext{Objects: matchedBinder})
	var coded bindingErrorCode
	if got != nil || !errors.As(err, &coded) || coded.Code() != UnboundCorrelation {
		t.Fatalf("absent QOV evaluation = (%v, %v), want UnboundCorrelation", got, err)
	}
}

func TestOrdinalObjectBinderRejectsWrongCurrentPresenceAndRuntimeShapes(t *testing.T) {
	t.Parallel()

	fixture := newLayoutFixture(t, "_invalid")
	layout := mustOrdinalLayout(t, fixture)
	validPresence, err := NewWindowMatchPresence([]WindowMatch{{Source: fixture.nullableSource, Matched: false}})
	if err != nil {
		t.Fatal(err)
	}
	if binder, err := NewOrdinalObjectBinder(layout, bindingFixtureRow(), nil, nil); binder != nil {
		t.Fatalf("missing presence returned binder %T", binder)
	} else {
		var coded bindingErrorCode
		if !errors.As(err, &coded) || coded.Code() != LayoutPresenceMissing {
			t.Fatalf("missing presence error = %v, want LayoutPresenceMissing", err)
		}
	}
	if binder, err := NewOrdinalObjectBinder(layout, bindingFixtureRow(), &hostileWindowPresence{}, nil); binder != nil {
		t.Fatalf("hostile presence returned binder %T", binder)
	} else {
		var coded bindingErrorCode
		if !errors.As(err, &coded) || coded.Code() != LayoutForeignValue {
			t.Fatalf("hostile presence error = %v, want LayoutForeignValue", err)
		}
	}
	short := &bindingTestRow{slots: []any{int64(10)}}
	if binder, err := NewOrdinalObjectBinder(layout, short, validPresence, nil); binder != nil {
		t.Fatalf("short carrier returned binder %T", binder)
	} else {
		var coded bindingErrorCode
		if !errors.As(err, &coded) || coded.Code() != LayoutRuntimeShape {
			t.Fatalf("short carrier error = %v, want LayoutRuntimeShape", err)
		}
	}

	binder, err := NewOrdinalObjectBinder(layout, bindingFixtureRow(), validPresence, nil)
	if err != nil {
		t.Fatal(err)
	}
	twin := &quantifiedObjectValue{correlation: fixture.carrier.correlation, flowed: fixture.carrier.flowed}
	value, present, err := binder.GetQuantifiedBinding(twin)
	requireBindingError(t, value, present, err, LayoutCarrierMismatch)

	conflicting, err := NewQuantifiedObjectValue(fixture.fieldSource.correlation, &RecordType{
		Fields: []Field{{Name: "OTHER", Ordinal: 0, FieldType: NotNullLong}},
	})
	if err != nil {
		t.Fatal(err)
	}
	value, present, err = binder.GetQuantifiedBinding(conflicting)
	requireBindingError(t, value, present, err, CorrelationTypeConflict)
}

func TestOrdinalObjectBinderScalarCarrierAndPresenceSnapshot(t *testing.T) {
	t.Parallel()

	carrier := mustLayoutCurrentQOV(t, NullableLong)
	layout, err := NewScalarOrdinalLayout(carrier)
	if err != nil {
		t.Fatal(err)
	}
	binder, err := NewOrdinalObjectBinder(layout, int64(42), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	got, present, err := binder.GetQuantifiedBinding(carrier)
	if err != nil || !present || got != int64(42) {
		t.Fatalf("scalar current = (%v, %t, %v), want (42, true, nil)", got, present, err)
	}
	if bad, err := NewOrdinalObjectBinder(layout, &bindingTestRow{}, nil, nil); bad != nil {
		t.Fatalf("scalar row returned binder %T", bad)
	} else {
		var coded bindingErrorCode
		if !errors.As(err, &coded) || coded.Code() != LayoutRuntimeShape {
			t.Fatalf("scalar row error = %v, want LayoutRuntimeShape", err)
		}
	}

	fixture := newLayoutFixture(t, "_snapshot")
	matches := []WindowMatch{{Source: fixture.nullableSource, Matched: false}}
	presence, err := NewWindowMatchPresence(matches)
	if err != nil {
		t.Fatal(err)
	}
	matches[0].Source = fixture.scalarSource
	matches[0].Matched = true
	matched, known := presence.MatchState(fixture.nullableSource)
	if !known || matched {
		t.Fatalf("presence changed after caller mutation: matched=%t known=%t", matched, known)
	}
}

func TestOrdinalWindowMatchStateRequiresExactNullSupplyingSource(t *testing.T) {
	t.Parallel()

	fixture := newLayoutFixture(t, "_window_state")
	layout := mustOrdinalLayout(t, fixture)
	presence, err := NewWindowMatchPresence([]WindowMatch{{
		Source: fixture.nullableSource, Matched: false,
	}})
	if err != nil {
		t.Fatal(err)
	}
	if nullSupplying, stateErr := LayoutWindowNullSupplying(layout, fixture.nullableSource); stateErr != nil || !nullSupplying {
		t.Fatalf("nullable source window = (%t,%v), want (true,nil)", nullSupplying, stateErr)
	}
	if matched, stateErr := OrdinalWindowMatchState(layout, presence, fixture.nullableSource); stateErr != nil || matched {
		t.Fatalf("unmatched source state = (%t,%v), want (false,nil)", matched, stateErr)
	}
	if nullSupplying, stateErr := LayoutWindowNullSupplying(layout, fixture.fieldSource); stateErr != nil || nullSupplying {
		t.Fatalf("ordinary source window = (%t,%v), want (false,nil)", nullSupplying, stateErr)
	}

	assertCode := func(t *testing.T, err error, want ResolutionErrorCode) {
		t.Helper()
		var coded bindingErrorCode
		if !errors.As(err, &coded) || coded.Code() != want {
			t.Fatalf("error = %v, want code %d", err, want)
		}
	}
	_, err = OrdinalWindowMatchState(layout, nil, fixture.nullableSource)
	assertCode(t, err, LayoutPresenceMissing)
	_, err = OrdinalWindowMatchState(layout, &hostileWindowPresence{}, fixture.nullableSource)
	assertCode(t, err, LayoutForeignValue)
	_, err = OrdinalWindowMatchState(layout, presence, fixture.fieldSource)
	assertCode(t, err, LayoutInvalidWindow)

	wrongType, err := NewQuantifiedObjectValue(
		fixture.nullableSource.Correlation(), NullableString)
	if err != nil {
		t.Fatal(err)
	}
	_, err = LayoutWindowNullSupplying(layout, wrongType)
	assertCode(t, err, CorrelationTypeConflict)
	_, err = OrdinalWindowMatchState(layout, presence, wrongType)
	assertCode(t, err, CorrelationTypeConflict)
	foreign, err := NewQuantifiedObjectValue(
		NamedCorrelationIdentifier("foreign_window"), fixture.nullableSource.FlowedType())
	if err != nil {
		t.Fatal(err)
	}
	_, err = LayoutWindowNullSupplying(layout, foreign)
	assertCode(t, err, LayoutSourceNotProvided)
	_, err = OrdinalWindowMatchState(layout, presence, foreign)
	assertCode(t, err, LayoutSourceNotProvided)

	matched, known := presence.MatchState(fixture.nullableSource)
	if !known || matched {
		t.Fatal("exact match-state reads mutated the source presence")
	}
}
