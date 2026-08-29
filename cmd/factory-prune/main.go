// Command factory-prune removes redundant scenarios from the committed
// RFC-201 §5 factory corpus.
//
// The corpus grows by ~1000 scenarios a night and its dedup key is
// (feature vector, plan shape) — a key fine enough that essentially every
// generated candidate is "novel" by it, so the corpus has no saturation
// signal and grows without bound. Measured on the 8150-scenario corpus at
// e342883c1: 223 distinct plan shapes, 36.5 scenarios per shape, the most
// covered shape holding 483. The cost is paid by every `just test` and every
// PR CI run, because the corpus target is ordinary suite content.
//
// WHAT COUNTS AS REDUNDANT. Not "looks similar" — a scenario is dropped only
// when every coverage TOKEN it carries is already carried by a scenario that
// is kept. The token set is the operational reading of "does this exercise a
// path we would not otherwise test":
//
//   - the plan shape (the physical operator tree the query compiles to);
//   - each (plan shape × predicate leaf kind) pair, so a shape reached by an
//     IN-list and the same shape reached by a LIKE are distinct work;
//   - the query shape, index configuration, projection class and ordering
//     class from the feature vector;
//   - the blessing authority, so a weaker-authority scenario can never be
//     dropped in favour of a stronger one and silently shrink what the census
//     says the corpus proves.
//
// Selection is greedy set cover over those tokens, then a per-shape FLOOR is
// topped up so that every plan shape retains at least -floor scenarios. The
// floor exists because tokens capture the query's STRUCTURE and not its DATA:
// two scenarios on one shape still differ in their rows, and row-dependent
// defects (NULL placement, signed zero, duplicate keys, empty partitions) are
// exactly the class a structural token set cannot see.
//
// The tool never edits a scenario. It keeps or drops whole scenarios and
// re-marshals each family file through factorycorpus.MarshalFamily, the same
// writer the factory uses, so a pruned file is byte-identical in form to a
// generated one.
//
// MEASURED, AND THE MEASUREMENT DOES NOT SUPPORT THE ELABORATE PART.
//
// cmd/corpus-mutation-proof ran five engine mutations against the full corpus
// and intersected each failure set with this tool's keep list AND with a
// same-size (1698) seeded RANDOM sample. Detecting scenarios, kept vs random:
//
//	like-inverted          212 of 8150 ( 2.6%)    55 kept, 49 random
//	sort-nulls-placement   675 of 8150 ( 8.3%)    96 kept, 148 random
//	sort-direction        4178 of 8150 (51.3%)   861 kept, 894 random
//	is-null-inverted      8150 of 8150 (100.0%) 1698 kept, 1698 random
//	is-not-null-inverted  1161 of 8150 (14.2%)  249 kept, 243 random
//
// Nothing was lost by pruning — but the random control detected every one too,
// and on sort-nulls-placement it did BETTER (148 vs 96). ZERO rows
// discriminated. So what these results establish is that keeping ~20% of the
// corpus costs no detection power for these defect classes; they do NOT
// establish that the token-based selection is doing any work. A random 20%
// sample performed the same.
//
// That is a real negative result about THIS tool and it is recorded here
// rather than in a commit message, because the next person to read the set
// cover above will otherwise assume it was justified by measurement.
//
// Why no row discriminated, and what would: a random p-fraction sample misses a
// mutation reached by N scenarios with probability (1-p)^N, which at p=0.2 is
// already ~0 by N=30. The narrowest mutation here is reached by 212. The
// narrowest predicate family in the whole corpus is `case` at 213 scenarios, so
// no single-family mutation can get into the regime where selection beats
// chance. Discriminating would need a defect reachable by a HANDFUL of
// scenarios — the corpus has exactly 2 singleton plan shapes, and a mutation
// only those reach is what would test the cover's actual guarantee.
//
// The guarantee itself is still worth having and is cheap: set cover gives a
// WORST-CASE bound (every token survives by construction) that a random sample
// gives only in expectation. Keep it for that reason — not because it has been
// shown to detect more.
package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"fdb.dev/pkg/relational/conformance/factorycorpus"
)

