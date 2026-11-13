package embedded

// The GROUP BY duplicate-key gate compares expression keys by the Value they
// resolve to in value context (Java's visitGroupByItem), never by their text.

import (
	"context"
	"database/sql/driver"
	"errors"
	"io"
	"testing"

	"fdb.dev/pkg/dst"
	"fdb.dev/pkg/fdbgo/fdb/subspace"
	"fdb.dev/pkg/recordlayer"
	"fdb.dev/pkg/relational/api"
	"fdb.dev/pkg/relational/core/catalog"
	"fdb.dev/pkg/relational/core/ddl"
	"fdb.dev/pkg/relational/core/keyspace"
	"fdb.dev/pkg/simfdb"
)

func newDuplicateGateConnection(t *testing.T, seed uint64) *EmbeddedConnection {
	t.Helper()
	env := dst.NewSim(seed)
	env.Buggify = dst.DisabledBuggifier()
	fdbDB := recordlayer.NewFDBDatabaseWithBackend(simfdb.New(env)).SetEnv(env)
	fdbDB.SetStoreStateCache(recordlayer.NewMetaDataVersionStampStoreStateCache())
	ks := keyspace.New(subspace.Sub())
	cat, err := catalog.NewRecordLayerStoreCatalog(ks.CatalogSubspace())
	if err != nil {
		t.Fatalf("catalog: %v", err)
	}
	c := New("/FRL/SIMDB", fdbDB, cat, ddl.NewRecordLayerMetadataOperationsFactoryWithKeyspace(cat, ks), ks)
	ctx := context.Background()
	for _, stmt := range []string{
		"CREATE DATABASE /FRL/simdb",
		"CREATE SCHEMA TEMPLATE tmpl " +
			"CREATE TABLE T_G1 (id BIGINT, category STRING, amount BIGINT, PRIMARY KEY (id)) " +
			"CREATE TABLE T_G2 (id BIGINT, category STRING, PRIMARY KEY (id))",
		"CREATE SCHEMA /FRL/simdb/s WITH TEMPLATE tmpl",
	} {
		if _, err := c.ExecContext(ctx, stmt, nil); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}
	c.SetDefaultSchema("S")
	for _, stmt := range []string{
		"INSERT INTO T_G1 VALUES (1, 'a', 10), (2, 'a', 20), (3, 'b', 30)",
		"INSERT INTO T_G2 VALUES (7, 'a')",
	} {
		if _, err := c.ExecContext(ctx, stmt, nil); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}
	return c
}

// runDuplicateGateQuery returns the row count, or the error from planning or
// from iterating the rows.
func runDuplicateGateQuery(c *EmbeddedConnection, sql string, args []driver.NamedValue) (int, error) {
	rows, err := c.QueryContext(context.Background(), sql, args)
	if err != nil {
		return 0, err
	}
	defer rows.Close() //nolint:errcheck
	dest := make([]driver.Value, len(rows.Columns()))
	n := 0
	for {
		if err := rows.Next(dest); err != nil {
			if err == io.EOF {
				return n, nil
			}
			return n, err
		}
		n++
	}
}

