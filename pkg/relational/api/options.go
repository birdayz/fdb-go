package api

import (
	"math"
	"reflect"
	"sync"
)

// Options carries per-connection / per-statement / per-engine
// configuration values, keyed by OptionName.
//
// Mirrors Java's com.apple.foundationdb.relational.api.Options. The
// semantics are:
//
//   - Options are immutable; With returns a new Options.
//   - An Options may have a parent; Get walks the parent chain, then
//     falls back to the default value.
//   - Nil values are stored explicitly (via a sentinel) so that a
//     set-to-nil in a child masks a parent value.
//
// Deviations from Java:
//
//   - No Properties serialization (JDBC-specific; Go users pass values
//     through context / DSN query params instead).
//   - Option validation contracts are minimal — full OptionContract DSL
//     deferred until needed.
type Options struct {
	parent *Options
	values map[OptionName]any
}

// OptionName is the name of a relational option. String-backed to
// match Java's Options.Name enum; values MUST match Java exactly so
// DSN query parameters and properties are wire-compatible.
type OptionName string

// Option names, matching Java's Options.Name enum 1:1. New names must
// be added here AND in Java simultaneously.
const (
	OptContinuation OptionName = "CONTINUATION"
	OptIndexHint    OptionName = "INDEX_HINT"
	OptMaxRows      OptionName = "MAX_ROWS"
	// OptMaxStatementMemoryBytes is the statement-wide memory byte budget
	// (RFC-130). It bounds the in-memory buffering operators (NLJ inner, UNION
	// buffered, sort buffers, distinct/dedup seen-sets, recursive-CTE working
	// sets, temp tables, DML echoes) by BYTES, where MaterializationLimit only
	// bounds them by ROW COUNT — so 100k large rows can no longer OOM. Default
	// 0 = unlimited (today's behaviour); a positive value caps the statement's
	// accounted buffering and a breach errors with 54F01. This is a Go-only
	// extension Java lacks, so it is deliberately NOT in defaultOptionValues
	// (the wire/plan-cache-sensitive default map): an absent option reads back
	// as 0 via the optInt64 fallback, keeping the default path identical to
	// pre-RFC-130 and the default option set byte-identical to Java's.
	OptMaxStatementMemoryBytes            OptionName = "MAX_STATEMENT_MEMORY_BYTES"
	OptRequiredMetadataTableVersion       OptionName = "REQUIRED_METADATA_TABLE_VERSION"
	OptTransactionTimeout                 OptionName = "TRANSACTION_TIMEOUT"
	OptReplaceOnDuplicatePK               OptionName = "REPLACE_ON_DUPLICATE_PK"
	OptPlanCachePrimaryMaxEntries         OptionName = "PLAN_CACHE_PRIMARY_MAX_ENTRIES"
	OptPlanCacheSecondaryMaxEntries       OptionName = "PLAN_CACHE_SECONDARY_MAX_ENTRIES"
	OptPlanCacheTertiaryMaxEntries        OptionName = "PLAN_CACHE_TERTIARY_MAX_ENTRIES"
	OptPlanCachePrimaryTimeToLiveMillis   OptionName = "PLAN_CACHE_PRIMARY_TIME_TO_LIVE_MILLIS"
	OptPlanCacheSecondaryTimeToLiveMillis OptionName = "PLAN_CACHE_SECONDARY_TIME_TO_LIVE_MILLIS"
	OptPlanCacheTertiaryTimeToLiveMillis  OptionName = "PLAN_CACHE_TERTIARY_TIME_TO_LIVE_MILLIS"
	OptIndexFetchMethod                   OptionName = "INDEX_FETCH_METHOD"
	OptDisabledPlannerRules               OptionName = "DISABLED_PLANNER_RULES"
	OptDisablePlannerRewriting            OptionName = "DISABLE_PLANNER_REWRITING"
	// OptLogQuery gates the SLF4J log level in Java. Go has no ambient
	// log-level concept: the planning-metrics hook (RFC-034) always emits a
	// record and the handler owns level + sampling, so this option is
	// intentionally not consumed by the embedded engine pending the
	// options-plumbing work for the gRPC/REPL frontends.
	OptLogQuery OptionName = "LOG_QUERY"
	// OptLogSlowQueryThresholdMicros is the canonical default source for the
	// connection's slow-query threshold (RFC-034); see
	// embedded.defaultSlowQueryThresholdMicros.
	OptLogSlowQueryThresholdMicros  OptionName = "LOG_SLOW_QUERY_THRESHOLD_MICROS"
	OptExecutionTimeLimit           OptionName = "EXECUTION_TIME_LIMIT"
	OptExecutionScannedBytesLimit   OptionName = "EXECUTION_SCANNED_BYTES_LIMIT"
	OptExecutionScannedRowsLimit    OptionName = "EXECUTION_SCANNED_ROWS_LIMIT"
	OptDryRun                       OptionName = "DRY_RUN"
	OptCaseSensitiveIdentifiers     OptionName = "CASE_SENSITIVE_IDENTIFIERS"
	OptCurrentPlanHashMode          OptionName = "CURRENT_PLAN_HASH_MODE"
	OptValidPlanHashModes           OptionName = "VALID_PLAN_HASH_MODES"
	OptAsyncOperationsTimeoutMillis OptionName = "ASYNC_OPERATIONS_TIMEOUT_MILLIS"
	OptEncryptWhenSerializing       OptionName = "ENCRYPT_WHEN_SERIALIZING"
	OptEncryptionKeyStore           OptionName = "ENCRYPTION_KEY_STORE"
	OptEncryptionKeyEntry           OptionName = "ENCRYPTION_KEY_ENTRY"
	OptEncryptionKeyEntryList       OptionName = "ENCRYPTION_KEY_ENTRY_LIST"
	OptEncryptionKeyPassword        OptionName = "ENCRYPTION_KEY_PASSWORD"
	OptCompressWhenSerializing      OptionName = "COMPRESS_WHEN_SERIALIZING"
)

