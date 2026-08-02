package main

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"fdb.dev/pkg/relational/conformance/factory"
	"fdb.dev/pkg/relational/conformance/factorycorpus"
	"fdb.dev/pkg/relational/conformance/yamsql"
)

// TestPersistKeepsFindingsWhenTheRestOfTheRunFails pins that an oracle
// disagreement survives a failure in any later persistence step.
//
// The reproducer is the real one, not a contrived error: a batch pointed at an
// EMPTY corpus directory. Finish re-reads the directory through the corpus
// loader, which rejects an empty one on purpose (a gate over an empty corpus
// passes vacuously), so bootstrapping into a fresh directory always takes the
// infra exit path. Ordering findings after that step meant such a run reported
// the infra failure and threw away the disagreements it had found — the one
// output of a factory run that cannot be reconstructed from anything else on
// disk.
//
// The assertion is deliberately BOTH halves: the run still fails, and the
// evidence is still there. Persisting the findings must not swallow the error.
func TestPersistKeepsFindingsWhenTheRestOfTheRunFails(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	corpus := filepath.Join(dir, "corpus")
	if err := os.MkdirAll(corpus, 0o755); err != nil {
		t.Fatalf("mkdir corpus: %v", err)
	}
	batch, err := factory.NewBatch(corpus, 10)
	if err != nil {
		t.Fatalf("NewBatch: %v", err)
	}
	cfg := config{
		seedStart:    1,
		seeds:        1,
		out:          corpus,
		findings:     filepath.Join(dir, "findings"),
		manifest:     filepath.Join(dir, "factory-manifest.json"),
		date:         "2026-07-31",
		updateCensus: false,
	}
	findings := []*factory.Finding{{
		Oracle:    "tlp",
		Seed:      7,
		Candidate: "s7_q0_p0",
		Detail:    "partition returned 3 rows, whole returned 4",
		Queries:   []string{"SELECT * FROM t WHERE a > 1"},
	}}

	if _, code := persist(cfg, batch, findings, "metamorphic", nil); code != exitInfra {
		t.Fatalf("persist returned exit %d, want %d: Finish over an empty corpus directory must still fail the run", code, exitInfra)
	}

	entries, err := os.ReadDir(cfg.findings)
	if err != nil {
		t.Fatalf("the findings directory does not exist after a run that found an oracle disagreement: %v — "+
			"the disagreement was DESTROYED by a later step failing, and it is the loudest signal the factory produces", err)
	}
	if len(entries) != 1 {
		t.Fatalf("findings directory holds %d files, want 1", len(entries))
	}
	data, err := os.ReadFile(filepath.Join(cfg.findings, entries[0].Name()))
	if err != nil {
		t.Fatalf("read finding: %v", err)
	}
	if len(data) == 0 {
		t.Fatalf("%s is empty", entries[0].Name())
	}
}

// TestPersistKeepsFindingsWhenTheSweepDies covers the other way a run ends
// with findings in memory: the sweep itself fails.
//
// It is the more expensive version of the same loss. A sweep that dies on seed
// 300 is holding 299 seeds' worth of disagreements, and returning from inside
// the loop threw all of them away — a whole night of container time, plus
// however long it takes for some future run to draw those seeds again. The
// corpus directory here is deliberately VALID, so the only thing that can end
// the run is the sweep error; a test that also relied on an empty corpus could
// not tell the two paths apart.
func TestPersistKeepsFindingsWhenTheSweepDies(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	corpus := seededCorpus(t, dir)
	batch, err := factory.NewBatch(corpus, 10)
	if err != nil {
		t.Fatalf("NewBatch: %v", err)
	}
	cfg := config{
		seedStart:    1,
		seeds:        400,
		out:          corpus,
		findings:     filepath.Join(dir, "findings"),
		manifest:     filepath.Join(dir, "factory-manifest.json"),
		date:         "2026-07-31",
		updateCensus: false,
	}
	findings := []*factory.Finding{{Oracle: "second-plan", Seed: 299, Candidate: "s299_q1_p0"}}

	_, code := persist(cfg, batch, findings, "metamorphic", errors.New("seed 300: connection reset"))
	if code != exitInfra {
		t.Fatalf("persist returned exit %d, want %d: a dead sweep must fail the run", code, exitInfra)
	}
	entries, err := os.ReadDir(cfg.findings)
	if err != nil || len(entries) != 1 {
		t.Fatalf("the sweep's findings did not survive the sweep failing (dir err %v, %d files): "+
			"every path out of a run that holds findings must persist them first", err, len(entries))
	}
	// A failed sweep must NOT leave a manifest claiming the run completed.
	if _, err := os.Stat(cfg.manifest); err == nil {
		t.Errorf("a manifest was written for a run whose sweep died; it would read as a completed batch")
	}
}

