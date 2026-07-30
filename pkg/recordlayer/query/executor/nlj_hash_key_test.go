package executor

import (
	"fmt"
	"math"
	"testing"

	"fdb.dev/pkg/recordlayer"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
)

// This file pins the NLJ hash fast-path's key-normalization contract: the
// hash pre-filter (tryBuildHashIndex + the evalLegHashKey probe) must NEVER
// disagree with the linear per-pair predicate re-check (cmpAny), which
// promotes numerics and handles []byte. Two historical defects pinned here:
//
//   - a []byte join key is an unhashable map key — the index build PANICKED
//     ("hash of unhashable type: []uint8") the moment the inner leg crossed
//     the 100-row hash threshold, so a join that worked at 99 rows crashed
//     at 100 (fix: bytes decline the hash, the linear path handles them);
//   - Go map lookup is exact-dynamic-type — int64(5) missed the float64(5)
//     bucket, and a key MISS yields zero candidates (not the degraded
//     full-candidate list, which only fires on resolution FAILURE), so
//     cross-type numeric equijoins (BIGINT=DOUBLE, covering-index float32 vs
//     stored-record float64) silently dropped EVERY match once the inner leg
//     hit 100 rows (fix: all numeric keys canonicalize to float64 on both
//     the build and probe sides; cmpAny itself compares pure-integer pairs
//     exactly via values.CompareExactInts, and equal VALUES convert to the
//     same float64, so the bucketing stays equality-faithful).

// nljKeyLegs builds the two-leg fixtures of ojWiringLegs with configurable
// join-key column types: A[ID outerKey, V long] / B[ID innerKey, W long],
// their typed QOVs, and the ordinal seed RC.
func nljKeyLegs(t *testing.T, outerKey, innerKey values.Type) (legA, legB *values.RecordType, qovA, qovB *values.QuantifiedObjectValue, seed *values.RecordConstructorValue) {
	t.Helper()
	legA = values.NewRecordType("", false, []values.Field{
		{Name: "ID", FieldType: outerKey, Ordinal: 0},
		{Name: "V", FieldType: values.NotNullLong, Ordinal: 1},
	})
	legB = values.NewRecordType("", false, []values.Field{
		{Name: "ID", FieldType: innerKey, Ordinal: 0},
		{Name: "W", FieldType: values.NotNullLong, Ordinal: 1},
	})
	qovA = values.NewQuantifiedObjectValueOfType(values.NamedCorrelationIdentifier("A"), legA)
	qovB = values.NewQuantifiedObjectValueOfType(values.NamedCorrelationIdentifier("B"), legB)
	seed = buildOrdinalJoinRC(t, qovA, qovB)
	return legA, legB, qovA, qovB, seed
}

// nljKeyEqPred builds the single-equality equijoin A.ID = B.ID over the
// typed leg QOVs — the exact shape extractEquijoinOperands accepts.
func nljKeyEqPred(qovA, qovB *values.QuantifiedObjectValue, outerKey, innerKey values.Type) predicates.QueryPredicate {
	return ojEqPred(
		values.NewCorrelatedFieldValueWithResolvedOrdinal(qovA, "ID", 0, outerKey),
		values.NewCorrelatedFieldValueWithResolvedOrdinal(qovB, "ID", 0, innerKey),
	)
}

