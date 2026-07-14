package query

import (
	"strings"

	"google.golang.org/protobuf/reflect/protoreflect"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/relational/api"
	"fdb.dev/pkg/relational/core/query/logical"
)

// RFC-173 unnest-residual class 4 — CHAINED lateral unnests (`FROM t, t.arr AS
// x, x.sub AS y`). The second unnest's OWNER `x` is the FIRST unnest's element,
// not a table, so segment-0 doesn't resolve to a scan and the single-unnest
// path bails to a table-not-found. Java calls generateCorrelatedFieldAccess
// once per link, each a nested Explode-under-forEach; the residual composition
// mirrors that (translateRef(j.Left) recurses through the left-deep join tree,
// so the nested FlatMap-over-FlatMap falls out).
//
// Runtime reuses the class-2 struct-descent VERBATIM: a struct-array element
// flows as a raw proto.Message, so the second unnest's collection is a
// multi-accessor FieldValue rooted at the owner alias (reads the element off
// the merged row) with the sub-field descended by name. The only new
// work is CLASSIFICATION — arrayFieldElementType collapses message-array
// elements to UnknownType, so the sub-array's element type is recovered from
// the proto DESCRIPTOR through the owner unnest's element message (the same
// descriptor-is-the-only-surviving-type finding as class 3).

// classifyChainedUnnestArray classifies the array field of a CHAINED unnest
// (owner = a prior unnest's element). Returns the sub-array's element type and
// the OUTPUT name the collection reads (the owner element's field name), plus a
// disposition reusing the derived enum. The owner chain must bottom at a
// REAL-TABLE scan (a CTE/derived/box root declines — design ruling 4,
// condition 4).
func (t *cascadesTranslator) classifyChainedUnnestArray(outerLeft logical.LogicalOperator, u *logical.LogicalUnnest) (elementType values.Type, fieldName string, disp derivedUnnestDisposition) {
	if len(u.Segments) != 2 {
		// A multi-segment chained path (`x.a.b`) descends the element struct
		// further — out of the class-4 single-sub-field whitelist for now.
		return values.UnknownType, "", derivedUnnestUnsupported
	}
	elemMsg, scalar, ok := t.chainedOwnerElementMessage(outerLeft, u.Segments[0])
	if !ok {
		// Owner not found, or its chain doesn't bottom at a real table
		// (CTE/derived/box root) — loud decline.
		return values.UnknownType, "", derivedUnnestUnsupported
	}
	if scalar {
		// The owner's element is a SCALAR — it has no field `sub` to read.
		// Java's lookupNestedField soft-returns empty for a non-struct base →
		// resolveIdentifier miss → UNDEFINED_COLUMN.
		return values.UnknownType, "", derivedUnnestUndefined
	}
	et, name, isArray, present := arrayFieldFromDescriptor(elemMsg.Fields(), u.Segments[1:])
	switch {
	case isArray:
		return et, name, derivedUnnestArray
	case present:
		// Present-but-scalar sub → Java's generateCorrelatedFieldAccess
		// "repeated type" assert (INVALID_COLUMN_REFERENCE, aligned above).
		return values.UnknownType, "", derivedUnnestWrongType
	default:
		return values.UnknownType, "", derivedUnnestUndefined
	}
}

// chainedOwnerElementMessage returns the message descriptor of each ELEMENT of
// the unnest named `alias` (a struct-array element's record type). Recursive:
// the owner unnest's array field lives on either a base-table record (scan
// root — the recursion's base case) or the element message of ITS OWN owner
// unnest (a deeper chain link). scalar=true when the element is a scalar (no
// message — the caller maps a sub-field read on it to UNDEFINED_COLUMN).
// ok=false when the unnest isn't found or the chain bottoms at a non-real-table
// owner (CTE/derived/box — condition 4's loud decline).
func (t *cascadesTranslator) chainedOwnerElementMessage(outerLeft logical.LogicalOperator, alias string) (md protoreflect.MessageDescriptor, scalar, ok bool) {
	u := logical.FindOwnerUnnest(outerLeft, alias)
	if u == nil || len(u.Segments) != 2 {
		return nil, false, false
	}
	// The record the owner's array field `u.Segments[1]` lives on. The base
	// branch is a REAL-TABLE scan ONLY: exclude both a CTE reference
	// (outerSourceIsCTE) AND a derived-table primary `(SELECT…) AS D`
	// (outerSourceIsDerivedTable) — condition 4 declines a CTE/derived-rooted
	// chain loudly. The derived guard is structural, not just a resolveRecordType
	// miss: a derived alias `D` that SHADOWS a real table `D` (with a matching
	// struct-array) would otherwise bottom at the base-table descriptor here
	// (the derived body isn't in cteScope until translateRef(j.Left) runs — the
	// P2a timing hole class-3 closes structurally), yielding wrong element-type
	// metadata. Declining routes it to the loud 0AF00.
	var base protoreflect.FieldDescriptors
	if scanTable := findOuterScanTable(outerLeft, u.Segments[0]); scanTable != "" &&
		!t.outerSourceIsCTE(scanTable) && !outerSourceIsDerivedTable(outerLeft, u.Segments[0]) {
		rt := t.resolveRecordType(scanTable)
		if rt == nil || rt.Descriptor == nil {
			return nil, false, false
		}
		base = rt.Descriptor.Fields()
	} else if inner := logical.FindOwnerUnnest(outerLeft, u.Segments[0]); inner != nil {
		// A deeper chain link: the owner's own owner is a prior unnest —
		// recurse to its element message (the record the owner's array lives
		// on). A scalar-element owner-of-owner cannot carry an array field.
		innerMsg, innerScalar, innerOK := t.chainedOwnerElementMessage(outerLeft, u.Segments[0])
		if !innerOK || innerScalar {
			return nil, innerScalar, false
		}
		base = innerMsg.Fields()
	} else {
		// CTE/derived/box owner root — no base descriptor (condition 4).
		return nil, false, false
	}
	// Descend the owner's array FIELD PATH (u.Segments[1:] — segment 0 is the
	// owner's own source, already resolved into `base`; the field path lives
	// under it): intermediates singular message, final repeated. A repeated
	// MESSAGE final yields the element message; a repeated SCALAR final yields
	// scalar=true.
	arrFd, ok := descendToArrayField(base, u.Segments[1:])
	if !ok {
		return nil, false, false
	}
	if arrFd.Kind() != protoreflect.MessageKind {
		return nil, true, true // scalar element
	}
	return arrFd.Message(), false, true
}

