package recordlayer

import (
	"errors"
	"fmt"
	"math"
	"math/big"
	"strings"
	"testing"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/descriptorpb"

	"fdb.dev/gen"
	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
)

// namedKey is a Go type with none of the tuple forms: Java's default arm.
type namedKey string

// TestSubspaceKeyIdentity pins subspaceKeyIdentity against Java's
// TupleTypeUtil.toTupleEquivalentValue followed by Object.equals, one row per
// normalization arm and per equality rule.
func TestSubspaceKeyIdentity(t *testing.T) {
	t.Parallel()
	nanA := math.Float64frombits(0x7ff8000000000001)
	nanB := math.Float64frombits(0x7ff8000000000002)
	nan32A := math.Float32frombits(0x7fc00001)
	nan32B := math.Float32frombits(0x7fc00002)
	maxLong := new(big.Int).SetInt64(math.MaxInt64)
	minLong := new(big.Int).SetInt64(math.MinInt64)
	twoTo63 := new(big.Int).Lsh(big.NewInt(1), 63)
	vs := tuple.Versionstamp{TransactionVersion: [10]byte{1, 2, 3}, UserVersion: 7}
	var raw [VersionBytes]byte
	copy(raw[:], vs.TransactionVersion[:])
	raw[GlobalVersionBytes], raw[GlobalVersionBytes+1] = 0, 7
	version := &FDBRecordVersion{raw: raw, complete: true}
	for _, c := range []struct {
		name  string
		a, b  any
		equal bool
	}{
		{"nil and nil", nil, nil, true},
		{"nil and empty string", nil, "", false},
		{"int and int64 (Integer and Long are one Long)", int(5), int64(5), true},
		{"int8 and int64", int8(5), int64(5), true},
		{"int16 and int64", int16(5), int64(5), true},
		{"int32 and int64", int32(5), int64(5), true},
		{"uint8 and int64", uint8(5), int64(5), true},
		{"uint16 and int64", uint16(5), int64(5), true},
		{"uint32 and int64", uint32(5), int64(5), true},
		{"uint and int64", uint(5), int64(5), true},
		{"uint64 MaxInt64 and int64 MaxInt64", uint64(math.MaxInt64), int64(math.MaxInt64), true},
		{"a string is not an integer", "5", int64(5), false},
		{"a double is not an integer", float64(5), int64(5), false},
		{"a big.Int inside the Long range is that Long", big.NewInt(5), int64(5), true},
		{"a big.Int value and pointer", *big.NewInt(-9), big.NewInt(-9), true},
		{"a BigInteger AT Long.MAX_VALUE stays a BigInteger", maxLong, int64(math.MaxInt64), false},
		{"a BigInteger AT Long.MIN_VALUE stays a BigInteger", minLong, int64(math.MinInt64), false},
		{"two BigIntegers at a bound", maxLong, new(big.Int).Set(maxLong), true},
		{"a uint64 above MaxInt64 is that BigInteger", uint64(1) << 63, twoTo63, true},
		{"byte arrays equal by content (ByteString)", []byte("pb"), []byte("pb"), true},
		{"byte arrays of different content", []byte("pb"), []byte("pc"), false},
		{"bytes and a string of the same content", []byte("pb"), "pb", false},
		{"an fdb.Key is bytes", fdb.Key("pb"), []byte("pb"), true},
		{"a nil and an empty byte array both pack as empty bytes", []byte(nil), []byte{}, true},
		{"a Tuple and a List of equal items", tuple.Tuple{"a", int(1)}, []any{"a", int64(1)}, true},
		{"lists of different lengths", tuple.Tuple{"a"}, tuple.Tuple{"a", "b"}, false},
		{"list boundaries are kept", tuple.Tuple{"a", "b"}, tuple.Tuple{"ab"}, false},
		{"list items keep their type", tuple.Tuple{[]byte("a")}, tuple.Tuple{"a"}, false},
		{"nested lists", tuple.Tuple{tuple.Tuple{int32(1)}, nil}, tuple.Tuple{[]any{int64(1)}, nil}, true},
		{"a list is not its only item", tuple.Tuple{"a"}, "a", false},
		{"every NaN equals every NaN (Double.equals)", nanA, nanB, true},
		{"0.0 and -0.0 differ (Double.equals)", 0.0, math.Copysign(0, -1), false},
		{"every float NaN equals every float NaN (Float.equals)", nan32A, nan32B, true},
		{"a Float is not a Double", float32(1), float64(1), false},
		{"booleans", true, true, true},
		{"different booleans", true, false, false},
		{"UUIDs", tuple.UUID{1}, tuple.UUID{1}, true},
		{"different UUIDs", tuple.UUID{1}, tuple.UUID{2}, false},
		{"versionstamps", vs, tuple.Versionstamp{TransactionVersion: vs.TransactionVersion, UserVersion: 7}, true},
		{"an FDBRecordVersion is its versionstamp", version, vs, true},
		{"a proto enum is its number as a Long", descriptorpb.FieldDescriptorProto_TYPE_INT64, int64(3), true},
		{"Java's default arm: same type and value", namedKey("x"), namedKey("x"), true},
		{"Java's default arm: not the string", namedKey("x"), "x", false},
		{"a value Go cannot compare equals nothing", []int{1}, []int{1}, false},
		{"a list holding one equals nothing", tuple.Tuple{[]int{1}}, tuple.Tuple{[]int{1}}, false},
	} {
		if got := subspaceKeysEqual(c.a, c.b); got != c.equal {
			t.Errorf("%s: subspaceKeysEqual(%#v, %#v) = %t, want %t", c.name, c.a, c.b, got, c.equal)
		}
		if got := subspaceKeysEqual(c.b, c.a); got != c.equal {
			t.Errorf("%s: not symmetric: subspaceKeysEqual(%#v, %#v) = %t, want %t", c.name, c.b, c.a, got, c.equal)
		}
	}

	// The identity is a map key: a key of a type Go cannot compare must not
	// panic there, and must not collide with itself.
	m := map[any]int{}
	m[subspaceKeyIdentity([]int{1})] = 1
	m[subspaceKeyIdentity([]int{1})] = 2
	if len(m) != 2 {
		t.Fatalf("two identities of an incomparable key collided: %d entries", len(m))
	}
}

