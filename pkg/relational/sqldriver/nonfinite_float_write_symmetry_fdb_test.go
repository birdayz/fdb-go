package sqldriver_test

// What a FLOAT/DOUBLE column accepts must not depend on the SYNTAX that writes
// it. Four write paths reach the same column — INSERT … VALUES, UPDATE,
// INSERT … SELECT, and a bound `?` parameter — and every one of them must give
// the same answer for the same value.
//
// They did not. INSERT … VALUES and bound parameters rejected NaN and ±Infinity
// with 22023 while UPDATE and INSERT … SELECT stored them, because the two
// families route through different converters (functions.ConvertToProtoValue
// versus executor.goToProtoScalarValue) and only one carried the guard. The
// asymmetry was load-bearing in the wrong direction: the float-ordering
// differentials seed their NaN ladders through UPDATE precisely BECAUSE INSERT
// refused, which is a fixture built around a defect.
//
// JAVA IS THE AUTHORITY AND IT HAS NO SUCH GUARD. A grep of
// fdb-relational-core and fdb-relational-api for NaN/Infinity handling returns
// nothing; the whole of fdb-record-layer-core's NaN awareness is
// CastValue.java:139-169, which rejects NaN/Infinite for casts to INT, LONG and
// FLOAT — an explicit CAST, not a column write. A DOUBLE column in Java stores
// NaN and ±Infinity, and indexes them.
//
// The one rule that survives is the DOUBLE→FLOAT NARROWING range check, and it
// is not about non-finiteness: a FINITE double beyond ±MaxFloat32 becomes
// Infinity when narrowed, which is silent value corruption. NaN and ±Infinity
// narrow EXACTLY. So the rule is "reject what the narrowing would change", and
// ±Infinity is rejected by it only incidentally — as NumericValueOutOfRange
// (22003), which is what the executor path already answered, not 22023.
//
// (Java cannot even express that narrowing: PromoteValue's table has
// FLOAT_TO_DOUBLE but no DOUBLE_TO_FLOAT (PromoteValue.java:76-81), so a DOUBLE
// literal into a FLOAT column needs an explicit CAST there. Go accepting the
// narrowing is a pre-existing extension; this file only requires that the
// extension answer the same way through every syntax.)

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"testing"

	"fdb.dev/pkg/relational/api"
)

// nonFiniteWrite names one way of getting a value into a column and reports
// what happened, so the four paths can be compared as data rather than as four
// hand-written assertions that can drift apart.
type nonFiniteOutcome struct {
	err error
	got float64
	// hadValue is false when the write failed, so got is meaningless.
	hadValue bool
}

func (o nonFiniteOutcome) code() string {
	if o.err == nil {
		return ""
	}
	var apiErr *api.Error
	if errors.As(o.err, &apiErr) {
		return string(apiErr.Code)
	}
	return "non-api-error: " + o.err.Error()
}

// nonFiniteExprs are the SQL spellings of the three non-finite doubles.
// strconv.ParseFloat and Java's Double.parseDouble both accept exactly these
// three strings, so the literal form is portable.
var nonFiniteExprs = map[string]string{
	"nan":     "CAST('NaN' AS DOUBLE)",
	"pos_inf": "CAST('Infinity' AS DOUBLE)",
	"neg_inf": "CAST('-Infinity' AS DOUBLE)",
}

var nonFiniteValues = map[string]float64{
	"nan":     math.NaN(),
	"pos_inf": math.Inf(1),
	"neg_inf": math.Inf(-1),
}

func nonFiniteMatches(name string, v float64) bool {
	switch name {
	case "nan":
		return math.IsNaN(v)
	case "pos_inf":
		return math.IsInf(v, 1)
	case "neg_inf":
		return math.IsInf(v, -1)
	}
	return false
}

