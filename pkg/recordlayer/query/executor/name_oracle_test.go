package executor

// TEST-ONLY name-model oracles. Production has no name-keyed row anywhere —
// every reference bakes its ordinal at plan time, and every runtime row is
// positional. These helpers survive purely as ASSERTION oracles: protoToMap
// exercises the shared proto→row-value conversion through an independent shape,
// and shadowMismatch cross-checks a positional row field-for-field against a
// name-keyed expectation map.

import (
	"reflect"
	"strings"

	"google.golang.org/protobuf/proto"
)

// protoToMap converts a proto.Message to map[string]any with UPPER-case keys.
// Only set fields are included; unset fields are omitted (NULL semantics). An
// EMPTY repeated field is omitted too (protoreflect's Has() reports false for
// an empty list) and so reads back as SQL NULL.
func protoToMap(msg proto.Message) map[string]any {
	if msg == nil {
		return nil
	}
	refl := msg.ProtoReflect()
	desc := refl.Descriptor()
	fields := desc.Fields()
	m := make(map[string]any, fields.Len())
	for i := 0; i < fields.Len(); i++ {
		fd := fields.Get(i)
		if !refl.Has(fd) {
			continue
		}
		key := strings.ToUpper(string(fd.Name()))
		m[key] = protoFieldToGo(fd, refl.Get(fd))
	}
	return m
}

// shadowMismatch returns the name of the first field where the positional row
// DISAGREES with the name-keyed expectation map, or "" if they agree on every
// named field of the row's type. A field the map omits reads as nil on both
// sides (map missing-key = NULL, unset slot = nil), so agreement holds for
// absent fields. Comparison is reflect.DeepEqual so list/bytes/message values
// compare correctly.
func shadowMismatch(row *PositionalRow, m map[string]any) string {
	if row == nil || row.Type == nil {
		return ""
	}
	for i, f := range row.Type.Fields {
		if f.Name == "" {
			continue
		}
		pos, _ := row.Get(i)
		named := m[f.Name] // absent -> nil
		if !reflect.DeepEqual(pos, named) {
			return f.Name
		}
	}
	return ""
}
