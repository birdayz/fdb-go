//go:build bazelrunfiles

package conformance_test

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"fdb.dev/gen"
	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/fdbgo/fdb/subspace"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	"fdb.dev/pkg/recordlayer"
	"github.com/google/uuid"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/descriptorpb"
	"google.golang.org/protobuf/types/dynamicpb"
)

// mapRecordsFile is a records file whose one record type holds a proto map
// field, m (map<string, int64>), whose entry message is MEntry, and a group, g.
// Protobuf-java's isRepeated() is true for a map field, and Go's IsList() is
// not, and protobuf-java's MESSAGE java type covers a group, which Go's
// MessageKind does not, so these are where the two engines could part.
func mapRecordsFile() *descriptorpb.FileDescriptorProto {
	optional := descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum()
	repeated := descriptorpb.FieldDescriptorProto_LABEL_REPEATED.Enum()
	field := func(name string, number int32, label *descriptorpb.FieldDescriptorProto_Label, typ descriptorpb.FieldDescriptorProto_Type, typeName string) *descriptorpb.FieldDescriptorProto {
		f := &descriptorpb.FieldDescriptorProto{Name: proto.String(name), Number: proto.Int32(number), Label: label, Type: typ.Enum()}
		if typeName != "" {
			f.TypeName = proto.String(typeName)
		}
		return f
	}
	return &descriptorpb.FileDescriptorProto{
		Name:    proto.String("key_validation_map.proto"),
		Package: proto.String("keyvalidation"),
		Syntax:  proto.String("proto2"),
		MessageType: []*descriptorpb.DescriptorProto{
			{
				Name: proto.String("MapRec"),
				Field: []*descriptorpb.FieldDescriptorProto{
					field("id", 1, optional, descriptorpb.FieldDescriptorProto_TYPE_INT64, ""),
					field("m", 2, repeated, descriptorpb.FieldDescriptorProto_TYPE_MESSAGE, ".keyvalidation.MapRec.MEntry"),
					field("g", 3, optional, descriptorpb.FieldDescriptorProto_TYPE_GROUP, ".keyvalidation.MapRec.G"),
				},
				NestedType: []*descriptorpb.DescriptorProto{{
					Name: proto.String("MEntry"),
					Field: []*descriptorpb.FieldDescriptorProto{
						field("key", 1, optional, descriptorpb.FieldDescriptorProto_TYPE_STRING, ""),
						field("value", 2, optional, descriptorpb.FieldDescriptorProto_TYPE_INT64, ""),
					},
					Options: &descriptorpb.MessageOptions{MapEntry: proto.Bool(true)},
				}, {
					Name:  proto.String("G"),
					Field: []*descriptorpb.FieldDescriptorProto{field("x", 1, optional, descriptorpb.FieldDescriptorProto_TYPE_INT64, "")},
				}},
			},
			{
				Name:  proto.String("RecordTypeUnion"),
				Field: []*descriptorpb.FieldDescriptorProto{field("_MapRec", 1, optional, descriptorpb.FieldDescriptorProto_TYPE_MESSAGE, ".keyvalidation.MapRec")},
			},
		},
	}
}

// mapMetaData is serialized meta-data over mapRecordsFile with one value index
// on MapRec whose root is root, written without Go's Build so a root Go
// refuses reaches both loaders.
func mapMetaData(root recordlayer.KeyExpression) *gen.MetaData {
	return &gen.MetaData{
		Records: mapRecordsFile(),
		RecordTypes: []*gen.RecordType{{
			Name:       proto.String("MapRec"),
			PrimaryKey: recordlayer.Field("id").ToKeyExpression(),
		}},
		Indexes: []*gen.Index{{
			Name:                proto.String("idx"),
			RecordType:          []string{"MapRec"},
			RootExpression:      root.ToKeyExpression(),
			AddedVersion:        proto.Int32(1),
			LastModifiedVersion: proto.Int32(1),
		}},
		Version: proto.Int32(1),
	}
}

// demoMetaDataWithRoot is the demo records with one value index on Order whose
// root is root, written without Go's Build.
func demoMetaDataWithRoot(root recordlayer.KeyExpression) *gen.MetaData {
	builder := recordlayer.NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
	builder.GetRecordType("Order").SetPrimaryKey(recordlayer.Field("order_id"))
	builder.GetRecordType("Customer").SetPrimaryKey(recordlayer.Field("customer_id"))
	builder.GetRecordType("TypedRecord").SetPrimaryKey(recordlayer.Field("id"))
	builder.AddIndex("Order", recordlayer.NewIndex("idx", recordlayer.Field("price")))
	md, err := builder.Build()
	Expect(err).NotTo(HaveOccurred())
	p, err := md.ToProto()
	Expect(err).NotTo(HaveOccurred())
	for _, idx := range p.GetIndexes() {
		if idx.GetName() == "idx" {
			idx.RootExpression = root.ToKeyExpression()
		}
	}
	return p
}

// Key validation at build, both loaders on the same bytes. The class is the
// Java exception's full name (the two InvalidExpressionExceptions share a
// simple name), and Go's error type is that class's (errors.go): a
// KeyExpression.InvalidExpressionException is KeyExpressionError, a
// Query.InvalidExpressionException is QueryInvalidExpressionError, an
// UnsupportedOperationException is UnsupportedOperationError. The texts are
// equal, not prefixes.
var _ = Describe("Key validation at build, as Java builds", func() {
	const (
		keyInvalid   = "com.apple.foundationdb.record.metadata.expressions.KeyExpression$InvalidExpressionException"
		queryInvalid = "com.apple.foundationdb.record.query.expressions.Query$InvalidExpressionException"
		unsupported  = "java.lang.UnsupportedOperationException"
	)
	for _, c := range []struct {
		name string
		md   func() *gen.MetaData
		// class is Java's, empty when Java builds; text is Java's message.
		class, text string
	}{
		{"a nesting into a scalar field", func() *gen.MetaData {
			return demoMetaDataWithRoot(recordlayer.Nest("price", recordlayer.Field("type")))
		}, unsupported, "This field is not of message type. (com.apple.foundationdb.record.Order.price)"},
		{"a map field read as a scalar", func() *gen.MetaData {
			return mapMetaData(recordlayer.Field("m"))
		}, keyInvalid, "m is repeated with FanType.None"},
		{"a map field fanned out as a leaf", func() *gen.MetaData {
			return mapMetaData(recordlayer.FanOut("m"))
		}, queryInvalid, "m is a nested message, but accessed as a scalar"},
		{"a map field nested with FanType.None", func() *gen.MetaData {
			return mapMetaData(recordlayer.Nest("m", recordlayer.Field("value")))
		}, keyInvalid, "m is repeated with FanType.None"},
		{"a map field fanned out into a field its entry lacks", func() *gen.MetaData {
			return mapMetaData(recordlayer.NestFanOut("m", recordlayer.Field("nope")))
		}, keyInvalid, "Descriptor MEntry does not have field: nope"},
		{"a map field fanned out into its entry's value", func() *gen.MetaData {
			return mapMetaData(recordlayer.NestFanOut("m", recordlayer.Field("value")))
		}, "", ""},
		{"a group nested into", func() *gen.MetaData {
			return mapMetaData(recordlayer.Nest("g", recordlayer.Field("x")))
		}, "", ""},
		{"a group read as a scalar", func() *gen.MetaData {
			return mapMetaData(recordlayer.Field("g"))
		}, queryInvalid, "g is a nested message, but accessed as a scalar"},
	} {
		It("builds as Java does: "+c.name, func() {
			p := c.md()
			var java javaAnyVerdict
			Expect(NewJavaInvoker().InvokeAs(context.Background(), "buildMetaDataAnyVerdict", map[string]any{
				"protoBytes": bytesToInts(marshalMetaData(p)),
			}, &java)).To(Succeed())
			_, goErr := recordlayer.RecordMetaDataFromProto(proto.Clone(p).(*gen.MetaData))
			fmt.Fprintf(GinkgoWriter, "KEY_VALIDATION %q java=%t %s %q go=%T %v\n", c.name, java.Valid, java.Class, java.Error, goErr, goErr)

			if c.class == "" {
				Expect(java.Valid).To(BeTrue(), "Java: %s %s", java.Class, java.Error)
				Expect(goErr).NotTo(HaveOccurred())
				return
			}
			Expect(java.Valid).To(BeFalse())
			Expect(java.Class).To(Equal(c.class))
			if c.text != "" {
				Expect(java.Error).To(Equal(c.text))
			}
			var goMessage string
			switch c.class {
			case keyInvalid:
				var e *recordlayer.KeyExpressionError
				Expect(errors.As(goErr, &e)).To(BeTrue(), "Go: %T %v", goErr, goErr)
				goMessage = e.Message
			case queryInvalid:
				var e *recordlayer.QueryInvalidExpressionError
				Expect(errors.As(goErr, &e)).To(BeTrue(), "Go: %T %v", goErr, goErr)
				goMessage = e.Message
			case unsupported:
				var e *recordlayer.UnsupportedOperationError
				Expect(errors.As(goErr, &e)).To(BeTrue(), "Go: %T %v", goErr, goErr)
				goMessage = e.Message
			}
			Expect(goMessage).To(Equal(java.Error))
		})
	}
})

