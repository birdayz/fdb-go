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
// corpus: every `ansi:` tag sits on a test that passes and every `ansi_gap:` on a
// test that is an unsupported pin (per-test binding — §4.3). A mis-tag here is a fake checkbox and fails the build.
func TestAnsiLedgerEvidenceExists(t *testing.T) {
	t.Parallel()
	_, violations, err := collectAnsiEvidence("testdata", ansiCoreRoster)
	if err != nil {
		t.Fatalf("collect ANSI evidence: %v", err)
	}
	if len(violations) > 0 {
		t.Fatalf("ANSI tag evidence violations (a `ansi:` tag without a passing test, a "+
			"`ansi_gap:` without an unsupported pin, or an unknown/typo'd ID):\n%s", strings.Join(violations, "\n"))
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
		"    ansi: [E0511]\n" + // typo: extra digit — not a real roster ID
		"    rows: [[1]]\n"
	if err := os.WriteFile(filepath.Join(dir, "phantom.yaml"), []byte(phantom), 0o644); err != nil {
		t.Fatal(err)
	}
	_, violations, err := collectAnsiEvidence(dir, ansiCoreRoster)
	if err != nil {
		t.Fatalf("collect ANSI evidence: %v", err)
	}
	if !containsSubstr(violations, "unknown ANSI ID") {
		t.Fatalf("phantom-ID guard did not fire on `ansi: [E0511]`; got %v", violations)
	}
}

// TestAnsiEvidenceGuardBites proves the per-test binding catches a fake checkbox
// (RFC-165 §7): a positive `ansi:` tag placed on a test that does NOT pass (it
// asserts an error) MUST be reported. "Evidence" can't decay to "a tag exists" —
// the tag is bound to its own test's outcome.
func TestAnsiEvidenceGuardBites(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	// E051 tagged positive ON a test whose outcome is a rejection (0A000).
	bad := "name: badtag\n" +
		"schema_template: |\n" +
		"  CREATE TABLE t (id BIGINT, PRIMARY KEY (id))\n" +
		"tests:\n" +
		"  - query: SELECT COUNT(DISTINCT id) FROM t\n" +
		"    ansi: [E051]\n" +
		"    error_code: \"0A000\"\n"
	if err := os.WriteFile(filepath.Join(dir, "badtag.yaml"), []byte(bad), 0o644); err != nil {
		t.Fatal(err)
	}
	_, violations, err := collectAnsiEvidence(dir, ansiCoreRoster)
	if err != nil {
		t.Fatalf("collect ANSI evidence: %v", err)
	}
	if !containsSubstr(violations, "does not pass") {
		t.Fatalf("evidence guard did not bite: `ansi: [E051]` on a rejection test produced no violation; got %v", violations)
	}
}

// TestAnsiGapWrongOutcomeBites is the gap-side mirror: an `ansi_gap:` tag placed
// on a test that PASSES (not a rejection) must be flagged.
func TestAnsiGapWrongOutcomeBites(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	bad := "name: gapbad\n" +
		"schema_template: |\n" +
		"  CREATE TABLE t (id BIGINT, PRIMARY KEY (id))\n" +
		"tests:\n" +
		"  - query: SELECT id FROM t\n" +
		"    ansi_gap: [E051]\n" +
		"    rows: [[1]]\n"
	if err := os.WriteFile(filepath.Join(dir, "gapbad.yaml"), []byte(bad), 0o644); err != nil {
		t.Fatal(err)
	}
	_, violations, err := collectAnsiEvidence(dir, ansiCoreRoster)
	if err != nil {
		t.Fatalf("collect ANSI evidence: %v", err)
	}
	if !containsSubstr(violations, "not an unsupported-feature pin") {
		t.Fatalf("gap guard did not fire on `ansi_gap: [E051]` on a passing test; got %v", violations)
	}
}

// TestAnsiPerTestMixedScenarioClean is the regression for the F261-01 class: a
// scenario that exercises one feature positively AND rejects another feature in
// the same file is now SOUND — because each tag is bound to its own test, the
// positive tag is credited only off its passing test and the gap tag only off
// its rejection, with NO violation and NO sibling-crediting (the exact case that
// produced the shipped fake-checkbox under the old scenario-level model).
func TestAnsiPerTestMixedScenarioClean(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	mixed := "name: mixed\n" +
		"schema_template: |\n" +
		"  CREATE TABLE t (id BIGINT, PRIMARY KEY (id))\n" +
		"tests:\n" +
		"  - query: SELECT COALESCE(id, 0) FROM t\n" +
		"    ansi: [F261-04]\n" +
		"    rows: [[0]]\n" +
		"  - query: SELECT NULLIF(id, 0) FROM t\n" +
		"    ansi_gap: [F261-03]\n" +
		"    error_code: \"42883\"\n"
	if err := os.WriteFile(filepath.Join(dir, "mixed.yaml"), []byte(mixed), 0o644); err != nil {
		t.Fatal(err)
	}
	ev, violations, err := collectAnsiEvidence(dir, ansiCoreRoster)
	if err != nil {
		t.Fatalf("collect ANSI evidence: %v", err)
	}
	if len(violations) != 0 {
		t.Fatalf("a correctly per-test-tagged mixed scenario should produce no violations; got %v", violations)
	}
	if ev["F261-04"] == nil || len(ev["F261-04"].positive) == 0 {
		t.Fatal("COALESCE (F261-04) should be credited positive off its own passing test")
	}
	if ev["F261-03"] == nil || len(ev["F261-03"].gaps) == 0 {
		t.Fatal("NULLIF (F261-03) should be credited as a gap off its own rejection test")
	}
	// F261-04 must NOT have been credited off the NULLIF rejection, and F261-03
	// must NOT be credited positive off the COALESCE pass.
	if ev["F261-04"] != nil && len(ev["F261-04"].gaps) != 0 {
		t.Fatal("F261-04 wrongly credited as a gap")
	}
	if ev["F261-03"] != nil && len(ev["F261-03"].positive) != 0 {
		t.Fatal("F261-03 wrongly credited positive (the old sibling-crediting bug)")
	}
}

// TestAnsiConflictGuard pins the conflict rule: the same feature ID cannot be
// tagged both `ansi:` (supported) and `ansi_gap:` (unsupported).
func TestAnsiConflictGuard(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	conflict := "name: conflict\n" +
		"schema_template: |\n" +
		"  CREATE TABLE t (id BIGINT, PRIMARY KEY (id))\n" +
		"tests:\n" +
		"  - query: SELECT id FROM t\n" +
		"    ansi: [E051]\n" +
		"    rows: [[1]]\n" +
		"  - query: SELECT bad FROM t\n" +
		"    ansi_gap: [E051]\n" +
		"    error_code: \"0A000\"\n"
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

// TestAnsiCrossFileConflictGuard pins the GLOBAL conflict guard (audit finding):
// the same atomic ID tagged `ansi:` in one scenario and `ansi_gap:` in
// ANOTHER must be flagged. The earlier per-file conflict check could not see this
// (its gapSet was rebuilt per file), so a Go rejection pin in file B could
// silently render as shared parity for a positive tag in file A. The global pass
// over the accumulated evidence catches it.
func TestAnsiCrossFileConflictGuard(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	pos := "name: posfile\n" +
		"schema_template: |\n" +
		"  CREATE TABLE t (id BIGINT, PRIMARY KEY (id))\n" +
		"tests:\n" +
		"  - query: SELECT id FROM t WHERE id BETWEEN 1 AND 5\n" +
		"    ansi: [E061-02]\n" +
		"    rows: [[1]]\n"
	gap := "name: gapfile\n" +
		"schema_template: |\n" +
		"  CREATE TABLE t (id BIGINT, PRIMARY KEY (id))\n" +
		"tests:\n" +
		"  - query: SELECT bad FROM t\n" +
		"    ansi_gap: [E061-02]\n" +
		"    error_code: \"0A000\"\n"
	for n, body := range map[string]string{"posfile.yaml": pos, "gapfile.yaml": gap} {
		if err := os.WriteFile(filepath.Join(dir, n), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	_, violations, err := collectAnsiEvidence(dir, ansiCoreRoster)
	if err != nil {
		t.Fatalf("collect ANSI evidence: %v", err)
	}
	if !containsSubstr(violations, "tagged both") {
		t.Fatalf("global conflict guard did not fire on CROSS-FILE pos+gap for E061-02; got %v", violations)
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

// TestAnsiTaggedCases pins the extractor the A3 Java?-verification lane consumes:
// it joins each per-test tag to the roster's Java fact and flags query-ness.
func TestAnsiTaggedCases(t *testing.T) {
	t.Parallel()
	cases, err := AnsiTaggedCases("testdata")
	if err != nil {
		t.Fatalf("AnsiTaggedCases: %v", err)
	}
	if len(cases) == 0 {
		t.Fatal("no tagged cases extracted")
	}
	byID := map[string]AnsiTaggedCase{}
	for _, c := range cases {
		byID[c.FeatureID] = c
		if c.Java == SupportUntested {
			t.Errorf("%s (%s): tagged feature has no roster Java fact (phantom?)", c.FeatureID, c.Scenario)
		}
		if c.Query == "" || c.SchemaTemplate == "" {
			t.Errorf("%s (%s): missing query or schema", c.FeatureID, c.Scenario)
		}
	}
	// NULLIF: gap-tagged, roster Java=None (both engines reject) — the case the
	// A3 lane would run against Java to confirm it really rejects.
	if c, ok := byID["F261-03"]; !ok || !c.Gap || c.Java != SupportNone {
		t.Errorf("F261-03 (NULLIF) expected gap-tagged with Java=None; got %+v ok=%v", c, ok)
	}
	// COALESCE: positive, Java=Full.
	if c, ok := byID["F261-04"]; !ok || c.Gap || c.Java != SupportFull {
		t.Errorf("F261-04 (COALESCE) expected positive with Java=Full; got %+v ok=%v", c, ok)
	}
}