func identityTestMetaData(version int, configure func(b *RecordMetaDataBuilder)) (*RecordMetaData, error) {
	b := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
	b.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
	b.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
	b.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))
	configure(b)
	b.SetVersion(version)
	return b.Build()
}

// TestMetaDataValidatorSubspaceKeys pins the subspace-key checks of Java's
// MetaDataValidator.validateCurrentAndFormerIndexes (MetaDataValidator.java:
// 103-165): index against index, former index against former index, and index
// against former index, each under Java's normalized equality and with Java's
// message.
func TestMetaDataValidatorSubspaceKeys(t *testing.T) {
	t.Parallel()
	index := func(name string, key any) *Index {
		idx := NewIndex(name, Field("price"))
		if key != nil {
			idx.SetSubspaceKey(key)
		}
		return idx
	}
	former := func(key any, name string) *FormerIndex {
		return &FormerIndex{SubspaceKey: key, AddedVersion: 1, RemovedVersion: 2, FormerName: name}
	}
	for _, c := range []struct {
		name      string
		indexes   []*Index
		formers   []*FormerIndex
		wantError string
	}{
		{
			name:    "a bytes key beside a string key of the same content: two prefixes",
			indexes: []*Index{index("pb", nil), index("other", []byte("pb"))},
		},
		{
			name:      "an int key and an int64 key of one value collide",
			indexes:   []*Index{index("a", int(5)), index("b", int64(5))},
			wantError: "Same subspace key 5 used by both b and a",
		},
		{
			name:      "two NaN keys collide, whatever their payloads",
			indexes:   []*Index{index("a", math.Float64frombits(0x7ff8000000000001)), index("b", math.Float64frombits(0x7ff8000000000002))},
			wantError: "Same subspace key NaN used by both b and a",
		},
		{
			name:    "0.0 and -0.0 are two prefixes",
			indexes: []*Index{index("a", 0.0), index("b", math.Copysign(0, -1))},
		},
		{
			name:      "two former indexes with one key",
			formers:   []*FormerIndex{former(int64(9), "gone"), former(int(9), "")},
			wantError: "Same subspace key 9 used by two former indexes <unknown> and gone",
		},
		{
			name:      "an index on a former index's key",
			indexes:   []*Index{index("a", int64(9))},
			formers:   []*FormerIndex{former(int(9), "gone")},
			wantError: "Same subspace key 9 used by index a and former index gone",
		},
		{
			name:      "an index on an unnamed former index's key",
			indexes:   []*Index{index("a", int64(9))},
			formers:   []*FormerIndex{former(int(9), "")},
			wantError: "Same subspace key 9 used by index a and former index",
		},
		{
			name:    "a former index on a bytes key beside an index on the string",
			indexes: []*Index{index("a", "k")},
			formers: []*FormerIndex{former([]byte("k"), "gone")},
		},
	} {
		_, err := identityTestMetaData(5, func(b *RecordMetaDataBuilder) {
			for _, idx := range c.indexes {
				b.AddIndex("Order", idx)
			}
			b.formerIndexes = append(b.formerIndexes, c.formers...)
		})
		if c.wantError == "" {
			if err != nil {
				t.Errorf("%s: Build: %v", c.name, err)
			}
			continue
		}
		var mdErr *MetaDataError
		if !errors.As(err, &mdErr) || mdErr.Message != c.wantError {
			t.Errorf("%s: Build error %v, want MetaDataError %q", c.name, err, c.wantError)
		}
	}
}

// TestEvolutionPairsIndexesBySubspaceKey pins Java's
// validateCurrentAndFormerIndexes (MetaDataEvolutionValidator.java:479-555):
// indexes and former indexes are paired by subspace key, not by name. The
// conformance spec "Subspace-key pairing in meta-data evolution" runs the same
// shapes through Java's validator.
func TestEvolutionPairsIndexesBySubspaceKey(t *testing.T) {
	t.Parallel()
	mustBuild := func(version int, configure func(b *RecordMetaDataBuilder)) *RecordMetaData {
		t.Helper()
		md, err := identityTestMetaData(version, configure)
		if err != nil {
			t.Fatalf("Build: %v", err)
		}
		return md
	}
	versioned := func(name string, key any, added, modified int) *Index {
		idx := NewIndex(name, Field("price"))
		if key != nil {
			idx.SetSubspaceKey(key)
		}
		idx.AddedVersion, idx.LastModifiedVersion = added, modified
		return idx
	}
	old := mustBuild(3, func(b *RecordMetaDataBuilder) { b.AddIndex("Order", versioned("a", nil, 1, 1)) })
	for _, c := range []struct {
		name      string
		validator *MetaDataEvolutionValidator
		new       *RecordMetaData
		wantError string
	}{
		{
			name:      "same name, another subspace key: the old key's entries are orphaned",
			new:       mustBuild(4, func(b *RecordMetaDataBuilder) { b.AddIndex("Order", versioned("a", "a2", 1, 1)) }),
			wantError: "index missing in new meta-data",
		},
		{
			name:      "same subspace key, another name",
			new:       mustBuild(4, func(b *RecordMetaDataBuilder) { b.AddIndex("Order", versioned("b", "a", 1, 1)) }),
			wantError: "index name changed",
		},
		{
			name: "the index moved to a new key, and its old key became a former index",
			new: mustBuild(6, func(b *RecordMetaDataBuilder) {
				b.AddIndex("Order", versioned("a", "a2", 5, 5))
				b.formerIndexes = append(b.formerIndexes, &FormerIndex{SubspaceKey: "a", AddedVersion: 1, RemovedVersion: 5, FormerName: "a"})
			}),
		},
		{
			name: "a former index naming another index than the one it replaces",
			new: mustBuild(6, func(b *RecordMetaDataBuilder) {
				b.formerIndexes = append(b.formerIndexes, &FormerIndex{SubspaceKey: "a", AddedVersion: 1, RemovedVersion: 5, FormerName: "x"})
			}),
			validator: NewMetaDataEvolutionValidator().SetAllowMissingFormerIndexNames(true).Build(),
			wantError: "former index has different name than old index",
		},
		{
			name: "an unnamed former index, with missing names allowed",
			new: mustBuild(6, func(b *RecordMetaDataBuilder) {
				b.formerIndexes = append(b.formerIndexes, &FormerIndex{SubspaceKey: "a", AddedVersion: 1, RemovedVersion: 5})
			}),
			validator: NewMetaDataEvolutionValidator().SetAllowMissingFormerIndexNames(true).Build(),
		},
	} {
		validator := c.validator
		if validator == nil {
			validator = DefaultMetaDataEvolutionValidator()
		}
		err := validator.Validate(old, c.new)
		if c.wantError == "" {
			if err != nil {
				t.Errorf("%s: Validate: %v", c.name, err)
			}
			continue
		}
		var evolErr *MetaDataEvolutionError
		if !errors.As(err, &evolErr) || !strings.HasPrefix(evolErr.Message, c.wantError) {
			t.Errorf("%s: Validate error %v, want a MetaDataEvolutionError starting %q", c.name, err, c.wantError)
		}
	}

	// A former index kept from the old meta-data may not be reused by an index.
	withFormer := mustBuild(6, func(b *RecordMetaDataBuilder) {
		b.formerIndexes = append(b.formerIndexes, &FormerIndex{SubspaceKey: "a", AddedVersion: 1, RemovedVersion: 5, FormerName: "a"})
	})
	reused := mustBuild(8, func(b *RecordMetaDataBuilder) { b.AddIndex("Order", versioned("z", "a", 7, 7)) })
	var evolErr *MetaDataEvolutionError
	if err := ValidateEvolution(withFormer, reused); !errors.As(err, &evolErr) ||
		!strings.HasPrefix(evolErr.Message, "former index key used for new index in meta-data") {
		t.Errorf("reusing a former index's key: Validate error %v", err)
	}
}