// A map field's entries fanned out by a nesting, and a nesting into a group,
// maintained as Java maintains them: each engine saves the same records, one
// transaction each, and the index key-value pairs written are equal.
var _ = Describe("Map and group key expressions are maintained as Java maintains them", func() {
	for _, c := range []struct {
		name      string
		root      recordlayer.KeyExpression
		predicate *gen.Predicate
	}{
		{"a map's entries fanned out", recordlayer.NestFanOut("m", recordlayer.Concat(recordlayer.Field("key"), recordlayer.Field("value"))), nil},
		{"a group nested into", recordlayer.Nest("g", recordlayer.Field("x")), nil},
		// An index predicate's field path steps into a group as into a message.
		{"a predicate over a group's field", recordlayer.Field("id"), &gen.Predicate{ValuePredicate: &gen.ValuePredicate{
			Value: []string{"g", "x"},
			Comparison: &gen.Comparison{SimpleComparison: &gen.SimpleComparison{
				Type: gen.ComparisonType_EQUALS.Enum(), Operand: &gen.Value{LongValue: proto.Int64(7)},
			}},
		}}},
	} {
		It(c.name, func() { expectMaintainedAsJava(c.name, c.root, c.predicate) })
	}
})

// A literal key column is maintained as Java maintains it: Java's
// Key.Evaluated.scalar holds the literal's Integer, Long or Float and Tuple
// packs it, an Integer as the integer it is. Go refused an int_value literal at
// the tuple encoder (an int32), which a store opened under Go-built DDL meta-data
// (int_value since the literal-carrier fix) would meet on its first save.
var _ = Describe("A literal key column is maintained as Java maintains it", func() {
	for _, c := range []struct {
		name string
		lit  any
	}{
		{"an int literal (int_value)", int32(7)},
		{"a long literal (long_value)", int64(7)},
		{"a float literal (float_value)", float32(1.5)},
		{"a string literal", "k"},
	} {
		It(c.name, func() {
			expectMaintainedAsJava(c.name, recordlayer.Concat(recordlayer.Field("id"), recordlayer.Literal(c.lit)), nil)
		})
	}
})

// expectMaintainedAsJava saves the same records over mapRecordsFile through both
// engines, one transaction each, under one value index whose root is root (its
// predicate predicate), and requires the index key-value pairs to be equal.
func expectMaintainedAsJava(label string, root recordlayer.KeyExpression, predicate *gen.Predicate) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	clusterFile, err := sharedContainer.ClusterFile(ctx)
	Expect(err).NotTo(HaveOccurred())
	mdProto := mapMetaData(root)
	mdProto.Indexes[0].Predicate = predicate
	md, err := recordlayer.RecordMetaDataFromProto(proto.Clone(mdProto).(*gen.MetaData))
	Expect(err).NotTo(HaveOccurred())
	desc := md.GetRecordType("MapRec").Descriptor
	record := func(id int64, entries map[string]int64, g *int64) []byte {
		m := dynamicpb.NewMessage(desc)
		m.Set(desc.Fields().ByName("id"), protoreflect.ValueOfInt64(id))
		mf := desc.Fields().ByName("m")
		mm := m.Mutable(mf).Map()
		for k, v := range entries {
			mm.Set(protoreflect.ValueOfString(k).MapKey(), protoreflect.ValueOfInt64(v))
		}
		if g != nil {
			gf := desc.Fields().ByName("g")
			gm := dynamicpb.NewMessage(gf.Message())
			gm.Set(gf.Message().Fields().ByName("x"), protoreflect.ValueOfInt64(*g))
			m.Set(gf, protoreflect.ValueOfMessage(gm))
		}
		b, err := proto.MarshalOptions{Deterministic: true}.Marshal(m)
		Expect(err).NotTo(HaveOccurred())
		return b
	}
	seven := int64(7)
	records := [][]byte{record(1, map[string]int64{"b": 2, "a": 1}, &seven), record(2, nil, nil), record(3, map[string]int64{"c": 3}, nil)}
	mdBytes, err := proto.Marshal(mdProto)
	Expect(err).NotTo(HaveOccurred())
	recordArgs := make([][]int, len(records))
	for i, r := range records {
		recordArgs[i] = BytesToIntArray(r)
	}
	javaSS := subspace.Sub(tuple.Tuple{"mapgroup_java", uuid.NewString()}...)
	var java struct {
		Verdicts []string   `json:"verdicts"`
		KVs      [][]string `json:"kvs"`
	}
	Expect(NewJavaInvoker().InvokeAs(ctx, "saveRecordsAndDumpIndexesJava", map[string]any{
		"clusterFile": clusterFile, "subspace": BytesToIntArray(javaSS.Bytes()),
		"metaData": BytesToIntArray(mdBytes), "recordTypeName": "MapRec", "records": recordArgs,
	}, &java)).To(Succeed())
	GinkgoWriter.Printf("MAPGROUP %q verdicts=%v kvs=%v\n", label, java.Verdicts, java.KVs)
	Expect(java.Verdicts).To(Equal([]string{"ok", "ok", "ok"}))
	Expect(java.KVs).NotTo(BeEmpty())

	goSS := subspace.Sub(tuple.Tuple{"mapgroup_go", uuid.NewString()}...)
	db := recordlayer.NewFDBDatabase(sharedDB)
	for _, rb := range records {
		msg := dynamicpb.NewMessage(desc)
		Expect(proto.Unmarshal(rb, msg)).To(Succeed())
		_, err := db.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
			store, err := recordlayer.NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(md).SetSubspace(goSS).CreateOrOpen()
			if err != nil {
				return nil, err
			}
			_, err = store.SaveRecord(msg)
			return nil, err
		})
		Expect(err).NotTo(HaveOccurred())
	}
	goKVs, err := dumpIndexKVs(ctx, db, goSS)
	Expect(err).NotTo(HaveOccurred())
	Expect(goKVs).To(Equal(java.KVs))
}

