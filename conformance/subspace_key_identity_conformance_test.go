//go:build bazelrunfiles

package conformance_test

import (
	"context"
	"errors"
	"fmt"
	"math"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/descriptorpb"

	"fdb.dev/gen"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	"fdb.dev/pkg/recordlayer"
)

// javaVerdict is what the metadata verdict steps return: valid, or the
// MetaDataException's message.
type javaVerdict struct {
	Valid bool   `json:"valid"`
	Error string `json:"error"`
}

// subspaceKeyTestProto builds meta-data over the demo records with one index
// per entry of keys (named i0, i1, ... on Order.price; a nil key keeps the
// default, the index name), then appends the former indexes, and returns its
// proto. The keys are written into the proto packed, as Java writes them, so a
// key Go's builder would refuse can still be put in front of both engines.
func subspaceKeyTestProto(version int, keys []any, formers []*gen.FormerIndex, configure func(i int, idx *recordlayer.Index)) *gen.MetaData {
	b := recordlayer.NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
	b.GetRecordType("Order").SetPrimaryKey(recordlayer.Field("order_id"))
	b.GetRecordType("Customer").SetPrimaryKey(recordlayer.Field("customer_id"))
	b.GetRecordType("TypedRecord").SetPrimaryKey(recordlayer.Field("id"))
	for i := range keys {
		idx := recordlayer.NewIndex(fmt.Sprintf("i%d", i), recordlayer.Field("price"))
		idx.AddedVersion, idx.LastModifiedVersion = 1, 1
		if configure != nil {
			configure(i, idx)
		}
		b.AddIndex("Order", idx)
	}
	b.SetVersion(version)
	md, err := b.Build()
	ExpectWithOffset(1, err).NotTo(HaveOccurred())
	p, err := md.ToProto()
	ExpectWithOffset(1, err).NotTo(HaveOccurred())
	for i, key := range keys {
		if key == nil {
			continue
		}
		for _, idx := range p.Indexes {
			if idx.GetName() == fmt.Sprintf("i%d", i) {
				idx.SubspaceKey = tuple.Tuple{key}.Pack()
			}
		}
	}
	p.FormerIndexes = append(p.FormerIndexes, formers...)
	return p
}

// withIndexI0 edits the proto of index i0, the edits a Go builder cannot make
// (a type or a root chosen after the build, a record-type list).
func withIndexI0(p *gen.MetaData, edit func(*gen.Index)) *gen.MetaData {
	for _, idx := range p.GetIndexes() {
		if idx.GetName() == "i0" {
			edit(idx)
		}
	}
	return p
}

func formerIndexProto(key any, added, removed int32, name string) *gen.FormerIndex {
	f := &gen.FormerIndex{SubspaceKey: tuple.Tuple{key}.Pack(), AddedVersion: proto.Int32(added), RemovedVersion: proto.Int32(removed)}
	if name != "" {
		f.FormerName = proto.String(name)
	}
	return f
}

func marshalMetaData(p *gen.MetaData) []byte {
	data, err := proto.Marshal(p)
	ExpectWithOffset(1, err).NotTo(HaveOccurred())
	return data
}

