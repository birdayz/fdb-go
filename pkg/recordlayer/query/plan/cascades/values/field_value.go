package values

import (
	"fmt"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
)

// FieldValue is the immutable read view of one completely resolved field
// access.  The concrete implementation is package-private so neither a zero
// value nor partially initialized path can enter a Value graph.
type FieldValue interface {
	Value
	ChildValue() Value
	Path() FieldPathView
	DisplayName() string
	ResultType() Type
	isFieldValueView()
}

// FieldPathView exposes only defensive/read-only path information.
type FieldPathView interface {
	Len() int
	Ordinals() []int
	Accessor(int) (ResolvedAccessorView, bool)
	// Transitional physical-addressing read views. They remain read-only while
	// RFC-232's OrdinalLayout migration removes OrdinalDomain/pin state from
	// logical Values.
	RootDomain() OrdinalDomain
	IsFrontierPinned() bool
	isFieldPathView()
}

// ResolvedAccessorView is one ordinal resolved against its enclosing exact
// record type.  DisplayName is diagnostic metadata only.
type ResolvedAccessorView interface {
	Ordinal() int
	DisplayName() (string, bool)
	FieldType() Type
	isResolvedAccessorView()
}

func (*fieldValue) isFieldValueView() {}
func (*fieldPath) isFieldPathView()   {}

type resolvedAccessorView struct {
	accessor *resolvedAccessor
}

func (*resolvedAccessorView) isResolvedAccessorView() {}

func (f *fieldValue) ChildValue() Value {
	if f == nil {
		return nil
	}
	return f.Child
}

func (f *fieldValue) Path() FieldPathView {
	if !isAdmittedFieldValue(f) {
		return nil
	}
	return f.Resolved
}

func (f *fieldValue) DisplayName() string {
	if f == nil {
		return ""
	}
	return f.Field
}

func (f *fieldValue) ResultType() Type {
	if f == nil || f.resultType == nil {
		return nil
	}
	return f.resultType.thaw()
}

func (p *fieldPath) Len() int {
	if p == nil {
		return 0
	}
	return len(p.Accessors)
}

func (p *fieldPath) Ordinals() []int {
	if p == nil {
		return nil
	}
	ordinals := make([]int, len(p.Accessors))
	for i := range p.Accessors {
		ordinals[i] = p.Accessors[i].Ordinal
	}
	return ordinals
}

func (p *fieldPath) Accessor(index int) (ResolvedAccessorView, bool) {
	if p == nil || index < 0 || index >= len(p.Accessors) {
		return nil, false
	}
	return &resolvedAccessorView{accessor: &p.Accessors[index]}, true
}

func (p *fieldPath) RootDomain() OrdinalDomain {
	if p == nil {
		return OrdinalDomain{}
	}
	return p.Domain
}

func (p *fieldPath) IsFrontierPinned() bool {
	return p != nil && p.FrontierPinned
}

func (a *resolvedAccessorView) Ordinal() int {
	if a == nil || a.accessor == nil {
		return -1
	}
	return a.accessor.Ordinal
}

func (a *resolvedAccessorView) DisplayName() (string, bool) {
	if a == nil || a.accessor == nil || a.accessor.Field == "" {
		return "", false
	}
	return a.accessor.Field, true
}

func (a *resolvedAccessorView) FieldType() Type {
	if a == nil || a.accessor == nil || a.accessor.fieldType == nil {
		return nil
	}
	return a.accessor.fieldType.thaw()
}

// FieldRequest is a sealed, exactly-one-step semantic access request.
type FieldRequest interface {
	isFieldRequest()
}

type fieldRequestKind uint8

const (
	fieldRequestByName fieldRequestKind = iota + 1
	fieldRequestByOrdinal
	fieldRequestByNameAndOrdinal
)

type fieldRequest struct {
	kind    fieldRequestKind
	name    string
	ordinal int
}

func (*fieldRequest) isFieldRequest() {}

// FieldByName constructs one semantic-name request.  It never splits dots.
func FieldByName(semanticName string) (FieldRequest, error) {
	if semanticName == "" {
		return nil, resolutionError(FieldInvalidRequest, "field.request", "field name is empty")
	}
	return &fieldRequest{kind: fieldRequestByName, name: semanticName}, nil
}

// FieldByOrdinal constructs one non-negative ordinal request.
func FieldByOrdinal(ordinal int) (FieldRequest, error) {
	if ordinal < 0 {
		return nil, resolutionError(FieldNegativeOrdinal, "field.request", "field ordinal is negative")
	}
	return &fieldRequest{kind: fieldRequestByOrdinal, ordinal: ordinal}, nil
}

