package values

// CollateValue applies a locale-specific collation to a string,
// producing a sort-key BYTES blob suitable for use as part of an
// index key. Mirrors Java's
// `com.apple.foundationdb.record.query.plan.cascades.values.CollateValue`.
//
// SQL surface:
//
//	collate(name, 'en_US', 'PRIMARY')
//	      ↑ string  ↑ locale  ↑ strength
//
// Locale and strength are optional — Java's grammar allows
// `collate(name)`, `collate(name, locale)`, or
// `collate(name, locale, strength)`.
//
// Strength enum (mirrors java.text.Collator):
//   - PRIMARY: only base-letter differences matter ('a' = 'A', 'a' ≠ 'b').
//   - SECONDARY: + accent differences ('a' ≠ 'á', 'a' = 'A').
//   - TERTIARY: + case differences ('a' ≠ 'A').
//   - IDENTICAL: full Unicode normalisation.
//
// Used in collation-aware sort + comparison: the produced BYTES
// blob is a normalised sort key — sorting / comparing the keys
// lexicographically reproduces locale-aware ordering.
//
// Result type: NotNullBytes. Even when the input string is NULL,
// the produced sort key is a sentinel byte sequence (Java's
// behaviour); the seed surfaces nil in that branch and defers the
// real sentinel to TextCollator integration.
//
// Eval is a placeholder — real eval needs Go's collation machinery
// (`golang.org/x/text/collate.Collator`) wired through. The
// Value-shape is reachable for parser / planner / serialisation
// work today; runtime integration lands when collation-aware index
// construction is needed.
type CollateValue struct {
	StringChild   Value
	LocaleChild   Value // nil = use registry default locale
	StrengthChild Value // nil = use registry default strength
}

// NewCollateValue constructs a collate-encoder. localeChild +
// strengthChild may be nil to indicate "use defaults".
func NewCollateValue(stringChild, localeChild, strengthChild Value) *CollateValue {
	return &CollateValue{
		StringChild:   stringChild,
		LocaleChild:   localeChild,
		StrengthChild: strengthChild,
	}
}

// Children returns [stringChild, localeChild?, strengthChild?] —
// optional children are skipped when nil. Mirrors Java's
// computeChildren ImmutableList.builder pattern.
func (v *CollateValue) Children() []Value {
	out := []Value{v.StringChild}
	if v.LocaleChild != nil {
		out = append(out, v.LocaleChild)
	}
	if v.StrengthChild != nil {
		out = append(out, v.StrengthChild)
	}
	return out
}

// Name returns the SQL function name.
func (*CollateValue) Name() string { return "collate" }

// Type returns NotNullBytes.
func (*CollateValue) Type() Type { return NotNullBytes }

// Evaluate is a placeholder — real eval needs golang.org/x/text/collate
// wiring. Returns nil per the existing placeholder pattern.
func (*CollateValue) Evaluate(any) (any, error) { return nil, nil }

// WithChildren returns a fresh CollateValue with new children.
// Caller is responsible for passing the right number of children
// (1 / 2 / 3); the constructor reflects the count back into the
// optional fields.
func (v *CollateValue) WithChildren(newChildren []Value) *CollateValue {
	if len(newChildren) == 0 {
		return v // nothing to rebuild
	}
	stringChild := newChildren[0]
	var localeChild, strengthChild Value
	if len(newChildren) >= 2 {
		localeChild = newChildren[1]
	}
	if len(newChildren) >= 3 {
		strengthChild = newChildren[2]
	}
	return NewCollateValue(stringChild, localeChild, strengthChild)
}
