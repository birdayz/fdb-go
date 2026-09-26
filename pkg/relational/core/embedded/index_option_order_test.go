package embedded

import (
	"slices"
	"testing"
)

// The DDL front end stores an index's options in Java's generator call order:
// MaterializedViewIndexGenerator calls setUnique (:107) before the permuted size
// (:174), so a permuted min/max index stores [unique, permutedSize] (the WS-J
// oracle measures the target storing exactly that). Before the order was
// recorded, Go ranged over a map and the stored bytes flipped between builds,
// so this builds the same DDL many times and requires the same order every time.
func TestIndexOptionOrder_PermutedIndexStoresUniqueFirst(t *testing.T) {
	t.Parallel()
	const ddl = "CREATE TABLE s (id BIGINT, region STRING, amount BIGINT, PRIMARY KEY (id)) " +
		"CREATE INDEX mx AS SELECT max(amount) FROM s GROUP BY region " +
		"CREATE UNIQUE INDEX r AS SELECT region FROM s ORDER BY region"
	want := map[string][]string{
		"MX": {"unique", "permutedSize"},
		"R":  {"unique"},
	}
	for range 50 {
		tmpl, err := BuildSchemaTemplateFromDDLNamed(ddl, "T")
		if err != nil {
			t.Fatalf("build: %v", err)
		}
		p, err := tmpl.Underlying().ToProto()
		if err != nil {
			t.Fatalf("ToProto: %v", err)
		}
		seen := 0
		for _, ix := range p.GetIndexes() {
			w, ok := want[ix.GetName()]
			if !ok {
				continue
			}
			seen++
			var got []string
			for _, o := range ix.GetOptions() {
				got = append(got, o.GetKey())
			}
			if !slices.Equal(got, w) {
				t.Fatalf("index %s stores options %v, want %v", ix.GetName(), got, w)
			}
		}
		if seen != len(want) {
			t.Fatalf("found %d of the %d indexes", seen, len(want))
		}
	}
}
