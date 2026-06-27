package sqldriver_test

import (
	"context"
	"database/sql"
	"fmt"
	"testing"
	"time"
)

// PIN (codex P2-1): a query-PARAMETER-bound scan in a join leg must plan and
// return correct rows — the parameter (ConstantObjectValue) comparand is an
// execution constant, NOT a row correlation, so scanComparisonCorrelations must
// NOT report its constant-pool alias. If it did, the parameter-bound scan
// `Scan(T,[k=?])` would look join-correlated and could perturb B1 leg detection /
// the GRAEFE-2 probe-fed-residual guard. Here the t-leg carries both a join probe
// (t.fk = o.id) and a parameter filter (t.k = ?); rows must be exactly those with
// k = 5.
func TestFDB_ParamBoundScanInJoinLeg_NotMisseenAsCorrelated(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	setup := openTestDB(t, "/testdb_paramleg")
	mwjoMustExec(t, setup, ctx, "CREATE DATABASE /testdb_paramleg")
	mwjoMustExec(t, setup, ctx,
		"CREATE SCHEMA TEMPLATE paramleg_tmpl "+
			"CREATE TABLE o (id BIGINT NOT NULL, PRIMARY KEY (id)) "+
			"CREATE TABLE t (id BIGINT NOT NULL, fk BIGINT, k BIGINT, PRIMARY KEY (id))")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA /testdb_paramleg/s WITH TEMPLATE paramleg_tmpl")

	dsn := fmt.Sprintf("fdbsql:///testdb_paramleg?cluster_file=%s&schema=s", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer db.Close()

	mwjoMustExec(t, db, ctx, "INSERT INTO o VALUES (1)")
	mwjoMustExec(t, db, ctx, "INSERT INTO o VALUES (2)")
	mwjoMustExec(t, db, ctx, "INSERT INTO t VALUES (10, 1, 5)")
	mwjoMustExec(t, db, ctx, "INSERT INTO t VALUES (11, 1, 7)")
	mwjoMustExec(t, db, ctx, "INSERT INTO t VALUES (12, 2, 5)")

	const q = "SELECT o.id FROM o, t WHERE t.fk = o.id AND t.k = ?"
	rows, err := db.QueryContext(ctx, q, 5)
	if err != nil {
		t.Fatalf("query %q: %v", q, err)
	}
	defer rows.Close()
	var got []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			t.Fatalf("scan: %v", err)
		}
		got = append(got, id)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows.Err: %v", err)
	}
	if len(got) != 2 {
		t.Errorf("%q [?=5]: got %d rows %v, want 2 (ids 1,2) — param scan mis-seen as correlated would perturb routing", q, len(got), got)
	}
}

