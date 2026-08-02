package embedded

import (
	"strings"
	"testing"

	"google.golang.org/protobuf/reflect/protoreflect"

	"fdb.dev/pkg/relational/api"
)

// RFC-204 Phase 1 — CREATE TYPE AS STRUCT: registration, forward
// references, topological resolution with cycle rejection, struct-typed
// columns, array-of-struct, nested primary keys, and Java's descriptor
// shape (top-level struct messages, LABEL_OPTIONAL fields).

func structTemplateDescriptor(t *testing.T, ddl string) protoreflect.FileDescriptor {
	t.Helper()
	tmpl, err := buildSchemaTemplateFromDDL(ddl)
	if err != nil {
		t.Fatalf("buildSchemaTemplateFromDDL: %v", err)
	}
	return tmpl.Underlying().FileDescriptor()
}

func TestStructDDL_BasicRegistration(t *testing.T) {
	t.Parallel()
	fd := structTemplateDescriptor(t,
		`CREATE TYPE AS STRUCT s1 (a BIGINT, b BIGINT)
		 CREATE TABLE t (id BIGINT, s s1, PRIMARY KEY (id))`)

	// The struct is a TOP-LEVEL message named by its normalized identifier
	// (Java: FileDescriptorSerializer copies struct descriptors to the file
	// level; measured live-JVM shape: S1 before T, alphabetical).
	s1 := fd.Messages().ByName("S1")
	if s1 == nil {
		t.Fatalf("S1 not a top-level message; messages: %v", messageNames(fd))
	}
	if s1.Fields().Len() != 2 || s1.Fields().Get(0).Name() != "A" || s1.Fields().Get(1).Name() != "B" {
		t.Fatalf("S1 fields = %v, want [A B]", s1.Fields())
	}
	tbl := fd.Messages().ByName("T")
	sField := tbl.Fields().ByName("S")
	if sField == nil || sField.Kind() != protoreflect.MessageKind || sField.Message() != s1 {
		t.Fatalf("T.S = %v, want message field referencing top-level S1", sField)
	}
	// Every struct/table field is LABEL_OPTIONAL (Java: defineProtoType
	// passes LABEL_OPTIONAL unconditionally).
	for _, f := range []protoreflect.FieldDescriptor{s1.Fields().Get(0), sField} {
		if f.Cardinality() != protoreflect.Optional {
			t.Errorf("field %v cardinality = %v, want optional", f.Name(), f.Cardinality())
		}
	}
}

func TestStructDDL_ForwardReference(t *testing.T) {
	t.Parallel()
	// The table references LATER; resolution happens at Build (Java:
	// lookupType returns UnresolvedType, resolveTypes fixes it up).
	fd := structTemplateDescriptor(t,
		`CREATE TABLE t (id BIGINT, s later, PRIMARY KEY (id))
		 CREATE TYPE AS STRUCT later (a BIGINT)`)
	if fd.Messages().ByName("LATER") == nil {
		t.Fatalf("LATER not emitted; messages: %v", messageNames(fd))
	}
	sf := fd.Messages().ByName("T").Fields().ByName("S")
	if sf == nil || sf.Kind() != protoreflect.MessageKind || sf.Message().Name() != "LATER" {
		t.Fatalf("T.S = %v, want message field of LATER", sf)
	}
}

func TestStructDDL_StructOfStructSharedAcrossTables(t *testing.T) {
	t.Parallel()
	fd := structTemplateDescriptor(t,
		`CREATE TYPE AS STRUCT s1 (a BIGINT, b BIGINT)
		 CREATE TYPE AS STRUCT s2 (c s1, d STRING)
		 CREATE TABLE t1 (id BIGINT, s s2, PRIMARY KEY (id))
		 CREATE TABLE t2 (id BIGINT, u s1, PRIMARY KEY (id))`)
	// One S1 descriptor shared by S2.C and T2.U (template-wide dedup —
	// Java's registerTypeDescriptors descriptorNames set).
	s1 := fd.Messages().ByName("S1")
	s2 := fd.Messages().ByName("S2")
	if s1 == nil || s2 == nil {
		t.Fatalf("missing top-level structs; messages: %v", messageNames(fd))
	}
	if s2.Fields().ByName("C").Message() != s1 {
		t.Errorf("S2.C references %v, want the shared S1", s2.Fields().ByName("C").Message().FullName())
	}
	if fd.Messages().ByName("T2").Fields().ByName("U").Message() != s1 {
		t.Errorf("T2.U must reference the shared S1")
	}
}

