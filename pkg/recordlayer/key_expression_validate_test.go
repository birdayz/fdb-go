package recordlayer

import (
	"errors"
	"testing"

	"fdb.dev/gen"
	"google.golang.org/protobuf/reflect/protoreflect"
)

// helper: get the Order message descriptor from the demo proto.
func orderDescriptor() protoreflect.MessageDescriptor {
	return gen.File_record_layer_demo_proto.Messages().ByName("Order")
}

// helper: get the Customer message descriptor from the demo proto.
func customerDescriptor() protoreflect.MessageDescriptor {
	return gen.File_record_layer_demo_proto.Messages().ByName("Customer")
}

// helper: build a RecordMetaDataBuilder pre-populated with record types and primary keys.
func baseBuilder() *RecordMetaDataBuilder {
	b := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
	b.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
	b.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
	b.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))
	return b
}

// requireKeyExpressionError asserts err is a *KeyExpressionError.
func requireKeyExpressionError(t *testing.T, err error) {
	t.Helper()
	if err == nil {
		t.Fatal("expected KeyExpressionError, got nil")
	}
	var ke *KeyExpressionError
	if !errors.As(err, &ke) {
		t.Fatalf("expected *KeyExpressionError, got %T: %v", err, err)
	}
}

// requireMetaDataError asserts err is a *MetaDataError.
func requireMetaDataError(t *testing.T, err error) {
	t.Helper()
	if err == nil {
		t.Fatal("expected MetaDataError, got nil")
	}
	var me *MetaDataError
	if !errors.As(err, &me) {
		t.Fatalf("expected *MetaDataError, got %T: %v", err, err)
	}
}