// descendToArrayField walks a field path over a proto record's fields:
// intermediates must be singular message fields, the final must be repeated.
// Returns the final (repeated) field descriptor, or ok=false when a step is
// absent or a non-descendable intermediate.
func descendToArrayField(fields protoreflect.FieldDescriptors, path []string) (protoreflect.FieldDescriptor, bool) {
	if len(path) == 0 {
		return nil, false
	}
	for _, seg := range path[:len(path)-1] {
		fd := protoFieldLookup(fields, seg)
		if fd == nil || fd.IsList() || fd.Kind() != protoreflect.MessageKind {
			return nil, false
		}
		fields = fd.Message().Fields()
	}
	fd := protoFieldLookup(fields, path[len(path)-1])
	if fd == nil || !fd.IsList() {
		return nil, false
	}
	return fd, true
}

// isChainedUnnest reports whether u's owner (segment 0) is a prior lateral
// unnest's element in outerLeft (the positive gate for the chained dispatch,
// design ruling 4 condition 2 — never merely outerTable=="").
func isChainedUnnest(outerLeft logical.LogicalOperator, u *logical.LogicalUnnest) bool {
	return len(u.Segments) >= 1 && logical.FindOwnerUnnest(outerLeft, u.Segments[0]) != nil
}

// filterInputHasChainedUnnest reports whether a filter's input contains a CHAINED
// lateral unnest (`… t.a AS x, x.b AS y …` — a LogicalUnnest whose owner segment-0
// is a prior unnest's element) IN THE JOIN SPINE the filter directly filters. The
// typed gate for the scalar-subquery-over-chained narrowed decline in
// translateFilter. It walks the whole JOIN SPINE — not just the direct rightmost
// source — so a chained unnest buried behind a trailing table (`FROM t, t.a AS x,
// x.b AS y, z`) or in a join leg (`FROM (t, t.a AS x, x.b AS y), B`) is still caught
// and its scalar-subquery predicate rejected LOUDLY rather than shipping the
// name-model silent-wrong `[]`. It STOPS at a relation boundary (Project / CTE /
// Aggregate / Union / Distinct / …): a chained unnest ENCAPSULATED in a derived
// table produces a materialized relation this predicate is NOT correlated to, so
// its scalar subquery never rides the chained positional bake — descending there
// would falsely reject a working shape.
func filterInputHasChainedUnnest(input logical.LogicalOperator) bool {
	found := false
	var walk func(logical.LogicalOperator)
	walk = func(o logical.LogicalOperator) {
		if found || o == nil {
			return
		}
		switch n := o.(type) {
		case *logical.LogicalUnnest:
			if len(n.Segments) >= 1 && logical.FindOwnerUnnest(input, n.Segments[0]) != nil {
				found = true
			}
		case *logical.LogicalJoin:
			// Descend ONLY the join spine — the direct FROM the filter filters.
			for _, c := range n.Children() {
				walk(c)
			}
		}
	}
	walk(input)
	return found
}

