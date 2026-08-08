package recordlayer

import (
	"errors"
	"sync"
	"testing"

	"fdb.dev/gen"
	"fdb.dev/pkg/fdbgo/fdb/subspace"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	. "github.com/onsi/gomega"
	"google.golang.org/protobuf/proto"
)

// The live key-encoding fast paths are EvaluateFlat, EvaluateScalar,
// EvaluateInt64 and PackDirect; the live slow path is Evaluate. Every index
// write picks the narrowest fast path the expression implements and falls back
// to Evaluate when it declines, so the two must agree on the exact bytes or the
// same record encodes to two different index keys depending on which branch the
// maintainer took. These tests hold the fast paths against Evaluate on the
// shapes that reach them.

// packSlow evaluates via Evaluate and packs the single resulting tuple. It is
// the reference every fast path is measured against.
func packSlow(t *testing.T, expr KeyExpression, record *FDBStoredRecord[proto.Message], msg proto.Message) (tuple.Tuple, []byte) {
	t.Helper()
	g := NewGomegaWithT(t)
	result, err := expr.Evaluate(record, msg)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(result).To(HaveLen(1), "reference path must produce exactly one tuple")
	out := make(tuple.Tuple, len(result[0]))
	for i, v := range result[0] {
		out[i] = v
	}
	return out, out.Pack()
}

// assertFastPathAgreement asserts that every live fast path the expression
// implements produces byte-identical output to Evaluate. A fast path that
// declines (PackDirect false, EvaluateInt64 not-ok) is not a failure — that is
// the documented route back to Evaluate — but a fast path that ACCEPTS and
// disagrees is a wire divergence.
func assertFastPathAgreement(t *testing.T, expr KeyExpression, msg proto.Message) tuple.Tuple {
	t.Helper()
	record := &FDBStoredRecord[proto.Message]{Record: msg}
	return assertFastPathAgreementForRecord(t, expr, record, msg)
}

func assertFastPathAgreementForRecord(t *testing.T, expr KeyExpression, record *FDBStoredRecord[proto.Message], msg proto.Message) tuple.Tuple {
	t.Helper()
	g := NewGomegaWithT(t)

	slow, slowBytes := packSlow(t, expr, record, msg)

	if fe, ok := expr.(FlatEvaluator); ok {
		flat, err := fe.EvaluateFlat(record, msg)
		g.Expect(err).NotTo(HaveOccurred(), "EvaluateFlat errored where Evaluate succeeded")
		ft := make(tuple.Tuple, len(flat))
		for i, v := range flat {
			ft[i] = v
		}
		g.Expect(ft.Pack()).To(Equal(slowBytes), "EvaluateFlat packed different bytes than Evaluate")
	}

	if se, ok := expr.(ScalarEvaluator); ok && expr.ColumnSize() == 1 {
		v, err := se.EvaluateScalar(record, msg)
		g.Expect(err).NotTo(HaveOccurred(), "EvaluateScalar errored where Evaluate succeeded")
		g.Expect(tuple.Tuple{v}.Pack()).To(Equal(slowBytes), "EvaluateScalar packed different bytes than Evaluate")
	}

	if ie, ok := expr.(Int64Evaluator); ok && expr.ColumnSize() == 1 {
		v, accepted, err := ie.EvaluateInt64(record, msg)
		g.Expect(err).NotTo(HaveOccurred(), "EvaluateInt64 errored where Evaluate succeeded")
		if accepted {
			g.Expect(tuple.Tuple{v}.Pack()).To(Equal(slowBytes), "EvaluateInt64 packed different bytes than Evaluate")
		}
	}

	if dp, ok := expr.(DirectPacker); ok {
		pk := tuple.GetPacker()
		pk.Reset()
		accepted := dp.PackDirect(pk, record, msg)
		if accepted {
			var buf []byte
			got := pk.AppendInto(&buf, nil)
			g.Expect(got).To(Equal(slowBytes), "PackDirect packed different bytes than Evaluate")
		}
		tuple.PutPacker(pk)
	}

	return slow
}

// --- Scalar field kinds ---

func TestKeyExpressionFastPath_SingleFieldInt64(t *testing.T) {
	t.Parallel()
	g := NewGomegaWithT(t)
	got := assertFastPathAgreement(t, Field("order_id"), &gen.Order{OrderId: proto.Int64(42)})
	g.Expect(got).To(HaveLen(1))
	g.Expect(got[0]).To(Equal(int64(42)))
}