// requireNoError fails the test if err is non-nil.
func requireNoError(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

// ---------------------------------------------------------------------------
// 1. validateKeyExpression on FieldKeyExpression
// ---------------------------------------------------------------------------

func TestValidateField_ValidFieldExists(t *testing.T) {
	t.Parallel()
	err := validateKeyExpression(Field("order_id"), orderDescriptor())
	requireNoError(t, err)
}

func TestValidateField_NonExistentField(t *testing.T) {
	t.Parallel()
	err := validateKeyExpression(Field("nonexistent"), orderDescriptor())
	requireKeyExpressionError(t, err)
}

func TestValidateField_FanOutOnNonRepeatedField(t *testing.T) {
	t.Parallel()
	err := validateKeyExpression(FanOut("order_id"), orderDescriptor())
	requireKeyExpressionError(t, err)
}

func TestValidateField_FanTypeNoneOnRepeatedField(t *testing.T) {
	t.Parallel()
	// "tags" is repeated string in Order
	err := validateKeyExpression(Field("tags"), orderDescriptor())
	requireKeyExpressionError(t, err)
}

func TestValidateField_MessageTypeFieldWithoutNest(t *testing.T) {
	t.Parallel()
	// "flower" is a message field in Order — Field() without Nest() should
	// fail, with Java's Query.InvalidExpressionException.
	err := validateKeyExpression(Field("flower"), orderDescriptor())
	var qe *QueryInvalidExpressionError
	if !errors.As(err, &qe) {
		t.Fatalf("expected *QueryInvalidExpressionError, got %T: %v", err, err)
	}
}

// TestKeyValidationTextsAreJavas pins every refusal of key validation to the
// text of Java's validate (FieldKeyExpression.java:145-172,
// KeyWithValueExpression.validate, SplitKeyExpression.validate; a nesting into
// a scalar is protobuf-java's getMessageType refusal, measured on the JVM by
// the conformance spec "Key validation at build, as Java builds") and to its
// class, named per case: KeyExpression.InvalidExpressionException is
// KeyExpressionError, Query.InvalidExpressionException is
// QueryInvalidExpressionError, UnsupportedOperationException is
// UnsupportedOperationError.
func TestKeyValidationTextsAreJavas(t *testing.T) {
	t.Parallel()
	const (
		keyClass = iota
		queryClass
		unsupportedClass
	)
	for _, c := range []struct {
		name  string
		expr  KeyExpression
		want  string
		class int
	}{
		{"a missing field", Field("nope"), "Descriptor Order does not have field: nope", keyClass},
		{"fan-out over a scalar", FanOut("price"), "price is not repeated with FanType.FanOut", keyClass},
		{"concatenate over a scalar", &FieldKeyExpression{fieldName: "price", fanType: FanTypeConcatenate}, "price is not repeated with FanType.Concatenate", keyClass},
		{"a repeated field as a scalar", Field("tags"), "tags is repeated with FanType.None", keyClass},
		{"a message as a scalar", Field("flower"), "flower is a nested message, but accessed as a scalar", queryClass},
		{"a nesting's missing parent", Nest("nope", Field("type")), "Descriptor Order does not have field: nope", keyClass},
		{"a nesting into a scalar", Nest("price", Field("type")), "This field is not of message type. (com.apple.foundationdb.record.Order.price)", unsupportedClass},
		{"a nested missing field", Nest("flower", Field("nope")), "Descriptor Flower does not have field: nope", keyClass},
		{"a covering key too short", KeyWithValue(Field("price"), 2), "Child expression of covering expression returns too few columns", keyClass},
		{"a split of two columns", Split(Concat(Field("price"), Field("quantity")), 1), "Must have a single key before splitting", keyClass},
		{"a split of one value", Split(Field("price"), 1), "Must produce multiple values for splitting", keyClass},
	} {
		err := validateKeyExpression(c.expr, orderDescriptor())
		if err == nil || err.Error() != c.want {
			t.Errorf("%s: %v (%T), want %q", c.name, err, err, c.want)
			continue
		}
		var ke *KeyExpressionError
		var qe *QueryInvalidExpressionError
		var ue *UnsupportedOperationError
		got := []bool{errors.As(err, &ke), errors.As(err, &qe), errors.As(err, &ue)}
		for class, is := range got {
			if is != (class == c.class) {
				t.Errorf("%s: %T is not the class the case names (%d)", c.name, err, c.class)
			}
		}
	}
}

func TestValidateField_FanTypeConcatenateOnRepeatedField(t *testing.T) {
	t.Parallel()
	expr := &FieldKeyExpression{fieldName: "tags", fanType: FanTypeConcatenate}
	err := validateKeyExpression(expr, orderDescriptor())
	requireNoError(t, err)
}

func TestValidateField_FanOutOnRepeatedField(t *testing.T) {
	t.Parallel()
	err := validateKeyExpression(FanOut("tags"), orderDescriptor())
	requireNoError(t, err)
}

func TestValidateField_ScalarField_Price(t *testing.T) {
	t.Parallel()
	err := validateKeyExpression(Field("price"), orderDescriptor())
	requireNoError(t, err)
}

// ---------------------------------------------------------------------------
// 2. validateKeyExpression on NestingKeyExpression
// ---------------------------------------------------------------------------

func TestValidateNesting_ValidNestedField(t *testing.T) {
	t.Parallel()
	// Nest("flower", Field("type")) — flower is a message, type is a string inside it
	err := validateKeyExpression(Nest("flower", Field("type")), orderDescriptor())
	requireNoError(t, err)
}

func TestValidateNesting_NonExistentParentField(t *testing.T) {
	t.Parallel()
	err := validateKeyExpression(Nest("nonexistent", Field("type")), orderDescriptor())
	requireKeyExpressionError(t, err)
}

func TestValidateNesting_ParentFieldNotAMessage(t *testing.T) {
	t.Parallel()
	// "price" is an int32, not a message — cannot nest into it. Java's
	// getMessageType throws UnsupportedOperationException.
	err := validateKeyExpression(Nest("price", Field("type")), orderDescriptor())
	var ue *UnsupportedOperationError
	if !errors.As(err, &ue) {
		t.Fatalf("expected *UnsupportedOperationError, got %T: %v", err, err)
	}
}

func TestValidateNesting_ValidChildFieldInNestedMessage(t *testing.T) {
	t.Parallel()
	// Nest("flower", Field("color")) — Flower has a "color" enum field
	err := validateKeyExpression(Nest("flower", Field("color")), orderDescriptor())
	requireNoError(t, err)
}

func TestValidateNesting_InvalidChildFieldInNestedMessage(t *testing.T) {
	t.Parallel()
	// Nest("flower", Field("nonexistent")) — Flower has no "nonexistent" field
	err := validateKeyExpression(Nest("flower", Field("nonexistent")), orderDescriptor())
	requireKeyExpressionError(t, err)
}

func TestValidateNesting_NestFanOutOnNonRepeatedMessage(t *testing.T) {
	t.Parallel()
	// "flower" is NOT repeated — NestFanOut should fail
	err := validateKeyExpression(NestFanOut("flower", Field("type")), orderDescriptor())
	requireKeyExpressionError(t, err)
}

// ---------------------------------------------------------------------------
// 3. validateKeyExpression on CompositeKeyExpression
// ---------------------------------------------------------------------------

func TestValidateComposite_AllChildrenValid(t *testing.T) {
	t.Parallel()
	expr := Concat(Field("order_id"), Field("price"))
	err := validateKeyExpression(expr, orderDescriptor())
	requireNoError(t, err)
}

func TestValidateComposite_OneChildInvalid(t *testing.T) {
	t.Parallel()
	expr := Concat(Field("order_id"), Field("nonexistent"))
	err := validateKeyExpression(expr, orderDescriptor())
	requireKeyExpressionError(t, err)
}

func TestValidateComposite_Empty(t *testing.T) {
	t.Parallel()
	expr := Concat()
	err := validateKeyExpression(expr, orderDescriptor())
	requireNoError(t, err)
}

func TestValidateComposite_NestedComposite(t *testing.T) {
	t.Parallel()
	expr := Concat(Concat(Field("order_id"), Field("price")), Field("quantity"))
	err := validateKeyExpression(expr, orderDescriptor())
	requireNoError(t, err)
}

func TestValidateComposite_NestedCompositeWithInvalid(t *testing.T) {
	t.Parallel()
	expr := Concat(Concat(Field("order_id"), Field("bad")), Field("price"))
	err := validateKeyExpression(expr, orderDescriptor())
	requireKeyExpressionError(t, err)
}

// ---------------------------------------------------------------------------
// 4. validateKeyExpression on GroupingKeyExpression
// ---------------------------------------------------------------------------

func TestValidateGrouping_DelegatesToWholeKey_Valid(t *testing.T) {
	t.Parallel()
	expr := GroupAll(Field("price"))
	err := validateKeyExpression(expr, orderDescriptor())
	requireNoError(t, err)
}

func TestValidateGrouping_DelegatesToWholeKey_Invalid(t *testing.T) {
	t.Parallel()
	expr := GroupAll(Field("nonexistent"))
	err := validateKeyExpression(expr, orderDescriptor())
	requireKeyExpressionError(t, err)
}

func TestValidateGrouping_Ungrouped(t *testing.T) {
	t.Parallel()
	expr := Ungrouped(EmptyKey())
	err := validateKeyExpression(expr, orderDescriptor())
	requireNoError(t, err)
}

func TestValidateGrouping_GroupBy(t *testing.T) {
	t.Parallel()
	expr := GroupBy(Field("price"), Field("quantity"))
	err := validateKeyExpression(expr, orderDescriptor())
	requireNoError(t, err)
}

func TestValidateGrouping_GroupByWithInvalidGrouped(t *testing.T) {
	t.Parallel()
	expr := GroupBy(Field("nonexistent"), Field("price"))
	err := validateKeyExpression(expr, orderDescriptor())
	requireKeyExpressionError(t, err)
}

func TestValidateGrouping_GroupByWithInvalidGrouping(t *testing.T) {
	t.Parallel()
	expr := GroupBy(Field("price"), Field("nonexistent"))
	err := validateKeyExpression(expr, orderDescriptor())
	requireKeyExpressionError(t, err)
}

// ---------------------------------------------------------------------------
// 5. validateKeyExpression on leaf types
// ---------------------------------------------------------------------------

func TestValidateLeaf_EmptyKeyExpression(t *testing.T) {
	t.Parallel()
	err := validateKeyExpression(EmptyKey(), orderDescriptor())
	requireNoError(t, err)
}

func TestValidateLeaf_RecordTypeKeyExpression(t *testing.T) {
	t.Parallel()
	err := validateKeyExpression(RecordTypeKey(), orderDescriptor())
	requireNoError(t, err)
}

func TestValidateLeaf_LiteralKeyExpression(t *testing.T) {
	t.Parallel()
	err := validateKeyExpression(Literal("hello"), orderDescriptor())
	requireNoError(t, err)
}

func TestValidateLeaf_LiteralKeyExpression_Int(t *testing.T) {
	t.Parallel()
	err := validateKeyExpression(Literal(42), orderDescriptor())
	requireNoError(t, err)
}

func TestValidateLeaf_LiteralKeyExpression_Nil(t *testing.T) {
	t.Parallel()
	err := validateKeyExpression(Literal(nil), orderDescriptor())
	requireNoError(t, err)
}

func TestValidateLeaf_VersionKeyExpression(t *testing.T) {
	t.Parallel()
	err := validateKeyExpression(VersionKey(), orderDescriptor())
	requireNoError(t, err)
}

func TestValidateLeaf_NilExpression(t *testing.T) {
	t.Parallel()
	err := validateKeyExpression(nil, orderDescriptor())
	requireNoError(t, err)
}

// ---------------------------------------------------------------------------
// 6. validateKeyExpression on KeyWithValueExpression
// ---------------------------------------------------------------------------

func TestValidateKeyWithValue_SplitPointGreaterThanInnerColumns(t *testing.T) {
	t.Parallel()
	// Field("price") has ColumnSize()=1, splitPoint=5 → error
	expr := KeyWithValue(Field("price"), 5)
	err := validateKeyExpression(expr, orderDescriptor())
	requireKeyExpressionError(t, err)
}

func TestValidateKeyWithValue_SplitPointEqualsInnerColumns(t *testing.T) {
	t.Parallel()
	// Concat(Field("price"), Field("quantity")) has ColumnSize()=2, splitPoint=2 → ok (all in key, nothing in value)
	inner := Concat(Field("price"), Field("quantity"))
	expr := KeyWithValue(inner, 2)
	err := validateKeyExpression(expr, orderDescriptor())
	requireNoError(t, err)
}

func TestValidateKeyWithValue_SplitPointLessThanInnerColumns(t *testing.T) {
	t.Parallel()
	// splitPoint=1 on a 2-column inner → 1 key column, 1 value column → valid
	inner := Concat(Field("price"), Field("quantity"))
	expr := KeyWithValue(inner, 1)
	err := validateKeyExpression(expr, orderDescriptor())
	requireNoError(t, err)
}

func TestValidateKeyWithValue_InvalidInnerExpression(t *testing.T) {
	t.Parallel()
	// Even with valid splitPoint, invalid inner field → error
	inner := Concat(Field("price"), Field("nonexistent"))
	expr := KeyWithValue(inner, 1)
	err := validateKeyExpression(expr, orderDescriptor())
	requireKeyExpressionError(t, err)
}

func TestValidateKeyWithValue_SplitPointZero(t *testing.T) {
	t.Parallel()
	// splitPoint=0, ColumnSize()=1 → 0 <= 1 → passes splitPoint check, validates inner
	expr := KeyWithValue(Field("price"), 0)
	err := validateKeyExpression(expr, orderDescriptor())
	requireNoError(t, err)
}

// ---------------------------------------------------------------------------
// 7. validateKeyExpression on SplitKeyExpression
// ---------------------------------------------------------------------------

func TestValidateSplit_JoinedHasNotOneColumn(t *testing.T) {
	t.Parallel()
	// Concat(Field("price"), Field("quantity")) has ColumnSize()=2 → error
	joined := Concat(Field("price"), Field("quantity"))
	expr := &SplitKeyExpression{joined: joined, splitSize: 2}
	err := validateKeyExpression(expr, orderDescriptor())
	requireKeyExpressionError(t, err)
}

func TestValidateSplit_JoinedDoesNotCreateDuplicates(t *testing.T) {
	t.Parallel()
	// Field("price") has ColumnSize()=1 but createsDuplicates=false → error
	expr := &SplitKeyExpression{joined: Field("price"), splitSize: 2}
	err := validateKeyExpression(expr, orderDescriptor())
	requireKeyExpressionError(t, err)
}

func TestValidateSplit_ValidSplit(t *testing.T) {
	t.Parallel()
	// FanOut("tags") has ColumnSize()=1, createsDuplicates=true → valid
	joined := FanOut("tags")
	expr := &SplitKeyExpression{joined: joined, splitSize: 2}
	err := validateKeyExpression(expr, orderDescriptor())
	requireNoError(t, err)
}

func TestValidateSplit_ValidSplitButInvalidInner(t *testing.T) {
	t.Parallel()
	// FanOut("nonexistent") has ColumnSize()=1, createsDuplicates=true, but field doesn't exist
	joined := FanOut("nonexistent")
	expr := &SplitKeyExpression{joined: joined, splitSize: 2}
	err := validateKeyExpression(expr, orderDescriptor())
	requireKeyExpressionError(t, err)
}

// ---------------------------------------------------------------------------
// 8. validateKeyExpression on FunctionKeyExpression
// ---------------------------------------------------------------------------

func TestValidateFunction_ValidatesArgumentsRecursively(t *testing.T) {
	t.Parallel()
	// bitnot is a registered function of one argument.
	// Arguments are a Field("price") which is valid on Order
	expr := FunctionExpr("bitnot", Field("price"))
	err := validateKeyExpression(expr, orderDescriptor())
	requireNoError(t, err)
}

func TestValidateFunction_InvalidArguments(t *testing.T) {
	t.Parallel()
	// Arguments reference a nonexistent field → error
	expr := FunctionExpr("bitnot", Field("nonexistent"))
	err := validateKeyExpression(expr, orderDescriptor())
	requireKeyExpressionError(t, err)
}

func TestValidateFunction_CompositeArguments(t *testing.T) {
	t.Parallel()
	expr := FunctionExpr("add", Concat(Field("order_id"), Field("price")))
	err := validateKeyExpression(expr, orderDescriptor())
	requireNoError(t, err)
}

func TestValidateFunction_CompositeArgumentsWithInvalid(t *testing.T) {
	t.Parallel()
	expr := FunctionExpr("add", Concat(Field("order_id"), Field("nope")))
	err := validateKeyExpression(expr, orderDescriptor())
	requireKeyExpressionError(t, err)
}

// ---------------------------------------------------------------------------
// 9. Build() validations
// ---------------------------------------------------------------------------

func TestBuild_NoRecordTypes(t *testing.T) {
	t.Parallel()
	b := NewRecordMetaDataBuilder()
	_, err := b.Build()
	requireMetaDataError(t, err)

	var me *MetaDataError
	if !errors.As(err, &me) {
		t.Fatalf("expected MetaDataError")
	}
	if me.Message != "No record types defined in meta-data" {
		t.Fatalf("unexpected message: %s", me.Message)
	}
}

func TestBuild_PrimaryKeyReferencingNonExistentField(t *testing.T) {
	t.Parallel()
	b := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
	b.GetRecordType("Order").SetPrimaryKey(Field("nonexistent_field"))
	b.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
	b.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))

	// Java's InvalidExpressionException from primaryKey.validate, unwrapped.
	_, err := b.Build()
	requireKeyExpressionError(t, err)
	if err.Error() != "Descriptor Order does not have field: nonexistent_field" {
		t.Fatalf("Build = %v", err)
	}
}

