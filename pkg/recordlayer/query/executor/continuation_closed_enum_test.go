package executor

import (
	"bytes"
	"testing"

	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/dynamicpb"

	"fdb.dev/gen"
)

// A buffered row resumes as the first page read it (RFC-257 ws-j-design.md 4g,
// the resumed-page read). A record Java wrote with a closed enum holding an
// undeclared number is read as protobuf-java reads it: the field unset, the
// number an unknown field. A sort or a recursive cursor buffers such a row into
// its continuation; on resume the row's bytes are decoded again, and
// protobuf-go's own decode puts the number back into the field, so a resumed
// page would read the number where the first page read NULL. Both arms of the
// decode are driven: a dynamic row (the metadata resolver's message) and a
// generated one (the registry's).
func TestContValue_ClosedEnumUndeclaredNumberResumesAsRead(t *testing.T) {
	t.Parallel()
	// Field 2 of Flower is its proto2 (closed) Color enum; 9 is not declared.
	undeclared := protowire.AppendVarint(protowire.AppendTag(nil, 2, protowire.VarintType), 9)
	md := (&gen.Flower{}).ProtoReflect().Descriptor()
	color := md.Fields().ByName("color")

	dynamic := dynamicpb.NewMessage(md)
	dynamic.SetUnknown(undeclared)
	generated := &gen.Flower{}
	generated.ProtoReflect().SetUnknown(undeclared)

	resolve := func(string) (protoreflect.MessageDescriptor, error) { return md, nil }
	for _, c := range []struct {
		name string
		row  protoreflect.ProtoMessage
	}{
		{"a dynamic row", dynamic},
		{"a generated row", generated},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			tok, err := appendContValue(nil, c.row)
			if err != nil {
				t.Fatal(err)
			}
			v, rest, err := readContValue(tok)
			if err != nil || len(rest) != 0 {
				t.Fatalf("readContValue: %v, %d trailing bytes", err, len(rest))
			}
			got, err := resolvePendingProtoValues(v, resolve)
			if err != nil {
				t.Fatal(err)
			}
			m, ok := got.(protoreflect.ProtoMessage)
			if !ok {
				t.Fatalf("resumed value is %T", got)
			}
			r := m.ProtoReflect()
			if r.Has(color) {
				t.Errorf("resumed: color holds %d, want it unset as the first page read it", r.Get(color).Enum())
			}
			if !bytes.Equal(r.GetUnknown(), undeclared) {
				t.Errorf("resumed unknown fields %x, want %x", r.GetUnknown(), undeclared)
			}
		})
	}
}
