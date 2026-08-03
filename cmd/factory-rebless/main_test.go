package main

import (
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"fdb.dev/pkg/relational/conformance/factorycorpus"
)

// corpusSrc is the committed corpus, reached from this package's directory.
// Declared as a Bazel data dep so the files exist in the runfiles tree.
const corpusSrc = "../../pkg/relational/conformance/factorycorpus/testdata"

// The two family files these tests build fixtures from. Both are among the
// smallest in the corpus, and `earlyFile` sorts BEFORE `lateFile`, which is
// what the atomicity test needs: filepath.Glob output is sorted, so the tool
// reaches the early file first.
const (
	earlyFile = "single__and_between-cmp-numfn__none.yamsql"
	lateFile  = "single__and_not_bit-not_strfn__none.yamsql"
)

// fixtureCorpus copies the named committed family files into a fresh directory
// and returns it. The file NAMES are preserved because the loader cross-checks
// a file's `family:` key against its own base name.
func fixtureCorpus(t *testing.T, names ...string) string {
	t.Helper()
	dir := t.TempDir()
	for _, n := range names {
		data, err := os.ReadFile(filepath.Join(corpusSrc, n))
		if err != nil {
			t.Fatalf("read committed corpus file %s: %v", n, err)
		}
		if err := os.WriteFile(filepath.Join(dir, n), data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func mustRead(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// editFile applies a single literal substitution, failing if it does not match
// exactly once — a fixture that silently stopped applying is a test that
// silently stopped testing.
func editFile(t *testing.T, path, old, new string) {
	t.Helper()
	src := string(mustRead(t, path))
	if n := strings.Count(src, old); n != 1 {
		t.Fatalf("fixture edit %q matches %d times in %s, want exactly 1", old, n, path)
	}
	if err := os.WriteFile(path, []byte(strings.Replace(src, old, new, 1)), 0o644); err != nil {
		t.Fatal(err)
	}
}

// editAllFile replaces every occurrence, failing if there are none.
func editAllFile(t *testing.T, path, old, new string) {
	t.Helper()
	src := string(mustRead(t, path))
	if !strings.Contains(src, old) {
		t.Fatalf("fixture edit %q matches nothing in %s", old, path)
	}
	if err := os.WriteFile(path, []byte(strings.ReplaceAll(src, old, new)), 0o644); err != nil {
		t.Fatal(err)
	}
}

// staleDigest is the plan-shape digest a fixture file is stamped with to make
// the tool decide the file needs re-blessing. It is a well-formed 16-hex digest
// that no plan shape produces.
const staleDigest = "0000000000000000"

// makePlanShapeStale rewrites a fixture's committed plan-shape digest so the
// tool recomputes a different one and queues the file for rewrite.
//
// The dedup key is restamped with it, because the loader cross-checks the key
// against a digest of (feature vector, plan shape) — a header carrying only a
// stale shape does not load at all, so it would abort the run for a reason
// these tests are not about.
func makePlanShapeStale(t *testing.T, path string) {
	t.Helper()
	f, err := factorycorpus.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, sc := range f.Scenarios {
		sc.Header.PlanShape = staleDigest
		sc.Header.DedupKey = factorycorpus.DedupKeyOf(sc.Header.FeatureVector, staleDigest)
	}
	data, err := factorycorpus.MarshalFamily(f.Scenarios)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestReblessLeavesTheTreeUntouchedWhenALaterFileDrifts is the ATOMICITY arm.
//
// The corpus plus its census baseline is one consistent unit. A tool that
// writes file N and then aborts on file N+1 leaves a tree that is half
// re-blessed, with a census describing neither half — and nothing downstream
// can tell that state apart from a deliberate partial re-bless, because it is
// byte-identical to one.
//
// The fixture is the exact shape that produces it: an EARLY file whose headers
// are stale (so the tool queues a rewrite) followed by a LATE file that drifts
// fatally (so the run must abort). The assertion is that the early file is
// byte-identical afterwards.
func TestReblessLeavesTheTreeUntouchedWhenALaterFileDrifts(t *testing.T) {
	t.Parallel()
	dir := fixtureCorpus(t, earlyFile, lateFile)
	early, late := filepath.Join(dir, earlyFile), filepath.Join(dir, lateFile)

	// The early file needs a rewrite: its committed plan-shape digest no longer
	// matches what the planner computes.
	makePlanShapeStale(t, early)
	before := mustRead(t, early)

	// The late file drifts in a way the tool must refuse: a generator version it
	// was not built by. That drift needs a re-blessed batch through the oracle
	// pipeline, not a header rewrite.
	editFile(t, late, "# generator: ", "# generator: not-this-build/")

	census := filepath.Join(t.TempDir(), "census_baseline.json")
	if got := run(dir, census, false); got != exitDrift {
		t.Fatalf("run() = %d, want exitDrift (%d): a generator drift must abort", got, exitDrift)
	}
	if string(mustRead(t, early)) != string(before) {
		t.Errorf("the early file was rewritten before the run aborted: the tree is now half re-blessed\n  %s", early)
	}
	if _, err := os.Stat(census); !os.IsNotExist(err) {
		t.Errorf("census baseline at %s exists after an aborted run (stat err: %v)", census, err)
	}
}

// TestReblessWritesEveryQueuedFileOnceTheRunClears is the atomicity arm's
// other direction: two-pass must not become "verify and never write". Without
// it, a tool that satisfies the test above by writing nothing at all would pass.
func TestReblessWritesEveryQueuedFileOnceTheRunClears(t *testing.T) {
	t.Parallel()
	dir := fixtureCorpus(t, earlyFile, lateFile)
	early, late := filepath.Join(dir, earlyFile), filepath.Join(dir, lateFile)
	for _, p := range []string{early, late} {
		makePlanShapeStale(t, p)
	}
	census := filepath.Join(t.TempDir(), "census_baseline.json")
	if got := run(dir, census, false); got != exitOK {
		t.Fatalf("run() = %d, want exitOK (%d)", got, exitOK)
	}
	for _, p := range []string{early, late} {
		if strings.Contains(string(mustRead(t, p)), staleDigest) {
			t.Errorf("%s still carries the stale plan-shape digest: it was queued but never written", p)
		}
	}
	if _, err := os.Stat(census); err != nil {
		t.Errorf("census baseline was not written: %v", err)
	}
}

// TestReblessAbortsAndWritesNothingWhenTheWriterMangledARow proves the
// row-integrity check is WIRED — that run() consults it and treats its verdict
// as a drift, leaving the tree untouched.
//
// The mangling is injected rather than committed because with today's writer no
// valid corpus file can produce one: every cell the loader accepts re-emits to
// the identical value (see verifyRowsSurvived's comment). The check is a
// tripwire for a future writer regression, so the test trips it directly. What
// would otherwise go unnoticed is somebody deleting the call, which is exactly
// what this catches.
func TestReblessAbortsAndWritesNothingWhenTheWriterMangledARow(t *testing.T) {
	t.Parallel()
	dir := fixtureCorpus(t, earlyFile)
	path := filepath.Join(dir, earlyFile)
	makePlanShapeStale(t, path)
	before := mustRead(t, path)

	// A writer that rounded a frozen DOUBLE cell: `D: 4.0` re-emitted as `D: 4.5`.
	mangle := func(b []byte) []byte {
		out := strings.Replace(string(b), "D: 4.0,", "D: 4.5,", 1)
		if out == string(b) {
			t.Errorf("the injected mangling matched nothing; this test would pass vacuously")
		}
		return []byte(out)
	}
	census := filepath.Join(t.TempDir(), "census_baseline.json")
	if got := runWith(dir, census, false, mangle); got != exitDrift {
		t.Fatalf("runWith(mangled) = %d, want exitDrift (%d): a re-emission that restates a frozen cell must abort", got, exitDrift)
	}
	if after := mustRead(t, path); string(after) != string(before) {
		t.Errorf("%s was written despite the mangled re-emission", path)
	}
	if _, err := os.Stat(census); !os.IsNotExist(err) {
		t.Errorf("census baseline exists after an aborted run (stat err: %v)", err)
	}
}

// loadOne loads a single committed family file from a fixture directory.
func loadOne(t *testing.T, dir, name string) *factorycorpus.FamilyFile {
	t.Helper()
	f, err := factorycorpus.Load(filepath.Join(dir, name))
	if err != nil {
		t.Fatal(err)
	}
	return f
}

// TestReblessRefusesAReEmissionThatRestatesAFrozenCell is the ROW-INTEGRITY
// arm. The tool rewrites two header lines by RE-RENDERING the whole family
// file, so every frozen cell passes through the writer on a run that has no
// business touching it. Each case here is a way the writer could come back with
// a different expectation than it was given.
//
// The `want` side is mutated rather than the emitted bytes because that is the
// direction the check runs in: committed rows in, re-emitted rows out.
func TestReblessRefusesAReEmissionThatRestatesAFrozenCell(t *testing.T) {
	t.Parallel()
	dir := fixtureCorpus(t, earlyFile)

	// The faithful re-emission of the untouched file, which every case below
	// compares a perturbed input against.
	emitted, err := factorycorpus.MarshalFamily(loadOne(t, dir, earlyFile).Scenarios)
	if err != nil {
		t.Fatal(err)
	}
	if msg := verifyRowsSurvived(earlyFile, loadOne(t, dir, earlyFile).Scenarios, emitted); msg != "" {
		t.Fatalf("the untouched corpus file does not survive its own re-emission: %s", msg)
	}

	// negZeroCell locates the fixture's `!f -0.0` cell, so the signed-zero case
	// fails loudly if the fixture ever stops holding one rather than silently
	// testing an ordinary value.
	negZeroCell := func(t *testing.T, sc *factorycorpus.Scenario) (int, int, int) {
		t.Helper()
		for ti := range sc.Doc.Tests {
			for ri, row := range sc.Doc.Tests[ti].Rows {
				for ci, cell := range row {
					if f, ok := cell.(float32); ok && f == 0 && math.Signbit(float64(f)) {
						return ti, ri, ci
					}
				}
			}
		}
		t.Fatalf("fixture %s holds no `!f -0.0` cell; the signed-zero case would test nothing", earlyFile)
		return 0, 0, 0
	}

	for _, tc := range []struct {
		name    string
		perturb func(*testing.T, *factorycorpus.Scenario)
		wantMsg string
	}{{
		name: "a cell's value changes",
		perturb: func(t *testing.T, sc *factorycorpus.Scenario) {
			sc.Doc.Tests[0].Rows[0][0] = int64(999)
		},
		wantMsg: "int64(999) became",
	}, {
		// The reason this does not call reflect.DeepEqual: DeepEqual compares
		// floats with ==, under which -0.0 equals +0.0. The corpus commits
		// `!f -0.0` cells, and this repo has already shipped one wrong-rows bug
		// on signed zero, so a check that cannot see the sign of a zero is not a
		// check over this corpus.
		name: "a zero loses its sign",
		perturb: func(t *testing.T, sc *factorycorpus.Scenario) {
			ti, ri, ci := negZeroCell(t, sc)
			sc.Doc.Tests[ti].Rows[ri][ci] = float32(0)
		},
		wantMsg: "float32(0) became float32(-0)",
	}, {
		name: "a column label is re-pointed",
		perturb: func(t *testing.T, sc *factorycorpus.Scenario) {
			sc.Doc.Tests[0].Columns[0] = "NOT_" + sc.Doc.Tests[0].Columns[0]
		},
		wantMsg: "column label 0",
	}, {
		name: "a row disappears",
		perturb: func(t *testing.T, sc *factorycorpus.Scenario) {
			sc.Doc.Tests[0].Rows = sc.Doc.Tests[0].Rows[1:]
		},
		wantMsg: "rows became",
	}, {
		name: "an ordered result becomes an unordered one",
		perturb: func(t *testing.T, sc *factorycorpus.Scenario) {
			sc.Doc.Tests[0].Unordered = !sc.Doc.Tests[0].Unordered
		},
		wantMsg: "unordered",
	}} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			in := loadOne(t, dir, earlyFile).Scenarios
			tc.perturb(t, in[0])
			msg := verifyRowsSurvived(earlyFile, in, emitted)
			if msg == "" {
				t.Fatalf("re-emission restated a frozen expectation and the run was allowed to proceed")
			}
			if !strings.Contains(msg, tc.wantMsg) {
				t.Errorf("abort message does not name the difference.\n  got:  %s\n  want it to contain: %s", msg, tc.wantMsg)
			}
		})
	}
}

// TestReblessToleratesTheFloatTagCanonicalization pins the DECIDED behaviour of
// the one case where a committed cell legitimately changes spelling across a
// re-emission.
//
// `!f 5` loads as float32(5) and re-emits as `!f 5.0`, because canonicalFloat32
// always writes a form YAML re-reads as a float. Both spellings decode to the
// identical float32 bit pattern, so the frozen expectation is unchanged — Java's
// Matchers compare the value, never the text. The decision is therefore to
// TOLERATE it, and to tolerate it by construction: the check compares decoded
// values, which cannot see a spelling difference at all.
//
// This is pinned because the alternative decision (compare emitted text) is the
// one a reader would reach for, and it would abort a legitimate re-bless over a
// difference that asserts nothing.
func TestReblessToleratesTheFloatTagCanonicalization(t *testing.T) {
	t.Parallel()
	dir := fixtureCorpus(t, earlyFile)
	path := filepath.Join(dir, earlyFile)

	// The committed corpus is already canonical, so the un-canonical spelling is
	// introduced here. `E: !f 1.0` is the shape; `!f 1` is what an older writer
	// (or a hand edit) would have left.
	editAllFile(t, path, "E: !f 1.0}", "E: !f 1}")

	f, err := factorycorpus.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	emitted, err := factorycorpus.MarshalFamily(f.Scenarios)
	if err != nil {
		t.Fatal(err)
	}
	// The canonicalization must actually have happened, or this test proves
	// nothing about tolerating it.
	if !strings.Contains(string(emitted), "E: !f 1.0}") {
		t.Fatalf("re-emission did not canonicalize `!f 1` to `!f 1.0`; this test no longer exercises the case it documents")
	}
	if msg := verifyRowsSurvived(path, f.Scenarios, emitted); msg != "" {
		t.Errorf("the `!f 5` -> `!f 5.0` canonicalization aborted a re-bless, but it restates no expectation: %s", msg)
	}
}
