package cascades

// The ordering keys a match candidate mints, and the equality that decides
// whether two of them are the same column.
//
// These two facts are ONE change and cannot be separated. A candidate whose
// ordering key states an ordinal is comparable by identity — and the moment it
// does, `ValuesStructurallyEqual` becomes actively wrong at the ordering match:
// its FieldPath.Equals is ordinal-only and does NOT compare the layout the
// ordinal indexes, so ordinal 0 of one row and ordinal 0 of another are "the
// same column". While the candidate side stayed a bare display name, that arm
// was unreachable (baked-vs-lazy is unequal by contract). Baking the candidate
// arms it.

import (
	"testing"

	"fdb.dev/gen"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/properties"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// orderingIdentityRecordRow is the ORDERS row layout both candidates below
// range over. CUSTOMER_ID deliberately does NOT sit at ordinal 0: an ordinal
// conflation between this row and a narrower one is only visible when the
// ordinals disagree, and a layout where every interesting column is at 0 cannot
// express the bug.
func orderingIdentityRecordRow() *values.RecordType {
	return values.NewRecordType("", false, []values.Field{
		{Name: "ID", FieldType: values.NullableLong, Ordinal: 0},
		{Name: "CUSTOMER_ID", FieldType: values.NullableLong, Ordinal: 1},
		{Name: "STATUS", FieldType: values.NullableString, Ordinal: 2},
		{Name: "AMOUNT", FieldType: values.NullableLong, Ordinal: 3},
	})
}

func orderingIdentityKeyExpression(cols ...string) *gen.KeyExpression {
	children := make([]*gen.KeyExpression, len(cols))
	for i, c := range cols {
		children[i] = candidateTestKeyField(c, gen.Field_SCALAR)
	}
	return &gen.KeyExpression{Then: &gen.Then{Child: children}}
}

// orderingIdentityCandidate builds an index candidate over the ORDERS row with
// the given key columns and ID as the primary key.
func orderingIdentityCandidate(name string, cols []string, rowType values.Type) (
	*ValueIndexScanMatchCandidate, []values.CorrelationIdentifier,
) {
	aliases := make([]values.CorrelationIdentifier, len(cols))
	for i := range cols {
		aliases[i] = values.UniqueCorrelationIdentifier()
	}
	noDuplicates := false
	candidate := NewValueIndexScanMatchCandidateWithFunctions(
		name,
		[]string{"ORDERS"},
		cols,
		nil,
		aliases,
		rowType,
		false,
		[]string{"ID"},
		&noDuplicates,
	).WithRootKeyExpression(orderingIdentityKeyExpression(cols...))
	return candidate, aliases
}

func orderingIdentityEmptyMatchInfo() MatchInfo {
	return NewRegularMatchInfo(
		map[values.CorrelationIdentifier]*predicates.ComparisonRange{},
		nil, nil, nil, nil, nil, nil, nil,
	)
}

// TestOrderingValuesEqualRefusesEqualOrdinalsInDifferentLayouts is the
// wrong-elision net.
//
// The pair is the one measured live on the corpus: a requested ORDER BY key
// addressing CUSTOMER_ID at slot 0 of an aggregate's OUTPUT row
// ([CUSTOMER_ID, COUNT(*)]), and a match candidate's key addressing ID at slot 0
// of the ORDERS RECORD row. Two different columns of two different rows. Both
// state an identity, and the identities disagree on the DOMAIN — the one element
// FieldPath.Equals does not compare.
//
// If this ever answers "equal", the access-path match reports a scan ordered by
// ID as satisfying a request for CUSTOMER_ID order, the enforcer sort is elided,
// and the query returns correctly-computed rows in the wrong order.
func TestOrderingValuesEqualRefusesEqualOrdinalsInDifferentLayouts(t *testing.T) {
	t.Parallel()

	recordDomain := values.OrdinalDomainOfType(orderingIdentityRecordRow())
	aggregateDomain := values.OrdinalDomainOfColumnNames(
		[]string{"CUSTOMER_ID", "COUNT(*)"})
	if !recordDomain.IsKnown() || !aggregateDomain.IsKnown() {
		t.Fatal("test setup: both layouts must be nameable")
	}
	if recordDomain == aggregateDomain {
		t.Fatal("test setup: the two layouts must be DIFFERENT, or there is no " +
			"conflation to refuse")
	}

	requested := values.NewFieldValueWithResolvedOrdinalInDomain(
		"CUSTOMER_ID", 0, values.UnknownType, aggregateDomain)
	candidate := values.NewFieldValueWithResolvedOrdinalInDomain(
		"ID", 0, values.UnknownType, recordDomain)

	if !values.ValuesStructurallyEqual(requested, candidate) {
		t.Fatal("test setup: ValuesStructurallyEqual no longer equates these " +
			"two, so the arm this test guards is gone. Good news — but check " +
			"that FieldPath.Equals now compares the domain, and if it does, " +
			"this test should assert THAT instead.")
	}

	if orderingValuesEqual(requested, candidate) {
		t.Fatalf("orderingValuesEqual(%q in %v, %q in %v) = true.\n\n"+
			"These are DIFFERENT columns of DIFFERENT rows that happen to share "+
			"an ordinal. Answering yes elides a sort against an ordering the "+
			"scan does not provide: measured live on "+
			"multi_column_index_java.yaml#5 the moment the candidate side "+
			"started stating its ordinal.\n\n"+
			"The cause is always the same: something let the decision fall "+
			"through to ValuesStructurallyEqual, whose FieldPath.Equals "+
			"compares ordinals and NOT the layout they index. Where both sides "+
			"state an identity, the identity is the decision and there is no "+
			"fallback.",
			values.ExplainValue(requested), aggregateDomain,
			values.ExplainValue(candidate), recordDomain)
	}

	if !orderingValuesEqual(candidate, candidate) {
		t.Fatal("orderingValuesEqual is not reflexive on a stated identity — " +
			"the identity arm refuses a value against itself, which would make " +
			"every access-path ordering match fail")
	}

	// The same-column direction, so the test cannot pass by refusing everything.
	sameColumn := values.NewFieldValueWithResolvedOrdinalInDomain(
		"CUSTOMER_ID", 1, values.UnknownType, recordDomain)
	otherSideSameColumn := values.NewFieldValueWithResolvedOrdinalInDomain(
		"CUSTOMER_ID", 1, values.UnknownType, recordDomain)
	if !orderingValuesEqual(sameColumn, otherSideSameColumn) {
		t.Fatal("orderingValuesEqual refuses two independently built references " +
			"to the SAME ordinal of the SAME layout. That is the common path; " +
			"refusing it costs every elision the identity was introduced to win")
	}
}

// TestIntersectionOrderingKeysShareTheCommonRecordLayout is DECISION 3's net.
//
// A primary-key intersection's premise is exactly ONE common record type, and
// its comparison keys exist to be compared ACROSS legs. So the two legs' keys
// must be domained in the RECORD's row layout — the same token on both sides —
// and NOT in anything per-index. Domain them per index and every leg gets a
// different token for the same primary-key column: the merged ordering comes
// back EMPTY and the merge intersection stops being planned at all.
//
// Both legs here are built over the same record row with DIFFERENT index key
// columns, which is what makes the shared token a claim about the record and
// not an accident of two identical candidates.
func TestIntersectionOrderingKeysShareTheCommonRecordLayout(t *testing.T) {
	t.Parallel()

	row := orderingIdentityRecordRow()
	recordDomain := values.OrdinalDomainOfType(row)

	statusCandidate, statusAliases := orderingIdentityCandidate(
		"IDX_STATUS", []string{"STATUS"}, row)
	amountCandidate, amountAliases := orderingIdentityCandidate(
		"IDX_AMOUNT", []string{"AMOUNT"}, row)

	statusParts := statusCandidate.ComputeMatchedOrderingParts(
		orderingIdentityEmptyMatchInfo(), statusAliases, false)
	amountParts := amountCandidate.ComputeMatchedOrderingParts(
		orderingIdentityEmptyMatchInfo(), amountAliases, false)

	// One key column plus the trimmed primary-key suffix (ID) on each side.
	if len(statusParts) != 2 || len(amountParts) != 2 {
		t.Fatalf("ordering parts = %d and %d, want 2 each (key column + PK "+
			"suffix); the PK suffix is the key both legs must agree on",
			len(statusParts), len(amountParts))
	}

	statusPK := statusParts[1].GetValue()
	amountPK := amountParts[1].GetValue()

	statusIdent, statusOK := values.OrderingIdentityOf(statusPK)
	amountIdent, amountOK := values.OrderingIdentityOf(amountPK)
	if !statusOK || !amountOK {
		t.Fatalf("the legs' primary-key comparison keys state no identity "+
			"(%q ok=%v, %q ok=%v).\n\n"+
			"An unaddressable comparison key cannot be matched across legs at "+
			"all: the merged ordering is empty and the merge intersection is "+
			"never enumerated.",
			values.ExplainValue(statusPK), statusOK,
			values.ExplainValue(amountPK), amountOK)
	}

	if statusIdent.Domain != recordDomain {
		t.Fatalf("leg IDX_STATUS domains its PK comparison key in %v, want the "+
			"COMMON RECORD layout %v.\n\n"+
			"A per-index domain is the failure DECISION 3 exists to prevent: "+
			"the keys exist for cross-leg comparison, so two legs holding two "+
			"different tokens for the same primary-key column collapse the "+
			"merge.", statusIdent.Domain, recordDomain)
	}
	if statusIdent != amountIdent {
		t.Fatalf("the two legs state DIFFERENT identities for the same "+
			"primary-key column: %+v vs %+v.\n\n"+
			"MergeOrderingsForIntersection can only intersect keys it can "+
			"equate; disagreeing tokens yield an empty merged ordering and no "+
			"intersection plan.", statusIdent, amountIdent)
	}

	// And the end-to-end consequence, so the test fails on the OUTCOME and not
	// only on the token: the two legs' orderings must actually intersect.
	statusOrdering := properties.NewRichOrdering(
		map[values.Value][]properties.OrderingBinding{
			statusPK: {properties.SortedBinding(properties.ProvidedSortOrderAscending)},
		},
		[]values.Value{statusPK}, true)
	amountOrdering := properties.NewRichOrdering(
		map[values.Value][]properties.OrderingBinding{
			amountPK: {properties.SortedBinding(properties.ProvidedSortOrderAscending)},
		},
		[]values.Value{amountPK}, true)
	merged := properties.MergeOrderingsForIntersection(statusOrdering, amountOrdering)
	if len(merged.GetKeys()) != 1 {
		t.Fatalf("merging the two legs' orderings yielded %d keys, want 1.\n\n"+
			"Both legs are ordered by the SAME primary-key column of the SAME "+
			"record; an empty merge means the two keys did not meet, which is "+
			"exactly how a per-index domain manifests.", len(merged.GetKeys()))
	}
}

// TestPrimaryScanOrderingKeyMeetsAnIndexScanLeg pins the primary-scan
// candidate's mint against the index candidate's.
//
// A primary scan is an ordinary intersection leg — `WHERE pk > ? AND status = ?`
// over idx(status) and the primary key — so its comparison key meets an index
// leg's in one merged ordering. The two candidates are different types with
// different metadata lists, and nothing but a shared authority makes their keys
// agree. Leave the primary-scan mint a bare display name and the merge has one
// baked and one lazy key: unequal by contract, merged ordering empty.
func TestPrimaryScanOrderingKeyMeetsAnIndexScanLeg(t *testing.T) {
	t.Parallel()

	row := orderingIdentityRecordRow()
	indexCandidate, indexAliases := orderingIdentityCandidate(
		"IDX_STATUS", []string{"STATUS"}, row)
	indexParts := indexCandidate.ComputeMatchedOrderingParts(
		orderingIdentityEmptyMatchInfo(), indexAliases, false)
	if len(indexParts) != 2 {
		t.Fatalf("index ordering parts = %d, want 2", len(indexParts))
	}
	indexPK := indexParts[1].GetValue()

	pkAlias := values.UniqueCorrelationIdentifier()
	primary := NewPrimaryScanMatchCandidate(
		nil,
		[]values.CorrelationIdentifier{pkAlias},
		[]string{"ORDERS"},
		[]string{"ORDERS"},
		[]string{"ID"},
		false,
		row,
	)
	primaryParts := primary.ComputeMatchedOrderingParts(
		orderingIdentityEmptyMatchInfo(),
		[]values.CorrelationIdentifier{pkAlias},
		false,
	)
	if len(primaryParts) != 1 {
		t.Fatalf("primary-scan ordering parts = %d, want 1", len(primaryParts))
	}
	primaryPK := primaryParts[0].GetValue()

	indexIdent, indexOK := values.OrderingIdentityOf(indexPK)
	primaryIdent, primaryOK := values.OrderingIdentityOf(primaryPK)
	if !indexOK || !primaryOK {
		t.Fatalf("a leg's primary-key ordering key states no identity "+
			"(index %q ok=%v, primary %q ok=%v). An unaddressable key cannot "+
			"meet the other leg's.",
			values.ExplainValue(indexPK), indexOK,
			values.ExplainValue(primaryPK), primaryOK)
	}
	if indexIdent != primaryIdent {
		t.Fatalf("the primary scan and the index scan state DIFFERENT "+
			"identities for ORDERS' primary key: %+v vs %+v.\n\n"+
			"Both flow the same record row and both resolve the same column "+
			"name against it; disagreeing here means one of them stopped using "+
			"that row as its authority, and an intersection mixing the two legs "+
			"loses its merged ordering.", indexIdent, primaryIdent)
	}
}

// TestOrderingKeyStaysLazyWhenTheLayoutIsAmbiguous pins the fail-closed
// direction.
//
// A multi-record-type index has no single row whose ordinals mean anything.
// Picking layouts[0] would stamp one record type's ordinals onto another's rows
// — an ordinal that reads as authoritative and addresses the wrong column, which
// is strictly worse than the display name it replaces. The key must stay
// unaddressable instead, costing an elision.
func TestOrderingKeyStaysLazyWhenTheLayoutIsAmbiguous(t *testing.T) {
	t.Parallel()

	firstRow := values.NewRecordType("", false, []values.Field{
		{Name: "ID", FieldType: values.NullableLong, Ordinal: 0},
		{Name: "STATUS", FieldType: values.NullableString, Ordinal: 1},
	})
	// The SAME column names in a DIFFERENT order, so a layouts[0] pick is a
	// silent wrong-slot read rather than a visible failure.
	secondRow := values.NewRecordType("", false, []values.Field{
		{Name: "STATUS", FieldType: values.NullableString, Ordinal: 0},
		{Name: "ID", FieldType: values.NullableLong, Ordinal: 1},
	})

	candidate, aliases := orderingIdentityCandidate(
		"IDX_MULTI", []string{"STATUS"}, values.UnknownType)
	candidate.WithRecordTypeRowTypes([]values.Type{firstRow, secondRow})

	parts := candidate.ComputeMatchedOrderingParts(
		orderingIdentityEmptyMatchInfo(), aliases, false)
	if len(parts) == 0 {
		t.Fatal("test setup: the candidate minted no ordering parts, so the " +
			"fail-closed claim is untested")
	}
	for i, part := range parts {
		if _, ok := values.OrderingIdentityOf(part.GetValue()); ok {
			t.Fatalf("ordering part %d (%q) states an identity over a "+
				"candidate with %d candidate row layouts.\n\n"+
				"There is no layout to state: the two rows here declare the "+
				"same names in opposite positions, so an ordinal resolved "+
				"against either one addresses the WRONG column of the other. "+
				"The key must stay unaddressable.",
				i, values.ExplainValue(part.GetValue()),
				len(candidate.rowLayouts()))
		}
	}
}

// TestOrderingKeyDeclinesADuplicateColumnName pins UNIQUE-match resolution.
//
// The candidate's column list drops nesting parents, so the name reaching the
// mint is a bare leaf. First-matching a leaf against a layout that declares the
// name twice silently bakes the FIRST slot — and the runtime authority this key
// is verified against (bakedIntersectionKeys) requires a unique match, so a
// first-match bake would also make the two disagree.
func TestOrderingKeyDeclinesADuplicateColumnName(t *testing.T) {
	t.Parallel()

	// The duplicate is a CASE difference, which is the shape that actually
	// reaches here: NewRecordType rejects a byte-identical repeat outright, but
	// its check is case-SENSITIVE while column resolution is case-INSENSITIVE
	// (SQL convention, and the fold OrdinalDomain's signature applies). So this
	// layout is constructible and declares "status" twice as far as any resolver
	// is concerned.
	duplicated := values.NewRecordType("", false, []values.Field{
		{Name: "ID", FieldType: values.NullableLong, Ordinal: 0},
		{Name: "STATUS", FieldType: values.NullableString, Ordinal: 1},
		{Name: "status", FieldType: values.NullableString, Ordinal: 2},
	})
	candidate, aliases := orderingIdentityCandidate(
		"IDX_DUP", []string{"STATUS"}, duplicated)

	parts := candidate.ComputeMatchedOrderingParts(
		orderingIdentityEmptyMatchInfo(), aliases, false)
	if len(parts) == 0 {
		t.Fatal("test setup: no ordering parts minted")
	}
	if ident, ok := values.OrderingIdentityOf(parts[0].GetValue()); ok {
		t.Fatalf("STATUS resolved to ordinal %d against a layout declaring "+
			"STATUS TWICE.\n\n"+
			"A first-match bake picks a slot the name does not identify. The "+
			"runtime authority (bakedIntersectionKeys) resolves the same name "+
			"with a UNIQUE-match rule and would reject this ordinal, so a "+
			"first-match mint also breaks the agreement between plan time and "+
			"run time.", ident.Ordinal)
	}
}
