package recordlayer

import (
	"fmt"
	"strings"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/descriptorpb"

	"fdb.dev/gen"
)

// relationalUnionName is Java's RecordMetaDataBuilder.DEFAULT_UNION_NAME
// (RecordMetaDataBuilder.java:101).
const relationalUnionName = "RecordTypeUnion"

// validateRecordDataTypes ports Java's RecordMetaDataBuilder.validateDataTypes
// (RecordMetaDataBuilder.java:640-685), which runs whenever a records descriptor is
// set (validateRecords, :635-638): every field of every message reachable from the
// file's top-level messages must have a type a key expression can encode the way
// Java does. Unsigned types are refused because protobuf-java hands them to the key
// evaluator as SIGNED Integer/Long, so a Go evaluator that read them unsigned would
// write different index and primary-key bytes for values >= 2^31; refusing them is
// what keeps the two engines' bytes from ever meeting on such a field.
func validateRecordDataTypes(fd protoreflect.FileDescriptor) error {
	queue := make([]protoreflect.MessageDescriptor, 0, fd.Messages().Len())
	for i := 0; i < fd.Messages().Len(); i++ {
		queue = append(queue, fd.Messages().Get(i))
	}
	seen := map[protoreflect.FullName]bool{}
	for len(queue) > 0 {
		msg := queue[0]
		queue = queue[1:]
		if seen[msg.FullName()] {
			continue
		}
		seen[msg.FullName()] = true
		fields := msg.Fields()
		for i := 0; i < fields.Len(); i++ {
			field := fields.Get(i)
			switch field.Kind() {
			case protoreflect.Int32Kind, protoreflect.Int64Kind, protoreflect.Sfixed32Kind,
				protoreflect.Sfixed64Kind, protoreflect.Sint32Kind, protoreflect.Sint64Kind,
				protoreflect.BoolKind, protoreflect.StringKind, protoreflect.BytesKind,
				protoreflect.FloatKind, protoreflect.DoubleKind, protoreflect.EnumKind:
			case protoreflect.MessageKind, protoreflect.GroupKind:
				if !seen[field.Message().FullName()] {
					queue = append(queue, field.Message())
				}
			case protoreflect.Fixed32Kind, protoreflect.Fixed64Kind,
				protoreflect.Uint32Kind, protoreflect.Uint64Kind:
				return &MetaDataError{Message: fmt.Sprintf("Field %s in message %s has illegal unsigned type %s",
					field.Name(), msg.FullName(), javaFieldTypeName(field.Kind()))}
			default:
				return &MetaDataError{Message: fmt.Sprintf("Field %s in message %s has unknown type %s",
					field.Name(), msg.FullName(), javaFieldTypeName(field.Kind()))}
			}
		}
	}
	return nil
}

// javaFieldTypeName is Descriptors.FieldDescriptor.Type.name(): the upper-case
// protobuf type name, which is what Java's messages print.
func javaFieldTypeName(k protoreflect.Kind) string {
	return strings.ToUpper(k.String())
}

// validateRecordUnion ports Java's RecordMetaDataBuilder.validateUnion
// (RecordMetaDataBuilder.java:687-747) and raises its FIRST fault, as Java
// throws it: per union field in field order, a non-message field, then a
// repeated one, then the relational union type, then a message whose record
// usage is not RECORD; and then every RECORD-usage message of the file must be a
// union field (the relational union message itself must not be one).
func validateRecordUnion(fd protoreflect.FileDescriptor, union protoreflect.MessageDescriptor) error {
	unionFields := union.Fields()
	for i := 0; i < unionFields.Len(); i++ {
		field := unionFields.Get(i)
		if field.Kind() != protoreflect.MessageKind {
			return &MetaDataError{Message: "Union field " + string(field.Name()) + " is not a message"}
		}
		if field.IsList() {
			return &MetaDataError{Message: "Union field " + string(field.Name()) + " should not be repeated"}
		}
		msg := field.Message()
		if msg.Name() == relationalUnionName {
			return &MetaDataError{Message: "Union message type " + string(msg.Name()) + " cannot be a union field."}
		}
		if usage, ok := recordUsage(msg); ok && usage != gen.RecordTypeOptions_RECORD {
			return &MetaDataError{Message: "Union field " + string(field.Name()) + " has type " +
				string(msg.Name()) + " which is not a record"}
		}
	}
	messages := fd.Messages()
	for i := 0; i < messages.Len(); i++ {
		msg := messages.Get(i)
		usage, ok := recordUsage(msg)
		if !ok || usage != gen.RecordTypeOptions_RECORD {
			continue
		}
		if msg.Name() == relationalUnionName {
			if unionHasMessageType(union, msg) {
				return &MetaDataError{Message: "Union message type " + string(msg.Name()) + " cannot be a union field."}
			}
		} else if !unionHasMessageType(union, msg) {
			return &MetaDataError{Message: "Record message type " + string(msg.Name()) + " must be a union field."}
		}
	}
	return nil
}

// recordUsage reads the (record).usage option, reporting whether it is set
// (Java's recordTypeOptions.hasUsage()).
func recordUsage(msg protoreflect.MessageDescriptor) (gen.RecordTypeOptions_Usage, bool) {
	opts, ok := msg.Options().(*descriptorpb.MessageOptions)
	if !ok || opts == nil || !proto.HasExtension(opts, gen.E_Record) {
		return 0, false
	}
	rto, ok := proto.GetExtension(opts, gen.E_Record).(*gen.RecordTypeOptions)
	if !ok || rto == nil || rto.Usage == nil {
		return 0, false
	}
	return rto.GetUsage(), true
}

// unionHasMessageType is Java's unionHasMessageType (:749-751): descriptor identity,
// which Go's protoreflect gives by full name within one resolved file set.
func unionHasMessageType(union, msg protoreflect.MessageDescriptor) bool {
	fields := union.Fields()
	for i := 0; i < fields.Len(); i++ {
		if f := fields.Get(i); f.Kind() == protoreflect.MessageKind && f.Message().FullName() == msg.FullName() {
			return true
		}
	}
	return false
}

// fetchUnionDescriptor ports Java's RecordMetaDataBuilder.fetchUnionDescriptor
// (RecordMetaDataBuilder.java:322-358), the discovery every path that is NOT
// handed a union name uses (setRecords, and metadata loaded from its proto): the
// union is the one top-level message with (record).usage = UNION, or else the one
// named RecordTypeUnion; two candidates, a RecordTypeUnion with NESTED usage, and
// no candidate at all are MetaDataExceptions with Java's messages.
func fetchUnionDescriptor(fd protoreflect.FileDescriptor) (protoreflect.MessageDescriptor, error) {
	var union protoreflect.MessageDescriptor
	messages := fd.Messages()
	for i := 0; i < messages.Len(); i++ {
		msg := messages.Get(i)
		if u, ok := recordUsage(msg); ok {
			switch u {
			case gen.RecordTypeOptions_UNION:
				if union != nil {
					return nil, &MetaDataError{Message: "Only one union descriptor is allowed"}
				}
				union = msg
				continue
			case gen.RecordTypeOptions_NESTED:
				if msg.Name() == relationalUnionName {
					return nil, &MetaDataError{Message: "Message type " + relationalUnionName + " cannot have NESTED usage"}
				}
				continue
			}
		}
		if msg.Name() == relationalUnionName {
			if union != nil {
				return nil, &MetaDataError{Message: "Only one union descriptor is allowed"}
			}
			union = msg
		}
	}
	if union == nil {
		return nil, &MetaDataError{Message: "Union descriptor is required"}
	}
	return union, nil
}
