package values

import (
	"fmt"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
)

// QuantifiedObjectBinder resolves an exact QOV to its whole runtime object.
// The boolean distinguishes an absent binding from a present SQL NULL.
type QuantifiedObjectBinder interface {
	GetQuantifiedBinding(QuantifiedObjectValue) (value any, present bool, err error)
}

// ExplicitNullQuantifiedObjectBinder is the optional proof carried by a binder
// that can intentionally bind SQL NULL for a statically non-nullable edge. The
// only current producer is FirstOrDefault: its empty arm is represented by an
// exact physical row shell plus explicit absence, rather than by changing the
// child QOV's declared type. Ordinary binders need not implement this; a nil
// delegated non-nullable QOV remains a loud nullability mismatch without this
// positive proof.
type ExplicitNullQuantifiedObjectBinder interface {
	IsExplicitNullQuantifiedBinding(QuantifiedObjectValue) (bool, error)
}

// WindowMatch records the row-local match state of one null-supplying object.
// Ordinarily Source names a retained source window. A row-producing operator
// that fabricates an exact physical carrier for a missing whole object (for
// example FirstOrDefault's empty arm) records the layout's current carrier
// instead. NewWindowMatchPresence snapshots and validates these mutable inputs.
type WindowMatch struct {
	Source  QuantifiedObjectValue
	Matched bool
}

// WindowMatchPresence is the immutable, values-owned match-state view consumed
// by an ordinal binder. The marker is not admission: consumers exact-recognize
// the private concrete before reading it.
type WindowMatchPresence interface {
	MatchState(QuantifiedObjectValue) (matched bool, known bool)
	isWindowMatchPresenceView()
}

type windowMatchPresence struct {
	bySource map[CorrelationIdentifier]windowMatchState
}

type windowMatchState struct {
	source  *quantifiedObjectValue
	matched bool
}

func (*windowMatchPresence) isWindowMatchPresenceView() {}

// NewWindowMatchPresence constructs immutable per-row match state. One
// correlation may occur only once and with one exact type.
func NewWindowMatchPresence(matches []WindowMatch) (WindowMatchPresence, error) {
	presence := &windowMatchPresence{bySource: make(map[CorrelationIdentifier]windowMatchState, len(matches))}
	for i := range matches {
		path := "layout.presence[" + uitoa(uint64(i)) + "]"
		source, err := exactLayoutQOV(matches[i].Source, path+".source")
		if err != nil {
			return nil, err
		}
		if source.correlation.isCurrent() {
			return nil, resolutionError(CorrelationKindMismatch, path+".source", "current cannot be a window source")
		}
		if previous, exists := presence.bySource[source.correlation]; exists {
			if exactTypesEqual(previous.source.flowed, source.flowed) {
				return nil, resolutionError(LayoutDuplicateSource, path+".source", "source match state is duplicated")
			}
			return nil, resolutionError(CorrelationTypeConflict, path+".source", "one correlation has conflicting match-state types")
		}
		presence.bySource[source.correlation] = windowMatchState{source: source, matched: matches[i].Matched}
	}
	return presence, nil
}

// NewOrdinalCarrierMatchPresence records whether layout's complete current
// object exists on one physical row. The carrier itself remains an exact,
// correctly-sized ordinal shell even when unmatched; the marker is what keeps
// that SQL NULL object distinct from a matched record whose every field is
// independently SQL NULL.
func NewOrdinalCarrierMatchPresence(layout OrdinalLayout, matched bool) (WindowMatchPresence, error) {
	exact, ok := exactOrdinalLayout(layout)
	if !ok {
		return nil, resolutionError(LayoutForeignValue, "layout", "layout is not a values-owned exact layout")
	}
	return &windowMatchPresence{bySource: map[CorrelationIdentifier]windowMatchState{
		exact.carrier.correlation: {source: exact.carrier, matched: matched},
	}}, nil
}