// TestNLJCursor_HashPath_BytesKeys pins the []byte-key crash (a join key of
// BYTES type is an unhashable Go map key): ≥100 inner rows used to PANIC in
// tryBuildHashIndex ("hash of unhashable type: []uint8"). Bytes keys must
// DECLINE the hash index — the linear path's cmpAny has a []byte arm and
// evaluates every pair correctly.
func TestNLJCursor_HashPath_BytesKeys(t *testing.T) {
	t.Parallel()
	legA, legB, qovA, qovB, seed := nljKeyLegs(t, values.NotNullBytes, values.NotNullBytes)
	pred := nljKeyEqPred(qovA, qovB, values.NotNullBytes, values.NotNullBytes)
	bkey := func(i int) []byte { return []byte(fmt.Sprintf("k%03d", i)) }
	outerRows := []QueryResult{ojLegQR(t, legA, bkey(5), int64(50))}

	run := func(t *testing.T, nInner int) (*nljCursor, []QueryResult) {
		t.Helper()
		innerRows := make([]QueryResult, nInner)
		for i := range innerRows {
			innerRows[i] = ojLegQR(t, legB, bkey(i), int64(i*10))
		}
		c := mustNLJCursor(t, recordlayer.FromList(outerRows), innerRows, plans.JoinInner,
			values.NamedCorrelationIdentifier("A"), values.NamedCorrelationIdentifier("B"), []predicates.QueryPredicate{pred}, seed, EmptyEvaluationContext(), nil)
		t.Cleanup(func() { c.Close() })
		return c, collectCursor(t, c)
	}

	t.Run("120 inner rows decline the hash and match linearly", func(t *testing.T) {
		t.Parallel()
		c, results := run(t, 120)
		if c.hashIndex != nil {
			t.Fatal("bytes join keys must DECLINE the hash index (unhashable map key), not build one")
		}
		if len(results) != 1 {
			t.Fatalf("got %d rows, want 1 (outer k005 matches inner k005)", len(results))
		}
		ojAssertSlots(t, results[0].Positional, bkey(5), int64(50), bkey(5), int64(50))
	})

	// The size cliff: 99 inner rows never reach the hash threshold, so the
	// linear path always handled bytes correctly — the SAME join must not
	// change answer (or crash) when the inner leg grows past 100 rows.
	t.Run("99-inner-row linear control", func(t *testing.T) {
		t.Parallel()
		c, results := run(t, 99)
		if c.hashIndex != nil {
			t.Fatal("99 inner rows are below the hash threshold — control must run the linear path")
		}
		if len(results) != 1 {
			t.Fatalf("got %d rows, want 1 (outer k005 matches inner k005)", len(results))
		}
		ojAssertSlots(t, results[0].Positional, bkey(5), int64(50), bkey(5), int64(50))
	})
}

// TestNLJCursor_HashPath_CrossNumericKeys pins the silent wrong-rows defect:
// a BIGINT=DOUBLE equijoin (outer int64(5), inner float64 keys) — cmpAny
// promotes and matches int64(5) = float64(5), but the raw-dynamic-type hash
// key made int64(5) MISS the float64(5) bucket, and a bucket miss yields
// zero candidates (no degraded re-check), dropping every match once the
// inner leg hit the 100-row hash threshold. Both sides must canonicalize
// numeric keys to float64 — and the hash must still FIRE (declining numerics
// would kill the optimization, not fix it).
func TestNLJCursor_HashPath_CrossNumericKeys(t *testing.T) {
	t.Parallel()
	legA, legB, qovA, qovB, seed := nljKeyLegs(t, values.NotNullLong, values.NotNullDouble)
	pred := nljKeyEqPred(qovA, qovB, values.NotNullLong, values.NotNullDouble)
	outerRows := []QueryResult{ojLegQR(t, legA, int64(5), int64(50))}

	run := func(t *testing.T, nInner int) (*nljCursor, []QueryResult) {
		t.Helper()
		innerRows := make([]QueryResult, nInner)
		for i := range innerRows {
			innerRows[i] = ojLegQR(t, legB, float64(i), int64(i*10))
		}
		c := mustNLJCursor(t, recordlayer.FromList(outerRows), innerRows, plans.JoinInner,
			values.NamedCorrelationIdentifier("A"), values.NamedCorrelationIdentifier("B"), []predicates.QueryPredicate{pred}, seed, EmptyEvaluationContext(), nil)
		t.Cleanup(func() { c.Close() })
		return c, collectCursor(t, c)
	}

	t.Run("120 inner rows match through the hash", func(t *testing.T) {
		t.Parallel()
		c, results := run(t, 120)
		if c.hashIndex == nil {
			t.Fatal("hash index was not built — cross-numeric keys must normalize, not decline")
		}
		if len(results) != 1 {
			t.Fatalf("got %d rows, want 1 (int64(5) = float64(5) per cmpAny's numeric promotion)", len(results))
		}
		ojAssertSlots(t, results[0].Positional, int64(5), int64(50), float64(5), int64(50))
	})

	// The size cliff: below the hash threshold the linear cmpAny path always
	// matched this shape — the SAME join must not lose its rows at 100.
	t.Run("50-inner-row linear control", func(t *testing.T) {
		t.Parallel()
		c, results := run(t, 50)
		if c.hashIndex != nil {
			t.Fatal("50 inner rows are below the hash threshold — control must run the linear path")
		}
		if len(results) != 1 {
			t.Fatalf("got %d rows, want 1 (int64(5) = float64(5) per cmpAny's numeric promotion)", len(results))
		}
		ojAssertSlots(t, results[0].Positional, int64(5), int64(50), float64(5), int64(50))
	})
}