// TestKeyExpressionEqualsArms pins the arms Java's KeyExpression.equals has
// and keyExpressionEquals once lacked.
func TestKeyExpressionEqualsArms(t *testing.T) {
	t.Parallel()
	dims := func(prefix int) KeyExpression {
		return Dimensions(Concat(Field("coord_x"), Field("coord_y")), prefix, 2)
	}
	shared := dims(0)
	for _, c := range []struct {
		name  string
		a, b  KeyExpression
		equal bool
	}{
		{"one Dimensions object", shared, shared, true},
		{"two equal Dimensions", dims(0), dims(0), true},
		{"Dimensions with another prefix size", dims(0), dims(1), false},
		{"Dimensions and its whole key", dims(0), Concat(Field("coord_x"), Field("coord_y")), false},
		{"cardinality and cardinality", CardinalityExpr(Field("tags")), CardinalityExpr(Field("tags")), true},
		{"cardinality and a function of the same name (instanceof)", CardinalityExpr(Field("tags")), FunctionExpr(FunctionNameCardinality, Field("tags")), true},
		{"a function and cardinality of the same name (instanceof)", FunctionExpr(FunctionNameCardinality, Field("tags")), CardinalityExpr(Field("tags")), true},
		{"cardinality over another field", CardinalityExpr(Field("tags")), CardinalityExpr(Field("names")), false},
	} {
		if got := keyExpressionEquals(c.a, c.b); got != c.equal {
			t.Errorf("%s: keyExpressionEquals = %t, want %t", c.name, got, c.equal)
		}
	}
}

// TestIndexEqualsJavaFieldByField varies one field of Java's Index.equals at a
// time (Index.java:695-711).
func TestIndexEqualsJavaFieldByField(t *testing.T) {
	t.Parallel()
	base := func() *Index {
		idx := NewIndex("i", Field("price"))
		idx.AddedVersion, idx.LastModifiedVersion = 1, 2
		idx.SetSubspaceKey(int64(5))
		idx.Options = map[string]string{"k": "v"}
		idx.primaryKeyComponentPositions = []int{-1, 0}
		return idx
	}
	window := func(size int32) *gen.Predicate {
		return &gen.Predicate{RowNumberWindowPredicate: &gen.RowNumberWindowPredicate{
			OrderingField: []string{"price"}, Size: &size,
			Direction:       gen.RowNumberWindowPredicate_ASC.Enum(),
			PartitionFields: []*gen.FieldPath{{Field: []string{"customer_id"}}},
		}}
	}
	constant := &gen.Predicate{ConstantPredicate: &gen.ConstantPredicate{Value: gen.ConstantPredicate_TRUE.Enum()}}
	behindConstant := func(p *gen.Predicate) *gen.Predicate {
		p.ConstantPredicate = &gen.ConstantPredicate{Value: gen.ConstantPredicate_TRUE.Enum()}
		return p
	}
	withProto := func(p *gen.Predicate) func(*Index) {
		return func(i *Index) { i.predicateProto = p }
	}
	sharedConstant := withProto(constant)
	for _, c := range []struct {
		name   string
		change func(*Index)
		equal  bool
	}{
		{"unchanged", func(*Index) {}, true},
		{"name", func(i *Index) { i.Name = "j" }, false},
		{"type", func(i *Index) { i.Type = IndexTypeCount }, false},
		{"a structurally equal root", func(i *Index) { i.RootExpression = Field("price") }, true},
		{"another root", func(i *Index) { i.RootExpression = Field("quantity") }, false},
		{"an int subspace key equal to the int64", func(i *Index) { i.SetSubspaceKey(int(5)) }, true},
		{"another subspace key", func(i *Index) { i.SetSubspaceKey(int64(6)) }, false},
		{"added version", func(i *Index) { i.AddedVersion = 0 }, false},
		{"last modified version", func(i *Index) { i.LastModifiedVersion = 3 }, false},
		{"positions changed", func(i *Index) { i.primaryKeyComponentPositions = []int{-1, 1} }, false},
		{"positions absent (Arrays.equals(null, a) is false)", func(i *Index) { i.primaryKeyComponentPositions = nil }, false},
		{"equal options in another map", func(i *Index) { i.Options = map[string]string{"k": "v"} }, true},
		{"another option value", func(i *Index) { i.Options = map[string]string{"k": "w"} }, false},
		{"an extra option", func(i *Index) { i.Options = map[string]string{"k": "v", "x": "y"} }, false},
		{"a predicate on one side", withProto(constant), false},
	} {
		a, b := base(), base()
		c.change(b)
		if got := a.equalsJava(b); got != c.equal {
			t.Errorf("%s: equalsJava = %t, want %t", c.name, got, c.equal)
		}
		if got := b.equalsJava(a); got != c.equal {
			t.Errorf("%s: not symmetric: equalsJava = %t, want %t", c.name, got, c.equal)
		}
	}

	// The type is compared as spelled: min_ever maintains what min_ever_long
	// does, and is still another type string.
	a, b := base(), base()
	a.Type, b.Type = IndexTypeMinEverLong, IndexTypeMinEver
	if a.equalsJava(b) {
		t.Error("min_ever equals min_ever_long")
	}

	// Bytes subspace keys are compared by content.
	a, b = base(), base()
	a.SetSubspaceKey([]byte("pb"))
	b.SetSubspaceKey([]byte("pb"))
	if !a.equalsJava(b) {
		t.Error("two Indexes with equal bytes subspace keys differ")
	}

	// Predicates: the same stored proto is one predicate object, as Index's
	// copy constructor shares Java's; two decoded And/Or/Not/Constant/Value
	// predicates are two objects, and only row-number windows compare their
	// fields.
	for _, c := range []struct {
		name   string
		pa, pb func(*Index)
		equal  bool
	}{
		{"one constant predicate", sharedConstant, sharedConstant, true},
		{"two equal constant predicates", withProto(constant), withProto(&gen.Predicate{ConstantPredicate: &gen.ConstantPredicate{Value: gen.ConstantPredicate_TRUE.Enum()}}), false},
		{"two equal row-number windows", withProto(window(3)), withProto(window(3)), true},
		{"two row-number windows of different sizes", withProto(window(3)), withProto(window(4)), false},
		{"a window behind a constant field is Java's constant predicate", withProto(behindConstant(window(3))), withProto(behindConstant(window(3))), false},
		{"a Go closure on both sides", func(i *Index) { i.Predicate = func(proto.Message) bool { return true } }, func(i *Index) { i.Predicate = func(proto.Message) bool { return true } }, false},
	} {
		a, b := base(), base()
		c.pa(a)
		c.pb(b)
		if got := a.equalsJava(b); got != c.equal {
			t.Errorf("predicate %s: equalsJava = %t, want %t", c.name, got, c.equal)
		}
	}
}

