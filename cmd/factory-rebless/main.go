// factory-rebless re-derives the PLAN-SHAPE and DEDUP-KEY headers of the
// committed RFC-201 factory corpus after a deliberate planner change, and
// rewrites the census baseline to match.
//
// It exists because RFC-201 §5.3 requires a behaviour-changing fix to re-bless
// the affected expectations in the fixing PR, and the only re-emission tool the
// repo had (cmd/factory-migrate) reads the retired v1 `fc_*.yaml` layout. This
// one works on the committed `.yamsql` family files.
//
// # WHAT IT WILL AND WILL NOT TOUCH
//
// It rewrites two header fields and nothing else. Both are PURE COMPUTATION
// over the committed reproduction recipe — the same computation
// TestFactoryDeterminism performs to decide the headers have drifted — so
// re-deriving them is not an edit of an expectation, it is recomputing a
// derived value whose input changed.
//
// The frozen RESULT ROWS are never touched, and that is the line that matters:
// rows are oracle output, and rewriting them is how a proof silently becomes an
// opinion. A planner change that alters rows is therefore OUT OF SCOPE here —
// it needs the oracle pipeline, not this tool. The corpus's own row-level
// gate (//pkg/relational/conformance/factorycorpus/full:full_test, which
// executes every scenario against a real cluster) is what proves the rows still
// hold, and it must be green BEFORE this tool is run and after.
//
// "Never touched" is ENFORCED, not merely intended — see verifyRowsSurvived.
// Intent alone is worth nothing here, because the tool does not edit two lines:
// it RE-RENDERS the whole family file through the writer, rows included. The
// re-emission is therefore parsed back and its rows compared cell by cell
// against the rows that went in, so a writer defect cannot restate a frozen
// expectation inside a diff that reads as a header change.
//
// Everything else the determinism gate checks — the candidate name, the feature
// vector, all four TLP renderings, the schema template, the setup INSERT — is
// VERIFIED, never rewritten. Any drift there means the change was not a pure
// plan flip and the run aborts, because the committed reproduction recipes
// would be lying about something this tool cannot honestly repair.
//
//	go run ./cmd/factory-rebless \
//	  -corpus pkg/relational/conformance/factorycorpus/testdata \
//	  -census pkg/relational/conformance/factorycorpus/census_baseline.json
//
// -dry-run reports what would change and writes nothing.
//
// # ALL-OR-NOTHING
//
// The run is TWO-PASS: every file is verified and its replacement bytes
// computed in pass one, and nothing reaches the tree until pass one has cleared
// every file. A single-pass tool writes file N and then aborts on file N+1's
// drift, leaving a tree that is half re-blessed with a census describing
// neither half — a state whose diff looks like a deliberate partial re-bless
// and which no gate distinguishes from one. The abort must leave the corpus
// exactly as it found it, so the abort happens before the first write.
//
// (Two-pass rather than write-temp-then-rename: rename gives per-file
// atomicity, which is not the property at risk. The corpus plus its census
// baseline is ONE consistent unit, so the transaction boundary is the whole
// run, not a file.)
//
// Exit codes: 0 = done (or, under -dry-run, inspected); 1 = a drift this tool
// must not paper over; 2 = infra failure.
package main

import (
	"flag"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strconv"

	"fdb.dev/pkg/relational/conformance/explaindiff"
	"fdb.dev/pkg/relational/conformance/factory"
	"fdb.dev/pkg/relational/conformance/factorycorpus"
	"fdb.dev/pkg/relational/conformance/yamsql"
	"fdb.dev/pkg/relational/core/embedded"
)

const (
	exitOK    = 0
	exitDrift = 1
	exitInfra = 2
)

func main() {
	var corpusDir, censusPath string
	var dryRun bool
	flag.StringVar(&corpusDir, "corpus", "pkg/relational/conformance/factorycorpus/testdata",
		"directory holding the committed .yamsql family files")
	flag.StringVar(&censusPath, "census", "pkg/relational/conformance/factorycorpus/census_baseline.json",
		"path of the census baseline to rewrite")
	flag.BoolVar(&dryRun, "dry-run", false, "report what would change and write nothing")
	flag.Parse()
	os.Exit(run(corpusDir, censusPath, dryRun))
}

