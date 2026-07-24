package embedded

import (
	"errors"
	"strings"
	"testing"

	"fdb.dev/pkg/relational/api"
)

const correlatedScalarCardinalitySchema = `
CREATE TABLE PARENT (
  id BIGINT NOT NULL,
  wanted BIGINT,
  PRIMARY KEY (id)
)
CREATE TABLE CHILD (
  id BIGINT NOT NULL,
  parent_id BIGINT,
  grp STRING,
  val BIGINT,
  PRIMARY KEY (id)
)
`

func requirePlanSQLState(t *testing.T, err error, want api.ErrorCode) {
	t.Helper()
	if err == nil {
		t.Fatalf("error = nil, want SQLSTATE %s", want)
	}
	var apiErr *api.Error
	if !errors.As(err, &apiErr) {
		t.Fatalf("error = %T %v, want *api.Error SQLSTATE %s", err, err, want)
	}
	if apiErr.Code != want {
		t.Fatalf("SQLSTATE = %s, want %s (error: %v)", apiErr.Code, want, err)
	}
}

func TestPlanHarness_CorrelatedScalarCardinalityConsumers(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		sql        string
		wantStrict bool
	}{
		{
			name:       "projection_nonaggregate",
			sql:        "SELECT (SELECT c.val FROM child c WHERE c.parent_id = p.id) FROM parent p",
			wantStrict: true,
		},
		{
			name:       "projection_simplified_away_correlation",
			sql:        "SELECT (SELECT c.val FROM child c WHERE c.parent_id = p.id OR 1 = 1) FROM parent p",
			wantStrict: true,
		},
		{
			name:       "projection_grouped_aggregate",
			sql:        "SELECT (SELECT SUM(c.val) FROM child c WHERE c.parent_id = p.id GROUP BY c.grp) FROM parent p",
			wantStrict: true,
		},
		{
			name:       "where_comparison_nonaggregate",
			sql:        "SELECT p.id FROM parent p WHERE p.wanted = (SELECT c.val FROM child c WHERE c.parent_id = p.id)",
			wantStrict: true,
		},
		{
			name:       "where_comparison_simplified_away_correlation",
			sql:        "SELECT p.id FROM parent p WHERE p.wanted = (SELECT c.val FROM child c WHERE c.parent_id = p.id OR 1 = 1)",
			wantStrict: true,
		},
		{
			name:       "where_comparison_grouped_aggregate",
			sql:        "SELECT p.id FROM parent p WHERE p.wanted = (SELECT SUM(c.val) FROM child c WHERE c.parent_id = p.id GROUP BY c.grp)",
			wantStrict: true,
		},
		{
			name:       "projection_explicit_limit_one",
			sql:        "SELECT (SELECT c.val FROM child c WHERE c.parent_id = p.id ORDER BY c.val DESC LIMIT 1) FROM parent p",
			wantStrict: false,
		},
		{
			name:       "where_explicit_limit_one",
			sql:        "SELECT p.id FROM parent p WHERE p.wanted = (SELECT c.val FROM child c WHERE c.parent_id = p.id ORDER BY c.val DESC LIMIT 1)",
			wantStrict: false,
		},
		{
			name:       "projection_global_aggregate_limit_five",
			sql:        "SELECT (SELECT MAX(c.val) FROM child c WHERE c.parent_id = p.id LIMIT 5) FROM parent p",
			wantStrict: false,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			plan, err := PlanQueryForTest(tc.sql, correlatedScalarCardinalitySchema, nil)
			if err != nil {
				t.Fatalf("plan: %v", err)
			}
			explain := plan
			if !strings.Contains(explain, "FlatMap") {
				t.Fatalf("plan must materialize the scalar per outer row through FlatMap: %s", explain)
			}
			gotStrict := strings.Contains(explain, "StrictFirstOrDefault")
			if gotStrict != tc.wantStrict {
				t.Fatalf("StrictFirstOrDefault present = %v, want %v; plan: %s", gotStrict, tc.wantStrict, explain)
			}
		})
	}
}

func TestPlanHarness_CorrelatedScalarCorrectOrLoudGuards(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		sql  string
	}{
		{
			name: "projection_limit_greater_than_one",
			sql:  "SELECT (SELECT c.val FROM child c WHERE c.parent_id = p.id LIMIT 5) FROM parent p",
		},
		{
			name: "where_limit_greater_than_one",
			sql:  "SELECT p.id FROM parent p WHERE p.wanted = (SELECT c.val FROM child c WHERE c.parent_id = p.id LIMIT 2)",
		},
		{
			name: "distinct",
			sql:  "SELECT (SELECT DISTINCT c.val FROM child c WHERE c.parent_id = p.id) FROM parent p",
		},
		{
			name: "window",
			sql:  "SELECT (SELECT SUM(c.val) OVER () FROM child c WHERE c.parent_id = p.id) FROM parent p",
		},
		{
			name: "unresolved_limit",
			sql:  "SELECT (SELECT c.val FROM child c WHERE c.parent_id = p.id LIMIT ?) FROM parent p",
		},
		{
			name: "unresolved_offset",
			sql:  "SELECT (SELECT c.val FROM child c WHERE c.parent_id = p.id LIMIT 1 OFFSET ?) FROM parent p",
		},
		{
			name: "group_key_only_having",
			sql:  "SELECT (SELECT c.grp FROM child c WHERE c.parent_id = p.id GROUP BY c.grp HAVING c.grp = 'A') FROM parent p",
		},
		{
			name: "projected_exists_with_where_scalar",
			sql:  "SELECT EXISTS (SELECT 1 FROM child m WHERE m.parent_id = p.id) FROM parent p WHERE p.wanted = (SELECT c.val FROM child c WHERE c.parent_id = p.id LIMIT 1)",
		},
		{
			name: "select_and_where_correlated_scalars",
			sql:  "SELECT (SELECT c.val FROM child c WHERE c.parent_id = p.id LIMIT 1) FROM parent p WHERE p.wanted = (SELECT d.val FROM child d WHERE d.parent_id = p.id LIMIT 1)",
		},
		{
			name: "exists_and_correlated_scalar_in_where",
			sql:  "SELECT p.id FROM parent p WHERE EXISTS (SELECT 1 FROM child m WHERE m.parent_id = p.id) AND p.wanted = (SELECT c.val FROM child c WHERE c.parent_id = p.id LIMIT 1)",
		},
		{
			name: "two_correlated_scalars_in_where",
			sql:  "SELECT p.id FROM parent p WHERE p.wanted = (SELECT c.val FROM child c WHERE c.parent_id = p.id LIMIT 1) AND p.wanted = (SELECT d.val FROM child d WHERE d.parent_id = p.id LIMIT 1)",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := PlanQueryForTest(tc.sql, correlatedScalarCardinalitySchema, nil)
			requirePlanSQLState(t, err, api.ErrCodeUnsupportedQuery)
		})
	}
}
