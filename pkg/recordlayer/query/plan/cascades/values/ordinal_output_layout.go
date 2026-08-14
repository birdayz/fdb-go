package values

// OrdinalOutputSource declares a quantified source whose complete object is
// retained by a flat RecordConstructor output. A record can be retained
// field-by-field or as one nested object; a scalar is necessarily retained as
// one object slot.
// NullSupplying is physical edge information; it is never inferred from a
// nullable logical type.
type OrdinalOutputSource struct {
	Source QuantifiedObjectValue
	// ObjectPath is non-nil when the result retains Source as one complete
	// nested object instead of copying its fields into separate output slots.
	// NewFlatOrdinalLayoutForRetainedResult derives this only from a bare exact
	// record QOV occupying one RecordConstructor slot.
	ObjectPath    []int
	NullSupplying bool
}

// NewFlatOrdinalLayoutForRetainedResult discovers every complete exact QOV
// retained by a flat result program and publishes an object/field window for it.
// Partial sources are intentionally absent: they cannot be materialized as a
// whole typed object. nullSupplying identifies the subset whose edge may be
// unmatched on a row.
func NewFlatOrdinalLayoutForRetainedResult(result Value, nullSupplying []QuantifiedObjectValue) (OrdinalLayout, error) {
	return NewFlatOrdinalLayoutForRetainedResultWithSources(result, nullSupplying, nil)
}

// NewFlatOrdinalLayoutForRetainedResultWithSources adds exact producer-proven
// whole-object sources to the sources discovered directly in result. This is
// used by a materializing parent whose flat result program copies one scalar
// source out of a selected child carrier: the scalar is no longer a direct QOV
// slot in the parent's program, but the child producer and the parent's output
// ordinal together still prove its complete ObjectPath.
//
// additional is not a compatibility escape hatch. NewFlatOrdinalLayoutForResult
// revalidates every source and path against result and rejects duplicate
// correlations, exact-type conflicts, and paths which do not select the exact
// source type.
func NewFlatOrdinalLayoutForRetainedResultWithSources(
	result Value,
	nullSupplying []QuantifiedObjectValue,
	additional []OrdinalOutputSource,
) (OrdinalLayout, error) {
	rc, ok := result.(*RecordConstructorValue)
	if !ok || rc == nil {
		return nil, resolutionError(LayoutInvalidWindow, "layout.result", "retained-source discovery requires a record constructor")
	}
	type retainedSource struct {
		source     *quantifiedObjectValue
		ordinals   []int
		objectPath []int
		duplicate  bool
	}
	ordered := make([]*retainedSource, 0)
	byCorrelation := make(map[CorrelationIdentifier]*retainedSource)
	for outputOrdinal := range rc.Fields {
		if root, isRoot := rc.Fields[outputOrdinal].Value.(*quantifiedObjectValue); isRoot &&
			root != nil && root.flowed != nil && !root.flowed.anyRecord {
			state := byCorrelation[root.correlation]
			if state == nil {
				state = &retainedSource{source: root, objectPath: []int{outputOrdinal}}
				byCorrelation[root.correlation] = state
				ordered = append(ordered, state)
			} else if !exactTypesEqual(state.source.flowed, root.flowed) {
				return nil, resolutionError(CorrelationTypeConflict, "layout.result", "one retained correlation has conflicting exact types")
			} else {
				state.duplicate = true
			}
			continue
		}
		field, isField := rc.Fields[outputOrdinal].Value.(*fieldValue)
		if !isField || !isAdmittedFieldValue(field) || len(field.Resolved.Accessors) != 1 {
			continue
		}
		root, rootOK := field.Child.(*quantifiedObjectValue)
		if !rootOK || root.flowed.code != TypeCodeRecord || root.flowed.anyRecord {
			continue
		}
		state := byCorrelation[root.correlation]
		if state == nil {
			state = &retainedSource{source: root, ordinals: make([]int, len(root.flowed.fields))}
			for i := range state.ordinals {
				state.ordinals[i] = -1
			}
			byCorrelation[root.correlation] = state
			ordered = append(ordered, state)
		} else if !exactTypesEqual(state.source.flowed, root.flowed) {
			return nil, resolutionError(CorrelationTypeConflict, "layout.result", "one retained correlation has conflicting exact types")
		} else if state.objectPath != nil {
			state.duplicate = true
			continue
		}
		sourceOrdinal := field.Resolved.Accessors[0].Ordinal
		if sourceOrdinal < 0 || sourceOrdinal >= len(state.ordinals) {
			return nil, resolutionError(LayoutInvalidPath, "layout.result", "retained field ordinal is outside its source")
		}
		if state.ordinals[sourceOrdinal] >= 0 {
			state.duplicate = true
			continue
		}
		state.ordinals[sourceOrdinal] = outputOrdinal
	}

	nullByCorrelation := make(map[CorrelationIdentifier]*quantifiedObjectValue, len(nullSupplying))
	for i := range nullSupplying {
		source, err := exactLayoutQOV(nullSupplying[i], "layout.nullSupplying["+uitoa(uint64(i))+"]")
		if err != nil {
			return nil, err
		}
		if previous, exists := nullByCorrelation[source.correlation]; exists {
			if exactTypesEqual(previous.flowed, source.flowed) {
				return nil, resolutionError(LayoutDuplicateSource, "layout.nullSupplying", "null-supplying source is duplicated")
			}
			return nil, resolutionError(CorrelationTypeConflict, "layout.nullSupplying", "one null-supplying correlation has conflicting exact types")
		}
		nullByCorrelation[source.correlation] = source
	}
	sources := make([]OrdinalOutputSource, 0, len(ordered)+len(additional))
	for _, state := range ordered {
		if state.duplicate {
			continue
		}
		if state.objectPath == nil {
			complete := true
			for _, ordinal := range state.ordinals {
				if ordinal < 0 {
					complete = false
					break
				}
			}
			if !complete {
				continue
			}
		}
		nullSource, isNullSupplying := nullByCorrelation[state.source.correlation]
		if isNullSupplying && !exactTypesEqual(nullSource.flowed, state.source.flowed) {
			return nil, resolutionError(CorrelationTypeConflict, "layout.nullSupplying", "null-supplying source type disagrees with retained source")
		}
		sources = append(sources, OrdinalOutputSource{
			Source: state.source, ObjectPath: append([]int(nil), state.objectPath...),
			NullSupplying: isNullSupplying,
		})
	}
	for i := range additional {
		sources = append(sources, OrdinalOutputSource{
			Source:        additional[i].Source,
			ObjectPath:    append([]int(nil), additional[i].ObjectPath...),
			NullSupplying: additional[i].NullSupplying,
		})
	}
	return NewFlatOrdinalLayoutForResult(result, sources)
}