func run(corpusDir, censusPath string, dryRun bool) int {
	return runWith(corpusDir, censusPath, dryRun, nil)
}

// runWith is run with one seam: mangle, when non-nil, perturbs a family's
// marshalled bytes on their way into the row-integrity check.
//
// The seam exists because that check is a tripwire for a WRITER regression, and
// with today's writer nothing a valid corpus file can contain trips it — every
// cell the loader accepts re-emits to the identical value. A tripwire that
// cannot be tripped is indistinguishable from one that was never wired up, and
// the failure mode that matters is somebody deleting the call. So the test
// drives the wire directly. It is a parameter rather than a package variable so
// the tests that use it stay parallel-safe.
func runWith(corpusDir, censusPath string, dryRun bool, mangle func([]byte) []byte) int {
	paths, err := filepath.Glob(filepath.Join(corpusDir, "*.yamsql"))
	if err != nil || len(paths) == 0 {
		fmt.Fprintf(os.Stderr, "INFRA: no .yamsql under %s (%v)\n", corpusDir, err)
		return exitInfra
	}
	sort.Strings(paths)

	// Candidates() is expensive and one seed serves many scenarios, so it is
	// derived once per seed — the same sharing TestFactoryDeterminism relies on
	// to prove the per-seed derivation is stable across the candidates under it.
	candCache := map[uint64][]factory.Candidate{}
	candidatesFor := func(seed uint64) []factory.Candidate {
		if c, ok := candCache[seed]; ok {
			return c
		}
		c := factory.Candidates(seed)
		candCache[seed] = c
		return c
	}

	var all []*factorycorpus.Scenario
	var changedScenarios int
	// pending holds pass one's computed replacements. Nothing is written until
	// every file has cleared, so an abort leaves the tree untouched.
	type rewrite struct {
		path string
		data []byte
	}
	var pending []rewrite
	// seenKey guards the invariant the dedup key exists to enforce: one
	// committed scenario per (feature vector, plan shape) point. A plan flip can
	// COLLAPSE two formerly distinct points onto one, and a corpus holding two
	// files at one point is a silently redundant pin, so it is named rather than
	// written out.
	seenKey := map[string]string{}
	var collisions []string

	for _, path := range paths {
		f, err := factorycorpus.Load(path)
		if err != nil {
			fmt.Fprintf(os.Stderr, "INFRA: load %s: %v\n", path, err)
			return exitInfra
		}
		fileChanged := false
		for _, sc := range f.Scenarios {
			h := &sc.Header
			if h.Generator != factory.GeneratorVersion {
				fmt.Fprintf(os.Stderr, "DRIFT: %s/%s: generator %q, this build is %q. A generator "+
					"version bump needs a re-blessed batch through the oracle pipeline, not a "+
					"header rewrite\n", path, h.Name, h.Generator, factory.GeneratorVersion)
				return exitDrift
			}
			var cand *factory.Candidate
			for i, c := range candidatesFor(h.Seed) {
				if c.QueryIndex == h.QueryIndex && c.ProjIndex == h.Projection {
					cand = &candidatesFor(h.Seed)[i]
					break
				}
			}
			if cand == nil {
				fmt.Fprintf(os.Stderr, "DRIFT: %s/%s: seed %d no longer yields a candidate at "+
					"query %d / projection %d — the reproduction recipe is dead\n",
					path, h.Name, h.Seed, h.QueryIndex, h.Projection)
				return exitDrift
			}
			if msg := verifyRecipe(path, h, cand, sc); msg != "" {
				fmt.Fprintln(os.Stderr, "DRIFT: "+msg)
				return exitDrift
			}

			queries := cand.TLPQueries()
			plan, err := embedded.PlanPhysicalForTest(cand.Case.SQL(queries[1], cand.Projection), cand.Case.DDL(), nil)
			if err != nil {
				fmt.Fprintf(os.Stderr, "DRIFT: %s/%s: the committed query no longer plans: %v\n",
					path, h.Name, err)
				return exitDrift
			}
			shape := factorycorpus.ShapeDigest(explaindiff.ShapeOf(plan))
			key := factorycorpus.DedupKeyOf(cand.FeatureVector, shape)
			if shape != h.PlanShape || key != h.DedupKey {
				changedScenarios++
				fileChanged = true
				h.PlanShape = shape
				h.DedupKey = key
			}
			if prev, dup := seenKey[key]; dup {
				collisions = append(collisions, fmt.Sprintf(
					"dedup key %s is now held by BOTH %s and %s", key, prev, h.Name))
			}
			seenKey[key] = h.Name
			all = append(all, sc)
		}
		if !fileChanged {
			continue
		}
		data, err := factorycorpus.MarshalFamily(f.Scenarios)
		if err != nil {
			fmt.Fprintf(os.Stderr, "INFRA: marshal %s: %v\n", path, err)
			return exitInfra
		}
		if mangle != nil {
			data = mangle(data)
		}
		// The rows in `data` are the rows this tool was handed. Prove it,
		// before those bytes can become a certified corpus file.
		if msg := verifyRowsSurvived(path, f.Scenarios, data); msg != "" {
			fmt.Fprintln(os.Stderr, "DRIFT: "+msg)
			return exitDrift
		}
		pending = append(pending, rewrite{path: path, data: data})
	}

	fmt.Printf("re-blessed %d scenarios across %d files (%d scenarios inspected)\n",
		changedScenarios, len(pending), len(all))
	for _, c := range collisions {
		fmt.Fprintln(os.Stderr, "COLLISION: "+c)
	}
	if len(collisions) > 0 {
		fmt.Fprintf(os.Stderr, "%d dedup-key collisions: the plan flip collapsed distinct plan "+
			"points onto one, so the corpus now pins the same point twice. Resolve before committing.\n",
			len(collisions))
		return exitDrift
	}
	if dryRun {
		return exitOK
	}

	// Pass two. Every file has cleared; only now does anything reach the tree.
	for _, w := range pending {
		if err := os.WriteFile(w.path, w.data, 0o644); err != nil {
			fmt.Fprintf(os.Stderr, "INFRA: write %s: %v\n", w.path, err)
			return exitInfra
		}
	}

	data, err := factorycorpus.RenderCensus(factorycorpus.ComputeCensus(all))
	if err != nil {
		fmt.Fprintf(os.Stderr, "INFRA: render census: %v\n", err)
		return exitInfra
	}
	if err := os.WriteFile(censusPath, data, 0o644); err != nil {
		fmt.Fprintf(os.Stderr, "INFRA: write census: %v\n", err)
		return exitInfra
	}
	fmt.Printf("census baseline rewritten: %s\n", censusPath)
	return exitOK
}

