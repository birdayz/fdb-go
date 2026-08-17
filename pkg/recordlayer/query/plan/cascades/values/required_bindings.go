package values

import "fmt"

// BindingOrigin classifies one exact QOV used by a physical evaluation phase.
// Origins are disjoint: a correlation may never be both an edge and a window,
// or both an external and an edge.
type BindingOrigin uint8

const (
	BindingOriginInvalid BindingOrigin = iota
	BindingOriginCurrent
	BindingOriginEdge
	BindingOriginWindow
	BindingOriginExternal
)

// TypedEdgeDeclaration is the immutable planning-time declaration of one
// complete object flowing over a quantifier edge.
type TypedEdgeDeclaration interface {
	QOV() QuantifiedObjectValue
	isTypedEdgeDeclarationView()
}

// TypedExternalDeclaration declares one exact correlation delegated to the
// surrounding evaluation context.
type TypedExternalDeclaration interface {
	QOV() QuantifiedObjectValue
	isTypedExternalDeclarationView()
}

// TypedEdgeBinding combines an admitted edge declaration with the whole
// runtime object carried by that edge. Present SQL NULL is represented by a
// nil object and is legal only for a nullable QOV.
type TypedEdgeBinding interface {
	Declaration() TypedEdgeDeclaration
	isTypedEdgeBindingView()
}

type typedBindingDeclaration struct {
	qov *quantifiedObjectValue
}

type (
	typedEdgeDeclaration     struct{ typedBindingDeclaration }
	typedExternalDeclaration struct{ typedBindingDeclaration }
)

type typedEdgeBinding struct {
	declaration *typedEdgeDeclaration
	wholeObject any
}

func (*typedEdgeDeclaration) isTypedEdgeDeclarationView()         {}
func (*typedExternalDeclaration) isTypedExternalDeclarationView() {}
func (*typedEdgeBinding) isTypedEdgeBindingView()                 {}

func (d *typedEdgeDeclaration) QOV() QuantifiedObjectValue {
	if d == nil {
		return nil
	}
	return d.qov
}

func (d *typedExternalDeclaration) QOV() QuantifiedObjectValue {
	if d == nil {
		return nil
	}
	return d.qov
}

func (b *typedEdgeBinding) Declaration() TypedEdgeDeclaration {
	if b == nil {
		return nil
	}
	return b.declaration
}

// NewTypedEdgeDeclaration snapshots one exact non-current edge QOV.
func NewTypedEdgeDeclaration(qov QuantifiedObjectValue) (TypedEdgeDeclaration, error) {
	exact, err := exactDeclaredQOV(qov, "binding.edge")
	if err != nil {
		return nil, err
	}
	return &typedEdgeDeclaration{typedBindingDeclaration{qov: exact}}, nil
}

// NewTypedExternalDeclaration snapshots one exact non-current external QOV.
func NewTypedExternalDeclaration(qov QuantifiedObjectValue) (TypedExternalDeclaration, error) {
	exact, err := exactDeclaredQOV(qov, "binding.external")
	if err != nil {
		return nil, err
	}
	return &typedExternalDeclaration{typedBindingDeclaration{qov: exact}}, nil
}

func exactDeclaredQOV(qov QuantifiedObjectValue, path string) (*quantifiedObjectValue, error) {
	exact, err := exactLayoutQOV(qov, path)
	if err != nil {
		return nil, err
	}
	if exact.correlation.isCurrent() {
		return nil, resolutionError(CorrelationKindMismatch, path, "edge/external declaration cannot use current")
	}
	return exact, nil
}

// BindTypedEdge validates a runtime whole-object binding without exposing it
// through the planning-time declaration view.
func BindTypedEdge(declaration TypedEdgeDeclaration, wholeObject any) (TypedEdgeBinding, error) {
	exact, ok := declaration.(*typedEdgeDeclaration)
	if !ok || exact == nil || exact.qov == nil || exact.qov.flowed == nil {
		return nil, resolutionError(CorrelationForeignValue, "binding.edge", "edge declaration is not values-owned")
	}
	if wholeObject == nil && !exact.qov.flowed.nullable {
		return nil, resolutionError(LayoutNullabilityMismatch, "binding.edge", "non-nullable edge object is SQL NULL")
	}
	return &typedEdgeBinding{declaration: exact, wholeObject: wholeObject}, nil
}

func exactTypedEdgeBinding(binding TypedEdgeBinding) (*typedEdgeBinding, bool) {
	exact, ok := binding.(*typedEdgeBinding)
	return exact, ok && exact != nil && exact.declaration != nil && exact.declaration.qov != nil
}

