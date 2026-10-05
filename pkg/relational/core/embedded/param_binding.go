package embedded

import (
	"bytes"
	"database/sql/driver"
	"errors"
	"fmt"
	"math"
	"reflect"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/recordlayer/vectorcodec"
	"fdb.dev/pkg/relational/api"
	antlrgen "fdb.dev/pkg/relational/core/parser/gen"
	"fdb.dev/pkg/relational/core/query/expr"
	"github.com/antlr4-go/antlr/v4"
	"github.com/google/uuid"
)

// bindStatementParameters binds every prepared-statement parameter of tree to
// a constant typed from its driver value, as Java's embedded JDBC does
// (Type.fromObject, MutablePlanGenerationContext.java:473-495): a positional
// `?` takes the next unnamed argument, a named `?x` or `$x` the argument named
// x (repeatable, mixable with positional ones). A missing value is 42F02; an
// unused argument is ignored. It returns the bindings' rendering for the plan
// cache key, and a release that removes the bindings.
func bindStatementParameters(tree antlr.Tree, args []driver.NamedValue) (string, func(), error) {
	var positional []any
	named := map[string]any{}
	for _, a := range args {
		if a.Name != "" {
			named[strings.ToUpper(a.Name)] = a.Value
		} else {
			positional = append(positional, a.Value)
		}
	}
	var bound []antlr.Token
	release := func() {
		for _, tok := range bound {
			expr.UnbindParameter(tok)
		}
	}
	var key strings.Builder
	next := 0
	var walk func(antlr.Tree) error
	walk = func(n antlr.Tree) error {
		// DDL binds nothing; a view refuses its parameters itself.
		if _, ok := n.(*antlrgen.CreateSchemaTemplateStatementContext); ok {
			return nil
		}
		if pp, ok := n.(*antlrgen.PreparedStatementParameterContext); ok {
			var raw any
			var param string
			switch {
			case pp.NAMED_PARAMETER() != nil:
				name := pp.NAMED_PARAMETER().GetText()[1:]
				param = name
				v, ok := named[strings.ToUpper(name)]
				if !ok {
					return api.NewErrorf(api.ErrCodeUndefinedParameter, "No value found for parameter %s", name)
				}
				raw = v
			default:
				if next >= len(positional) {
					return api.NewErrorf(api.ErrCodeUndefinedParameter, "No value found for parameter %d", next+1)
				}
				raw = positional[next]
				next++
				param = strconv.Itoa(next)
			}
			v, err := parameterConstant(raw)
			var badText *invalidUTF8ParameterError
			if errors.As(err, &badText) {
				return api.NewErrorf(api.ErrCodeCharacterNotInRepertoire, "parameter %s is not valid UTF-8", param)
			}
			var unsupported *unsupportedParameterError
			if errors.As(err, &unsupported) {
				return api.NewErrorf(api.ErrCodeInvalidParameter, "parameter %s has unsupported type %T", param, raw)
			}
			var overflow *timeOutOfDomainParameterError
			if errors.As(err, &overflow) {
				return api.NewErrorf(api.ErrCodeDatetimeFieldOverflow,
					"parameter %s is the instant %s, outside the TIMESTAMP years 0000-9999 in UTC", param, overflow.instant)
			}
			if err != nil {
				return err
			}
			expr.BindParameter(pp.GetStart(), v)
			bound = append(bound, pp.GetStart())
			fmt.Fprintf(&key, "\x00%s=", v.Type())
			writeBindingKey(&key, constantPayload(v))
			return nil
		}
		for i := 0; i < n.GetChildCount(); i++ {
			if err := walk(n.GetChild(i)); err != nil {
				return err
			}
		}
		return nil
	}
	if err := walk(tree); err != nil {
		release()
		return "", func() {}, err
	}
	return key.String(), release, nil
}

// constantPayload is a bound constant's carrier; a BOOLEAN binds as a
// BooleanValue, whose payload is its bool (nil for NULL).
func constantPayload(v values.Value) any {
	switch c := v.(type) {
	case *values.ConstantValue:
		return c.Value
	case *values.BooleanValue:
		if c.Value != nil {
			return *c.Value
		}
	}
	return nil
}