// FieldByNameAndOrdinal constructs a request whose two authorities must agree.
func FieldByNameAndOrdinal(semanticName string, ordinal int) (FieldRequest, error) {
	if semanticName == "" {
		return nil, resolutionError(FieldInvalidRequest, "field.request", "field name is empty")
	}
	if ordinal < 0 {
		return nil, resolutionError(FieldNegativeOrdinal, "field.request", "field ordinal is negative")
	}
	return &fieldRequest{kind: fieldRequestByNameAndOrdinal, name: semanticName, ordinal: ordinal}, nil
}

// AsFieldValue exact-recognizes only the values-owned immutable concrete node.
// Embedded and nil-embedded interface impostors are rejected without invoking
// any of their methods.
func AsFieldValue(value Value) (FieldValue, bool) {
	field, ok := value.(*fieldValue)
	if !ok || !isAdmittedFieldValue(field) {
		return nil, false
	}
	return field, true
}

func isAdmittedFieldValue(field *fieldValue) bool {
	if field == nil || field.rootType == nil || field.resultType == nil ||
		field.Resolved == nil || len(field.Resolved.Accessors) == 0 {
		return false
	}
	child, ok := field.Child.(*quantifiedObjectValue)
	if !ok || child == nil || child.flowed == nil || !exactTypesEqual(child.flowed, field.rootType) {
		return false
	}
	for i := range field.Resolved.Accessors {
		accessor := &field.Resolved.Accessors[i]
		if accessor.Ordinal < 0 || accessor.fieldType == nil {
			return false
		}
	}
	return true
}

// ResolveFieldAccess resolves the complete request path atomically.  It
// returns Value because resolving through a record constructor may collapse to
// the constructor's selected child rather than allocating a FieldValue.
func ResolveFieldAccess(child Value, path []FieldRequest) (Value, error) {
	if child == nil {
		return nil, resolutionError(FieldNilChild, "field.child", "field child is nil")
	}
	if len(path) == 0 {
		return nil, resolutionError(FieldEmptyPath, "field.path", "field path is empty")
	}
	requests := make([]*fieldRequest, len(path))
	for i, request := range path {
		exact, ok := request.(*fieldRequest)
		if !ok || exact == nil || exact.kind < fieldRequestByName || exact.kind > fieldRequestByNameAndOrdinal {
			return nil, resolutionError(FieldInvalidRequest, fmt.Sprintf("field.path[%d]", i), "foreign or malformed field request")
		}
		requests[i] = &fieldRequest{kind: exact.kind, name: exact.name, ordinal: exact.ordinal}
	}
	return resolveFieldAccess(child, requests)
}

// ResolveFieldOrdinals is the compact purpose API for code which already owns
// a proven ordinal vector. It still validates the entire vector against the
// child's exact type before publishing a Value.
func ResolveFieldOrdinals(child Value, ordinals []int) (Value, error) {
	requests := make([]FieldRequest, len(ordinals))
	for i, ordinal := range ordinals {
		request, err := FieldByOrdinal(ordinal)
		if err != nil {
			return nil, err
		}
		requests[i] = request
	}
	return ResolveFieldAccess(child, requests)
}

// ResolveOrdinalSeedField constructs the one-field access used by ordinal
// join/gather seed machinery. Unlike an ordinary semantic field access, the
// root ordinal is final for the exact QOV layout captured here, so the path is
// stamped FrontierPinned together with the domain derived from that same
// immutable exact root. Callers cannot supply either marker independently.
//
// This is deliberately narrower than ResolveFieldOrdinals: a physical seed is
// always one top-level slot of one exact quantified object. Nested semantic
// access, record-constructor collapse, and already-built FieldValues must use
// the ordinary resolver and cannot acquire a seed contract accidentally.
func ResolveOrdinalSeedField(child Value, ordinal int) (Value, error) {
	qov, ok := child.(*quantifiedObjectValue)
	if !ok || qov == nil || qov.flowed == nil || qov.correlation.IsZero() {
		return nil, resolutionError(FieldUnsupportedChild, "field.seed.child", "ordinal seed child is not an exact quantified object")
	}
	resolved, err := ResolveFieldOrdinals(qov, []int{ordinal})
	if err != nil {
		return nil, err
	}
	field, ok := resolved.(*fieldValue)
	if !ok || !isAdmittedFieldValue(field) {
		return nil, resolutionError(FieldUnsupportedChild, "field.seed.child", "ordinal seed resolution did not produce an exact field")
	}

	pathCopy := *field.Resolved
	pathCopy.Accessors = append([]resolvedAccessor(nil), field.Resolved.Accessors...)
	pathCopy.FrontierPinned = true
	copy := *field
	copy.Resolved = &pathCopy
	return &copy, nil
}