// Java normalizes every index and former-index subspace key when it is
// assigned (TupleTypeUtil.toTupleEquivalentValue), and every comparison of
// keys is Object.equals on the normalized objects: MetaDataValidator's maps and
// MetaDataEvolutionValidator's, which pair indexes across two meta-data by key.
// Each shape runs through Java's validator and Go's; the verdicts must agree,
// and a refusal's Go message must start with Java's.
var _ = Describe("Subspace-key identity in meta-data validation", func() {
	nanA := math.Float64frombits(0x7ff8000000000001)
	nanB := math.Float64frombits(0x7ff8000000000002)
	for _, c := range []struct {
		name       string
		keys       []any
		formers    []*gen.FormerIndex
		wantPrefix string // empty: valid
	}{
		{"a bytes key beside the string key of the same content", []any{"pb", []byte("pb")}, nil, ""},
		{"two NaN keys with different payloads", []any{nanA, nanB}, nil, "Same subspace key NaN used by both "},
		{"0.0 and -0.0", []any{0.0, math.Copysign(0, -1)}, nil, ""},
		{"two former indexes with one key", []any{"k"}, []*gen.FormerIndex{formerIndexProto(int64(9), 1, 2, "gone"), formerIndexProto(int64(9), 1, 2, "")}, "Same subspace key 9 used by two former indexes "},
		{"an index on a former index's key", []any{int64(9)}, []*gen.FormerIndex{formerIndexProto(int64(9), 1, 2, "gone")}, "Same subspace key 9 used by index i0 and former index gone"},
		{"a former index on the bytes of an index's string key", []any{"k"}, []*gen.FormerIndex{formerIndexProto([]byte("k"), 1, 2, "gone")}, ""},
	} {
		It("builds or refuses as Java does: "+c.name, func() {
			p := subspaceKeyTestProto(5, c.keys, c.formers, nil)
			var java javaVerdict
			Expect(NewJavaInvoker().InvokeAs(context.Background(), "buildMetaDataVerdict",
				map[string]any{"protoBytes": bytesToInts(marshalMetaData(p))}, &java)).To(Succeed())
			_, err := recordlayer.RecordMetaDataFromProto(p)
			fmt.Fprintf(GinkgoWriter, "SUBSPACE_KEY_BUILD %q java=%t %q go=%v\n", c.name, java.Valid, java.Error, err)

			if c.wantPrefix == "" {
				Expect(java.Valid).To(BeTrue(), "Java: %s", java.Error)
				Expect(err).NotTo(HaveOccurred())
				return
			}
			Expect(java.Valid).To(BeFalse())
			Expect(java.Error).To(HavePrefix(c.wantPrefix))
			var mdErr *recordlayer.MetaDataError
			Expect(errors.As(err, &mdErr)).To(BeTrue(), "Go error: %v", err)
			// Java's whole message, not only the shape's fixed prefix, so an
			// index named in another order than Java names it fails here.
			Expect(mdErr.Message).To(HavePrefix(java.Error))
		})
	}
})

