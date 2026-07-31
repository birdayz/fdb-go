package factorycorpus_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"fdb.dev/pkg/relational/conformance/factorycorpus"
)

// TestHeaderRejectsADuplicateKey pins that a repeated provenance key is an
// error rather than last-wins.
//
// The header block is a SET of facts about the file, but it is parsed line by
// line, so a repeated key silently resolves to whichever line came last. That
// makes the file's provenance depend on line order for a block nothing else
// treats as ordered — and it splits what a human reads from what the census
// counts. A reader scanning the header sees the first `dedup-key:` and
// reasons about that point; the loader takes the second and files the scenario
// somewhere else.
//
// Both arrival routes are covered, because they fail differently. A hand-edit
// that appends a corrected line instead of replacing one leaves the ORIGINAL
// value first; a writer change that emits a key twice leaves two values that
// may both be "right" while the file is still ambiguous.
func TestHeaderRejectsADuplicateKey(t *testing.T) {
	t.Parallel()
	h := sampleHeader("fc_dup")
	full := h.Render()

	// Sanity: the unmodified header must parse, or every case below passes for
	// the wrong reason.
	if _, err := factorycorpus.ParseHeader([]byte(full)); err != nil {
		t.Fatalf("the unmodified header does not parse: %v", err)
	}

	for _, tc := range []struct {
		name      string
		extraLine string
	}{
		{"a corrected date appended after the original", "# date: 2099-01-01"},
		{"a second dedup-key, the corpus's primary key", "# dedup-key: deadbeefdeadbeef"},
		{"a second feature-vector, the axis the census counts on", "# feature-vector: shape=agg;order=none"},
		{"a repeated key carrying the SAME value", "# seed: 4242"},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			doubled := strings.TrimRight(full, "\n") + "\n" + tc.extraLine + "\n"
			if _, err := factorycorpus.ParseHeader([]byte(doubled)); err == nil {
				t.Fatalf("a header carrying %q twice parsed clean; the duplicate resolved silently to one of "+
					"the two lines and nothing downstream can tell which", tc.extraLine)
			}
		})
	}

	// Prose lines are NOT keys and must stay free-form: the banner is
	// deliberately re-wordable without breaking older files, and a duplicate
	// check that swept up prose would freeze it.
	withProse := strings.TrimRight(full, "\n") + "\n# some prose line\n# some prose line\n"
	if _, err := factorycorpus.ParseHeader([]byte(withProse)); err != nil {
		t.Fatalf("repeated PROSE lines were rejected (%v); only `key: value` lines are the contract", err)
	}
}

// TestLoadRejectsAScenarioWithNoTests pins the fact that makes the corpus
// runners' TestsRun == 0 guard latent rather than dead.
//
// Both execution tiers refuse a scenario that ran zero tests, because zero
// failures out of zero tests passes every other assertion they make and would
// report the corpus green while executing none of it. Today that guard cannot
// fire: the loader rejects a testless scenario before a runner ever sees one,
// and the writer refuses to emit one.
//
// Nothing pinned that. Relaxing either check — to tolerate a scenario that is
// setup-only, say — would silently ARM the vacuous-pass path, and the only
// thing standing in front of it would be a guard nobody remembered was there.
// This test is what makes that trade visible: if it goes red, the guards in
// sample_fdb_test.go and full/full_corpus_fdb_test.go have just become
// load-bearing.
func TestLoadRejectsAScenarioWithNoTests(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	h := sampleHeader("fc_notests")
	doc := h.Render() +
		"name: fc_notests\n" +
		"schema_template: CREATE TABLE t (id BIGINT NOT NULL, PRIMARY KEY (id))\n" +
		"setup:\n  - INSERT INTO t VALUES (1)\n" +
		"tests: []\n"
	path := filepath.Join(dir, "fc_notests.yaml")
	if err := os.WriteFile(path, []byte(doc), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := factorycorpus.Load(path); err == nil {
		t.Fatal("a committed scenario with ZERO tests loaded clean. It asserts nothing, it cannot fail, and " +
			"both corpus runners would report it green — their TestsRun == 0 guards are now the only thing " +
			"between this file and a vacuous pass, so verify they are still in place before allowing this.")
	}
}
