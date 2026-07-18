package executor

import (
	"bytes"
	"context"
	"fmt"

	"google.golang.org/protobuf/proto"

	"fdb.dev/gen"
	"fdb.dev/pkg/recordlayer"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
)

// flatMapCursor implements RecordCursor[QueryResult] matching Java's
// FlatMapPipelinedCursor. For each outer row, re-executes the inner
// plan with the outer row bound as a correlation.
//
// Go simplification: no async pipelining (Java uses pipeline depth 5
// for overlapping FDB I/O). Continuation: Go uses FlatMapContinuation
// proto (outer+inner+check_value). check_value stores the outer row's
// PK bytes; on resume, verifies the outer row hasn't changed between
// transactions (concurrent-modification detection).
// LEFT-OUTER note: this cursor has NO leftOuter/innerHadMatch state. LEFT-OUTER
// null-extension is emergent from the inner plan being wrapped in DefaultOnEmpty
// (OrElse continuation carries the empty-vs-nonempty decision, resume-safe), as in
// Java — see rule_implement_nested_loop_join.go. The earlier in-memory flag pair
// re-decided the extension per page (the F2 spurious-null resume bug) and was
// removed as dead code once the OrElse lowering became the only production path.
type flatMapCursor struct {
	outerCursor recordlayer.RecordCursor[QueryResult]
	innerPlan   plans.RecordQueryPlan
	store       *recordlayer.FDBRecordStore
	evalCtx     *EvaluationContext
	outerAlias  values.CorrelationIdentifier
	innerAlias  values.CorrelationIdentifier
	resultValue values.Value
	props       recordlayer.ExecuteProperties

	innerCursor    recordlayer.RecordCursor[QueryResult]
	currentOuter   *QueryResult
	outerExhausted bool
	closed         bool

	// build is the ordinal-BUILD state, probed ONCE at construction from
	// resultValue (nil = a non-build FlatMap: an identity pass-through or a
	// folded projection). When enabled, computeResult builds the positional row
	// from the RC with per-leg bindings; the RC's baked references resolve by
	// ordinal against that row.
	build *ordinalJoinBuild

	// outerBakedType is the DISABLED-BUILD probe result: the outer's typed
	// RecordType recovered from the inner plan's FrontierPinned baked
	// references over outerAlias, when this cursor has NO build of its own (an
	// identity-RV existential FlatMap over an ordinal outer). Non-nil is the
	// layout authority for the outer binding: adaptLegPositional adapts the
	// outer row to it (an index-shaped covering row is re-synthesized into the
	// logical layout the baked ordinals were bound against); adaptation
	// failure is LOUD — never a silent wrong-slot read.
	outerBakedType *values.RecordType

	// foldLegSpans / foldWindowsOK: when this FlatMap's OUTER is a gated
	// ordinal join (a projected-EXISTS fold whose step-1 NLJ builds the
	// leg-concat seed), the folded projection reads leg columns as
	// heterogeneous refs — flat baked merged-row reads and QOV leg refs. The
	// projection is NEVER rebased; instead it evaluates over the step-1 merged
	// positional row through legWindowRowContext(pos, ctx, foldLegSpans), the
	// SAME context every plain gated-join downstream projection uses (flat baked
	// refs read the merged row's slots, legWindowBinder windows the QOV refs).
	// Derived once from the outer plan's ordinal seed (downstreamLegWindows).
	foldLegSpans  []legSpan
	foldWindowsOK bool

	// outerMergedType is the outer gated ordinal join's
	// merged row RecordType, when this FlatMap's outer is one (nil otherwise).
	// The identity-FlatMap pass-through propagates a leg-independent existential's
	// gated outer positional row through it — adapting against this type (LOUD on
	// a layout mismatch) so a minted-dup upper resolves positionally instead of
	// the executor's probe-gate declining loud at translation.
	outerMergedType *values.RecordType

	// outerIdentityPassthrough marks the PLAIN-SCAN correlated-EXISTS
	// shape — a WHERE-EXISTS identity pass-through (RV == QOV(outer)) whose outer
	// is neither an inner-baked existential (outerBakedType nil) nor a gated join
	// (outerMergedType nil). The outer binds onto its OWN scan positional row
	// (bound RAW: the inner subquery's correlated ref carries a SOURCE-RELATIVE
	// baked ordinal — the resolver's declared-column-order bind — and the scan
	// row IS the source's own layout, so the ordinal reads the right slot; a
	// covering scan that cannot bind the logical layout is refused loud at
	// construction), and the identity output edge propagates that same
	// positional row so the downstream projection reads by ordinal. false = a
	// non-identity RV, or a baked/gated outer with its own more-specific layout
	// authority.
	outerIdentityPassthrough bool

	// lastNoNext replays a prior terminal result on a contract-violating
	// re-call (Java FlatMapPipelinedCursor:123-125: any no-next result is
	// cached and returned verbatim). Without it, a re-call after an inner
	// out-of-band stop (innerCursor already nil) silently advances to the
	// NEXT outer row — dropping the unfinished inner's rows — and a re-call
	// after an outer out-of-band stop reports a false SourceExhausted.
	lastNoNext *recordlayer.RecordCursorResult[QueryResult]

	// Continuation state for cross-transaction resume.
	priorOuterContinuation recordlayer.RecordCursorContinuation
	lastOuterContinuation  recordlayer.RecordCursorContinuation
	initialInnerCont       []byte
	hasPendingInner        bool
	pendingCheckValue      []byte
}