// PinValueToExactFrontier marks every admitted field path rooted at carrier as
// a machinery-owned read of that exact physical frontier.  The pointer check is
// the authority: another current QOV with the same record shape represents a
// different producer phase and is left unchanged, as are named outer/source
// correlations.
//
// A materializing plan uses this after it has checked-reanchored an evaluation
// program onto its selected child's output carrier.  The pin tells the runtime
// evaluator to read the already-materialized flat row directly rather than
// routing the unpinned path through retained join-leg windows.  It is excluded
// from Value equality/hash/explain, so this changes only the evaluation
// contract, not memo identity.
func PinValueToExactFrontier(value Value, carrier QuantifiedObjectValue) (Value, error) {
	if value == nil {
		return nil, resolutionError(FieldUnsupportedChild, "field.pin.value", "value is nil")
	}
	exactCarrier, ok := carrier.(*quantifiedObjectValue)
	if !ok || exactCarrier == nil || exactCarrier.flowed == nil || !exactCarrier.correlation.isCurrent() {
		return nil, resolutionError(FieldUnsupportedChild, "field.pin.carrier", "carrier is not an exact current QOV")
	}

	var pin func(Value) (Value, error)
	pin = func(node Value) (Value, error) {
		if node == nil {
			return nil, resolutionError(RewriteNilReplacement, "field.pin.value", "value tree contains nil")
		}
		if field, isField := node.(*fieldValue); isField {
			if !isAdmittedFieldValue(field) {
				return nil, resolutionError(FieldUnsupportedChild, "field.pin.value", "value tree contains a malformed field")
			}
			if field.Child != exactCarrier || field.Resolved.FrontierPinned {
				return field, nil
			}
			pathCopy := *field.Resolved
			pathCopy.Accessors = append([]resolvedAccessor(nil), field.Resolved.Accessors...)
			pathCopy.FrontierPinned = true
			fieldCopy := *field
			fieldCopy.Resolved = &pathCopy
			return &fieldCopy, nil
		}

		children := node.Children()
		if len(children) == 0 {
			return node, nil
		}
		var rebuilt []Value
		for index, child := range children {
			pinned, err := pin(child)
			if err != nil {
				return nil, err
			}
			if pinned != child {
				if rebuilt == nil {
					rebuilt = append([]Value(nil), children...)
				}
				rebuilt[index] = pinned
			}
		}
		if rebuilt == nil {
			return node, nil
		}
		return withChildrenChecked(node, rebuilt)
	}
	return pin(value)
}

// ResolveOrdinalSeedAccess constructs a machinery-owned seed access whose
// first component is a top-level physical slot and whose remaining components
// are ordinary semantic descent requests. The root pin is derived here from
// the exact QOV and cannot be supplied independently; suffixes never acquire a
// second physical offset. This is the fused collection form used by lateral
// unnest over a nested record column.
func ResolveOrdinalSeedAccess(child Value, ordinal int, suffix []FieldRequest) (Value, error) {
	root, err := ResolveOrdinalSeedField(child, ordinal)
	if err != nil {
		return nil, err
	}
	if len(suffix) == 0 {
		return root, nil
	}
	return ResolveFieldAccess(root, suffix)
}

// resolveFieldOrdinalInDomain is the values-internal bridge for legacy
// physical-domain consumers while OrdinalLayout replaces OrdinalDomain. The
// semantic FV remains fully resolved/exact; the domain is non-semantic
// transitional metadata and is never exposed through FieldPathView.
func resolveFieldOrdinalInDomain(child Value, ordinal int, domain OrdinalDomain) (Value, error) {
	resolved, err := ResolveFieldOrdinals(child, []int{ordinal})
	if err != nil {
		return nil, err
	}
	field, ok := resolved.(*fieldValue)
	if !ok {
		return resolved, nil
	}
	pathCopy := *field.Resolved
	pathCopy.Domain = domain
	copy := *field
	copy.Resolved = &pathCopy
	return &copy, nil
}

// TryResolveFieldAccess converts only the RFC's documented optional-candidate
// resolution misses into an inapplicable result.
func TryResolveFieldAccess(child Value, path []FieldRequest) (Value, bool, error) {
	resolved, err := ResolveFieldAccess(child, path)
	if err == nil {
		return resolved, true, nil
	}
	var coded interface{ Code() ResolutionErrorCode }
	if !asResolutionCode(err, &coded) {
		return nil, false, err
	}
	switch coded.Code() {
	case FieldUnknownName, FieldAmbiguousName, FieldNonRecord, FieldOutOfRange:
		return nil, false, nil
	default:
		return nil, false, err
	}
}

func asResolutionCode(err error, target *interface{ Code() ResolutionErrorCode }) bool {
	for err != nil {
		if coded, ok := err.(interface{ Code() ResolutionErrorCode }); ok {
			*target = coded
			return true
		}
		unwrapper, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = unwrapper.Unwrap()
	}
	return false
}

