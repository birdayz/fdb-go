package plugin

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/grafana/grafana-plugin-sdk-go/data"
	"github.com/grafana/grafana-plugin-sdk-go/data/sqlutil"

	"fdb.dev/pkg/relational/api"
)

// testStruct is an api.Struct with named attributes in declared order.
type testStruct struct {
	names []string
	vals  []any
}

func (s testStruct) MetaData() api.StructMetaData        { return testStructMD(s) }
func (s testStruct) AttributeCount() int                 { return len(s.vals) }
func (s testStruct) Attribute(i int) (any, error)        { return s.vals[i-1], nil }
func (s testStruct) AttributeByName(string) (any, error) { return nil, errors.New("unused") }
func (s testStruct) Attributes() []any                   { return s.vals }

type testStructMD testStruct

func (m testStructMD) TypeName() string                            { return "S" }
func (m testStructMD) AttributeCount() int                         { return len(m.names) }
func (m testStructMD) AttributeName(i int) (string, error)         { return m.names[i-1], nil }
func (m testStructMD) AttributeType(int) (int, error)              { return 0, nil }
func (m testStructMD) AttributeTypeName(int) (string, error)       { return "", nil }
func (m testStructMD) AttributeDataType(int) (api.DataType, error) { return nil, nil }
func (m testStructMD) AttributeNullable(int) (int, error)          { return 0, nil }

func conv(t *testing.T, f func(any) (any, error), v any) any {
	t.Helper()
	out, err := f(&v)
	if err != nil {
		t.Fatalf("convert %v: %v", v, err)
	}
	return out
}

func TestConvertTime(t *testing.T) {
	t.Parallel()
	ns := int64(1_704_164_645_006_007_008)
	if got := conv(t, convertTime, ns).(*time.Time); !got.Equal(time.Unix(0, ns)) || got.Location() != time.UTC {
		t.Fatalf("ns: %v", got)
	}
	if got := conv(t, convertTime, int32(5)).(*time.Time); got.UnixNano() != 5 {
		t.Fatalf("int32: %v", got)
	}
	if got := conv(t, convertTime, nil).(*time.Time); got != nil {
		t.Fatalf("null: %v", got)
	}
	if got := conv(t, convertTime, "2024-01-02 03:04:05").(*time.Time); !got.Equal(time.Date(2024, 1, 2, 3, 4, 5, 0, time.UTC)) {
		t.Fatalf("timestamp text: %v", got)
	}
	for _, bad := range []any{"yesterday", 1.5, []byte{1}} {
		v := bad
		if _, err := convertTime(&v); !errors.Is(err, sqlutil.ErrorUnexpectedTypeConversion) {
			t.Errorf("%v: got %v", bad, err)
		}
	}
}

func TestConvertBytes(t *testing.T) {
	t.Parallel()
	if got := *conv(t, convertBytes, []byte{0x00, 0xAB, 0xff}).(*string); got != "00abff" {
		t.Fatalf("hex %q", got)
	}
	if got := conv(t, convertBytes, nil).(*string); got != nil {
		t.Fatalf("null %v", got)
	}
	exact := *conv(t, convertBytes, make([]byte, maxHexChars/2)).(*string)
	if len(exact) != maxHexChars || strings.HasSuffix(exact, truncationMark) {
		t.Fatalf("2 KiB value must render whole: %d", len(exact))
	}
	big := *conv(t, convertBytes, make([]byte, maxHexChars/2+1)).(*string)
	if big != strings.Repeat("0", maxHexChars)+truncationMark {
		t.Fatalf("cap: len %d", len(big))
	}
	v := any(42)
	if _, err := convertBytes(&v); err == nil {
		t.Fatal("int accepted as bytes")
	}
}

func TestConvertJSON(t *testing.T) {
	t.Parallel()
	nested := testStruct{
		names: []string{"z_first", "a_second", "bytes", "nan", "arr", "when"},
		vals:  []any{int64(1), "s", []byte{0xde, 0xad}, math.NaN(), []any{testStruct{names: []string{"k"}, vals: []any{true}}, math.Inf(-1), nil}, time.Date(2024, 1, 2, 3, 4, 5, 6, time.UTC)},
	}
	got := string(*conv(t, convertJSON, nested).(*json.RawMessage))
	want := `{"z_first":1,"a_second":"s","bytes":"dead","nan":"NaN","arr":[{"k":true},"-Inf",null],"when":"2024-01-02T03:04:05.000000006Z"}`
	if got != want {
		t.Fatalf("struct\n got %s\nwant %s", got, want)
	}
	if got := string(*conv(t, convertJSON, []any{1.5, float32(2), "x"}).(*json.RawMessage)); got != `[1.5,2,"x"]` {
		t.Fatalf("array %s", got)
	}
	if got := conv(t, convertJSON, nil).(*json.RawMessage); got != nil {
		t.Fatalf("null %v", got)
	}
	if !json.Valid(*conv(t, convertJSON, []any{math.Inf(1)}).(*json.RawMessage)) {
		t.Fatal("invalid JSON for +Inf")
	}
}

// The converters are chosen by column name and type through sqlutil, as
// sqlds runs them: a BIGINT "time" column becomes a time field so
// long-to-wide works; BINARY becomes hex, ARRAY and STRUCT JSON.
func TestConvertersSelectedByFrameFromRows(t *testing.T) {
	t.Parallel()
	f := &fakeSQL{answer: func(string, string) fakeResult {
		return fakeResult{
			cols:  []string{"time", "id", "attrs", "n", "TIMESTAMP"},
			types: []string{"BIGINT", "BINARY", "ARRAY", "BIGINT", "BIGINT"},
			rows: [][]driver.Value{
				{int64(2_000_000_000), []byte{1, 2}, []any{"a"}, int64(7), int64(3)},
				{nil, nil, nil, nil, nil},
			},
		}
	}}
	conn, _ := f.factory(nil, "fdbsql:///X")
	db := sql.OpenDB(conn)
	defer db.Close()
	rows, err := db.QueryContext(context.Background(), "SELECT")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	frame, err := sqlutil.FrameFromRows(rows, -1, converters()...)
	if err != nil {
		t.Fatal(err)
	}
	wantTypes := []data.FieldType{data.FieldTypeNullableTime, data.FieldTypeNullableString, data.FieldTypeNullableJSON, data.FieldTypeNullableInt64, data.FieldTypeNullableTime}
	for i, ft := range wantTypes {
		if got := frame.Fields[i].Type(); got != ft {
			t.Errorf("field %s: %s, want %s", frame.Fields[i].Name, got, ft)
		}
	}
	if ts := frame.Fields[0].At(0).(*time.Time); ts.Unix() != 2 {
		t.Errorf("time %v", ts)
	}
	if h := frame.Fields[1].At(0).(*string); *h != "0102" {
		t.Errorf("hex %v", *h)
	}
	if j := frame.Fields[2].At(0).(*json.RawMessage); string(*j) != `["a"]` {
		t.Errorf("json %s", *j)
	}
}
