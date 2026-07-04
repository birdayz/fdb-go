package embedded

import (
	"errors"
	"strings"
	"testing"

	"fdb.dev/pkg/relational/api"
	"fdb.dev/pkg/relational/core/query/logical"
)

// RFC-173 W4-left — the duplicate-FROM-alias ambiguity approximation is
// COLUMN-AWARE for every leg kind, tracks ALL prior sources under an alias,
// and never masks an undefined-table error. Each case here pins a review
// finding that shipped green:
//
//   - a three-source duplicate whose colliding pair does not involve the
//     FIRST source planned silently (last-leg-wins wrong rows);
//   - derived-table / CTE legs reported the garbage column "?" in a
//     user-facing message instead of their real derived columns;
//   - a duplicate alias naming an UNDEFINED table reported 42702 ambiguity
//     instead of the 42F01 undefined-table error.
func TestRFC173W4Left_DupAliasColumnAware(t *testing.T) {
	t.Parallel()
	ddl := `CREATE TABLE orders (id bigint, qid bigint, PRIMARY KEY(id))
	        CREATE TABLE quotes (qid bigint, note string, PRIMARY KEY(qid))
	        CREATE TABLE prices (pid bigint, amount bigint, PRIMARY KEY(pid))`
	tmpl, err := buildSchemaTemplateFromDDL(ddl)
	if err != nil {
		t.Fatalf("schema: %v", err)
	}
	md := tmpl.Underlying()

	for _, tc := range []struct {
		name, sql string
		wantCode  api.ErrorCode // "" = must NOT error with 42702 (any other outcome ok)
		wantMsg   string
	}{
		// Every later duplicate compares against EVERY prior source under
		// the alias: prices is column-disjoint from both, but orders/quotes
		// share QID — the first-source-only tracking missed this pair.
		{
			name:     "three_way_later_pair",
			sql:      `SELECT x.qid FROM prices AS x, orders AS x, quotes AS x`,
			wantCode: api.ErrCodeAmbiguousColumn,
			wantMsg:  "Ambiguous reference X.QID",
		},
		// Derived-table legs derive their output columns from the body —
		// the message names the real shared column, never "?".
		{
			name:     "derived_vs_base",
			sql:      `SELECT d.id FROM orders AS d, (SELECT id FROM orders) AS d`,
			wantCode: api.ErrCodeAmbiguousColumn,
			wantMsg:  "Ambiguous reference D.ID",
		},
		{
			name:     "derived_vs_derived",
			sql:      `SELECT * FROM (SELECT id FROM orders) AS d, (SELECT id FROM orders) AS d`,
			wantCode: api.ErrCodeAmbiguousColumn,
			wantMsg:  "Ambiguous reference D.ID",
		},
		{
			name:     "cte_dup",
			sql:      `WITH w AS (SELECT id FROM orders) SELECT * FROM w, w`,
			wantCode: api.ErrCodeAmbiguousColumn,
			wantMsg:  "Ambiguous reference W.ID",
		},
		// An alias-renamed derived column set: the derived leg's output is
		// the ALIAS (oid), which does not collide with quotes' columns —
		// disjoint duplicates pass the FROM check (Java answers these
		// per-attribute; the name model's qualified keys bind them).
		{
			name: "derived_aliased_disjoint",
			sql:  `SELECT a.note FROM quotes AS a, (SELECT id AS oid FROM orders) AS a`,
		},
		// ... and the same shape WITH a collision on the alias name.
		{
			name:     "derived_aliased_colliding",
			sql:      `SELECT a.note FROM quotes AS a, (SELECT id AS note FROM orders) AS a`,
			wantCode: api.ErrCodeAmbiguousColumn,
			wantMsg:  "Ambiguous reference A.NOTE",
		},
		// A duplicate alias naming an UNDEFINED table is table validation's
		// failure (42F01), not the ambiguity approximation's.
		{
			name:     "undefined_table_first",
			sql:      `SELECT * FROM missing AS a, orders AS a`,
			wantCode: api.ErrCodeUndefinedTable,
			wantMsg:  "does not exist",
		},
		{
			name:     "undefined_table_second",
			sql:      `SELECT * FROM orders AS a, missing AS a`,
			wantCode: api.ErrCodeUndefinedTable,
			wantMsg:  "does not exist",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, perr := PlanRecordQueryWithMetadataSchema(tc.sql, md, "test", nil)
			if tc.wantCode == "" {
				var apiErr *api.Error
				if errors.As(perr, &apiErr) && apiErr.Code == api.ErrCodeAmbiguousColumn {
					t.Fatalf("dup check over-rejected a disjoint duplicate: %v", perr)
				}
				return
			}
			var apiErr *api.Error
			if !errors.As(perr, &apiErr) {
				t.Fatalf("want %s error, got %v", tc.wantCode, perr)
			}
			if apiErr.Code != tc.wantCode {
				t.Fatalf("code = %s, want %s (err: %v)", apiErr.Code, tc.wantCode, perr)
			}
			if !strings.Contains(perr.Error(), tc.wantMsg) {
				t.Fatalf("message %q does not contain %q", perr.Error(), tc.wantMsg)
			}
		})
	}
}

// The both-sides-underivable corner: neither leg has a nameable column to
// report, the rejection names the alias alone — never the garbage "?" the
// retired sharedColumnUpper emitted. Built directly (SQL that reaches the
// planner with two underivable same-aliased legs also has derivable columns
// today, so the tree is hand-assembled).
func TestRFC173W4Left_DupAliasBothUnderivable(t *testing.T) {
	t.Parallel()
	ddl := `CREATE TABLE orders (id bigint, PRIMARY KEY(id))`
	tmpl, err := buildSchemaTemplateFromDDL(ddl)
	if err != nil {
		t.Fatalf("schema: %v", err)
	}
	md := tmpl.Underlying()

	// A LogicalUnnest body is outside fromLegColumnsUpper's shape set.
	underivableLeg := func() logical.LogicalOperator {
		return logical.NewCTE("d", &logical.LogicalUnnest{}, logical.NewScan("d", ""), false)
	}
	j := logical.NewJoin(underivableLeg(), underivableLeg(), logical.JoinInner, "")

	perr := rejectDuplicateUnnestAlias(j, md)
	var apiErr *api.Error
	if !errors.As(perr, &apiErr) || apiErr.Code != api.ErrCodeAmbiguousColumn {
		t.Fatalf("want 42702, got %v", perr)
	}
	if got := perr.Error(); !strings.Contains(got, "Ambiguous reference D") || strings.Contains(got, "?") {
		t.Fatalf("message %q: want alias-only rejection, never a %q column", got, "?")
	}
}