// IndexFetchMethod mirrors Java's Options.IndexFetchMethod enum.
type IndexFetchMethod int

const (
	IndexFetchScanAndFetch IndexFetchMethod = iota
	IndexFetchUseRemoteFetch
	IndexFetchUseRemoteFetchWithFallback
)

// String returns the enum name matching Java.
func (m IndexFetchMethod) String() string {
	switch m {
	case IndexFetchScanAndFetch:
		return "SCAN_AND_FETCH"
	case IndexFetchUseRemoteFetch:
		return "USE_REMOTE_FETCH"
	case IndexFetchUseRemoteFetchWithFallback:
		return "USE_REMOTE_FETCH_WITH_FALLBACK"
	default:
		return "?"
	}
}

// nullSentinel is a non-nil marker stored in the values map to encode
// an explicitly-unset value. Matches Java's NULL_STANDIN. It MUST NOT
// be exposed to callers.
type nullSentinel struct{}

var nullValue = &nullSentinel{}

// defaultOptionValues holds the default values mirroring Java's
// OPTIONS_DEFAULT_VALUES static block. Built once; never mutated.
//
// Wire note: changing any value here is a wire-format change — the
// default flows through to plan cache keys and round-trip tests
// against Java. Keep in sync with Java's Options static initializer.
//
// TODO: OptIndexFetchMethod defaults to USE_REMOTE_FETCH_WITH_FALLBACK
// to match Java. Embedded Go users cannot use remote fetch until
// Phase 9 (gRPC server); in the meantime the concrete Connection
// impl should detect embedded mode and fall back to SCAN_AND_FETCH.
var defaultOptionValues = map[OptionName]any{
	OptMaxRows:                            math.MaxInt32,
	OptIndexFetchMethod:                   IndexFetchUseRemoteFetchWithFallback,
	OptDisablePlannerRewriting:            false,
	OptDisabledPlannerRules:               []string{},
	OptPlanCachePrimaryMaxEntries:         int64(1024),
	OptPlanCachePrimaryTimeToLiveMillis:   int64(10_000),
	OptPlanCacheSecondaryMaxEntries:       int64(256),
	OptPlanCacheSecondaryTimeToLiveMillis: int64(30_000),
	OptPlanCacheTertiaryMaxEntries:        int64(8),
	OptPlanCacheTertiaryTimeToLiveMillis:  int64(30_000),
	OptReplaceOnDuplicatePK:               false,
	OptLogQuery:                           false,
	OptLogSlowQueryThresholdMicros:        int64(2_000_000),
	OptExecutionScannedBytesLimit:         int64(math.MaxInt64),
	OptExecutionTimeLimit:                 int64(0),
	OptExecutionScannedRowsLimit:          math.MaxInt32,
	OptDryRun:                             false,
	OptCaseSensitiveIdentifiers:           false,
	OptAsyncOperationsTimeoutMillis:       int64(10_000),
	OptEncryptWhenSerializing:             false,
	OptEncryptionKeyPassword:              "",
	OptCompressWhenSerializing:            true,
}