// TestNLJCursor_HashPath_UnsignedKeys pins the unsigned half of the
// integer join-key domain: tuple decoding preserves positive integers above
// math.MaxInt64 as uint64, so uint64 reaches the equijoin key path on valid
// rows. Before values.CompareExactInts, cmpAny admitted no unsigned form —
// the pair failed comparison and the join dropped the row on BOTH the hash
// and linear paths; and ToFloat64's unsigned exclusion made
// normalizeNLJHashKey decline the key outright. Both a same-type uint64
// pair and the cmpAny-equal cross-form uint64(5)=int64(5) must match
// through the hash (equal VALUES convert to the same float64 bucket).
func TestNLJCursor_HashPath_UnsignedKeys(t *testing.T) {
	t.Parallel()
	legA, legB, qovA, qovB, seed := nljKeyLegs(t, values.NotNullLong, values.NotNullLong)
	pred := nljKeyEqPred(qovA, qovB, values.NotNullLong, values.NotNullLong)
	big := uint64(math.MaxInt64) + 7
	outerRows := []QueryResult{
		ojLegQR(t, legA, big, int64(50)),
		ojLegQR(t, legA, uint64(5), int64(51)),
	}

	run := func(t *testing.T, nInner int) (*nljCursor, []QueryResult) {
		t.Helper()
		innerRows := make([]QueryResult, 0, nInner)
		innerRows = append(innerRows, ojLegQR(t, legB, big, int64(1000)))      // same-type uint64 match
		innerRows = append(innerRows, ojLegQR(t, legB, int64(5), int64(1001))) // cross-form match
		for i := len(innerRows); i < nInner; i++ {
			innerRows = append(innerRows, ojLegQR(t, legB, int64(1_000_000+i), int64(i)))
		}
		c := mustNLJCursor(t, recordlayer.FromList(outerRows), innerRows, plans.JoinInner,
			values.NamedCorrelationIdentifier("A"), values.NamedCorrelationIdentifier("B"), []predicates.QueryPredicate{pred}, seed, EmptyEvaluationContext(), nil)
		t.Cleanup(func() { c.Close() })
		return c, collectCursor(t, c)
	}

	t.Run("120 inner rows match through the hash", func(t *testing.T) {
		t.Parallel()
		c, results := run(t, 120)
		if c.hashIndex == nil {
			t.Fatal("hash index was not built — unsigned keys must normalize, not decline")
		}
		if len(results) != 2 {
			t.Fatalf("got %d rows, want 2 (uint64 same-type + uint64(5)=int64(5) cross-form)", len(results))
		}
	})

	t.Run("50-inner-row linear control", func(t *testing.T) {
		t.Parallel()
		c, results := run(t, 50)
		if c.hashIndex != nil {
			t.Fatal("50 inner rows are below the hash threshold — control must run the linear path")
		}
		if len(results) != 2 {
			t.Fatalf("got %d rows, want 2 on the linear path", len(results))
		}
	})
}

