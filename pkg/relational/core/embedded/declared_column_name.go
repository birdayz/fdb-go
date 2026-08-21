package embedded

import (
	"fdb.dev/pkg/recordlayer/protoname"
	"google.golang.org/protobuf/reflect/protoreflect"
)

// nameIsWholeDeclaredColumn reports whether `name`, in full, is the SQL name of
// a column declared by one of these descriptors.
//
// It exists to answer one question the string cannot: is a dot inside a flat
// column name a QUALIFIER, or part of the column's own declared name? A column
// declared `"a.b"` is stored as the proto field `a__2b` and comes back through
// ToUserIdentifier spelled `a.b`, so `a.b` and `C.NAME` are the same shape to
// any parser — and splitting the first at its dot reports the label `b`, which
// is a name no engine calls that column. Java never faces the question: an
// identifier there is a LIST of parts, and Identifier.withoutQualifier takes
// the last PART.
//
// The SCHEMA is the authority Go has for the same fact, and it is the same one
// star expansion already consults, which is why star reported `a.b` correctly
// while the explicit-projection label did not.
//
// It answers only about WHOLE names. A qualified `C.NAME` is not a declared
// column under that spelling and returns false, which leaves it to the split —
// the right answer, since its dot really is a qualifier.
func nameIsWholeDeclaredColumn(name string, descs []protoreflect.MessageDescriptor) bool {
	if name == "" {
		return false
	}
	for _, d := range descs {
		if d == nil {
			continue
		}
		fields := d.Fields()
		for i := 0; i < fields.Len(); i++ {
			if protoname.ToUserIdentifier(string(fields.Get(i).Name())) == name {
				return true
			}
		}
	}
	return false
}
