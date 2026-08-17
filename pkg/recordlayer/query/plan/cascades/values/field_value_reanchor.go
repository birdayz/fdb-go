package values

import "errors"

// ReanchorFieldValue maps one exact source-relative field access onto the
// exact current carrier owned by layout. The mapping is entirely ordinal: no
// display name or inferred start offset participates.
func ReanchorFieldValue(
	field FieldValue,
	target QuantifiedObjectValue,
	layout OrdinalLayout,
) (FieldValue, error) {
	original, ok := field.(*fieldValue)
	if !ok || !isAdmittedFieldValue(original) {
		return nil, resolutionError(ReanchorInvalidValue, "reanchor.field", "field is not a values-owned exact FieldValue")
	}
	exactTarget, err := exactLayoutQOV(target, "reanchor.target")
	if err != nil {
		return nil, resolutionError(ReanchorTargetMismatch, "reanchor.target", "target is not a values-owned exact QOV")
	}
	exactLayout, ok := exactOrdinalLayout(layout)
	if !ok {
		return nil, resolutionError(LayoutForeignValue, "reanchor.layout", "layout is not a values-owned exact layout")
	}
	if exactTarget != exactLayout.carrier {
		return nil, resolutionError(ReanchorTargetMismatch, "reanchor.target", "target is not the layout's exact carrier handle")
	}

	root := original.Child.(*quantifiedObjectValue)
	if root == exactTarget {
		return original, nil
	}

	sourcePath := original.Resolved.Ordinals()
	var mapped []int
	if root.correlation.isCurrent() {
		// A current-rooted field from another independently built owner phase is
		// structurally re-resolved onto this layout's exact handle. It is never a
		// pointer-preserving no-op.
		if !exactTypesEqual(root.flowed, exactTarget.flowed) {
			return nil, resolutionError(ReanchorTargetMismatch, "reanchor.field",
				"current root and target have different exact types: root "+
					describeExactType(root.flowed)+", target "+describeExactType(exactTarget.flowed))
		}
		mapped = append([]int(nil), sourcePath...)
	} else {
		windowIndex, exists := exactLayout.bySource[root.correlation]
		if !exists {
			// A machinery-owned path is already expressed in the complete
			// frontier's ordinal domain. Its QOV alias may be the logical owner
			// that existed before a physical/materializing edge was selected, but
			// the pinned path and exact whole-row type prove that its ordinals are
			// the target carrier's ordinals. Source-relative (unpinned) paths never
			// take this bridge; they require an explicit layout window.
			if original.Resolved.FrontierPinned && exactTypesEqual(root.flowed, exactTarget.flowed) {
				mapped = append([]int(nil), sourcePath...)
			} else {
				return nil, resolutionError(ReanchorUnmappedSource, "reanchor.field",
					"layout does not provide the field's exact source "+root.correlation.Name()+
						" ("+describeExactType(root.flowed)+"; pinned="+boolText(original.Resolved.FrontierPinned)+")")
			}
		} else {
			window := &exactLayout.windows[windowIndex]
			if !exactTypesEqual(window.source.flowed, root.flowed) {
				return nil, resolutionError(ReanchorUnmappedSource, "reanchor.field",
					"layout source "+root.correlation.Name()+" has a different exact type: window "+
						describeExactType(window.source.flowed)+", read "+describeExactType(root.flowed))
			}
			if window.objectPath != nil {
				mapped = make([]int, 0, len(window.objectPath)+len(sourcePath))
				mapped = append(mapped, window.objectPath...)
				mapped = append(mapped, sourcePath...)
			} else {
				if len(sourcePath) == 0 || sourcePath[0] < 0 || sourcePath[0] >= len(window.fieldPaths) {
					return nil, resolutionError(ReanchorInvalidMappedPath, "reanchor.field", "source path cannot select a field-window path")
				}
				prefix := window.fieldPaths[sourcePath[0]]
				mapped = make([]int, 0, len(prefix)+len(sourcePath)-1)
				mapped = append(mapped, prefix...)
				mapped = append(mapped, sourcePath[1:]...)
			}
		}
	}

	rebuilt, err := ResolveFieldOrdinals(exactTarget, mapped)
	if err != nil {
		return nil, resolutionError(ReanchorInvalidMappedPath, "reanchor.field", err.Error())
	}
	reanchored, ok := rebuilt.(*fieldValue)
	if !ok || !isAdmittedFieldValue(reanchored) {
		return nil, resolutionError(ReanchorInvalidMappedPath, "reanchor.field", "mapped path did not produce an exact FieldValue")
	}
	if !exactTypesEqual(original.resultType, reanchored.resultType) {
		return nil, resolutionError(ReanchorResultTypeMismatch, "reanchor.field", "mapped path changes the exact result type or nullability")
	}
	return reanchored, nil
}

