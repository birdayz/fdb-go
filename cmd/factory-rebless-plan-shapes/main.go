// Command factory-rebless-plan-shapes re-derives the PLAN SHAPE and dedup key
// of every committed RFC-201 factory scenario and rewrites the ones that moved.
//
// This is the RFC-201 §5.3 re-bless restricted to the header fields a planner
// change can legitimately move. Nothing else is touched: the candidate, the
// four renderings, the schema, the setup and the frozen rows are recomputed and
// compared, and any difference in them is an error rather than something to
// write down — those are oracle OUTPUT, and a planner change that alters them
// is a bug report, not a re-bless.
//
//	go run ./cmd/factory-rebless-plan-shapes \
//	  -corpus pkg/relational/conformance/factorycorpus/testdata \
//	  -census pkg/relational/conformance/factorycorpus/census_baseline.json
//
// Exit codes: 0 = nothing moved or everything moved cleanly; 1 = a scenario
// changed in a way a re-bless may not absorb; 2 = infra failure.
package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
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

func main() {
	corpusDir := flag.String("corpus", "", "committed corpus directory")
	censusPath := flag.String("census", "", "census baseline to rewrite (optional)")
	dryRun := flag.Bool("dry-run", false, "report what would move without writing")
	beforeDir := flag.String("before", "", "corpus directory as the base commit committed it (required with -ledger)")
	ledgerPath := flag.String("ledger", "", "retirement ledger to emit for the transition")
	baseCommit := flag.String("base-commit", "", "commit the -before corpus was taken from")
	rfc := flag.String("rfc", "", "RFC the re-bless is authorized by")
	date := flag.String("date", "", "ledger date, YYYY-MM-DD")
	reason := flag.String("reason", "", "one reviewed reason for the transition")
	flag.Parse()
	if *corpusDir == "" {
		fmt.Fprintln(os.Stderr, "-corpus is required")
		os.Exit(2)
	}
	if err := run(*corpusDir, *censusPath, *dryRun); err != nil {
		fmt.Fprintln(os.Stderr, "factory-rebless-plan-shapes:", err)
		os.Exit(1)
	}
	if *ledgerPath == "" {
		return
	}
	if err := writeLedger(*beforeDir, *corpusDir, *ledgerPath, *baseCommit, *rfc, *date, *reason); err != nil {
		fmt.Fprintln(os.Stderr, "factory-rebless-plan-shapes:", err)
		os.Exit(1)
	}
}

// genSeed is a scenario's whole reproduction recipe: the generator NAME its
// header declares plus the seed. Both halves are load-bearing — see
// groupByRecipe.
type genSeed struct {
	gen  string
	seed uint64
}

// groupByRecipe buckets committed scenarios by (generator, seed) and returns
// the buckets in a stable order.
//
// The GENERATOR half is what makes this correct. The corpus is produced by more
// than one generator, each recording its name in the header, and the same seed
// yields a DIFFERENT case under each. Keying on the seed alone merges two
// unrelated recipes into one bucket, and every scenario in it is then compared
// against whichever generator's candidates the lookup happens to return.
func groupByRecipe(scenarios []*factorycorpus.Scenario) ([]genSeed, map[genSeed][]*factorycorpus.Scenario) {
	byRecipe := map[genSeed][]*factorycorpus.Scenario{}
	var recipes []genSeed
	for _, s := range scenarios {
		gs := genSeed{s.Header.Generator, s.Header.Seed}
		if _, seen := byRecipe[gs]; !seen {
			recipes = append(recipes, gs)
		}
		byRecipe[gs] = append(byRecipe[gs], s)
	}
	sort.Slice(recipes, func(i, j int) bool {
		if recipes[i].gen != recipes[j].gen {
			return recipes[i].gen < recipes[j].gen
		}
		return recipes[i].seed < recipes[j].seed
	})
	return recipes, byRecipe
}

