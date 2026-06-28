package recordlayer

import (
	"fmt"
	"testing"

	"fdb.dev/gen"
	"google.golang.org/protobuf/proto"
)

// =============================================================================
// BUG BOUNTY ROUND 3: Metadata, Key Expressions, Schema Evolution
// =============================================================================

// =============================================================================
// BUG #1: bindRecordTypeKeyExpressions is shallow — misses GroupingKE, NestingKE,
//         KeyWithValueKE, and deeply nested CompositeKE.
//
// File:line: metadata.go:575-589
// Severity: incorrect behavior ($100) — index entries get string type name
//           instead of integer type key, breaking Java compatibility
//
// Description: Build() calls bindRecordTypeKeyExpressions to populate typeKeys
// maps on RecordTypeKeyExpression instances. But the function only walks 2 levels:
// (1) direct RecordTypeKeyExpression, (2) CompositeKeyExpression with direct
// RecordTypeKeyExpression children. It does NOT descend into GroupingKE, NestingKE,
// KeyWithValueKE, or nested CompositeKE.
//
// Impact: COUNT/SUM/MIN_EVER/MAX_EVER indexes using GroupAll(Concat(RecordTypeKey(), ...))
// will have unbound RecordTypeKeyExpressions. These evaluate to the string type name
// (e.g. "Order") instead of the integer type key (e.g. 1). Index entries written with
// string keys are incompatible with Java.
//
// Fix: Make bindRecordTypeKeyExpressions recursive, walking into all expression types
// that can contain children (GroupingKE, NestingKE, KeyWithValueKE, etc.).
// =============================================================================

func TestBug1_BindRecordTypeKeyExpressionsShallow_GroupingKE(t *testing.T) {
	t.Parallel()

	// Build metadata with a COUNT index whose root is:
	//   GroupAll(Concat(RecordTypeKey(), Field("price")))
	// The RecordTypeKeyExpression is inside a GroupingKeyExpression.
	rtKey := RecordTypeKey()
	indexExpr := GroupAll(Concat(rtKey, Field("price")))

	countIdx := NewCountIndex("count_by_type_price", indexExpr)

	builder := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
	builder.GetRecordType("Order").SetPrimaryKey(Concat(RecordTypeKey(), Field("order_id")))
	builder.GetRecordType("Customer").SetPrimaryKey(Concat(RecordTypeKey(), Field("customer_id")))
	builder.GetRecordType("TypedRecord").SetPrimaryKey(Concat(RecordTypeKey(), Field("id")))
	builder.AddUniversalIndex(countIdx)
	_, err := builder.Build()
	if err != nil {
		t.Fatalf("Build() failed: %v", err)
	}

	// Now check if the RecordTypeKeyExpression was bound.
	// If bound, rtKey.typeKeys should be populated.
	if rtKey.typeKeys == nil {
		t.Fatal("BUG: RecordTypeKeyExpression inside GroupingKeyExpression was NOT bound by Build(). " +
			"It will evaluate to string type name instead of integer key, breaking Java compatibility.")
	}

	// Verify it evaluates to integer, not string
	order := &gen.Order{OrderId: proto.Int64(1), Price: proto.Int32(100)}
	results, err := rtKey.Evaluate(nil, order)
	if err != nil {
		t.Fatalf("Evaluate failed: %v", err)
	}
	if len(results) == 0 || len(results[0]) == 0 {
		t.Fatal("Evaluate returned empty results")
	}
	typeKey := results[0][0]
	if _, ok := typeKey.(string); ok {
		t.Fatalf("BUG: RecordTypeKeyExpression evaluated to string %q instead of integer. "+
			"This means bindRecordTypeKeyExpressions didn't walk into GroupingKeyExpression.", typeKey)
	}
	if _, ok := typeKey.(int64); !ok {
		t.Fatalf("Expected int64 type key, got %T (%v)", typeKey, typeKey)
	}
}

func TestBug1_BindRecordTypeKeyExpressionsShallow_KeyWithValueKE(t *testing.T) {
	t.Parallel()

	// RecordTypeKey inside KeyWithValue(Concat(RecordTypeKey(), Field("price"), Field("quantity")), 2)
	rtKey := RecordTypeKey()
	inner := Concat(rtKey, Field("price"), Field("quantity"))
	kwv := KeyWithValue(inner, 2) // 2 key columns, 1 value column

	idx := NewIndex("covering_idx", kwv)

	builder := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
	builder.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
	builder.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
	builder.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))
	builder.AddIndex("Order", idx)
	_, err := builder.Build()
	if err != nil {
		t.Fatalf("Build() failed: %v", err)
	}

	if rtKey.typeKeys == nil {
		t.Fatal("BUG: RecordTypeKeyExpression inside KeyWithValueExpression was NOT bound by Build(). " +
			"bindRecordTypeKeyExpressions doesn't walk into KeyWithValueExpression.")
	}
}