// ReanchorValueForLayout rewrites every source-relative FieldValue that the
// layout can address onto the layout's exact current carrier. It also moves a
// bare reserved-current QOV onto that carrier. The rewrite is copy-on-write and
// atomic: an invalid mapped path or exact-type disagreement returns no partial
// Value.
//
// A non-current source absent from layout is preserved. Such a root may be a
// declared physical edge or an outer correlation supplied by the evaluation
// context; the owning plan validates those bindings. A source that layout does
// provide is never left relative, which is load-bearing at materialization
// boundaries where source windows are intentionally discarded after the row is
// built.
func ReanchorValueForLayout(
	value Value,
	target QuantifiedObjectValue,
	layout OrdinalLayout,
) (Value, error) {
	if value == nil {
		return nil, resolutionError(ReanchorInvalidValue, "reanchor.value", "value is nil")
	}
	exactTarget, err := exactLayoutQOV(target, "reanchor.target")
	if err != nil {
		return nil, err
	}
	exactLayout, ok := exactOrdinalLayout(layout)
	if !ok {
		return nil, resolutionError(LayoutForeignValue, "reanchor.layout", "layout is not a values-owned exact layout")
	}
	if exactTarget != exactLayout.carrier {
		return nil, resolutionError(ReanchorTargetMismatch, "reanchor.target", "target is not the layout's exact carrier handle")
	}

	var visit func(Value) (Value, error)
	visit = func(node Value) (Value, error) {
		if node == nil {
			return nil, resolutionError(RewriteNilReplacement, "reanchor.value", "value tree contains nil")
		}
		if field, isField := AsFieldValue(node); isField {
			reanchored, reanchorErr := ReanchorFieldValue(field, exactTarget, exactLayout)
			if reanchorErr == nil {
				return reanchored, nil
			}
			var coded interface{ Code() ResolutionErrorCode }
			if errors.As(reanchorErr, &coded) && coded.Code() == ReanchorUnmappedSource {
				// The owner decides whether this is a declared edge/outer binding or
				// a genuine foreign source. Preserve the complete FieldValue; do not
				// separately rewrite its child and create a mismatched path.
				return node, nil
			}
			return nil, reanchorErr
		}
		if root, isQOV := AsQuantifiedObjectValue(node); isQOV &&
			root.Correlation() == CurrentCorrelation() {
			return TranslatePhaseRoot(root, root, exactTarget)
		}

		children := node.Children()
		if len(children) == 0 {
			return node, nil
		}
		var rebuiltChildren []Value
		for i, child := range children {
			rebuilt, childErr := visit(child)
			if childErr != nil {
				return nil, childErr
			}
			if rebuilt != child {
				if rebuiltChildren == nil {
					rebuiltChildren = make([]Value, len(children))
					copy(rebuiltChildren[:i], children[:i])
				}
				rebuiltChildren[i] = rebuilt
			} else if rebuiltChildren != nil {
				rebuiltChildren[i] = child
			}
		}
		if rebuiltChildren == nil {
			return node, nil
		}
		return withChildrenChecked(node, rebuiltChildren)
	}
	return visit(value)
}

