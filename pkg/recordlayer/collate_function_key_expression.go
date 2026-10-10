// Portions derived from FoundationDB Record Layer (
// CollateFunctionKeyExpression.java),
// Copyright 2015-2018 Apple Inc. and the FoundationDB project authors
// Licensed under the Apache License, Version 2.0; translated to Go and modified.

package recordlayer

import (
	"fmt"
	"strings"
	"sync"

	"golang.org/x/text/collate"
	"golang.org/x/text/language"
	"google.golang.org/protobuf/proto"

	"fdb.dev/gen"
)

// Collation function names matching Java's CollateFunctionKeyExpressionFactory.
// Both names use golang.org/x/text/collate, not Java's JRE/ICU key formats.
// Persisting or querying those keys requires StoreBuilder.SetGoOnlyCollation.
const (
	CollateFuncJRE = "collate_jre"
	CollateFuncICU = "collate_icu"
)

// Collation strength levels matching Java's Collator.PRIMARY/SECONDARY/TERTIARY.
const (
	CollateStrengthPrimary   = 0 // Base form only (ignores case and accents)
	CollateStrengthSecondary = 1 // Base form + accents (case-insensitive)
	CollateStrengthTertiary  = 2 // Base form + accents + case
)

// collatorPools caches sync.Pool instances by (locale, strength) for reuse.
// Each pool creates goroutine-safe Collator instances on demand.
// collate.Collator is NOT goroutine-safe, so we pool instead of sharing.
var (
	collatorPoolsMu sync.RWMutex
	collatorPools   = make(map[collatorPoolKey]*sync.Pool)
)

type collatorPoolKey struct {
	locale   string
	strength int
}

func init() {
	eval := makeCollateEvaluator()
	// CollateFunctionKeyExpression.java:151-159, :169, :183. collate_icu is the
	// target's ICU module's (CollateFunctionKeyExpressionFactoryICU, a
	// CollateFunctionKeyExpression), not its core registry's.
	spec := FunctionSpec{Evaluator: eval, MinArguments: 1, MaxArguments: 3, ColumnSize: 1, NullIsNonUnique: true}
	registerCoreFunction(CollateFuncJRE, spec)
	registerCoreFunction(CollateFuncICU, spec)
}

// collationFunctionIn is the name of the first collate_jre / collate_icu
// function in expr, or "" when it has none.
func collationFunctionIn(expr KeyExpression) string {
	switch e := expr.(type) {
	case *FunctionKeyExpression:
		if e.name == CollateFuncJRE || e.name == CollateFuncICU {
			return e.name
		}
		return collationFunctionIn(e.arguments)
	case *CardinalityFunctionKeyExpression:
		return collationFunctionIn(e.arguments)
	case *CompositeKeyExpression:
		return firstCollationFunctionIn(e.expressions)
	case *ListKeyExpression:
		return firstCollationFunctionIn(e.children)
	case *NestingKeyExpression:
		return collationFunctionIn(e.child)
	case *GroupingKeyExpression:
		return collationFunctionIn(e.wholeKey)
	case *KeyWithValueExpression:
		return collationFunctionIn(e.innerKey)
	case *SplitKeyExpression:
		return collationFunctionIn(e.joined)
	case *DimensionsKeyExpression:
		return collationFunctionIn(e.WholeKey)
	default:
		return ""
	}
}

func firstCollationFunctionIn(exprs []KeyExpression) string {
	for _, child := range exprs {
		if fn := collationFunctionIn(child); fn != "" {
			return fn
		}
	}
	return ""
}

// Collated index bounds and entries are provider-specific, even at equal strengths.
func (store *FDBRecordStore) checkIndexCollation(index *Index) error {
	if store.goOnlyCollation {
		return nil
	}
	if fn := collationFunctionIn(index.RootExpression); fn != "" {
		return &GoOnlyCollationError{Function: fn, IndexName: index.Name}
	}
	return nil
}

// A collated primary key files the record under a key Java would not compute.
func (store *FDBRecordStore) checkPrimaryKeyCollation(recordType *RecordType) error {
	if store.goOnlyCollation {
		return nil
	}
	if fn := collationFunctionIn(recordType.PrimaryKey); fn != "" {
		return &GoOnlyCollationError{Function: fn, RecordTypeName: recordType.Name}
	}
	return nil
}

func (store *FDBRecordStore) checkRecordCountCollation(countKey KeyExpression) error {
	if !store.goOnlyCollation {
		if fn := collationFunctionIn(countKey); fn != "" {
			return &GoOnlyCollationError{Function: fn, RecordCountKey: true}
		}
	}
	return nil
}