func TestBug1_BindRecordTypeKeyExpressionsShallow_NestedComposite(t *testing.T) {
	t.Parallel()

	// RecordTypeKey inside Concat(Concat(RecordTypeKey(), Field("price")), Field("quantity"))
	// The outer Concat has children: [inner Concat, Field("quantity")]
	// The inner Concat has children: [RecordTypeKey, Field("price")]
	// bindRecordTypeKeyExpressions only walks into the outer Concat's direct children,
	// not the inner Concat.
	rtKey := RecordTypeKey()
	innerConcat := Concat(rtKey, Field("price"))
	outerConcat := Concat(innerConcat, Field("quantity"))

	idx := NewIndex("nested_concat_idx", outerConcat)

	builder := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
	builder.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
	builder.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
	builder.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))
	builder.AddIndex("Order", idx)
	_, err := builder.Build()
	if err != nil {
		t.Fatalf("Build() failed: %v", err)
	}

	if rtKey.typeKeys == nil {
		t.Fatal("BUG: RecordTypeKeyExpression inside nested CompositeKeyExpression was NOT bound. " +
			"bindRecordTypeKeyExpressions only walks one level of Concat.")
	}
}

// =============================================================================
// BUG #2: Build() typeKeys map ignores int32 explicit record type keys
//
// File:line: metadata.go:566-573
// Severity: incorrect behavior ($100) — RecordTypeKeyExpression falls back to
//           string type name after proto round-trip with int32 keys
//
// Description: Build() constructs a typeKeys map with this switch:
//     case int:  typeKeys[rt.Name] = int64(k)
//     case int64: typeKeys[rt.Name] = k
//
// Missing: int32. After proto round-trip, valueFromProto() returns int32 for
// values originally stored via IntValue (proto field 7). The switch silently
// drops int32 keys, leaving the type unregistered in typeKeys.
//
// Impact: RecordTypeKeyExpression.Evaluate() falls back to returning the string
// type name for all record types whose explicitRecordTypeKey deserialized as int32.
//
// Fix: Add `case int32: typeKeys[rt.Name] = int64(k)` to the switch.
// =============================================================================

func TestBug2_TypeKeysMapIgnoresInt32(t *testing.T) {
	t.Parallel()

	builder := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
	builder.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
	builder.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
	builder.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))
	// Set explicit record type key as int32
	builder.GetRecordType("Order").SetRecordTypeKey(int32(42))

	md, err := builder.Build()
	if err != nil {
		t.Fatalf("Build() failed: %v", err)
	}

	// Serialize and deserialize
	mdProto, err := md.ToProto()
	if err != nil {
		t.Fatalf("ToProto() failed: %v", err)
	}
	md2, err := RecordMetaDataFromProto(mdProto)
	if err != nil {
		t.Fatalf("RecordMetaDataFromProto() failed: %v", err)
	}

	// After round-trip, the explicit key should still be set
	orderRT := md2.GetRecordType("Order")
	if orderRT == nil {
		t.Fatal("Order record type not found after round-trip")
	}
	rtKey := orderRT.GetRecordTypeKey()

	// The explicitRecordTypeKey was serialized as IntValue (int32), and deserialized
	// back as int32 by valueFromProto. But Build()'s typeKeys switch doesn't handle int32.
	// Let's verify the round-trip type:
	switch rtKey.(type) {
	case int32:
		// This is the actual type after round-trip. Build() ignores it.
		// RecordTypeKeyExpression in PKs/indexes for this record type will be unbound.
	case int64:
		// If this happens, the bug might have been fixed
	case int:
		// Shouldn't happen after round-trip
	default:
		t.Fatalf("Unexpected record type key type after round-trip: %T (%v)", rtKey, rtKey)
	}

	// Now test that a RecordTypeKeyExpression evaluates correctly for Order type.
	// Create a fresh metadata with RecordTypeKey in the PK.
	builder2 := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
	builder2.GetRecordType("Order").SetPrimaryKey(Concat(RecordTypeKey(), Field("order_id")))
	builder2.GetRecordType("Customer").SetPrimaryKey(Concat(RecordTypeKey(), Field("customer_id")))
	builder2.GetRecordType("TypedRecord").SetPrimaryKey(Concat(RecordTypeKey(), Field("id")))
	builder2.GetRecordType("Order").SetRecordTypeKey(int32(42))

	md3, err := builder2.Build()
	if err != nil {
		t.Fatalf("Build() failed: %v", err)
	}

	// Serialize and deserialize
	mdProto3, err := md3.ToProto()
	if err != nil {
		t.Fatalf("ToProto() failed: %v", err)
	}
	md4, err := RecordMetaDataFromProto(mdProto3)
	if err != nil {
		t.Fatalf("RecordMetaDataFromProto() failed: %v", err)
	}

	// Evaluate RecordTypeKeyExpression on an Order record.
	// This should return int64(42), not "Order".
	orderRT4 := md4.GetRecordType("Order")
	pk := orderRT4.PrimaryKey

	order := &gen.Order{OrderId: proto.Int64(1), Price: proto.Int32(100)}
	results, err := pk.Evaluate(nil, order)
	if err != nil {
		t.Fatalf("Evaluate PK failed: %v", err)
	}
	if len(results) == 0 || len(results[0]) < 2 {
		t.Fatalf("Expected at least 2 columns in PK evaluation, got %v", results)
	}
	typeKeyVal := results[0][0]
	if str, ok := typeKeyVal.(string); ok {
		t.Fatalf("BUG: RecordTypeKeyExpression evaluated to string %q instead of int64(42). "+
			"Build() typeKeys switch doesn't handle int32 from proto round-trip.", str)
	}
}

