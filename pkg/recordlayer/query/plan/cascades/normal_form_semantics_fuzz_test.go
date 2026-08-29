package cascades

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
)

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
// produces: 43 predicate nodes, cnfSize estimate 19683 (1.9% of the 1,000,000
// production limit), and normalizeCNF then takes 2.9s and allocates 349 MiB.
// That is Java-aligned rather than a Go defect — Java's minorToNormalized
// builds the same eager cross-product and its getNormalizedSize likewise
// returns the major count (BooleanPredicateNormalizer.java:336-361, 509-543;
// Java even computes normalFormMaximumNumMinors, the clause WIDTH, and does not
// gate on it either) — but it will OOM a fuzz worker, which is how a
// differential becomes a flake.
//
// So the fuzzed inputs are bounded by the same estimate the transform uses,
// at a threshold picked from that measurement rather than by feel: 349 MiB over
// 19683 clauses is ~18 KiB per clause, so normalFormFuzzSizeBound keeps the
// worst case in single-digit MiB. Semantics, not stress, is what this target
// measures; the transform still runs, and the pinned blow-up above is what says
// where the stress question lives.
func FuzzNormalForm_PreservesSemantics(f *testing.F) {
	f.Add([]byte{0x00})
	f.Add([]byte{0x10, 0x01, 0x02, 0x20, 0x03})
	f.Add([]byte{0x21, 0x00, 0x11, 0x30, 0x04})
	f.Add([]byte{0x32, 0x05, 0x00, 0x12, 0x22})
	f.Add([]byte{0x40, 0x01, 0x41, 0x02, 0x03, 0x04})
	f.Add([]byte{0x70, 0x33, 0x44, 0x05, 0x16, 0x27})

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
			estimate: cnfSize,
			fn: func(p predicates.QueryPredicate) (predicates.QueryPredicate, bool) {
				return normalizeCNF(p, cnfSizeLimit)
			},
		},
		{
			name:     "dnf",
			estimate: dnfSize,
			fn: func(p predicates.QueryPredicate) (predicates.QueryPredicate, bool) {
				return NormalizeDNF(p, cnfSizeLimit)
			},
		},
		{
			name:     "dnf-exact",
			estimate: func(p predicates.QueryPredicate) int64 { return dnfSizeNegated(p, false) },
			fn: func(p predicates.QueryPredicate) (predicates.QueryPredicate, bool) {
				return NormalizeDNFWithoutSimplification(p, NormalizerDefaultSizeLimit)
			},
		},
	}

	f.Fuzz(func(t *testing.T, script []byte) {
		if len(script) == 0 {
			return
		}
		b := &predicateBuilder{script: script}
		pred := b.build(0)
		if pred == nil {
			return
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
// it is derived from: ~18 KiB per clause, so this keeps the worst case in
// single-digit MiB and out of the OOM that made the unbounded version flake.
//
// It is deliberately far below the production cnfSizeLimit (1,000,000). A
// differential is a semantics instrument; the clause count at which
// normalization becomes expensive is a different question, asked in a different
// place.
const normalFormFuzzSizeBound = 300
