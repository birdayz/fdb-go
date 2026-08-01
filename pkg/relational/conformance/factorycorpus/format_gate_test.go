package factorycorpus_test

import (
	"os"
	"path/filepath"
	"testing"

	"fdb.dev/pkg/relational/conformance/factorycorpus"
	"fdb.dev/pkg/relational/conformance/javayamsql"
)

// TestCommittedCorpusIsCleanYamsql is the emitted-format gate over the whole
// committed corpus: every family file must parse through the SAME strict
// parser that gates the vendored Java corpus, with zero inert directives.
//
// This is deliberately independent of the corpus loader (which also parses,
// but then keeps only what it reconstructs): the property being pinned is that
// the committed files are genuine yamsql — the Java yaml-tests runner could
// execute them verbatim, which is what makes cross-engine blessing literal —
// and that no committed key is one Java would silently ignore. Format drift in
// the writer breaks the build here, before any container is paid for.
func TestCommittedCorpusIsCleanYamsql(t *testing.T) {
	t.Parallel()
	matches, err := filepath.Glob(filepath.Join(factorycorpus.TestdataDir, "*.yamsql"))
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) == 0 {
		t.Fatal("no committed family files: this gate would pass over nothing")
	}
	blocks := 0
	for _, path := range matches {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		file, err := javayamsql.Parse(filepath.Base(path), data)
		if err != nil {
			t.Errorf("%s is not clean yamsql: %v", path, err)
			continue
		}
		for _, blk := range file.Blocks {
			blocks++
			for _, inert := range blk.Inert {
				t.Errorf("%s: %s block carries inert %s — Java would silently ignore it, so the file claims a check it does not make",
					path, blk.Kind, inert)
			}
		}
	}
	t.Logf("parsed %d family files, %d blocks, all clean yamsql", len(matches), blocks)
}

// TestFamilyPlacementIsSelfConsistent pins the grouping convention itself:
// every committed scenario's feature vector must map onto the family file it
// sits in, and the family key must round-trip through the file-name mapping.
// This is what makes FamilyOf changeable only WITH a corpus regeneration —
// silently changing the mapping would strand every committed scenario in a
// file whose name no longer derives from its content.
func TestFamilyPlacementIsSelfConsistent(t *testing.T) {
	t.Parallel()
	scenarios, err := factorycorpus.LoadDir(factorycorpus.TestdataDir)
	if err != nil {
		t.Fatalf("LoadDir: %v", err)
	}
	families := map[string]bool{}
	for _, s := range scenarios {
		if err := factorycorpus.CheckFamilyPlacement(s.Path, s.Header.FeatureVector); err != nil {
			t.Error(err)
		}
		families[factorycorpus.FamilyOf(s.Header.FeatureVector)] = true
	}
	t.Logf("%d scenarios across %d families", len(scenarios), len(families))
}