// PIN (codex P2-2): a 3-way join whose plan nests an inner FlatMap (a correlated
// probe) as the OUTER of an upper join. The inner FlatMap binds its own outer/inner
// aliases, so the completed sub-join is NOT externally correlated; yieldGeneralFlat
// Map must bind the wrapper quantifiers with the plan's real aliases (not fresh
// ForEach aliases) so Reference.GetCorrelatedTo subtracts them. If they leaked, the
// upper join would see the self-contained sub-join as still correlated and skip /
// misroute it. Plan is FlatMap(FlatMap(Scan(C), probe(B)), probe(A)); rows must be
// the chain matches.
func TestFDB_NestedFlatMapUnderJoin_NoCorrelationLeak(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	setup := openTestDB(t, "/testdb_nestedfm")
	mwjoMustExec(t, setup, ctx, "CREATE DATABASE /testdb_nestedfm")
	mwjoMustExec(t, setup, ctx,
		"CREATE SCHEMA TEMPLATE nestedfm_tmpl "+
			"CREATE TABLE a (id BIGINT NOT NULL, PRIMARY KEY (id)) "+
			"CREATE TABLE b (id BIGINT NOT NULL, fk BIGINT, PRIMARY KEY (id)) "+
			"CREATE TABLE c (id BIGINT NOT NULL, fk BIGINT, PRIMARY KEY (id))")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA /testdb_nestedfm/s WITH TEMPLATE nestedfm_tmpl")

	dsn := fmt.Sprintf("fdbsql:///testdb_nestedfm?cluster_file=%s&schema=s", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer db.Close()

	mwjoMustExec(t, db, ctx, "INSERT INTO a VALUES (1)")
	mwjoMustExec(t, db, ctx, "INSERT INTO a VALUES (2)")
	mwjoMustExec(t, db, ctx, "INSERT INTO b VALUES (10, 1)")
	mwjoMustExec(t, db, ctx, "INSERT INTO b VALUES (11, 2)")
	// c20→b10→a1 MATCH; c21→b11→a2 MATCH; c22→b99 no b → no.
	mwjoMustExec(t, db, ctx, "INSERT INTO c VALUES (20, 10)")
	mwjoMustExec(t, db, ctx, "INSERT INTO c VALUES (21, 11)")
	mwjoMustExec(t, db, ctx, "INSERT INTO c VALUES (22, 99)")

	const q = "SELECT c.id FROM a, b, c WHERE b.fk = a.id AND c.fk = b.id"
	got := queryIDs(t, db, ctx, q)
	if len(got) != 2 || got[0] != 20 || got[1] != 21 {
		t.Errorf("%q: got %v, want [20 21] — a leaked sub-join correlation would misroute the upper join", q, got)
	}
}

// PIN (@claude/Torvalds REQUIRED-2, the EXISTS/FOD twin of codex P2-2): a
// correlated EXISTS whose FlatMap sits over an upper join. The EXISTS FlatMap binds
// its outer with the plan's real outer alias (NamedForEachQuantifier(mergedOuterCorr
// / outerCorrelation)) and its FOD inner with NamedPhysicalQuantifier(existCorr /
// innerCorrelation) — NOT fresh aliases — so a completed correlated-EXISTS subplan
// does not leak its bound outer correlation upward (Reference.GetCorrelatedTo would
// otherwise fail to subtract a fresh alias) and the enclosing join is not misrouted.
// EXISTS c.fk=a.id correlates the subquery to a (joined to b above); rows must be
// exactly the a's that join b AND have a matching c.
func TestFDB_CorrelatedExistsUnderJoin_NoCorrelationLeak(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	// Unique db/schema per process so the pin is stable under --runs_per_test
	// (separate processes sharing one FDB container).
	u := time.Now().UnixNano()
	dbp := fmt.Sprintf("/testdb_existsleak_%d", u)
	tmpl := fmt.Sprintf("existsleak_tmpl_%d", u)
	setup := openTestDB(t, dbp)
	mwjoMustExec(t, setup, ctx, "CREATE DATABASE "+dbp)
	mwjoMustExec(t, setup, ctx,
		"CREATE SCHEMA TEMPLATE "+tmpl+" "+
			"CREATE TABLE a (id BIGINT NOT NULL, PRIMARY KEY (id)) "+
			"CREATE TABLE b (id BIGINT NOT NULL, fk BIGINT, PRIMARY KEY (id)) "+
			"CREATE TABLE c (id BIGINT NOT NULL, fk BIGINT, PRIMARY KEY (id))")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA "+dbp+"/s WITH TEMPLATE "+tmpl)

	dsn := fmt.Sprintf("fdbsql://%s?cluster_file=%s&schema=s", dbp, clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer db.Close()

	mwjoMustExec(t, db, ctx, "INSERT INTO a VALUES (1)")
	mwjoMustExec(t, db, ctx, "INSERT INTO a VALUES (2)")
	mwjoMustExec(t, db, ctx, "INSERT INTO a VALUES (3)")
	mwjoMustExec(t, db, ctx, "INSERT INTO b VALUES (10, 1)")
	mwjoMustExec(t, db, ctx, "INSERT INTO b VALUES (11, 2)")
	mwjoMustExec(t, db, ctx, "INSERT INTO b VALUES (12, 3)")
	// c has fk for a=1 and a=3 only.
	mwjoMustExec(t, db, ctx, "INSERT INTO c VALUES (20, 1)")
	mwjoMustExec(t, db, ctx, "INSERT INTO c VALUES (21, 3)")

	// a=1: joins b10, EXISTS c(fk=1) -> yes; a=2: joins b11, no c(fk=2) -> no;
	// a=3: joins b12, EXISTS c(fk=3) -> yes. Project BOTH leg columns: the
	// pre-fix bug (qualifyOuterRow clobbering the merged row's qualified "A.ID"
	// with the bare last-leg-wins value) collapsed a.id and b.id onto one leg
	// nondeterministically — so assert the exact (a.id, b.id) pairs, not just a.id.
	const q = "SELECT a.id, b.id FROM a, b WHERE b.fk = a.id AND EXISTS (SELECT 1 FROM c WHERE c.fk = a.id)"
	got := queryIDCounts(t, db, ctx, q) // reuses (int64,int64) row reader; .id=a.id .count=b.id
	want := []idCount{{1, 10}, {3, 12}}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Errorf("%q: got %v, want [{1 10} {3 12}] — a leaked/collapsed EXISTS-join correlation misroutes the columns", q, got)
	}
}
