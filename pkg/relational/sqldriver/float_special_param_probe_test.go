package sqldriver_test

// A non-finite float64 parameter (NaN / ±Infinity) has no BARE SQL literal
// form: "%g" renders it as NaN/+Inf/-Inf, which the parser reads as an
// identifier and rejects with a confusing 42601. It does have a CAST form, and
// substituteParams emits it — 'NaN', 'Infinity' and '-Infinity' are exactly the
// strings both Go's strconv.ParseFloat and Java's Double.parseDouble accept.
//
// The earlier answer was to refuse the parameter with 22023, which turned a
// TRANSPORT limitation of this driver into a TYPE restriction: a DOUBLE column
// stores these values happily through INSERT … VALUES, UPDATE and
// INSERT … SELECT, and only the `?` path could not reach them.
//
// Each value gets its OWN primary key. The previous revision reused id=1 for
// all three, so the second and third writes were rejected as duplicate keys and
// the assertion "an error came back" held for a reason that had nothing to do
// with the parameter — two thirds of it was vacuous.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"strings"
	"testing"

	"fdb.dev/pkg/relational/api"
)

func TestFDB_FloatSpecialParamProbe(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	setup := openTestDB(t, "/testdb_fspecialp")
	mwjoMustExec(t, setup, ctx, "CREATE DATABASE /testdb_fspecialp")
	mwjoMustExec(t, setup, ctx,
		"CREATE SCHEMA TEMPLATE fspecialp CREATE TABLE t (id BIGINT, d DOUBLE, PRIMARY KEY (id))")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA /testdb_fspecialp/s WITH TEMPLATE fspecialp")
	dsn := fmt.Sprintf("fdbsql:///testdb_fspecialp?cluster_file=%s&schema=s", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	roundTrips := func(id int64, name string, v float64, ok func(float64) bool) {
		t.Run(name, func(t *testing.T) {
			if _, err := db.ExecContext(ctx,
				"INSERT INTO t (id, d) VALUES (?, ?)", id, v); err != nil {
				if strings.Contains(err.Error(), "42601") {
					t.Fatalf("%s param produced a SYNTAX error (%v) — the interpolated form "+
						"is not parseable", name, err)
				}
				t.Fatalf("%s param rejected: %v — a DOUBLE column carries every IEEE-754 "+
					"value through every other syntax", name, err)
			}
			var got float64
			if err := db.QueryRowContext(ctx,
				fmt.Sprintf("SELECT d FROM t WHERE id = %d", id)).Scan(&got); err != nil {
				t.Fatalf("%s readback: %v", name, err)
			}
			if !ok(got) {
				t.Errorf("%s param round-tripped as %v (bits %#016x)", name, got, math.Float64bits(got))
			}
		})
	}
	roundTrips(1, "nan", math.NaN(), func(v float64) bool { return math.IsNaN(v) })
	roundTrips(2, "pos_inf", math.Inf(1), func(v float64) bool { return math.IsInf(v, 1) })
	roundTrips(3, "neg_inf", math.Inf(-1), func(v float64) bool { return math.IsInf(v, -1) })

	t.Run("finite_floats_roundtrip", func(t *testing.T) {
		for i, v := range []float64{0.0, -1.5, 1e300, -1e-300, math.MaxFloat64} {
			id := 100 + i
			if _, err := db.ExecContext(ctx, "INSERT INTO t (id, d) VALUES (?, ?)", int64(id), v); err != nil {
				t.Fatalf("finite %v insert: %v", v, err)
			}
			var got float64
			if err := db.QueryRowContext(ctx, fmt.Sprintf("SELECT d FROM t WHERE id = %d", id)).Scan(&got); err != nil {
				t.Fatalf("scan: %v", err)
			}
			if got != v {
				t.Errorf("finite float %v round-trip = %v", v, got)
			}
		}
	})
}

// A bound NaN must either round-trip BIT-EXACT or be refused. What it must
// never do is arrive as different bits than were bound.
//
// A NaN carries a sign and a 51-bit payload, and those bits are OBSERVABLE:
// they come back on readback, and an aggregate index packs its grouping key
// from them, so two payloads are two physical entries (which is exactly what
// TestFDB_FloatAggregateIndexSplitsNaNPayloads pins). Rendering every NaN as
// CAST('NaN' AS DOUBLE) would silently replace the bound bits with the parse
// constant — the same class of defect the write-symmetry work removed, one
// level down: a stored value that depends on which syntax carried it.
//
// Bit-preservation is not reachable from here. Parameters become interpolated
// SQL TEXT (substituteParams is the only channel; there is no typed parameter
// path into the planner), and no literal in this grammar denotes an arbitrary
// double bit pattern — arithmetic reaches ±Infinity and one negative NaN, not a
// payload. So the contract is a narrow refusal, and this test states it as the
// DISJUNCTION rather than asserting an error, so a future typed-parameter path
// that starts preserving bits satisfies it without an edit.
func TestFDB_FloatSpecialParam_NaNBitsAreExactOrRefused(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	setup := openTestDB(t, "/testdb_fnanbits")
	mwjoMustExec(t, setup, ctx, "CREATE DATABASE /testdb_fnanbits")
	mwjoMustExec(t, setup, ctx,
		"CREATE SCHEMA TEMPLATE fnanbits CREATE TABLE t (id BIGINT, d DOUBLE, PRIMARY KEY (id))")
	mwjoMustExec(t, setup, ctx, "CREATE SCHEMA /testdb_fnanbits/s WITH TEMPLATE fnanbits")
	dsn := fmt.Sprintf("fdbsql:///testdb_fnanbits?cluster_file=%s&schema=s", clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	for i, tc := range []struct {
		name string
		bits uint64
	}{
		// What math.NaN() and the 'NaN' literal both produce. This one MUST be
		// accepted: refusing it would break the ordinary case, and it is the
		// pattern the write-symmetry test binds.
		{name: "go_default_nan", bits: 0x7ff8000000000001},
		// Sign bit set — (+Inf)+(-Inf). Storable through UPDATE arithmetic, so
		// the column can hold it; the question is only whether the BIND path
		// can carry it truthfully.
		{name: "negative_nan", bits: 0xfff8000000000000},
		// An arbitrary payload.
		{name: "payload_nan", bits: 0x7ff123456789abcd},
	} {
		t.Run(tc.name, func(t *testing.T) {
			id := int64(500 + i)
			bound := math.Float64frombits(tc.bits)
			_, execErr := db.ExecContext(ctx,
				"INSERT INTO t (id, d) VALUES (?, ?)", id, bound)
			if execErr != nil {
				// Refusal is allowed — but it must be a clear typed rejection,
				// not the confusing 42601 that an unrenderable literal produces.
				if strings.Contains(execErr.Error(), "42601") {
					t.Fatalf("%s produced a SYNTAX error (%v); a refusal must say what it "+
						"cannot represent", tc.name, execErr)
				}
				var apiErr *api.Error
				if !errors.As(execErr, &apiErr) || apiErr.Code != api.ErrCodeInvalidParameter {
					t.Fatalf("%s refused with %v, want a typed %s", tc.name, execErr,
						api.ErrCodeInvalidParameter)
				}
				return
			}
			var got float64
			if scanErr := db.QueryRowContext(ctx,
				fmt.Sprintf("SELECT d FROM t WHERE id = %d", id)).Scan(&got); scanErr != nil {
				t.Fatalf("%s readback: %v", tc.name, scanErr)
			}
			if math.Float64bits(got) != tc.bits {
				t.Errorf("%s was ACCEPTED but stored %#016x instead of the bound %#016x. A bound "+
					"value that is neither preserved nor refused is silent corruption: the sign "+
					"and payload are observable on readback and in index keys",
					tc.name, math.Float64bits(got), tc.bits)
			}
		})
	}

	// The accepted pattern is not merely "some NaN" — it must be the bound bits.
	t.Run("accepted_default_nan_is_bit_exact", func(t *testing.T) {
		if _, e := db.ExecContext(ctx,
			"INSERT INTO t (id, d) VALUES (?, ?)", int64(600), math.NaN()); e != nil {
			t.Fatalf("math.NaN() must bind — it is the pattern the 'NaN' literal parses to: %v", e)
		}
		var got float64
		if e := db.QueryRowContext(ctx,
			"SELECT d FROM t WHERE id = 600").Scan(&got); e != nil {
			t.Fatalf("readback: %v", e)
		}
		if math.Float64bits(got) != math.Float64bits(math.NaN()) {
			t.Errorf("math.NaN() round-tripped as %#016x, want %#016x",
				math.Float64bits(got), math.Float64bits(math.NaN()))
		}
	})
}
