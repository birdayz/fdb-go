package values

import (
	"fmt"
	"slices"
	"strings"
)

// CallSiteArguments is Java's CallSiteArguments: the arguments of one function
// invocation, positional or named, with its call-site options and window
// specification. A value is immutable; the With methods return a copy.
type CallSiteArguments struct {
	values  []Value
	names   []string // set for named arguments, parallel to values
	named   bool
	options CallSiteOptions
	window  WindowSpecification
}

// PositionalCallSite is CallSiteArguments.ofPositional.
func PositionalCallSite(values ...Value) CallSiteArguments {
	return CallSiteArguments{values: slices.Clone(values)}
}

// NamedCallSite is CallSiteArguments.ofNamed, in the given order.
func NamedCallSite(names []string, values []Value) CallSiteArguments {
	return CallSiteArguments{values: slices.Clone(values), names: slices.Clone(names), named: true}
}

func (a CallSiteArguments) Arguments() []Value          { return a.values }
func (a CallSiteArguments) ArgumentNames() []string     { return a.names }
func (a CallSiteArguments) Options() CallSiteOptions    { return a.options }
func (a CallSiteArguments) Window() WindowSpecification { return a.window }
func (a CallSiteArguments) IsNamed() bool               { return a.named }
func (a CallSiteArguments) IsWindowed() bool            { return !a.window.IsNone() }
func (a CallSiteArguments) HasOptions() bool            { return !a.options.IsEmpty() }
func (a CallSiteArguments) Arity() int                  { return len(a.values) }
func (a CallSiteArguments) IsSimple() bool              { return !a.IsWindowed() && !a.HasOptions() }
func (a CallSiteArguments) WithOptions(o CallSiteOptions) CallSiteArguments {
	a.options = o
	return a
}

func (a CallSiteArguments) WithWindow(w WindowSpecification) CallSiteArguments {
	a.window = w
	return a
}

// WindowSpecification is CallSiteArguments.WindowSpecification.
type WindowSpecification struct {
	Partitioning []Value
	Ordering     []WindowOrderingPart
}

// WindowOrderingPart is Java's WindowOrderingPart: an ordering value and its
// requested direction.
type WindowOrderingPart struct {
	Value      Value
	Descending bool
	NullsLast  bool
}

func (w WindowSpecification) IsNone() bool { return len(w.Partitioning) == 0 && len(w.Ordering) == 0 }

// CallSiteErrorCode is the SemanticException.ErrorCode a call-site option
// failure raises.
type CallSiteErrorCode int

const (
	// CallSiteIncompatibleType is INCOMPATIBLE_TYPE: a NULL value or one its
	// option cannot take.
	CallSiteIncompatibleType CallSiteErrorCode = iota + 1
	// CallSiteFunctionUndefined is FUNCTION_UNDEFINED_FOR_GIVEN_ARGUMENT_TYPES:
	// a repeated or unsupported option.
	CallSiteFunctionUndefined
)

// CallSiteOptionError is the SemanticException CallSiteArguments raises for
// an option, with its log info.
type CallSiteOptionError struct {
	Code     CallSiteErrorCode
	Function string
	Option   string
	Detail   string
}

func (e *CallSiteOptionError) Error() string {
	if e.Function != "" {
		return fmt.Sprintf("%s (function %s, option %s)", e.Detail, e.Function, e.Option)
	}
	return fmt.Sprintf("%s (option %s)", e.Detail, e.Option)
}

func unexpectedOptionValueType(name string) error {
	return &CallSiteOptionError{Code: CallSiteIncompatibleType, Option: name, Detail: "option value is of an unexpected type"}
}

func nullOptionValue(name string) error {
	return &CallSiteOptionError{Code: CallSiteIncompatibleType, Option: name, Detail: "option value must not be null"}
}

// CallSiteOption is Java's CallSiteArguments.Option: a named option and the
// coercion to its declared type.
type CallSiteOption[T any] struct {
	name   string
	coerce func(name string, raw any) (T, error)
}