func main() {
	var (
		dir    = flag.String("dir", "pkg/relational/conformance/factorycorpus/testdata", "corpus directory")
		floor  = flag.Int("floor", 8, "minimum scenarios retained per plan shape (data diversity the token set cannot see)")
		apply  = flag.Bool("apply", false, "rewrite the corpus; without it the tool only reports")
		report = flag.String("report", "", "write the drop list to this path")
		keepAt = flag.String("keep-list", "", "write the retained scenario names to this path")
	)
	flag.Parse()

	scenarios, err := factorycorpus.LoadDir(*dir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "load %s: %v\n", *dir, err)
		os.Exit(1)
	}
	if len(scenarios) == 0 {
		fmt.Fprintf(os.Stderr, "load %s: corpus is EMPTY — refusing to run, a prune over nothing reports a clean success\n", *dir)
		os.Exit(1)
	}

	keep, stats := selectKeep(scenarios, *floor)
	fmt.Printf("scenarios      %d\n", len(scenarios))
	fmt.Printf("plan shapes    %d\n", stats.shapes)
	fmt.Printf("tokens         %d\n", stats.tokens)
	fmt.Printf("kept by cover  %d\n", stats.byCover)
	fmt.Printf("kept by floor  %d\n", stats.byFloor)
	fmt.Printf("KEPT           %d (%.1f%%)\n", len(keep), 100*float64(len(keep))/float64(len(scenarios)))
	fmt.Printf("DROPPED        %d (%.1f%%)\n", len(scenarios)-len(keep), 100*float64(len(scenarios)-len(keep))/float64(len(scenarios)))

	// Every token must survive, or the prune removed coverage rather than
	// redundancy. This is the tool's own correctness gate and it runs on every
	// invocation, not only under -apply.
	if missing := uncoveredTokens(scenarios, keep); len(missing) > 0 {
		fmt.Fprintf(os.Stderr, "REFUSING: %d coverage tokens would be lost, e.g. %v\n", len(missing), first(missing, 5))
		os.Exit(1)
	}
	fmt.Println("token check    OK (every token retained)")

	if *report != "" {
		if err := writeReport(*report, scenarios, keep); err != nil {
			fmt.Fprintf(os.Stderr, "write report: %v\n", err)
			os.Exit(1)
		}
	}
	if *keepAt != "" {
		if err := writeKeepList(*keepAt, keep); err != nil {
			fmt.Fprintf(os.Stderr, "write keep list: %v\n", err)
			os.Exit(1)
		}
	}
	if !*apply {
		fmt.Println("(dry run — pass -apply to rewrite the corpus)")
		return
	}
	if err := rewrite(*dir, keep); err != nil {
		fmt.Fprintf(os.Stderr, "rewrite: %v\n", err)
		os.Exit(1)
	}
	fmt.Println("corpus rewritten")
}

type selectStats struct {
	shapes, tokens, byCover, byFloor int
}

// featureField pulls one `k=v` field out of a feature vector, which is
// rendered as `shape=single;idx=A,S;proj=star;where=cmp.gt;order=asc.nf+asc`.
func featureField(fv, key string) string {
	for _, part := range strings.Split(fv, ";") {
		if strings.HasPrefix(part, key+"=") {
			return part[len(key)+1:]
		}
	}
	return ""
}

