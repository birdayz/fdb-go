package cascades

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
)

// buildFilteredDistinctOverT creates
//
//	Distinct(Projection([projected], Filter(preds, Scan(T))))
//
// with a physical FinalMember in the projection Reference — the same shape
// buildDistinctOverProjection produces, plus the filter whose conjuncts are the
// only thing R2 reads.
func buildFilteredDistinctOverT(
	t testing.TB,
	projected []values.Value, preds []predicates.QueryPredicate,
) *expressions.Reference {
	t.Helper()
	scan, scanErr := expressions.NewFullUnorderedScanExpression([]string{"T"}, distinctScanType("T"))
	scan = mustConstruct(t, scan, scanErr)
	scanQ := expressions.ForEachQuantifier(expressions.InitialOf(scan))

	filter, filterErr := expressions.NewLogicalFilterExpression(preds, scanQ)
	filter = mustConstruct(t, filter, filterErr)
	filterQ := expressions.ForEachQuantifier(expressions.InitialOf(filter))

	proj, projErr := expressions.NewLogicalProjectionExpression(projected, filterQ)
	proj = mustConstruct(t, proj, projErr)
	projRef := expressions.InitialOf(proj)
	// A physical member of a projection group must flow the projection's exact
	// row, not the wider base scan row. Memo admission rejects mixed result
	// types before the distinct proof can inspect this group.
	projRef.Insert(makeFakePlanWrapperForType("T", proj.GetResultValue().Type(), false))

	distinct, distinctErr := expressions.NewLogicalDistinctExpression(
		expressions.ForEachQuantifier(projRef))
	distinct = mustConstruct(t, distinct, distinctErr)
	return expressions.InitialOf(distinct)
}

func nullRejectingCmp(col string, t predicates.ComparisonType) predicates.QueryPredicate {
	return predicates.NewComparisonPredicate(
		distinctRead("T", col), predicates.NewLiteralComparison(t, "x"))
}

// fireFilteredDistinct fires the rule for
// `SELECT DISTINCT <projected...> FROM T WHERE <preds>` with index-state
// evidence established, and reports whether a physical distinct survived and
// which index (if any) stamped the elided plan.
func fireFilteredDistinct(
	t *testing.T, projected []string, preds []predicates.QueryPredicate,
) (retained bool, stampedBy string, fired bool) {
	t.Helper()
	return fireFilteredDistinctOver(t, secondaryUniqueTestCandidates(), projected, preds)
}

func fireFilteredDistinctOver(
	t *testing.T, candidates []MatchCandidate, projected []string,
	preds []predicates.QueryPredicate,
) (retained bool, stampedBy string, fired bool) {
	t.Helper()
	cols := make([]values.Value, len(projected))
	for i, c := range projected {
		cols[i] = distinctRead("T", c)
	}
	ctx := &indexTestPlanContext{
		candidates:      candidates,
		readableIndexes: AllIndexesReadable(),
	}
	results := mustFireImplementationRuleWithContext(t,
		NewImplementDistinctFinalRule(),
		buildFilteredDistinctOverT(t, cols, preds), ctx, nil)
	for _, result := range results {
		fired = true
		if _, ok := result.(*plans.RecordQueryDistinctPlan); ok {
			retained = true
			continue
		}
		if stamped, ok := result.(plans.DistinctProofStamped); ok {
			if name := stamped.GetDistinctProofIndexName(); name != "" {
				stampedBy = name
			}
		}
	}
	return retained, stampedBy, fired
}

