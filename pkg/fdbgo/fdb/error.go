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

// errNotSupported is returned by stubs for operations not yet implemented
// in the pure Go client.
var (
	errNotSupported         = Error{Code: 2051}
	errTenantNotFound       = Error{Code: 2131}
	errTenantExists         = Error{Code: 2132}
	errTenantNotEmpty       = Error{Code: 2133}
	errTenantInvalid        = Error{Code: 2134}
	errTenantPrefixConflict = Error{Code: 2135}
	errTenantsDisabled      = Error{Code: 2136}
	errClusterNoCapacity    = Error{Code: 2141}
)

// FDB error code descriptions. Source: flow/include/flow/error_definitions.h.
var errorDescriptions = map[int]string{
	// Internal errors
	4:    "operation_failed",
	1004: "key_outside_legal_range",
	1006: "all_alternatives_failed",
	1007: "transaction_too_old",
	1009: "future_version",
	1020: "not_committed",
	1021: "commit_unknown_result",
	1025: "transaction_cancelled",
	1031: "transaction_timed_out",
	1032: "too_many_watches",
	1034: "watches_disabled",
	1036: "accessed_unreadable",
	1037: "process_behind",
	1038: "database_locked",
	1039: "cluster_version_changed",
	1042: "proxy_memory_limit_exceeded",
	1051: "batch_transaction_throttled",
	1062: "wrong_shard_server",
	1078: "grv_proxy_memory_limit_exceeded",
	1079: "blob_granule_request_failed",
	1213: "tag_throttled",
	1223: "proxy_tag_throttled",
	// Client errors
	2000: "client_invalid_operation",
	2002: "commit_read_incomplete",
	2004: "key_outside_legal_range",
	2005: "inverted_range",
	2006: "invalid_option_value",
	2007: "invalid_option",
	2008: "network_not_setup",
	2009: "network_already_setup",
	2015: "used_during_commit",
	2017: "used_during_commit",
	2018: "invalid_mutation_type",
	2051: "operation_not_supported",
	2101: "transaction_too_large",
	// Tenant errors
	2131: "tenant_not_found",
	2132: "tenant_already_exists",
	2133: "tenant_not_empty",
	2134: "invalid_tenant_name",
	2135: "tenant_prefix_allocator_conflict",
	2136: "tenants_disabled",
	2141: "cluster_no_capacity",
	// API version errors
	2200: "api_version_unset",
	2201: "api_version_not_supported",
}
