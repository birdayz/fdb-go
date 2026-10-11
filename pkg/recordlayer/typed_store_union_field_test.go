package recordlayer

import (
	"context"
	"errors"

	"fdb.dev/gen"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"
	"google.golang.org/protobuf/types/dynamicpb"
)

// A typed store over the catalog schema, whose union is CatalogUnion: Java's
// TypedRecordSerializer binds the store to one union field, so a record held
// in another field is refused on load and never overwritten by a save.
var _ = Describe("TypedStoreUnionField", func() {
	catalogMetaData := func(fd protoreflect.FileDescriptor) *RecordMetaData {
		b := NewRecordMetaDataBuilder().SetRecords(fd)
		// One key space for both types, so a key can hold either.
		b.GetRecordType("Databases").SetPrimaryKey(Field("DATABASE_ID"))
		b.GetRecordType("Schemas").SetPrimaryKey(Field("DATABASE_ID"))
		b.GetRecordType("Templates").SetPrimaryKey(Field("TEMPLATE_NAME"))
		md, err := b.Build()
		Expect(err).NotTo(HaveOccurred())
		return md
	}
	// A copy of the compiled file, so the metadata decodes its records as dynamicpb.
	dynamicCatalog := func() protoreflect.FileDescriptor {
		fd, err := protodesc.NewFile(protodesc.ToFileDescriptorProto(gen.File_catalog_data_proto), protoregistry.GlobalFiles)
		Expect(err).NotTo(HaveOccurred())
		return fd
	}
	openStore := func(rtx *FDBRecordContext, md *RecordMetaData) *FDBRecordStore {
		store, err := NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(md).SetSubspace(specSubspace()).CreateOrOpen()
		Expect(err).NotTo(HaveOccurred())
		return store
	}
	expectSerializationError := func(err error, message string) {
		GinkgoHelper()
		var serErr *RecordSerializationError
		Expect(errors.As(err, &serErr)).To(BeTrue(), "got %v (%T), want a RecordSerializationError", err, err)
		Expect(serErr.Message).To(Equal(message))
	}
	expectMetaDataError := func(err error, message string) {
		GinkgoHelper()
		var mdErr *MetaDataError
		Expect(errors.As(err, &mdErr)).To(BeTrue(), "got %v (%T), want a MetaDataError", err, err)
		Expect(mdErr.Message).To(Equal(message))
	}
	expectDeserializationError := func(err error, message string) {
		GinkgoHelper()
		var deErr *RecordDeserializationError
		Expect(errors.As(err, &deErr)).To(BeTrue(), "got %v (%T), want a RecordDeserializationError", err, err)
		expectSerializationError(err, message)
	}

	It("round-trips records of a non-demo union", func() {
		md := catalogMetaData(gen.File_catalog_data_proto)
		_, err := sharedDB.Run(context.Background(), func(rtx *FDBRecordContext) (any, error) {
			store := openStore(rtx, md)
			databases, err := GetTypedRecordStore[*gen.Databases](store, "Databases")
			Expect(err).NotTo(HaveOccurred())
			templates, err := GetTypedRecordStore[*gen.Templates](store, "Templates")
			Expect(err).NotTo(HaveOccurred())

			db := &gen.Databases{DATABASE_ID: proto.String("/FRL/DB")}
			_, err = databases.SaveRecord(db)
			Expect(err).NotTo(HaveOccurred())
			tmpl := &gen.Templates{TEMPLATE_NAME: proto.String("T"), TEMPLATE_VERSION: proto.Int32(3), META_DATA: []byte{1, 2}}
			_, err = templates.SaveRecord(tmpl)
			Expect(err).NotTo(HaveOccurred())

			loadedDB, err := databases.LoadRecord(tuple.Tuple{"/FRL/DB"})
			Expect(err).NotTo(HaveOccurred())
			Expect(proto.Equal(loadedDB.Record, db)).To(BeTrue(), "loaded %v", loadedDB.Record)
			Expect(loadedDB.RecordType.Name).To(Equal("Databases"))
			loadedTmpl, err := templates.LoadRecord(tuple.Tuple{"T"})
			Expect(err).NotTo(HaveOccurred())
			Expect(proto.Equal(loadedTmpl.Record, tmpl)).To(BeTrue(), "loaded %v", loadedTmpl.Record)

			missing, err := databases.LoadRecord(tuple.Tuple{"/FRL/NONE"})
			Expect(err).NotTo(HaveOccurred())
			Expect(missing).To(BeNil())
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("refuses a record held in another union field and leaves it in place", func() {
		md := catalogMetaData(gen.File_catalog_data_proto)
		_, err := sharedDB.Run(context.Background(), func(rtx *FDBRecordContext) (any, error) {
			store := openStore(rtx, md)
			schema := &gen.Schemas{DATABASE_ID: proto.String("/FRL/X"), SCHEMA_NAME: proto.String("S")}
			_, err := store.SaveRecord(schema)
			Expect(err).NotTo(HaveOccurred())
			databases, err := GetTypedRecordStore[*gen.Databases](store, "Databases")
			Expect(err).NotTo(HaveOccurred())

			_, err = databases.LoadRecord(tuple.Tuple{"/FRL/X"})
			expectDeserializationError(err, "Specified union field was not set")

			// Java's typed save and delete read the existing record through the typed serializer.
			_, err = databases.SaveRecord(&gen.Databases{DATABASE_ID: proto.String("/FRL/X")})
			expectDeserializationError(err, "Specified union field was not set")
			_, err = databases.DeleteRecord(tuple.Tuple{"/FRL/X"})
			expectDeserializationError(err, "Specified union field was not set")
			stored, err := store.LoadRecord(tuple.Tuple{"/FRL/X"})
			Expect(err).NotTo(HaveOccurred())
			Expect(proto.Equal(stored.Record, schema)).To(BeTrue(), "the other type's record was overwritten: %v", stored.Record)
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("binds a dynamicpb typed store to its record type's union field", func() {
		md := catalogMetaData(dynamicCatalog())
		dbDesc := md.GetRecordType("Databases").Descriptor
		schemaDesc := md.GetRecordType("Schemas").Descriptor
		_, err := sharedDB.Run(context.Background(), func(rtx *FDBRecordContext) (any, error) {
			store := openStore(rtx, md)
			databases, err := GetTypedRecordStore[*dynamicpb.Message](store, "Databases")
			Expect(err).NotTo(HaveOccurred())

			db := dynamicpb.NewMessage(dbDesc)
			db.Set(dbDesc.Fields().ByName("DATABASE_ID"), protoreflect.ValueOfString("/FRL/DYN"))
			_, err = databases.SaveRecord(db)
			Expect(err).NotTo(HaveOccurred())
			loaded, err := databases.LoadRecord(tuple.Tuple{"/FRL/DYN"})
			Expect(err).NotTo(HaveOccurred())
			Expect(proto.Equal(loaded.Record, db)).To(BeTrue(), "loaded %v", loaded.Record)

			schema := dynamicpb.NewMessage(schemaDesc)
			schema.Set(schemaDesc.Fields().ByName("DATABASE_ID"), protoreflect.ValueOfString("/FRL/OTHER"))
			_, err = databases.SaveRecord(schema)
			expectSerializationError(err, "fieldDescriptor does not match recordType")
			exists, err := store.RecordExists(tuple.Tuple{"/FRL/OTHER"}, IsolationLevelSerializable)
			Expect(err).NotTo(HaveOccurred())
			Expect(exists).To(BeFalse(), "a Schemas record was saved through the Databases store")

			_, err = store.SaveRecord(schema)
			Expect(err).NotTo(HaveOccurred())
			_, err = databases.LoadRecord(tuple.Tuple{"/FRL/OTHER"})
			expectDeserializationError(err, "Specified union field was not set")
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("refuses a Go type the metadata does not decode the record type as", func() {
		md := catalogMetaData(dynamicCatalog())
		dbDesc := md.GetRecordType("Databases").Descriptor
		_, err := sharedDB.Run(context.Background(), func(rtx *FDBRecordContext) (any, error) {
			store := openStore(rtx, md)
			db := dynamicpb.NewMessage(dbDesc)
			db.Set(dbDesc.Fields().ByName("DATABASE_ID"), protoreflect.ValueOfString("/FRL/GEN"))
			_, err := store.SaveRecord(db)
			Expect(err).NotTo(HaveOccurred())
			// The metadata decodes Databases as dynamicpb, never as the compiled type.
			databases, err := GetTypedRecordStore[*gen.Databases](store, "Databases")
			Expect(err).NotTo(HaveOccurred())

			_, err = databases.LoadRecord(tuple.Tuple{"/FRL/GEN"})
			expectDeserializationError(err, "Builder does not match union type")
			// A typed save resolves the record's type by its descriptor before
			// anything else (FDBRecordStore.java:552, RecordMetaData.java:219-223),
			// ahead of the existing record's load at /FRL/GEN.
			for _, key := range []string{"/FRL/GEN", "/FRL/GEN2"} {
				_, err = databases.SaveRecord(&gen.Databases{DATABASE_ID: proto.String(key)})
				expectMetaDataError(err, "descriptor did not match record type")
			}
			var scanErr error
			for _, err := range Seq2(databases.ScanRecords(nil, ForwardScan()), context.Background()) {
				if err != nil {
					scanErr = err
					break
				}
			}
			expectDeserializationError(scanErr, "Builder does not match union type")
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})
})
