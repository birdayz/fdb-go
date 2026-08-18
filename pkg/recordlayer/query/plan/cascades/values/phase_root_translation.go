package values

// TranslatePhaseRoot atomically replaces one exact QOV root with another
// exact QOV of the same semantic object shape. It preserves every resolved
// FieldValue path and never maps a physical carrier prefix; layout-relative
// mapping belongs exclusively to ReanchorFieldValue.
func TranslatePhaseRoot(
	value Value,
	source QuantifiedObjectValue,
	target QuantifiedObjectValue,
) (Value, error) {
	if value == nil {
		return nil, resolutionError(RewriteNilReplacement, "phase.value", "phase Value is nil")
	}
	exactSource, err := exactLayoutQOV(source, "phase.source")
	if err != nil {
		return nil, err
	}
	exactTarget, err := exactLayoutQOV(target, "phase.target")
	if err != nil {
		return nil, err
	}
	if !exactTypesEqual(exactSource.flowed, exactTarget.flowed) {
		return nil, resolutionError(LayoutTypeMismatch, "phase.target", "source and target exact object shapes disagree")
	}
	if exactSource == exactTarget {
		return value, nil
	}
	return replaceLeavesOnceMaybeChecked(value, func(leaf Value) (Value, error) {
		qov, ok := leaf.(*quantifiedObjectValue)
		if !ok || qov != exactSource {
			return leaf, nil
		}
		return exactTarget, nil
	})
}

// TranslateDeclaredEdgeRoot replaces QOV leaves that belong to one declared
// physical edge with an exact target QOV. Unlike TranslatePhaseRoot, this
// matches the declaration by correlation plus exact type rather than object
// pointer: Quantifier.RequireFlowedObjectValue snapshots a new values-owned QOV
// on each call, so pointer identity is not stable across plan construction and
// extraction. A same-correlation leaf with another exact type is retained: a
// merged-row source window may conventionally share the whole edge's alias,
// and exact type is what distinguishes those two declarations. The owning plan
// validates any root that remains after layout/source normalization.
func TranslateDeclaredEdgeRoot(
	value Value,
	declaration QuantifiedObjectValue,
	target QuantifiedObjectValue,
) (Value, error) {
	if value == nil {
		return nil, resolutionError(RewriteNilReplacement, "edge.value", "edge Value is nil")
	}
	exactDeclaration, err := exactLayoutQOV(declaration, "edge.declaration")
	if err != nil {
		return nil, err
	}
	if exactDeclaration.correlation.isCurrent() {
		return nil, resolutionError(CorrelationKindMismatch, "edge.declaration", "physical edge cannot use current correlation")
	}
	exactTarget, err := exactLayoutQOV(target, "edge.target")
	if err != nil {
		return nil, err
	}
	if !exactTypesEqual(exactDeclaration.flowed, exactTarget.flowed) {
		return nil, resolutionError(LayoutTypeMismatch, "edge.target", "edge declaration and target exact object shapes disagree")
	}
	return replaceLeavesOnceMaybeChecked(value, func(leaf Value) (Value, error) {
		root, ok := leaf.(*quantifiedObjectValue)
		if !ok || root.correlation != exactDeclaration.correlation {
			return leaf, nil
		}
		if !exactTypesEqual(root.flowed, exactDeclaration.flowed) {
			return leaf, nil
		}
		return exactTarget, nil
	})
}

