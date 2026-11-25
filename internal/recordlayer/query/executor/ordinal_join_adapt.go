// Portions derived from FoundationDB Record Layer (RecordQueryFlatMapPlan.java,
// ImplementNestedLoopJoinRule.java, FieldValue.java, PartitionSelectRule.java,
// and others),
// Copyright 2015-2019 Apple Inc. and the FoundationDB project authors
// Copyright 2015-2021 Apple Inc. and the FoundationDB project authors
// Copyright 2015-2022 Apple Inc. and the FoundationDB project authors
// Licensed under the Apache License, Version 2.0; translated to Go and modified.

package executor

import (
	"fmt"
	"strings"

	"fdb.dev/internal/unicode15"
	"fdb.dev/pkg/recordlayer"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// adaptLegPositional is the sanctioned row-FORMAT adapter at the
// join-input boundary: it bridges a leg row whose positional LAYOUT differs
// from the leg type the join's ordinals were baked against (an INDEX-shaped
// covering row vs. the logical table-shaped leg type — see the permutation
// note in the body). A leg whose layout already matches flows through
// untouched; otherwise the row's slots are gathered into legType order via
// the row's own plan-produced RecordType (a missing column is a nil slot =
// SQL NULL); a row with no positional yields an all-nil row of the leg's
// width.
//
// LOUD when the gather matches ZERO of the leg's columns against a NON-EMPTY
// MERGE-shaped row: such a row carries dotted-qualified names ("A.ID") the
// bare leg-type names never match, so the silent path would all-NULL the leg
// — indistinguishable from a legitimate all-NULL row. The translation-time
// cluster-arity gate makes such legs unreachable for gated joins, so this
// error is a belt-and-braces tripwire on that invariant, not a supported path.
// A row whose columns are PRESENT with nil values (a genuine all-NULL row)
// matches normally and stays silent.
//
// This is format-only bridging — correlation-SEMANTIC bridging (an ordinal
// join consumed as a leg of a wider merge select) is explicitly out of
// scope and prevented upstream by the cluster-arity gate.
func adaptLegPositional(qr QueryResult, legType *values.RecordType, owner values.CorrelationIdentifier) (values.OrdinalRow, error) {
	if qr.Positional != nil {
		_, absent, err := qr.Positional.wholeObjectBinding()
		if err != nil || absent {
			return nil, err
		}
		// The passthrough requires ORDERED per-slot name agreement with the
		// leg type — a width-only check is not enough: two layouts of the
		// same columns are the same width, and a baked leg ordinal over the
		// wrong layout silently reads the wrong slot. A non-aligned
		// positional row is a LEGITIMATE plan shape (not a gate breach):
		// fall through to the per-layout gather below (rowSlotForLegColumn
		// binds each leg column against the row's own plan-produced type),
		// with the zero-match tripwire as the final guard.
		if legType == nil || positionalMatchesLegType(qr.Positional, legType) {
			return qr.Positional, nil
		}
	}
	row := NewPositionalRow(legType)
	if legType == nil {
		return row, nil
	}
	// Layout PERMUTATION at the leg boundary: gather the row's slots into
	// legType order via the row's own plan-produced RecordType
	// (rowSlotForLegColumn, which resolves only an UNAMBIGUOUS name and hands
	// back the ambiguity otherwise). A documented
	// residual of Go's two-layout seed: gated-join legs are seeded with the
	// LOGICAL (table-shaped) leg type while a physical leg may emit a row
	// typed by its own plan output (covering-index rows are LOGICAL-shaped
	// today — buildCoveringLogicalRow — but the leg contract does not pin
	// every producer's layout), so the two layouts can be permutations of
	// each other. Java has no such adapter —
	// its planner rebinds every FieldValue ordinal against the physical
	// quantifier's actual flowed type (translateCorrelations), so the baked
	// ordinal IS the physical slot; retiring this gather requires Go's seed to
	// bake against the chosen physical leg layout the same way. Until then the
	// bind is per-layout TYPE metadata, never a per-value name probe, and a
	// zero-match merge-shaped row is loud below.
	if qr.Positional != nil {
		matched := 0
		if qr.Positional.Type != nil {
			for i, f := range legType.Fields {
				idx, bind := rowSlotForLegColumn(qr.Positional.Type, f.Name, owner)
				switch bind {
				case legColumnBound:
					v, _ := qr.Positional.Get(idx)
					row.Slots[i] = v
					matched++
				case legColumnAmbiguous:
					// LOUD. The source row carries this leg column at more than
					// one slot and nothing here can say which one the leg type
					// meant. Leaving the slot nil would answer NULL for a
					// column that has a value; taking either duplicate would
					// read the other leg's column. Both return plausible rows
					// that no downstream check rejects, which is why this is a
					// refusal and not a degradation.
					return nil, fmt.Errorf("leg adapter: leg column %q is declared %d times by the source row %v — "+
						"a merged row puts every leg's columns in one flat namespace, so this name addresses two "+
						"different columns and the leg type does not say which; the producer must qualify the leg "+
						"type's column names (or carry a baked ordinal) rather than have this reader guess",
						f.Name, qr.Positional.Type.FieldNameHits(f.Name), typeFieldNames(qr.Positional.Type))
				case legColumnUnbound:
					// The source row does not carry this leg column at all; the
					// slot stays nil and the zero-match tripwire below is the
					// guard for the case where NONE of them bound.
				}
			}
		}
		if matched == 0 && len(qr.Positional.Slots) > 0 && len(legType.Fields) > 0 {
			switch {
			case rowIsMergeShaped(qr.Positional):
				// LOUD for a genuinely MERGE-SHAPED row (leg windows or dotted-
				// qualified field names) that matched nothing:
				// a merge leg wrongly consumed by a gated join.
				return nil, fmt.Errorf("leg adapter: merge-shaped leg row carries NONE of the leg type's %d columns %v (row width: %d) — a gated join must not consume a merge-shaped leg (cluster-arity gate breach or leg-type mismatch)", len(legType.Fields), typeFieldNames(legType), len(qr.Positional.Slots))
			case len(qr.Positional.Slots) == len(legType.Fields):
				// A SIMPLE row whose column names are DISJOINT from the leg type
				// but SAME WIDTH — e.g. a materialized computed-scalar leg named by
				// its expression `(AMOUNT + 0)` consumed as the anonymous `_0`
				// positional leg. ofOrdinal reads by SLOT, so position is the
				// authority: map slot-for-slot.
				copy(row.Slots, qr.Positional.Slots)
			default:
				// A simple row of a different width that matches nothing (e.g. a
				// bare-scalar `_0` UNNEST element bound as a redundant gather leg)
				// degrades to an all-nil leg silently.
			}
		}
	}
	return row, nil
}

// legColumnBind is what one leg-type column name did against a source row's
// layout. THREE outcomes, not two, because the two ways of failing to produce
// an ordinal call for opposite handling by the caller.
type legColumnBind int

const (
	// legColumnBound: exactly one slot answers, and the returned ordinal is it.
	legColumnBound legColumnBind = iota
	// legColumnUnbound: no slot answers. Benign — the source row simply does
	// not carry this leg column, and the gather leaves it nil.
	legColumnUnbound
	// legColumnAmbiguous: the row declares the name MORE THAN ONCE and nothing
	// at this reader can choose between them. Never benign: the value is there,
	// twice, so leaving the slot nil is a WRONG value rather than a missing one
	// — and picking either duplicate is a wrong-leg read of the kind
	// leg_column_dotted_containment_test.go pins (`ID` in both the O and I legs
	// of one merged row addresses two different columns). The caller must
	// refuse, not degrade.
	legColumnAmbiguous
)

// rowSlotForLegColumn binds one leg-type column name to a slot of the source
// row's plan-produced RecordType: the flat lookup first, then — for a DOTTED
// name over a row whose type carries leg boundaries (RecordType.Legs, the
// merged concat / clustered box layout) — the per-leg window: qualifier → the
// leg's window, column → the column WITHIN it. The dotted arm serves the
// correlated-scalar seed legs, whose seed leg types name columns literally
// `LEG.COL` while the physical leg emits the merged row those names address.
//
// THE FLAT LOOKUP DECLINES ON A DUPLICATE NAME, and that decline is reported as
// legColumnAmbiguous rather than folded into the miss. A merged row puts every
// leg's columns in one flat namespace, so two legs that both declare `ID` put
// `ID` at two slots; answering either would be a wrong-leg read that returns a
// plausible value, and answering "not present" would hand the caller a nil for
// a column the row does carry. Both are silent wrong rows, so the ambiguity
// travels to the caller intact and fails there.
//
// A DOTTED name whose flat lookup was ambiguous still tries the per-leg arm:
// the qualifier is strictly more information than the flat namespace has, so if
// a leg window resolves it, that answer is not a guess. Only when nothing
// resolves does the ambiguity stand.
//
// owner is the IDENTITY of the leg whose type supplied `name` — the correlation
// adaptLegPositional is adapting a row FOR. It does not steer the lookup; it is
// recorded so the census can answer the question the conversion turns on: would
// selecting the source window by identity have chosen the leg the text chose?
func rowSlotForLegColumn(rt *values.RecordType, name string, owner values.CorrelationIdentifier) (int, legColumnBind) {
	census := values.Census()
	flatHits := rt.FieldNameHits(name)
	if flatHits == 1 {
		i, _ := rt.FieldIndexUnique(name)
		if census != nil {
			census.LegColumnProvenance(rt, name, true, false, nil, "", owner)
		}
		return i, legColumnBound
	}
	flatAmbiguous := flatHits > 1
	unresolved := legColumnUnbound
	if flatAmbiguous {
		unresolved = legColumnAmbiguous
	}
	di := strings.IndexByte(name, '.')
	if di <= 0 || len(rt.Legs) == 0 {
		if census != nil {
			census.LegColumnProvenance(rt, name, false, flatAmbiguous, nil, "", owner)
		}
		return 0, unresolved
	}
	qual, col := name[:di], name[di+1:]
	for i := range rt.Legs {
		leg := rt.Legs[i]
		if !unicode15.EqualFold(leg.Name, qual) {
			continue
		}
		end := leg.Start + leg.Width
		if leg.Start < 0 || end > len(rt.Fields) {
			break
		}
		for k := leg.Start; k < end; k++ {
			if unicode15.EqualFold(rt.Fields[k].Name, col) {
				// The census is told WHICH leg answered, so it can ask whether
				// that leg also states an identity — the fact that decides
				// whether this reader can be re-keyed off the name.
				if census != nil {
					census.LegColumnProvenance(rt, name, false, flatAmbiguous, &rt.Legs[i], qual, owner)
				}
				return k, legColumnBound
			}
		}
		break
	}
	if census != nil {
		census.LegColumnProvenance(rt, name, false, flatAmbiguous, nil, qual, owner)
	}
	return 0, unresolved
}

// rowIsMergeShaped reports whether a positional row is a JOIN-MERGE output — it
// carries leg windows (Type.Legs) or dotted-qualified field names. Such a row's
// bare columns are last-leg-wins leftovers, so a gated join adapting it to a
// bare leg type that matches nothing is a wrong-source hazard (loud). A
// simple row (a scan/element leg) that matches nothing is a benign redundant
// binding that degrades to an all-nil leg.
func rowIsMergeShaped(pos *PositionalRow) bool {
	if pos == nil || pos.Type == nil {
		return false
	}
	if len(pos.Type.Legs) > 0 {
		return true
	}
	for _, f := range pos.Type.Fields {
		if isDottedQualifiedName(f.Name) {
			return true
		}
	}
	return false
}

// isDottedQualifiedName reports whether a column name is a DOTTED QUALIFIER
// (`alias.column`) — the merge-row signal — as opposed to a name that merely
// CONTAINS a dot.
//
// The distinction is load-bearing because a composite column's name is its
// RENDERED TYPE, and a rendered record type contains the dots of every
// qualified column inside it: a one-field record over `C.ID` renders as
// `{_0: C.ID#0}`. Testing for a bare dot classified such a row as merge-shaped,
// which sent a perfectly ordinary one-column leg into the leg adapter's
// zero-match tripwire ("a gated join must not consume a merge-shaped leg")
// instead of the slot-for-slot arm that is correct for it. A rendered composite
// is delimited — a qualifier never is — so the delimiter, not the dot, is what
// separates the two.
func isDottedQualifiedName(name string) bool {
	if strings.HasPrefix(name, "{") || strings.HasPrefix(name, "[") {
		return false
	}
	return strings.IndexByte(name, '.') >= 0
}

// positionalMatchesLegType reports whether a leg's pre-existing positional
// row is LAYOUT-IDENTICAL to the seed's leg type: same width AND the same
// column name at every ordinal (case-insensitive) — the condition under which
// baked leg ordinals read the right slots. Order-sensitive by design: a
// covering-index row [V, ID] over a table typed [ID, V] must NOT pass.
func positionalMatchesLegType(row *PositionalRow, legType *values.RecordType) bool {
	if row.Type == nil || len(row.Type.Fields) != len(legType.Fields) || len(row.Slots) != len(legType.Fields) {
		return false
	}
	for i := range legType.Fields {
		if !unicode15.EqualFold(row.Type.Fields[i].Name, legType.Fields[i].Name) {
			return false
		}
	}
	return true
}

// typeFieldNames lists a RecordType's field names for diagnostics.
func typeFieldNames(rt *values.RecordType) []string {
	names := make([]string, len(rt.Fields))
	for i, f := range rt.Fields {
		names[i] = f.Name
	}
	return names
}

// evaluateOrdinalJoinRow builds the join's merged positional row: each field
// of the ordinal join RC (a BAKED leg reference) is evaluated with the legs
// bound through bindings, writing merged slot i. A NULL LEG is expressed by
// the binder returning (nil, true) for that leg's correlation — the baked
// node's evaluateCorrelated `return bound, nil` arm yields nil, so the leg's
// slots come out NULL (appendNullLeg ≡ evaluating the
// merged RC with the leg QOV bound to nil; the null extension falls out, no
// ad-hoc per-row types).
func evaluateOrdinalJoinRow(rc *values.RecordConstructorValue, mergedType *values.RecordType, bindings values.CorrelationBinder, clock values.StatementClock) (*PositionalRow, error) {
	if len(rc.Fields) != len(mergedType.Fields) {
		// The merged type is derived from this RC by ordinalJoinSpans — a
		// mismatch means the plan is malformed (a planner bug), which must
		// fail the query, not the process.
		return nil, fmt.Errorf("ordinal join row: RC has %d fields but merged type has %d — the merged type must derive from this RC (ordinalJoinSpans); malformed plan", len(rc.Fields), len(mergedType.Fields))
	}
	row := NewPositionalRow(mergedType)
	// A FOLDED result value (the B1 existential wrap) can carry an uncorrelated
	// ScalarSubqueryValue — a per-query constant the executor pre-evaluates and
	// binds by alias. Thread that map onto the build eval context so the scalar
	// resolves here (else ScalarSubqueryValue.Evaluate is loud: UnboundScalarSubquery).
	// The pristine ordinal-join SEED (baked leg refs only) carries none, so this is
	// nil for the NLJ path — harmless.
	evalCtx := joinBuildRowContext(bindings, clock)
	for i, f := range rc.Fields {
		// Join-leg QOVs are whole record objects. The cursor adapters guarantee
		// an OrdinalRow (or present nil for an unmatched leg); reject a hostile
		// direct binder before the exact FieldValue's Java-compatible
		// non-record=>NULL behavior could turn a malformed executor boundary into
		// a plausible row of NULLs.
		if field, isField := values.AsFieldValue(f.Value); isField {
			if qov, isQOV := values.AsQuantifiedObjectValue(field.ChildValue()); isQOV && bindings != nil && qov.FlowedType().Code() == values.TypeCodeRecord {
				if bound, present := bindings.GetCorrelationBinding(qov.Correlation()); present && bound != nil {
					if _, isOrdinal := bound.(values.OrdinalRow); !isOrdinal {
						ordinal := -1
						if path := field.Path(); path != nil {
							ordinals := path.Ordinals()
							if len(ordinals) > 0 {
								ordinal = ordinals[0]
							}
						}
						return nil, &values.BakedNameContextError{Field: field.DisplayName(), Ordinal: ordinal}
					}
				}
			}
		}
		v, err := f.Value.Evaluate(evalCtx)
		if err != nil {
			return nil, recordlayer.TranslateTextCmpError(err)
		}
		row.Slots[i] = v
	}
	return row, nil
}

// evaluateOrdinalJoinBareRow builds the emitted row for a BARE (non-RC) baked
// result value — see ordinalJoinBuild.Bare. The value is evaluated ONCE against
// the leg bindings and the OUTPUT SHAPE follows from what it produced, not from
// a plan flag:
//
//   - a ROW (the leg row the reference names — every leg adapter produces a
//     *PositionalRow) flows through AS ITSELF, keeping its own type and any leg
//     windows that type carries. Re-wrapping it into a 1-slot row would
//     double-nest it: a downstream ordinal read of column i would land on the
//     whole record instead of the column, which is precisely how the non-build
//     path returns zero rows for this shape;
//   - anything else is a scalar column and wraps into the 1-slot `_0` row every
//     other scalar-output path uses (scalarPositionalRow);
//   - NULL flows as a nil row (the deliberately-NULL leg's extension).
//
// A wrongly-shaped binding still fails LOUD inside Evaluate: a baked ordinal
// read against a row that cannot serve it is an OrdinalResolutionError, so
// dropping the RC-only construction check trades no silence for reach.
func evaluateOrdinalJoinBareRow(v values.Value, bindings values.CorrelationBinder, clock values.StatementClock) (*PositionalRow, error) {
	evalCtx := joinBuildRowContext(bindings, clock)
	out, err := v.Evaluate(evalCtx)
	if err != nil {
		return nil, recordlayer.TranslateTextCmpError(err)
	}
	switch row := out.(type) {
	case nil:
		return nil, nil
	case *PositionalRow:
		return row, nil
	}
	return scalarPositionalRow(out), nil
}
