package yamsql

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestAnsiRosterIntegrity pins the hand-authored roster's structural facts: the
// SQL:2023 Core row count (176 per PostgreSQL 18), unique IDs, Core-only, and
// every declared subfeature resolving to a real roster row. A wrong/duplicated
// ID — the "pinned fact" a reviewer would catch — fails here.
func TestAnsiRosterIntegrity(t *testing.T) {
	t.Parallel()
	const wantCount = 176 // PostgreSQL 18 Core (was 177 in PG13–15; F812 dropped in PG16)
	if len(ansiCoreRoster) != wantCount {
		t.Errorf("roster has %d rows, want %d SQL:2023 Core rows (PostgreSQL 18)", len(ansiCoreRoster), wantCount)
	}
	ids := map[string]bool{}
	for _, f := range ansiCoreRoster {
		if ids[f.ID] {
			t.Errorf("duplicate roster ID %q", f.ID)
		}
		ids[f.ID] = true
		if !f.Core {
			t.Errorf("%s: roster is Core-only but Core=false", f.ID)
		}
		if f.ID == "" || f.Name == "" {
			t.Errorf("roster row with empty ID or Name: %+v", f)
		}
	}
	for _, f := range ansiCoreRoster {
		for _, sub := range f.Subfeatures {
			if !ids[sub] {
				t.Errorf("%s declares subfeature %s which is not a roster row", f.ID, sub)
			}
		}
	}
}

// TestAnsiCorpusTagsClean is the exercised-not-exists guard against the real
// corpus: every `# ansi:` tag must sit on a scenario with a passing test and
// every `# ansi-gap:` on a scenario with an unsupported pin. A mis-tag here is a
// fake checkbox and fails the build.
func TestAnsiCorpusTagsClean(t *testing.T) {
	t.Parallel()
	_, violations, err := collectAnsiEvidence("testdata")
	if err != nil {
		t.Fatalf("collect ANSI evidence: %v", err)
	}
	if len(violations) > 0 {
		t.Fatalf("ANSI tag evidence violations (a `# ansi:` tag without a passing test, or a "+
			"`# ansi-gap:` without an unsupported pin):\n%s", strings.Join(violations, "\n"))
	}
}

// TestAnsiEvidenceGuardBites proves the guard actually catches a fake checkbox
// (RFC-165 §7): a scenario tagged `# ansi:` whose only test is an unsupported
// pin (no positive result) MUST be reported. Without this, "evidence" could
// silently decay to "a file exists" — the exact rot this ledger replaces.
func TestAnsiEvidenceGuardBites(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	// Tagged positive for E051 but the only test is an unsupported-feature pin.
	bad := "name: badtag\n" +
		"schema_template: |\n" +
		"  CREATE TABLE t (id BIGINT, PRIMARY KEY (id))\n" +
		"tests:\n" +
		"  - query: SELECT COUNT(DISTINCT id) FROM t\n" +
		"    error_code: \"0A000\"\n" +
		"# ansi: E051\n"
	if err := os.WriteFile(filepath.Join(dir, "badtag.yaml"), []byte(bad), 0o644); err != nil {
		t.Fatal(err)
	}
	_, violations, err := collectAnsiEvidence(dir)
	if err != nil {
		t.Fatalf("collect ANSI evidence: %v", err)
	}
	if len(violations) == 0 {
		t.Fatal("evidence guard did not bite: `# ansi: E051` on a scenario with no passing test produced no violation")
	}
}
