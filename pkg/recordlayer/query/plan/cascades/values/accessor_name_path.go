package values

import "strings"

// AccessorNamePath returns the ordered accessor NAME path (root QOV/alias
// EXCLUDED) of a plan-time column reference, from the two STRUCTURED
// representations only (RFC-187 §3.0):
//
//	(a) nested Child chain → walk Child, prepend each Field, stop at the root QOV
//	(b) baked Resolved     → Resolved.Accessors[].Field (Child is the root QOV, excluded)
//
// ok=false — a conservative "cannot establish identity", callers MUST NOT
// match — when:
//   - any accessor is pure-ordinal (Field==""): a machinery-owned bake has no
//     name to compare, so identity is asserted-unknown here rather than falling
//     back to a silent name match;
//   - a LAZY Field carries a '.' (flat-dotted "form c"): the string is
//     AMBIGUOUS — a real nested path (addr.city) and an alias-qualified leaf
//     (T.city / leg.COL, composed by clustered_outer_scalar / cascades_translator)
//     are indistinguishable as strings, so splitting on '.' would be the very
//     string hack this kills AND could mis-root an alias as an accessor. We
//     deliberately do NOT split (this also disposes of the quoted-"a.b"-identifier
//     concern — no split, no hack). Such a value must not reach a match site;
//     if one does, ok=false makes it a loud conservative miss, never a wrong bind.
//
// Names are UPPER-cased to the resolver's normalization rule so identity is
// case-insensitive at every accessor.
//
// This is the match-DOMAIN column identity: the candidate side is name-based by
// construction (columnNames []string, no ordinals), so identity can only be
// compared in the name domain. The ordinal identity (FieldPath.Equals) is a
// different, evaluation-domain concern; unifying them by ordinalizing the
// candidate is the RFC-187 §8 / RFC-173-endgame follow-up.
func AccessorNamePath(v Value) ([]string, bool) {
	var rev []string // collected leaf→root, reversed to root→leaf before return
	cur := v
	for {
		fv, ok := cur.(*FieldValue)
		if !ok {
			// Reached a non-FieldValue node: the root (QuantifiedObjectValue and
			// friends — excluded) or nil. The accessor path is what we collected.
			break
		}
		if fv.Resolved != nil {
			// Baked: Resolved carries the ENTIRE accessor path root→leaf; the Child
			// is the root QOV, not more accessors (a FieldPath is held by one node,
			// never chained — values.go FieldPath doc). Emit leaf→root to match the
			// lazy-walk order below. Defensive: if Child is itself a FieldValue we
			// keep walking rather than silently drop its accessors.
			accs := fv.Resolved.Accessors
			for i := len(accs) - 1; i >= 0; i-- {
				if accs[i].Field == "" {
					return nil, false // pure-ordinal accessor: no name to compare
				}
				rev = append(rev, strings.ToUpper(accs[i].Field))
			}
			cur = fv.Child
			continue
		}
		// Lazy.
		if strings.Contains(fv.Field, ".") {
			return nil, false // ambiguous flat-dotted (form c): do not split
		}
		if fv.Field == "" {
			return nil, false // no name to compare
		}
		rev = append(rev, strings.ToUpper(fv.Field))
		cur = fv.Child
	}
	if len(rev) == 0 {
		return nil, false // not a column reference (reached a root with no accessors)
	}
	// Reverse leaf→root into root→leaf.
	for i, j := 0, len(rev)-1; i < j; i, j = i+1, j-1 {
		rev[i], rev[j] = rev[j], rev[i]
	}
	return rev, true
}

// ColumnNamePathsEqual reports whether two plan-time column references denote
// the same accessor path (RFC-187 §3.0). It is the single match-domain
// column-identity notion; every value↔value match site routes through it.
//
// Returns false — a conservative reject (the caller leaves the predicate a
// residual filter / does not elide the sort → correct rows, correct order via
// the slower path) — when either side's AccessorNamePath is !ok, or the paths
// differ at ANY position: every intermediate accessor is compared, not just the
// leaf, not leaf+root — which is what distinguishes a nested path from a
// same-leaf-named top-level column.
//
// CardinalityValue is a transparent wrapper: CARDINALITY(x) matches CARDINALITY(y)
// iff x and y denote the same column. (DistanceRowNumber's metric-class
// discrimination is handled at its call site, which then compares the
// partition/argument column paths through this function.)
func ColumnNamePathsEqual(a, b Value) bool {
	if a == nil || b == nil {
		return false
	}
	if ac, ok := a.(*CardinalityValue); ok {
		bc, ok := b.(*CardinalityValue)
		return ok && ac.Child != nil && bc.Child != nil && ColumnNamePathsEqual(ac.Child, bc.Child)
	}
	if _, ok := b.(*CardinalityValue); ok {
		return false // a is not a CardinalityValue (handled above) → mismatched wrappers
	}
	pa, oka := AccessorNamePath(a)
	if !oka {
		return false
	}
	pb, okb := AccessorNamePath(b)
	if !okb || len(pa) != len(pb) {
		return false
	}
	for i := range pa {
		if pa[i] != pb[i] {
			return false
		}
	}
	return true
}

