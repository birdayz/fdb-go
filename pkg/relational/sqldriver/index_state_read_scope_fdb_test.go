package sqldriver_test

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"

	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/fdbgo/fdb/subspace"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	"fdb.dev/pkg/recordlayer"
	"fdb.dev/pkg/relational/api"
	"fdb.dev/pkg/relational/core/keyspace"
)

// Index-state read conflicts are the target's (RFC-257 WS-E 6.4, measured by
// Java's indexStateReadScopeProbe): a reader's explicit transaction conflicts
// with a state change of an index it SCANNED, and commits through a state
// change of an index it did not use, including under a record scan. Planning
// and per-page plan revalidation read the loaded states without a conflict;
// before, both took a conflict key for every index of the metadata, so the
// reader aborted on any index's state change.
//
// The state change is the probe's: a raw write of the index's state key in
// its own transaction, never MarkIndexDisabled (which also clears the index's
// data and would conflict on the data range instead).
func TestFDB_IndexStateReadScope(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	for _, tc := range []struct {
		name, query, changed string
		explainHas           string
		conflict             bool
	}{
		{"scanned_index", "SELECT id FROM X WHERE v = 5", "X_V", "IndexScan(X_V", true},
		{"unused_index", "SELECT id FROM X WHERE v = 5", "X_W", "IndexScan(X_V", false},
		{"record_scan", "SELECT id FROM X WHERE id = 1", "X_V", "Scan(X", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dbName := "/FRL/TESTDB_INDEX_STATE_SCOPE_" + strings.ToUpper(tc.name)
			setup := openTestDB(t, dbName)
			for _, stmt := range []string{
				"CREATE DATABASE " + dbName,
				"CREATE SCHEMA TEMPLATE index_state_scope_" + tc.name + " " +
					"CREATE TABLE X (id BIGINT, v BIGINT, w BIGINT, PRIMARY KEY (id)) " +
					"CREATE INDEX X_V ON X(v) CREATE INDEX X_W ON X(w) " +
					"CREATE TABLE WR (id BIGINT, PRIMARY KEY (id))",
				"CREATE SCHEMA " + dbName + "/S WITH TEMPLATE index_state_scope_" + tc.name,
			} {
				if _, err := setup.ExecContext(ctx, stmt); err != nil {
					t.Fatalf("%s: %v", stmt, err)
				}
			}
			db, err := sql.Open("fdbsql", fmt.Sprintf("fdbsql://%s?cluster_file=%s&schema=S", dbName, clusterFilePath))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { db.Close() })
			if _, err := db.ExecContext(ctx, "INSERT INTO X VALUES (1, 5, 6), (2, 7, 8)"); err != nil {
				t.Fatal(err)
			}
			var plan string
			if err := db.QueryRowContext(ctx, "EXPLAIN "+tc.query).Scan(&plan); err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(plan, tc.explainHas) {
				t.Fatalf("plan %s, want %s: the case would not test the scope it names", plan, tc.explainHas)
			}

			rawDB, err := fdb.OpenDatabase(clusterFilePath)
			if err != nil {
				t.Fatal(err)
			}
			ss, err := keyspace.New(subspace.Sub()).LookupSchemaSubspace(ctx, recordlayer.NewFDBDatabase(rawDB), dbName, "S")
			if err != nil {
				t.Fatal(err)
			}
			stateKey := ss.Sub(recordlayer.IndexStateSpaceKey).Pack(tuple.Tuple{tc.changed})
			setState := func(disabled bool) {
				if _, err := rawDB.Transact(func(tr fdb.WritableTransaction) (any, error) {
					if disabled {
						tr.Set(stateKey, tuple.Tuple{int64(recordlayer.IndexStateDisabled)}.Pack())
					} else {
						tr.Clear(stateKey)
					}
					return nil, nil
				}); err != nil {
					t.Fatal(err)
				}
			}
			// An attempt pre-empted by the transaction time limit starts
			// again from a readable index, so it reads by the same plan.
			retryTx(t, db, txRetryOpts{BeforeAttempt: func(int) { setState(false) }}, func(a txAttempt) error {
				var id int64
				if err := a.tx.QueryRowContext(ctx, tc.query).Scan(&id); err != nil {
					return err
				}
				if id != 1 {
					t.Fatalf("read id %d, want 1", id)
				}
				setState(true)
				if _, err := a.tx.ExecContext(ctx, "INSERT INTO WR VALUES (1)"); err != nil {
					return err
				}
				err := a.tx.Commit()
				if api.IsTransactionTimeLimit(err) {
					return err
				}
				if tc.conflict {
					assertSerializationFailure(t, err)
				} else if err != nil {
					t.Fatalf("commit after a state change of %s, which the read did not scan: %v", tc.changed, err)
				}
				return nil
			})
		})
	}
}