var _ = Describe("Subspace-key pairing in meta-data evolution", func() {
	// One index, i0, whose key is its name.
	old := func() *gen.MetaData { return subspaceKeyTestProto(3, []any{nil}, nil, nil) }
	for _, c := range []struct {
		name         string
		old          func() *gen.MetaData
		new          func() *gen.MetaData
		allowMissing bool
		allowOlder   bool
		wantPrefix   string // empty: valid
	}{
		{
			name: "same name, another key",
			old:  old,
			new:  func() *gen.MetaData { return subspaceKeyTestProto(4, []any{"i0-moved"}, nil, nil) },
			// The old key's entries would be left behind under a readable index.
			wantPrefix: "index missing in new meta-data",
		},
		{
			name: "same key, another name",
			old:  old,
			new: func() *gen.MetaData {
				p := subspaceKeyTestProto(4, []any{"i0"}, nil, nil)
				p.Indexes[0].Name = proto.String("renamed")
				return p
			},
			wantPrefix: "index name changed",
		},
		{
			name: "the index moved to a new key, and its old key became a former index",
			old:  old,
			new: func() *gen.MetaData {
				return subspaceKeyTestProto(6, []any{"i0-moved"}, []*gen.FormerIndex{formerIndexProto("i0", 1, 5, "i0")}, func(_ int, idx *recordlayer.Index) {
					idx.AddedVersion, idx.LastModifiedVersion = 5, 5
				})
			},
		},
		{
			name: "a former index's key reused by an index",
			old: func() *gen.MetaData {
				return subspaceKeyTestProto(6, nil, []*gen.FormerIndex{formerIndexProto("k", 1, 5, "k")}, nil)
			},
			new: func() *gen.MetaData {
				return subspaceKeyTestProto(8, []any{"k"}, nil, func(_ int, idx *recordlayer.Index) {
					idx.AddedVersion, idx.LastModifiedVersion = 7, 7
				})
			},
			wantPrefix: "former index key used for new index in meta-data",
		},
		{
			name: "a kept former index renamed, with missing names allowed",
			old: func() *gen.MetaData {
				return subspaceKeyTestProto(6, nil, []*gen.FormerIndex{formerIndexProto("k", 1, 5, "k")}, nil)
			},
			new: func() *gen.MetaData {
				return subspaceKeyTestProto(7, nil, []*gen.FormerIndex{formerIndexProto("k", 1, 5, "other")}, nil)
			},
			allowMissing: true,
			wantPrefix:   "name of former index differs from prior version",
		},
		{
			name: "a replacing former index naming another index, with missing names allowed",
			old:  old,
			new: func() *gen.MetaData {
				return subspaceKeyTestProto(6, nil, []*gen.FormerIndex{formerIndexProto("i0", 1, 5, "other")}, nil)
			},
			allowMissing: true,
			wantPrefix:   "former index has different name than old index",
		},
		{
			name: "an unnamed replacing former index, with missing names allowed",
			old:  old,
			new: func() *gen.MetaData {
				return subspaceKeyTestProto(6, nil, []*gen.FormerIndex{formerIndexProto("i0", 1, 5, "")}, nil)
			},
			allowMissing: true,
		},
		{
			name: "a last-modified version that went back",
			old: func() *gen.MetaData {
				return subspaceKeyTestProto(5, []any{nil}, nil, func(_ int, idx *recordlayer.Index) { idx.LastModifiedVersion = 5 })
			},
			new: func() *gen.MetaData {
				return subspaceKeyTestProto(7, []any{nil}, nil, func(_ int, idx *recordlayer.Index) { idx.LastModifiedVersion = 3 })
			},
			wantPrefix: "old index has last-modified version newer than new index",
		},
		{
			name: "a replacing former index added after the index",
			old: func() *gen.MetaData {
				return subspaceKeyTestProto(3, []any{nil}, nil, func(_ int, idx *recordlayer.Index) { idx.AddedVersion, idx.LastModifiedVersion = 2, 2 })
			},
			new: func() *gen.MetaData {
				return subspaceKeyTestProto(6, nil, []*gen.FormerIndex{formerIndexProto("i0", 3, 4, "i0")}, nil)
			},
			wantPrefix: "former index added after old index",
		},
		{
			name: "a replacing former index added before the index",
			old: func() *gen.MetaData {
				return subspaceKeyTestProto(5, []any{nil}, nil, func(_ int, idx *recordlayer.Index) { idx.AddedVersion, idx.LastModifiedVersion = 3, 3 })
			},
			new: func() *gen.MetaData {
				return subspaceKeyTestProto(7, nil, []*gen.FormerIndex{formerIndexProto("i0", 1, 6, "i0")}, nil)
			},
			wantPrefix: "former index reports added version older than replacing index",
		},
		{
			name: "the index type changed",
			old:  old,
			new: func() *gen.MetaData {
				return withIndexI0(subspaceKeyTestProto(4, []any{nil}, nil, nil), func(idx *gen.Index) { idx.Type = proto.String("rank") })
			},
			wantPrefix: "index type changed",
		},
		{
			name: "the root changed",
			old:  old,
			new: func() *gen.MetaData {
				return withIndexI0(subspaceKeyTestProto(4, []any{nil}, nil, nil), func(idx *gen.Index) {
					idx.RootExpression = recordlayer.Field("quantity").ToKeyExpression()
				})
			},
			wantPrefix: "index key expression changed",
		},
		{
			// FieldKeyExpression.equals ignores the null standin
			// (FieldKeyExpression.java:406-410); Go compared roots by proto.
			name: "the root changed only in its null interpretation",
			old:  old,
			new: func() *gen.MetaData {
				return withIndexI0(subspaceKeyTestProto(4, []any{nil}, nil, nil), func(idx *gen.Index) {
					idx.RootExpression.Field.NullInterpretation = gen.Field_UNIQUE.Enum()
				})
			},
		},
		{
			name: "the index no longer covers a record type",
			old: func() *gen.MetaData {
				return withIndexI0(subspaceKeyTestProto(3, []any{nil}, nil, nil), func(idx *gen.Index) { idx.RecordType = []string{"Order", "Customer"} })
			},
			new: func() *gen.MetaData {
				return withIndexI0(subspaceKeyTestProto(4, []any{nil}, nil, nil), func(idx *gen.Index) { idx.RecordType = []string{"Order"} })
			},
			wantPrefix: "new index removes record type",
		},
		{
			name: "the index covers a record type that is not newer",
			old:  old,
			new: func() *gen.MetaData {
				return withIndexI0(subspaceKeyTestProto(4, []any{nil}, nil, nil), func(idx *gen.Index) { idx.RecordType = []string{"Order", "Customer"} })
			},
			wantPrefix: "new index adds record type that is not newer than old meta-data",
		},
		{
			name: "a replacing former index added before the index, with older added versions allowed",
			old: func() *gen.MetaData {
				return subspaceKeyTestProto(5, []any{nil}, nil, func(_ int, idx *recordlayer.Index) { idx.AddedVersion, idx.LastModifiedVersion = 3, 3 })
			},
			new: func() *gen.MetaData {
				return subspaceKeyTestProto(7, nil, []*gen.FormerIndex{formerIndexProto("i0", 1, 6, "i0")}, nil)
			},
			allowOlder: true,
		},
	} {
		It("validates as Java does: "+c.name, func() {
			oldProto, newProto := c.old(), c.new()
			var java javaVerdict
			Expect(NewJavaInvoker().InvokeAs(context.Background(), "validateMetaDataEvolutionFlags", map[string]any{
				"oldProtoBytes":                      bytesToInts(marshalMetaData(oldProto)),
				"newProtoBytes":                      bytesToInts(marshalMetaData(newProto)),
				"allowMissingFormerIndexNames":       c.allowMissing,
				"allowOlderFormerIndexAddedVersions": c.allowOlder,
				"allowIndexRebuilds":                 false,
			}, &java)).To(Succeed())

			oldMD, err := recordlayer.RecordMetaDataFromProto(oldProto)
			Expect(err).NotTo(HaveOccurred())
			newMD, err := recordlayer.RecordMetaDataFromProto(newProto)
			Expect(err).NotTo(HaveOccurred())
			goErr := recordlayer.NewMetaDataEvolutionValidator().
				SetAllowMissingFormerIndexNames(c.allowMissing).
				SetAllowOlderFormerIndexAddedVersion(c.allowOlder).
				Build().Validate(oldMD, newMD)
			fmt.Fprintf(GinkgoWriter, "SUBSPACE_KEY_EVOLUTION %q java=%t %q go=%v\n", c.name, java.Valid, java.Error, goErr)

			if c.wantPrefix == "" {
				Expect(java.Valid).To(BeTrue(), "Java: %s", java.Error)
				Expect(goErr).NotTo(HaveOccurred())
				return
			}
			Expect(java.Valid).To(BeFalse())
			Expect(java.Error).To(HavePrefix(c.wantPrefix))
			var evolErr *recordlayer.MetaDataEvolutionError
			Expect(errors.As(goErr, &evolErr)).To(BeTrue(), "Go error: %v", goErr)
			Expect(evolErr.Message).To(HavePrefix(java.Error))
		})
	}
})