// NewFlatOrdinalLayoutForResult derives a physical flat carrier and exact
// source windows from a RecordConstructor result program. A record source in
// field mode must occur exactly once per field as one-step FieldValues. Any
// exact source (including a scalar) may instead occupy one complete ObjectPath
// slot. Computed/constant output fields remain ordinary flat carrier slots.
//
// Partial sources deliberately fail instead of fabricating sparse records: a
// downstream source-relative FieldValue is legal only when the whole typed
// QOV object can be bound. Projections that retain only part of a source must
// address their output through the carrier/current QOV instead.
func NewFlatOrdinalLayoutForResult(result Value, sources []OrdinalOutputSource) (OrdinalLayout, error) {
	rc, ok := result.(*RecordConstructorValue)
	if !ok || rc == nil {
		return nil, resolutionError(LayoutInvalidWindow, "layout.result", "flat output layout requires an exact record constructor")
	}
	resultHandle, err := SnapshotExactType(rc.Type())
	if err != nil {
		return nil, err
	}
	resultType := resultHandle.(*exactType)
	if resultType.code != TypeCodeRecord || resultType.anyRecord {
		return nil, resolutionError(LayoutNonRecordCarrier, "layout.result", "record constructor did not produce a concrete record")
	}

	var tiles []OrdinalTileSpec
	if width := len(resultType.fields); width > 0 {
		tiles = []OrdinalTileSpec{{Start: 0, Width: width, Kind: OrdinalTileFlat}}
	}
	windows := make([]OrdinalWindowSpec, 0, len(sources))
	windowByCorrelation := make(map[CorrelationIdentifier]int, len(sources))
	seenSources := make(map[CorrelationIdentifier]*quantifiedObjectValue, len(sources))
	for sourceIndex := range sources {
		path := "layout.outputSource[" + uitoa(uint64(sourceIndex)) + "]"
		source, sourceErr := exactLayoutQOV(sources[sourceIndex].Source, path+".source")
		if sourceErr != nil {
			return nil, sourceErr
		}
		if source.correlation.isCurrent() {
			return nil, resolutionError(CorrelationKindMismatch, path+".source", "current cannot be a retained output source")
		}
		if source.flowed.anyRecord {
			return nil, resolutionError(LayoutInvalidWindow, path+".source", "flat retained source must have one exact type")
		}
		if sources[sourceIndex].ObjectPath == nil && source.flowed.code != TypeCodeRecord {
			return nil, resolutionError(LayoutInvalidWindow, path+".source", "field-addressed retained source must be a concrete record")
		}
		if previous, exists := seenSources[source.correlation]; exists {
			if exactTypesEqual(previous.flowed, source.flowed) {
				return nil, resolutionError(LayoutDuplicateSource, path+".source", "retained source is duplicated")
			}
			return nil, resolutionError(CorrelationTypeConflict, path+".source", "one retained correlation has conflicting exact types")
		}
		seenSources[source.correlation] = source

		fieldPaths := make([][]int, len(source.flowed.fields))
		objectPath := append([]int(nil), sources[sourceIndex].ObjectPath...)
		if objectPath != nil {
			if len(objectPath) != 1 || objectPath[0] < 0 || objectPath[0] >= len(resultType.fields) ||
				!exactTypesEqual(resultType.fields[objectPath[0]].typ, source.flowed) {
				return nil, resolutionError(LayoutInvalidPath, path, "whole-object source path does not select its exact result slot")
			}
			for fieldOrdinal := range fieldPaths {
				fieldPaths[fieldOrdinal] = []int{objectPath[0], fieldOrdinal}
			}
		} else {
			for outputOrdinal := range rc.Fields {
				field, isField := rc.Fields[outputOrdinal].Value.(*fieldValue)
				if !isField || !isAdmittedFieldValue(field) {
					continue
				}
				root, rootOK := field.Child.(*quantifiedObjectValue)
				if !rootOK || root.correlation != source.correlation || !exactTypesEqual(root.flowed, source.flowed) {
					continue
				}
				if len(field.Resolved.Accessors) != 1 {
					continue
				}
				sourceOrdinal := field.Resolved.Accessors[0].Ordinal
				if sourceOrdinal < 0 || sourceOrdinal >= len(fieldPaths) {
					return nil, resolutionError(LayoutInvalidPath, path, "retained field ordinal is outside its exact source")
				}
				if fieldPaths[sourceOrdinal] != nil {
					return nil, resolutionError(LayoutInvalidWindow, path, "one retained source field appears more than once")
				}
				fieldPaths[sourceOrdinal] = []int{outputOrdinal}
			}
			for fieldOrdinal := range fieldPaths {
				if fieldPaths[fieldOrdinal] == nil {
					return nil, resolutionError(LayoutSourceNotProvided, path, "result does not retain every field of the declared source")
				}
			}
		}
		topLevel := OrdinalWindowSpec{
			Source:        source,
			ObjectPath:    objectPath,
			FieldPaths:    fieldPaths,
			NullSupplying: sources[sourceIndex].NullSupplying,
		}
		if objectPath != nil {
			topLevel.FieldPaths = nil
		}
		windowByCorrelation[source.correlation] = len(windows)
		windows = append(windows, topLevel)

		buried, buriedErr := buriedOrdinalOutputWindows(
			source, fieldPaths, sources[sourceIndex].NullSupplying)
		if buriedErr != nil {
			return nil, buriedErr
		}
		for _, spec := range buried {
			buriedSource := spec.Source.(*quantifiedObjectValue)
			if existing, taken := windowByCorrelation[buriedSource.correlation]; taken {
				// A box is conventionally bound under its rightmost leaf's
				// correlation. That leaf window is more precise than the whole-box
				// window and replaces it; any other collision is already provided by
				// a distinct top-level source and remains authoritative.
				if buriedSource.correlation == source.correlation {
					windows[existing] = spec
				}
				continue
			}
			windowByCorrelation[buriedSource.correlation] = len(windows)
			windows = append(windows, spec)
		}
	}

	carrier, err := newCurrentQOVForLayout(resultType.thaw())
	if err != nil {
		return nil, err
	}
	return NewOrdinalLayout(carrier, tiles, windows)
}

