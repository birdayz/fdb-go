package sqldriver

import (
	"context"
	"database/sql"
	"fmt"
	mrand "math/rand/v2"
	"testing"
)

// TestSQL_SimFDB_WorkloadDriver is RFC-199 Tier 2's serial seed-reproducible SQL workload driver:
// a seeded stream of INSERT / UPDATE / DELETE statements runs over SimFDB while an in-memory model
// shadows the table; periodic SELECTs must agree with the model. Because SimFDB is deterministic
// and the driver is single-threaded off one seed, the whole run is reproducible — the container-
// free, seed-reproducible SQL workload the goroutine-based stress harness cannot be.
func TestSQL_SimFDB_WorkloadDriver(t *testing.T) {
	t.Parallel()
	key := injectSimFDB(t, 7)
	ctx := context.Background()

	setup, err := sql.Open("fdbsql", fmt.Sprintf("fdbsql:///wl?cluster_file=%s", key))
	if err != nil {
		t.Fatalf("open setup: %v", err)
	}
	defer setup.Close()
	mustExecSQL(t, setup, ctx, "CREATE DATABASE /wl")
	mustExecSQL(t, setup, ctx,
		"CREATE SCHEMA TEMPLATE wt CREATE TABLE t (id BIGINT NOT NULL, v BIGINT, PRIMARY KEY (id))")
	mustExecSQL(t, setup, ctx, "CREATE SCHEMA /wl/s WITH TEMPLATE wt")

	db, err := sql.Open("fdbsql", fmt.Sprintf("fdbsql:///wl?cluster_file=%s&schema=s", key))
	if err != nil {
		t.Fatalf("open query conn: %v", err)
	}
	defer db.Close()

	model := map[int64]int64{}
	verify := func(op int) {
		rows, err := db.QueryContext(ctx, "SELECT id, v FROM t ORDER BY id")
		if err != nil {
			t.Fatalf("select at op %d: %v", op, err)
		}
		got := map[int64]int64{}
		var prev int64 = -1
		for rows.Next() {
			var id, v int64
			if err := rows.Scan(&id, &v); err != nil {
				t.Fatalf("scan at op %d: %v", op, err)
			}
			if id <= prev {
				t.Fatalf("op %d: rows not ordered (%d after %d)", op, id, prev)
			}
			prev = id
			got[id] = v
		}
		if err := rows.Err(); err != nil {
			t.Fatalf("rows.Err at op %d: %v", op, err)
		}
		_ = rows.Close()
		if len(got) != len(model) {
			t.Fatalf("op %d: table has %d rows, model has %d", op, len(got), len(model))
		}
		for id, v := range model {
			if got[id] != v {
				t.Fatalf("op %d: id=%d has v=%d, model expects %d", op, id, got[id], v)
			}
		}
	}

	rng := mrand.New(mrand.NewPCG(7, 0))
	const numOps = 120
	for i := 0; i < numOps; i++ {
		id := int64(rng.IntN(20))
		v := int64(rng.IntN(1000))
		switch rng.IntN(3) {
		case 0: // insert if absent, else update (INSERT on an existing PK would error)
			if _, present := model[id]; present {
				mustExecSQL(t, db, ctx, fmt.Sprintf("UPDATE t SET v=%d WHERE id=%d", v, id))
			} else {
				mustExecSQL(t, db, ctx, fmt.Sprintf("INSERT INTO t (id, v) VALUES (%d, %d)", id, v))
			}
			model[id] = v
		case 1: // update (a no-op if the row is absent — matches the model)
			mustExecSQL(t, db, ctx, fmt.Sprintf("UPDATE t SET v=%d WHERE id=%d", v, id))
			if _, present := model[id]; present {
				model[id] = v
			}
		case 2: // delete
			mustExecSQL(t, db, ctx, fmt.Sprintf("DELETE FROM t WHERE id=%d", id))
			delete(model, id)
		}
		if i%20 == 19 {
			verify(i)
		}
	}
	verify(numOps)
	if len(model) == 0 {
		t.Fatal("workload emptied the table — op mix produced nothing to verify")
	}
}
