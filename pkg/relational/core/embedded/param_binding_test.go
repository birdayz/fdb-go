package embedded

import (
	"bytes"
	"database/sql"
	"database/sql/driver"
	"errors"
	"math"
	"strings"
	"testing"
	"time"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/recordlayer/vectorcodec"
	"fdb.dev/pkg/relational/api"
	"fdb.dev/pkg/relational/core/parser"
	"github.com/google/uuid"
)

type textValuer string

func (v textValuer) Value() (driver.Value, error) { return string(v), nil }

func TestParameterConstant_Types(t *testing.T) {
	t.Parallel()
	u := uuid.MustParse("123e4567-e89b-12d3-a456-426614174000")
	one := int64(1)
	for _, c := range []struct {
		name string
		in   any
		code values.TypeCode
	}{
		{"nil", nil, values.TypeCodeNull},
		{"nil pointer", (*int64)(nil), values.TypeCodeNull},
		{"int32", int32(5), values.TypeCodeInt},
		{"int64", int64(5), values.TypeCodeLong},
		{"int fits", 5, values.TypeCodeInt},
		{"int wide", 3000000000, values.TypeCodeLong},
		{"uint64", uint64(5), values.TypeCodeLong},
		{"float32", float32(1.5), values.TypeCodeFloat},
		{"float64", 1.5, values.TypeCodeDouble},
		{"string", "a", values.TypeCodeString},
		{"bool", true, values.TypeCodeBoolean},
		{"bytes", []byte{1}, values.TypeCodeBytes},
		{"uuid", u, values.TypeCodeUuid},
		{"pointer", &one, values.TypeCodeLong},
		{"valuer", textValuer("x"), values.TypeCodeString},
		{"int64 slice", []int64{1, 3}, values.TypeCodeArray},
		{"empty slice", []int64{}, values.TypeCodeArray},
	} {
		v, err := parameterConstant(c.in)
		if err != nil {
			t.Errorf("%s: %v", c.name, err)
			continue
		}
		if got := v.Type().Code(); got != c.code {
			t.Errorf("%s: type %v, want %v", c.name, got, c.code)
		}
	}
	// A NaN payload binds bit-exact: nothing is rendered as text.
	nan := math.Float64frombits(0x7ff8000000000abc)
	v, err := parameterConstant(nan)
	if err != nil || math.Float64bits(v.(*values.ConstantValue).Value.(float64)) != 0x7ff8000000000abc {
		t.Fatalf("NaN payload: %v, %v", v, err)
	}
	if _, err := parameterConstant([]*int64{nil}); err == nil {
		t.Fatal("a NULL array element must be refused")
	}
	if _, err := parameterConstant(uint64(math.MaxUint64)); err == nil {
		t.Fatal("a uint64 beyond LONG must be refused")
	}
	if _, err := parameterConstant(struct{}{}); err == nil {
		t.Fatal("an unsupported type must be refused")
	}
}

func TestBindStatementParameters(t *testing.T) {
	t.Parallel()
	root, err := parser.Parse("SELECT id FROM t WHERE id = ? OR id = ?x OR id = $x OR id = ?")
	if err != nil {
		t.Fatal(err)
	}
	args := []driver.NamedValue{
		{Ordinal: 1, Value: int64(1)},
		{Name: "x", Ordinal: 2, Value: int32(2)},
		{Ordinal: 3, Value: "three"},
		{Ordinal: 4, Value: int64(9)}, // unused: ignored
	}
	key, release, err := bindStatementParameters(root, args)
	if err != nil {
		t.Fatal(err)
	}
	release()
	key2, release2, _ := bindStatementParameters(root, []driver.NamedValue{
		{Ordinal: 1, Value: int32(1)}, {Name: "x", Value: int32(2)}, {Ordinal: 3, Value: "three"},
	})
	release2()
	if key == key2 {
		t.Fatal("a LONG and an INT binding share a plan-cache key")
	}
	var apiErr *api.Error
	if _, _, err := bindStatementParameters(root, args[:1]); !errors.As(err, &apiErr) || apiErr.Code != api.ErrCodeUndefinedParameter {
		t.Fatalf("missing parameter: want 42F02, got %v", err)
	}
}