func newFlatMapCursor(
	outerCursor recordlayer.RecordCursor[QueryResult],
	outerPlan plans.RecordQueryPlan,
	innerPlan plans.RecordQueryPlan,
	store *recordlayer.FDBRecordStore,
	evalCtx *EvaluationContext,
	outerAlias, innerAlias values.CorrelationIdentifier,
	resultValue values.Value,
	props recordlayer.ExecuteProperties,
) (*flatMapCursor, error) {
	build, err := newOrdinalJoinBuild(resultValue, nil)
	if err != nil {
		return nil, err
	}
	// A TRANSLATED top RV (fused merge references) yields no spans from the RV
	// alone — recover them through the leg subplans' result values (the merge
	// RC is where the merged-away leg aliases survive), so the build's leg
	// windows (Spans) get populated for the leg adapter even when the top RV
	// was folded past a plain probe.
	if build.enabled() {
		legRVs := make(map[values.CorrelationIdentifier]values.Value)
		addJoinLegRV(legRVs, outerAlias, outerPlan)
		addJoinLegRV(legRVs, innerAlias, innerPlan)
		if !build.WindowsOK {
			if spans, _, ok := ordinalJoinSpansOf(resultValue, legRVs); ok {
				build.Spans = spans
				build.WindowsOK = true
			}
		}
	}
	// The correlated implementation pushes the join's baked ON references INTO
	// the inner plan (SARGs, residual filters), so LegTypes must be widened
	// from the inner plan's predicate surfaces — a folded result value can
	// drop a leg those references still need (see widenLegTypesFromPlan).
	build.widenLegTypesFromPlan(innerPlan)
	// Producer context (RFC-142): a WITH-ORDINALITY unnest's inner IS an
	// ordinality Explode, flowing a row keyed by the internal `_0`/`_1`
	// positions. Mark the inner leg so it binds STRICTLY POSITIONALLY (see
	// ordinalJoinBuild.OrdinalityLegs) — a user AS/AT alias spelling `_0`/`_1`
	// then cannot route the wrong internal key, and a leg whose own
	// columns are aliased `_0`/`_1` (shape-identical, but NOT an ordinality
	// Explode) still binds correctly through the normal leg adapter.
	if build.enabled() && innerIsOrdinalityExplode(innerPlan) {
		if build.OrdinalityLegs == nil {
			build.OrdinalityLegs = map[values.CorrelationIdentifier]struct{}{}
		}
		build.OrdinalityLegs[innerAlias] = struct{}{}
	}
	// A DISABLED-build FlatMap (identity RV — the
	// WHERE-EXISTS pass-through) probes its inner plan for baked references
	// over the outer alias; a hit means the outer must bind positionally
	// (see outerBakedType).
	var outerBakedType *values.RecordType
	var foldLegSpans []legSpan
	var foldWindowsOK bool
	var outerMergedType *values.RecordType
	var outerIdentityPassthrough bool
	if !build.enabled() {
		outerBakedType = probeOuterBakedType(innerPlan, outerAlias)
		// Recognise a gated ordinal join OUTER (downstreamLegWindows
		// accepts a genuine FrontierPinned full-coverage seed only). Two
		// consumers of that recognition:
		//   - a projected-EXISTS FOLD resolves its leg refs through
		//     legWindowRowContext(foldLegSpans). EXCLUDES the identity pass-through
		//     (RV == QOV(outer)) — that reads the whole-outer object, not per-leg
		//     columns — so foldWindowsOK gates on !isIdentityOuterRV, decided once.
		//   - the identity pass-through propagates a leg-independent
		//     existential's gated outer positional row (outerMergedType), which the
		//     inner-probe (outerBakedType) misses when the exists inner is
		//     leg-independent.
		var outerIsGatedJoin bool
		if outerPlan != nil {
			foldLegSpans, outerMergedType, outerIsGatedJoin = downstreamLegWindowsTyped(outerPlan)
		}
		foldWindowsOK = outerIsGatedJoin && !isIdentityOuterRV(resultValue, outerAlias)
		// The PLAIN-SCAN correlated-EXISTS shape: an identity pass-through
		// (RV == QOV(outer)) whose outer is NOT a gated join (outerMergedType nil)
		// and whose inner carries no baked outer refs (outerBakedType nil — the
		// inner correlated ref is LAZY). The outer then binds onto its OWN scan
		// positional row and the identity output propagates it (see
		// outerIdentityPassthrough). A gated outer keeps its outerMergedType
		// authority (it is the more specific layout); this catches only the
		// all-nil plain scan.
		outerIdentityPassthrough = outerBakedType == nil && outerMergedType == nil &&
			isIdentityOuterRV(resultValue, outerAlias)
	}
	return &flatMapCursor{
		outerCursor:              outerCursor,
		innerPlan:                innerPlan,
		store:                    store,
		evalCtx:                  evalCtx,
		outerAlias:               outerAlias,
		innerAlias:               innerAlias,
		resultValue:              resultValue,
		props:                    props,
		build:                    build,
		outerBakedType:           outerBakedType,
		foldLegSpans:             foldLegSpans,
		foldWindowsOK:            foldWindowsOK,
		outerMergedType:          outerMergedType,
		outerIdentityPassthrough: outerIdentityPassthrough,
	}, nil
}