func TestKeyExpressionFastPath_SingleFieldString(t *testing.T) {
	t.Parallel()
	g := NewGomegaWithT(t)
	got := assertFastPathAgreement(t, Field("name"), &gen.Customer{CustomerId: proto.Int64(1), Name: proto.String("test")})
	g.Expect(got).To(HaveLen(1))
	g.Expect(got[0]).To(Equal("test"))
}

func TestKeyExpressionFastPath_SingleFieldFloat32(t *testing.T) {
	t.Parallel()
	g := NewGomegaWithT(t)
	got := assertFastPathAgreement(t, Field("val_float"), &gen.TypedRecord{Id: proto.Int64(1), ValFloat: proto.Float32(3.14)})
	g.Expect(got).To(HaveLen(1))
	g.Expect(got[0]).To(BeAssignableToTypeOf(float32(0)), "must be float32, not float64 — the two pack to different bytes")
	g.Expect(got[0]).To(BeNumerically("~", 3.14, 0.001))
}

func TestKeyExpressionFastPath_SingleFieldFloat64(t *testing.T) {
	t.Parallel()
	g := NewGomegaWithT(t)
	got := assertFastPathAgreement(t, Field("val_double"), &gen.TypedRecord{Id: proto.Int64(1), ValDouble: proto.Float64(2.71828)})
	g.Expect(got).To(HaveLen(1))
	g.Expect(got[0]).To(BeAssignableToTypeOf(float64(0)))
	g.Expect(got[0]).To(Equal(float64(2.71828)))
}

func TestKeyExpressionFastPath_SingleFieldEnum(t *testing.T) {
	t.Parallel()
	g := NewGomegaWithT(t)
	got := assertFastPathAgreement(t, Field("val_enum"), &gen.TypedRecord{Id: proto.Int64(1), ValEnum: gen.Color_RED.Enum()})
	g.Expect(got).To(HaveLen(1))
	g.Expect(got[0]).To(Equal(int64(1))) // RED=1
}

func TestKeyExpressionFastPath_Int32Field(t *testing.T) {
	t.Parallel()
	assertFastPathAgreement(t, Field("val_int32"), &gen.TypedRecord{Id: proto.Int64(1), ValInt32: proto.Int32(-100)})
}

func TestKeyExpressionFastPath_Sint32Field(t *testing.T) {
	t.Parallel()
	assertFastPathAgreement(t, Field("val_sint32"), &gen.TypedRecord{Id: proto.Int64(1), ValSint32: proto.Int32(-50)})
}

func TestKeyExpressionFastPath_Sint64Field(t *testing.T) {
	t.Parallel()
	assertFastPathAgreement(t, Field("val_sint64"), &gen.TypedRecord{Id: proto.Int64(1), ValSint64: proto.Int64(-999999)})
}

func TestKeyExpressionFastPath_Sfixed32Field(t *testing.T) {
	t.Parallel()
	assertFastPathAgreement(t, Field("val_sfixed32"), &gen.TypedRecord{Id: proto.Int64(1), ValSfixed32: proto.Int32(12345)})
}

func TestKeyExpressionFastPath_Sfixed64Field(t *testing.T) {
	t.Parallel()
	assertFastPathAgreement(t, Field("val_sfixed64"), &gen.TypedRecord{Id: proto.Int64(1), ValSfixed64: proto.Int64(-12345)})
}

func TestKeyExpressionFastPath_BoolField(t *testing.T) {
	t.Parallel()
	assertFastPathAgreement(t, Field("val_bool"), &gen.TypedRecord{Id: proto.Int64(1), ValBool: proto.Bool(true)})
}

func TestKeyExpressionFastPath_BoolFieldFalse(t *testing.T) {
	t.Parallel()
	assertFastPathAgreement(t, Field("val_bool"), &gen.TypedRecord{Id: proto.Int64(1), ValBool: proto.Bool(false)})
}

func TestKeyExpressionFastPath_BytesField(t *testing.T) {
	t.Parallel()
	assertFastPathAgreement(t, Field("val_bytes"), &gen.TypedRecord{Id: proto.Int64(1), ValBytes: []byte{0x00, 0xFF, 0xAB}})
}

func TestKeyExpressionFastPath_LargeInt64(t *testing.T) {
	t.Parallel()
	assertFastPathAgreement(t, Field("order_id"), &gen.Order{OrderId: proto.Int64(9223372036854775807)}) // MaxInt64
}

func TestKeyExpressionFastPath_NegativeInt64(t *testing.T) {
	t.Parallel()
	assertFastPathAgreement(t, Field("order_id"), &gen.Order{OrderId: proto.Int64(-9223372036854775808)}) // MinInt64
}