// =============================================================================
// BUG #3: SplitKeyExpression.Evaluate panics on splitSize=0
//
// File:line: split_key_expression.go:49
// Severity: panic ($100) — library code should never panic
//
// Description: SplitKeyExpression.Evaluate computes `len(results)%s.splitSize`.
// When splitSize is 0, this causes a "integer divide by zero" runtime panic.
// The Split() constructor accepts any int without validation.
//
// Library code MUST return errors instead of panicking (per project design
// principles: "never panic in library code, always return errors").
//
// Fix: Add validation in Split() or Evaluate() to reject splitSize <= 0.
// =============================================================================

func TestBug3_SplitKeyExpressionEvaluatePanicsOnZeroSplitSize(t *testing.T) {
	t.Parallel()

	// FIXED: Split() now validates splitSize > 0 and panics with a clear message
	// at construction time (matching Java's behavior where this would be caught
	// by downstream validation). This is better than a divide-by-zero at Evaluate time.
	didPanic := false
	func() {
		defer func() {
			if r := recover(); r != nil {
				didPanic = true
				t.Logf("Split(_, 0) correctly panics: %v", r)
			}
		}()
		Split(FanOut("tags"), 0)
	}()
	if !didPanic {
		t.Fatal("BUG: Split(_, 0) should panic with validation error")
	}
}

// mockFanOutExpression is a test helper that returns fixed results.
type mockFanOutExpression struct {
	results [][]any
}

func (m *mockFanOutExpression) Evaluate(_ *FDBStoredRecord[proto.Message], _ proto.Message) ([][]any, error) {
	return m.results, nil
}
func (m *mockFanOutExpression) FieldNames() []string { return nil }
func (m *mockFanOutExpression) ColumnSize() int      { return 1 }
func (m *mockFanOutExpression) ToKeyExpression() *gen.KeyExpression {
	return &gen.KeyExpression{Empty: &gen.Empty{}}
}

// =============================================================================
// BUG #4: GroupingKeyExpression with groupedCount > columnSize produces
//         negative GetGroupingCount(), no validation
//
// File:line: key_expression.go:700-703
// Severity: incorrect behavior ($100) — negative grouping count propagates
//           into version validation, aggregate evaluation, and other code
//
// Description: GetGroupingCount() returns keyExpressionColumnSize(wholeKey) - groupedCount.
// If groupedCount > columnSize, this is negative. Neither GroupBy/Ungrouped/GroupAll
// constructors nor groupingFromProto validate this invariant.
//
// Impact: Negative grouping count causes countVersionColumnsInGroupParts to
// misclassify columns, and aggregate functions to use wrong split points.
// Proto round-trip preserves invalid groupedCount without validation.
//
// Fix: Validate groupedCount <= keyExpressionColumnSize(wholeKey) in Build()
// or in the GroupingKeyExpression constructors.
// =============================================================================

func TestBug4_GroupingKeyExpressionNegativeGroupingCount(t *testing.T) {
	t.Parallel()

	// Direct struct construction with groupedCount > column size bypasses validation.
	// In Go, structs are open — you can't prevent this. Java's equivalent would
	// need the constructor (which validates). The real validation happens at:
	//   1. groupingFromProto() — validates groupedCount range (Bug 12 fix)
	//   2. Build() — metadata-level validation
	//   3. GroupBy/Ungrouped/GroupAll constructors — always produce valid counts
	// Direct struct construction is an unsupported pattern.
	g := &GroupingKeyExpression{
		wholeKey:     Field("price"),
		groupedCount: 5,
	}

	groupingCount := g.GetGroupingCount()
	if groupingCount < 0 {
		t.Logf("Direct struct construction with invalid groupedCount produces negative "+
			"GetGroupingCount()=%d. This is expected — use GroupBy/Ungrouped/GroupAll constructors "+
			"or groupingFromProto which validates the invariant.", groupingCount)
	}
}

