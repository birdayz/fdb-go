package embedded

import (
	"errors"
	"testing"

	"github.com/google/uuid"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"

	"fdb.dev/pkg/relational/api"
	"fdb.dev/pkg/relational/core/rowstruct"
)

// directAccessSchema is Java's UniqueIndexTests T4 with the #4243 UUID
// attribute, plus a table with a scalar UUID column.
const directAccessSchema = "CREATE TYPE AS STRUCT ST1(st1_a bigint, st1_b uuid) " +
	"CREATE TABLE T4(t4_p bigint, t4_st1 st1 array, primary key(t4_p)) " +
	"CREATE TABLE TU(tu_p bigint, tu_u uuid, tu_s st1, primary key(tu_p)) " +
	"CREATE TABLE TA(ta_p bigint, ta_a uuid array, primary key(ta_p))"

// TestStructToRecord_UUIDs pins Java's RecordTypeTable.toDynamicMessage with
// #4243: a UUID attribute is written as the two-word UUID message, at the top
// level, in a struct, and in a struct inside an array, and reads back as the
// same UUID.
func TestStructToRecord_UUIDs(t *testing.T) {
	t.Parallel()
	tmpl, err := buildSchemaTemplateFromDDL(directAccessSchema)
	if err != nil {
		t.Fatal(err)
	}
	md := tmpl.Underlying()
	u1, u2, u3 := uuid.New(), uuid.New(), uuid.New()

	tu := rowstruct.NewStructBuilder().
		AddLong("TU_P", 1).
		AddUUID("TU_U", u1).
		AddStruct("TU_S", rowstruct.NewStructBuilder().AddLong("ST1_A", 7).AddUUID("ST1_B", u2).Build()).
		Build()
	msg, err := structToRecord(tu, md.GetRecordType("TU").Descriptor)
	if err != nil {
		t.Fatal(err)
	}
	read, err := rowstruct.New(msg)
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := read.AttributeByName("TU_U"); got != u1.String() {
		t.Errorf("TU_U = %v, want %s", got, u1)
	}
	nested, _ := read.AttributeByName("TU_S")
	if got, _ := nested.(api.Struct).AttributeByName("ST1_B"); got != u2.String() {
		t.Errorf("TU_S.ST1_B = %v, want %s", got, u2)
	}

	t4 := rowstruct.NewStructBuilder().
		AddLong("T4_P", 2).
		AddArray("T4_ST1", rowstruct.NewArrayBuilder().
			AddStruct(rowstruct.NewStructBuilder().AddUUID("ST1_B", u3).Build()).
			AddStruct(rowstruct.NewStructBuilder().AddLong("ST1_A", 9).Build()).
			Build()).
		Build()
	msg, err = structToRecord(t4, md.GetRecordType("T4").Descriptor)
	if err != nil {
		t.Fatal(err)
	}
	read, err = rowstruct.New(msg)
	if err != nil {
		t.Fatal(err)
	}
	arr, _ := read.AttributeByName("T4_ST1")
	elems, ok := arr.([]any)
	if !ok || len(elems) != 2 {
		t.Fatalf("T4_ST1 = %#v, want two structs", arr)
	}
	if got, _ := elems[0].(api.Struct).AttributeByName("ST1_B"); got != u3.String() {
		t.Errorf("T4_ST1[1].ST1_B = %v, want %s", got, u3)
	}
	if got, _ := elems[1].(api.Struct).AttributeByName("ST1_B"); got != nil {
		t.Errorf("T4_ST1[2].ST1_B = %v, want NULL (attribute not given)", got)
	}

	// The stored UUID is the record layer's two-word message, the same bytes
	// a SQL insert of the UUID writes.
	field := msg.Descriptor().Fields().ByName("T4_ST1")
	if field == nil {
		t.Fatal("no T4_ST1 field")
	}
	if _, err := proto.Marshal(msg); err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var uuidMsg protoreflect.Message
	msg.Get(field).Message().Range(func(fd protoreflect.FieldDescriptor, v protoreflect.Value) bool {
		uuidMsg = v.List().Get(0).Message().Get(v.List().Get(0).Message().Descriptor().Fields().ByName("ST1_B")).Message()
		return false
	})
	if uuidMsg == nil || string(uuidMsg.Descriptor().FullName()) != "com.apple.foundationdb.record.UUID" {
		t.Fatalf("ST1_B is stored as %v, want the record layer's UUID message", uuidMsg)
	}

	// A bare UUID array is a Go extension (DIVERGENCES, Go-only extensions):
	// Java's repeated-field path cannot write it; Go writes each element as
	// the UUID message.
	ta := rowstruct.NewStructBuilder().
		AddLong("TA_P", 3).
		AddArray("TA_A", rowstruct.NewArrayBuilder().AddUUID(u1).AddUUID(u2).Build()).
		Build()
	msg, err = structToRecord(ta, md.GetRecordType("TA").Descriptor)
	if err != nil {
		t.Fatal(err)
	}
	read, err = rowstruct.New(msg)
	if err != nil {
		t.Fatal(err)
	}
	arr, _ = read.AttributeByName("TA_A")
	if elems, ok := arr.([]any); !ok || len(elems) != 2 || elems[0] != u1.String() || elems[1] != u2.String() {
		t.Errorf("TA_A = %#v, want [%s %s]", arr, u1, u2)
	}
}

// TestStructToRecord_Refusals pins the target's refusals: an attribute that
// names no column (INVALID_PARAMETER) and a value its column cannot take
// (CANNOT_CONVERT_TYPE, "Unexpected Column type").
func TestStructToRecord_Refusals(t *testing.T) {
	t.Parallel()
	tmpl, err := buildSchemaTemplateFromDDL(directAccessSchema)
	if err != nil {
		t.Fatal(err)
	}
	desc := tmpl.Underlying().GetRecordType("TU").Descriptor
	for _, c := range []struct {
		name string
		st   api.Struct
		code api.ErrorCode
	}{
		{"unknown column", rowstruct.NewStructBuilder().AddLong("NOPE", 1).Build(), api.ErrCodeInvalidParameter},
		{"string into a bigint", rowstruct.NewStructBuilder().AddString("TU_P", "x").Build(), api.ErrCodeCannotConvertType},
	} {
		_, err := structToRecord(c.st, desc)
		var apiErr *api.Error
		if !errors.As(err, &apiErr) || apiErr.Code != c.code {
			t.Errorf("%s: err = %v, want SQLSTATE %s", c.name, err, c.code)
		}
	}
}
