package cascades

import (
	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// positionalMergeCase is the Java-aligned merge arm
// (PartitionSelectRule.java:283-322): a collapse of ≥2 live lowers builds the
// UNNAMED positional merged
// row (RC(_i: QOV(live_i)) — Column.unnamedOf per collapsed leg) and rebases
// every upper reference through a TranslationMap
// `.When(live_i).Then(ofOrdinalNumber(QOV(merge), i))` — enclosing baked
// references FUSE into two-step FieldPaths during the rebuild (Java's
// withNewChild mechanic, the fuse arm).
//
// The arm ALWAYS yields (invalid shapes die loudly at the tripwire
// or the constructors); the caller's nil-check is cheap defense for future
// decline arms, not a live contract.
func (r *PartitionSelectRule) positionalMergeCase(
	call *ExpressionRuleCall,
	sel *expressions.SelectExpression,
	resultValue values.Value,
	aliasToQ map[values.CorrelationIdentifier]expressions.Quantifier,
	allAliases []values.CorrelationIdentifier,
	upperAliases map[values.CorrelationIdentifier]struct{},
	live []values.CorrelationIdentifier,
	lowerBuilder *GraphExpansionBuilder,
	upperPredicates []predicates.QueryPredicate,
) *expressions.SelectExpression {
	// A null-on-empty quantifier — the dissolved-LEFT
	// machinery — SPLICES through a merged select as a quantifier but must
	// never be COLLAPSED into a positional lower: the null-extension is
	// per-outer-row (Java's SelectMergeRule matches via
	// forEachQuantifierWithoutDefaultOnEmptyOverRef — the quantifier merges,
	// its child never does). DECLINE the collapse and leave the select to
	// the per-quantifier NLJ implementation (DefaultOnEmpty — Java's
	// planPartitionToPhysical), never a silently mis-merged null extension.
	liveSet := make(map[values.CorrelationIdentifier]struct{}, len(live))
	for _, a := range live {
		liveSet[a] = struct{}{}
	}
	for _, q := range sel.GetQuantifiers() {
		if q.IsNullOnEmpty() {
			if _, collapsed := liveSet[q.GetAlias()]; collapsed {
				return nil
			}
		}
	}

	// The collapsed lower's result value: the nested positional merge row.
	// Field types flow the legs' whole row types (record-of-records — the
	// shape the executor evaluates). An untyped merge slot would strip the leg
	// types the executor's span recovery and downstream fused references resolve
	// through — and worse, silently: a reference the bake could not type stays
	// SOURCE-RELATIVE, and a source-relative operand pushed into a leg's scan as
	// a SARG evaluates to NULL against the build-bound row, so the scan matches
	// nothing and the join returns zero rows with no error.
	//
	// The QUANTIFIER is the authority for its own row type, exactly as in Java
	// (`quantifier.getFlowedObjectValue()` is `QuantifiedObjectValue.of(alias,
	// getFlowedObjectType())` — Quantifier.java:801-803, always typed).
	// legRowTypes remains as the fallback for a quantifier whose reference
	// carries no typed result value yet; it scavenges the select's own value
	// surfaces, where every baked reference is a copy of the one
	// planner-constructed typed leg QOV. It cannot be the primary source: when
	// the select's result value is itself an untyped flowed row (the
	// single-live-lower arm's `getFlowedObjectValue()`) there is no baked
	// reference anywhere to scavenge, which is precisely the shape that lost the
	// types.
	legTypes := legRowTypes(resultValue, sel.GetPredicates())
	fields := make([]values.RecordConstructorField, len(live))
	mergedFields := make([]values.Field, len(live))
	for i, a := range live {
		fov, err := aliasToQ[a].GetFlowedObjectValueTyped()
		if err != nil {
			// The quantifier's own reference has members flowing DIFFERENT row shapes,
			// so no member is authoritative and there is no honest merge slot type to
			// pick. Decline the rule rather than choose by insertion order — Java
			// Verify-fails at the same point (Reference.java:504-513).
			return nil
		}
		if _, typed := fov.Type().(*values.RecordType); !typed {
			if rt := legTypes[a]; rt != nil {
				fov = values.NewQuantifiedObjectValueOfType(a, rt)
			}
		}
		fields[i] = values.RecordConstructorField{Name: values.OrdinalFieldName(i), Value: fov}
		mergedFields[i] = values.Field{Name: values.OrdinalFieldName(i), FieldType: fov.Type(), Ordinal: i}
	}
	lowerRC := values.NewRawRecordConstructorValue(fields...)
	lowerSelectExpr := lowerBuilder.Build().Seal().BuildSelectWithResultValue(lowerRC)

	// Per-plan deterministic merge alias (NextMergeAlias, RFC-077 7.5:
	// stable plan hash; alias-aware
	// interning collapses same-shape merges across bipartitions).
	var mergeAlias values.CorrelationIdentifier
	if call.memo != nil {
		mergeAlias = call.memo.NextMergeAlias()
	} else {
		mergeAlias = values.UniqueCorrelationIdentifier()
	}
	newLowerQ := expressions.NamedForEachQuantifier(
		mergeAlias,
		call.MemoizeExpression(lowerSelectExpr),
	)

	// The rebase map: every reference to a collapsed leg becomes a PLAIN
	// ofOrdinalNumber over the merge quantifier (PartitionSelectRule.java:302
	// — composition with enclosing references is the rebuild's job).
	mergedType := &values.RecordType{Fields: mergedFields}
	upperQOV := values.NewQuantifiedObjectValueOfType(mergeAlias, mergedType)
	b := values.NewTranslationMapBuilder()
	for i, a := range live {
		idx := i
		b = b.When(a).Then(func(_ values.CorrelationIdentifier, _ values.Value) values.Value {
			fv, err := values.NewFieldValueOfOrdinal(upperQOV, idx)
			if err != nil {
				// Impossible by construction (idx ranges over mergedType's
				// own fields) — loud, matching the seed-bake discipline.
				panic("positional merge: " + err.Error())
			}
			return fv
		})
	}
	m := b.Build()

	upperBuilder := NewGraphExpansionBuilder()
	upperBuilder.AddQuantifier(newLowerQ)
	for _, a := range allAliases {
		if _, inUpper := upperAliases[a]; inUpper {
			upperBuilder.AddQuantifier(aliasToQ[a])
		}
	}
	for _, p := range upperPredicates {
		upperBuilder.AddPredicate(predicates.TranslateLeafPredicates(p, m))
	}
	return upperBuilder.Build().Seal().
		BuildSelectWithResultValue(values.TranslateCorrelations(resultValue, m))
}

// legRowTypes recovers each quantifier's flowed ROW type from a select's value
// surfaces (result value + predicates): every QOV embedded in a baked/fused
// reference is a copy of the ONE planner-constructed typed leg QOV
// (seed-baked, or a previous merge round's typed merge QOV), so first-found is
// authoritative. Needed because Go's Quantifier.GetFlowedObjectValue flows an
// UNTYPED QOV — a leg referenced nowhere stays absent (and the merge slot then
// flows untyped, exactly today's shape).
func legRowTypes(resultValue values.Value, preds []predicates.QueryPredicate) map[values.CorrelationIdentifier]*values.RecordType {
	types := make(map[values.CorrelationIdentifier]*values.RecordType)
	collect := func(v values.Value) bool {
		if qov, isQOV := v.(*values.QuantifiedObjectValue); isQOV {
			if rt, isRT := qov.Type().(*values.RecordType); isRT {
				if _, seen := types[qov.Correlation]; !seen {
					types[qov.Correlation] = rt
				}
			}
		}
		return true
	}
	values.WalkValue(resultValue, collect)
	for _, p := range preds {
		predicates.ReplaceValues(p, func(v values.Value) values.Value {
			collect(v)
			return v
		})
	}
	return types
}
