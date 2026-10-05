package recordlayer

import (
	"fmt"
	"unicode/utf8"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
)

// InvalidUTF8StringError is a record whose string field holds bytes that are
// not valid UTF-8. Java's strings are UTF-16 and protobuf-java writes them as
// valid UTF-8, so Java would read these bytes back as different text and
// compute different index keys; the save refuses them. Field is the dotted
// path from the record.
type InvalidUTF8StringError struct {
	Field string
}

func (e *InvalidUTF8StringError) Error() string {
	return fmt.Sprintf("string field %s is not valid UTF-8", e.Field)
}

// stringReach is, for every message type the meta-data's record types reach,
// whether a string field can occur in it at any depth (map keys and values
// included: a map entry is a message).
type stringReach map[protoreflect.MessageDescriptor]bool

func newStringReach(roots ...protoreflect.MessageDescriptor) stringReach {
	return newTypeReach(func(md protoreflect.MessageDescriptor) bool {
		fields := md.Fields()
		for i := 0; i < fields.Len(); i++ {
			if fields.Get(i).Kind() == protoreflect.StringKind {
				return true
			}
		}
		return false
	}, roots...)
}

func (r stringReach) reaches(md protoreflect.MessageDescriptor) bool {
	if v, ok := r[md]; ok {
		return v
	}
	return newStringReach(md)[md]
}

// checkUTF8Strings refuses a record of rt holding a string that is not valid
// UTF-8.
func (rt *RecordType) checkUTF8Strings(record proto.Message) error {
	if record == nil || !rt.reachesString {
		return nil
	}
	return rt.stringReach.check(record.ProtoReflect(), "")
}

func (r stringReach) check(m protoreflect.Message, prefix string) error {
	var err error
	m.Range(func(fd protoreflect.FieldDescriptor, v protoreflect.Value) bool {
		path := prefix + string(fd.Name())
		switch {
		case fd.IsMap():
			if fd.MapKey().Kind() != protoreflect.StringKind && !r.valueReaches(fd.MapValue()) {
				return true
			}
			v.Map().Range(func(k protoreflect.MapKey, mv protoreflect.Value) bool {
				if fd.MapKey().Kind() == protoreflect.StringKind && !utf8.ValidString(k.String()) {
					err = &InvalidUTF8StringError{Field: path}
					return false
				}
				err = r.checkValue(fd.MapValue(), mv, path)
				return err == nil
			})
		case fd.IsList():
			if !r.valueReaches(fd) {
				return true
			}
			list := v.List()
			for i := 0; i < list.Len() && err == nil; i++ {
				err = r.checkValue(fd, list.Get(i), path)
			}
		default:
			err = r.checkValue(fd, v, path)
		}
		return err == nil
	})
	return err
}

func (r stringReach) valueReaches(fd protoreflect.FieldDescriptor) bool {
	if fd.Kind() == protoreflect.StringKind {
		return true
	}
	return fd.Message() != nil && r.reaches(fd.Message())
}

func (r stringReach) checkValue(fd protoreflect.FieldDescriptor, v protoreflect.Value, path string) error {
	switch fd.Kind() {
	case protoreflect.StringKind:
		if !utf8.ValidString(v.String()) {
			return &InvalidUTF8StringError{Field: path}
		}
	case protoreflect.MessageKind, protoreflect.GroupKind:
		if r.reaches(fd.Message()) {
			return r.check(v.Message(), path+".")
		}
	}
	return nil
}
