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
// EVERY ARM SEEDS BOTH TYPES, and that is not tidiness. With only the type under
// test in the subspace, "want 1" is satisfied by a predicate that matches
// EVERYTHING, and "want 0" is satisfied by an EMPTY STORE -- so a regression to
// match-all would have passed the whole file. The Customer row is what makes
// each count discriminate.
var _ = Describe("ScanRecordsByType with a SQL identifier", func() {
	ctx := context.Background()

	// Order's primary key here is Field("order_id"), NOT a RecordTypeKey
	// concat, so this fixture takes the slow path by construction.
	renamedOrderMetaData := func() *RecordMetaData {
		md, err := renameRecordTypes(multiTypeMetaData(), map[string]string{"Order": "MY__1TABLE"})
		Expect(err).NotTo(HaveOccurred())
		return md
	}

	// seedOneOfEach writes one MY__1TABLE row and one Customer row, so a count
	// of 1 means "matched the right type" rather than "matched anything".
	seedOneOfEach := func(md *RecordMetaData) {
		GinkgoHelper()
		renamed := md.GetRecordType("MY__1TABLE")
		Expect(renamed).NotTo(BeNil())
		order := dynamicpb.NewMessage(renamed.Descriptor)
		order.Set(renamed.Descriptor.Fields().ByName("order_id"), protoreflect.ValueOfInt64(7))

		customer := md.GetRecordType("Customer")
		Expect(customer).NotTo(BeNil())
		cust := dynamicpb.NewMessage(customer.Descriptor)
		cust.Set(customer.Descriptor.Fields().ByName("customer_id"), protoreflect.ValueOfInt64(9))

		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(specSubspace()).CreateOrOpen()
			if err != nil {
				return nil, err
			}
			if _, err := store.SaveRecord(order); err != nil {
				return nil, err
			}
			return store.SaveRecord(cust)
		})
		Expect(err).NotTo(HaveOccurred())
	}

	scanNames := func(md *RecordMetaData, typeName string) []string {
		GinkgoHelper()
		out, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(specSubspace()).CreateOrOpen()
			if err != nil {
				return nil, err
			}
			recs, err := AsList(ctx, store.ScanRecordsByType(typeName, nil, ForwardScan()))
			if err != nil {
				return nil, err
			}
			names := make([]string, 0, len(recs))
			for _, r := range recs {
				names = append(names, r.RecordType.Name)
			}
			return names, nil
		})
		Expect(err).NotTo(HaveOccurred())
		return out.([]string)
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

	DescribeTable("finds only that type's records under either spelling of its name",
		func(scanName string) {
			md := renamedOrderMetaData()
			seedOneOfEach(md)
			Expect(scanNames(md, scanName)).To(Equal([]string{"MY__1TABLE"}),
				"ScanRecordsByType(%q) did not return exactly the MY__1TABLE row. Zero rows\n"+
					"with a nil error is what a raw comparison produces -- GetRecordType\n"+
					"accepts the SQL identifier, so the caller gets past validation, and the\n"+
					"slow-path predicate must compare against the RESOLVED RecordType.Name.\n"+
					"A Customer row is present too, so a match-everything predicate fails\n"+
					"here rather than passing as 'found it'.", scanName)
		},
		Entry("storage spelling", "MY__1TABLE"),
		Entry("SQL identifier", "MY$TABLE"),
	)

	It("still matches nothing for a name that resolves to no type at all", func() {
		md := renamedOrderMetaData()
		// Seeded deliberately: scanning an EMPTY subspace returns nothing no
		// matter what the predicate does, so the arm would hold under a
		// match-everything regression and pin nothing at all.
		seedOneOfEach(md)
		Expect(scanNames(md, "NoSuchType")).To(BeEmpty(),
			"An unresolvable name started matching records. Resolving the name for the\n"+
				"predicate must not turn a miss into a match-everything.")
	})
})
