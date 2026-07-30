package values

import (
	"strings"
	"sync/atomic"
)

// CorrelationIdentifier + Correlated.
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
// The Go surface is narrow: CorrelationIdentifier as a wrapped string +
// factory + the Correlated interface signature. Concrete Values
// implement it (FieldValue, QuantifiedObjectValue, …), and the
// GetCorrelatedToOfValue walk aggregates over arbitrary Value trees.

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

// SameLeg reports whether two identifiers name the same quantifier — the
// CORRELATION element of RFC-197's identity triple, compared in ONE place so
// every proof that asks "does this value read that leg?" gets the same answer.
//
// The comparison is EXACT, matching Java, whose CorrelationIdentifier.equals
// is `Objects.equals(id, that.id)` (CorrelationIdentifier.java:132).
//
// Exactness is not merely Java-fidelity here — it is load-bearing. Alias
// namespaces in this planner are deliberately case-DISJOINT: the semantic
// scope upper-folds every user correlation at its single registration
// chokepoint (semantic/scope.go), while UniqueCorrelationIdentifier mints the
// machine counter in LOWERCASE (`q$1`). scope.go states the consequence it is
// protecting: a quoted `"q$5"` cannot forge a planner-minted q$5. A
// case-folding comparison erases exactly that protection — it lets a user
// alias that upper-folds onto the minted namespace be accepted as the minted
// leg, and every caller of this helper is a proof about which row a value
// reads. A forged match there is a wrong-rows plan or a fabricated
// cardinality, not a lost optimization.
//
// The same call was already made independently at the qualified-key rewrite in
// rule_implement_nested_loop_join.go, whose comment reads "a fold here would
// let a quoted user alias cross into the lowercase machine namespace". This
// helper now agrees with it rather than contradicting it.
//
// Folding was tempting because the translator does not upper-case aliases
// consistently — some paths fold an alias at construction and others pass the
// spelling through verbatim — so an exact comparison can fail to recognize a leg
// that really is the right one. (This used to cite three specific translator
// lines; two of them had already drifted onto unrelated code, which is why the
// claim is stated structurally now. A line number is not a citation once the file
// moves under it.) That costs a DECLINE — every caller reads
// "cannot tell" as "do not apply the correction" — and it is the translator's
// inconsistency to fix at the source, not this helper's to mask. Masking it
// here would trade a recoverable missed optimization for an unrecoverable
// forged identity.
// An UNSTATED identifier (the Go zero value, empty name) names nothing, and two
// of them name nothing in common. Go's zero value has no Java analogue —
// Quantifier.getAlias() is @Nonnull and a CorrelationIdentifier is never
// constructed empty — so the only way to hold one here is a producer that forgot
// to state an identity. Answering "same leg" for that pair is how an
// unstated-identity leg binds a zero-value correlation and starts serving its
// slots: every caller reads true as "this correlation names this leg". Declining
// turns the omission into the miss it actually is.
func SameLeg(a, b CorrelationIdentifier) bool {
	if a.name == "" || b.name == "" {
		return false
	}
	return a.name == b.name
}

// CurrentAlias is the well-known CorrelationIdentifier representing
// "the current row". Mirrors Java's `Quantifier.current()` — used by
// set-operation comparison key values and other contexts where a Value
// references "the row currently being processed" without binding to a
// specific named Quantifier.
var CurrentAlias = CorrelationIdentifier{name: "_current"}

// Correlated is the interface Java's `Correlated<T>` maps to. A
// Correlated value knows which CorrelationIdentifiers it depends
// on, and can rebind them (used by TranslationMap rewrites).
// Correlation-bearing Values and Predicates implement it.
type Correlated interface {
	// GetCorrelatedTo returns the set of Quantifier IDs this value
	// references. A "leaf" value (ConstantValue) returns an empty
	// set. A FieldValue on Quantifier q returns {q}.
	GetCorrelatedTo() map[CorrelationIdentifier]struct{}
}

// uitoa formats an unsigned 64-bit integer as a decimal string
// without depending on fmt/strconv (keeping this package's imports
// minimal).
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
