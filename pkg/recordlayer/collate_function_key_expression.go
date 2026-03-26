package recordlayer

import (
	"fmt"
	"strings"
	"sync"

	"golang.org/x/text/collate"
	"golang.org/x/text/language"
	"google.golang.org/protobuf/proto"
)

// Collation function names matching Java's CollateFunctionKeyExpressionFactory.
// Both JRE and ICU variants are registered since Go uses golang.org/x/text/collate
// (CLDR-based, similar to ICU).
//
// NOTE: Collation key bytes are NOT wire-compatible with Java's. Java uses
// java.text.CollationKey.toByteArray() (JRE) or com.ibm.icu.text.CollationKey.toByteArray()
// (ICU), whose binary formats are Java/ICU-version-specific. Go's collation keys
// use golang.org/x/text's CLDR-based format. This means:
// - Go can read/write its own collated indexes correctly
// - Go preserves collated index definitions during metadata round-trip
// - Go CANNOT share collated indexes with Java (different sort key bytes)
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
	RegisterFunction(CollateFuncJRE, eval)
	RegisterFunction(CollateFuncICU, eval)
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