// OrdinalCarrierMatchState reads the optional whole-current-object state after
// exact-recognizing both inputs. Absence of a carrier marker is not an error:
// ordinary rows predate no match decision and are therefore treated as
// present by their executor owner. A hostile presence implementation is never
// invoked through its public interface.
func OrdinalCarrierMatchState(
	layout OrdinalLayout,
	presence WindowMatchPresence,
) (matched bool, known bool, err error) {
	exactLayout, ok := exactOrdinalLayout(layout)
	if !ok {
		return false, false, resolutionError(LayoutForeignValue, "layout", "layout is not a values-owned exact layout")
	}
	if presence == nil {
		return false, false, nil
	}
	exactPresence, ok := exactWindowMatchPresence(presence)
	if !ok {
		return false, false, resolutionError(LayoutForeignValue, "layout.presence", "match presence is not values-owned")
	}
	state, exists := exactPresence.bySource[exactLayout.carrier.correlation]
	if !exists {
		return false, false, nil
	}
	if !exactTypesEqual(state.source.flowed, exactLayout.carrier.flowed) {
		return false, false, resolutionError(CorrelationTypeConflict, "layout.presence", "current-carrier match state has a different exact type")
	}
	return state.matched, true, nil
}

// OrdinalWindowMatchState returns the exact per-row match state of one
// null-supplying source window. Both layout and presence are values-owned and
// exact-recognized; missing state is a malformed physical row, never an
// invitation to infer absence from all-NULL field values.
func OrdinalWindowMatchState(
	layout OrdinalLayout,
	presence WindowMatchPresence,
	source QuantifiedObjectValue,
) (bool, error) {
	exactLayout, ok := exactOrdinalLayout(layout)
	if !ok {
		return false, resolutionError(LayoutForeignValue, "layout", "layout is not a values-owned exact layout")
	}
	exactSource, err := exactLayoutQOV(source, "layout.source")
	if err != nil {
		return false, err
	}
	index, exists := exactLayout.bySource[exactSource.correlation]
	if !exists {
		return false, resolutionError(LayoutSourceNotProvided, "layout.source", "layout does not provide this source")
	}
	window := &exactLayout.windows[index]
	if !exactTypesEqual(window.source.flowed, exactSource.flowed) {
		return false, exactTypeConflict("layout.source", "provided correlation", exactSource.correlation, window.source.flowed, exactSource.flowed)
	}
	if !window.nullSupplying {
		return false, resolutionError(LayoutInvalidWindow, "layout.source", "match state requested for a non-null-supplying window")
	}
	if presence == nil {
		return false, resolutionError(LayoutPresenceMissing, "layout.presence", "null-supplying window has no row match state")
	}
	exactPresence, ok := exactWindowMatchPresence(presence)
	if !ok {
		return false, resolutionError(LayoutForeignValue, "layout.presence", "match presence is not values-owned")
	}
	state, exists := exactPresence.bySource[exactSource.correlation]
	if !exists {
		return false, resolutionError(LayoutPresenceMissing, "layout.presence", "null-supplying window match state is unknown")
	}
	if !exactTypesEqual(state.source.flowed, exactSource.flowed) {
		return false, exactTypeConflict("layout.presence", "window match state", exactSource.correlation, state.source.flowed, exactSource.flowed)
	}
	return state.matched, nil
}

// NewWindowMatchPresenceFromCorrelations snapshots the per-row match state of
// every null-supplying window in layout from an existing leg/edge binder. A
// present nil binding is unmatched; a present non-nil row is matched (including
// a row whose every field is SQL NULL). Missing required bindings fail loudly.
func NewWindowMatchPresenceFromCorrelations(layout OrdinalLayout, bindings CorrelationBinder) (WindowMatchPresence, error) {
	exact, ok := exactOrdinalLayout(layout)
	if !ok {
		return nil, resolutionError(LayoutForeignValue, "layout", "layout is not a values-owned exact layout")
	}
	matches := make([]WindowMatch, 0, len(exact.windows))
	for i := range exact.windows {
		window := &exact.windows[i]
		if !window.nullSupplying {
			continue
		}
		if bindings == nil {
			return nil, resolutionError(LayoutPresenceMissing, "layout.window["+uitoa(uint64(i))+"].source", "null-supplying source has no correlation binder")
		}
		value, present := bindings.GetCorrelationBinding(window.source.correlation)
		if !present {
			return nil, resolutionError(LayoutPresenceMissing, "layout.window["+uitoa(uint64(i))+"].source", "null-supplying source binding is absent")
		}
		matches = append(matches, WindowMatch{Source: window.source, Matched: value != nil})
	}
	if len(matches) == 0 {
		return nil, nil
	}
	return NewWindowMatchPresence(matches)
}