// A record's map entries are maintained in the order its bytes hold them, as
// Java maintains them (record_wire_map_order.go). The root is a covering index
// whose key two entries can share, so the stored value is the last entry's:
// the order is visible in the bytes. The records are raw bytes: entries out of
// key order, a key written twice, and an entry with no value. Java saves them
// with the index (a DynamicMessage keeps the bytes' order); Go builds the index
// online over the same records, once as Java wrote them and once as the raw
// bytes themselves, and the index key-value pairs of all three are equal.
var _ = Describe("Map entries are maintained in the record's wire order, as Java maintains them", func() {
	It("a covering index two entries write one key of, and a Go re-save of what Java wrote", func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		clusterFile, err := sharedContainer.ClusterFile(ctx)
		Expect(err).NotTo(HaveOccurred())

		root := recordlayer.KeyWithValue(recordlayer.NestFanOut("m", recordlayer.Concat(recordlayer.Field("value"), recordlayer.Field("key"))), 1)
		withIndex := mapMetaData(root)
		withIndex.Version = proto.Int32(2)
		withIndex.Indexes[0].AddedVersion = proto.Int32(2)
		withIndex.Indexes[0].LastModifiedVersion = proto.Int32(2)
		without := proto.Clone(withIndex).(*gen.MetaData)
		without.Indexes = nil
		without.Version = proto.Int32(1)

		type entry struct {
			key      string
			value    int64
			hasValue bool
		}
		record := func(id int64, entries ...entry) []byte {
			b := protowire.AppendTag(nil, 1, protowire.VarintType)
			b = protowire.AppendVarint(b, uint64(id))
			for _, e := range entries {
				body := protowire.AppendTag(nil, 1, protowire.BytesType)
				body = protowire.AppendString(body, e.key)
				if e.hasValue {
					body = protowire.AppendTag(body, 2, protowire.VarintType)
					body = protowire.AppendVarint(body, uint64(e.value))
				}
				b = protowire.AppendTag(b, 2, protowire.BytesType)
				b = protowire.AppendBytes(b, body)
			}
			return b
		}
		records := [][]byte{
			record(1, entry{"b", 5, true}, entry{"a", 5, true}),
			record(2, entry{"x", 1, true}, entry{"y", 2, true}, entry{"x", 3, true}),
			record(3, entry{"k", 0, false}, entry{"j", 2, true}),
		}
		recordArgs := make([][]int, len(records))
		for i, r := range records {
			recordArgs[i] = BytesToIntArray(r)
		}
		save := func(ss subspace.Subspace, md *gen.MetaData) [][]string {
			mdBytes, err := proto.Marshal(md)
			Expect(err).NotTo(HaveOccurred())
			var java struct {
				Verdicts []string   `json:"verdicts"`
				KVs      [][]string `json:"kvs"`
			}
			Expect(NewJavaInvoker().InvokeAs(ctx, "saveRecordsAndDumpIndexesJava", map[string]any{
				"clusterFile": clusterFile, "subspace": BytesToIntArray(ss.Bytes()),
				"metaData": BytesToIntArray(mdBytes), "recordTypeName": "MapRec", "records": recordArgs,
			}, &java)).To(Succeed())
			Expect(java.Verdicts).To(Equal([]string{"ok", "ok", "ok"}))
			return java.KVs
		}
		javaKVs := save(subspace.Sub(tuple.Tuple{"mapwire_java", uuid.NewString()}...), withIndex)
		GinkgoWriter.Printf("MAPWIRE java kvs=%v\n", javaKVs)
		// Six entries: the two of record 1 share one key.
		Expect(javaKVs).To(HaveLen(6))

		md, err := recordlayer.RecordMetaDataFromProto(proto.Clone(withIndex).(*gen.MetaData))
		Expect(err).NotTo(HaveOccurred())
		db := recordlayer.NewFDBDatabase(sharedDB)
		build := func(ss subspace.Subspace) [][]string {
			indexer, err := recordlayer.NewOnlineIndexerBuilder().
				SetDatabase(db).SetMetaData(md).SetIndex(md.GetIndex("idx")).SetSubspace(ss).Build()
			Expect(err).NotTo(HaveOccurred())
			_, err = indexer.BuildIndex(ctx)
			Expect(err).NotTo(HaveOccurred())
			kvs, err := dumpIndexKVs(ctx, db, ss)
			Expect(err).NotTo(HaveOccurred())
			return kvs
		}

		// Records Java wrote, indexed by Go.
		asJavaWrote := subspace.Sub(tuple.Tuple{"mapwire_go", uuid.NewString()}...)
		Expect(save(asJavaWrote, without)).To(BeEmpty())
		Expect(build(asJavaWrote)).To(Equal(javaKVs))

		// The raw bytes themselves, indexed by Go: every stored record value is
		// replaced by the union-wrapped raw bytes Java parsed.
		raw := subspace.Sub(tuple.Tuple{"mapwire_raw", uuid.NewString()}...)
		Expect(save(raw, without)).To(BeEmpty())
		_, err = db.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
			begin, end := raw.Sub(int64(recordlayer.RecordKey)).FDBRangeKeys()
			kvs, err := rtx.Transaction().GetRange(fdb.KeyRange{Begin: begin, End: end}, fdb.RangeOptions{}).GetSliceWithError()
			if err != nil {
				return nil, err
			}
			Expect(kvs).To(HaveLen(len(records)))
			for _, kv := range kvs {
				t, err := raw.Unpack(kv.Key)
				if err != nil {
					return nil, err
				}
				id := t[1].(int64)
				union := protowire.AppendTag(nil, 1, protowire.BytesType)
				rtx.Transaction().Set(kv.Key, protowire.AppendBytes(union, records[id-1]))
			}
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
		Expect(build(raw)).To(Equal(javaKVs))

		// Go loads each record Java wrote and saves it unchanged, and so does
		// Java: the stored bytes are equal, each map written back in its stored
		// order, the key written twice with both its entries and the entry
		// stored without a value with its default, and neither index changes.
		// The maintained index equals a build over the re-saved bytes, Go's
		// and Java's.
		javaSS := subspace.Sub(tuple.Tuple{"mapwire_resave", uuid.NewString()}...)
		Expect(save(javaSS, withIndex)).To(Equal(javaKVs))
		storedKeys := func(ss subspace.Subspace) map[int64][]string {
			out := map[int64][]string{}
			_, err := db.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
				begin, end := ss.Sub(int64(recordlayer.RecordKey)).FDBRangeKeys()
				kvs, err := rtx.Transaction().GetRange(fdb.KeyRange{Begin: begin, End: end}, fdb.RangeOptions{}).GetSliceWithError()
				if err != nil {
					return nil, err
				}
				for _, kv := range kvs {
					t, err := ss.Unpack(kv.Key)
					if err != nil {
						return nil, err
					}
					_, _, n := protowire.ConsumeTag(kv.Value)
					inner, _ := protowire.ConsumeBytes(kv.Value[n:])
					for len(inner) > 0 {
						num, typ, k := protowire.ConsumeTag(inner)
						size := protowire.ConsumeFieldValue(num, typ, inner[k:])
						if num == 2 {
							entry, _ := protowire.ConsumeBytes(inner[k : k+size])
							_, _, tk := protowire.ConsumeTag(entry)
							key, _ := protowire.ConsumeString(entry[tk:])
							out[t[1].(int64)] = append(out[t[1].(int64)], key)
						}
						inner = inner[k+size:]
					}
				}
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
			return out
		}
		Expect(storedKeys(javaSS)).To(Equal(map[int64][]string{1: {"b", "a"}, 2: {"x", "y", "x"}, 3: {"k", "j"}}))
		resave := func(ss subspace.Subspace, md *recordlayer.RecordMetaData) {
			for id := int64(1); id <= 3; id++ {
				_, err := db.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
					store, err := recordlayer.NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ss).Open()
					if err != nil {
						return nil, err
					}
					rec, err := store.LoadRecord(tuple.Tuple{id})
					if err != nil {
						return nil, err
					}
					_, err = store.SaveRecord(rec.Record)
					return nil, err
				})
				Expect(err).NotTo(HaveOccurred())
			}
		}
		resave(javaSS, md)
		// Java's load-then-save of the same records, for its bytes.
		javaResave := subspace.Sub(tuple.Tuple{"mapwire_javaresave", uuid.NewString()}...)
		Expect(save(javaResave, withIndex)).To(Equal(javaKVs))
		mdBytes, err := proto.Marshal(withIndex)
		Expect(err).NotTo(HaveOccurred())
		var javaResaved struct {
			Records [][]string `json:"records"`
			KVs     [][]string `json:"kvs"`
		}
		Expect(NewJavaInvoker().InvokeAs(ctx, "resaveRecordsJava", map[string]any{
			"clusterFile": clusterFile, "subspace": BytesToIntArray(javaResave.Bytes()),
			"metaData": BytesToIntArray(mdBytes), "count": len(records),
		}, &javaResaved)).To(Succeed())
		recordKVs := func(ss subspace.Subspace) [][]string {
			var out [][]string
			_, err := db.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
				begin, end := ss.Sub(int64(recordlayer.RecordKey)).FDBRangeKeys()
				kvs, err := rtx.Transaction().GetRange(fdb.KeyRange{Begin: begin, End: end}, fdb.RangeOptions{}).GetSliceWithError()
				if err != nil {
					return nil, err
				}
				for _, kv := range kvs {
					out = append(out, []string{hex.EncodeToString(kv.Key[len(ss.Bytes()):]), hex.EncodeToString(kv.Value)})
				}
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
			return out
		}
		GinkgoWriter.Printf("MAPWIRE java re-save records=%v kvs=%v\n", javaResaved.Records, javaResaved.KVs)
		GinkgoWriter.Printf("MAPWIRE go re-save records=%v\n", recordKVs(javaSS))
		Expect(javaResaved.Records).To(HaveLen(len(records)))
		Expect(javaResaved.KVs).To(Equal(javaKVs))
		Expect(recordKVs(javaSS)).To(Equal(javaResaved.Records))
		resaved := map[int64][]string{1: {"b", "a"}, 2: {"x", "y", "x"}, 3: {"k", "j"}}
		Expect(storedKeys(javaSS)).To(Equal(resaved))
		maintained, err := dumpIndexKVs(ctx, db, javaSS)
		Expect(err).NotTo(HaveOccurred())
		Expect(maintained).To(Equal(javaKVs))

		// The re-saved bytes, copied under a store Java wrote without the
		// index: Go builds the index over them, and Java, opening the store
		// with the index, rebuilds it from them; both equal the maintained one.
		copyRecords := func(from, to subspace.Subspace) {
			_, err := db.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
				begin, end := from.Sub(int64(recordlayer.RecordKey)).FDBRangeKeys()
				kvs, err := rtx.Transaction().GetRange(fdb.KeyRange{Begin: begin, End: end}, fdb.RangeOptions{}).GetSliceWithError()
				if err != nil {
					return nil, err
				}
				Expect(kvs).To(HaveLen(len(records)))
				for _, kv := range kvs {
					rtx.Transaction().Set(fdb.Key(append(append([]byte(nil), to.Bytes()...), kv.Key[len(from.Bytes()):]...)), kv.Value)
				}
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		}
		rebuilt := subspace.Sub(tuple.Tuple{"mapwire_rebuilt", uuid.NewString()}...)
		Expect(save(rebuilt, without)).To(BeEmpty())
		copyRecords(javaSS, rebuilt)
		Expect(build(rebuilt)).To(Equal(maintained))
		openJava := func(ss subspace.Subspace, md *gen.MetaData) [][]string {
			mdBytes, err := proto.Marshal(md)
			Expect(err).NotTo(HaveOccurred())
			var java struct {
				KVs [][]string `json:"kvs"`
			}
			Expect(NewJavaInvoker().InvokeAs(ctx, "openStoreAndDumpIndexesJava", map[string]any{
				"clusterFile": clusterFile, "subspace": BytesToIntArray(ss.Bytes()), "metaData": BytesToIntArray(mdBytes),
			}, &java)).To(Succeed())
			return java.KVs
		}
		javaRead := subspace.Sub(tuple.Tuple{"mapwire_javaread", uuid.NewString()}...)
		Expect(save(javaRead, without)).To(BeEmpty())
		copyRecords(javaSS, javaRead)
		javaRebuilt := openJava(javaRead, withIndex)
		GinkgoWriter.Printf("MAPWIRE java rebuild of the go re-save kvs=%v\n", javaRebuilt)
		Expect(javaRebuilt).To(Equal(maintained))

		// A record stored under the older of its type's two union fields (both
		// loaders prefer _MapRec, the field named for the type): Go's re-save
		// finds the stored map order under the field it decoded the record
		// from, and writes the record under the preferred field.
		twoFields := func(md *gen.MetaData) *gen.MetaData {
			c := proto.Clone(md).(*gen.MetaData)
			for _, m := range c.Records.MessageType {
				if m.GetName() == "RecordTypeUnion" {
					m.Field = append(m.Field, &descriptorpb.FieldDescriptorProto{
						Name: proto.String("MapRec_v0"), Number: proto.Int32(2),
						Label: descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(), Type: descriptorpb.FieldDescriptorProto_TYPE_MESSAGE.Enum(),
						TypeName: proto.String(".keyvalidation.MapRec"),
					})
				}
			}
			return c
		}
		unionNumbers := func(ss subspace.Subspace, rewrapAs protowire.Number) []protowire.Number {
			var out []protowire.Number
			_, err := db.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
				begin, end := ss.Sub(int64(recordlayer.RecordKey)).FDBRangeKeys()
				kvs, err := rtx.Transaction().GetRange(fdb.KeyRange{Begin: begin, End: end}, fdb.RangeOptions{}).GetSliceWithError()
				if err != nil {
					return nil, err
				}
				for _, kv := range kvs {
					num, _, n := protowire.ConsumeTag(kv.Value)
					out = append(out, num)
					if rewrapAs != 0 {
						inner, _ := protowire.ConsumeBytes(kv.Value[n:])
						rtx.Transaction().Set(kv.Key, protowire.AppendBytes(protowire.AppendTag(nil, rewrapAs, protowire.BytesType), inner))
					}
				}
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
			return out
		}
		older := subspace.Sub(tuple.Tuple{"mapwire_older", uuid.NewString()}...)
		Expect(save(older, twoFields(withIndex))).To(Equal(javaKVs))
		Expect(unionNumbers(older, 2)).To(Equal([]protowire.Number{1, 1, 1}))
		Expect(unionNumbers(older, 0)).To(Equal([]protowire.Number{2, 2, 2}))
		olderMD, err := recordlayer.RecordMetaDataFromProto(twoFields(withIndex))
		Expect(err).NotTo(HaveOccurred())
		resave(older, olderMD)
		Expect(unionNumbers(older, 0)).To(Equal([]protowire.Number{1, 1, 1}))
		Expect(recordKVs(older)).To(Equal(javaResaved.Records))
		Expect(storedKeys(older)).To(Equal(resaved))
		olderMaintained, err := dumpIndexKVs(ctx, db, older)
		Expect(err).NotTo(HaveOccurred())
		Expect(olderMaintained).To(Equal(javaKVs))
	})
})