func (c *flatMapCursor) OnNext(ctx context.Context) (recordlayer.RecordCursorResult[QueryResult], error) {
	if c.closed {
		return recordlayer.NewResultNoNext[QueryResult](recordlayer.SourceExhausted, &recordlayer.EndContinuation{}), nil
	}
	if c.lastNoNext != nil {
		return *c.lastNoNext, nil
	}

	for {
		if err := ctx.Err(); err != nil {
			return recordlayer.RecordCursorResult[QueryResult]{}, err
		}
		// If we have an active inner cursor, pull from it.
		if c.innerCursor != nil {
			result, err := c.innerCursor.OnNext(ctx)
			if err != nil {
				return recordlayer.RecordCursorResult[QueryResult]{}, err
			}
			if result.HasNext() {
				innerRow := result.GetValue()

				outputRow, err := c.computeResult(*c.currentOuter, innerRow)
				if err != nil {
					return recordlayer.RecordCursorResult[QueryResult]{}, err
				}
				cont := c.buildContinuation(result.GetContinuation())
				return recordlayer.NewResultWithValue(outputRow, cont), nil
			}
			// Inner exhausted for this outer row — close and advance outer.
			reason := result.GetNoNextReason()
			innerCont := result.GetContinuation()
			c.innerCursor.Close()
			c.innerCursor = nil

			if reason.IsOutOfBand() {
				// Inner hit a scan/time/byte limit — serialize
				// FlatMapContinuation with current outer + inner
				// position so the next page resumes correctly.
				cont := c.buildContinuation(innerCont)
				res := recordlayer.NewResultNoNext[QueryResult](reason, cont)
				c.lastNoNext = &res
				return res, nil
			}
		}

		// Advance the outer cursor.
		if c.outerExhausted {
			return recordlayer.NewResultNoNext[QueryResult](
				recordlayer.SourceExhausted, &recordlayer.EndContinuation{},
			), nil
		}

		outerResult, err := c.outerCursor.OnNext(ctx)
		if err != nil {
			return recordlayer.RecordCursorResult[QueryResult]{}, err
		}
		if !outerResult.HasNext() {
			reason := outerResult.GetNoNextReason()
			// Only a genuine source exhaustion is terminal; an out-of-band
			// stop is a page boundary — the replay guard (not this flag)
			// keeps a re-call from re-pulling the outer.
			c.outerExhausted = reason == recordlayer.SourceExhausted
			cont := c.wrapOuterContinuation(outerResult.GetContinuation())
			res := recordlayer.NewResultNoNext[QueryResult](reason, cont)
			c.lastNoNext = &res
			return res, nil
		}

		outerRow := outerResult.GetValue()
		c.currentOuter = &outerRow
		c.priorOuterContinuation = c.lastOuterContinuation
		c.lastOuterContinuation = outerResult.GetContinuation()

		// Bind the outer row as a correlation and execute the inner plan.
		// Use initialInnerCont for the first outer row on resume.
		//
		// The outer binds as its POSITIONAL row. For a gated ORDINAL-build
		// join, or an inner plan carrying baked SARG operands over the outer alias
		// (ofOrdinal over QOV(outer), pushed down as scan-range/index-probe
		// comparisons), the outer must present its LEG type so those operands resolve
		// by ordinal (adaptLegPositional; failure is LOUD). Otherwise the outer's own
		// scan row is bound raw, stamped with the outer alias as a leg window so an
		// alias-qualified inner ref ("D.DID") resolves the same as a bare one.
		var outerBinding any
		switch {
		case c.build.enabled():
			row, aerr := adaptLegPositional(outerRow, c.build.legType(c.outerAlias))
			if aerr != nil {
				return recordlayer.RecordCursorResult[QueryResult]{}, aerr
			}
			outerBinding = row
		case c.outerBakedType != nil:
			row, aerr := adaptLegPositional(outerRow, c.outerBakedType)
			if aerr != nil {
				return recordlayer.RecordCursorResult[QueryResult]{}, aerr
			}
			outerBinding = row
		default:
			outerBinding = qualifyOuterPositional(outerRow.Positional, c.outerAlias.Name())
		}
		correlatedCtx := c.evalCtx.WithBinding(c.outerAlias, outerBinding)
		// Java FlatMapPipelinedCursor (:210-220): the initial inner
		// continuation stays ARMED across outer rows until the row whose
		// check value matches (or either check value is absent) CONSUMES it
		// — Java nulls initialInnerContinuation ONLY in the match branch. A
		// mismatching row (data changed between transactions — e.g. a row
		// inserted before the saved one) runs its inner from the beginning
		// while the armed state waits for the saved row to re-appear;
		// discarding on the first mismatch would re-emit the saved row's
		// already-delivered inner prefix when it arrives later (duplicate
		// rows). The Cascades RecordQueryFlatMapPlan passes checker=null in
		// Java (no check at all); Go keeps the PK check as a resume-safety
		// net with Java's restart-not-error semantics.
		var innerContBytes []byte
		if c.initialInnerCont != nil {
			if len(c.pendingCheckValue) > 0 && outerRow.PrimaryKey != nil &&
				!bytes.Equal(outerRow.PrimaryKey.Pack(), c.pendingCheckValue) {
				// Not the saved outer row: fresh inner, stay armed.
			} else {
				innerContBytes = c.initialInnerCont
				c.initialInnerCont = nil
				c.hasPendingInner = false
				c.pendingCheckValue = nil
			}
		}
		innerCursor, err := ExecutePlan(ctx, c.innerPlan, c.store, correlatedCtx, innerContBytes, c.props)
		if err != nil {
			return recordlayer.RecordCursorResult[QueryResult]{}, err
		}
		c.innerCursor = innerCursor
	}
}