// translateChainedUnnestJoin lowers a chained lateral unnest (`FROM t, t.arr AS
// x, x.sub AS y`, second unnest owned by the first's element) into the residual
// nested FlatMap composition. The nesting falls out of translateRef(j.Left)
// recursing through the left-deep join tree (the first unnest is the outer's
// own SelectExpression).
//
// RFC-173 S4 Slice 1: when the FIRST link ITSELF ordinalizes (a single-source
// base whose W4c binary seed gate at :1565 passes), the chained link takes an
// ORDINAL result-value seed (unnestOrdinalSeed) + a POSITIONAL collection
// (unnestBakedRootCollection rooted at the owner-alias column) instead of the
// name-model buildUnnestResultValue + chainedUnnestCollection. This retires the
// :1151 enclosure's chained-unnest residency: the ordinal seed carries the
// outer's own columns positionally (T4.ID etc.), so clearing that enclosure no
// longer strands the chained row (`field "T4.ID" not resolvable … ordinal -1`).
// The enclosure is CLEARED only for the first link's own translateRef (so it
// ordinalizes) and restored immediately; the rest of the lowering keeps the bit.
//
// FAIL-OPEN: any decline — the chained gate not met (enclosed, deeper-chain
// base, non-single-source base), or a nil seed/collection — falls through to the
// EXISTING name-model path (buildUnnestResultValue + chainedUnnestCollection),
// which stays the residual for the bare-twin / CTE-rooted / 3+-link cases.
//
// nil with a set translate error on a loud classification failure; nil with no
// error to decline to the caller's fallback.
func (t *cascadesTranslator) translateChainedUnnestJoin(j *logical.LogicalJoin, u *logical.LogicalUnnest, prevEnclosure bool) expressions.RelationalExpression {
	if u.Alias != "" && u.AtAlias != "" && strings.EqualFold(u.Alias, u.AtAlias) {
		// Mirror translateUnnestJoin's AS==AT overwrite guard: buildUnnestResultValue
		// appends the element and the ordinal under the SAME bare+qualified name, and
		// the map-keyed RecordConstructorValue.Evaluate silently OVERWRITES the element
		// with the ordinal — `... AS Y AT Y` makes `SELECT Y` return the ordinal, not
		// the unnested value. The chained path returns before the non-chained guard, so
		// it must repeat the check. Java binds AS and AT to two distinct quantifier
		// columns (visitAtomTableItem); a duplicate is a binding error upstream. RFC-142.
		t.setTranslateErr(api.NewError(api.ErrCodeDuplicateAlias,
			"lateral unnest AS and AT aliases must be distinct; use different names for the element and the ordinal"))
		return nil
	}
	elementType, _, disp := t.classifyChainedUnnestArray(j.Left, u)
	switch disp {
	case derivedUnnestArray:
		// proceed
	case derivedUnnestWrongType:
		t.setTranslateErr(api.NewError(api.ErrCodeInvalidColumnReference,
			"join correlation can occur only on a column of repeated (array) type"))
		return nil
	case derivedUnnestUndefined:
		t.setTranslateErr(api.NewErrorf(api.ErrCodeUndefinedColumn,
			"column %q does not exist on source %q",
			strings.Join(u.Segments[1:], "."), u.Segments[0]))
		return nil
	default: // derivedUnnestUnsupported — two distinct causes, both loud 0AF00
		if len(u.Segments) > 2 {
			// A multi-HOP sub-path on the element (`x.a.b AS y`) — Java DOES
			// support this (a further struct descent per link); it's a Go reach
			// gap, not a CTE-root decline. Name the real cause.
			t.setTranslateErr(api.NewError(api.ErrCodeUnsupportedQuery,
				"multi-segment chained unnest sub-path (x.a.b) is not yet supported"))
		} else {
			t.setTranslateErr(api.NewError(api.ErrCodeUnsupportedQuery,
				"chained lateral unnest rooted at a CTE/derived-table source is not yet supported"))
		}
		return nil
	}

	outerAlias := sourceAlias(j.Left)
	outerCorr := values.NamedCorrelationIdentifier(outerAlias)
	innerCorr := unnestSourceCorrelation(u)

	// RFC-173 S4 Slice 1: try the ORDINAL seed when the FIRST link ordinalizes.
	// Mirror the single-source binary seed gate (:1565) applied to the FIRST
	// LINK's own base: the chained link ordinalizes iff, with the enclosure bit
	// cleared, translateRef(j.Left) would take the first link's :1565 path — a
	// single-source base (clusterArity==1), exists-safe, and both links' segment
	// paths present. The gate is decided (and the seed/collection built) BEFORE
	// translating outerRef, so a decline never leaves an ORDINAL first link under
	// the name-model builder (which reads it by name → wrong rows).
	if sel := t.translateChainedUnnestOrdinal(j, u, outerAlias, outerCorr, innerCorr, elementType, prevEnclosure); sel != nil {
		return sel
	}

	// RFC-173 S4 cap (gap 1): the chained ordinal seed DECLINED. A chained lateral
	// unnest whose spine BOTTOMS in a FULL OUTER box that could not ordinalize (a
	// box-leg predicate the box-leg window composition cannot yet fold into the
	// chained seed, or a nested outer box) has no ordinal representation, and the
	// name-model residual retires at the cap — LOUD-REJECT rather than fall to it.
	// This is Java-aligned: Java rejects FULL OUTER JOIN at the grammar level
	// (RelationalParser.g4 `joinPart` has no FULL alternative; QueryVisitor asserts
	// ErrCodeUnsupportedQuery "FULL OUTER JOIN is not currently supported"), so a
	// lateral unnest over a FULL box is a shape Java cannot express at all — it is a
	// Go-only extension whose reach we cap here rather than sink into a per-leg window
	// composition for a shape Java lacks entirely. Plain FULL-box unnest (single link,
	// translateUnnestJoin) and the UNFILTERED chained FULL-box spine that DO ordinalize
	// return non-nil above and never reach here — only the un-ordinalizable straddle does.
	if chainedSpineBottomsInFullBox(j.Left) {
		t.setTranslateErr(api.NewError(api.ErrCodeUnsupportedQuery,
			"lateral unnest over a FULL OUTER JOIN with a join-leg predicate is not supported"))
		return nil
	}

	// The ordinal gate DECLINED and no cap matched (an inadmissible spine —
	// twin-fork ownership, an underivable leg). The name-model residual
	// composition is DELETED (RFC-173 S4 item B — the full-suite producer
	// census fired zero, so no production shape lands here); the chain is
	// untranslatable — LOUD, never silent wrong rows.
	if t.translateErr == nil {
		t.setTranslateErr(api.NewError(api.ErrCodeUnsupportedQuery,
			"chained lateral unnest did not ordinalize (inadmissible spine)"))
	}
	return nil
}