func resolveFieldAccess(child Value, requests []*fieldRequest) (Value, error) {
	switch typed := child.(type) {
	case *quantifiedObjectValue:
		if typed == nil || typed.flowed == nil || typed.correlation.IsZero() {
			return nil, resolutionError(FieldUnsupportedChild, "field.child", "malformed quantified object child")
		}
		return resolveAgainstQOV(typed, nil, requests)
	case *fieldValue:
		if !isAdmittedFieldValue(typed) {
			return nil, resolutionError(FieldUnsupportedChild, "field.child", "malformed field child")
		}
		base := typed.Child.(*quantifiedObjectValue)
		prefix := make([]resolvedAccessor, len(typed.Resolved.Accessors))
		copy(prefix, typed.Resolved.Accessors)
		resolved, err := resolveAgainstQOV(base, prefix, requests)
		if err != nil || !typed.Resolved.FrontierPinned {
			return resolved, err
		}
		fused, ok := resolved.(*fieldValue)
		if !ok || !isAdmittedFieldValue(fused) {
			return nil, resolutionError(FieldUnsupportedChild, "field.child", "pinned suffix resolution did not produce an exact field")
		}
		pathCopy := *fused.Resolved
		pathCopy.Accessors = append([]resolvedAccessor(nil), fused.Resolved.Accessors...)
		pathCopy.FrontierPinned = true
		resultCopy := *fused
		resultCopy.Resolved = &pathCopy
		return &resultCopy, nil
	case *RecordConstructorValue:
		return resolveAgainstRecordConstructor(typed, requests)
	default:
		// Do not call an open Value method at this admission boundary.
		return nil, resolutionError(FieldUnsupportedChild, "field.child", "field child is not a QOV, admitted FieldValue, or record constructor")
	}
}

func resolveAgainstQOV(
	qov *quantifiedObjectValue,
	prefix []resolvedAccessor,
	requests []*fieldRequest,
) (Value, error) {
	root := qov.flowed
	current := root
	nullable := root.nullable
	accessors := make([]resolvedAccessor, 0, len(prefix)+len(requests))
	if len(prefix) != 0 {
		for i := range prefix {
			resolved, next, stepNullable, err := resolveOrdinalStep(current, prefix[i].Ordinal, fmt.Sprintf("field.prefix[%d]", i))
			if err != nil {
				return nil, resolutionError(FieldIncompatibleRoot, fmt.Sprintf("field.prefix[%d]", i), err.Error())
			}
			// Preserve display metadata from the already-admitted inner path.
			resolved.Field = prefix[i].Field
			accessors = append(accessors, resolved)
			current = next
			nullable = nullable || stepNullable
		}
	}
	for i, request := range requests {
		resolved, next, stepNullable, err := resolveRequestStep(current, request, len(requests)-i > 1, fmt.Sprintf("field.path[%d]", i))
		if err != nil {
			return nil, err
		}
		accessors = append(accessors, resolved)
		current = next
		nullable = nullable || stepNullable
	}
	result := exactWithNullability(current, nullable)
	// The QOV's captured exact root is the only authority for the physical
	// ordinal domain.  Callers never supply this token: deriving it here makes
	// every admitted FieldValue domain-complete while keeping the initial path
	// source-relative (unpinned).
	path := &fieldPath{
		Accessors: accessors,
		// thawShared, not thaw: the graph is read for its column names and
		// discarded on the next line. A defensive copy here allocated a whole
		// Type graph per resolution to answer a question about an immutable
		// handle, and field resolution is on the planner's inner loop.
		Domain: OrdinalDomainOfType(root.thawShared()),
	}
	return &fieldValue{
		Field:      accessors[len(accessors)-1].Field,
		Typ:        result.thaw(),
		Child:      qov,
		Resolved:   path,
		rootType:   root,
		resultType: result,
	}, nil
}

