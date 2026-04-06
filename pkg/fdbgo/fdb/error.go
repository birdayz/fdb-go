package fdb

import "fmt"

// Error represents a low-level error returned by FoundationDB.
// Compare the Code field against FDB error codes, or pass to
// Transaction.OnError to determine if the error is retryable.
type Error struct {
	Code int
}

func (e Error) Error() string {
	if desc, ok := errorDescriptions[e.Code]; ok {
		return fmt.Sprintf("%s (%d)", desc, e.Code)
	}
	return fmt.Sprintf("FoundationDB error: %d", e.Code)
}

// Common FDB error code descriptions.
var errorDescriptions = map[int]string{
	1007: "transaction_too_old",
	1009: "future_version",
	1020: "not_committed",
	1021: "commit_unknown_result",
	1031: "transaction_timed_out",
	1037: "process_behind",
	1038: "database_locked",
	1042: "proxy_memory_limit_exceeded",
	1051: "batch_transaction_throttled",
	1062: "wrong_shard_server",
	2000: "operation_failed",
	2051: "operation_not_supported",
	2005: "inverted_range",
	2015: "used_during_commit",
	2101: "transaction_too_large",
	2131: "tenant_not_found",
	2200: "api_version_unset",
	2201: "api_version_not_supported",
}