// ReanchorOwnedValueThroughProducer rewrites fields supplied by a
// record-producing Value onto that producer's exact output carrier. This is the
// checked lineage bridge used before a materializer discards its input layout's
// source windows, and it is the ONLY exported way in: an ownership set is
// mandatory, because the bridge's name fallback is a false-accept machine
// without one.
//
// Only roots listed in owned may be claimed by producer. A foreign or outer
// root comes back BYTE-FOR-BYTE UNCHANGED even when a one-slot producer exposes
// the same accessor name and leaf type, and an unresolved root then fails
// loudly downstream instead of silently reading a neighbouring column. Three
// wrong-answer bugs came out of the name-only path — A.VAL and B.VAL both
// reading A's slot, an EXISTS answering on another leg's ID, a sort key read
// under an aliased column's label — so the caller must state what the producer
// owns rather than let the match decide.
//
// Within the owned set a field is selected by its complete accessor-name path
// and exact result type: a matching source correlation wins when duplicate
// column names exist, and absent that ownership proof exactly one matching
// producer slot is required. The producer's output ordinal — not a copied input
// ordinal — is the address installed on target.
func ReanchorOwnedValueThroughProducer(
	value Value,
	producer Value,
	target QuantifiedObjectValue,
	owned map[CorrelationIdentifier]struct{},
) (Value, error) {
	return reanchorValueThroughProducer(value, producer, target, owned)
}