func (p *windowMatchPresence) MatchState(source QuantifiedObjectValue) (bool, bool) {
	exact, err := exactLayoutQOV(source, "layout.presence.source")
	if err != nil || p == nil {
		return false, false
	}
	state, exists := p.bySource[exact.correlation]
	if !exists || !exactTypesEqual(state.source.flowed, exact.flowed) {
		return false, false
	}
	return state.matched, true
}

func exactWindowMatchPresence(view WindowMatchPresence) (*windowMatchPresence, bool) {
	concrete, ok := view.(*windowMatchPresence)
	return concrete, ok && concrete != nil && concrete.bySource != nil
}

// ordinalObjectBinder is a row-local materialization of one immutable layout.
// All source windows are resolved once at construction; lookup is then exact
// QOV validation plus an O(1) map read.
type ordinalObjectBinder struct {
	layout                *ordinalLayout
	current               any
	currentExplicitAbsent bool
	bindings              map[CorrelationIdentifier]ordinalObjectBinding
	base                  QuantifiedObjectBinder
}

type ordinalObjectBinding struct {
	source         *quantifiedObjectValue
	value          any
	explicitAbsent bool
}

// NewOrdinalObjectBinder materializes the current carrier and every local
// source window for one row. A record carrier is an OrdinalRow (or nil only for
// a nullable current record); a scalar carrier is the scalar datum itself.
// External/edge bindings may be delegated to base.
func NewOrdinalObjectBinder(
	layout OrdinalLayout,
	carrier any,
	presence WindowMatchPresence,
	base QuantifiedObjectBinder,
) (QuantifiedObjectBinder, error) {
	exactLayout, ok := exactOrdinalLayout(layout)
	if !ok {
		return nil, resolutionError(LayoutForeignValue, "layout", "layout is not a values-owned exact layout")
	}

	var record OrdinalRow
	switch exactLayout.carrierKind {
	case OrdinalCarrierRecord:
		if carrier != nil {
			var recordOK bool
			record, recordOK = carrier.(OrdinalRow)
			if !recordOK {
				return nil, resolutionError(LayoutRuntimeShape, "layout.carrier", fmt.Sprintf("record carrier has runtime type %T", carrier))
			}
		} else if !exactLayout.carrier.flowed.nullable {
			return nil, resolutionError(LayoutNullabilityMismatch, "layout.carrier", "non-nullable record carrier is SQL NULL")
		}
	case OrdinalCarrierScalar:
		if _, isRow := carrier.(OrdinalRow); isRow {
			return nil, resolutionError(LayoutRuntimeShape, "layout.carrier", "scalar layout received an ordinal row")
		}
		if carrier == nil && !exactLayout.carrier.flowed.nullable {
			return nil, resolutionError(LayoutNullabilityMismatch, "layout.carrier", "non-nullable scalar carrier is SQL NULL")
		}
	default:
		return nil, resolutionError(LayoutForeignValue, "layout.carrier", "layout carrier kind is invalid")
	}

	var exactPresence *windowMatchPresence
	if presence != nil {
		var admitted bool
		exactPresence, admitted = exactWindowMatchPresence(presence)
		if !admitted {
			return nil, resolutionError(LayoutForeignValue, "layout.presence", "match presence is not values-owned")
		}
	}

	carrierMatched, carrierMatchKnown, err := ordinalCarrierMatchStateExact(exactLayout, exactPresence)
	if err != nil {
		return nil, err
	}
	current := carrier
	currentExplicitAbsent := false
	if carrierMatchKnown && !carrierMatched {
		// The non-nil carrier is the exact physical shape shell. Its whole SQL
		// object is nevertheless NULL; field access will also resolve NULL from
		// the shell's nil slots. Keeping the two facts separate distinguishes
		// this state from a matched all-NULL record.
		current = nil
		currentExplicitAbsent = true
	}

	binder := &ordinalObjectBinder{
		layout:                exactLayout,
		current:               current,
		currentExplicitAbsent: currentExplicitAbsent,
		bindings:              make(map[CorrelationIdentifier]ordinalObjectBinding, len(exactLayout.windows)),
		base:                  base,
	}
	for i := range exactLayout.windows {
		window := &exactLayout.windows[i]
		path := "layout.window[" + uitoa(uint64(i)) + "]"
		if carrierMatchKnown && !carrierMatched {
			// No retained source can exist inside an absent whole carrier. The
			// carrier marker is stronger than each individual window marker and
			// lets a fabricated default stay current-only in meaning even when it
			// reuses a pass-through child's richer physical layout.
			binder.bindings[window.source.correlation] = ordinalObjectBinding{
				source: window.source, explicitAbsent: true,
			}
			continue
		}
		if window.nullSupplying {
			if exactPresence == nil {
				return nil, resolutionError(LayoutPresenceMissing, path, "null-supplying window has no row match state")
			}
			state, exists := exactPresence.bySource[window.source.correlation]
			if !exists || !exactTypesEqual(state.source.flowed, window.source.flowed) {
				return nil, resolutionError(LayoutPresenceMissing, path, "null-supplying window match state is unknown")
			}
			if !state.matched {
				binder.bindings[window.source.correlation] = ordinalObjectBinding{
					source: window.source, explicitAbsent: true,
				}
				continue
			}
		}
		if record == nil {
			return nil, resolutionError(LayoutRuntimeShape, path, "matched source window has a NULL carrier")
		}
		var value any
		var err error
		if window.objectPath != nil {
			value, err = readOrdinalLayoutPath(record, exactLayout.carrier.flowed, window.objectPath)
		} else {
			value, err = materializeFieldWindow(record, exactLayout.carrier.flowed, window)
		}
		if err != nil {
			return nil, err
		}
		if value == nil && !window.source.flowed.nullable {
			return nil, resolutionError(LayoutNullabilityMismatch, path, "non-nullable source window resolved to SQL NULL")
		}
		binder.bindings[window.source.correlation] = ordinalObjectBinding{source: window.source, value: value}
	}
	if exactPresence != nil {
		for correlation := range exactPresence.bySource {
			if correlation == exactLayout.carrier.correlation {
				continue
			}
			index, exists := exactLayout.bySource[correlation]
			if !exists || !exactLayout.windows[index].nullSupplying {
				return nil, resolutionError(LayoutInvalidWindow, "layout.presence", "match state does not name a null-supplying layout window")
			}
		}
	}
	return binder, nil
}

