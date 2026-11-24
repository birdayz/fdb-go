package norecover

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
)

type parseErrorKind int

const (
	errKindMissingSection parseErrorKind = iota + 1
	errKindEmptyTable
	errKindInvalidRow
	errKindInvalidCount
	errKindDuplicateFile
)

type panicAuditParseError struct {
	kind   parseErrorKind
	line   int
	file   string
	detail string
	cause  error
}

func (e *panicAuditParseError) Error() string {
	switch {
	case e.line > 0 && e.file != "":
		return fmt.Sprintf("docs/panic-audit.md line %d (%s): %s", e.line, e.file, e.detail)
	case e.line > 0:
		return fmt.Sprintf("docs/panic-audit.md line %d: %s", e.line, e.detail)
	default:
		return fmt.Sprintf("docs/panic-audit.md: %s", e.detail)
	}
}

func (e *panicAuditParseError) Unwrap() error {
	return e.cause
}

type countMismatch struct {
	file       string
	allowCount int
	docCount   int
}

type allowlistSyncError struct {
	missingInDoc    []string
	extraInDoc      []string
	countMismatches []countMismatch
}

func (e *allowlistSyncError) Error() string {
	var parts []string
	for _, f := range e.missingInDoc {
		parts = append(parts, fmt.Sprintf("missing in docs/panic-audit.md §2: %s", f))
	}
	for _, f := range e.extraInDoc {
		parts = append(parts, fmt.Sprintf("extra in docs/panic-audit.md §2 (absent from norecover.Allowlist): %s", f))
	}
	for _, m := range e.countMismatches {
		parts = append(parts, fmt.Sprintf("count mismatch for %s: norecover.Allowlist=%d, docs/panic-audit.md §2=%d",
			m.file, m.allowCount, m.docCount))
	}
	return "norecover.Allowlist and docs/panic-audit.md §2 are out of sync:\n  - " + strings.Join(parts, "\n  - ")
}

// parsePanicAuditTable parses the markdown table under "## §2" in docs/panic-audit.md
// into a repo-relative file -> positive recover() count map.
func parsePanicAuditTable(content string) (map[string]int, error) {
	lines := strings.Split(content, "\n")
	inSection2 := false
	inTable := false
	sawSeparator := false
	entries := map[string]int{}

	for i, rawLine := range lines {
		lineNum := i + 1
		trimmed := strings.TrimSpace(rawLine)

		if !inSection2 {
			if isSection2Heading(trimmed) {
				inSection2 = true
			}
			continue
		}

		if isMarkdownHeading(trimmed) {
			break
		}

		if !strings.HasPrefix(trimmed, "|") {
			if inTable {
				break
			}
			continue
		}

		cells := splitTableRow(trimmed)
		if !inTable {
			if len(cells) < 2 {
				return nil, &panicAuditParseError{
					kind:   errKindInvalidRow,
					line:   lineNum,
					detail: "table header must have at least 2 columns",
				}
			}
			inTable = true
			continue
		}

		if !sawSeparator {
			if !isSeparatorRow(cells) {
				return nil, &panicAuditParseError{
					kind:   errKindInvalidRow,
					line:   lineNum,
					detail: "expected markdown table separator row after header",
				}
			}
			sawSeparator = true
			continue
		}

		if len(cells) < 2 {
			return nil, &panicAuditParseError{
				kind:   errKindInvalidRow,
				line:   lineNum,
				detail: "table row must have at least 2 columns",
			}
		}

		file := strings.TrimSpace(strings.Trim(cells[0], "`"))
		if file == "" {
			return nil, &panicAuditParseError{
				kind:   errKindInvalidRow,
				line:   lineNum,
				detail: "empty file entry in §2 table",
			}
		}

		count, err := strconv.Atoi(cells[1])
		if err != nil || count <= 0 {
			return nil, &panicAuditParseError{
				kind:   errKindInvalidCount,
				line:   lineNum,
				file:   file,
				detail: fmt.Sprintf("invalid recover count %q (must be a positive integer)", cells[1]),
				cause:  err,
			}
		}

		if _, dup := entries[file]; dup {
			return nil, &panicAuditParseError{
				kind:   errKindDuplicateFile,
				line:   lineNum,
				file:   file,
				detail: "duplicate file entry in §2 table",
			}
		}
		entries[file] = count
	}

	if !inSection2 {
		return nil, &panicAuditParseError{
			kind:   errKindMissingSection,
			detail: "missing ## §2 heading",
		}
	}
	if len(entries) == 0 {
		return nil, &panicAuditParseError{
			kind:   errKindEmptyTable,
			detail: "§2 allowlist table has no entries",
		}
	}
	return entries, nil
}

func isSection2Heading(trimmed string) bool {
	const prefix = "## §2"
	if !strings.HasPrefix(trimmed, prefix) {
		return false
	}
	if len(trimmed) == len(prefix) {
		return true
	}
	next := trimmed[len(prefix)]
	return next < '0' || next > '9'
}

func isMarkdownHeading(trimmed string) bool {
	return strings.HasPrefix(trimmed, "# ") || strings.HasPrefix(trimmed, "## ")
}

