package properties

import "testing"

// A QUOTED TABLE NAME MUST STILL FIND ITS COUNT.
//
// Two namespaces meet at RecordTypeCardinality. Metadata carries STORAGE names
// — ToProtoBufCompliantName of the user name — while a relational scan asks
// with the SQL name it was parsed from (cascades_translator.go passes the table
// name straight into FullUnorderedScanExpression). They are the same string for
// almost every table, and differ exactly for a quoted identifier carrying '$',
// '.' or "__".
//
// A miss there is not a small error. It returns the whole-store total, so a
// SMALL escaped table is priced as the entire schema and the join drives from
// the wrong side — while statistics are present, fresh, complete, and the gate
// reports USABLE. Nothing anywhere says the number was wrong.
func TestCollectedStatisticsResolvesEscapedNames(t *testing.T) {
	t.Parallel()
	// Keyed the way the collector keys it: by storage name.
	stats := NewCollectedStatistics(map[string]float64{
		"my__1table": 12,   // storage form of a user name containing '$'
		"PLAIN":      3000, // unescaped, to make the store total large
	})

	t.Run("the storage name resolves", func(t *testing.T) {
		t.Parallel()
		if got := stats.RecordTypeCardinality("my__1table"); got != 12 {
			t.Errorf("storage-name lookup = %v, want 12", got)
		}
	})

	t.Run("the SQL name resolves to the same count", func(t *testing.T) {
		t.Parallel()
		got := stats.RecordTypeCardinality("my$table")
		if got == stats.storeTotal() {
			t.Fatalf("SQL name fell back to the whole store (%v) instead of finding its "+
				"count — a 12-row table priced as the whole schema drives the join from "+
				"the wrong side, with every gate reporting healthy", got)
		}
		if got != 12 {
			t.Errorf("SQL-name lookup = %v, want 12", got)
		}
	})

	t.Run("a genuinely unknown name still falls back", func(t *testing.T) {
		t.Parallel()
		// The escaping fallback must not turn an unknown table into a hit; it
		// only re-asks under the canonical spelling of the SAME name.
		if got := stats.RecordTypeCardinality("NOSUCHTABLE"); got != stats.storeTotal() {
			t.Errorf("unknown name = %v, want the store total %v", got, stats.storeTotal())
		}
	})
}
