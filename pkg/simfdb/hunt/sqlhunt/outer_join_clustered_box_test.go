package sqlhunt

import (
	"context"
	"database/sql"
	"fmt"
	"testing"

	"fdb.dev/pkg/dst"
	"fdb.dev/pkg/recordlayer"
	"fdb.dev/pkg/relational/sqldriver"
	"fdb.dev/pkg/simfdb"
)

// threeTableHarness is the ma/mb/mc shape an outer join over a CLUSTERED BOX
// needs: two tables joined into one box, and a third the box is outer-joined
// against. The fixed `t`-only harness cannot express it.
//
// It runs on SimFDB — a full in-process FDB — so this is the same planner,
// cursors and Value evaluation a container run exercises, at roughly a second
// per iteration instead of a container start. That difference is what makes the
// shape practical to pin here as well as in the FDB suite.
func threeTableHarness(t *testing.T, seed uint64) *sql.DB {
	t.Helper()
	env := dst.NewSim(seed)
	env.Buggify = dst.DisabledBuggifier()

	simDB := recordlayer.NewFDBDatabaseWithBackend(simfdb.New(env)).SetEnv(env)
	simDB.SetStoreStateCache(recordlayer.NewMetaDataVersionStampStoreStateCache())

	key := fmt.Sprintf("sim://outerjoinbox/%d/%d", seed, qcKeyCounter.Add(1))
	t.Cleanup(sqldriver.RegisterBackend(key, simDB))

	setup, err := sql.Open("fdbsql", "fdbsql://"+qcDBPath+"?cluster_file="+key)
	if err != nil {
		t.Fatalf("open setup: %v", err)
	}
	setup.SetMaxOpenConns(1)
	t.Cleanup(func() { setup.Close() }) //nolint:errcheck

	ctx := context.Background()
	for _, stmt := range []string{
		"CREATE DATABASE " + qcDBPath,
		"CREATE SCHEMA TEMPLATE boxtmpl " +
			"CREATE TABLE ma (id BIGINT, v BIGINT, PRIMARY KEY (id)) " +
			"CREATE TABLE mb (bid BIGINT, ref BIGINT, PRIMARY KEY (bid)) " +
			"CREATE TABLE mc (cid BIGINT, w BIGINT, PRIMARY KEY (cid))",
		"CREATE SCHEMA " + qcDBPath + "/s WITH TEMPLATE boxtmpl",
	} {
		if _, err := setup.ExecContext(ctx, stmt); err != nil {
			t.Fatalf("setup %q: %v", stmt, err)
		}
	}

	db, err := sql.Open("fdbsql", "fdbsql://"+qcDBPath+"?cluster_file="+key+"&schema=s")
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { db.Close() }) //nolint:errcheck
	return db
}

// TestOuterJoinOverClusteredBoxResolvesBuriedLegColumns pins that a projection
// reading a column BURIED inside the null-supplying leg of an outer join
// resolves to a runtime binding.
//
// `mb JOIN mc` is one clustered box; `RIGHT JOIN ma` makes that whole box the
// null-supplying side. The projection then reads `b.bid` — b is not a direct leg
// of the outer join, it is a leg of the box the outer join null-extends — so the
// value has to cross TWO producer boundaries to reach an output ordinal. When
// the crossing fails, the value survives rooted on B and admission rejects it
// with "exact QOV B has no declared runtime binding": a hard error on a query
// that must return one padded row, not a wrong answer, which is the right
// direction but still a broken query.
//
// The padded row is asserted as well as the plan running, because a resolution
// that lands on the WRONG buried column would also produce three columns and
// no error. NULL for both box columns is the only correct answer here: ma has a
// row, mb and mc are empty, so the box supplies nothing.
func TestOuterJoinOverClusteredBoxResolvesBuriedLegColumns(t *testing.T) {
	t.Parallel()
	db := threeTableHarness(t, 920001)
	ctx := context.Background()

	if _, err := db.ExecContext(ctx, "INSERT INTO ma VALUES (1, 10)"); err != nil {
		t.Fatalf("seed: %v", err)
	}

	rows, err := db.QueryContext(ctx,
		"SELECT ma.id, b.bid, c.cid FROM mb AS b JOIN mc AS c ON b.ref = c.cid "+
			"RIGHT JOIN ma ON ma.id = b.bid")
	if err != nil {
		t.Fatalf("outer join over a clustered box failed to run: %v", err)
	}
	defer rows.Close() //nolint:errcheck

	seen := 0
	for rows.Next() {
		var id int64
		var bid, cid sql.NullInt64
		if err := rows.Scan(&id, &bid, &cid); err != nil {
			t.Fatalf("scan: %v", err)
		}
		seen++
		if id != 1 {
			t.Errorf("preserved ma.id = %d, want 1", id)
		}
		if bid.Valid || cid.Valid {
			t.Errorf("padded box columns are (bid=%v, cid=%v), want both NULL — "+
				"a non-NULL here means the buried leg column resolved to some other "+
				"slot rather than to the null-extended box", bid, cid)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	if seen != 1 {
		t.Fatalf("returned %d rows, want the single padded preserved row", seen)
	}
}