func TestBuild_IndexReferencingNonExistentField(t *testing.T) {
	t.Parallel()
	b := baseBuilder()
	b.AddIndex("Order", NewIndex("bad_idx", Field("nonexistent_field")))

	_, err := b.Build()
	requireKeyExpressionError(t, err)
	if err.Error() != "Descriptor Order does not have field: nonexistent_field" {
		t.Fatalf("Build = %v", err)
	}
}

func TestBuild_UniversalIndexMissingFromOneType(t *testing.T) {
	t.Parallel()
	b := baseBuilder()
	// "name" exists on Customer but not on Order
	b.AddUniversalIndex(NewIndex("universal_name", Field("name")))

	_, err := b.Build()
	requireKeyExpressionError(t, err)
	if err.Error() != "Descriptor Order does not have field: name" {
		t.Fatalf("Build = %v", err)
	}
}

func TestBuild_ValidMetadataBuildsSuccessfully(t *testing.T) {
	t.Parallel()
	b := baseBuilder()
	// "price" exists on all three record types (Order, Customer, TypedRecord)
	b.AddUniversalIndex(NewIndex("universal_price", Field("price")))

	md, err := b.Build()
	requireNoError(t, err)
	if md == nil {
		t.Fatal("expected non-nil metadata")
	}
	if md.GetIndex("universal_price") == nil {
		t.Fatal("expected universal_price index")
	}
}