// validateRecordTypes, validateMessage and validateField (MetaDataEvolutionValidator.
// java:257-448) over the demo records file edited one field at a time: each
// shape runs through Java's validator and Go's, and the verdicts must agree,
// a refusal with Java's whole message as the prefix of Go's.
var _ = Describe("Field and record-type changes in meta-data evolution", func() {
	demo := func(version int, edit func(order *descriptorpb.DescriptorProto, file *descriptorpb.FileDescriptorProto, p *gen.MetaData)) func() *gen.MetaData {
		return func() *gen.MetaData {
			p := subspaceKeyTestProto(version, []any{nil}, nil, nil)
			if edit != nil {
				for _, m := range p.GetRecords().GetMessageType() {
					if m.GetName() == "Order" {
						edit(m, p.GetRecords(), p)
					}
				}
			}
			return p
		}
	}
	field := func(m *descriptorpb.DescriptorProto, name string) *descriptorpb.FieldDescriptorProto {
		for _, f := range m.GetField() {
			if f.GetName() == name {
				return f
			}
		}
		Fail("no field " + name)
		return nil
	}
	old := demo(3, nil)
	for _, c := range []struct {
		name       string
		new        func() *gen.MetaData
		wantPrefix string // empty: valid
	}{
		{"a field renamed", demo(4, func(m *descriptorpb.DescriptorProto, _ *descriptorpb.FileDescriptorProto, _ *gen.MetaData) {
			field(m, "quantity").Name = proto.String("qty")
		}), "field renamed"},
		{"a field's type changed", demo(4, func(m *descriptorpb.DescriptorProto, _ *descriptorpb.FileDescriptorProto, _ *gen.MetaData) {
			field(m, "quantity").Type = descriptorpb.FieldDescriptorProto_TYPE_STRING.Enum()
		}), "field type changed"},
		{"an int32 widened to int64", demo(4, func(m *descriptorpb.DescriptorProto, _ *descriptorpb.FileDescriptorProto, _ *gen.MetaData) {
			field(m, "quantity").Type = descriptorpb.FieldDescriptorProto_TYPE_INT64.Enum()
		}), ""},
		{"a field removed", demo(4, func(m *descriptorpb.DescriptorProto, _ *descriptorpb.FileDescriptorProto, _ *gen.MetaData) {
			kept := m.Field[:0]
			for _, f := range m.Field {
				if f.GetName() != "coord_y" {
					kept = append(kept, f)
				}
			}
			m.Field = kept
		}), "field removed from message descriptor"},
		{"a required field added", demo(4, func(m *descriptorpb.DescriptorProto, _ *descriptorpb.FileDescriptorProto, _ *gen.MetaData) {
			m.Field = append(m.Field, &descriptorpb.FieldDescriptorProto{
				Name: proto.String("extra"), Number: proto.Int32(20),
				Type: descriptorpb.FieldDescriptorProto_TYPE_INT32.Enum(), Label: descriptorpb.FieldDescriptorProto_LABEL_REQUIRED.Enum(),
			})
		}), "required field added to record type"},
		// Java checks a required field losing its label and a repeated one
		// losing it, and presence; an optional field made required keeps its
		// presence (MetaDataEvolutionValidator.java:306-315).
		{"an optional field made required", demo(4, func(m *descriptorpb.DescriptorProto, _ *descriptorpb.FileDescriptorProto, _ *gen.MetaData) {
			field(m, "quantity").Label = descriptorpb.FieldDescriptorProto_LABEL_REQUIRED.Enum()
		}), ""},
		{"a repeated field made optional", demo(4, func(m *descriptorpb.DescriptorProto, _ *descriptorpb.FileDescriptorProto, _ *gen.MetaData) {
			field(m, "tags").Label = descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum()
		}), "repeated field is no longer repeated"},
		{"an enum value removed", demo(4, func(_ *descriptorpb.DescriptorProto, file *descriptorpb.FileDescriptorProto, _ *gen.MetaData) {
			for _, m := range file.GetMessageType() {
				for _, e := range m.GetEnumType() {
					kept := e.Value[:0]
					for _, v := range e.Value {
						if v.GetName() != "PINK" {
							kept = append(kept, v)
						}
					}
					e.Value = kept
				}
			}
			for _, e := range file.GetEnumType() {
				kept := e.Value[:0]
				for _, v := range e.Value {
					if v.GetName() != "PINK" {
						kept = append(kept, v)
					}
				}
				e.Value = kept
			}
		}), "enum removes value"},
		{"a primary key changed", demo(4, func(_ *descriptorpb.DescriptorProto, _ *descriptorpb.FileDescriptorProto, p *gen.MetaData) {
			for _, rt := range p.GetRecordTypes() {
				if rt.GetName() == "Order" {
					rt.PrimaryKey = recordlayer.Field("quantity").ToKeyExpression()
				}
			}
		}), "record type primary key changed"},
	} {
		It("validates as Java does: "+c.name, func() {
			oldProto, newProto := old(), c.new()
			var java javaVerdict
			Expect(NewJavaInvoker().InvokeAs(context.Background(), "validateMetaDataEvolutionFlags", map[string]any{
				"oldProtoBytes":                      bytesToInts(marshalMetaData(oldProto)),
				"newProtoBytes":                      bytesToInts(marshalMetaData(newProto)),
				"allowMissingFormerIndexNames":       false,
				"allowOlderFormerIndexAddedVersions": false,
				"allowIndexRebuilds":                 false,
			}, &java)).To(Succeed())
			oldMD, err := recordlayer.RecordMetaDataFromProto(oldProto)
			Expect(err).NotTo(HaveOccurred())
			newMD, err := recordlayer.RecordMetaDataFromProto(newProto)
			Expect(err).NotTo(HaveOccurred())
			goErr := recordlayer.NewMetaDataEvolutionValidator().Build().Validate(oldMD, newMD)
			fmt.Fprintf(GinkgoWriter, "FIELD_EVOLUTION %q java=%t %q go=%v\n", c.name, java.Valid, java.Error, goErr)
			if c.wantPrefix == "" {
				Expect(java.Valid).To(BeTrue(), "Java: %s", java.Error)
				Expect(goErr).NotTo(HaveOccurred())
				return
			}
			Expect(java.Valid).To(BeFalse())
			Expect(java.Error).To(HavePrefix(c.wantPrefix))
			var evolErr *recordlayer.MetaDataEvolutionError
			Expect(errors.As(goErr, &evolErr)).To(BeTrue(), "Go error: %v", goErr)
			Expect(evolErr.Message).To(HavePrefix(java.Error))
		})
	}
})