func resolveRequestStep(
	current *exactType,
	request *fieldRequest,
	mustDescend bool,
	location string,
) (resolvedAccessor, *exactType, bool, error) {
	if current == nil || current.code != TypeCodeRecord {
		return resolvedAccessor{}, nil, false, resolutionError(FieldNonRecord, location, "field step does not address a record")
	}
	if current.anyRecord {
		return resolvedAccessor{}, nil, false, resolutionError(TypeErased, location, "field layout of AnyRecord is erased")
	}
	ordinal := -1
	switch request.kind {
	case fieldRequestByOrdinal:
		ordinal = request.ordinal
	case fieldRequestByNameAndOrdinal:
		// BOTH authorities are stated, so the ORDINAL SELECTS and the NAME
		// VERIFIES.
		//
		// Name AMBIGUITY is therefore not an error on this kind; it is the
		// situation the kind exists for. A row may legitimately carry two
		// same-named columns — `GROUP BY ot.k, it.k` produces an aggregate whose
		// output row is [K, K, COUNT(*)] — and "the K at slot 1" is a complete
		// question. Routing it through the by-name scan answered AMBIGUOUS for a
		// request that names exactly one field, which left the aggregate unable
		// to state its own provided ordering: HintOrdering declined, the ORDER BY
		// it already satisfies stopped matching, and the planner stacked a SECOND
		// InMemorySort above it. Rows stay correct, so only a plan assertion can
		// see that.
		ordinal = request.ordinal
		if ordinal >= 0 && ordinal < len(current.fields) {
			if current.fields[ordinal].name != request.name {
				return resolvedAccessor{}, nil, false, resolutionError(FieldNameOrdinalMismatch, location, "field name and ordinal select different fields")
			}
			break
		}
		// The ordinal cannot address this record. When the NAME can, the two
		// authorities disagree and that is the sharper diagnosis — the same one
		// this kind reported before the ordinal became the selector. Otherwise
		// fall through to the ordinal range errors below.
		for i := range current.fields {
			if current.fields[i].name == request.name {
				return resolvedAccessor{}, nil, false, resolutionError(FieldNameOrdinalMismatch, location, "field name and ordinal select different fields")
			}
		}
	case fieldRequestByName:
		matches := 0
		for i := range current.fields {
			if current.fields[i].name == request.name {
				matches++
				ordinal = i
			}
		}
		if matches == 0 {
			return resolvedAccessor{}, nil, false, resolutionError(FieldUnknownName, location, "record contains no field with the requested semantic name")
		}
		if matches > 1 {
			return resolvedAccessor{}, nil, false, resolutionError(FieldAmbiguousName, location, "record contains multiple fields with the requested semantic name")
		}
	default:
		return resolvedAccessor{}, nil, false, resolutionError(FieldInvalidRequest, location, "malformed field request kind")
	}
	if ordinal < 0 {
		return resolvedAccessor{}, nil, false, resolutionError(FieldNegativeOrdinal, location, "field ordinal is negative")
	}
	if ordinal >= len(current.fields) {
		return resolvedAccessor{}, nil, false, resolutionError(FieldOutOfRange, location, "field ordinal is outside the record")
	}
	field := current.fields[ordinal]
	if mustDescend {
		if field.typ.code != TypeCodeRecord {
			return resolvedAccessor{}, nil, false, resolutionError(FieldNonRecord, location, "non-final field step selects a scalar")
		}
		if field.typ.anyRecord {
			return resolvedAccessor{}, nil, false, resolutionError(TypeErased, location, "non-final field step selects an AnyRecord with erased layout")
		}
	}
	return resolvedAccessor{
		Field:     field.name,
		Ordinal:   ordinal,
		fieldType: field.typ,
	}, field.typ, field.typ.nullable, nil
}

func resolveOrdinalStep(current *exactType, ordinal int, location string) (resolvedAccessor, *exactType, bool, error) {
	return resolveRequestStep(current, &fieldRequest{kind: fieldRequestByOrdinal, ordinal: ordinal}, false, location)
}

func resolveAgainstRecordConstructor(constructor *RecordConstructorValue, requests []*fieldRequest) (Value, error) {
	if constructor == nil {
		return nil, resolutionError(FieldNilChild, "field.child", "record constructor is typed nil")
	}
	root, err := exactRecordConstructorType(constructor)
	if err != nil {
		return nil, err
	}
	first, next, stepNullable, err := resolveRequestStep(root, requests[0], len(requests) > 1, "field.path[0]")
	if err != nil {
		return nil, err
	}
	selected := constructor.Fields[first.Ordinal].Value
	if selected == nil {
		return nil, resolutionError(FieldUnsupportedChild, "field.child.record", "selected record-constructor child is nil")
	}
	selectedType, err := exactTypeOfKnownValue(selected)
	if err != nil {
		return nil, err
	}
	accessResult := exactWithNullability(next, root.nullable || stepNullable)
	if len(requests) == 1 {
		if !exactTypesEqual(selectedType, accessResult) {
			return nil, resolutionError(FieldIncompatibleRoot, "field.child.record", "record-constructor collapse would change result nullability or type")
		}
		return selected, nil
	}
	resolved, err := resolveFieldAccess(selected, requests[1:])
	if err != nil {
		return nil, err
	}
	resolvedType, err := exactTypeOfKnownValue(resolved)
	if err != nil {
		return nil, err
	}
	// The outer constructor can only collapse when it does not widen the nested
	// result.  Otherwise a bound QOV is required to preserve Java's OR rule.
	if root.nullable && !resolvedType.nullable {
		return nil, resolutionError(FieldIncompatibleRoot, "field.child.record", "nullable record constructor requires a QOV boundary")
	}
	return resolved, nil
}