func ordinalCarrierMatchStateExact(
	layout *ordinalLayout,
	presence *windowMatchPresence,
) (matched bool, known bool, err error) {
	if layout == nil || presence == nil {
		return false, false, nil
	}
	state, exists := presence.bySource[layout.carrier.correlation]
	if !exists {
		return false, false, nil
	}
	if !exactTypesEqual(state.source.flowed, layout.carrier.flowed) {
		return false, false, resolutionError(CorrelationTypeConflict, "layout.presence", "current-carrier match state has a different exact type")
	}
	return state.matched, true, nil
}

// NewRequiredOrdinalObjectBinder constructs the runtime binder only after the
// planning-time origin manifest proves that the selected layout provides
// exactly its local window sources and that every declared edge has one
// runtime whole-object binding. External origins continue through base.
func NewRequiredOrdinalObjectBinder(
	layout OrdinalLayout,
	carrier any,
	presence WindowMatchPresence,
	required RequiredBindings,
	edgeBindings []TypedEdgeBinding,
	base QuantifiedObjectBinder,
) (QuantifiedObjectBinder, error) {
	exactRequired, ok := exactRequiredBindings(required)
	if !ok {
		return nil, resolutionError(LayoutForeignValue, "binding.required", "required bindings are not values-owned")
	}
	satisfied, err := LayoutSatisfies(layout, exactRequired)
	if err != nil {
		return nil, err
	}
	if !satisfied {
		return nil, resolutionError(LayoutSourceNotProvided, "binding.required", "selected layout does not provide every required source")
	}
	edgeBinder := &requiredEdgeBinder{
		base:     base,
		bindings: make(map[CorrelationIdentifier]ordinalObjectBinding, len(edgeBindings)),
	}
	for i, view := range edgeBindings {
		binding, admitted := exactTypedEdgeBinding(view)
		if !admitted {
			return nil, resolutionError(CorrelationForeignValue, fmt.Sprintf("binding.edge[%d]", i), "edge binding is not values-owned")
		}
		qov := binding.declaration.qov
		declared, exists := exactRequired.edges[qov.correlation]
		if !exists {
			return nil, resolutionError(LayoutInvalidWindow, fmt.Sprintf("binding.edge[%d]", i), "runtime edge was not declared by this phase")
		}
		if !exactTypesEqual(declared.flowed, qov.flowed) {
			return nil, resolutionError(CorrelationTypeConflict, fmt.Sprintf("binding.edge[%d]", i), "runtime edge type disagrees with its phase declaration")
		}
		if _, duplicate := edgeBinder.bindings[qov.correlation]; duplicate {
			return nil, resolutionError(LayoutInvalidWindow, fmt.Sprintf("binding.edge[%d]", i), "runtime edge is duplicated")
		}
		edgeBinder.bindings[qov.correlation] = ordinalObjectBinding{source: qov, value: binding.wholeObject}
	}
	if len(edgeBinder.bindings) != len(exactRequired.edges) {
		return nil, resolutionError(UnboundCorrelation, "binding.edge", "one or more declared edges have no runtime whole object")
	}
	return NewOrdinalObjectBinder(layout, carrier, presence, edgeBinder)
}

