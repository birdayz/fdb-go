package query

import (
	"strings"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/relational/core/query/logical"
)

// The GATHERED multi-source lateral unnest, translated flat.
//
// `FROM A, B, A.arr AS x [AT o]` translated as Java translates it
// (LogicalOperator.generateCorrelatedFieldAccess + generateSimpleSelect): ONE
// flat select over {QOV(A), QOV(B), ForEach(Explode)} where the Explode's
// collection is a BAKED reference to the OWNING source's own quantifier —
// `ofOrdinal(QOV(A, typA), FieldIndexUnique(ARR))` — a genuine per-source
// correlation every Cascades rule can see. This replaces the name-model
// binary FlatMap-over-merged-outer whose buried dotted read
// (`FieldValue(QOV(rightmost), "A.ARR")`) required the dotted-prefix
// bipartition classifiers; with the per-source edge, bipartition validity is
// EMERGENT (the rangesOver sibling-edge keeps A with its Explode — exactly
// Java's Quantifier.getCorrelatedTo edge; a whole-cluster concat alternative
// was ruled out for over-constraining re-enumeration).
//
// The single-source binary seed (unnest_seed.go) remains the N=1 path (this
// builder is N>=2 only). A decline is correct-or-loud: shapes that retain a
// surviving non-ordinal lowering fall back to it (several ungated/residual
// compositions work today that way, and declining loud instead would regress
// them); an enclosed outer box whose anchored seed was deleted has no such
// residual and is LOUD-rejected downstream (0AF00 "join did not ordinalize"),
// never silently wrong.

// unnestTrailing is the unnestPos for the ROOT form: the unnest follows every
// plain leg in FROM order (`FROM A, B, A.arr AS x`), so the element fields and
// the Explode quantifier append last.
const unnestTrailing = -1

