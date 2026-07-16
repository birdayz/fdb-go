// Regression pins for the strict scenario loader (RFC-180 Y5/Y6).
//
// The corpus shipped for months with exec:/rowcount:/count:/rows_affected:
// stanzas the Test struct didn't have — the YAML unmarshaller silently
// DROPPED the keys, the "statements" ran empty, and post-DML assertions
// verified nothing while green. Each test here is red against the lenient
// loader: reverting KnownFields(true) or any validation re-opens the hole.

package yamsql

import (
	"strings"
	"testing"
)

const strictTestHeader = `name: strict
schema_template: |
  CREATE TABLE t (id BIGINT NOT NULL, PRIMARY KEY (id))
tests:
`

func loadExpectingError(t *testing.T, body, wantSubstr string) {
	t.Helper()
	path := writeTempYaml(t, strictTestHeader+body)
	_, err := Load(path)
	if err == nil {
		t.Fatalf("want load error containing %q, got nil", wantSubstr)
	}
	if !strings.Contains(err.Error(), wantSubstr) {
		t.Errorf("want error containing %q, got: %v", wantSubstr, err)
	}
}

// TestLoad_RejectsUnknownFields is THE pin for the silent-drop hole: an
// unrecognized key (the historical exec:/rowcount:/count:/rows_affected:
// class, or any future typo) must be a load error, never a no-op.
func TestLoad_RejectsUnknownFields(t *testing.T) {
	t.Parallel()
	loadExpectingError(t, `  - query: SELECT id FROM t
    rows_affected: 1
`, "field rows_affected not found")
}

func TestLoad_ExecAndQueryMutuallyExclusive(t *testing.T) {
	t.Parallel()
	loadExpectingError(t, `  - query: SELECT id FROM t
    exec: DELETE FROM t
`, "mutually exclusive")
}

func TestLoad_RequiresQueryOrExec(t *testing.T) {
	t.Parallel()
	loadExpectingError(t, `  - rows:
      - [1]
`, "one of query/exec is required")
}

func TestLoad_ExecRejectsSelect(t *testing.T) {
	t.Parallel()
	loadExpectingError(t, `  - exec: SELECT id FROM t
`, "exec is for non-query statements")
}

func TestLoad_ExecRejectsRows(t *testing.T) {
	t.Parallel()
	loadExpectingError(t, `  - exec: DELETE FROM t
    rows:
      - [1]
`, "exec cannot expect rows")
}

func TestLoad_RowcountOnQueryRejected(t *testing.T) {
	t.Parallel()
	loadExpectingError(t, `  - query: SELECT id FROM t
    rowcount: 1
`, "rowcount is only valid on non-query statements")
}

func TestLoad_RowcountWithErrorRejected(t *testing.T) {
	t.Parallel()
	loadExpectingError(t, `  - exec: DELETE FROM t
    rowcount: 1
    error_code: "42601"
`, "rowcount and error assertions are mutually exclusive")
}

// Plan/order assertions ride only on the Query path — the runner's
// non-query branch returns before plan checking and row diffing, so a
// DML stanza carrying them would silently skip the assertion (the same
// dropped-assertion class).
func TestLoad_PlanAssertOnDMLRejected(t *testing.T) {
	t.Parallel()
	loadExpectingError(t, `  - exec: DELETE FROM t
    plan_contains: Scan
`, "plan assertions are only valid on queries")
}

func TestLoad_UnorderedOnDMLRejected(t *testing.T) {
	t.Parallel()
	loadExpectingError(t, `  - query: DELETE FROM t
    unordered: true
`, "unordered is only valid on queries")
}

// TestLoad_ExecNormalizesIntoQuery pins the aliasing contract: downstream
// consumers (runner, coverage, ANSI ledger) read Query only.
func TestLoad_ExecNormalizesIntoQuery(t *testing.T) {
	t.Parallel()
	path := writeTempYaml(t, strictTestHeader+`  - exec: DELETE FROM t
    rowcount: 2
`)
	s, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	tt := s.Tests[0]
	if tt.Query != "DELETE FROM t" || tt.Exec != "" {
		t.Errorf("exec not normalized into Query: query=%q exec=%q", tt.Query, tt.Exec)
	}
	if tt.Rowcount == nil || *tt.Rowcount != 2 {
		t.Errorf("rowcount not preserved: %v", tt.Rowcount)
	}
}