func reanchorValueThroughProducer(
	value Value,
	producer Value,
	target QuantifiedObjectValue,
	owned map[CorrelationIdentifier]struct{},
) (Value, error) {
	if value == nil || producer == nil {
		return nil, resolutionError(ReanchorInvalidValue, "producer.value", "value and producer must be non-nil")
	}
	exactTarget, err := exactLayoutQOV(target, "producer.target")
	if err != nil {
		return nil, err
	}
	rc, ok := producer.(*RecordConstructorValue)
	if !ok || rc == nil {
		return value, nil
	}
	producerType, err := SnapshotExactType(rc.Type())
	if err != nil {
		return nil, err
	}
	if !exactTypesEqual(producerType.(*exactType), exactTarget.flowed) {
		return nil, resolutionError(LayoutTypeMismatch, "producer.target", "producer and target exact row types disagree")
	}

	var visit func(Value) (Value, error)
	visit = func(node Value) (Value, error) {
		if node == nil {
			return nil, resolutionError(RewriteNilReplacement, "producer.value", "value tree contains nil")
		}
		if requestedRoot, isRoot := node.(*quantifiedObjectValue); isRoot &&
			requestedRoot != nil && !requestedRoot.correlation.isCurrent() {
			if owned != nil {
				if _, isOwned := owned[requestedRoot.correlation]; !isOwned {
					return node, nil
				}
			}
			// An Explode leg flows its element as a bare QOV. When a FlatMap
			// retains that complete element in one RC slot, a parent reading the
			// source alias must address the produced slot, just as a retained
			// FieldValue addresses its producer output ordinal. The RC itself is
			// the authority: require one exact correlation+type match. An
			// independently minted current phase, foreign alias, type drift, or a
			// duplicated source slot remains unchanged rather than being guessed.
			matchedOrdinal := -1
			for i, output := range rc.Fields {
				candidate, candidateIsRoot := output.Value.(*quantifiedObjectValue)
				if !candidateIsRoot || candidate == nil ||
					candidate.correlation != requestedRoot.correlation ||
					!exactTypesEqual(candidate.flowed, requestedRoot.flowed) {
					continue
				}
				if matchedOrdinal >= 0 {
					return node, nil
				}
				matchedOrdinal = i
			}
			if matchedOrdinal >= 0 {
				reanchored, resolveErr := ResolveFieldOrdinals(exactTarget, []int{matchedOrdinal})
				if resolveErr != nil {
					return nil, resolutionError(ReanchorInvalidMappedPath, "producer.value", resolveErr.Error())
				}
				mappedType, typeErr := exactTypeOfKnownValue(reanchored)
				if typeErr != nil {
					return nil, typeErr
				}
				if !exactTypesEqual(mappedType, requestedRoot.flowed) {
					return nil, resolutionError(ReanchorResultTypeMismatch, "producer.value", "producer output slot changes the exact object type")
				}
				return reanchored, nil
			}
		}
		if requested, isField := node.(*fieldValue); isField && isAdmittedFieldValue(requested) {
			requestedRoot := requested.Child.(*quantifiedObjectValue)
			// Already expressed on this producer's OUTPUT row: its ordinal IS
			// the answer, and there is nothing to map. This function's job is to
			// carry a value from the producer's INPUTS to its output, and
			// sending an output-domain value through it asks the name fallback
			// to re-derive an address that is already resolved.
			//
			// The fallback then answers by NAME, which is how a cleanup
			// projection reading output slot 0 came back reading slot 2:
			// `SELECT col1 AS id, EXISTS (…) AS e FROM t1 ORDER BY t1.id`
			// appends the sort key as a third output column, and the producer
			// program's only slot whose accessor is named ID is that appended
			// `T1.ID#0` — slot 0 is `T1.COL1#1` and does not match the name.
			// The query returned t1.id under the label the user aliased onto
			// col1.
			if requestedRoot == exactTarget {
				return node, nil
			}
			if owned != nil {
				if _, isOwned := owned[requestedRoot.correlation]; !isOwned {
					return node, nil
				}
			}
			requestedPath := requested.Resolved.Ordinals()
			allMatches := make([][]int, 0, 1)
			ownerMatches := make([][]int, 0, 1)
			ownerOrdinalMatches := make([][]int, 0, 1)
			for i, output := range rc.Fields {
				switch candidate := output.Value.(type) {
				case *fieldValue:
					// A chained positional merge retains an already-produced row as
					// one complete nested field (for example $m._0). The lower
					// materializer expresses a buried leg field on that producer's
					// exact current carrier ($current._0.NAME). Match the complete
					// candidate prefix and carry only the suffix into this producer's
					// output slot. This is exact ordinal lineage: a same-shaped named
					// foreign root cannot take the reserved-current bridge, and a
					// different nested path cannot match the prefix.
					if isAdmittedFieldValue(candidate) {
						candidateRoot := candidate.Child.(*quantifiedObjectValue)
						candidatePath := candidate.Resolved.Ordinals()
						rootOwned := candidateRoot.correlation == requestedRoot.correlation
						currentPhase := requestedRoot.correlation.isCurrent()
						// An OUTER JOIN's null-supplying leg is spelled NULLABLE
						// by the producer that may have to null-pad it, and NOT
						// NULL by a consumer that inherited the leg's own row
						// type. Same correlation, same ordinal path, one bit of
						// nullability apart — which is the widening the leg
						// edge performs, not a different row.
						//
						// Requiring raw exact equality here made this branch
						// reject exactly that pair, and the nested read stayed
						// rooted on a leg the materializer had already dropped:
						// `SELECT l.n.b … l LEFT JOIN r … ORDER BY l.id, r.id`
						// failed at runtime with `exact QOV "L" … has no
						// declared runtime binding`. The TOP-LEVEL name branch
						// below already tolerates it through
						// quantifiedRootsDenoteTheSameRow, which is why
						// `l.id` resolved while `l.n.b` did not — the two
						// branches disagreed about what "the same row" means.
						//
						// Only an OWNED root is widened: the predicate requires
						// equal correlations, so this never crosses legs. The
						// leaf result type is still compared exactly after the
						// match, so a genuine type change is a hard error, not
						// a silent remap.
						rootsAgree := exactTypesEqual(candidateRoot.flowed, requestedRoot.flowed) ||
							(rootOwned && quantifiedRootsDenoteTheSameRow(candidateRoot, requestedRoot))
						if (rootOwned || currentPhase) && rootsAgree &&
							len(requestedPath) > len(candidatePath) &&
							ordinalPathHasPrefix(requestedPath, candidatePath) {
							path := make([]int, 1, 1+len(requestedPath)-len(candidatePath))
							path[0] = i
							path = append(path, requestedPath[len(candidatePath):]...)
							allMatches = append(allMatches, path)
							ownerMatches = append(ownerMatches, path)
							continue
						}
					}
					if !isAdmittedFieldValue(candidate) ||
						!exactTypesEqual(requested.resultType, candidate.resultType) ||
						!ColumnNamePathsEqual(requested, candidate) {
						continue
					}
					candidateRoot := candidate.Child.(*quantifiedObjectValue)
					sameRead := quantifiedRootsDenoteTheSameRow(candidateRoot, requestedRoot) &&
						ordinalPathsEqual(candidate.Resolved.Ordinals(), requestedPath)
					path := []int{i}
					// A name+type match belonging to ANOTHER source is admissible
					// only while this producer does not produce the requested
					// source at all.
					//
					// This set is the compatibility fallback for a logical source
					// whose storage record differs only in NOMINAL naming, and there
					// the candidate legitimately carries a different correlation —
					// so a blanket cross-source ban is wrong and breaks it. What is
					// never legitimate is preferring another leg's slot while the
					// requested leg IS one of this producer's own sources: then the
					// right slot exists here, ownership is the proof that finds it,
					// and failing to find it means the answer is unknown, not that
					// the same-named column next door will do.
					//
					// Without that distinction the owned sets can be EMPTY while
					// this one holds exactly one entry — the other leg's slot — so
					// the single-match guard below passes and publishes it.
					// Measured on `SELECT A.VAL, B.VAL FROM LEFTT A LEFT JOIN
					// RIGHTT B`: B.VAL reanchored to A's slot 1 of
					// [LID VAL RID VAL] (owner=0, all=1), so both output columns
					// read A.VAL — wrong VALUES, with the null-supplying leg's
					// nullability lost along with them.
					// AND, for a CROSS-SOURCE match, only while the producer
					// offers no OTHER slot with the same leaf column and type.
					// The fallback's whole claim is that the one same-named slot
					// must be the requested column; a row holding several is not
					// evidence of anything, and which one this loop admits then
					// turns on an incidental difference in PATH LENGTH — a leg
					// retained as a nested object reads `$m._0.ID` and fails the
					// one-step name-path compare, while an unmerged sibling leg
					// reads `A2.ID` and passes. Measured on `… WHERE EXISTS
					// (SELECT 1 FROM b, c, a a2 WHERE b.id = 10 + a.id - 1)`,
					// whose three legs are all RECORD(ID): B.ID mapped to A2's
					// slot, the EXISTS answered on the wrong column, and the query
					// returned no rows. Latent until record NAMES left type
					// identity, which is what made three legs one shape.
					if !producesSource(rc, requestedRoot.correlation) ||
						candidateRoot.correlation == requestedRoot.correlation {
						if candidateRoot.correlation == requestedRoot.correlation ||
							!leafColumnAppearsMoreThanOnce(rc, requested) {
							allMatches = append(allMatches, path)
						}
					}
					nonCurrentOwner := !requestedRoot.correlation.isCurrent() &&
						candidateRoot.correlation == requestedRoot.correlation
					currentOwner := requestedRoot.correlation.isCurrent() &&
						exactTypesEqual(candidateRoot.flowed, requestedRoot.flowed) &&
						ordinalPathsEqual(candidate.Resolved.Ordinals(), requestedPath)
					if nonCurrentOwner || currentOwner {
						ownerMatches = append(ownerMatches, path)
						// One source can legally expose duplicate same-named,
						// same-typed fields. Correlation ownership alone then names
						// both producer slots, while the already-resolved complete
						// ordinal path names exactly one. Prefer that stronger proof;
						// keep owner/name matching below as the compatibility fallback
						// for a logical source whose storage record differs only in
						// nominal naming.
						if sameRead {
							ownerOrdinalMatches = append(ownerOrdinalMatches, path)
						}
					}
				case *quantifiedObjectValue:
					// A positional merge RC retains each leg as one complete
					// record slot. A field of that exact leg is therefore the
					// output slot followed by its already-resolved leg-local path.
					// Correlation plus exact root type is required; a same-shaped
					// foreign leg cannot select this nested slot by name.
					if candidate.correlation != requestedRoot.correlation ||
						!exactTypesEqual(candidate.flowed, requestedRoot.flowed) {
						continue
					}
					path := make([]int, 1, 1+len(requested.Resolved.Accessors))
					path[0] = i
					path = append(path, requested.Resolved.Ordinals()...)
					allMatches = append(allMatches, path)
					ownerMatches = append(ownerMatches, path)
				}
			}

			matches := allMatches
			if len(ownerOrdinalMatches) != 0 {
				matches = ownerOrdinalMatches
			} else if len(ownerMatches) != 0 {
				matches = ownerMatches
			}
			if len(matches) != 1 {
				// The field may be an outer correlation, or its name may be
				// ambiguous across producer legs. Preserve it for the owner to
				// validate; never publish a guessed ordinal.
				return node, nil
			}
			reanchored, resolveErr := ResolveFieldOrdinals(exactTarget, matches[0])
			if resolveErr != nil {
				return nil, resolutionError(ReanchorInvalidMappedPath, "producer.value", resolveErr.Error())
			}
			mapped, mappedOK := reanchored.(*fieldValue)
			if !mappedOK || !isAdmittedFieldValue(mapped) {
				return nil, resolutionError(ReanchorResultTypeMismatch, "producer.value",
					"producer output slot did not resolve to an exact FieldValue")
			}
			if !exactTypesEqual(requested.resultType, mapped.resultType) {
				return nil, resolutionError(ReanchorResultTypeMismatch, "producer.value",
					"producer output slot changes the exact result type: read "+
						describeExactType(requested.resultType)+", slot "+describeExactType(mapped.resultType))
			}
			return mapped, nil
		}

		children := node.Children()
		if len(children) == 0 {
			return node, nil
		}
		var rebuiltChildren []Value
		for i, child := range children {
			rebuilt, childErr := visit(child)
			if childErr != nil {
				return nil, childErr
			}
			if rebuilt != child {
				if rebuiltChildren == nil {
					rebuiltChildren = make([]Value, len(children))
					copy(rebuiltChildren[:i], children[:i])
				}
				rebuiltChildren[i] = rebuilt
			} else if rebuiltChildren != nil {
				rebuiltChildren[i] = child
			}
		}
		if rebuiltChildren == nil {
			return node, nil
		}
		return withChildrenChecked(node, rebuiltChildren)
	}
	return visit(value)
}