// translateGatheredUnnestCluster builds the flat (N+1)-quantifier select for
// a multi-source lateral unnest. The caller (translateUnnestJoin) has already
// run the FULL classification/validation gauntlet — metadata, CTE/derived
// rejection, array-field typing, chained-unnest guard, alias-collision, and
// AS/AT-distinct rejections — so every input reaching this point is a VALID
// unnest; nil here means DECLINE (the caller falls back to the binary
// name-model path), never an error.
//
// unnestPos is the unnest's FROM position (the number of plain legs BEFORE
// it): the enclosed form (`FROM A, A.arr AS x, B`) places the element
// fields/quantifier mid-list so a `SELECT *` expansion preserves the user's
// FROM order — the seed assert accepts leg runs in any order, and every
// downstream consumer resolves by span offset, not run position.
func (t *cascadesTranslator) translateGatheredUnnestCluster(
	j *logical.LogicalJoin,
	u *logical.LogicalUnnest,
	innerCorr values.CorrelationIdentifier,
	elementType values.Type,
	fieldName string,
	unnestPos int,
) expressions.RelationalExpression {
	leftJoin, isJoin := j.Left.(*logical.LogicalJoin)
	if !isJoin {
		return nil // single-source outer — the single-source binary seed owns it
	}
	// The left cluster must be a GATED-if-fresh cluster (the enclosure-free
	// probe, side-effect-free Decide — the ordinalEligible pattern): an
	// ungated cluster flows name-model rows the baked collection read cannot
	// consume, so it falls through to the fail-open name-model residual.
	prev := t.inInnerCluster
	t.inInnerCluster = false
	d := t.ordinalWedgeGateDecide(leftJoin)
	t.inInnerCluster = prev
	if !d.Gated {
		return nil
	}

	var legs []clusterLeg
	if leftJoin.Kind != logical.JoinInner {
		// A non-EXISTS WHERE conjunct on a box leg (unnestBoxLegConjunct): the
		// box gathered as ONE OPAQUE leg still carries a BURIED window for every
		// leaf (addBuriedBakeWindows: bakeCorr = the $BOX quantifier), so a
		// BAKEABLE conjunct (pre-verified metadata-only: every box-leg ref
		// resolves in its buried window's leafTyp, no subquery values) is
		// ADMITTED — the WHERE-merge arm bakes it over this seed's RECORDED
		// legTypes (the seed⟺merge one-authority law; the record is written at
		// the tail below). Only an UNBAKEABLE verdict (EXISTS path,
		// subquery-carrying, unresolvable ref) declines — correct-or-loud: a shape
		// with a surviving non-ordinal lowering takes it; an enclosed outer box,
		// whose anchored seed was deleted, is LOUD-rejected downstream (never a
		// silent wrong-rows plan). (The gathered path is
		// unreachable under EXISTS — translateUnnestJoin gates it on
		// !unnestUnderExistential — so a non-None verdict here always reflects a
		// plain non-EXISTS WHERE.)
		if t.unnestBoxLegConjunct == boxConjUnbakeable {
			return nil
		}
		// A gated OUTER box as the unnest's left: the box is ONE OPAQUE leg —
		// its legs are never gathered into the flat inner select (the flat
		// seed has no arm for null-supplying roles; the box quantifier
		// carries them, and its buried leaves stay addressable through the
		// buried bake windows). The unnest is the user's own INNER comma
		// lateral over the box: a null-supplying owner's NULL array explodes
		// to zero rows and drops that padded row — Java's Explode-over-NULL
		// spec (RecordQueryExplodePlan: null collection → List.of()), not
		// a LEFT violation.
		legs = []clusterLeg{clusterLegOf(leftJoin, false)}
	} else {
		legs = t.legsOfGatedJoin(leftJoin)
	}
	fields, legTypes := t.ordinalJoinSeedFields(legs)
	if fields == nil {
		return nil // a leg untranslatable — same decline rule as the seed
	}

	// EVERY dup column name GATHERS via the RAW seed (NO decline, NO wrap) — shared ACROSS
	// legs (the bare-twin `FROM A, B, A.arr AS x` with A,B both carrying `k`; the box-buried
	// cross-leg `FROM (A FULL C), B, C.arr AS x` with buried A.k dup-named with scan B.k) OR
	// twice WITHIN ONE box's concat (a `(A FULL B)` box carrying both A.k and B.k). Each
	// buried leaf gets its OWN [Offset,Width) window (buriedLegBounds / finalizeSeedWindows,
	// recursing through nested boxes), so a QUALIFIED read routes to its own SLOT, not
	// first-match, uniformly through every outer operator — SELECT / WHERE / GROUP BY /
	// ORDER BY / DISTINCT / cross-leg predicate / buried-element predicate (the qualifier-
	// honoring authority). A within-box dup's DOUBLY-NULL-fill (both same-named leaves NULL
	// on OPPOSITE unmatched rows) resolves through the FULL-NULL substrate, discriminated by
	// disjoint value sets. A BARE ambiguous reference (`SELECT k`, `GROUP BY k`) errors 42702
	// at semantic analysis BEFORE the translator, so only qualified reads reach here — NO dup
	// shape declines. A positional wrap is NOT needed and is actively WRONG: its bare
	// pass-through key first-matches a qualified dup column, dropping every row of
	// `WHERE B.k=200`. (The former declineBoxDup gate — cross-leg-to-box, then within-box —
	// is fully retired; both classes gather uniformly on the same buried-leaf-window +
	// FULL-NULL substrate.)
	// A GROUPED gather (underAggregate) UN-COLLAPSES to the raw per-leg seed (below)
	// — no name-keyed wrap — and the ancestor GROUP-BY POSITIONALLY BAKES its keys /
	// operands over it (translateAggregate: OrdinalSeedLegWindows for leg columns,
	// element-anywhere including the MID-LIST split-run; fieldValueReferencesInner for
	// the element). The grouped bare-twin's qualified dup keys route by slot (not
	// first-match), and the outer WHERE bakes for free (bakeGatedJoinPredicates fires
	// on the SelectExpression). This retires the collapse AND the ALIAS.COL name keys
	// (the name-model residue).

	// The OWNING source: the legTypes map IS the owner index — a PLAIN leg's
	// own window keyed by its binding (== UPPER alias for the FROM-order
	// first match; a later duplicate's minted binding is unreachable by
	// name), and every BURIED leaf of a box leg via its buried window
	// (bakeCorr = the box quantifier, leafOffset = the leaf's slot in the
	// concat). The collection bakes at the WINDOW: ofOrdinal(QOV(corr,
	// window type), leafOffset + arrIdx) — for a plain leg that degenerates
	// to the old leg-local bake verbatim (offset 0, own type, own binding).
	seg0 := strings.ToUpper(u.Segments[0])
	ownerWindow, isOwner := legTypes[seg0]
	if !isOwner || ownerWindow.typ == nil || ownerWindow.leafTyp == nil || len(u.Segments) < 2 {
		return nil
	}
	// The ROOT column of the collection path: for a single-segment path the
	// classifier's proto-derived name; for a MULTI-SEGMENT path (`t.rec.arr`)
	// the FIRST field segment — the remaining segments ride as a FUSED
	// suffix (Java's lookupNestedField →
	// ofFieldsAndFuseIfPossible shape) and descend the struct value at eval
	// through FieldValue's proto-message arm. Suffix accessors are
	// NAME-addressed (the proto descent resolves by field name); the
	// classifier has already validated every intermediate as a singular
	// record field.
	rootField := strings.ToUpper(fieldName)
	if len(u.Segments) > 2 {
		rootField = strings.ToUpper(u.Segments[1])
	}
	arrIdx, found := ownerWindow.leafTyp.FieldIndexUnique(rootField)
	if !found {
		return nil
	}
	ownerCorr := seg0
	if ownerWindow.bakeCorr != "" {
		ownerCorr = strings.ToUpper(ownerWindow.bakeCorr)
	}
	ownerQOV := values.NewQuantifiedObjectValueOfType(values.NamedCorrelationIdentifier(ownerCorr), ownerWindow.typ)
	collection, err := values.NewFieldValueOfOrdinal(ownerQOV, ownerWindow.leafOffset+arrIdx)
	if err != nil {
		return nil
	}
	if len(u.Segments) > 2 {
		suffix := make([]values.ResolvedAccessor, 0, len(u.Segments)-2)
		for _, seg := range u.Segments[2:] {
			// NAME-addressed: the struct descent (FieldValue's proto-message
			// arm) resolves each suffix step by field NAME. Ordinal is the
			// LOUD sentinel -1 — a struct materializes as a proto message, not
			// a positional row, so the ordinal is never consulted; should one
			// ever reach the OrdinalRow descent arm, Get(-1) fails
			// out-of-range (a clean error) rather than silently reading slot 0.
			suffix = append(suffix, values.ResolvedAccessor{Field: strings.ToUpper(seg), Ordinal: -1})
		}
		fused := collection.Resolved.WithSuffix(&values.FieldPath{Accessors: suffix})
		collection = &values.FieldValue{Field: strings.ToUpper(fieldName), Typ: collection.Typ, Child: collection.Child, Resolved: fused}
	}
	// The baked node carries the leg type's field TYPE for the array column;
	// the classifier's proto-derived element type is authoritative for the
	// Explode (ordinalLegType columns are best-effort for derived shapes).
	collection.Typ = values.NewArrayType(true, elementType)

	// Legs translate FRESH (legs of a GATED parent gate
	// independently); the Explode is one more ordinary quantifier, correlated
	// to its owner by the baked collection itself.
	prevEnclosure := t.inInnerCluster
	t.inInnerCluster = false
	quantifiers := make([]expressions.Quantifier, 0, len(legs)+1)
	sourceAliases := make([]string, 0, len(legs)+1)
	for _, leg := range legs {
		ref := t.translateRef(leg.op)
		if ref == nil {
			t.inInnerCluster = prevEnclosure
			return nil
		}
		quantifiers = append(quantifiers, expressions.NamedForEachQuantifier(
			values.NamedCorrelationIdentifier(leg.binding), ref,
		))
		sourceAliases = append(sourceAliases, leg.binding)
	}
	t.inInnerCluster = prevEnclosure

	// The unnest's FROM position: quantifier index + seed-field offset (the
	// sum of the preceding legs' run widths). Out-of-range = trailing.
	if unnestPos < 0 || unnestPos > len(legs) {
		unnestPos = len(legs)
	}
	fieldsAt := 0
	for i := 0; i < unnestPos; i++ {
		// BINDING-keyed, matching ordinalJoinSeedFields' map — an alias
		// lookup would nil-miss a duplicate leg's entry and panic on .typ
		// (the seed and this consumer must share ONE key discipline).
		fieldsAt += len(legTypes[legs[i].binding].typ.Fields)
	}

	explode := expressions.NewExplodeExpressionWithOrdinality(collection, u.AtAlias != "")
	explodeQ := expressions.NamedForEachQuantifier(innerCorr, expressions.InitialOf(explode))
	quantifiers = append(quantifiers[:unnestPos:unnestPos],
		append([]expressions.Quantifier{explodeQ}, quantifiers[unnestPos:]...)...)
	sourceAliases = append(sourceAliases[:unnestPos:unnestPos],
		append([]string{innerCorr.Name()}, sourceAliases[unnestPos:]...)...)

	// Seed RV: the flat ordinal leg runs + the unnest inner fields (the
	// element/ordinal branch shared with the single-source seed) at the
	// unnest's FROM position. The assert runs only for the full-baked shape
	// (AS+AT); the mixed direct-QOV element and the AT-only partial run
	// legitimately skip it — exactly the single-source seed's rule.
	innerFields, fullBaked, ok := unnestSeedInnerFields(innerCorr, u, elementType)
	if !ok {
		return nil // degenerate no-AS/no-AT — name-model residual
	}
	fields = append(fields[:fieldsAt:fieldsAt],
		append(innerFields, fields[fieldsAt:]...)...)
	rc := values.NewRawRecordConstructorValue(fields...)
	if fullBaked {
		values.AssertOrdinalJoinSeed(rc)
	}

	// Predicates: the cluster root's OWN ON conjunct (gatherInnerClusterPreds
	// deliberately skips its argument's own ON — "the root's own ON is the
	// caller's") + the nested inner joins' ON conjuncts + the unnest join's
	// (a comma join carries none itself, but the ROTATION parks every
	// collected enclosed-spine conjunct here), cross-leg conjuncts baked
	// through the same legTypes the seed used. The root ON may reference the
	// ELEMENT/ordinal alias (that is WHY the rotation parks it at the root),
	// so it takes rewriteUnnestPredicate — the WHERE merge arm's exact
	// treatment: the Explode flows a bare scalar (no-AT) or `_0`/`_1` fields,
	// and an unrewritten `FieldValue(QOV(EL), "EL")` evaluates NIL, silently
	// dropping or misfiltering every row (review finding, pinned). The
	// LEFT-cluster ONs cannot reference the element (SQL scope: bound after
	// them) and stay unrewritten.
	var preds []predicates.QueryPredicate
	// INNER root only: an inner cluster root's ON is WHERE-equivalent and the
	// gather (via gatherInnerClusterPreds' "the root's own ON is the caller's"
	// contract) must hoist it here. A NON-inner root is the class-1 OPAQUE
	// OUTER box: its ON is the outer join's GATING condition, already carried
	// inside the box leg's own translation — hoisting it too would re-apply it
	// as a flat-select filter and silently convert LEFT to INNER (the padded
	// row's NULL-supplied columns fail the re-applied ON and the row drops).
	if leftJoin.Kind == logical.JoinInner {
		if qp, isQP := leftJoin.OnPredicate.(predicates.QueryPredicate); isQP && qp != nil {
			preds = append(preds, qp)
		}
	}
	if j.OnPredicate != nil {
		if qp, isQP := j.OnPredicate.(predicates.QueryPredicate); isQP {
			preds = append(preds, rewriteUnnestPredicate(qp, u))
		}
	}
	// INNER root only — the SAME opaque-box barrier as the ON-hoist guard
	// above, for the NESTED cluster ONs. A non-inner root is the class-1
	// opaque OUTER box; its whole internal predicate spine (e.g. the nested
	// inner cluster ON of `SRC LEFT JOIN (AUX JOIN AUX2 ON x.XID = y.YID)`)
	// is already enforced inside the box leg's own translation. Re-collecting
	// those here and re-applying them flat over the box's NULL-padded rows
	// fails on the null-supplied slots and silently drops the preserved row
	// (LEFT→INNER — the ON-hoist bug's twin). Only an INNER root's nested ONs
	// are WHERE-equivalent and belong on the flat select.
	if leftJoin.Kind == logical.JoinInner {
		preds = append(preds, gatherInnerClusterPreds(leftJoin)...)
	}
	preds = bakeGatedJoinPredicates(preds, legTypes)

	// RECORD the seed's legTypes for the BOX-outer arm, keyed by the
	// unnest-join node — the WHERE-merge arm bakes the filter's box-leg
	// conjunct over THIS map (the seed and the merge must share one
	// authority: the box select has only 2 quantifiers, indistinguishable by
	// count from the binary name-model select, and a re-derived map would
	// key the box's legs by aliases this select does not bind). Written only
	// on SUCCESS (every decline above returns before this point).
	if leftJoin.Kind != logical.JoinInner {
		if t.unnestGatherBoxLegTypes == nil {
			t.unnestGatherBoxLegTypes = make(map[*logical.LogicalJoin]map[string]bakeLegType)
		}
		t.unnestGatherBoxLegTypes[j] = legTypes
	}

	seedSel := expressions.NewSelectExpressionWithJoinType(
		rc,
		quantifiers,
		preds,
		sourceAliases,
		expressions.JoinInner,
	)
	return seedSel
}

