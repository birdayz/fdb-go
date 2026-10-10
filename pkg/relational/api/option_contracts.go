// Portions derived from FoundationDB Record Layer (Options.java,
// OptionContract.java, CollectionContract.java, OrderedCollectionContract.java),
// Copyright 2015-2025 Apple Inc. and the FoundationDB project authors
// Copyright 2021-2024 Apple Inc. and the FoundationDB project authors
// Copyright 2025 Apple Inc. and the FoundationDB project authors
// Licensed under the Apache License, Version 2.0; translated to Go and modified.

package api

import (
	"math"
	"reflect"
	"strconv"
	"strings"
)

// optionContract is one option's admission rule, Java's OptionContract list
// (Options.makeContracts, Options.java:569-604): a carrier type, whether NULL
// is admitted, and an inclusive range for the integer options.
type optionContract struct {
	kind     optionKind
	nullable bool
	min, max int64
}

type optionKind int

const (
	optionBool optionKind = iota
	// optionInt is Java's Integer: any Go integer whose value fits int32.
	optionInt
	// optionLong is Java's Long: any Go integer.
	optionLong
	optionString
	// optionStrings is Java's CollectionContract/OrderedCollectionContract of
	// strings: a []string.
	optionStrings
	optionIndexFetchMethod
	optionVectorIndexEnginePreference
	// optionAny is an option whose carrier Go does not constrain here.
	optionAny
)

func boolOption() optionContract { return optionContract{kind: optionBool} }

func intOption(min, max int64) optionContract {
	return optionContract{kind: optionInt, min: min, max: max}
}

func longOption(min int64) optionContract {
	return optionContract{kind: optionLong, min: min, max: math.MaxInt64}
}

// optionContracts is Java's contract table, plus the Go-only options with the
// carrier their readers take.
var optionContracts = map[OptionName]optionContract{
	OptContinuation:                       {kind: optionAny},
	OptMaxRows:                            intOption(0, math.MaxInt32),
	OptIndexFetchMethod:                   {kind: optionIndexFetchMethod},
	OptDisablePlannerRewriting:            boolOption(),
	OptVectorIndexEnginePreference:        {kind: optionVectorIndexEnginePreference},
	OptDisabledPlannerRules:               {kind: optionStrings},
	OptIndexHint:                          {kind: optionString},
	OptPlanCachePrimaryMaxEntries:         intOption(0, math.MaxInt32),
	OptPlanCachePrimaryTimeToLiveMillis:   longOption(10),
	OptPlanCacheSecondaryMaxEntries:       intOption(1, math.MaxInt32),
	OptPlanCacheSecondaryTimeToLiveMillis: longOption(10),
	OptPlanCacheTertiaryMaxEntries:        intOption(1, math.MaxInt32),
	OptPlanCacheTertiaryTimeToLiveMillis:  longOption(10),
	OptReplaceOnDuplicatePK:               boolOption(),
	OptRequiredMetadataTableVersion:       intOption(-1, math.MaxInt32),
	OptTransactionTimeout:                 longOption(-1),
	OptLogQuery:                           boolOption(),
	OptLogSlowQueryThresholdMicros:        longOption(0),
	OptExecutionTimeLimit:                 longOption(0),
	OptExecutionScannedRowsLimit:          intOption(0, math.MaxInt32),
	OptExecutionScannedBytesLimit:         longOption(0),
	OptDryRun:                             boolOption(),
	OptPlanRightDeep:                      boolOption(),
	OptIsolationLevelSnapshot:             boolOption(),
	OptCaseSensitiveIdentifiers:           boolOption(),
	OptCurrentPlanHashMode:                {kind: optionString},
	OptValidPlanHashModes:                 {kind: optionString},
	OptAsyncOperationsTimeoutMillis:       longOption(0),
	OptEncryptWhenSerializing:             boolOption(),
	OptEncryptionKeyStore:                 {kind: optionString, nullable: true},
	OptEncryptionKeyEntry:                 {kind: optionString, nullable: true},
	OptEncryptionKeyEntryList:             {kind: optionStrings},
	OptEncryptionKeyPassword:              {kind: optionString, nullable: true},
	OptCompressWhenSerializing:            boolOption(),
	// Go-only options.
	OptMaxStatementMemoryBytes:      longOption(0),
	OptPlannerStatistics:            boolOption(),
	OptRestrictDDLToSessionDatabase: boolOption(),
	OptTransactionTags:              {kind: optionStrings},
	OptMaxTaskQueueSize:             intOption(0, math.MaxInt32),
	OptMaxTotalTaskCount:            intOption(0, math.MaxInt32),
	OptMaxNumMatchesPerRuleCall:     intOption(0, math.MaxInt32),
}