var (
	noneOnce sync.Once
	noneRef  *Options
)

// NoOptions returns the empty Options (corresponds to Java's
// Options.NONE singleton).
func NoOptions() *Options {
	noneOnce.Do(func() {
		noneRef = &Options{values: map[OptionName]any{}}
	})
	return noneRef
}

// DefaultOptionValues returns a copy of the default value map.
// Mutating the returned map does not affect defaults.
func DefaultOptionValues() map[OptionName]any {
	out := make(map[OptionName]any, len(defaultOptionValues))
	for k, v := range defaultOptionValues {
		out[k] = v
	}
	return out
}

// Get returns the value for name, walking the parent chain, then
// falling back to the default. Returns nil if the option has been
// explicitly set to nil in this or a parent Options, or if no
// default exists for name. The nil-unset-default and nil-explicit
// cases are indistinguishable via Get alone — Entries can be used
// to check whether an option was explicitly set to nil on this
// specific Options instance.
func (o *Options) Get(name OptionName) any {
	if v, ok := o.values[name]; ok {
		if _, isNull := v.(*nullSentinel); isNull {
			return nil
		}
		return v
	}
	if o.parent != nil {
		return o.parent.Get(name)
	}
	return defaultOptionValues[name]
}

// With returns a new Options with name=value applied on top of o.
// Explicit nil values are preserved via the null sentinel.
func (o *Options) With(name OptionName, value any) *Options {
	next := make(map[OptionName]any, len(o.values)+1)
	for k, v := range o.values {
		next[k] = v
	}
	if value == nil {
		next[name] = nullValue
	} else {
		next[name] = value
	}
	return &Options{parent: o.parent, values: next}
}

// WithChild returns a new Options whose parent is o and whose own
// values are those of child. Matches Java's Options.combine.
// Returns an error if child already has a parent.
func (o *Options) WithChild(child *Options) (*Options, error) {
	if child.parent != nil {
		return nil, NewError(ErrCodeInternalError, "cannot override parent options")
	}
	if o == child {
		return child, nil
	}
	return &Options{parent: o, values: child.values}, nil
}

// Entries returns a copy of this Options' own values (parent values
// are not included). For parent-inclusive traversal, use AllEntries.
func (o *Options) Entries() map[OptionName]any {
	out := make(map[OptionName]any, len(o.values))
	for k, v := range o.values {
		if _, isNull := v.(*nullSentinel); isNull {
			out[k] = nil
		} else {
			out[k] = v
		}
	}
	return out
}