// fieldValueReferencesInner reports whether a seed field's VALUE reads the unnest's
// Explode inner correlation (a bare QOV(inner) for a scalar element, or ofOrdinal
// over QOV(inner) for a with-ordinality element/ordinal). It is the seed's OWN
// definition of "which field is the element"; translateAggregate uses it to locate
// the ELEMENT/ordinal field(s) and bake a grouped element read POSITIONALLY at that
// slot (element-first — winning the bare namespace over a same-named outer column,
// the name-model shadow), and slotInGatheredSeed consults the resulting element slots.
func fieldValueReferencesInner(v values.Value, inner values.CorrelationIdentifier) bool {
	switch tv := v.(type) {
	case *values.QuantifiedObjectValue:
		return tv.Correlation == inner
	case *values.FieldValue:
		if qov, ok := tv.Child.(*values.QuantifiedObjectValue); ok {
			return qov.Correlation == inner
		}
	}
	return false
}

// bakeGatheredGroupValue rewrites a GROUP-BY key / aggregate operand Value into a
// POSITIONAL read over the un-collapsed gathered seed. It
// RECURSES the value tree (values.Replace) so a compound operand — `SUM(A.K+B.K)`,
// `SUM(A.K*2)` — bakes each nested FieldValue LEAF that names a seed slot (a
// top-level-only bake would leave a dup column in the operand reading first-match
// = silent wrong rows). Each leaf: a qualified (`FieldValue{Field:"K",Child:QOV("B")}`),
// flat-dotted (`FieldValue{Field:"A.K"}`), or bare/element (`FieldValue{Field:"EL"}`)
// column that names a slot becomes a resolved-ordinal read there; a literal, `*`, or a
// reference the seed doesn't carry passes through unchanged (name-model, resolved
// elsewhere). This is the group-by twin of bakeGatedJoinPredicates — the
// qualifier-honoring positional read that replaces the wrap's name key.
//
// Two of those three bake. The flat-dotted shape DECLINES (slotInGatheredSeed's
// first arm): its qualifier is text with no identifier behind it, and this walk
// selects a leg window by correlation. It is still recognized here — `qualified`
// is set for it — because a dotted reference must not fall into the BARE
// namespace, which is the fail-open that made `A.K` read a same-named element.
//
// That last sentence describes what the flag does NOW. It did not describe what
// the flag did when it was written: the flag reached only the bare-LEG scan, while
// the element arm beside it was ungated, so a qualified read whose correlation
// failed to select a window still read the element. The flag gates the whole bare
// namespace today because slotInGatheredSeed declines every unresolved qualified
// read before either bare arm — see the gate there.
func bakeGatheredGroupValue(v values.Value, windows map[values.CorrelationIdentifier]values.OrdinalSeedLegWindow, elementSlots map[string]int, seedQOV values.Value) values.Value {
	return values.Replace(v, func(node values.Value) values.Value {
		fv, isFV := node.(*values.FieldValue)
		// A source-relative baked ref names a seed slot like its lazy twin —
		// re-bake it here; only machinery-owned baked nodes are final.
		if !isFV || (fv.Resolved != nil && !fv.SourceRelativeBaked()) {
			return node
		}
		qualified, col := false, strings.ToUpper(fv.Field)
		// corr is the reference's OWN correlation where it has one, and the zero
		// identifier where the qualifier was sliced out of the NAME instead. Both
		// arms used to produce a string and hand it to one lookup, which made them
		// indistinguishable at the point of use; the leg window is now selected by
		// the correlation, so only the arm that has one can select a leg.
		//
		// `qualified` still tracks BOTH, and carrying the dotted arm in it is what
		// makes the resolver SAFE rather than merely tidy. It gates ONE thing, and
		// that is the correction: the ENTIRE bare namespace of slotInGatheredSeed —
		// the element arm and the bare-leg scan alike. It used to be described as
		// gating "two things", the bare-column fallback and the no-identifier
		// decline, and that enumeration was the bug in prose: it omitted the element
		// arm because the element arm was not in fact gated. Dropping the flag on the
		// dotted arm would send `A.K` into that namespace, where the element-first
		// fallback answers with the ELEMENT whenever the two share a leaf name. A
		// dotted reference is not a bare one whether or not its qualifier resolves;
		// that is the whole content of the flag.
		//
		// The standing seed-window reader census pins the dotted arm's population:
		// its QUALIFIED-NO-IDENTITY class is a hard zero, so a producer that starts
		// routing flat-dotted names here reads RED instead of silently declining the
		// gather to the name model.
		var corr values.CorrelationIdentifier
		if qov, ok := fv.Child.(*values.QuantifiedObjectValue); ok {
			qualified, corr = true, qov.Correlation
		} else if dot := strings.IndexByte(col, '.'); dot >= 0 {
			qualified, col = true, col[dot+1:]
		}
		if slot, ok := slotInGatheredSeed(windows, elementSlots, corr, col, qualified); ok {
			if baked, err := values.NewFieldValueOfOrdinal(seedQOV, slot); err == nil {
				// The positional read (Resolved ordinal) is authoritative; Field is
				// display-only. Preserve the ORIGINAL reference's display name so the
				// group-by OUTPUT column matches what the SELECT projects (`EL`, `A.K`).
				values.NoteFieldValueMint(fv.Field, baked.Resolved != nil)
				baked.Field = fv.Field
				return baked
			}
		}
		return node
	})
}

