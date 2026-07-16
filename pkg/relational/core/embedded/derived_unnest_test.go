package embedded

import (
	"errors"
	"strings"
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/properties"
	"fdb.dev/pkg/relational/api"
)

// CTE/derived-table unnest support. These are PLAN-LEVEL pins (plans
// successfully with the gathered FlatMap-over-Explode shape, or fails with
// the stated loud code) — the mechanism-agnostic disposition surface these
// shapes must satisfy (rows are FDB-pinned separately). The two P2a
// regressions are the load-bearing cases: a same-named base table must NEVER
// be classified against instead of the derived body.
const derivedUnnestSchema = `
CREATE TABLE td (id BIGINT, arr BIGINT ARRAY, sc BIGINT, PRIMARY KEY (id))
CREATE TABLE d (did BIGINT, arr BIGINT ARRAY, PRIMARY KEY (did))
CREATE TABLE inr (id BIGINT, arr BIGINT ARRAY, PRIMARY KEY (id))
`

func TestDerivedUnnest_Dispositions(t *testing.T) {
	t.Parallel()
	tmpl, err := buildSchemaTemplateFromDDL(derivedUnnestSchema)
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

	// Supported (Java plans them — parity restored).
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
	t.Run("scalar_source_invalidref", func(t *testing.T) {
		code(t, `SELECT x FROM (SELECT id AS arr FROM td) AS d, d.arr AS x`, api.ErrCodeInvalidColumnReference)
	})
	t.Run("computed_unsupported", func(t *testing.T) {
		code(t, `SELECT x FROM (SELECT id + 1 AS arr FROM td) AS d, d.arr AS x`, api.ErrCodeUnsupportedQuery)
	})
	t.Run("absent_undefined", func(t *testing.T) {
		code(t, `SELECT x FROM (SELECT id FROM td) AS d, d.nope AS x`, api.ErrCodeUndefinedColumn)
	})

	// P2a regressions — a same-named base table
	// must never be classified against instead of the derived body.
	t.Run("p2a_outer_alias_shadow", func(t *testing.T) {
		// Base table `d` has an ARRAY `arr`; the derived `d` projects scalar
		// `id AS arr`. Must WRONG_TYPE via the body (td.id scalar), never plan
		// against base d.arr.
		code(t, `SELECT x FROM (SELECT id AS arr FROM td) AS d, d.arr AS x`, api.ErrCodeInvalidColumnReference)
	})
	t.Run("p2a_nested_derived", func(t *testing.T) {
		// The body's source `inr` is a CTE (also a real base table with array
		// arr). Must decline (unsupported), never resolve through base inr.
		code(t, `WITH inr AS (SELECT id, arr FROM td) SELECT x FROM (SELECT arr FROM inr) AS d, d.arr AS x`, api.ErrCodeUnsupportedQuery)
	})
}

// TestDerivedUnnest_QualifiedPassthrough pins that a
// qualified-but-UNALIASED passthrough (`SELECT t.arr FROM t`) plans: the
// output column name is derived bare (qualifier stripped) so a `d.arr`
// reference maps to it. Java plans this; before the strip it over-rejected.
func TestDerivedUnnest_QualifiedPassthrough(t *testing.T) {
	t.Parallel()
	tmpl, err := buildSchemaTemplateFromDDL(derivedUnnestSchema)
	if err != nil {
		t.Fatalf("schema: %v", err)
	}
	stats := properties.FixedStatistics{Cardinality: 1_000_000}
	plan, err := PlanRecordQueryWithMetadata(
		`SELECT x FROM (SELECT td.arr FROM td) AS d, d.arr AS x`, tmpl.Underlying(), stats)
	if err != nil {
		t.Fatalf("qualified passthrough must plan, got %v", err)
	}
	if !strings.Contains(plan.Explain(), "inner=Explode") {
		t.Fatalf("plan lacks the Explode FlatMap: %s", plan.Explain())
	}
}
