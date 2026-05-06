// Pure-Go unit tests for the scenario loader and diff layer. These
// do NOT require FDB or Docker — they exercise the harness plumbing
// independently of the actual conformance runs.

package yamsql

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoad_RequiresSchemaTemplate(t *testing.T) {
	t.Parallel()
	path := writeTempYaml(t, `name: incomplete
tests:
  - query: SELECT 1
    rows:
      - [1]
`)
	_, err := Load(path)
	if err == nil {
		t.Fatal("want error when schema_template is missing, got nil")
	}
	if !strings.Contains(err.Error(), "schema_template is required") {
		t.Errorf("error should mention schema_template, got: %v", err)
	}
}

func TestLoad_DefaultsNameToBasename(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "my_scenario.yaml")
	writeFile(t, path, `schema_template: |
  CREATE TABLE t (id BIGINT NOT NULL, PRIMARY KEY (id))
tests: []
`)
	s, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if s.Name != "my_scenario" {
		t.Errorf("Name = %q, want %q (extension stripped)", s.Name, "my_scenario")
	}
}

func TestLoad_ExplicitNameWins(t *testing.T) {
	t.Parallel()
	path := writeTempYaml(t, `name: explicit
schema_template: |
  CREATE TABLE t (id BIGINT NOT NULL, PRIMARY KEY (id))
tests: []
`)
	s, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if s.Name != "explicit" {
		t.Errorf("Name = %q, want %q (explicit field should win over basename)", s.Name, "explicit")
	}
}

func TestLoad_MissingFile(t *testing.T) {
	t.Parallel()
	_, err := Load("/nonexistent/path/file.yaml")
	if err == nil {
		t.Fatal("want error for missing file, got nil")
	}
}

func TestDiffRows_Identical(t *testing.T) {
	t.Parallel()
	expected := [][]any{{int64(1), "alice"}, {int64(2), "bob"}}
	actual := [][]any{{int64(1), "alice"}, {int64(2), "bob"}}
	if d := diffRows(expected, actual, false); d != "" {
		t.Errorf("want empty diff, got: %s", d)
	}
}

func TestDiffRows_DifferentLength(t *testing.T) {
	t.Parallel()
	expected := [][]any{{int64(1)}, {int64(2)}}
	actual := [][]any{{int64(1)}}
	d := diffRows(expected, actual, false)
	if d == "" {
		t.Error("want non-empty diff for length mismatch")
	}
	if !strings.Contains(d, "expected (2 rows)") || !strings.Contains(d, "actual (1 rows)") {
		t.Errorf("diff should show both counts, got: %s", d)
	}
}

func TestDiffRows_NumericPromotion(t *testing.T) {
	t.Parallel()
	// YAML parses 5 as int. The driver returns int64. The diff layer
	// promotes int→int64 so they compare equal.
	expected := [][]any{{5}}
	actual := [][]any{{int64(5)}}
	if d := diffRows(expected, actual, false); d != "" {
		t.Errorf("int should compare equal to int64, got: %s", d)
	}
}

func TestDiffRows_NullHandling(t *testing.T) {
	t.Parallel()
	// nil must match nil; nil must NOT match non-nil.
	if d := diffRows([][]any{{nil}}, [][]any{{nil}}, false); d != "" {
		t.Errorf("nil vs nil: want empty diff, got %s", d)
	}
	if d := diffRows([][]any{{nil}}, [][]any{{int64(1)}}, false); d == "" {
		t.Error("nil vs int64(1): want diff")
	}
	if d := diffRows([][]any{{int64(1)}}, [][]any{{nil}}, false); d == "" {
		t.Error("int64(1) vs nil: want diff")
	}
}

func TestDiffRows_UnorderedMatches(t *testing.T) {
	t.Parallel()
	// Same rows in different order — unordered=true accepts it.
	expected := [][]any{{int64(1), "a"}, {int64(2), "b"}}
	actual := [][]any{{int64(2), "b"}, {int64(1), "a"}}
	if d := diffRows(expected, actual, false); d == "" {
		t.Error("unordered=false should reject permuted rows")
	}
	if d := diffRows(expected, actual, true); d != "" {
		t.Errorf("unordered=true should accept permuted rows, got: %s", d)
	}
}

func TestDiffRows_TypeTaggedNotStringified(t *testing.T) {
	t.Parallel()
	// int64(5) must NOT compare equal to string "5" — the harness
	// mirrors valuesEqual's strict cross-type rule.
	expected := [][]any{{int64(5)}}
	actual := [][]any{{"5"}}
	if d := diffRows(expected, actual, false); d == "" {
		t.Error("int 5 and string '5' should not match")
	}
}

func TestIsQuery(t *testing.T) {
	t.Parallel()
	queries := []string{
		"SELECT * FROM t",
		"select 1",
		"  \tSELECT a FROM t",
		"WITH hi AS (SELECT id FROM t) SELECT * FROM hi",
		"(SELECT 1)",
		"VALUES (1)",
	}
	for _, q := range queries {
		if !IsQuery(q) {
			t.Errorf("IsQuery(%q) = false, want true", q)
		}
	}
	nonQueries := []string{
		"INSERT INTO t VALUES (1)",
		"UPDATE t SET v = 2",
		"DELETE FROM t",
		"CREATE TABLE t (id BIGINT)",
		"DROP DATABASE foo",
	}
	for _, q := range nonQueries {
		if IsQuery(q) {
			t.Errorf("IsQuery(%q) = true, want false", q)
		}
	}
}

func writeTempYaml(t *testing.T, content string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "scenario.yaml")
	writeFile(t, path, content)
	return path
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
}
