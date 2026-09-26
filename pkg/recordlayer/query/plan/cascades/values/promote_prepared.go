package values

import (
	"errors"
	"fmt"
	"reflect"

	"github.com/google/uuid"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/dynamicpb"
)

// PromotionError reports an invalid declared coercion, unprepared literal, or
// mutation of an already prepared relation. It is detected before child evaluation.
type PromotionError struct{ Reason string }

func (e *PromotionError) Error() string { return "promotion: " + e.Reason }

// promotionNode is the declared-type coercion tree, corresponding to Java's
// PromoteValue.computePromotionsTrie. It is immutable once published to readers.
type promotionNode struct {
	source, target Type
	children       []*promotionNode
	sourceExact    ExactTypeHandle
	descriptor     protoreflect.MessageDescriptor
	planned        bool
}

type preparedPromotion struct {
	child Value
	root  *promotionNode
}

// copyPromotionType owns logical shape without requiring an exact QOV type:
// ANY, NONE, NULL and unresolved/erased forms must survive copying so admission
// can apply the coercion's own rules. Physical row-layout provenance is not part
// of the coercion relation.
func copyPromotionType(t Type, active map[Type]bool) (Type, error) {
	if isNilBinding(t) {
		return nil, &PromotionError{Reason: "nil type"}
	}
	switch v := t.(type) {
	case *PrimitiveType:
		copy := *v
		return &copy, nil
	case anyRecordType:
		return v, nil
	case *EnumType:
		copy := *v
		copy.Values = append([]EnumValue(nil), v.Values...)
		return &copy, nil
	case *RecordType, *ArrayType:
		if active[t] {
			return nil, &PromotionError{Reason: "cyclic type"}
		}
		active[t] = true
		defer delete(active, t)
	default:
		return nil, &PromotionError{Reason: fmt.Sprintf("unsupported type %T", t)}
	}
	switch v := t.(type) {
	case *ArrayType:
		copy := *v
		if v.ElementType != nil {
			var err error
			copy.ElementType, err = copyPromotionType(v.ElementType, active)
			if err != nil {
				return nil, err
			}
		}
		return &copy, nil
	case *RecordType:
		fields := make([]Field, len(v.Fields))
		for i, field := range v.Fields {
			fieldType, err := copyPromotionType(field.FieldType, active)
			if err != nil {
				return nil, err
			}
			fields[i] = Field{Name: field.Name, Ordinal: field.Ordinal, FieldType: fieldType}
		}
		return &RecordType{RecordName: v.RecordName, Nullable: v.Nullable, Fields: fields}, nil
	}
	return nil, &PromotionError{Reason: "unsupported type"}
}

func promotionTypesEqual(a, b Type) bool {
	if isNilBinding(a) || isNilBinding(b) {
		return isNilBinding(a) && isNilBinding(b)
	}
	if a.Code() != b.Code() || a.IsNullable() != b.IsNullable() {
		return false
	}
	switch av := a.(type) {
	case *RecordType:
		bv, ok := b.(*RecordType)
		if !ok || av.RecordName != bv.RecordName || len(av.Fields) != len(bv.Fields) {
			return false
		}
		for i, f := range av.Fields {
			g := bv.Fields[i]
			if f.Name != g.Name || f.Ordinal != g.Ordinal || !promotionTypesEqual(f.FieldType, g.FieldType) {
				return false
			}
		}
		return true
	case *ArrayType:
		bv, ok := b.(*ArrayType)
		return ok && promotionTypesEqual(av.ElementType, bv.ElementType)
	case *EnumType:
		bv, ok := b.(*EnumType)
		return ok && av.EnumName == bv.EnumName && av.Equals(bv)
	default:
		return a.Equals(b)
	}
}