// AllEntries returns the effective values (child overriding parent).
// Defaults are NOT included; iterate DefaultOptionValues separately
// to reason about them.
func (o *Options) AllEntries() map[OptionName]any {
	out := map[OptionName]any{}
	if o.parent != nil {
		for k, v := range o.parent.AllEntries() {
			out[k] = v
		}
	}
	for k, v := range o.values {
		if _, isNull := v.(*nullSentinel); isNull {
			out[k] = nil
		} else {
			out[k] = v
		}
	}
	return out
}

// Equal reports structural equality: same own values AND a structurally
// equal parent chain. Matches Java's Options.equals() which does a
// recursive Objects.equals() on parent — not pointer identity.
func (o *Options) Equal(other *Options) bool {
	if o == other {
		return true
	}
	if o == nil || other == nil {
		return false
	}
	// Recursively compare parent chains. Nil parents are equal; two
	// different but structurally-equal parents are equal (matches
	// Java). Two-pointer short-circuit avoids infinite recursion on
	// shared parents.
	switch {
	case o.parent == other.parent:
		// same pointer (or both nil) — ok
	case o.parent == nil || other.parent == nil:
		return false
	case !o.parent.Equal(other.parent):
		return false
	}
	if len(o.values) != len(other.values) {
		return false
	}
	for k, v := range o.values {
		ov, ok := other.values[k]
		if !ok {
			return false
		}
		// Both-null sentinels compare equal.
		_, vN := v.(*nullSentinel)
		_, oN := ov.(*nullSentinel)
		if vN != oN {
			return false
		}
		if !vN && !anyEqualValue(v, ov) {
			return false
		}
	}
	return true
}

// anyEqualValue reports whether two option values are equal. Uses a
// fast path for []string (the only slice-typed option today — see
// OptDisabledPlannerRules and OptEncryptionKeyEntryList) and falls
// through to reflect.DeepEqual for anything else.
//
// The reflect path matters: a future option value type that's
// uncomparable (another slice, a map, a struct with slice fields)
// must not cause Equal to panic — it's used by the plan cache.
func anyEqualValue(a, b any) bool {
	// []string fast path — common and avoids reflect's overhead.
	if as, aok := a.([]string); aok {
		bs, bok := b.([]string)
		if !bok || len(as) != len(bs) {
			return false
		}
		for i := range as {
			if as[i] != bs[i] {
				return false
			}
		}
		return true
	}
	return reflect.DeepEqual(a, b)
}

// OptionsBuilder builds an Options.
type OptionsBuilder struct {
	opts *Options
}

// NewOptionsBuilder starts a builder atop NoOptions().
func NewOptionsBuilder() *OptionsBuilder {
	return &OptionsBuilder{opts: NoOptions()}
}

// From starts a builder atop the given Options (its own values are
// preserved; its parent chain is carried through).
func (b *OptionsBuilder) From(o *Options) *OptionsBuilder {
	if o == nil {
		return b
	}
	cp := make(map[OptionName]any, len(o.values))
	for k, v := range o.values {
		cp[k] = v
	}
	b.opts = &Options{parent: o.parent, values: cp}
	return b
}

// Set applies name=value to the builder. Returns the builder for
// chaining. Nil values are stored as an explicit "masked" flag so
// they shadow parent/default values.
func (b *OptionsBuilder) Set(name OptionName, value any) *OptionsBuilder {
	cp := make(map[OptionName]any, len(b.opts.values)+1)
	for k, v := range b.opts.values {
		cp[k] = v
	}
	if value == nil {
		cp[name] = nullValue
	} else {
		cp[name] = value
	}
	b.opts = &Options{parent: b.opts.parent, values: cp}
	return b
}

// Build returns the accumulated Options. The builder may be reused
// (the returned Options does not alias the builder's internal map).
func (b *OptionsBuilder) Build() *Options {
	cp := make(map[OptionName]any, len(b.opts.values))
	for k, v := range b.opts.values {
		cp[k] = v
	}
	return &Options{parent: b.opts.parent, values: cp}
}