// validatorIndex is one serialized index for indexValidationMetaData.
type validatorIndex struct {
	name, recordType, typ string
	root                  recordlayer.KeyExpression
	options               map[string]string
	// added and lastModified default to 1; key is the subspace key, the name
	// when empty.
	added, lastModified int32
	key                 string
}

// indexValidationMetaData is the demo records at version 5 with the given
// indexes and former indexes, serialized without Go's Build so a shape Go
// refuses reaches both loaders.
func indexValidationMetaData(storeVersions bool, indexes []validatorIndex, formers []*gen.FormerIndex) *gen.MetaData {
	builder := recordlayer.NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
	builder.GetRecordType("Order").SetPrimaryKey(recordlayer.Field("order_id"))
	builder.GetRecordType("Customer").SetPrimaryKey(recordlayer.Field("customer_id"))
	builder.GetRecordType("TypedRecord").SetPrimaryKey(recordlayer.Field("id"))
	md, err := builder.Build()
	Expect(err).NotTo(HaveOccurred())
	p, err := md.ToProto()
	Expect(err).NotTo(HaveOccurred())
	p.Version = proto.Int32(5)
	p.StoreRecordVersions = proto.Bool(storeVersions)
	for _, vi := range indexes {
		added, lastModified := vi.added, vi.lastModified
		if added == 0 {
			added = 1
		}
		if lastModified == 0 {
			lastModified = 1
		}
		key := vi.key
		if key == "" {
			key = vi.name
		}
		idx := &gen.Index{
			Name:                proto.String(vi.name),
			RecordType:          []string{vi.recordType},
			Type:                proto.String(vi.typ),
			RootExpression:      vi.root.ToKeyExpression(),
			SubspaceKey:         tuple.Tuple{key}.Pack(),
			AddedVersion:        proto.Int32(added),
			LastModifiedVersion: proto.Int32(lastModified),
		}
		for k, v := range vi.options {
			idx.Options = append(idx.Options, &gen.Index_Option{Key: proto.String(k), Value: proto.String(v)})
		}
		p.Indexes = append(p.Indexes, idx)
	}
	p.FormerIndexes = append(p.FormerIndexes, formers...)
	return p
}

func formerIndex(name, key string, added, removed int32) *gen.FormerIndex {
	return &gen.FormerIndex{FormerName: proto.String(name), SubspaceKey: tuple.Tuple{key}.Pack(), AddedVersion: proto.Int32(added), RemovedVersion: proto.Int32(removed)}
}

