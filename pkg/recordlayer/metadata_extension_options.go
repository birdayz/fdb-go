package recordlayer

import (
	"fmt"

	"fdb.dev/gen"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
)

// The extension options Java's in-code setRecords reads from a records file
// (RecordMetaDataBuilder.setRecords(FileDescriptor) builds with
// processExtensionOptions true; its loader of stored meta-data with it false).
// A Go program over a records file that carries them builds the meta-data a
// Java program over the same file builds: the record type keys, since versions,
// primary keys and indexes the options declare, and the schema's record
// options. Each refusal is recorded as a builder fault in program order, as
// Java throws it from setRecords.

// processSchemaOptions is Java's processSchemaOptions (RecordMetaDataBuilder.
// java:161-174): the file's (schema) split_long_records and
// store_record_versions, set without a version bump, as Java assigns the
// fields.
func (b *RecordMetaDataBuilder) processSchemaOptions(fd protoreflect.FileDescriptor) {
	schema, ok := messageExtension(fd.Options(), gen.E_Schema).(*gen.SchemaOptions)
	if !ok || schema == nil {
		return
	}
	if schema.SplitLongRecords != nil {
		b.splitLongRecords = schema.GetSplitLongRecords()
	}
	if schema.StoreRecordVersions != nil {
		b.storeRecordVersions = schema.GetStoreRecordVersions()
	}
}

// processRecordTypeOptions is the extension half of Java's processRecordType
// (RecordMetaDataBuilder.java:890-908): the message's (record) since_version
// and record_type_key, then each field's (field) options.
func (b *RecordMetaDataBuilder) processRecordTypeOptions(rt *RecordType) {
	if opts, ok := messageExtension(rt.Descriptor.Options(), gen.E_Record).(*gen.RecordTypeOptions); ok && opts != nil {
		if opts.SinceVersion != nil {
			rt.SinceVersion = int(opts.GetSinceVersion())
		}
		if opts.RecordTypeKey != nil {
			key, err := valueFromProto(opts.GetRecordTypeKey())
			if err != nil {
				b.recordBuildError(err)
			} else {
				(&RecordTypeBuilder{recordType: rt, builder: b}).SetRecordTypeKey(key)
			}
		}
	}
	fields := rt.Descriptor.Fields()
	for i := 0; i < fields.Len(); i++ {
		fd := fields.Get(i)
		if fo, ok := messageExtension(fd.Options(), gen.E_Field).(*gen.FieldOptions); ok && fo != nil {
			b.processFieldOptions(rt, fd, fo)
		}
	}
}

// processFieldOptions is Java's protoFieldOptions (RecordMetaDataBuilder.java:
// 922-957): an index option adds an index named Type$field over the field (a
// RANK over it ungrouped); a primary key option sets the primary key, once, on
// a field that is not repeated.
func (b *RecordMetaDataBuilder) processFieldOptions(rt *RecordType, fd protoreflect.FieldDescriptor, fo *gen.FieldOptions) {
	switch {
	case fo.Index != nil || fo.Indexed != nil:
		var typ string
		var unique bool
		var options []*gen.Index_Option
		if fo.Index != nil {
			typ = fo.GetIndex().GetType()
			unique = fo.GetIndex().GetUnique()
			options = fo.GetIndex().GetOptions()
		} else {
			typ = legacyIndexTypeToType(fo.GetIndexed())
			unique = legacyIndexTypeIsUnique(fo.GetIndexed())
		}
		field, err := fieldFromDescriptor(rt.Descriptor, fd)
		if err != nil {
			b.recordBuildError(err)
			return
		}
		var expr KeyExpression = field
		if typ == IndexTypeRank {
			expr = Ungrouped(field)
		}
		idx := NewIndex(rt.Name+"$"+string(fd.Name()), expr)
		idx.Type = typ
		// Index.buildOptions (Index.java:253-266): the unique option first,
		// then the listed ones, a key given twice refused as Guava refuses it.
		if unique {
			idx.SetOption(IndexOptionUnique, "true")
		}
		for _, opt := range options {
			if prev, dup := idx.Options[opt.GetKey()]; dup {
				b.recordBuildError(&DuplicateIndexOptionError{Key: opt.GetKey(), First: prev, Second: opt.GetValue()})
				return
			}
			idx.SetOption(opt.GetKey(), opt.GetValue())
		}
		b.AddIndex(rt.Name, idx)
	case fo.GetPrimaryKey():
		if rt.PrimaryKey != nil {
			b.recordBuildError(&MetaDataError{Message: fmt.Sprintf(
				"Only one primary key per record type is allowed have: %s; adding on %s", javaKeyExpressionString(rt.PrimaryKey), fd.Name())})
			return
		}
		if fd.Cardinality() == protoreflect.Repeated {
			b.recordBuildError(&MetaDataError{Message: "Primary key cannot be set on a repeated field"})
			return
		}
		field, err := fieldFromDescriptor(rt.Descriptor, fd)
		if err != nil {
			b.recordBuildError(err)
			return
		}
		rt.PrimaryKey = field
	}
}

// fieldFromDescriptor is Java's Key.Expressions.fromDescriptor (Key.java:
// 352-357): the field, fanned out when it is repeated, validated as a scalar.
func fieldFromDescriptor(desc protoreflect.MessageDescriptor, fd protoreflect.FieldDescriptor) (*FieldKeyExpression, error) {
	field := &FieldKeyExpression{fieldName: string(fd.Name()), fanType: FanTypeNone}
	if fd.Cardinality() == protoreflect.Repeated {
		field.fanType = FanTypeFanOut
	}
	if err := validateFieldKeyExpression(field, desc, false); err != nil {
		return nil, err
	}
	return field, nil
}

// javaKeyExpressionString is Java's toString of a key expression set from a
// field option, the one kind a second primary key option can meet: a
// FieldKeyExpression, "Field { 'name' FanType}". Anything else renders as Go's
// %v.
func javaKeyExpressionString(expr KeyExpression) string {
	if f, ok := expr.(*FieldKeyExpression); ok {
		return fmt.Sprintf("Field { '%s' %s}", f.fieldName, javaFanTypeName(f.fanType))
	}
	return fmt.Sprintf("%v", expr)
}

// messageExtension is the extension xt of a descriptor's options, or nil when
// the options are absent or do not carry it (Java's getExtension returns the
// default instance there, whose options all read as unset).
func messageExtension(options proto.Message, xt protoreflect.ExtensionType) any {
	if options == nil || !options.ProtoReflect().IsValid() || !proto.HasExtension(options, xt) {
		return nil
	}
	return proto.GetExtension(options, xt)
}
