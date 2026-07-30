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
// which is Go's canonical source-relative root (see
// RequestedOrdering.NormalizeRootsTo).
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

// SameOrderingColumn reports whether two ORDERING Values denote the same
// column by identity: the same ordinal path in the same STATED layout, read off
// a compatible root. Both sides must have stated an identity; a side that has
// not declines, and nothing here consults a display name.
//
// It is the ordinal-domain counterpart of CanBridgeOrderingValueRoots' name
// comparison, and it exists because the obvious alternative is unsound:
// ValuesStructurallyEqual routes two baked FieldValues through
// FieldPath.Equals, which is ordinal-only and DOMAIN-BLIND (Java's
// ResolvedAccessor.equals, FieldValue.java:675-689 — sound there because a Java
// FieldValue always has a non-null typed childValue, so the layout an ordinal
// indexes is never in question). Go mints CHILDLESS bakes against several
// different rows, so ordinal 0 of a record row and ordinal 0 of an aggregate's
// output row compare EQUAL there. That conflation is measured, not
// hypothetical, and it reads as authoritative — which is strictly worse than
// the name comparison it would replace.
//
// The ROOT rule permits one asymmetry: a childless value against a
// QOV-rooted one. A childless bake is Go's canonical source-relative root (see
// RequestedOrdering.NormalizeRootsTo), and the match-candidate side is
// childless by construction while a request scoped to its owning quantifier is
// not. Two DIFFERENT named quantifiers are different columns and decline: that
// is what keeps `o.a` and `i.a` apart in a self-join.
// OrderingFieldPair reports whether both ordering values are plain FieldValue
// column reads. It is the TYPE test that dispatches the two ordering
// comparators: a pair inside this class is decided by SameOrderingColumn and by
// nothing else, so the class is an equivalence relation (see
// StatesOrderingColumn for the intransitivity that availability dispatch had).
//
// A CardinalityValue wrapping a field is deliberately OUTSIDE the class. It is
// not a column of any row layout, so it has no column identity to state and is
// matched as a whole Value instead.
func OrderingFieldPair(a, b Value) bool {
	af, aIsField := a.(*FieldValue)
	bf, bIsField := b.(*FieldValue)
	return aIsField && bIsField && af != nil && bf != nil
}

func SameOrderingColumn(a, b Value) bool {
	af, aIsField := a.(*FieldValue)
	bf, bIsField := b.(*FieldValue)
	if !aIsField || !bIsField || af == nil || bf == nil {
		return false
	}
	aCorr, aRootOK := orderingRootCorrelation(af)
	bCorr, bRootOK := orderingRootCorrelation(bf)
	if !aRootOK || !bRootOK {
		return false
	}
	if aCorr != bCorr && !aCorr.IsZero() && !bCorr.IsZero() {
		return false
	}
	return SameColumnPath(af.Resolved, bf.Resolved)
}

// orderingRootCorrelation returns the quantifier a flat ordering reference
// reads from — the zero identifier for a childless (source-relative) value.
// A child that is neither absent nor a quantifier reads some other row and
// declines.
func orderingRootCorrelation(f *FieldValue) (CorrelationIdentifier, bool) {
	switch child := f.Child.(type) {
	case nil:
		return CorrelationIdentifier{}, true
	case *QuantifiedObjectValue:
		if child == nil {
			return CorrelationIdentifier{}, false
		}
		return child.Correlation, true
	default:
		return CorrelationIdentifier{}, false
	}
}

// StatesOrderingColumn reports whether ONE ordering value has stated a column
// identity SameOrderingColumn can read — a flat-or-nested resolved path in a
// known layout, off a root that is absent or a quantifier.
//
// It is deliberately a ONE-VALUE predicate, and that shape is the whole point.
// The pairwise form it replaced ("do BOTH sides state an identity?") was used to
// DISPATCH: identity decided the pair when both stated one, and a pair where
// either side did not fell through to the domain-blind structural comparison.
// That dispatch is INTRANSITIVE inside the FieldValue class. Witness, all three
// values baked at ordinal path [0]:
//
//	A = [0] in layout D1          states an identity
//	B = [0] in layout D2          states an identity
//	U = [0], layout UNKNOWN       states none
//
// Identity separates A from B (different layouts, EqualsWithoutChildren's
// FieldValue arm never looks at the layout). Availability dispatch sends both
// A~U and B~U to the structural arm, which compares ordinals only and says
// EQUAL. So U≡A, U≡B, A≢B — and a comparator that is not an equivalence
// relation makes every set it builds depend on INSERTION ORDER, which is a
// nondeterministic plan.
//
// So dispatch is by TYPE — both operands *FieldValue means identity decides and
// the decision is FINAL — and this predicate exists only to CLASSIFY the
// population, never to choose an arm. A value that does not state an identity is
// UNADDRESSABLE: the comparator declines it, and the fix belongs at the producer
// that minted it without a layout.
func StatesOrderingColumn(v Value) bool {
	f, isField := v.(*FieldValue)
	if !isField || f == nil {
		return false
	}
	if _, ok := orderingRootCorrelation(f); !ok {
		return false
	}
	return statesColumnPath(f.Resolved)
}

// statesColumnPath reports whether a resolved path is one SameColumnPath can
// decide on: a known layout and a non-negative ordinal at every step.
func statesColumnPath(p *FieldPath) bool {
	if p == nil || len(p.Accessors) == 0 || !p.Domain.IsKnown() {
		return false
	}
	for _, a := range p.Accessors {
		if a.Ordinal < 0 {
			return false
		}
	}
	return true
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
