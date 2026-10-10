package sqldriver

// End to end over SimFDB: DSN execution limits and planner knobs reach the
// statements a connection runs, and a connection-level SetOption overrides
// them only until the connection returns to the pool.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"fdb.dev/pkg/dst"
	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/recordlayer"
	"fdb.dev/pkg/relational/api"
	"fdb.dev/pkg/relational/core/embedded"
	"fdb.dev/pkg/simfdb"
)

// u has no index, so ORDER BY u.a needs an in-memory sort.
const limitsDDL = "CREATE TABLE t (id BIGINT, a BIGINT, PRIMARY KEY (id)) CREATE INDEX idx_a ON t(a) " +
	"CREATE TABLE u (id BIGINT, a BIGINT, PRIMARY KEY (id))"

// openLimitsDB seeds eight rows on a fresh SimFDB backend and opens params on
// it; the returned recorder sees every transaction the backend creates.
func openLimitsDB(t *testing.T, params string) (*sql.DB, *txOptionRecorder) {
	t.Helper()
	// dsn_test has no testkit.Main to register the domain.
	RegisterDomainIfNotExists("FRL")
	env := dst.NewSim(221)
	env.Buggify = dst.DisabledBuggifier()
	rec := &txOptionRecorder{SimDB: simfdb.New(env)}
	simDB := recordlayer.NewFDBDatabaseWithBackend(rec).SetEnv(env)
	simDB.SetStoreStateCache(recordlayer.NewMetaDataVersionStampStoreStateCache())
	key := "sim://" + t.Name()
	fdbDBCache.Store(key, simDB)
	t.Cleanup(func() { fdbDBCache.Delete(key) })

	ctx := context.Background()
	setup, err := sql.Open(DriverName, "fdbsql:///FRL/SIMDB?cluster_file="+url.QueryEscape(key))
	if err != nil {
		t.Fatal(err)
	}
	defer setup.Close()
	for _, stmt := range []string{
		"CREATE DATABASE /FRL/simdb",
		"CREATE SCHEMA TEMPLATE tmpl " + limitsDDL,
		"CREATE SCHEMA /FRL/simdb/s WITH TEMPLATE tmpl",
	} {
		if _, err := setup.ExecContext(ctx, stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}
	seed, err := sql.Open(DriverName, "fdbsql:///FRL/SIMDB?schema=S&cluster_file="+url.QueryEscape(key))
	if err != nil {
		t.Fatal(err)
	}
	defer seed.Close()
	for i := 0; i < 8; i++ {
		for _, table := range []string{"t", "u"} {
			if _, err := seed.ExecContext(ctx, fmt.Sprintf("INSERT INTO %s VALUES (%d, %d)", table, i, 8-i)); err != nil {
				t.Fatal(err)
			}
		}
	}
	db, err := sql.Open(DriverName, "fdbsql:///FRL/SIMDB?schema=S&cluster_file="+url.QueryEscape(key)+"&"+params)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db, rec
}

func countRows(ctx context.Context, q interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}, query string,
) (int, error) {
	rows, err := q.QueryContext(ctx, query)
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	n := 0
	for rows.Next() {
		n++
	}
	return n, rows.Err()
}