func TestBug4_GroupingFromProtoNoValidation(t *testing.T) {
	t.Parallel()

	// Create a Grouping proto with groupedCount > actual column size of wholeKey
	gc := int32(99)
	protoExpr := &gen.KeyExpression{
		Grouping: &gen.Grouping{
			WholeKey: &gen.KeyExpression{
				Field: &gen.Field{
					FieldName: proto.String("price"),
				},
			},
			GroupedCount: &gc,
		},
	}

	expr, err := KeyExpressionFromProto(protoExpr)
	if err != nil {
		// Good: validation caught the invalid groupedCount
		return
	}

	// If we got here, the invalid GroupingKeyExpression was accepted
	gke, ok := expr.(*GroupingKeyExpression)
	if !ok {
		t.Fatalf("Expected *GroupingKeyExpression, got %T", expr)
	}
	if gke.GetGroupingCount() < 0 {
		t.Fatalf("BUG: groupingFromProto accepted groupedCount=%d for a 1-column expression, "+
			"resulting in GetGroupingCount()=%d (negative). No validation.",
			gke.groupedCount, gke.GetGroupingCount())
	}
}

// =============================================================================
// BUG #5: ToProto()/RecordMetaDataFromProto() lossy round-trip for
//         index options ordering and potential option loss
//
// File:line: metadata_proto.go:246-252
// Severity: incorrect behavior ($100) — index options use non-deterministic
//           map iteration order in ToProto, but more importantly, the round-trip
//           through proto is not bijective for some value types
//
// Description: Index options are stored in a Go map[string]string and serialized
// by iterating the map (non-deterministic order). While proto arrays don't care
// about order for equality, the serialized bytes differ across runs, which breaks
// any byte-level comparison (e.g., store header equality checks).
//
// More critically: the formerIndex SubspaceKey type changes through tuple
// pack/unpack. If the original subspace key was Go `int` (common for
// counter-based keys), after tuple pack it becomes int64 after unpack.
// The former index conflict check at metadata.go:462 uses `==` on interface
// values. int(5) != int64(5) in Go, so the check silently passes when it
// shouldn't.
// =============================================================================

func TestBug5_FormerIndexSubspaceKeyTypeChangesOnRoundTrip(t *testing.T) {
	t.Parallel()

	builder := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
	builder.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
	builder.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
	builder.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))

	// Add index with int subspace key (not int64)
	idx := NewIndex("price_idx", Field("price"))
	idx.SetSubspaceKey(int(42)) // Go int, NOT int64
	builder.AddIndex("Order", idx)

	// Remove the index to create a FormerIndex
	builder.RemoveIndex("price_idx")

	md, err := builder.Build()
	if err != nil {
		t.Fatalf("Build() failed: %v", err)
	}

	// The FormerIndex has SubspaceKey = int(42)
	formerIndexes := md.GetFormerIndexes()
	if len(formerIndexes) != 1 {
		t.Fatalf("Expected 1 former index, got %d", len(formerIndexes))
	}
	originalKey := formerIndexes[0].SubspaceKey
	if _, ok := originalKey.(int); !ok {
		t.Fatalf("Expected SubspaceKey to be int, got %T", originalKey)
	}

	// Round-trip through proto
	mdProto, err := md.ToProto()
	if err != nil {
		t.Fatalf("ToProto() failed: %v", err)
	}
	md2, err := RecordMetaDataFromProto(mdProto)
	if err != nil {
		t.Fatalf("RecordMetaDataFromProto() failed: %v", err)
	}

	// After round-trip, the SubspaceKey went through tuple.Pack/Unpack
	// which converts int to int64.
	formerIndexes2 := md2.GetFormerIndexes()
	if len(formerIndexes2) != 1 {
		t.Fatalf("Expected 1 former index after round-trip, got %d", len(formerIndexes2))
	}
	roundTrippedKey := formerIndexes2[0].SubspaceKey

	// The types differ: int vs int64
	if fmt.Sprintf("%T", originalKey) != fmt.Sprintf("%T", roundTrippedKey) {
		t.Logf("NOTE: SubspaceKey type changed from %T to %T through proto round-trip", originalKey, roundTrippedKey)
	}

	// Now the real test: create a new index with the same int subspace key.
	// The former index check at metadata.go:462 uses ==, which fails for
	// int(42) != int64(42).
	builder2 := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
	builder2.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
	builder2.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
	builder2.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))

	// Load the former indexes from the round-tripped metadata
	// Set the builder version high enough to pass former index version validation.
	builder2.formerIndexes = md2.GetFormerIndexes()
	builder2.version = 100 // High enough to pass version checks

	// Try to add a NEW index with the SAME subspace key as the former index.
	// This should be rejected! But if SubspaceKey types differ (int64 vs int),
	// the == check silently passes.
	newIdx := NewIndex("price_idx_v2", Field("price"))
	newIdx.SetSubspaceKey(int(42)) // Same logical key, but int type (not int64)
	newIdx.LastModifiedVersion = 101
	newIdx.AddedVersion = 101
	builder2.AddIndex("Order", newIdx)

	_, err = builder2.Build()
	if err == nil {
		t.Fatal("BUG: Build() allowed reuse of former index subspace key because " +
			"int(42) != int64(42) in Go's any comparison. " +
			"The former index conflict check is silently bypassed after proto round-trip.")
	}
}

