//go:build bazelrunfiles

package conformance_test

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/descriptorpb"
	"google.golang.org/protobuf/types/dynamicpb"

	"fdb.dev/gen"
	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/fdbgo/fdb/subspace"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	"fdb.dev/pkg/recordlayer"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/relational/core/rowstruct"
)

// Java's Key.Evaluated.NullStandin (Key.java:394-421), end to end: a field's
// nullInterpretation decides what an absent field evaluates to and whether a
// unique index or COUNT_NOT_NULL ignores the null (IndexEntry.
// keyContainsNonUniqueNull). Each case builds one meta-data proto, and both
// engines save the same records into their own store, one transaction per
// record: the save verdicts and every index key-value pair written must be
// equal, and the Java verdicts are pinned.

const nsViolation = "com.apple.foundationdb.record.RecordIndexUniquenessViolation"

// nsRecords is the records file: Rec{id, a, b, c, sub}, Sub{x, tags}. syntax
// is "proto2" or "proto3" (implicit presence: a field at its default is
// absent to protobuf-java's hasField).
func nsRecords(syntax string) *descriptorpb.FileDescriptorProto {
	label := descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum()
	field := func(name string, number int32, typ descriptorpb.FieldDescriptorProto_Type) *descriptorpb.FieldDescriptorProto {
		return &descriptorpb.FieldDescriptorProto{Name: proto.String(name), Number: proto.Int32(number), Label: label, Type: typ.Enum()}
	}
	message := func(name string, number int32, typeName string) *descriptorpb.FieldDescriptorProto {
		f := field(name, number, descriptorpb.FieldDescriptorProto_TYPE_MESSAGE)
		f.TypeName = proto.String(typeName)
		return f
	}
	tags := field("tags", 2, descriptorpb.FieldDescriptorProto_TYPE_STRING)
	tags.Label = descriptorpb.FieldDescriptorProto_LABEL_REPEATED.Enum()
	return &descriptorpb.FileDescriptorProto{
		Name:    proto.String("null_standin_" + syntax + ".proto"),
		Package: proto.String("nullstandin"),
		Syntax:  proto.String(syntax),
		MessageType: []*descriptorpb.DescriptorProto{
			{Name: proto.String("Sub"), Field: []*descriptorpb.FieldDescriptorProto{
				field("x", 1, descriptorpb.FieldDescriptorProto_TYPE_INT64), tags,
			}},
			{Name: proto.String("Rec"), Field: []*descriptorpb.FieldDescriptorProto{
				field("id", 1, descriptorpb.FieldDescriptorProto_TYPE_INT64),
				field("a", 2, descriptorpb.FieldDescriptorProto_TYPE_INT64),
				field("b", 3, descriptorpb.FieldDescriptorProto_TYPE_INT64),
				field("c", 4, descriptorpb.FieldDescriptorProto_TYPE_STRING),
				message("sub", 5, ".nullstandin.Sub"),
			}},
			{Name: proto.String("RecordTypeUnion"), Field: []*descriptorpb.FieldDescriptorProto{
				message("_Rec", 1, ".nullstandin.Rec"),
			}},
		},
	}
}

// dumpIndexKVs is every key-value pair of a store's INDEX and
// INDEX_SECONDARY_SPACE keyspaces (2 and 3), as hex relative to the store
// subspace, in key order: the Go side of saveRecordsAndDumpIndexesJava.
func dumpIndexKVs(ctx context.Context, db *recordlayer.FDBDatabase, ss subspace.Subspace) ([][]string, error) {
	out, err := db.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
		var out [][]string
		for _, space := range []int64{2, 3} {
			begin, end := ss.Sub(space).FDBRangeKeys()
			kvs, err := rtx.Transaction().GetRange(fdb.KeyRange{Begin: begin, End: end}, fdb.RangeOptions{}).GetSliceWithError()
			if err != nil {
				return nil, err
			}
			for _, kv := range kvs {
				out = append(out, []string{hex.EncodeToString(kv.Key[len(ss.Bytes()):]), hex.EncodeToString(kv.Value)})
			}
		}
		return out, nil
	})
	if err != nil {
		return nil, err
	}
	kvs, _ := out.([][]string)
	return kvs, nil
}