func compilePromotion(source, target Type) (*promotionNode, error) {
	n := &promotionNode{source: source, target: target}
	bad := func() (*promotionNode, error) {
		return nil, &PromotionError{Reason: fmt.Sprintf("incompatible types %s -> %s", source, target)}
	}
	if target.Code() == TypeCodeAny {
		return n, nil
	}
	// The exported Go planner accepts unresolved runtime comparands for UUID
	// and ENUM. Its binder can supply either text or an already converted
	// value, unlike Java's parameter literals, whose types are known while
	// planning. Preserve that narrow boundary operator; UNKNOWN is not a
	// license to infer numeric or structured promotions from runtime values.
	if source.Code() == TypeCodeUnknown && (target.Code() == TypeCodeUuid || target.Code() == TypeCodeEnum) {
		return n, nil
	}
	if source.Code() == TypeCodeNull {
		switch target.Code() {
		case TypeCodeInt, TypeCodeLong, TypeCodeFloat, TypeCodeDouble, TypeCodeBoolean, TypeCodeString, TypeCodeArray, TypeCodeRecord, TypeCodeEnum, TypeCodeBytes, TypeCodeVersion:
			return n, nil
		}
		return bad()
	}
	if source.Code() == TypeCodeNone {
		if target.Code() == TypeCodeArray {
			return n, nil
		}
		return bad()
	}
	if sr, ok := source.(*RecordType); ok {
		tr, ok := target.(*RecordType)
		if !ok || len(sr.Fields) != len(tr.Fields) {
			return bad()
		}
		for i := range sr.Fields {
			child, err := compilePromotion(sr.Fields[i].FieldType, tr.Fields[i].FieldType)
			if err != nil {
				return nil, err
			}
			n.children = append(n.children, child)
		}
		// Exact admission is used only for protobuf inputs. Raw records can
		// still carry placeholders for which no protobuf form exists.
		n.sourceExact, _ = SnapshotExactType(source)
		return n, nil
	}
	if sa, ok := source.(*ArrayType); ok {
		ta, ok := target.(*ArrayType)
		if !ok || sa.ElementType == nil || ta.ElementType == nil {
			return bad()
		}
		child, err := compilePromotion(sa.ElementType, ta.ElementType)
		if err != nil {
			return nil, err
		}
		n.children = []*promotionNode{child}
		return n, nil
	}
	if source.Code() == target.Code() && source.Code() != TypeCodeUnknown {
		if source.Code() == TypeCodeEnum && !WithNullability(source, false).Equals(WithNullability(target, false)) {
			return bad()
		}
		return n, nil
	}
	switch source.Code() {
	case TypeCodeInt:
		if target.Code() == TypeCodeLong || target.Code() == TypeCodeFloat || target.Code() == TypeCodeDouble {
			return n, nil
		}
	case TypeCodeLong:
		if target.Code() == TypeCodeFloat || target.Code() == TypeCodeDouble {
			return n, nil
		}
	case TypeCodeFloat:
		if target.Code() == TypeCodeDouble {
			return n, nil
		}
	case TypeCodeString:
		if target.Code() == TypeCodeEnum || target.Code() == TypeCodeUuid {
			return n, nil
		}
	}
	return bad()
}

// Prepare prepares a direct struct literal. It never repairs a mutated prepared
// node: use checked reconstruction to create a new relation instead.
func (p *PromoteValue) Prepare() error {
	if p.prepared != nil {
		return p.validatePreparation()
	}
	if p.prepareErr != nil {
		return p.prepareErr
	}
	if isNilBinding(p.Child) {
		return &PromotionError{Reason: "nil child"}
	}
	source, err := copyPromotionType(p.Child.Type(), make(map[Type]bool))
	if err != nil {
		return err
	}
	target, err := copyPromotionType(p.Target, make(map[Type]bool))
	if err != nil {
		return err
	}
	root, err := compilePromotion(source, target)
	if err != nil {
		return err
	}
	if target.Code() == TypeCodeRecord || target.Code() == TypeCodeArray || target.Code() == TypeCodeEnum {
		repo := NewTypeProtoRepository()
		if err := registerPromotionTargets(root, repo); err != nil {
			return err
		}
		if err := repo.Seal(); err != nil {
			return err
		}
		if err := bindPromotionTargets(root, repo, false); err != nil {
			return err
		}
	}
	p.prepared = &preparedPromotion{child: p.Child, root: root}
	return nil
}

