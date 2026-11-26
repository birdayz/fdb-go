package recordlayer

import (
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"

	"fdb.dev/internal/recordlayer/rlwire"
)

// InvalidUTF8StringError is a record whose string field holds bytes that are
// not valid UTF-8. Java's strings are UTF-16 and protobuf-java writes them as
// valid UTF-8, so Java would read these bytes back as different text and
// compute different index keys; the save refuses them. Field is the dotted
// path from the record.
type InvalidUTF8StringError = rlwire.InvalidUTF8StringError

type (
	recordWire  = rlwire.RecordWire
	mapReach    = rlwire.MapReach
	stringReach = rlwire.StringReach
)

func newMapReach(roots ...protoreflect.MessageDescriptor) mapReach {
	return rlwire.NewMapReach(roots...)
}

func newStringReach(roots ...protoreflect.MessageDescriptor) stringReach {
	return rlwire.NewStringReach(roots...)
}

func newTypeReach(holds func(protoreflect.MessageDescriptor) bool, roots ...protoreflect.MessageDescriptor) map[protoreflect.MessageDescriptor]bool {
	return rlwire.NewTypeReach(holds, roots...)
}

func partialRecordUnmarshaler(md protoreflect.MessageDescriptor, enums *closedEnumReach) rlwire.PartialUnmarshalFunc {
	if enums == nil {
		enums = dynamicReach(md)
	} else if _, ok := enums.reach[md]; !ok {
		enums = dynamicReach(md)
	}
	return func(b []byte, m proto.Message) error {
		if _, ok := enums.reach[m.ProtoReflect().Descriptor()]; ok {
			return javaPartialRule.unmarshal(b, m, enums)
		}
		return UnmarshalRecordAsJava(b, m, true)
	}
}

func newRecordWire(rt *RecordType, raw []byte) *recordWire {
	if rt == nil || !rt.reachesMap || len(raw) == 0 {
		return nil
	}
	return rlwire.NewRecordWire(rt.reachesMap, rt.mapReach, raw, partialRecordUnmarshaler(rt.Descriptor, rt.closedEnumReach))
}

func marshalMapRecord(record proto.Message, prior []byte, reach mapReach, enums *closedEnumReach) ([]byte, error) {
	return rlwire.MarshalMapRecord(record, prior, reach, partialRecordUnmarshaler(record.ProtoReflect().Descriptor(), enums))
}

func (rt *RecordType) checkUTF8Strings(record proto.Message) error {
	return rlwire.ValidateUTF8Record(record, rt.reachesString, rt.stringReach)
}

func sortedMapEntries(mp protoreflect.Map, fd protoreflect.FieldDescriptor) []protoreflect.Message {
	return rlwire.SortedMapEntries(mp, fd)
}