func TestBuild_PrimaryKeyNil(t *testing.T) {
	t.Parallel()
	b := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
	// Don't set primary key for Order
	b.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
	b.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))

	_, err := b.Build()
	requireMetaDataError(t, err)
}

func TestBuild_PrimaryKeyWithDuplicates(t *testing.T) {
	t.Parallel()
	b := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
	// FanOut on PK → should fail (creates duplicates)
	b.GetRecordType("Order").SetPrimaryKey(FanOut("tags"))
	b.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
	b.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))

	_, err := b.Build()
	requireMetaDataError(t, err)
}

func TestBuild_PrimaryKeyEmptyColumnSize(t *testing.T) {
	t.Parallel()
	b := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
	// EmptyKey has ColumnSize() == 0 → should fail
	b.GetRecordType("Order").SetPrimaryKey(EmptyKey())
	b.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
	b.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))

	_, err := b.Build()
	requireMetaDataError(t, err)
}

func TestBuild_IndexOnMessageFieldWithoutNest(t *testing.T) {
	t.Parallel()
	b := baseBuilder()
	// "flower" is a message field — Field() without Nest should fail validation
	b.AddIndex("Order", NewIndex("bad_flower_idx", Field("flower")))

	// Java's Query.InvalidExpressionException.
	_, err := b.Build()
	var qe *QueryInvalidExpressionError
	if !errors.As(err, &qe) || qe.Message != "flower is a nested message, but accessed as a scalar" {
		t.Fatalf("Build = %v (%T)", err, err)
	}
}