func nsField(name string, fan gen.Field_FanType, ni gen.Field_NullInterpretation) *gen.Field {
	return &gen.Field{FieldName: proto.String(name), FanType: fan.Enum(), NullInterpretation: ni.Enum()}
}

func nsScalar(name string, ni gen.Field_NullInterpretation) *gen.KeyExpression {
	return &gen.KeyExpression{Field: nsField(name, gen.Field_SCALAR, ni)}
}

func nsNest(parent gen.Field_NullInterpretation, child *gen.KeyExpression) *gen.KeyExpression {
	return &gen.KeyExpression{Nesting: &gen.Nesting{Parent: nsField("sub", gen.Field_SCALAR, parent), Child: child}}
}

func nsThen(children ...*gen.KeyExpression) *gen.KeyExpression {
	return &gen.KeyExpression{Then: &gen.Then{Child: children}}
}

const (
	nsNull       = gen.Field_NOT_UNIQUE
	nsNullUnique = gen.Field_UNIQUE
	nsNotNull    = gen.Field_NOT_NULL
)

type nsRec struct {
	id   int64
	a, b *int64
	c    *string
	sub  bool // set an empty Sub
}

type nsCase struct {
	name      string
	syntax    string
	indexType string
	unique    bool
	root      *gen.KeyExpression
	records   []nsRec
	// javaVerdicts is the measured Java outcome of each save.
	javaVerdicts []string
	// filter is the stores' IndexMaintenanceFilter: "" for NORMAL, or
	// "NO_NULLS".
	filter string
}

func nsMetaData(c nsCase) *gen.MetaData {
	idx := &gen.Index{
		Name:           proto.String("ns_idx"),
		RecordType:     []string{"Rec"},
		RootExpression: c.root,
		Type:           proto.String(c.indexType),
		SubspaceKey:    tuple.Tuple{"ns_idx"}.Pack(),
	}
	if c.unique {
		idx.Options = []*gen.Index_Option{{Key: proto.String("unique"), Value: proto.String("true")}}
	}
	return &gen.MetaData{
		Records:     nsRecords(c.syntax),
		RecordTypes: []*gen.RecordType{{Name: proto.String("Rec"), PrimaryKey: nsScalar("id", nsNull)}},
		Indexes:     []*gen.Index{idx},
		Version:     proto.Int32(1),
	}
}

func nsRecordBytes(desc protoreflect.MessageDescriptor, r nsRec) []byte {
	m := dynamicpb.NewMessage(desc)
	set := func(name string, v protoreflect.Value) { m.Set(desc.Fields().ByName(protoreflect.Name(name)), v) }
	set("id", protoreflect.ValueOfInt64(r.id))
	if r.a != nil {
		set("a", protoreflect.ValueOfInt64(*r.a))
	}
	if r.b != nil {
		set("b", protoreflect.ValueOfInt64(*r.b))
	}
	if r.c != nil {
		set("c", protoreflect.ValueOfString(*r.c))
	}
	if r.sub {
		fd := desc.Fields().ByName("sub")
		set("sub", protoreflect.ValueOfMessage(dynamicpb.NewMessage(fd.Message())))
	}
	b, err := proto.Marshal(m)
	Expect(err).NotTo(HaveOccurred())
	return b
}

