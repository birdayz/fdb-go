package yamsql_test

import (
	"os"
	"path/filepath"
	"testing"
)

// TestRunner_AppliesConnectionOptions pins that the runner runs a scenario's
// tests on a connection carrying its connection_options: MAX_ROWS caps the
// statement at one row, which the pooled connection would not.
func TestRunner_AppliesConnectionOptions(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	path := filepath.Join(t.TempDir(), "connection_options.yaml")
	if err := os.WriteFile(path, []byte(`name: connection_options
schema_template: |
  CREATE TABLE t (id BIGINT, PRIMARY KEY (id))
connection_options:
  MAX_ROWS: 1
setup:
  - INSERT INTO t VALUES (1), (2)
tests:
  - query: SELECT id FROM t
    rows:
      - [1]
`), 0o600); err != nil {
		t.Fatal(err)
	}
	runScenario(t, path, "runner_connection_options")
}