// A stored former-index subspace key must pack exactly one non-null item, as
// Java's FormerIndex(proto) reads it (FormerIndex.java:51-68, through
// Index.decodeSubspaceKey): an absent key is the empty tuple. Go used to read
// all four malformed shapes as a nil key. Each shape is marshalled and read
// back through Go's decoder, so the presence of an empty key is what Go's
// unmarshaller keeps.
var _ = Describe("Former-index subspace keys read as Java reads them", func() {
	coreError := "ERROR com.apple.foundationdb.record.RecordCoreException subspace key must encode a single item tuple"
	nullError := "ERROR com.apple.foundationdb.record.RecordCoreArgumentException FormerIndex initialized with null subspace key"
	for _, c := range []struct {
		name     string
		key      []byte
		javaWant string
		goClass  string // "core" or "argument"; empty: loads
	}{
		{"absent", nil, coreError, "core"},
		{"present and empty", []byte{}, coreError, "core"},
		{"two items", tuple.Tuple{"a", "b"}.Pack(), coreError, "core"},
		{"a null item", tuple.Tuple{nil}.Pack(), nullError, "argument"},
		{"one string item", tuple.Tuple{"k"}.Pack(), "OK k", ""},
	} {
		It("reads the key as Java does: "+c.name, func() {
			former := &gen.FormerIndex{SubspaceKey: c.key, AddedVersion: proto.Int32(1), RemovedVersion: proto.Int32(2), FormerName: proto.String("gone")}
			var java struct {
				Outcome string `json:"outcome"`
			}
			encoded, err := proto.Marshal(former)
			Expect(err).NotTo(HaveOccurred())
			Expect(NewJavaInvoker().InvokeAs(context.Background(), "formerIndexFromProtoVerdict",
				map[string]any{"formerIndexProto": bytesToInts(encoded)}, &java)).To(Succeed())

			var decoded gen.MetaData
			Expect(proto.Unmarshal(marshalMetaData(subspaceKeyTestProto(3, nil, []*gen.FormerIndex{former}, nil)), &decoded)).To(Succeed())
			md, goErr := recordlayer.RecordMetaDataFromProto(&decoded)
			fmt.Fprintf(GinkgoWriter, "FORMER_INDEX_KEY %q java=%s go=%v\n", c.name, java.Outcome, goErr)

			Expect(java.Outcome).To(Equal(c.javaWant))
			var core *recordlayer.RecordCoreError
			var arg *recordlayer.RecordCoreArgumentError
			switch c.goClass {
			case "":
				Expect(goErr).NotTo(HaveOccurred())
				Expect(md.GetFormerIndexes()).To(HaveLen(1))
				Expect(md.GetFormerIndexes()[0].SubspaceKey).To(Equal("k"))
			case "core":
				Expect(errors.As(goErr, &core)).To(BeTrue(), "Go error: %v", goErr)
				Expect(core.Message).To(Equal("subspace key must encode a single item tuple"))
			case "argument":
				Expect(errors.As(goErr, &arg)).To(BeTrue(), "Go error: %v", goErr)
				Expect(arg.Message).To(Equal("FormerIndex initialized with null subspace key"))
				Expect(arg.IndexName).To(Equal("gone"))
			}
		})
	}
})