// verifyRecipe checks everything the determinism gate checks EXCEPT the two
// derived headers this tool owns. A non-empty result is a drift that means the
// change was not a pure plan flip.
func verifyRecipe(path string, h *factorycorpus.Header, cand *factory.Candidate, sc *factorycorpus.Scenario) string {
	if cand.Name() != h.Name {
		return fmt.Sprintf("%s/%s: regenerated candidate is named %q", path, h.Name, cand.Name())
	}
	if cand.FeatureVector != h.FeatureVector {
		return fmt.Sprintf("%s/%s: feature vector drifted\n  header:      %s\n  regenerated: %s",
			path, h.Name, h.FeatureVector, cand.FeatureVector)
	}
	queries := cand.TLPQueries()
	if len(queries) != len(sc.Doc.Tests) {
		return fmt.Sprintf("%s/%s: %d committed tests, generator now yields %d renderings",
			path, h.Name, len(sc.Doc.Tests), len(queries))
	}
	for i, q := range queries {
		want := cand.Case.SQL(q, cand.Projection)
		if got := sc.Doc.Tests[i].Query; got != want {
			return fmt.Sprintf("%s/%s: tests[%d] (%s) SQL drifted\n  committed:   %s\n  regenerated: %s",
				path, h.Name, i, factory.TLPLabels[i], got, want)
		}
	}
	if got, want := sc.Doc.SchemaTemplate, cand.Case.DDL(); got != want {
		return fmt.Sprintf("%s/%s: schema_template drifted\n  committed:   %s\n  regenerated: %s",
			path, h.Name, got, want)
	}
	if len(sc.Doc.Setup) != 1 || sc.Doc.Setup[0] != cand.Case.InsertSQL() {
		return fmt.Sprintf("%s/%s: setup drifted; the frozen rows were computed over different data",
			path, h.Name)
	}
	return ""
}

