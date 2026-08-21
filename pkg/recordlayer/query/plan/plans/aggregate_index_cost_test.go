package plans

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/properties"
)

// AN AGGREGATE LEAF PRICES ITSELF FROM ITS INDEX'S RECORD TYPES, NOT FROM ITS
// NAME FIELD.
//
// RecordQueryAggregateIndexPlan used to cost from GetRecordTypeName(), an
// IDENTITY field its three construction sites fill as
// `if len(rts) > 0 { name = rts[0] }` (rule_aggregate_data_access.go). Two
// distinct bugs lived there, and neither is expressible in a single-table
// fixture — which is exactly how the first version of this test passed with the
// bug fully restored:
//
//   - EMPTY NAME. A candidate declaring no record types leaves the name "", and
//     an empty name asks a provider for the WHOLE STORE. The leaf then priced
//     itself from the sum of every table in the schema while a typed sibling in
//     the same plan priced one.
//   - MULTI-TYPE. An index over several record types priced exactly rts[0] and
//     ignored the rest.
//
// Both arms are built HERE rather than through SQL, because SQL cannot express
// either: relational indexes are per-table, so every SQL-declared aggregate
// index has exactly one record type, and with one type the old and new formulas
// are BIT-IDENTICAL. A fixture that cannot distinguish two implementations
// cannot test the difference between them, however carefully it asserts.
//
// Each arm therefore asserts the value the CURRENT rule gives AND that it
// differs from the value the OLD rule gave — the second half is what makes the
// assertion a statement about the change rather than about arithmetic.
func TestAggregateIndexHintCost_PricesFromIndexRecordTypes(t *testing.T) {
	t.Parallel()

	const orders, customers = 150.0, 40.0
	stats := properties.MapStatistics{
		PerType:  map[string]float64{"ORDERS": orders, "CUSTOMERS": customers},
		Fallback: properties.LeafScanCardinality,
	}

	newAgg := func(t *testing.T, recordTypes []string, nameField string) *RecordQueryAggregateIndexPlan {
		t.Helper()
		idx := mustChecked(t, func() (*RecordQueryIndexPlan, error) {
			return NewRecordQueryIndexPlan("agg_idx", nil, recordTypes, exactTestRecordType(), false)
		})
		return mustChecked(t, func() (*RecordQueryAggregateIndexPlan, error) {
			return NewRecordQueryAggregateIndexPlan(idx, nameField, exactTestRecordType(), "SUM")
		})
	}

	tests := []struct {
		name string
		// The plan's index declares these types...
		indexTypes []string
		// ...while its identity name field says this.
		nameField string
		wantNew   float64
		wantOld   float64
		why       string
	}{
		{
			name:       "empty name field, index declares one type",
			indexTypes: []string{"ORDERS"},
			nameField:  "",
			wantNew:    orders * properties.DistinctSelectivity,
			wantOld:    properties.LeafScanCardinality * properties.DistinctSelectivity,
			why: "an empty name asks the provider for the WHOLE STORE, so the leaf " +
				"priced itself from every table in the schema at once",
		},
		{
			name:       "index declares two types, name field names the first",
			indexTypes: []string{"ORDERS", "CUSTOMERS"},
			nameField:  "ORDERS",
			wantNew:    (orders + customers) * properties.DistinctSelectivity,
			wantOld:    orders * properties.DistinctSelectivity,
			why:        "a multi-type index priced rts[0] and ignored every other type it covers",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			// The arm is only meaningful while the two formulas actually differ
			// on this fixture. Asserted rather than assumed: the previous
			// version of this test used a fixture where they coincided, and so
			// passed with the defect fully present.
			if tc.wantNew == tc.wantOld {
				t.Fatalf("this fixture prices the same under both rules (%v), so it "+
					"cannot detect the defect it names", tc.wantNew)
			}
			got := newAgg(t, tc.indexTypes, tc.nameField).HintCost(nil, stats).Cardinality
			if got != tc.wantNew {
				t.Fatalf("aggregate leaf priced at %v, want %v.\nPricing from the name "+
					"field instead of the index's record types gives %v — %s",
					got, tc.wantNew, tc.wantOld, tc.why)
			}
		})
	}
}