// Build's index validation, both loaders on the same bytes: each index type's
// validator (IndexValidator.java and each factory's getIndexValidator) with
// Java's text and class, and MetaDataValidator.validateCurrentAndFormerIndexes'
// order, index by index. Two faults on two indexes are placed so that the
// per-index order and a per-check order disagree; the index names ("a", "b")
// are in the same order in Java's HashMap and Go's name order, which is the
// only order this spec relies on.
var _ = Describe("Index validation at build, as Java builds", func() {
	const (
		keyInvalid = "com.apple.foundationdb.record.metadata.expressions.KeyExpression$InvalidExpressionException"
		metaData   = "com.apple.foundationdb.record.metadata.MetaDataException"
		// outOfBounds marks a row where Java throws an IndexOutOfBoundsException
		// and Go its declared refusal (see the rows).
		outOfBounds = "IndexOutOfBoundsException"
	)
	grouped := func(groupedField string, groupBy ...string) recordlayer.KeyExpression {
		keys := make([]recordlayer.KeyExpression, len(groupBy))
		for i, g := range groupBy {
			keys[i] = recordlayer.Field(g)
		}
		return recordlayer.GroupBy(recordlayer.Field(groupedField), keys...)
	}
	for _, c := range []struct {
		name          string
		storeVersions bool
		indexes       []validatorIndex
		formers       []*gen.FormerIndex
		class, text   string
	}{
		// The validators.
		{
			"a value index over a grouping", false,
			[]validatorIndex{{name: "a", recordType: "Order", typ: "value", root: grouped("price", "quantity")}},
			nil,
			keyInvalid, "grouping not possible in index type",
		},
		{
			"a value index over a version", false,
			[]validatorIndex{{name: "a", recordType: "Order", typ: "value", root: recordlayer.Concat(recordlayer.Field("price"), recordlayer.VersionKey())}},
			nil,
			keyInvalid, "version key not possible in index type",
		},
		// Measured: both loaders read a rank root that is not a grouping as
		// one grouped column, so it reaches the validator valid.
		{
			"a rank index over a plain field", false,
			[]validatorIndex{{name: "a", recordType: "Order", typ: "rank", root: recordlayer.Field("price")}},
			nil,
			"", "",
		},
		{
			"a rank index grouping everything", false,
			[]validatorIndex{{name: "a", recordType: "Order", typ: "rank", root: recordlayer.GroupAll(recordlayer.Field("price"))}},
			nil,
			keyInvalid, "index type requires grouping at least 1 fields",
		},
		{
			"a leaderboard without a grouping", false,
			[]validatorIndex{{name: "a", recordType: "Order", typ: "time_window_leaderboard", root: recordlayer.Field("price")}},
			nil,
			keyInvalid, "index type requires grouping",
		},
		{
			"a count with a grouped field", false,
			[]validatorIndex{{name: "a", recordType: "Order", typ: "count", root: grouped("price", "quantity")}},
			nil,
			keyInvalid, "index type does not support non-group fields; use COUNT_NOT_NULL",
		},
		{
			"a sum of two fields", false,
			[]validatorIndex{{name: "a", recordType: "Order", typ: "sum", root: recordlayer.GroupBy(recordlayer.Concat(recordlayer.Field("price"), recordlayer.Field("quantity")), recordlayer.Field("order_id"))}},
			nil,
			keyInvalid, "index type only supports single field",
		},
		{
			"a sum of a string", false,
			[]validatorIndex{{name: "a", recordType: "Customer", typ: "sum", root: grouped("name", "price")}},
			nil,
			keyInvalid, "index type only supports integer field",
		},
		{
			"a sum of an sfixed32", false,
			[]validatorIndex{{name: "a", recordType: "TypedRecord", typ: "sum", root: grouped("val_sfixed32", "price")}},
			nil,
			keyInvalid, "index type only supports integer field",
		},
		{
			"count updates cleared when zero", false,
			[]validatorIndex{{name: "a", recordType: "Order", typ: "count_updates", root: recordlayer.GroupAll(recordlayer.Field("price")), options: map[string]string{"clearWhenZero": "true"}}},
			nil,
			metaData, "index type does not support clearWhenZero",
		},
		{
			"count not null cleared when zero over a grouped field", false,
			[]validatorIndex{{name: "a", recordType: "Order", typ: "count_not_null", root: grouped("price", "quantity"), options: map[string]string{"clearWhenZero": "true"}}},
			nil,
			keyInvalid, "index type does not support non-group fields; use COUNT_NOT_NULL",
		},
		{
			"a bitmap over a string position", false,
			[]validatorIndex{{name: "a", recordType: "Customer", typ: "bitmap_value", root: grouped("name", "price")}},
			nil,
			keyInvalid, "index type only supports integer position key",
		},
		{
			"a bitmap over an sfixed32 position", false,
			[]validatorIndex{{name: "a", recordType: "TypedRecord", typ: "bitmap_value", root: grouped("val_sfixed32", "price")}},
			nil,
			"", "",
		},
		{
			"a bitmap of two positions", false,
			[]validatorIndex{{name: "a", recordType: "Order", typ: "bitmap_value", root: recordlayer.GroupBy(recordlayer.Concat(recordlayer.Field("price"), recordlayer.Field("quantity")), recordlayer.Field("order_id"))}},
			nil,
			keyInvalid, "index type needs grouped position",
		},
		{
			"a version index without record versions", false,
			[]validatorIndex{{name: "a", recordType: "Order", typ: "version", root: recordlayer.VersionKey()}},
			nil,
			metaData, "index type requires metadata store record version",
		},
		{
			"a max ever version without record versions", false,
			[]validatorIndex{{name: "a", recordType: "Order", typ: "max_ever_version", root: recordlayer.Ungrouped(recordlayer.VersionKey())}},
			nil,
			"", "",
		},
		{
			"a multidimensional index without dimensions", false,
			[]validatorIndex{{name: "a", recordType: "Order", typ: "multidimensional", root: recordlayer.Concat(recordlayer.Field("coord_x"), recordlayer.Field("coord_y"))}},
			nil,
			keyInvalid, "no dimensions key expression or at incorrect place in index",
		},
		{
			"a unique text index", false,
			[]validatorIndex{{name: "a", recordType: "Customer", typ: "text", root: recordlayer.Field("name"), options: map[string]string{"unique": "true"}}},
			nil,
			metaData, "index type does not allow unique indexes",
		},
		{
			"a text index with an empty tokenizer name", false,
			[]validatorIndex{{name: "a", recordType: "Customer", typ: "text", root: recordlayer.Field("name"), options: map[string]string{"textTokenizerName": ""}}},
			nil,
			metaData, "unrecognized text tokenizer",
		},
		// TEXT: the tokenizer and its version, the value, the text field.
		{
			"a text index with an unknown tokenizer", false,
			[]validatorIndex{{name: "a", recordType: "Customer", typ: "text", root: recordlayer.Field("name"), options: map[string]string{"textTokenizerName": "nope"}}},
			nil,
			metaData, "unrecognized text tokenizer",
		},
		{
			"a text index whose tokenizer version is not a number", false,
			[]validatorIndex{{name: "a", recordType: "Customer", typ: "text", root: recordlayer.Field("name"), options: map[string]string{"textTokenizerVersion": "x"}}},
			nil,
			metaData, "tokenizer version could not be parsed as int",
		},
		{
			"a text index whose tokenizer version is above the tokenizer's", false,
			[]validatorIndex{{name: "a", recordType: "Customer", typ: "text", root: recordlayer.Field("name"), options: map[string]string{"textTokenizerVersion": "99"}}},
			nil,
			metaData, "unknown tokenizer version",
		},
		{
			"a text index with a value", false,
			[]validatorIndex{{name: "a", recordType: "Customer", typ: "text", root: recordlayer.KeyWithValue(recordlayer.Concat(recordlayer.Field("name"), recordlayer.Field("customer_id")), 1)}},
			nil,
			keyInvalid, "no value expression allowed in index type",
		},
		{
			"a text index over a number", false,
			[]validatorIndex{{name: "a", recordType: "Customer", typ: "text", root: recordlayer.Field("customer_id")}},
			nil,
			keyInvalid, "text index has non-string type as text field",
		},
		{
			"a text index over a repeated string", false,
			[]validatorIndex{{name: "a", recordType: "Order", typ: "text", root: recordlayer.FanOut("tags")}},
			nil,
			keyInvalid, "text index does not allow a repeated field for text body",
		},
		// VERSION.
		{
			"a version index of two versions", true,
			[]validatorIndex{{name: "a", recordType: "Order", typ: "version", root: recordlayer.Concat(recordlayer.VersionKey(), recordlayer.VersionKey())}},
			nil,
			keyInvalid, "there must be exactly 1 version entry in index",
		},
		{
			"a version index without a version", true,
			[]validatorIndex{{name: "a", recordType: "Order", typ: "version", root: recordlayer.Field("price")}},
			nil,
			keyInvalid, "there must be exactly 1 version entry in index",
		},
		{
			"a unique version index", true,
			[]validatorIndex{{name: "a", recordType: "Order", typ: "version", root: recordlayer.VersionKey(), options: map[string]string{"unique": "true"}}},
			nil,
			metaData, "index type does not allow unique indexes",
		},
		// MAX_EVER_VERSION's version columns.
		{
			"a max ever version grouped by a version", true,
			[]validatorIndex{{name: "a", recordType: "Order", typ: "max_ever_version", root: recordlayer.GroupBy(recordlayer.VersionKey(), recordlayer.VersionKey())}},
			nil,
			keyInvalid, "there must be no version entries in grouping key in index",
		},
		{
			"a max ever version of no version", true,
			[]validatorIndex{{name: "a", recordType: "Order", typ: "max_ever_version", root: grouped("price", "quantity")}},
			nil,
			keyInvalid, "there must be exactly 1 version entry in grouped key in index",
		},
		// MULTIDIMENSIONAL and the dimensions key.
		{
			"dimensions wider than their key", false,
			[]validatorIndex{{name: "a", recordType: "Order", typ: "multidimensional", root: recordlayer.Dimensions(recordlayer.Concat(recordlayer.Field("coord_x"), recordlayer.Field("coord_y")), 1, 2)}},
			nil,
			keyInvalid, "dimensions declared a prefix size and number of dimensions that are together larger than the number of columns in the index",
		},
		{
			"dimensions over int32 fields", false,
			[]validatorIndex{{name: "a", recordType: "Order", typ: "multidimensional", root: recordlayer.Dimensions(recordlayer.Concat(recordlayer.Field("price"), recordlayer.Field("quantity")), 0, 2)}},
			nil,
			keyInvalid, "the declared dimension columns have to be of type INT64",
		},
		{
			"dimensions over int64 fields", false,
			[]validatorIndex{{name: "a", recordType: "Order", typ: "multidimensional", root: recordlayer.Dimensions(recordlayer.Concat(recordlayer.Field("coord_x"), recordlayer.Field("coord_y")), 0, 2)}},
			nil,
			"", "",
		},
		{
			"dimensions that do not cover the key", false,
			[]validatorIndex{{name: "a", recordType: "Order", typ: "multidimensional", root: recordlayer.KeyWithValue(recordlayer.Concat(
				recordlayer.Dimensions(recordlayer.Concat(recordlayer.Field("coord_x"), recordlayer.Field("coord_y")), 0, 2), recordlayer.Field("price"), recordlayer.Field("quantity")), 3)}},
			nil,
			keyInvalid, "dimensions key expression must cover exactly all key parts in index",
		},
		// The dimension positions index the validated field list, which has no
		// entry for a literal: the fields after one take its place in both
		// engines. A position outside the list is Java's unguarded List.get,
		// an IndexOutOfBoundsException, and Go's declared INT64 refusal
		// (key_expression_validate.go, validateDimensionsKeyExpression).
		{
			"dimensions after a literal read the next fields", false,
			[]validatorIndex{{name: "a", recordType: "Order", typ: "multidimensional", root: recordlayer.Dimensions(recordlayer.Concat(recordlayer.Literal(int64(7)), recordlayer.Field("coord_x"), recordlayer.Field("coord_y")), 0, 2)}},
			nil,
			"", "",
		},
		{
			"dimensions past the fields", false,
			[]validatorIndex{{name: "a", recordType: "Order", typ: "multidimensional", root: recordlayer.Dimensions(recordlayer.Concat(recordlayer.Field("coord_x"), recordlayer.Literal(int64(7))), 0, 2)}},
			nil,
			outOfBounds, "",
		},
		{
			"a negative dimensions prefix", false,
			[]validatorIndex{{name: "a", recordType: "Order", typ: "multidimensional", root: recordlayer.Dimensions(recordlayer.Concat(recordlayer.Field("coord_x"), recordlayer.Field("coord_y")), -1, 2)}},
			nil,
			outOfBounds, "",
		},
		// CARDINALITY's argument.
		{
			"a cardinality over a fanned-out field", false,
			[]validatorIndex{{name: "a", recordType: "Order", typ: "value", root: recordlayer.CardinalityExpr(recordlayer.FanOut("tags"))}},
			nil,
			keyInvalid, "The CARDINALITY() argument must produce a single value.",
		},
		{
			"a cardinality over a missing field", false,
			[]validatorIndex{{name: "a", recordType: "Order", typ: "value", root: recordlayer.CardinalityExpr(recordlayer.FieldConcatenate("nope"))}},
			nil,
			keyInvalid, "Descriptor Order does not have field: nope",
		},
		// A bare root on an aggregate type: Java's Index(proto) wraps it for
		// RANK, COUNT, MAX_EVER, MIN_EVER and SUM (Index.java:206-213), COUNT
		// with every column grouped, the others with one.
		{
			"a count over a bare field", false,
			[]validatorIndex{{name: "a", recordType: "Order", typ: "count", root: recordlayer.Field("price")}},
			nil,
			keyInvalid, "index type does not support non-group fields; use COUNT_NOT_NULL",
		},
		{
			"a sum over a bare field", false,
			[]validatorIndex{{name: "a", recordType: "Order", typ: "sum", root: recordlayer.Field("price")}},
			nil,
			"", "",
		},
		{
			"a max ever over a bare field", false,
			[]validatorIndex{{name: "a", recordType: "Order", typ: "max_ever", root: recordlayer.Field("price")}},
			nil,
			"", "",
		},
		{
			"a min ever long over a bare field", false,
			[]validatorIndex{{name: "a", recordType: "Order", typ: "min_ever_long", root: recordlayer.Field("price")}},
			nil,
			keyInvalid, "index type requires grouping",
		},
		{
			"a leaderboard over a bare Then", false,
			[]validatorIndex{{name: "a", recordType: "Order", typ: "time_window_leaderboard", root: recordlayer.Concat(recordlayer.Field("price"), recordlayer.Field("quantity"))}},
			nil,
			keyInvalid, "index type requires grouping",
		},
		// The order.
		{"a validator before a shared subspace key", false, []validatorIndex{
			{name: "a", recordType: "Order", typ: "value", root: recordlayer.Field("price"), key: "k"},
			{name: "b", recordType: "Order", typ: "permuted_min", root: grouped("price", "quantity"), key: "k"},
		}, nil, metaData, "permuted size not specified"},
		{"an index's own checks before a later index's key", false, []validatorIndex{
			{name: "a", recordType: "Order", typ: "value", root: recordlayer.Field("price"), added: 2, lastModified: 1},
			{name: "b", recordType: "Order", typ: "value", root: recordlayer.Field("nope")},
		}, nil, metaData, "Index a has added version 2 which is greater than the last modified version 1"},
		{"the added version before the type's checks", false, []validatorIndex{
			{name: "a", recordType: "Order", typ: "value", root: grouped("price", "quantity"), added: 2, lastModified: 1},
		}, nil, metaData, "Index a has added version 2 which is greater than the last modified version 1"},
		{"the type's checks before the meta-data version", false, []validatorIndex{
			{name: "a", recordType: "Order", typ: "value", root: grouped("price", "quantity"), lastModified: 9},
		}, nil, keyInvalid, "grouping not possible in index type"},
		{"an index before a former index", false, []validatorIndex{
			{name: "a", recordType: "Order", typ: "value", root: recordlayer.Field("nope")},
		}, []*gen.FormerIndex{formerIndex("f", "f", 3, 2)}, keyInvalid, "Descriptor Order does not have field: nope"},
		{
			"former indexes before an index sharing a key with one", false,
			[]validatorIndex{
				{name: "a", recordType: "Order", typ: "value", root: recordlayer.Field("price"), key: "k"},
			},
			[]*gen.FormerIndex{formerIndex("f1", "k", 1, 2), formerIndex("f2", "j", 1, 2), formerIndex("f3", "j", 1, 2)},
			metaData, "Same subspace key j used by two former indexes f3 and f2",
		},
	} {
		It("builds as Java does: "+c.name, func() {
			p := indexValidationMetaData(c.storeVersions, c.indexes, c.formers)
			var java javaAnyVerdict
			Expect(NewJavaInvoker().InvokeAs(context.Background(), "buildMetaDataAnyVerdict", map[string]any{
				"protoBytes": bytesToInts(marshalMetaData(p)),
			}, &java)).To(Succeed())
			_, goErr := recordlayer.RecordMetaDataFromProto(proto.Clone(p).(*gen.MetaData))
			fmt.Fprintf(GinkgoWriter, "INDEX_VALIDATION %q java=%t %s %q go=%T %v\n", c.name, java.Valid, java.Class, java.Error, goErr, goErr)
			if c.class == "" {
				Expect(java.Valid).To(BeTrue(), "Java: %s %s", java.Class, java.Error)
				Expect(goErr).NotTo(HaveOccurred())
				return
			}
			Expect(java.Valid).To(BeFalse())
			if c.class == outOfBounds {
				// Java's unguarded List.get: the class is the JDK's or Guava's
				// IndexOutOfBoundsException, whose text is the list's own; Go's
				// verdict is the declared INT64 refusal.
				Expect(java.Class).To(HaveSuffix("IndexOutOfBoundsException"))
				var e *recordlayer.KeyExpressionError
				Expect(errors.As(goErr, &e)).To(BeTrue(), "Go: %T %v", goErr, goErr)
				Expect(e.Message).To(Equal("the declared dimension columns have to be of type INT64"))
				return
			}
			Expect(java.Class).To(Equal(c.class))
			Expect(java.Error).To(Equal(c.text))
			var goMessage string
			switch c.class {
			case keyInvalid:
				var e *recordlayer.KeyExpressionError
				Expect(errors.As(goErr, &e)).To(BeTrue(), "Go: %T %v", goErr, goErr)
				goMessage = e.Message
			case metaData:
				var e *recordlayer.MetaDataError
				Expect(errors.As(goErr, &e)).To(BeTrue(), "Go: %T %v", goErr, goErr)
				goMessage = e.Message
			}
			Expect(goMessage).To(Equal(java.Error))
		})
	}
})

