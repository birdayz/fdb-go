package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"fdb.dev/pkg/relational/conformance/factory"
	"fdb.dev/pkg/relational/conformance/factorycorpus"
)

// TestPersistVerifiesTheCommittedBaselineByDefault is the gate on the gate.
//
// A producer that rewrites its own baseline every run makes the ratchet green
// BY CONSTRUCTION: whatever the batch produced becomes the standard the batch
// is measured against. No run can fail it, the automated lane that commits the
// result never sees a shrink, and the census line in the batch PR still reads
// like evidence — a gate that cannot fire, which is strictly worse than no
// gate because it is trusted.
//
// So the default path VERIFIES and only an explicit flag rewrites. Both halves
// are asserted here: verifying must catch a downgrade, and rewriting must
// actually raise the file, or the flag is decorative in the other direction.
func TestPersistVerifiesTheCommittedBaselineByDefault(t *testing.T) {
	t.Parallel()

	// A baseline claiming authority the corpus on disk does not have. The
	// scenario the temp corpus holds is blessed metamorphic; the baseline says
	// that same dedup key was cross-engine, so the corpus has been DOWNGRADED.
	seedBaseline := func(t *testing.T, dir, corpus string, blessing factorycorpus.Blessing) {
		t.Helper()
		files, err := factorycorpus.LoadDir(corpus)
		if err != nil {
			t.Fatalf("LoadDir: %v", err)
		}
		c := factorycorpus.ComputeCensus(files)
		for k := range c.ByKeyBlessing {
			c.ByKeyBlessing[k] = string(blessing)
		}
		data, err := factorycorpus.RenderCensus(c)
		if err != nil {
			t.Fatalf("RenderCensus: %v", err)
		}
		if err := os.WriteFile(censusPath(corpus), data, 0o644); err != nil {
			t.Fatalf("write baseline: %v", err)
		}
	}

	newCfg := func(dir, corpus string, update bool) config {
		return config{
			seedStart: 1, seeds: 1, quota: 10, date: "2026-07-31",
			out:          corpus,
			manifest:     filepath.Join(dir, "manifest.json"),
			findings:     filepath.Join(dir, "findings"),
			updateCensus: update,
		}
	}

	t.Run("a downgrade is refused", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		corpus := seededCorpus(t, dir)
		seedBaseline(t, dir, corpus, factorycorpus.BlessingCrossEngine)

		batch, err := factory.NewBatch(corpus, 10)
		if err != nil {
			t.Fatalf("NewBatch: %v", err)
		}
		_, code := persist(newCfg(dir, corpus, false), batch, nil, "metamorphic", nil)
		if code != exitInfra {
			t.Fatalf("exit %d for a batch that downgraded a committed scenario's authority, want %d (infra). "+
				"The baseline is being trusted without being checked", code, exitInfra)
		}
	})

	t.Run("verifying does not rewrite", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		corpus := seededCorpus(t, dir)
		seedBaseline(t, dir, corpus, factorycorpus.BlessingCrossEngine)
		before, err := os.ReadFile(censusPath(corpus))
		if err != nil {
			t.Fatal(err)
		}

		batch, err := factory.NewBatch(corpus, 10)
		if err != nil {
			t.Fatalf("NewBatch: %v", err)
		}
		persist(newCfg(dir, corpus, false), batch, nil, "metamorphic", nil)

		after, err := os.ReadFile(censusPath(corpus))
		if err != nil {
			t.Fatal(err)
		}
		if string(before) != string(after) {
			t.Error("the default path REWROTE the committed baseline. A producer that restates its own " +
				"standard every run has a ratchet that can never fire")
		}
	})

	t.Run("the explicit flag raises it", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		corpus := seededCorpus(t, dir)
		seedBaseline(t, dir, corpus, factorycorpus.BlessingMetamorphic)

		batch, err := factory.NewBatch(corpus, 10)
		if err != nil {
			t.Fatalf("NewBatch: %v", err)
		}
		if _, code := persist(newCfg(dir, corpus, true), batch, nil, "metamorphic", nil); code != exitOK {
			t.Fatalf("exit %d raising the baseline on a corpus that did not shrink, want %d", code, exitOK)
		}
		raised, err := factorycorpus.LoadCensus(censusPath(corpus))
		if err != nil {
			t.Fatalf("read raised baseline: %v", err)
		}
		if raised.Scenarios != 1 {
			t.Errorf("raised baseline records %d scenarios, want the 1 on disk — the flag did not measure "+
				"the corpus", raised.Scenarios)
		}
	})

	t.Run("a missing baseline is an error unless bootstrapping", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		corpus := seededCorpus(t, dir)
		batch, err := factory.NewBatch(corpus, 10)
		if err != nil {
			t.Fatalf("NewBatch: %v", err)
		}
		// No baseline written. Verifying against a file that is not there must
		// fail rather than pass vacuously — "no baseline" and "baseline met"
		// are the same outcome to a gate that treats a read error as empty.
		if _, code := persist(newCfg(dir, corpus, false), batch, nil, "metamorphic", nil); code != exitInfra {
			t.Errorf("exit %d with NO committed baseline to verify against, want %d — a ratchet with no "+
				"baseline passes everything", code, exitInfra)
		}
	})
}

// TestCensusPathSitsBesideTheCorpus pins that the baseline is not inside the
// directory the corpus loader globs. A census file under testdata/ would be
// picked up as a scenario and fail to parse as one.
func TestCensusPathSitsBesideTheCorpus(t *testing.T) {
	t.Parallel()
	p := censusPath("pkg/relational/conformance/factorycorpus/testdata")
	if strings.HasSuffix(filepath.Dir(filepath.Clean(p)), "testdata") {
		t.Fatalf("the census baseline resolves to %q, inside the corpus directory; the loader globs that "+
			"directory and would read it as a scenario", p)
	}
	if filepath.Base(p) != factorycorpus.CensusBaselinePath {
		t.Errorf("census path basename is %q, want %q", filepath.Base(p), factorycorpus.CensusBaselinePath)
	}
}