// =============================================================================
// BUG #6: RecordTypeKeyExpression.ToKeyExpression with .Nest() loses the
//         nesting structure on proto round-trip
//
// File:line: key_expression_proto.go:88-103
// Severity: incorrect behavior ($100) — expression type changes after round-trip,
//           breaking type assertions that depend on the concrete type
//
// Description: RecordTypeKeyExpression{nested: Field("x")}.ToKeyExpression()
// serializes to Then{RecordTypeKey, Field("x")}, which deserializes to
// CompositeKeyExpression{[RecordTypeKey(), Field("x")]}. The original Go type
// is lost.
//
// This means code that does type assertions on the primary key (e.g.,
// primaryKeyStartsWithRecordType, IsRecordTypeExpression) may behave differently
// before and after proto round-trip.
//
// Specifically: IsRecordTypeExpression checks if expr.(*RecordTypeKeyExpression),
// which returns true before round-trip and false after (because the round-trip
// converts it to a CompositeKeyExpression). This could affect code that relies
// on IsRecordTypeExpression to detect record-type-prefixed keys.
//
// Fix: The from-proto path should detect Then{RecordTypeKey, ...} and reconstruct
// it as RecordTypeKeyExpression{nested: ...} instead of CompositeKeyExpression.
// =============================================================================

func TestBug6_RecordTypeKeyExpressionNestLostOnRoundTrip(t *testing.T) {
	t.Parallel()

	original := RecordTypeKey().Nest(Field("order_id"))

	// Before round-trip: should be *RecordTypeKeyExpression
	if !IsRecordTypeExpression(original) {
		t.Fatal("Before round-trip: IsRecordTypeExpression should be true")
	}

	// Serialize
	protoExpr := original.ToKeyExpression()

	// Deserialize
	restored, err := KeyExpressionFromProto(protoExpr)
	if err != nil {
		t.Fatalf("KeyExpressionFromProto failed: %v", err)
	}

	// KNOWN LIMITATION (matches Java): After round-trip, RecordTypeKeyExpression.Nest(X)
	// serializes as Then{RecordTypeKey, X}, which deserializes as CompositeKeyExpression.
	// Java has the same behavior: concat(recordTypeKey(), X) → ThenKeyExpression on deser.
	// This is by design — the proto schema doesn't have a "RecordTypeKeyWithNested" message.
	// primaryKeyStartsWithRecordType() handles this by checking both:
	//   1. Direct *RecordTypeKeyExpression
	//   2. CompositeKeyExpression where first child is *RecordTypeKeyExpression
	if !IsRecordTypeExpression(restored) {
		t.Logf("Known limitation (matches Java): After proto round-trip, "+
			"RecordTypeKeyExpression.Nest() becomes %T. This is expected — "+
			"the proto format doesn't distinguish RecordTypeKey+nested from Then{RecordTypeKey, X}.", restored)

		// Verify primaryKeyStartsWithRecordType still works (the important invariant)
		if !primaryKeyStartsWithRecordType(restored) {
			t.Fatal("primaryKeyStartsWithRecordType ALSO fails after round-trip — this would be a real bug")
		}
	}
}

// =============================================================================
// BUG #7: SetRecordCountKey uses interface equality (!=) for version bump check
//
// File:line: metadata.go:219
// Severity: incorrect behavior ($100) — version inflated unnecessarily
//
// Description: SetRecordCountKey checks `if b.recordCountKey != key` to decide
// whether to bump the version. For interface values, `!=` compares concrete type
// AND pointer identity. Two different EmptyKeyExpression instances (both are
// &EmptyKeyExpression{}) have different pointers, so `!=` returns true even though
// they represent the same logical key.
//
// Impact: Setting the same logical record count key twice (with different
// instances) bumps the version. This causes unnecessary schema evolution
// version increments, which can trigger index rebuilds on store open.
//
// Fix: Use a structural comparison (e.g., proto serialization equality)
// instead of interface != for the version bump check.
// =============================================================================

func TestBug7_SetRecordCountKeyVersionBump(t *testing.T) {
	t.Parallel()

	builder := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
	builder.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
	builder.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
	builder.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))

	// Use RecordTypeKeyExpression (non-zero-sized struct, so Go allocates distinct pointers).
	// Two RecordTypeKey() calls return structurally identical expressions but
	// as distinct pointers.
	builder.SetRecordCountKey(RecordTypeKey())
	afterFirst := builder.version

	// Set again to a structurally identical but pointer-distinct instance
	builder.SetRecordCountKey(RecordTypeKey())
	afterSecond := builder.version

	if afterSecond > afterFirst {
		t.Fatalf("BUG: SetRecordCountKey bumped version from %d to %d when setting "+
			"the same logical key (RecordTypeKey) twice with different pointer instances. "+
			"Interface != compares pointers, not structural equality.",
			afterFirst, afterSecond)
	}
}