// buriedOrdinalOutputWindows turns the private physical source-layout snapshot
// on a complete retained box QOV into explicit OrdinalLayout windows. The
// snapshot never re-enters Type()/semantic identity; after this conversion the
// immutable OrdinalLayout is the sole runtime authority.
func buriedOrdinalOutputWindows(
	parent *quantifiedObjectValue,
	parentPaths [][]int,
	nullSupplying bool,
) ([]OrdinalWindowSpec, error) {
	if parent == nil {
		return nil, nil
	}
	return buriedOrdinalOutputWindowsFromLayout(
		parent.sourceLayout, parent.flowed, parentPaths, nullSupplying)
}

func buriedOrdinalOutputWindowsFromLayout(
	layout *qovRecordLayout,
	flowed *exactType,
	parentPaths [][]int,
	nullSupplying bool,
) ([]OrdinalWindowSpec, error) {
	if layout == nil || flowed == nil || flowed.code != TypeCodeRecord || flowed.anyRecord {
		return nil, nil
	}
	out := make([]OrdinalWindowSpec, 0, len(layout.legs))
	for legIndex, leg := range layout.legs {
		path := "layout.sourceLeg[" + uitoa(uint64(legIndex)) + "]"
		if leg.Alias.IsZero() || leg.Start < 0 || leg.Start >= len(flowed.fields) {
			return nil, resolutionError(LayoutInvalidWindow, path, "buried source has invalid identity or bounds")
		}
		switch leg.Kind {
		case LegKindFlatRun:
			if leg.Width <= 0 || leg.Start+leg.Width > len(flowed.fields) {
				return nil, resolutionError(LayoutInvalidWindow, path, "flat buried source has invalid width")
			}
			fields := make([]Field, leg.Width)
			fieldPaths := make([][]int, leg.Width)
			for i := 0; i < leg.Width; i++ {
				exactField := flowed.fields[leg.Start+i]
				fields[i] = Field{Name: exactField.name, Ordinal: i, FieldType: exactField.typ.thaw()}
				fieldPaths[i] = append([]int(nil), parentPaths[leg.Start+i]...)
			}
			sourceType := &RecordType{Nullable: flowed.nullable, Fields: fields}
			source, err := NewQuantifiedObjectValue(leg.Alias, sourceType)
			if err != nil {
				return nil, err
			}
			out = append(out, OrdinalWindowSpec{
				Source:        source,
				FieldPaths:    fieldPaths,
				NullSupplying: nullSupplying,
			})
		case LegKindNested:
			if leg.Width != 1 || flowed.fields[leg.Start].typ.code != TypeCodeRecord ||
				flowed.fields[leg.Start].typ.anyRecord {
				return nil, resolutionError(LayoutInvalidWindow, path, "nested buried source must occupy one concrete record slot")
			}
			nestedExact := flowed.fields[leg.Start].typ
			nestedType := nestedExact.thaw()
			if nullSupplying {
				nestedType = WithNullability(nestedType, true)
			}
			source, err := NewQuantifiedObjectValue(leg.Alias, nestedType)
			if err != nil {
				return nil, err
			}
			var nestedLayout *qovRecordLayout
			if leg.Start < len(layout.fields) {
				nestedLayout = layout.fields[leg.Start]
			}
			if concrete, ok := source.(*quantifiedObjectValue); ok {
				concrete.sourceLayout = nestedLayout
			}
			objectPath := append([]int(nil), parentPaths[leg.Start]...)
			out = append(out, OrdinalWindowSpec{
				Source:        source,
				ObjectPath:    objectPath,
				NullSupplying: nullSupplying,
			})
			// A positional merge can retain another positional merge as one
			// complete slot. Its nested QOV snapshot carries the next level's leg
			// table, so recurse with the already-composed object path. This is the
			// exact two-level lineage a 4+ table FlatMap chain needs; declining here
			// leaves only the immediate E/P windows and every buried D/C read later
			// falls through to the first same-named column.
			nestedPaths := make([][]int, len(nestedExact.fields))
			for nestedOrdinal := range nestedPaths {
				nestedPaths[nestedOrdinal] = make([]int, 0, len(objectPath)+1)
				nestedPaths[nestedOrdinal] = append(nestedPaths[nestedOrdinal], objectPath...)
				nestedPaths[nestedOrdinal] = append(nestedPaths[nestedOrdinal], nestedOrdinal)
			}
			buried, err := buriedOrdinalOutputWindowsFromLayout(
				nestedLayout, nestedExact, nestedPaths, nullSupplying)
			if err != nil {
				return nil, err
			}
			out = append(out, buried...)
		default:
			return nil, resolutionError(LayoutInvalidWindow, path, "buried source has an invalid leg kind")
		}
	}
	return out, nil
}
