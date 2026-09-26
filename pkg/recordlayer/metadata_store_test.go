package recordlayer

import (
	"context"
	"errors"

	"fdb.dev/gen"
	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"
	"google.golang.org/protobuf/types/descriptorpb"
)

var _ = Describe("FDBMetaDataStore", func() {
	ctx := context.Background()

	// buildMDProto builds a real, buildable MetaData proto —
	// SaveRecordMetaData validates like Java's saveAndSetCurrent, so bare
	// version-only protos are rejected ("new metadata does not build").
	buildMDProto := func(version int, configure func(b *RecordMetaDataBuilder)) *gen.MetaData {
		builder := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
		builder.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
		builder.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
		builder.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))
		if configure != nil {
			configure(builder)
		}
		builder.SetVersion(version)
		md, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())
		mdProto, err := md.ToProto()
		Expect(err).NotTo(HaveOccurred())
		return mdProto
	}

	saveMD := func(store *FDBMetaDataStore, mdProto *gen.MetaData) error {
		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			return nil, store.SaveRecordMetaData(rtx.Transaction(), mdProto)
		})
		return err
	}

	loadCurrent := func(store *FDBMetaDataStore) *gen.MetaData {
		var loaded *gen.MetaData
		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			var loadErr error
			loaded, loadErr = store.LoadRecordMetaDataProto(rtx.Transaction())
			return nil, loadErr
		})
		Expect(err).NotTo(HaveOccurred())
		return loaded
	}

	loadAtVersion := func(store *FDBMetaDataStore, version int32) *gen.MetaData {
		var loaded *gen.MetaData
		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			var loadErr error
			loaded, loadErr = store.LoadRecordMetaDataProtoAtVersion(rtx.Transaction(), version)
			return nil, loadErr
		})
		Expect(err).NotTo(HaveOccurred())
		return loaded
	}

	It("saves and loads metadata proto", func() {
		ss := specSubspace()
		store := NewFDBMetaDataStore(ss)

		mdProto := buildMDProto(1, nil)
		Expect(saveMD(store, mdProto)).To(Succeed())

		loaded := loadCurrent(store)
		Expect(loaded).NotTo(BeNil())
		Expect(proto.Equal(loaded, mdProto)).To(BeTrue())
	})

	It("archives previous version on save", func() {
		ss := specSubspace()
		store := NewFDBMetaDataStore(ss)

		Expect(saveMD(store, buildMDProto(1, nil))).To(Succeed())
		Expect(saveMD(store, buildMDProto(2, nil))).To(Succeed())

		Expect(loadCurrent(store).GetVersion()).To(Equal(int32(2)))

		historical := loadAtVersion(store, 1)
		Expect(historical).NotTo(BeNil())
		Expect(historical.GetVersion()).To(Equal(int32(1)))
	})

	It("rejects a version that does not increase, without touching history", func() {
		ss := specSubspace()
		store := NewFDBMetaDataStore(ss)

		Expect(saveMD(store, buildMDProto(2, nil))).To(Succeed())

		// Equal version: rejected — Java's saveAndSetCurrent hard check,
		// which runs BEFORE the evolution validator and does not consult
		// allowNoVersionChange.
		err := saveMD(store, buildMDProto(2, nil))
		var versionErr *MetaDataVersionMustIncreaseError
		Expect(errors.As(err, &versionErr)).To(BeTrue(), "got: %v", err)
		Expect(versionErr.OldVersion).To(Equal(int32(2)))
		Expect(versionErr.NewVersion).To(Equal(int32(2)))

		// Lower version: rejected too.
		err = saveMD(store, buildMDProto(1, nil))
		Expect(errors.As(err, &versionErr)).To(BeTrue(), "got: %v", err)

		// The failed saves must not have archived anything: a retried
		// save that re-ran after its first attempt committed would
		// otherwise re-archive the NEW metadata at H/newVersion.
		Expect(loadAtVersion(store, 2)).To(BeNil())
		Expect(loadCurrent(store).GetVersion()).To(Equal(int32(2)))
	})

	It("rejects an invalid evolution inside the save transaction", func() {
		ss := specSubspace()
		store := NewFDBMetaDataStore(ss)

		Expect(saveMD(store, buildMDProto(1, nil))).To(Succeed())

		// Changing a record type's primary key is never a legal
		// evolution, whatever the version says.
		badEvolution := buildMDProto(2, func(b *RecordMetaDataBuilder) {
			b.GetRecordType("Order").SetPrimaryKey(Field("price"))
		})
		err := saveMD(store, badEvolution)
		Expect(err).To(HaveOccurred())

		// Nothing was written: current stays v1, no archive appeared.
		Expect(loadCurrent(store).GetVersion()).To(Equal(int32(1)))
		Expect(loadAtVersion(store, 1)).To(BeNil())
	})

	It("SetEvolutionValidator wires custom knobs into the save gate", func() {
		ss := specSubspace()
		store := NewFDBMetaDataStore(ss)

		v1 := buildMDProto(1, func(b *RecordMetaDataBuilder) {
			b.AddIndex("Order", NewIndex("Order$flex", Field("price")))
		})
		Expect(saveMD(store, v1)).To(Succeed())

		// Same index name, different key expression — an index rebuild.
		// A real evolution keeps the index's original added_version and
		// bumps last_modified_version, so craft v2 from v1's proto rather
		// than a fresh builder (which would restamp added_version and trip
		// the separate added-version check).
		alt := buildMDProto(1, func(b *RecordMetaDataBuilder) {
			b.AddIndex("Order", NewIndex("Order$flex", Field("quantity")))
		})
		v2 := proto.Clone(v1).(*gen.MetaData)
		v2.Version = proto.Int32(2)
		v2.GetIndexes()[0].RootExpression = alt.GetIndexes()[0].GetRootExpression()
		v2.GetIndexes()[0].LastModifiedVersion = proto.Int32(2)

		// Default validator (Java's getDefaultInstance): rejected.
		Expect(saveMD(store, v2)).To(HaveOccurred())

		// allowIndexRebuilds: accepted — proving the setter reaches the
		// in-transaction validation.
		store.SetEvolutionValidator(NewMetaDataEvolutionValidator().
			SetAllowIndexRebuilds(true).Build())
		Expect(saveMD(store, v2)).To(Succeed())
		Expect(loadCurrent(store).GetVersion()).To(Equal(int32(2)))
	})

	It("persists ignored index options with history and rejects unrelated changes atomically", func() {
		ss := specSubspace()
		store := NewFDBMetaDataStore(ss)
		v1 := buildMDProto(1, func(b *RecordMetaDataBuilder) {
			b.AddIndex("Order", NewIndex("price", Field("price")))
		})
		Expect(saveMD(store, v1)).To(Succeed())
		v2 := proto.Clone(v1).(*gen.MetaData)
		v2.Version = proto.Int32(2)
		v2.Indexes[0].Options = append(v2.Indexes[0].Options, &gen.Index_Option{
			Key: proto.String("applicationTag"), Value: proto.String("new"),
		})
		Expect(saveMD(store, v2)).To(MatchError(ContainSubstring("applicationTag")))
		Expect(proto.Equal(loadCurrent(store), v1)).To(BeTrue())
		Expect(loadAtVersion(store, 1)).To(BeNil())
		store.SetEvolutionValidator(NewMetaDataEvolutionValidator().SetIgnoredIndexOptions([]string{"applicationTag"}).Build())
		Expect(saveMD(store, v2)).To(Succeed())
		reopened := NewFDBMetaDataStore(ss)
		Expect(proto.Equal(loadCurrent(reopened), v2)).To(BeTrue())
		Expect(proto.Equal(loadAtVersion(reopened, 1), v1)).To(BeTrue())
		v3 := proto.Clone(v2).(*gen.MetaData)
		v3.Version = proto.Int32(3)
		v3.Indexes[0].Options = append(v3.Indexes[0].Options, &gen.Index_Option{
			Key: proto.String(IndexOptionUnique), Value: proto.String("true"),
		})
		Expect(saveMD(store, v3)).To(MatchError(ContainSubstring("index adds uniqueness constraint")))
		Expect(proto.Equal(loadCurrent(reopened), v2)).To(BeTrue())
		Expect(loadAtVersion(reopened, 2)).To(BeNil())
	})

	It("rejects new metadata that does not build", func() {
		ss := specSubspace()
		store := NewFDBMetaDataStore(ss)

		err := saveMD(store, &gen.MetaData{Version: proto.Int32(1)})
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("new metadata does not build"))
		Expect(loadCurrent(store)).To(BeNil())
	})

	// A records file with a message named UnionDescriptor beside the usage=UNION
	// union: the target's union is the usage=UNION message, and the store saves
	// and reloads the metadata so (ws-j-design.md 4d; data a pre-release Go build
	// framed by the message named UnionDescriptor is not supported, RFC-257 item 9).
	It("stores metadata over a records file with a message named UnionDescriptor beside the union, as the target does", func() {
		store := NewFDBMetaDataStore(specSubspace())
		records := func() protoreflect.FileDescriptor {
			usage := func(on bool) *descriptorpb.MessageOptions {
				if !on {
					return nil
				}
				o := &descriptorpb.MessageOptions{}
				proto.SetExtension(o, gen.E_Record, &gen.RecordTypeOptions{Usage: gen.RecordTypeOptions_UNION.Enum()})
				return o
			}
			holdsT := func(name string, union bool) *descriptorpb.DescriptorProto {
				return &descriptorpb.DescriptorProto{Name: proto.String(name), Options: usage(union), Field: []*descriptorpb.FieldDescriptorProto{{
					Name: proto.String("_T"), Number: proto.Int32(1), Type: descriptorpb.FieldDescriptorProto_TYPE_MESSAGE.Enum(),
					Label: descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(), TypeName: proto.String(".sd.T"),
				}}}
			}
			fd, err := protodesc.NewFile(&descriptorpb.FileDescriptorProto{
				Name: proto.String("store_shape_d.proto"), Package: proto.String("sd"), Syntax: proto.String("proto2"),
				Dependency: []string{"record_metadata_options.proto"},
				MessageType: []*descriptorpb.DescriptorProto{
					{Name: proto.String("T"), Field: []*descriptorpb.FieldDescriptorProto{{
						Name: proto.String("id"), Number: proto.Int32(1), Type: descriptorpb.FieldDescriptorProto_TYPE_INT64.Enum(),
						Label: descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(),
					}}},
					holdsT("Envelope", true),
					holdsT("UnionDescriptor", false),
				},
			}, protoregistry.GlobalFiles)
			Expect(err).NotTo(HaveOccurred())
			return fd
		}
		build := func(b *RecordMetaDataBuilder) *gen.MetaData {
			b.GetRecordType("T").SetPrimaryKey(Field("id"))
			b.SetVersion(1)
			md, err := b.Build()
			Expect(err).NotTo(HaveOccurred())
			mdProto, err := md.ToProto()
			Expect(err).NotTo(HaveOccurred())
			return mdProto
		}

		named := build(NewRecordMetaDataBuilder().SetRecordsWithUnionName(records(), "Envelope"))
		Expect(saveMD(store, named)).To(Succeed())
		Expect(proto.Equal(loadCurrent(store), named)).To(BeTrue())
		loaded, err := RecordMetaDataFromProto(loadCurrent(store))
		Expect(err).NotTo(HaveOccurred())
		Expect(string(loaded.GetUnionDescriptor().Name())).To(Equal("Envelope"))
	})

	It("refuses to overwrite corrupt current metadata", func() {
		ss := specSubspace()
		store := NewFDBMetaDataStore(ss)

		// Plant garbage at the CURRENT_KEY unsplit slot.
		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			key := ss.Pack(tuple.Tuple{nil, int64(unsplitRecord)})
			rtx.Transaction().Set(fdb.Key(key), []byte{0xff})
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())

		err = saveMD(store, buildMDProto(1, nil))
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("parse current metadata"))
	})

	It("returns nil for non-existent metadata", func() {
		ss := specSubspace()
		store := NewFDBMetaDataStore(ss)
		Expect(loadCurrent(store)).To(BeNil())
	})

	It("returns nil for non-existent historical version", func() {
		ss := specSubspace()
		store := NewFDBMetaDataStore(ss)
		Expect(loadAtVersion(store, 99)).To(BeNil())
	})

	It("Subspace returns the configured subspace", func() {
		ss := specSubspace()
		store := NewFDBMetaDataStore(ss)
		Expect(store.Subspace()).To(Equal(ss))
	})

	It("stores metadata at unsplit suffix 0 for Java wire compatibility", func() {
		ss := specSubspace()
		store := NewFDBMetaDataStore(ss)

		Expect(saveMD(store, buildMDProto(1, nil))).To(Succeed())

		// Verify the raw FDB key matches Java's format: subspace.pack(null, 0)
		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			expectedKey := ss.Pack(tuple.Tuple{nil, int64(unsplitRecord)})
			value, getErr := rtx.Transaction().Get(fdb.Key(expectedKey)).Get()
			Expect(getErr).NotTo(HaveOccurred())
			Expect(value).NotTo(BeNil(), "metadata should be stored at unsplit suffix 0 key")
			Expect(len(value)).To(BeNumerically(">", 0))
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})
})