// chainedSpineBottomsInFullBox reports whether a chained-unnest outer subtree
// bottoms in a FULL OUTER box — the RFC-173 S4 cap loud-reject discriminant for
// gap 1. Peels the left-deep lateral-unnest joins (Right is a LogicalUnnest) off
// the top exactly as chainedSpineWalk does, then reports whether the remaining
// bottom operator is a FULL join. A nested outer box `(A LEFT B) FULL C` bottoms
// in the outermost FULL join and is detected too. Only consulted AFTER the ordinal
// seed declined, so an ordinalizing FULL-box spine never reaches it.
func chainedSpineBottomsInFullBox(op logical.LogicalOperator) bool {
	cur := op
	for {
		bj, ok := cur.(*logical.LogicalJoin)
		if !ok {
			return false
		}
		if _, isUnnest := bj.Right.(*logical.LogicalUnnest); isUnnest {
			cur = bj.Left
			continue
		}
		return bj.Kind == logical.JoinFull
	}
}

// chainedSpineBottomOuterBox peels a chained-unnest outer subtree's lateral
// links (Right is a LogicalUnnest) off the top — the same peel as
// chainedSpineBottomsInFullBox — and returns the remaining bottom operator IF
// it is a LEFT/RIGHT OUTER box. nil for every other bottom (a plain scan, an
// INNER cluster, a FULL box — each has its own arm). The translateFilter WHERE
// path consults it to LOUD-REJECT a box-leg WHERE conjunct over a chained
// LEFT/RIGHT box (the un-ordinalizable straddle, the S4 cap).
func chainedSpineBottomOuterBox(op logical.LogicalOperator) *logical.LogicalJoin {
	cur := op
	sawLink := false
	for {
		bj, ok := cur.(*logical.LogicalJoin)
		if !ok {
			return nil
		}
		if _, isUnnest := bj.Right.(*logical.LogicalUnnest); isUnnest {
			sawLink = true
			cur = bj.Left
			continue
		}
		if !sawLink || (bj.Kind != logical.JoinLeft && bj.Kind != logical.JoinRight) {
			return nil
		}
		return bj
	}
}

