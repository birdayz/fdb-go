package rowdiff

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"

	"fdb.dev/pkg/dst"
	"fdb.dev/pkg/recordlayer"
	"fdb.dev/pkg/relational/sqldriver"
	"fdb.dev/pkg/simfdb"
)

// TestLoadFixtureReplaysOnlyALostWindow pins loadFixture's retry rule against
// injected commit faults: a transaction that outlived FDB's window (1007)
// committed nothing and is replayed, and the fixture then holds every row
// exactly once; a conflict (1020) and an unknown commit result (1021) are
// returned, never replayed, because replaying them could hide a real error or
// duplicate rows.
func TestLoadFixtureReplaysOnlyALostWindow(t *testing.T) {
	t.Parallel()
	const rows = 50
	var b strings.Builder
	b.WriteString("INSERT INTO T VALUES ")
	for i := range rows {
		if i > 0 {
			b.WriteString(", ")
		}
		fmt.Fprintf(&b, "(%d)", i)
	}
	insert := b.String()

	for _, tc := range []struct {
		name     string
		inject   int
		wantErr  bool
		wantRows int
	}{
		{"window_lost_is_replayed", 1007, false, rows},
		{"conflict_is_returned", 1020, true, 0},
		{"unknown_commit_is_returned", simfdb.CommitUnknownApplied, true, -1},
		{"clean", 0, false, rows},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			env := dst.NewSim(7)
			env.Buggify = dst.DisabledBuggifier()
			sim := simfdb.New(env)
			backend := recordlayer.NewFDBDatabaseWithBackend(sim).SetEnv(env)
			backend.SetStoreStateCache(recordlayer.NewMetaDataVersionStampStoreStateCache())
			key := "sim://" + t.Name()
			t.Cleanup(sqldriver.RegisterBackend(key, backend))
			open := func(schema string) *sql.DB {
				dsn := "fdbsql:///FRL/FIXTURE?cluster_file=" + key
				if schema != "" {
					dsn += "&schema=" + schema
				}
				db, err := sql.Open("fdbsql", dsn)
				if err != nil {
					t.Fatal(err)
				}
				db.SetMaxOpenConns(1)
				t.Cleanup(func() { _ = db.Close() })
				return db
			}
			ctx := context.Background()
			setup := open("")
			for _, ddl := range []string{
				"CREATE DATABASE /FRL/fixture",
				"CREATE SCHEMA TEMPLATE fixture_tmpl CREATE TABLE T (id BIGINT, PRIMARY KEY (id))",
				"CREATE SCHEMA /FRL/fixture/S WITH TEMPLATE fixture_tmpl",
			} {
				if _, err := setup.ExecContext(ctx, ddl); err != nil {
					t.Fatal(err)
				}
			}
			db := open("S")
			// Warm the connection's catalog bootstrap so the injected fault
			// lands on the fixture's own commit.
			var n int
			if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM T").Scan(&n); err != nil || n != 0 {
				t.Fatalf("initial population %d, %v", n, err)
			}
			if tc.inject != 0 {
				sim.InjectOnce(tc.inject)
			}
			err := loadFixture(ctx, db, insert)
			if (err != nil) != tc.wantErr {
				t.Fatalf("loadFixture err = %v, want error %v", err, tc.wantErr)
			}
			if tc.wantRows < 0 {
				return // 1021-applied may or may not have landed; only the refusal to replay is pinned
			}
			if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM T").Scan(&n); err != nil {
				t.Fatal(err)
			}
			if n != tc.wantRows {
				t.Fatalf("fixture holds %d rows, want %d", n, tc.wantRows)
			}
		})
	}
}