// CurrentPhaseCarrierForEdge returns the reserved-current carrier that denotes
// the row phase a declared physical edge delivers. The edge's exact flowed type
// is carried over unchanged, so the result is the exact QOV a selected child's
// layout would publish for that same phase — TranslateDeclaredEdgeRoot's own
// precondition is that declaration and target agree on the exact shape.
//
// It exists for the alias-to-current rebases Java spells as
// AliasMap.ofAliases(quantifier.getAlias(), Quantifier.current()): a value an
// expression states over one of its own child EDGES, handed to that child's
// Reference, has to arrive in the reference's own row space, because the
// reference has never heard of the parent's alias for it.
//
// Pair it with TranslateDeclaredEdgeRoot as the TARGET, never as the declaration.
// The shape precondition — declaration and target agree on the exact object — is
// satisfied by construction, since the edge's exact type is carried over
// unchanged. The result is a CURRENT correlation, though, and
// TranslateDeclaredEdgeRoot rejects a current declaration outright, so passing it
// on the other side is an error rather than a no-op.
//
// The exact type is what makes the target well-defined: every member of a
// Reference carries that Reference's result type (memo admission enforces it),
// so the carrier derived from the edge describes the row every member of that
// group delivers, not one alternative's.
func CurrentPhaseCarrierForEdge(edge QuantifiedObjectValue) (QuantifiedObjectValue, error) {
	exact, err := exactLayoutQOV(edge, "edge.declaration")
	if err != nil {
		return nil, err
	}
	if exact.correlation.isCurrent() {
		return exact, nil
	}
	return &quantifiedObjectValue{correlation: CurrentCorrelation(), flowed: exact.flowed}, nil
}