func exactRecordConstructorType(constructor *RecordConstructorValue) (*exactType, error) {
	fields := make([]exactField, len(constructor.Fields))
	for i := range constructor.Fields {
		fieldType, err := exactTypeOfKnownValue(constructor.Fields[i].Value)
		if err != nil {
			return nil, err
		}
		name := constructor.Fields[i].Name
		if name == "" {
			name = OrdinalFieldName(i)
		}
		fields[i] = exactField{name: name, ordinal: i, typ: fieldType}
	}
	root := &exactType{code: TypeCodeRecord, name: constructor.typeName, fields: fields}
	root.finishCanonical()
	return root, nil
}

func exactTypeOfKnownValue(value Value) (*exactType, error) {
	switch typed := value.(type) {
	case *quantifiedObjectValue:
		if typed == nil || typed.flowed == nil {
			return nil, resolutionError(FieldUnsupportedChild, "field.child", "malformed QOV")
		}
		return typed.flowed, nil
	case *fieldValue:
		if !isAdmittedFieldValue(typed) {
			return nil, resolutionError(FieldUnsupportedChild, "field.child", "malformed FieldValue")
		}
		return typed.resultType, nil
	case *RecordConstructorValue:
		return exactRecordConstructorType(typed)
	case *ConstantValue, *BooleanValue, *NullValue, *ConstantObjectValue,
		*ArithmeticValue, *CastValue, *PromoteValue, *ParameterValue,
		*ScalarFunctionValue, *ExistsValue:
		handle, err := SnapshotExactType(value.Type())
		if err != nil {
			return nil, err
		}
		return handle.(*exactType), nil
	default:
		return nil, resolutionError(FieldUnsupportedChild, "field.child", "unsupported record-constructor child Value kind")
	}
}

func exactWithNullability(source *exactType, nullable bool) *exactType {
	if source == nil || source.nullable == nullable || source.code == TypeCodeRelation {
		return source
	}
	// Field-by-field rather than `*source`: the struct carries a memo whose key
	// is a descriptor and whose answer belongs to the handle that derived it.
	// Copying it wholesale would hand the derived handle an answer it never
	// computed — and this function exists precisely to produce a handle that
	// DIFFERS from its source, so the two are not interchangeable by
	// construction. Leaving the memo zero costs one recomputation.
	derived := &exactType{
		code:       source.code,
		nullable:   nullable,
		anyRecord:  source.anyRecord,
		name:       source.name,
		fields:     source.fields,
		element:    source.element,
		enumValues: source.enumValues,
	}
	derived.finishCanonical()
	return derived
}

// RebuildFieldValue resolves an admitted FieldValue's complete ordinal path on
// a replacement child and refuses any type/nullability drift.
func RebuildFieldValue(field FieldValue, child Value) (Value, error) {
	original, ok := field.(*fieldValue)
	if !ok || !isAdmittedFieldValue(original) {
		return nil, resolutionError(FieldUnsupportedChild, "field.rebuild", "foreign or malformed FieldValue")
	}
	requests := make([]*fieldRequest, len(original.Resolved.Accessors))
	for i := range original.Resolved.Accessors {
		requests[i] = &fieldRequest{kind: fieldRequestByOrdinal, ordinal: original.Resolved.Accessors[i].Ordinal}
	}
	rebuilt, err := resolveFieldAccess(child, requests)
	if err != nil {
		return nil, resolutionError(FieldIncompatibleRoot, "field.rebuild", err.Error())
	}
	rebuiltType, err := exactTypeOfKnownValue(rebuilt)
	if err != nil || !exactTypesEqual(original.resultType, rebuiltType) {
		return nil, resolutionError(FieldIncompatibleRoot, "field.rebuild", "replacement child changes the field result type")
	}
	if rebuiltField, ok := rebuilt.(*fieldValue); ok {
		if !isAdmittedFieldValue(rebuiltField) {
			return nil, resolutionError(FieldUnsupportedChild, "field.rebuild", "rebuild produced a malformed FieldValue")
		}
		pathCopy := *rebuiltField.Resolved
		pathCopy.Accessors = append([]resolvedAccessor(nil), rebuiltField.Resolved.Accessors...)
		pathCopy.FrontierPinned = original.Resolved.FrontierPinned
		copy := *rebuiltField
		copy.Resolved = &pathCopy
		return &copy, nil
	}
	return rebuilt, nil
}

