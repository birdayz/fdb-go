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
// Exit codes: 0 = done (or, under -dry-run, inspected); 1 = a drift this tool
// must not paper over; 2 = infra failure.
package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"fdb.dev/pkg/relational/conformance/explaindiff"
	"fdb.dev/pkg/relational/conformance/factory"
	"fdb.dev/pkg/relational/conformance/factorycorpus"
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
	var changedFiles, changedScenarios int
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
		changedFiles++
		if dryRun {
			continue
		}
		data, err := factorycorpus.MarshalFamily(f.Scenarios)
		if err != nil {
			fmt.Fprintf(os.Stderr, "INFRA: marshal %s: %v\n", path, err)
			return exitInfra
		}
		if err := os.WriteFile(path, data, 0o644); err != nil {
			fmt.Fprintf(os.Stderr, "INFRA: write %s: %v\n", path, err)
			return exitInfra
		}
	}

	fmt.Printf("re-blessed %d scenarios across %d files (%d scenarios inspected)\n",
		changedScenarios, changedFiles, len(all))
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
