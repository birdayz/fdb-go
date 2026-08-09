package executor

import (
	"bytes"
	"errors"
	"math"
	"testing"

	"fdb.dev/pkg/fdbgo/fdb/tuple"
	"fdb.dev/pkg/recordlayer"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
)

func assertScanIdentitiesDistinct(t *testing.T, base string, variants map[string]string) {
	t.Helper()
	if len(base) != sha256DigestSize {
		t.Fatalf("identity length = %d, want SHA-256 length %d", len(base), sha256DigestSize)
	}
	seen := map[string]string{base: "base"}
	for name, identity := range variants {
		if identity == base {
			t.Errorf("%s identity equals base", name)
		}
		if previous, ok := seen[identity]; ok {
			t.Errorf("%s identity aliases %s", name, previous)
		} else {
			seen[identity] = name
		}
	}
}

const sha256DigestSize = 32

func mustScanIdentity(t testing.TB, identity string, err error) string {
	t.Helper()
	if err != nil {
		t.Fatalf("build scan identity: %v", err)
	}
	return identity
}

func mustEncodeScanIdentityType(t testing.TB, typ values.Type) []byte {
	t.Helper()
	encoded, err := encodeScanIdentityType(typ)
	if err != nil {
		t.Fatalf("encode scan identity type: %v", err)
	}
	return encoded
}

func mustPrimaryScanIdentity(t testing.TB, plan *plans.RecordQueryScanPlan) string {
	t.Helper()
	identity, err := primaryScanRangeFingerprintSalt(plan)
	return mustScanIdentity(t, identity, err)
}

func mustIndexScanIdentity(
	t testing.TB,
	plan *plans.RecordQueryIndexPlan,
	scanType recordlayer.IndexScanType,
) string {
	t.Helper()
	identity, err := indexScanRangeFingerprintSalt(plan, scanType)
	return mustScanIdentity(t, identity, err)
}

func mustAggregateScanIdentity(
	t testing.TB,
	plan *plans.RecordQueryAggregateIndexPlan,
	indexType string,
	scanType recordlayer.IndexScanType,
	physicalGroupingCount int,
	physicalGroupingPrefixCount int,
) string {
	t.Helper()
	identity, err := aggregateScanRangeFingerprintSalt(
		plan,
		indexType,
		scanType,
		physicalGroupingCount,
		physicalGroupingPrefixCount,
	)
	return mustScanIdentity(t, identity, err)
}

func mustVectorScanIdentity(
	t testing.TB,
	plan *plans.RecordQueryVectorIndexPlan,
	indexType string,
	scanType recordlayer.IndexScanType,
	queryVector []float64,
	requestedK int,
	rankCap int,
	effectiveEfSearch int,
	invocationRange recordlayer.TupleRange,
) string {
	t.Helper()
	identity, err := vectorScanRangeFingerprintSalt(
		plan,
		indexType,
		scanType,
		queryVector,
		requestedK,
		rankCap,
		effectiveEfSearch,
		invocationRange,
	)
	return mustScanIdentity(t, identity, err)
}

func primaryIdentityTestPlan(recordTypes []string, reverse bool, keyType, flowedType values.Type) *plans.RecordQueryScanPlan {
	return plans.NewRecordQueryScanPlan(recordTypes, flowedType, reverse).
		WithScanComparisons([]*predicates.ComparisonRange{predicates.EmptyComparisonRange()}).
		WithKeyComponentTypes([]values.Type{keyType})
}

func TestPrimaryScanRangeFingerprintSaltCoversPhysicalExecutionShape(t *testing.T) {
	t.Parallel()
	basePlan := primaryIdentityTestPlan([]string{"ab", "c"}, false, values.NotNullDouble, values.UnknownType)
	base := mustPrimaryScanIdentity(t, basePlan)
	if repeat := mustPrimaryScanIdentity(t, basePlan); repeat != base {
		t.Fatal("primary identity is not deterministic")
	}

	assertScanIdentitiesDistinct(t, base, map[string]string{
		"length-delimited record types": mustPrimaryScanIdentity(t,
			primaryIdentityTestPlan([]string{"a", "bc"}, false, values.NotNullDouble, values.UnknownType)),
		"direction": mustPrimaryScanIdentity(t,
			primaryIdentityTestPlan([]string{"ab", "c"}, true, values.NotNullDouble, values.UnknownType)),
		"physical width": mustPrimaryScanIdentity(t,
			primaryIdentityTestPlan([]string{"ab", "c"}, false, values.NotNullFloat, values.UnknownType)),
		"physical nullability": mustPrimaryScanIdentity(t,
			primaryIdentityTestPlan([]string{"ab", "c"}, false, values.NullableDouble, values.UnknownType)),
		"flowed schema": mustPrimaryScanIdentity(t,
			primaryIdentityTestPlan([]string{"ab", "c"}, false, values.NotNullDouble, values.NullableString)),
	})
}