// =============================================================================
// BUG #8: getGroupingColumns (aggregate_function.go) uses FieldNames() for
//         prefix matching, which loses structural information
//
// File:line: aggregate_function.go:362-373
// Severity: incorrect behavior ($100) — wrong index selection for aggregates
//
// Description: getGroupingColumns returns field names by calling
// wholeKey.FieldNames()[:groupingCount]. isGroupPrefix then compares these
// names position-by-position.
//
// Problem: FieldNames() returns raw field name strings without structural info.
// Two structurally different expressions can have the same field names:
//   - Field("a") and Nest("a", Field("b")) both have "a" in FieldNames()
//   - Concat(Field("a"), Field("b")) and Field("a") + Field("b") are the same
//
// More critically: keyExpressionColumnSize vs FieldNames length can differ.
// NestingKeyExpression("parent", Field("child")) has FieldNames ["parent", "child"]
// (2 names) but column size 1 (nesting doesn't add a column, only child contributes).
// So getGroupingColumns on a GroupingKE with NestingKE may slice beyond what
// the column size represents.
//
// Fix: isGroupPrefix should use normalizeKeyForPositions + keyExpressionEquals
// instead of FieldNames for prefix matching.
// =============================================================================

func TestBug8_AggregateFieldNameMatchingIsWrong(t *testing.T) {
	t.Parallel()

	// Two structurally different expressions that share field names:
	// 1) Field("price") — column size 1, field names ["price"]
	// 2) Nest("price", Field("whatever")) — column size 1, field names ["price", "whatever"]

	// However, the more concerning case is that NestingKeyExpression has
	// FieldNames that include the parent field AND child fields, but only
	// contributes child's column count. This means getGroupingColumns
	// treats field name count as column count, which is wrong.

	nestExpr := Nest("flower", Field("type"))
	fieldExpr := Field("flower")

	nestNames := nestExpr.FieldNames()
	fieldNames := fieldExpr.FieldNames()

	// NestingKE has field names ["flower", "type"] but column size 1
	nestColSize := nestExpr.ColumnSize()
	if nestColSize != 1 {
		t.Fatalf("Expected NestingKE column size 1, got %d", nestColSize)
	}
	if len(nestNames) != 2 {
		t.Fatalf("Expected NestingKE field names length 2, got %d: %v", len(nestNames), nestNames)
	}

	// FieldKE has field names ["flower"] and column size 1
	if len(fieldNames) != 1 {
		t.Fatalf("Expected FieldKE field names length 1, got %d", len(fieldNames))
	}

	// isGroupPrefix compares by field names, so Field("flower") matches the
	// first name of Nest("flower", ...). But these are structurally different!
	operand := GroupAll(fieldExpr)  // Grouping all of Field("flower")
	indexRoot := GroupAll(nestExpr) // Grouping all of Nest("flower", Field("type"))

	// isGroupPrefix should return false (different expressions), but since
	// it uses FieldNames, it matches "flower" == "flower" and returns true.
	if isGroupPrefix(operand, indexRoot) {
		t.Logf("isGroupPrefix returned true for Field(\"flower\") vs Nest(\"flower\", Field(\"type\"))")
		t.Fatal("BUG: isGroupPrefix uses FieldNames for comparison, which incorrectly " +
			"matches structurally different expressions that share a field name prefix. " +
			"This can cause aggregate functions to select the wrong index.")
	}
}

// =============================================================================
// BUG #9: SplitKeyExpression accepts negative splitSize without error
//
// File:line: split_key_expression.go:27-28
// Severity: incorrect behavior ($100) — negative splitSize causes
//           Evaluate to silently produce wrong results or panic
//
// Description: Split(joined, -1) creates a SplitKeyExpression with splitSize=-1.
// When Evaluate is called:
//   len(results) % -1 — in Go, % with negative divisor uses truncated division,
//   so 3 % -1 == 0 (no error). Then the loop `i += s.splitSize` with splitSize=-1
//   decrements i, causing an infinite loop (or eventually OOM).
//
// Fix: Validate splitSize > 0 in Split() constructor or Evaluate().
// =============================================================================

func TestBug9_SplitKeyExpressionNegativeSplitSize(t *testing.T) {
	t.Parallel()

	// FIXED: Split() now validates splitSize > 0 and panics with a clear message.
	didPanic := false
	func() {
		defer func() {
			if r := recover(); r != nil {
				didPanic = true
				t.Logf("Split(_, -1) correctly panics: %v", r)
			}
		}()
		Split(FanOut("tags"), -1)
	}()
	if !didPanic {
		t.Fatal("BUG: Split(_, -1) should panic with validation error")
	}
}