func registerPromotionTargets(n *promotionNode, repo *TypeProtoRepository) error {
	if n.target.Code() == TypeCodeRecord || n.target.Code() == TypeCodeArray || n.target.Code() == TypeCodeEnum {
		if err := repo.RegisterType(n.target); err != nil {
			var raw *ProtoTypeError
			if !errors.As(err, &raw) {
				return err
			}
		}
	}
	for _, child := range n.children {
		if err := registerPromotionTargets(child, repo); err != nil {
			return err
		}
	}
	return nil
}

func bindPromotionTargets(n *promotionNode, repo *TypeProtoRepository, planned bool) error {
	n.planned = planned
	if n.target.Code() == TypeCodeRecord {
		md, err := repo.MessageDescriptorFor(n.target)
		if err == nil {
			n.descriptor = md
		} else {
			var raw *ProtoTypeError
			var missing *SealedTypeRepositoryError
			if !errors.As(err, &raw) && !errors.As(err, &missing) {
				return err
			}
		}
	}
	for _, child := range n.children {
		if err := bindPromotionTargets(child, repo, planned); err != nil {
			return err
		}
	}
	return nil
}

func (p *PromoteValue) validatePreparation() error {
	if p.prepareErr != nil {
		return p.prepareErr
	}
	if p.prepared == nil {
		return &PromotionError{Reason: "unprepared literal"}
	}
	child := reflect.ValueOf(p.Child)
	if !child.IsValid() || !child.Comparable() || !child.Equal(reflect.ValueOf(p.prepared.child)) {
		return &PromotionError{Reason: "child identity changed"}
	}
	if !promotionTypesEqual(p.Child.Type(), p.prepared.root.source) || !promotionTypesEqual(p.Target, p.prepared.root.target) {
		return &PromotionError{Reason: "declared source or target type changed"}
	}
	return nil
}

// RegisterPromotionTypes and BindPromotionTypes are the collect and bind halves
// used by the plan finalizer. Both precede publication to concurrent evaluators.
func (p *PromoteValue) RegisterPromotionTypes(repo *TypeProtoRepository) error {
	if err := p.validatePreparation(); err != nil {
		return err
	}
	return registerPromotionTargets(p.prepared.root, repo)
}

func (p *PromoteValue) BindPromotionTypes(repo *TypeProtoRepository) error {
	if err := p.validatePreparation(); err != nil {
		return err
	}
	return bindPromotionTargets(p.prepared.root, repo, true)
}

// NewPromoteValueChecked eagerly prepares the relation and reports admission errors.
func NewPromoteValueChecked(child Value, target Type) (*PromoteValue, error) {
	owned, err := copyPromotionType(target, make(map[Type]bool))
	if err != nil {
		return nil, err
	}
	p := &PromoteValue{Child: child, Target: owned}
	if err := p.Prepare(); err != nil {
		return nil, err
	}
	return p, nil
}

func (p *PromoteValue) Evaluate(evalCtx any) (any, error) {
	if err := p.validatePreparation(); err != nil {
		return nil, err
	}
	v, err := p.Child.Evaluate(evalCtx)
	if err != nil || v == nil {
		return nil, err
	}
	return p.prepared.root.coerce(v)
}