func indexIdentityTestPlan(
	name string,
	reverse bool,
	coveringColumns []string,
	keyType values.Type,
	recordTypes []string,
	flowedType values.Type,
) *plans.RecordQueryIndexPlan {
	plan := plans.NewRecordQueryIndexPlan(
		name,
		[]*predicates.ComparisonRange{predicates.EmptyComparisonRange()},
		recordTypes,
		flowedType,
		reverse,
	).WithKeyComponentTypes([]values.Type{keyType})
	// Covered columns are DERIVED from the inner scan now (RFC-220), so the
	// column list a covering plan will report is stamped here as the index's key
	// columns rather than handed to a constructor.
	if coveringColumns != nil {
		plan = plan.WithIndexMetadata(coveringColumns, []string{"ID"}, false)
	}
	return plan
}

// mustCoveringIndexScanIdentity is the covering-plan salt. It must reproduce
// indexScanRangeFingerprintSalt's builder prefix and field order exactly, which
// is what TestCoveringIndexScanSaltIsByteIdenticalToTheFlagEncoding pins.
func mustCoveringIndexScanIdentity(
	t testing.TB,
	plan *plans.RecordQueryIndexPlan,
	scanType recordlayer.IndexScanType,
) string {
	t.Helper()
	identity, err := coveringIndexScanRangeFingerprintSalt(
		plans.NewRecordQueryCoveringIndexPlan(plan), scanType)
	return mustScanIdentity(t, identity, err)
}

func TestIndexScanRangeFingerprintSaltCoversScanAndOutputShape(t *testing.T) {
	t.Parallel()
	basePlan := indexIdentityTestPlan("idx", false, []string{"ab", "c"}, values.NotNullDouble, []string{"T"}, values.UnknownType)
	base := mustCoveringIndexScanIdentity(t, basePlan, recordlayer.IndexScanByValue)

	// Every variant below is a COVERING plan differing from the base in exactly
	// one axis, except "fetch instead of covering", which is the same plan read
	// through the NON-covering salt — that is the axis it names.
	assertScanIdentitiesDistinct(t, base, map[string]string{
		"index name": mustCoveringIndexScanIdentity(t,
			indexIdentityTestPlan("idx2", false, []string{"ab", "c"}, values.NotNullDouble, []string{"T"}, values.UnknownType),
			recordlayer.IndexScanByValue),
		"scan kind": mustCoveringIndexScanIdentity(t, basePlan, recordlayer.IndexScanByRank),
		"direction": mustCoveringIndexScanIdentity(t,
			indexIdentityTestPlan("idx", true, []string{"ab", "c"}, values.NotNullDouble, []string{"T"}, values.UnknownType),
			recordlayer.IndexScanByValue),
		"fetch instead of covering": mustIndexScanIdentity(t, basePlan, recordlayer.IndexScanByValue),
		"length-delimited covering columns": mustCoveringIndexScanIdentity(t,
			indexIdentityTestPlan("idx", false, []string{"a", "bc"}, values.NotNullDouble, []string{"T"}, values.UnknownType),
			recordlayer.IndexScanByValue),
		"covering column order": mustCoveringIndexScanIdentity(t,
			indexIdentityTestPlan("idx", false, []string{"c", "ab"}, values.NotNullDouble, []string{"T"}, values.UnknownType),
			recordlayer.IndexScanByValue),
		"primary-key coverage layout": mustCoveringIndexScanIdentity(t,
			indexIdentityTestPlan("idx", false, []string{"ab", "c"}, values.NotNullDouble, []string{"T"}, values.UnknownType).
				WithIndexMetadata([]string{"ab", "c"}, []string{"OTHER_ID"}, false),
			recordlayer.IndexScanByValue),
		"physical schema": mustCoveringIndexScanIdentity(t,
			indexIdentityTestPlan("idx", false, []string{"ab", "c"}, values.NotNullFloat, []string{"T"}, values.UnknownType),
			recordlayer.IndexScanByValue),
		"record type": mustCoveringIndexScanIdentity(t,
			indexIdentityTestPlan("idx", false, []string{"ab", "c"}, values.NotNullDouble, []string{"U"}, values.UnknownType),
			recordlayer.IndexScanByValue),
		"flowed schema": mustCoveringIndexScanIdentity(t,
			indexIdentityTestPlan("idx", false, []string{"ab", "c"}, values.NotNullDouble, []string{"T"}, values.NullableString),
			recordlayer.IndexScanByValue),
	})

	foreignPlan := indexIdentityTestPlan(
		"idx", false, []string{"ab", "c"}, values.NotNullDouble,
		[]string{"T"}, values.UnknownType,
	).WithIndexMetadata([]string{"K"}, []string{"OTHER_ID"}, false)
	foreignSalt := mustIndexScanIdentity(t, foreignPlan, recordlayer.IndexScanByValue)
	comparisons := []*predicates.ComparisonRange{
		scanRangeTestEq(t, float64(0)),
		scanRangeTestEq(t, int64(5)),
	}
	types := []values.Type{values.NotNullDouble, values.NotNullLong}
	baseSpec, err := bindScanComparisonsToRangeSet(comparisons, types, nil, false, base)
	if err != nil {
		t.Fatalf("bind base range set: %v", err)
	}
	foreignSpec, err := bindScanComparisonsToRangeSet(comparisons, types, nil, false, foreignSalt)
	if err != nil {
		t.Fatalf("bind foreign range set: %v", err)
	}
	started := true
	token := marshalRangeSetContinuationForTest(
		t, baseSpec.fingerprint, []uint32{0, 0}, []byte{1}, &started,
	)
	opened := 0
	_, err = newScanRangeSetCursor(
		foreignSpec,
		token,
		recordlayer.ForwardScan(),
		func(recordlayer.TupleRange, []byte, recordlayer.ScanProperties) (recordlayer.RecordCursor[int], error) {
			opened++
			return recordlayer.Empty[int](), nil
		},
	)
	var continuationErr *recordlayer.ContinuationParseError
	if !errors.As(err, &continuationErr) {
		t.Fatalf("foreign PK-layout token error = %T %v, want ContinuationParseError", err, err)
	}
	if opened != 0 {
		t.Fatalf("foreign PK-layout token opened %d storage cursors, want zero", opened)
	}
}