// chainedSpineWalk PEELS a chained-unnest outer into its lateral links
// (bottom-most first) and reports whether the spine is ADMITTED to the ordinal
// seed — the single walk authority the chained gate and chainedOwnerElementSlot
// both consume. One iterative pass: strip unnest-right joins off the left-deep
// tree, then check the two admission laws over the peeled links.
//
// ADMISSION LAW 1 — the bottom: whatever remains under the deepest link must be
// a SINGLE lateral source (clusterArity 1 — a plain scan through transparent
// wrappers, or a merge-opaque FULL box). A MULTI-source BOX bottom
// (clusterArity ≥ 2 — `FROM t, u, t.arr AS x, x.sub AS y`, whose arr link sits
// on the box `t ⋈ u`) is DECLINED: its positional owner windows are the c5b
// buried-box concern, and the innermost-Explode predicate placement is unproven
// over a box base. It fails open to the name-model residual (pinned: a box-base
// chain answers correctly via the name-key rebase, no strand).
//
// ADMISSION LAW 2 — ownership: every link ABOVE the first must consume the
// element of exactly ONE deeper link, resolved BY ALIAS within this same walk
// (no second spine walk). Forks are therefore admitted — `…, x.substruct AS y,
// x.sub AS w` (w's owner x is two links back) is as valid as a linear chain,
// because the collection root is computed from the OWNER link's element slot
// (chainedOwnerElementSlot), never positionally from the preceding link. The
// first link's owner is the bottom source itself, validated by the chained
// classification machinery. An owner alias matching ZERO deeper links (a
// table-owned mid-spine unnest — upstream-rejected as multiple lateral unnests)
// or MORE THAN ONE (duplicate FROM aliases — 42712-loud upstream; this arm is
// defensive) declines to name-model. The per-link len(Segments)<2 check is
// load-bearing, not decorative: a 1-segment LogicalUnnest is constructible via
// the AT-source parser path, and such a malformed link must decline
// conservatively.
//
// The seed machinery (ordinalLegColumns/unnestOrdinalSeed/unnestBakedRootCollection)
// accumulates the merged row per link for arbitrary depth (rfc173_ordinal_seed.go),
// so any admitted spine seeds correctly regardless of length or fork topology.
//
// pureSpine reports whether the spine BOTTOMS at a source binding exactly ONE
// alias. A FULL OUTER box is ALSO clusterArity==1 (merge-opaque) and therefore
// ADMITTED — but it binds its LEG aliases, which are genuine BOX LEGS, not
// chain links: the box-leg-conjunct arm of unnestExistsSeedSafe must stay
// ACTIVE for it (pureSpine=false), or a box-leg WHERE ordinalizes the chained
// link while the first link's own gate keeps a name-model seed over the box —
// an ordinal read over a name-keyed row, SILENTLY WRONG rows. The discriminator
// is the arm's own authority (outerBoundAliases == 1), not a structural box
// probe, so any future single-arity multi-alias source stays conservatively
// impure.
func (t *cascadesTranslator) chainedSpineWalk(op logical.LogicalOperator) (links []chainedSpineLink, admitted, pureSpine bool) {
	// Peel the unnest-right joins off the left-deep spine, outermost-first.
	var rev []chainedSpineLink
	cur := op
	for {
		bj, ok := cur.(*logical.LogicalJoin)
		if !ok {
			break
		}
		un, isU := bj.Right.(*logical.LogicalUnnest)
		if !isU {
			break // a non-unnest join: the spine BOTTOM (a box)
		}
		if len(un.Segments) < 2 {
			return nil, false, false
		}
		rev = append(rev, chainedSpineLink{join: bj, un: un})
		cur = bj.Left
	}
	// The spine BOTTOM must compose an ordinal row. Three admitted shapes:
	//   - clusterArity 1: a single lateral source (a plain scan through
	//     transparent wrappers) or a merge-opaque FULL box.
	//   - a MULTI-source gated INNER cluster (`FROM t, u, t.arr AS x, x.sub AS
	//     y` — the arr link's base is the box `t ⋈ u`): its per-leg windows
	//     compose into the chained merged row (ordinalLegColumns recurses into
	//     the box arm; buriedLegBounds records each leaf's [Start,Width) window)
	//     and the FIRST link over it ordinalizes via the GATHERED path
	//     (translateGatheredUnnestCluster).
	//   - a gated LEFT/RIGHT OUTER box (`a LEFT b [LEFT c], a.arr AS x, x.sub
	//     AS y` — single or nested): the FIRST link gathers it as ONE OPAQUE
	//     leg through the SAME fresh-gate authority
	//     (translateGatheredUnnestCluster's ordinalWedgeGateDecide probe =
	//     gatesAsFreshCluster), the box births its whole leg-concat
	//     positionally (null-supplied legs NULL in their windows), and
	//     ordinalLegColumns' join arm composes the identical concat into the
	//     chained merged row. The name-model residual CANNOT serve this shape
	//     at all post-cap (its chainedUnnestCollection descends by name over a
	//     row with no name keys — a guaranteed loud ordinal-(-1) strand at
	//     execution), so ordinalizing here is the only working representation
	//     for the ELEMENT / leg-projection / element-or-AT-WHERE shapes. A
	//     box-leg WHERE conjunct over this bottom is the un-ordinalizable
	//     straddle (the merged-corr rebase collides with the first link's inner
	//     Explode; a box-quantifier bake sinks below the nested outer
	//     null-extension into the null-supplied scan) — it is set Unbakeable
	//     upstream and DECLINES here, then translateFilter LOUD-REJECTS it (the
	//     S4 cap; never wrong rows). FULL stays on the clusterArity==1 arm above
	//     (pureSpine=false, box-leg-conjunct arm active + the cap's FULL-box
	//     reject).
	bottomInnerBox := false
	bottomOuterBox := false
	if t.clusterArity(cur) != 1 {
		bj, isJoin := cur.(*logical.LogicalJoin)
		if !isJoin || len(bj.OnExistsSubqueries) > 0 || !t.gatesAsFreshCluster(bj) {
			return nil, false, false
		}
		switch bj.Kind {
		case logical.JoinInner:
			bottomInnerBox = true
		case logical.JoinLeft, logical.JoinRight:
			if t.unnestBoxLegConjunct == boxConjUnbakeable {
				return nil, false, false
			}
			bottomOuterBox = true
		default:
			return nil, false, false
		}
	}
	links = make([]chainedSpineLink, 0, len(rev))
	for i := len(rev) - 1; i >= 0; i-- {
		links = append(links, rev[i])
	}
	for i := 1; i < len(links); i++ {
		matches := 0
		for k := range i {
			if links[k].un.Alias != "" && strings.EqualFold(links[i].un.Segments[0], links[k].un.Alias) {
				matches++
			}
		}
		if matches != 1 {
			return nil, false, false
		}
	}
	// pureSpine reports whether the chained rebase authority handles EVERY
	// reachable outer ref POSITIONALLY — true for a single-source bottom, for
	// a gated INNER box bottom, and for a gated LEFT/RIGHT OUTER box bottom
	// (all compose per-leg ordinal windows: a single-namespace prefix, or the
	// box's buried-leaf windows via buriedLegBounds/ordinalSlotInLegWindow).
	// These exempt the box-leg-conjunct decline arm (unnestExistsSeedSafe) — a
	// pure/INNER/LEFT-RIGHT spine has no box-leg WHERE that reaches the ordinal
	// seed here (an INNER-box straddle bakes over the gather record; a
	// LEFT/RIGHT-box straddle is Unbakeable and already declined above; a FULL
	// box keeps pureSpine=false so the arm stays active and the cap
	// loud-rejects). The LEFT/RIGHT bottom is admitted ONLY for the
	// element/leg-projection/element-WHERE shapes, whose refs are all
	// positional over the box's per-leg windows (buriedLegBounds/
	// ordinalSlotInLegWindow), null-supplied legs serving NULL in their slots.
	return links, true, bottomInnerBox || bottomOuterBox || len(outerBoundAliases(cur)) == 1
}

// chainedSpineLink is one lateral-unnest link of a chained spine as peeled by
// chainedSpineWalk, bottom-most first: join's Right IS un, and join.Left is
// everything below the link (the merged-row prefix its element appends to).
type chainedSpineLink struct {
	join *logical.LogicalJoin
	un   *logical.LogicalUnnest
}

