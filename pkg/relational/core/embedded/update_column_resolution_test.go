package embedded

import (
	"slices"
	"testing"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/descriptorpb"

	"fdb.dev/gen"
)

// TestResolveUpdateColumn drives resolveUpdateColumn over Order (order_id,
// flower{type, color}, price, tags, …): the qualified reading first, a nested
// struct field by position, the case fold, and every refusal: a qualifier
// that is not the target, a field of a non-struct, a field of an array, no
// such field.
func TestResolveUpdateColumn(t *testing.T) {
	t.Parallel()
	desc := (&gen.Order{}).ProtoReflect().Descriptor()
	for _, c := range []struct {
		name     string
		segments []string
		ordinals []int
		names    []string
		hits     int
	}{
		{"a column", []string{"price"}, []int{2}, []string{"price"}, 1},
		{"qualified by the target", []string{"Order", "price"}, []int{2}, []string{"price"}, 1},
		// The qualifier names the target, an alias compared exactly (as the
		// SELECT scope compares it): `ORDER.price` in table "Order" names
		// nothing, as `SELECT ORDER.price FROM "Order"` does not.
		{"a qualifier is not folded", []string{"ORDER", "price"}, nil, nil, 0},
		{"a field of a struct column", []string{"flower", "color"}, []int{1, 1}, []string{"flower", "color"}, 1},
		{"a qualified field of a struct column", []string{"Order", "flower", "type"}, []int{1, 0}, []string{"flower", "type"}, 1},
		{"a column folded", []string{"PRICE"}, []int{2}, []string{"price"}, 1},
		{"a qualifier that is not the target", []string{"nosuch", "price"}, nil, nil, 0},
		{"a field of a scalar", []string{"price", "x"}, nil, nil, 0},
		{"a field of an array", []string{"tags", "x"}, nil, nil, 0},
		{"no such field of a struct", []string{"flower", "nosuch"}, nil, nil, 0},
		{"no segments", nil, nil, nil, 0},
	} {
		ordinals, names, hits := resolveUpdateColumn(desc, []string{"Order"}, c.segments)
		if hits != c.hits || !slices.Equal(ordinals, c.ordinals) || !slices.Equal(names, c.names) {
			t.Errorf("%s: resolveUpdateColumn(%q) = %v, %q, %d; want %v, %q, %d",
				c.name, c.segments, ordinals, names, hits, c.ordinals, c.names, c.hits)
		}
	}
	// A target the statement qualifies (`UPDATE T.Order`) is named by the whole
	// identifier: `T.Order.price` qualifies its column, `Order.price` does not,
	// and the doubled reading prepends the name's last segment to the whole
	// name (`Order.T.Order.price`), as Java's withQualifier does.
	for _, c := range []struct {
		name     string
		segments []string
		ordinals []int
		hits     int
	}{
		{"the qualified name", []string{"T", "Order", "price"}, []int{2}, 1},
		{"the qualified name's last segment alone", []string{"Order", "price"}, nil, 0},
		{"a nested path through the qualified name", []string{"T", "Order", "flower", "color"}, []int{1, 1}, 1},
		{"the doubled reading of a qualified name", []string{"Order", "T", "Order", "price"}, []int{2}, 1},
		{"the doubled reading is top-level only", []string{"Order", "T", "Order", "flower", "color"}, nil, 0},
		{"the bare column", []string{"price"}, []int{2}, 1},
	} {
		ordinals, _, hits := resolveUpdateColumn(desc, []string{"T", "Order"}, c.segments)
		if hits != c.hits || !slices.Equal(ordinals, c.ordinals) {
			t.Errorf("qualified target, %s: resolveUpdateColumn(%q) = %v, %d; want %v, %d", c.name, c.segments, ordinals, hits, c.ordinals, c.hits)
		}
	}
}