// A string parameter must be valid UTF-8 wherever it sits — directly, behind
// a pointer or Valuer, or inside an array — and the error names the parameter.
func TestBindStatementParameters_InvalidUTF8(t *testing.T) {
	t.Parallel()
	root, err := parser.Parse("SELECT id FROM t WHERE name = ? OR name = ?label")
	if err != nil {
		t.Fatal(err)
	}
	bad := "ab\xffcd"
	for _, tc := range []struct {
		name string
		args []driver.NamedValue
		want string
	}{
		{"positional", []driver.NamedValue{{Ordinal: 1, Value: bad}, {Name: "label", Value: "ok"}}, "parameter 1"},
		{"named", []driver.NamedValue{{Ordinal: 1, Value: "ok"}, {Name: "label", Value: bad}}, "parameter label"},
		{"pointer", []driver.NamedValue{{Ordinal: 1, Value: &bad}, {Name: "label", Value: "ok"}}, "parameter 1"},
		{"valuer", []driver.NamedValue{{Ordinal: 1, Value: textValuer(bad)}, {Name: "label", Value: "ok"}}, "parameter 1"},
		{"array element", []driver.NamedValue{{Ordinal: 1, Value: "ok"}, {Name: "label", Value: []string{"x", bad}}}, "parameter label"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, release, err := bindStatementParameters(root, tc.args)
			release()
			var apiErr *api.Error
			if !errors.As(err, &apiErr) || apiErr.Code != api.ErrCodeCharacterNotInRepertoire {
				t.Fatalf("want 22021, got %v", err)
			}
			if !strings.Contains(apiErr.Message, tc.want) {
				t.Fatalf("error %q does not name %q", apiErr.Message, tc.want)
			}
		})
	}
	_, release, err := bindStatementParameters(root, []driver.NamedValue{{Ordinal: 1, Value: "naïve"}, {Name: "label", Value: []string{"日本"}}})
	release()
	if err != nil {
		t.Fatalf("valid UTF-8 refused: %v", err)
	}
}

// A bound byte value is copied at bind time: database/sql callers may reuse
// their buffers once the call returns, while the bound constant lives on in
// lazily fetched pages.
func TestParameterConstant_CopiesBuffers(t *testing.T) {
	t.Parallel()
	payload := func(v values.Value) []byte {
		switch p := v.(*values.ConstantValue).Value.(type) {
		case []byte:
			return p
		case []any:
			return p[0].([]byte)
		}
		t.Fatalf("unexpected payload %T", v.(*values.ConstantValue).Value)
		return nil
	}
	for name, mk := range map[string]func() (any, []byte){
		"bytes":         func() (any, []byte) { b := []byte{1, 2, 3}; return b, b },
		"array element": func() (any, []byte) { b := []byte{1, 2, 3}; return [][]byte{b}, b },
		"vector": func() (any, []byte) {
			v := api.Vector(vectorcodec.Serialize([]float64{1, 2}))
			return v, v
		},
	} {
		in, buf := mk()
		v, err := parameterConstant(in)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		want := bytes.Clone(payload(v))
		for i := range buf {
			buf[i] ^= 0xff
		}
		if !bytes.Equal(payload(v), want) {
			t.Errorf("%s: the bound constant follows the caller's reused buffer", name)
		}
	}
}

// Every statement of a batch is bound before any is planned, so a binding
// error in a later statement leaves the earlier ones unapplied.
func TestBindStatementParameters_BatchBindsAllFirst(t *testing.T) {
	t.Parallel()
	root, err := parser.Parse("INSERT INTO t VALUES (?); INSERT INTO t VALUES (?)")
	if err != nil {
		t.Fatal(err)
	}
	_, release, err := bindStatementParameters(root, []driver.NamedValue{{Ordinal: 1, Value: "ok"}, {Ordinal: 2, Value: "\xff"}})
	release()
	var apiErr *api.Error
	if !errors.As(err, &apiErr) || apiErr.Code != api.ErrCodeCharacterNotInRepertoire || !strings.Contains(apiErr.Message, "parameter 2") {
		t.Fatalf("want 22021 naming parameter 2, got %v", err)
	}
}

type blob []byte

