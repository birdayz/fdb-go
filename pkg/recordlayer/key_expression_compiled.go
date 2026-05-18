package recordlayer

import (
	"fmt"

	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/fdb/subspace"
	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/fdb/tuple"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
)

// compiledKeyEvaluator is a pre-compiled evaluator for a KeyExpression.
// Instead of walking the expression tree per record and allocating []any
// intermediate results, it directly packs the key for a given record and
// subspace. This eliminates all intermediate allocations from EvaluateFlat.
//
// Only works for simple expressions (Concat of Field/RecordTypeKey, no fan-out).
// Falls back to EvaluateFlat for unsupported expressions.
type compiledKeyEvaluator struct {
	steps []compiledStep
	ok    bool // false if expression couldn't be compiled
}

type compiledStep interface {
	// packInto appends this step's key element(s) to the packer.
	packInto(p *tupleAppender, record *FDBStoredRecord[proto.Message], msg proto.Message) error
}

// tupleAppender builds a tuple.Tuple without intermediate []any.
type tupleAppender struct {
	elements []tuple.TupleElement
}

func (a *tupleAppender) reset() {
	a.elements = a.elements[:0]
}

func (a *tupleAppender) appendInt64(v int64) {
	a.elements = append(a.elements, v)
}

func (a *tupleAppender) appendString(v string) {
	a.elements = append(a.elements, v)
}

func (a *tupleAppender) appendAny(v any) {
	a.elements = append(a.elements, v)
}

func (a *tupleAppender) toTuple() tuple.Tuple {
	return tuple.Tuple(a.elements)
}

func (a *tupleAppender) packWithPrefix(prefix []byte) []byte {
	return a.toTuple().PackWithPrefix(prefix)
}

// --- Compiled step implementations ---

type recordTypeKeyStep struct {
	typeKeys map[string]int64
}

func (s *recordTypeKeyStep) packInto(a *tupleAppender, record *FDBStoredRecord[proto.Message], msg proto.Message) error {
	if msg == nil {
		a.appendAny(nil)
		return nil
	}
	typeName := string(msg.ProtoReflect().Descriptor().Name())
	if s.typeKeys != nil {
		if k, ok := s.typeKeys[typeName]; ok {
			a.appendInt64(k)
			return nil
		}
	}
	a.appendString(typeName)
	return nil
}

type fieldStep struct {
	fieldName     string
	cachedFD      protoreflect.FieldDescriptor // safe: fieldStep is per-batch, not shared
	cachedMsgName string
}

func (s *fieldStep) packInto(a *tupleAppender, _ *FDBStoredRecord[proto.Message], msg proto.Message) error {
	if msg == nil {
		a.appendAny(nil)
		return nil
	}
	m := msg.ProtoReflect()

	msgName := string(m.Descriptor().FullName())
	fd := s.cachedFD
	if s.cachedMsgName != msgName || fd == nil {
		fd = m.Descriptor().Fields().ByName(protoreflect.Name(s.fieldName))
		if fd == nil {
			a.appendAny(nil)
			return nil
		}
		s.cachedFD = fd
		s.cachedMsgName = msgName
	}

	if fd.IsList() {
		return errFanOut
	}
	if fd.HasPresence() && !m.Has(fd) {
		a.appendAny(nil)
		return nil
	}

	// Typed append — avoids scalarToInterface any boxing.
	switch fd.Kind() {
	case protoreflect.Int32Kind, protoreflect.Sint32Kind, protoreflect.Sfixed32Kind,
		protoreflect.Int64Kind, protoreflect.Sint64Kind, protoreflect.Sfixed64Kind:
		a.appendInt64(m.Get(fd).Int())
	case protoreflect.Uint32Kind, protoreflect.Fixed32Kind,
		protoreflect.Uint64Kind, protoreflect.Fixed64Kind:
		a.appendInt64(int64(m.Get(fd).Uint()))
	case protoreflect.EnumKind:
		a.appendInt64(int64(m.Get(fd).Enum()))
	case protoreflect.StringKind:
		a.appendString(m.Get(fd).String())
	case protoreflect.FloatKind:
		a.appendAny(float32(m.Get(fd).Float()))
	case protoreflect.DoubleKind:
		a.appendAny(m.Get(fd).Float())
	default:
		value := m.Get(fd)
		result, err := scalarToInterface(fd, value)
		if err != nil {
			return err
		}
		a.appendAny(result)
	}
	return nil
}

type emptyKeyStep struct{}

func (s *emptyKeyStep) packInto(_ *tupleAppender, _ *FDBStoredRecord[proto.Message], _ proto.Message) error {
	return nil // empty key appends nothing
}

var errFanOut = fmt.Errorf("fan-out not supported by compiled evaluator")

// compileKeyExpression attempts to compile a KeyExpression into a fast evaluator.
// Returns nil if the expression can't be compiled (fan-out, nesting, etc.).
func compileKeyExpression(expr KeyExpression) *compiledKeyEvaluator {
	switch e := expr.(type) {
	case *CompositeKeyExpression:
		var steps []compiledStep
		for _, child := range e.expressions {
			step := compileStep(child)
			if step == nil {
				return nil
			}
			steps = append(steps, step)
		}
		return &compiledKeyEvaluator{steps: steps, ok: true}
	default:
		step := compileStep(expr)
		if step == nil {
			return nil
		}
		return &compiledKeyEvaluator{steps: []compiledStep{step}, ok: true}
	}
}

func compileStep(expr KeyExpression) compiledStep {
	switch e := expr.(type) {
	case *FieldKeyExpression:
		if e.fanType != FanTypeNone {
			return nil
		}
		return &fieldStep{fieldName: e.fieldName}
	case *RecordTypeKeyExpression:
		if e.nested != nil {
			return nil
		}
		return &recordTypeKeyStep{typeKeys: e.typeKeys}
	case *EmptyKeyExpression:
		return &emptyKeyStep{}
	case *GroupingKeyExpression:
		return compileStep(e.wholeKey)
	case *CompositeKeyExpression:
		// Flatten nested composites
		var steps []compiledStep
		for _, child := range e.expressions {
			step := compileStep(child)
			if step == nil {
				return nil
			}
			steps = append(steps, step)
		}
		// Return a composite step
		return &compositeStep{steps: steps}
	default:
		return nil
	}
}

type compositeStep struct {
	steps []compiledStep
}

func (s *compositeStep) packInto(a *tupleAppender, record *FDBStoredRecord[proto.Message], msg proto.Message) error {
	for _, step := range s.steps {
		if err := step.packInto(a, record, msg); err != nil {
			return err
		}
	}
	return nil
}

// evaluateCompiled evaluates the compiled expression and returns the result as
// a tuple. Uses a reusable tupleAppender to avoid allocations.
func (c *compiledKeyEvaluator) evaluate(appender *tupleAppender, record *FDBStoredRecord[proto.Message], msg proto.Message) error {
	appender.reset()
	for _, step := range c.steps {
		if err := step.packInto(appender, record, msg); err != nil {
			return err
		}
	}
	return nil
}

// packKey evaluates the expression and packs the result with the given subspace prefix.
func (c *compiledKeyEvaluator) packKey(appender *tupleAppender, ss subspace.Subspace, record *FDBStoredRecord[proto.Message], msg proto.Message) ([]byte, error) {
	if err := c.evaluate(appender, record, msg); err != nil {
		return nil, err
	}
	return appender.packWithPrefix(ss.Bytes()), nil
}