// TestNLJCursor_HashPath_Float32VsFloat64Keys pins the mixed-width float
// shape of the same defect: a FLOAT=FLOAT join where one leg decodes from a
// covering index (tuple decode → float32) and the other from a stored record
// (proto FloatKind → float64). cmpAny promotes both to float64 and matches;
// the raw-dynamic-type hash key put float32(5) and float64(5) in different
// buckets and dropped the match.
func TestNLJCursor_HashPath_Float32VsFloat64Keys(t *testing.T) {
	t.Parallel()
	legA, legB, qovA, qovB, seed := nljKeyLegs(t, values.NotNullFloat, values.NotNullDouble)
	pred := nljKeyEqPred(qovA, qovB, values.NotNullFloat, values.NotNullDouble)

	innerRows := make([]QueryResult, 120)
	for i := range innerRows {
		innerRows[i] = ojLegQR(t, legB, float64(i), int64(i*10))
	}
	outerRows := []QueryResult{ojLegQR(t, legA, float32(5), int64(50))}

	c := mustNLJCursor(t, recordlayer.FromList(outerRows), innerRows, plans.JoinInner,
		values.NamedCorrelationIdentifier("A"), values.NamedCorrelationIdentifier("B"), []predicates.QueryPredicate{pred}, seed, EmptyEvaluationContext(), nil)
	defer c.Close()
	if c.hashIndex == nil {
		t.Fatal("hash index was not built — mixed-width float keys must normalize, not decline")
	}
	results := collectCursor(t, c)
	if len(results) != 1 {
		t.Fatalf("got %d rows, want 1 (float32(5) = float64(5) per cmpAny's float promotion)", len(results))
	}
	ojAssertSlots(t, results[0].Positional, float32(5), int64(50), float64(5), int64(50))
}

// TestNLJCursor_HashPath_StringKeys guards the already-correct exact-type
// case through the hash path: string keys are hashable and cmpAny's string
// arm is exact equality, so the hash must still FIRE and match — the
// normalization whitelist must not accidentally decline strings.
func TestNLJCursor_HashPath_StringKeys(t *testing.T) {
	t.Parallel()
	legA, legB, qovA, qovB, seed := nljKeyLegs(t, values.NotNullString, values.NotNullString)
	pred := nljKeyEqPred(qovA, qovB, values.NotNullString, values.NotNullString)

	innerRows := make([]QueryResult, 120)
	for i := range innerRows {
		innerRows[i] = ojLegQR(t, legB, fmt.Sprintf("k%03d", i), int64(i*10))
	}
	outerRows := []QueryResult{ojLegQR(t, legA, "k005", int64(50))}

	c := mustNLJCursor(t, recordlayer.FromList(outerRows), innerRows, plans.JoinInner,
		values.NamedCorrelationIdentifier("A"), values.NamedCorrelationIdentifier("B"), []predicates.QueryPredicate{pred}, seed, EmptyEvaluationContext(), nil)
	defer c.Close()
	if c.hashIndex == nil {
		t.Fatal("hash index was not built — string keys are hashable and exact, must not decline")
	}
	results := collectCursor(t, c)
	if len(results) != 1 {
		t.Fatalf("got %d rows, want 1 (outer k005 matches inner k005)", len(results))
	}
	ojAssertSlots(t, results[0].Positional, "k005", int64(50), "k005", int64(50))
}