func TestBuild_IndexWithNestIsValid(t *testing.T) {
	t.Parallel()
	b := baseBuilder()
	b.AddIndex("Order", NewIndex("flower_type_idx", Nest("flower", Field("type"))))

	md, err := b.Build()
	requireNoError(t, err)
	if md.GetIndex("flower_type_idx") == nil {
		t.Fatal("expected flower_type_idx index")
	}
}

func TestBuild_CompositePrimaryKey(t *testing.T) {
	t.Parallel()
	b := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
	b.GetRecordType("Order").SetPrimaryKey(Concat(Field("order_id"), Field("price")))
	b.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
	b.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))

	md, err := b.Build()
	requireNoError(t, err)
	if md == nil {
		t.Fatal("expected non-nil metadata")
	}
}

func TestBuild_RecordTypeKeyInPrimaryKey(t *testing.T) {
	t.Parallel()
	b := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
	b.GetRecordType("Order").SetPrimaryKey(Concat(RecordTypeKey(), Field("order_id")))
	b.GetRecordType("Customer").SetPrimaryKey(Concat(RecordTypeKey(), Field("customer_id")))
	b.GetRecordType("TypedRecord").SetPrimaryKey(Concat(RecordTypeKey(), Field("id")))

	md, err := b.Build()
	requireNoError(t, err)
	if md == nil {
		t.Fatal("expected non-nil metadata")
	}
}