// A windowed VECTOR index's integer and double options parse as Java's
// VectorOptionKey parses them, Integer::parseInt and Double::parseDouble
// (VectorOptionKey.java:213, :220): each row edits one option of a windowed
// index Java builds (the sliding-window conformance shape) and requires the
// same verdict from both loaders, a refusal being Java's MetaDataException
// "incorrect index options", whose cause Go carries (Unwrap).
var _ = Describe("A windowed VECTOR index's options parse as Java parses them", func() {
	windowed := func(option, value string) *gen.MetaData {
		return windowedEdited(func(ix *gen.Index) {
			set := false
			for _, o := range ix.Options {
				if o.GetKey() == option {
					o.Value, set = proto.String(value), true
				}
			}
			if !set {
				ix.Options = append(ix.Options, &gen.Index_Option{Key: proto.String(option), Value: proto.String(value)})
			}
		})
	}
	for _, c := range []struct{ option, value string }{
		{recordlayer.IndexOptionHNSWM, "16"},
		{recordlayer.IndexOptionHNSWM, "2147483648"},
		{recordlayer.IndexOptionHNSWM, "１６"},
		{recordlayer.IndexOptionHNSWM, "+16"},
		{recordlayer.IndexOptionHNSWM, "0x10"},
		{recordlayer.IndexOptionHNSWM, " 16"},
		{recordlayer.IndexOptionHNSWSampleVectorStatsProbability, "0.5"},
		{recordlayer.IndexOptionHNSWSampleVectorStatsProbability, "0.5d"},
		{recordlayer.IndexOptionHNSWSampleVectorStatsProbability, " 0.5 "},
		{recordlayer.IndexOptionHNSWSampleVectorStatsProbability, ".5"},
		{recordlayer.IndexOptionHNSWSampleVectorStatsProbability, "inf"},
		{recordlayer.IndexOptionHNSWSampleVectorStatsProbability, "nan"},
		{recordlayer.IndexOptionHNSWSampleVectorStatsProbability, "1_0"},
		{recordlayer.IndexOptionHNSWSampleVectorStatsProbability, "1.2.3"},
		{recordlayer.IndexOptionHNSWSampleVectorStatsProbability, ""},
		{recordlayer.IndexOptionHNSWM, ""},
		{recordlayer.IndexOptionVectorMetric, "COSINE_METRIC"},
		{recordlayer.IndexOptionVectorMetric, "EUCLIDEAN_SQUARE_METRIC"},
		{recordlayer.IndexOptionVectorMetric, "cosine"},
		{recordlayer.IndexOptionVectorMetric, "inner_product"},
		{recordlayer.IndexOptionVectorMetric, "euclidean"},
		{recordlayer.IndexOptionVectorMetric, "COSINE_METRIC "},
	} {
		It(fmt.Sprintf("%s=%q", c.option, c.value), func() {
			expectWindowedVerdictAsJava(fmt.Sprintf("%s=%q", c.option, c.value), windowed(c.option, c.value))
		})
	}
})

