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
// the merged row's Datum) with the sub-field descended by name. The only new
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

// chainedUnnestCollection builds the Explode collection for a chained unnest:
// a multi-accessor FieldValue rooted at the OWNER alias (segment 0 — reads the
// element proto message off the merged row's Datum key) with the sub-path
// (segments[1:]) descended by NAME through the proto-message arm. Ordinals are
// the loud -1 sentinel (the descent is name-addressed; a struct element never
// materializes positionally). Child = QOV(sourceAlias(j.Left)) = the owner
// alias the merged row binds. Design ruling 4 condition 3.
func chainedUnnestCollection(u *logical.LogicalUnnest, outerCorr values.CorrelationIdentifier, elementType values.Type) values.Value {
	accs := make([]values.ResolvedAccessor, 0, len(u.Segments))
	for _, seg := range u.Segments {
		accs = append(accs, values.ResolvedAccessor{Field: strings.ToUpper(seg), Ordinal: -1})
	}
	return &values.FieldValue{
		Field:    strings.ToUpper(u.Segments[len(u.Segments)-1]),
		Typ:      values.NewArrayType(true, elementType),
		Child:    values.NewQuantifiedObjectValue(outerCorr),
		Resolved: &values.FieldPath{Accessors: accs},
	}
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

	// FAIL-OPEN name-model residual (bare-twin / CTE-rooted / 3+-link chains).
	// The chained outer (`j.Left`) is translated with the enclosure bit LEFT AS
	// THIS unnest's translateUnnestJoin set it (name-model residual): a chained
	// unnest is always name-model here, and a base-table unnest nested in
	// `j.Left` relies on that enclosure bit to stay name-model too. A PRIOR
	// chained link in `j.Left` still dispatches chained because the dispatch is
	// gated on isChainedUnnest, NOT on the enclosure bit (Java nests each link the
	// same way regardless of enclosure).
	outerRef := t.translateRef(j.Left)
	if outerRef == nil {
		return nil
	}

	collection := chainedUnnestCollection(u, outerCorr, elementType)
	explode := expressions.NewExplodeExpressionWithOrdinality(collection, u.AtAlias != "")
	innerQ := expressions.NamedForEachQuantifier(innerCorr, expressions.InitialOf(explode))
	outerQ := expressions.NamedForEachQuantifier(outerCorr, outerRef)

	resultValue := t.buildUnnestResultValue(j.Left, outerCorr, outerAlias, innerCorr, u, elementType)
	if resultValue == nil {
		return nil
	}
	return expressions.NewSelectExpressionWithJoinType(
		resultValue,
		[]expressions.Quantifier{outerQ, innerQ},
		nil,
		[]string{outerAlias, innerCorr.Name()},
		expressions.JoinInner,
	)
}

// translateChainedUnnestOrdinal builds the ORDINAL SelectExpression for a chained
// unnest whose FIRST link ordinalizes (RFC-173 S4 Slice 1), or returns nil to
// DECLINE (the caller fails open to the name-model path). The seed is the SAME
// unnestOrdinalSeed the single-source W4c path uses — its outer positional run
// (ofOrdinal over ordinalLegType(j.Left)) carries the first link's merged row
// [outer cols … element] positionally, and unnestSeedInnerFields carries the
// chained element/ordinal — and the collection is unnestBakedRootCollection
// rooted at the OWNER ALIAS column (rootSegmentIndex 0). Both DECLINE (nil) on an
// underivable leg, keeping this fail-open.
func (t *cascadesTranslator) translateChainedUnnestOrdinal(
	j *logical.LogicalJoin,
	u *logical.LogicalUnnest,
	outerAlias string,
	outerCorr, innerCorr values.CorrelationIdentifier,
	elementType values.Type,
	prevEnclosure bool,
) expressions.RelationalExpression {
	// The chained link ordinalizes only when it is NOT enclosed in a larger
	// name-model composition (the :1565 !prevEnclosure gate). An enclosed chained
	// link (a deeper chain's inner link, or a chain under a name-model parent)
	// keeps the name-model residual so its outer flows a name-keyed row.
	if prevEnclosure || len(u.Segments) < 2 {
		return nil
	}
	// The FIRST link must be a single-source lateral unnest that will itself take
	// the :1565 binary ordinal seed. `j.Left` is that first link: a LogicalJoin
	// whose Right is the first LogicalUnnest and whose Left is the BASE. Mirror
	// the first link's own :1565 gate on that base precisely — anything else
	// (a deeper-chain base whose clusterArity is poison, a multi-source base,
	// an exists-unsafe base, a single-segment first link) declines here so we
	// never build an ordinal seed over a first link that stays name-model.
	firstLink, ok := j.Left.(*logical.LogicalJoin)
	if !ok {
		return nil
	}
	firstUnnest, ok := firstLink.Right.(*logical.LogicalUnnest)
	if !ok {
		return nil
	}
	firstBase := firstLink.Left
	if t.clusterArity(firstBase) != 1 || !t.unnestExistsSeedSafe(firstBase) || len(firstUnnest.Segments) < 2 {
		return nil
	}

	// Build the ordinal collection + seed FIRST (types only, no outerRef yet): a
	// nil from either declines WITHOUT having cleared the enclosure or translated
	// an ordinal first link, so the caller's name-model fallback stays sound. The
	// collection roots at the owner alias (Segments[0]) — the first link's ELEMENT
	// column. That element is at slot len(ordinalLegColumns(firstBase)) in
	// ordinalLegType(j.Left) (= ordinalLegColumns(firstBase) ++ legColumns(first
	// unnest), the element being legColumns[0]). Pass that slot EXPLICITLY: a
	// name lookup would pick an OUTER column that SHADOWS the alias (an outer scalar
	// named the same as the first unnest alias precedes the element in the merged
	// row → wrong root → the sub-path descends the wrong column).
	firstBaseCols := t.ordinalLegColumns(firstBase)
	if firstBaseCols == nil {
		return nil
	}
	elementRootIdx := len(firstBaseCols)
	fieldName := u.Segments[len(u.Segments)-1]
	collection := t.unnestBakedRootCollection(j.Left, outerCorr, u, fieldName, elementType, 0, elementRootIdx)
	if collection == nil {
		return nil
	}
	resultValue := t.unnestOrdinalSeed(j.Left, outerCorr, innerCorr, u, elementType)
	if resultValue == nil {
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