// Preflight before record mutations: discovering an incompatible index afterward
// would leave a partial write in a transaction the caller could still commit.
func (store *FDBRecordStore) checkRecordWriteCollation(countChanges bool, types ...*RecordType) error {
	if store.goOnlyCollation {
		return nil
	}
	if err := store.ensureStoreStateLoadedErr(); err != nil {
		return err
	}
	store.stateMu.RLock()
	defer store.stateMu.RUnlock()
	return store.checkRecordWriteCollationLocked(countChanges, types...)
}

func (store *FDBRecordStore) checkRecordWriteCollationLocked(countChanges bool, types ...*RecordType) error {
	if store.goOnlyCollation {
		return nil
	}
	if countChanges && (store.storeHeader == nil || store.storeHeader.GetRecordCountState() != gen.DataStoreInfo_DISABLED) {
		if err := store.checkRecordCountCollation(store.metaData.GetRecordCountKey()); err != nil {
			return err
		}
	}
	check := func(index *Index) error {
		if err := store.checkIndexCollation(index); err != nil {
			maintain, stateErr := store.shouldMaintainIndex(index.Name)
			if stateErr != nil {
				return stateErr
			}
			if maintain {
				return err
			}
		}
		return nil
	}
	for _, rt := range types {
		if rt == nil {
			continue
		}
		for _, index := range store.metaData.GetIndexesForRecordType(rt.Name) {
			if err := check(index); err != nil {
				return err
			}
		}
	}
	for _, index := range store.metaData.GetUniversalIndexes() {
		if err := check(index); err != nil {
			return err
		}
	}
	return nil
}

// makeCollateEvaluator creates a FunctionEvaluator for collation functions.
// Arguments: (string_value [, locale_string [, strength_int]])
// Returns: byte array collation key (preserves locale-specific ordering)
// Null input → null output.
//
// Matches Java's CollateFunctionKeyExpression.evaluateFunction().
func makeCollateEvaluator() FunctionEvaluator {
	return func(_ *FDBStoredRecord[proto.Message], _ proto.Message, arguments [][]any) ([][]any, error) {
		results := make([][]any, 0, len(arguments))
		for _, args := range arguments {
			if len(args) < 1 {
				return nil, &KeyExpressionError{Message: "collate function requires at least 1 argument"}
			}

			// Null string → null result
			if args[0] == nil {
				results = append(results, []any{nil})
				continue
			}

			str, ok := args[0].(string)
			if !ok {
				return nil, &KeyExpressionError{Message: fmt.Sprintf("collate function argument must be string, got %T", args[0])}
			}

			// Extract locale (default: root)
			localeName := ""
			if len(args) >= 2 && args[1] != nil {
				if s, ok := args[1].(string); ok {
					localeName = s
				}
			}

			// Extract strength (default: primary = 0, matching Java's default)
			strength := CollateStrengthPrimary
			if len(args) >= 3 && args[2] != nil {
				switch v := args[2].(type) {
				case int64:
					strength = int(v)
				case int:
					strength = v
				case int32:
					strength = int(v)
				}
			}

			pool := getCollatorPool(localeName, strength)
			c := pool.Get().(*collate.Collator)
			var buf collate.Buffer
			key := c.KeyFromString(&buf, str)
			// Copy the key bytes (buf is local, but key references buf internals)
			keyCopy := make([]byte, len(key))
			copy(keyCopy, key)
			pool.Put(c)
			results = append(results, []any{keyCopy})
		}
		return results, nil
	}
}

// getCollatorPool returns a pool of Collator instances for the given locale and strength.
// Collators are NOT goroutine-safe, so each concurrent user borrows one from the pool.
func getCollatorPool(locale string, strength int) *sync.Pool {
	pk := collatorPoolKey{locale: locale, strength: strength}

	collatorPoolsMu.RLock()
	pool, ok := collatorPools[pk]
	collatorPoolsMu.RUnlock()
	if ok {
		return pool
	}

	collatorPoolsMu.Lock()
	defer collatorPoolsMu.Unlock()

	// Double-check after acquiring write lock
	if pool, ok = collatorPools[pk]; ok {
		return pool
	}

	pool = &sync.Pool{
		New: func() any {
			return newCollator(locale, strength)
		},
	}
	collatorPools[pk] = pool
	return pool
}

func newCollator(locale string, strength int) *collate.Collator {
	tag := language.Und // Root locale
	if locale != "" {
		// Java converts underscore to hyphen for BCP 47 compatibility
		tag = language.Make(strings.ReplaceAll(locale, "_", "-"))
	}

	var opts []collate.Option
	switch strength {
	case CollateStrengthPrimary:
		// Loose = ignore case + diacritics + width (matches Java's PRIMARY)
		opts = append(opts, collate.Loose)
	case CollateStrengthSecondary:
		opts = append(opts, collate.IgnoreCase)
	case CollateStrengthTertiary:
		// No options needed — full comparison
	}

	return collate.New(tag, opts...)
}