// slotInGatheredSeed resolves a group-by key / operand reference to its flat slot in
// the gathered seed. It has TWO namespaces and they do not overlap.
//
// A QUALIFIED read is answered by its qualifier's leg window or not at all: the
// leg routes through its [Offset,Width) window from the SHARED authority
// OrdinalSeedLegWindows (agreeing bit-for-bit with the executor spans by the
// cross-agreement fixture), and every way of failing that lookup declines.
//
// A BARE read gets the element-first namespace: the ELEMENT wins (so an element AS
// alias shadowing a leg column reads the element), its slot being its rc index
// located via the seed's OWN element predicate (fieldValueReferencesInner) — a
// single distinguished field, NOT a layout window (rc-index↔slot is the substrate,
// no drift) — and a bare LEG column resolves only when exactly one leg carries it.
//
// Element-first is a BARE-namespace rule. Describing it as the site's first step
// full stop is what this doc used to do, and it read as a licence for the element
// to answer a qualified reference it does not name.
func slotInGatheredSeed(windows map[values.CorrelationIdentifier]values.OrdinalSeedLegWindow, elementSlots map[string]int, corr values.CorrelationIdentifier, col string, qualified bool) (int, bool) {
	// A QUALIFIED read with NO correlation DECLINES here, before anything else.
	// This is the flat-dotted arm (`FieldValue{Field:"A.K"}`), whose qualifier was
	// sliced out of the NAME: there is no identifier to select a leg window with,
	// so the leg lookup below cannot run. Falling through was a FAIL-OPEN — the
	// element-first fallback would answer with the ELEMENT's slot whenever the
	// element and the qualified leg column share a leaf name, so `A.K` silently
	// read the element instead of A's K. The qualifier is not advisory; a read
	// that states one and cannot be resolved BY it must resolve to nothing.
	//
	// A decline, not a text route. Re-keying by the sliced qualifier would mint a
	// CorrelationIdentifier out of a column name — the forgery RFC-197 exists to
	// remove — and the sibling walk (rebaseLegRefsToBox) declines the identical
	// shape for the identical reason. The real answer is CQ-52: the parser HAS
	// the qualifier/leaf segments and joins them only for this site to split them
	// back apart. Until it hands them over, this shape goes to the name model.
	if qualified && corr.IsZero() {
		if values.LegIdentityCensusEnabled() {
			values.RecordSeedWindowRead(values.SeedWindowSiteGatheredGroupSlot,
				values.SeedWindowQualifiedNoIdentity)
		}
		return 0, false
	}
	// A QUALIFIED read whose CORRELATION names a LEG window resolves to THAT leg's
	// column — the qualifier wins (`U.V` is U's V, never the element, even when the
	// element AS alias is also `V`). This precedes element-first so an explicit leg
	// qualifier is never shadowed by a same-named element.
	if !corr.IsZero() {
		w, isLeg := windows[corr]
		if values.LegIdentityCensusEnabled() {
			values.RecordSeedWindowLookup(values.SeedWindowSiteGatheredGroupSlot, isLeg)
		}
		// This site's CONTRACT is a flat slot index into the gathered seed's row,
		// and a NESTED leg has none: its column lives one level down, reachable
		// only by descending. LegKindUnset is refused for a different reason — a
		// window that reached a slot resolver without a stated kind is a producer
		// bug, and the key it would have resolved is better unresolved than
		// resolved to a guess.
		//
		// Neither refusal happens HERE. Failing this condition only leaves the
		// block; the decline that answers for it is the `qualified` gate below,
		// which owns every way a qualified read can fail to resolve. This comment
		// used to claim the two kinds "DECLINE" at this point and that the arm
		// above already declined a correlation-less qualified read "a few lines
		// above" — the second half was true, the first was not, and the gap
		// between them was a live wrong-column read.
		if isLeg && w.Kind == values.LegKindFlatRun {
			if idx, found := w.Typ.FieldIndexUnique(col); found {
				return w.Offset + idx, true
			}
		}
	}
	// A QUALIFIED read that its own qualifier could not resolve DECLINES, and this
	// is the GENERAL form of the rule the no-correlation arm states at the top of
	// this function: a reference that names a source is answered BY that source or
	// not at all. Three shapes fail the lookup above, and each used to FALL
	// THROUGH into the element/bare arms below, where `U.V` silently read the
	// ELEMENT's `V`. They are NOT equally reachable, and saying so is the point —
	// an unqualified "three shapes reach here" was what this comment said, and a
	// reachability claim nobody checked is how the original defect survived a
	// reading:
	//
	//   - LIVE — a correlation no window is filed under (an existential inner's
	//     quantifier, say) — the same shape bakeUnnestElementRefOrdinal was
	//     patched to dodge at the PRODUCER, which left the resolver still wrong
	//     for every other producer;
	//   - LIVE — a FLAT window that declares `col` TWICE, so FieldIndexUnique
	//     picks nothing. That shape became reachable when the first-match
	//     FieldIndex was deleted: a leg window's Typ is a leg-concat for a
	//     clustered box run and may legitimately repeat a leaf name;
	//   - DEFENSIVE at THIS call site — a window whose Kind cannot be
	//     flat-addressed (LegKindNested, LegKindUnset), per the block above. No
	//     nested window can exist here at all: LegKindNested is stamped in exactly
	//     one place, positionalMergeWindows, which is reachable only through
	//     `acceptNested && IsPositionalMergeRC` — and the windows here come from
	//     gatheredSeedBakeContext, which calls OrdinalSeedLegWindows with
	//     acceptNested=false. So the kind is never minted on this path, rather
	//     than being minted and then filtered. (The neighbouring
	//     `leg.Kind == LegKindNested && !acceptNested` refusal is a different
	//     mechanism — it governs SUB-legs of a box run — and is not what makes
	//     this arm unreachable.)
	//     Every window that survives to this function is stamped LegKindFlatRun.
	//     The dispatch stays because the refusal must be by KIND and not by luck
	//     of which entry point the caller picked — but it is a contract guard,
	//     not a class with production traffic, and the difference matters to
	//     anyone reasoning about what the gate below actually catches.
	//
	// Java answers the same way structurally rather than by a rule: each
	// quantifier is bound under its OWN alias (RecordQueryFlatMapPlan.java:135,140)
	// and the unnest element lives on the Explode's own quantifier
	// (LogicalOperator.java:318-329), so there is no shared namespace a qualified
	// read could fall into.
	//
	// ONE gate, not one per arm. The arms below are the BARE namespace and nothing
	// else; the bare-leg scan's own `!qualified` guard was removed with this gate's
	// introduction, because two encodings of one rule is what let a reader assume
	// the element arm carried the guard it did not.
	//
	// IT DOES NOT SWALLOW THE ELEMENT-QUALIFIED READ, and that is worth stating
	// because `qualified` is set for ANY FieldValue over a QuantifiedObjectValue —
	// not only for a dotted spelling — so this line reads like a gate on a
	// namespace the ELEMENT legitimately owns. The corpus really does mint an
	// element-qualified key: `FROM GD, GD.ARR AS V GROUP BY V` groups on
	// FieldValue(QOV(V), V), so the grouping is the ELEMENT and not a later
	// same-named column. It resolves above this gate, in the leg arm, because
	// OrdinalSeedLegWindows files the unnest element under its OWN correlation —
	// a synthesized one-column flat run for a scalar element, an ordinary leg run
	// for a record one. The gate is therefore reachable only by a qualifier that
	// names NO source in the seed, which is the whole of its intent. That
	// invariant lives in the producer, not here, so it is pinned there:
	// TestGatheredSeed_ElementQualifiedReadIsServedByItsOwnLegWindow drives all
	// three arms off a real seed, and TestFDB_ArrayUnnestOrdinality/R19b pins the
	// end-to-end consequence — the group key bakes to the element's slot rather
	// than degrading to the name model.
	if qualified {
		return 0, false
	}
	// A BARE read: the ELEMENT wins the bare namespace (element-first — an element
	// AS alias shadowing a later leg column reads the element, matching the
	// name-model shadow the un-collapse preserves). Element-first is a rule about
	// the BARE namespace only; a qualified read never arrives here.
	if slot, ok := elementSlots[col]; ok {
		return slot, true
	}
	// A BARE LEG column that is neither the element nor an alias-qualified read — e.g.
	// GROUP BY over a SELECT-* CTE/derived source, where the CTE output column `AID`
	// carries no `A.` qualifier. Resolve it against the leg windows when exactly ONE leg
	// carries it (unambiguous). An ambiguous bare column (a dup-named `K` present in two
	// legs) declines here → name-model, exactly as an unqualified dup would be ambiguous
	// in SQL. Map order is irrelevant: a unique hit is order-independent; >1 declines.
	//
	// It carried its own `!qualified` guard until the single gate above took over.
	// The guard was real here and ABSENT on the element arm above, which is
	// precisely how the fall-through hid: two bare arms, one gated, and a reader
	// checking either one drew the wrong conclusion about the other.
	slot, hits := 0, 0
	for _, w := range windows {
		// THE CENSUS CANNOT SEE THIS ARM. It is not a keyed read — it ranges
		// every window instead of selecting one — so the seed-window reader
		// census records nothing here, and this line is exactly as dangerous as
		// the five keyed sites: it does offset arithmetic.
		//
		// A NESTED window must not contribute a hit, for the same reason the
		// qualified gate above declines one: `w.Offset + idx` is not this leg's
		// column, and worse, a nested contribution would ALSO move `hits` — so a
		// column present in one flat leg and one nested leg would go from a
		// unique resolution to an ambiguous decline, or a nested-only column
		// would resolve to a slot in a neighbouring leg.
		if w.Kind != values.LegKindFlatRun {
			continue
		}
		if idx, found := w.Typ.FieldIndexUnique(col); found {
			slot, hits = w.Offset+idx, hits+1
		}
	}
	if hits == 1 {
		return slot, true
	}
	return 0, false
}