// computeResult evaluates the resultValue with both outer and inner
// bound as correlations. Mirrors Java's FlatMapPipelinedCursor:
//
//	nestedContext = fromOuterContext.withBinding(CORRELATION, innerAlias, innerResult)
//	computed = resultValue.eval(store, nestedContext)
//	return inheritOuter ? outerResult.withComputed(computed) : QueryResult.ofComputed(computed)
func (c *flatMapCursor) computeResult(outerRow, innerRow QueryResult) (QueryResult, error) {
	return c.computeResultLegs(outerRow, &innerRow)
}

// isIdentityOuterRV reports whether a FlatMap result value is exactly the outer
// quantifier's object (the WHERE-EXISTS pass-through): the output IS the outer
// row, flowed through unchanged (the identity branch in computeResultLegs). Such
// a cursor must NOT reroute the outer through the leg windows — it reads the
// whole outer object, not per-leg columns (the
// projected-fold leg-window path is only for a projection over the merged row).
// The ONE predicate both the construction-time foldWindowsOK exclusion and the
// identity branch read, so the exclusion can never drift from the branch it
// mirrors.
func isIdentityOuterRV(rv values.Value, outerAlias values.CorrelationIdentifier) bool {
	qov, ok := rv.(*values.QuantifiedObjectValue)
	return ok && qov.Correlation == outerAlias
}