// rebuildFieldValueOnChangedChild is the generic rewrite spine's exact
// FieldValue reconstruction. A changed child is resolved atomically; no copy of
// display/path metadata is publishable until the replacement proves the same
// exact result type.
//
// A FieldValue replacement is the TranslationMap merge shape. Resolving the
// outer path against that admitted child canonicalizes both paths onto the
// replacement's QOV root, preserving its frontier pin and exact metadata. A
// QOV/record-constructor replacement uses the ordinary checked rebuild.
func rebuildFieldValueOnChangedChild(original *fieldValue, child Value) (Value, error) {
	if !isAdmittedFieldValue(original) {
		return nil, resolutionError(FieldUnsupportedChild, "field.rebuild", "foreign or malformed FieldValue")
	}
	if child == nil {
		return nil, resolutionError(RewriteNilReplacement, "field.rebuild.child", "replacement child is nil")
	}
	if inner, ok := child.(*fieldValue); ok {
		if !isAdmittedFieldValue(inner) {
			return nil, resolutionError(FieldUnsupportedChild, "field.rebuild.child", "replacement FieldValue is malformed")
		}
		requests := make([]*fieldRequest, len(original.Resolved.Accessors))
		for i := range original.Resolved.Accessors {
			requests[i] = &fieldRequest{kind: fieldRequestByOrdinal, ordinal: original.Resolved.Accessors[i].Ordinal}
		}
		rebuilt, err := resolveFieldAccess(inner, requests)
		if err != nil {
			return nil, resolutionError(FieldIncompatibleRoot, "field.rebuild", err.Error())
		}
		rebuiltField, ok := rebuilt.(*fieldValue)
		if !ok || !isAdmittedFieldValue(rebuiltField) ||
			!exactTypesEqual(original.resultType, rebuiltField.resultType) {
			return nil, resolutionError(FieldIncompatibleRoot, "field.rebuild", "fused replacement changes the field result type")
		}
		pathCopy := *rebuiltField.Resolved
		pathCopy.Accessors = append([]resolvedAccessor(nil), rebuiltField.Resolved.Accessors...)
		pathCopy.FrontierPinned = inner.Resolved.FrontierPinned
		copy := *rebuiltField
		copy.Resolved = &pathCopy
		return &copy, nil
	}
	return RebuildFieldValue(original, child)
}

// evaluateResolved evaluates the child exactly once and walks the complete
// ordinal vector.  Field display names are never consulted at runtime.
func (f *fieldValue) evaluateResolved(evalCtx any) (any, error) {
	childResult, err := f.Child.Evaluate(evalCtx)
	if err != nil || childResult == nil {
		return childResult, err
	}
	current := childResult
	expected := f.rootType
	for i := range f.Resolved.Accessors {
		if current == nil {
			return nil, nil
		}
		accessor := &f.Resolved.Accessors[i]
		switch carrier := current.(type) {
		case OrdinalRow:
			value, ok := carrier.Get(accessor.Ordinal)
			if !ok {
				return nil, resolutionError(LayoutRuntimeShape, fmt.Sprintf("field.path[%d]", i), "ordinal row is shorter than the resolved path")
			}
			current = value
		case proto.Message:
			value, readErr := readProtoOrdinal(carrier.ProtoReflect(), expected, accessor.Ordinal, i)
			if readErr != nil {
				return nil, readErr
			}
			current = value
		case protoreflect.Message:
			value, readErr := readProtoOrdinal(carrier, expected, accessor.Ordinal, i)
			if readErr != nil {
				return nil, readErr
			}
			current = value
		default:
			// Java FieldValue returns NULL for a non-Message child.
			return nil, nil
		}
		expected = accessor.fieldType
	}
	return current, nil
}

// protoShapeVerdict is one memoized answer from protoRecordShapeCompatible,
// stored whole so a reader can never see a descriptor paired with a different
// descriptor's verdict. See exactType.protoShape for why it is a single entry.
type protoShapeVerdict struct {
	descriptor protoreflect.MessageDescriptor
	compatible bool
}

// protoShapeCompatibleCached is protoRecordShapeCompatible with its answer
// remembered per (descriptor, expected). The underlying question is a pure
// function of two immutable things — a descriptor and a captured exact type —
// but it was being re-answered on every column read of every row, which made a
// full scan pay O(fields²) per row for a verdict that cannot change within one
// scan.
func protoShapeCompatibleCached(descriptor protoreflect.MessageDescriptor, expected *exactType) bool {
	if descriptor == nil || expected == nil {
		return false
	}
	if cached := expected.protoShape.Load(); cached != nil && cached.descriptor == descriptor {
		return cached.compatible
	}
	compatible := protoRecordShapeCompatible(descriptor, expected)
	expected.protoShape.Store(&protoShapeVerdict{descriptor: descriptor, compatible: compatible})
	return compatible
}