type requiredEdgeBinder struct {
	base     QuantifiedObjectBinder
	bindings map[CorrelationIdentifier]ordinalObjectBinding
}

func (b *requiredEdgeBinder) GetQuantifiedBinding(view QuantifiedObjectValue) (any, bool, error) {
	exact, err := exactLayoutQOV(view, "binding.edge.qov")
	if err != nil {
		return nil, false, err
	}
	if binding, exists := b.bindings[exact.correlation]; exists {
		if !exactRowShapesAgree(binding.source.flowed, exact.flowed) {
			return nil, false, exactTypeConflict("binding.edge.qov", "edge lookup", exact.correlation, binding.source.flowed, exact.flowed)
		}
		return binding.value, true, nil
	}
	if b.base != nil {
		return b.base.GetQuantifiedBinding(view)
	}
	return nil, false, nil
}

// IsExplicitNullQuantifiedBinding preserves the row-local absence proof for an
// exact carrier or retained window. A FirstOrDefault empty arm deliberately
// keeps a non-nil typed shell for physical validation while its whole SQL
// object is NULL; consumers that copy the binding into another exact context
// must be able to carry that fact without treating an arbitrary non-nullable
// nil as valid. A matched all-NULL record has explicitAbsent=false.
func (b *ordinalObjectBinder) IsExplicitNullQuantifiedBinding(view QuantifiedObjectValue) (bool, error) {
	qov, err := exactLayoutQOV(view, "binding.qov")
	if err != nil {
		return false, err
	}
	if qov.correlation.isCurrent() {
		if qov != b.layout.carrier {
			return false, resolutionError(LayoutCarrierMismatch, "binding.qov", "current QOV is not this layout's exact carrier handle")
		}
		return b.currentExplicitAbsent, nil
	}
	if binding, exists := b.bindings[qov.correlation]; exists {
		if !exactRowShapesAgree(binding.source.flowed, qov.flowed) {
			return false, exactTypeConflict("binding.qov", "source absence lookup", qov.correlation, binding.source.flowed, qov.flowed)
		}
		return binding.explicitAbsent, nil
	}
	if proof, ok := b.base.(ExplicitNullQuantifiedObjectBinder); ok {
		return proof.IsExplicitNullQuantifiedBinding(view)
	}
	return false, nil
}