// TestNLJCursor_HashPath_NaNKeys pins hash/linear AGREEMENT on NaN join
// keys. cmpAny follows Java's Double semantics (values.CompareFloat64):
// NaN never equals a non-NaN float (NaN is the greatest value, so
// `NaN = 5.0` is false), and NaN equals NaN. A NaN Go map key, meanwhile,
// matches NOTHING through the hash (its bucket is unreachable, probes
// always miss), so NaN must abandon the hash entirely: an inner-side NaN
// declines the index at build, an outer-side NaN degrades that probe to
// the full candidate list — and in both cases the per-pair predicate
// re-check (cmpAny) is the sole authority on which pairs match. The
// invariant under test is that the ≥100-row hash path returns exactly what
// the linear path returns, and that both agree with Java: NaN matches only
// another NaN, never a number.
func TestNLJCursor_HashPath_NaNKeys(t *testing.T) {
	t.Parallel()

	t.Run("NaN inner key declines the hash at build", func(t *testing.T) {
		t.Parallel()
		legA, legB, qovA, qovB, seed := nljKeyLegs(t, values.NotNullLong, values.NotNullDouble)
		pred := nljKeyEqPred(qovA, qovB, values.NotNullLong, values.NotNullDouble)
		innerRows := make([]QueryResult, 120)
		for i := range innerRows {
			key := float64(i)
			if i == 7 {
				key = math.NaN()
			}
			innerRows[i] = ojLegQR(t, legB, key, int64(i*10))
		}
		outerRows := []QueryResult{ojLegQR(t, legA, int64(5), int64(50))}
		c := mustNLJCursor(t, recordlayer.FromList(outerRows), innerRows, plans.JoinInner,
			values.NamedCorrelationIdentifier("A"), values.NamedCorrelationIdentifier("B"), []predicates.QueryPredicate{pred}, seed, EmptyEvaluationContext(), nil)
		defer c.Close()
		if c.hashIndex != nil {
			t.Fatal("an inner NaN key must DECLINE the hash index — its bucket would be unreachable, so the whole join falls back to the linear predicate")
		}
		results := collectCursor(t, c)
		// Java-faithful linear semantics: int64(5) matches float64(5) only;
		// the NaN inner key does NOT equal int64(5) (NaN vs a number is never
		// equal), so it is not a match.
		if len(results) != 1 {
			t.Fatalf("got %d rows, want 1 (only float64(5) equals int64(5); the NaN inner key does not)", len(results))
		}
	})

	t.Run("NaN probe degrades to the full candidate list", func(t *testing.T) {
		t.Parallel()
		legA, legB, qovA, qovB, seed := nljKeyLegs(t, values.NotNullDouble, values.NotNullDouble)
		pred := nljKeyEqPred(qovA, qovB, values.NotNullDouble, values.NotNullDouble)
		innerRows := make([]QueryResult, 120)
		for i := range innerRows {
			innerRows[i] = ojLegQR(t, legB, float64(i), int64(i*10))
		}
		outerRows := []QueryResult{ojLegQR(t, legA, math.NaN(), int64(50))}
		c := mustNLJCursor(t, recordlayer.FromList(outerRows), innerRows, plans.JoinInner,
			values.NamedCorrelationIdentifier("A"), values.NamedCorrelationIdentifier("B"), []predicates.QueryPredicate{pred}, seed, EmptyEvaluationContext(), nil)
		defer c.Close()
		if c.hashIndex == nil {
			t.Fatal("clean float inner keys must build the hash — only the NaN PROBE degrades")
		}
		results := collectCursor(t, c)
		// Java-faithful linear semantics: a NaN probe equals NO non-NaN float
		// (NaN is the greatest value, never equal to a finite number). The
		// degraded full-candidate list is re-checked per pair and matches none.
		if len(results) != 0 {
			t.Fatalf("got %d rows, want 0 (a NaN probe equals no numeric inner key)", len(results))
		}
	})

	t.Run("NaN matches NaN", func(t *testing.T) {
		t.Parallel()
		// The positive edge: NaN = NaN is TRUE (Double.compare(NaN,NaN)==0).
		// An inner NaN declines the hash, so the linear predicate decides —
		// and it must match the NaN outer against exactly the NaN inner key,
		// never against the finite floats.
		legA, legB, qovA, qovB, seed := nljKeyLegs(t, values.NotNullDouble, values.NotNullDouble)
		pred := nljKeyEqPred(qovA, qovB, values.NotNullDouble, values.NotNullDouble)
		innerRows := make([]QueryResult, 120)
		for i := range innerRows {
			key := float64(i)
			if i == 7 {
				key = math.NaN()
			}
			innerRows[i] = ojLegQR(t, legB, key, int64(i*10))
		}
		outerRows := []QueryResult{ojLegQR(t, legA, math.NaN(), int64(50))}
		c := mustNLJCursor(t, recordlayer.FromList(outerRows), innerRows, plans.JoinInner,
			values.NamedCorrelationIdentifier("A"), values.NamedCorrelationIdentifier("B"), []predicates.QueryPredicate{pred}, seed, EmptyEvaluationContext(), nil)
		defer c.Close()
		if c.hashIndex != nil {
			t.Fatal("an inner NaN key must DECLINE the hash index")
		}
		results := collectCursor(t, c)
		if len(results) != 1 {
			t.Fatalf("got %d rows, want 1 (NaN outer equals only the NaN inner key)", len(results))
		}
	})
}
