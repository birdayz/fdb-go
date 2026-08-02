package factory_test

import (
	"os"
	"path/filepath"
	"testing"

	"fdb.dev/pkg/relational/conformance/explaindiff"
	"fdb.dev/pkg/relational/conformance/factory"
	"fdb.dev/pkg/relational/conformance/factorycorpus"
	"fdb.dev/pkg/relational/core/embedded"
)

const corpusDir = "../factorycorpus/testdata"

// TestFactoryDeterminism is RFC-201 §5.4's gate: a committed generator plus a
// committed seed regenerates a committed file.
//
// It regenerates everything EXCEPT the rows, and that is the honest boundary,
// not a shortcut. The rows are oracle OUTPUT — reproducing them means running
// the query against a real cluster, which the fast tier has no container for.
// Everything upstream of the rows is pure computation and is checked here for
// EVERY committed file rather than a sample: the candidate the seed derives,
// the SQL of all four renderings, the feature vector, the plan shape, and the
// dedup key those two digest into.
//
// That is the property that matters. If a generator change silently alters
// what a seed produces, every committed file's reproduction recipe becomes a
// lie — the header says "regenerate with seed N" and seed N now makes
// something else. This goes red on the first such change, across the whole
// corpus, in seconds.
func TestFactoryDeterminism(t *testing.T) {
	t.Parallel()
	files := loadCorpus(t)

	// Candidates() is called once per seed and the result reused, both because
	// it is the expensive part and because doing it that way proves the
	// per-seed derivation is stable across the candidates that share it.
	bySeed := map[uint64][]factory.Candidate{}
	checked := 0
	for _, f := range files {
		h := f.Header
		if h.Generator != factory.GeneratorVersion {
			t.Errorf("%s: generator %q, this build is %q. A generator version bump must come with a re-blessed batch, "+
				"or the committed reproduction recipes no longer reproduce", f.Path, h.Generator, factory.GeneratorVersion)
			continue
		}
		cands, ok := bySeed[h.Seed]
		if !ok {
			cands = factory.Candidates(h.Seed)
			bySeed[h.Seed] = cands
		}
		var cand *factory.Candidate
		for i := range cands {
			if cands[i].QueryIndex == h.QueryIndex && cands[i].ProjIndex == h.Projection {
				cand = &cands[i]
				break
			}
		}
		if cand == nil {
			t.Errorf("%s: seed %d no longer yields a candidate at query %d / projection %d — the file's "+
				"reproduction recipe is dead", f.Path, h.Seed, h.QueryIndex, h.Projection)
			continue
		}
		if cand.Name() != h.Name {
			t.Errorf("%s: regenerated candidate is named %q", f.Path, cand.Name())
			continue
		}
		if cand.FeatureVector != h.FeatureVector {
			t.Errorf("%s: feature vector drifted\n  header:      %s\n  regenerated: %s",
				f.Path, h.FeatureVector, cand.FeatureVector)
		}

		queries := cand.TLPQueries()
		if len(queries) != len(f.Doc.Tests) {
			t.Errorf("%s: %d committed tests, generator now yields %d renderings",
				f.Path, len(f.Doc.Tests), len(queries))
			continue
		}
		for i, q := range queries {
			want := cand.Case.SQL(q, cand.Projection)
			if got := f.Doc.Tests[i].Query; got != want {
				t.Errorf("%s: tests[%d] (%s) SQL drifted\n  committed:   %s\n  regenerated: %s",
					f.Path, i, factory.TLPLabels[i], got, want)
			}
		}
		if got, want := f.Doc.SchemaTemplate, cand.Case.DDL(); got != want {
			t.Errorf("%s: schema_template drifted\n  committed:   %s\n  regenerated: %s", f.Path, got, want)
		}
		if len(f.Doc.Setup) != 1 || f.Doc.Setup[0] != cand.Case.InsertSQL() {
			t.Errorf("%s: setup drifted; the frozen rows were computed over different data", f.Path)
		}

		plan, err := embedded.PlanPhysicalForTest(cand.Case.SQL(queries[1], cand.Projection), cand.Case.DDL(), nil)
		if err != nil {
			t.Errorf("%s: the committed query no longer plans: %v", f.Path, err)
			continue
		}
		shape := factorycorpus.ShapeDigest(explaindiff.ShapeOf(plan))
		if shape != h.PlanShape {
			t.Errorf("%s: plan shape drifted (header %s, now %s). The dedup key is derived from it, so the "+
				"corpus may now hold two files at one point or none at another", f.Path, h.PlanShape, shape)
		}
		if key := factorycorpus.DedupKeyOf(cand.FeatureVector, shape); key != h.DedupKey {
			t.Errorf("%s: dedup key drifted (header %s, now %s)", f.Path, h.DedupKey, key)
		}
		checked++
	}

	if checked == 0 {
		t.Fatal("zero committed files were regenerated — this gate is vacuous")
	}
	t.Logf("regenerated %d committed files from %d seeds", checked, len(bySeed))
}

// TestCommittedFilesAreCanonical pins that every committed file is exactly
// what the writer emits. A file that differs — hand-edited, or written by an
// older writer — produces a spurious diff on the next re-bless, and a
// reader looking for the expectation that moved has to find it among
// reformatting.
func TestCommittedFilesAreCanonical(t *testing.T) {
	t.Parallel()
	matches, err := filepath.Glob(filepath.Join(corpusDir, "*.yamsql"))
	if err != nil || len(matches) == 0 {
		t.Fatalf("glob: %v (%d matches)", err, len(matches))
	}
	for _, path := range matches {
		onDisk, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		f, err := factorycorpus.Load(path)
		if err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		canonical, err := factorycorpus.MarshalFamily(f.Scenarios)
		if err != nil {
			t.Errorf("%s: re-marshal: %v", path, err)
			continue
		}
		if string(onDisk) != string(canonical) {
			t.Errorf("%s is not in canonical form; a re-bless would diff on formatting rather than on expectations",
				filepath.Base(path))
		}
	}
}
