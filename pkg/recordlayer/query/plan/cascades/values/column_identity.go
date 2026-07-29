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

// OrderingIdentityOf is the identity an ORDERING key is addressed by: the
// correlation its root reads from, the layout that root indexes, and the
// ordinal within it.
//
// This is the resolution Java gets for free. Java's `Ordering` is a
// `PartiallyOrderedSet<Value>` and a `SetMultimap<Value, Binding>` keyed by
// `Value.equals` (Ordering.java:176-183, :336) — semantic equality under the
// EMPTY alias map, so a correlation identifier must match exactly and a
// display name is never consulted. Go cannot key on the Value itself because
// the two sides are built by different producers and are not pointer- or
// structurally-equal even when they name the same column; so the identity is
// extracted and compared instead of the whole node.
//
// It declines — and the caller must then treat the key as UNADDRESSABLE, never
// fall back to a name — for:
//
//   - anything that is not a FieldValue (an arithmetic or function ordering
//     key has no column identity to state);
//   - a chained accessor path (more than one accessor, or a non-QOV child):
//     the root's layout is not the layout the deeper ordinal indexes, exactly
//     as IdentityIn declines;
//   - a LAZY node, a negative name-only ordinal, or an unknown domain — the
//     three shapes where an ordinal comparison would be vacuous or would
//     address some other layout.
//
// The correlation is the element that keeps two quantifiers over the SAME
// table apart: `o.a` and `i.a` in a self-join share a domain and an ordinal
// and are different columns. A childless value carries the ZERO correlation,
// which is Go's canonical source-relative root: it is the form the ordering
// PROVIDERS mint (a scan resolving its metadata column list against the row it
// flows) and the form a single-child plan's pull-up restates its child's keys
// in (plans.sourceLocalPullUp — Go's stand-in for Java's `Quantifier.current()`).
// Reconciling it against a QOV-rooted request is what
// CanBridgeOrderingValueRoots does, by NAME, and closing that bridge is the
// translation this type exists to make possible.
func OrderingIdentityOf(v Value) (ColumnIdentity, bool) {
	fv, isField := v.(*FieldValue)
	if !isField || fv == nil {
		return ColumnIdentity{}, false
	}
	var corr CorrelationIdentifier
	switch child := fv.Child.(type) {
	case nil:
	case *QuantifiedObjectValue:
		corr = child.Correlation
	default:
		return ColumnIdentity{}, false
	}
	path := fv.Resolved
	if path == nil || len(path.Accessors) != 1 ||
		path.Accessors[0].Ordinal < 0 || !path.Domain.IsKnown() {
		return ColumnIdentity{}, false
	}
	return ColumnIdentity{
		Correlation: corr,
		Domain:      path.Domain,
		Ordinal:     path.Accessors[0].Ordinal,
	}, true
}

// SameColumnPath reports whether two RESOLVED accessor paths denote the same
// column. It is the ORDINAL-PATH element of the identity triple asked BETWEEN
// two references, rather than resolved into a caller's stated layout the way
// IdentityIn is — the shape a matcher holding both operands needs. The
// CORRELATION element is not covered here: a matcher that admits two different
// child shapes (a childless source-relative bake against a QOV-qualified read of
// the same source) must prove the correlation its own way, and one that does not
// admit them should be comparing whole values.
//
// Java's FieldPath.equals is element-wise ordinal equality with the per-step
// name excluded (FieldValue.java:411-420, over ResolvedAccessor.equals at
// :676-685, which is getOrdinal()-only). Go needs two proofs on top, both for
// shapes Java cannot express:
//
//   - the DOMAIN, KNOWN on both sides and equal. Java's childValue is non-null
//     and typed, so the layout an ordinal indexes is always derivable; Go mints
//     childless bakes, and two ordinal-equal paths can address two different
//     layouts. An ordinal conflation reads as authoritative, which is strictly
//     worse than the name conflation it would replace.
//   - a NON-NEGATIVE ordinal at every step. Java asserts ordinal >= 0 at
//     construction (FieldValue.java:651); Go mints Ordinal -1 name-only
//     accessors at its unnest/gather/index-expansion seeds. Two of those are
//     ordinal-equal BY CONSTRUCTION, so an ordinal comparison over them is
//     vacuous and would bind two distinct nested fields as one.
//
// Both declines are the fail-closed direction: a refused match costs a rewrite,
// an accepted one binds the wrong column.
func SameColumnPath(a, b *FieldPath) bool {
	if a == nil || b == nil || len(a.Accessors) == 0 || len(a.Accessors) != len(b.Accessors) {
		return false
	}
	if !a.Domain.IsKnown() || a.Domain != b.Domain {
		return false
	}
	for i := range a.Accessors {
		if a.Accessors[i].Ordinal < 0 || a.Accessors[i].Ordinal != b.Accessors[i].Ordinal {
			return false
		}
	}
	return true
}
