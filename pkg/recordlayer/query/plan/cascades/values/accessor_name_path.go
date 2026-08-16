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
	// Census: which arm carried this call. See accessor_name_path_census.go for
	// why the '.' decline in particular is worth counting rather than reasoning
	// about. Disabled, this is one atomic load per call.
	census := LegIdentityCensusEnabled()
	sawLazyAccessor := false
	decline := func(c AccessorPathClass) ([]string, bool) {
		if census {
			RecordAccessorPathCall(c)
		}
		return nil, false
	}

	var rev []string // collected leaf→root, reversed to root→leaf before return
	cur := v
	for {
		fv, ok := cur.(*fieldValue)
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
					return decline(AccessorPathDeclinePureOrdinal)
				}
				rev = append(rev, strings.ToUpper(accs[i].Field))
			}
			cur = fv.Child
			continue
		}
		// Lazy.
		sawLazyAccessor = true
		if strings.Contains(fv.Field, ".") {
			// Ambiguous flat-dotted (form c): do not split. THE RATCHET ARM.
			if census {
				RecordAccessorPathDottedWitness(fv.Field)
			}
			return decline(AccessorPathDeclineDotted)
		}
		if fv.Field == "" {
			return decline(AccessorPathDeclineEmptyName)
		}
		rev = append(rev, strings.ToUpper(fv.Field))
		cur = fv.Child
	}
	if len(rev) == 0 {
		return decline(AccessorPathDeclineNotAColumn)
	}
	// Reverse leaf→root into root→leaf.
	for i, j := 0, len(rev)-1; i < j; i, j = i+1, j-1 {
		rev[i], rev[j] = rev[j], rev[i]
	}
	if census {
		// The success split is what says how much of this function's traffic
		// COULD be ordinal-compared today (all-baked) versus how much is waiting
		// on resolve-at-mint (has-lazy).
		if sawLazyAccessor {
			RecordAccessorPathCall(AccessorPathOKHasLazy)
		} else {
			RecordAccessorPathCall(AccessorPathOKAllBaked)
		}
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
// The second bridge normally requires exactly one QOV-rooted side and compares
// the complete accessor name path. There is one deliberately narrower rooted
// exception: a values-owned tagged-current root may bridge to one ordinary
// named root. That is the physical-provider -> logical-request phase boundary;
// two named roots remain ambiguous (self-join hazard), and two independently
// minted current roots name different owner phases. A shared current handle is
// accepted only for an otherwise structurally identical value.
//
// When both values carry baked paths, their ordinal paths must also agree; this
// prevents a QOV-rooted NAME#1 from collapsing with source-local NAME#2. A
// baked value may still bridge to a lazy value with the same complete name
// path. Callers must scope a request to its owning quantifier before using this
// bridge.
func CanBridgeOrderingValueRoots(left, right Value) bool {
	if CanBridgeOrderingFieldValues(left, right) {
		return true
	}
	leftRoot, leftOK := orderingValueQOVRoot(left)
	rightRoot, rightOK := orderingValueQOVRoot(right)
	if !leftOK || !rightOK || !orderingValueTypesEqual(left, right) ||
		!ColumnNamePathsEqual(left, right) {
		return false
	}

	leftRooted := leftRoot != nil
	rightRooted := rightRoot != nil
	switch {
	case leftRooted != rightRooted:
		// Existing qualified-to-source-local bridge. A named value whose text
		// copies the reserved current spelling is not a current owner handle and
		// cannot acquire current's phase privilege through this path.
		root := leftRoot
		if root == nil {
			root = rightRoot
		}
		if isNamedCurrentForgery(root) {
			return false
		}
	case leftRooted && rightRooted:
		leftCurrent := leftRoot.correlation.isCurrent()
		rightCurrent := rightRoot.correlation.isCurrent()
		if leftCurrent == rightCurrent {
			// Two ordinary roots are ambiguous. Two current roots are the same
			// phase only when they share the exact owner-minted handle.
			return leftCurrent && leftRoot == rightRoot &&
				ValuesStructurallyEqual(left, right)
		}
		namedRoot := leftRoot
		if leftCurrent {
			namedRoot = rightRoot
		}
		if isNamedCurrentForgery(namedRoot) {
			return false
		}
	default:
		return false
	}

	leftResolved := orderingValueResolvedPath(left)
	rightResolved := orderingValueResolvedPath(right)
	return leftResolved == nil || rightResolved == nil ||
		leftResolved.Equals(rightResolved)
}

func orderingValueTypesEqual(left, right Value) bool {
	leftType, rightType := left.Type(), right.Type()
	return leftType != nil && rightType != nil && leftType.Equals(rightType)
}

// isNamedCurrentForgery reports whether root is an ORDINARY named correlation
// whose text copies the reserved current spelling. Such a root must not acquire
// current's phase privilege through the bridge above.
//
// The comparison FOLDS CASE. The reserved handle is spelled `_current` in
// lowercase, but a user correlation reaches here through the SQL path, which
// upper-folds every alias — so `FROM t AS _current` arrives as `_CURRENT` and an
// exact-match guard waves it straight through, which is precisely the input the
// guard exists to catch. Nothing else distinguishes the two: correlationKind is
// private and cannot be forged, so the KIND check below is what proves this is
// not the real handle, and this one is what recognises the impostor.
func isNamedCurrentForgery(root *quantifiedObjectValue) bool {
	return root != nil && !root.correlation.isCurrent() &&
		strings.EqualFold(root.correlation.Name(), CurrentCorrelation().Name())
}

func orderingValueResolvedPath(value Value) *fieldPath {
	if cardinality, ok := value.(*CardinalityValue); ok {
		value = cardinality.Child
	}
	field, ok := value.(*fieldValue)
	if !ok {
		return nil
	}
	return field.Resolved
}

func orderingValueQOVRoot(value Value) (*quantifiedObjectValue, bool) {
	if cardinality, ok := value.(*CardinalityValue); ok {
		value = cardinality.Child
	}
	for {
		field, ok := value.(*fieldValue)
		if !ok {
			return nil, false
		}
		switch child := field.Child.(type) {
		case nil:
			return nil, true
		case *quantifiedObjectValue:
			return child, child != nil
		case *fieldValue:
			value = child
		default:
			return nil, false
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