func TestKeyExpressionFastPath_ZeroInt64(t *testing.T) {
	t.Parallel()
	assertFastPathAgreement(t, Field("order_id"), &gen.Order{OrderId: proto.Int64(0)})
}

func TestKeyExpressionFastPath_EmptyString(t *testing.T) {
	t.Parallel()
	assertFastPathAgreement(t, Field("name"), &gen.Customer{CustomerId: proto.Int64(1), Name: proto.String("")})
}

func TestKeyExpressionFastPath_UnicodeString(t *testing.T) {
	t.Parallel()
	assertFastPathAgreement(t, Field("name"), &gen.Customer{CustomerId: proto.Int64(1), Name: proto.String("Hello 世界 \U0001F600")})
}

func TestKeyExpressionFastPath_UnsetOptionalField(t *testing.T) {
	t.Parallel()
	g := NewGomegaWithT(t)
	// val_float is NOT set: every fast path must agree it encodes as tuple null.
	got := assertFastPathAgreement(t, Field("val_float"), &gen.TypedRecord{Id: proto.Int64(1)})
	g.Expect(got).To(HaveLen(1))
	g.Expect(got[0]).To(BeNil())
}

// --- Composite shapes ---

func TestKeyExpressionFastPath_CompositeKeyTwoFields(t *testing.T) {
	t.Parallel()
	g := NewGomegaWithT(t)
	got := assertFastPathAgreement(t, Concat(Field("order_id"), Field("price")),
		&gen.Order{OrderId: proto.Int64(99), Price: proto.Int32(250)})
	g.Expect(got).To(HaveLen(2))
	g.Expect(got[0]).To(Equal(int64(99)))
	g.Expect(got[1]).To(Equal(int64(250)))
}

func TestKeyExpressionFastPath_CompositeWithMixedTypes(t *testing.T) {
	t.Parallel()
	g := NewGomegaWithT(t)
	got := assertFastPathAgreement(t,
		Concat(Field("val_int32"), Field("val_float"), Field("val_string")),
		&gen.TypedRecord{
			Id:        proto.Int64(1),
			ValInt32:  proto.Int32(42),
			ValFloat:  proto.Float32(1.5),
			ValString: proto.String("abc"),
		})
	g.Expect(got).To(HaveLen(3))
	g.Expect(got[0]).To(Equal(int64(42)))
	g.Expect(got[1]).To(BeAssignableToTypeOf(float32(0)))
	g.Expect(got[1]).To(BeNumerically("~", 1.5, 0.001))
	g.Expect(got[2]).To(Equal("abc"))
}

func TestKeyExpressionFastPath_CompositeWithUnsetFields(t *testing.T) {
	t.Parallel()
	g := NewGomegaWithT(t)
	got := assertFastPathAgreement(t, Concat(Field("val_int32"), Field("val_string")),
		&gen.TypedRecord{Id: proto.Int64(1)})
	g.Expect(got).To(HaveLen(2))
	g.Expect(got[0]).To(BeNil())
	g.Expect(got[1]).To(BeNil())
}

func TestKeyExpressionFastPath_AllIntegerTypes(t *testing.T) {
	t.Parallel()
	g := NewGomegaWithT(t)
	got := assertFastPathAgreement(t,
		Concat(
			Field("val_int32"),
			Field("val_int64"),
			Field("val_sint32"),
			Field("val_sint64"),
			Field("val_sfixed32"),
			Field("val_sfixed64"),
		),
		&gen.TypedRecord{
			Id:          proto.Int64(1),
			ValInt32:    proto.Int32(32),
			ValInt64:    proto.Int64(64),
			ValSint32:   proto.Int32(-32),
			ValSint64:   proto.Int64(-64),
			ValSfixed32: proto.Int32(320),
			ValSfixed64: proto.Int64(640),
		})
	g.Expect(got).To(HaveLen(6))
	for i := range got {
		g.Expect(got[i]).To(BeAssignableToTypeOf(int64(0)), "element %d should be int64", i)
	}
}

func TestKeyExpressionFastPath_EmptyKeyExpression(t *testing.T) {
	t.Parallel()
	g := NewGomegaWithT(t)
	got := assertFastPathAgreement(t, EmptyKey(), &gen.Order{OrderId: proto.Int64(1)})
	g.Expect(got).To(HaveLen(0), "empty key produces no elements")
}

