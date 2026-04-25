package values

import (
	"strings"
	"sync/atomic"
)

// CorrelationIdentifier + Correlated — seed.
//
// Ports Java's
// `com.apple.foundationdb.record.query.plan.cascades.CorrelationIdentifier`
// — a symbolic ID for a Quantifier (the thing a Value correlates to)
// and the `Correlated<T>` interface (types that can report and
// rebind their correlation set). Java keeps `CorrelationIdentifier`
// in the root cascades package; we moved it into our `values/`
// sub-package per RFC-025 §"Don't split" since values is the only
// intra-package consumer.
//
// Most Cascades types implement Correlated — Value, QueryPredicate,
// RelationalExpression, Quantifier. It tells rules which upstream
// Quantifier an expression depends on so rewrites can detect when a
// rewrite changes correlation shape.
//
// Seed is narrow: CorrelationIdentifier as a wrapped string +
// factory + the Correlated interface signature. Concrete
// implementations (FieldValue.GetCorrelatedTo etc.) land as Values
// get ported.

// CorrelationIdentifier is an opaque alias for a Quantifier — two
// distinct Quantifiers get distinct IDs. Comparable by value
// (underlying string) so CorrelationIdentifiers can live in maps.
type CorrelationIdentifier struct {
	name string
}

var uniqueCorrelationCounter atomic.Uint64

// UniqueCorrelationIdentifier generates a fresh CorrelationIdentifier
// with a monotonically-increasing suffix. Used when the analyzer
// needs to allocate a new Quantifier mid-rewrite. Java calls the
// equivalent `CorrelationIdentifier.uniqueID()`.
//
// Format: "q$1", "q$2", ... — leading 'q' matches Java's convention
// so explain output diffs cleanly against Java's.
func UniqueCorrelationIdentifier() CorrelationIdentifier {
	n := uniqueCorrelationCounter.Add(1)
	var b strings.Builder
	b.WriteString("q$")
	// Avoid fmt import for a one-shot format.
	b.WriteString(uitoa(n))
	return CorrelationIdentifier{name: b.String()}
}

// NamedCorrelationIdentifier wraps an explicit name (e.g. a SQL
// alias). Two NamedCorrelationIdentifiers with the same name are
// equal — unlike UniqueCorrelationIdentifier which always allocates.
func NamedCorrelationIdentifier(name string) CorrelationIdentifier {
	return CorrelationIdentifier{name: name}
}

// Name returns the underlying identifier string.
func (c CorrelationIdentifier) Name() string { return c.name }

// String implements fmt.Stringer.
func (c CorrelationIdentifier) String() string { return c.name }

// IsZero reports whether c is the zero-value CorrelationIdentifier.
// Useful for nil-checks without a pointer.
func (c CorrelationIdentifier) IsZero() bool { return c.name == "" }

// Correlated is the interface Java's `Correlated<T>` maps to. A
// Correlated value knows which CorrelationIdentifiers it depends
// on, and can rebind them (used by TranslationMap rewrites).
//
// Seed signature. As Values and Predicates get ported, they
// implement this.
type Correlated interface {
	// GetCorrelatedTo returns the set of Quantifier IDs this value
	// references. A "leaf" value (ConstantValue) returns an empty
	// set. A FieldValue on Quantifier q returns {q}.
	GetCorrelatedTo() map[CorrelationIdentifier]struct{}
}

// uitoa formats an unsigned 64-bit integer as a decimal string
// without depending on fmt/strconv (keeping this package's imports
// minimal for the seed).
func uitoa(n uint64) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}
