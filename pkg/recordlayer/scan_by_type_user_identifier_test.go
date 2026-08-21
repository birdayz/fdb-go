package recordlayer

import (
	"context"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/dynamicpb"
)

// A SQL identifier reaches ScanRecordsByType because GetRecordType accepts one:
// the escape fallback resolves `MY$TABLE` to the type stored as `MY__1TABLE`.
// The FAST path therefore works, and only the fast path was ever exercised --
// every checked-in fixture with a name worth resolving also has a RecordTypeKey
// prefix.
//
// The SLOW path compared the caller's string against `rec.RecordType.Name`, the
// STORED spelling, so it matched nothing and the scan returned zero rows with a
// nil error. That is the worst shape a wrong answer can take: an empty result is
// indistinguishable from "this type has no records", so a script scanning with
// the very name `record scan -o json` had just printed would report success and
// silently process nothing.
//
// The two paths must agree, and the only way they can disagree is one resolving
// the name while the other compares it raw.
var _ = Describe("ScanRecordsByType with a SQL identifier", func() {
	ctx := context.Background()

	// Order's primary key here is Field("order_id"), NOT a RecordTypeKey
	// concat, so this fixture takes the slow path by construction.
	renamedOrderMetaData := func() *RecordMetaData {
		md, err := renameRecordTypes(multiTypeMetaData(), map[string]string{"Order": "MY__1TABLE"})
		Expect(err).NotTo(HaveOccurred())
		return md
	}

	It("routes this fixture down the slow path, which is what makes the arms below mean anything", func() {
		md := renamedOrderMetaData()
		rt := md.GetRecordType("MY__1TABLE")
		Expect(rt).NotTo(BeNil(), "the rename did not take; the fixture cannot express the defect")
		Expect(primaryKeyHasRecordTypePrefix(rt.PrimaryKey)).To(BeFalse(),
			"MY__1TABLE grew a RecordTypeKey prefix, so ScanRecordsByType now takes the\n"+
				"FAST path and the SQL-name arm below passes for a reason that has nothing\n"+
				"to do with the predicate it is meant to pin. Give this fixture a\n"+
				"prefix-less primary key again, or move the arms to a type that has one.")
	})

	DescribeTable("finds the record under either spelling of its name",
		func(scanName string) {
			md := renamedOrderMetaData()
			ks := specSubspace()

			rt := md.GetRecordType("MY__1TABLE")
			Expect(rt).NotTo(BeNil())
			rec := dynamicpb.NewMessage(rt.Descriptor)
			rec.Set(rt.Descriptor.Fields().ByName("order_id"), protoreflect.ValueOfInt64(7))

			_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
				if err != nil {
					return nil, err
				}
				return store.SaveRecord(rec)
			})
			Expect(err).NotTo(HaveOccurred())

			got, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
				if err != nil {
					return nil, err
				}
				return AsList(ctx, store.ScanRecordsByType(scanName, nil, ForwardScan()))
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(got).To(HaveLen(1),
				"ScanRecordsByType(%q) returned no rows and no error. GetRecordType accepts\n"+
					"the SQL identifier, so the caller gets past validation; the slow-path\n"+
					"predicate must compare against the RESOLVED RecordType.Name, never the\n"+
					"string it was handed.", scanName)
		},
		Entry("storage spelling", "MY__1TABLE"),
		Entry("SQL identifier", "MY$TABLE"),
	)

	It("still matches nothing for a name that resolves to no type at all", func() {
		md := renamedOrderMetaData()
		ks := specSubspace()

		got, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
			if err != nil {
				return nil, err
			}
			return AsList(ctx, store.ScanRecordsByType("NoSuchType", nil, ForwardScan()))
		})
		Expect(err).NotTo(HaveOccurred())
		Expect(got).To(BeEmpty(),
			"An unresolvable name started matching records. Resolving the name for the\n"+
				"predicate must not turn a miss into a match-everything.")
	})
})
