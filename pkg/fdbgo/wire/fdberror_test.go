package wire

import (
	"strings"
	"testing"
)

// (The TestFDBError_Retryable_* tests were removed in RFC-105 along with the dead
// wire.FDBError.Retryable() method. Retry classification is pinned in the
// client/fdb layers: fdb.IsRetryable (fdb_error_predicate) and
// client.onErrorRetryable (Transaction.onError), each C++-pinned exhaustively.)

// TestFDBError_Description_LatentBugFixes pins the description fixes
// from the wire-side fdbErrorDescriptions cleanup. Five latent bugs
// found and fixed dayshift-58:
//
//   - 1006 = "all_alternatives_failed" (added; previously missing).
//   - 1042 = "commit_proxy_memory_limit_exceeded" (was incorrectly
//     "proxy_memory_limit_exceeded" — missing 'commit_' prefix).
//   - 1062 = "change_feed_cancelled" (was incorrectly
//     "wrong_shard_server" — that's actually code 1001).
//   - 1200 = "all_proxies_unreachable" (Go-internal override; was
//     incorrectly "all_alternatives_failed" — that's actually 1006).
//   - 2015 = "future_not_set" (was incorrectly "used_during_commit").
//   - 2017 = "used_during_commit" (added; the real used_during_commit
//     code).
func TestFDBError_Description_LatentBugFixes(t *testing.T) {
	t.Parallel()
	cases := []struct {
		code     int
		wantDesc string
	}{
		{1006, "all_alternatives_failed"},
		{1042, "commit_proxy_memory_limit_exceeded"},
		{1062, "change_feed_cancelled"},
		{1200, "all_proxies_unreachable"},
		{2015, "future_not_set"},
		{2017, "used_during_commit"},
	}
	for _, tc := range cases {
		e := &FDBError{Code: tc.code}
		got := e.Error()
		if !strings.Contains(got, tc.wantDesc) {
			t.Errorf("FDBError{Code:%d}.Error() = %q; want substring %q", tc.code, got, tc.wantDesc)
		}
	}
}