func (o CallSiteOption[T]) Name() string { return o.name }

// Coerce is Option.coerce: NULL is refused, a value of the declared type is
// taken as is, anything else goes through the option's coercion.
func (o CallSiteOption[T]) Coerce(raw any) (T, error) {
	var zero T
	if raw == nil {
		return zero, nullOptionValue(o.name)
	}
	if v, ok := raw.(T); ok {
		return v, nil
	}
	return o.coerce(o.name, raw)
}

func (o CallSiteOption[T]) resolve(raw any) (any, error) { return o.Coerce(raw) }

// callSiteOptionResolver is an option of any declared type, as a function's
// supported set holds them.
type callSiteOptionResolver interface {
	Name() string
	resolve(raw any) (any, error)
}

// optionInteger is Option.toLong: only exactly-representable integers.
func optionInteger(name string, raw any) (int64, error) {
	switch v := raw.(type) {
	case int8:
		return int64(v), nil
	case int16:
		return int64(v), nil
	case int32:
		return int64(v), nil
	case int64:
		return v, nil
	case int:
		return int64(v), nil
	}
	return 0, unexpectedOptionValueType(name)
}

// IntegerOption is Option.ofInteger.
func IntegerOption(name string) CallSiteOption[int32] {
	return CallSiteOption[int32]{name: name, coerce: func(name string, raw any) (int32, error) {
		v, err := optionInteger(name, raw)
		if err != nil {
			return 0, err
		}
		if v < -1<<31 || v > 1<<31-1 {
			return 0, &CallSiteOptionError{Code: CallSiteIncompatibleType, Option: name, Detail: "option value is out of range for the option's type"}
		}
		return int32(v), nil
	}}
}

// LongOption is Option.ofLong.
func LongOption(name string) CallSiteOption[int64] {
	return CallSiteOption[int64]{name: name, coerce: optionInteger}
}

// DoubleOption is Option.ofDouble: any number.
func DoubleOption(name string) CallSiteOption[float64] {
	return CallSiteOption[float64]{name: name, coerce: func(name string, raw any) (float64, error) {
		if v, err := optionInteger(name, raw); err == nil {
			return float64(v), nil
		}
		if v, ok := raw.(float32); ok {
			return float64(v), nil
		}
		return 0, unexpectedOptionValueType(name)
	}}
}

// BooleanOption is Option.ofBoolean: only an actual boolean.
func BooleanOption(name string) CallSiteOption[bool] {
	return CallSiteOption[bool]{name: name, coerce: func(name string, _ any) (bool, error) {
		return false, unexpectedOptionValueType(name)
	}}
}

// StringOption is Option.ofString.
func StringOption(name string) CallSiteOption[string] {
	return CallSiteOption[string]{name: name, coerce: func(name string, raw any) (string, error) {
		if s, ok := raw.(fmt.Stringer); ok {
			return s.String(), nil
		}
		return "", unexpectedOptionValueType(name)
	}}
}

// EnumConstant is the value of an enum option: one of its constants, so that
// raw text is never taken for one without the check.
type EnumConstant string

// EnumOption is Option.ofEnum over the given constants: text naming one,
// ignoring case.
func EnumOption(name string, constants ...string) CallSiteOption[EnumConstant] {
	return CallSiteOption[EnumConstant]{name: name, coerce: func(name string, raw any) (EnumConstant, error) {
		if s, ok := raw.(fmt.Stringer); ok {
			raw = s.String()
		}
		if s, ok := raw.(string); ok {
			for _, c := range constants {
				if strings.EqualFold(c, s) {
					return EnumConstant(c), nil
				}
			}
		}
		return "", unexpectedOptionValueType(name)
	}}
}

// CallSiteOptions is CallSiteArguments.Options: raw option values by name, in
// the order given.
type CallSiteOptions struct {
	names  []string
	values map[string]any
}

func (o CallSiteOptions) IsEmpty() bool   { return len(o.names) == 0 }
func (o CallSiteOptions) Names() []string { return o.names }

