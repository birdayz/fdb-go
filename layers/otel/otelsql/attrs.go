package otelsql

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"math"
	"strconv"

	"fdb.dev/layers/otel/otelsql/otelv1pb"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	"google.golang.org/protobuf/proto"
)

// Caps of the canonical attribute set: a key over MaxAttributeKeyBytes, or a
// value whose projection exceeds MaxAttributeValueBytes, is not projected.
const (
	MaxAttributeKeyBytes   = 1024
	MaxAttributeValueBytes = 64000
)

// CanonicalNaN is the NaN a DOUBLE_VALUE stores for every NaN payload, so an
// equality on "NaN" matches them all; the row's OTLP keeps the original bits.
var CanonicalNaN = math.Float64frombits(0x7FF8000000000000)

// ProjectedDouble is v as a DOUBLE_VALUE stores it.
func ProjectedDouble(v float64) float64 {
	if math.IsNaN(v) {
		return CanonicalNaN
	}
	return v
}

// MessageAttributeKeys are the GenAI content attributes, by the attribute code
// a MessageRef carries; index 0 is unused.
var MessageAttributeKeys = [...]string{1: "gen_ai.input.messages", 2: "gen_ai.output.messages", 3: "gen_ai.system_instructions", 4: "gen_ai.tool.definitions"}

// MessageAttributeCode is key's code in MessageAttributeKeys, or 0.
func MessageAttributeCode(key string) int64 {
	for i, k := range MessageAttributeKeys {
		if i > 0 && k == key {
			return int64(i)
		}
	}
	return 0
}

// Projection reports what a row's canonical attribute sets left out of its
// equality indexes, over the indexed scopes (resource, scope and span or log).
type Projection struct {
	// Normalized: an indexed scope's list had nil entries or repeated keys,
	// or left attributes out (caps, excluded GenAI message keys).
	Normalized bool
	// Unindexed counts the keys no equality index holds: capped, excluded,
	// and those whose value is bytes, an array, a kvlist or unset (reported
	// in bytes_attrs and any_attrs, not indexed).
	Unindexed int64
}

// projectionOf is the Projection of a row's indexed scopes' sets.
func projectionOf(sets ...attrSet) Projection {
	var p Projection
	for _, set := range sets {
		p.Normalized = p.Normalized || set.normalized
		p.Unindexed += set.unindexed
		for _, kv := range set.kvs {
			switch kv.GetValue().GetValue().(type) {
			case *commonpb.AnyValue_StringValue, *commonpb.AnyValue_IntValue, *commonpb.AnyValue_DoubleValue, *commonpb.AnyValue_BoolValue:
			default:
				p.Unindexed++
			}
		}
	}
	return p
}

// attrSet is one scope's canonical attribute set: unique keys in first
// appearance order, each with its last value, within the caps.
type attrSet struct {
	kvs        []*commonpb.KeyValue
	normalized bool
	unindexed  int64 // keys left out
}

// canonicalSet is the projection of kvs; exclude drops the GenAI message keys.
// A key whose last value is capped is left out, whatever its earlier values.
func canonicalSet(kvs []*commonpb.KeyValue, exclude bool) attrSet {
	if canonicalAlready(kvs, exclude) {
		return attrSet{kvs: kvs}
	}
	out := attrSet{normalized: true}
	var order []string
	last := make(map[string]*commonpb.KeyValue, len(kvs))
	for _, kv := range kvs {
		if kv == nil {
			continue
		}
		if _, seen := last[kv.Key]; !seen {
			order = append(order, kv.Key)
		}
		last[kv.Key] = kv
	}
	for _, key := range order {
		kv := last[key]
		if !withinCaps(kv) || (exclude && MessageAttributeCode(key) > 0) {
			out.unindexed++
			continue
		}
		out.kvs = append(out.kvs, kv)
	}
	return out
}

// canonicalAlready reports whether kvs is its own canonical set, without
// allocating for the short lists most spans carry.
func canonicalAlready(kvs []*commonpb.KeyValue, exclude bool) bool {
	var seen map[string]bool
	if len(kvs) > 16 {
		seen = make(map[string]bool, len(kvs))
	}
	for i, kv := range kvs {
		if kv == nil || !withinCaps(kv) || (exclude && MessageAttributeCode(kv.Key) > 0) {
			return false
		}
		if seen != nil {
			if seen[kv.Key] {
				return false
			}
			seen[kv.Key] = true
			continue
		}
		for _, prev := range kvs[:i] {
			if prev.Key == kv.Key {
				return false
			}
		}
	}
	return true
}

func withinCaps(kv *commonpb.KeyValue) bool {
	if len(kv.Key) > MaxAttributeKeyBytes {
		return false
	}
	switch v := kv.GetValue().GetValue().(type) {
	case *commonpb.AnyValue_StringValue:
		return len(v.StringValue) <= MaxAttributeValueBytes
	case *commonpb.AnyValue_BytesValue:
		return len(v.BytesValue) <= MaxAttributeValueBytes
	case *commonpb.AnyValue_ArrayValue, *commonpb.AnyValue_KvlistValue:
		return len(CanonicalJSON(kv.GetValue())) <= MaxAttributeValueBytes
	}
	return true
}

// attrLists are a row's or struct's typed attribute arrays as the compiled
// element types; long is false for event and link structs, which have no
// long_str_attrs or bytes_attrs (their long strings and bytes are not
// projected).
type attrLists struct {
	long    bool
	str     []*otelv1pb.STR_ATTR
	longStr []*otelv1pb.LONG_STR_ATTR
	ints    []*otelv1pb.INT_ATTR
	doubles []*otelv1pb.DOUBLE_ATTR
	bools   []*otelv1pb.BOOL_ATTR
	bytes   []*otelv1pb.BYTES_ATTR
	anys    []*otelv1pb.ANY_ATTR
}

