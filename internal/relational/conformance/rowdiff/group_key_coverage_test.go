package rowdiff

import (
	"math"
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
)

// A generator restriction is invisible: the sweep stays green because the shape
// is never emitted, and "green" reads as "covered". That is how a signed-zero
// GROUP BY defect went unfound here — the D/E columns seed -0.0, but grouping
// keys were hard-restricted to BIGINT, so the harness was structurally
// incapable of producing the query that would have exposed it. Only reading
// what the generator could not generate surfaced the gap.
//
// This pins the coverage itself, so lifting the restriction cannot silently
// regress. It is a property of the GENERATOR, not of the engine — no FDB, no
// planning, just: does the query stream actually contain the shape we believe
// it contains?
//
// The discriminating condition is precise, and deliberately not a proxy for it.
// An earlier version of this test asked only whether the CASE's insert SQL
// mentioned "-0.0" and then credited every float-key query in that case. That
// passes when the -0.0 sits in a column the query does not group on, or when
// the query's WHERE removes those rows — i.e. it could report full coverage
// while no emitted query's actual input distinguished signed zero at all. It
// was the very defect this file exists to prevent, one level up.
//
// What actually discriminates: two rows that SURVIVE the query's WHERE whose
// grouping-key tuples are equal in every component except one float component,
// where one side is -0.0 and the other +0.0. Only such a pair makes merge and
// split observably different answers.
func TestGeneratorEmitsFloatGroupByKeys(t *testing.T) {
	t.Parallel()

	floatCols := map[string]bool{"D": true, "E": true}

	// signedZeroKind reports -1 for -0.0, +1 for +0.0, 0 for anything else.
	signedZeroKind := func(v any) int {
		f, ok := v.(float64)
		if !ok || f != 0 {
			return 0
		}
		if math.Signbit(f) {
			return -1
		}
		return 1
	}

	const seeds = 400
	var anyGroupBy, floatGroupBy, discriminating int

	for seed := uint64(1); seed <= seeds; seed++ {
		c := Generate(seed)
		for _, q := range c.Queries {
			if q.Agg == nil || len(q.Agg.GroupBy) == 0 {
				continue
			}
			anyGroupBy++
			hasFloatKey := false
			for _, k := range q.Agg.GroupBy {
				if floatCols[k] {
					hasFloatKey = true
					break
				}
			}
			if !hasFloatKey {
				continue
			}
			floatGroupBy++

			// Rows that survive the query's WHERE — the actual grouping input.
			var kept []Row
			for _, r := range c.Rows {
				if q.Where != nil {
					tb, err := evalBool(q.Where, r)
					if err != nil || tb != predicates.TriTrue {
						continue
					}
				}
				kept = append(kept, r)
			}

			// Look for two surviving rows whose grouping tuples differ ONLY in
			// the sign of one zero-valued float component.
			found := false
			for i := 0; i < len(kept) && !found; i++ {
				for j := i + 1; j < len(kept) && !found; j++ {
					signFlips, otherDiffs := 0, 0
					for _, k := range q.Agg.GroupBy {
						a, b := kept[i][k], kept[j][k]
						ka, kb := signedZeroKind(a), signedZeroKind(b)
						if ka != 0 && kb != 0 && ka != kb {
							signFlips++
							continue
						}
						if a != b {
							otherDiffs++
						}
					}
					if signFlips == 1 && otherDiffs == 0 {
						found = true
					}
				}
			}
			if found {
				discriminating++
			}
		}
	}

	if anyGroupBy == 0 {
		t.Fatalf("no GROUP BY queries at all in %d seeds — the aggregate generator is not "+
			"producing grouped shapes, which invalidates far more than this test", seeds)
	}
	if floatGroupBy == 0 {
		t.Fatalf("%d GROUP BY queries in %d seeds and NONE grouped on a DOUBLE/FLOAT column. "+
			"The restriction that hid the signed-zero GROUP BY shape is back: the harness is "+
			"green because it cannot emit the query, not because the query passes", anyGroupBy, seeds)
	}
	if discriminating == 0 {
		t.Fatalf("%d float GROUP BY queries in %d seeds, but NONE has two surviving rows whose "+
			"grouping keys differ only in the sign of a zero. The shape is generated but no "+
			"instance of it can tell merge from split, so it proves nothing about signed-zero "+
			"grouping", floatGroupBy, seeds)
	}
	t.Logf("coverage over %d seeds: %d GROUP BY queries, %d on a float key, %d with a surviving "+
		"pair that differs only in a zero's sign", seeds, anyGroupBy, floatGroupBy, discriminating)
}