// qualifyOuterPositional stamps the outer quantifier's leg window onto the
// outer scan row the WHERE-EXISTS identity pass-through flows, so a downstream
// reference qualified by the outer alias (a source-relative baked QOV(E).FNAME)
// binds its leg window through the row's own Legs metadata (rowLegsBinder /
// legWindowBinder) — a single leg window named after the alias covering the
// whole row; bare references read their baked slots directly. A plain scan row
// carries no Legs of its own (a clustered outer would be an outerMergedType
// shape, not this arm), so a fresh single-leg RecordType is stamped over the
// SAME slots (the Slots are shared — values are read, never mutated; only the
// type gains the alias window). Returns the row unchanged when the alias is
// empty or the type is absent.
func qualifyOuterPositional(row *PositionalRow, alias string) *PositionalRow {
	if row == nil || row.Type == nil || alias == "" {
		return row
	}
	// A MERGED outer row (a clustered join outer — e.g. a FULL OUTER JOIN A,B)
	// ALREADY carries its own per-leg windows (A, B): an alias-qualified read
	// resolves through THOSE (A.K → leg A). Stamping a single whole-row alias leg
	// here would CLOBBER them — a leg-window bind for "A.K" would then find no leg A and the
	// dup "K" is ambiguous → unresolvable. So preserve the sub-legs; only a PLAIN
	// scan row (no legs of its own) gets the whole-row alias window that lets a
	// downstream `E.FNAME` resolve.
	if len(row.Type.Legs) > 0 {
		return row
	}
	qualified := &values.RecordType{
		RecordName: row.Type.RecordName,
		Nullable:   row.Type.Nullable,
		Fields:     row.Type.Fields,
		Legs:       []values.RecordTypeLeg{{Name: alias, Start: 0, Width: len(row.Type.Fields)}},
	}
	return &PositionalRow{Type: qualified, Slots: row.Slots}
}