// writeBindingKey renders a bound carrier exactly for the plan-cache key: a
// cached plan carries its bound constants, so values that render alike would
// share one plan and run the first one's constant. Each carrier is tagged;
// floats are their bits (NaN payloads and -0.0 distinct); strings and bytes
// are length-prefixed, so NULL, an empty STRING and an empty BYTES differ and no text can
// imitate a delimiter; an array is its count, then its elements.
func writeBindingKey(b *strings.Builder, v any) {
	switch x := v.(type) {
	case nil:
		b.WriteByte('N')
	case bool:
		if x {
			b.WriteString("T")
		} else {
			b.WriteString("F")
		}
	case int64:
		fmt.Fprintf(b, "L%d;", x)
	case float64:
		fmt.Fprintf(b, "D%016x", math.Float64bits(x))
	case float32:
		fmt.Fprintf(b, "R%08x", math.Float32bits(x))
	case string:
		fmt.Fprintf(b, "S%d:%s", len(x), x)
	case []byte:
		fmt.Fprintf(b, "B%d:%s", len(x), x)
	case [16]byte:
		fmt.Fprintf(b, "U%x", x)
	case []any:
		fmt.Fprintf(b, "A%d[", len(x))
		for _, e := range x {
			writeBindingKey(b, e)
		}
		b.WriteByte(']')
	default:
		// Every carrier parameterConstant produces is listed above; another
		// is still keyed by its type and value, never dropped.
		s := fmt.Sprintf("%T:%#v", x, x)
		fmt.Fprintf(b, "?%d:%s", len(s), s)
	}
}

// parameterConstant types a driver value as Java's Type.fromObject types a
// JDBC parameter: int32 is INT and int64 LONG (setInt, setLong), a Go int or
// other narrow integer takes an unsuffixed literal's type, float32 is FLOAT,
// float64 DOUBLE, a UUID is UUID, bytes are BYTES, any other slice an ARRAY of
// its element type, and nil an untyped NULL. Known types, and pointers to
// them, are typed before any Valuer, whose Value would erase their lane.
func parameterConstant(raw any) (values.Value, error) {
	if raw == nil {
		return values.NewNullValue(values.NullType), nil
	}
	if v, ok, err := knownParameter(raw); ok {
		return v, err
	}
	rv := reflect.ValueOf(raw)
	if rv.Kind() == reflect.Pointer && isKnownParameterType(rv.Type().Elem()) {
		if rv.IsNil() {
			return values.NewNullValue(values.NullType), nil
		}
		return parameterConstant(rv.Elem().Interface())
	}
	if vr, ok := raw.(driver.Valuer); ok {
		// As database/sql answers: a nil pointer whose Value has a value
		// receiver is NULL, not a nil dereference.
		if rv.Kind() == reflect.Pointer && rv.IsNil() && rv.Type().Elem().Implements(valuerType) {
			return values.NewNullValue(values.NullType), nil
		}
		dv, err := vr.Value()
		if err != nil {
			return nil, err
		}
		// driver.Value has no int32, so a Valuer's int64 carries no lane.
		if i, ok := dv.(int64); ok {
			return intParameter(i), nil
		}
		return parameterConstant(dv)
	}
	switch rv.Kind() {
	case reflect.Pointer:
		if rv.IsNil() {
			return values.NewNullValue(values.NullType), nil
		}
		return parameterConstant(rv.Elem().Interface())
	case reflect.Int32:
		return &values.ConstantValue{Value: rv.Int(), Typ: values.NotNullInt}, nil
	case reflect.Int64:
		return &values.ConstantValue{Value: rv.Int(), Typ: values.NotNullLong}, nil
	case reflect.Int, reflect.Int8, reflect.Int16:
		return intParameter(rv.Int()), nil
	case reflect.Uint8, reflect.Uint16, reflect.Uint32:
		return intParameter(int64(rv.Uint())), nil
	case reflect.Uint, reflect.Uint64:
		if rv.Uint() > math.MaxInt64 {
			return nil, api.NewErrorf(api.ErrCodeNumericValueOutOfRange, "parameter value %d is out of range for LONG", rv.Uint())
		}
		return &values.ConstantValue{Value: int64(rv.Uint()), Typ: values.NotNullLong}, nil
	case reflect.Float32:
		return &values.ConstantValue{Value: rv.Float(), Typ: values.NotNullFloat}, nil
	case reflect.Float64:
		return &values.ConstantValue{Value: rv.Float(), Typ: values.NotNullDouble}, nil
	case reflect.String:
		if !utf8.ValidString(rv.String()) {
			return nil, &invalidUTF8ParameterError{}
		}
		return &values.ConstantValue{Value: rv.String(), Typ: values.NotNullString}, nil
	case reflect.Bool:
		return values.NewBooleanValue(rv.Bool()), nil
	case reflect.Slice, reflect.Array:
		if rv.Type().Elem().Kind() == reflect.Uint8 {
			// Copied: the caller may reuse the buffer while the constant lives on
			// in lazily fetched pages. A [16]byte is BYTES; a UUID binds as
			// uuid.UUID.
			b := make([]byte, rv.Len())
			reflect.Copy(reflect.ValueOf(b), rv)
			return &values.ConstantValue{Value: b, Typ: values.NotNullBytes}, nil
		}
		return arrayParameter(rv)
	}
	return nil, &unsupportedParameterError{}
}