func splitTableRow(trimmed string) []string {
	inner := strings.TrimPrefix(trimmed, "|")
	inner = strings.TrimSuffix(inner, "|")
	cells := strings.Split(inner, "|")
	for i := range cells {
		cells[i] = strings.TrimSpace(cells[i])
	}
	return cells
}

func isSeparatorRow(cells []string) bool {
	if len(cells) < 2 {
		return false
	}
	for _, cell := range cells {
		if cell == "" {
			return false
		}
		hasDash := false
		for _, r := range cell {
			switch r {
			case '-':
				hasDash = true
			case ':', ' ':
			default:
				return false
			}
		}
		if !hasDash {
			return false
		}
	}
	return true
}

func compareAllowlistWithDoc(allowlist, docCounts map[string]int) error {
	var missingInDoc []string
	var extraInDoc []string
	var countMismatches []countMismatch

	for file, allowCount := range allowlist {
		docCount, ok := docCounts[file]
		if !ok {
			missingInDoc = append(missingInDoc, file)
			continue
		}
		if docCount != allowCount {
			countMismatches = append(countMismatches, countMismatch{
				file:       file,
				allowCount: allowCount,
				docCount:   docCount,
			})
		}
	}
	for file := range docCounts {
		if _, ok := allowlist[file]; !ok {
			extraInDoc = append(extraInDoc, file)
		}
	}

	if len(missingInDoc) == 0 && len(extraInDoc) == 0 && len(countMismatches) == 0 {
		return nil
	}
	slices.Sort(missingInDoc)
	slices.Sort(extraInDoc)
	slices.SortFunc(countMismatches, func(a, b countMismatch) int {
		return strings.Compare(a.file, b.file)
	})
	return &allowlistSyncError{
		missingInDoc:    missingInDoc,
		extraInDoc:      extraInDoc,
		countMismatches: countMismatches,
	}
}

// panicAuditPath locates docs/panic-audit.md in Bazel runfiles when present, or
// walks up from the working directory to MODULE.bazel under go test.
func panicAuditPath(t *testing.T) string {
	t.Helper()
	if sd, ws := os.Getenv("TEST_SRCDIR"), os.Getenv("TEST_WORKSPACE"); sd != "" && ws != "" {
		return filepath.Join(sd, ws, "docs/panic-audit.md")
	}
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "MODULE.bazel")); err == nil {
			return filepath.Join(dir, "docs/panic-audit.md")
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatalf("MODULE.bazel not found walking up from %q; check Bazel data deps", dir)
		}
		dir = parent
	}
}

// TestAllowlistMatchesPanicAuditDoc pins exact 1:1 bidirectional equality between
// norecover.Allowlist and the §2 boundary table in docs/panic-audit.md.
func TestAllowlistMatchesPanicAuditDoc(t *testing.T) {
	t.Parallel()
	if len(Allowlist) == 0 {
		t.Fatal("norecover.Allowlist is empty; sync gate cannot pass vacuously")
	}
	path := panicAuditPath(t)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v (check Bazel data deps for //:docs/panic-audit.md)", path, err)
	}
	docCounts, err := parsePanicAuditTable(string(raw))
	if err != nil {
		t.Fatalf("parsing %s: %v", path, err)
	}
	if err := compareAllowlistWithDoc(Allowlist, docCounts); err != nil {
		t.Fatal(err)
	}
}

func TestParsePanicAuditTable_Valid(t *testing.T) {
	t.Parallel()
	doc := `# Audit

Reference to §2 in prose should not trigger section matching.

## §2 — Boundary allowlist

Introductory prose before the table.

| File | Count | Boundary / role |
|---|---|---|
| ` + "`pkg/a/one.go`" + ` | 1 | first boundary |
| pkg/b/two.go | 3 | second boundary |

Trailing note.

## §3 — Next section

| File | Count |
|---|---|
| pkg/ignored/other.go | 99 |
`
	got, err := parsePanicAuditTable(doc)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 2 || got["pkg/a/one.go"] != 1 || got["pkg/b/two.go"] != 3 {
		t.Fatalf("parsed = %v, want {pkg/a/one.go:1, pkg/b/two.go:3}", got)
	}
}