func TestKeyExpressionFastPath_GroupingKeyDelegatesToWholeKey(t *testing.T) {
	t.Parallel()
	g := NewGomegaWithT(t)

	// GroupBy(grouped, grouping...) builds wholeKey = Concat(grouping..., grouped).
	grouped := GroupBy(Field("price"), Field("order_id"))
	msg := &gen.Order{OrderId: proto.Int64(5), Price: proto.Int32(100)}

	got := assertFastPathAgreement(t, grouped, msg)
	g.Expect(got).To(HaveLen(2))

	_, innerBytes := packSlow(t, Concat(Field("order_id"), Field("price")),
		&FDBStoredRecord[proto.Message]{Record: msg}, msg)
	g.Expect(got.Pack()).To(Equal(innerBytes), "grouping key must encode as its wholeKey")
}

// --- Record type key ---

// storedOfType builds metadata with the given explicit record type keys and
// returns a stored record of typeName wrapping msg. The record type key comes
// from the record's own type, so a test that wants a specific key states it on
// the type rather than on the expression.
func storedOfType(t *testing.T, msg proto.Message, typeName string, keys map[string]any) *FDBStoredRecord[proto.Message] {
	t.Helper()
	b := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
	b.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
	b.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
	b.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))
	for name, key := range keys {
		b.GetRecordType(name).SetRecordTypeKey(key)
	}
	md, err := b.Build()
	if err != nil {
		t.Fatalf("metadata build failed: %v", err)
	}
	rt := md.GetRecordType(typeName)
	if rt == nil {
		t.Fatalf("record type %q missing", typeName)
	}
	return &FDBStoredRecord[proto.Message]{RecordType: rt, Record: msg}
}

func TestKeyExpressionFastPath_RecordTypeKeyWithField(t *testing.T) {
	t.Parallel()
	g := NewGomegaWithT(t)

	expr := Concat(RecordTypeKey(), Field("order_id"))
	msg := &gen.Order{OrderId: proto.Int64(42)}
	record := storedOfType(t, msg, "Order", map[string]any{"Order": 1, "Customer": 2})

	got := assertFastPathAgreementForRecord(t, expr, record, msg)
	g.Expect(got).To(HaveLen(2))
	g.Expect(got[0]).To(Equal(int64(1))) // Order's record type key
	g.Expect(got[1]).To(Equal(int64(42)))
}

func TestKeyExpressionFastPath_ThreeFieldComposite(t *testing.T) {
	t.Parallel()
	g := NewGomegaWithT(t)

	expr := Concat(RecordTypeKey(), Field("order_id"), Field("price"))
	msg := &gen.Order{OrderId: proto.Int64(42), Price: proto.Int32(250)}
	record := storedOfType(t, msg, "Order", map[string]any{"Order": 7})

	got := assertFastPathAgreementForRecord(t, expr, record, msg)
	g.Expect(got).To(HaveLen(3))
	g.Expect(got[0]).To(Equal(int64(7)))
	g.Expect(got[1]).To(Equal(int64(42)))
	g.Expect(got[2]).To(Equal(int64(250)))
}

// The key follows the RECORD's type, not anything captured on the expression:
// one expression, two record types, two different keys.
func TestKeyExpressionFastPath_RecordTypeKeyFollowsTheRecord(t *testing.T) {
	t.Parallel()
	g := NewGomegaWithT(t)

	rtk := RecordTypeKey()
	keys := map[string]any{"Order": 1, "Customer": 2}

	orderMsg := &gen.Order{OrderId: proto.Int64(1)}
	orderRec := storedOfType(t, orderMsg, "Order", keys)
	g.Expect(assertFastPathAgreementForRecord(t, rtk, orderRec, orderMsg)[0]).To(Equal(int64(1)))

	customerMsg := &gen.Customer{CustomerId: proto.Int64(1)}
	customerRec := storedOfType(t, customerMsg, "Customer", keys)
	g.Expect(assertFastPathAgreementForRecord(t, rtk, customerRec, customerMsg)[0]).To(Equal(int64(2)))
}

// A record whose type was never resolved cannot yield a record type key. Every
// live path must refuse it the SAME way: Evaluate errors, and PackDirect
// reports false so the caller is routed to that error rather than packing a
// guess. A PackDirect that returned true here would write key bytes Evaluate
// never produces.
func TestKeyExpressionFastPath_RecordTypeKeyUnresolvedType(t *testing.T) {
	t.Parallel()
	g := NewGomegaWithT(t)

	rtk := &RecordTypeKeyExpression{}
	msg := &gen.Order{OrderId: proto.Int64(1)}
	record := &FDBStoredRecord[proto.Message]{Record: msg} // no RecordType

	_, err := rtk.Evaluate(record, msg)
	var want *RecordTypeKeyUnresolvedError
	g.Expect(errors.As(err, &want)).To(BeTrue(),
		"Evaluate accepted a record with no resolved type: %v", err)

	pk := tuple.GetPacker()
	pk.Reset()
	g.Expect(rtk.PackDirect(pk, record, msg)).To(BeFalse(),
		"PackDirect packed a guess for an unresolved record type instead of declining to the erroring path")
	tuple.PutPacker(pk)

	_, serr := rtk.EvaluateScalar(record, msg)
	g.Expect(errors.As(serr, &want)).To(BeTrue(),
		"EvaluateScalar accepted a record with no resolved type: %v", serr)
}