func TestStructDDL_ArrayOfStruct(t *testing.T) {
	t.Parallel()
	// Nullable array-of-struct: OPTIONAL wrapper field whose `values` list
	// has the struct element type (the C2 NullableArrayWrapper shape).
	fd := structTemplateDescriptor(t,
		`CREATE TYPE AS STRUCT p (x BIGINT, y BIGINT)
		 CREATE TABLE t (id BIGINT, pts p ARRAY, PRIMARY KEY (id))`)
	pts := fd.Messages().ByName("T").Fields().ByName("PTS")
	if pts == nil || pts.IsList() || pts.Kind() != protoreflect.MessageKind {
		t.Fatalf("PTS = %v, want an optional wrapper-message field", pts)
	}
	wrapper := pts.Message()
	values := wrapper.Fields().ByName("values")
	if wrapper.Fields().Len() != 1 || values == nil || !values.IsList() ||
		values.Kind() != protoreflect.MessageKind || values.Message().Name() != "P" {
		t.Fatalf("wrapper = %v, want single repeated `values` of P", wrapper)
	}
}

func TestStructDDL_CyclicRejected(t *testing.T) {
	t.Parallel()
	for name, ddl := range map[string]string{
		"mutual": `CREATE TYPE AS STRUCT a (x b)
		           CREATE TYPE AS STRUCT b (y a)
		           CREATE TABLE t (id BIGINT, s a, PRIMARY KEY (id))`,
		"self": `CREATE TYPE AS STRUCT a (x a)
		         CREATE TABLE t (id BIGINT, s a, PRIMARY KEY (id))`,
	} {
		ddl := ddl
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			_, err := buildSchemaTemplateFromDDL(ddl)
			if err == nil {
				t.Fatal("cyclic struct dependency must be rejected")
			}
			// Java: Assert.thatUnchecked(sorted.isPresent(),
			// INVALID_SCHEMA_TEMPLATE, "Invalid cyclic dependency in the
			// schema definition") — RecordLayerSchemaTemplate.resolveTypes.
			if !strings.Contains(err.Error(), "Invalid cyclic dependency in the schema definition") {
				t.Fatalf("error = %v, want Java's cyclic-dependency wording", err)
			}
		})
	}
}

func TestStructDDL_UnknownTypeRejected(t *testing.T) {
	t.Parallel()
	_, err := buildSchemaTemplateFromDDL(
		`CREATE TABLE t (id BIGINT, s nosuchtype, PRIMARY KEY (id))`)
	if err == nil {
		t.Fatal("unknown custom type must be rejected")
	}
	// Java: ErrorCode.UNKNOWN_TYPE, "could not find type '%s'".
	if !strings.Contains(err.Error(), "could not find type 'NOSUCHTYPE'") {
		t.Fatalf("error = %v, want Java's could-not-find-type wording", err)
	}
	if apiErr := api.AsError(err); apiErr == nil || apiErr.Code != api.ErrCodeUnknownType {
		t.Fatalf("error code = %v, want 42F18 UNKNOWN_TYPE", err)
	}
}

func TestStructDDL_DuplicateTypeNameRejected(t *testing.T) {
	t.Parallel()
	_, err := buildSchemaTemplateFromDDL(
		`CREATE TYPE AS STRUCT s1 (a BIGINT)
		 CREATE TYPE AS STRUCT s1 (b BIGINT)
		 CREATE TABLE t (id BIGINT, s s1, PRIMARY KEY (id))`)
	if err == nil {
		t.Fatal("duplicate struct type name must be rejected")
	}
	// Java: verifyNameIsNotUsed — "type with name 'S1' already exists".
	if !strings.Contains(err.Error(), "type with name 'S1' already exists") {
		t.Fatalf("error = %v, want Java's name-collision wording", err)
	}
}

func TestStructDDL_TableAsColumnType(t *testing.T) {
	t.Parallel()
	// Java's findType looks TABLES first — a table name is itself usable as
	// a column type (its row struct).
	fd := structTemplateDescriptor(t,
		`CREATE TABLE base (id BIGINT, v BIGINT, PRIMARY KEY (id))
		 CREATE TABLE holder (id BIGINT, b base, PRIMARY KEY (id))`)
	bf := fd.Messages().ByName("HOLDER").Fields().ByName("B")
	if bf == nil || bf.Kind() != protoreflect.MessageKind || bf.Message().Name() != "BASE" {
		t.Fatalf("HOLDER.B = %v, want message field of BASE", bf)
	}
}

