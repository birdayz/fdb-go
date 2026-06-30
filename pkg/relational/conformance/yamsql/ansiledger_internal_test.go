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
	// @claude L2: a parent marked Java=Full must not have a subfeature marked
	// Java=None — that is a contradiction (the feature can't be fully supported
	// while a subfeature is absent). Catches an inconsistent hand-authored fact.
	byID := make(map[string]AnsiFeature, len(ansiCoreRoster))
	for _, f := range ansiCoreRoster {
		byID[f.ID] = f
	}
	for _, f := range ansiCoreRoster {
		if f.Java != SupportFull {
			continue
		}
		for _, sub := range f.Subfeatures {
			if byID[sub].Java == SupportNone {
				t.Errorf("%s is Java=Full but subfeature %s is Java=None — contradictory roster fact", f.ID, sub)
			}
		}
	}
}

// TestAnsiLedgerEvidenceExists is the exercised-not-exists guard against the real
// corpus: every `# ansi:` tag must sit on a scenario with a passing test and
// every `# ansi-gap:` on a scenario with an unsupported pin (and a positive tag
// coexisting with an unsupported pin must declare the gap — the cross-feature
// guard). A mis-tag here is a fake checkbox and fails the build.
func TestAnsiLedgerEvidenceExists(t *testing.T) {
	t.Parallel()
	_, violations, err := collectAnsiEvidence("testdata", ansiCoreRoster)
	if err != nil {
		t.Fatalf("collect ANSI evidence: %v", err)
	}
	if len(violations) > 0 {
		t.Fatalf("ANSI tag evidence violations (a `# ansi:` tag without a passing test, a "+
			"`# ansi-gap:` without an unsupported pin, or an unknown/typo'd ID):\n%s", strings.Join(violations, "\n"))
	}
}

// TestAnsiPhantomIDBites pins the phantom-ID guard (@claude M1): a corpus tag for
// an ID not in the roster (a typo like `E0511`) must be flagged, not silently
// dropped — else the real feature stays untested and the typo is invisible.
func TestAnsiPhantomIDBites(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	phantom := "name: phantom\n" +
		"schema_template: |\n" +
		"  CREATE TABLE t (id BIGINT, PRIMARY KEY (id))\n" +
		"tests:\n" +
		"  - query: SELECT id FROM t\n" +
		"    rows: [[1]]\n" +
		"# ansi: E0511\n" // typo: extra digit — not a real roster ID
	if err := os.WriteFile(filepath.Join(dir, "phantom.yaml"), []byte(phantom), 0o644); err != nil {
		t.Fatal(err)
	}
	_, violations, err := collectAnsiEvidence(dir, ansiCoreRoster)
	if err != nil {
		t.Fatalf("collect ANSI evidence: %v", err)
	}
	if !containsSubstr(violations, "unknown ANSI ID") {
		t.Fatalf("phantom-ID guard did not fire on `# ansi: E0511`; got %v", violations)
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
	_, violations, err := collectAnsiEvidence(dir, ansiCoreRoster)
	if err != nil {
		t.Fatalf("collect ANSI evidence: %v", err)
	}
	if len(violations) == 0 {
		t.Fatal("evidence guard did not bite: `# ansi: E051` on a scenario with no passing test produced no violation")
	}
}

// TestAnsiCrossFeatureGuard pins the cross-feature rule (the "NULLIF credited by
// COALESCE" trap codex caught): a scenario with a positive `# ansi:` tag AND an
// unsupported-feature pin, but no `# ansi-gap:` tag, must be flagged — the
// passing test might exercise a sibling feature, not the tagged one.
func TestAnsiCrossFeatureGuard(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	mixed := "name: mixed\n" +
		"schema_template: |\n" +
		"  CREATE TABLE t (id BIGINT, PRIMARY KEY (id))\n" +
		"tests:\n" +
		"  - query: SELECT COALESCE(id, 0) FROM t\n" +
		"    rows: [[0]]\n" +
		"  - query: SELECT NULLIF(id, 0) FROM t\n" +
		"    error_code: \"42883\"\n" +
		"# ansi: F261-04\n" // positive tag, but the scenario also rejects NULLIF and declares no gap
	if err := os.WriteFile(filepath.Join(dir, "mixed.yaml"), []byte(mixed), 0o644); err != nil {
		t.Fatal(err)
	}
	_, violations, err := collectAnsiEvidence(dir, ansiCoreRoster)
	if err != nil {
		t.Fatalf("collect ANSI evidence: %v", err)
	}
	if !containsSubstr(violations, "no `# ansi-gap:`") {
		t.Fatalf("cross-feature guard did not fire on positive tag + unsupported pin with no gap tag; got %v", violations)
	}
}

// TestAnsiConflictGuard pins the conflict rule: the same feature ID cannot be
// tagged both `# ansi:` (supported) and `# ansi-gap:` (unsupported).
func TestAnsiConflictGuard(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	conflict := "name: conflict\n" +
		"schema_template: |\n" +
		"  CREATE TABLE t (id BIGINT, PRIMARY KEY (id))\n" +
		"tests:\n" +
		"  - query: SELECT id FROM t\n" +
		"    rows: [[1]]\n" +
		"  - query: SELECT bad FROM t\n" +
		"    error_code: \"0A000\"\n" +
		"# ansi: E051\n" +
		"# ansi-gap: E051\n"
	if err := os.WriteFile(filepath.Join(dir, "conflict.yaml"), []byte(conflict), 0o644); err != nil {
		t.Fatal(err)
	}
	_, violations, err := collectAnsiEvidence(dir, ansiCoreRoster)
	if err != nil {
		t.Fatalf("collect ANSI evidence: %v", err)
	}
	if !containsSubstr(violations, "tagged both") {
		t.Fatalf("conflict guard did not fire on same-ID pos+gap; got %v", violations)
	}
}

func containsSubstr(ss []string, sub string) bool {
	for _, s := range ss {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}