// rotateBuriedChainedSpine probes the BURIED chained-spine class — a CHAINED
// lateral-unnest spine followed by TRAILING plain comma legs (`FROM t, t.arr AS
// x, x.sub AS y, z`) — and, when it classifies, ROTATES the cluster to the
// BOX-BOTTOM chained form the ordinal machinery owns: the trailing legs join
// the spine's bottom (`FROM t, z, t.arr AS x, x.sub AS y`), so the spine's top
// link is the rightmost source and the whole chain takes the chained ordinal
// dispatch (chainedSpineWalk admits the widened INNER-box bottom; the ordinal
// seed composes the box's per-leg windows). Without the rotation the spine is
// a buried LEG of the trailing join, which name-models the parent (the
// unnest-right gate poison) — the exact producer-caller class RFC-173 item B
// retires. The chained twin of rotateEnclosedUnnest (which owns the
// SINGLE-link buried class and declines chains); like it, the rotation is
// inner-join-equivalent — every link references only sources at or below its
// own spine position, and a trailing leg is independent of the links, so
// re-parenting it below the spine preserves rows and multiplicities.
//
// Fail-open (ok=false keeps the ORIGINAL tree): declines on any ON-carrying
// or non-INNER join in the peel (an ON conjunct may reference a link element,
// which is out of scope below the links), a single-link spine
// (rotateEnclosedUnnest's), a non-chained top link, or a rotated spine the
// chained walk does NOT admit — so a shape the ordinal path cannot serve
// keeps today's name-model translation instead of trading it for a decline.
func (t *cascadesTranslator) rotateBuriedChainedSpine(j *logical.LogicalJoin) (*logical.LogicalJoin, bool) {
	if t.md == nil || j.Kind != logical.JoinInner || len(j.OnExistsSubqueries) > 0 ||
		j.OnPredicate != nil || j.OnText != "" {
		return nil, false
	}
	if _, rightUnnest := j.Right.(*logical.LogicalUnnest); rightUnnest {
		return nil, false // root form — the unnest dispatch owns it
	}
	// Peel the TRAILING legs (collected in REVERSE FROM order) off the
	// left-deep tree until the spine's top link surfaces.
	var trailingRev []logical.LogicalOperator
	cur := j
	var spineTop *logical.LogicalJoin
	for {
		trailingRev = append(trailingRev, cur.Right)
		lj, isJ := cur.Left.(*logical.LogicalJoin)
		if !isJ {
			return nil, false
		}
		if _, isU := lj.Right.(*logical.LogicalUnnest); isU {
			spineTop = lj
			break
		}
		if lj.Kind != logical.JoinInner || len(lj.OnExistsSubqueries) > 0 ||
			lj.OnPredicate != nil || lj.OnText != "" {
			return nil, false
		}
		cur = lj
	}
	// Collect the spine's link joins, top-down, and its bottom.
	var linksTopDown []*logical.LogicalJoin
	var bottom logical.LogicalOperator
	for sj := spineTop; ; {
		un, isU := sj.Right.(*logical.LogicalUnnest)
		if !isU {
			bottom = sj
			break
		}
		if sj.Kind != logical.JoinInner || len(sj.OnExistsSubqueries) > 0 ||
			sj.OnPredicate != nil || sj.OnText != "" || len(un.Segments) < 2 {
			return nil, false
		}
		linksTopDown = append(linksTopDown, sj)
		nj, isJ := sj.Left.(*logical.LogicalJoin)
		if !isJ {
			bottom = sj.Left
			break
		}
		sj = nj
	}
	// Only a genuinely CHAINED spine (≥2 links, top link owned by a deeper
	// link's element): the single-link buried class rotates via
	// rotateEnclosedUnnest into the FLAT gathered form instead.
	if len(linksTopDown) < 2 ||
		!isChainedUnnest(spineTop.Left, spineTop.Right.(*logical.LogicalUnnest)) {
		return nil, false
	}
	// DEFENSE-IN-DEPTH (rotation safety): each trailing leg is reparented BELOW the
	// spine links, so it must NOT laterally correlate to a link's element/AT alias —
	// a leg that read a spine element would find it out of scope once moved below.
	// The only lateral construct in the FROM comma-list is an UNNEST, and a trailing
	// unnest owned by a spine element is already peeled as a FORK by chainedSpineWalk
	// (never a plain trailing leg), so this is unreachable on today's surface. Assert
	// it explicitly rather than rely on that reachability: decline the rotation if any
	// trailing leg's subtree unnests off a spine link's alias. (A future lateral
	// derived-table syntax would need the same decline — correct-or-name-model, never
	// wrong rows.)
	spineAliases := make(map[string]struct{}, 2*len(linksTopDown))
	for _, lk := range linksTopDown {
		if un, ok := lk.Right.(*logical.LogicalUnnest); ok {
			if un.Alias != "" {
				spineAliases[strings.ToUpper(un.Alias)] = struct{}{}
			}
			if un.AtAlias != "" {
				spineAliases[strings.ToUpper(un.AtAlias)] = struct{}{}
			}
		}
	}
	for _, leg := range trailingRev {
		if subtreeUnnestsOffAlias(leg, spineAliases) {
			return nil, false
		}
	}
	// Rebuild: bottom ⋈ trailing legs (FROM order) as the new spine bottom,
	// then the links re-stacked bottom-most first.
	newOp := bottom
	for i := len(trailingRev) - 1; i >= 0; i-- {
		newOp = &logical.LogicalJoin{Left: newOp, Right: trailingRev[i], Kind: logical.JoinInner}
	}
	for i := len(linksTopDown) - 1; i >= 0; i-- {
		newOp = &logical.LogicalJoin{Left: newOp, Right: linksTopDown[i].Right, Kind: logical.JoinInner}
	}
	rotated := newOp.(*logical.LogicalJoin)
	// Admission probe: rotate only when the rotated spine ORDINALIZES
	// (chainedSpineWalk is side-effect-free). A declined rotated form (a poison
	// trailing leg, an inadmissible bottom) keeps the original tree — its
	// name-model translation answers correct rows today, while the rotated
	// residual is an unverified shape.
	if _, admitted, _ := t.chainedSpineWalk(rotated.Left); !admitted {
		return nil, false
	}
	return rotated, true
}