// --- Nil messages ---

func TestKeyExpressionFastPath_NilMessage(t *testing.T) {
	t.Parallel()
	g := NewGomegaWithT(t)

	expr := Field("order_id")
	record := &FDBStoredRecord[proto.Message]{Record: nil}

	got := assertFastPathAgreementForRecord(t, expr, record, nil)
	g.Expect(got).To(HaveLen(1))
	g.Expect(got[0]).To(BeNil())
}

func TestKeyExpressionFastPath_NilMessageRecordTypeKey(t *testing.T) {
	t.Parallel()
	g := NewGomegaWithT(t)

	rtk := RecordTypeKey()
	record := &FDBStoredRecord[proto.Message]{Record: nil}

	got := assertFastPathAgreementForRecord(t, rtk, record, nil)
	g.Expect(got).To(HaveLen(1))
	g.Expect(got[0]).To(BeNil())
}

// --- Shapes the fast path must DECLINE ---

// Fan-out, concatenate and nesting produce key shapes no direct packer can
// express. The fast paths must decline them so the maintainer falls back to
// Evaluate; a fast path that accepted one would flatten a multi-entry or nested
// key into a single wrong index key.
func TestKeyExpressionFastPath_FanOutFieldDeclinesFastPath(t *testing.T) {
	t.Parallel()
	g := NewGomegaWithT(t)

	expr := FanOut("tags")
	msg := &gen.Order{OrderId: proto.Int64(1), Tags: []string{"a", "b"}}
	record := &FDBStoredRecord[proto.Message]{Record: msg}

	// Fan-out genuinely produces more than one tuple — the shape no fast path
	// can carry.
	result, err := expr.Evaluate(record, msg)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(result).To(HaveLen(2))

	pk := tuple.GetPacker()
	pk.Reset()
	g.Expect(expr.(DirectPacker).PackDirect(pk, record, msg)).To(BeFalse(),
		"PackDirect accepted a fan-out field — it would collapse 2 index entries into 1")
	tuple.PutPacker(pk)

	_, accepted, ierr := expr.(Int64Evaluator).EvaluateInt64(record, msg)
	g.Expect(ierr).NotTo(HaveOccurred())
	g.Expect(accepted).To(BeFalse(), "EvaluateInt64 accepted a repeated field")
}

func TestKeyExpressionFastPath_RepeatedFieldInCompositeDeclinesFastPath(t *testing.T) {
	t.Parallel()
	g := NewGomegaWithT(t)

	expr := Concat(Field("order_id"), FanOut("tags"))
	msg := &gen.Order{OrderId: proto.Int64(1), Tags: []string{"a", "b"}}
	record := &FDBStoredRecord[proto.Message]{Record: msg}

	pk := tuple.GetPacker()
	pk.Reset()
	g.Expect(expr.(DirectPacker).PackDirect(pk, record, msg)).To(BeFalse(),
		"composite PackDirect accepted a fan-out child")
	tuple.PutPacker(pk)

	result, err := expr.Evaluate(record, msg)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(result).To(HaveLen(2), "the slow path is what produces the two fan-out entries")
}