// OptionFromString is the value name's contract parses from s, Java's
// Options.parseStringOption (Options.java:462-469), which
// Builder.withOptionFromString and Options.fromProperties use: a boolean is
// Boolean.parseBoolean ("true" in any case, anything else false), an integer
// Integer.parseInt / Long.parseLong, a string itself, a collection the
// comma-separated, trimmed elements, an enum its valueOf. The result is not
// validated; the caller sets it through the contract check as withOption does.
// CONTINUATION has no string form (Java throws UnsupportedOperationException).
func OptionFromString(name OptionName, s string) (any, error) {
	contract, ok := optionContracts[name]
	if !ok {
		return nil, NewErrorf(ErrCodeInvalidParameter, "Unknown option %s", name)
	}
	switch contract.kind {
	case optionBool:
		return strings.EqualFold(s, "true"), nil
	case optionInt:
		n, err := strconv.ParseInt(s, 10, 32)
		if err != nil {
			return nil, NewErrorf(ErrCodeInvalidParameter, "For input string: \"%s\"", s)
		}
		return int(n), nil
	case optionLong:
		n, err := strconv.ParseInt(s, 10, 64)
		if err != nil {
			return nil, NewErrorf(ErrCodeInvalidParameter, "For input string: \"%s\"", s)
		}
		return n, nil
	case optionString:
		return s, nil
	case optionStrings:
		parts := strings.Split(s, ",")
		out := make([]string, len(parts))
		for i, p := range parts {
			out[i] = strings.TrimSpace(p)
		}
		return out, nil
	case optionIndexFetchMethod:
		return ParseIndexFetchMethod(s)
	case optionVectorIndexEnginePreference:
		return ParseVectorIndexEnginePreference(s)
	}
	return nil, NewErrorf(ErrCodeUnsupportedOperation, "Option %s has no string form", name)
}

// ValidateOption checks a value against its option's contract, as Java's
// Options.Builder.withOption does (Options.java:424-436, 471-475), with
// Java's INVALID_PARAMETER (22023) and its messages, the carrier named by its
// Go type. An option name with no contract is refused the same way.
func ValidateOption(name OptionName, value any) error {
	contract, ok := optionContracts[name]
	if !ok {
		return NewErrorf(ErrCodeInvalidParameter, "Unknown option %s", name)
	}
	if value == nil {
		if contract.nullable || contract.kind == optionAny {
			return nil
		}
		return NewErrorf(ErrCodeInvalidParameter, "Option %s should not be null", name)
	}
	wrongType := func(want string) error {
		return NewErrorf(ErrCodeInvalidParameter, "Option %s should be of type %s but is %T", name, want, value)
	}
	switch contract.kind {
	case optionBool:
		if _, ok := value.(bool); !ok {
			return wrongType("bool")
		}
	case optionString:
		if _, ok := value.(string); !ok {
			return wrongType("string")
		}
	case optionStrings:
		if _, ok := value.([]string); !ok {
			return wrongType("[]string")
		}
	case optionIndexFetchMethod:
		if _, ok := value.(IndexFetchMethod); !ok {
			return wrongType("api.IndexFetchMethod")
		}
	case optionVectorIndexEnginePreference:
		if _, ok := value.(VectorIndexEnginePreference); !ok {
			return wrongType("api.VectorIndexEnginePreference")
		}
	case optionInt, optionLong:
		rv := reflect.ValueOf(value)
		var n int64
		switch rv.Kind() {
		case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
			n = rv.Int()
		default:
			if contract.kind == optionInt {
				return wrongType("int")
			}
			return wrongType("int64")
		}
		if contract.kind == optionInt && (n < math.MinInt32 || n > math.MaxInt32) {
			return wrongType("int")
		}
		if n < contract.min || n > contract.max {
			return NewErrorf(ErrCodeInvalidParameter, "Option %s should be in range [%d, %d] but is %v",
				name, contract.min, contract.max, value)
		}
	}
	return nil
}
