// Portions derived from FoundationDB Record Layer (RecordMetaDataBuilder.java,
// MetaDataProtoEditor.java), Copyright 2015-2018 Apple Inc. and the
// FoundationDB project authors
// Licensed under the Apache License, Version 2.0; translated to Go and modified.

package recordlayer

import (
	"fdb.dev/gen"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"
	"google.golang.org/protobuf/types/descriptorpb"
)

// MetaDataProtoOption is a RecordMetaDataBuilder setting Java applies before
// setRecords(MetaData), for RecordMetaDataFromProto.
type MetaDataProtoOption func(*metaDataProtoOptions)

type metaDataProtoOptions struct {
	localFileDescriptor protoreflect.FileDescriptor
	evolutionValidator  *MetaDataEvolutionValidator
}

// WithLocalFileDescriptor is Java's RecordMetaDataBuilder.setLocalFileDescriptor:
// the loaded meta-data's record types and union take their descriptors from
// local (typically the compiled file), once it validates as an evolution of
// the stored records, so records of the compiled types can be saved. Such
// meta-data cannot be written back with ToProto.
func WithLocalFileDescriptor(local protoreflect.FileDescriptor) MetaDataProtoOption {
	return func(o *metaDataProtoOptions) { o.localFileDescriptor = local }
}

// WithEvolutionValidator is Java's RecordMetaDataBuilder.setEvolutionValidator:
// the validator that checks the local file descriptor against the stored one
// (the default instance otherwise).
func WithEvolutionValidator(v *MetaDataEvolutionValidator) MetaDataProtoOption {
	return func(o *metaDataProtoOptions) { o.evolutionValidator = v }
}

// useLocalFileDescriptor is the tail of Java's loadFromProto
// (RecordMetaDataBuilder.java:281-288): the stored union must evolve into the
// local one, whose record types and union fields then replace the stored ones.
func (b *RecordMetaDataBuilder) useLocalFileDescriptor(local protoreflect.FileDescriptor, v *MetaDataEvolutionValidator) error {
	localUnion, err := b.fetchLocalUnionDescriptor(local)
	if err != nil {
		return err
	}
	if v == nil {
		v = DefaultMetaDataEvolutionValidator()
	}
	if b.unionDescriptor != localUnion {
		if _, err := v.unionDescriptorCorrespondence(b.unionDescriptor, localUnion); err != nil {
			return err
		}
	}
	if err := b.updateUnionFieldsAndRecordTypesFromLocal(localUnion); err != nil {
		return err
	}
	b.unionDescriptor = localUnion
	b.usesLocalRecordsDescriptor = true
	return nil
}

// fetchLocalUnionDescriptor is Java's method of the same name
// (RecordMetaDataBuilder.java:559-574): local's union, or one synthesized from
// the stored union when local has none, validated with local as validateRecords does.
func (b *RecordMetaDataBuilder) fetchLocalUnionDescriptor(local protoreflect.FileDescriptor) (protoreflect.MessageDescriptor, error) {
	var union protoreflect.MessageDescriptor
	var err error
	if hasUnion(local) {
		union, err = fetchUnionDescriptor(local)
	} else {
		union, err = b.buildSyntheticUnion(local)
	}
	if err != nil {
		return nil, err
	}
	if err := validateRecordDataTypes(local); err != nil {
		return nil, err
	}
	if err := validateRecordUnion(local, union); err != nil {
		return nil, err
	}
	return union, nil
}

// hasUnion is MetaDataProtoEditor.hasUnion (MetaDataProtoEditor.java:220-228,
// :878-885): a top-level message named RecordTypeUnion or of usage UNION.
func hasUnion(fd protoreflect.FileDescriptor) bool {
	for i := 0; i < fd.Messages().Len(); i++ {
		msg := fd.Messages().Get(i)
		if usage, ok := recordUsage(msg); msg.Name() == relationalUnionName || ok && usage == gen.RecordTypeOptions_UNION {
			return true
		}
	}
	return false
}