// The configuration each engine READS from a windowed VECTOR index's options,
// for spellings only Java's parsers read as the number (full-width digits, a
// sign, a suffix, padding, an exponent): the validator admitting a value is not
// enough, the maintainer must read the same number from it. Java's side is
// HnswVectorIndexEngine.parseConfig (through HnswConformanceAccess), Go's
// recordlayer.HNSWConfigOf. Each value differs from the default, so a reader that
// fell back to the default would be seen.
var _ = Describe("A windowed VECTOR index's options are read as Java reads them", func() {
	for _, c := range []struct {
		options map[string]string
	}{
		{map[string]string{recordlayer.IndexOptionHNSWM: "８", recordlayer.IndexOptionHNSWMMax: "+12", recordlayer.IndexOptionHNSWMMax0: "２４"}},
		{map[string]string{recordlayer.IndexOptionHNSWEfConstruction: "１５０", recordlayer.IndexOptionHNSWStatsThreshold: "-3"}},
		{map[string]string{recordlayer.IndexOptionHNSWSampleVectorStatsProbability: "0.25d", recordlayer.IndexOptionHNSWMaintainStatsProbability: " 2.5e-1 "}},
		{map[string]string{recordlayer.IndexOptionHNSWSampleVectorStatsProbability: "0x1p-2", recordlayer.IndexOptionHNSWMaintainStatsProbability: ".125F"}},
	} {
		It(fmt.Sprintf("%v", c.options), func() {
			p := windowedEdited(func(ix *gen.Index) {
				for k, v := range c.options {
					ix.Options = append(ix.Options, &gen.Index_Option{Key: proto.String(k), Value: proto.String(v)})
				}
			})
			var java map[string]float64
			Expect(NewJavaInvoker().InvokeAs(context.Background(), "vectorIndexConfigJava", map[string]any{
				"protoBytes": bytesToInts(marshalMetaData(p)), "indexName": "w",
			}, &java)).To(Succeed())
			md, err := recordlayer.RecordMetaDataFromProto(proto.Clone(p).(*gen.MetaData))
			Expect(err).NotTo(HaveOccurred())
			cfg := recordlayer.HNSWConfigOf(md.GetIndex("w"))
			goValues := map[string]float64{
				"numDimensions": float64(cfg.NumDimensions), "m": float64(cfg.M), "mMax": float64(cfg.MMax),
				"mMax0": float64(cfg.MMax0), "efConstruction": float64(cfg.EfConstruction), "efRepair": float64(cfg.EfRepair),
				"statsThreshold": float64(cfg.StatsThreshold), "sampleVectorStatsProbability": cfg.SampleVectorStatsProbability,
				"maintainStatsProbability": cfg.MaintainStatsProbability,
			}
			fmt.Fprintf(GinkgoWriter, "VECTOR_CONFIG %v java=%v go=%v\n", c.options, java, goValues)
			for key := range c.options {
				name := map[string]string{
					recordlayer.IndexOptionHNSWM: "m", recordlayer.IndexOptionHNSWMMax: "mMax", recordlayer.IndexOptionHNSWMMax0: "mMax0",
					recordlayer.IndexOptionHNSWEfConstruction: "efConstruction", recordlayer.IndexOptionHNSWStatsThreshold: "statsThreshold",
					recordlayer.IndexOptionHNSWSampleVectorStatsProbability: "sampleVectorStatsProbability",
					recordlayer.IndexOptionHNSWMaintainStatsProbability:     "maintainStatsProbability",
				}[key]
				Expect(name).NotTo(BeEmpty(), key)
				Expect(goValues[name]).To(Equal(java[name]), "%s=%q", key, c.options[key])
			}
		})
	}
})

// The sliding-window validator's arms, whole text and class on both engines:
// Java's MetaDataException for each of its own arms and for the vector
// validator's missing dimension count, and IndexPredicate's RecordCoreException
// for a window under a disjunction.
var _ = Describe("A windowed VECTOR index is validated as Java validates it", func() {
	window := func() *gen.Predicate {
		return &gen.Predicate{RowNumberWindowPredicate: &gen.RowNumberWindowPredicate{
			OrderingField: []string{"price"}, Size: proto.Int32(3), Direction: gen.RowNumberWindowPredicate_ASC.Enum(),
		}}
	}
	for _, c := range []struct {
		name string
		edit func(*gen.Index)
	}{
		{"a unique windowed index", func(ix *gen.Index) {
			ix.Options = append(ix.Options, &gen.Index_Option{Key: proto.String("unique"), Value: proto.String("true")})
		}},
		{"a window under a disjunction alone (not decorated, so not refused)", func(ix *gen.Index) {
			ix.Predicate = &gen.Predicate{OrPredicate: &gen.OrPredicate{Children: []*gen.Predicate{
				window(), {ConstantPredicate: &gen.ConstantPredicate{Value: gen.ConstantPredicate_TRUE.Enum()}},
			}}}
		}},
		// The decoration gate finds a window through AND only, so the placement
		// check sees a window under an OR only beside a decorating one.
		{"a window beside a window under a disjunction", func(ix *gen.Index) {
			ix.Predicate = &gen.Predicate{AndPredicate: &gen.AndPredicate{Children: []*gen.Predicate{
				window(), {OrPredicate: &gen.OrPredicate{Children: []*gen.Predicate{
					{ConstantPredicate: &gen.ConstantPredicate{Value: gen.ConstantPredicate_TRUE.Enum()}}, window(),
				}}},
			}}}
		}},
		{"a window under a conjunction", func(ix *gen.Index) {
			ix.Predicate = &gen.Predicate{AndPredicate: &gen.AndPredicate{Children: []*gen.Predicate{
				window(), {ConstantPredicate: &gen.ConstantPredicate{Value: gen.ConstantPredicate_TRUE.Enum()}},
			}}}
		}},
		{"a windowed index over two record types", func(ix *gen.Index) {
			ix.RecordType = []string{"Order", "Customer"}
		}},
		{"a windowed index without its dimension count", func(ix *gen.Index) {
			var kept []*gen.Index_Option
			for _, o := range ix.Options {
				if o.GetKey() != recordlayer.IndexOptionVectorNumDimensions {
					kept = append(kept, o)
				}
			}
			ix.Options = kept
		}},
	} {
		It(c.name, func() { expectWindowedVerdictAsJava(c.name, windowedEdited(c.edit)) })
	}
})

