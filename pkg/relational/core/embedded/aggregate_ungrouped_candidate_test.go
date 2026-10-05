package embedded

import (
	"strings"
	"testing"

	"fdb.dev/pkg/recordlayer"
)

// TestUngroupedAggregateIndexIsACandidate: an ungrouped aggregate index is a
// match candidate, as Java's is. Java's aggregate-empty-table.yamsql :577-580
// plans `select sum(col1) from T2` over a SUM index as
// `AISCAN(T2_I5 <,> BY_GROUP ...)` and reads the stored 0 over an emptied table,
// where the unindexed control (:547-550) answers NULL. Go reads the same
// (ungrouped_aggregate_index.yaml).
func TestUngroupedAggregateIndexIsACandidate(t *testing.T) {
	t.Parallel()

	const schema = `
CREATE TABLE SALES (
  id BIGINT,
  category STRING,
  amount BIGINT,
  PRIMARY KEY (id)
)
CREATE INDEX sum_all AS SELECT SUM(amount) FROM SALES
CREATE INDEX sum_by_cat AS SELECT SUM(amount) FROM SALES GROUP BY category
CREATE INDEX min_ever_all AS SELECT min_ever(amount) FROM SALES
`
	tmpl, err := BuildSchemaTemplateFromDDL(schema)
	if err != nil {
		t.Fatalf("build schema template: %v", err)
	}
	md := tmpl.Underlying()
	if md == nil {
		t.Fatal("schema template carries no metadata")
	}

	find := func(want string) *recordlayer.Index {
		t.Helper()
		for name, idx := range md.GetAllIndexes() {
			if strings.EqualFold(name, want) {
				return idx
			}
		}
		var names []string
		for n := range md.GetAllIndexes() {
			names = append(names, n)
		}
		t.Fatalf("index %q not found (have %v)", want, names)
		return nil
	}

	// The GROUPED sibling of the same aggregate over the same table is a
	// candidate too.
	grouped := find("sum_by_cat")
	if got := tryAggregateIndexCandidate(grouped, md); got == nil {
		t.Fatal("a GROUPED SUM index produced no aggregate candidate")
	}

	ungrouped := find("sum_all")
	gke, ok := ungrouped.RootExpression.(*recordlayer.GroupingKeyExpression)
	if !ok {
		t.Fatalf("ungrouped SUM index root is %T, want *recordlayer.GroupingKeyExpression", ungrouped.RootExpression)
	}
	if gc := gke.GetGroupingCount(); gc != 0 {
		t.Fatalf("the ungrouped SUM index reports grouping count %d, want 0 — the DDL no longer "+
			"builds `SELECT SUM(v) FROM t` as an UNGROUPED aggregate, so this test is no longer "+
			"testing the shape it names", gc)
	}

	if got := tryAggregateIndexCandidate(ungrouped, md); got == nil {
		t.Fatal("an UNGROUPED SUM index produced no match candidate, where Java's serves " +
			"`SELECT SUM(amount) FROM sales` from it")
	}

	if got := tryAggregateIndexCandidate(find("min_ever_all"), md); got == nil {
		t.Fatal("an ungrouped MIN_EVER index produced no candidate: `SELECT min_ever(amount) FROM sales` " +
			"has no plan without it, since no streaming accumulator computes an _EVER aggregate")
	}
}

// TestBareEverIndexTypesAreNotCandidates: Java's AggregateIndexExpansionVisitor
// builds candidates for MAX_EVER_LONG/_TUPLE and MIN_EVER_LONG/_TUPLE only; the
// deprecated bare "max_ever"/"min_ever" types are not in its aggregate map
// (supportsAggregateIndexType), so no query reads them, though their
// maintenance is _LONG's.
func TestBareEverIndexTypesAreNotCandidates(t *testing.T) {
	t.Parallel()
	tmpl, err := BuildSchemaTemplateFromDDL(`
CREATE TABLE SALES (id BIGINT, category STRING, amount BIGINT, PRIMARY KEY (id))
CREATE INDEX max_by_cat AS SELECT category, max_ever(amount) FROM SALES GROUP BY category
CREATE INDEX min_by_cat AS SELECT category, min_ever(amount) FROM SALES GROUP BY category
`)
	if err != nil {
		t.Fatalf("build schema template: %v", err)
	}
	md := tmpl.Underlying()
	for _, c := range []struct{ name, long, tuple, bare string }{
		{"MAX_BY_CAT", recordlayer.IndexTypeMaxEverLong, recordlayer.IndexTypeMaxEverTuple, recordlayer.IndexTypeMaxEver},
		{"MIN_BY_CAT", recordlayer.IndexTypeMinEverLong, recordlayer.IndexTypeMinEverTuple, recordlayer.IndexTypeMinEver},
	} {
		idx := md.GetIndex(c.name)
		if idx == nil || (idx.Type != c.long && idx.Type != c.tuple) {
			t.Fatalf("%s: want a %s or %s index, got %v", c.name, c.long, c.tuple, idx)
		}
		if tryAggregateIndexCandidate(idx, md) == nil {
			t.Fatalf("control: the %s index is not a candidate", idx.Type)
		}
		bare := *idx
		bare.Type = c.bare
		if got := tryAggregateIndexCandidate(&bare, md); got != nil {
			t.Fatalf("a bare %q index is a candidate; Java builds none for it", c.bare)
		}
	}
}
