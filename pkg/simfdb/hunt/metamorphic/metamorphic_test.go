package metamorphic

import (
	"os"
	"path/filepath"
	"testing"
)

// TestSeedCorpus runs the hand-written equivalence relations: a correct engine must return ZERO
// violations (every asserted-equivalent query group returns identical rows over SimFDB). A
// violation here is either a real engine bug or a wrong seed relation — investigate, don't silence.
func TestSeedCorpus(t *testing.T) {
	t.Parallel()
	for _, s := range SeedCorpus() {
		s := s
		t.Run(s.Name, func(t *testing.T) {
			t.Parallel()
			viols, err := Check(s)
			if err != nil {
				t.Fatalf("check setup: %v", err)
			}
			for _, v := range viols {
				t.Errorf("metamorphic violation: %s", v)
			}
		})
	}
}

// findingsDir holds the scenarios the adversarial metamorphic loop produced. They are checked in
// as data; this is what makes them TESTS.
const findingsDir = "testdata/findings"

// TestFindingsAreRegressionSentinels runs every checked-in finding and requires ZERO violations.
//
// These scenarios were minted when the loop caught real engine bugs. Those bugs are fixed, so
// each file's equivalence now HOLDS — which makes the file a regression sentinel, and a sentinel
// that nothing runs is a text file. Shipping them as data with a README saying "not wired into
// any test, RED until the engine bugs are fixed" left three reproducers guarding nothing, while
// the README's claim was false at HEAD (the judge reports no inequivalence for any of them).
//
// A violation here means an engine regression on a shape that was already broken once — the
// highest-value signal this suite can produce. Do not silence it and do not delete the file:
// find the regression.
func TestFindingsAreRegressionSentinels(t *testing.T) {
	t.Parallel()
	scenarios, err := LoadDir(findingsDir)
	if err != nil {
		t.Fatalf("load %s: %v", findingsDir, err)
	}
	// A count floor, so deleting or renaming a finding file cannot silently empty this test.
	// Every .json in the directory is a scenario someone paid for with a real bug.
	files, err := os.ReadDir(findingsDir)
	if err != nil {
		t.Fatalf("read %s: %v", findingsDir, err)
	}
	var jsonCount int
	for _, f := range files {
		if !f.IsDir() && filepath.Ext(f.Name()) == ".json" {
			jsonCount++
		}
	}
	if jsonCount == 0 {
		t.Fatalf("%s holds no .json scenarios: the reproducer corpus is empty", findingsDir)
	}
	if len(scenarios) < jsonCount {
		t.Fatalf("loaded %d scenarios from %d .json files — a reproducer failed to parse and "+
			"would have been silently skipped", len(scenarios), jsonCount)
	}

	for _, s := range scenarios {
		s := s
		t.Run(s.Name, func(t *testing.T) {
			t.Parallel()
			viols, err := Check(s)
			if err != nil {
				t.Fatalf("check setup: %v", err)
			}
			for _, v := range viols {
				t.Errorf("REGRESSION on a known-bug reproducer: %s", v)
			}
		})
	}
}

// TestTeeth proves the oracle catches a real inequivalence: two queries that are NOT equivalent
// must produce a violation. Without this, a green seed corpus could mean a toothless checker.
func TestTeeth(t *testing.T) {
	t.Parallel()
	bad := Scenario{
		Name:   "teeth",
		Seed:   2,
		Tables: []string{"CREATE TABLE t (id BIGINT NOT NULL, a BIGINT, PRIMARY KEY (id))"},
		Data: []string{
			"INSERT INTO t (id, a) VALUES (1, 1)",
			"INSERT INTO t (id, a) VALUES (2, 5)",
			"INSERT INTO t (id, a) VALUES (3, 9)",
		},
		Groups: []Group{{
			Name:   "not-equivalent",
			Reason: "deliberately false: a>3 is not a<3",
			Queries: []string{
				"SELECT id FROM t WHERE a > 3",
				"SELECT id FROM t WHERE a < 3",
			},
		}},
	}
	viols, err := Check(bad)
	if err != nil {
		t.Fatalf("check: %v", err)
	}
	if len(viols) == 0 {
		t.Fatal("oracle has no teeth: a non-equivalent group produced no violation")
	}
	t.Logf("teeth confirmed: %s", viols[0])
}

// TestDeterminism: the check is stable — same scenario, same violations, twice.
func TestDeterminism(t *testing.T) {
	t.Parallel()
	s := SeedCorpus()[0]
	a, err := Check(s)
	if err != nil {
		t.Fatal(err)
	}
	b, err := Check(s)
	if err != nil {
		t.Fatal(err)
	}
	if len(a) != len(b) {
		t.Fatalf("nondeterministic check: %d vs %d violations", len(a), len(b))
	}
}

// TestLoadDir round-trips a generated-style JSON scenario through the loader.
func TestLoadDir(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	js := `[{"name":"gen1","seed":3,"tables":["CREATE TABLE t (id BIGINT NOT NULL, a BIGINT, PRIMARY KEY (id))"],
		"data":["INSERT INTO t (id,a) VALUES (1,1)"],
		"groups":[{"name":"g","reason":"trivial","queries":["SELECT id FROM t","SELECT id FROM t"]}]}]`
	if err := os.WriteFile(filepath.Join(dir, "gen.json"), []byte(js), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := LoadDir(dir)
	if err != nil {
		t.Fatalf("LoadDir: %v", err)
	}
	if len(got) != 1 || got[0].Name != "gen1" || len(got[0].Groups) != 1 {
		t.Fatalf("unexpected load: %+v", got)
	}
	if viols, err := Check(got[0]); err != nil || len(viols) != 0 {
		t.Fatalf("loaded scenario check: viols=%v err=%v", viols, err)
	}
}