// verifyRowsSurvived parses the re-emitted family back and proves every frozen
// result cell in it is the cell that went in. A non-empty result aborts the run.
//
// This is the arm that makes "the rows are never touched" a checked property
// rather than a promise. The hazard is specific to a tool that RE-EMITS: this
// tool means to rewrite two header lines, but the mechanism it rewrites them
// with re-renders the ENTIRE family file, rows included. So every frozen cell
// passes through the writer on a run whose stated purpose does not involve
// them, and a writer defect — a rounding change in canonicalFloat, a tag
// mapping that stops matching the loader's — would silently restate an
// expectation inside a diff that reads as "digests moved".
//
// # WHAT THIS DOES NOT CATCH, STATED PLAINLY
//
// It does NOT detect a row that was falsified in the committed file. It cannot:
// the comparison is round-trip fidelity, committed rows against re-emitted
// rows, and a hand-edited cell is on BOTH sides of it. Nothing in this tool can
// detect that, because a frozen row's only authority is the oracle that
// produced it — the check that catches falsification is
// //pkg/relational/conformance/factorycorpus/full:full_test, which re-executes
// every committed scenario against a real cluster, and it is required green
// around a re-bless for exactly this reason. What this arm owns is narrower and
// worth owning: the re-emission is faithful, so whatever the rows asserted
// before the run they assert after it.
//
// # WHAT IS COMPARED, AND WHY IT IS VALUES AND NOT TEXT
//
// The comparison is over DECODED cells, not over the emitted bytes. That is a
// deliberate choice with one known consequence.
//
// A cell may legitimately change SPELLING across a round trip: `!f 5` re-emits
// as `!f 5.0`, because the loader decodes an `!f`-tagged integer to float32(5)
// and canonicalFloat32 always writes a form YAML re-reads as a float
// (RFC-201's writer appends `.0` when the shortest form carries no `.` or `e`).
// Both spellings decode to the identical float32 bit pattern, so the frozen
// EXPECTATION is unchanged — Java's Matchers compare the value, never the text.
// Aborting on that would refuse a re-bless over a difference that asserts
// nothing, so it is tolerated, and tolerated by construction: comparing values
// cannot see it. (The committed corpus is already canonical — every `!f` cell
// in it carries a `.0` or an exponent — so this is a latent path kept honest
// rather than one in daily use.)
//
// What the value comparison must NOT lose is the sign of a zero, and
// reflect.DeepEqual would lose exactly that: it compares floats with ==, under
// which -0.0 equals +0.0. The corpus commits `!f -0.0` cells today, and this
// repo has already shipped one wrong-rows bug on signed zero. So floats are
// compared by BIT PATTERN, which is strictly stronger than DeepEqual on every
// input and is the reason this does not simply call it.
func verifyRowsSurvived(path string, in []*factorycorpus.Scenario, data []byte) string {
	back, err := factorycorpus.LoadBytes(path, data)
	if err != nil {
		return fmt.Sprintf("%s: the re-emitted family does not load: %v", path, err)
	}
	// MarshalFamily sorts by (seed, query index, projection); index the reloaded
	// side by scenario name so the comparison does not depend on that order.
	byName := map[string]*factorycorpus.Scenario{}
	for _, sc := range back.Scenarios {
		byName[sc.Header.Name] = sc
	}
	if len(back.Scenarios) != len(in) {
		return fmt.Sprintf("%s: re-emitted %d scenarios for %d committed",
			path, len(back.Scenarios), len(in))
	}
	for _, want := range in {
		got, ok := byName[want.Header.Name]
		if !ok {
			return fmt.Sprintf("%s/%s: scenario is missing from the re-emitted family", path, want.Header.Name)
		}
		if len(got.Doc.Tests) != len(want.Doc.Tests) {
			return fmt.Sprintf("%s/%s: re-emitted %d tests for %d committed",
				path, want.Header.Name, len(got.Doc.Tests), len(want.Doc.Tests))
		}
		for i := range want.Doc.Tests {
			w, g := &want.Doc.Tests[i], &got.Doc.Tests[i]
			if msg := diffFrozenResult(w, g); msg != "" {
				return fmt.Sprintf("%s/%s: tests[%d] (%s) frozen result changed across re-emission — %s.\n"+
					"  Result rows are oracle output; this tool rewrites two header lines and nothing else. "+
					"The writer mangled a frozen cell on the way back out, so re-emitting the family would "+
					"restate an expectation nobody asked to restate. Fix the writer; do not re-bless",
					path, want.Header.Name, i, factory.TLPLabels[i], msg)
			}
		}
	}
	return ""
}

