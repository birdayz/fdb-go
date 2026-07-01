// End-to-end tests for `frl sql` and `frl meta catalog` — the relational
// side of the CLI (RFC-174 Slice 1; previously the largest and only
// write-capable command had zero e2e coverage). Shares the process-wide
// FDB testcontainer with the record-layer integration tests; the SQL
// data lives under its own database URI + the fixed `__SYS/CATALOG`
// subspace, so neither side can see the other's keys.
package cmd

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// sqlFixtureOnce bootstraps the relational schema exactly once:
// database /frlsql, template frlsql_tpl (one `items` table), schema
// /frlsql/main, and two seeded rows. Later tests only read.
var (
	sqlFixtureOnce sync.Once
	sqlFixtureErr  error
)

// setupSQLFixture creates the /frlsql database + schema + seed rows via
// the real `frl sql` command path (not the driver directly) — the DDL
// and DML execution IS part of what these tests cover.
func setupSQLFixture(t *testing.T) {
	t.Helper()
	bindConfig(t)
	sqlFixtureOnce.Do(func() {
		schema := `
CREATE DATABASE /frlsql;

CREATE SCHEMA TEMPLATE frlsql_tpl
CREATE TABLE items (
  id   BIGINT NOT NULL,
  name STRING,
  PRIMARY KEY (id)
);

CREATE SCHEMA /frlsql/main WITH TEMPLATE frlsql_tpl;

INSERT INTO items VALUES (1, 'alpha'), (2, 'beta');
`
		path := filepath.Join(os.TempDir(), fmt.Sprintf("frlsql-%d.sql", os.Getpid()))
		if err := os.WriteFile(path, []byte(schema), 0o600); err != nil {
			sqlFixtureErr = fmt.Errorf("write schema.sql: %w", err)
			return
		}
		defer os.Remove(path)
		out, err := runCmd(t, "sql", "--database", "/frlsql", "--schema", "main", "-f", path)
		if err != nil {
			sqlFixtureErr = fmt.Errorf("bootstrap via sql -f: %w\noutput: %s", err, out)
		}
	})
	if sqlFixtureErr != nil {
		t.Fatalf("sql fixture: %v", sqlFixtureErr)
	}
}

func TestIntegration_SQL_SelectViaCommandFlag(t *testing.T) {
	setupSQLFixture(t)
	out, err := runCmd(t, "sql", "--database", "/frlsql", "--schema", "main",
		"-c", "SELECT id, name FROM items WHERE id = 1")
	if err != nil {
		t.Fatalf("sql -c: %v\noutput: %s", err, out)
	}
	if !strings.Contains(out, "alpha") {
		t.Errorf("missing row value in output:\n%s", out)
	}
	// The -c path renders through renderTable on a non-TTY writer: the
	// output must be pure 7-bit ASCII with zero ANSI escapes (RFC-174
	// bug 2 + codex P2-3, end-to-end on the real driver path — the unit
	// test only covers renderStaticTable).
	if strings.ContainsRune(out, 0x1b) {
		t.Errorf("piped sql output contains ANSI escape:\n%q", out)
	}
	for i := 0; i < len(out); i++ {
		if out[i] >= 0x80 {
			t.Errorf("piped sql output contains non-ASCII byte 0x%02x at %d:\n%q", out[i], i, out)
			break
		}
	}
}