func aggregateIdentityTestPlan(
	indexName string,
	reverse bool,
	physicalPrefix int,
	keyType values.Type,
	groupColumns []string,
	aggregateColumn string,
	aggregateFunction string,
	resultType values.Type,
) *plans.RecordQueryAggregateIndexPlan {
	indexPlan := plans.NewRecordQueryIndexPlan(
		indexName,
		[]*predicates.ComparisonRange{predicates.EmptyComparisonRange()},
		[]string{"T"},
		values.UnknownType,
		reverse,
	).WithKeyComponentTypes([]values.Type{keyType}).
		WithPhysicalGroupingPrefixCount(physicalPrefix)
	return plans.NewRecordQueryAggregateIndexPlan(indexPlan, "T", resultType, aggregateFunction).
		WithGroupColumns(groupColumns, aggregateColumn)
}

func TestAggregateScanRangeFingerprintSaltCoversLogicalAndPhysicalLayout(t *testing.T) {
	t.Parallel()
	basePlan := aggregateIdentityTestPlan("agg_idx", false, 1, values.NotNullDouble, []string{"ab", "c"}, "V", "MAX", values.UnknownType)
	base := mustAggregateScanIdentity(
		t, basePlan, recordlayer.IndexTypePermutedMax, recordlayer.IndexScanByGroup, 2, 1)

	assertScanIdentitiesDistinct(t, base, map[string]string{
		"index name": mustAggregateScanIdentity(t,
			aggregateIdentityTestPlan("agg_idx_2", false, 1, values.NotNullDouble, []string{"ab", "c"}, "V", "MAX", values.UnknownType),
			recordlayer.IndexTypePermutedMax, recordlayer.IndexScanByGroup, 2, 1),
		"index type": mustAggregateScanIdentity(t,
			basePlan, recordlayer.IndexTypePermutedMin, recordlayer.IndexScanByGroup, 2, 1),
		"scan kind": mustAggregateScanIdentity(t,
			basePlan, recordlayer.IndexTypePermutedMax, recordlayer.IndexScanByValue, 2, 1),
		"direction": mustAggregateScanIdentity(t,
			aggregateIdentityTestPlan("agg_idx", true, 1, values.NotNullDouble, []string{"ab", "c"}, "V", "MAX", values.UnknownType),
			recordlayer.IndexTypePermutedMax, recordlayer.IndexScanByGroup, 2, 1),
		"length-delimited groups": mustAggregateScanIdentity(t,
			aggregateIdentityTestPlan("agg_idx", false, 1, values.NotNullDouble, []string{"a", "bc"}, "V", "MAX", values.UnknownType),
			recordlayer.IndexTypePermutedMax, recordlayer.IndexScanByGroup, 2, 1),
		"aggregate column": mustAggregateScanIdentity(t,
			aggregateIdentityTestPlan("agg_idx", false, 1, values.NotNullDouble, []string{"ab", "c"}, "W", "MAX", values.UnknownType),
			recordlayer.IndexTypePermutedMax, recordlayer.IndexScanByGroup, 2, 1),
		"aggregate function": mustAggregateScanIdentity(t,
			aggregateIdentityTestPlan("agg_idx", false, 1, values.NotNullDouble, []string{"ab", "c"}, "V", "MIN", values.UnknownType),
			recordlayer.IndexTypePermutedMax, recordlayer.IndexScanByGroup, 2, 1),
		"grouping count": mustAggregateScanIdentity(t,
			basePlan, recordlayer.IndexTypePermutedMax, recordlayer.IndexScanByGroup, 3, 1),
		"permuted insertion boundary": mustAggregateScanIdentity(t,
			basePlan, recordlayer.IndexTypePermutedMax, recordlayer.IndexScanByGroup, 2, 2),
		"physical schema": mustAggregateScanIdentity(t,
			aggregateIdentityTestPlan("agg_idx", false, 1, values.NotNullFloat, []string{"ab", "c"}, "V", "MAX", values.UnknownType),
			recordlayer.IndexTypePermutedMax, recordlayer.IndexScanByGroup, 2, 1),
		"result schema": mustAggregateScanIdentity(t,
			aggregateIdentityTestPlan("agg_idx", false, 1, values.NotNullDouble, []string{"ab", "c"}, "V", "MAX", values.NullableString),
			recordlayer.IndexTypePermutedMax, recordlayer.IndexScanByGroup, 2, 1),
	})
}

