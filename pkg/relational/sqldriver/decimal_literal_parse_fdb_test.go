package sqldriver_test

import (
	"context"
	"database/sql"
	"errors"
	"math"
	"strings"
	"testing"

	"fdb.dev/pkg/relational/api"
)

// A decimal literal is parsed as Java's ParseHelpers.parseDecimal parses it,
// wherever it stands (conformance/window_options_conformance_test.go): a text
// Long/Integer.parseLong refuses — an exponent without '.', or a value beyond
// its width — is the NumberFormatException, unclassified; a REAL with '.' that
// overflows is an infinity.
func TestFDB_DecimalLiteralParsesAsJava(t *testing.T) {
	t.Parallel()
	db := setupPlanShapeDB(t, "declit", `CREATE TABLE t (id BIGINT, d DOUBLE, PRIMARY KEY (id))`)
	ctx := context.Background()
	if _, err := db.ExecContext(ctx, `INSERT INTO t VALUES (1, 1.0)`); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct{ sql, input string }{
		{`SELECT 1e5 FROM t`, "1e5"},
		{`SELECT id FROM t WHERE id = 1e5`, "1e5"},
		{`SELECT id FROM t WHERE id IN (1, 1e5)`, "1e5"},
		{`SELECT -1e5 FROM t`, "-1e5"},
		{`SELECT 1e5f FROM t`, "1e5f"},
		{`SELECT 1e309 FROM t`, "1e309"},
		{`SELECT 3000000000I FROM t`, "3000000000"},
		{`SELECT -2147483649I FROM t`, "-2147483649"},
		{`SELECT 99999999999999999999 FROM t`, "99999999999999999999"},
		{`SELECT id FROM t WHERE id < 99999999999999999999`, "99999999999999999999"},
		{`SELECT -9223372036854775809 FROM t`, "-9223372036854775809"},
		{`SELECT 9223372036854775808L FROM t`, "9223372036854775808"},
		{`EXPLAIN SELECT 1e5 FROM t`, "1e5"},
		{`INSERT INTO t VALUES (2, 1e5)`, "1e5"},
	} {
		err := runStatement(ctx, db, c.sql)
		var apiErr *api.Error
		if !errors.As(err, &apiErr) || apiErr.Code != api.ErrCodeUnknown || apiErr.Message != `For input string: "`+c.input+`"` {
			t.Fatalf("%s: error %v, want XXXXX For input string: %q", c.sql, err, c.input)
		}
	}
	for _, c := range []struct {
		sql  string
		want float64
	}{
		{`SELECT 1.0e400 FROM t`, math.Inf(1)},
		{`SELECT -1.0e400 FROM t`, math.Inf(-1)},
		{`SELECT 1.0e40f FROM t`, math.Inf(1)},
		{`SELECT 1.5e3 FROM t`, 1500},
		{`SELECT -9223372036854775808 FROM t`, math.MinInt64},
		{`SELECT -2147483648I FROM t`, math.MinInt32},
	} {
		var got float64
		if err := db.QueryRowContext(ctx, c.sql).Scan(&got); err != nil {
			t.Fatalf("%s: %v", c.sql, err)
		}
		if got != c.want {
			t.Fatalf("%s: %v, want %v", c.sql, got, c.want)
		}
	}
}

// runStatement executes an INSERT, and runs any other statement as a query
// whose rows are drained.
func runStatement(ctx context.Context, db *sql.DB, statement string) error {
	if strings.HasPrefix(statement, "INSERT") {
		_, err := db.ExecContext(ctx, statement)
		return err
	}
	rows, err := db.QueryContext(ctx, statement)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
	}
	return rows.Err()
}