// gatherLegsWithBuriedUnnest walks j's direct inner-join spine collecting the
// PLAIN legs (FROM order), every nested ON conjunct, and exactly ONE buried
// unnest (a nested `Join(L, Unnest)` — the `FROM A, A.arr AS x, B` enclosed
// class). ok=false when there is no unnest or more than one — the caller then
// leaves the original tree to today's paths. A NON-INNER (or existential-
// rider) nested join is NOT a decline: the walk absorbs that whole subtree as
// ONE OPAQUE plain leg (never decomposing across it), which is exactly what
// makes rotating the remaining inner legs safe — an unnest buried UNDER such
// a subtree is invisible here and stays on the residual path.
func gatherLegsWithBuriedUnnest(j *logical.LogicalJoin) (plainLegs []logical.LogicalOperator, preds []predicates.QueryPredicate, uLeft logical.LogicalOperator, u *logical.LogicalUnnest, unnestPos int, ok bool) {
	ok = true
	var walk func(op logical.LogicalOperator)
	walk = func(op logical.LogicalOperator) {
		if !ok {
			return
		}
		nj, isJoin := op.(*logical.LogicalJoin)
		if !isJoin || nj.Kind != logical.JoinInner || len(nj.OnExistsSubqueries) > 0 {
			plainLegs = append(plainLegs, op)
			return
		}
		if un, isUn := nj.Right.(*logical.LogicalUnnest); isUn {
			if u != nil {
				ok = false // a second unnest — out of scope (chained/multi)
				return
			}
			walk(nj.Left)
			u = un
			uLeft = nj.Left
			unnestPos = len(plainLegs) // FROM position: legs before it are walked
			if qp, isQP := nj.OnPredicate.(predicates.QueryPredicate); isQP && qp != nil {
				preds = append(preds, qp)
			}
			return
		}
		walk(nj.Left)
		walk(nj.Right)
		if qp, isQP := nj.OnPredicate.(predicates.QueryPredicate); isQP && qp != nil {
			preds = append(preds, qp)
		}
	}
	walk(j)
	if !ok || u == nil {
		return nil, nil, nil, nil, 0, false
	}
	return plainLegs, preds, uLeft, u, unnestPos, true
}