func TestGroupByDuplicateGateComparesResolvedValues(t *testing.T) {
	t.Parallel()
	// Each statement text runs once on this connection, except where a test
	// checks what a cached plan answers.
	c := newDuplicateGateConnection(t, 5207)

	wantErr := func(t *testing.T, sql string, args []driver.NamedValue, code api.ErrorCode, msg string) {
		t.Helper()
		_, err := runDuplicateGateQuery(c, sql, args)
		var apiErr *api.Error
		if !errors.As(err, &apiErr) || apiErr.Code != code || apiErr.Message != msg {
			t.Fatalf("%s\n  got  %v\n  want %s %q", sql, err, code, msg)
		}
	}
	wantRows := func(t *testing.T, sql string, args []driver.NamedValue, want int) {
		t.Helper()
		n, err := runDuplicateGateQuery(c, sql, args)
		if err != nil || n != want {
			t.Fatalf("%s\n  got  %d rows, err %v\n  want %d rows", sql, n, err, want)
		}
	}

	t.Run("a duplicate comparison is refused whatever its spelling", func(t *testing.T) {
		// The message names the key's display, which only the build-time
		// gate reports; the later pull-up guard says "predicate".
		wantErr(t, "SELECT COUNT(*) FROM T_G1 GROUP BY amount=1, amount = 1", nil,
			api.ErrCodeAmbiguousColumn, "Ambiguous columns for amount = 1")
		wantErr(t, "SELECT COUNT(*) FROM T_G1 GROUP BY amount = 1, AMOUNT = 1", nil,
			api.ErrCodeAmbiguousColumn, "Ambiguous columns for AMOUNT = 1")
		wantErr(t, "SELECT COUNT(*) FROM T_G1 GROUP BY amount <> 1, amount != 1", nil,
			api.ErrCodeAmbiguousColumn, "Ambiguous columns for amount != 1")
		wantErr(t, "SELECT COUNT(*) FROM T_G1 GROUP BY amount = 1, amount = 01", nil,
			api.ErrCodeAmbiguousColumn, "Ambiguous columns for amount = 01")
		wantErr(t, "SELECT COUNT(*) FROM T_G1 GROUP BY (amount=1), (amount = 1)", nil,
			api.ErrCodeAmbiguousColumn, "Ambiguous columns for (amount = 1)")
		// Positive control on the arithmetic route the gate always typed.
		wantErr(t, "SELECT COUNT(*) FROM T_G1 GROUP BY amount+1, amount + 1", nil,
			api.ErrCodeAmbiguousColumn, "Ambiguous columns for amount + 1")
	})

	t.Run("duplicates are refused over every kind of source", func(t *testing.T) {
		// Inline VALUES, a VALUES leg, an unnest and a CTE: the gate resolves
		// the keys in each, so no source falls through to treating two equal
		// expression keys as distinct.
		for _, q := range []struct{ sql, display string }{
			{"SELECT COUNT(*) FROM VALUES (1), (2) AS v(a) GROUP BY a + 1, a + 1", "a + 1"},
			{"SELECT COUNT(*) FROM VALUES (1, 2) AS v(a, b) GROUP BY a = 1, a = 1", "a = 1"},
			{"SELECT COUNT(*) FROM T_G1, VALUES (1) AS v(a) GROUP BY a + amount, a + amount", "a + amount"},
			{"SELECT COUNT(*) FROM T_G1 t, t.category AS c GROUP BY amount + 1, amount + 1", "amount + 1"},
			{"WITH w AS (SELECT amount FROM T_G1) SELECT COUNT(*) FROM w GROUP BY amount + 1, amount + 1", "amount + 1"},
		} {
			wantErr(t, q.sql, nil, api.ErrCodeAmbiguousColumn, "Ambiguous columns for "+q.display)
		}
		// Control: distinct keys over the same source group.
		wantRows(t, "SELECT COUNT(*) FROM VALUES (1), (2) AS v(a) GROUP BY a + 1, a + 2", nil, 2)
	})

	t.Run("keys that differ in literal case or bound value are two keys", func(t *testing.T) {
		// 'a' and 'A' are different literals: two groups (a,a,b rows).
		wantRows(t, "SELECT COUNT(*) FROM T_G1 GROUP BY category = 'a', category = 'A'", nil, 2)
		wantRows(t, "SELECT category = 'a', category = 'A', COUNT(*) FROM T_G1 GROUP BY category = 'a', category = 'A'", nil, 2)
		// 10 and 20 bound to two `?`: (t,f), (f,t), (f,f).
		wantRows(t, "SELECT COUNT(*) FROM T_G1 GROUP BY amount = ?, amount = ?",
			[]driver.NamedValue{{Ordinal: 1, Value: int64(10)}, {Ordinal: 2, Value: int64(20)}}, 3)
		// Equal bound values make the keys equal: 42702, also with the plan
		// for 10/20 cached under the same text (the gate pins the values).
		wantErr(t, "SELECT COUNT(*) FROM T_G1 GROUP BY amount = ?, amount = ?",
			[]driver.NamedValue{{Ordinal: 1, Value: int64(10)}, {Ordinal: 2, Value: int64(10)}},
			api.ErrCodeAmbiguousColumn, "Ambiguous columns for amount = ?")
		wantRows(t, "SELECT COUNT(*) FROM T_G1 GROUP BY amount = ?, amount = ?",
			[]driver.NamedValue{{Ordinal: 1, Value: int64(20)}, {Ordinal: 2, Value: int64(30)}}, 3)
		wantRows(t, "SELECT COUNT(*) FROM T_G1 GROUP BY amount + ?, amount + ?",
			[]driver.NamedValue{{Ordinal: 1, Value: int64(1)}, {Ordinal: 2, Value: int64(2)}}, 3)
		wantErr(t, "SELECT COUNT(*) FROM T_G1 GROUP BY amount + ?, amount + ?",
			[]driver.NamedValue{{Ordinal: 1, Value: int64(1)}, {Ordinal: 2, Value: int64(1)}},
			api.ErrCodeAmbiguousColumn, "Ambiguous columns for amount + ?")
	})

	t.Run("an unresolvable key reports its own error, not 42702", func(t *testing.T) {
		wantErr(t, "SELECT COUNT(*) FROM T_G1 GROUP BY nosuch + 1, nosuch + 1", nil,
			api.ErrCodeUndefinedColumn, "Attempting to query non existing column NOSUCH")
		wantErr(t, "SELECT COUNT(*) FROM T_G1 GROUP BY NOSUCH = 1, nosuch = 1", nil,
			api.ErrCodeUndefinedColumn, "Attempting to query non existing column NOSUCH")
		wantErr(t, "SELECT COUNT(*) FROM T_G1 a JOIN T_G2 b ON a.id = b.id GROUP BY id + 1, id + 1", nil,
			api.ErrCodeAmbiguousColumn, "Ambiguous reference ID")
		// No semantic scope at all (a schema-qualified source Java also
		// refuses at FROM: LogicalOperator.java:217-219, SemanticAnalyzer
		// .java:544).
		wantErr(t, "SELECT COUNT(*) FROM S.T_G1 GROUP BY amount + 1, amount + 1", nil,
			api.ErrCodeUndefinedColumn, "Unknown reference S.T_G1")
	})

	t.Run("a duplicated key fails exactly as the key alone does", func(t *testing.T) {
		for _, pair := range [][2]string{
			{"SELECT COUNT(*) FROM T_G1 GROUP BY nosuchfn(amount), nosuchfn(amount)", "SELECT COUNT(*) FROM T_G1 GROUP BY nosuchfn(amount)"},
			{"SELECT COUNT(*) FROM T_G1 GROUP BY (SELECT 1), (SELECT 1)", "SELECT COUNT(*) FROM T_G1 GROUP BY (SELECT 1)"},
			{"SELECT COUNT(*) FROM T_G1 GROUP BY EXISTS (SELECT 1 FROM T_G2), EXISTS (SELECT 1 FROM T_G2)", "SELECT COUNT(*) FROM T_G1 GROUP BY EXISTS (SELECT 1 FROM T_G2)"},
		} {
			_, dupErr := runDuplicateGateQuery(c, pair[0], nil)
			_, oneErr := runDuplicateGateQuery(c, pair[1], nil)
			if dupErr == nil || oneErr == nil || dupErr.Error() != oneErr.Error() {
				t.Fatalf("%s\n  got  %v\n  want the single key's error %v", pair[0], dupErr, oneErr)
			}
			var apiErr *api.Error
			if errors.As(dupErr, &apiErr) && apiErr.Code == api.ErrCodeAmbiguousColumn {
				t.Fatalf("%s: a key that does not resolve was refused as a duplicate: %v", pair[0], dupErr)
			}
		}
	})
}