func TestFDB_NonFiniteFloatWrite_IsSyntaxIndependent(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	setup := openTestDB(t, "/testdb_nffw")
	mwjoMustExec(t, setup, ctx, "CREATE DATABASE /testdb_nffw")
	// The DOUBLE and FLOAT targets are separate tables with exactly two columns
	// each, because INSERT … SELECT with an explicit column list is rejected
	// outright (0AF00) and would mask the value question behind a syntax one.
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA TEMPLATE nffw "+
		"CREATE TABLE td (id BIGINT, d DOUBLE, PRIMARY KEY (id)) "+
		"CREATE TABLE tg (id BIGINT, g FLOAT, PRIMARY KEY (id)) "+
		// src feeds the INSERT … SELECT path, so that path carries a value read
		// out of a column rather than a re-parsed literal.
		"CREATE TABLE src (id BIGINT, d DOUBLE, PRIMARY KEY (id))")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA /testdb_nffw/s WITH TEMPLATE nffw")
	dsn := fmt.Sprintf("fdbsql:///testdb_nffw?cluster_file=%s&schema=s", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	// tbl is "td"/"tg", col is its value column — the two travel together.
	readBack := func(tbl, col string, id int64) (float64, bool) {
		var v sql.NullFloat64
		if qErr := db.QueryRowContext(ctx,
			fmt.Sprintf("SELECT %s FROM %s WHERE id = %d", col, tbl, id)).Scan(&v); qErr != nil {
			return 0, false
		}
		return v.Float64, v.Valid
	}

	// The four write paths, each producing the same shape of outcome.
	insertValues := func(tbl, col string, id int64, expr string) nonFiniteOutcome {
		_, e := db.ExecContext(ctx,
			fmt.Sprintf("INSERT INTO %s (id, %s) VALUES (%d, %s)", tbl, col, id, expr))
		if e != nil {
			return nonFiniteOutcome{err: e}
		}
		v, ok := readBack(tbl, col, id)
		return nonFiniteOutcome{got: v, hadValue: ok}
	}
	update := func(tbl, col string, id int64, expr string) nonFiniteOutcome {
		if _, e := db.ExecContext(ctx,
			fmt.Sprintf("INSERT INTO %s (id, %s) VALUES (%d, 1.0)", tbl, col, id)); e != nil {
			return nonFiniteOutcome{err: fmt.Errorf("seed row: %w", e)}
		}
		_, e := db.ExecContext(ctx,
			fmt.Sprintf("UPDATE %s SET %s = %s WHERE id = %d", tbl, col, expr, id))
		if e != nil {
			return nonFiniteOutcome{err: e}
		}
		v, ok := readBack(tbl, col, id)
		return nonFiniteOutcome{got: v, hadValue: ok}
	}
	// The source row is written with UPDATE, never INSERT … VALUES: seeding it
	// through the very path under test would make this path's outcome a
	// restatement of that one's rather than an independent measurement.
	insertSelect := func(tbl, col string, id int64, expr string) nonFiniteOutcome {
		if _, e := db.ExecContext(ctx,
			fmt.Sprintf("INSERT INTO src (id, d) VALUES (%d, 1.0)", id)); e != nil {
			return nonFiniteOutcome{err: fmt.Errorf("seed src: %w", e)}
		}
		if _, e := db.ExecContext(ctx,
			fmt.Sprintf("UPDATE src SET d = %s WHERE id = %d", expr, id)); e != nil {
			return nonFiniteOutcome{err: fmt.Errorf("seed src value: %w", e)}
		}
		_, e := db.ExecContext(ctx,
			fmt.Sprintf("INSERT INTO %s SELECT id, d FROM src WHERE id = %d", tbl, id))
		if e != nil {
			return nonFiniteOutcome{err: e}
		}
		v, ok := readBack(tbl, col, id)
		return nonFiniteOutcome{got: v, hadValue: ok}
	}
	boundParam := func(tbl, col string, id int64, v float64) nonFiniteOutcome {
		_, e := db.ExecContext(ctx,
			fmt.Sprintf("INSERT INTO %s (id, %s) VALUES (%d, ?)", tbl, col, id), v)
		if e != nil {
			return nonFiniteOutcome{err: e}
		}
		got, ok := readBack(tbl, col, id)
		return nonFiniteOutcome{got: got, hadValue: ok}
	}

	// A DOUBLE column holds every IEEE-754 value, through every syntax. This is
	// straight Java parity: nothing on Java's write or query path inspects a
	// double's finiteness.
	t.Run("double_column_accepts_every_syntax", func(t *testing.T) {
		id := int64(1000)
		for name, expr := range nonFiniteExprs {
			t.Run(name, func(t *testing.T) {
				paths := map[string]nonFiniteOutcome{}
				id++
				paths["insert_values"] = insertValues("td", "d", id, expr)
				id++
				paths["update"] = update("td", "d", id, expr)
				id++
				paths["insert_select"] = insertSelect("td", "d", id, expr)
				id++
				paths["bound_param"] = boundParam("td", "d", id, nonFiniteValues[name])
				for path, out := range paths {
					if out.err != nil {
						t.Errorf("DOUBLE column, %s of %s: rejected with %s (%v) — Java stores "+
							"NaN and +/-Infinity in a DOUBLE column and has no guard anywhere "+
							"on that path", path, name, out.code(), out.err)
						continue
					}
					if !out.hadValue || !nonFiniteMatches(name, out.got) {
						t.Errorf("DOUBLE column, %s of %s: stored %v (present=%v), want %s",
							path, name, out.got, out.hadValue, name)
					}
				}
			})
		}
	})

	// A FLOAT column narrows. NaN and +/-Infinity narrow EXACTLY, so the only
	// thing the narrowing check may reject is a FINITE double it would change —
	// and it must reject that identically through every syntax, with the
	// out-of-range code rather than an invalid-parameter one.
	t.Run("float_column_narrows_exactly", func(t *testing.T) {
		t.Run("nan_is_stored", func(t *testing.T) {
			id := int64(2000)
			paths := map[string]nonFiniteOutcome{}
			id++
			paths["insert_values"] = insertValues("tg", "g", id, nonFiniteExprs["nan"])
			id++
			paths["update"] = update("tg", "g", id, nonFiniteExprs["nan"])
			id++
			paths["insert_select"] = insertSelect("tg", "g", id, nonFiniteExprs["nan"])
			id++
			paths["bound_param"] = boundParam("tg", "g", id, math.NaN())
			for path, out := range paths {
				if out.err != nil {
					t.Errorf("FLOAT column, %s of NaN: rejected with %s (%v) — float64→float32 "+
						"narrowing of a NaN is exact, so there is nothing for a range check to "+
						"protect", path, out.code(), out.err)
					continue
				}
				if !out.hadValue || !math.IsNaN(out.got) {
					t.Errorf("FLOAT column, %s of NaN: stored %v (present=%v)", path, out.got, out.hadValue)
				}
			}
		})

		// A finite double past ±MaxFloat32 WOULD change under narrowing (it
		// becomes Infinity), so every path must refuse it — and refuse it the
		// same way. This is the half of the old guard that survives, and
		// without it the removal above would be a value-corruption hole.
		t.Run("finite_overflow_is_rejected_everywhere", func(t *testing.T) {
			id := int64(3000)
			paths := map[string]nonFiniteOutcome{}
			id++
			paths["insert_values"] = insertValues("tg", "g", id, "1.0e308")
			id++
			paths["update"] = update("tg", "g", id, "1.0e308")
			id++
			paths["insert_select"] = insertSelect("tg", "g", id, "1.0e308")
			id++
			paths["bound_param"] = boundParam("tg", "g", id, 1.0e308)
			for path, out := range paths {
				if out.err == nil {
					t.Errorf("FLOAT column, %s of 1.0e308: ACCEPTED, storing %v — a finite "+
						"double past MaxFloat32 becomes Infinity when narrowed, which is "+
						"silent value corruption", path, out.got)
					continue
				}
				if out.code() != string(api.ErrCodeNumericValueOutOfRange) {
					t.Errorf("FLOAT column, %s of 1.0e308: rejected with %s, want %s — an "+
						"overflowing narrowing is a range error, not an invalid parameter",
						path, out.code(), api.ErrCodeNumericValueOutOfRange)
				}
			}
		})
	})
}