// TestDistinctFinal_R2NullRejectingPredicateAdmitsNullableUniqueIndex is the
// pin on R2: a UNIQUE index over a NULLABLE column proves nothing in general,
// and proves a DISTINCT redundant on a stream from which the NULL rows have
// already been filtered out.
//
// This is the arm that makes the whole proof reachable from SQL. The SQL DDL
// rejects a NOT NULL scalar column, so the metadata route (R1) is false for
// EVERY SQL-expressible unique index — correct, and inert. R2 is what a user
// gets by writing the WHERE clause they would write anyway.
//
// Each arm below is a direction in which R2 can be wrong, and every one of them
// emits DUPLICATE ROWS rather than a missed optimization.
func TestDistinctFinal_R2NullRejectingPredicateAdmitsNullableUniqueIndex(t *testing.T) {
	t.Parallel()

	// The positive: IS NOT NULL on the index's only key column.
	retained, stampedBy, fired := fireFilteredDistinct(t,
		[]string{"NULLABLE_EMAIL"},
		[]predicates.QueryPredicate{predicates.NewComparisonPredicate(
			distinctRead("T", "NULLABLE_EMAIL"),
			predicates.Comparison{Type: predicates.ComparisonIsNotNull})})
	if !fired {
		t.Fatal("the rule did not fire at all, so nothing below observed anything")
	}
	if retained {
		t.Fatal("DISTINCT(NULLABLE_EMAIL) WHERE NULLABLE_EMAIL IS NOT NULL was " +
			"retained. Under NULLS DISTINCT the index's exempt set is exactly the " +
			"NULL entries, and the filter has emptied it on this stream — the " +
			"uniqueness declaration is a true invariant over every row that reaches " +
			"the operator, so the operator removes nothing")
	}
	if stampedBy != "T$nullable_email_unique" {
		t.Fatalf("the R2-elided plan records its proving index as %q, want "+
			"\"T$nullable_email_unique\". R2 narrows WHICH STREAM the uniqueness "+
			"holds on; it does not make the index's state stop mattering, so the "+
			"dependency is recorded exactly as under R1", stampedBy)
	}

	// Every comparison kind on the allow-list admits the column. A kind that
	// silently stopped rejecting NULL would cost an optimization; one that
	// wrongly started would emit duplicates, which is why the refusals below are
	// the same test written the other way round.
	for name, cmpType := range map[string]predicates.ComparisonType{
		"Equals":          predicates.ComparisonEquals,
		"NotEquals":       predicates.ComparisonNotEquals,
		"LessThan":        predicates.ComparisonLessThan,
		"LessThanOrEq":    predicates.ComparisonLessThanOrEq,
		"GreaterThan":     predicates.ComparisonGreaterThan,
		"GreaterThanOrEq": predicates.ComparisonGreaterThanEq,
		"StartsWith":      predicates.ComparisonStartsWith,
		"Like":            predicates.ComparisonLike,
		"In":              predicates.ComparisonIn,
	} {
		retained, _, fired := fireFilteredDistinct(t, []string{"NULLABLE_EMAIL"},
			[]predicates.QueryPredicate{nullRejectingCmp("NULLABLE_EMAIL", cmpType)})
		if !fired {
			t.Fatalf("%s: rule did not fire", name)
		}
		if retained {
			t.Fatalf("%s on the key column did not admit the nullable unique index. "+
				"NULL compares UNKNOWN under every one of these, so no NULL row "+
				"reaches the DISTINCT and the index's exempt set is empty here", name)
		}
	}
}

// TestDistinctFinal_R2RefusesNullAdmittingConjuncts is R2's fail-closed half,
// and NotDistinctFrom is the reason it exists as its own test.
//
// canBeUsedInScanPrefix (range_constraints.go:161-173) lists NotDistinctFrom
// beside Equals, because for the SCAN-PREFIX question the two behave alike. For
// the NULL-REJECTION question they are OPPOSITES: NotDistinctFrom is NULL-safe
// equality, so `x IS NOT DISTINCT FROM NULL` is TRUE for a NULL x and the row
// survives into the DISTINCT. An implementation that reuses the scan-prefix list
// as R2's allow-list is wrong in exactly that one entry, silently, on the shape
// most likely to be written by someone who knows three-valued semantics.
func TestDistinctFinal_R2RefusesNullAdmittingConjuncts(t *testing.T) {
	t.Parallel()

	for name, cmpType := range map[string]predicates.ComparisonType{
		"NotDistinctFrom — NULL-safe equality, TRUE for a NULL subject": predicates.ComparisonNotDistinctFrom,
		"IsDistinctFrom — NULL-safe, NULL IS DISTINCT FROM 1 is TRUE":   predicates.ComparisonIsDistinctFrom,
		"IsNull — admits ONLY NULL, the exact inverse":                  predicates.ComparisonIsNull,
	} {
		retained, _, fired := fireFilteredDistinct(t, []string{"NULLABLE_EMAIL"},
			[]predicates.QueryPredicate{nullRejectingCmp("NULLABLE_EMAIL", cmpType)})
		if !fired {
			t.Fatalf("%s: rule did not fire", name)
		}
		if !retained {
			t.Fatalf("DISTINCT was elided on a stream a %s conjunct does NOT clear "+
				"of NULLs. The index legitimately holds (NULL),(NULL),(NULL) and "+
				"they all reach the operator, so eliding emits three rows where "+
				"DISTINCT emits one", name)
		}
	}

	// Under OR: the other disjunct admits everything the NULL-rejecting one
	// excluded, so the conjunct proves nothing. Under NOT: the admission is
	// inverted. Neither is descended into.
	notNull := predicates.NewComparisonPredicate(
		distinctRead("T", "NULLABLE_EMAIL"),
		predicates.Comparison{Type: predicates.ComparisonIsNotNull})
	for name, pred := range map[string]predicates.QueryPredicate{
		"under OR":  predicates.NewOr(notNull, nullRejectingCmp("EMAIL", predicates.ComparisonEquals)),
		"under NOT": predicates.NewNot(notNull),
	} {
		retained, _, fired := fireFilteredDistinct(t,
			[]string{"NULLABLE_EMAIL"}, []predicates.QueryPredicate{pred})
		if !fired {
			t.Fatalf("%s: rule did not fire", name)
		}
		if !retained {
			t.Fatalf("a NULL-rejecting comparison %s was credited as clearing the "+
				"stream of NULLs. Only TOP-LEVEL conjuncts prove anything", name)
		}
	}

	// The conjunct must name THIS key column. A rejection on a different column
	// says nothing about the index's key.
	retained, _, fired := fireFilteredDistinct(t, []string{"NULLABLE_EMAIL"},
		[]predicates.QueryPredicate{predicates.NewComparisonPredicate(
			distinctRead("T", "EMAIL"),
			predicates.Comparison{Type: predicates.ComparisonIsNotNull})})
	if !fired {
		t.Fatal("rule did not fire")
	}
	if !retained {
		t.Fatal("a NULL rejection on a DIFFERENT column was credited as covering " +
			"NULLABLE_EMAIL's key. R2's coverage is per key column, resolved by " +
			"ordinal in the scan row's layout, never by whichever conjunct happened " +
			"to be present")
	}

	// The FLOAT/DOUBLE half of the exempt set is NOT overridable. `d IS NOT
	// NULL` admits every NaN encoding, so a raw-NaN pair the index holds as two
	// distinct tuple keys is still one logical value to the comparator.
	retained, _, fired = fireFilteredDistinct(t, []string{"DBL"},
		[]predicates.QueryPredicate{predicates.NewComparisonPredicate(
			distinctRead("T", "DBL"),
			predicates.Comparison{Type: predicates.ComparisonIsNotNull})})
	if !fired {
		t.Fatal("rule did not fire")
	}
	if !retained {
		t.Fatal("a NULL-rejecting conjunct was credited as clearing the RAW-NaN " +
			"half of the exempt set. It clears nothing there: IS NOT NULL admits " +
			"every NaN, and storage identity stays finer than logical equality")
	}
}

