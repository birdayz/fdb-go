package recordlayer

import (
	"context"
	"time"

	"fdb.dev/gen"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// Java's FDBRecordStore.updateStoreHeaderAsync stamps last_update_time on every
// header update (FDBRecordStore.java:3086), and every header mutator goes
// through it: cacheability, header user fields, the lock state, the
// incarnation, the record-count state, the user version and the format
// version. Each mutator here runs in its own
// transaction after the previous one, so each must leave a later stamp.
var _ = Describe("Store header last_update_time", func() {
	ctx := context.Background()

	It("is stamped by every header update", func() {
		ks := specSubspace()
		builder := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
		builder.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
		builder.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
		builder.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))
		md, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())

		stampAfter := func(what string, mutate func(*FDBRecordStore) error) uint64 {
			// The stamp is milliseconds; two updates inside one would tie.
			time.Sleep(3 * time.Millisecond)
			var stamp uint64
			_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
				if err != nil {
					return nil, err
				}
				if err := mutate(store); err != nil {
					return nil, err
				}
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred(), what)
			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).Open()
				if err != nil {
					return nil, err
				}
				stamp = store.GetStoreHeader().GetLastUpdateTime()
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred(), what)
			return stamp
		}

		last := stampAfter("create", func(*FDBRecordStore) error { return nil })
		Expect(last).NotTo(BeZero())
		for _, step := range []struct {
			what   string
			mutate func(*FDBRecordStore) error
		}{
			{"SetStateCacheability", func(s *FDBRecordStore) error {
				changed, err := s.SetStateCacheability(true)
				Expect(changed).To(BeTrue())
				return err
			}},
			{"SetHeaderUserField", func(s *FDBRecordStore) error { return s.SetHeaderUserField("k", []byte("v")) }},
			{"ClearHeaderUserField", func(s *FDBRecordStore) error { return s.ClearHeaderUserField("k") }},
			{"SetStoreLockState", func(s *FDBRecordStore) error {
				return s.SetStoreLockState(gen.DataStoreInfo_StoreLockState_FORBID_RECORD_UPDATE, "stamp")
			}},
			{"ClearStoreLockState", func(s *FDBRecordStore) error { return s.ClearStoreLockState() }},
			{"UpdateIncarnation", func(s *FDBRecordStore) error {
				return s.UpdateIncarnation(func(current int32) int32 { return current + 1 })
			}},
			{"UpdateRecordCountState", func(s *FDBRecordStore) error {
				return s.UpdateRecordCountState(gen.DataStoreInfo_WRITE_ONLY)
			}},
			{"SetUserVersion", func(s *FDBRecordStore) error { return s.SetUserVersion(s.GetUserVersion() + 1) }},
			// The same format version again: a header update all the same.
			{"SetFormatVersion", func(s *FDBRecordStore) error {
				return s.SetFormatVersion(s.GetStoreHeader().GetFormatVersion())
			}},
		} {
			stamp := stampAfter(step.what, step.mutate)
			Expect(stamp).To(BeNumerically(">", last), "%s left last_update_time at %d", step.what, stamp)
			last = stamp
		}
	})
})