func vectorIdentityTestPlan(
	indexName string,
	rankType predicates.ComparisonType,
	efSearch *int,
	returningVectors *bool,
	keyType values.Type,
	resultType values.Type,
) *plans.RecordQueryVectorIndexPlan {
	return plans.NewRecordQueryVectorIndexPlan(
		indexName,
		[]*predicates.ComparisonRange{predicates.EmptyComparisonRange()},
		values.LiteralValue([]float64{1, 2}),
		values.LiteralValue(5),
		rankType,
		efSearch,
		returningVectors,
		[]string{"T"},
		resultType,
	).WithPartitionKeyComponentTypes([]values.Type{keyType})
}

func TestVectorScanRangeFingerprintSaltCoversEvaluatedInvocation(t *testing.T) {
	t.Parallel()
	ef := 200
	returnVectors := true
	basePlan := vectorIdentityTestPlan(
		"vec_idx", predicates.ComparisonDistanceRankLessThanOrEq, &ef, &returnVectors, values.NotNullDouble, values.UnknownType)
	baseVector := []float64{0, 1}
	baseRange := recordlayer.VectorDistanceScanRangeWithPrefix(baseVector, 5, ef, nil)
	identity := func(
		plan *plans.RecordQueryVectorIndexPlan,
		indexType string,
		scanType recordlayer.IndexScanType,
		queryVector []float64,
		requestedK, rankCap, effectiveEf int,
		invocationRange recordlayer.TupleRange,
	) string {
		return mustVectorScanIdentity(
			t, plan, indexType, scanType, queryVector,
			requestedK, rankCap, effectiveEf, invocationRange,
		)
	}
	base := identity(
		basePlan, recordlayer.IndexTypeVector, recordlayer.IndexScanByDistance,
		baseVector, 5, 5, ef, baseRange)

	negativeZero := math.Copysign(0, -1)
	nanPayloadOne := math.Float64frombits(0x7ff8000000000001)
	nanPayloadTwo := math.Float64frombits(0x7ff8000000000002)
	falseValue := false
	orderedPlan := basePlan.WithOrderedStream()
	lessThanPlan := vectorIdentityTestPlan(
		"vec_idx", predicates.ComparisonDistanceRankLessThan, &ef, &returnVectors, values.NotNullDouble, values.UnknownType)
	noEfPlan := vectorIdentityTestPlan(
		"vec_idx", predicates.ComparisonDistanceRankLessThanOrEq, nil, &returnVectors, values.NotNullDouble, values.UnknownType)
	notReturningPlan := vectorIdentityTestPlan(
		"vec_idx", predicates.ComparisonDistanceRankLessThanOrEq, &ef, &falseValue, values.NotNullDouble, values.UnknownType)
	changedRange := baseRange
	changedRange.High = tuple.Tuple{int64(6), int64(ef)}

	assertScanIdentitiesDistinct(t, base, map[string]string{
		"index name": identity(
			vectorIdentityTestPlan("vec_idx_2", predicates.ComparisonDistanceRankLessThanOrEq, &ef, &returnVectors, values.NotNullDouble, values.UnknownType),
			recordlayer.IndexTypeVector, recordlayer.IndexScanByDistance, baseVector, 5, 5, ef, baseRange),
		"index implementation": identity(
			basePlan, recordlayer.IndexTypeVectorSPFresh, recordlayer.IndexScanByDistance, baseVector, 5, 5, ef, baseRange),
		"scan kind": identity(
			basePlan, recordlayer.IndexTypeVector, recordlayer.IndexScanByDistanceOrderedStream, baseVector, 5, 5, ef, baseRange),
		"rank comparison": identity(
			lessThanPlan, recordlayer.IndexTypeVector, recordlayer.IndexScanByDistance, baseVector, 5, 5, ef, baseRange),
		"ordered mode": identity(
			orderedPlan, recordlayer.IndexTypeVector, recordlayer.IndexScanByDistance, baseVector, 5, 5, ef, baseRange),
		"return-vectors mode": identity(
			notReturningPlan, recordlayer.IndexTypeVector, recordlayer.IndexScanByDistance, baseVector, 5, 5, ef, baseRange),
		"supplied ef option": identity(
			noEfPlan, recordlayer.IndexTypeVector, recordlayer.IndexScanByDistance, baseVector, 5, 5, ef, baseRange),
		"query signed zero": identity(
			basePlan, recordlayer.IndexTypeVector, recordlayer.IndexScanByDistance, []float64{negativeZero, 1}, 5, 5, ef, baseRange),
		"query NaN payload": identity(
			basePlan, recordlayer.IndexTypeVector, recordlayer.IndexScanByDistance, []float64{nanPayloadOne}, 5, 5, ef,
			recordlayer.VectorDistanceScanRange([]float64{nanPayloadTwo}, 5, ef)),
		"requested k": identity(
			basePlan, recordlayer.IndexTypeVector, recordlayer.IndexScanByDistance, baseVector, 6, 5, ef, baseRange),
		"adjusted cap": identity(
			basePlan, recordlayer.IndexTypeVector, recordlayer.IndexScanByDistance, baseVector, 5, 4, ef, baseRange),
		"effective ef": identity(
			basePlan, recordlayer.IndexTypeVector, recordlayer.IndexScanByDistance, baseVector, 5, 5, ef+1, baseRange),
		"physical invocation range": identity(
			basePlan, recordlayer.IndexTypeVector, recordlayer.IndexScanByDistance, baseVector, 5, 5, ef, changedRange),
		"physical partition schema": identity(
			vectorIdentityTestPlan("vec_idx", predicates.ComparisonDistanceRankLessThanOrEq, &ef, &returnVectors, values.NotNullFloat, values.UnknownType),
			recordlayer.IndexTypeVector, recordlayer.IndexScanByDistance, baseVector, 5, 5, ef, baseRange),
		"result schema": identity(
			vectorIdentityTestPlan("vec_idx", predicates.ComparisonDistanceRankLessThanOrEq, &ef, &returnVectors, values.NotNullDouble, values.NullableString),
			recordlayer.IndexTypeVector, recordlayer.IndexScanByDistance, baseVector, 5, 5, ef, baseRange),
	})
}

