package fdb

import "fmt"

// Error represents a low-level error returned by FoundationDB.
// Compare the Code field against FDB error codes, pass to
// Transaction.OnError to determine if the error is retryable, or
// call IsRetryable(code) directly for the canonical
// FDB_ERROR_PREDICATE_RETRYABLE classification.
type Error struct {
	Code int
}

func (e Error) Error() string {
	if desc, ok := errorDescriptions[e.Code]; ok {
		return fmt.Sprintf("%s (%d)", desc, e.Code)
	}
	return fmt.Sprintf("FoundationDB error: %d", e.Code)
}

// Retryable reports whether this error is retryable per the canonical
// FDB_ERROR_PREDICATE_RETRYABLE classification. Convenience method for
// the package-level IsRetryable function — symmetrical with the
// wire-side FDBError.Retryable in pkg/fdbgo/wire.
func (e Error) Retryable() bool {
	return IsRetryable(e.Code)
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

// FDB error code descriptions. Source: flow/include/flow/error_definitions.h
// (FoundationDB upstream, snake_case `name` from each ERROR(name, code, desc) macro).
// Ordering and section grouping mirror the C++ source.
var errorDescriptions = map[int]string{
	// Special
	0: "success",
	1: "end_of_stream",

	// 1xxx Normal failures
	1000: "operation_failed",
	1001: "wrong_shard_server",
	1002: "operation_obsolete",
	1003: "cold_cache_server",
	1004: "timed_out",
	1005: "coordinated_state_conflict",
	1006: "all_alternatives_failed",
	1007: "transaction_too_old",
	1008: "no_more_servers",
	1009: "future_version",
	1010: "movekeys_conflict",
	1011: "tlog_stopped",
	1012: "server_request_queue_full",
	1020: "not_committed",
	1021: "commit_unknown_result",
	1022: "commit_unknown_result_fatal",
	1025: "transaction_cancelled",
	1026: "connection_failed",
	1027: "coordinators_changed",
	1028: "new_coordinators_timed_out",
	1029: "watch_cancelled",
	1030: "request_maybe_delivered",
	1031: "transaction_timed_out",
	1032: "too_many_watches",
	1033: "locality_information_unavailable",
	1034: "watches_disabled",
	1035: "default_error_or",
	1036: "accessed_unreadable",
	1037: "process_behind",
	1038: "database_locked",
	1039: "cluster_version_changed",
	1040: "external_client_already_loaded",
	1041: "lookup_failed",
	1042: "commit_proxy_memory_limit_exceeded",
	1043: "shutdown_in_progress",
	1044: "serialization_failed",
	1048: "connection_unreferenced",
	1049: "connection_idle",
	1050: "disk_adapter_reset",
	1051: "batch_transaction_throttled",
	1052: "dd_cancelled",
	1053: "dd_not_found",
	1054: "wrong_connection_file",
	1055: "version_already_compacted",
	1056: "local_config_changed",
	1057: "failed_to_reach_quorum",
	1058: "unsupported_format_version",
	1059: "unknown_change_feed",
	1060: "change_feed_not_registered",
	1061: "granule_assignment_conflict",
	1062: "change_feed_cancelled",
	1063: "blob_granule_file_load_error",
	1064: "blob_granule_transaction_too_old",
	1065: "blob_manager_replaced",
	1066: "change_feed_popped",
	1067: "remote_kvs_cancelled",
	1068: "page_header_wrong_page_id",
	1069: "page_header_checksum_failed",
	1070: "page_header_version_not_supported",
	1071: "page_encoding_not_supported",
	1072: "page_decoding_failed",
	1073: "unexpected_encoding_type",
	1074: "encryption_key_not_found",
	1075: "data_move_cancelled",
	1076: "data_move_dest_team_not_found",
	1077: "blob_worker_full",
	1078: "grv_proxy_memory_limit_exceeded",
	1079: "blob_granule_request_failed",
	1080: "storage_too_many_feed_streams",
	1081: "storage_engine_not_initialized",
	1082: "unknown_storage_engine",
	1083: "duplicate_snapshot_request",
	1084: "dd_config_changed",
	1085: "consistency_check_urgent_task_failed",
	1086: "data_move_conflict",
	1087: "consistency_check_urgent_duplicate_request",

	// 11xx Promise/operation failures
	1100: "broken_promise",
	1101: "operation_cancelled",
	1102: "future_released",
	1103: "connection_leaked",
	1104: "never_reply",
	1105: "retry",

	// 12xx Recovery / cluster control failures
	1200: "recruitment_failed",
	1201: "move_to_removed_server",
	1202: "worker_removed",
	1203: "cluster_recovery_failed",
	1204: "master_max_versions_in_flight",
	1205: "tlog_failed",
	1206: "worker_recovery_failed",
	1207: "please_reboot",
	1208: "please_reboot_delete",
	1209: "commit_proxy_failed",
	1210: "resolver_failed",
	1211: "server_overloaded",
	1212: "backup_worker_failed",
	1213: "tag_throttled",
	1214: "grv_proxy_failed",
	1215: "dd_tracker_cancelled",
	1216: "failed_to_progress",
	1217: "invalid_cluster_id",
	1218: "restart_cluster_controller",
	1219: "please_reboot_kv_store",
	1220: "incompatible_software_version",
	1221: "audit_storage_failed",
	1222: "audit_storage_exceeded_request_limit",
	1223: "proxy_tag_throttled",
	1224: "key_value_store_deadline_exceeded",
	1225: "storage_quota_exceeded",
	1226: "audit_storage_error",
	1227: "master_failed",
	1228: "test_failed",
	1229: "retry_clean_up_datamove_tombstone_added",
	1230: "persist_new_audit_metadata_error",
	1231: "cancel_audit_storage_failed",
	1232: "audit_storage_cancelled",
	1233: "location_metadata_corruption",
	1234: "audit_storage_task_outdated",

	// 15xx Platform errors
	1500: "platform_error",
	1501: "large_alloc_failed",
	1502: "performance_counter_error",
	1503: "bad_allocator",
	1510: "io_error",
	1511: "file_not_found",
	1512: "bind_failed",
	1513: "file_not_readable",
	1514: "file_not_writable",
	1515: "no_cluster_file_found",
	1516: "file_too_large",
	1517: "non_sequential_op",
	1518: "http_bad_response",
	1519: "http_not_accepted",
	1520: "checksum_failed",
	1521: "io_timeout",
	1522: "file_corrupt",
	1523: "http_request_failed",
	1524: "http_auth_failed",
	1525: "http_bad_request_id",
	1526: "rest_invalid_uri",
	1527: "rest_invalid_rest_client_knob",
	1528: "rest_connectpool_key_not_found",
	1529: "lock_file_failure",
	1530: "rest_unsupported_protocol",
	1531: "rest_malformed_response",
	1532: "rest_max_base_cipher_len",

	// 20xx Client errors
	2000: "client_invalid_operation",
	2002: "commit_read_incomplete",
	2003: "test_specification_invalid",
	2004: "key_outside_legal_range",
	2005: "inverted_range",
	2006: "invalid_option_value",
	2007: "invalid_option",
	2008: "network_not_setup",
	2009: "network_already_setup",
	2010: "read_version_already_set",
	2011: "version_invalid",
	2012: "range_limits_invalid",
	2013: "invalid_database_name",
	2014: "attribute_not_found",
	2015: "future_not_set",
	2016: "future_not_error",
	2017: "used_during_commit",
	2018: "invalid_mutation_type",
	2019: "attribute_too_large",
	2020: "transaction_invalid_version",
	2021: "no_commit_version",
	2022: "environment_variable_network_option_failed",
	2023: "transaction_read_only",
	2024: "invalid_cache_eviction_policy",
	2025: "network_cannot_be_restarted",
	2026: "blocked_from_network_thread",
	2027: "invalid_config_db_range_read",
	2028: "invalid_config_db_key",
	2029: "invalid_config_path",
	2030: "mapper_bad_index",
	2031: "mapper_no_such_key",
	2032: "mapper_bad_range_decriptor",
	2033: "quick_get_key_values_has_more",
	2034: "quick_get_value_miss",
	2035: "quick_get_key_values_miss",
	2036: "blob_granule_no_ryw",
	2037: "blob_granule_not_materialized",
	2038: "get_mapped_key_values_has_more",
	2039: "get_mapped_range_reads_your_writes",
	2040: "checkpoint_not_found",
	2041: "key_not_tuple",
	2042: "value_not_tuple",
	2043: "mapper_not_tuple",
	2044: "invalid_checkpoint_format",
	2045: "invalid_throttle_quota_value",
	2046: "failed_to_create_checkpoint",
	2047: "failed_to_restore_checkpoint",
	2048: "failed_to_create_checkpoint_shard_metadata",
	2049: "address_parse_error",

	// 21xx Connection / API / library errors
	2100: "incompatible_protocol_version",
	2101: "transaction_too_large",
	2102: "key_too_large",
	2103: "value_too_large",
	2104: "connection_string_invalid",
	2105: "address_in_use",
	2106: "invalid_local_address",
	2107: "tls_error",
	2108: "unsupported_operation",
	2109: "too_many_tags",
	2110: "tag_too_long",
	2111: "too_many_tag_throttles",
	2112: "special_keys_cross_module_read",
	2113: "special_keys_no_module_found",
	2114: "special_keys_write_disabled",
	2115: "special_keys_no_write_module_found",
	2116: "special_keys_cross_module_clear",
	2117: "special_keys_api_failure",
	2118: "client_lib_invalid_metadata",
	2119: "client_lib_already_exists",
	2120: "client_lib_not_found",
	2121: "client_lib_not_available",
	2122: "client_lib_invalid_binary",
	2123: "no_external_client_provided",
	2124: "all_external_clients_failed",
	2125: "incompatible_client",

	// 2130-2159 Tenant errors
	2130: "tenant_name_required",
	2131: "tenant_not_found",
	2132: "tenant_already_exists",
	2133: "tenant_not_empty",
	2134: "invalid_tenant_name",
	2135: "tenant_prefix_allocator_conflict",
	2136: "tenants_disabled",
	2138: "illegal_tenant_access",
	2139: "invalid_tenant_group_name",
	2140: "invalid_tenant_configuration",
	2141: "cluster_no_capacity",
	2142: "tenant_removed",
	2143: "invalid_tenant_state",
	2144: "tenant_locked",

	// 2160-2199 Metacluster errors
	2160: "invalid_cluster_name",
	2161: "invalid_metacluster_operation",
	2162: "cluster_already_exists",
	2163: "cluster_not_found",
	2164: "cluster_not_empty",
	2165: "cluster_already_registered",
	2166: "metacluster_no_capacity",
	2167: "management_cluster_invalid_access",
	2168: "tenant_creation_permanently_failed",
	2169: "cluster_removed",
	2170: "cluster_restoring",
	2171: "invalid_data_cluster",
	2172: "metacluster_mismatch",
	2173: "conflicting_restore",
	2174: "invalid_metacluster_configuration",

	// 22xx Bindings / API errors
	2200: "api_version_unset",
	2201: "api_version_already_set",
	2202: "api_version_invalid",
	2203: "api_version_not_supported",
	2204: "api_function_missing",
	2210: "exact_mode_without_limits",

	// 2250-2299 Tuple / directory errors
	2250: "invalid_tuple_data_type",
	2251: "invalid_tuple_index",
	2252: "key_not_in_subspace",
	2253: "manual_prefixes_not_enabled",
	2254: "prefix_in_partition",
	2255: "cannot_open_root_directory",
	2256: "directory_already_exists",
	2257: "directory_does_not_exist",
	2258: "parent_directory_does_not_exist",
	2259: "mismatched_layer",
	2260: "invalid_directory_layer_metadata",
	2261: "cannot_move_directory_between_partitions",
	2262: "cannot_use_partition_as_subspace",
	2263: "incompatible_directory_version",
	2264: "directory_prefix_not_empty",
	2265: "directory_prefix_in_use",
	2266: "invalid_destination_directory",
	2267: "cannot_modify_root_directory",
	2268: "invalid_uuid_size",
	2269: "invalid_versionstamp_size",

	// 23xx Backup and restore errors
	2300: "backup_error",
	2301: "restore_error",
	2311: "backup_duplicate",
	2312: "backup_unneeded",
	2313: "backup_bad_block_size",
	2314: "backup_invalid_url",
	2315: "backup_invalid_info",
	2316: "backup_cannot_expire",
	2317: "backup_auth_missing",
	2318: "backup_auth_unreadable",
	2319: "backup_does_not_exist",
	2320: "backup_not_filterable_with_key_ranges",
	2321: "backup_not_overlapped_with_keys_filter",
	2361: "restore_invalid_version",
	2362: "restore_corrupted_data",
	2363: "restore_missing_data",
	2364: "restore_duplicate_tag",
	2365: "restore_unknown_tag",
	2366: "restore_unknown_file_type",
	2367: "restore_unsupported_file_version",
	2368: "restore_bad_read",
	2369: "restore_corrupted_data_padding",
	2370: "restore_destination_not_empty",
	2371: "restore_duplicate_uid",
	2381: "task_invalid_version",
	2382: "task_interrupted",
	2383: "invalid_encryption_key_file",
	2384: "blob_restore_missing_logs",
	2385: "blob_restore_corrupted_logs",
	2386: "blob_restore_invalid_manifest_url",
	2387: "blob_restore_corrupted_manifest",
	2388: "blob_restore_missing_manifest",
	2400: "key_not_found",
	2401: "json_malformed",
	2402: "json_eof_expected",

	// 25xx Disk snapshot backup errors
	2500: "snap_disable_tlog_pop_failed",
	2501: "snap_storage_failed",
	2502: "snap_tlog_failed",
	2503: "snap_coord_failed",
	2504: "snap_enable_tlog_pop_failed",
	2505: "snap_path_not_whitelisted",
	2506: "snap_not_fully_recovered_unsupported",
	2507: "snap_log_anti_quorum_unsupported",
	2508: "snap_with_recovery_unsupported",
	2509: "snap_invalid_uid_string",

	// 27xx Encryption errors
	2700: "encrypt_ops_error",
	2701: "encrypt_header_metadata_mismatch",
	2702: "encrypt_key_not_found",
	2703: "encrypt_key_ttl_expired",
	2704: "encrypt_header_authtoken_mismatch",
	2705: "encrypt_update_cipher",
	2706: "encrypt_invalid_id",
	2707: "encrypt_keys_fetch_failed",
	2708: "encrypt_invalid_kms_config",
	2709: "encrypt_unsupported",
	2710: "encrypt_mode_mismatch",
	2711: "encrypt_key_check_value_mismatch",
	2712: "encrypt_max_base_cipher_len",

	// 4xxx Internal errors (bugs)
	4000: "unknown_error",
	4100: "internal_error",
	4200: "not_implemented",

	// 6xxx Authorization / authentication errors
	6000: "permission_denied",
	6001: "unauthorized_attempt",
	6002: "digital_signature_ops_error",
	6003: "authorization_token_verify_failed",
	6004: "pkey_decode_error",
	6005: "pkey_encode_error",
}

// IsRetryable reports whether the given FDB error code is retryable per
// FDB's `fdb_error_predicate(FDB_ERROR_PREDICATE_RETRYABLE, code)`.
//
// RETRYABLE = MAYBE_COMMITTED ∪ RETRYABLE_NOT_COMMITTED.
//
// Source of truth: bindings/c/fdb_c.cpp (function fdb_error_predicate).
// The set is intentionally small — most 1xxx codes are NOT retryable, despite
// the surrounding range being "Normal failures". Anything 2xxx+ (client error,
// API misuse, tenant/cluster errors, tuple/directory errors, backup errors,
// auth errors, internal bugs) is non-retryable.
//
// NOTE: transaction_timed_out (1031) is explicitly NOT retryable (matches C++
// Transaction::onError, where a timed-out transaction propagates the error
// without resetting / retrying).
func IsRetryable(code int) bool {
	switch code {
	// MAYBE_COMMITTED (transaction may have committed; retry only if idempotent).
	case 1021, // commit_unknown_result
		1039: // cluster_version_changed
		return true
	// RETRYABLE_NOT_COMMITTED (definitely not committed; safe to retry).
	case 1007, // transaction_too_old
		1009, // future_version
		1020, // not_committed (mvcc conflict)
		1037, // process_behind
		1038, // database_locked
		1042, // commit_proxy_memory_limit_exceeded
		1051, // batch_transaction_throttled
		1078, // grv_proxy_memory_limit_exceeded
		1213, // tag_throttled
		1223: // proxy_tag_throttled
		return true
	}
	return false
}

// IsOnErrorRetryable reports whether fdb_transaction_on_error retries `code` (i.e. resets
// the transaction and backs off, rather than re-raising it terminal). This is a DIFFERENT,
// larger set than IsRetryable (the fdb_error_predicate RETRYABLE set): OnError additionally
// retries blob_granule_request_failed (1079) — retried by C++ Transaction::onError
// (NativeAPI.actor.cpp:7743-7768) but absent from the error-predicate set — plus the Go
// extensions the pure-Go client documents (cluster_version_changed 1039 via the MVC layer,
// the Go-internal 1200, and 7.4+ 1235/1242).
//
// MUST stay in sync with the pure-Go client's onErrorRetryable (client/commitpath.go:231),
// which is the same set; both trace to C++ Transaction::onError + the documented MVC/Go
// extensions. The duplication is forced: the cgo backend can't import client (fdb imports
// client, so client can't import fdb), and this is the predicate that decides whether a
// libfdb_c OnError has a backoff worth ctx-bounding. Verified by TestIsOnErrorRetryable.
func IsOnErrorRetryable(code int) bool {
	switch code {
	case 1007, // transaction_too_old
		1009, // future_version
		1020, // not_committed
		1021, // commit_unknown_result (MAYBE_COMMITTED)
		1037, // process_behind
		1038, // database_locked
		1039, // cluster_version_changed (MAYBE_COMMITTED; retried via the MVC layer)
		1042, // proxy_memory_limit_exceeded
		1051, // batch_transaction_throttled
		1078, // grv_proxy_memory_limit_exceeded
		1079, // blob_granule_request_failed (retried by Transaction::onError; NOT in IsRetryable)
		1200, // all_proxies_unreachable (Go-internal Layer-2)
		1213, // tag_throttled
		1223, // proxy_tag_throttled
		1235, // transaction_throttled_hot_shard (FDB 7.4+)
		1242: // transaction_rejected_range_locked (FDB 7.4+)
		return true
	}
	return false
}