// RecordTypeKey().Nest(...) is a Go-only extension — Java's
// RecordTypeKeyExpression is KeyExpressionWithoutChildren with getColumnSize()
// fixed at 1. It makes ColumnSize() > 1, so the single-element fast paths
// cannot express it: EvaluateScalar and PackDirect must decline, and
// EvaluateFlat must produce the nested columns. Any of them emitting only the
// type key writes an index key SHORT of ColumnSize() — and because deletes go
// through Evaluate, which produces the full-width key, the short entry is
// never matched and the index leaks an orphan on every write.
func TestKeyExpressionFastPath_NestedRecordTypeKeyKeepsAllColumns(t *testing.T) {
	t.Parallel()
	g := NewGomegaWithT(t)

	expr := RecordTypeKey().Nest(Field("order_id"))
	msg := &gen.Order{OrderId: proto.Int64(42)}
	record := storedOfType(t, msg, "Order", map[string]any{"Order": 7})

	g.Expect(expr.ColumnSize()).To(Equal(2))

	slow, slowBytes := packSlow(t, expr, record, msg)
	g.Expect(slow).To(HaveLen(2))
	g.Expect(slow[0]).To(Equal(int64(7)))
	g.Expect(slow[1]).To(Equal(int64(42)))

	rtk := expr.(*RecordTypeKeyExpression)

	// EvaluateFlat must carry both columns — evaluateKeyFlat propagates it
	// with no fallback.
	flat, err := rtk.EvaluateFlat(record, msg)
	g.Expect(err).NotTo(HaveOccurred())
	ft := make(tuple.Tuple, len(flat))
	for i, v := range flat {
		ft[i] = v
	}
	g.Expect(ft.Pack()).To(Equal(slowBytes),
		"EvaluateFlat dropped the nested columns — a short index key that Evaluate-based deletes can never match")

	// EvaluateScalar has room for exactly one column, so it must decline.
	_, serr := rtk.EvaluateScalar(record, msg)
	g.Expect(serr).To(HaveOccurred(),
		"EvaluateScalar returned a single value for a 2-column expression — the nested column is silently dropped")

	// PackDirect emits one element, so it must decline rather than pack a
	// truncated key.
	pk := tuple.GetPacker()
	pk.Reset()
	g.Expect(rtk.PackDirect(pk, record, msg)).To(BeFalse(),
		"PackDirect accepted a nested record type key and packed a key short of ColumnSize()")
	tuple.PutPacker(pk)

	// And the same holds when it is a child of a composite index root: the
	// composite must not silently lose the nested column either.
	comp := Concat(RecordTypeKey().Nest(Field("order_id")), Field("price"))
	msg2 := &gen.Order{OrderId: proto.Int64(42), Price: proto.Int32(9)}
	rec2 := storedOfType(t, msg2, "Order", map[string]any{"Order": 7})

	g.Expect(comp.ColumnSize()).To(Equal(3))

	cflat, cerr := comp.(FlatEvaluator).EvaluateFlat(rec2, msg2)
	g.Expect(cerr).NotTo(HaveOccurred())
	g.Expect(cflat).To(HaveLen(3),
		"composite EvaluateFlat dropped the nested record-type-key column")
	g.Expect(cflat[0]).To(Equal(int64(7)))
	g.Expect(cflat[1]).To(Equal(int64(42)))
	g.Expect(cflat[2]).To(Equal(int64(9)))

	cpk := tuple.GetPacker()
	cpk.Reset()
	g.Expect(comp.(DirectPacker).PackDirect(cpk, rec2, msg2)).To(BeFalse(),
		"composite PackDirect packed a key short of ColumnSize()")
	tuple.PutPacker(cpk)
}

// store.go and store_batch.go compute every record's primary key with
// evaluateKeyFlat, which calls EvaluateFlat and does NOT fall back to
// Evaluate. So an EvaluateFlat that drops the nested columns does not merely
// mis-encode a key — it collapses every record of the type onto the SAME
// primary key, and each save overwrites the last. This is the shape
// metadata_builder_test.go uses as a real primary key.
func TestKeyExpressionFastPath_NestedRecordTypeKeyPrimaryKeysStayDistinct(t *testing.T) {
	t.Parallel()
	g := NewGomegaWithT(t)

	pkExpr := RecordTypeKey().Nest(Field("order_id"))

	keyFor := func(id int64) []any {
		msg := &gen.Order{OrderId: proto.Int64(id)}
		rec := storedOfType(t, msg, "Order", map[string]any{"Order": 7})
		vals, err := evaluateKeyFlat(pkExpr, rec, msg)
		g.Expect(err).NotTo(HaveOccurred())
		return vals
	}

	first := keyFor(42)
	second := keyFor(43)

	g.Expect(first).To(HaveLen(2), "primary key lost the nested column")
	g.Expect(second).To(HaveLen(2), "primary key lost the nested column")
	g.Expect(first).To(Equal([]any{int64(7), int64(42)}))
	g.Expect(second).To(Equal([]any{int64(7), int64(43)}))
	g.Expect(first).NotTo(Equal(second),
		"two distinct records share one primary key — every save of this record type overwrites the previous one")
}

