package cmd

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"google.golang.org/protobuf/types/known/structpb"

	"fdb.dev/pkg/fdbgo/fdb/subspace"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
)

// Typed keyspace tuples (RFC-174 §3.1): the escape hatch for stores
// whose tuple path contains non-string elements — something the
// slash-path syntax cannot express. Accepted as a JSON/YAML array of:
//
//   - strings                 → tuple string element
//   - integers                → int64
//   - non-integral numbers    → float64
//   - {"uuid": "8-4-4-4-12"}  → tuple.UUID
//   - {"bytes_hex": "…"}      → []byte
//
// Shared by the --keyspace-tuple flag (JSON string) and the config's
// keyspace_tuple field (structpb.ListValue via protoyaml). Note the
// number path goes through float64 in both decoders, so integers with
// magnitude above 2^53 are not representable here — use nothing this
// syntax for such stores (no known ones exist; keyspace elements are
// small discriminators in practice).

// tupleFromJSON parses the --keyspace-tuple flag value.
func tupleFromJSON(raw string) (tuple.Tuple, error) {
	dec := json.NewDecoder(bytes.NewReader([]byte(raw)))
	dec.UseNumber()
	var arr []any
	if err := dec.Decode(&arr); err != nil {
		return nil, fmt.Errorf("keyspace tuple must be a JSON array (e.g. '[\"myapp\", 42]'): %w", err)
	}
	return tupleFromAny(arr)
}

// tupleFromListValue converts the config's keyspace_tuple field.
func tupleFromListValue(lv *structpb.ListValue) (tuple.Tuple, error) {
	return tupleFromAny(lv.AsSlice())
}

func tupleFromAny(arr []any) (tuple.Tuple, error) {
	if len(arr) == 0 {
		return nil, fmt.Errorf("keyspace tuple must have at least one element")
	}
	t := make(tuple.Tuple, len(arr))
	for i, e := range arr {
		elem, err := tupleElementFromAny(e)
		if err != nil {
			return nil, fmt.Errorf("keyspace tuple element %d: %w", i, err)
		}
		t[i] = elem
	}
	return t, nil
}

func tupleElementFromAny(e any) (tuple.TupleElement, error) {
	switch v := e.(type) {
	case string:
		return v, nil
	case json.Number:
		if n, err := v.Int64(); err == nil {
			return n, nil
		}
		f, err := v.Float64()
		if err != nil {
			return nil, fmt.Errorf("unparseable number %q", v.String())
		}
		return f, nil
	case float64:
		// structpb numbers arrive as float64; render integral values as
		// int64 (the tuple layer's integer representation).
		if v == float64(int64(v)) {
			return int64(v), nil
		}
		return v, nil
	case map[string]any:
		return taggedTupleElement(v)
	default:
		return nil, fmt.Errorf("unsupported type %T (want string, number, {\"uuid\": …}, or {\"bytes_hex\": …})", e)
	}
}

func taggedTupleElement(m map[string]any) (tuple.TupleElement, error) {
	if len(m) != 1 {
		return nil, fmt.Errorf("tagged element must have exactly one key (uuid or bytes_hex), got %d", len(m))
	}
	for k, raw := range m {
		s, ok := raw.(string)
		if !ok {
			return nil, fmt.Errorf("%s value must be a string, got %T", k, raw)
		}
		switch k {
		case "uuid":
			cleaned := strings.ReplaceAll(s, "-", "")
			b, err := hex.DecodeString(cleaned)
			if err != nil || len(b) != 16 {
				return nil, fmt.Errorf("uuid %q is not a valid 16-byte UUID", s)
			}
			var u tuple.UUID
			copy(u[:], b)
			return u, nil
		case "bytes_hex":
			b, err := hex.DecodeString(s)
			if err != nil {
				return nil, fmt.Errorf("bytes_hex %q is not valid hex: %w", s, err)
			}
			return b, nil
		default:
			return nil, fmt.Errorf("unknown tag %q (want uuid or bytes_hex)", k)
		}
	}
	return nil, fmt.Errorf("unreachable")
}

// subspaceFromTuple packs a parsed keyspace tuple into a subspace.
func subspaceFromTuple(t tuple.Tuple) subspace.Subspace {
	return subspace.Sub(t...)
}

// tupleToJSON renders a keyspace tuple in the exact syntax tupleFromJSON
// accepts — compact (no spaces) so an operator can type it back at
// `store truncate`'s confirmation gate. Inverse of tupleFromJSON for
// every element type that parser produces.
func tupleToJSON(t tuple.Tuple) string {
	elems := make([]string, len(t))
	for i, e := range t {
		elems[i] = tupleElementToJSON(e)
	}
	return "[" + strings.Join(elems, ",") + "]"
}

func tupleElementToJSON(e tuple.TupleElement) string {
	switch v := e.(type) {
	case string:
		b, _ := json.Marshal(v)
		return string(b)
	case int64:
		return strconv.FormatInt(v, 10)
	case float64:
		return strconv.FormatFloat(v, 'g', -1, 64)
	case tuple.UUID:
		return fmt.Sprintf(`{"uuid":"%x-%x-%x-%x-%x"}`, v[0:4], v[4:6], v[6:8], v[8:10], v[10:16])
	case []byte:
		return fmt.Sprintf(`{"bytes_hex":"%s"}`, hex.EncodeToString(v))
	default:
		// Unreachable for tuples built by tupleFromAny; keep errors
		// readable if a future element type slips through.
		return fmt.Sprintf("%v", v)
	}
}