// leafKinds extracts the predicate leaf kinds (`cmp.gt`, `in.in`, `bit.le`, …)
// from a feature vector's where clause, deduplicated.
func leafKinds(fv string) []string {
	where := featureField(fv, "where")
	seen := map[string]bool{}
	var cur strings.Builder
	flush := func() {
		s := cur.String()
		cur.Reset()
		if strings.Contains(s, ".") {
			seen[s] = true
		}
	}
	for _, r := range where {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '.' {
			cur.WriteRune(r)
			continue
		}
		flush()
	}
	flush()
	out := make([]string, 0, len(seen))
	for k := range seen {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// tokensOf is the coverage a scenario claims. Two scenarios with the same
// token set are interchangeable as far as anything structural can tell.
func tokensOf(s *factorycorpus.Scenario) []string {
	fv := s.Header.FeatureVector
	shape := s.Header.PlanShape
	out := []string{
		"plan:" + shape,
		"qshape:" + featureField(fv, "shape"),
		"idx:" + featureField(fv, "idx"),
		"proj:" + featureField(fv, "proj"),
		"order:" + featureField(fv, "order"),
		"bless:" + string(s.Header.Blessing),
		// The blessing authority is per-shape, not global: dropping the only
		// cross-engine scenario on a shape weakens what that shape proves even
		// when some other shape still has one.
		"bless@plan:" + string(s.Header.Blessing) + "@" + shape,
	}
	for _, k := range leafKinds(fv) {
		out = append(out, "leaf:"+k, "leaf@plan:"+k+"@"+shape)
	}
	return out
}

func selectKeep(scenarios []*factorycorpus.Scenario, floor int) ([]*factorycorpus.Scenario, selectStats) {
	// Deterministic order: the corpus must prune identically on every machine
	// and every re-run, or two runs produce two different corpora and the
	// census ratchet cannot tell a prune from a regression.
	ordered := append([]*factorycorpus.Scenario(nil), scenarios...)
	sort.Slice(ordered, func(i, j int) bool {
		if ordered[i].Header.Seed != ordered[j].Header.Seed {
			return ordered[i].Header.Seed < ordered[j].Header.Seed
		}
		return ordered[i].Header.Name < ordered[j].Header.Name
	})

	allTokens := map[string]bool{}
	toks := make([][]string, len(ordered))
	shapes := map[string][]int{}
	for i, s := range ordered {
		toks[i] = tokensOf(s)
		for _, t := range toks[i] {
			allTokens[t] = true
		}
		shapes[s.Header.PlanShape] = append(shapes[s.Header.PlanShape], i)
	}

	covered := map[string]bool{}
	kept := make([]bool, len(ordered))
	var stats selectStats
	stats.shapes = len(shapes)
	stats.tokens = len(allTokens)

	// Greedy set cover: repeatedly take the scenario adding the most uncovered
	// tokens. Ties break on the deterministic order above.
	for {
		best, bestGain := -1, 0
		for i := range ordered {
			if kept[i] {
				continue
			}
			gain := 0
			for _, t := range toks[i] {
				if !covered[t] {
					gain++
				}
			}
			if gain > bestGain {
				best, bestGain = i, gain
			}
		}
		if best < 0 {
			break
		}
		kept[best] = true
		stats.byCover++
		for _, t := range toks[best] {
			covered[t] = true
		}
	}

	// Per-shape floor, in the deterministic order, for the row-level diversity
	// the token set is blind to.
	shapeNames := make([]string, 0, len(shapes))
	for k := range shapes {
		shapeNames = append(shapeNames, k)
	}
	sort.Strings(shapeNames)
	for _, sh := range shapeNames {
		idxs := shapes[sh]
		have := 0
		for _, i := range idxs {
			if kept[i] {
				have++
			}
		}
		for _, i := range idxs {
			if have >= floor {
				break
			}
			if !kept[i] {
				kept[i] = true
				have++
				stats.byFloor++
			}
		}
	}

	var out []*factorycorpus.Scenario
	for i, s := range ordered {
		if kept[i] {
			out = append(out, s)
		}
	}
	return out, stats
}

func uncoveredTokens(all, keep []*factorycorpus.Scenario) []string {
	want := map[string]bool{}
	for _, s := range all {
		for _, t := range tokensOf(s) {
			want[t] = true
		}
	}
	for _, s := range keep {
		for _, t := range tokensOf(s) {
			delete(want, t)
		}
	}
	out := make([]string, 0, len(want))
	for t := range want {
		out = append(out, t)
	}
	sort.Strings(out)
	return out
}

func first(xs []string, n int) []string {
	if len(xs) < n {
		return xs
	}
	return xs[:n]
}

func writeReport(path string, all, keep []*factorycorpus.Scenario) error {
	kept := map[string]bool{}
	for _, s := range keep {
		kept[s.Header.Name] = true
	}
	var b strings.Builder
	fmt.Fprintf(&b, "# factory-prune drop list\n# %d of %d scenarios dropped\n", len(all)-len(keep), len(all))
	for _, s := range all {
		if !kept[s.Header.Name] {
			fmt.Fprintf(&b, "%s\t%s\t%s\n", s.Header.Name, s.Header.PlanShape, s.Header.FeatureVector)
		}
	}
	return os.WriteFile(path, []byte(b.String()), 0o644)
}

// writeKeepList emits just the retained scenario NAMES, one per line. The
// mutation experiment needs this: "does the pruned corpus still catch bug X"
// is answerable from a single run of the FULL corpus, by asking whether the
// scenarios that detected X intersect this list. Running both corpora
// separately would cost twice as much and prove exactly the same thing.
func writeKeepList(path string, keep []*factorycorpus.Scenario) error {
	var b strings.Builder
	for _, s := range keep {
		b.WriteString(s.Header.Name)
		b.WriteByte('\n')
	}
	return os.WriteFile(path, []byte(b.String()), 0o644)
}

func rewrite(dir string, keep []*factorycorpus.Scenario) error {
	byFile := map[string][]*factorycorpus.Scenario{}
	for _, s := range keep {
		byFile[s.Path] = append(byFile[s.Path], s)
	}
	existing, err := filepath.Glob(filepath.Join(dir, "*.yamsql"))
	if err != nil {
		return err
	}
	for _, p := range existing {
		entries, ok := byFile[p]
		if !ok {
			// Every scenario in this family was dropped. Only reachable if a
			// whole plan shape vanished, which the token check above forbids,
			// so this is a defensive removal rather than an expected path.
			if err := os.Remove(p); err != nil {
				return err
			}
			continue
		}
		data, err := factorycorpus.MarshalFamily(entries)
		if err != nil {
			return fmt.Errorf("marshal %s: %w", p, err)
		}
		if err := os.WriteFile(p, data, 0o644); err != nil {
			return err
		}
	}
	return nil
}