// computeResultLegs is computeResult with the inner leg as a pointer: nil is
// the LEFT-OUTER null-inner emission. For an ordinal-build cursor it builds
// the positional row from the RC with per-leg bindings — the nil
// inner pointer becomes the NULL leg (QOV(inner)→nil). The
// row IS its PositionalRow; the ordinal RC resolves its baked references by
// ordinal against it. A non-build cursor (identity pass-through / folded
// projection) keeps the Evaluate path, with a nil inner binding for the
// null-inner emission.
func (c *flatMapCursor) computeResultLegs(outerRow QueryResult, inner *QueryResult) (QueryResult, error) {
	if c.build.enabled() {
		pos, err := c.build.evaluate(c.outerAlias.Name(), c.innerAlias.Name(), &outerRow, inner, correlationBase(c.evalCtx))
		if err != nil {
			return QueryResult{}, err
		}
		// The ordinal-build FlatMap row IS its PositionalRow.
		return QueryResult{Positional: pos}, nil
	}
	innerRow := QueryResult{}
	if inner != nil {
		innerRow = *inner
	}
	// Build evaluation context with both correlations bound. The inner binding is
	// the inner's POSITIONAL row; a correlated array UNNEST (RFC-142) flows a BARE
	// SCALAR element (wrapped by scalarPositionalRow into a 1-slot `_0` row), so
	// bind the UNWRAPPED scalar under QOV(inner) — the AS alias reads the whole
	// element. A row-shaped inner binds its positional row unchanged.
	var innerBinding any
	if innerRow.Positional != nil {
		if isBareScalarRow(innerRow.Positional) {
			innerBinding = innerRow.Positional.Slots[0]
		} else {
			innerBinding = innerRow.Positional
		}
	}
	outerBinding := qualifyOuterPositional(outerRow.Positional, c.outerAlias.Name())
	nestedCtx := c.evalCtx.
		WithBinding(c.outerAlias, outerBinding).
		WithBinding(c.innerAlias, innerBinding)

	// Evaluate against a RowEvalContext whose row is the outer positional, so a
	// BARE outer FieldValue (e.g. a projected `ID` with no QOV qualifier — RFC-141
	// projected EXISTS folds the SELECT list into the result value) resolves by
	// ordinal against the outer row, while QOV references to the outer/inner aliases
	// resolve through the correlation bindings (Correlations).
	var rowCtx any = nestedCtx.RowContextPositional(outerRow.Positional)
	// A PROJECTED-EXISTS fold whose OUTER is a gated
	// ordinal join's positional merged row. Evaluate the folded projection through
	// legWindowRowContext — spanAwareRow resolves its dotted "T1.ID" frontier reads
	// positionally against the leg windows, legWindowBinder its QOV leg refs, and
	// the composed nestedCtx binds the existential inner (existCorr) for the
	// projected ExistsValue.
	if outerRow.Positional != nil && c.foldWindowsOK {
		rowCtx = legWindowRowContext(outerRow.Positional, nestedCtx, c.foldLegSpans)
	}
	var foldPos *PositionalRow
	if rc, isRC := c.resultValue.(*values.RecordConstructorValue); isRC {
		// Emit the authoritative ordinal OUTPUT row. The folded SELECT
		// list's RC.Fields ARE the output columns in output order, so a slot per
		// field — evaluated INDIVIDUALLY against rowCtx (never through a collapsing
		// name map, so a duplicate output name keeps both slots) — is the row's
		// ordinal output, what a projected-EXISTS SELECT list (`SELECT id,
		// EXISTS(...) AS has_t2 FROM t`) materializes from.
		posNames := make([]string, len(rc.Fields))
		posSlots := make([]any, len(rc.Fields))
		for i, f := range rc.Fields {
			posNames[i] = f.Name
			fv, ferr := f.Value.Evaluate(rowCtx)
			if ferr != nil {
				return QueryResult{}, ferr
			}
			posSlots[i] = fv
		}
		foldPos = &PositionalRow{Type: positionalTypeFromNames(posNames), Slots: posSlots}
	} else {
		// A non-RC, non-identity result value: evaluate it to a scalar output row.
		computed, err := c.resultValue.Evaluate(rowCtx)
		if err != nil {
			return QueryResult{}, err
		}
		if !isIdentityOuterRV(c.resultValue, c.outerAlias) {
			foldPos = scalarPositionalRow(computed)
		}
	}
	// Identity-over-outer FlatMap (the result value is exactly the outer
	// quantifier's object — the WHERE-EXISTS pass-through, RFC-141): the output
	// IS the outer row flowed under the outer quantifier, mirroring Java's
	// outer-record-under-outer-quantifier flow.
	if isIdentityOuterRV(c.resultValue, c.outerAlias) {
		out := QueryResult{Record: outerRow.Record, PrimaryKey: outerRow.PrimaryKey}
		// The outer's positional row flows through instead of dying at the
		// FlatMap boundary — downstream ordinal consumers keep resolving
		// against it (merged outers keep their leg windows via the
		// unwrapToJoinPlan identity arm). The published row is the ADAPTED
		// row — the same derivation the outer-binding arm uses: an
		// INDEX-shaped outer positional (a covering row [V, ID] under a baked
		// [ID, V] QOV) is re-synthesized into the logical layout, since
		// publishing the ORIGINAL row verbatim would hand downstream baked
		// ordinals the wrong layout — a silent wrong-slot read.
		// Layout-matching outers flow the same row object through; adaptation
		// failure is LOUD — never a silent wrong-slot read.
		//
		// The layout authority for the propagated outer positional row: the
		// inner-probed baked type (an existential whose inner carries baked
		// outer refs), else the outer's own gated-join merged type when the
		// exists inner is LEG-INDEPENDENT (probe negative, so outerBakedType
		// is nil, but the outer IS a genuine gated ordinal seed,
		// outerMergedType != nil).
		adaptType := c.outerBakedType
		if adaptType == nil {
			adaptType = c.outerMergedType
		}
		if adaptType != nil && outerRow.Positional != nil {
			adapted, aerr := adaptLegPositional(outerRow, adaptType)
			if aerr != nil {
				return QueryResult{}, aerr
			}
			if pos, isPos := adapted.(*PositionalRow); isPos {
				out.Positional = pos
			}
		} else if c.outerIdentityPassthrough && outerRow.Positional != nil {
			// The PLAIN-SCAN correlated-EXISTS output edge: the identity
			// output IS the outer scan row, so its OWN positional row flows
			// through (no baked/gated layout authority applies; the outer
			// binds RAW at the input edge for the same reason), stamped with
			// the outer alias as a leg window (qualifyOuterPositional). The
			// downstream projection reads its columns qualified by the outer
			// alias ("E.FNAME"), so the baked qualified reference binds its
			// leg window via the Legs metadata — a bare-named plain-scan row
			// alone would loud-miss the qualified read. The stamp is only
			// correct when the row IS the outer source's own layout, which is
			// why this arm is gated on outerIdentityPassthrough (both layout
			// probes nil AND identity RV).
			out.Positional = qualifyOuterPositional(outerRow.Positional, c.outerAlias.Name())
		} else {
			// The identity output IS the outer row — flow its own Positional
			// through.
			out.Positional = outerRow.Positional
		}
		return out, nil
	}
	return QueryResult{Positional: foldPos}, nil
}