// seededCorpus builds a temp corpus directory holding one valid scenario, so
// the batch loader has something to measure.
//
// The scenario is SYNTHESIZED through the writer rather than copied out of the
// committed corpus. Copying coupled a test about persistence ordering to 6.6MB
// of testdata it never reads, and it failed in exactly the state this file
// exists to protect: a run against an EMPTY corpus directory, which is the
// bootstrap case where findings are most likely to be the only output worth
// keeping. It also broke under Bazel, where a relative path out of the package
// reaches nothing unless the data is declared — a dependency the test did not
// need in the first place.
func seededCorpus(t *testing.T, dir string) string {
	t.Helper()
	corpus := filepath.Join(dir, "testdata")
	if err := os.MkdirAll(corpus, 0o755); err != nil {
		t.Fatalf("mkdir corpus: %v", err)
	}
	const name = "fc_0000000001_q0_p0"
	const fv = "shape=single;idx=A;proj=star;where=cmp.eq;order=none"
	const shape = "0123456789abcdef"
	data, err := factorycorpus.MarshalFamily([]*factorycorpus.Scenario{{
		Header: factorycorpus.Header{
			Name:          name,
			Generator:     factory.GeneratorVersion,
			Seed:          1,
			Date:          "2026-07-31",
			Blessing:      factorycorpus.BlessingMetamorphic,
			Oracles:       []string{"tlp", "second-plan"},
			FeatureVector: fv,
			PlanShape:     shape,
			DedupKey:      factorycorpus.DedupKeyOf(fv, shape),
		},
		Doc: &yamsql.Scenario{
			Name:           name,
			SchemaTemplate: "CREATE TABLE t (id BIGINT NOT NULL, a BIGINT, PRIMARY KEY (id))",
			Setup:          []string{"INSERT INTO t VALUES (1, 2)"},
			Tests: []yamsql.Test{{
				Query:     "SELECT id FROM t WHERE a = 2",
				Unordered: true,
				Columns:   []string{"ID"},
				Rows:      [][]any{{int64(1)}},
			}},
		},
	}})
	if err != nil {
		t.Fatalf("marshal seed scenario: %v", err)
	}
	fileName := factorycorpus.FamilyFileName(factorycorpus.FamilyOf(fv))
	if err := os.WriteFile(filepath.Join(corpus, fileName), data, 0o644); err != nil {
		t.Fatalf("write corpus file: %v", err)
	}
	return corpus
}

// TestPersistWritesManifestAndCensusOnASuccessfulRun pins that reordering the
// findings to the front did not drop the outputs that follow them.
//
// A durability fix that quietly stopped writing the manifest would pass the
// test above and break every consumer of the batch ledger, so the healthy path
// is asserted in the same file.
func TestPersistWritesManifestAndCensusOnASuccessfulRun(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	corpus := seededCorpus(t, dir)
	batch, err := factory.NewBatch(corpus, 10)
	if err != nil {
		t.Fatalf("NewBatch: %v", err)
	}
	cfg := config{
		seedStart:    1,
		seeds:        1,
		out:          corpus,
		findings:     filepath.Join(dir, "findings"),
		manifest:     filepath.Join(dir, "factory-manifest.json"),
		date:         "2026-07-31",
		updateCensus: true,
	}
	manifest, code := persist(cfg, batch, nil, "metamorphic", nil)
	if code != exitOK {
		t.Fatalf("persist returned exit %d, want %d", code, exitOK)
	}
	if manifest.Census.Scenarios != 1 {
		t.Errorf("census counts %d scenarios, want 1", manifest.Census.Scenarios)
	}
	for _, want := range []string{cfg.manifest, censusPath(cfg.out)} {
		if _, err := os.Stat(want); err != nil {
			t.Errorf("%s was not written: %v", want, err)
		}
	}
}