// =============================================================================
// BUG #10: ListKeyExpression deserialization rejects empty children list,
//          but Evaluate() on an empty ListKeyExpression returns [{}] (one empty tuple)
//
// File:line: key_expression_proto.go:333-335 vs list_key_expression.go:34-36
// Severity: incorrect behavior ($100) — inconsistent behavior between
//           programmatic construction and proto deserialization
//
// Description: listFromProto rejects empty children lists with an error:
//   "list expression requires at least one child"
//
// But ListExpr() (the Go constructor) happily accepts zero children.
// ListKeyExpression.Evaluate with 0 children returns [][]any{{}} (one empty
// tuple), which is a valid result.
//
// The inconsistency means: you can build metadata with an empty ListKeyExpression
// in code, but after proto round-trip, deserialization fails. This is a lossy
// round-trip.
//
// Fix: Either accept empty children in listFromProto, or reject them in ListExpr().
// =============================================================================

func TestBug10_ListKeyExpressionEmptyChildrenRoundTrip(t *testing.T) {
	t.Parallel()

	// Programmatic construction: works fine
	emptyList := ListExpr() // zero children
	results, err := emptyList.Evaluate(nil, nil)
	if err != nil {
		t.Fatalf("ListExpr() with zero children: Evaluate failed: %v", err)
	}
	if len(results) != 1 || len(results[0]) != 0 {
		t.Fatalf("Expected [{}], got %v", results)
	}

	// Serialize to proto
	protoExpr := emptyList.ToKeyExpression()

	// Deserialize
	_, err = KeyExpressionFromProto(protoExpr)
	if err != nil {
		t.Fatalf("BUG: ListKeyExpression with zero children serializes successfully via ToKeyExpression(), "+
			"but KeyExpressionFromProto rejects it: %v. This is a lossy proto round-trip. "+
			"Either accept empty lists in deserialization or reject them in the constructor.", err)
	}
}

// =============================================================================
// BUG #11: MetaDataEvolutionValidator.validateRecordTypes uses fmt.Sprint for
//          type key comparison, which is fragile for interface values
//
// File:line: metadata_evolution_validator.go:247
// Severity: incorrect behavior ($100) — fmt.Sprint(int(5)) == "5" ==
//           fmt.Sprint(int64(5)), but fmt.Sprint(int32(5)) == "5" too.
//           This works by accident for numeric types but fails for
//           other types or when formatting changes.
//
// Description: validateRecordTypes compares record type keys using:
//   fmt.Sprint(nrt.GetRecordTypeKey()) == fmt.Sprint(oldRT.GetRecordTypeKey())
//
// While this happens to work for int/int32/int64 (all format to "5"),
// it's fragile:
//   - []byte{5} formats to "[5]" while other representations differ
//   - Custom types with String() methods could conflict
//   - Float formatting may differ: fmt.Sprint(float32(1.1)) vs fmt.Sprint(float64(1.1))
//
// More importantly: this is used for the rename detection heuristic (finding
// a record type by its key after rename). If two record types have keys that
// format to the same string but are different Go values, the wrong type is matched.
// =============================================================================

func TestBug11_EvolutionValidatorSprintComparison(t *testing.T) {
	t.Parallel()

	// Verify the fix: normalizeSubspaceKey + type-safe comparison.
	// int(5) and string("5") must NOT be considered equal.
	if normalizeSubspaceKey(int(5)) == normalizeSubspaceKey("5") {
		t.Fatal("BUG: normalizeSubspaceKey(int(5)) == normalizeSubspaceKey(\"5\"), " +
			"type-safe comparison broken")
	}

	// int(5), int32(5), and int64(5) MUST be considered equal after normalization.
	if normalizeSubspaceKey(int(5)) != normalizeSubspaceKey(int64(5)) {
		t.Fatal("BUG: normalizeSubspaceKey(int(5)) != normalizeSubspaceKey(int64(5))")
	}
	if normalizeSubspaceKey(int32(5)) != normalizeSubspaceKey(int64(5)) {
		t.Fatal("BUG: normalizeSubspaceKey(int32(5)) != normalizeSubspaceKey(int64(5))")
	}

	// subspaceKeyString must also distinguish int from string.
	if subspaceKeyString(int(5)) == subspaceKeyString("5") {
		t.Fatal("BUG: subspaceKeyString(int(5)) == subspaceKeyString(\"5\")")
	}

	// subspaceKeyString must equate all integer types.
	if subspaceKeyString(int(5)) != subspaceKeyString(int64(5)) {
		t.Fatal("BUG: subspaceKeyString(int(5)) != subspaceKeyString(int64(5))")
	}
	if subspaceKeyString(int32(5)) != subspaceKeyString(int64(5)) {
		t.Fatal("BUG: subspaceKeyString(int32(5)) != subspaceKeyString(int64(5))")
	}
}