// buildContinuation creates a FlatMapContinuation proto. The decision is purely
// on the inner cursor's state (matching Java FlatMapPipelinedCursor.toByteString,
// :413-430): if the inner has a resumable position (not END) — a value emit or an
// inner out-of-band stop mid-row — encode the prior outer position + inner
// position so resume continues THIS outer's inner. If the inner is exhausted
// (END), encode the advanced outer position with no inner (next outer on resume).
func (c *flatMapCursor) buildContinuation(innerCont recordlayer.RecordCursorContinuation) recordlayer.RecordCursorContinuation {
	if innerCont != nil && innerCont.IsEnd() && c.lastOuterContinuation != nil && c.lastOuterContinuation.IsEnd() {
		return &recordlayer.EndContinuation{}
	}

	cont := &flatMapCursorContinuation{}

	// Java FlatMapPipelinedCursor.Continuation.toByteString (:415-430) pairs the
	// position AT the current outer row with the inner continuation, and the
	// check value is written ONLY in the mid-inner branch (alongside the inner
	// continuation) — never in the advanced-outer branch. The decision is purely
	// whether the inner has a resumable position:
	//   - inner NOT exhausted (a value emit mid-inner, or an inner out-of-band
	//     stop): encode (priorOuter, checkValue, inner) so resume re-opens THIS
	//     outer, validates the check value, and continues its inner after the last
	//     row. Encoding the ADVANCED outer position here (as a prior Go-only
	//     innerTimeLimited flag did for the value-emit path) skips the rest of this
	//     outer's inner rows on resume — a silent row-drop on any mid-inner page
	//     boundary.
	//   - inner exhausted (END): advance to the next outer (lastOuter, no inner,
	//     NO check value). Java writes only setOuterContinuation here. Writing the
	//     check value in this branch mismatches DETERMINISTICALLY on resume: the
	//     resumed outer is the NEXT row, whose PK differs from the exhausted row's
	//     PK, so the stale check value never matches (this is the F24 trap the
	//     LEFT-OUTER null-inner emission hit — a null-extension page boundary
	//     wrote check_value into an advanced-outer continuation).
	if innerCont != nil && !innerCont.IsEnd() {
		if c.priorOuterContinuation != nil && !c.priorOuterContinuation.IsEnd() {
			cont.outerCont = c.priorOuterContinuation
		}
		if c.currentOuter != nil && c.currentOuter.PrimaryKey != nil {
			cont.checkValue = c.currentOuter.PrimaryKey.Pack()
		}
		cont.innerCont = innerCont
	} else {
		if c.lastOuterContinuation != nil && !c.lastOuterContinuation.IsEnd() {
			cont.outerCont = c.lastOuterContinuation
		}
	}
	return cont
}

