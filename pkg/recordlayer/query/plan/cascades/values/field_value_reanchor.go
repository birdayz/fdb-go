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
			return nil, resolutionError(ReanchorTargetMismatch, "reanchor.field", "current root and target have different exact types")
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
				return nil, resolutionError(ReanchorUnmappedSource, "reanchor.field", "layout does not provide the field's exact source")
			}
		} else {
			window := &exactLayout.windows[windowIndex]
			if !exactTypesEqual(window.source.flowed, root.flowed) {
				return nil, resolutionError(ReanchorUnmappedSource, "reanchor.field", "layout source correlation has a different exact type")
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

// ReanchorValueThroughProducer rewrites fields supplied by a record-producing
// Value onto that producer's exact output carrier. This is the checked lineage
// bridge used before a materializer discards its input layout's source windows.
//
// A field is selected by its complete accessor-name path and exact result type.
// A matching source correlation wins when duplicate column names exist; absent
// that ownership proof, exactly one matching producer slot is required. Thus a
// parent edge spelling may cross into a uniquely identified OID slot, while an
// unowned CID in a two-leg join remains unchanged instead of guessing. The
// producer's output ordinal—not a copied input ordinal—is the address installed
// on target.
func ReanchorValueThroughProducer(
	value Value,
	producer Value,
	target QuantifiedObjectValue,
) (Value, error) {
	return reanchorValueThroughProducer(value, producer, target, nil)
}

// ReanchorOwnedValueThroughProducer is the predicate-safe producer bridge.
// Only exact FieldValue roots listed in owned may be claimed by producer;
// foreign/outer roots remain byte-for-byte unchanged even when a one-slot
// producer exposes the same accessor name and leaf type. The ordinary bridge
// intentionally retains its unique-slot fallback for projection and ordering
// programs whose extraction edge no longer carries the logical owner alias.
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
						if (rootOwned || currentPhase) &&
							exactTypesEqual(candidateRoot.flowed, requestedRoot.flowed) &&
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
					path := []int{i}
					allMatches = append(allMatches, path)
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
						if exactTypesEqual(candidateRoot.flowed, requestedRoot.flowed) &&
							ordinalPathsEqual(candidate.Resolved.Ordinals(), requestedPath) {
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
			if !mappedOK || !isAdmittedFieldValue(mapped) ||
				!exactTypesEqual(requested.resultType, mapped.resultType) {
				return nil, resolutionError(ReanchorResultTypeMismatch, "producer.value", "producer output slot changes the exact result type")
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