func TestKeyExpressionFastPath_NestingExpressionDeclinesFastPath(t *testing.T) {
	t.Parallel()
	g := NewGomegaWithT(t)

	// NestingKeyExpression implements only Evaluate — it deliberately carries
	// none of the fast-path interfaces, so every caller structurally falls back.
	expr := Nest("flower", Field("type"))

	_, isDirect := expr.(DirectPacker)
	g.Expect(isDirect).To(BeFalse(), "NestingKeyExpression must not implement DirectPacker")
	_, isInt64 := expr.(Int64Evaluator)
	g.Expect(isInt64).To(BeFalse(), "NestingKeyExpression must not implement Int64Evaluator")
	_, isScalar := expr.(ScalarEvaluator)
	g.Expect(isScalar).To(BeFalse(), "NestingKeyExpression must not implement ScalarEvaluator")
}

// --- The divergence this file exists to pin ---

// A key expression naming a field the message does not have must ERROR on
// every live path — it must never encode as a value. A path that returned
// (nil, nil) here would pack tuple null (0x00) into an index key, silently
// indexing every record of that type under the same wrong key and leaving no
// trace that the field was missing. If this test fails, that corruption is
// re-armed on the path every index write takes.
func TestKeyExpressionFastPath_UnknownFieldErrorsEverywhere(t *testing.T) {
	t.Parallel()
	g := NewGomegaWithT(t)

	expr := Field("no_such_field")
	msg := &gen.Order{OrderId: proto.Int64(1)}
	record := &FDBStoredRecord[proto.Message]{Record: msg}

	assertUnknownFieldError := func(err error, path string) {
		t.Helper()
		var want *KeyExpressionError
		g.Expect(errors.As(err, &want)).To(BeTrue(),
			"%s accepted an unknown field instead of erroring — tuple null would be packed into an index key; got err=%v", path, err)
		g.Expect(want.Message).To(ContainSubstring("no_such_field"))
	}

	_, err := expr.Evaluate(record, msg)
	assertUnknownFieldError(err, "Evaluate")

	_, ferr := expr.(FlatEvaluator).EvaluateFlat(record, msg)
	assertUnknownFieldError(ferr, "EvaluateFlat")

	_, serr := expr.(ScalarEvaluator).EvaluateScalar(record, msg)
	assertUnknownFieldError(serr, "EvaluateScalar")

	_, _, ierr := expr.(Int64Evaluator).EvaluateInt64(record, msg)
	assertUnknownFieldError(ierr, "EvaluateInt64")

	// PackDirect has no error channel, so its contract is to DECLINE, routing
	// the caller to the erroring path rather than packing a guess.
	pk := tuple.GetPacker()
	pk.Reset()
	g.Expect(expr.(DirectPacker).PackDirect(pk, record, msg)).To(BeFalse(),
		"PackDirect packed an unknown field instead of declining to the erroring path")
	tuple.PutPacker(pk)
}

// The same must hold when the unknown field is one child of a composite: a
// composite that swallowed the child error would pack a short-but-valid key.
func TestKeyExpressionFastPath_UnknownFieldInCompositeErrors(t *testing.T) {
	t.Parallel()
	g := NewGomegaWithT(t)

	expr := Concat(Field("order_id"), Field("no_such_field"))
	msg := &gen.Order{OrderId: proto.Int64(1)}
	record := &FDBStoredRecord[proto.Message]{Record: msg}

	var want *KeyExpressionError

	_, err := expr.Evaluate(record, msg)
	g.Expect(errors.As(err, &want)).To(BeTrue(),
		"composite Evaluate swallowed an unknown-field child and packed a key: %v", err)

	_, ferr := expr.(FlatEvaluator).EvaluateFlat(record, msg)
	g.Expect(errors.As(ferr, &want)).To(BeTrue(),
		"composite EvaluateFlat swallowed an unknown-field child and packed a key: %v", ferr)

	pk := tuple.GetPacker()
	pk.Reset()
	g.Expect(expr.(DirectPacker).PackDirect(pk, record, msg)).To(BeFalse(),
		"composite PackDirect packed a key containing an unknown field")
	tuple.PutPacker(pk)
}

// --- Field-descriptor cache ---

// One expression is shared across every record of every type that uses it, so
// the descriptor cache must re-resolve when the message type changes rather
// than reusing a descriptor from a different message.
func TestKeyExpressionFastPath_FieldCacheAcrossMessageTypes(t *testing.T) {
	t.Parallel()
	g := NewGomegaWithT(t)

	// Both Order and TypedRecord have a "price" field.
	expr := Field("price")
	se := expr.(ScalarEvaluator)

	orderMsg := &gen.Order{OrderId: proto.Int64(1), Price: proto.Int32(100)}
	orderRec := &FDBStoredRecord[proto.Message]{Record: orderMsg}
	v, err := se.EvaluateScalar(orderRec, orderMsg)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(v).To(Equal(int64(100)))

	typedMsg := &gen.TypedRecord{Id: proto.Int64(2), Price: proto.Int32(200)}
	typedRec := &FDBStoredRecord[proto.Message]{Record: typedMsg}
	v, err = se.EvaluateScalar(typedRec, typedMsg)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(v).To(Equal(int64(200)))

	// Back to Order: the cache must have re-resolved, not stuck on TypedRecord.
	v, err = se.EvaluateScalar(orderRec, orderMsg)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(v).To(Equal(int64(100)))
}

