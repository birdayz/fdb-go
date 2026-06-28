package recordlayer

import (
	"context"
	"errors"
	"fmt"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// The multi-rebalancer lease-skip guard (spfresh_rebalancer.go:227,
// spfresh_split.go:51, merge/npa/csplit) is `errors.Is(err, errSPFreshLeaseHeld)`
// on an error that has travelled the full transact stack: a sentinel returned by
// spfreshTaskClaim, %w-wrapped by the caller, through spfreshRun -> db.Run ->
// facade TransactCtx (unconvertError/convertError) -> client Transact (OnError).
// The guard is what keeps two concurrent rebalancers from failing the run when
// one loses a benign lease race (the "two concurrent rebalancers ... never orphan
// entries" spec). If any of those layers ever rebuilt the error and dropped the
// %w chain, errors.Is would silently return false, the guard would stop skipping,
// and that spec would flake red again — exactly the failure mode that motivated
// 0b3263b2ff. These deterministic checks pin the chain-survival invariant so a
// regression fails here, loudly, instead of only as a probabilistic concurrency
// flake.
var _ = Describe("SPFresh lease-held error chain", func() {
	It("errors.Is(errSPFreshLeaseHeld) survives a db.Run round-trip", func() {
		wrapped := fmt.Errorf("spfresh split: claim task for SEALED centroid %d: %w", int64(65541), errSPFreshLeaseHeld)
		_, err := sharedDB.Run(context.Background(), func(rtx *FDBRecordContext) (any, error) {
			return nil, wrapped
		})
		Expect(err).To(HaveOccurred())
		Expect(errors.Is(err, errSPFreshLeaseHeld)).To(BeTrue(),
			"db.Run dropped the errSPFreshLeaseHeld %w chain — the rebalancer lease-skip guard would silently fail: got %T = %v", err, err)
	})

	It("errors.Is(errSPFreshLeaseHeld) survives the spfreshRun helper", func() {
		wrapped := fmt.Errorf("spfresh split: claim task: %w", errSPFreshLeaseHeld)
		err := spfreshRun(context.Background(), sharedDB, func(rtx *FDBRecordContext) error {
			return wrapped
		})
		Expect(err).To(HaveOccurred())
		Expect(errors.Is(err, errSPFreshLeaseHeld)).To(BeTrue(),
			"spfreshRun dropped the errSPFreshLeaseHeld %w chain: got %T = %v", err, err)
	})
})
