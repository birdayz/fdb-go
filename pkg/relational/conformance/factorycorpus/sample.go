package factorycorpus

import (
	"sort"
	"strings"
)

// SampleSize is how many scenarios the per-PR tier runs.
//
// RFC-201 §7 puts a stratified sample in the per-PR tier and the full corpus
// in the nightly. The number is a budget, not a coverage claim: each scenario
// pays a schema create/drop against a real container, so a hundred is roughly
// the most that fits beside the rest of the fast tier. Coverage comes from
// STRATIFICATION — the budget is spread evenly over query families rather than
// over files — so the sample's value grows with the corpus's diversity rather
// than with its size.
const SampleSize = 100

// featureComponent returns the value of a `key=value` component of a feature
// vector, or "" when the vector has no such component.
//
// Feature vectors are `;`-separated `key=value` components and the set of keys
// is OPEN — a generator that learns a new query construct adds a component
// (`exists=`, `scalarsub=`) without touching the older ones. Reading them
// positionally would therefore be correct only until the next construct lands,
// so every reader here goes by key.
func featureComponent(fv, key string) string {
	for _, part := range strings.Split(fv, ";") {
		if v, ok := strings.CutPrefix(part, key+"="); ok {
			return v
		}
	}
	return ""
}

// ShapeFamily is a feature vector's query-shape token — `single`,
// `join2.inner`, `join3.left-outer` and so on. It is the coarsest thing a
// generated query can be said to BE, and the axis a sample that has collapsed
// onto one query family collapses along.
func ShapeFamily(fv string) string { return featureComponent(fv, "shape") }

// whereClass is the predicate tree's top node class: the connective (`and`,
// `or`, `!and`) or, for a bare leaf, its comparison kind (`cmp`, `between`,
// `in`, `bit`, `arith`, `colcol`). Negation is preserved because `!and(...)`
// and `and(...)` reach different planner paths.
//
// It is deliberately the TOP node only. The whole predicate tree is already in
// the feature vector and is far too fine to stratify on; what this has to
// capture is the handful of structurally different things a WHERE clause can
// be, so that a budget spread over it covers all of them.
func whereClass(where string) string {
	if i := strings.IndexAny(where, ".("); i >= 0 {
		return where[:i]
	}
	return where
}

// StratumKey is the key StratifiedSample spreads the per-PR budget over:
// query shape crossed with WHERE-clause class.
//
// Its defining property is that its CARDINALITY FITS THE BUDGET. The full
// feature vector cannot be the stratification key, however natural it looks:
// it carries the index set, the projection and the ordering, so the committed
// corpus holds nearly as many distinct vectors as files. Round-robin over more
// groups than the budget never completes one pass, which makes it a
// lexicographic PREFIX of the group list wearing a stratifier's name — and
// since the vector starts with `shape=`, that prefix is a single query family.
// A coarse key with a few dozen values gets several passes and therefore
// actually stratifies.
func StratumKey(fv string) string {
	return ShapeFamily(fv) + "|" + whereClass(featureComponent(fv, "where")) + "|" + subqueryClass(fv)
}

// subqueryClass names the correlated sub-construct a query carries, because
// `shape` does not: a correlated EXISTS and a scalar subquery both hang off a
// query whose shape reads `single`, and they reach the semi-join and
// scalar-subquery paths that a plain single-table scan never touches.
//
// It is a stratum axis rather than a detail because these families are SMALL —
// tens of files in a corpus of thousands — and a budget spread over shape and
// predicate class alone steps over them entirely. Small and structurally
// distinct is exactly the combination that has no other coverage to fall back
// on, which is the case equal-allocation stratification exists to protect.
func subqueryClass(fv string) string {
	var parts []string
	if featureComponent(fv, "exists") != "" {
		parts = append(parts, "exists")
	}
	if featureComponent(fv, "scalarsub") != "" {
		parts = append(parts, "scalarsub")
	}
	if len(parts) == 0 {
		return "none"
	}
	return strings.Join(parts, "+")
}

// StratifiedSample picks up to n files, spreading the budget evenly over
// strata (StratumKey) and, within a stratum, over distinct feature vectors.
//
// The walk is round-robin over strata in sorted order, taking one file per
// stratum per pass; within a stratum the files are pre-interleaved by feature
// vector, so a stratum's second pick is a different vector rather than a
// second file of the same one. Dimension coverage therefore comes first at
// both levels and depth only after.
//
// It is a pure function of the file set — no clock, no randomness, no
// dependence on directory order (LoadDir already sorts). A stable sample
// matters as much as a representative one: a PR-tier gate that runs different
// tests each time turns a real regression into an intermittent.
//
// Equal allocation per stratum, not proportional. A systematic stride over the
// sorted vectors (`keys[i*len(keys)/n]`) also breaks the single-family
// collapse, and it loses on the case that matters: it samples each family in
// PROPORTION to its size, so a family holding fewer files than the stride
// length can be stepped over entirely — and the thinly covered families are
// exactly the ones with no other coverage to fall back on. Equal allocation
// makes reaching every stratum a guarantee rather than an artifact of the
// spacing. A stride is also brittle in a way this is not: it reads meaning
// into lexicographic ADJACENCY, so re-ordering the components of the feature
// vector string would silently change what gets sampled, whereas the
// stratification dimension here is named in code and cannot move without an
// edit.
func StratifiedSample(files []*File, n int) []*File {
	if n <= 0 || len(files) == 0 {
		return nil
	}

	byStratum := map[string]map[string][]*File{}
	for _, f := range files {
		sk := StratumKey(f.Header.FeatureVector)
		if byStratum[sk] == nil {
			byStratum[sk] = map[string][]*File{}
		}
		fv := f.Header.FeatureVector
		byStratum[sk][fv] = append(byStratum[sk][fv], f)
	}

	strata := make([]string, 0, len(byStratum))
	for sk := range byStratum {
		strata = append(strata, sk)
	}
	sort.Strings(strata)

	order := make(map[string][]*File, len(byStratum))
	for sk, byVector := range byStratum {
		vectors := make([]string, 0, len(byVector))
		for fv := range byVector {
			vectors = append(vectors, fv)
			sort.Slice(byVector[fv], func(i, j int) bool { return byVector[fv][i].Path < byVector[fv][j].Path })
		}
		sort.Strings(vectors)
		for depth := 0; ; depth++ {
			progressed := false
			for _, fv := range vectors {
				if depth >= len(byVector[fv]) {
					continue
				}
				progressed = true
				order[sk] = append(order[sk], byVector[fv][depth])
			}
			if !progressed {
				break
			}
		}
	}

	out := make([]*File, 0, n)
	for depth := 0; len(out) < n; depth++ {
		progressed := false
		for _, sk := range strata {
			g := order[sk]
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