// The design's binding order where database/sql's own conversion would erase
// a lane: known types (and pointers to them) before any Valuer, bytes before
// the general array rule, and an array typed from its static element type.
func TestParameterConstant_TypingOrder(t *testing.T) {
	t.Parallel()
	u := uuid.MustParse("123e4567-e89b-12d3-a456-426614174000")
	arrayOf := func(code values.TypeCode) func(values.Type) bool {
		return func(typ values.Type) bool {
			at, ok := typ.(*values.ArrayType)
			return ok && at.ElementType.Code() == code
		}
	}
	is := func(code values.TypeCode) func(values.Type) bool {
		return func(typ values.Type) bool { return typ.Code() == code }
	}
	for _, c := range []struct {
		name string
		in   any
		want func(values.Type) bool
	}{
		{"*uuid.UUID", &u, is(values.TypeCodeUuid)},
		{"nil *uuid.UUID", (*uuid.UUID)(nil), is(values.TypeCodeNull)},
		{"nil value-receiver Valuer pointer", (*textValuer)(nil), is(values.TypeCodeNull)},
		{"[16]byte", [16]byte{1}, is(values.TypeCodeBytes)},
		{"[3]byte", [3]byte{1, 2, 3}, is(values.TypeCodeBytes)},
		{"named []byte", blob{1}, is(values.TypeCodeBytes)},
		{"sql.NullInt64", sql.NullInt64{Int64: 5, Valid: true}, is(values.TypeCodeLong)},
		{"sql.NullInt64 invalid", sql.NullInt64{}, is(values.TypeCodeNull)},
		{"sql.NullInt32", sql.NullInt32{Int32: 5, Valid: true}, is(values.TypeCodeInt)},
		{"sql.Null[int64]", sql.Null[int64]{V: 5, Valid: true}, is(values.TypeCodeLong)},
		{"sql.Null[uuid.UUID]", sql.Null[uuid.UUID]{V: u, Valid: true}, is(values.TypeCodeUuid)},
		{"*sql.NullInt64", &sql.NullInt64{Int64: 5, Valid: true}, is(values.TypeCodeLong)},
		{"empty []int", []int{}, is(values.TypeCodeNone)},
		{"empty []any", []any{}, is(values.TypeCodeNone)},
		{"empty []int32", []int32{}, arrayOf(values.TypeCodeInt)},
		{"nil []string", []string(nil), arrayOf(values.TypeCodeString)},
		{"empty []*int64", []*int64{}, arrayOf(values.TypeCodeLong)},
		{"empty [][]byte", [][]byte{}, arrayOf(values.TypeCodeBytes)},
		{"[][2]byte", [][2]byte{{1, 2}}, arrayOf(values.TypeCodeBytes)},
		{"[]int mixed widths", []int{1, 3000000000}, arrayOf(values.TypeCodeLong)},
		{"[]uuid.UUID", []uuid.UUID{u}, arrayOf(values.TypeCodeUuid)},
	} {
		v, err := parameterConstant(c.in)
		if err != nil {
			t.Errorf("%s: %v", c.name, err)
			continue
		}
		if !c.want(v.Type()) {
			t.Errorf("%s: type %v", c.name, v.Type())
		}
	}
	// [16]byte binds its bytes, not a UUID.
	if v, _ := parameterConstant([16]byte{1, 2}); !bytes.Equal(v.(*values.ConstantValue).Value.([]byte), append([]byte{1, 2}, make([]byte, 14)...)) {
		t.Errorf("[16]byte payload: %v", v)
	}
	// An empty array's value is empty, not NULL.
	if v, _ := parameterConstant([]int{}); v.(*values.ConstantValue).Value == nil || len(v.(*values.ConstantValue).Value.([]any)) != 0 {
		t.Errorf("empty []int payload: %#v", v)
	}
}

// A directly nested array and a type with no binding are 22023 naming the
// parameter and its Go type.
func TestBindStatementParameters_UnsupportedTypeNamesParameter(t *testing.T) {
	t.Parallel()
	root, err := parser.Parse("SELECT id FROM t WHERE id IN ? OR id = ?x")
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		args []driver.NamedValue
		want string
	}{
		{[]driver.NamedValue{{Ordinal: 1, Value: [][]int{{1}}}, {Name: "x", Value: 1}}, "parameter 1 has unsupported type [][]int"},
		{[]driver.NamedValue{{Ordinal: 1, Value: []int{1}}, {Name: "x", Value: struct{}{}}}, "parameter x has unsupported type struct {}"},
		{[]driver.NamedValue{{Ordinal: 1, Value: []any{[]int{1}}}, {Name: "x", Value: 1}}, "parameter 1 has unsupported type []interface {}"},
	} {
		_, release, err := bindStatementParameters(root, c.args)
		release()
		var apiErr *api.Error
		if !errors.As(err, &apiErr) || apiErr.Code != api.ErrCodeInvalidParameter || apiErr.Message != c.want {
			t.Errorf("want 22023 %q, got %v", c.want, err)
		}
	}
}