func TestBuild_CountIndexWithoutGrouping(t *testing.T) {
	t.Parallel()
	b := baseBuilder()
	// COUNT index without GroupingKeyExpression → Java's validateGrouping(0)
	// refuses it in code (IndexValidator.java:58-77).
	b.AddIndex("Order", NewCountIndex("bad_count", Field("price")))

	_, err := b.Build()
	var keyErr *KeyExpressionError
	if !errors.As(err, &keyErr) || keyErr.Message != "index type requires grouping" {
		t.Fatalf("Build = %v (%T), want Java's KeyExpression.InvalidExpressionException", err, err)
	}
}

func TestBuild_CountIndexWithGrouping(t *testing.T) {
	t.Parallel()
	b := baseBuilder()
	b.AddIndex("Order", NewCountIndex("good_count", GroupAll(Field("price"))))

	md, err := b.Build()
	requireNoError(t, err)
	if md.GetIndex("good_count") == nil {
		t.Fatal("expected good_count index")
	}
}

// ---------------------------------------------------------------------------
// Cross-cutting: various expression compositions
// ---------------------------------------------------------------------------

func TestValidate_CompositeWithNesting(t *testing.T) {
	t.Parallel()
	expr := Concat(Field("order_id"), Nest("flower", Field("type")))
	err := validateKeyExpression(expr, orderDescriptor())
	requireNoError(t, err)
}