func TestScanRangeExecutionIdentityTypeEncodingIsStructural(t *testing.T) {
	t.Parallel()
	left := values.NewRecordType("R", false, []values.Field{
		{Name: "ab", FieldType: values.NotNullDouble},
		{Name: "c", FieldType: values.NullableString},
	})
	right := values.NewRecordType("R", false, []values.Field{
		{Name: "a", FieldType: values.NotNullDouble},
		{Name: "bc", FieldType: values.NullableString},
	})
	if bytes.Equal(mustEncodeScanIdentityType(t, left), mustEncodeScanIdentityType(t, right)) {
		t.Fatal("length-delimited record fields produced the same encoding")
	}
	if !bytes.Equal(mustEncodeScanIdentityType(t, left), mustEncodeScanIdentityType(t, left)) {
		t.Fatal("type encoding is not deterministic")
	}
}

func requireScanIdentityTypeError(
	t testing.TB,
	typ values.Type,
	wantKind ScanIdentityTypeErrorKind,
) *InvalidScanRangeExecutionIdentityTypeError {
	t.Helper()
	_, err := encodeScanIdentityType(typ)
	var identityErr *InvalidScanRangeExecutionIdentityTypeError
	if !errors.As(err, &identityErr) {
		t.Fatalf("encode error = %T(%v), want *InvalidScanRangeExecutionIdentityTypeError", err, err)
	}
	if identityErr.Kind != wantKind {
		t.Fatalf("identity error kind = %v, want %v: %v", identityErr.Kind, wantKind, identityErr)
	}
	if identityErr.Implementation == "" || identityErr.Path == "" {
		t.Fatalf("identity error lacks implementation/path: %+v", identityErr)
	}
	return identityErr
}