// nullableCompositeCandidate is a UNIQUE (NULLABLE_EMAIL, SPARSE_EMAIL) whose
// BOTH key components are nullable, so the metadata route refuses it outright
// and every admission it ever gets comes from R2. It is deliberately the ONLY
// candidate its test sees: the shared fixture also holds a single-column
// nullable unique index on NULLABLE_EMAIL, which a projection of both columns
// covers and a NULL rejection on that one column licenses — correctly, and it
// would elide the DISTINCT before the composite arm observed anything.
func nullableCompositeCandidate() []MatchCandidate {
	scalar := false
	return []MatchCandidate{NewValueIndexScanMatchCandidateWithFunctions(
		"T$nullable_pair_unique",
		[]string{"T"},
		[]string{"NULLABLE_EMAIL", "SPARSE_EMAIL"},
		nil,
		[]values.CorrelationIdentifier{
			values.UniqueCorrelationIdentifier(),
			values.UniqueCorrelationIdentifier(),
		},
		distinctScanType("T"),
		true,
		nil,
		&scalar,
	).WithKeyComponentTypes([]values.Type{values.NullableString, values.NullableString})}
}

// TestDistinctFinal_R2NeedsAConjunctPerKeyColumn pins that coverage is per key
// column and not per filter. A composite UNIQUE (a, b) constrains the PAIR, so
// clearing NULLs from `a` alone still leaves (1, NULL), (1, NULL) — two entries
// the index holds legitimately and one row DISTINCT must return.
//
// Both key columns are projected throughout, or clause 6 would refuse the
// candidate first and the test would pass having observed nothing.
func TestDistinctFinal_R2NeedsAConjunctPerKeyColumn(t *testing.T) {
	t.Parallel()

	notNullOn := func(col string) predicates.QueryPredicate {
		return predicates.NewComparisonPredicate(distinctRead("T", col),
			predicates.Comparison{Type: predicates.ComparisonIsNotNull})
	}
	projected := []string{"NULLABLE_EMAIL", "SPARSE_EMAIL"}

	// Partial coverage: the first key column is cleared of NULLs, the second is
	// not. The pair is still not unique on this stream.
	retained, _, fired := fireFilteredDistinctOver(t, nullableCompositeCandidate(),
		projected, []predicates.QueryPredicate{notNullOn("NULLABLE_EMAIL")})
	if !fired {
		t.Fatal("rule did not fire")
	}
	if !retained {
		t.Fatal("a composite UNIQUE was admitted with a NULL rejection on its FIRST " +
			"key column alone. The second column's NULLs still reach the operator, " +
			"and the index holds ('a', NULL), ('a', NULL) as two entries the " +
			"DISTINCT must collapse to one row")
	}

	// The other half of the same claim: covering only the SECOND column must
	// fail too, or the positional mapping is crediting whichever conjunct it
	// found rather than the column it names.
	retained, _, fired = fireFilteredDistinctOver(t, nullableCompositeCandidate(),
		projected, []predicates.QueryPredicate{notNullOn("SPARSE_EMAIL")})
	if !fired {
		t.Fatal("rule did not fire")
	}
	if !retained {
		t.Fatal("a composite UNIQUE was admitted with a NULL rejection on its SECOND " +
			"key column alone")
	}

	// Full coverage: a conjunct per key column admits it. Without this arm the
	// two refusals above would pass with R2 inert on composites entirely.
	retained, stampedBy, fired := fireFilteredDistinctOver(t, nullableCompositeCandidate(),
		projected, []predicates.QueryPredicate{
			notNullOn("NULLABLE_EMAIL"), notNullOn("SPARSE_EMAIL"),
		})
	if !fired {
		t.Fatal("rule did not fire")
	}
	if retained {
		t.Fatal("a composite UNIQUE with a NULL-rejecting conjunct on EVERY key " +
			"column was refused. The pair is a true uniqueness invariant over every " +
			"row that reaches the operator")
	}
	if stampedBy != "T$nullable_pair_unique" {
		t.Fatalf("the composite R2 elision records its proving index as %q, want "+
			"\"T$nullable_pair_unique\"", stampedBy)
	}
}