// resolveCandidates re-derives a recipe's candidates through the generator the
// header NAMES — the same lookup determinism_test.go uses.
//
// `factory.Candidates(seed)` is the FLAT generator unconditionally, so using it
// here handed every nested-generator file an unrelated candidate. That did not
// fail quietly: it surfaced as a "feature vector moved" refusal naming a family
// change that never happened, which stopped the re-bless at its first file and
// would have rewritten headers to describe the wrong scenario had the
// comparison not been there.
func resolveCandidates(gs genSeed) ([]factory.Candidate, error) {
	candidates, known := factory.CandidatesForGenerator(gs.gen, gs.seed)
	if !known {
		return nil, fmt.Errorf("committed scenarios name generator %q, which this build does not have; their reproduction recipes cannot be checked at all", gs.gen)
	}
	return candidates, nil
}

func run(corpusDir, censusPath string, dryRun bool) error {
	scenarios, err := factorycorpus.LoadDir(corpusDir)
	if err != nil {
		return err
	}
	recipes, byRecipe := groupByRecipe(scenarios)

	moved := 0
	touchedFiles := map[string]bool{}
	for _, gs := range recipes {
		candidates, candErr := resolveCandidates(gs)
		if candErr != nil {
			return candErr
		}
		for _, s := range byRecipe[gs] {
			header := s.Header
			var candidate *factory.Candidate
			for i := range candidates {
				if candidates[i].QueryIndex == header.QueryIndex && candidates[i].ProjIndex == header.Projection {
					candidate = &candidates[i]
					break
				}
			}
			if candidate == nil {
				return fmt.Errorf("%s: seed %d no longer yields query %d / projection %d; a dead reproduction recipe is a retirement, not a re-bless",
					header.Name, header.Seed, header.QueryIndex, header.Projection)
			}
			if candidate.FeatureVector != header.FeatureVector {
				return fmt.Errorf("%s: feature vector moved (%s -> %s); that changes the scenario's family and is not a plan-shape re-bless",
					header.Name, header.FeatureVector, candidate.FeatureVector)
			}
			queries := candidate.TLPQueries()
			if len(queries) != len(s.Doc.Tests) {
				return fmt.Errorf("%s: %d committed tests, generator now yields %d renderings", header.Name, len(s.Doc.Tests), len(queries))
			}
			for i, q := range queries {
				if got, want := s.Doc.Tests[i].Query, candidate.Case.SQL(q, candidate.Projection); got != want {
					return fmt.Errorf("%s: tests[%d] SQL moved; the committed rows were computed for a different query", header.Name, i)
				}
			}
			if s.Doc.SchemaTemplate != candidate.Case.DDL() ||
				len(s.Doc.Setup) != 1 || s.Doc.Setup[0] != candidate.Case.InsertSQL() {
				return fmt.Errorf("%s: schema or setup moved; the frozen rows were computed over different data", header.Name)
			}
			plan, planErr := embedded.PlanPhysicalForTest(candidate.Case.SQL(queries[1], candidate.Projection), candidate.Case.DDL(), nil)
			if planErr != nil {
				return fmt.Errorf("%s: the committed query no longer plans: %w", header.Name, planErr)
			}
			shape := factorycorpus.ShapeDigest(explaindiff.ShapeOf(plan))
			key := factorycorpus.DedupKeyOf(candidate.FeatureVector, shape)
			if shape == header.PlanShape && key == header.DedupKey {
				continue
			}
			fmt.Printf("re-bless %s: shape %s -> %s, dedup key %s -> %s\n", header.Name, header.PlanShape, shape, header.DedupKey, key)
			s.Header.PlanShape = shape
			s.Header.DedupKey = key
			moved++
			touchedFiles[s.Path] = true
		}
	}
	fmt.Printf("%d of %d committed scenarios moved, across %d family files\n", moved, len(scenarios), len(touchedFiles))
	if moved == 0 || dryRun {
		return nil
	}

	byFile := map[string][]*factorycorpus.Scenario{}
	for _, s := range scenarios {
		byFile[s.Path] = append(byFile[s.Path], s)
	}
	for path := range touchedFiles {
		data, marshalErr := factorycorpus.MarshalFamily(byFile[path])
		if marshalErr != nil {
			return fmt.Errorf("re-marshal %s: %w", filepath.Base(path), marshalErr)
		}
		if writeErr := os.WriteFile(path, data, 0o644); writeErr != nil {
			return writeErr
		}
	}
	if censusPath == "" {
		return nil
	}
	reloaded, err := factorycorpus.LoadDir(corpusDir)
	if err != nil {
		return fmt.Errorf("reload re-blessed corpus: %w", err)
	}
	rendered, err := factorycorpus.RenderCensus(factorycorpus.ComputeCensus(reloaded))
	if err != nil {
		return err
	}
	return os.WriteFile(censusPath, rendered, 0o644)
}

