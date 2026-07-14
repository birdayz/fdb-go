package executor

import (
	"bytes"
	"context"
	"fmt"
	"strings"

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
type flatMapCursor struct {
	outerCursor recordlayer.RecordCursor[QueryResult]
	innerPlan   plans.RecordQueryPlan
	store       *recordlayer.FDBRecordStore
	evalCtx     *EvaluationContext
	outerAlias  values.CorrelationIdentifier
	innerAlias  values.CorrelationIdentifier
	resultValue values.Value
	leftOuter   bool
	props       recordlayer.ExecuteProperties

	innerCursor    recordlayer.RecordCursor[QueryResult]
	currentOuter   *QueryResult
	innerHadMatch  bool
	outerExhausted bool
	closed         bool

	// birth is the RFC-173 Slice 2 ordinal-BIRTH state, probed ONCE at
	// construction from resultValue (nil = the lazy/name-model flatMap path).
	// When enabled, computeResult births the positional row from the RC with
	// per-leg bindings; the RC's baked references resolve by ordinal against
	// that row and can no longer evaluate over name contexts.
	birth *ordinalJoinBirth

	// outerBakedType is the RFC-173 item-2 commit-2 DISABLED-BIRTH probe
	// result: the outer's typed RecordType recovered from the inner plan's
	// FrontierPinned baked references over outerAlias, when this cursor has
	// NO birth of its own (an identity-RV existential FlatMap over an
	// ordinal outer — the E2 shape). Non-nil flips the outer binding
	// positional (adaptLegPositional); adaptation failure is LOUD
	// (design-ruling amendment B — never the Datum fallback, which would
	// feed the baked references a name-keyed context). S4 KILL LIST
	// (amendment A): dead scaffolding once the name model dies.
	outerBakedType *values.RecordType

	// foldLegSpans / foldWindowsOK (RFC-173 S4 commit 2, C): when this FlatMap's
	// OUTER is a gated ordinal join (a projected-EXISTS fold whose step-1 NLJ
	// births the leg-concat seed), the folded projection reads leg columns as
	// heterogeneous refs — dotted frontier reads ("T1.ID") and QOV refs. The
	// projection is NEVER rebased; instead it evaluates over the step-1 merged
	// positional row through legWindowRowContext(pos, ctx, foldLegSpans), the SAME
	// coexistence context every plain gated-join downstream projection uses
	// (spanAwareRow resolves the dotted reads, legWindowBinder the QOV refs).
	// Derived once from the outer plan's ordinal seed (downstreamLegWindows).
	foldLegSpans  []legSpan
	foldWindowsOK bool

	// outerMergedType (RFC-173 S4 commit 4): the outer gated ordinal join's
	// merged row RecordType, when this FlatMap's outer is one (nil otherwise).
	// The identity-FlatMap pass-through propagates a leg-independent existential's
	// gated outer positional row through it — adapting against this type (LOUD on
	// a layout mismatch) so a minted-dup upper resolves positionally instead of
	// the executor's probe-gate declining loud at translation.
	outerMergedType *values.RecordType

	// outerIdentityPassthrough (RFC-173 S4): the PLAIN-SCAN correlated-EXISTS
	// shape — a WHERE-EXISTS identity pass-through (RV == QOV(outer)) whose outer
	// is neither an inner-baked existential (outerBakedType nil) nor a gated join
	// (outerMergedType nil). The outer binds onto its OWN scan positional row
	// (bound RAW: the inner subquery's correlated ref is LAZY — no baked ordinal —
	// so it resolves by GetByName against the row's own type, layout-robust by
	// construction, NOT by a QOV-child ordinal that could mis-slot a covering
	// index), and the identity output edge propagates that same positional row so
	// the downstream projection reads by ordinal instead of by name. false = the
	// name-keyed Datum binding (a non-identity RV, or a baked/gated outer with its
	// own more-specific layout authority). S4 KILL LIST: dead once the name model
	// dies.
	outerIdentityPassthrough bool

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
	leftOuter bool,
	props recordlayer.ExecuteProperties,
) (*flatMapCursor, error) {
	birth, err := newOrdinalJoinBirth(resultValue, nil)
	if err != nil {
		return nil, err
	}
	// A TRANSLATED top RV (fused merge references) yields no spans from the RV
	// alone — recover them through the leg subplans' result values (the merge
	// RC is where the merged-away leg aliases survive), so the birth's leg
	// windows (Spans) get populated for the leg adapter even when the top RV
	// was folded past a plain probe.
	if birth.enabled() {
		legRVs := make(map[values.CorrelationIdentifier]values.Value)
		addJoinLegRV(legRVs, outerAlias, outerPlan)
		addJoinLegRV(legRVs, innerAlias, innerPlan)
		if !birth.WindowsOK {
			if spans, _, ok := ordinalJoinSpansOf(resultValue, legRVs); ok {
				birth.Spans = spans
				birth.WindowsOK = true
			}
		}
	}
	// The FlatMap half of the PR-447 review P1 (@claude final-pass catch): the
	// correlated implementation pushes the join's baked ON references INTO
	// the inner plan (SARGs, residual filters), so LegTypes must be widened
	// from the inner plan's predicate surfaces — a folded result value can
	// drop a leg those references still need (see widenLegTypesFromPlan).
	birth.widenLegTypesFromPlan(innerPlan)
	// Producer context (RFC-142 W4c): a WITH-ORDINALITY unnest's inner IS an
	// ordinality Explode, flowing a Datum keyed by the internal `_0`/`_1`
	// positions. Mark the inner leg so it binds STRICTLY POSITIONALLY (see
	// ordinalJoinBirth.OrdinalityLegs) — a user AS/AT alias spelling `_0`/`_1`
	// then cannot route the wrong internal key, and a name-model leg whose own
	// columns are aliased `_0`/`_1` (shape-identical, but NOT an ordinality
	// Explode) still binds correctly by name.
	if birth.enabled() && innerIsOrdinalityExplode(innerPlan) {
		if birth.OrdinalityLegs == nil {
			birth.OrdinalityLegs = map[values.CorrelationIdentifier]struct{}{}
		}
		birth.OrdinalityLegs[innerAlias] = struct{}{}
	}
	// RFC-173 item-2 commit 2: a DISABLED-birth FlatMap (identity RV — the
	// WHERE-EXISTS pass-through) probes its inner plan for baked references
	// over the outer alias; a hit means the outer must bind positionally
	// (see outerBakedType).
	var outerBakedType *values.RecordType
	var foldLegSpans []legSpan
	var foldWindowsOK bool
	var outerMergedType *values.RecordType
	var outerIdentityPassthrough bool
	if !birth.enabled() {
		outerBakedType = probeOuterBakedType(innerPlan, outerAlias)
		// RFC-173: recognise a gated ordinal join OUTER (downstreamLegWindows
		// accepts a genuine FrontierPinned full-coverage seed only — a name-model
		// anchored RC is rejected). Two consumers of that recognition:
		//   - commit 2 (C): a projected-EXISTS FOLD resolves its leg refs through
		//     legWindowRowContext(foldLegSpans). EXCLUDES the identity pass-through
		//     (RV == QOV(outer)) — that reads the whole-outer object, not per-leg
		//     columns — so foldWindowsOK gates on !isIdentityOuterRV, decided once.
		//   - commit 4: the identity pass-through propagates a leg-independent
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
		leftOuter:                leftOuter,
		props:                    props,
		birth:                    birth,
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
				c.innerHadMatch = true
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
				return recordlayer.NewResultNoNext[QueryResult](reason, cont), nil
			}

			// LEFT OUTER: emit outer row with NULLs when inner had no match.
			// The nil inner pointer is the RFC-173 NULL leg for an
			// ordinal-birth cursor; the name-model path reconstructs the
			// empty-Datum inner row (bit-identical to before).
			if c.leftOuter && !c.innerHadMatch {
				outputRow, err := c.computeResultLegs(*c.currentOuter, nil)
				if err != nil {
					return recordlayer.RecordCursorResult[QueryResult]{}, err
				}
				cont := c.buildContinuation(innerCont)
				return recordlayer.NewResultWithValue(outputRow, cont), nil
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
			c.outerExhausted = true
			reason := outerResult.GetNoNextReason()
			cont := c.wrapOuterContinuation(outerResult.GetContinuation())
			return recordlayer.NewResultNoNext[QueryResult](reason, cont), nil
		}

		outerRow := outerResult.GetValue()
		c.currentOuter = &outerRow
		c.innerHadMatch = false
		c.priorOuterContinuation = c.lastOuterContinuation
		c.lastOuterContinuation = outerResult.GetContinuation()

		if len(c.pendingCheckValue) > 0 && outerRow.PrimaryKey != nil {
			currentCheck := outerRow.PrimaryKey.Pack()
			if !bytes.Equal(currentCheck, c.pendingCheckValue) {
				return recordlayer.RecordCursorResult[QueryResult]{},
					fmt.Errorf("flatMap: outer row changed between transactions (check_value mismatch)")
			}
			c.pendingCheckValue = nil
		}

		// Bind the outer row as a correlation and execute the inner plan.
		// Use initialInnerCont for the first outer row on resume.
		//
		// RFC-173: the outer binds as its POSITIONAL row. For a gated ORDINAL-birth
		// join, or an inner plan carrying baked SARG operands over the outer alias
		// (ofOrdinal over QOV(outer), pushed down as scan-range/index-probe
		// comparisons), the outer must present its LEG type so those operands resolve
		// by ordinal (adaptLegPositional; failure is LOUD). Otherwise the outer's own
		// scan row is bound raw, stamped with the outer alias as a leg window so an
		// alias-qualified inner ref ("D.DID") resolves the same as a bare one.
		var outerBinding any
		switch {
		case c.birth.enabled():
			row, aerr := adaptLegPositional(outerRow, c.birth.legType(c.outerAlias))
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
		var innerContBytes []byte
		if c.initialInnerCont != nil {
			innerContBytes = c.initialInnerCont
			c.initialInnerCont = nil
			c.hasPendingInner = false
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
// whole outer object, not per-leg columns (RFC-173 S4 commit 2, C: the
// projected-fold leg-window path is only for a projection over the merged row).
// The ONE predicate both the construction-time foldWindowsOK exclusion and the
// identity branch read, so the exclusion can never drift from the branch it
// mirrors.
func isIdentityOuterRV(rv values.Value, outerAlias values.CorrelationIdentifier) bool {
	qov, ok := rv.(*values.QuantifiedObjectValue)
	return ok && qov.Correlation == outerAlias
}

// qualifyOuterPositional is the ORDINAL-model analog of qualifyOuterRow: the
// WHERE-EXISTS identity pass-through flows the outer scan row UNDER the outer
// quantifier, so a downstream reference qualified by the outer alias ("E.FNAME")
// must resolve against it. In the name model qualifyOuterRow writes "ALIAS.COL"
// Datum keys; here the equivalent is a single item-3 leg window named after the
// alias covering the whole row — PositionalRow.GetByName then resolves both a
// BARE column ("DNAME", via FieldIndex) and a DOTTED "ALIAS.COL" ("E.FNAME", via
// the Legs path) off the same slots. A plain scan row carries no Legs of its own
// (a clustered outer would be an outerMergedType shape, not this arm), so a fresh
// single-leg RecordType is stamped over the SAME slots (the Slots are shared —
// values are read, never mutated; only the type gains the alias window). Returns
// the row unchanged when the alias is empty or the type is absent.
func qualifyOuterPositional(row *PositionalRow, alias string) *PositionalRow {
	if row == nil || row.Type == nil || alias == "" {
		return row
	}
	// A MERGED outer row (a clustered join outer — e.g. a FULL OUTER JOIN A,B)
	// ALREADY carries its own per-leg windows (A, B): an alias-qualified read
	// resolves through THOSE (A.K → leg A). Stamping a single whole-row alias leg
	// here would CLOBBER them — GetByName("A.K") would then find no leg A and the
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
		Legs:       []values.RecordTypeLeg{{Name: strings.ToUpper(alias), Start: 0, Width: len(row.Type.Fields)}},
	}
	return &PositionalRow{Type: qualified, Slots: row.Slots}
}

// computeResultLegs is computeResult with the inner leg as a pointer: nil is
// the LEFT-OUTER null-inner emission. For an ordinal-birth cursor (RFC-173
// S2) it births the positional row from the RC with per-leg bindings — the nil
// inner pointer becomes the NULL leg (QOV(inner)→nil, contract ruling #3). The
// row IS its PositionalRow; the ordinal RC resolves its baked references by
// ordinal against it, never over a name-keyed context. The lazy/name-model
// cursor keeps today's Evaluate path, reconstructing the empty inner row for
// the null-inner emission.
func (c *flatMapCursor) computeResultLegs(outerRow QueryResult, inner *QueryResult) (QueryResult, error) {
	if c.birth.enabled() {
		pos, err := c.birth.evaluate(c.outerAlias.Name(), c.innerAlias.Name(), &outerRow, inner, correlationBase(c.evalCtx))
		if err != nil {
			return QueryResult{}, err
		}
		// RFC-173 cap: the ordinal-birth FlatMap row IS its PositionalRow; the
		// coexistence name-keyed Datum it used to carry alongside is deleted with
		// the name model.
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
	// RFC-173 S4 commit 2 (C): a PROJECTED-EXISTS fold whose OUTER is a gated
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
		// RFC-173: emit the authoritative ordinal OUTPUT row. The folded SELECT
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
	// IS the outer record flowed under the outer quantifier, so qualify its keys
	// under the outer alias. Downstream projections reference the outer columns
	// as `ALIAS.COL` (a FieldValue over QOV(outer)); a bare-keyed map would not
	// resolve them. Mirrors the prior semi-join cursor's qualifyOuterRow and
	// Java's outer-record-under-outer-quantifier flow.
	if isIdentityOuterRV(c.resultValue, c.outerAlias) {
		{
			out := QueryResult{Record: outerRow.Record, PrimaryKey: outerRow.PrimaryKey}
			// RFC-173 item-2 commit 2, the I1 pass-through: the identity
			// FlatMap's output IS the outer row, so the outer's positional row
			// flows through instead of dying at the FlatMap boundary —
			// downstream ordinal consumers keep resolving against it (merged
			// outers keep their leg windows via the unwrapToJoinPlan identity
			// arm). PROBE-GATED: outerBakedType is the ordinal-era
			// discriminator. A NAME-model existential (probe negative — lazy
			// inner refs) has name-shaped uppers reading qualifyOuterRow's
			// qualified Datum keys as flat dotted fields ("E.FNAME"); an
			// unconditional pass-through flipped those consumers onto the
			// ordinal path where the dotted name loud-misses the bare-named
			// row (the live TestFDB_CorrelatedExistsCrossJoin catch). An
			// ordinal-era shape whose probe is negative but whose uppers are
			// baked fails LOUD downstream (BakedNameContextError), never
			// silent — widen the gate when that shape materializes (commit 3).
			// The published row is the ADAPTED row — the same derivation the
			// outer-binding arm uses: an INDEX-shaped outer positional (a
			// covering row [V, ID] under a baked [ID, V] QOV) passes the
			// binding through synthesis, but publishing the ORIGINAL row
			// verbatim hands downstream baked ordinals the wrong layout — a
			// silent wrong-slot read. Layout-matching outers flow
			// the same row object through; adaptation failure is LOUD
			// (amendment B). Gated on the outer actually CARRYING a
			// positional row: propagation, not a birth — under the §5 oracle
			// the outer never carries one, so re-synthesizing from a
			// Datum-only outer here would be a new birth site violating the
			// oracle registry (the executeMap frontier-propagation
			// precedent). S4 KILL LIST (amendment A) with the disabled-birth
			// probe.
			// The layout authority for the propagated outer positional row: the
			// inner-probed baked type (an ordinal-era existential whose inner
			// carries baked outer refs), OR — RFC-173 S4 commit 4 — the outer's
			// own gated-join merged type when the exists inner is LEG-INDEPENDENT
			// (probe negative, so outerBakedType is nil, but the outer IS a genuine
			// gated ordinal seed, outerMergedType != nil). Adapting against the
			// type is a no-op on a seed-layout row but LOUD on a mismatch
			// (amendment B: never a silent wrong-slot read).
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
				// RFC-173 S4 — the PLAIN-SCAN correlated-EXISTS output edge: the
				// identity output IS the outer scan row, so its OWN positional row
				// flows through UNCHANGED (no adapter — no baked/gated layout
				// authority applies; the outer binds RAW at the input edge for the
				// same reason). The downstream projection (SELECT dname) then reads
				// by ordinal against this row instead of by name off the qualified
				// Datum. Narrowly gated on outerIdentityPassthrough (both probes nil
				// AND identity RV) so a name-model existential whose downstream
				// uppers read DOTTED "ALIAS.COL" fields (the outerBakedType-negative
				// join shape — TestFDB_CorrelatedExistsCrossJoin) is NOT flipped onto
				// the ordinal path where the dotted name would loud-miss the
				// bare-named row. Under the §5 oracle the outer never carries a
				// positional, so this is pure propagation, not a birth. The row is
				// stamped with the outer alias as a leg window (qualifyOuterPositional):
				// the downstream projection reads its columns qualified by the outer
				// alias ("E.FNAME"), so GetByName resolves the dotted reference via the
				// Legs path — the ordinal analog of qualifyOuterRow's "ALIAS.COL" Datum
				// keys, and the reason a bare-named plain-scan row alone would loud-miss
				// the qualified read (the TestFDB_CorrelatedExistsCrossJoin catch).
				out.Positional = qualifyOuterPositional(outerRow.Positional, c.outerAlias.Name())
			} else {
				// RFC-173 cap: the identity output IS the outer row — flow its own
				// Positional through (the name-keyed qualifyOuterRow Datum is deleted).
				out.Positional = outerRow.Positional
			}
			return out, nil
		}
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

	fmc := &gen.FlatMapContinuation{}

	if c.currentOuter != nil && c.currentOuter.PrimaryKey != nil {
		fmc.CheckValue = c.currentOuter.PrimaryKey.Pack()
	}

	// Java FlatMapPipelinedCursor.Continuation (FlatMapPipelinedCursor.java:373)
	// ALWAYS pairs priorOuterContinuation (the position AT the current outer row)
	// with the inner continuation — there is no "value emit vs limit emit"
	// distinction. The decision is purely whether the inner has a resumable
	// position:
	//   - inner NOT exhausted (a value emit mid-inner, or an inner out-of-band
	//     stop): encode (priorOuter, inner) so resume re-opens THIS outer and
	//     continues its inner after the last row. Encoding the ADVANCED outer
	//     position here (as a prior Go-only innerTimeLimited flag did for the
	//     value-emit path) skips the rest of this outer's inner rows on resume —
	//     a silent row-drop on any mid-inner page boundary.
	//   - inner exhausted (END): advance to the next outer (lastOuter, no inner).
	//     Equivalent to Java's (priorOuter, inner=END), which re-opens the outer
	//     and immediately advances.
	if innerCont != nil && !innerCont.IsEnd() {
		if c.priorOuterContinuation != nil && !c.priorOuterContinuation.IsEnd() {
			fmc.OuterContinuation, _ = c.priorOuterContinuation.ToBytes()
		}
		fmc.InnerContinuation, _ = innerCont.ToBytes()
	} else {
		if c.lastOuterContinuation != nil && !c.lastOuterContinuation.IsEnd() {
			fmc.OuterContinuation, _ = c.lastOuterContinuation.ToBytes()
		}
	}

	data, err := proto.Marshal(fmc)
	if err != nil {
		return nonEndContinuation
	}
	return recordlayer.NewBytesContinuation(data)
}

// wrapOuterContinuation wraps the outer cursor's continuation in a
// FlatMapContinuation proto. Used when the outer cursor stops (e.g.,
// TimeLimitReached) before producing a value.
func (c *flatMapCursor) wrapOuterContinuation(outerCont recordlayer.RecordCursorContinuation) recordlayer.RecordCursorContinuation {
	if outerCont != nil && outerCont.IsEnd() {
		return &recordlayer.EndContinuation{}
	}
	fmc := &gen.FlatMapContinuation{}
	if outerCont != nil {
		fmc.OuterContinuation, _ = outerCont.ToBytes()
	}
	if c.hasPendingInner {
		fmc.InnerContinuation = c.initialInnerCont
	}
	data, err := proto.Marshal(fmc)
	if err != nil {
		return nonEndContinuation
	}
	return recordlayer.NewBytesContinuation(data)
}

func (c *flatMapCursor) Close() error {
	c.closed = true
	if c.innerCursor != nil {
		c.innerCursor.Close()
	}
	return c.outerCursor.Close()
}

func (c *flatMapCursor) IsClosed() bool { return c.closed }

var _ recordlayer.RecordCursor[QueryResult] = (*flatMapCursor)(nil)