// rotateEnclosedUnnest probes the ENCLOSED unnest class (`FROM A, A.arr AS x,
// B` — the unnest join buried as a LEG of a larger inner cluster) and, when
// it classifies, ROTATES the cluster to the root form
// translateGatheredUnnestCluster owns: Join(Join(plain legs, FROM order),
// Unnest). The rotation is inner-join-equivalent — the lateral dependency
// needs only the owner in scope, and every collected ON conjunct rides the
// rebuilt ROOT's OnPredicate (a conjunct may reference the ELEMENT/ordinal
// alias, `... INNER JOIN B ON B.K = EL`, which is only in scope at the flat
// select the builder folds the root ON into; an ON-free left also keeps the
// gate probe on the pure comma cluster case).
//
// The classification is MINIMAL and fail-open (ok=false — the caller keeps
// the ORIGINAL tree, whose residual path still produces the faithful
// diagnostics), mirroring translateUnnestJoin's gauntlet with DECLINES
// instead of errors. Seed-field order places the element LAST (not at its
// FROM position) — observable only via SELECT-*-over-multi-source, which
// cannot plan today (a known follow-on fix, not yet implemented).
func (t *cascadesTranslator) rotateEnclosedUnnest(j *logical.LogicalJoin) (rebuilt *logical.LogicalJoin, u *logical.LogicalUnnest, elementType values.Type, fieldName string, unnestPos int, ok bool) {
	if t.md == nil || j.Kind != logical.JoinInner || len(j.OnExistsSubqueries) > 0 {
		return nil, nil, nil, "", 0, false
	}
	if _, rootUnnest := j.Right.(*logical.LogicalUnnest); rootUnnest {
		return nil, nil, nil, "", 0, false // the root form — translateUnnestJoin owns it
	}
	plainLegs, preds, uLeft, u, unnestPos, gok := gatherLegsWithBuriedUnnest(j)
	if !gok || len(plainLegs) < 2 {
		return nil, nil, nil, "", 0, false
	}

	// Classification against the unnest's OWN scope (uLeft — the sources
	// before it in FROM order). Multi-segment paths (`t.rec.arr`) pass
	// through: the gathered builder bakes them as fused root+suffix
	// collections, and the classifier below validates every intermediate
	// segment.
	if len(u.Segments) < 2 {
		return nil, nil, nil, "", 0, false
	}
	outerTable := findOuterScanTable(uLeft, u.Segments[0])
	if outerTable == "" {
		return nil, nil, nil, "", 0, false
	}
	if t.outerSourceIsCTE(outerTable) || outerSourceIsDerivedTable(uLeft, u.Segments[0]) {
		return nil, nil, nil, "", 0, false
	}
	elementType, fieldName, isArray, _ := t.unnestArrayElementType(outerTable, u.Segments[1:])
	if !isArray {
		return nil, nil, nil, "", 0, false
	}
	if containsLateralUnnest(uLeft) {
		return nil, nil, nil, "", 0, false
	}

	newLeft := plainLegs[0]
	for _, leg := range plainLegs[1:] {
		newLeft = logical.NewJoin(newLeft, leg, logical.JoinInner, "")
	}
	if _, isLJ := newLeft.(*logical.LogicalJoin); !isLJ {
		return nil, nil, nil, "", 0, false
	}

	// Alias-collision and AS/AT-distinct checks against ALL plain legs — the
	// flat select binds every leg alias (including legs AFTER the unnest in
	// FROM order), which is the root-form gauntlet's scope after the
	// rotation. The residual rejects these faithfully when this declines.
	bound := outerBoundAliases(newLeft)
	collide := func(name string) bool {
		if name == "" {
			return false
		}
		if strings.EqualFold(name, sourceAlias(newLeft)) {
			return true
		}
		_, isBound := bound[strings.ToUpper(name)]
		return isBound
	}
	if collide(u.Alias) || collide(u.AtAlias) {
		return nil, nil, nil, "", 0, false
	}
	if unnestAliasReject(u) != nil {
		// Decline the shape; the raw body surfaces the loud duplicate-alias
		// rejection at translation (unnestAliasReject in translateUnnestJoin).
		return nil, nil, nil, "", 0, false
	}

	var onPred predicates.QueryPredicate
	if len(preds) > 0 {
		onPred = preds[0]
		if len(preds) > 1 {
			onPred = predicates.NewAnd(preds...)
		}
	}
	return logical.NewJoinWithPredicate(newLeft, u, logical.JoinInner, onPred), u, elementType, fieldName, unnestPos, true
}

// translateEnclosedUnnestGather is the dispatch for the enclosed unnest
// class: rotate (rotateEnclosedUnnest), then hand the root form to the
// shared gathered builder. Fail-open — nil falls back to the caller's
// name-model paths.
func (t *cascadesTranslator) translateEnclosedUnnestGather(j *logical.LogicalJoin) expressions.RelationalExpression {
	if t.inInnerCluster || t.unnestUnderExistential {
		return nil
	}
	// The translateFilter probe already built this select — consume it
	// (once) instead of translating the cluster a second time.
	if sel, cached := t.enclosedGatherCache[j]; cached {
		delete(t.enclosedGatherCache, j)
		return sel
	}
	rebuilt, u, elementType, fieldName, unnestPos, ok := t.rotateEnclosedUnnest(j)
	if !ok {
		return nil
	}
	return t.translateGatheredUnnestCluster(rebuilt, u, unnestSourceCorrelation(u), elementType, fieldName, unnestPos)
}