// TestDistinctFinal_R2DoesNotFireWithoutAFilter is the over-admission pin: with
// clause 8 relaxed per-column, the danger is that the relaxation leaks to
// columns nothing rejected. The unfiltered query must stay exactly as strict as
// it was under R1 alone.
func TestDistinctFinal_R2DoesNotFireWithoutAFilter(t *testing.T) {
	t.Parallel()

	for name, preds := range map[string][]predicates.QueryPredicate{
		"no filter node at all":    nil,
		"filter with no conjuncts": {},
	} {
		retained, _, fired := fireFilteredDistinct(t, []string{"NULLABLE_EMAIL"}, preds)
		if !fired {
			t.Fatalf("%s: rule did not fire", name)
		}
		if !retained {
			t.Fatalf("%s: DISTINCT over a NULLABLE unique index was elided with "+
				"NOTHING rejecting NULL. The index holds (NULL),(NULL),(NULL) and "+
				"the operator is what collapses them", name)
		}
	}
}

// TestNullRejectingComparisonIsAClosedEnumeration pins the allow-list itself,
// separately from the rule, so a comparison kind added later cannot quietly
// acquire a NULL-rejection claim nobody reasoned about. The list is CLOSED: an
// unlisted kind rejects nothing, which costs an optimization rather than
// emitting duplicate rows.
func TestNullRejectingComparisonIsAClosedEnumeration(t *testing.T) {
	t.Parallel()

	admitted := map[predicates.ComparisonType]bool{
		predicates.ComparisonIsNotNull:     true,
		predicates.ComparisonEquals:        true,
		predicates.ComparisonNotEquals:     true,
		predicates.ComparisonLessThan:      true,
		predicates.ComparisonLessThanOrEq:  true,
		predicates.ComparisonGreaterThan:   true,
		predicates.ComparisonGreaterThanEq: true,
		predicates.ComparisonStartsWith:    true,
		predicates.ComparisonLike:          true,
		predicates.ComparisonIn:            true,
	}
	// Every constant in the enum, walked by ordinal so a newly added kind lands
	// here as "not admitted" and this test says so if it was expected to be.
	for cmpType := predicates.ComparisonEquals; cmpType <= predicates.ComparisonDistanceRankLessThanOrEq; cmpType++ {
		got := nullRejectingComparison(cmpType)
		if got != admitted[cmpType] {
			t.Fatalf("nullRejectingComparison(%s) = %v, want %v. R2's allow-list is "+
				"its own closed enumeration and is deliberately NOT "+
				"canBeUsedInScanPrefix — the two differ on NotDistinctFrom, which "+
				"bounds a scan prefix and admits NULL", cmpType.Symbol(), got, admitted[cmpType])
		}
	}
	if nullRejectingComparison(predicates.ComparisonNotDistinctFrom) {
		t.Fatal("NotDistinctFrom rejects NULL. It does not: it is NULL-SAFE " +
			"equality, so `x IS NOT DISTINCT FROM NULL` is TRUE for a NULL x")
	}
}
