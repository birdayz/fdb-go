package metamorphic

import "testing"

// TestAggregateSumIndexDropsAllNullGroup pins a LIVE divergence the indexed seed scenario found
// on its first run. Read it as a problem statement, not as a bug under test: it asserts what the
// engine does TODAY and goes RED the moment that changes, in either direction.
//
// THE SHAPE. With an aggregate index `SELECT SUM(v) FROM t GROUP BY g`, the query
//
//	SELECT g, SUM(v) FROM t GROUP BY g
//
// omits any group whose v is NULL in every row. The same query written so the planner cannot use
// the index (here: an extra no-op predicate) returns the group with a NULL sum. Without the index
// on the table, both forms return the group. By SQL semantics GROUP BY g yields one group per
// distinct g present, and SUM over an all-NULL group is NULL, so the group belongs in the answer.
//
// WHY IT IS NOT FIXED HERE. The index side of it is Java's model, not a Go slip:
// AtomicMutation.Standard.getMutationParam returns null for a NULL SUM_LONG value
// (fdb-record-layer-core/.../indexes/AtomicMutation.java:181-189), so no index entry is written
// for such a record and an all-NULL group has no entry to scan. That is identical in Go. What is
// NOT established is whether Java's SQL layer still lets the planner answer a plain GROUP BY from
// that index — i.e. whether Java diverges the same way at the query level, which would make this
// inherited rather than a Go defect. Answering that needs the Java conformance path and a
// query-engine ruling, so the fix is owner-gated; this pin is what keeps the finding armed
// instead of filed.
//
// This is the same bug FAMILY as the checked-in aggindex-minmax-allnull-group-dropped finding,
// which WAS fixed (SQL MAX now maps to PERMUTED_MAX, which indexes the NULL extremum, instead of
// MAX_EVER_LONG, which skips NULLs). Whoever takes this should start there: the question is
// whether SUM has an analogous index type, or whether the planner must decline the index when a
// group's aggregate can be NULL.
func TestAggregateSumIndexDropsAllNullGroup(t *testing.T) {
	t.Parallel()

	const (
		table    = "CREATE TABLE t (id BIGINT NOT NULL, g BIGINT, v BIGINT, PRIMARY KEY (id))"
		sumIndex = "CREATE INDEX sum_by_g AS SELECT SUM(v) FROM t GROUP BY g"
	)
	data := []string{
		"INSERT INTO t (id, g, v) VALUES (1, 1, 10)",
		"INSERT INTO t (id, g, v) VALUES (3, 2, NULL)",
		"INSERT INTO t (id, g, v) VALUES (4, 2, NULL)",
	}
	// The two spellings select identical rows: id is a NOT NULL primary key, so `id IS NOT NULL`
	// is a no-op that only stops the planner reaching for the aggregate index.
	group := Group{
		Name:   "sum-allnull-group",
		Reason: "GROUP BY g must yield a group for every distinct g present; g=2 is all-NULL so SUM(v) is NULL and [2, NULL] belongs in the answer",
		Queries: []string{
			"SELECT g, SUM(v) FROM t GROUP BY g",
			"SELECT g, SUM(v) FROM t WHERE id IS NOT NULL GROUP BY g",
		},
	}

	t.Run("without the aggregate index the two forms agree", func(t *testing.T) {
		t.Parallel()
		viols, err := Check(Scenario{
			Name: "sum-nullgroup-noindex", Seed: 4,
			Tables: []string{table}, Data: data, Groups: []Group{group},
		})
		if err != nil {
			t.Fatalf("check setup: %v", err)
		}
		if len(viols) != 0 {
			t.Fatalf("the NO-INDEX path diverged: %v. That would be a plain aggregate bug, not "+
				"the index-path divergence this test is about — root-cause it now.", viols)
		}
	})

	t.Run("with the aggregate index the group is dropped", func(t *testing.T) {
		t.Parallel()
		viols, err := Check(Scenario{
			Name: "sum-nullgroup-indexed", Seed: 4,
			Tables: []string{table, sumIndex}, Data: data, Groups: []Group{group},
		})
		if err != nil {
			t.Fatalf("check setup: %v", err)
		}
		if len(viols) == 0 {
			t.Fatal("the aggregate-index SUM path now AGREES with the scan path on an all-NULL " +
				"group. That is the CORRECT answer and this divergence is FIXED — delete this " +
				"test and move the relation into the `indexed` seed scenario in corpus.go, " +
				"where it becomes a permanent equivalence.")
		}
		t.Logf("known-open divergence still present: %s", viols[0])
	})
}