// RequiredBindings is the immutable binding-origin manifest for one physical
// evaluation phase. WindowSources returns a defensive slice of immutable QOV
// views.
type RequiredBindings interface {
	WindowSources() []QuantifiedObjectValue
	ValidateAgainst(OrdinalLayout) (bool, error)
	isRequiredBindingsView()
}

type requiredBindings struct {
	current  *quantifiedObjectValue
	edges    map[CorrelationIdentifier]*quantifiedObjectValue
	external map[CorrelationIdentifier]*quantifiedObjectValue
	windows  map[CorrelationIdentifier]*quantifiedObjectValue
	ordered  []*quantifiedObjectValue
}

func (*requiredBindings) isRequiredBindingsView() {}

func (r *requiredBindings) WindowSources() []QuantifiedObjectValue {
	if r == nil || len(r.ordered) == 0 {
		return nil
	}
	result := make([]QuantifiedObjectValue, len(r.ordered))
	for i := range r.ordered {
		result[i] = r.ordered[i]
	}
	return result
}

// CollectRequiredBindings classifies every exact QOV root in one phase. The
// roots slice is the union of the phase result, conditions, properties and
// SARG values; callers must not collect each lane independently.
func CollectRequiredBindings(
	current QuantifiedObjectValue,
	roots []Value,
	edges []TypedEdgeDeclaration,
	externals []TypedExternalDeclaration,
) (result RequiredBindings, err error) {
	exactCurrent, err := exactLayoutQOV(current, "binding.current")
	if err != nil {
		return nil, err
	}
	if !exactCurrent.correlation.isCurrent() {
		return nil, resolutionError(CorrelationKindMismatch, "binding.current", "phase current is not tagged current")
	}
	required := &requiredBindings{
		current:  exactCurrent,
		edges:    make(map[CorrelationIdentifier]*quantifiedObjectValue, len(edges)),
		external: make(map[CorrelationIdentifier]*quantifiedObjectValue, len(externals)),
		windows:  make(map[CorrelationIdentifier]*quantifiedObjectValue),
	}
	for i, declaration := range edges {
		exact, ok := declaration.(*typedEdgeDeclaration)
		if !ok || exact == nil || exact.qov == nil {
			return nil, resolutionError(CorrelationForeignValue, fmt.Sprintf("binding.edge[%d]", i), "edge declaration is not values-owned")
		}
		if err := addDeclaredOrigin(required, exact.qov, BindingOriginEdge, fmt.Sprintf("binding.edge[%d]", i)); err != nil {
			return nil, err
		}
	}
	for i, declaration := range externals {
		exact, ok := declaration.(*typedExternalDeclaration)
		if !ok || exact == nil || exact.qov == nil {
			return nil, resolutionError(CorrelationForeignValue, fmt.Sprintf("binding.external[%d]", i), "external declaration is not values-owned")
		}
		if err := addDeclaredOrigin(required, exact.qov, BindingOriginExternal, fmt.Sprintf("binding.external[%d]", i)); err != nil {
			return nil, err
		}
	}

	// A hostile open Value must become a stable error rather than a planner
	// panic. Owner admission will ultimately make this recovery unreachable,
	// but the public boundary remains total during the migration.
	deferredPanicPath := ""
	defer func() {
		if recovered := recover(); recovered != nil {
			result = nil
			err = resolutionError(LayoutForeignValue, deferredPanicPath, fmt.Sprintf("Value traversal panicked: %v", recovered))
		}
	}()
	for rootIndex, root := range roots {
		if root == nil {
			return nil, resolutionError(LayoutForeignValue, fmt.Sprintf("binding.value[%d]", rootIndex), "phase Value is nil")
		}
		deferredPanicPath = fmt.Sprintf("binding.value[%d]", rootIndex)
		var visitErr error
		WalkValue(root, func(value Value) bool {
			qov, ok := value.(*quantifiedObjectValue)
			if !ok {
				return true
			}
			if qov == nil || qov.flowed == nil || qov.correlation.IsZero() {
				visitErr = resolutionError(CorrelationForeignValue, deferredPanicPath, "phase contains a malformed QOV")
				return false
			}
			visitErr = required.classify(qov, deferredPanicPath)
			return visitErr == nil
		})
		if visitErr != nil {
			return nil, visitErr
		}
	}
	return required, nil
}

