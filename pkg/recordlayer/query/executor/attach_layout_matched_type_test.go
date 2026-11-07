package executor

import (
	"context"
	"errors"
	"testing"

	"fdb.dev/pkg/recordlayer"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
)

// These pin attachProvidedOutputLayout's memo of the last row type it
// admitted without widening: it skips the check only for that exact type
// object, so every other row type is still checked and widened or refused.

// drainAttached runs rows through plan's output boundary. It returns the rows
// published and the error that stopped the cursor, nil at its end.
func drainAttached(t *testing.T, plan plans.RecordQueryPlan, rows ...*PositionalRow) ([]*PositionalRow, error) {
	t.Helper()
	results := make([]QueryResult, len(rows))
	for i, row := range rows {
		results[i] = QueryResult{Positional: row}
	}
	cursor, err := attachProvidedOutputLayout(plan, recordlayer.FromList(results))
	if err != nil {
		t.Fatalf("attach output layout: %v", err)
	}
	var out []*PositionalRow
	for {
		result, err := cursor.OnNext(context.Background())
		if err != nil {
			return out, err
		}
		if !result.HasNext() {
			return out, nil
		}
		out = append(out, result.GetValue().Positional)
	}
}

func requireCarrierMismatch(t *testing.T, err error) {
	t.Helper()
	var resolution *values.ResolutionError
	if !errors.As(err, &resolution) || resolution.ErrorCode != values.LayoutCarrierMismatch {
		t.Fatalf("error = %v, want a LayoutCarrierMismatch ResolutionError", err)
	}
}

func matchedTypeFields() []values.Field {
	return []values.Field{
		{Name: "ID", FieldType: values.NotNullLong, Ordinal: 0},
		{Name: "B", FieldType: values.NullableLong, Ordinal: 1},
	}
}

// requirePublished checks that out[i] is in[i] under layout with type want.
func requirePublished(t *testing.T, in, out *PositionalRow, layout values.OrdinalLayout, want *values.RecordType) {
	t.Helper()
	if out.Layout != layout {
		t.Error("published row does not carry the plan's layout")
	}
	if out.Type != want {
		t.Errorf("published row type %v (%p), want %v (%p)", out.Type, out.Type, want, want)
	}
	if len(out.Slots) != len(in.Slots) {
		t.Fatalf("published %d slots, want %d", len(out.Slots), len(in.Slots))
	}
	for i := range in.Slots {
		if out.Slots[i] != in.Slots[i] {
			t.Errorf("slot %d = %v, want %v", i, out.Slots[i], in.Slots[i])
		}
	}
}

// Rows sharing one type object that matches the carrier all attach, as does a
// structurally equal type object between them.
func TestAttachProvidedOutputLayout_RowsOfAMatchedTypeAttach(t *testing.T) {
	t.Parallel()
	rowType := values.NewRecordType("plan_output", false, matchedTypeFields())
	twin := values.NewRecordType("plan_output", false, matchedTypeFields())
	plan, layout := scanPlanWithLayout(t, rowType)

	rows := []*PositionalRow{
		{Type: rowType, Slots: []any{int64(1), nil}},
		{Type: rowType, Slots: []any{int64(2), int64(20)}},
		{Type: twin, Slots: []any{int64(3), nil}},
		{Type: rowType, Slots: []any{int64(4), int64(40)}},
		{Type: rowType, Slots: []any{int64(5), nil}},
	}
	out, err := drainAttached(t, plan, rows...)
	if err != nil {
		t.Fatalf("matching rows refused: %v", err)
	}
	if len(out) != len(rows) {
		t.Fatalf("published %d rows, want %d", len(out), len(rows))
	}
	for i := range rows {
		requirePublished(t, rows[i], out[i], layout, rows[i].Type)
	}
}