func ordinalPathsEqual(left, right []int) bool {
	if len(left) != len(right) {
		return false
	}
	for i := range left {
		if left[i] != right[i] {
			return false
		}
	}
	return true
}

func ordinalPathHasPrefix(path, prefix []int) bool {
	if len(prefix) > len(path) {
		return false
	}
	for i := range prefix {
		if path[i] != prefix[i] {
			return false
		}
	}
	return true
}

// quantifiedRootsDenoteTheSameRow reports whether two QOV roots address the
// SAME BOUND ROW, which is what makes an already-resolved ordinal path a
// binding proof rather than a coincidence.
//
// For a NAMED root, correlation identity is the caller's precondition and the
// remaining question is only whether the two spellings describe one row —
// exactly the question QuantifiedRowShapesAgree answers, so the top-level
// nullable bit does not participate. It cannot: an outer join pulls its
// output columns up through Java's
// Quantifier.pullUpResultColumnsWithNullability(true), so the producer's leg
// root is nullable while a lineage crossing mints the same leg from the
// child's own non-nullable carrier. Requiring byte equality there discarded
// the ordinal proof and left ownership-by-name as the only evidence — which is
// ambiguous the moment the leg carries two same-named columns, and a
// `FULL OUTER JOIN` of two tables that both have a `K` is that shape. The
// value then survived the crossing still rooted on an intermediate box that
// nothing binds at runtime:
//
//	exact QOV "LB$BOX" (…) has no declared runtime binding
//
// For a CURRENT root the correlation says nothing (every phase spells it
// `_current`), so the exact type is the only thing that can distinguish two
// row phases and the strict comparison stays.
func quantifiedRootsDenoteTheSameRow(candidate, requested *quantifiedObjectValue) bool {
	if candidate == nil || requested == nil ||
		candidate.correlation != requested.correlation {
		return false
	}
	if exactTypesEqual(candidate.flowed, requested.flowed) {
		return true
	}
	if requested.correlation.isCurrent() {
		return false
	}
	return QuantifiedRowShapesAgree(candidate.flowed.Type(), requested.flowed.Type())
}

