package executor

import (
	"reflect"
	"testing"

	"google.golang.org/protobuf/proto"

	"fdb.dev/pkg/fdbgo/fdb/tuple"
	"fdb.dev/pkg/recordlayer"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
)

// TestFlatMapInheritOuterRecordProperties pins the Java FlatMap contract used
// by existential compensation: reshaping the row must not discard the outer
// stored-record/primary-key identity that a later primary-key distinct needs.
func TestFlatMapInheritOuterRecordProperties(t *testing.T) {
	t.Parallel()

	legA, legB, qovA, qovB, seed := ojWiringLegs(t)
	outerRecord := &recordlayer.FDBStoredRecord[proto.Message]{}
	innerRecord := &recordlayer.FDBStoredRecord[proto.Message]{}
	outer := ojLegQR(t, legA, int64(1), int64(10))
	outer.Record = outerRecord
	outer.PrimaryKey = tuple.Tuple{"outer", int64(1)}
	inner := ojLegQR(t, legB, int64(2), int64(20))
	inner.Record = innerRecord
	inner.PrimaryKey = tuple.Tuple{"inner", int64(2)}

	t.Run("ordinal_build_keeps_outer_identity_only_when_enabled", func(t *testing.T) {
		t.Parallel()
		for _, tc := range []struct {
			name    string
			inherit bool
		}{
			{name: "enabled", inherit: true},
			{name: "disabled", inherit: false},
		} {
			tc := tc
			t.Run(tc.name, func(t *testing.T) {
				t.Parallel()
				c, err := newFlatMapCursorWithOuterProperties(
					recordlayer.FromList([]QueryResult{}),
					nil,
					nil,
					nil,
					EmptyEvaluationContext(),
					qovA.Correlation,
					qovB.Correlation,
					seed,
					recordlayer.ExecuteProperties{},
					tc.inherit,
				)
				if err != nil {
					t.Fatalf("newFlatMapCursorWithOuterProperties: %v", err)
				}
				defer c.Close()

				got, err := c.computeResult(outer, inner)
				if err != nil {
					t.Fatalf("computeResult: %v", err)
				}
				if got.Positional == nil {
					t.Fatal("ordinal FlatMap must still publish its computed positional payload")
				}
				if tc.inherit {
					if got.Record != outerRecord || !reflect.DeepEqual(got.PrimaryKey, outer.PrimaryKey) {
						t.Fatalf("inherited identity = (%p, %v), want outer (%p, %v)",
							got.Record, got.PrimaryKey, outerRecord, outer.PrimaryKey)
					}
				} else if got.Record != nil || got.PrimaryKey != nil {
					t.Fatalf("non-inheriting FlatMap leaked outer identity: (%p, %v)", got.Record, got.PrimaryKey)
				}
			})
		}
	})

	t.Run("identity_inner_payload_still_uses_outer_identity", func(t *testing.T) {
		t.Parallel()
		outerPlan := plans.NewRecordQueryScanPlan([]string{"A"}, legA, false)
		innerPlan := plans.NewRecordQueryScanPlan([]string{"B"}, legB, false)
		c, err := newFlatMapCursorWithOuterProperties(
			recordlayer.FromList([]QueryResult{}),
			outerPlan,
			innerPlan,
			nil,
			EmptyEvaluationContext(),
			qovA.Correlation,
			qovB.Correlation,
			values.NewQuantifiedObjectValueOfType(qovB.Correlation, legB),
			recordlayer.ExecuteProperties{},
			true,
		)
		if err != nil {
			t.Fatalf("newFlatMapCursorWithOuterProperties: %v", err)
		}
		defer c.Close()

		got, err := c.computeResult(outer, inner)
		if err != nil {
			t.Fatalf("computeResult: %v", err)
		}
		if got.Positional == nil || !reflect.DeepEqual(got.Positional.Slots, inner.Positional.Slots) {
			t.Fatalf("identity-inner payload slots = %v, want inner slots %v",
				got.Positional, inner.Positional.Slots)
		}
		if got.Record != outerRecord || !reflect.DeepEqual(got.PrimaryKey, outer.PrimaryKey) {
			t.Fatalf("identity-inner inherited identity = (%p, %v), want outer (%p, %v)",
				got.Record, got.PrimaryKey, outerRecord, outer.PrimaryKey)
		}
	})
}