var _ = Describe("RFC-257 NullStandin: absent fields, unique indexes and COUNT_NOT_NULL", func() {
	i64 := func(v int64) *int64 { return &v }
	str := func(v string) *string { return &v }

	cases := []nsCase{
		{
			name: "NULL: two absent fields do not collide", syntax: "proto2", indexType: "value", unique: true,
			root:         nsScalar("a", nsNull),
			records:      []nsRec{{id: 1}, {id: 2}},
			javaVerdicts: []string{"ok", "ok"},
		},
		{
			name: "NULL_UNIQUE: two absent fields collide", syntax: "proto2", indexType: "value", unique: true,
			root:         nsScalar("a", nsNullUnique),
			records:      []nsRec{{id: 1}, {id: 2}},
			javaVerdicts: []string{"ok", nsViolation},
		},
		{
			name: "NOT_NULL: an absent field is its default, and collides with a set default", syntax: "proto2", indexType: "value", unique: true,
			root:         nsScalar("a", nsNotNull),
			records:      []nsRec{{id: 1}, {id: 2, a: i64(0)}},
			javaVerdicts: []string{"ok", nsViolation},
		},
		{
			name: "NOT_NULL parent: the child reads the parent's default message", syntax: "proto2", indexType: "value", unique: true,
			root:         nsNest(nsNotNull, nsScalar("x", nsNotNull)),
			records:      []nsRec{{id: 1}, {id: 2, sub: true}},
			javaVerdicts: []string{"ok", nsViolation},
		},
		{
			name: "NULL parent: a NOT_NULL child over a null message is a null that collides", syntax: "proto2", indexType: "value", unique: true,
			root:         nsNest(nsNull, nsScalar("x", nsNotNull)),
			records:      []nsRec{{id: 1}, {id: 2}},
			javaVerdicts: []string{"ok", nsViolation},
		},
		{
			name: "NOT_NULL parent: a NULL child of the default message does not collide", syntax: "proto2", indexType: "value", unique: true,
			root:         nsNest(nsNotNull, nsScalar("x", nsNull)),
			records:      []nsRec{{id: 1}, {id: 2}},
			javaVerdicts: []string{"ok", "ok"},
		},
		{
			name: "Concatenate under an absent parent: NOT_NULL is the empty list, NULL is null", syntax: "proto2", indexType: "value",
			root: nsThen(
				nsNest(nsNull, &gen.KeyExpression{Field: nsField("tags", gen.Field_CONCATENATE, nsNotNull)}),
				nsNest(nsNull, &gen.KeyExpression{Field: nsField("tags", gen.Field_CONCATENATE, nsNull)}),
			),
			records:      []nsRec{{id: 1}, {id: 2, sub: true}},
			javaVerdicts: []string{"ok", "ok"},
		},
		{
			name: "Then: only a NULL column exempts the key", syntax: "proto2", indexType: "value", unique: true,
			root:         nsThen(nsScalar("a", nsNull), nsScalar("b", nsNullUnique)),
			records:      []nsRec{{id: 1, b: i64(5)}, {id: 2, b: i64(5)}, {id: 3, a: i64(1)}, {id: 4, a: i64(1)}},
			javaVerdicts: []string{"ok", "ok", "ok", nsViolation},
		},
		{
			name: "a function's plain null collides", syntax: "proto2", indexType: "value", unique: true,
			root: &gen.KeyExpression{Function: &gen.Function{
				Name: proto.String("add"), Arguments: nsThen(nsScalar("a", nsNull), nsScalar("b", nsNull)),
			}},
			records:      []nsRec{{id: 1, b: i64(1)}, {id: 2, b: i64(1)}},
			javaVerdicts: []string{"ok", nsViolation},
		},
		{
			name: "a unique RANK index ignores only a NULL", syntax: "proto2", indexType: "rank", unique: true,
			root:         nsThen(nsScalar("a", nsNull), nsScalar("b", nsNullUnique)),
			records:      []nsRec{{id: 1, b: i64(5)}, {id: 2, b: i64(5)}, {id: 3, a: i64(1)}, {id: 4, a: i64(1)}},
			javaVerdicts: []string{"ok", "ok", "ok", nsViolation},
		},
		{
			name: "COUNT_NOT_NULL counts a NULL_UNIQUE null", syntax: "proto2", indexType: "count_not_null",
			root: &gen.KeyExpression{Grouping: &gen.Grouping{
				WholeKey: nsThen(nsScalar("c", nsNull), nsScalar("b", nsNullUnique)), GroupedCount: proto.Int32(1),
			}},
			records:      []nsRec{{id: 1, c: str("x")}, {id: 2, c: str("x"), b: i64(1)}},
			javaVerdicts: []string{"ok", "ok"},
		},
		{
			name: "COUNT_NOT_NULL skips a NULL null", syntax: "proto2", indexType: "count_not_null",
			root: &gen.KeyExpression{Grouping: &gen.Grouping{
				WholeKey: nsThen(nsScalar("c", nsNull), nsScalar("b", nsNull)), GroupedCount: proto.Int32(1),
			}},
			records:      []nsRec{{id: 1, c: str("x")}, {id: 2, c: str("x"), b: i64(1)}},
			javaVerdicts: []string{"ok", "ok"},
		},
		{
			name: "proto3: a field at its default is absent", syntax: "proto3", indexType: "value", unique: true,
			root:         nsScalar("a", nsNull),
			records:      []nsRec{{id: 1, a: i64(0)}, {id: 2, a: i64(0)}, {id: 3, a: i64(7)}},
			javaVerdicts: []string{"ok", "ok", "ok"},
		},
		{
			name: "proto3: NULL_UNIQUE collides on a field at its default", syntax: "proto3", indexType: "value", unique: true,
			root:         nsScalar("a", nsNullUnique),
			records:      []nsRec{{id: 1, a: i64(0)}, {id: 2}},
			javaVerdicts: []string{"ok", nsViolation},
		},
		// IndexMaintenanceFilter.NO_NULLS: an entry whose key holds a
		// NullStandin.NULL is not maintained, in every maintainer.
		{
			name: "NO_NULLS: a VALUE entry of a NULL field is not maintained, through inserts, updates and deletes", syntax: "proto2", indexType: "value", filter: "NO_NULLS",
			root:         nsScalar("a", nsNull),
			records:      []nsRec{{id: 1}, {id: 1, a: i64(5)}, {id: 2, a: i64(6)}, {id: 2}, {id: 3}},
			javaVerdicts: []string{"ok", "ok", "ok", "ok", "ok"},
		},
		{
			name: "NO_NULLS: a NULL_UNIQUE null is maintained", syntax: "proto2", indexType: "value", filter: "NO_NULLS",
			root:         nsThen(nsScalar("a", nsNullUnique), nsScalar("b", nsNull)),
			records:      []nsRec{{id: 1, b: i64(1)}, {id: 2, a: i64(1)}, {id: 3}},
			javaVerdicts: []string{"ok", "ok", "ok"},
		},
		{
			name: "NO_NULLS: a COUNT's whole key is filtered, a null grouping column included", syntax: "proto2", indexType: "count", filter: "NO_NULLS",
			root: &gen.KeyExpression{Grouping: &gen.Grouping{
				WholeKey: nsScalar("c", nsNull), GroupedCount: proto.Int32(0),
			}},
			records:      []nsRec{{id: 1}, {id: 2, c: str("x")}, {id: 3, c: str("x")}, {id: 2}},
			javaVerdicts: []string{"ok", "ok", "ok", "ok"},
		},
		{
			name: "NORMAL: a COUNT counts a null grouping column", syntax: "proto2", indexType: "count",
			root: &gen.KeyExpression{Grouping: &gen.Grouping{
				WholeKey: nsScalar("c", nsNull), GroupedCount: proto.Int32(0),
			}},
			records:      []nsRec{{id: 1}, {id: 2, c: str("x")}},
			javaVerdicts: []string{"ok", "ok"},
		},
		{
			name: "NO_NULLS: a SUM entry with a null grouping column is not maintained", syntax: "proto2", indexType: "sum", filter: "NO_NULLS",
			root: &gen.KeyExpression{Grouping: &gen.Grouping{
				WholeKey: nsThen(nsScalar("c", nsNull), nsScalar("b", nsNull)), GroupedCount: proto.Int32(1),
			}},
			records:      []nsRec{{id: 1, b: i64(4)}, {id: 2, c: str("x"), b: i64(5)}, {id: 3, c: str("x"), b: i64(6)}},
			javaVerdicts: []string{"ok", "ok", "ok"},
		},
		{
			name: "NO_NULLS: a TEXT entry of a null field is not maintained, its tokenizer version is", syntax: "proto2", indexType: "text", filter: "NO_NULLS",
			root:         nsScalar("c", nsNull),
			records:      []nsRec{{id: 1}, {id: 2, c: str("hello world")}, {id: 2}},
			javaVerdicts: []string{"ok", "ok", "ok"},
		},
		{
			name: "NO_NULLS: a RANK entry with a null grouping column is not maintained", syntax: "proto2", indexType: "rank", filter: "NO_NULLS",
			root: &gen.KeyExpression{Grouping: &gen.Grouping{
				WholeKey: nsThen(nsScalar("c", nsNull), nsScalar("b", nsNull)), GroupedCount: proto.Int32(1),
			}},
			records:      []nsRec{{id: 1, b: i64(4)}, {id: 2, c: str("x"), b: i64(5)}, {id: 3, c: str("x")}},
			javaVerdicts: []string{"ok", "ok", "ok"},
		},
	}

	// Java's FieldKeyExpression.equals does not compare the standin
	// (FieldKeyExpression.java:406-410), so an index whose field changes only
	// its null interpretation is the same index to the evolution validator.
	It("an index's null interpretation is not part of its definition", func() {
		base := nsCase{syntax: "proto2", indexType: "value", root: nsThen(nsScalar("a", nsNull), nsNest(nsNull, nsScalar("x", nsNull)))}
		changed := base
		changed.root = nsThen(nsScalar("a", nsNotNull), nsNest(nsNotNull, nsScalar("x", nsNullUnique)))
		oldProto, newProto := nsMetaData(base), nsMetaData(changed)
		newProto.Version = proto.Int32(2)
		var java javaAnyVerdict
		Expect(NewJavaInvoker().InvokeAs(context.Background(), "validateMetaDataEvolutionAnyVerdict", map[string]any{
			"oldProtoBytes": bytesToInts(marshalMetaData(oldProto)),
			"newProtoBytes": bytesToInts(marshalMetaData(newProto)),
		}, &java)).To(Succeed())
		Expect(java.Valid).To(BeTrue(), "Java: %s %s", java.Class, java.Error)
		oldMD, err := recordlayer.RecordMetaDataFromProto(oldProto)
		Expect(err).NotTo(HaveOccurred())
		newMD, err := recordlayer.RecordMetaDataFromProto(newProto)
		Expect(err).NotTo(HaveOccurred())
		Expect(recordlayer.NewMetaDataEvolutionValidator().Build().Validate(oldMD, newMD)).To(Succeed())
		// The standin is still carried: the new meta-data serializes it.
		reserialized, err := newMD.ToProto()
		Expect(err).NotTo(HaveOccurred())
		Expect(reserialized.GetIndexes()[0].GetRootExpression().GetThen().GetChild()[0].GetField().GetNullInterpretation()).To(Equal(nsNotNull))
		Expect(reserialized.GetIndexes()[0].GetRootExpression().GetThen().GetChild()[1].GetNesting().GetParent().GetNullInterpretation()).To(Equal(nsNotNull))
		Expect(reserialized.GetIndexes()[0].GetRootExpression().GetThen().GetChild()[1].GetNesting().GetChild().GetField().GetNullInterpretation()).To(Equal(nsNullUnique))
	})

	for _, c := range cases {
		It(c.name, func() {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
			defer cancel()
			clusterFile, err := sharedContainer.ClusterFile(ctx)
			Expect(err).NotTo(HaveOccurred())
			mdProto := nsMetaData(c)
			mdBytes, err := proto.Marshal(mdProto)
			Expect(err).NotTo(HaveOccurred())
			md, err := recordlayer.RecordMetaDataFromProto(proto.Clone(mdProto).(*gen.MetaData))
			Expect(err).NotTo(HaveOccurred())
			desc := md.GetRecordType("Rec").Descriptor
			records := make([][]byte, len(c.records))
			for i, r := range c.records {
				records[i] = nsRecordBytes(desc, r)
			}

			javaSS := subspace.Sub(tuple.Tuple{"ns_java", uuid.NewString()}...)
			var java struct {
				Verdicts []string   `json:"verdicts"`
				KVs      [][]string `json:"kvs"`
			}
			recordArgs := make([][]int, len(records))
			for i, r := range records {
				recordArgs[i] = BytesToIntArray(r)
			}
			Expect(NewJavaInvoker().InvokeAs(ctx, "saveRecordsAndDumpIndexesJava", map[string]any{
				"clusterFile": clusterFile, "subspace": BytesToIntArray(javaSS.Bytes()),
				"metaData": BytesToIntArray(mdBytes), "recordTypeName": "Rec", "records": recordArgs, "filter": c.filter,
			}, &java)).To(Succeed())
			Expect(java.Verdicts).To(Equal(c.javaVerdicts), "Java's verdicts moved")

			goSS := subspace.Sub(tuple.Tuple{"ns_go", uuid.NewString()}...)
			db := recordlayer.NewFDBDatabase(sharedDB)
			var goVerdicts []string
			for _, rb := range records {
				msg := dynamicpb.NewMessage(desc)
				Expect(proto.Unmarshal(rb, msg)).To(Succeed())
				_, err := db.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
					builder := recordlayer.NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(md).SetSubspace(goSS)
					if c.filter == "NO_NULLS" {
						builder.SetIndexMaintenanceFilter(recordlayer.IndexMaintenanceFilterNoNulls)
					}
					store, err := builder.CreateOrOpen()
					if err != nil {
						return nil, err
					}
					_, err = store.SaveRecord(msg)
					return nil, err
				})
				var violation *recordlayer.RecordIndexUniquenessViolationError
				switch {
				case err == nil:
					goVerdicts = append(goVerdicts, "ok")
				case errors.As(err, &violation):
					goVerdicts = append(goVerdicts, nsViolation)
				default:
					goVerdicts = append(goVerdicts, fmt.Sprintf("go error: %v", err))
				}
			}
			Expect(goVerdicts).To(Equal(java.Verdicts))

			goKVs, err := dumpIndexKVs(ctx, db, goSS)
			Expect(err).NotTo(HaveOccurred())
			Expect(java.KVs).NotTo(BeEmpty())
			Expect(goKVs).To(Equal(java.KVs))
			GinkgoWriter.Printf("NULLSTANDIN %q verdicts=%v kvs=%v\n", c.name, java.Verdicts, java.KVs)
		})
	}
})