func TestStructDDL_NestedPrimaryKey(t *testing.T) {
	t.Parallel()
	// primary key(id.a, id.b) — the nested walk emits
	// field(ID).nest(field(A)) (Java RecordLayerTable.toKeyExpression).
	tmpl, err := buildSchemaTemplateFromDDL(
		`CREATE TYPE AS STRUCT s1 (a BIGINT, b BIGINT)
		 CREATE TABLE t1 (id s1, g BIGINT, PRIMARY KEY (id.a, id.b))`)
	if err != nil {
		t.Fatalf("nested primary key must build: %v", err)
	}
	pk := tmpl.Underlying().GetRecordType("T1").PrimaryKey
	ke := pk.ToKeyExpression()
	then := ke.GetThen()
	if then == nil || len(then.GetChild()) != 3 {
		t.Fatalf("pk = %v, want then(recordType, nest(ID.A), nest(ID.B))", ke)
	}
	first := then.GetChild()[1].GetNesting()
	if first == nil || first.GetParent().GetFieldName() != "ID" ||
		first.GetChild().GetField().GetFieldName() != "A" {
		t.Fatalf("pk part = %v, want field(ID).nest(field(A))", then.GetChild()[1])
	}
}

func TestStructDDL_NullabilityVariantsCollapse(t *testing.T) {
	t.Parallel()
	// One named struct used nullable, NULL-marked, and as an array element
	// shares ONE descriptor — Java's nullability collapse
	// (RecordLayerTable.calculateDataType TODO), reproduced because the
	// collapsed descriptor is the wire shape. If this starts emitting per-
	// nullability descriptors, upstream fixed the TODO — re-measure before
	// "fixing" this test.
	fd := structTemplateDescriptor(t,
		`CREATE TYPE AS STRUCT address (street STRING, city STRING)
		 CREATE TABLE users (id BIGINT, home address, PRIMARY KEY (id))
		 CREATE TABLE biz (id BIGINT, hq address, branches address ARRAY, PRIMARY KEY (id))`)
	var addressCount int
	for i := 0; i < fd.Messages().Len(); i++ {
		if fd.Messages().Get(i).Name() == "ADDRESS" {
			addressCount++
		}
	}
	if addressCount != 1 {
		t.Fatalf("ADDRESS emitted %d times, want exactly 1 (nullability collapse)", addressCount)
	}
}

// TestStructDDL_Phase1QuerySurface pins the measured Phase 1 query surface
// over a struct table: scalar columns stay fully queryable, while NESTED
// field access is not yet resolvable — the last-dot colRef split makes `s.a`
// die as a clean 42703 (unresolvable qualifier). The 42703 is the Phase 3
// re-arm: when nested resolution lands (SemanticAnalyzer.lookupNestedField
// port), this arm flips to a rows assertion.
func TestStructDDL_Phase1QuerySurface(t *testing.T) {
	t.Parallel()
	const ddl = `CREATE TYPE AS STRUCT s1 (a BIGINT, b BIGINT)
		 CREATE TABLE t (id BIGINT, s s1, PRIMARY KEY (id))`
	if _, err := PlanQueryForTest(`SELECT id FROM t`, ddl, nil); err != nil {
		t.Fatalf("scalar columns of a struct table must stay queryable: %v", err)
	}
	_, err := PlanQueryForTest(`SELECT s.a FROM t`, ddl, nil)
	if err == nil {
		t.Fatal("nested field access resolved — RFC-204 Phase 3 landed; replace this arm with a rows assertion")
	}
	if apiErr := api.AsError(err); apiErr == nil || apiErr.Code != api.ErrCodeUndefinedColumn {
		t.Fatalf("nested access error = %v, want the clean 42703 qualifier miss", err)
	}
}

func messageNames(fd protoreflect.FileDescriptor) []string {
	out := make([]string, 0, fd.Messages().Len())
	for i := 0; i < fd.Messages().Len(); i++ {
		out = append(out, string(fd.Messages().Get(i).Name()))
	}
	return out
}
