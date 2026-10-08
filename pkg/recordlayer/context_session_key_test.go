package recordlayer

import (
	"context"
	"testing"

	"fdb.dev/gen"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"google.golang.org/protobuf/proto"
)

// Typed session keys are equal by name and never equal a string key; the two
// write-only keys share Java's name, so they are one entry.
func TestContextSessionKeys(t *testing.T) {
	t.Parallel()
	rc := &FDBRecordContext{}
	if got := GetInSession(rc, ReadableIndexesUpdated); got != nil {
		t.Fatalf("absent key read %v, want nil", got)
	}
	rc.addToSessionSet(WriteOnlyIndexesUpdated, "a")
	rc.addToSessionSet(WriteOnlyWithQueueIndexesUpdated, "q")
	want := map[string]struct{}{"a": {}, "q": {}}
	for _, k := range []ContextSessionKey[map[string]struct{}]{WriteOnlyIndexesUpdated, WriteOnlyWithQueueIndexesUpdated} {
		if got := GetInSession(rc, k); len(got) != 2 || got["a"] != want["a"] || got["q"] != want["q"] {
			t.Errorf("%s = %v, want both write-only sets' indexes %v (Java's shared name)", k.Name(), got, want)
		}
	}
	rc.PutSession("writeOnlyIndexesUpdated", "a string key")
	if got := GetInSession(rc, WriteOnlyIndexesUpdated); len(got) != 2 {
		t.Errorf("a string key replaced the typed key's set: %v", got)
	}
	if rc.Session("writeOnlyIndexesUpdated") != "a string key" {
		t.Error("the typed key replaced the string key")
	}
	got := GetInSession(rc, WriteOnlyIndexesUpdated)
	got["mutated"] = struct{}{}
	if _, leaked := GetInSession(rc, WriteOnlyIndexesUpdated)["mutated"]; leaked {
		t.Error("GetInSession returned the session's own set")
	}
}

var _ = Describe("Index-update session sets", func() {
	ctx := context.Background()
	// Java's FDBRecordStore.updateSecondaryIndexes (#4289): a record write
	// records each index it updated in the context's set for that index's
	// state; a DISABLED index is not updated and is in none.
	It("records the indexes a write updated by state", func() {
		index := NewVectorIndex("queued", KeyWithValue(Concat(Field("quantity"), Field("price")), 1), 1)
		builder := baseBuilder()
		builder.GetRecordType("Order").SetPrimaryKey(Concat(Field("quantity"), Field("order_id")))
		builder.AddIndex("Order", index)
		builder.AddIndex("Order", NewIndex("readable", Field("quantity")))
		builder.AddIndex("Order", NewIndex("writeonly", Field("price")))
		builder.AddIndex("Order", NewIndex("disabled", Field("order_id")))
		md, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())
		root := specSubspace()
		_, err = sharedDB.Run(ctx, func(rc *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().SetContext(rc).SetMetaDataProvider(md).SetSubspace(root).SetFormatVersion(15).Create()
			if err != nil {
				return nil, err
			}
			if _, err := store.MarkIndexWriteOnlyWithQueue(index.Name); err != nil {
				return nil, err
			}
			if _, err := store.MarkIndexWriteOnly("writeonly"); err != nil {
				return nil, err
			}
			_, err = store.MarkIndexDisabled("disabled")
			return nil, err
		})
		Expect(err).NotTo(HaveOccurred())
		_, err = sharedDB.Run(ctx, func(rc *FDBRecordContext) (any, error) {
			Expect(GetInSession(rc, ReadableIndexesUpdated)).To(BeNil())
			store, err := NewStoreBuilder().SetContext(rc).SetMetaDataProvider(md).SetSubspace(root).Open()
			if err != nil {
				return nil, err
			}
			if _, err := store.SaveRecord(&gen.Order{OrderId: proto.Int64(1), Quantity: proto.Int32(2), Price: proto.Int32(3)}); err != nil {
				return nil, err
			}
			Expect(GetInSession(rc, ReadableIndexesUpdated)).To(Equal(map[string]struct{}{"readable": {}}))
			writeOnly := map[string]struct{}{"writeonly": {}, "queued": {}}
			Expect(GetInSession(rc, WriteOnlyIndexesUpdated)).To(Equal(writeOnly))
			Expect(GetInSession(rc, WriteOnlyWithQueueIndexesUpdated)).To(Equal(writeOnly))
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})
})
