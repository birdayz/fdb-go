package cascades

import (
	"bytes"
	"sync/atomic"
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/internal/fuzzfloor"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
)

// normalFormIdentitySeedScripts is the ORIGINAL corpus, and every one of these
// six normalizes to ITSELF: all three transforms return changed=false on all
// six.
//
// That was the whole corpus. CI runs `go test`, which runs the seed corpus and
// never `-fuzz`, so this differential asserted NOTHING on every CI run it has
// ever been part of — while reporting the same green it reports when the
// normalizers agree. The transformsApplied floor below is what makes that state
// loud, and it was written and observed failing BEFORE the seeds that satisfy
// it were added.
//
// They are kept rather than replaced: a transform declining is a real outcome
// and these hold the decline path, which is where a normalizer that starts
// rewriting an already-normal predicate would show up.
var normalFormIdentitySeedScripts = [][]byte{
	{0x00},
	{0x10, 0x01, 0x02, 0x20, 0x03},
	{0x21, 0x00, 0x11, 0x30, 0x04},
	{0x32, 0x05, 0x00, 0x12, 0x22},
	{0x40, 0x01, 0x41, 0x02, 0x03, 0x04},
	{0x70, 0x33, 0x44, 0x05, 0x16, 0x27},
}

// normalFormTransformingSeedScripts are inputs on which ALL THREE transforms
// report changed — derived by searching the builder's script space, then picked
// for SHAPE rather than for count. In order: a NOT over an OR of NOTs (pure De
// Morgan, the rewrite RFC-240 added), a NOT over an OR beside nested ANDs, a NOT
// over a leaf inside an OR of ANDs (absorption territory), an UNKNOWN conjunct
// over a distributing subtree (three-valued distribution, where a two-valued
// justification breaks), a mixed AND/OR/TRUE/IS-NULL tree, and a wide OR tree
// with a NOT (dedup territory).
//
// Being a SEPARATE slice is what lets the floor be exact rather than a fraction:
// every one of these must drive every transform, so the floor is
// len(this) * len(transforms) and any single seed going quiet reddens
// immediately, instead of after a third of the corpus has gone dark.
var normalFormTransformingSeedScripts = [][]byte{
	{0xa7, 0xb5, 0x5d, 0x4f, 0xe1, 0x45, 0xdf},
	{0x13, 0xe1, 0x17, 0x35, 0x82},
	{0xc5, 0x13, 0x3f, 0x79},
	{0x34, 0xcd, 0x18, 0xe8, 0xbc},
	{0x5d, 0x78, 0x04, 0x0e, 0xcb, 0xea, 0x03},
	{0x2e, 0x07, 0x39, 0x6d, 0x78},
}

// normalFormSeedScripts is the deterministic corpus
// FuzzNormalForm_PreservesSemantics runs on every `go test` with no -fuzz flag.
// It is the population that target's coverage floors are written against, which
// is why it is a named slice rather than inline f.Add calls: a floor written as
// a literal stops describing the corpus the first time a seed is added.
var normalFormSeedScripts = append(
	append([][]byte(nil), normalFormIdentitySeedScripts...),
	normalFormTransformingSeedScripts...)