var valuerType = reflect.TypeFor[driver.Valuer]()

// knownParameter types the values a Valuer would misreport: a UUID (its Value
// is text), a time, a VECTOR, and database/sql's null wrappers, typed by their
// payload (an invalid one is an untyped NULL).
func knownParameter(raw any) (values.Value, bool, error) {
	switch v := raw.(type) {
	case uuid.UUID:
		return &values.ConstantValue{Value: [16]byte(v), Typ: values.NotNullUuid}, true, nil
	case time.Time:
		c, err := timeParameter(v)
		return c, true, err
	case api.Vector:
		_, payload, stride, ok := vectorcodec.Payload(v)
		if !ok || len(payload)%stride != 0 {
			return nil, true, api.NewError(api.ErrCodeInvalidParameter, "invalid serialized VECTOR parameter")
		}
		return &values.ConstantValue{Value: bytes.Clone(v), Typ: values.NewVectorType(false, stride*8, len(payload)/stride)}, true, nil
	}
	if payload, valid, ok := sqlNullPayload(reflect.ValueOf(raw)); ok {
		if !valid {
			return values.NewNullValue(values.NullType), true, nil
		}
		v, err := parameterConstant(payload)
		return v, true, err
	}
	return nil, false, nil
}

// timeParameter binds a time as the TIMESTAMP value of its instant: the
// canonical UTC text, to the second, the carrier every TIMESTAMP value has.
// It is typed by the instant, never by its wall clock (a time at midnight in
// its own zone is not a DATE). Sub-second precision is dropped. An instant
// whose UTC year is outside 0000-9999 has no canonical text that parses back
// or sorts by instant, and is refused (22008).
func timeParameter(t time.Time) (values.Value, error) {
	if year := t.UTC().Year(); year < 0 || year > 9999 {
		return nil, &timeOutOfDomainParameterError{instant: t.UTC().Format(time.RFC3339)}
	}
	return &values.ConstantValue{Value: values.CanonicalTimestampText(t), Typ: values.NotNullTimestamp}, nil
}

func isKnownParameterType(t reflect.Type) bool {
	switch t {
	case reflect.TypeFor[uuid.UUID](), reflect.TypeFor[time.Time](), reflect.TypeFor[api.Vector]():
		return true
	}
	return isSQLNullType(t)
}

// isSQLNullType is database/sql's NullInt64, NullString, ..., and Null[T]: a
// struct of the payload and a Valid flag.
func isSQLNullType(t reflect.Type) bool {
	if t.Kind() != reflect.Struct || t.PkgPath() != "database/sql" || t.NumField() != 2 {
		return false
	}
	valid, ok := t.FieldByName("Valid")
	return ok && valid.Type.Kind() == reflect.Bool && valid.Index[0] == 1
}

func sqlNullPayload(rv reflect.Value) (payload any, valid, ok bool) {
	if !isSQLNullType(rv.Type()) {
		return nil, false, false
	}
	return rv.Field(0).Interface(), rv.Field(1).Bool(), true
}

// unsupportedParameterError marks a value with no SQL type; the binder
// reports it as 22023 naming the parameter and its Go type.
type unsupportedParameterError struct{}

func (*unsupportedParameterError) Error() string { return "unsupported parameter type" }

