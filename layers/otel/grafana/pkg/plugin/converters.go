package plugin

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"reflect"
	"time"

	"github.com/grafana/grafana-plugin-sdk-go/data"
	"github.com/grafana/grafana-plugin-sdk-go/data/sqlutil"

	"fdb.dev/pkg/relational/api"
)

// maxHexChars caps a rendered BYTES value (4 KiB of hex = 2 KiB of bytes).
const maxHexChars = 4096

// truncationMark ends a hex rendering cut at maxHexChars.
const truncationMark = "…"

var anyType = reflect.TypeFor[any]()

// converters map driver values Grafana cannot use as-is (RFC §6): BIGINT ns
// columns named time/timestamp become time.Time so long-to-wide works,
// BYTES/BINARY become lowercase hex, ARRAY and STRUCT become JSON. The
// time converters come first: sqlutil uses the first converter that matches.
func converters() []sqlutil.Converter {
	var out []sqlutil.Converter
	for _, name := range []string{"time", "TIME", "timestamp", "TIMESTAMP"} {
		out = append(out, sqlutil.Converter{
			Name:            "ns time column " + name,
			InputScanType:   anyType,
			InputColumnName: name,
			FrameConverter:  sqlutil.FrameConverter{FieldType: data.FieldTypeNullableTime, ConverterFunc: convertTime},
		})
	}
	for _, typ := range []string{"BINARY", "BYTES"} {
		out = append(out, sqlutil.Converter{
			Name:           "hex " + typ,
			InputScanType:  anyType,
			InputTypeName:  typ,
			FrameConverter: sqlutil.FrameConverter{FieldType: data.FieldTypeNullableString, ConverterFunc: convertBytes},
		})
	}
	for _, typ := range []string{"ARRAY", "STRUCT"} {
		out = append(out, sqlutil.Converter{
			Name:           "json " + typ,
			InputScanType:  anyType,
			InputTypeName:  typ,
			FrameConverter: sqlutil.FrameConverter{FieldType: data.FieldTypeNullableJSON, ConverterFunc: convertJSON},
		})
	}
	return out
}

// TIMESTAMP values reach the driver as their canonical text.
var timestampLayouts = []string{"2006-01-02 15:04:05.999999999", time.RFC3339Nano}

func convertTime(in any) (any, error) {
	switch v := (*in.(*any)).(type) {
	case nil:
		return (*time.Time)(nil), nil
	case int64:
		t := time.Unix(0, v).UTC()
		return &t, nil
	case int32:
		t := time.Unix(0, int64(v)).UTC()
		return &t, nil
	case time.Time:
		t := v.UTC()
		return &t, nil
	case string:
		for _, layout := range timestampLayouts {
			if t, err := time.Parse(layout, v); err == nil {
				t = t.UTC()
				return &t, nil
			}
		}
		return nil, fmt.Errorf("%w: %q is neither ns since the epoch nor a timestamp", sqlutil.ErrorUnexpectedTypeConversion, v)
	default:
		return nil, fmt.Errorf("%w: a time column holds %T, want BIGINT ns", sqlutil.ErrorUnexpectedTypeConversion, v)
	}
}

func convertBytes(in any) (any, error) {
	switch v := (*in.(*any)).(type) {
	case nil:
		return (*string)(nil), nil
	case []byte:
		s := hexCapped(v)
		return &s, nil
	case string:
		s := hexCapped([]byte(v))
		return &s, nil
	default:
		return nil, fmt.Errorf("%w: a BYTES column holds %T", sqlutil.ErrorUnexpectedTypeConversion, v)
	}
}

func hexCapped(b []byte) string {
	if len(b)*2 <= maxHexChars {
		return hex.EncodeToString(b)
	}
	return hex.EncodeToString(b[:maxHexChars/2]) + truncationMark
}

func convertJSON(in any) (any, error) {
	v := *in.(*any)
	if v == nil {
		return (*json.RawMessage)(nil), nil
	}
	var buf bytes.Buffer
	if err := writeJSON(&buf, v); err != nil {
		return nil, err
	}
	raw := json.RawMessage(buf.Bytes())
	return &raw, nil
}

// writeJSON renders a driver value: STRUCT as an object in declared field
// order, ARRAY as an array, BYTES as hex, non-finite floats as strings.
func writeJSON(buf *bytes.Buffer, v any) error {
	switch x := v.(type) {
	case nil:
		buf.WriteString("null")
	case api.Struct:
		md := x.MetaData()
		buf.WriteByte('{')
		for i, attr := range x.Attributes() {
			name, err := md.AttributeName(i + 1)
			if err != nil {
				return err
			}
			if i > 0 {
				buf.WriteByte(',')
			}
			if err := writeJSON(buf, name); err != nil {
				return err
			}
			buf.WriteByte(':')
			if err := writeJSON(buf, attr); err != nil {
				return err
			}
		}
		buf.WriteByte('}')
	case api.Array:
		return writeJSON(buf, x.Elements())
	case []any:
		buf.WriteByte('[')
		for i, e := range x {
			if i > 0 {
				buf.WriteByte(',')
			}
			if err := writeJSON(buf, e); err != nil {
				return err
			}
		}
		buf.WriteByte(']')
	case []byte:
		return writeJSON(buf, hexCapped(x))
	case float64:
		return writeFloat(buf, x)
	case float32:
		return writeFloat(buf, float64(x))
	case time.Time:
		return writeJSON(buf, x.UTC().Format(time.RFC3339Nano))
	default:
		b, err := json.Marshal(x)
		if err != nil {
			b, _ = json.Marshal(fmt.Sprint(x))
		}
		buf.Write(b)
	}
	return nil
}

func writeFloat(buf *bytes.Buffer, f float64) error {
	switch {
	case math.IsNaN(f):
		buf.WriteString(`"NaN"`)
	case math.IsInf(f, 1):
		buf.WriteString(`"+Inf"`)
	case math.IsInf(f, -1):
		buf.WriteString(`"-Inf"`)
	default:
		b, err := json.Marshal(f)
		if err != nil {
			return err
		}
		buf.Write(b)
	}
	return nil
}
