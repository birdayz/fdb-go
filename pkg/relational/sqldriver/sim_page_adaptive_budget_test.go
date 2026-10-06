package sqldriver

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"fdb.dev/pkg/dst"
	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/recordlayer"
	"fdb.dev/pkg/simfdb"
)

// shortWindowBackend injects transaction_too_old after a page exceeds the
// simulated MVCC window. The commit boundary also exercises failures outside
// the SQL callback, which a callback error handler alone cannot observe.
type shortWindowBackend struct {
	*simfdb.SimDB
	clock     *gatedSteppingClock
	failures  atomic.Int64
	successes atomic.Int64
}

func (b *shortWindowBackend) instant() time.Time {
	b.clock.mu.Lock()
	defer b.clock.mu.Unlock()
	return b.clock.now
}

func (b *shortWindowBackend) Transact(fn func(fdb.WritableTransaction) (any, error)) (any, error) {
	return b.SimDB.Transact(func(tx fdb.WritableTransaction) (any, error) {
		start := b.instant()
		result, err := fn(tx)
		if err != nil {
			return result, err
		}
		if b.instant().Sub(start) > 1500*time.Millisecond {
			b.failures.Add(1)
			return nil, fdb.Error{Code: 1007}
		}
		b.successes.Add(1)
		return result, nil
	})
}

func TestSimPageBudgetAdaptsToShortMVCCWindow(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	clock := &gatedSteppingClock{now: dst.Epoch}
	env := dst.NewSim(257)
	env.Clock = clock
	env.Buggify = dst.DisabledBuggifier()
	backend := &shortWindowBackend{SimDB: simfdb.New(env), clock: clock}
	rdb := recordlayer.NewFDBDatabaseWithBackend(backend).SetEnv(env)
	rdb.SetStoreStateCache(recordlayer.NewMetaDataVersionStampStoreStateCache())
	key := "sim://" + t.Name()
	fdbDBCache.Store(key, rdb)
	t.Cleanup(func() { fdbDBCache.Delete(key) })
	setup, err := sql.Open("fdbsql", fmt.Sprintf("fdbsql:///FRL/SIMDB?cluster_file=%s", key))
	if err != nil {
		t.Fatal(err)
	}
	defer setup.Close()
	for _, q := range []string{"CREATE DATABASE /FRL/simdb", "CREATE SCHEMA TEMPLATE tmpl CREATE TABLE t (id BIGINT, PRIMARY KEY (id))", "CREATE SCHEMA /FRL/simdb/s WITH TEMPLATE tmpl"} {
		mustExecSQL(t, setup, ctx, q)
	}
	db, err := sql.Open("fdbsql", fmt.Sprintf("fdbsql:///FRL/SIMDB?cluster_file=%s&schema=S", key))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	const count = 600
	var insert strings.Builder
	insert.WriteString("INSERT INTO t (id) VALUES ")
	for i := 0; i < count; i++ {
		if i > 0 {
			insert.WriteByte(',')
		}
		fmt.Fprintf(&insert, "(%d)", i)
	}
	mustExecSQL(t, db, ctx, insert.String())
	before := backend.successes.Load()
	clock.Arm(10 * time.Millisecond)
	rows, err := db.QueryContext(ctx, "SELECT id FROM t ORDER BY id")
	if err != nil {
		t.Fatalf("page retries never fit the short MVCC window (%d failures): %v", backend.failures.Load(), err)
	}
	defer rows.Close()
	got := 0
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		if id != int64(got) {
			t.Fatalf("row %d = %d: retry lost or duplicated rows", got, id)
		}
		got++
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if got != count {
		t.Fatalf("got %d rows, want %d", got, count)
	}
	if failures := backend.failures.Load(); failures < 2 || failures > 20 {
		t.Fatalf("got %d retries; want repeated failure followed by convergence", failures)
	}
	if pages := backend.successes.Load() - before; pages < 3 {
		t.Fatalf("only %d successful transactions: multi-page resume not exercised", pages)
	}
	t.Logf("returned %d ordered rows after %d short-window failures", got, backend.failures.Load())
}