// A time binds as the TIMESTAMP value of its instant: canonical UTC text to
// the second, never classified by its own zone's midnight (which once bound
// 2024-01-01T00:00+02:00 as the day '2023-12-31', 22 hours away).
func TestParameterConstant_Time(t *testing.T) {
	t.Parallel()
	plus2 := time.FixedZone("UTC+2", 2*3600)
	minus5 := time.FixedZone("UTC-5", -5*3600)
	at := time.Date(2024, 7, 4, 15, 30, 45, 0, time.UTC)
	for _, c := range []struct {
		name string
		in   any
		want string
	}{
		{"utc", at, "2024-07-04 15:30:45"},
		{"utc midnight", time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC), "2024-01-01 00:00:00"},
		{"midnight east of utc", time.Date(2024, 1, 1, 0, 0, 0, 0, plus2), "2023-12-31 22:00:00"},
		{"midnight west of utc", time.Date(2024, 1, 1, 0, 0, 0, 0, minus5), "2024-01-01 05:00:00"},
		{"evening west of utc", time.Date(2024, 1, 1, 20, 0, 0, 0, minus5), "2024-01-02 01:00:00"},
		{"subsecond dropped", time.Date(2024, 7, 4, 15, 30, 45, 999999999, time.UTC), "2024-07-04 15:30:45"},
		{"domain floor", time.Date(0, 1, 1, 0, 0, 0, 0, time.UTC), "0000-01-01 00:00:00"},
		{"domain ceiling", time.Date(9999, 12, 31, 23, 59, 59, 999999999, time.UTC), "9999-12-31 23:59:59"},
		{"pointer", &at, "2024-07-04 15:30:45"},
		{"sql.NullTime", sql.NullTime{Time: at, Valid: true}, "2024-07-04 15:30:45"},
		{"sql.Null[time.Time]", sql.Null[time.Time]{V: at, Valid: true}, "2024-07-04 15:30:45"},
	} {
		v, err := parameterConstant(c.in)
		if err != nil {
			t.Errorf("%s: %v", c.name, err)
			continue
		}
		cv, ok := v.(*values.ConstantValue)
		if !ok || !cv.Typ.Equals(values.NotNullTimestamp) || cv.Value != c.want {
			t.Errorf("%s: bound %#v, want NOT NULL TIMESTAMP %q", c.name, v, c.want)
		}
	}
	for _, in := range []any{(*time.Time)(nil), sql.NullTime{}} {
		if v, err := parameterConstant(in); err != nil || v.Type().Code() != values.TypeCodeNull {
			t.Errorf("%#v: bound %v, %v; want an untyped NULL", in, v, err)
		}
	}
	// An array of times is ARRAY<TIMESTAMP> from its static element type, an
	// empty one included, each element taking the canonical rule.
	for _, in := range []any{[]time.Time{at, time.Date(2024, 1, 1, 0, 0, 0, 0, plus2)}, []time.Time{}, []*time.Time{&at}} {
		v, err := parameterConstant(in)
		if err != nil {
			t.Fatalf("%#v: %v", in, err)
		}
		arr, ok := v.Type().(*values.ArrayType)
		if !ok || !arr.ElementType.Equals(values.NotNullTimestamp) {
			t.Errorf("%#v: type %v, want ARRAY<TIMESTAMP>", in, v.Type())
		}
	}
	v, _ := parameterConstant([]time.Time{at, time.Date(2024, 1, 1, 0, 0, 0, 0, plus2)})
	if got := v.(*values.ConstantValue).Value.([]any); got[0] != "2024-07-04 15:30:45" || got[1] != "2023-12-31 22:00:00" {
		t.Errorf("[]time.Time payload: %v", got)
	}
}

// A time whose UTC year is outside 0000-9999 is refused at bind with 22008
// naming the parameter, as an array element too: its text would neither
// parse back nor sort by instant.
func TestBindStatementParameters_TimeOutOfDomain(t *testing.T) {
	t.Parallel()
	root, err := parser.Parse("SELECT id FROM t WHERE ts = ? OR ts IN ?when")
	if err != nil {
		t.Fatal(err)
	}
	ok := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	year10000 := time.Date(9999, 12, 31, 23, 0, 0, 0, time.FixedZone("UTC-5", -5*3600))
	yearMinus1 := time.Date(0, 1, 1, 0, 30, 0, 0, time.FixedZone("UTC+1", 3600))
	for _, c := range []struct {
		name string
		args []driver.NamedValue
		want string
	}{
		{"year 10000 in utc", []driver.NamedValue{{Ordinal: 1, Value: year10000}, {Name: "when", Value: []time.Time{ok}}}, "parameter 1"},
		{"year -1 in utc", []driver.NamedValue{{Ordinal: 1, Value: yearMinus1}, {Name: "when", Value: []time.Time{ok}}}, "parameter 1"},
		{"pointer", []driver.NamedValue{{Ordinal: 1, Value: &year10000}, {Name: "when", Value: []time.Time{ok}}}, "parameter 1"},
		{"array element", []driver.NamedValue{{Ordinal: 1, Value: ok}, {Name: "when", Value: []time.Time{ok, year10000}}}, "parameter when"},
	} {
		_, release, err := bindStatementParameters(root, c.args)
		release()
		var apiErr *api.Error
		if !errors.As(err, &apiErr) || apiErr.Code != api.ErrCodeDatetimeFieldOverflow || !strings.Contains(apiErr.Message, c.want) {
			t.Errorf("%s: want 22008 naming %q, got %v", c.name, c.want, err)
		}
	}
}