// leafColumnAppearsMoreThanOnce reports whether more than one of rc's output
// slots reads a column with the same LEAF name and the same exact result type
// as requested.
//
// It counts leaves rather than whole name PATHS on purpose. The cross-source
// fallback matches on the full path, so a leg the producer retained as a nested
// object (`$m._0.ID`) is invisible to it while a sibling leg carried flat
// (`A2.ID`) is not — and the fallback then reads "exactly one candidate" off a
// row that actually holds three copies of the same column. What makes the
// fallback safe is that the requested column is UNAMBIGUOUS in this row, and
// that question has to be asked at the leaf.
func leafColumnAppearsMoreThanOnce(rc *RecordConstructorValue, requested *fieldValue) bool {
	requestedLeaf, ok := leafAccessorName(requested)
	if !ok {
		return false
	}
	seen := 0
	for _, output := range rc.Fields {
		candidate, isField := output.Value.(*fieldValue)
		if !isField || !isAdmittedFieldValue(candidate) ||
			!exactTypesEqual(requested.resultType, candidate.resultType) {
			continue
		}
		candidateLeaf, candidateOK := leafAccessorName(candidate)
		if !candidateOK || candidateLeaf != requestedLeaf {
			continue
		}
		seen++
		if seen > 1 {
			return true
		}
	}
	return false
}

