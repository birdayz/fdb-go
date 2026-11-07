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
	return rt.stringReach.check(record.ProtoReflect())
}

// check walks m; an error's Field is the path below m, which each enclosing
// level prefixes as the error returns (no path is built for valid records).
func (r stringReach) check(m protoreflect.Message) error {
	var err error
	m.Range(func(fd protoreflect.FieldDescriptor, v protoreflect.Value) bool {
		switch {
		case fd.IsMap():
			if fd.MapKey().Kind() != protoreflect.StringKind && !r.valueReaches(fd.MapValue()) {
				return true
			}
			v.Map().Range(func(k protoreflect.MapKey, mv protoreflect.Value) bool {
				if fd.MapKey().Kind() == protoreflect.StringKind && !utf8.ValidString(k.String()) {
					err = &InvalidUTF8StringError{}
					return false
				}
				err = r.checkValue(fd.MapValue(), mv)
				return err == nil
			})
		case fd.IsList():
			if !r.valueReaches(fd) {
				return true
			}
			list := v.List()
			for i := 0; i < list.Len() && err == nil; i++ {
				err = r.checkValue(fd, list.Get(i))
			}
		default:
			err = r.checkValue(fd, v)
		}
		if err != nil {
			if e, ok := err.(*InvalidUTF8StringError); ok {
				if e.Field == "" {
					e.Field = string(fd.Name())
				} else {
					e.Field = string(fd.Name()) + "." + e.Field
				}
			}
			return false
		}
		return true
	})
	return err
}

func (r stringReach) valueReaches(fd protoreflect.FieldDescriptor) bool {
	if fd.Kind() == protoreflect.StringKind {
		return true
	}
	return fd.Message() != nil && r.reaches(fd.Message())
}

// checkValue checks one value of fd; an error's Field is the path below fd.
func (r stringReach) checkValue(fd protoreflect.FieldDescriptor, v protoreflect.Value) error {
	switch fd.Kind() {
	case protoreflect.StringKind:
		if !utf8.ValidString(v.String()) {
			return &InvalidUTF8StringError{}
		}
	case protoreflect.MessageKind, protoreflect.GroupKind:
		if r.reaches(fd.Message()) {
			return r.check(v.Message())
		}
	}
	return nil
}