// TestSetSubspaceKeyNormalizesAsJava pins Index.setSubspaceKey's
// normalization (Index.java:88-97, :413-415): the stored key is the tuple
// equivalent, so it packs, and a nil key is refused and the key kept.
func TestSetSubspaceKeyNormalizesAsJava(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name string
		key  any
		want any
	}{
		{"an int32 is a Long", int32(7), int64(7)},
		{"a uint16 is a Long", uint16(7), int64(7)},
		{"a list is a Tuple of normalized items", []any{int32(1), "a"}, tuple.Tuple{int64(1), "a"}},
		{"a proto enum is its number", descriptorpb.FieldDescriptorProto_TYPE_INT64, int64(3)},
		{"a big.Int in the Long range is a Long", big.NewInt(-4), int64(-4)},
		{"a string is kept", "k", "k"},
	} {
		idx := NewIndex("i", Field("price")).SetSubspaceKey(c.key)
		got := idx.SubspaceTupleKey()
		if !subspaceKeysEqual(got, c.want) || fmt.Sprintf("%T", got) != fmt.Sprintf("%T", c.want) {
			t.Errorf("%s: stored %#v (%T), want %#v (%T)", c.name, got, got, c.want, c.want)
		}
		// The stored key packs: the encoder has no case for the raw forms.
		if packed := (tuple.Tuple{got}).Pack(); len(packed) == 0 {
			t.Errorf("%s: packed to nothing", c.name)
		}
	}

	// A copied byte array: the caller's array may change afterwards.
	raw := []byte("ab")
	idx := NewIndex("i", Field("price")).SetSubspaceKey(raw)
	raw[0] = 'z'
	if got := idx.SubspaceTupleKey().([]byte); string(got) != "ab" {
		t.Errorf("the stored bytes key follows the caller's array: %q", got)
	}

	// nil is refused, from Build, and the key stays what it was.
	refused := NewIndex("refused", Field("price")).SetSubspaceKey(nil)
	if refused.SubspaceTupleKey() != "refused" {
		t.Errorf("a refused nil key replaced the key: %#v", refused.SubspaceTupleKey())
	}
	_, err := identityTestMetaData(3, func(b *RecordMetaDataBuilder) { b.AddIndex("Order", refused) })
	var argErr *RecordCoreArgumentError
	if !errors.As(err, &argErr) || argErr.Error() != `Index subspace key cannot be null (index="refused", subspace_key=<nil>)` {
		t.Errorf("Build with a nil subspace key: %v, want the RecordCoreArgumentError", err)
	}

	// Normalizing changes no comparison: the identity of each normalized key
	// is the identity of the key.
	for _, key := range []any{
		int(5), int8(-5), uint32(5), uint64(math.MaxUint64), *big.NewInt(9), new(big.Int).SetInt64(math.MaxInt64),
		[]byte("pb"),
		tuple.Tuple{int16(1), []any{"x"}},
		math.NaN(), float32(1.5), true,
		tuple.UUID{3},
		descriptorpb.FieldDescriptorProto_TYPE_BOOL, fdb.Key("k"), namedKey("n"),
	} {
		if subspaceKeyIdentity(tupleEquivalentValue(key)) != subspaceKeyIdentity(key) {
			t.Errorf("normalizing %#v (%T) changed its identity", key, key)
		}
	}
}

// TestRecordCoreArgumentErrorRendersSetFields pins every arm of Error: the
// message alone, and each field only when its site set it.
func TestRecordCoreArgumentErrorRendersSetFields(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		err  *RecordCoreArgumentError
		want string
	}{
		{&RecordCoreArgumentError{Message: "m"}, "m"},
		{&RecordCoreArgumentError{Message: "m", ScanType: IndexScanByValue}, "m (scanType=" + string(IndexScanByValue) + ")"},
		{&RecordCoreArgumentError{Message: "m", IndexName: "ix"}, `m (index="ix")`},
		{&RecordCoreArgumentError{Message: "m", ScanType: IndexScanByRank, IndexName: "ix"}, "m (scanType=" + string(IndexScanByRank) + `, index="ix")`},
		{&RecordCoreArgumentError{Message: "m", IndexName: "ix", HasSubspaceKey: true}, `m (index="ix", subspace_key=<nil>)`},
		{&RecordCoreArgumentError{Message: "m", SubspaceKey: int64(4), HasSubspaceKey: true}, "m (subspace_key=4)"},
	} {
		if got := c.err.Error(); got != c.want {
			t.Errorf("Error() = %q, want %q", got, c.want)
		}
	}
}