// writeLedger renders the RFC-201 §8 retirement ledger for a completed
// re-bless. beforeDir must hold the corpus exactly as baseCommit committed it;
// the ledger's endpoints are fingerprints of real trees on both sides, so the
// authorization cannot be written before the transition it authorizes.
func writeLedger(beforeDir, corpusDir, ledgerPath, baseCommit, rfc, date, reason string) error {
	beforeFiles, err := factorycorpus.LoadDir(beforeDir)
	if err != nil {
		return fmt.Errorf("load BEFORE corpus: %w", err)
	}
	afterFiles, err := factorycorpus.LoadDir(corpusDir)
	if err != nil {
		return fmt.Errorf("load AFTER corpus: %w", err)
	}
	beforeCensus, err := factorycorpus.CensusSHA256(factorycorpus.ComputeCensus(beforeFiles))
	if err != nil {
		return err
	}
	afterCensus, err := factorycorpus.CensusSHA256(factorycorpus.ComputeCensus(afterFiles))
	if err != nil {
		return err
	}
	beforeTree, err := factorycorpus.CorpusTreeSHA256(beforeDir)
	if err != nil {
		return err
	}
	afterTree, err := factorycorpus.CorpusTreeSHA256(corpusDir)
	if err != nil {
		return err
	}
	changes, err := corpusFileChanges(beforeDir, corpusDir)
	if err != nil {
		return err
	}
	ledger := factorycorpus.RetirementLedger{
		FormatVersion:      2,
		RFC:                rfc,
		Date:               date,
		Reason:             reason,
		BaseCommit:         baseCommit,
		BeforeCensusSHA256: beforeCensus,
		AfterCensusSHA256:  afterCensus,
		BeforeTreeSHA256:   beforeTree,
		AfterTreeSHA256:    afterTree,
		Changes:            changes,
	}
	data, err := json.MarshalIndent(ledger, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(ledgerPath, append(data, '\n'), 0o644)
}

func corpusFileChanges(beforeDir, afterDir string) ([]factorycorpus.RetirementChange, error) {
	before, err := corpusFileDigests(beforeDir)
	if err != nil {
		return nil, err
	}
	after, err := corpusFileDigests(afterDir)
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(before)+len(after))
	for name := range before {
		names = append(names, name)
	}
	for name := range after {
		if _, seen := before[name]; !seen {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	var changes []factorycorpus.RetirementChange
	for _, name := range names {
		oldDigest, existed := before[name]
		newDigest, exists := after[name]
		switch {
		case existed && !exists:
			changes = append(changes, factorycorpus.RetirementChange{
				Name: name, Disposition: factorycorpus.DispositionRetired, OldSHA256: oldDigest,
			})
		case !existed && exists:
			changes = append(changes, factorycorpus.RetirementChange{
				Name: name, Disposition: factorycorpus.DispositionAdded, NewSHA256: newDigest,
			})
		case oldDigest != newDigest:
			changes = append(changes, factorycorpus.RetirementChange{
				Name: name, Disposition: factorycorpus.DispositionReplaced,
				OldSHA256: oldDigest, NewSHA256: newDigest,
			})
		}
	}
	return changes, nil
}

func corpusFileDigests(dir string) (map[string]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	digests := make(map[string]string, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != factorycorpus.FileExt {
			continue
		}
		data, readErr := os.ReadFile(filepath.Join(dir, entry.Name()))
		if readErr != nil {
			return nil, readErr
		}
		sum := sha256.Sum256(data)
		digests[entry.Name()] = hex.EncodeToString(sum[:])
	}
	return digests, nil
}