func (n *promotionNode) coerce(v any) (any, error) {
	if v == nil {
		if !n.target.IsNullable() {
			return nil, &NonNullableFieldError{Field: "promotion"}
		}
		return nil, nil
	}
	if n.target.Code() == TypeCodeAny {
		return v, nil
	}
	if n.source.Code() == TypeCodeNone {
		return v, nil
	}
	if n.target.Code() == TypeCodeRecord {
		return n.coerceRecord(v)
	}
	if n.target.Code() == TypeCodeArray {
		list, ok := v.([]any)
		if !ok {
			return nil, &PromotionError{Reason: fmt.Sprintf("array carrier is %T", v)}
		}
		out := make([]any, len(list))
		for i, element := range list {
			// Raw Go arrays retain the approved nullable-element extension;
			// protobuf field assignment still rejects unrepresentable NULLs.
			converted, err := n.children[0].coerce(element)
			if err != nil {
				return nil, err
			}
			out[i] = converted
		}
		return out, nil
	}
	if n.source.Code() == n.target.Code() {
		return coerceNumericResult(v, n.target), nil
	}
	if n.source.Code() == TypeCodeUnknown {
		if _, text := v.(string); !text {
			return v, nil
		}
	}
	if n.source.Code() == TypeCodeString || n.source.Code() == TypeCodeUnknown {
		text, ok := v.(string)
		if !ok {
			return nil, &PromotionError{Reason: fmt.Sprintf("STRING carrier is %T", v)}
		}
		if target, ok := n.target.(*EnumType); ok {
			return stringToEnumValue(target, text)
		}
		u, err := uuid.Parse(text)
		if err != nil {
			return nil, fmt.Errorf("Invalid UUID value for the UUID type %s", text)
		}
		return [16]byte(u), nil
	}
	// The operator was selected from declared types during preparation. These
	// carrier conversions implement that operator, not runtime type inference.
	switch n.source.Code() {
	case TypeCodeInt, TypeCodeLong:
		i, ok := asInt64(v)
		if !ok {
			return nil, &PromotionError{Reason: fmt.Sprintf("integer carrier is %T", v)}
		}
		switch n.target.Code() {
		case TypeCodeLong:
			return i, nil
		case TypeCodeFloat:
			return float64(float32(i)), nil
		case TypeCodeDouble:
			return float64(i), nil
		}
	case TypeCodeFloat:
		f, ok := asFloat64(v)
		if ok {
			return float64(float32(f)), nil
		}
	}
	return nil, &PromotionError{Reason: fmt.Sprintf("invalid carrier %T for %s", v, n.source)}
}

func (n *promotionNode) coerceRecord(v any) (any, error) {
	source, sourceOK := n.source.(*RecordType)
	target, targetOK := n.target.(*RecordType)
	if !sourceOK || !targetOK {
		return nil, &PromotionError{Reason: "record relation has no declared fields"}
	}
	var message protoreflect.Message
	var raw map[string]any
	if input, ok := v.(proto.Message); ok {
		message = input.ProtoReflect()
		if n.descriptor == nil || !ProtoRecordDescriptorCompatible(message.Descriptor(), n.sourceExact) {
			return nil, &PromotionError{Reason: "protobuf input disagrees with declared source or target has no descriptor"}
		}
	} else {
		var ok bool
		raw, ok = v.(map[string]any)
		if !ok {
			return nil, &PromotionError{Reason: fmt.Sprintf("record carrier is %T", v)}
		}
	}
	var result *dynamicpb.Message
	if message != nil || (n.planned && n.descriptor != nil) {
		result = dynamicpb.NewMessage(n.descriptor)
	}
	out := make(map[string]any, len(target.Fields))
	for i, field := range source.Fields {
		name := field.Name
		if name == "" {
			name = OrdinalFieldName(i)
		}
		value, present := raw[name]
		if message != nil {
			fd := message.Descriptor().Fields().Get(i)
			present = fd.IsList() || message.Has(fd)
			if present {
				value = ProtoFieldToRowValue(fd, message.Get(fd))
			}
		}
		if !present {
			continue
		}
		converted, err := n.children[i].coerce(value)
		if err != nil {
			return nil, err
		}
		if result != nil {
			if converted == nil {
				continue
			}
			fd := n.descriptor.Fields().Get(i)
			pv, err := rowValueToProtoField(result, fd, converted)
			if err != nil {
				return nil, err
			}
			result.Set(fd, pv)
		} else {
			name := target.Fields[i].Name
			if name == "" {
				name = OrdinalFieldName(i)
			}
			out[name] = converted
		}
	}
	if result != nil {
		return result, nil
	}
	return out, nil
}