func TestDSN_ExecutionLimitsReachPages(t *testing.T) {
	t.Parallel()
	for _, limit := range []string{"execution_scanned_rows_limit=2", "execution_scanned_bytes_limit=1"} {
		t.Run(limit, func(t *testing.T) {
			t.Parallel()
			db, _ := openLimitsDB(t, limit+"&max_rows=5")
			ctx := context.Background()
			c, err := db.Conn(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer c.Close()
			stats := &lastStats{}
			if err := c.Raw(func(dc any) error {
				dc.(*embedded.EmbeddedConnection).SetExecutionStatsLogger(stats)
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			if n, err := countRows(ctx, c, "SELECT id FROM t"); err != nil || n != 5 {
				t.Fatalf("DSN max_rows across pages: count=%d err=%v", n, err)
			}
			if got := stats.get(); got.Pages < 2 || got.RowsReturned != 5 {
				t.Fatalf("DSN scan budget did not reach pagination: %+v", got)
			}
		})
	}
}

type lastStats struct {
	mu    sync.Mutex
	stats embedded.ExecutionStats
}

func (l *lastStats) LogExecutionStats(_ context.Context, s embedded.ExecutionStats) {
	l.mu.Lock()
	l.stats = s
	l.mu.Unlock()
}

func (l *lastStats) get() embedded.ExecutionStats {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.stats
}

func TestDSN_MemoryLimitReachesSort(t *testing.T) {
	t.Parallel()
	db, _ := openLimitsDB(t, "max_statement_memory_bytes=1")
	_, err := countRows(context.Background(), db, "SELECT id FROM u ORDER BY a")
	var apiErr *api.Error
	if !errors.As(err, &apiErr) || apiErr.Code != api.ErrCodeExecutionLimitReached {
		t.Fatalf("DSN memory budget ignored: %v", err)
	}
}

// The DSN value is the connection default: SetOption overrides it for one
// borrow, and ResetSession restores it for the next.
func TestDSN_SetOptionOverridesUntilReset(t *testing.T) {
	t.Parallel()
	db, _ := openLimitsDB(t, "max_rows=5")
	db.SetMaxOpenConns(1)
	ctx := context.Background()
	c, err := db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if n, err := countRows(ctx, c, "SELECT id FROM t"); err != nil || n != 5 {
		t.Fatalf("DSN max_rows: count=%d err=%v", n, err)
	}
	if err := c.Raw(func(dc any) error {
		return dc.(*embedded.EmbeddedConnection).SetOption(api.OptMaxRows, 3)
	}); err != nil {
		t.Fatal(err)
	}
	if n, err := countRows(ctx, c, "SELECT id FROM t"); err != nil || n != 3 {
		t.Fatalf("SetOption over DSN max_rows: count=%d err=%v", n, err)
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	if n, err := countRows(ctx, db, "SELECT id FROM t"); err != nil || n != 5 {
		t.Fatalf("pooled connection kept the override: count=%d err=%v", n, err)
	}
}

func TestDSN_DisabledPlannerRulesReachPlan(t *testing.T) {
	t.Parallel()
	const query = "EXPLAIN SELECT id FROM t WHERE a = 1"
	for _, tc := range []struct {
		params    string
		indexScan bool
	}{
		{"", true},
		{"disabled_planner_rules=MatchLeafRule", false},
	} {
		t.Run(tc.params, func(t *testing.T) {
			t.Parallel()
			db, _ := openLimitsDB(t, tc.params)
			var plan string
			if err := db.QueryRowContext(context.Background(), query).Scan(&plan); err != nil {
				t.Fatal(err)
			}
			if got := strings.Contains(plan, "IndexScan(IDX_A"); got != tc.indexScan {
				t.Fatalf("%q: plan %s", tc.params, plan)
			}
		})
	}
}

// Every statement transaction, autocommit or explicit, read-only or not, DML or
// DDL, carries the connection's timeout and tags, as Java configures every
// connection transaction.
func TestDSN_TransactionOptionsReachEveryTransaction(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		params  string
		set     any // a SetOption(TRANSACTION_TIMEOUT) value, if not nil
		timeout []int64
	}{
		{"dsn_2500", "transaction_timeout=2500", nil, []int64{2500}},
		{"dsn_0", "transaction_timeout=0", nil, []int64{0}},
		{"dsn_-1", "transaction_timeout=-1", nil, nil},
		{"set_int", "", 2500, []int64{2500}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			db, rec := openLimitsDB(t, tc.params+"&transaction_tags=tenant&planner_statistics=true")
			ctx := context.Background()
			c, err := db.Conn(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer c.Close()
			if tc.set != nil {
				if err := c.Raw(func(dc any) error {
					return dc.(*embedded.EmbeddedConnection).SetOption(api.OptTransactionTimeout, tc.set)
				}); err != nil {
					t.Fatal(err)
				}
			}
			// The session's first statement bootstraps the shared catalog, which
			// Java does at engine start, outside any connection transaction.
			if _, err := countRows(ctx, c, "SELECT id FROM u"); err != nil {
				t.Fatal(err)
			}
			check := func(what string, txs []*recordedTx) {
				t.Helper()
				if len(txs) == 0 {
					t.Fatalf("%s opened no transaction", what)
				}
				for _, tx := range txs {
					tx.mu.Lock()
					ok := slices.Equal(tx.timeouts, tc.timeout) && slices.Equal(tx.tags, []string{"tenant"})
					tx.mu.Unlock()
					if !ok {
						t.Errorf("%s: %s", what, describeTxs(txs))
						return
					}
				}
			}
			for _, stmt := range []string{
				"SELECT id FROM t WHERE a > 2",
				"UPDATE t SET a = a + 1 WHERE id = 0",
				"INSERT INTO t VALUES (100, 1)",
				"CREATE SCHEMA /FRL/simdb/s2 WITH TEMPLATE tmpl",
			} {
				for run := 0; run < 2; run++ {
					stmt := strings.NewReplacer("100", fmt.Sprint(100+run), "s2", fmt.Sprintf("s%d", 2+run)).Replace(stmt)
					txs := rec.capture(func() {
						var err error
						if strings.HasPrefix(stmt, "SELECT") {
							_, err = countRows(ctx, c, stmt)
						} else {
							_, err = c.ExecContext(ctx, stmt)
						}
						if err != nil {
							t.Fatalf("%s: %v", stmt, err)
						}
					})
					check(fmt.Sprintf("%s (run %d)", stmt, run), txs)
				}
			}
			check("explicit transaction", rec.capture(func() {
				tx, err := c.BeginTx(ctx, nil)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := countRows(ctx, tx, "SELECT id FROM t"); err != nil {
					t.Fatal(err)
				}
				if _, err := tx.ExecContext(ctx, "UPDATE t SET a = a + 1 WHERE id = 1"); err != nil {
					t.Fatal(err)
				}
				if err := tx.Commit(); err != nil {
					t.Fatal(err)
				}
			}))
			if rec.reads.Load() == 0 {
				t.Fatal("no read-only transaction was observed; the statistics read is unpinned")
			}
		})
	}
}

// txOptionRecorder records the timeout and tag options set on every
// transaction the wrapped SimFDB creates, which SimFDB itself ignores.
type txOptionRecorder struct {
	*simfdb.SimDB
	mu    sync.Mutex
	txs   *[]*recordedTx
	reads atomic.Int32 // read-only transactions created while capturing
}

type recordedTx struct {
	mu       sync.Mutex
	timeouts []int64
	tags     []string
}

func describeTxs(txs []*recordedTx) string {
	parts := make([]string, len(txs))
	for i, tx := range txs {
		tx.mu.Lock()
		parts[i] = fmt.Sprintf("{timeouts=%v tags=%v}", tx.timeouts, tx.tags)
		tx.mu.Unlock()
	}
	return fmt.Sprintf("%d transactions %s", len(txs), strings.Join(parts, " "))
}

// capture returns the transactions created while fn runs.
func (r *txOptionRecorder) capture(fn func()) []*recordedTx {
	var txs []*recordedTx
	r.mu.Lock()
	r.txs = &txs
	r.mu.Unlock()
	fn()
	r.mu.Lock()
	r.txs = nil
	r.mu.Unlock()
	return txs
}

func (r *txOptionRecorder) wrap(tx fdb.WritableTransaction) fdb.WritableTransaction {
	return recordingTx{WritableTransaction: tx, rec: r.record(false)}
}

func (r *txOptionRecorder) record(read bool) *recordedTx {
	rt := &recordedTx{}
	r.mu.Lock()
	if r.txs != nil {
		*r.txs = append(*r.txs, rt)
		if read {
			r.reads.Add(1)
		}
	}
	r.mu.Unlock()
	return rt
}

func (r *txOptionRecorder) ReadTransact(fn func(fdb.ReadTransaction) (any, error)) (any, error) {
	return r.SimDB.ReadTransact(func(tx fdb.ReadTransaction) (any, error) {
		return fn(recordingReadTx{ReadTransaction: tx, rec: r.record(true)})
	})
}

type recordingReadTx struct {
	fdb.ReadTransaction
	rec *recordedTx
}

func (t recordingReadTx) Options() fdb.TransactionOptions {
	return recordingOptions{TransactionOptions: t.ReadTransaction.Options(), rec: t.rec}
}

func (r *txOptionRecorder) Transact(fn func(fdb.WritableTransaction) (any, error)) (any, error) {
	return r.SimDB.Transact(func(tx fdb.WritableTransaction) (any, error) { return fn(r.wrap(tx)) })
}

func (r *txOptionRecorder) CreateWritableTransaction() (fdb.WritableTransaction, error) {
	tx, err := r.SimDB.CreateWritableTransaction()
	if err != nil {
		return nil, err
	}
	return r.wrap(tx), nil
}

type recordingTx struct {
	fdb.WritableTransaction
	rec *recordedTx
}

func (t recordingTx) Options() fdb.TransactionOptions {
	return recordingOptions{TransactionOptions: t.WritableTransaction.Options(), rec: t.rec}
}

type recordingOptions struct {
	fdb.TransactionOptions
	rec *recordedTx
}

func (o recordingOptions) SetTimeout(ms int64) error {
	o.rec.mu.Lock()
	o.rec.timeouts = append(o.rec.timeouts, ms)
	o.rec.mu.Unlock()
	return o.TransactionOptions.SetTimeout(ms)
}

func (o recordingOptions) SetTag(tag string) error {
	o.rec.mu.Lock()
	o.rec.tags = append(o.rec.tags, tag)
	o.rec.mu.Unlock()
	return o.TransactionOptions.SetTag(tag)
}