// wrapOuterContinuation wraps the outer cursor's continuation in a
// FlatMapContinuation proto. Used when the outer cursor stops (e.g.,
// TimeLimitReached) before producing a value.
func (c *flatMapCursor) wrapOuterContinuation(outerCont recordlayer.RecordCursorContinuation) recordlayer.RecordCursorContinuation {
	if outerCont != nil && outerCont.IsEnd() {
		return &recordlayer.EndContinuation{}
	}
	cont := &flatMapCursorContinuation{outerCont: outerCont}
	if c.hasPendingInner {
		// Snapshot the pending inner bytes now: the cursor nils
		// initialInnerCont when it consumes the pending resume, and an
		// already-issued continuation must not change under it. The check
		// value rides along — without it a re-resume would apply the armed
		// inner to the FIRST outer row unverified (the wrong row, if the
		// saved one is no longer first).
		cont.pendingInner = c.initialInnerCont
		cont.checkValue = c.pendingCheckValue
	}
	return cont
}

// flatMapCursorContinuation lazily encodes the FlatMapContinuation proto at
// ToBytes() time, so a child continuation whose encode fails propagates that
// error through the RecordCursorContinuation contract (mirroring
// flatMapContinuationWrapper in pkg/recordlayer/cursor_combinators.go and the
// aggregate continuation's encode-error propagation in continuation.go). The
// eager predecessor swallowed child ToBytes errors (`, _ =`) — serializing a
// FlatMapContinuation with the failed component silently MISSING, which on
// resume restarts the current outer's inner from scratch (duplicate rows) or
// the whole outer scan — and returned the fake nonEndContinuation on a failed
// proto.Marshal. Exactly one of innerCont/pendingInner is set: innerCont is
// the mid-inner resume position (buildContinuation), pendingInner the
// pre-serialized not-yet-consumed inner from a resume whose outer stopped
// before re-reaching the resumed row (wrapOuterContinuation).
type flatMapCursorContinuation struct {
	outerCont    recordlayer.RecordCursorContinuation // outer position (prior or advanced); nil = omit
	innerCont    recordlayer.RecordCursorContinuation // mid-inner resume position; nil = omit
	checkValue   []byte                               // current outer PK (mid-inner branch only)
	pendingInner []byte                               // pending inner bytes (outer-stop path only)
}

func (w *flatMapCursorContinuation) ToBytes() ([]byte, error) {
	fmc := &gen.FlatMapContinuation{CheckValue: w.checkValue}
	if w.outerCont != nil {
		b, err := w.outerCont.ToBytes()
		if err != nil {
			return nil, fmt.Errorf("flatmap continuation outer: %w", err)
		}
		fmc.OuterContinuation = b
	}
	switch {
	case w.innerCont != nil:
		b, err := w.innerCont.ToBytes()
		if err != nil {
			return nil, fmt.Errorf("flatmap continuation inner: %w", err)
		}
		fmc.InnerContinuation = b
	case w.pendingInner != nil:
		fmc.InnerContinuation = w.pendingInner
	}
	return proto.Marshal(fmc)
}

func (w *flatMapCursorContinuation) IsEnd() bool { return false }

func (c *flatMapCursor) Close() error {
	c.closed = true
	if c.innerCursor != nil {
		c.innerCursor.Close()
	}
	return c.outerCursor.Close()
}

func (c *flatMapCursor) IsClosed() bool { return c.closed }

var _ recordlayer.RecordCursor[QueryResult] = (*flatMapCursor)(nil)