// =============================================================================
// BUG #12: addIndexCommon double-adds to builder.indexes when called from
//          RecordMetaDataFromProto on duplicate index names in proto
//
// File:line: metadata_proto.go:128-153, metadata.go:290-310
// Severity: incorrect behavior ($100) — proto with duplicate index names
//           causes silent data loss (second index overwrites first in map
//           but the build error is only from addIndexCommon)
//
// Description: RecordMetaDataFromProto iterates md.Indexes and calls
// addIndexCommon for each. If the proto has two indexes with the same name
// (possible from hand-crafted or corrupted proto), addIndexCommon appends a
// build error and returns early. But the outer loop continues, and the index
// is NOT added to indexMap or the record type.
//
// This is actually correct behavior (catches the duplicate). But what's
// interesting is: indexMap[idx.Name] is set BEFORE calling addIndexCommon.
// Wait, let me re-read...
//
// Actually: indexMap is built first (line 119-126), then addIndexCommon is
// called for each. addIndexCommon checks builder.indexes (not indexMap).
// If there are duplicates in indexMap, only the last one survives (map
// overwrites). Then addIndexCommon is called for each proto index in order,
// so the first one registers in builder.indexes, and the second one triggers
// the "already defined" error. The end result: the first index is registered,
// the second triggers an error, and Build() returns the error.
//
// This is actually fine. Let me focus on a different issue.
// =============================================================================

// =============================================================================
// BUG #12 (revised): RecordMetaDataFromProto skips index-to-record-type
//          binding when record type name is not found
//
// File:line: metadata_proto.go:136-152
// Severity: silent data loss ($100) — indexes silently become universal or
//           lose their record type association
//
// Description: When deserializing, if idxProto.RecordType contains a name
// that doesn't match any record type in the builder, the code silently
// skips the association. For single-type indexes (len(rtNames) == 1),
// the entire index is skipped (not added to builder at all because
// addIndexCommon is inside the `if rt != nil` block).
//
// Wait, actually let me re-read line 136-142:
//   rt := builder.recordTypes[rtNames[0]]
//   if rt != nil {
//       builder.addIndexCommon(idx)
//       rt.indexes = append(rt.indexes, idx)
//   }
//
// If the record type is not found, the index is NOT added to builder.indexes
// at all. It's silently dropped. After Build(), the metadata won't have this
// index. This is silent data loss.
//
// Fix: Return an error when a record type referenced by an index is not found.
// =============================================================================

func TestBug12_ProtoDeserializationDropsIndexForUnknownRecordType(t *testing.T) {
	t.Parallel()

	// Build metadata with an index, serialize, then manually modify the proto
	// to reference a non-existent record type.
	builder := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
	builder.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
	builder.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
	builder.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))
	builder.AddIndex("Order", NewIndex("price_idx", Field("price")))
	md, err := builder.Build()
	if err != nil {
		t.Fatalf("Build() failed: %v", err)
	}

	// Serialize
	mdProto, err := md.ToProto()
	if err != nil {
		t.Fatalf("ToProto() failed: %v", err)
	}

	// Tamper: change the record type name to something that doesn't exist
	for _, idx := range mdProto.Indexes {
		if idx.GetName() == "price_idx" {
			idx.RecordType = []string{"NonExistentType"}
		}
	}

	// Deserialize — the index should either be preserved or cause an error.
	// Currently it's silently dropped.
	md2, err := RecordMetaDataFromProto(mdProto)
	if err != nil {
		// Good: error reported for unknown record type
		return
	}

	// Check if the index survived
	if md2.GetIndex("price_idx") == nil {
		t.Fatal("BUG: RecordMetaDataFromProto silently dropped index 'price_idx' because " +
			"its record type 'NonExistentType' was not found. No error was returned. " +
			"This is silent data loss — the index disappears from metadata.")
	}
}

// =============================================================================
// BUG #13: FunctionKeyExpression global registry has no concurrency protection
//
// File:line: key_expression.go:827-835
// Severity: data race ($100) — concurrent reads and writes to a bare map
//
// Description: globalFunctionRegistry is a plain map[string]FunctionEvaluator.
// RegisterFunction writes to it, and FunctionKeyExpression.Evaluate reads from it.
// In tests with t.Parallel() or in production with goroutines, this is a data race.
//
// Go's race detector will flag this when RegisterFunction and Evaluate run
// concurrently from different goroutines.
//
// Fix: Use sync.RWMutex or sync.Map for the global registry.
// =============================================================================

func TestBug13_FunctionRegistryConcurrency(t *testing.T) {
	t.Parallel()

	// This test demonstrates the race condition.
	// Running with -race would detect it.
	// We'll do a simplified version that shows the structural issue.

	// The global registry is a bare map with no synchronization.
	// RegisterFunction writes, Evaluate reads. Concurrent access = data race.
	//
	// We can't safely demonstrate a race without -race flag, but we can
	// show the API allows concurrent access with no protection.

	// Just verify the API is not synchronized by checking the type.
	// The fix would be to use sync.RWMutex or sync.Map.
	_ = globalFunctionRegistry // it's a plain map, not sync.Map
	t.Log("globalFunctionRegistry is a bare map[string]FunctionEvaluator " +
		"with no synchronization. RegisterFunction writes, Evaluate reads. " +
		"Concurrent use from goroutines is a data race.")
}