func TestParsePanicAuditTable_Errors(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		doc      string
		wantKind parseErrorKind
	}{
		{
			name:     "missing section 2 heading",
			doc:      "# Audit\n\n## §1 — Intro\n\n| File | Count |\n|---|---|\n| a.go | 1 |\n",
			wantKind: errKindMissingSection,
		},
		{
			name:     "section 20 heading does not match section 2",
			doc:      "## §20 — Other\n\n| File | Count |\n|---|---|\n| a.go | 1 |\n",
			wantKind: errKindMissingSection,
		},
		{
			name:     "empty table with header and separator only",
			doc:      "## §2 — Allowlist\n\n| File | Count | Role |\n|---|---|---|\n\n## §3 — Done\n",
			wantKind: errKindEmptyTable,
		},
		{
			name:     "section 2 with no table",
			doc:      "## §2 — Allowlist\n\nNo table present.\n\n## §3 — Done\n",
			wantKind: errKindEmptyTable,
		},
		{
			name:     "duplicate file entry",
			doc:      "## §2\n\n| File | Count |\n|---|---|\n| pkg/a.go | 1 |\n| pkg/a.go | 2 |\n",
			wantKind: errKindDuplicateFile,
		},
		{
			name:     "zero count",
			doc:      "## §2\n\n| File | Count |\n|---|---|\n| pkg/a.go | 0 |\n",
			wantKind: errKindInvalidCount,
		},
		{
			name:     "negative count",
			doc:      "## §2\n\n| File | Count |\n|---|---|\n| pkg/a.go | -2 |\n",
			wantKind: errKindInvalidCount,
		},
		{
			name:     "non-numeric count",
			doc:      "## §2\n\n| File | Count |\n|---|---|\n| pkg/a.go | two |\n",
			wantKind: errKindInvalidCount,
		},
		{
			name:     "empty file cell",
			doc:      "## §2\n\n| File | Count |\n|---|---|\n|  | 1 |\n",
			wantKind: errKindInvalidRow,
		},
		{
			name:     "row with too few columns",
			doc:      "## §2\n\n| File | Count |\n|---|---|\n| pkg/a.go |\n",
			wantKind: errKindInvalidRow,
		},
		{
			name:     "missing separator row",
			doc:      "## §2\n\n| File | Count |\n| pkg/a.go | 1 |\n",
			wantKind: errKindInvalidRow,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := parsePanicAuditTable(tc.doc)
			if err == nil {
				t.Fatal("expected error, got nil")
			}
			var parseErr *panicAuditParseError
			if !errors.As(err, &parseErr) {
				t.Fatalf("expected *panicAuditParseError, got %T: %v", err, err)
			}
			if parseErr.kind != tc.wantKind {
				t.Fatalf("error kind = %v, want %v (err: %v)", parseErr.kind, tc.wantKind, err)
			}
		})
	}
}

func TestCompareAllowlistWithDoc(t *testing.T) {
	t.Parallel()

	t.Run("exact match", func(t *testing.T) {
		t.Parallel()
		allow := map[string]int{"pkg/a.go": 1, "pkg/b.go": 2}
		doc := map[string]int{"pkg/a.go": 1, "pkg/b.go": 2}
		if err := compareAllowlistWithDoc(allow, doc); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	})

	t.Run("missing doc entry", func(t *testing.T) {
		t.Parallel()
		allow := map[string]int{"pkg/a.go": 1, "pkg/b.go": 2}
		doc := map[string]int{"pkg/a.go": 1}
		err := compareAllowlistWithDoc(allow, doc)
		var syncErr *allowlistSyncError
		if !errors.As(err, &syncErr) {
			t.Fatalf("expected *allowlistSyncError, got %T: %v", err, err)
		}
		if !slices.Equal(syncErr.missingInDoc, []string{"pkg/b.go"}) {
			t.Fatalf("missingInDoc = %v, want [pkg/b.go]", syncErr.missingInDoc)
		}
		if len(syncErr.extraInDoc) != 0 || len(syncErr.countMismatches) != 0 {
			t.Fatalf("unexpected extra/mismatch fields: %+v", syncErr)
		}
	})

	t.Run("extra doc entry", func(t *testing.T) {
		t.Parallel()
		allow := map[string]int{"pkg/a.go": 1}
		doc := map[string]int{"pkg/a.go": 1, "pkg/c.go": 3}
		err := compareAllowlistWithDoc(allow, doc)
		var syncErr *allowlistSyncError
		if !errors.As(err, &syncErr) {
			t.Fatalf("expected *allowlistSyncError, got %T: %v", err, err)
		}
		if !slices.Equal(syncErr.extraInDoc, []string{"pkg/c.go"}) {
			t.Fatalf("extraInDoc = %v, want [pkg/c.go]", syncErr.extraInDoc)
		}
		if len(syncErr.missingInDoc) != 0 || len(syncErr.countMismatches) != 0 {
			t.Fatalf("unexpected missing/mismatch fields: %+v", syncErr)
		}
	})

	t.Run("count mismatch", func(t *testing.T) {
		t.Parallel()
		allow := map[string]int{"pkg/a.go": 2}
		doc := map[string]int{"pkg/a.go": 1}
		err := compareAllowlistWithDoc(allow, doc)
		var syncErr *allowlistSyncError
		if !errors.As(err, &syncErr) {
			t.Fatalf("expected *allowlistSyncError, got %T: %v", err, err)
		}
		want := []countMismatch{{file: "pkg/a.go", allowCount: 2, docCount: 1}}
		if !slices.Equal(syncErr.countMismatches, want) {
			t.Fatalf("countMismatches = %+v, want %+v", syncErr.countMismatches, want)
		}
		if len(syncErr.missingInDoc) != 0 || len(syncErr.extraInDoc) != 0 {
			t.Fatalf("unexpected missing/extra fields: %+v", syncErr)
		}
	})
}
