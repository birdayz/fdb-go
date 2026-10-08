package query

import (
	"strings"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/relational/api"
	"fdb.dev/pkg/relational/core/query/logical"
)

// isChainedUnnest reports whether u's owner (segment 0) is a prior lateral
// unnest's element in outerLeft (the positive gate for the chained dispatch —
// never merely outerTable=="").
func isChainedUnnest(outerLeft logical.LogicalOperator, u *logical.LogicalUnnest) bool {
	return boundUnnestOwner(outerLeft, u) != nil
}

// isSpineLink reports whether u is a further link of a lateral-unnest spine —
// owned by a prior element (isChainedUnnest) or a sibling over it
// (isSiblingUnnestOverSpine). Every site that lowers, rebases or gates a chained
// link keys on this one predicate, so a sibling takes exactly the chained
// treatment: its outer is the spine's merged row, bound under the same
// correlation as the element it keeps, which only the chained WHERE rebase
// tells apart.
func isSpineLink(outerLeft logical.LogicalOperator, u *logical.LogicalUnnest) bool {
	return isChainedUnnest(outerLeft, u) || isSiblingUnnestOverSpine(outerLeft, u)
}

// isSiblingUnnestOverSpine reports whether u is a further lateral unnest over
// a spine that is not owned by a prior element: one owned by the spine's
// bottom source — its single source (`FROM w, w.arr AS v, w.arr AS v2`) or a
// leaf of an INNER box bottom (`FROM w, h, w.arr AS v, w.arr AS v2`) — whose
// collection roots at that source's window of the merged row
// (chainedBottomSourceCollection), or one over an enclosing query's array
// (`EXISTS (SELECT 1 FROM h, w.arr AS v, w.arr AS v2 …)`), whose collection is
// that binding itself. Java makes either one more ForEach quantifier of the
// block; here it is one more link of the chain.
func isSiblingUnnestOverSpine(outerLeft logical.LogicalOperator, u *logical.LogicalUnnest) bool {
	if boundUnnestOwner(outerLeft, u) != nil || !containsLateralUnnest(outerLeft) {
		return false
	}
	if enclosingOwnedLink(outerLeft, u) {
		return true
	}
	bottom := chainedSpineBottom(outerLeft)
	return bottom != nil && ownedByBottomSource(bottom, u)
}

// ownedByBottomSource reports whether u's owner is a source of the spine's
// bottom: its single source, or one leaf of an INNER box bottom (`FROM w, h,
// w.arr AS v, w.arr AS v2`), whose flat window the merged row carries.
func ownedByBottomSource(bottom logical.LogicalOperator, u *logical.LogicalUnnest) bool {
	if boundUnnestSingleSource(bottom, u) {
		return true
	}
	bj, isJoin := bottom.(*logical.LogicalJoin)
	if !isJoin || bj.Kind != logical.JoinInner {
		return false
	}
	owner, _, _ := boundUnnestCollection(u)
	if owner == nil {
		return false
	}
	for _, leaf := range []logical.LogicalOperator{bj.Left, bj.Right} {
		if ownedByBottomSource(leaf, u) {
			return true
		}
	}
	return false
}

// enclosingOwnedLink reports whether u's collection is an enclosing query's
// array that no source of outer binds — a link that reads nothing of the
// merged row.
func enclosingOwnedLink(outer logical.LogicalOperator, u *logical.LogicalUnnest) bool {
	return u.EnclosingOwner && !unnestOwnedByLeft(outer, u)
}

// chainedSpineBottom peels the lateral-unnest links off a spine and returns
// what they sit on; nil when a peeled link is not a bound unnest.
func chainedSpineBottom(op logical.LogicalOperator) logical.LogicalOperator {
	for {
		bj, ok := op.(*logical.LogicalJoin)
		if !ok {
			return op
		}
		un, isU := bj.Right.(*logical.LogicalUnnest)
		if !isU {
			return op
		}
		if owner, _, _ := boundUnnestCollection(un); owner == nil {
			return nil
		}
		op = bj.Left
	}
}