func (b *ordinalObjectBinder) GetQuantifiedBinding(view QuantifiedObjectValue) (any, bool, error) {
	qov, err := exactLayoutQOV(view, "binding.qov")
	if err != nil {
		return nil, false, err
	}
	if qov.correlation.isCurrent() {
		if qov != b.layout.carrier {
			return nil, false, resolutionError(LayoutCarrierMismatch, "binding.qov", "current QOV is not this layout's exact carrier handle")
		}
		return b.current, true, nil
	}
	if binding, exists := b.bindings[qov.correlation]; exists {
		if !exactRowShapesAgree(binding.source.flowed, qov.flowed) {
			return nil, false, exactTypeConflict("binding.qov", "source binding", qov.correlation, binding.source.flowed, qov.flowed)
		}
		return binding.value, true, nil
	}
	if b.base != nil {
		value, present, baseErr := b.base.GetQuantifiedBinding(view)
		if baseErr != nil || !present {
			return value, present, baseErr
		}
		if value == nil && !qov.flowed.nullable {
			explicit := false
			if proof, ok := b.base.(ExplicitNullQuantifiedObjectBinder); ok {
				explicit, baseErr = proof.IsExplicitNullQuantifiedBinding(view)
				if baseErr != nil {
					return nil, false, baseErr
				}
			}
			if !explicit {
				return nil, false, resolutionError(LayoutNullabilityMismatch, "binding.qov", "non-nullable delegated QOV is SQL NULL")
			}
		}
		return value, true, nil
	}
	return nil, false, nil
}

type materializedOrdinalWindow struct {
	names []string
	slots []any
}

func (w *materializedOrdinalWindow) Get(ordinal int) (any, bool) {
	if w == nil || ordinal < 0 || ordinal >= len(w.slots) {
		return nil, false
	}
	return w.slots[ordinal], true
}

func (w *materializedOrdinalWindow) TypeNames() []string {
	if w == nil {
		return nil
	}
	return append([]string(nil), w.names...)
}

func materializeFieldWindow(record OrdinalRow, root *exactType, window *ordinalWindow) (OrdinalRow, error) {
	slots := make([]any, len(window.fieldPaths))
	names := make([]string, len(window.fieldPaths))
	for i := range window.fieldPaths {
		value, err := readOrdinalLayoutPath(record, root, window.fieldPaths[i])
		if err != nil {
			return nil, err
		}
		slots[i] = value
		names[i] = window.source.flowed.fields[i].name
	}
	return &materializedOrdinalWindow{names: names, slots: slots}, nil
}

func readOrdinalLayoutPath(carrier any, root *exactType, path []int) (any, error) {
	current := carrier
	expected := root
	for depth, ordinal := range path {
		if current == nil {
			return nil, nil
		}
		if expected == nil || expected.code != TypeCodeRecord || expected.anyRecord || ordinal < 0 || ordinal >= len(expected.fields) {
			return nil, resolutionError(LayoutRuntimeShape, fmt.Sprintf("layout.path[%d]", depth), "captured type cannot address the ordinal")
		}
		switch typed := current.(type) {
		case OrdinalRow:
			value, exists := typed.Get(ordinal)
			if !exists {
				return nil, resolutionError(LayoutRuntimeShape, fmt.Sprintf("layout.path[%d]", depth), "ordinal row is shorter than the layout path")
			}
			current = value
		case proto.Message:
			value, err := readProtoOrdinal(typed.ProtoReflect(), expected, ordinal, depth)
			if err != nil {
				return nil, err
			}
			current = value
		case protoreflect.Message:
			value, err := readProtoOrdinal(typed, expected, ordinal, depth)
			if err != nil {
				return nil, err
			}
			current = value
		default:
			return nil, resolutionError(LayoutRuntimeShape, fmt.Sprintf("layout.path[%d]", depth), fmt.Sprintf("cannot descend through runtime type %T", current))
		}
		expected = expected.fields[ordinal].typ
	}
	return current, nil
}
