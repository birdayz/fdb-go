package recordlayer

import (
	"context"

	"fdb.dev/pkg/fdbgo/fdb/tuple"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("FDBRecordContext", func() {
	var ctx context.Context

	BeforeEach(func() {
		ctx = context.Background()
	})

	Describe("GetReadVersion / SetReadVersion", func() {
		It("returns a read version", func() {
			_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				rv, err := rtx.GetReadVersion()
				Expect(err).NotTo(HaveOccurred())
				Expect(rv).To(BeNumerically(">", 0))
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})

		It("uses a set read version", func() {
			// Get a valid read version first
			var readVersion int64
			_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				rv, err := rtx.GetReadVersion()
				if err != nil {
					return nil, err
				}
				readVersion = rv
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())

			// Use that read version in another transaction
			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				rtx.SetReadVersion(readVersion)
				rv, err := rtx.GetReadVersion()
				Expect(err).NotTo(HaveOccurred())
				Expect(rv).To(Equal(readVersion))
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})
	})

	Describe("Versionstamp conversion", func() {
		It("FromVersionstamp creates a complete version", func() {
			vs := tuple.Versionstamp{
				UserVersion: 42,
			}
			vs.TransactionVersion = [10]byte{0, 0, 0, 0, 0, 0, 0, 1, 0, 0}
			v := FromVersionstamp(vs)
			Expect(v.IsComplete()).To(BeTrue())
			Expect(v.GetLocalVersion()).To(Equal(42))
			dbv, err := v.GetDBVersion()
			Expect(err).NotTo(HaveOccurred())
			Expect(dbv).To(Equal(int64(1)))
		})

		It("ToVersionstamp round-trips", func() {
			vs := tuple.Versionstamp{
				UserVersion: 99,
			}
			vs.TransactionVersion = [10]byte{0, 0, 0, 0, 0, 0, 0, 7, 0, 3}
			v := FromVersionstamp(vs)
			roundTripped, err := v.ToVersionstamp()
			Expect(err).NotTo(HaveOccurred())
			Expect(roundTripped.TransactionVersion).To(Equal(vs.TransactionVersion))
			Expect(roundTripped.UserVersion).To(Equal(vs.UserVersion))
		})

		It("errors on incomplete ToVersionstamp", func() {
			v, err := IncompleteVersion(5)
			Expect(err).NotTo(HaveOccurred())
			_, err = v.ToVersionstamp()
			Expect(err).To(HaveOccurred())
		})
	})
})