// windowedEdited is the demo meta-data with a windowed VECTOR index "w" on
// Order that Java builds (the sliding-window conformance shape), its Index
// message edited by edit.
func windowedEdited(edit func(*gen.Index)) *gen.MetaData {
	builder := recordlayer.NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
	builder.GetRecordType("Order").SetPrimaryKey(recordlayer.Field("order_id"))
	builder.GetRecordType("Customer").SetPrimaryKey(recordlayer.Field("customer_id"))
	builder.GetRecordType("TypedRecord").SetPrimaryKey(recordlayer.Field("id"))
	idx := recordlayer.NewVectorIndex("w", recordlayer.KeyWithValue(recordlayer.Field("vector_data"), 0), 3)
	idx.SetOption(recordlayer.IndexOptionVectorMetric, "EUCLIDEAN_SQUARE_METRIC")
	Expect(idx.SetPredicateProto(&gen.Predicate{RowNumberWindowPredicate: &gen.RowNumberWindowPredicate{
		OrderingField: []string{"price"}, Size: proto.Int32(3), Direction: gen.RowNumberWindowPredicate_ASC.Enum(),
	}})).To(Succeed())
	builder.AddIndex("Order", idx)
	md, err := builder.Build()
	Expect(err).NotTo(HaveOccurred())
	p, err := md.ToProto()
	Expect(err).NotTo(HaveOccurred())
	for _, ix := range p.GetIndexes() {
		if ix.GetName() == "w" {
			edit(ix)
		}
	}
	return p
}

// expectWindowedVerdictAsJava loads p in both engines and requires the same
// verdict, and for a refusal Java's class (as its Go error type) and whole text.
func expectWindowedVerdictAsJava(label string, p *gen.MetaData) {
	var java javaAnyVerdict
	Expect(NewJavaInvoker().InvokeAs(context.Background(), "buildMetaDataAnyVerdict", map[string]any{
		"protoBytes": bytesToInts(marshalMetaData(p)),
	}, &java)).To(Succeed())
	_, goErr := recordlayer.RecordMetaDataFromProto(proto.Clone(p).(*gen.MetaData))
	fmt.Fprintf(GinkgoWriter, "WINDOWED %s java=%t %s %q cause=%s %q go=%T %v cause=%v\n", label, java.Valid, java.Class, java.Error,
		java.CauseClass, java.CauseError, goErr, goErr, errors.Unwrap(goErr))
	Expect(goErr == nil).To(Equal(java.Valid), "Java: %t %s %q; Go: %v", java.Valid, java.Class, java.Error, goErr)
	if java.Valid {
		return
	}
	var goMessage string
	switch java.Class {
	case "com.apple.foundationdb.record.metadata.MetaDataException":
		var e *recordlayer.MetaDataError
		Expect(errors.As(goErr, &e)).To(BeTrue(), "Go: %T %v", goErr, goErr)
		goMessage = e.Message
		// The parse failure behind "incorrect index options" is the cause, in
		// both engines: a NumberFormatException's class and text are Java's, and
		// so is an unknown metric's (Enum.valueOf's "No enum constant"); any
		// other IllegalArgumentException's text is Go's own (DIVERGENCES.md,
		// VECTOR).
		switch java.CauseClass {
		case "java.lang.NumberFormatException":
			var nfe *recordlayer.NumberFormatError
			Expect(errors.As(e.Cause, &nfe)).To(BeTrue(), "Go cause: %T %v", e.Cause, e.Cause)
			Expect(nfe.Error()).To(Equal(java.CauseError), "the parse failure's text")
		case "java.lang.IllegalArgumentException":
			var iae *recordlayer.IllegalArgumentError
			Expect(errors.As(e.Cause, &iae)).To(BeTrue(), "Go cause: %T %v", e.Cause, e.Cause)
			if strings.HasPrefix(java.CauseError, "No enum constant ") {
				Expect(iae.Message).To(Equal(java.CauseError), "the metric refusal's text")
			}
		case "":
			Expect(e.Cause).To(BeNil(), "Java's exception has no cause")
		default:
			Fail("unexpected Java cause class " + java.CauseClass)
		}
	case "com.apple.foundationdb.record.RecordCoreException":
		var e *recordlayer.RecordCoreError
		Expect(errors.As(goErr, &e)).To(BeTrue(), "Go: %T %v", goErr, goErr)
		goMessage = e.Message
	default:
		Fail("unexpected Java class " + java.Class)
	}
	Expect(goMessage).To(Equal(java.Error))
}

// The online build's records-range preset for a build of one index, as Java
// writes it: both engines save the same records under the same meta-data
// (Order keyed "order-key" or by its union field, Customer by its union field,
// all primary keys led by the record type key), mark the index on Order
// disabled, build it without marking it readable, and the index's range-set
// key-value pairs are equal. Java presets the range of the index's record types
// before any build, one target or several (OnlineIndexer.java:302-314,
// IndexingMultiTargetByRecords.java:120), ordering record type key tuples with
// Tuple.compareTo (IndexingCommon.computeRecordsRange), a string key included.
var _ = Describe("The online build of one index presets its record types' range as Java does", func() {
	for _, orderKey := range []any{"order-key", nil} {
		It(fmt.Sprintf("writes the range set Java writes, Order keyed %v", orderKey), func() {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
			defer cancel()
			clusterFile, err := sharedContainer.ClusterFile(ctx)
			Expect(err).NotTo(HaveOccurred())

			builder := recordlayer.NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
			builder.GetRecordType("Order").SetPrimaryKey(recordlayer.Concat(recordlayer.RecordTypeKey(), recordlayer.Field("order_id")))
			if orderKey != nil {
				builder.GetRecordType("Order").SetRecordTypeKey(orderKey)
			}
			builder.GetRecordType("Customer").SetPrimaryKey(recordlayer.Concat(recordlayer.RecordTypeKey(), recordlayer.Field("customer_id")))
			builder.GetRecordType("TypedRecord").SetPrimaryKey(recordlayer.Concat(recordlayer.RecordTypeKey(), recordlayer.Field("id")))
			builder.AddIndex("Order", recordlayer.NewIndex("Order$price", recordlayer.Field("price")))
			md, err := builder.Build()
			Expect(err).NotTo(HaveOccurred())
			p, err := md.ToProto()
			Expect(err).NotTo(HaveOccurred())
			mdBytes, err := proto.Marshal(p)
			Expect(err).NotTo(HaveOccurred())

			javaSS := subspace.Sub(tuple.Tuple{"preset_java", uuid.NewString()}...)
			var java struct {
				KVs [][]string `json:"kvs"`
			}
			Expect(NewJavaInvoker().InvokeAs(ctx, "buildIndexDumpRangeSetJava", map[string]any{
				"clusterFile": clusterFile, "subspace": BytesToIntArray(javaSS.Bytes()),
				"metaData": BytesToIntArray(mdBytes), "indexName": "Order$price",
			}, &java)).To(Succeed())
			GinkgoWriter.Printf("PRESET java kvs=%v\n", java.KVs)
			Expect(java.KVs).NotTo(BeEmpty())

			db := recordlayer.NewFDBDatabase(sharedDB)
			goSS := subspace.Sub(tuple.Tuple{"preset_go", uuid.NewString()}...)
			_, err = db.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
				store, err := recordlayer.NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(md).SetSubspace(goSS).CreateOrOpen()
				if err != nil {
					return nil, err
				}
				for i := int64(1); i <= 3; i++ {
					if _, err := store.SaveRecord(&gen.Order{OrderId: proto.Int64(i), Price: proto.Int32(int32(10 * i))}); err != nil {
						return nil, err
					}
				}
				for i := int64(101); i <= 102; i++ {
					if _, err := store.SaveRecord(&gen.Customer{CustomerId: proto.Int64(i)}); err != nil {
						return nil, err
					}
				}
				_, err = store.MarkIndexDisabled("Order$price")
				return nil, err
			})
			Expect(err).NotTo(HaveOccurred())
			indexer, err := recordlayer.NewOnlineIndexerBuilder().
				SetDatabase(db).SetMetaData(md).SetIndex(md.GetIndex("Order$price")).SetSubspace(goSS).SetMarkReadable(false).Build()
			Expect(err).NotTo(HaveOccurred())
			_, err = indexer.BuildIndex(ctx)
			Expect(err).NotTo(HaveOccurred())
			goKVs, err := db.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
				begin, end := goSS.Sub(int64(recordlayer.IndexRangeSpaceKey), "Order$price").FDBRangeKeys()
				kvs, err := rtx.Transaction().GetRange(fdb.KeyRange{Begin: begin, End: end}, fdb.RangeOptions{}).GetSliceWithError()
				if err != nil {
					return nil, err
				}
				var out [][]string
				for _, kv := range kvs {
					out = append(out, []string{hex.EncodeToString(kv.Key[len(goSS.Bytes()):]), hex.EncodeToString(kv.Value)})
				}
				return out, nil
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(goKVs).To(Equal(java.KVs))
		})
	}
})
