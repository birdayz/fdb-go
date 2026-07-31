package factorycorpus

import "sort"

// SampleSize is how many scenarios the per-PR tier runs.
//
// RFC-201 §7 puts a stratified sample in the per-PR tier and the full corpus
// in the nightly. The number is a budget, not a coverage claim: each scenario
// pays a schema create/drop against a real container, so a hundred is roughly
// the most that fits beside the rest of the fast tier. Coverage comes from
// STRATIFICATION — every feature vector appears before any repeats — so the
// sample's value grows with the corpus's diversity rather than with its size.
const SampleSize = 100

// StratifiedSample picks up to n files with every feature vector represented,
// deterministically.
//
// RFC-201 §7 puts a sample of the committed corpus in the per-PR tier, and the
// only sample worth running there is one stratified by feature vector: a
// uniform random sample of a corpus dominated by one query family tests that
// family n times and everything else zero times, which is the volumetric
// fallacy the whole design is arranged against.
//
// The walk is round-robin over feature-vector groups in sorted order, taking
// one file per group per pass. That gives full dimension coverage first and
// only then depth, and it is a pure function of the file set — no clock, no
// randomness, no dependence on directory order (LoadDir already sorts). A
// stable sample matters as much as a representative one: a PR-tier gate that
// runs different tests each time turns a real regression into an intermittent.
func StratifiedSample(files []*File, n int) []*File {
	if n <= 0 || len(files) == 0 {
		return nil
	}
	groups := map[string][]*File{}
	for _, f := range files {
		groups[f.Header.FeatureVector] = append(groups[f.Header.FeatureVector], f)
	}
	keys := make([]string, 0, len(groups))
	for k := range groups {
		keys = append(keys, k)
		sort.Slice(groups[k], func(i, j int) bool { return groups[k][i].Path < groups[k][j].Path })
	}
	sort.Strings(keys)

	out := make([]*File, 0, n)
	for depth := 0; len(out) < n; depth++ {
		progressed := false
		for _, k := range keys {
			g := groups[k]
			if depth >= len(g) {
				continue
			}
			progressed = true
			out = append(out, g[depth])
			if len(out) == n {
				return out
			}
		}
		if !progressed {
			break
		}
	}
	return out
}
