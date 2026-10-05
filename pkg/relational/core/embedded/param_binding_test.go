package embedded

import (
	"bytes"
	"database/sql/driver"
	"errors"
	"math"
	"strings"
	"testing"

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