// add appends the canonical set of kvs under scope.
func (a *attrLists) add(scope string, set attrSet) {
	for _, kv := range set.kvs {
		key := kv.Key
		switch v := kv.GetValue().GetValue().(type) {
		case *commonpb.AnyValue_StringValue:
			if len(v.StringValue) <= MaxIndexedStringBytes {
				a.str = append(a.str, &otelv1pb.STR_ATTR{SCOPE: proto.String(scope), ATTR_KEY: proto.String(key), STR_VALUE: proto.String(v.StringValue)})
			} else if a.long {
				a.longStr = append(a.longStr, &otelv1pb.LONG_STR_ATTR{
					SCOPE: proto.String(scope), ATTR_KEY: proto.String(key), DIGEST: LongStringDigest(v.StringValue), STR_VALUE: proto.String(v.StringValue),
				})
			}
		case *commonpb.AnyValue_IntValue:
			a.ints = append(a.ints, &otelv1pb.INT_ATTR{SCOPE: proto.String(scope), ATTR_KEY: proto.String(key), INT_VALUE: proto.Int64(v.IntValue)})
		case *commonpb.AnyValue_DoubleValue:
			a.doubles = append(a.doubles, &otelv1pb.DOUBLE_ATTR{SCOPE: proto.String(scope), ATTR_KEY: proto.String(key), DOUBLE_VALUE: proto.Float64(ProjectedDouble(v.DoubleValue))})
		case *commonpb.AnyValue_BoolValue:
			a.bools = append(a.bools, &otelv1pb.BOOL_ATTR{SCOPE: proto.String(scope), ATTR_KEY: proto.String(key), BOOL_VALUE: proto.Bool(v.BoolValue)})
		case *commonpb.AnyValue_BytesValue:
			if a.long {
				a.bytes = append(a.bytes, &otelv1pb.BYTES_ATTR{SCOPE: proto.String(scope), ATTR_KEY: proto.String(key), BYTES_VALUE: presentBytes(v.BytesValue)})
			}
		default:
			a.anys = append(a.anys, &otelv1pb.ANY_ATTR{SCOPE: proto.String(scope), ATTR_KEY: proto.String(key), JSON_VALUE: proto.String(string(CanonicalJSON(kv.GetValue())))})
		}
	}
}

// LongStringDigest is the long_str_attrs digest of s.
func LongStringDigest(s string) []byte {
	sum := sha256.Sum256([]byte(s))
	return sum[:LongStringDigestBytes]
}

// CanonicalJSON is the any_attrs representation of v: deterministic for equal
// OTLP values, keeping array and kvlist order (and repeated keys). Doubles
// are shortest round-trip, with NaN and infinities as strings; bytes are
// standard base64 strings; an unset value is null.
func CanonicalJSON(v *commonpb.AnyValue) []byte { return appendCanonicalJSON(nil, v) }

func appendCanonicalJSON(dst []byte, v *commonpb.AnyValue) []byte {
	switch x := v.GetValue().(type) {
	case *commonpb.AnyValue_StringValue:
		return appendJSONString(dst, x.StringValue)
	case *commonpb.AnyValue_IntValue:
		return strconv.AppendInt(dst, x.IntValue, 10)
	case *commonpb.AnyValue_BoolValue:
		return strconv.AppendBool(dst, x.BoolValue)
	case *commonpb.AnyValue_DoubleValue:
		if math.IsNaN(x.DoubleValue) || math.IsInf(x.DoubleValue, 0) {
			return appendJSONString(dst, FormatDouble(x.DoubleValue))
		}
		return strconv.AppendFloat(dst, x.DoubleValue, 'g', -1, 64)
	case *commonpb.AnyValue_BytesValue:
		return appendJSONString(dst, base64.StdEncoding.EncodeToString(x.BytesValue))
	case *commonpb.AnyValue_ArrayValue:
		dst = append(dst, '[')
		for i, item := range x.ArrayValue.GetValues() {
			if i > 0 {
				dst = append(dst, ',')
			}
			dst = appendCanonicalJSON(dst, item)
		}
		return append(dst, ']')
	case *commonpb.AnyValue_KvlistValue:
		dst = append(dst, '{')
		for i, kv := range x.KvlistValue.GetValues() {
			if i > 0 {
				dst = append(dst, ',')
			}
			dst = appendJSONString(dst, kv.GetKey())
			dst = append(dst, ':')
			dst = appendCanonicalJSON(dst, kv.GetValue())
		}
		return append(dst, '}')
	default:
		return append(dst, "null"...)
	}
}

// FormatDouble is a double's canonical text: shortest round-trip, with NaN,
// +Inf and -Inf by name. It is CanonicalValue's "double" value.
func FormatDouble(f float64) string {
	switch {
	case math.IsNaN(f):
		return "NaN"
	case math.IsInf(f, 1):
		return "+Inf"
	case math.IsInf(f, -1):
		return "-Inf"
	}
	return strconv.FormatFloat(f, 'g', -1, 64)
}

func appendJSONString(dst []byte, s string) []byte {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(s) // strings always encode; invalid UTF-8 becomes U+FFFD.
	return append(dst, bytes.TrimSuffix(buf.Bytes(), []byte("\n"))...)
}