// GetOption is Options.get: the option's value coerced to its type.
func GetOption[T any](o CallSiteOptions, option CallSiteOption[T]) (T, bool, error) {
	raw, ok := o.values[option.name]
	if !ok {
		var zero T
		return zero, false, nil
	}
	v, err := option.Coerce(raw)
	return v, err == nil, err
}

// Resolve is Options.resolve: every option must be one the function supports,
// and is coerced to that option's type.
func (o CallSiteOptions) Resolve(function string, supported ...callSiteOptionResolver) (CallSiteOptions, error) {
	if o.IsEmpty() {
		return o, nil
	}
	resolved := CallSiteOptions{names: o.names, values: make(map[string]any, len(o.names))}
	for _, name := range o.names {
		i := slices.IndexFunc(supported, func(s callSiteOptionResolver) bool { return s.Name() == name })
		if i < 0 {
			return CallSiteOptions{}, &CallSiteOptionError{
				Code: CallSiteFunctionUndefined, Function: function, Option: name,
				Detail: "unsupported option for function",
			}
		}
		v, err := supported[i].resolve(o.values[name])
		if err != nil {
			return CallSiteOptions{}, err
		}
		resolved.values[name] = v
	}
	return resolved, nil
}

// CallSiteOptionsBuilder is Options.Builder.
type CallSiteOptionsBuilder struct{ options CallSiteOptions }

func NewCallSiteOptionsBuilder() *CallSiteOptionsBuilder {
	return &CallSiteOptionsBuilder{options: CallSiteOptions{values: map[string]any{}}}
}

// PutRaw is Builder.putRaw: NULL and a repeated name are refused.
func (b *CallSiteOptionsBuilder) PutRaw(name string, raw any) error {
	if raw == nil {
		return nullOptionValue(name)
	}
	if _, dup := b.options.values[name]; dup {
		return &CallSiteOptionError{Code: CallSiteFunctionUndefined, Option: name, Detail: "option specified more than once"}
	}
	b.options.names = append(b.options.names, name)
	b.options.values[name] = raw
	return nil
}

func (b *CallSiteOptionsBuilder) Build() CallSiteOptions {
	return CallSiteOptions{names: slices.Clone(b.options.names), values: b.options.values}
}

// RowNumberEfSearch and RowNumberReturnVectors are RowNumberFn's options,
// named after the vector scan options they feed (VectorIndexScanOptions
// HNSW_EF_SEARCH and VECTOR_RETURN_VECTORS).
var (
	RowNumberEfSearch      = IntegerOption("hnswEfSearch")
	RowNumberReturnVectors = BooleanOption("vectorReturnVectors")
)

// EncapsulateRowNumber is RowNumberFn.encapsulate: a built-in function takes
// no named arguments and row_number no positional one; its options are
// resolved against EF_SEARCH and RETURN_VECTORS, and the window gives the
// partitioning and argument values.
func EncapsulateRowNumber(args CallSiteArguments) (*RowNumberValue, error) {
	if args.IsNamed() {
		return nil, fmt.Errorf("built-in functions do not support named argument calling conventions")
	}
	if args.Arity() != 0 {
		return nil, fmt.Errorf("row_number takes no arguments, got %d", args.Arity())
	}
	options, err := args.Options().Resolve("row_number", RowNumberEfSearch, RowNumberReturnVectors)
	if err != nil {
		return nil, err
	}
	var efSearch *int
	if v, ok, err := GetOption(options, RowNumberEfSearch); err != nil {
		return nil, err
	} else if ok {
		n := int(v)
		efSearch = &n
	}
	var returnVectors *bool
	if v, ok, err := GetOption(options, RowNumberReturnVectors); err != nil {
		return nil, err
	} else if ok {
		returnVectors = &v
	}
	window := args.Window()
	ordering := make([]Value, len(window.Ordering))
	for i, part := range window.Ordering {
		ordering[i] = part.Value
	}
	return NewRowNumberValue(window.Partitioning, ordering, efSearch, returnVectors), nil
}