// buildSyntheticUnion is Java's method of the same name
// (RecordMetaDataBuilder.java:547-557) over MetaDataProtoEditor.createSyntheticUnion (:851-875):
// a RecordTypeUnion, in a file of its own importing local, holding each stored
// union field retargeted at local's message of the same name.
func (b *RecordMetaDataBuilder) buildSyntheticUnion(local protoreflect.FileDescriptor) (protoreflect.MessageDescriptor, error) {
	base := b.unionDescriptor
	if base.Oneofs().Len() > 0 {
		return nil, &MetaDataError{Message: "Adding record type to oneof is not allowed"}
	}
	union := &descriptorpb.DescriptorProto{Name: proto.String(relationalUnionName)}
	for i := 0; i < base.Fields().Len(); i++ {
		field := base.Fields().Get(i)
		msg := local.Messages().ByName(field.Message().Name())
		if msg == nil {
			return nil, &MetaDataError{Message: "Record type " + string(field.Message().Name()) + " removed"}
		}
		if usage, ok := recordUsage(msg); ok && usage == gen.RecordTypeOptions_NESTED {
			continue
		}
		fp := protodesc.ToFieldDescriptorProto(field)
		fp.TypeName = proto.String("." + string(msg.FullName()))
		union.Field = append(union.Field, fp)
	}
	files := new(protoregistry.Files)
	if err := files.RegisterFile(local); err != nil {
		return nil, &MetaDataError{Message: "cannot build a synthetic union: " + err.Error()}
	}
	fd, err := protodesc.NewFile(&descriptorpb.FileDescriptorProto{
		Name:        proto.String("_synthetic_" + local.Path()),
		Dependency:  []string{local.Path()},
		MessageType: []*descriptorpb.DescriptorProto{union},
	}, files)
	if err != nil {
		return nil, &MetaDataError{Message: "cannot build a synthetic union: " + err.Error()}
	}
	return fetchUnionDescriptor(fd)
}

// updateUnionFieldsAndRecordTypesFromLocal is Java's method of the same name
// (RecordMetaDataBuilder.java:787-815): each stored record type takes the
// local descriptor its union field now holds, keeping its primary key, indexes
// and record type key; a local type with no stored counterpart is not added.
func (b *RecordMetaDataBuilder) updateUnionFieldsAndRecordTypesFromLocal(union protoreflect.MessageDescriptor) error {
	oldByName := b.recordTypes
	oldUnionField := make(map[protoreflect.MessageDescriptor]protoreflect.FieldDescriptor, len(oldByName))
	for _, rt := range oldByName {
		oldUnionField[rt.Descriptor] = rt.UnionFieldDescriptor
	}
	updated := make(map[string]*RecordType, len(oldByName))
	byNewDescriptor := make(map[protoreflect.MessageDescriptor]*RecordType, len(oldByName))
	for i := 0; i < union.Fields().Len(); i++ {
		field := union.Fields().Get(i)
		newDescriptor := field.Message()
		oldDescriptor := findOldDescriptor(b.unionDescriptor, field, union)
		if oldDescriptor == nil {
			continue
		}
		if rt := byNewDescriptor[newDescriptor]; rt != nil {
			// Of two fields of one type, the one numbered as the stored field
			// keeps the record's bytes the same (RecordMetaDataBuilder.java:804-809).
			if old := oldUnionField[oldDescriptor]; old != nil && field.Number() == old.Number() {
				rt.UnionFieldDescriptor = field
			}
			continue
		}
		rt := oldByName[string(oldDescriptor.Name())]
		if rt == nil {
			return &MetaDataError{Message: "Unknown record type for union field " + string(field.Name())}
		}
		rt.Name = string(newDescriptor.Name())
		rt.Descriptor = newDescriptor
		rt.UnionFieldDescriptor = field
		updated[rt.Name] = rt
		byNewDescriptor[newDescriptor] = rt
	}
	// The derived record type key is the smallest union field of the type in
	// the meta-data's union, now the local one (RecordType.java:171-182).
	for newDescriptor, rt := range byNewDescriptor {
		smallest := 0
		for i := 0; i < union.Fields().Len(); i++ {
			if field := union.Fields().Get(i); field.Message() == newDescriptor && (smallest == 0 || int(field.Number()) < smallest) {
				smallest = int(field.Number())
			}
		}
		rt.RecordTypeIndex = smallest
	}
	b.recordTypes = updated
	return nil
}

// findOldDescriptor is Java's method of the same name
// (RecordMetaDataBuilder.java:828-849): the stored type at newField's number,
// else at the number of another new union field of the same type.
func findOldDescriptor(oldUnion protoreflect.MessageDescriptor, newField protoreflect.FieldDescriptor, newUnion protoreflect.MessageDescriptor) protoreflect.MessageDescriptor {
	if old := oldUnion.Fields().ByNumber(newField.Number()); old != nil {
		return old.Message()
	}
	for i := 0; i < newUnion.Fields().Len(); i++ {
		other := newUnion.Fields().Get(i)
		if other.Message() == newField.Message() {
			if old := oldUnion.Fields().ByNumber(other.Number()); old != nil {
				return old.Message()
			}
		}
	}
	return nil
}
