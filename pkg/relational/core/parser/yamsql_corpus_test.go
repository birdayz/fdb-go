//go:build yamsql

package parser

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/birdayz/fdb-record-layer-go/pkg/relational/api"
)

// TestYamsqlCorpus_ParsesEveryStatement is a grammar-coverage smoke test:
// it walks the vendored Java yamsql corpus, extracts every SQL statement
// (schema templates + test-block queries) and runs it through Parse().
//
// The corpus lives under fdb-record-layer/yaml-tests/src/test/resources/
// in the gitignored Java source tree. If the directory is missing (CI
// machines that don't check out the submodule) the test skips.
//
// This is not a correctness check — parse trees are not inspected. The
// assertion is "the grammar recognises every statement the Java test
// corpus feeds it". A regression here means we've drifted from Java's
// SQL dialect.
func TestYamsqlCorpus_ParsesEveryStatement(t *testing.T) {
	t.Parallel()

	root := findCorpusRoot(t)
	if root == "" {
		t.Skip("yamsql corpus not found at fdb-record-layer/yaml-tests/src/test/resources (Java submodule likely missing)")
	}

	var (
		filesParsed int
		stmtsTotal  int
		stmtsFailed int
		firstFails  []string // capped sample for the failure report
	)
	const maxSample = 20

	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, werr error) error {
		if werr != nil {
			return werr
		}
		if d.IsDir() || !strings.HasSuffix(path, ".yamsql") {
			return nil
		}
		stmts, perr := extractStatements(path)
		if perr != nil {
			t.Errorf("extract %s: %v", path, perr)
			return nil
		}
		filesParsed++
		for _, s := range stmts {
			stmtsTotal++
			if _, err := Parse(s.sql); err != nil {
				stmtsFailed++
				if len(firstFails) < maxSample {
					rel, _ := filepath.Rel(root, path)
					firstFails = append(firstFails, fmt.Sprintf("%s:%s: %s\n  sql: %s", rel, s.kind, firstLine(err), firstLine(errors.New(s.sql))))
				}
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}

	if filesParsed == 0 {
		t.Skip("no .yamsql files found")
	}
	t.Logf("parsed %d files, %d statements, %d failed", filesParsed, stmtsTotal, stmtsFailed)
	if stmtsFailed > 0 {
		t.Fatalf("%d of %d corpus statements failed to parse; first %d:\n%s",
			stmtsFailed, stmtsTotal, len(firstFails), strings.Join(firstFails, "\n"))
	}
}

func findCorpusRoot(t *testing.T) string {
	t.Helper()
	// Walk up from CWD looking for fdb-record-layer/yaml-tests.
	// `go test` runs in the package dir; the repo root is a few levels up.
	dir, err := os.Getwd()
	if err != nil {
		return ""
	}
	for i := 0; i < 8; i++ {
		cand := filepath.Join(dir, "fdb-record-layer", "yaml-tests", "src", "test", "resources")
		if fi, err := os.Stat(cand); err == nil && fi.IsDir() {
			return cand
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return ""
		}
		dir = parent
	}
	return ""
}

// yamsqlStmt is one SQL statement extracted from a yamsql file, tagged
// with the YAML field it came from so failure reports are diagnostic.
type yamsqlStmt struct {
	sql  string
	kind string // "schema_template" | "query"
}

// extractStatements reads path, splits it into YAML documents, and
// returns every SQL statement it can find. For schema_template, the
// multi-line free-form DDL string is wrapped in "CREATE SCHEMA TEMPLATE
// test_template {...}" so it parses as a top-level statement — that's
// how the Java harness feeds it to the parser.
func extractStatements(path string) ([]yamsqlStmt, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	// Split on YAML document boundaries. yaml.v3 Decoder handles this
	// natively but the yamsql files mix several shapes and we only care
	// about a couple of keys, so a manual scan stays simpler.
	dec := yaml.NewDecoder(bytes.NewReader(data))
	var out []yamsqlStmt
	for {
		var doc map[string]any
		if err := dec.Decode(&doc); err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			// Malformed YAML — don't fail the whole run, skip this file.
			return out, nil
		}
		if tmpl, ok := doc["schema_template"].(string); ok {
			sql := "CREATE SCHEMA TEMPLATE test_template " + tmpl
			out = append(out, yamsqlStmt{sql: sql, kind: "schema_template"})
		}
		if tb, ok := doc["test_block"].(map[string]any); ok {
			collectQueries(tb, &out)
		}
		if setup, ok := doc["setup"].(map[string]any); ok {
			collectQueries(setup, &out)
		}
	}
	return out, nil
}

func collectQueries(node any, out *[]yamsqlStmt) {
	switch v := node.(type) {
	case map[string]any:
		for k, val := range v {
			if k == "query" {
				if s, ok := val.(string); ok && isRealSQL(s) {
					*out = append(*out, yamsqlStmt{sql: s, kind: "query"})
				}
				continue
			}
			collectQueries(val, out)
		}
	case []any:
		// A test entry looks like [{query: ...}, {result|count|error: ...}].
		// If any sibling declares `error:` the corpus expects the parser to
		// reject this SQL — skip the query so we don't flag its rejection
		// as grammar drift. `errorMsg:` (semantic-level errors that still
		// parse successfully) is NOT filtered here because our parse
		// target is purely syntactic; if the corpus ever adds
		// errorMsg-tagged queries that are also syntactically invalid,
		// extend isExpectedFailEntry.
		if isExpectedFailEntry(v) {
			return
		}
		for _, e := range v {
			collectQueries(e, out)
		}
	}
}

// isExpectedFailEntry reports whether a YAML sequence (one test entry)
// contains an error: key — in which case the query is deliberately bad
// SQL and the parser is expected to reject it.
func isExpectedFailEntry(entry []any) bool {
	for _, e := range entry {
		m, ok := e.(map[string]any)
		if !ok {
			continue
		}
		if _, hasErr := m["error"]; hasErr {
			return true
		}
	}
	return false
}

// isRealSQL filters out yamsql-specific noise that the Java test harness
// pre-processes before the parser ever sees it:
//
//   - "!! ... !!" macros: typed-value injectors (!v32 vectors, !n nulls,
//     !randomStr generators, etc.). The harness substitutes real SQL
//     literals in their place.
//   - "SHOULD ERROR" / "SHOULD NOT ERROR": test-expectation sentinels
//     that go in the query slot but aren't parsed as SQL.
//   - Empty or whitespace-only entries.
//
// Any real grammar drift ends up in what remains.
func isRealSQL(s string) bool {
	trimmed := strings.TrimSpace(s)
	if trimmed == "" {
		return false
	}
	if strings.Contains(trimmed, "!!") {
		return false
	}
	upper := strings.ToUpper(trimmed)
	if strings.HasPrefix(upper, "SHOULD ERROR") || strings.HasPrefix(upper, "SHOULD NOT ERROR") {
		return false
	}
	return true
}

func firstLine(err error) string {
	if err == nil {
		return ""
	}
	msg := err.Error()
	// Trim at first newline for compact failure reports.
	if i := strings.IndexByte(msg, '\n'); i >= 0 {
		return msg[:i]
	}
	// If this is an *api.Error, report the code + first line for readability.
	var apiErr *api.Error
	if errors.As(err, &apiErr) {
		first := apiErr.Message
		if i := strings.IndexByte(first, '\n'); i >= 0 {
			first = first[:i]
		}
		return fmt.Sprintf("%s: %s", apiErr.Code, first)
	}
	return msg
}