// TestFormerIndexProtoAsJava pins FormerIndex(proto) and FormerIndex.toProto
// (FormerIndex.java:51-68, :119-133) through marshalled bytes, since the
// presence of an empty key is what the unmarshaller keeps.
func TestFormerIndexProtoAsJava(t *testing.T) {
	t.Parallel()
	read := func(p *gen.FormerIndex) (*FormerIndex, error) {
		data, err := proto.Marshal(p)
		if err != nil {
			t.Fatal(err)
		}
		var decoded gen.FormerIndex
		if err := proto.Unmarshal(data, &decoded); err != nil {
			t.Fatal(err)
		}
		return formerIndexFromProto(&decoded)
	}
	for _, c := range []struct {
		name    string
		key     []byte
		wantErr string
		class   string
	}{
		{"absent", nil, "subspace key must encode a single item tuple", "core"},
		{"present and empty", []byte{}, "subspace key must encode a single item tuple", "core"},
		{"two items", tuple.Tuple{"a", "b"}.Pack(), "subspace key must encode a single item tuple", "core"},
		{"a null item", tuple.Tuple{nil}.Pack(), "FormerIndex initialized with null subspace key", "argument"},
	} {
		_, err := read(&gen.FormerIndex{SubspaceKey: c.key, FormerName: proto.String("gone")})
		var core *RecordCoreError
		var arg *RecordCoreArgumentError
		switch {
		case c.class == "core" && errors.As(err, &core) && core.Message == c.wantErr:
		case c.class == "argument" && errors.As(err, &arg) && arg.Message == c.wantErr && arg.IndexName == "gone":
		default:
			t.Errorf("%s: %v (%T), want %s error %q", c.name, err, err, c.class, c.wantErr)
		}
	}

	// An unnamed former index writes no name, and reads back unnamed; the key
	// is always written.
	p, err := formerIndexToProto(&FormerIndex{SubspaceKey: int64(3), AddedVersion: 1, RemovedVersion: 2})
	if err != nil {
		t.Fatal(err)
	}
	if p.FormerName != nil {
		t.Errorf("an unnamed former index wrote the name %q", p.GetFormerName())
	}
	fi, err := read(p)
	if err != nil || fi.FormerName != "" || fi.SubspaceKey != int64(3) {
		t.Errorf("read back %+v, %v", fi, err)
	}
}