// leafAccessorName returns the last accessor name in v's column-name path.
func leafAccessorName(v Value) (string, bool) {
	path, ok := AccessorNamePath(v)
	if !ok || len(path) == 0 {
		return "", false
	}
	return path[len(path)-1], true
}

// producesSource reports whether rc has any output rooted at correlation — i.e.
// whether this producer carries that source itself.
//
// It is the discriminator between "this producer has no opinion about the
// requested source, so a nominally-renamed candidate is the best available
// evidence" and "this producer owns that source, so the correct slot is here
// and ownership is what finds it". In the second case a same-named slot from a
// DIFFERENT source is not weaker evidence, it is the wrong column.
// The walk goes through the WHOLE slot expression, not just its root. A slot
// spelled `10 + A.ID` carries A as surely as a slot spelled `A.ID` does, and
// reading only the top level answered "no opinion" for it — which handed the
// decision back to the cross-source name path for a source this producer
// demonstrably reads. That is the same shape as the wrong-column bug the
// discriminator exists to prevent, just reached through an expression instead
// of directly.
func producesSource(rc *RecordConstructorValue, correlation CorrelationIdentifier) bool {
	var carries func(Value) bool
	carries = func(node Value) bool {
		switch candidate := node.(type) {
		case nil:
			return false
		case *fieldValue:
			if root, isRoot := candidate.Child.(*quantifiedObjectValue); isRoot &&
				root.correlation == correlation {
				return true
			}
		case *quantifiedObjectValue:
			if candidate.correlation == correlation {
				return true
			}
		}
		for _, child := range node.Children() {
			if carries(child) {
				return true
			}
		}
		return false
	}
	for _, output := range rc.Fields {
		if output.Value != nil && carries(output.Value) {
			return true
		}
	}
	return false
}

// boolText renders a bool for a diagnostic without pulling fmt into this file.
func boolText(b bool) string {
	if b {
		return "true"
	}
	return "false"
}
