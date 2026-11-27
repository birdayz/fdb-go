package values

import (
	"errors"
	"testing"
)

func TestNarrowValueRefusesWhatTheSlotCannotHold(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		child  Value
		target Type
		want   any
		null   bool
		fails  bool
	}{
		{"value fits", &ConstantValue{Value: int32(3), Typ: NullableInt}, NotNullInt, int32(3), false, false},
		{"NULL into NOT NULL", &ConstantValue{Typ: NullableInt}, NotNullInt, nil, true, true},
		{"NULL into nullable", &ConstantValue{Typ: NullableString}, NullableInt, nil, false, false},
		{"another type", &ConstantValue{Value: "x", Typ: NotNullString}, NotNullInt, nil, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			narrowed := NewNarrowValue(tc.child, tc.target)
			if !narrowed.Type().Equals(tc.target) {
				t.Fatalf("type = %s, want %s", narrowed.Type(), tc.target)
			}
			got, err := narrowed.Evaluate(nil)
			var slotErr *SlotAssignmentError
			if tc.fails {
				if !errors.As(err, &slotErr) || slotErr.Null != tc.null {
					t.Fatalf("Evaluate = (%v, %v), want a SlotAssignmentError with Null=%t", got, err, tc.null)
				}
				return
			}
			if err != nil || got != tc.want {
				t.Fatalf("Evaluate = (%v, %v), want %v", got, err, tc.want)
			}
		})
	}
	if msg := (&SlotAssignmentError{Null: true}).Error(); msg != "Cannot set a non-nullable field to the NULL value" {
		t.Fatalf("NULL message = %q", msg)
	}
	if msg := (&SlotAssignmentError{From: NullableLong, To: NotNullInt}).Error(); msg != "BIGINT value cannot be stored in a column of type INT" {
		t.Fatalf("type message = %q", msg)
	}
}
