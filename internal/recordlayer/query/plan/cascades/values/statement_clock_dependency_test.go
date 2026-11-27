package values

import (
	"testing"
	"time"
)

// DependsOnStatementClock is the executor's ONCE-per-operator probe that
// decides whether a bare frontier row must be wrapped in a clock-bearing
// RowEvalContext. A false negative silently re-arms per-row
// CURRENT_TIMESTAMP drift; a false positive only costs an allocation.
func TestDependsOnStatementClock(t *testing.T) {
	t.Parallel()
	ts := NewScalarFunctionValue("CURRENT_TIMESTAMP", NullableTimestamp)
	cases := []struct {
		name string
		v    Value
		want bool
	}{
		{"nil", nil, false},
		{"bare CURRENT_TIMESTAMP", ts, true},
		{"CURRENT_DATE", NewScalarFunctionValue("CURRENT_DATE", NullableDate), true},
		{"CURRENT_TIME", NewScalarFunctionValue("CURRENT_TIME", NullableTimestamp), true},
		{"LOCALTIME", NewScalarFunctionValue("LOCALTIME", NullableTimestamp), true},
		{"plain literal", &ConstantValue{Value: int64(1), Typ: NullableLong}, false},
		{"clockless function", NewScalarFunctionValue("UPPER", NullableString, &ConstantValue{Value: "x", Typ: NullableString}), false},
		{
			// The shape the SELECT-path drift pin exercises: the clocked
			// function BURIED under an interior node, where a shallow check
			// would miss it.
			"nested under record constructor",
			NewRecordConstructorValue(RecordConstructorField{Name: "TS", Value: ts}),
			true,
		},
		{
			"nested under clockless function",
			NewScalarFunctionValue("COALESCE", NullableTimestamp, &NullValue{}, ts),
			true,
		},
	}
	for _, tc := range cases {
		if got := DependsOnStatementClock(tc.v); got != tc.want {
			t.Errorf("%s: DependsOnStatementClock = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// RowEvalContext.StatementNow serves the carried Clock, and only falls
// back to the wall clock without one — the fallback IS the drift, so the
// pin here is that a set Clock wins.
func TestRowEvalContextStatementNow(t *testing.T) {
	t.Parallel()
	fixed := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	rc := &RowEvalContext{Clock: fixedClock{now: fixed}}
	if got := rc.StatementNow(); !got.Equal(fixed) {
		t.Fatalf("StatementNow with Clock = %v, want %v", got, fixed)
	}
	bare := &RowEvalContext{}
	before := time.Now().UTC().Add(-time.Minute)
	if got := bare.StatementNow(); got.Before(before) {
		t.Fatalf("StatementNow without Clock = %v, want ~wall clock", got)
	}
}

type fixedClock struct{ now time.Time }

func (c fixedClock) StatementNow() time.Time { return c.now }