// TestResolveUpdateColumnExactBeforeFold: Java compares names exactly, so both
// readings are tried exactly before either folds case. Table W holds a
// top-level F and a struct column "w" whose field is F: `"w".F` is the struct
// column's field (the unqualified exact reading), never the top-level F by
// folding "w" onto the table; `W.F` is the top-level F (the qualified exact
// reading); `W."w".F` the struct's field. Table X holds a struct column spelled
// exactly like it, where both exact readings of `X.F` name a field.
func TestResolveUpdateColumnExactBeforeFold(t *testing.T) {
	t.Parallel()
	optional := descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum()
	int64Type := descriptorpb.FieldDescriptorProto_TYPE_INT64.Enum()
	fdp := &descriptorpb.FileDescriptorProto{
		Name:    proto.String("resolve_update_column_test.proto"),
		Package: proto.String("resolvetest"),
		Syntax:  proto.String("proto2"),
		MessageType: []*descriptorpb.DescriptorProto{
			{Name: proto.String("S"), Field: []*descriptorpb.FieldDescriptorProto{
				{Name: proto.String("F"), Number: proto.Int32(1), Label: optional, Type: int64Type},
			}},
			{Name: proto.String("W"), Field: []*descriptorpb.FieldDescriptorProto{
				{Name: proto.String("ID"), Number: proto.Int32(1), Label: optional, Type: int64Type},
				{Name: proto.String("F"), Number: proto.Int32(2), Label: optional, Type: int64Type},
				{
					Name: proto.String("w"), Number: proto.Int32(3), Label: optional,
					Type: descriptorpb.FieldDescriptorProto_TYPE_MESSAGE.Enum(), TypeName: proto.String(".resolvetest.S"),
				},
			}},
			// Two columns one fold apart, so a doubled column that matches
			// neither exactly folds onto both.
			{Name: proto.String("Z"), Field: []*descriptorpb.FieldDescriptorProto{
				{Name: proto.String("ID"), Number: proto.Int32(1), Label: optional, Type: int64Type},
				{Name: proto.String("Ab"), Number: proto.Int32(2), Label: optional, Type: int64Type},
				{Name: proto.String("aB"), Number: proto.Int32(3), Label: optional, Type: int64Type},
			}},
			// A struct column spelled EXACTLY like its table, so both exact
			// readings of `X.F` name a field.
			{Name: proto.String("X"), Field: []*descriptorpb.FieldDescriptorProto{
				{Name: proto.String("ID"), Number: proto.Int32(1), Label: optional, Type: int64Type},
				{Name: proto.String("F"), Number: proto.Int32(2), Label: optional, Type: int64Type},
				{
					Name: proto.String("X"), Number: proto.Int32(3), Label: optional,
					Type: descriptorpb.FieldDescriptorProto_TYPE_MESSAGE.Enum(), TypeName: proto.String(".resolvetest.S"),
				},
			}},
		},
	}
	file, err := protodesc.NewFile(fdp, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		name     string
		table    string
		segments []string
		ordinals []int
		names    []string
		hits     int
	}{
		{"the struct column spelled like the table", "W", []string{"w", "F"}, []int{2, 0}, []string{"w", "F"}, 1},
		{"qualified by the table", "W", []string{"W", "F"}, []int{1}, []string{"F"}, 1},
		{"qualified, then the struct column", "W", []string{"W", "w", "F"}, []int{2, 0}, []string{"w", "F"}, 1},
		{"folded only when nothing matches exactly", "W", []string{"w", "f"}, []int{2, 0}, []string{"w", "F"}, 1},
		// Folded, `W.f` is the table's F AND the struct column w's F: two
		// candidates, ambiguous, as a fold onto two columns is.
		{"both readings folded is ambiguous", "W", []string{"W", "f"}, nil, nil, 2},
		// Both EXACT readings name a field: the qualified one wins, as Java's
		// resolveIdentifierMaybe tries the qualified lookup first.
		{"exact: the qualified reading first", "X", []string{"X", "F"}, []int{1}, []string{"F"}, 1},
		// The table named twice before a top-level column is Java's qualified
		// reading too (the operator's name prepended to the attribute's
		// table-qualified name), a top-level column only: `W.W.F` is F,
		// `W.W.w.F` names nothing, and in X, whose struct column is named
		// like it, `X.X.F` is both X's F and the struct's F, 42702, as Java.
		{"the table twice, then a column", "W", []string{"W", "W", "F"}, []int{1}, []string{"F"}, 1},
		{"the table twice, then a nested path", "W", []string{"W", "W", "w", "F"}, nil, nil, 0},
		{"the table twice beside a struct column so named", "X", []string{"X", "X", "F"}, nil, nil, 2},
		{"the table twice, then a column only the row has", "X", []string{"X", "X", "ID"}, []int{0}, []string{"ID"}, 1},
		// The fold pass: `W.W.f` names nothing exactly; folded, it is the
		// struct column w's F by the qualified reading and W's F by the
		// doubled one, two candidates. And a doubled column that folds onto
		// two columns counts both.
		{"the doubled reading in the fold pass", "W", []string{"W", "W", "f"}, nil, nil, 2},
		{"the doubled column folding onto two", "Z", []string{"Z", "Z", "AB"}, nil, nil, 2},
		{"the doubled column exact among two folds", "Z", []string{"Z", "Z", "Ab"}, []int{1}, []string{"Ab"}, 1},
	} {
		ordinals, names, hits := resolveUpdateColumn(file.Messages().ByName(protoreflect.Name(c.table)), []string{c.table}, c.segments)
		if hits != c.hits || !slices.Equal(ordinals, c.ordinals) || !slices.Equal(names, c.names) {
			t.Errorf("%s: resolveUpdateColumn(%q) = %v, %q, %d; want %v, %q, %d",
				c.name, c.segments, ordinals, names, hits, c.ordinals, c.names, c.hits)
		}
	}
}