// timeOutOfDomainParameterError marks a time whose UTC year is outside
// 0000-9999; the binder reports it as 22008 naming the parameter.
type timeOutOfDomainParameterError struct{ instant string }

func (e *timeOutOfDomainParameterError) Error() string {
	return "time parameter " + e.instant + " is outside the years 0000-9999"
}

// invalidUTF8ParameterError marks a string parameter that is not valid UTF-8;
// the binder reports it as 22021 naming the parameter.
type invalidUTF8ParameterError struct{}

func (*invalidUTF8ParameterError) Error() string { return "parameter is not valid UTF-8" }

func intParameter(i int64) values.Value {
	if i >= math.MinInt32 && i <= math.MaxInt32 {
		return &values.ConstantValue{Value: i, Typ: values.NotNullInt}
	}
	return &values.ConstantValue{Value: i, Typ: values.NotNullLong}
}

// arrayParameter types a slice or array as an ARRAY, once: from its static
// element type where that has one (INT for int32, BYTES for []byte), else as
// the maximum of its elements' types, as the target's array constructor
// promotes them ([]int{1, 3000000000} is ARRAY<LONG>). An empty one with no
// static type is the untyped empty array (NONE), which any array column
// accepts. A NULL element is refused, as every ARRAY element is; a directly
// nested array is not supported.
func arrayParameter(rv reflect.Value) (values.Value, error) {
	static, err := staticElementType(rv.Type().Elem())
	if err != nil {
		return nil, err
	}
	elems := make([]any, rv.Len())
	var types []values.Type
	for i := range elems {
		ev, err := parameterConstant(rv.Index(i).Interface())
		if err != nil {
			return nil, err
		}
		// A bool binds as a BooleanValue, not a constant.
		if b, isBool := ev.(*values.BooleanValue); isBool && b.Value != nil {
			elems[i] = *b.Value
			types = append(types, values.NotNullBoolean)
			continue
		}
		c, ok := ev.(*values.ConstantValue)
		if !ok || c.Value == nil {
			return nil, api.NewError(api.ErrCodeUnsupportedOperation, "An ARRAY value cannot have NULL elements")
		}
		if _, nested := c.Typ.(*values.ArrayType); nested || c.Typ.Code() == values.TypeCodeNone {
			return nil, &unsupportedParameterError{}
		}
		elems[i] = c.Value
		types = append(types, c.Typ)
	}
	if static != nil {
		return &values.ConstantValue{Value: elems, Typ: values.NewArrayType(false, static)}, nil
	}
	if len(types) == 0 {
		return &values.ConstantValue{Value: elems, Typ: values.NoneType}, nil
	}
	elemType := values.MaximumTypeOfMany(types...)
	if elemType == nil {
		return nil, api.NewError(api.ErrCodeInvalidParameter, "ARRAY parameter elements have no common type")
	}
	return &values.ConstantValue{Value: elems, Typ: values.NewArrayType(false, values.WithNullability(elemType, false))}, nil
}

// staticElementType is the element type an array of t has whatever its
// values: nil where the type depends on the values (a Go int, an interface, a
// Valuer), and an unsupported type for a nested array or a type with no
// binding. A pointer element is typed by its pointee.
func staticElementType(t reflect.Type) (values.Type, error) {
	if t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	switch t {
	case reflect.TypeFor[uuid.UUID]():
		return values.NotNullUuid, nil
	case reflect.TypeFor[time.Time]():
		return values.NotNullTimestamp, nil
	}
	if isKnownParameterType(t) || t.Implements(valuerType) || reflect.PointerTo(t).Implements(valuerType) {
		return nil, nil
	}
	switch t.Kind() {
	case reflect.Int32:
		return values.NotNullInt, nil
	case reflect.Int64, reflect.Uint, reflect.Uint64:
		return values.NotNullLong, nil
	case reflect.Float32:
		return values.NotNullFloat, nil
	case reflect.Float64:
		return values.NotNullDouble, nil
	case reflect.String:
		return values.NotNullString, nil
	case reflect.Bool:
		return values.NotNullBoolean, nil
	case reflect.Slice, reflect.Array:
		if t.Elem().Kind() == reflect.Uint8 {
			return values.NotNullBytes, nil
		}
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Interface:
		return nil, nil
	}
	return nil, &unsupportedParameterError{}
}