// CanBridgeOrderingValueRoots reconciles the two safe representation seams an
// ordering request crosses on its way to a scan candidate:
//   - a lazy flat field and the same baked flat field;
//   - a field rooted at its owning SELECT quantifier and the same source-local
//     candidate field.
//
// The second bridge requires exactly one QOV-rooted side and compares the
// complete accessor name path. When both values carry baked paths, their
// ordinal paths must also agree; this prevents a QOV-rooted NAME#1 from
// collapsing with source-local NAME#2. A baked value may still bridge to a
// lazy value with the same complete name path. Callers must scope a request to
// its owning quantifier before using this bridge.
func CanBridgeOrderingValueRoots(left, right Value) bool {
	if CanBridgeOrderingFieldValues(left, right) {
		return true
	}
	leftQOV, leftOK := orderingValueHasQOVRoot(left)
	rightQOV, rightOK := orderingValueHasQOVRoot(right)
	if !leftOK || !rightOK || leftQOV == rightQOV ||
		!ColumnNamePathsEqual(left, right) {
		return false
	}
	leftResolved := orderingValueResolvedPath(left)
	rightResolved := orderingValueResolvedPath(right)
	return leftResolved == nil || rightResolved == nil ||
		leftResolved.Equals(rightResolved)
}

func orderingValueResolvedPath(value Value) *FieldPath {
	if cardinality, ok := value.(*CardinalityValue); ok {
		value = cardinality.Child
	}
	field, ok := value.(*FieldValue)
	if !ok {
		return nil
	}
	return field.Resolved
}

func orderingValueHasQOVRoot(value Value) (bool, bool) {
	if cardinality, ok := value.(*CardinalityValue); ok {
		value = cardinality.Child
	}
	for {
		field, ok := value.(*FieldValue)
		if !ok {
			return false, false
		}
		switch child := field.Child.(type) {
		case nil:
			return false, true
		case *QuantifiedObjectValue:
			return child != nil, child != nil
		case *FieldValue:
			value = child
		default:
			return false, false
		}
	}
}

// AccessorNamePathKey returns a canonical map key for a column reference's
// accessor path (the path segments joined with a NUL separator), or ok=false
// when the path cannot be established (AccessorNamePath !ok). It is the
// map-keyed form of column identity over plain FieldValue references: two such
// references produce the same key iff ColumnNamePathsEqual reports them equal,
// so a set keyed by this string distinguishes a nested addr.city from a
// same-leaf-named top-level city. (It does NOT cover the CardinalityValue
// wrapper ColumnNamePathsEqual also handles — a CardinalityValue yields
// ok=false here; the key-form callers, S7/S9 grouping keys, never pass one.)
func AccessorNamePathKey(v Value) (string, bool) {
	path, ok := AccessorNamePath(v)
	if !ok {
		return "", false
	}
	return strings.Join(path, "\x00"), true
}

// AccessorNamePathMatchesNames reports whether v's accessor name path equals the
// given candidate name path (already UPPER-cased accessor names, root→leaf). It
// is the value↔declared-path form used by the sites whose candidate side is a
// string path (aggregate group/agg columns, ordered index/PK sort columns)
// rather than a Value. A single-element candidate (a top-level column) therefore
// cannot match a nested query path — the fix for those sites (RFC-187 §3.2/§3.3).
func AccessorNamePathMatchesNames(v Value, candidate []string) bool {
	pv, ok := AccessorNamePath(v)
	if !ok || len(pv) != len(candidate) {
		return false
	}
	for i := range pv {
		if pv[i] != strings.ToUpper(candidate[i]) {
			return false
		}
	}
	return true
}
