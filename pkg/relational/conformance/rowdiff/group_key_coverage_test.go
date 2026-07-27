package rowdiff

import (
	"strings"
	"testing"
)

// A generator restriction is invisible: the sweep stays green because the shape
// is never emitted, and "green" reads as "covered". That is how a signed-zero
// GROUP BY defect went unfound here — the D/E columns seed -0.0, but grouping
// keys were hard-restricted to BIGINT, so the harness was structurally
// incapable of producing the query that would have exposed it. A reviewer had
// to find by reading what the generator could not generate.
//
// This pins the coverage itself, so lifting the restriction cannot silently
// regress. It is a property of the GENERATOR, not of the engine — no FDB, no
// planning, just: does the query stream actually contain the shape we believe
// it contains?
func TestGeneratorEmitsFloatGroupByKeys(t *testing.T) {
	t.Parallel()

	// Float-typed columns in the generator's fixed schema pool.
	floatCols := map[string]bool{"D": true, "E": true}

	const seeds = 400
	var (
		anyGroupBy    int
		floatGroupBy  int
		floatWithZero int
	)
	for seed := uint64(1); seed <= seeds; seed++ {
		c := Generate(seed)
		// A -0.0 anywhere in this case's data is what makes a float grouping
		// key interesting rather than merely present.
		hasNegZero := strings.Contains(c.InsertSQL(), "-0.0")
		for _, q := range c.Queries {
			if q.Agg == nil || len(q.Agg.GroupBy) == 0 {
				continue
			}
			anyGroupBy++
			for _, k := range q.Agg.GroupBy {
				if floatCols[strings.ToUpper(k)] {
					floatGroupBy++
					if hasNegZero {
						floatWithZero++
					}
					break
				}
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
	// The combination is the one that matters — a float grouping key over data
	// that actually contains both signed zeros.
	if floatWithZero == 0 {
		t.Fatalf("%d float GROUP BY queries in %d seeds but none over a case seeding -0.0 — "+
			"the shape is generated but never with the data that makes it discriminating",
			floatGroupBy, seeds)
	}
	t.Logf("coverage over %d seeds: %d GROUP BY queries, %d on a float key, %d of those over "+
		"data containing -0.0", seeds, anyGroupBy, floatGroupBy, floatWithZero)
}
