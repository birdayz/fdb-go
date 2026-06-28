package recordlayer

import (
	"context"
	"errors"

	"fdb.dev/gen"
	"fdb.dev/pkg/fdbgo/fdb"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// Regression for a store-open retry bug: a retryable FDB read error surfacing
// while opening a store (reading the store-info header) MUST stay retryable
// through the Transact loop.
//
// loadRecordStoreState (store_state_cache.go) and checkStoreExists
// (store_builder.go) used to wrap the raw FDB read error with %v, which
// flattens the fdb.Error/*wire.FDBError type down to a plain string. Both the
// facade's unconvertError and the client's OnError decide retryability via
// errors.As; a %v-flattened error matches neither, so a transient
// future_version / transaction_too_old / process_behind during store-open was
// classified as FATAL and failed the caller's transaction instead of being
// retried. (Surfaced under heavy concurrent ingest: future_version (1009) from
// a storage server lagging the logs killed the whole build.)
//
// We induce a real future_version (1009) by setting an absurd read version
// (> 10^15) on the transaction — the pure-Go client rejects such versions
// client-side in grv.validateVersion. Poisoning only the first attempt lets the
// retry (after reset() clears the user-set version) take a fresh GRV and
// succeed, so the test terminates.
var _ = Describe("StoreOpen_RetryableReadError", func() {
	ctx := context.Background()

	metaBuilder := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
	metaBuilder.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
	metaBuilder.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
	metaBuilder.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))
	metaData, _ := metaBuilder.Build()

	// > 10^15 → grv.validateVersion returns future_version (1009) client-side.
	const absurdReadVersion = int64(2_000_000_000_000_000)

	It("retries a transient future_version during store-open instead of failing", func() {
		ks := specSubspace()

		// Pre-create the store so the retried open reads a real header.
		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			_, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(metaData).SetSubspace(ks).
				CreateOrOpen()
			return nil, err
		})
		Expect(err).NotTo(HaveOccurred())

		attempts := 0
		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			attempts++
			if attempts == 1 {
				// Poison ONLY the first attempt with a real client-side future_version.
				rtx.Transaction().SetReadVersion(absurdReadVersion)
			}
			_, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(metaData).SetSubspace(ks).
				CreateOrOpen()
			return nil, err
		})

		Expect(err).NotTo(HaveOccurred(), "future_version during store-open must be retried, not fatal")
		Expect(attempts).To(BeNumerically(">=", 2), "first attempt's future_version should have forced a retry")
	})

	It("keeps the fdb.Error type when wrapping a failed store-open read", func() {
		ks := specSubspace()

		// Poison only the FIRST attempt, capture the open error it produced, and
		// RETURN it so the loop retries cleanly. Swallowing it (return nil, nil)
		// is not an option: a failed read poisons the transaction's commit with
		// the same error (RFC-098 — C++ commit() waits on ryw->reading, so
		// libfdb_c behaves identically), and since 1009 is retryable the
		// swallow-then-commit shape retries the still-poisoning closure forever.
		var openErr error
		attempts := 0
		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			attempts++
			if attempts > 1 {
				return nil, nil // clean retry — proves the captured error was retryable
			}
			rtx.Transaction().SetReadVersion(absurdReadVersion)
			_, openErr = NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(metaData).SetSubspace(ks).
				CreateOrOpen()
			return nil, openErr
		})
		Expect(err).NotTo(HaveOccurred())
		Expect(attempts).To(BeNumerically(">=", 2), "the wrapped 1009 should have been classified retryable")
		Expect(openErr).To(HaveOccurred())

		var fe fdb.Error
		Expect(errors.As(openErr, &fe)).To(BeTrue(),
			"wrapped store-open error lost its fdb.Error type (would be classified non-retryable): %v", openErr)
		Expect(fe.Code).To(Equal(1009))
	})
})