// TranslateLogicalSourceRoot moves one explicitly declared logical source onto
// the exact physical QOV selected for that source. Logical join seeds retain a
// nominal record identity (for example B RECORD<...>) while stored-row plans
// publish the executor's physical carrier (RECORD<...>). Those are not equal
// exact types, so an alias-only rebase leaves a Value that names the physical
// edge with the logical type and cannot be bound at runtime.
//
// The declaration makes this conversion unambiguous. A FieldValue is eligible
// only when its root matches BOTH the declaration's correlation and exact type;
// a same-correlation retained window of another type remains untouched. The
// complete ordinal path is then resolved again on target and its exact result
// type must stay unchanged. Thus this bridge may discard a phase-local root
// record name, but it cannot change which slot is read, its leaf type, or its
// nullability. A bare declared QOV becomes target directly.
//
// The rewrite is copy-on-write and atomic: any selected path that the physical
// target cannot represent returns an error and no partial Value.
func TranslateLogicalSourceRoot(
	value Value,
	declaration QuantifiedObjectValue,
	target QuantifiedObjectValue,
) (Value, error) {
	if value == nil {
		return nil, resolutionError(RewriteNilReplacement, "logical-source.value", "logical source Value is nil")
	}
	exactDeclaration, err := exactLayoutQOV(declaration, "logical-source.declaration")
	if err != nil {
		return nil, err
	}
	if exactDeclaration.correlation.isCurrent() {
		return nil, resolutionError(CorrelationKindMismatch, "logical-source.declaration", "logical source cannot use current correlation")
	}
	exactTarget, err := exactLayoutQOV(target, "logical-source.target")
	if err != nil {
		return nil, err
	}

	var visit func(Value) (Value, error)
	visit = func(node Value) (Value, error) {
		if node == nil {
			return nil, resolutionError(RewriteNilReplacement, "logical-source.value", "value tree contains nil")
		}
		if field, isField := node.(*fieldValue); isField && isAdmittedFieldValue(field) {
			root := field.Child.(*quantifiedObjectValue)
			if root.correlation != exactDeclaration.correlation ||
				!exactTypesEqual(root.flowed, exactDeclaration.flowed) {
				// Preserve the complete FieldValue. Descending into its QOV child
				// separately would attach the old resolved path to a different
				// domain and create a partially retargeted field.
				return node, nil
			}
			resolved, resolveErr := RebuildFieldValue(field, exactTarget)
			if resolveErr != nil {
				return nil, resolutionError(ReanchorInvalidMappedPath, "logical-source.field", resolveErr.Error())
			}
			retargeted, ok := resolved.(*fieldValue)
			if !ok || !isAdmittedFieldValue(retargeted) {
				return nil, resolutionError(ReanchorInvalidMappedPath, "logical-source.field", "resolved path did not produce an exact FieldValue")
			}
			if !exactTypesEqual(field.resultType, retargeted.resultType) {
				return nil, resolutionError(ReanchorResultTypeMismatch, "logical-source.field", "physical source path changes the exact result type or nullability")
			}
			return retargeted, nil
		}
		if root, isQOV := node.(*quantifiedObjectValue); isQOV &&
			root.correlation == exactDeclaration.correlation &&
			exactTypesEqual(root.flowed, exactDeclaration.flowed) {
			return exactTarget, nil
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

// TranslateLogicalSourceNameNormalization RE-ROOTS every QOV at source onto the
// target INSTANCE when the two denote the same exact row. Logical table sources
// commonly retain a nominal record name (EMP RECORD<...>) while their physical
// scan publishes the same executor row anonymously; both are the same row, and
// the physical carrier is the instance the producer's layout and its retained
// windows are keyed by.
//
// It is a re-rooting, NOT a type conversion. It once was a conversion — record
// names were part of Go's Type identity, so a nominal row and its anonymous
// carrier were two "different" types and this bridge existed to cross them.
// Names are provenance in Java (Type.Record.equals compares typeCode,
// nullability and fields only) and now in Go, so the two sides are simply
// equal and what remains to move is which QOV instance the program hangs from.
//
// A narrower retained window that shares the source correlation, a foreign
// correlation, or any structural drift remains untouched. Eligible FieldValues
// are rebuilt by their complete ordinal path, and the rewrite is atomic on
// error.
func TranslateLogicalSourceNameNormalization(
	value Value,
	source CorrelationIdentifier,
	target QuantifiedObjectValue,
) (Value, error) {
	if value == nil {
		return nil, resolutionError(RewriteNilReplacement, "logical-source-name.value", "logical source Value is nil")
	}
	if source.isCurrent() {
		return nil, resolutionError(CorrelationKindMismatch, "logical-source-name.source", "logical source cannot use current correlation")
	}
	exactTarget, err := exactLayoutQOV(target, "logical-source-name.target")
	if err != nil {
		return nil, err
	}

	var visit func(Value) (Value, error)
	visit = func(node Value) (Value, error) {
		if node == nil {
			return nil, resolutionError(RewriteNilReplacement, "logical-source-name.value", "value tree contains nil")
		}
		if field, isField := node.(*fieldValue); isField && isAdmittedFieldValue(field) {
			root := field.Child.(*quantifiedObjectValue)
			if root.correlation != source ||
				!logicalSourceSameExactRow(root.flowed, exactTarget.flowed) {
				return node, nil
			}
			resolved, resolveErr := RebuildFieldValue(field, exactTarget)
			if resolveErr != nil {
				return nil, resolutionError(ReanchorInvalidMappedPath, "logical-source-name.field", resolveErr.Error())
			}
			retargeted, ok := resolved.(*fieldValue)
			if !ok || !isAdmittedFieldValue(retargeted) {
				return nil, resolutionError(ReanchorInvalidMappedPath, "logical-source-name.field", "resolved path did not produce an exact FieldValue")
			}
			if !exactTypesEqual(field.resultType, retargeted.resultType) {
				return nil, resolutionError(ReanchorResultTypeMismatch, "logical-source-name.field", "physical source path changes the exact result type or nullability")
			}
			return retargeted, nil
		}
		if root, isQOV := node.(*quantifiedObjectValue); isQOV &&
			root.correlation == source &&
			logicalSourceSameExactRow(root.flowed, exactTarget.flowed) {
			return exactTarget, nil
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

// TranslateLogicalSourceNameNormalizationToCorrelation is the retained-window
// form of TranslateLogicalSourceNameNormalization. It changes only the exact
// top-level record name while preserving the logical source correlation. A
// materializing producer (NLJ/FlatMap) can then prove that source's output
// ordinal through its retained result program; changing the alias to the whole
// physical edge first would erase the source ownership needed for duplicate
// and nested fields.
//
// Every QOV rooted at source is considered independently. Only one whose full
// concrete row differs from targetType by the top-level record name is rebuilt;
// narrower same-alias windows, structural drift, and foreign correlations are
// left untouched.
func TranslateLogicalSourceNameNormalizationToCorrelation(
	value Value,
	source CorrelationIdentifier,
	targetType Type,
) (Value, error) {
	if value == nil {
		return nil, resolutionError(RewriteNilReplacement, "logical-source-name.value", "logical source Value is nil")
	}
	if source.isCurrent() {
		return nil, resolutionError(CorrelationKindMismatch, "logical-source-name.source", "logical source cannot use current correlation")
	}
	target, err := NewQuantifiedObjectValue(source, targetType)
	if err != nil {
		return nil, err
	}
	return TranslateLogicalSourceNameNormalization(value, source, target)
}

// TranslateLogicalSourceNameNormalizationInValue retargets a logical source
// onto the one exact same-correlation row declaration already retained by
// authority. It is the producer-local form used when the selected child's scan
// type is anonymous but the producer result has preserved the physical named
// source window. No target is minted from a child edge: the exact QOV embedded
// in authority is reused, and ambiguity/conflicting target declarations decline.
func TranslateLogicalSourceNameNormalizationInValue(
	value Value,
	source CorrelationIdentifier,
	authority Value,
) (Value, error) {
	if value == nil || authority == nil {
		return nil, resolutionError(RewriteNilReplacement, "logical-source-name.value", "value and authority must be non-nil")
	}
	if source.isCurrent() {
		return nil, resolutionError(CorrelationKindMismatch, "logical-source-name.source", "logical source cannot use current correlation")
	}
	var target QuantifiedObjectValue
	var conflict bool
	WalkValue(authority, func(node Value) bool {
		candidate, ok := AsQuantifiedObjectValue(node)
		if !ok || candidate.Correlation() != source {
			return true
		}
		// Shared graphs: this asks the two rows a question and keeps neither.
		// FlowedType here rebuilt BOTH whole type graphs per candidate node of
		// every authority walk — 6.2GB on the pure-planner sweep, the single
		// largest consumer of the defensive copy.
		if target != nil && !SharedFlowedType(target).Equals(SharedFlowedType(candidate)) {
			conflict = true
			return false
		}
		target = candidate
		return true
	})
	if conflict || target == nil {
		return value, nil
	}
	return TranslateLogicalSourceNameNormalization(value, source, target)
}

// logicalSourceSameExactRow reports whether a logical source declaration and a
// physical target denote the SAME concrete row, which is the only case the
// re-rooting bridge above may cross.
//
// It is exact-type equality with a concrete-record guard, and it is written
// that way on purpose rather than as its own field-by-field walk: the walk it
// replaced compared arity, per-field name, ordinal and type but skipped the
// RECORD NAME, because a nominal row and its anonymous carrier were then two
// different types. Record names are no longer identity, so that walk had become
// a hand-rolled copy of exactTypesEqual that could only drift away from it.
func logicalSourceSameExactRow(declaration, target *exactType) bool {
	if declaration == nil || target == nil ||
		declaration.code != TypeCodeRecord || target.code != TypeCodeRecord ||
		declaration.anyRecord || target.anyRecord {
		return false
	}
	return exactTypesEqual(declaration, target)
}

// TranslateProjectionInputNameNormalization moves a projection program from
// the logical input row declaration onto the exact physical input edge when
// the producer changed only its top-level output field names. Duplicate SQL
// aliases are the motivating case: the logical derived-table row can legally
// contain [X, X, Y], while the projection producer publishes the unambiguous
// physical row [X, X_2, Y]. The field ordinal remains the identity.
//
// This is deliberately narrower than a general row conversion. Declaration
// and target must use the same exact correlation, have the same concrete
// record name, width, record nullability, field ordinals, and exact field
// types. Only the top-level field names may disagree; nested names remain part
// of each field's exact type. A matching FieldValue is rebuilt by its complete
// ordinal path, so neither a rendered name nor a name lookup participates.
// Same-correlation leaves of another exact type and foreign correlations are
// retained for their owning binding authority to validate.
func TranslateProjectionInputNameNormalization(
	value Value,
	declaration QuantifiedObjectValue,
	target QuantifiedObjectValue,
) (Value, error) {
	if value == nil {
		return nil, resolutionError(RewriteNilReplacement, "projection-input.value", "projection input Value is nil")
	}
	exactDeclaration, err := exactLayoutQOV(declaration, "projection-input.declaration")
	if err != nil {
		return nil, err
	}
	exactTarget, err := exactLayoutQOV(target, "projection-input.target")
	if err != nil {
		return nil, err
	}
	if exactDeclaration.correlation != exactTarget.correlation {
		return nil, resolutionError(CorrelationTypeConflict, "projection-input.target", "projection declaration and target correlations disagree")
	}
	if !projectionInputNamesOnlyCompatible(exactDeclaration.flowed, exactTarget.flowed) {
		// Both rows are in the message because the difference is the whole
		// finding, and every caller reports this failure as a query-level
		// 0AF00 where nothing downstream can say WHICH of record name, width,
		// ordinal or leaf type moved.
		return nil, resolutionError(LayoutTypeMismatch, "projection-input.target",
			"projection declaration and target differ beyond top-level field names: declared "+
				describeExactType(exactDeclaration.flowed)+", target "+describeExactType(exactTarget.flowed))
	}

	return replaceLeavesOnceMaybeChecked(value, func(leaf Value) (Value, error) {
		root, ok := leaf.(*quantifiedObjectValue)
		if !ok || root.correlation != exactDeclaration.correlation ||
			!exactTypesEqual(root.flowed, exactDeclaration.flowed) {
			return leaf, nil
		}
		return exactTarget, nil
	})
}

// TranslateProjectionInputNameNormalizationToCorrelation is the
// source-correlation form of TranslateProjectionInputNameNormalization. It is
// used at a boundary where the physical producer's exact row type is known but
// the logical declaration is embedded in the Value program. WITH ORDINALITY is
// such a boundary: SQL names its two output slots with AS/AT aliases while the
// physical Explode carrier deliberately retains the private positional names
// _0/_1.
//
// Only an exact source-correlated record whose top-level field names actually
// differ, but whose record identity, width, nullability, ordinals, and exact
// leaf types all agree, is moved. Same-named declarations are pointer-stable;
// foreign/current roots and every structural drift are retained for their own
// binding authority. The ordinary checked Value rebuild preserves the complete
// resolved ordinal path and rejects a result-type change atomically.
func TranslateProjectionInputNameNormalizationToCorrelation(
	value Value,
	source CorrelationIdentifier,
	targetType Type,
) (Value, error) {
	if value == nil {
		return nil, resolutionError(RewriteNilReplacement, "projection-input.value", "projection input Value is nil")
	}
	if source.isCurrent() {
		return nil, resolutionError(CorrelationKindMismatch, "projection-input.source", "projection source cannot use current correlation")
	}
	target, err := NewQuantifiedObjectValue(source, targetType)
	if err != nil {
		return nil, err
	}
	exactTarget, err := exactLayoutQOV(target, "projection-input.target")
	if err != nil {
		return nil, err
	}
	return replaceLeavesOnceMaybeChecked(value, func(leaf Value) (Value, error) {
		root, ok := leaf.(*quantifiedObjectValue)
		if !ok || root.correlation != source ||
			!projectionInputNamesOnlyCompatible(root.flowed, exactTarget.flowed) ||
			!projectionInputFieldNamesDiffer(root.flowed, exactTarget.flowed) {
			return leaf, nil
		}
		return exactTarget, nil
	})
}

func projectionInputFieldNamesDiffer(declaration, target *exactType) bool {
	if declaration == nil || target == nil || len(declaration.fields) != len(target.fields) {
		return false
	}
	for i := range declaration.fields {
		if declaration.fields[i].name != target.fields[i].name {
			return true
		}
	}
	return false
}

func projectionInputNamesOnlyCompatible(declaration, target *exactType) bool {
	if declaration == nil || target == nil ||
		declaration.code != TypeCodeRecord || target.code != TypeCodeRecord ||
		declaration.anyRecord || target.anyRecord ||
		declaration.name != target.name || declaration.nullable != target.nullable ||
		len(declaration.fields) != len(target.fields) {
		return false
	}
	for i := range declaration.fields {
		declaredField := declaration.fields[i]
		targetField := target.fields[i]
		if declaredField.ordinal != i || targetField.ordinal != i ||
			!exactTypesEqual(declaredField.typ, targetField.typ) {
			return false
		}
	}
	return true
}

// TranslateNullExtendedPhaseRoot crosses the ONE phase boundary where a row's
// exact type legitimately changes: an operator that may emit NO row for its
// child republishes the child's row widened to nullable. Ordinals, arity, field
// names, record names and every NESTED nullability are unchanged — only the
// top-level bit moves, and only in the widening direction.
//
// This exists because that boundary is otherwise IMPASSABLE by construction and
// was being crossed by guesswork instead. A lineage walk stops at any wrapper
// whose layout differs from its child's, which is right in general — provenance
// must not leak through a reshaping operator — but a null-extension wrapper is
// not reshaping anything. The value then arrived at the enclosing producer still
// rooted on the child's row, no ownership proof could place it, and the only
// thing that ever resolved it was one output slot happening to carry the same
// accessor name. On `SELECT ma.id, b.bid, c.cid FROM mb b JOIN mc c ON b.ref =
// c.cid RIGHT JOIN ma ON ma.id = b.bid` that name match is correct; give the box
// two same-named columns and it is a coin flip that reads the wrong one.
//
// The widening direction is asserted rather than tolerated in both directions.
// Narrowing a nullable row onto a NOT NULL phase would be a claim that a row
// which may be absent is always present, which is exactly the fact an outer
// join exists to record.
func TranslateNullExtendedPhaseRoot(
	value Value,
	source QuantifiedObjectValue,
	target QuantifiedObjectValue,
) (Value, error) {
	if value == nil {
		return nil, resolutionError(RewriteNilReplacement, "null-extended.value", "phase Value is nil")
	}
	exactSource, err := exactLayoutQOV(source, "null-extended.source")
	if err != nil {
		return nil, err
	}
	exactTarget, err := exactLayoutQOV(target, "null-extended.target")
	if err != nil {
		return nil, err
	}
	if exactSource == exactTarget {
		return value, nil
	}
	if !exactRowShapesAgree(exactSource.flowed, exactTarget.flowed) {
		return nil, resolutionError(LayoutTypeMismatch, "null-extended.target",
			"source and target are not the same row: source "+describeExactType(exactSource.flowed)+
				", target "+describeExactType(exactTarget.flowed))
	}
	if exactSource.flowed.nullable || !exactTarget.flowed.nullable {
		return nil, resolutionError(LayoutTypeMismatch, "null-extended.target",
			"a null-extension widens a present row to an absent-capable one; source nullable="+
				boolText(exactSource.flowed.Type().IsNullable())+", target nullable="+
				boolText(exactTarget.flowed.Type().IsNullable()))
	}

	var visit func(Value) (Value, error)
	visit = func(node Value) (Value, error) {
		if node == nil {
			return nil, resolutionError(RewriteNilReplacement, "null-extended.value", "value tree contains nil")
		}
		if field, isField := node.(*fieldValue); isField && isAdmittedFieldValue(field) {
			if field.Child.(*quantifiedObjectValue) != exactSource {
				// Preserve the complete FieldValue rather than descending into its
				// root; a separately retargeted child would carry the old resolved
				// path into a different domain.
				return node, nil
			}
			rebuilt, rebuildErr := RebuildFieldValue(field, exactTarget)
			if rebuildErr != nil {
				return nil, resolutionError(ReanchorInvalidMappedPath, "null-extended.field", rebuildErr.Error())
			}
			retargeted, ok := rebuilt.(*fieldValue)
			if !ok || !isAdmittedFieldValue(retargeted) {
				return nil, resolutionError(ReanchorInvalidMappedPath, "null-extended.field",
					"resolved path did not produce an exact FieldValue")
			}
			// Reading INTO the row is unaffected by whether the row may be absent,
			// so a leaf whose exact type moved means the two rows were not the same
			// row after all and the shape check above was not strong enough.
			if !exactTypesEqual(field.resultType, retargeted.resultType) {
				return nil, resolutionError(ReanchorResultTypeMismatch, "null-extended.field",
					"crossing the null-extension changed a leaf's exact type")
			}
			return retargeted, nil
		}
		if root, isQOV := node.(*quantifiedObjectValue); isQOV && root == exactSource {
			// The whole-row read is the one place the type is SUPPOSED to change:
			// the caller asked for the row this operator emits, which may be absent.
			return exactTarget, nil
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