func TestIntegration_SQL_TransactionScript(t *testing.T) {
	setupSQLFixture(t)
	// BEGIN / INSERT / COMMIT through the -f path exercises the pinned
	// per-connection transaction plumbing (BEGIN → START TRANSACTION
	// rewrite included), then a SELECT proves the commit landed.
	script := filepath.Join(t.TempDir(), "tx.sql")
	if err := os.WriteFile(script, []byte(
		"BEGIN;\nINSERT INTO items VALUES (3, 'gamma');\nCOMMIT;\n"), 0o600); err != nil {
		t.Fatalf("write tx.sql: %v", err)
	}
	if out, err := runCmd(t, "sql", "--database", "/frlsql", "--schema", "main", "-f", script); err != nil {
		t.Fatalf("sql -f tx: %v\noutput: %s", err, out)
	}
	out, err := runCmd(t, "sql", "--database", "/frlsql", "--schema", "main",
		"-c", "SELECT count(*) AS n FROM items")
	if err != nil {
		t.Fatalf("sql -c count: %v\noutput: %s", err, out)
	}
	if !strings.Contains(out, "3") {
		t.Errorf("expected 3 rows after committed insert:\n%s", out)
	}
}

func TestIntegration_SQL_SyntaxErrorFailsNonZero(t *testing.T) {
	setupSQLFixture(t)
	out, err := runCmd(t, "sql", "--database", "/frlsql", "--schema", "main",
		"-c", "SELEC broken")
	if err == nil {
		t.Fatalf("expected error for bad SQL, got success:\n%s", out)
	}
	if !strings.Contains(err.Error(), "42601") {
		t.Errorf("expected SQLSTATE 42601 syntax error, got: %v", err)
	}
}

func TestIntegration_MetaCatalog_DatabasesSchemasTemplates(t *testing.T) {
	setupSQLFixture(t)

	out, err := runCmd(t, "meta", "catalog", "databases")
	if err != nil {
		t.Fatalf("meta catalog databases: %v\noutput: %s", err, out)
	}
	if !strings.Contains(out, "/frlsql") {
		t.Errorf("databases output missing /frlsql:\n%s", out)
	}

	out, err = runCmd(t, "meta", "catalog", "schemas", "--database", "/frlsql")
	if err != nil {
		t.Fatalf("meta catalog schemas: %v\noutput: %s", err, out)
	}
	if !strings.Contains(out, "main") || !strings.Contains(out, "frlsql_tpl") {
		t.Errorf("schemas output missing main/frlsql_tpl:\n%s", out)
	}

	out, err = runCmd(t, "meta", "catalog", "templates", "-o", "json")
	if err != nil {
		t.Fatalf("meta catalog templates: %v\noutput: %s", err, out)
	}
	var templates []map[string]any
	if err := json.Unmarshal([]byte(out), &templates); err != nil {
		t.Fatalf("templates -o json is not JSON: %v\nraw:\n%s", err, out)
	}
	found := false
	for _, tpl := range templates {
		if tpl["name"] == "frlsql_tpl" {
			found = true
		}
	}
	if !found {
		t.Errorf("templates JSON missing frlsql_tpl: %v", templates)
	}
}

func TestIntegration_MetaCatalog_GetRendersMetaData(t *testing.T) {
	setupSQLFixture(t)
	out, err := runCmd(t, "meta", "catalog", "get", "frlsql_tpl")
	if err != nil {
		t.Fatalf("meta catalog get: %v\noutput: %s", err, out)
	}
	var md map[string]any
	if err := json.Unmarshal([]byte(out), &md); err != nil {
		t.Fatalf("catalog get output is not JSON: %v\nraw:\n%s", err, out)
	}
	// The rendered MetaData must carry the table as a record type with
	// its descriptor — the exact object Slice 2's CatalogSource will
	// feed to record/index/store commands.
	if _, ok := md["records"]; !ok {
		t.Errorf("catalog get JSON missing records descriptor:\n%s", out)
	}
	if !strings.Contains(out, "ITEMS") && !strings.Contains(out, "items") {
		t.Errorf("catalog get JSON does not mention the items table:\n%s", out)
	}
}

func TestIntegration_MetaCatalog_UnknownTemplateErrors(t *testing.T) {
	setupSQLFixture(t)
	_, err := runCmd(t, "meta", "catalog", "get", "no_such_template")
	if err == nil {
		t.Fatal("expected error for unknown template")
	}
}
