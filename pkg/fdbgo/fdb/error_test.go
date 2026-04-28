package fdb_test

import (
	"strings"
	"testing"

	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/fdb"
	. "github.com/onsi/gomega"
)

// TestIsRetryable_KnownRetryable verifies that codes documented as retryable
// by C++ fdb_error_predicate(RETRYABLE, ...) all return true.
func TestIsRetryable_KnownRetryable(t *testing.T) {
	t.Parallel()
	g := NewWithT(t)

	// MAYBE_COMMITTED set.
	g.Expect(fdb.IsRetryable(1021)).To(BeTrue(), "1021 commit_unknown_result is MAYBE_COMMITTED")
	g.Expect(fdb.IsRetryable(1039)).To(BeTrue(), "1039 cluster_version_changed is MAYBE_COMMITTED")

	// RETRYABLE_NOT_COMMITTED set.
	g.Expect(fdb.IsRetryable(1007)).To(BeTrue(), "1007 transaction_too_old is RETRYABLE_NOT_COMMITTED")
	g.Expect(fdb.IsRetryable(1009)).To(BeTrue(), "1009 future_version is RETRYABLE_NOT_COMMITTED")
	g.Expect(fdb.IsRetryable(1020)).To(BeTrue(), "1020 not_committed is RETRYABLE_NOT_COMMITTED")
	g.Expect(fdb.IsRetryable(1037)).To(BeTrue(), "1037 process_behind is RETRYABLE_NOT_COMMITTED")
	g.Expect(fdb.IsRetryable(1038)).To(BeTrue(), "1038 database_locked is RETRYABLE_NOT_COMMITTED")
	g.Expect(fdb.IsRetryable(1042)).To(BeTrue(), "1042 commit_proxy_memory_limit_exceeded")
	g.Expect(fdb.IsRetryable(1051)).To(BeTrue(), "1051 batch_transaction_throttled")
	g.Expect(fdb.IsRetryable(1078)).To(BeTrue(), "1078 grv_proxy_memory_limit_exceeded")
	g.Expect(fdb.IsRetryable(1213)).To(BeTrue(), "1213 tag_throttled")
	g.Expect(fdb.IsRetryable(1223)).To(BeTrue(), "1223 proxy_tag_throttled")
}

// TestIsRetryable_KnownNonRetryable verifies that codes that look retryable
// (in the 1xxx Normal failures range) but are not in fdb_error_predicate's
// list, plus a sampling of 2xxx client errors, all return false.
func TestIsRetryable_KnownNonRetryable(t *testing.T) {
	t.Parallel()
	g := NewWithT(t)

	// transaction_timed_out is explicitly NOT retryable (C++ Transaction::onError
	// surfaces it without resetting). Critical guarantee for users who set a timeout.
	g.Expect(fdb.IsRetryable(1031)).To(BeFalse(), "1031 transaction_timed_out NEVER retryable")

	// transaction_cancelled — definitive failure, not retryable.
	g.Expect(fdb.IsRetryable(1025)).To(BeFalse(), "1025 transaction_cancelled not retryable")

	// 2xxx client errors are never retryable.
	g.Expect(fdb.IsRetryable(2101)).To(BeFalse(), "2101 transaction_too_large not retryable")
	g.Expect(fdb.IsRetryable(2018)).To(BeFalse(), "2018 invalid_mutation_type not retryable")
	g.Expect(fdb.IsRetryable(2102)).To(BeFalse(), "2102 key_too_large not retryable")
	g.Expect(fdb.IsRetryable(2131)).To(BeFalse(), "2131 tenant_not_found not retryable")

	// 4xxx internal-error / 6xxx authorization codes are never retryable.
	g.Expect(fdb.IsRetryable(4000)).To(BeFalse(), "4000 unknown_error not retryable")
	g.Expect(fdb.IsRetryable(6000)).To(BeFalse(), "6000 permission_denied not retryable")
}

// TestIsRetryable_UnknownCode verifies the function defaults to non-retryable
// for codes not in the table — fail-safe default.
func TestIsRetryable_UnknownCode(t *testing.T) {
	t.Parallel()
	g := NewWithT(t)

	g.Expect(fdb.IsRetryable(0)).To(BeFalse(), "0 success is not retryable (and shouldn't be passed in anyway)")
	g.Expect(fdb.IsRetryable(99999)).To(BeFalse(), "unknown future code defaults to non-retryable")
	g.Expect(fdb.IsRetryable(-1)).To(BeFalse(), "negative codes are not retryable")
}

// TestError_Retryable verifies the convenience method matches the
// package-level IsRetryable for representative codes.
func TestError_Retryable(t *testing.T) {
	t.Parallel()
	g := NewWithT(t)
	g.Expect((fdb.Error{Code: 1021}).Retryable()).To(BeTrue(), "1021 commit_unknown_result")
	g.Expect((fdb.Error{Code: 1031}).Retryable()).To(BeFalse(), "1031 transaction_timed_out NEVER retryable")
	g.Expect((fdb.Error{Code: 99999}).Retryable()).To(BeFalse(), "unknown code")
}

// TestErrorString_KnownCode verifies the Error() formatting of a couple of
// representative codes.
func TestErrorString_KnownCode(t *testing.T) {
	t.Parallel()
	g := NewWithT(t)

	// Known code includes the snake_case description and the numeric code.
	got := (fdb.Error{Code: 1007}).Error()
	g.Expect(got).To(ContainSubstring("transaction_too_old"))
	g.Expect(got).To(ContainSubstring("1007"))

	// Unknown code falls back to a generic format.
	got = (fdb.Error{Code: 99999}).Error()
	g.Expect(strings.Contains(got, "99999")).To(BeTrue(), "unknown code surfaces the numeric value")
}
