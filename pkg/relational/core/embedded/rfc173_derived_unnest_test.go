package embedded

import (
	"errors"
	"strings"
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/properties"
	"fdb.dev/pkg/relational/api"
)

// RFC-173 unnest-residual class 3 — CTE/derived-table unnest owner. These are
// PLAN-LEVEL pins (plans successfully with the gathered FlatMap-over-Explode
// shape, or fails with the stated loud code) — the mechanism-agnostic
// disposition surface the design ruling requires (rows are FDB-pinned
// separately). The two P2a regressions are the load-bearing cases: a same-named
// base table must NEVER be classified against instead of the derived body.
const rfc173DerivedSchema = `
CREATE TABLE td (id BIGINT, arr BIGINT ARRAY, sc BIGINT, PRIMARY KEY (id))
CREATE TABLE d (did BIGINT, arr BIGINT ARRAY, PRIMARY KEY (did))
CREATE TABLE inr (id BIGINT, arr BIGINT ARRAY, PRIMARY KEY (id))
`

func TestRFC173DerivedUnnest_Dispositions(t *testing.T) {
	t.Parallel()
	tmpl, err := buildSchemaTemplateFromDDL(rfc173DerivedSchema)
	if err != nil {
		t.Fatalf("schema: %v", err)
	}
	stats := properties.FixedStatistics{Cardinality: 1_000_000}

	plans := func(t *testing.T, sql string) {
		t.Helper()
		plan, err := PlanRecordQueryWithMetadata(sql, tmpl.Underlying(), stats)
		if err != nil {
			t.Fatalf("want a plan, got err %v\n  sql: %s", err, sql)
		}
		ex := plan.Explain()
		// Gathered shape: a FlatMap whose inner is the Explode, over the
		// derived leg's projection.
		if !strings.Contains(ex, "inner=Explode") {
			t.Fatalf("plan lacks the Explode FlatMap: %s\n  sql: %s", ex, sql)
		}
	}
	code := func(t *testing.T, sql string, want api.ErrorCode) {
		t.Helper()
		_, err := PlanRecordQueryWithMetadata(sql, tmpl.Underlying(), stats)
		if err == nil {
			t.Fatalf("want error %s, got a plan\n  sql: %s", want, sql)
		}
		var apiErr *api.Error
		if !errors.As(err, &apiErr) || apiErr.Code != want {
			t.Fatalf("err = %v (%T), want %s\n  sql: %s", err, err, want, sql)
		}
	}

	// Supported (were 0A000 pre-slice; Java plans them — parity restored).
	t.Run("derived_passthrough", func(t *testing.T) {
		plans(t, `SELECT d.id, x FROM (SELECT id, arr FROM td) AS d, d.arr AS x`)
	})
	t.Run("derived_alias_rename", func(t *testing.T) {
		plans(t, `SELECT d.id, x FROM (SELECT id, arr AS a FROM td) AS d, d.a AS x`)
	})
	t.Run("cte_passthrough", func(t *testing.T) {
		plans(t, `WITH c AS (SELECT id, arr FROM td) SELECT c.id, x FROM c, c.arr AS x`)
	})
	t.Run("derived_with_where", func(t *testing.T) {
		plans(t, `SELECT x FROM (SELECT id, arr FROM td WHERE sc > 0) AS d, d.arr AS x`)
	})

	// Declined loudly (honest unsupported / wrong-type / absent — never
	// silent-wrong).
	t.Run("scalar_source_wrongtype", func(t *testing.T) {
		code(t, `SELECT x FROM (SELECT id AS arr FROM td) AS d, d.arr AS x`, api.ErrCodeWrongObjectType)
	})
	t.Run("computed_unsupported", func(t *testing.T) {
		code(t, `SELECT x FROM (SELECT id + 1 AS arr FROM td) AS d, d.arr AS x`, api.ErrCodeUnsupportedQuery)
	})
	t.Run("absent_undefined", func(t *testing.T) {
		code(t, `SELECT x FROM (SELECT id FROM td) AS d, d.nope AS x`, api.ErrCodeUndefinedColumn)
	})

	// P2a regressions (design ruling 3, condition 3) — a same-named base table
	// must never be classified against instead of the derived body.
	t.Run("p2a_outer_alias_shadow", func(t *testing.T) {
		// Base table `d` has an ARRAY `arr`; the derived `d` projects scalar
		// `id AS arr`. Must WRONG_TYPE via the body (td.id scalar), never plan
		// against base d.arr.
		code(t, `SELECT x FROM (SELECT id AS arr FROM td) AS d, d.arr AS x`, api.ErrCodeWrongObjectType)
	})
	t.Run("p2a_nested_derived", func(t *testing.T) {
		// The body's source `inr` is a CTE (also a real base table with array
		// arr). Must decline (unsupported), never resolve through base inr.
		code(t, `WITH inr AS (SELECT id, arr FROM td) SELECT x FROM (SELECT arr FROM inr) AS d, d.arr AS x`, api.ErrCodeUnsupportedQuery)
	})
}