// Metadata — and therefore the expression holding the cache — is shared across
// concurrent transactions. Run under -race, this pins that the cache is an
// atomic and not a plain mutable field.
func TestKeyExpressionFastPath_FieldCacheConcurrent(t *testing.T) {
	t.Parallel()
	g := NewGomegaWithT(t)

	expr := Field("price")
	se := expr.(ScalarEvaluator)

	orderMsg := &gen.Order{OrderId: proto.Int64(1), Price: proto.Int32(100)}
	orderRec := &FDBStoredRecord[proto.Message]{Record: orderMsg}
	typedMsg := &gen.TypedRecord{Id: proto.Int64(2), Price: proto.Int32(200)}
	typedRec := &FDBStoredRecord[proto.Message]{Record: typedMsg}

	var wg sync.WaitGroup
	errs := make(chan error, 64)
	for i := 0; i < 32; i++ {
		rec, m, want := orderRec, proto.Message(orderMsg), int64(100)
		if i%2 == 1 {
			rec, m, want = typedRec, proto.Message(typedMsg), int64(200)
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 200; j++ {
				got, err := se.EvaluateScalar(rec, m)
				if err != nil {
					errs <- err
					return
				}
				if got != want {
					errs <- &KeyExpressionError{Message: "cache returned the wrong message type's value"}
					return
				}
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		g.Expect(err).NotTo(HaveOccurred())
	}
}

// --- Packer reuse and subspace prefixing ---

// The maintainer packs straight into a pooled packer with a subspace prefix,
// so PackDirect + AppendInto(prefix) must equal the slow path's
// PackWithPrefix.
func TestKeyExpressionFastPath_PackDirectWithSubspacePrefix(t *testing.T) {
	t.Parallel()
	g := NewGomegaWithT(t)

	expr := Concat(Field("order_id"), Field("price"))
	msg := &gen.Order{OrderId: proto.Int64(42), Price: proto.Int32(100)}
	record := &FDBStoredRecord[proto.Message]{Record: msg}
	ss := subspace.Sub(int64(1), int64(2))

	slow, _ := packSlow(t, expr, record, msg)
	expected := slow.PackWithPrefix(ss.Bytes())

	pk := tuple.GetPacker()
	pk.Reset()
	g.Expect(expr.(DirectPacker).PackDirect(pk, record, msg)).To(BeTrue())
	var buf []byte
	got := pk.AppendInto(&buf, ss.Bytes())
	tuple.PutPacker(pk)

	g.Expect(got).To(Equal(expected), "PackDirect with a subspace prefix diverged from PackWithPrefix")
}

// The maintainer reuses one pooled packer across records, so Reset must clear
// it completely — a leftover element from the previous record would be
// prepended to the next record's index key.
func TestKeyExpressionFastPath_PackerResetLeavesNoResidue(t *testing.T) {
	t.Parallel()
	g := NewGomegaWithT(t)

	expr := Field("order_id")
	dp := expr.(DirectPacker)
	pk := tuple.GetPacker()
	defer tuple.PutPacker(pk)

	msg1 := &gen.Order{OrderId: proto.Int64(1)}
	rec1 := &FDBStoredRecord[proto.Message]{Record: msg1}
	pk.Reset()
	g.Expect(dp.PackDirect(pk, rec1, msg1)).To(BeTrue())
	var buf1 []byte
	first := append([]byte(nil), pk.AppendInto(&buf1, nil)...)
	g.Expect(first).To(Equal(tuple.Tuple{int64(1)}.Pack()))

	// Same packer, second record.
	msg2 := &gen.Order{OrderId: proto.Int64(999)}
	rec2 := &FDBStoredRecord[proto.Message]{Record: msg2}
	pk.Reset()
	g.Expect(dp.PackDirect(pk, rec2, msg2)).To(BeTrue())
	var buf2 []byte
	second := pk.AppendInto(&buf2, nil)

	g.Expect(second).To(Equal(tuple.Tuple{int64(999)}.Pack()),
		"packer retained residue from the previous record")
}