// TestNilSubspaceKeyIsRefusedOnEveryPath pins Java's refusal of a null
// subspace key (Index.java:413-416, FormerIndex.java:51-58) on the paths a Go
// program can take: a nil set after AddIndex, a nil set followed by a valid
// one (Java throws at the first, so the second never runs), a typed nil, a
// struct-literal index (Java's constructors always key it by its name), and a
// former index whose exported key a caller set to nil.
func TestNilSubspaceKeyIsRefusedOnEveryPath(t *testing.T) {
	t.Parallel()
	const nullKey = "Index subspace key cannot be null"
	refusedBy := func(t *testing.T, err error, msg string) {
		t.Helper()
		var argErr *RecordCoreArgumentError
		if !errors.As(err, &argErr) || argErr.Message != msg {
			t.Fatalf("Build = %v, want the RecordCoreArgumentError %q", err, msg)
		}
	}

	t.Run("a nil set after AddIndex", func(t *testing.T) {
		t.Parallel()
		_, err := identityTestMetaData(3, func(b *RecordMetaDataBuilder) {
			idx := NewIndex("late", Field("price"))
			b.AddIndex("Order", idx)
			idx.SetSubspaceKey(nil)
		})
		refusedBy(t, err, nullKey)
	})
	t.Run("a nil set is sticky", func(t *testing.T) {
		t.Parallel()
		idx := NewIndex("sticky", Field("price")).SetSubspaceKey(nil).SetSubspaceKey("k")
		if idx.SubspaceTupleKey() != "sticky" {
			t.Errorf("a set after a refused one replaced the key: %#v", idx.SubspaceTupleKey())
		}
		_, err := identityTestMetaData(3, func(b *RecordMetaDataBuilder) { b.AddIndex("Order", idx) })
		refusedBy(t, err, nullKey)
	})
	for _, typedNil := range []any{(*big.Int)(nil), (*FDBRecordVersion)(nil)} {
		t.Run(fmt.Sprintf("a typed nil %T", typedNil), func(t *testing.T) {
			t.Parallel()
			idx := NewIndex("typed", Field("price")).SetSubspaceKey(typedNil)
			if idx.SubspaceTupleKey() != "typed" {
				t.Errorf("a typed nil replaced the key: %#v", idx.SubspaceTupleKey())
			}
			_, err := identityTestMetaData(3, func(b *RecordMetaDataBuilder) { b.AddIndex("Order", idx) })
			refusedBy(t, err, nullKey)
		})
	}
	t.Run("a typed nil enum pointer", func(t *testing.T) {
		t.Parallel()
		// A pointer to a generated enum has the enum's methods; a nil one is
		// Java's null, not a panic in Number.
		idx := NewIndex("enum", Field("price")).SetSubspaceKey((*gen.Color)(nil))
		if idx.SubspaceTupleKey() != "enum" || idx.HasExplicitSubspaceKey() {
			t.Errorf("a nil enum pointer changed the index: key %#v explicit %t", idx.SubspaceTupleKey(), idx.HasExplicitSubspaceKey())
		}
		_, err := identityTestMetaData(3, func(b *RecordMetaDataBuilder) { b.AddIndex("Order", idx) })
		refusedBy(t, err, nullKey)
		// A non-nil enum pointer is its number, as Java's
		// toTupleEquivalentValue makes a ProtocolMessageEnum.
		if got := NewIndex("enum2", Field("price")).SetSubspaceKey(gen.Color_BLUE.Enum()).SubspaceTupleKey(); got != int64(2) {
			t.Errorf("an enum pointer key = %#v, want its number", got)
		}
	})
	t.Run("a refused set leaves the index as it was", func(t *testing.T) {
		t.Parallel()
		// Java normalizes before it assigns or marks (Index.java:413-416).
		idx := NewIndex("unmarked", Field("price")).SetSubspaceKey(nil)
		if idx.HasExplicitSubspaceKey() {
			t.Error("a refused set marked the key explicit")
		}
		if err := idx.SubspaceKeyError(); err == nil || err.Error() == "" {
			t.Error("SubspaceKeyError does not report the refusal")
		}
	})
	t.Run("a refused set on an index of built meta-data", func(t *testing.T) {
		t.Parallel()
		md, err := identityTestMetaData(3, func(b *RecordMetaDataBuilder) {
			b.AddIndex("Order", NewIndex("built", Field("price")))
		})
		if err != nil {
			t.Fatalf("Build: %v", err)
		}
		idx := md.GetIndex("built")
		idx.SetSubspaceKey(nil)
		// Declared divergence: no Build is left to return it; the key and the
		// explicit mark are unchanged and SubspaceKeyError holds the refusal.
		if idx.SubspaceTupleKey() != "built" || idx.HasExplicitSubspaceKey() {
			t.Errorf("the refused set changed the built index: key %#v explicit %t", idx.SubspaceTupleKey(), idx.HasExplicitSubspaceKey())
		}
		var argErr *RecordCoreArgumentError
		if err := idx.SubspaceKeyError(); !errors.As(err, &argErr) || argErr.Message != nullKey {
			t.Errorf("SubspaceKeyError = %v, want %q", err, nullKey)
		}
	})
	t.Run("the refusal comes first in program order", func(t *testing.T) {
		t.Parallel()
		// Java throws at the set: a duplicate added afterwards never runs.
		_, err := identityTestMetaData(3, func(b *RecordMetaDataBuilder) {
			idx := NewIndex("first", Field("price")).SetSubspaceKey(nil)
			b.AddIndex("Order", idx)
			b.AddIndex("Order", NewIndex("first", Field("quantity")))
		})
		refusedBy(t, err, nullKey)
		// Two refusals: the earlier set is the one reported, not the first
		// name.
		_, err = identityTestMetaData(3, func(b *RecordMetaDataBuilder) {
			zed := NewIndex("zed", Field("price"))
			b.AddIndex("Order", zed)
			zed.SetSubspaceKey(nil)
			abc := NewIndex("abc", Field("quantity"))
			b.AddIndex("Order", abc)
			abc.SetSubspaceKey(nil)
		})
		var argErr *RecordCoreArgumentError
		if !errors.As(err, &argErr) || argErr.IndexName != "zed" {
			t.Fatalf("Build = %v, want zed's refusal, the earlier set", err)
		}
		// A fault recorded before the set comes first.
		_, err = identityTestMetaData(3, func(b *RecordMetaDataBuilder) {
			late := NewIndex("late", Field("price"))
			b.AddIndex("Order", late)
			b.AddIndex("Order", NewIndex("late", Field("quantity")))
			late.SetSubspaceKey(nil)
		})
		var mdErr *MetaDataError
		if !errors.As(err, &mdErr) || mdErr.Message != "Index late already defined" {
			t.Fatalf("Build = %v, want the duplicate recorded before the set", err)
		}
	})
	t.Run("a refused index handed to AddIndex with an unknown record type", func(t *testing.T) {
		t.Parallel()
		// Java threw at the set; the unknown type's refusal never ran.
		_, err := identityTestMetaData(3, func(b *RecordMetaDataBuilder) {
			idx := NewIndex("orphan", Field("price")).SetSubspaceKey(nil)
			b.AddIndex("NoSuchType", idx)
		})
		refusedBy(t, err, nullKey)
		// And the other way round: the unknown type first.
		_, err = identityTestMetaData(3, func(b *RecordMetaDataBuilder) {
			idx := NewIndex("orphan", Field("price"))
			b.AddIndex("NoSuchType", idx)
			idx.SetSubspaceKey(nil)
		})
		var mdErr *MetaDataError
		if !errors.As(err, &mdErr) || mdErr.Message != "Unknown record type NoSuchType" {
			t.Fatalf("Build = %v, want the unknown type, recorded before the set", err)
		}
	})
	t.Run("removing a name no index has", func(t *testing.T) {
		t.Parallel()
		// RecordMetaDataBuilder.java:1199-1203.
		_, err := identityTestMetaData(3, func(b *RecordMetaDataBuilder) {
			b.RemoveIndex("never")
		})
		var mdErr *MetaDataError
		if !errors.As(err, &mdErr) || mdErr.Message != "No index named never defined" {
			t.Fatalf("Build = %v, want Java's refusal of the removal", err)
		}
		// A second removal of one index is refused as well, and in program
		// order: the set after it is never reached in Java.
		_, err = identityTestMetaData(3, func(b *RecordMetaDataBuilder) {
			idx := NewIndex("twice", Field("price"))
			b.AddIndex("Order", idx)
			b.RemoveIndex("twice")
			b.RemoveIndex("twice")
			idx.SetSubspaceKey(nil)
		})
		if !errors.As(err, &mdErr) || mdErr.Message != "No index named twice defined" {
			t.Fatalf("Build = %v, want the second removal's refusal before the set", err)
		}
	})
	t.Run("a refused index removed before Build", func(t *testing.T) {
		t.Parallel()
		_, err := identityTestMetaData(3, func(b *RecordMetaDataBuilder) {
			idx := NewIndex("gone", Field("price"))
			b.AddIndex("Order", idx)
			idx.SetSubspaceKey(nil)
			b.RemoveIndex("gone")
		})
		// Java threw at the set, before the removal ran.
		refusedBy(t, err, nullKey)
	})
	t.Run("a struct-literal index is keyed by its name", func(t *testing.T) {
		t.Parallel()
		md, err := identityTestMetaData(3, func(b *RecordMetaDataBuilder) {
			b.AddIndex("Order", &Index{Name: "literal", Type: IndexTypeValue, RootExpression: Field("price"), Options: map[string]string{}})
		})
		if err != nil {
			t.Fatalf("Build: %v", err)
		}
		if got := md.GetIndex("literal").SubspaceTupleKey(); got != "literal" {
			t.Fatalf("key = %#v, want the name", got)
		}
		p, err := md.ToProto()
		if err != nil {
			t.Fatalf("ToProto: %v", err)
		}
		for _, idx := range p.GetIndexes() {
			if idx.GetName() == "literal" && string(idx.GetSubspaceKey()) != string(tuple.Tuple{"literal"}.Pack()) {
				t.Fatalf("stored key = %x, want the packed name", idx.GetSubspaceKey())
			}
		}
		// Removed, it becomes a former index under the same key, which saves
		// and reloads.
		b := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
		b.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
		b.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
		b.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))
		b.AddIndex("Order", &Index{Name: "literal", Type: IndexTypeValue, RootExpression: Field("price"), Options: map[string]string{}})
		b.RemoveIndex("literal")
		removed, err := b.Build()
		if err != nil {
			t.Fatalf("Build after RemoveIndex: %v", err)
		}
		rp, err := removed.ToProto()
		if err != nil {
			t.Fatalf("ToProto after RemoveIndex: %v", err)
		}
		reloaded, err := RecordMetaDataFromProto(rp)
		if err != nil {
			t.Fatalf("reload after RemoveIndex: %v", err)
		}
		if fis := reloaded.GetFormerIndexes(); len(fis) != 1 || fis[0].SubspaceKey != "literal" {
			t.Fatalf("former indexes = %#v, want one keyed by the name", fis)
		}
	})
	t.Run("a former index with a nil key", func(t *testing.T) {
		t.Parallel()
		_, err := identityTestMetaData(5, func(b *RecordMetaDataBuilder) {
			b.AddIndex("Order", NewIndex("gone", Field("price")))
			b.RemoveIndex("gone")
			b.GetFormerIndexes()[0].SubspaceKey = nil
		})
		refusedBy(t, err, "FormerIndex initialized with null subspace key")
	})
	t.Run("an fdb.Key is copied as bytes", func(t *testing.T) {
		t.Parallel()
		raw := fdb.Key("ab")
		idx := NewIndex("k", Field("price")).SetSubspaceKey(raw)
		raw[0] = 'z'
		if got, ok := idx.SubspaceTupleKey().([]byte); !ok || string(got) != "ab" {
			t.Fatalf("stored %#v (%T), want a []byte copy of \"ab\"", idx.SubspaceTupleKey(), idx.SubspaceTupleKey())
		}
	})
}

