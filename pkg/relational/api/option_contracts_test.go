package api

import (
	"errors"
	"math"
	"testing"
)

// Every option has a contract, and each contract admits its default value.
func TestOptionContractsCoverEveryOption(t *testing.T) {
	t.Parallel()
	all := []OptionName{
		OptContinuation, OptIndexHint, OptMaxRows, OptMaxStatementMemoryBytes,
		OptRequiredMetadataTableVersion, OptTransactionTimeout, OptReplaceOnDuplicatePK,
		OptPlanCachePrimaryMaxEntries, OptPlanCacheSecondaryMaxEntries, OptPlanCacheTertiaryMaxEntries,
		OptPlanCachePrimaryTimeToLiveMillis, OptPlanCacheSecondaryTimeToLiveMillis, OptPlanCacheTertiaryTimeToLiveMillis,
		OptIndexFetchMethod, OptDisabledPlannerRules, OptDisablePlannerRewriting, OptPlanRightDeep,
		OptVectorIndexEnginePreference, OptIsolationLevelSnapshot, OptPlannerStatistics, OptLogQuery,
		OptLogSlowQueryThresholdMicros, OptExecutionTimeLimit, OptExecutionScannedBytesLimit,
		OptExecutionScannedRowsLimit, OptDryRun, OptCaseSensitiveIdentifiers, OptCurrentPlanHashMode,
		OptValidPlanHashModes, OptAsyncOperationsTimeoutMillis, OptEncryptWhenSerializing,
		OptEncryptionKeyStore, OptEncryptionKeyEntry, OptEncryptionKeyEntryList, OptEncryptionKeyPassword,
		OptCompressWhenSerializing, OptRestrictDDLToSessionDatabase, OptTransactionTags,
	}
	if len(all) != len(optionContracts) {
		t.Fatalf("%d option names, %d contracts", len(all), len(optionContracts))
	}
	for _, name := range all {
		if _, ok := optionContracts[name]; !ok {
			t.Errorf("option %s has no contract", name)
		}
	}
	for name, value := range DefaultOptionValues() {
		if err := ValidateOption(name, value); err != nil {
			t.Errorf("default %s = %#v refused: %v", name, value, err)
		}
	}
}

// Java's TypeContract and RangeContract outcomes, as INVALID_PARAMETER.
func TestValidateOption(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name  OptionName
		value any
		want  string // "" admits
	}{
		{OptDryRun, true, ""},
		{OptDryRun, "true", "Option DRY_RUN should be of type bool but is string"},
		{OptDryRun, nil, "Option DRY_RUN should not be null"},
		{OptMaxRows, 5, ""},
		{OptMaxRows, int64(5), ""},
		{OptMaxRows, -1, "Option MAX_ROWS should be in range [0, 2147483647] but is -1"},
		{OptMaxRows, int64(math.MaxInt32) + 1, "Option MAX_ROWS should be of type int but is int64"},
		{OptMaxRows, 1.5, "Option MAX_ROWS should be of type int but is float64"},
		{OptPlanCacheSecondaryMaxEntries, 0, "Option PLAN_CACHE_SECONDARY_MAX_ENTRIES should be in range [1, 2147483647] but is 0"},
		{OptPlanCachePrimaryTimeToLiveMillis, int64(9), "Option PLAN_CACHE_PRIMARY_TIME_TO_LIVE_MILLIS should be in range [10, 9223372036854775807] but is 9"},
		{OptTransactionTimeout, int64(-1), ""},
		{OptExecutionScannedBytesLimit, int64(math.MaxInt64), ""},
		{OptDisabledPlannerRules, []string{"A"}, ""},
		{OptDisabledPlannerRules, "A", "Option DISABLED_PLANNER_RULES should be of type []string but is string"},
		{OptEncryptionKeyPassword, nil, ""},
		{OptIndexFetchMethod, IndexFetchScanAndFetch, ""},
		{OptIndexFetchMethod, "SCAN_AND_FETCH", "Option INDEX_FETCH_METHOD should be of type api.IndexFetchMethod but is string"},
		{OptTransactionTags, []string{"t"}, ""},
		{OptionName("NO_SUCH_OPTION"), true, "Unknown option NO_SUCH_OPTION"},
	} {
		err := ValidateOption(c.name, c.value)
		if c.want == "" {
			if err != nil {
				t.Errorf("%s = %#v refused: %v", c.name, c.value, err)
			}
			continue
		}
		var apiErr *Error
		if !errors.As(err, &apiErr) || apiErr.Code != ErrCodeInvalidParameter || apiErr.Message != c.want {
			t.Errorf("%s = %#v: %v; want 22023 %q", c.name, c.value, err, c.want)
		}
	}
}