func TestValidate_CompositeWithFanOutAndNesting(t *testing.T) {
	t.Parallel()
	expr := Concat(FanOut("tags"), Nest("flower", Field("color")))
	err := validateKeyExpression(expr, orderDescriptor())
	requireNoError(t, err)
}

func TestValidate_GroupingWithNesting(t *testing.T) {
	t.Parallel()
	expr := GroupBy(Field("price"), Nest("flower", Field("type")))
	err := validateKeyExpression(expr, orderDescriptor())
	requireNoError(t, err)
}

func TestValidate_KeyWithValueWithNesting(t *testing.T) {
	t.Parallel()
	inner := Concat(Nest("flower", Field("type")), Field("price"))
	expr := KeyWithValue(inner, 1)
	err := validateKeyExpression(expr, orderDescriptor())
	requireNoError(t, err)
}

func TestValidate_DeeplyNestedInvalid(t *testing.T) {
	t.Parallel()
	// GroupBy wrapping Concat wrapping Nest wrapping Field("nonexistent")
	expr := GroupBy(
		Concat(Nest("flower", Field("nonexistent")), Field("price")),
	)
	err := validateKeyExpression(expr, orderDescriptor())
	requireKeyExpressionError(t, err)
}

func TestValidate_VersionKeyInComposite(t *testing.T) {
	t.Parallel()
	expr := Concat(Field("order_id"), VersionKey())
	err := validateKeyExpression(expr, orderDescriptor())
	requireNoError(t, err)
}

func TestValidate_RecordTypeKeyInComposite(t *testing.T) {
	t.Parallel()
	expr := Concat(RecordTypeKey(), Field("order_id"))
	err := validateKeyExpression(expr, orderDescriptor())
	requireNoError(t, err)
}

func TestValidate_CustomerDescriptor_ValidField(t *testing.T) {
	t.Parallel()
	err := validateKeyExpression(Field("name"), customerDescriptor())
	requireNoError(t, err)
}

func TestValidate_CustomerDescriptor_OrderFieldMissing(t *testing.T) {
	t.Parallel()
	// "order_id" exists on Order, not Customer
	err := validateKeyExpression(Field("order_id"), customerDescriptor())
	requireKeyExpressionError(t, err)
}