func TestScanRangeExecutionIdentityRejectsSelfReferentialBuiltInTypes(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		typ  func() values.Type
	}{
		{
			name: "record",
			typ: func() values.Type {
				record := &values.RecordType{RecordName: "SelfRecord"}
				record.Fields = []values.Field{{Name: "self", FieldType: record}}
				return record
			},
		},
		{
			name: "array",
			typ: func() values.Type {
				array := &values.ArrayType{}
				array.ElementType = array
				return array
			},
		},
		{
			name: "relation",
			typ: func() values.Type {
				relation := &values.RelationType{}
				relation.InnerType = relation
				return relation
			},
		},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			identityErr := requireScanIdentityTypeError(t, test.typ(), ScanIdentityTypeErrorCycle)
			if identityErr.ActivePath == "" || identityErr.ActivePath == identityErr.Path {
				t.Fatalf("cycle paths = active %q reentry %q, want distinct non-empty paths", identityErr.ActivePath, identityErr.Path)
			}
		})
	}
}

func TestScanRangeExecutionIdentityRejectsMutualRecordArrayRelationCycle(t *testing.T) {
	t.Parallel()

	record := &values.RecordType{RecordName: "Cycle"}
	array := &values.ArrayType{}
	relation := &values.RelationType{}
	record.Fields = []values.Field{{Name: "array", FieldType: array}}
	array.ElementType = relation
	relation.InnerType = record

	identityErr := requireScanIdentityTypeError(t, record, ScanIdentityTypeErrorCycle)
	if identityErr.ActivePath != "$type" {
		t.Fatalf("cycle entry path = %q, want root $type", identityErr.ActivePath)
	}
}

func TestScanRangeExecutionIdentityRejectsTypedNilTypes(t *testing.T) {
	t.Parallel()

	var primitive *values.PrimitiveType
	var record *values.RecordType
	var array *values.ArrayType
	var enum *values.EnumType
	var relation *values.RelationType
	tests := []struct {
		name string
		typ  values.Type
	}{
		{name: "primitive", typ: primitive},
		{name: "record", typ: record},
		{name: "array", typ: array},
		{name: "enum", typ: enum},
		{name: "relation", typ: relation},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			requireScanIdentityTypeError(t, test.typ, ScanIdentityTypeErrorTypedNil)
		})
	}
}