// TestBuildFormerIndexVersionMessagesAreJavas pins MetaDataValidator's
// former-index version messages (MetaDataValidator.java:163-178), named and
// unnamed, through the loader.
func TestBuildFormerIndexVersionMessagesAreJavas(t *testing.T) {
	t.Parallel()
	md, err := identityTestMetaData(5, func(*RecordMetaDataBuilder) {})
	if err != nil {
		t.Fatal(err)
	}
	base, err := md.ToProto()
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		name           string
		added, removed int32
		want           string
	}{
		{"gone", 4, 3, "Former index gone has added version 4 which is greater than the removed version 3"},
		{"", 4, 3, "Former index has added version 4 which is greater than the removed version 3"},
		{"gone", 6, 7, "Former index gone has added version 6 which is greater than the meta-data version 5"},
		{"gone", 2, 7, "Former index gone has removed version 7 which is greater than the meta-data version 5"},
	} {
		p := proto.Clone(base).(*gen.MetaData)
		fi := &gen.FormerIndex{SubspaceKey: tuple.Tuple{"gone"}.Pack(), AddedVersion: proto.Int32(c.added), RemovedVersion: proto.Int32(c.removed)}
		if c.name != "" {
			fi.FormerName = proto.String(c.name)
		}
		p.FormerIndexes = append(p.FormerIndexes, fi)
		_, err := RecordMetaDataFromProto(p)
		var mdErr *MetaDataError
		if !errors.As(err, &mdErr) || mdErr.Message != c.want {
			t.Errorf("%+v: %v, want %q", c, err, c.want)
		}
	}
}

// TestBuildRecordTypeChecksAreJavasInNameOrder pins Build's record-type
// checks to Java's texts (RecordMetaDataBuilder.java:1480-1491,
// MetaDataValidator.java:80-101) and to one order: the record types walked by
// name, each type's checks in validateRecordType's order, all of them before
// any index is validated. Each case runs 32 times, so a walk over the map
// cannot pass it by luck.
func TestBuildRecordTypeChecksAreJavasInNameOrder(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name      string
		configure func(b *RecordMetaDataBuilder)
		want      string
		// keyClass: Java throws KeyExpression.InvalidExpressionException
		// (KeyExpressionError); otherwise a MetaDataException (MetaDataError).
		keyClass bool
	}{
		{"primary key missing on two types", func(b *RecordMetaDataBuilder) {
			b.GetRecordType("TypedRecord").SetPrimaryKey(nil)
			b.GetRecordType("Customer").SetPrimaryKey(nil)
		}, "Record type Customer must have a primary key", false},
		{"primary key missing beside a since version", func(b *RecordMetaDataBuilder) {
			// Java's build refuses the key before validate runs.
			b.GetRecordType("Customer").recordType.SinceVersion = 9
			b.GetRecordType("TypedRecord").SetPrimaryKey(nil)
		}, "Record type TypedRecord must have a primary key", false},
		{"a fanned-out primary key", func(b *RecordMetaDataBuilder) {
			b.GetRecordType("Order").SetPrimaryKey(FanOut("tags"))
		}, "Primary key for Order can generate more than one entry", false},
		{"a primary key validated before its fan-out", func(b *RecordMetaDataBuilder) {
			// validatePrimaryKeyForRecordType validates, then checks
			// createsDuplicates.
			b.GetRecordType("Order").SetPrimaryKey(FanOut("order_id"))
		}, "order_id is not repeated with FanType.FanOut", true},
		{"one record type key on three types", func(b *RecordMetaDataBuilder) {
			for _, name := range []string{"TypedRecord", "Order", "Customer"} {
				b.GetRecordType(name).SetRecordTypeKey(int64(7))
			}
		}, "Same record type key 7 used by both Order and Customer", false},
		{"since versions on two types", func(b *RecordMetaDataBuilder) {
			b.GetRecordType("TypedRecord").recordType.SinceVersion = 8
			b.GetRecordType("Order").recordType.SinceVersion = 9
		}, "Record type Order has since version of 9 which is greater than the meta-data version 3", false},
		{"a type's checks in validateRecordType's order", func(b *RecordMetaDataBuilder) {
			// Order's key collides with Customer's and its since version is
			// too new: the key is checked first.
			b.GetRecordType("Customer").SetRecordTypeKey(int64(7))
			b.GetRecordType("Order").SetRecordTypeKey(int64(7))
			b.GetRecordType("Order").recordType.SinceVersion = 9
		}, "Same record type key 7 used by both Order and Customer", false},
		{"an earlier type's last check before a later type's first", func(b *RecordMetaDataBuilder) {
			b.GetRecordType("Customer").recordType.SinceVersion = 9
			b.GetRecordType("Order").SetPrimaryKey(FanOut("tags"))
		}, "Record type Customer has since version of 9 which is greater than the meta-data version 3", false},
		{"record types before indexes", func(b *RecordMetaDataBuilder) {
			b.AddIndex("Customer", NewIndex("bad", Field("no_such_field")))
			b.GetRecordType("TypedRecord").recordType.SinceVersion = 9
		}, "Record type TypedRecord has since version of 9 which is greater than the meta-data version 3", false},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			for range 32 {
				_, err := identityTestMetaData(3, c.configure)
				if err == nil || err.Error() != c.want {
					t.Fatalf("Build = %v, want %q", err, c.want)
				}
				// Java's class, named per case.
				var mdErr *MetaDataError
				var keyErr *KeyExpressionError
				if errors.As(err, &keyErr) != c.keyClass || errors.As(err, &mdErr) == c.keyClass {
					t.Fatalf("Build = %v (%T), want the class the case names (key expression: %t)", err, err, c.keyClass)
				}
			}
		})
	}
}