// diffFrozenResult reports the first difference between two frozen result sets,
// or "" when they are identical. Column labels and the ordered/unordered
// discipline are part of the expectation: a renamed label re-points every cell
// and an unorderedResult asserts strictly less than a result.
func diffFrozenResult(want, got *yamsql.Test) string {
	if want.Unordered != got.Unordered {
		return fmt.Sprintf("unordered %v became %v", want.Unordered, got.Unordered)
	}
	if len(want.Columns) != len(got.Columns) {
		return fmt.Sprintf("%d column labels became %d", len(want.Columns), len(got.Columns))
	}
	for i := range want.Columns {
		if want.Columns[i] != got.Columns[i] {
			return fmt.Sprintf("column label %d %q became %q", i, want.Columns[i], got.Columns[i])
		}
	}
	if len(want.Rows) != len(got.Rows) {
		return fmt.Sprintf("%d rows became %d", len(want.Rows), len(got.Rows))
	}
	for i := range want.Rows {
		if len(want.Rows[i]) != len(got.Rows[i]) {
			return fmt.Sprintf("row %d had %d cells, now %d", i, len(want.Rows[i]), len(got.Rows[i]))
		}
		for j := range want.Rows[i] {
			if !sameCell(want.Rows[i][j], got.Rows[i][j]) {
				return fmt.Sprintf("row %d cell %d (%s): %s became %s",
					i, j, columnLabel(want.Columns, j),
					renderCell(want.Rows[i][j]), renderCell(got.Rows[i][j]))
			}
		}
	}
	return ""
}

// sameCell compares two decoded expectation cells exactly: same dynamic type
// and, for floats, the same BIT PATTERN — so -0.0 and +0.0 are different cells,
// which == and reflect.DeepEqual both say they are not.
func sameCell(a, b any) bool {
	switch x := a.(type) {
	case nil:
		return b == nil
	case float64:
		y, ok := b.(float64)
		return ok && math.Float64bits(x) == math.Float64bits(y)
	case float32:
		y, ok := b.(float32)
		return ok && math.Float32bits(x) == math.Float32bits(y)
	default:
		return a == b
	}
}

func renderCell(v any) string {
	switch x := v.(type) {
	case nil:
		return "NULL"
	case float64:
		return fmt.Sprintf("%T(%s)", x, strconv.FormatFloat(x, 'g', -1, 64))
	case float32:
		return fmt.Sprintf("%T(%s)", x, strconv.FormatFloat(float64(x), 'g', -1, 32))
	default:
		return fmt.Sprintf("%T(%v)", v, v)
	}
}

func columnLabel(cols []string, j int) string {
	if j < len(cols) {
		return cols[j]
	}
	return "?"
}