// subtreeUnnestsOffAlias reports whether op's subtree contains a LogicalUnnest
// whose OWNER (Segments[0]) is one of `aliases` — i.e. a lateral unnest correlated
// to that alias. rotateBuriedChainedSpine uses it as a defense-in-depth guard: a
// trailing leg that laterally reads a spine element cannot be reparented below the
// spine links. Case-insensitive to match the alias-comparison convention here.
func subtreeUnnestsOffAlias(op logical.LogicalOperator, aliases map[string]struct{}) bool {
	if op == nil {
		return false
	}
	if un, ok := op.(*logical.LogicalUnnest); ok && len(un.Segments) > 0 {
		if _, hit := aliases[strings.ToUpper(un.Segments[0])]; hit {
			return true
		}
	}
	for _, c := range op.Children() {
		if subtreeUnnestsOffAlias(c, aliases) {
			return true
		}
	}
	return false
}

// chainedOwnerElementSlot resolves ownerAlias to exactly one walked link and
// returns its ELEMENT's slot in the merged ordinal row. The layout law (pinned
// per AT-combination in the slot tests): each link appends [element, AT?] to
// the row, so the element is always the FIRST column its link contributes —
// slot = len(ordinalLegColumns(owner.join.Left)) — invariant under the owner's
// own AT ordinal (which FOLLOWS the element), under downstream links (which
// append after), and under upstream AT columns (counted inside the prefix, an
// AT-only upstream link contributing ONE column included). ok=false declines to
// name-model: absent alias, a defensive duplicate (42712-loud upstream), or an
// underivable prefix. NEVER resolve the root by name — an outer scalar with
// the owner's name precedes the element in the merged row and would shadow it.
func (t *cascadesTranslator) chainedOwnerElementSlot(links []chainedSpineLink, ownerAlias string) (int, bool) {
	ownerIdx := -1
	for i, l := range links {
		if l.un.Alias != "" && strings.EqualFold(ownerAlias, l.un.Alias) {
			if ownerIdx >= 0 {
				return 0, false
			}
			ownerIdx = i
		}
	}
	if ownerIdx < 0 {
		return 0, false
	}
	prefix := t.ordinalLegColumns(links[ownerIdx].join.Left)
	if prefix == nil {
		return 0, false
	}
	return len(prefix), true
}