func addDeclaredOrigin(r *requiredBindings, qov *quantifiedObjectValue, origin BindingOrigin, path string) error {
	if qov == nil || qov.flowed == nil || qov.correlation.IsZero() || qov.correlation.isCurrent() {
		return resolutionError(CorrelationKindMismatch, path, "declared binding must be a non-current exact QOV")
	}
	if previous, exists := r.edges[qov.correlation]; exists {
		if !exactTypesEqual(previous.flowed, qov.flowed) {
			return resolutionError(CorrelationTypeConflict, path, "edge correlation has conflicting exact types")
		}
		return resolutionError(LayoutInvalidWindow, path, "binding origin is duplicated")
	}
	if previous, exists := r.external[qov.correlation]; exists {
		if !exactTypesEqual(previous.flowed, qov.flowed) {
			return resolutionError(CorrelationTypeConflict, path, "external correlation has conflicting exact types")
		}
		return resolutionError(LayoutInvalidWindow, path, "binding origin is duplicated")
	}
	if origin == BindingOriginEdge {
		r.edges[qov.correlation] = qov
	} else {
		r.external[qov.correlation] = qov
	}
	return nil
}

func (r *requiredBindings) classify(qov *quantifiedObjectValue, path string) error {
	if qov.correlation.isCurrent() {
		if qov != r.current {
			return resolutionError(LayoutCarrierMismatch, path, "phase contains another owner's current handle")
		}
		return nil
	}
	if declared, exists := r.edges[qov.correlation]; exists {
		if !exactTypesEqual(declared.flowed, qov.flowed) {
			return resolutionError(CorrelationTypeConflict, path, "edge use disagrees with its declared exact type")
		}
		return nil
	}
	if declared, exists := r.external[qov.correlation]; exists {
		if !exactTypesEqual(declared.flowed, qov.flowed) {
			return resolutionError(CorrelationTypeConflict, path, "external use disagrees with its declared exact type")
		}
		return nil
	}
	if previous, exists := r.windows[qov.correlation]; exists {
		if !exactTypesEqual(previous.flowed, qov.flowed) {
			return resolutionError(CorrelationTypeConflict, path, "window source correlation has conflicting exact types")
		}
		return nil
	}
	r.windows[qov.correlation] = qov
	r.ordered = append(r.ordered, qov)
	return nil
}

func exactRequiredBindings(view RequiredBindings) (*requiredBindings, bool) {
	exact, ok := view.(*requiredBindings)
	return exact, ok && exact != nil && exact.current != nil && exact.edges != nil && exact.external != nil && exact.windows != nil
}

// ValidateRequiredBindingsAdmission exact-recognizes a binding manifest
// without invoking methods on a hostile interface implementation.
func ValidateRequiredBindingsAdmission(view RequiredBindings) error {
	if _, ok := exactRequiredBindings(view); !ok {
		return resolutionError(LayoutForeignValue, "binding.required", "required bindings are not values-owned")
	}
	return nil
}

// LayoutSatisfies applies one immutable binding manifest to a candidate
// provided layout. Missing windows are a normal physical incompatibility;
// malformed/extra/colliding sources remain errors.
func LayoutSatisfies(layout OrdinalLayout, required RequiredBindings) (bool, error) {
	exactRequired, ok := exactRequiredBindings(required)
	if !ok {
		return false, resolutionError(LayoutForeignValue, "binding.required", "required bindings are not values-owned")
	}
	exactLayout, ok := exactOrdinalLayout(layout)
	if !ok {
		return false, resolutionError(LayoutForeignValue, "layout", "layout is not values-owned")
	}
	if exactLayout.carrier != exactRequired.current {
		return false, resolutionError(LayoutCarrierMismatch, "layout.carrier", "layout carrier is not the phase's exact current handle")
	}
	for correlation, qov := range exactRequired.windows {
		index, exists := exactLayout.bySource[correlation]
		if !exists {
			return false, nil
		}
		if !exactTypesEqual(exactLayout.windows[index].source.flowed, qov.flowed) {
			return false, resolutionError(CorrelationTypeConflict, "layout.window", "layout window type disagrees with the required source")
		}
	}
	for i := range exactLayout.windows {
		window := &exactLayout.windows[i]
		if _, exists := exactRequired.windows[window.source.correlation]; !exists {
			return false, resolutionError(LayoutInvalidWindow, fmt.Sprintf("layout.window[%d]", i), "layout provides an undeclared local source")
		}
		if _, collision := exactRequired.edges[window.source.correlation]; collision {
			return false, resolutionError(LayoutInvalidWindow, fmt.Sprintf("layout.window[%d]", i), "layout window collides with an edge origin")
		}
		if _, collision := exactRequired.external[window.source.correlation]; collision {
			return false, resolutionError(LayoutInvalidWindow, fmt.Sprintf("layout.window[%d]", i), "layout window collides with an external origin")
		}
	}
	return true, nil
}

func (r *requiredBindings) ValidateAgainst(layout OrdinalLayout) (bool, error) {
	return LayoutSatisfies(layout, r)
}