func TestScanRangeExecutionIdentitySharedDAGIsPurelyStructural(t *testing.T) {
	t.Parallel()

	leaf := values.NewRecordType("Leaf", false, []values.Field{
		{Name: "value", FieldType: values.NotNullLong},
	})
	sharedArray := values.NewArrayType(false, leaf)
	shared := values.NewRecordType("Root", false, []values.Field{
		{Name: "left", FieldType: sharedArray},
		{Name: "right", FieldType: sharedArray},
	})

	newArray := func() values.Type {
		return values.NewArrayType(false, values.NewRecordType("Leaf", false, []values.Field{
			{Name: "value", FieldType: values.NotNullLong},
		}))
	}
	duplicated := values.NewRecordType("Root", false, []values.Field{
		{Name: "left", FieldType: newArray()},
		{Name: "right", FieldType: newArray()},
	})

	sharedEncoding := mustEncodeScanIdentityType(t, shared)
	duplicatedEncoding := mustEncodeScanIdentityType(t, duplicated)
	if !bytes.Equal(sharedEncoding, duplicatedEncoding) {
		t.Fatal("shared acyclic subgraph leaked pointer identity into structural encoding")
	}
}

func TestScanRangeExecutionIdentityNilAndEmptySemantics(t *testing.T) {
	t.Parallel()

	var nilTuple bytes.Buffer
	var emptyTuple bytes.Buffer
	writeIdentityTuple(&nilTuple, nil)
	writeIdentityTuple(&emptyTuple, tuple.Tuple{})
	if bytes.Equal(nilTuple.Bytes(), emptyTuple.Bytes()) {
		t.Fatal("nil and non-nil empty tuples alias")
	}

	if bytes.Equal(
		mustEncodeScanIdentityType(t, nil),
		mustEncodeScanIdentityType(t, values.UnknownType),
	) {
		t.Fatal("absent Type and UnknownType alias")
	}

	nilFields := &values.RecordType{RecordName: "Unit", Fields: nil}
	emptyFields := &values.RecordType{RecordName: "Unit", Fields: []values.Field{}}
	if !bytes.Equal(
		mustEncodeScanIdentityType(t, nilFields),
		mustEncodeScanIdentityType(t, emptyFields),
	) {
		t.Fatal("nil and empty zero-field records differ despite identical structural semantics")
	}

	if bytes.Equal(
		mustEncodeScanIdentityType(t, values.NewArrayType(false, nil)),
		mustEncodeScanIdentityType(t, values.NewArrayType(false, values.UnknownType)),
	) {
		t.Fatal("erased array element and UnknownType element alias")
	}
	if bytes.Equal(
		mustEncodeScanIdentityType(t, values.NewRelationType(nil)),
		mustEncodeScanIdentityType(t, values.NewRelationType(values.UnknownType)),
	) {
		t.Fatal("erased relation inner and UnknownType inner alias")
	}
}

func TestScanRangeExecutionIdentitySaltsPropagateInvalidType(t *testing.T) {
	t.Parallel()

	cyclic := &values.ArrayType{}
	cyclic.ElementType = cyclic
	efSearch := 200
	returnVectors := true
	tests := []struct {
		name  string
		build func() error
	}{
		{
			name: "primary",
			build: func() error {
				_, err := primaryScanRangeFingerprintSalt(
					primaryIdentityTestPlan([]string{"T"}, false, values.NotNullDouble, cyclic),
				)
				return err
			},
		},
		{
			name: "index",
			build: func() error {
				_, err := indexScanRangeFingerprintSalt(
					indexIdentityTestPlan("idx", false, nil, cyclic, []string{"T"}, values.UnknownType),
					recordlayer.IndexScanByValue,
				)
				return err
			},
		},
		{
			name: "aggregate",
			build: func() error {
				_, err := aggregateScanRangeFingerprintSalt(
					aggregateIdentityTestPlan("agg", false, 0, cyclic, nil, "V", "MAX", values.UnknownType),
					recordlayer.IndexTypeMaxEverLong,
					recordlayer.IndexScanByValue,
					0,
					0,
				)
				return err
			},
		},
		{
			name: "vector",
			build: func() error {
				plan := vectorIdentityTestPlan(
					"vec", predicates.ComparisonDistanceRankLessThanOrEq,
					&efSearch, &returnVectors, cyclic, values.UnknownType,
				)
				_, err := vectorScanRangeFingerprintSalt(
					plan,
					recordlayer.IndexTypeVector,
					recordlayer.IndexScanByDistance,
					[]float64{1},
					1,
					1,
					efSearch,
					recordlayer.VectorDistanceScanRange([]float64{1}, 1, efSearch),
				)
				return err
			},
		},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			err := test.build()
			var identityErr *InvalidScanRangeExecutionIdentityTypeError
			if !errors.As(err, &identityErr) || identityErr.Kind != ScanIdentityTypeErrorCycle {
				t.Fatalf("identity error = %T(%v), want propagated cycle error", err, err)
			}
		})
	}
}