// chainedUnnestOrdinalGate decides — SIDE-EFFECT-FREE — whether a chained
// unnest join (j, u) takes the ordinal seed, and builds the baked collection +
// seed result value it would carry. ok=false DECLINES (the translation caller
// fails open; the star-body admission stays un-admitted). Factored out of
// translateChainedUnnestOrdinal so the star-body admission
// (derivedBodyStarOrdinalLeg) consumes the EXACT gate the fresh body
// translation runs — an admission/translation drift would type a derived
// boundary positionally over a residual body (misaligned reads), so both
// consumers must share one predicate.
//
// The spine walk peels j.Left (the WHOLE outer) into its links, admitting a
// spine whose bottom is a single lateral source (clusterArity 1 — a plain
// source or a merge-opaque FULL box) with every above-first link's owner
// resolving to exactly one deeper link (forks included; the walk doc has
// the ownership law). A multi-source BOX base (c5b territory) or an
// exists-unsafe base declines here so we never build an ordinal seed over a
// first link that stays name-model. Spine admission is computed BEFORE the
// seed-safe call, and only pureSpine — NOT admitted — exempts the
// box-leg-conjunct decline arm (whose "box legs" a pure chained spine does
// not have; the chained rebase authority bakes or lazies every reachable
// outer ref, with the (pred, !ok) fail-closed net behind it), while every
// OTHER decline arm (existential scope today, anything added later) stays
// live for spines too. An admitted spine that bottoms in a FULL box keeps
// the arm ACTIVE (its bottom aliases are box legs), so a box-leg WHERE
// declines the WHOLE chain to name-model coherently with the first link's
// own gate. The seed-safe operand is the TIP link's base — the same operand
// the pre-fork gate passed. The existential arm is unreachable for a chain
// in practice — EXISTS-over-chained is 0AF00 upstream — but it is scoped by
// semantics, not by that reachability accident.
func (t *cascadesTranslator) chainedUnnestOrdinalGate(
	j *logical.LogicalJoin,
	u *logical.LogicalUnnest,
	outerCorr, innerCorr values.CorrelationIdentifier,
	elementType values.Type,
) (collection, resultValue values.Value, ok bool) {
	if len(u.Segments) < 2 {
		return nil, nil, false
	}
	links, spineAdmitted, pureSpine := t.chainedSpineWalk(j.Left)
	if !spineAdmitted || len(links) == 0 {
		return nil, nil, false
	}
	tipBase := links[len(links)-1].join.Left
	if !t.unnestExistsSeedSafe(tipBase, pureSpine) {
		return nil, nil, false
	}
	// A CONSERVATIVE COHERENCE GUARD for an IMPURE bottom — the chained twin of
	// the single-unnest law at the box-outer enclosure site ("either half alone
	// is broken"), applied through the SAME predicate
	// (boxOuterBirthsPositional): the seed advertises the bottom box
	// positionally only when that predicate says the box births positional.
	// HONEST SCOPE: no demonstrated wrong-rows shape motivates this guard on
	// the reachable path — adversarial rows-probes on the pre-guard tree (a
	// nested outer box `(A LEFT B) FULL C` under a chain, element/box-column/
	// null-supplied-leg projections) all answered CORRECTLY, because the
	// cleared-enclosure recursive translate ordinalizes the first link too (the
	// :1565 gate does not consult boxGatesFresh), leaving the tower coherently
	// positional. What the guard buys: ordinal-over-a-non-fresh-gating box is
	// an UNVALIDATED tower (zero e2e coverage; boxGatesFresh excludes these
	// shapes from the box slices' verified surface, and the coexistence dual
	// emission currently backstops any read the positional path misses) — so
	// until the box substrate validates it (Outcome B), the whole chain
	// declines to name-model: fail-open, rows correct by name, and the
	// boundary is a pinned LAW rather than an accident of dual emission that
	// would flip to wrong rows when the name model is deleted at the cap.
	if !pureSpine && !t.boxOuterBirthsPositional(links[0].join.Left) {
		return nil, nil, false
	}

	// Build the ordinal collection + seed (types only — no translation): a
	// nil from either declines WITHOUT the caller having cleared the enclosure
	// or translated an ordinal first link, so the name-model fallback stays
	// sound. The collection roots at u's OWNER ALIAS (Segments[0]) — the owner
	// link's ELEMENT column, resolved to its slot in the merged ordinal row by
	// chainedOwnerElementSlot over the SAME walk's links (for a linear chain the
	// owner is the tip link and the slot equals the old
	// len(ordinalLegColumns(tip.Left)); for a FORK it is the deeper owner's
	// element — the generalization this admission relies on). Pass the slot
	// EXPLICITLY: a name lookup would pick an OUTER column that SHADOWS the
	// alias (an outer scalar named the same as the owner precedes the element in
	// the merged row → wrong root → the sub-path descends the wrong column, the
	// silent-wrong axis the colliding-schema cert pins).
	elementRootIdx, slotOK := t.chainedOwnerElementSlot(links, u.Segments[0])
	if !slotOK {
		return nil, nil, false
	}
	fieldName := u.Segments[len(u.Segments)-1]
	collection = t.unnestBakedRootCollection(j.Left, outerCorr, u, fieldName, elementType, 0, elementRootIdx)
	if collection == nil {
		return nil, nil, false
	}
	resultValue = t.unnestOrdinalSeed(j.Left, outerCorr, innerCorr, u, elementType)
	if resultValue == nil {
		return nil, nil, false
	}
	return collection, resultValue, true
}

// translateChainedUnnestOrdinal builds the ORDINAL SelectExpression for a chained
// unnest whose FIRST link ordinalizes (RFC-173 S4 Slice 1), or returns nil to
// DECLINE (the caller fails open to the name-model path). The seed is the SAME
// unnestOrdinalSeed the single-source W4c path uses — its outer positional run
// (ofOrdinal over ordinalLegType(j.Left)) carries the first link's merged row
// [outer cols … element] positionally, and unnestSeedInnerFields carries the
// chained element/ordinal — and the collection is unnestBakedRootCollection
// rooted at the OWNER ALIAS column (rootSegmentIndex 0). Both DECLINE (nil) on an
// underivable leg, keeping this fail-open. The whole decision + seed/collection
// construction is chainedUnnestOrdinalGate (shared with the star-body admission).
func (t *cascadesTranslator) translateChainedUnnestOrdinal(
	j *logical.LogicalJoin,
	u *logical.LogicalUnnest,
	outerAlias string,
	outerCorr, innerCorr values.CorrelationIdentifier,
	elementType values.Type,
	prevEnclosure bool,
) expressions.RelationalExpression {
	// RFC-173 cap: the enclosure bit no longer forces the name-model residual —
	// with the name-keyed row deleted, an ENCLOSED chained link's outer flows an
	// ORDINAL row too (a chain buried behind a trailing table `..., T4C`), so it
	// must ordinalize rather than strand its baked references at the -1 sentinel.
	_ = prevEnclosure
	collection, resultValue, ok := t.chainedUnnestOrdinalGate(j, u, outerCorr, innerCorr, elementType)
	if !ok {
		return nil
	}

	// The ordinal path: clear enclosure ONLY for the first link's translateRef so
	// it takes its :1565 binary ordinal seed (flowing a POSITIONAL row the seed's
	// ofOrdinal reads land on), then restore.
	saved := t.inInnerCluster
	t.inInnerCluster = false
	outerRef := t.translateRef(j.Left)
	t.inInnerCluster = saved
	if outerRef == nil {
		return nil
	}

	explode := expressions.NewExplodeExpressionWithOrdinality(collection, u.AtAlias != "")
	innerQ := expressions.NamedForEachQuantifier(innerCorr, expressions.InitialOf(explode))
	outerQ := expressions.NamedForEachQuantifier(outerCorr, outerRef)
	return expressions.NewSelectExpressionWithJoinType(
		resultValue,
		[]expressions.Quantifier{outerQ, innerQ},
		nil,
		[]string{outerAlias, innerCorr.Name()},
		expressions.JoinInner,
	)
}