// TestEvolutionComparesRootsAsJavaDoes pins the root comparison of
// validateIndex to KeyExpression.equals: a field's null interpretation is in
// its proto and not in FieldKeyExpression.equals (FieldKeyExpression.java:
// 406-410), so a root that changes only that is the same root; and the
// position arm that no other test reaches, positions dropped.
func TestEvolutionComparesRootsAsJavaDoes(t *testing.T) {
	t.Parallel()
	md, err := identityTestMetaData(3, func(b *RecordMetaDataBuilder) {
		idx := NewIndex("i", Field("price"))
		idx.AddedVersion, idx.LastModifiedVersion = 1, 1
		b.AddIndex("Order", idx)
	})
	if err != nil {
		t.Fatal(err)
	}
	oldProto, err := md.ToProto()
	if err != nil {
		t.Fatal(err)
	}
	withIndex := func(version int32, edit func(*gen.Index)) *RecordMetaData {
		t.Helper()
		p := proto.Clone(oldProto).(*gen.MetaData)
		p.Version = proto.Int32(version)
		for _, idx := range p.GetIndexes() {
			if idx.GetName() == "i" {
				edit(idx)
			}
		}
		out, err := RecordMetaDataFromProto(p)
		if err != nil {
			t.Fatalf("load: %v", err)
		}
		return out
	}
	old := withIndex(3, func(*gen.Index) {})
	unique := withIndex(4, func(idx *gen.Index) {
		idx.RootExpression.Field.NullInterpretation = gen.Field_UNIQUE.Enum()
	})
	if err := DefaultMetaDataEvolutionValidator().Validate(old, unique); err != nil {
		t.Errorf("a root changed only in its null interpretation: %v, want admitted", err)
	}
	// Positions are computed by the builder, not stored, so the old side's
	// are forced on the index, as the other position tests force them.
	withPositions, err := identityTestMetaData(3, func(b *RecordMetaDataBuilder) {
		idx := NewIndex("i", Field("price"))
		idx.AddedVersion, idx.LastModifiedVersion = 1, 1
		idx.primaryKeyComponentPositions = []int{-1}
		b.AddIndex("Order", idx)
	})
	if err != nil {
		t.Fatal(err)
	}
	if !withPositions.GetIndex("i").HasPrimaryKeyComponentPositions() {
		t.Fatal("the forced positions did not survive Build")
	}
	dropped := withIndex(4, func(*gen.Index) {})
	var evolErr *MetaDataEvolutionError
	if err := DefaultMetaDataEvolutionValidator().Validate(withPositions, dropped); !errors.As(err, &evolErr) ||
		!strings.HasPrefix(evolErr.Message, "new index drops primary key component positions") {
		t.Errorf("positions dropped: %v, want Java's message first", err)
	}
}

// TestSavedSourceIndexResolvesANestedTupleKey pins Go's side of the declared
// difference at OnlineIndexer.savedSourceIndex (DIVERGENCES.md, "BY_INDEX
// resume over a nested-tuple source key"): the stamp's decoded item is a
// tuple, the index's normalized key a list, and Go resolves the index the
// stamp names, where Java's Tuple never equals its List key.
func TestSavedSourceIndexResolvesANestedTupleKey(t *testing.T) {
	t.Parallel()
	md, err := identityTestMetaData(3, func(b *RecordMetaDataBuilder) {
		b.AddIndex("Order", NewIndex("nested", Field("price")).SetSubspaceKey(tuple.Tuple{"a", int64(1)}))
		b.AddIndex("Order", NewIndex("flat", Field("quantity")).SetSubspaceKey("flat_key"))
	})
	if err != nil {
		t.Fatal(err)
	}
	oi := &OnlineIndexer{metaData: md}
	for _, c := range []struct {
		key  any
		want string
	}{
		{tuple.Tuple{"a", int64(1)}, "nested"},
		{"flat_key", "flat"},
	} {
		stamp := &gen.IndexBuildIndexingStamp{SourceIndexSubspaceKey: tuple.Tuple{c.key}.Pack()}
		got, err := oi.savedSourceIndex(stamp)
		if err != nil || got == nil || got.Name != c.want {
			t.Errorf("savedSourceIndex(%#v) = %v, %v, want %s", c.key, got, err, c.want)
		}
	}
}
