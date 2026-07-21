package executor

import (
	"strings"
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// Regression pins for the ComparisonKeyFunc error channel: a comparison-key
// eval failure or a keyless/PK-less/positional-less row must FAIL THE QUERY
// (an error through the closure's error return, surfaced by the merge
// cursor's advance()), never the process. Before the fix these paths were
// panic(err) — a direct Record Layer API caller crashed on a malformed plan
// (the SQL surface survived only via the cascades_generator boundary
// recover). Java parity: KeyedMergeCursorState.comparisonKeyFunction
// failures propagate as RecordCoreException — an error, never a crash.

// evalFailKeyVals returns comparison-key values whose evaluation fails
// against a 1-slot scalar row: the resolved ordinal 5 misses (loud) in
// FieldValue.Evaluate's ordinal read.
func evalFailKeyVals() []values.Value {
	return []values.Value{values.NewFieldValueWithResolvedOrdinal("X", 5, values.TypeInt)}
}

func TestIntersectionCompKeyFunc_EvalFailure_ReturnsError(t *testing.T) {
	t.Parallel()
	for name, fn := range map[string]func(QueryResult) (any, error){
		"intersection": func(qr QueryResult) (any, error) {
			return intersectionCompKeyFunc(evalFailKeyVals())(qr)
		},
		"multiIntersection": func(qr QueryResult) (any, error) {
			return multiIntersectionCompKeyFunc(evalFailKeyVals())(qr)
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			_, err := fn(dscalar(int64(1)))
			if err == nil {
				t.Fatal("comparison-key eval failure must return an error, got nil")
			}
			if !strings.Contains(err.Error(), "comparison key") {
				t.Fatalf("error should name the comparison-key site, got: %v", err)
			}
		})
	}
}

func TestIntersectionCompKeyFunc_NoKeysNoPKNoPositional_ReturnsError(t *testing.T) {
	t.Parallel()
	for name, fn := range map[string]func(QueryResult) (any, error){
		"intersection": func(qr QueryResult) (any, error) {
			return intersectionCompKeyFunc(nil)(qr)
		},
		"multiIntersection": func(qr QueryResult) (any, error) {
			return multiIntersectionCompKeyFunc(nil)(qr)
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			_, err := fn(QueryResult{})
			if err == nil {
				t.Fatal("a row with no keys, no PK and no positional content must return an error, got nil")
			}
			if !strings.Contains(err.Error(), "malformed plan") {
				t.Fatalf("error should identify the malformed plan, got: %v", err)
			}
		})
	}
}