func TestValidate_LiteralInComposite(t *testing.T) {
	t.Parallel()
	expr := Concat(Literal("prefix"), Field("order_id"))
	err := validateKeyExpression(expr, orderDescriptor())
	requireNoError(t, err)
}

func TestValidate_FunctionInComposite(t *testing.T) {
	t.Parallel()
	expr := Concat(Field("order_id"), FunctionExpr("bitnot", Field("price")))
	err := validateKeyExpression(expr, orderDescriptor())
	requireNoError(t, err)
}

func TestValidatedKeyExpressionFields(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		expr KeyExpression
		want []protoreflect.Name
	}{
		{"nil", nil, nil},
		{"empty", EmptyKey(), nil},
		{"literal", Literal("constant"), nil},
		{"record type", RecordTypeKey(), nil},
		{"version", VersionKey(), nil},
		{"field", Field("price"), []protoreflect.Name{"price"}},
		{"nested", Nest("flower", Field("type")), []protoreflect.Name{"type"}},
		{"composite", Concat(Field("price"), Nest("flower", Field("type"))), []protoreflect.Name{"price", "type"}},
		{"grouped", GroupBy(Nest("flower", Field("type")), Field("order_id")), []protoreflect.Name{"order_id", "type"}},
		{"function", FunctionExpr("identity", Field("price")), []protoreflect.Name{"price"}},
		{"dimensions", Dimensions(Concat(Field("coord_x"), Field("coord_y")), 0, 2), []protoreflect.Name{"coord_x", "coord_y"}},
		{"covering", KeyWithValue(Concat(Field("price"), Field("quantity")), 1), []protoreflect.Name{"price", "quantity"}},
		{"split", Split(FanOut("tags"), 2), []protoreflect.Name{"tags"}},
		{"list", ListExpr(Concat(Field("price"), Field("quantity")), Literal(1)), []protoreflect.Name{"price", "quantity"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			fields, err := validateKeyExpressionFields(tc.expr, orderDescriptor())
			requireNoError(t, err)
			if len(fields) != len(tc.want) {
				t.Fatalf("got %d fields, want %v", len(fields), tc.want)
			}
			for i, field := range fields {
				if field.Name() != tc.want[i] {
					t.Fatalf("field %d = %s, want %s", i, field.FullName(), tc.want[i])
				}
				if field.Name() == "type" && field.ContainingMessage().Name() != "Flower" {
					t.Fatalf("nested leaf must retain its actual descriptor: %s", field.FullName())
				}
			}
		})
	}
}

// The dimension positions index the validated field list, which has no entry
// for a literal or a version: the fields after one take its place, as in Java.
// A position outside the list, a negative prefix among them, is refused as a
// column that is not INT64 (Java: IndexOutOfBoundsException), never a panic.
func TestValidateDimensionsPositionsIndexTheFieldList(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name string
		expr KeyExpression
		ok   bool
	}{
		{"after a literal", Dimensions(Concat(Literal(int64(7)), Field("coord_x"), Field("coord_y")), 0, 2), true},
		{"after a version", Dimensions(Concat(VersionKey(), Field("coord_x")), 0, 1), true},
		{"past the fields", Dimensions(Concat(Field("coord_x"), Literal(int64(7))), 0, 2), false},
		{"a negative prefix", Dimensions(Concat(Field("coord_x"), Field("coord_y")), -1, 2), false},
		{"a negative prefix past the start alone", Dimensions(Field("coord_x"), -1, 1), false},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			err := validateKeyExpression(c.expr, orderDescriptor())
			if c.ok {
				requireNoError(t, err)
				return
			}
			var kerr *KeyExpressionError
			if !errors.As(err, &kerr) || kerr.Message != "the declared dimension columns have to be of type INT64" {
				t.Fatalf("err = %v, want the INT64 refusal", err)
			}
		})
	}
}