// A query reads a record's field by Java's MessageHelpers.getFieldOnMessage
// rule (MessageHelpers.java:124-142), the rule values.ProtoFieldReadsValue
// ports: an implicit-presence (proto3) field at its default is null, as the
// index's key evaluation reads it, and an unset proto2 field with an explicit
// default reads the default.
var _ = Describe("RFC-257 a query reads a field as Java's getFieldOnMessage", func() {
	file := func(syntax string) *descriptorpb.FileDescriptorProto {
		label := descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum()
		field := func(name string, number int32) *descriptorpb.FieldDescriptorProto {
			return &descriptorpb.FieldDescriptorProto{Name: proto.String(name), Number: proto.Int32(number), Label: label, Type: descriptorpb.FieldDescriptorProto_TYPE_INT64.Enum()}
		}
		repeated := field("r", 3)
		repeated.Label = descriptorpb.FieldDescriptorProto_LABEL_REPEATED.Enum()
		sub := field("s", 4)
		sub.Type = descriptorpb.FieldDescriptorProto_TYPE_MESSAGE.Enum()
		sub.TypeName = proto.String(".reads.Sub")
		fields := []*descriptorpb.FieldDescriptorProto{field("a", 1), repeated, sub}
		if syntax == "proto2" {
			withDefault := field("d", 2)
			withDefault.DefaultValue = proto.String("7")
			fields = append(fields, withDefault)
		}
		return &descriptorpb.FileDescriptorProto{
			Name: proto.String("reads_" + syntax + ".proto"), Package: proto.String("reads"), Syntax: proto.String(syntax),
			MessageType: []*descriptorpb.DescriptorProto{
				{Name: proto.String("Sub"), Field: []*descriptorpb.FieldDescriptorProto{field("x", 1)}},
				{Name: proto.String("Rec"), Field: fields},
			},
		}
	}
	for _, c := range []struct {
		syntax string
		set    map[string]int64
		field  string
		// javaNull and javaValue are the measured Java read.
		javaNull  bool
		javaValue string
	}{
		{"proto2", nil, "a", true, "null"},
		{"proto2", map[string]int64{"a": 0}, "a", false, "0"},
		{"proto2", nil, "d", false, "7"},
		{"proto2", map[string]int64{"d": 0}, "d", false, "0"},
		{"proto2", nil, "r", false, "[]"},
		{"proto2", nil, "s", true, "null"},
		{"proto3", nil, "a", true, "null"},
		{"proto3", map[string]int64{"a": 0}, "a", true, "null"},
		{"proto3", map[string]int64{"a": 5}, "a", false, "5"},
		{"proto3", nil, "r", false, "[]"},
		{"proto3", nil, "s", true, "null"},
	} {
		It(fmt.Sprintf("%s %v reads %s", c.syntax, c.set, c.field), func() {
			fdp := file(c.syntax)
			fd, err := protodesc.NewFile(fdp, nil)
			Expect(err).NotTo(HaveOccurred())
			desc := fd.Messages().ByName("Rec")
			msg := dynamicpb.NewMessage(desc)
			for name, v := range c.set {
				msg.Set(desc.Fields().ByName(protoreflect.Name(name)), protoreflect.ValueOfInt64(v))
			}
			msgBytes, err := proto.Marshal(msg)
			Expect(err).NotTo(HaveOccurred())
			fdpBytes, err := proto.Marshal(fdp)
			Expect(err).NotTo(HaveOccurred())
			var java struct {
				IsNull      bool   `json:"isNull"`
				Value       string `json:"value"`
				TupleIsNull bool   `json:"tupleIsNull"`
				TupleValue  string `json:"tupleValue"`
			}
			Expect(NewJavaInvoker().InvokeAs(context.Background(), "getFieldOnMessageJava", map[string]any{
				"fileDescriptorProto": BytesToIntArray(fdpBytes), "messageName": "Rec",
				"message": BytesToIntArray(msgBytes), "fieldName": c.field,
			}, &java)).To(Succeed())
			Expect([]any{java.IsNull, java.Value}).To(Equal([]any{c.javaNull, c.javaValue}), "Java's read moved")

			parsed := dynamicpb.NewMessage(desc)
			Expect(proto.Unmarshal(msgBytes, parsed)).To(Succeed())
			fdField := desc.Fields().ByName(protoreflect.Name(c.field))
			goValue := "null"
			if values.ProtoFieldReadsValue(parsed, fdField) {
				v := parsed.Get(fdField)
				if fdField.IsList() {
					goValue = fmt.Sprint(make([]any, v.List().Len()))
				} else {
					goValue = fmt.Sprint(v.Int())
				}
			}
			Expect(goValue).To(Equal(java.Value))
			// The driver's struct read is MessageTuple's, which differs from
			// getFieldOnMessage only for an unset field that declares a default
			// (null there); Go's rowstruct is its port.
			rs, err := rowstruct.New(parsed)
			Expect(err).NotTo(HaveOccurred())
			attr, err := rs.AttributeByName(c.field)
			Expect(err).NotTo(HaveOccurred())
			goTuple := "null"
			switch v := attr.(type) {
			case nil:
			case []any:
				goTuple = fmt.Sprint(v)
			default:
				goTuple = fmt.Sprint(v)
			}
			Expect(goTuple).To(Equal(java.TupleValue), "MessageTuple's read")
			wantTupleNull := c.javaNull || c.syntax == "proto2" && c.field == "d" && c.set == nil
			Expect(java.TupleIsNull).To(Equal(wantTupleNull), "Java's MessageTuple read moved")
			GinkgoWriter.Printf("GETFIELDONMESSAGE %s %v %s java=%t/%s tuple=%t/%s go=%s rowstruct=%s\n", c.syntax, c.set, c.field,
				java.IsNull, java.Value, java.TupleIsNull, java.TupleValue, goValue, goTuple)
		})
	}
})