// chainedBottomSourceCollection roots a link owned by the spine's bottom
// SOURCE (a sibling of the first link) at that source's flat window, which
// leads the merged row at slot 0; nil when the owner is not the bottom's whole
// single source.
func (t *cascadesTranslator) chainedBottomSourceCollection(
	j *logical.LogicalJoin,
	bottom logical.LogicalOperator,
	outerCorr values.CorrelationIdentifier,
	u *logical.LogicalUnnest,
) values.Value {
	if bottom == nil || !ownedByBottomSource(bottom, u) {
		return nil
	}
	if !boundUnnestSingleSource(bottom, u) {
		// A leaf of a box bottom: its window is one of the box's flat runs,
		// which the merged row's leg bounds locate (the box arm of
		// unnestBakedRootCollection); never assumed to start at slot 0.
		return t.unnestBakedRootCollection(j.Left, outerCorr, u, -1)
	}
	outerType := t.ordinalLegType(j.Left)
	if outerType == nil {
		return nil
	}
	outerQOV, err := values.NewQuantifiedObjectValue(outerCorr, outerType)
	if err != nil {
		return nil
	}
	return resolveBoundSeedCollection(outerQOV, u, 0, false)
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
			if boundUnnestOwner(input, n) != nil || isSiblingUnnestOverSpine(siblingSpineLeft(input, n), n) {
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
// When the FIRST link ITSELF ordinalizes (a single-source
// base that takes the binary unnest ordinal seed — see translateUnnestJoin),
// the chained link takes an
// ORDINAL result-value seed (unnestOrdinalSeed) + a POSITIONAL collection
// (unnestBakedRootCollection rooted at the owner-alias column). Clearing the
// chained-unnest enclosure no
// longer strands the chained row (`field "T4.ID" not resolvable … ordinal -1`),
// because the ordinal seed carries the outer's own columns positionally
// (T4.ID etc.).
// The enclosure is CLEARED only for the first link's own translateRef (so it
// ordinalizes) and restored immediately; the rest of the lowering keeps the bit.
//
// Any decline — the chained gate not met (enclosed, deeper-chain
// base, non-single-source base), or a nil seed/collection — is untranslatable:
// there is no remaining name-model fallback for this shape, so the caller
// surfaces a loud UnsupportedQuery error rather than silently returning wrong
// rows.
//
// nil with a set translate error on a loud classification failure; nil with no
// error to decline to the caller's fallback.
func (t *cascadesTranslator) translateChainedUnnestJoin(j *logical.LogicalJoin, u *logical.LogicalUnnest, prevEnclosure bool) expressions.RelationalExpression {
	if err := unnestCorrelationReject(j.Left, u); err != nil {
		t.setTranslateErr(err)
		return nil
	}
	_, _, array := boundUnnestCollection(u)
	if array == nil {
		t.setTranslateErr(boundUnnestBindingError(u))
		return nil
	}
	elementType := array.ElementType

	outerAlias := sourceAlias(j.Left)
	outerCorr := unnestOuterCorrelation(j.Left)
	innerCorr := unnestSourceCorrelation(u)

	// Try the ORDINAL seed when the FIRST link ordinalizes.
	// Mirror the single-source binary seed gate (translateUnnestJoin's own
	// ordinal-seed condition) applied to the FIRST
	// LINK's own base: the chained link ordinalizes iff, with the enclosure bit
	// cleared, translateRef(j.Left) would take the first link's ordinal-seed
	// path — a
	// single-source base (clusterArity==1), exists-safe, and both links' segment
	// paths present. The gate is decided (and the seed/collection built) BEFORE
	// translating outerRef, so a decline never leaves an ORDINAL first link under
	// the name-model builder (which reads it by name → wrong rows).
	if sel := t.translateChainedUnnestOrdinal(j, u, outerAlias, outerCorr, innerCorr, elementType, prevEnclosure); sel != nil {
		return sel
	}

	// The chained ordinal seed DECLINED. A chained lateral
	// unnest whose spine BOTTOMS in a FULL OUTER box that could not ordinalize (a
	// box-leg predicate the box-leg window composition cannot yet fold into the
	// chained seed, or a nested outer box) has no ordinal representation, and the
	// there is no remaining name-model residual — LOUD-REJECT rather than fall to it.
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

	// The ordinal gate DECLINED and no special case matched (an inadmissible
	// spine — twin-fork ownership, an underivable leg). There is no remaining
	// name-model residual composition (a full-suite producer
	// census fired zero, so no production shape lands here); the chain is
	// untranslatable — LOUD, never silent wrong rows.
	if t.translateErr == nil {
		t.setTranslateErr(api.NewError(api.ErrCodeUnsupportedQuery,
			"chained lateral unnest did not ordinalize (inadmissible spine)"))
	}
	return nil
}

// chainedSpineBottomsInFullBox reports whether a chained-unnest outer subtree
// bottoms in a FULL OUTER box — the loud-reject discriminant for
// the un-ordinalizable straddle above. Peels the left-deep lateral-unnest joins (Right is a LogicalUnnest) off
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
// LEFT/RIGHT box (the un-ordinalizable straddle).
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
// ADMISSION LAW 1 — the bottom: whatever remains under the deepest link must
// compose an ordinal row: a SINGLE lateral source (clusterArity 1 — a plain
// scan through transparent wrappers, or a merge-opaque FULL box), a bound
// standalone unnest (a block's first FROM item over an enclosing array), a
// gated INNER box (`FROM t, u, t.arr AS x, x.sub AS y`, whose per-leg windows
// compose into the merged row), or a gated LEFT/RIGHT box gathered as one
// opaque leg. The arms and their limits are spelled out at the bottom check
// below; anything else declines.
//
// ADMISSION LAW 2 — ownership: every link ABOVE the first must have exactly ONE
// owner, resolved BY ALIAS within this same walk (no second spine walk): a
// deeper link's element, the element of a standalone first-item bottom, a
// bottom source's own row (a sibling: `t, t.arr AS x, t.arr AS x2` on a single
// source, or a leaf of an INNER box bottom — ownedByBottomSource), or an
// enclosing query's row no source here binds. Forks are therefore admitted —
// `…, x.substruct AS y, x.sub AS w` (w's owner x is two links back) is as valid
// as a linear chain, because the collection root is computed from the OWNER's
// slot (chainedOwnerElementSlot, chainedBottomSourceCollection), never
// positionally from the preceding link. The first link's owner is a bottom
// source, the standalone bottom's element, or an enclosing row
// (`EXISTS (SELECT 1 FROM w.arr AS v, w.arr AS v2)`: v2 reads the enclosing w),
// validated by the chained classification machinery. An owner
// matching NONE of these, or MORE THAN ONE (duplicate FROM aliases — 42712-loud
// upstream; this arm is defensive), declines. The per-link len(Segments)<2 check is
// load-bearing, not decorative: a 1-segment LogicalUnnest is constructible via
// the AT-source parser path, and such a malformed link must decline
// conservatively.
//
// The seed machinery (ordinalLegColumns/unnestOrdinalSeed/unnestBakedRootCollection)
// accumulates the merged row per link for arbitrary depth,
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
		if owner, _, _ := boundUnnestCollection(un); owner == nil {
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
	//     gatesAsFreshCluster), the box builds its whole leg-concat
	//     positionally (null-supplied legs NULL in their windows), and
	//     ordinalLegColumns' join arm composes the identical concat into the
	//     chained merged row. There is no remaining name-model way to serve this
	//     shape (a name-based collection descend would have nothing to read — the
	//     row has no name keys — a guaranteed loud ordinal-(-1) strand at
	//     execution), so ordinalizing here is the only working representation
	//     for the ELEMENT / leg-projection / element-or-AT-WHERE shapes. A
	//     box-leg WHERE conjunct over this bottom is the un-ordinalizable
	//     straddle (the merged-corr rebase collides with the first link's inner
	//     Explode; a box-quantifier bake sinks below the nested outer
	//     null-extension into the null-supplied scan) — it is set Unbakeable
	//     upstream and DECLINES here, then translateFilter LOUD-REJECTS it (never
	//     wrong rows). FULL stays on the clusterArity==1 arm above
	//     (pureSpine=false, box-leg-conjunct arm active + the FULL-box
	//     reject below).
	bottomInnerBox := false
	bottomOuterBox := false
	if _, bareUnnest := cur.(*logical.LogicalUnnest); bareUnnest && standaloneUnnestLeg(cur) == nil {
		// Only a bound standalone unnest is a spine bottom. A block's first
		// FROM item over an enclosing query's array (`EXISTS (SELECT t FROM
		// q.bs AS b, b.tags AS t)`) is Java's first ForEach quantifier, the
		// links above read its element, and its Explode's flowed element or
		// element/ordinal pair is composed directly by the seeds'
		// standalone arms (unnestOrdinalSeed, unnestBakedRootCollection) and
		// typed by ordinalLegColumns'. Anything else is no leg row at all.
		return nil, false, false
	}
	if t.clusterArity(cur) != 1 {
		bj, isJoin := cur.(*logical.LogicalJoin)
		if !isJoin || !t.gatesAsFreshCluster(bj) {
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
	// A standalone first-item bottom binds an element exactly as a link does,
	// so a link above the first may own it too (`c.bs AS b, c.bs AS b2, …`).
	// A single-source bottom's own row owns a SIBLING link the same way (`w,
	// w.arr AS v, w.arr AS v2`): its collection roots at the bottom's window.
	bottomUnnest := standaloneUnnestLeg(cur)
	for i := 1; i < len(links); i++ {
		owner, _, _ := boundUnnestCollection(links[i].un)
		matches := 0
		if owner != nil && bottomUnnest != nil && bottomUnnest.Alias != "" &&
			owner.Correlation() == unnestSourceCorrelation(bottomUnnest) {
			matches++
		}
		if owner != nil && bottomUnnest == nil && ownedByBottomSource(cur, links[i].un) {
			matches++
		}
		if owner != nil && enclosingOwnedLink(links[i].join.Left, links[i].un) {
			matches++
		}
		for k := range i {
			if owner != nil && links[k].un.Alias != "" && owner.Correlation() == unnestSourceCorrelation(links[k].un) {
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
	// box keeps pureSpine=false so the arm stays active and
	// loud-rejects it). The LEFT/RIGHT bottom is admitted ONLY for the
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
// unnest-right gate poison) — the exact producer-caller class the retired
// name-model builder used to own. The chained twin of rotateEnclosedUnnest (which owns the
// SINGLE-link buried class and declines chains); like it, the rotation is
// inner-join-equivalent — every link references only sources at or below its
// own spine position, and a trailing leg is independent of the links, so
// re-parenting it below the spine preserves rows and multiplicities.
//
// Fail-open (ok=false keeps the ORIGINAL tree): declines on any ON-carrying
// or non-INNER join in the peel (an ON conjunct may reference a link element,
// which is out of scope below the links), a single-link spine
// (rotateEnclosedUnnest's), a top link that is no spine link (isSpineLink:
// owned by a deeper element, a sibling over a bottom source, or an enclosing
// row), or a rotated spine the
// chained walk does NOT admit — so a shape the ordinal path cannot serve
// keeps today's name-model translation instead of trading it for a decline.
func (t *cascadesTranslator) rotateBuriedChainedSpine(j *logical.LogicalJoin) (*logical.LogicalJoin, bool) {
	if t.md == nil || j.Kind != logical.JoinInner ||
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
		if lj.Kind != logical.JoinInner ||
			lj.OnPredicate != nil || lj.OnText != "" {
			return nil, false
		}
		cur = lj
	}
	// Collect the spine's link joins, top-down, and its bottom.
	var linksTopDown []*logical.LogicalJoin
	var bottom logical.LogicalOperator
	for sj := spineTop; ; {
		_, isU := sj.Right.(*logical.LogicalUnnest)
		if !isU {
			bottom = sj
			break
		}
		if sj.Kind != logical.JoinInner ||
			sj.OnPredicate != nil || sj.OnText != "" {
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
	// A block's first FROM item over an enclosing array at the bottom reads no
	// source of this FROM, so it becomes the FIRST LINK over the trailing legs
	// (`FROM q.bs AS b, b.tags AS t, h` rotates to `FROM h, q.bs AS b,
	// b.tags AS t`) and counts as one.
	bottomUnnest := standaloneUnnestLeg(bottom)
	links := len(linksTopDown)
	if bottomUnnest != nil {
		links++
	}
	// Only a spine of ≥2 links whose top is a spine link (isSpineLink: owned
	// by a deeper element, a sibling over a bottom source, or an enclosing
	// row): the single-link buried class rotates via rotateEnclosedUnnest
	// into the FLAT gathered form instead.
	if links < 2 ||
		!isSpineLink(spineTop.Left, spineTop.Right.(*logical.LogicalUnnest)) {
		return nil, false
	}
	// DEFENSE-IN-DEPTH (rotation safety): each trailing leg is reparented BELOW the
	// spine links, so it must NOT laterally correlate to a link's element/AT alias —
	// a leg that read a spine element would find it out of scope once moved below.
	// A trailing unnest owned by a spine element is already peeled as a FORK by
	// chainedSpineWalk (never a plain trailing leg); this asserts it rather than rely on
	// that. A lateral leg reading a link element through its values (`…, (SELECT … t …)
	// AS d`) is not an unnest; lateralTrailingLegs keeps it above the links instead.
	spineAliases := make(map[string]struct{}, 2*links)
	spineUnnests := make([]*logical.LogicalUnnest, 0, links)
	for _, lk := range linksTopDown {
		if un, ok := lk.Right.(*logical.LogicalUnnest); ok {
			spineUnnests = append(spineUnnests, un)
		}
	}
	if bottomUnnest != nil {
		spineUnnests = append(spineUnnests, bottomUnnest)
	}
	for _, un := range spineUnnests {
		if un.Alias != "" {
			spineAliases[strings.ToUpper(un.Alias)] = struct{}{}
		}
		if un.AtAlias != "" {
			spineAliases[strings.ToUpper(un.AtAlias)] = struct{}{}
		}
	}
	for _, leg := range trailingRev {
		if subtreeUnnestsOffAlias(leg, spineAliases) {
			return nil, false
		}
	}
	trailingRev, lateral := lateralTrailingLegs(trailingRev, linksTopDown, bottomUnnest != nil)
	if len(trailingRev) == 0 {
		return nil, false // only lateral legs: translateLateralLegsOverSpine joins them
	}
	// Rebuild: bottom ⋈ trailing legs (FROM order) as the new spine bottom,
	// then the links re-stacked bottom-most first — a first-item bottom as the
	// first of them.
	var newOp logical.LogicalOperator
	if bottomUnnest != nil {
		newOp = trailingRev[len(trailingRev)-1]
		for i := len(trailingRev) - 2; i >= 0; i-- {
			newOp = &logical.LogicalJoin{Left: newOp, Right: trailingRev[i], Kind: logical.JoinInner}
		}
		newOp = &logical.LogicalJoin{Left: newOp, Right: bottomUnnest, Kind: logical.JoinInner}
	} else {
		newOp = bottom
		for i := len(trailingRev) - 1; i >= 0; i-- {
			newOp = &logical.LogicalJoin{Left: newOp, Right: trailingRev[i], Kind: logical.JoinInner}
		}
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
	for _, leg := range lateral {
		rotated = &logical.LogicalJoin{Left: rotated, Right: leg, Kind: logical.JoinInner}
	}
	return rotated, true
}

// lateralTrailingLegs splits a buried spine's trailing legs (reverse FROM
// order) into those the rotation may reparent below the links and the LATERAL
// ones (FROM order), which read the element of a link that would then sit
// above them — every link but the first over a table bottom, whose select the
// reparented legs share, and every link over a first-item bottom. A leg reading
// a lateral leg is lateral too. A leg whose values cannot be enumerated is
// reparented, as before.
func lateralTrailingLegs(
	trailingRev []logical.LogicalOperator,
	linksTopDown []*logical.LogicalJoin,
	firstItemBottom bool,
) (safeRev, lateral []logical.LogicalOperator) {
	upper := linksTopDown
	if !firstItemBottom && len(upper) > 0 {
		upper = upper[:len(upper)-1]
	}
	above := make(map[values.CorrelationIdentifier]struct{}, len(upper))
	for _, lk := range upper {
		if un, isUnnest := lk.Right.(*logical.LogicalUnnest); isUnnest {
			above[unnestSourceCorrelation(un)] = struct{}{}
		}
	}
	var safe []logical.LogicalOperator
	for i := len(trailingRev) - 1; i >= 0; i-- {
		leg := trailingRev[i]
		if reads, enumerated := logicalReads(leg, above); enumerated && reads {
			lateral = append(lateral, leg)
			above[values.NamedCorrelationIdentifier(sourceBinding(leg))] = struct{}{}
			continue
		}
		safe = append(safe, leg)
	}
	for i := len(safe) - 1; i >= 0; i-- {
		safeRev = append(safeRev, safe[i])
	}
	return safeRev, lateral
}

// hoistInterleavedSpineLinks moves a spine link that a later FROM item
// separates from the link owning it back beside that owner (`FROM q, q.bs AS
// b, h, b.tags AS t` becomes `FROM q, q.bs AS b, b.tags AS t, h`). Java binds
// every FROM item as a quantifier of one select wherever it stands; the
// chained spine composes its merged row from adjacent links only. Comma items
// are inner joins and a link reads only its owner's element, so the move keeps
// every row; the separating item then trails the spine, where
// rotateBuriedChainedSpine or translateLateralLegsOverSpine places it.
func hoistInterleavedSpineLinks(j *logical.LogicalJoin) (*logical.LogicalJoin, bool) {
	plainComma := func(lj *logical.LogicalJoin) bool {
		return lj.Kind == logical.JoinInner && lj.OnPredicate == nil && lj.OnText == "" &&
			(lj.BoundOn == nil || (lj.BoundOn.Predicate == nil && len(lj.BoundOn.Exists) == 0)) &&
			len(lj.OnExistsSubqueries) == 0
	}
	if !plainComma(j) {
		return nil, false
	}
	var rev []logical.LogicalOperator
	var base logical.LogicalOperator
	for cur := j; ; {
		rev = append(rev, cur.Right)
		next, isJoin := cur.Left.(*logical.LogicalJoin)
		if !isJoin || !plainComma(next) {
			base = cur.Left
			break
		}
		cur = next
	}
	// The base owns a link when it is a block's first FROM item over an
	// enclosing array (`FROM q.bs AS b, h, b.tags AS t`); it sits before
	// every item, at index -1.
	baseUnnest, _ := base.(*logical.LogicalUnnest)
	var items []logical.LogicalOperator
	moved := false
	for i := len(rev) - 1; i >= 0; i-- {
		item := rev[i]
		owner, owned := -1, false
		if u, isUnnest := item.(*logical.LogicalUnnest); isUnnest {
			if bound, _, _ := boundUnnestCollection(u); bound != nil {
				if baseUnnest != nil && unnestSourceCorrelation(baseUnnest) == bound.Correlation() {
					owned = true
				}
				for k, prior := range items {
					if pu, isPrior := prior.(*logical.LogicalUnnest); isPrior && unnestSourceCorrelation(pu) == bound.Correlation() {
						owner, owned = k, true
					}
				}
			}
		}
		if !owned {
			items = append(items, item)
			continue
		}
		at := owner + 1
		for at < len(items) {
			if _, isUnnest := items[at].(*logical.LogicalUnnest); !isUnnest {
				break
			}
			at++
		}
		moved = moved || at < len(items)
		items = append(items[:at], append([]logical.LogicalOperator{item}, items[at:]...)...)
	}
	// A source owning a later unnest, standing after another unnest, joins the
	// spine's bottom box ahead of every link (`FROM w, w.arr AS v, q, q.bs AS
	// x` becomes `FROM w, q, w.arr AS v, q.bs AS x`) unless it reads a
	// source it would pass, including a lateral source depending on a link.
	for i := 0; i < len(items); i++ {
		source := items[i]
		if _, isUnnest := source.(*logical.LogicalUnnest); isUnnest {
			continue
		}
		passed := make(map[values.CorrelationIdentifier]struct{})
		if baseUnnest != nil {
			passed[unnestSourceCorrelation(baseUnnest)] = struct{}{}
		}
		first := -1
		for k := 0; k < i; k++ {
			if _, isUnnest := items[k].(*logical.LogicalUnnest); isUnnest && first < 0 {
				first = k
			}
			if first >= 0 || baseUnnest != nil {
				for binding := range outerBoundAliases(items[k]) {
					passed[values.NamedCorrelationIdentifier(binding)] = struct{}{}
				}
			}
		}
		if len(passed) == 0 || !ownsALaterUnnest(source, items[i+1:]) {
			continue
		}
		if reads, enumerated := logicalReads(source, passed); !enumerated || reads {
			continue
		}
		rest := append(append([]logical.LogicalOperator(nil), items[:i]...), items[i+1:]...)
		if baseUnnest != nil {
			// The block's first FROM item is itself an unnest: the source
			// becomes the bottom and that unnest its first link.
			items = append([]logical.LogicalOperator{base}, rest...)
			base, baseUnnest = source, nil
		} else {
			items = append(rest[:first], append([]logical.LogicalOperator{source}, rest[first:]...)...)
		}
		moved = true
	}
	if !moved {
		return nil, false
	}
	op := base
	for _, item := range items {
		op = &logical.LogicalJoin{Left: op, Right: item, Kind: logical.JoinInner}
	}
	return op.(*logical.LogicalJoin), true
}

// ownsALaterUnnest reports whether source is the whole single source a later
// unnest reads its collection from.
func ownsALaterUnnest(source logical.LogicalOperator, later []logical.LogicalOperator) bool {
	for _, item := range later {
		if u, isUnnest := item.(*logical.LogicalUnnest); isUnnest && boundUnnestSingleSource(source, u) {
			return true
		}
	}
	return false
}

// translateLateralLegsOverSpine translates trailing legs that read a spine
// link's element (`FROM q, q.bs AS b, b.tags AS t, (SELECT … t …) AS d`) as
// further quantifiers of the spine tip's select — Java's one select over every
// FROM item, at the level where the elements they read are bound. The tip's
// element is a sibling quantifier; reads of deeper links and of the spine's
// tables re-root on the tip's outer merged row, exactly as the tip's WHERE
// does, and so does where. applies is false when j is not that shape; a nil
// expression that applies carries a translate error.
func (t *cascadesTranslator) translateLateralLegsOverSpine(
	j *logical.LogicalJoin,
	where predicates.QueryPredicate,
) (expr expressions.RelationalExpression, applies bool) {
	// Peel the legs above the tip: FROM items reading the spine, and unnests
	// of their array columns.
	var legs []logical.LogicalOperator
	unnestLeg := false
	var firstItem *logical.LogicalUnnest
	cur := j
	for {
		if cur.Kind != logical.JoinInner || cur.OnPredicate != nil || cur.OnText != "" {
			return nil, false
		}
		if u, isUnnest := cur.Right.(*logical.LogicalUnnest); isUnnest {
			if !ownedByALegAboveTheTip(cur.Left, u) {
				break
			}
			unnestLeg = true
		}
		legs = append([]logical.LogicalOperator{cur.Right}, legs...)
		next, isJoin := cur.Left.(*logical.LogicalJoin)
		if !isJoin {
			firstItem = standaloneUnnestLeg(cur.Left)
			if firstItem == nil {
				return nil, false
			}
			break
		}
		cur = next
	}
	u := firstItem
	var prefix logical.LogicalOperator
	if u == nil {
		u = cur.Right.(*logical.LogicalUnnest)
		prefix = cur.Left
	}
	_, _, array := boundUnnestCollection(u)
	if len(legs) == 0 || array == nil {
		return nil, false
	}
	var outerAlias string
	var outerCorr values.CorrelationIdentifier
	if prefix != nil {
		outerAlias = sourceAlias(prefix)
		outerCorr = unnestOuterCorrelation(prefix)
	}
	innerCorr := unnestSourceCorrelation(u)
	read := make(map[values.CorrelationIdentifier]struct{}, len(legs)+2)
	var collection, seed values.Value
	switch {
	case firstItem != nil:
		// The block starts at the enclosing array's Explode; it has no outer
		// row of its own to rebase, just the element its later legs read.
		read[innerCorr] = struct{}{}
		fields, _, ok := unnestSeedInnerFields(innerCorr, u, array.ElementType)
		if !ok {
			return nil, false
		}
		collection = u.CorrelatedCollection
		seed = values.NewRawRecordConstructorValue(fields...)
	case isSpineLink(cur.Left, u):
		if _, admitted, _ := t.chainedSpineWalk(cur); !admitted {
			return nil, false
		}
		links, ok := t.spineElementLinks(cur)
		if !ok {
			return nil, false
		}
		for _, l := range links {
			read[l.corr] = struct{}{}
		}
		var gated bool
		collection, seed, gated = t.chainedUnnestOrdinalGate(cur, u, outerCorr, innerCorr, array.ElementType)
		if !gated {
			return nil, false
		}
	case unnestLeg && boundUnnestSingleSource(cur.Left, u):
		// A single link: legs without an unnest of their own take the
		// gathered path, which already places them.
		read[innerCorr] = struct{}{}
		seed = t.unnestOrdinalSeed(cur.Left, outerCorr, innerCorr, u, array.ElementType)
		collection = t.unnestSeedCollection(cur.Left, outerCorr, u)
		if collection == nil {
			return nil, false
		}
	default:
		return nil, false
	}
	seedRC, isRC := seed.(*values.RecordConstructorValue)
	if !isRC {
		return nil, false
	}
	for _, leg := range legs {
		if lu, isUnnest := leg.(*logical.LogicalUnnest); isUnnest {
			read[unnestSourceCorrelation(lu)] = struct{}{}
			continue
		}
		if reads, enumerated := logicalReads(leg, read); !enumerated || !reads || legBinding(leg) != sourceBinding(leg) {
			return nil, false
		}
		read[values.NamedCorrelationIdentifier(sourceBinding(leg))] = struct{}{}
	}
	fail := func(format string, args ...any) (expressions.RelationalExpression, bool) {
		if t.translateErr == nil {
			t.setTranslateErr(api.NewErrorf(api.ErrCodeUnsupportedQuery, format, args...))
		}
		return nil, true
	}

	var quants []expressions.Quantifier
	var aliases []string
	if prefix != nil {
		saved := t.inInnerCluster
		t.inInnerCluster = false
		outerRef := t.translateRef(prefix)
		t.inInnerCluster = saved
		if outerRef == nil {
			return fail("lateral leg over a chained unnest: the spine below its last link did not translate")
		}
		quants = append(quants, expressions.NamedForEachQuantifier(outerCorr, outerRef))
		aliases = append(aliases, outerAlias)
	}
	explode, err := unnestExplode(collection, u)
	if err != nil {
		t.setTranslateErr(err)
		return nil, true
	}
	quants = append(quants, expressions.NamedForEachQuantifier(innerCorr, expressions.InitialOf(explode)))
	aliases = append(aliases, innerCorr.Name())
	fields := append([]values.RecordConstructorField(nil), seedRC.Fields...)

	var ordType *values.RecordType
	var outerLegs map[string]struct{}
	if prefix != nil {
		ordType = t.ordinalLegType(prefix)
		outerLegs = unnestOuterLegAliases(prefix, outerCorr)
	}
	rebased := true
	rebase := func(node values.Value) values.Value {
		if !rebased || prefix == nil {
			return node
		}
		if fv, isFV := values.AsFieldValue(node); isFV {
			if _, rooted := values.AsQuantifiedObjectValue(fv.ChildValue()); !rooted {
				return node
			}
		} else if _, isQOV := values.AsQuantifiedObjectValue(node); !isQOV {
			return node
		}
		var p predicates.QueryPredicate = predicates.NewValuePredicate(node)
		p, ok := t.rebaseSpineElementRefs(p, cur.Left, outerCorr, ordType)
		if ok {
			p, ok = rebaseUnnestOuterLegPredicateOrdinal(p, ordType, ordType, outerLegs, outerCorr)
		}
		vp, isVP := p.(*predicates.ValuePredicate)
		if !ok || !isVP {
			rebased = false
			return node
		}
		return vp.Value
	}
	var unnestLegs []*logical.LogicalUnnest
	for _, leg := range legs {
		if lu, isUnnest := leg.(*logical.LogicalUnnest); isUnnest {
			// An unnest of a leg's array column: one more Explode quantifier,
			// its collection read off that leg's quantifier.
			_, _, legArray := boundUnnestCollection(lu)
			if legArray == nil || lu.CorrelatedCollection == nil {
				return fail("unnest %s over a lateral leg has no exact collection", sourceAlias(lu))
			}
			legExplode, err := unnestExplode(lu.CorrelatedCollection, lu)
			if err != nil {
				t.setTranslateErr(err)
				return nil, true
			}
			legCorr := unnestSourceCorrelation(lu)
			legFields, _, ok := unnestSeedInnerFields(legCorr, lu, legArray.ElementType)
			if !ok {
				return fail("unnest %s over a lateral leg binds no column", sourceAlias(lu))
			}
			fields = append(fields, legFields...)
			quants = append(quants, expressions.NamedForEachQuantifier(legCorr, expressions.InitialOf(legExplode)))
			aliases = append(aliases, legCorr.Name())
			unnestLegs = append(unnestLegs, lu)
			continue
		}
		rebuilt, enumerated := rebuildInnerWithValues(leg, rebase)
		if !enumerated || !rebased {
			return fail("lateral leg %s over a chained unnest reads the spine where its merged row cannot place it", sourceAlias(leg))
		}
		ref := t.translateRef(rebuilt)
		if ref == nil {
			return nil, true
		}
		legCorr := values.NamedCorrelationIdentifier(legBinding(rebuilt))
		legType := t.ordinalLegType(rebuilt)
		if legType == nil {
			return fail("lateral leg %s over a chained unnest has no positional row", sourceAlias(leg))
		}
		legQOV, err := values.NewQuantifiedObjectValue(legCorr, legType)
		if err != nil {
			t.setTranslateErr(err)
			return nil, true
		}
		for i := range legType.Fields {
			resolved, err := values.ResolveOrdinalSeedField(legQOV, i)
			if err != nil {
				t.setTranslateErr(err)
				return nil, true
			}
			fv, _ := values.AsFieldValue(resolved)
			fields = append(fields, values.RecordConstructorField{Name: fv.DisplayName(), Value: resolved})
		}
		quants = append(quants, expressions.NamedForEachQuantifier(legCorr, ref))
		aliases = append(aliases, sourceAlias(leg))
	}
	sel, err := expressions.NewSelectExpressionWithJoinType(
		values.NewRawRecordConstructorValue(fields...), quants, nil, aliases, expressions.JoinInner)
	if err != nil {
		t.setTranslateErr(err)
		return nil, true
	}
	if where == nil {
		return sel, true
	}
	var preds []predicates.QueryPredicate
	for _, conjunct := range predicates.FlattenConjunction([]predicates.QueryPredicate{where}) {
		conjunct = rewriteUnnestPredicate(conjunct, u)
		for _, lu := range unnestLegs {
			conjunct = rewriteUnnestPredicate(conjunct, lu)
		}
		if prefix != nil {
			baked, ok := t.chainedSpineConjunct(cur, sel, conjunct)
			if !ok {
				return fail("a WHERE over a chained unnest and its lateral legs reads the spine where its merged row cannot place it")
			}
			conjunct = baked
		}
		preds = append(preds, conjunct)
	}
	return t.exactSelectWithJoinType(sel.GetResultValue(), quants, preds, aliases, expressions.JoinInner), true
}

// ownedByALegAboveTheTip reports whether u unnests the array column of a FROM
// item standing between u and the spine tip below it in left.
func ownedByALegAboveTheTip(left logical.LogicalOperator, u *logical.LogicalUnnest) bool {
	for {
		lj, isJoin := left.(*logical.LogicalJoin)
		if !isJoin || lj.Kind != logical.JoinInner {
			return false
		}
		if lu, isUnnest := lj.Right.(*logical.LogicalUnnest); isUnnest {
			if !ownedByALegAboveTheTip(lj.Left, lu) {
				return false // lu is the tip
			}
		} else if boundUnnestSingleSource(lj.Right, u) {
			return true
		}
		left = lj.Left
	}
}

// logicalReads reports whether op's values read any of corrs; enumerated is
// false when op carries a node or subquery rider the value walk cannot visit.
func logicalReads(op logical.LogicalOperator, corrs map[values.CorrelationIdentifier]struct{}) (reads, enumerated bool) {
	_, enumerated = rebuildInnerWithValues(op, func(node values.Value) values.Value {
		if qov, isQOV := values.AsQuantifiedObjectValue(node); isQOV {
			if _, hit := corrs[qov.Correlation()]; hit {
				reads = true
			}
		}
		return node
	})
	return reads, enumerated
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
	if un, ok := op.(*logical.LogicalUnnest); ok {
		owner, _, _ := boundUnnestCollection(un)
		if owner == nil {
			return len(aliases) > 0 // unknown dependencies cannot justify a rotation
		}
		if _, hit := aliases[owner.Correlation().Name()]; hit {
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
func (t *cascadesTranslator) chainedOwnerElementSlot(bottom logical.LogicalOperator, links []chainedSpineLink, ownerAlias string) (int, bool) {
	ownerIdx := -1
	for i, l := range links {
		if l.un.Alias != "" && ownerAlias == unnestSourceCorrelation(l.un).Name() {
			if ownerIdx >= 0 {
				return 0, false
			}
			ownerIdx = i
		}
	}
	// A standalone first-item bottom leads the merged row with its own
	// element, so an owner it binds is slot 0 — unless a link binds the same
	// name too, the defensive duplicate.
	if b := standaloneUnnestLeg(bottom); b != nil && b.Alias != "" && ownerAlias == unnestSourceCorrelation(b).Name() {
		if ownerIdx >= 0 {
			return 0, false
		}
		return 0, true
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
// the pre-fork gate passed. Under an EXISTS the scope arm admits a pure
// spine: its correlation bakes over the merged row like its WHERE refs.
func (t *cascadesTranslator) chainedUnnestOrdinalGate(
	j *logical.LogicalJoin,
	u *logical.LogicalUnnest,
	outerCorr, innerCorr values.CorrelationIdentifier,
	elementType values.Type,
) (collection, resultValue values.Value, ok bool) {
	owner, _, _ := boundUnnestCollection(u)
	if owner == nil {
		return nil, nil, false
	}
	links, spineAdmitted, pureSpine := t.chainedSpineWalk(j.Left)
	// The bottom a spine's first link sits on — the whole outer when the
	// only link is u itself, which is chained exactly when that bottom is a
	// standalone first-item unnest owning it.
	bottom := j.Left
	if len(links) > 0 {
		bottom = links[0].join.Left
	}
	if !spineAdmitted || (len(links) == 0 && standaloneUnnestLeg(bottom) == nil) {
		return nil, nil, false
	}
	tipBase := j.Left
	if len(links) > 0 {
		tipBase = links[len(links)-1].join.Left
	}
	if !t.unnestExistsSeedSafe(tipBase, pureSpine) {
		return nil, nil, false
	}
	// A CONSERVATIVE COHERENCE GUARD for an IMPURE bottom — the chained twin of
	// the single-unnest law at the box-outer enclosure site ("either half alone
	// is broken"), applied through the SAME predicate
	// (boxOuterBuildsPositional): the seed advertises the bottom box
	// positionally only when that predicate says the box builds positional.
	// HONEST SCOPE: no demonstrated wrong-rows shape motivates this guard on
	// the reachable path — adversarial rows-probes on the pre-guard tree (a
	// nested outer box `(A LEFT B) FULL C` under a chain, element/box-column/
	// null-supplied-leg projections) all answered CORRECTLY, because the
	// cleared-enclosure recursive translate ordinalizes the first link too (the
	// ordinal-seed gate does not consult boxGatesFresh), leaving the tower coherently
	// positional. What the guard buys: ordinal-over-a-non-fresh-gating box is
	// an UNVALIDATED tower (zero e2e coverage; boxGatesFresh excludes these
	// shapes from the box paths' verified surface) — so
	// until that substrate is validated, the whole chain
	// declines to name-model: fail-open, rows correct by name, and the
	// boundary is a pinned LAW rather than a guess that happens to work today.
	if !pureSpine && !t.boxOuterBuildsPositional(bottom) {
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
	switch {
	case enclosingOwnedLink(j.Left, u):
		collection = u.CorrelatedCollection
	default:
		collection = t.chainedBottomSourceCollection(j, bottom, outerCorr, u)
	}
	if collection == nil {
		elementRootIdx, slotOK := t.chainedOwnerElementSlot(bottom, links, owner.Correlation().Name())
		if !slotOK {
			return nil, nil, false
		}
		collection = t.unnestBakedRootCollection(j.Left, outerCorr, u, elementRootIdx)
		if collection == nil {
			return nil, nil, false
		}
	}
	resultValue = t.unnestOrdinalSeed(j.Left, outerCorr, innerCorr, u, elementType)
	if resultValue == nil {
		return nil, nil, false
	}
	return collection, resultValue, true
}

// translateChainedUnnestOrdinal builds the ORDINAL SelectExpression for a chained
// unnest whose FIRST link ordinalizes, or returns nil to
// DECLINE (the caller fails open to the name-model path). The seed is the SAME
// unnestOrdinalSeed the single-source unnest path uses — its outer positional run
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
	// The enclosure bit no longer forces the name-model residual —
	// with the name-keyed row deleted, an ENCLOSED chained link's outer flows an
	// ORDINAL row too (a chain buried behind a trailing table `..., T4C`), so it
	// must ordinalize rather than strand its baked references at the -1 sentinel.
	_ = prevEnclosure
	collection, resultValue, ok := t.chainedUnnestOrdinalGate(j, u, outerCorr, innerCorr, elementType)
	if !ok {
		return nil
	}

	// The ordinal path: clear enclosure ONLY for the first link's translateRef so
	// it takes its own binary ordinal seed (flowing a POSITIONAL row the seed's
	// ofOrdinal reads land on), then restore.
	saved := t.inInnerCluster
	t.inInnerCluster = false
	outerRef := t.translateRef(j.Left)
	t.inInnerCluster = saved
	if outerRef == nil {
		return nil
	}

	explode, err := unnestExplode(collection, u)
	if err != nil {
		t.setTranslateErr(err)
		return nil
	}
	innerQ := expressions.NamedForEachQuantifier(innerCorr, expressions.InitialOf(explode))
	outerQ := expressions.NamedForEachQuantifier(outerCorr, outerRef)
	selectExpr, err := expressions.NewSelectExpressionWithJoinType(
		resultValue,
		[]expressions.Quantifier{outerQ, innerQ},
		nil,
		[]string{outerAlias, innerCorr.Name()},
		expressions.JoinInner,
	)
	if err != nil {
		t.setTranslateErr(err)
		return nil
	}
	return selectExpr
}

// siblingSpineLeft is the left operand u is joined to inside the spine rooted
// at op, or nil when u is not a join's right-hand unnest there.
func siblingSpineLeft(op logical.LogicalOperator, u *logical.LogicalUnnest) logical.LogicalOperator {
	for {
		bj, ok := op.(*logical.LogicalJoin)
		if !ok {
			return nil
		}
		if bj.Right == logical.LogicalOperator(u) {
			return bj.Left
		}
		op = bj.Left
	}
}

// spineElementLink is one unnest link of a spine and the slot its columns
// (boundUnnestLegColumns: the element, then the ordinal) start at in the
// spine's merged row.
type spineElementLink struct {
	link *logical.LogicalUnnest
	corr values.CorrelationIdentifier
	slot int
}

// root maps a reference path over the link's element, or over its AT pair
// (element, ordinal), to the merged-row slot it starts at and the path left
// to walk from there. An AT-only link emits only its ordinal.
func (l spineElementLink) root(path []int) (int, []int, bool) {
	if l.link.AtAlias == "" {
		return l.slot, path, true
	}
	if len(path) == 0 {
		return 0, nil, false
	}
	switch {
	case l.link.Alias != "" && (path[0] == 0 || path[0] == 1):
		return l.slot + path[0], path[1:], true
	case l.link.Alias == "" && path[0] == 1:
		return l.slot, path[1:], true
	}
	return 0, nil, false
}

// spineElementLinks lists every unnest link of the spine rooted at op, the
// link under the tip first, then a standalone first-item bottom beneath the
// links. ok=false when a link's prefix is underivable.
func (t *cascadesTranslator) spineElementLinks(op logical.LogicalOperator) ([]spineElementLink, bool) {
	var links []spineElementLink
	cur := op
	for {
		sj, isJoin := cur.(*logical.LogicalJoin)
		if !isJoin {
			break
		}
		link, isUnnest := sj.Right.(*logical.LogicalUnnest)
		if !isUnnest {
			break
		}
		prefix := t.ordinalLegColumns(sj.Left)
		if bottom := standaloneUnnestLeg(sj.Left); bottom != nil {
			prefix = boundUnnestLegColumns(bottom)
		}
		if prefix == nil {
			return nil, false
		}
		links = append(links, spineElementLink{link: link, corr: unnestSourceCorrelation(link), slot: len(prefix)})
		cur = sj.Left
	}
	if bottom := standaloneUnnestLeg(cur); bottom != nil && cur != op {
		links = append(links, spineElementLink{link: bottom, corr: unnestSourceCorrelation(bottom), slot: 0})
	}
	return links, true
}

// rebaseSpineElementRefs bakes a WHERE conjunct's references to the element of
// any link of a spine onto that element's slot of the merged row. The merged
// row is bound under the binding of the link directly under the tip
// (sourceBinding(spine) — its rightmost leaf), so `b.k` in `FROM q, q.bs AS b,
// b.tags AS t WHERE t > b.k` names B's ELEMENT while the chained select's
// outer quantifier B flows the whole merged row: left as is, one correlation
// carries two exact types and the plan is malformed at execution. A deeper
// link's element (`c.k` under `qq.cs AS c, c.bs AS b, b.tags AS t`) is bound
// at no level above its own, so it re-roots the same way. The element is a
// whole-object slot (no AT) or the flat element/ordinal run (AT), exactly as
// resolveBoundSeedCollection roots a link's collection.
//
// A first-item bottom directly under the tip needs nothing: there the outer
// IS that element. Refs to base legs are rebaseChainedOuterLegPredicate's.
// ok=false when a reference cannot be re-rooted (a bare AT pair, an AT-only
// link's hidden element, an underivable prefix): the caller declines.
func (t *cascadesTranslator) rebaseSpineElementRefs(
	p predicates.QueryPredicate,
	spine logical.LogicalOperator,
	mergedCorr values.CorrelationIdentifier,
	ordType *values.RecordType,
) (predicates.QueryPredicate, bool) {
	if p == nil || ordType == nil {
		return p, true
	}
	links, ok := t.spineElementLinks(spine)
	if !ok {
		return p, false
	}
	if len(links) == 0 {
		return p, true
	}
	merged, err := values.NewQuantifiedObjectValue(mergedCorr, ordType)
	if err != nil {
		return p, false
	}
	// The link under the tip shares its binding with the merged row, so its
	// element reference is told from the merged row by exact type; one whose
	// type cannot be established is left alone — the plan then fails loud on
	// it — never re-rooted on a guess. A deeper link's binding names only its
	// element.
	linkOf := func(v values.Value) (spineElementLink, bool) {
		qov, isQOV := values.AsQuantifiedObjectValue(v)
		if !isQOV {
			return spineElementLink{}, false
		}
		for _, l := range links {
			if qov.Correlation() != l.corr {
				continue
			}
			if l.corr == mergedCorr && (values.FlowedExactType(qov) == nil || values.FlowedTypesEqual(qov, merged)) {
				return spineElementLink{}, false
			}
			return l, true
		}
		return spineElementLink{}, false
	}
	rebased := predicates.ReplaceValues(p, func(v values.Value) values.Value {
		if !ok {
			return v
		}
		if fv, isFV := values.AsFieldValue(v); isFV {
			l, isElement := linkOf(fv.ChildValue())
			if !isElement {
				return v
			}
			root, rest, rooted := l.root(fv.Path().Ordinals())
			if !rooted {
				ok = false
				return v
			}
			requests := make([]values.FieldRequest, len(rest))
			for i, ordinal := range rest {
				request, err := values.FieldByOrdinal(ordinal)
				if err != nil {
					ok = false
					return v
				}
				requests[i] = request
			}
			access, err := values.ResolveOrdinalSeedAccess(merged, root, requests)
			if err != nil {
				ok = false
				return v
			}
			return access
		}
		if l, isElement := linkOf(v); isElement {
			if l.link.AtAlias != "" {
				ok = false
				return v
			}
			access, err := values.ResolveOrdinalSeedField(merged, l.slot)
			if err != nil {
				ok = false
				return v
			}
			return access
		}
		return v
	})
	return rebased, ok
}