// A row whose type is not the matched object is checked, so one outside the
// carrier is refused however many matching rows came first.
func TestAttachProvidedOutputLayout_MatchedTypeAdmitsNoOther(t *testing.T) {
	t.Parallel()
	rowType := values.NewRecordType("plan_output", false, matchedTypeFields())
	plan, layout := scanPlanWithLayout(t, rowType)
	matching := func(id int64) *PositionalRow { return &PositionalRow{Type: rowType, Slots: []any{id, nil}} }

	wider := values.NewRecordType("plan_output", false, append(matchedTypeFields(),
		values.Field{Name: "EXTRA", FieldType: values.NotNullLong, Ordinal: 2}))
	otherField := values.NewRecordType("plan_output", false, []values.Field{
		{Name: "ID", FieldType: values.NotNullLong, Ordinal: 0},
		{Name: "B", FieldType: values.NullableString, Ordinal: 1},
	})
	nullable, ok := values.WithNullability(rowType, true).(*values.RecordType)
	if !ok || !nullable.IsNullable() {
		t.Fatal("fixture: the nullable row type is not a nullable record")
	}
	for _, c := range []struct {
		name string
		bad  *PositionalRow
	}{
		{"foreign width", &PositionalRow{Type: wider, Slots: []any{int64(9), nil, int64(1)}}},
		{"foreign field type", &PositionalRow{Type: otherField, Slots: []any{int64(9), nil}}},
		{"nullable row under a NOT NULL carrier", &PositionalRow{Type: nullable, Slots: []any{int64(9), nil}}},
		{"untyped row", &PositionalRow{}},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			good := []*PositionalRow{matching(1), matching(2), matching(3)}
			out, err := drainAttached(t, plan, append(good, c.bad, matching(4))...)
			requireCarrierMismatch(t, err)
			if len(out) != len(good) {
				t.Fatalf("published %d rows before the refusal, want %d", len(out), len(good))
			}
			for i := range good {
				requirePublished(t, good[i], out[i], layout, rowType)
			}
			// The same row first, before any type is matched, is refused too.
			_, err = drainAttached(t, plan, c.bad)
			requireCarrierMismatch(t, err)
		})
	}
}

// A NOT NULL row under the nullable carrier takes the carrier's type every
// time, before and after a row of an exactly matching type, and is never the
// type remembered as matched: a refused row after either is still refused.
func TestAttachProvidedOutputLayout_WideningIsNotMemoised(t *testing.T) {
	t.Parallel()
	notNull := values.NewRecordType("leg", false, matchedTypeFields())
	nullable, ok := values.WithNullability(notNull, true).(*values.RecordType)
	if !ok || !nullable.IsNullable() {
		t.Fatal("fixture: the nullable carrier is not a nullable record")
	}
	plan, layout := scanPlanWithLayout(t, nullable)
	widened := func(id int64) *PositionalRow { return &PositionalRow{Type: notNull, Slots: []any{id, nil}} }
	exact := func(id int64) *PositionalRow { return &PositionalRow{Type: nullable, Slots: []any{id, id * 10}} }

	rows := []*PositionalRow{widened(1), widened(2), exact(3), exact(4), widened(5), exact(6), widened(7)}
	out, err := drainAttached(t, plan, rows...)
	if err != nil {
		t.Fatalf("rows of the carrier and its NOT NULL form refused: %v", err)
	}
	if len(out) != len(rows) {
		t.Fatalf("published %d rows, want %d", len(out), len(rows))
	}
	for i, in := range rows {
		if in.Type == nullable {
			requirePublished(t, in, out[i], layout, nullable)
			continue
		}
		if out[i] == in || in.Type != notNull {
			t.Errorf("row %d: the NOT NULL input row was changed instead of copied", i)
		}
		if out[i].Type == notNull || !out[i].Type.IsNullable() || !out[i].Type.Equals(nullable) {
			t.Errorf("row %d: NOT NULL row published as %v, want the nullable carrier type", i, out[i].Type)
		}
		requirePublished(t, in, out[i], layout, out[i].Type)
	}

	foreign := &PositionalRow{Type: values.NewRecordType("leg", false, []values.Field{
		{Name: "ID", FieldType: values.NullableLong, Ordinal: 0},
		{Name: "B", FieldType: values.NullableLong, Ordinal: 1},
	}), Slots: []any{int64(9), nil}}
	for name, prefix := range map[string][]*PositionalRow{
		"after widened rows only": {widened(1), widened(2)},
		"after a matched row":     {widened(1), exact(2)},
	} {
		for _, bad := range []*PositionalRow{foreign, {}} {
			out, err := drainAttached(t, plan, append(append([]*PositionalRow(nil), prefix...), bad)...)
			if err == nil {
				t.Errorf("%s: row of type %v published", name, bad.Type)
				continue
			}
			requireCarrierMismatch(t, err)
			if len(out) != len(prefix) {
				t.Errorf("%s: published %d rows before the refusal, want %d", name, len(out), len(prefix))
			}
		}
	}
}
