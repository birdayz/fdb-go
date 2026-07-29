package values

import "strings"

// ColumnIdentity is a column's identity — RFC-197's triple, as ONE comparable
// key an escaping helper can return instead of a bare display name.
//
// Java's counterpart is `FieldValue.ResolvedAccessor`, whose `equals` is
// ordinal-only (FieldValue.java:684) and whose constructor asserts
// `ordinal >= 0` (FieldValue.java:651) — the name is deliberately excluded from
// identity there and is excluded here. Go needs two elements Java's accessor
// gets for free: the CORRELATION (ordinal 0 of two quantifiers are different
// columns, and Java's rebase machinery keeps that on the value's own child),
// and the DOMAIN (Java derives it from the non-null typed `childValue`; Go
// mints childless bakes and must carry a token).
//
// The type deliberately has NO string field, at any depth reachable from a
// call site:
//
//   - `OrdinalDomain.sig` is unexported and is a signature of a WHOLE ordered
//     layout, obtainable only from OrdinalDomainOfType/OrdinalDomainOfColumnNames.
//     A single column's display name cannot be put there.
//   - `CorrelationIdentifier.name` is unexported and is a QUANTIFIER alias, a
//     different identity element with its own producers — never a column name.
//
// That is a structural defense, not a stylistic one. `pkg/docscheck`'s
// `.Field` gate fires on composite-literal KEYS and on returned selectors; a
// display name smuggled through a returned struct FIELD is invisible to it.
// So the key type simply has nowhere to put one, and
// `column_identity_test.go`'s reflection walk fails the build if that ever
// stops being true.
type ColumnIdentity struct {
	// Correlation is the quantifier whose row the ordinal indexes. The zero
	// value means "the value's own source", used by sites whose references are
	// uncorrelated (a leaf-relative metadata column).
	Correlation CorrelationIdentifier

	// Domain is the layout the ordinal indexes. Always KNOWN in a
	// ColumnIdentity that some producer returned ok for: every derivation
	// below routes through OrdinalIn, which fails closed on an unknown domain
	// on either side.
	Domain OrdinalDomain

	// Ordinal is the column's position in Domain. Always >= 0 for the same
	// reason: OrdinalIn declines the `-1` name-only accessors Go mints at its
	// unnest/gather/index-expansion seeds, where two accessors are
	// ordinal-equal by construction and the name is the only identity left.
	Ordinal int
}

// IdentityIn returns the identity of the column f reads, stated in the
// caller's layout, or declines.
//
// It is OrdinalIn plus the correlation: everything OrdinalIn fails closed on —
// a lazy value, a fused multi-accessor path, a negative name-only ordinal, an
// unknown domain on either side, an ordinal addressing some other layout —
// declines here too, and nothing falls back to the display name.
//
// The correlation comes from a DIRECT QuantifiedObjectValue child only. A
// chained accessor (`FieldValue{ID, Child: FieldValue{ADDRESS, Child: QOV}}`)
// reads a column of a NESTED record, so the quantifier's row is not the layout
// its ordinal indexes; it declines rather than reporting the root
// quantifier and an ordinal from a different layout. A CHILDLESS value keeps
// the zero correlation — it reads its own source's row, which is what the
// leaf-relative metadata sites hold.
func (f *FieldValue) IdentityIn(frontier OrdinalDomain) (ColumnIdentity, bool) {
	if f == nil {
		return ColumnIdentity{}, false
	}
	var corr CorrelationIdentifier
	switch child := f.Child.(type) {
	case nil:
	case *QuantifiedObjectValue:
		corr = child.Correlation
	default:
		return ColumnIdentity{}, false
	}
	ord, ok := f.OrdinalIn(frontier)
	if !ok {
		return ColumnIdentity{}, false
	}
	return ColumnIdentity{Correlation: corr, Domain: frontier, Ordinal: ord}, true
}

// CorrelatedIdentityIn is IdentityIn restricted to values that are a BARE
// column read off a quantifier — the shape the join/cost proofs consume. A
// childless value declines: those proofs compare against a named join leg, and
// a value with no quantifier cannot be one.
func (f *FieldValue) CorrelatedIdentityIn(frontier OrdinalDomain) (ColumnIdentity, bool) {
	if f == nil {
		return ColumnIdentity{}, false
	}
	if _, isQOV := f.Child.(*QuantifiedObjectValue); !isQOV {
		return ColumnIdentity{}, false
	}
	return f.IdentityIn(frontier)
}

// OrdinalOfNameIn resolves a METADATA column name — an index definition's
// column list, a primary key's column list, a proto descriptor's field — to
// its ordinal in a stated layout.
//
// This is the ONLY sanctioned direction for a name in this file, and it is the
// boundary rule of RFC-197: the metadata layer names its columns, that name is
// resolved ONCE against the layout it indexes, and it dies there. The result is
// a ColumnIdentity, so no caller downstream can compare names again.
//
// Matching is case-insensitive FIRST-MATCH against the layout's declared column
// order — the resolver's own sourceColumnOrdinal rule, and the same fold
// OrdinalDomain's signature applies. RecordType.FieldIndex is deliberately NOT
// used: it matches EXACTLY, and the metadata lists this function resolves
// (index definitions, primary keys, proto descriptors) do not agree with a
// record type's field spelling on case, so an exact match would silently
// decline a column that is plainly there.
//
// A name the layout does not declare, or a layout with no declared column
// order, declines.
func OrdinalOfNameIn(layout Type, name string) (ColumnIdentity, bool) {
	rt, isRecord := layout.(*RecordType)
	if !isRecord || name == "" {
		return ColumnIdentity{}, false
	}
	domain := OrdinalDomainOfType(layout)
	if !domain.IsKnown() {
		return ColumnIdentity{}, false
	}
	for i, f := range rt.Fields {
		if strings.EqualFold(f.Name, name) {
			return ColumnIdentity{Domain: domain, Ordinal: i}, true
		}
	}
	return ColumnIdentity{}, false
}

// WithCorrelation returns a copy of the identity read off the given quantifier.
// Used where a metadata-resolved identity (which has no quantifier of its own)
// must be compared against a reference read off a named join leg.
func (c ColumnIdentity) WithCorrelation(corr CorrelationIdentifier) ColumnIdentity {
	c.Correlation = corr
	return c
}