// FuzzNormalForm_PreservesSemantics is the semantics differential for the three
// normal-form entry points — the CNF one is the ONLY predicate rewrite in this
// package that the default planner pipeline actually runs
// (NormalizePredicatesRule, default_rules.go:148 and :178).
//
// That is worth stating because the sibling differential in this package,
// FuzzSimplifyPredicate_PreservesSemantics, covers the boolean rule sets, and
// those have exactly one production caller between them
// (rule_eliminate_null_on_empty.go:153). Normalization is where a defect would
// actually reach a query.
//
// What can go wrong here is distribution and absorption under three-valued
// logic. Distributing OR over AND multiplies a subterm across clauses;
// absorption DELETES a clause on a subset test; both are steps whose textbook
// justification is two-valued, and both are re-checked here against rows where
// a column is NULL and the leaf is therefore UNKNOWN rather than FALSE.
//
// UNKNOWN is a third outcome, not an error: a row that goes UNKNOWN -> FALSE is
// dropped either way, but UNKNOWN -> TRUE is a row appearing from nowhere.
// Both directions fail.
//
// SIZE BOUND, and why it is not the production one. Normalization builds its
// cross-product eagerly, and the size guard counts CLAUSES, not atoms — so the
// clause count is a poor proxy for memory. Measured on a tree this builder
// produces: 43 predicate nodes, normalFormSize CNF 19683 — 1.9% of the
// 1,000,000 production limit — and normalizeCNF then takes 638ms and allocates
// 67 MiB, about 3.5 KiB per clause.
//
// That is Java-aligned rather than a Go defect: Java's minorToNormalized builds
// the same eager cross-product and its getNormalizedSize likewise returns the
// major count (BooleanPredicateNormalizer.java:336-361, 509-543; Java even
// computes normalFormMaximumNumMinors, the clause WIDTH, and does not gate on
// it either). But it will OOM a fuzz worker, which is how a differential
// becomes a flake, so the fuzzed inputs are bounded by the same estimate the
// transform uses — normalFormFuzzSizeBound, which at 3.5 KiB per clause keeps
// the worst case near a megabyte.
//
// The 638ms/67 MiB figures are RE-DERIVED, not carried: the same input measured
// 2.9s and 349 MiB before RFC-240 unified the two normalizers. The ported
// minorToNormalized preallocates each cross-product generation
// (make(..., 0, len(cross)*len(alternatives))) where the retired orToCNF grew
// it by append, which is where the ~5x went. The estimate is unchanged at 19683
// because this input carries no NOT over a connective, so the negate-aware
// metric and the negate-blind one agree on it.
//
// Semantics, not stress, is what this target measures; the transform still
// runs, and the measurement above is what says where the stress question lives.
func FuzzNormalForm_PreservesSemantics(f *testing.F) {
	for _, seed := range normalFormSeedScripts {
		f.Add(seed)
	}

	rows := predicateSemanticsRows()
	transforms := []struct {
		name string
		// estimate is the transform's OWN size estimate for this input, used
		// to bound the fuzzed population — each transform is bounded by the
		// estimate it would itself consult, not by a shared approximation.
		estimate func(predicates.QueryPredicate) int64
		fn       func(predicates.QueryPredicate) (predicates.QueryPredicate, bool)
	}{
		{
			name:     "cnf",
			estimate: func(p predicates.QueryPredicate) int64 { return normalFormSize(p, false, normalFormCNF) },
			fn: func(p predicates.QueryPredicate) (predicates.QueryPredicate, bool) {
				return normalizeCNF(p, cnfSizeLimit)
			},
		},
		{
			name:     "dnf",
			estimate: func(p predicates.QueryPredicate) int64 { return normalFormSize(p, false, normalFormDNF) },
			fn: func(p predicates.QueryPredicate) (predicates.QueryPredicate, bool) {
				return NormalizeDNF(p, cnfSizeLimit)
			},
		},
		{
			name:     "dnf-exact",
			estimate: func(p predicates.QueryPredicate) int64 { return normalFormSize(p, false, normalFormDNF) },
			fn: func(p predicates.QueryPredicate) (predicates.QueryPredicate, bool) {
				return NormalizeDNFWithoutSimplification(p, NormalizerDefaultSizeLimit)
			},
		},
	}

	// FOUR escapes stand between an input and an assertion here: an empty script,
	// a builder returning nil, an estimate over the size bound, and a transform
	// that DECLINED (!changed). Each is a bare `continue`, so a size bound tuned
	// too low, a builder that stopped producing connectives, or a normalizer that
	// started declining everything would leave this target reporting the same
	// green it reports when it agrees. The `!changed` escape is the live one: it
	// is the normal outcome for an already-normal predicate, so it costs nothing
	// to hit and would swallow the whole corpus silently.
	//
	// The floors below therefore count the transforms that ACTUALLY RAN and the
	// rows actually compared. They describe the seed-corpus run and nothing else
	// — see fuzzfloor.SuppressedFor for why a floor cannot be enforced against
	// the target being fuzzed, and why "is anything being fuzzed" is the wrong
	// question to ask.
	//
	// They count PER SEED, not in aggregate, and that is not fussiness. A single
	// total over 12 seeds against a floor of 6x3 is satisfiable by the wrong 18:
	// an identity seed that started transforming would MASK a transforming seed
	// that went dark, and the sum would sit exactly on its floor while half the
	// shapes this corpus was built for stopped being exercised. Per-seed
	// equality cannot be satisfied by a substitution.
	var builtPredicates atomic.Int64
	transformsBySeed := make([]atomic.Int64, len(normalFormTransformingSeedScripts))
	comparedBySeed := make([]atomic.Int64, len(normalFormTransformingSeedScripts))
	var identityTransforms atomic.Int64

	f.Cleanup(func() {
		if fuzzfloor.SuppressedFor("FuzzNormalForm_PreservesSemantics") {
			return
		}
		if builtPredicates.Load() < int64(len(normalFormSeedScripts)) {
			f.Errorf("the builder produced %d usable predicates from %d seeds — it has stopped "+
				"building, and every assertion in this differential ran on nothing",
				builtPredicates.Load(), len(normalFormSeedScripts))
		}
		// Every transforming seed must drive EVERY transform. Exact, per seed.
		for i := range transformsBySeed {
			if got, want := transformsBySeed[i].Load(), int64(len(transforms)); got != want {
				f.Errorf("transforming seed %d (%#x) drove %d of %d transforms — it has gone "+
					"quiet, and the shape it was chosen for is no longer exercised. An "+
					"aggregate floor would have hidden this behind the other seeds.",
					i, normalFormTransformingSeedScripts[i], got, want)
			}
			if got, want := comparedBySeed[i].Load(), int64(len(transforms)*len(rows)); got != want {
				f.Errorf("transforming seed %d compared %d (transform, row) pairs, want %d — "+
					"rows are being skipped by the wantErr escape, so this seed is asking "+
					"less than it looks", i, got, want)
			}
		}
		// The identity seeds are watched for GROWTH, not collapse: zero is their
		// steady state, and a non-zero here means a normalizer started rewriting
		// an already-normal predicate. That is a real finding rather than a
		// broken gate, but it must not be allowed to silently satisfy the floors
		// above, which is exactly what an aggregate count would let it do.
		if got := identityTransforms.Load(); got != 0 {
			f.Errorf("%d transform runs rewrote an IDENTITY seed, which normalizes to itself. "+
				"Either a normalizer changed behaviour, or a seed moved between the two "+
				"slices. Reconcile the slices; do not relax this to >= 0.", got)
		}
	})

	f.Fuzz(func(t *testing.T, script []byte) {
		if len(script) == 0 {
			return
		}
		b := &predicateBuilder{script: script}
		pred := b.build(0)
		if pred == nil {
			return
		}
		builtPredicates.Add(1)

		// Which seed is this? Under -fuzz a generated input matches neither
		// slice and is counted in neither, which is correct: the floors are
		// suppressed for this target then anyway.
		seedIdx := -1
		for i, s := range normalFormTransformingSeedScripts {
			if bytes.Equal(s, script) {
				seedIdx = i
				break
			}
		}
		isIdentitySeed := false
		for _, s := range normalFormIdentitySeedScripts {
			if bytes.Equal(s, script) {
				isIdentitySeed = true
				break
			}
		}

		for _, tr := range transforms {
			if tr.estimate(pred) > normalFormFuzzSizeBound {
				continue
			}
			out, changed := tr.fn(pred)
			if out == nil {
				t.Fatalf("%s returned nil for %s", tr.name, pred.Explain())
			}
			if !changed {
				// Declined — nothing was transformed, so there is nothing to
				// differential. Asserting on it would be asserting that the
				// identity preserves semantics.
				continue
			}
			switch {
			case seedIdx >= 0:
				transformsBySeed[seedIdx].Add(1)
			case isIdentitySeed:
				identityTransforms.Add(1)
			}

			for i, row := range rows {
				want, wantErr := evalPredicateSafely(pred, row)
				if wantErr != nil {
					continue
				}
				got, gotErr := evalPredicateSafely(out, row)
				if gotErr != nil {
					t.Fatalf("%s row %d: original evaluated cleanly to %s but the normal form errored: %v\n  in:  %s\n  out: %s",
						tr.name, i, triName(want), gotErr, pred.Explain(), out.Explain())
				}
				if seedIdx >= 0 {
					comparedBySeed[seedIdx].Add(1)
				}
				if triName(want) != triName(got) {
					t.Fatalf("%s row %d: normalization changed the truth value: want %s, got %s\n  in:  %s\n  out: %s",
						tr.name, i, triName(want), triName(got), pred.Explain(), out.Explain())
				}
			}
		}
	})
}

// normalFormFuzzSizeBound caps the normal-form CLAUSE COUNT this differential
// will drive a transform to. See the target's doc comment for the measurement
// it is derived from: ~3.5 KiB per clause, so this keeps the worst case near
// a megabyte and out of the OOM that made the unbounded version flake.
//
// It is deliberately below the production gates — 10x under cnfSizeLimit
// (3000, Java's DEFAULT_COMPLEXITY_THRESHOLD) and far under
// NormalizerDefaultSizeLimit (1,000,000, the write path's). The ratio is
// written against BOTH because they are no longer the same number, and this
// comment said "the production cnfSizeLimit (1,000,000)" after one of them
// moved.
//
// A differential is a semantics instrument; the clause count at which
// normalization becomes expensive is a different question, asked in a different
// place.
const normalFormFuzzSizeBound = 300