func readProtoOrdinal(message protoreflect.Message, expected *exactType, ordinal, depth int) (any, error) {
	if !protoShapeCompatibleCached(message.Descriptor(), expected) {
		return nil, resolutionError(LayoutRuntimeShape, fmt.Sprintf("field.path[%d]", depth), "protobuf descriptor disagrees with the captured exact record shape")
	}
	fields := message.Descriptor().Fields()
	if ordinal < 0 || ordinal >= fields.Len() {
		return nil, resolutionError(LayoutRuntimeShape, fmt.Sprintf("field.path[%d]", depth), "protobuf descriptor is shorter than the resolved path")
	}
	field := fields.Get(ordinal)
	if field.HasPresence() && !message.Has(field) {
		if field.HasDefault() {
			return ProtoFieldToRowValue(field, field.Default()), nil
		}
		return nil, nil
	}
	return ProtoFieldToRowValue(field, message.Get(field)), nil
}

func protoRecordShapeCompatible(descriptor protoreflect.MessageDescriptor, expected *exactType) bool {
	if descriptor == nil || expected == nil || expected.code != TypeCodeRecord || expected.anyRecord || descriptor.Fields().Len() != len(expected.fields) {
		return false
	}
	for i := 0; i < descriptor.Fields().Len(); i++ {
		if !protoFieldShapeCompatible(descriptor.Fields().Get(i), expected.fields[i].typ) {
			return false
		}
	}
	return true
}

func protoFieldShapeCompatible(field protoreflect.FieldDescriptor, expected *exactType) bool {
	if field == nil || expected == nil {
		return false
	}
	if list, wrapped, ok := EffectiveListField(field); ok {
		return expected.code == TypeCodeArray && expected.element != nil &&
			expected.nullable == wrapped &&
			protoScalarShapeCompatible(list, expected.element)
	}
	if field.IsMap() {
		return false
	}
	return protoScalarShapeCompatible(field, expected)
}

// protoScalarShapeCompatible answers whether a captured exact type can read the
// value a descriptor stores. The scalar answer is DELEGATED to
// ScalarTypeForProtoKind — the one authority that types stored columns — rather
// than restated as a switch here.
//
// It was restated, and the two copies had drifted. The mapper carries every
// unsigned and fixed integer kind as LONG (the runtime materializer emits
// int64 for them) and carries an alias-bearing enum as LONG too, because the
// engine's EnumType refuses number aliases. This switch had no case for either,
// so both fell to `default: return false` — and the caller reads false as "this
// descriptor disagrees with the captured shape", which rejects the WHOLE
// protobuf row before any ordinal can be read. A table with a `uint64` column
// was unreadable through this path, which matters most for exactly the records
// this port exists to share: Java writes them.
//
// NULLABILITY IS DELIBERATELY NOT COMPARED, and that is not an oversight to fix
// later. The DDL emitter never emits proto2 `required`
// (metadata.TestDDLEmitterNeverEmitsRequired pins it), so every column this
// engine creates is `optional` and protoFieldNullable reports true for it —
// NOT NULL columns and primary keys included. Requiring the captured
// nullability to equal the descriptor's presence would therefore reject every
// primary key in every table. The captured type is the authority on
// nullability; the descriptor is the authority on the value's SHAPE, and this
// function answers only the second question.
func protoScalarShapeCompatible(field protoreflect.FieldDescriptor, expected *exactType) bool {
	if field == nil || expected == nil {
		return false
	}
	switch field.Kind() {
	case protoreflect.MessageKind, protoreflect.GroupKind:
		// A record's compatibility is STRUCTURAL and stays here: comparing type
		// codes alone would accept any record for any other.
		if string(field.Message().FullName()) == uuidProtoMessageName {
			return expected.code == TypeCodeUuid
		}
		if expected.code == TypeCodeRecord && expected.anyRecord {
			return true
		}
		return expected.code == TypeCodeRecord && protoRecordShapeCompatible(field.Message(), expected)
	}
	// The CODE, not the Type. This runs per field read per row, and building the
	// Type here allocated one per scalar — plus a value slice and a number set
	// for every enum — to answer a question about an int. ScalarCodeForProtoKind
	// is the same authority with the construction left out, so delegation is
	// preserved and the allocation is not.
	mapped, ok := ScalarCodeForProtoKind(field)
	if !ok || mapped == TypeCodeUnknown {
		return false
	}
	if mapped == expected.code {
		return true
	}
	// The two STORAGE ALIASES, kept explicitly: a SQL type whose stored carrier
	// is a different code. Everything else must match the mapper exactly.
	switch expected.code {
	case TypeCodeVersion:
		return mapped == TypeCodeLong
	case TypeCodeDate, TypeCodeTimestamp:
		return mapped == TypeCodeString
	}
	return false
}
