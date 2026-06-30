package yamsql_test

import (
	"os"
	"path/filepath"
	"testing"

	"fdb.dev/pkg/relational/conformance/yamsql"
)

// TestAnsiLedgerUpToDate is the anti-rot guard for SQL_ANSI_CONFORMANCE.md
// (Ledger A): it regenerates the ANSI scorecard from the roster + corpus tags
// and fails if the committed file differs. Because Go?/completeness are derived
// from `# ansi:` tags (not hand-typed), regenerating after a corpus or roster
// change keeps the scoreboard honest. No Docker — repoRootForMatrix is shared
// with featurematrix_test.go.
func TestAnsiLedgerUpToDate(t *testing.T) {
	t.Parallel()

	got, err := yamsql.GenerateAnsiLedger("testdata")
	if err != nil {
		t.Fatalf("generate ANSI ledger: %v", err)
	}

	root := repoRootForMatrix(t)
	want, err := os.ReadFile(filepath.Join(root, "SQL_ANSI_CONFORMANCE.md"))
	if err != nil {
		t.Fatalf("read SQL_ANSI_CONFORMANCE.md: %v", err)
	}

	if string(want) != got {
		t.Fatalf("SQL_ANSI_CONFORMANCE.md is stale — run `just sql-coverage` and commit the result "+
			"(committed %d bytes, regenerated %d bytes)", len(want), len(got))
	}
}
