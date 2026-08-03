package sqldriver_test

import (
	"context"
	"database/sql"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/dynamicpb"

	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/fdbgo/fdb/subspace"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	"fdb.dev/pkg/recordlayer"
	"fdb.dev/pkg/relational/api"
	"fdb.dev/pkg/relational/core/embedded"
)

// structInsertDDL is the schema every struct-DML test in this file shares.
// The nested shape (S3 of S1 and S2) is nested-tests.yamsql's; the
// array-of-struct table is array-join-at.yamsql's; SARR carries an ARRAY
// INSIDE a struct, the shape that forces the converter's struct arm to
// re-enter the list arm (NullableArrayWrapper included).
const structInsertDDL = `CREATE TYPE AS STRUCT s1 (a BIGINT, b BIGINT)
	CREATE TYPE AS STRUCT s2 (c BIGINT, d BIGINT)
	CREATE TYPE AS STRUCT s3 (e s1, f s2)
	CREATE TYPE AS STRUCT sel (a INTEGER, b STRING)
	CREATE TYPE AS STRUCT sarr (vals BIGINT ARRAY, label STRING)
	CREATE TABLE t1 (id BIGINT, g BIGINT, h s3, PRIMARY KEY (id))
	CREATE TABLE t3 (id BIGINT, arr sel ARRAY, PRIMARY KEY (id))
	CREATE TABLE t4 (id BIGINT, s sarr, PRIMARY KEY (id))`

// structInsertDB provisions a database + schema carrying structInsertDDL and
// returns the driver handle plus the keyspace the driver writes under, so a
// test can read the stored record back through the record layer and compare
// its bytes.
func structInsertDB(t *testing.T, tag string) (*sql.DB, context.Context, subspace.Subspace) {
	t.Helper()
	ctx := context.Background()
	dbPath := "/structins_" + tag
	setup := openTestDB(t, dbPath)
	if _, err := setup.ExecContext(ctx, "CREATE DATABASE "+dbPath); err != nil {
		t.Fatalf("db: %v", err)
	}
	tmpl := "structins_tmpl_" + tag
	if _, err := setup.ExecContext(ctx, "CREATE SCHEMA TEMPLATE "+tmpl+" "+structInsertDDL); err != nil {
		t.Fatalf("tmpl: %v", err)
	}
	if _, err := setup.ExecContext(ctx, "CREATE SCHEMA "+dbPath+"/main WITH TEMPLATE "+tmpl); err != nil {
		t.Fatalf("schema: %v", err)
	}
	db, err := sql.Open("fdbsql", "fdbsql://"+dbPath+"?cluster_file="+clusterFilePath+"&schema=main")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db, ctx, subspace.Sub().Sub(tuple.Tuple{dbPath, "MAIN"})
}

// structInsertMetaData rebuilds structInsertDDL's metadata out-of-band. The
// DDL→descriptor path is what RFC-204 Phase 1 pinned byte-exact against the
// live JVM, so reusing it for the DESCRIPTOR and hand-building the MESSAGE is
// the split these tests need: Phase 1 proved the schema bytes, Phase 2 proves
// the record bytes written under them.
func structInsertMetaData(t *testing.T) *recordlayer.RecordMetaData {
	t.Helper()
	tmpl, err := embedded.BuildSchemaTemplateFromDDL(structInsertDDL)
	if err != nil {
		t.Fatalf("metadata build: %v", err)
	}
	return tmpl.Underlying()
}

// storedRecordBytes loads the record the driver wrote and returns its
// deterministic serialization.
func storedRecordBytes(t *testing.T, ctx context.Context, ss subspace.Subspace, md *recordlayer.RecordMetaData, table string, pk int64) []byte {
	t.Helper()
	fdb.MustAPIVersion(730)
	rawDB, err := fdb.OpenDatabase(clusterFilePath)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	rlDB := recordlayer.NewFDBDatabase(rawDB)
	var out []byte
	_, err = rlDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
		store, sErr := recordlayer.NewStoreBuilder().
			SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ss).Open()
		if sErr != nil {
			return nil, sErr
		}
		rt := md.GetRecordType(table)
		rec, lErr := store.LoadRecord(tuple.Tuple{rt.GetRecordTypeKey(), pk})
		if lErr != nil {
			return nil, lErr
		}
		if rec == nil {
			return nil, fmt.Errorf("record %s pk=%d not found under %v", table, pk, ss)
		}
		bs, mErr := proto.MarshalOptions{Deterministic: true}.Marshal(rec.Record)
		if mErr != nil {
			return nil, mErr
		}
		out = bs
		return nil, nil
	})
	if err != nil {
		t.Fatalf("load %s pk=%d: %v", table, pk, err)
	}
	return out
}

func goldenBytes(t *testing.T, m *dynamicpb.Message) []byte {
	t.Helper()
	bs, err := proto.MarshalOptions{Deterministic: true}.Marshal(m)
	if err != nil {
		t.Fatalf("golden marshal: %v", err)
	}
	return bs
}

// TestFDB_StructLiteralInsertWireBytes is the DATA half of RFC-204's wire
// proof: a struct literal in INSERT … VALUES must serialize as the nested
// message the table descriptor declares, byte-equal to the same message
// built directly through protobuf. Java builds this message in
// RecordConstructorValue.eval (RecordConstructorValue.java:113-139), typed by
// the target-type push-down (ExpressionVisitor.parseRecordField:967-1008);
// a Go divergence here corrupts a cluster shared with Java readers.
func TestFDB_StructLiteralInsertWireBytes(t *testing.T) {
	t.Parallel()
	db, ctx, ss := structInsertDB(t, "wire")
	md := structInsertMetaData(t)

	t1 := md.GetRecordType("T1").Descriptor
	hFD := t1.Fields().ByName("H")
	s3 := hFD.Message()
	eFD, fFD := s3.Fields().ByName("E"), s3.Fields().ByName("F")

	putS := func(parent *dynamicpb.Message, fd protoreflect.FieldDescriptor, x, y int64) {
		sub := dynamicpb.NewMessage(fd.Message())
		sub.Set(fd.Message().Fields().Get(0), protoreflect.ValueOfInt64(x))
		sub.Set(fd.Message().Fields().Get(1), protoreflect.ValueOfInt64(y))
		parent.Set(fd, protoreflect.ValueOfMessage(sub))
	}

	t.Run("nested_struct", func(t *testing.T) {
		if _, err := db.ExecContext(ctx, "INSERT INTO T1 VALUES (1, 2, ((3, 4), (5, 6)))"); err != nil {
			t.Fatalf("insert: %v", err)
		}
		golden := dynamicpb.NewMessage(t1)
		golden.Set(t1.Fields().ByName("ID"), protoreflect.ValueOfInt64(1))
		golden.Set(t1.Fields().ByName("G"), protoreflect.ValueOfInt64(2))
		h := dynamicpb.NewMessage(s3)
		putS(h, eFD, 3, 4)
		putS(h, fFD, 5, 6)
		golden.Set(hFD, protoreflect.ValueOfMessage(h))

		got := storedRecordBytes(t, ctx, ss, md, "T1", 1)
		if want := goldenBytes(t, golden); !reflect.DeepEqual(got, want) {
			t.Fatalf("stored bytes diverge from the nested-message golden:\n stored=%x\n golden=%x", got, want)
		}
	})

	t.Run("null_nested_struct_field_is_absent", func(t *testing.T) {
		// nested-tests.yamsql T2's row: a NULL nested struct is the ABSENT
		// field, never a present empty message — Java's eval simply does not
		// set a null child (RecordConstructorValue.java:135). A present empty
		// message would read back as a non-NULL struct of NULLs.
		if _, err := db.ExecContext(ctx, "INSERT INTO T1 VALUES (2, 2, ((3, 4), null))"); err != nil {
			t.Fatalf("insert: %v", err)
		}
		golden := dynamicpb.NewMessage(t1)
		golden.Set(t1.Fields().ByName("ID"), protoreflect.ValueOfInt64(2))
		golden.Set(t1.Fields().ByName("G"), protoreflect.ValueOfInt64(2))
		h := dynamicpb.NewMessage(s3)
		putS(h, eFD, 3, 4)
		golden.Set(hFD, protoreflect.ValueOfMessage(h))

		got := storedRecordBytes(t, ctx, ss, md, "T1", 2)
		if want := goldenBytes(t, golden); !reflect.DeepEqual(got, want) {
			t.Fatalf("stored bytes diverge:\n stored=%x\n golden=%x", got, want)
		}
	})

	t.Run("null_leaf_fields_are_absent", func(t *testing.T) {
		if _, err := db.ExecContext(ctx, "INSERT INTO T1 VALUES (3, 20, ((null, null), null))"); err != nil {
			t.Fatalf("insert: %v", err)
		}
		golden := dynamicpb.NewMessage(t1)
		golden.Set(t1.Fields().ByName("ID"), protoreflect.ValueOfInt64(3))
		golden.Set(t1.Fields().ByName("G"), protoreflect.ValueOfInt64(20))
		h := dynamicpb.NewMessage(s3)
		// E is PRESENT but empty (its two fields are NULL); F is absent.
		h.Set(eFD, protoreflect.ValueOfMessage(dynamicpb.NewMessage(eFD.Message())))
		golden.Set(hFD, protoreflect.ValueOfMessage(h))

		got := storedRecordBytes(t, ctx, ss, md, "T1", 3)
		if want := goldenBytes(t, golden); !reflect.DeepEqual(got, want) {
			t.Fatalf("stored bytes diverge:\n stored=%x\n golden=%x", got, want)
		}
	})

	t.Run("null_struct_column", func(t *testing.T) {
		if _, err := db.ExecContext(ctx, "INSERT INTO T1 VALUES (4, 40, null)"); err != nil {
			t.Fatalf("insert: %v", err)
		}
		golden := dynamicpb.NewMessage(t1)
		golden.Set(t1.Fields().ByName("ID"), protoreflect.ValueOfInt64(4))
		golden.Set(t1.Fields().ByName("G"), protoreflect.ValueOfInt64(40))

		got := storedRecordBytes(t, ctx, ss, md, "T1", 4)
		if want := goldenBytes(t, golden); !reflect.DeepEqual(got, want) {
			t.Fatalf("stored bytes diverge:\n stored=%x\n golden=%x", got, want)
		}
	})

	t.Run("array_of_struct", func(t *testing.T) {
		// array-join-at.yamsql's T3 seed. ARR is a NULLABLE array, so the
		// elements live inside the NullableArrayWrapper — the struct arm and
		// the list arm compose.
		if _, err := db.ExecContext(ctx, "INSERT INTO T3 VALUES (1, [(10, 's11'), (11, 's12')])"); err != nil {
			t.Fatalf("insert: %v", err)
		}
		t3 := md.GetRecordType("T3").Descriptor
		arrFD := t3.Fields().ByName("ARR")
		golden := dynamicpb.NewMessage(t3)
		golden.Set(t3.Fields().ByName("ID"), protoreflect.ValueOfInt64(1))
		elem := func(a int32, b string) protoreflect.Value {
			ed := arrayElementMessageDescriptor(arrFD)
			m := dynamicpb.NewMessage(ed)
			m.Set(ed.Fields().ByName("A"), protoreflect.ValueOfInt32(a))
			m.Set(ed.Fields().ByName("B"), protoreflect.ValueOfString(b))
			return protoreflect.ValueOfMessage(m)
		}
		setArrayField(golden, arrFD, elem(10, "s11"), elem(11, "s12"))

		got := storedRecordBytes(t, ctx, ss, md, "T3", 1)
		if want := goldenBytes(t, golden); !reflect.DeepEqual(got, want) {
			t.Fatalf("stored bytes diverge from the array-of-struct golden:\n stored=%x\n golden=%x", got, want)
		}
	})

	t.Run("empty_array_of_struct", func(t *testing.T) {
		// `[]` at a struct-array column: a PRESENT wrapper with an empty
		// list, which is what keeps [] distinct from NULL on the wire.
		if _, err := db.ExecContext(ctx, "INSERT INTO T3 VALUES (2, [])"); err != nil {
			t.Fatalf("insert: %v", err)
		}
		t3 := md.GetRecordType("T3").Descriptor
		golden := dynamicpb.NewMessage(t3)
		golden.Set(t3.Fields().ByName("ID"), protoreflect.ValueOfInt64(2))
		setArrayField(golden, t3.Fields().ByName("ARR"))

		got := storedRecordBytes(t, ctx, ss, md, "T3", 2)
		if want := goldenBytes(t, golden); !reflect.DeepEqual(got, want) {
			t.Fatalf("stored bytes diverge:\n stored=%x\n golden=%x", got, want)
		}
	})

	t.Run("array_inside_struct", func(t *testing.T) {
		// The composition the converter's struct arm must delegate for: a
		// struct whose field is itself a NULLABLE array, wrapped exactly as
		// Java wraps it inside eval (RecordConstructorValue.java:127-131).
		if _, err := db.ExecContext(ctx, "INSERT INTO T4 VALUES (1, ([7, 8], 'lab'))"); err != nil {
			t.Fatalf("insert: %v", err)
		}
		t4 := md.GetRecordType("T4").Descriptor
		sFD := t4.Fields().ByName("S")
		sarr := sFD.Message()
		golden := dynamicpb.NewMessage(t4)
		golden.Set(t4.Fields().ByName("ID"), protoreflect.ValueOfInt64(1))
		s := dynamicpb.NewMessage(sarr)
		setArrayField(s, sarr.Fields().ByName("VALS"),
			protoreflect.ValueOfInt64(7), protoreflect.ValueOfInt64(8))
		s.Set(sarr.Fields().ByName("LABEL"), protoreflect.ValueOfString("lab"))
		golden.Set(sFD, protoreflect.ValueOfMessage(s))

		got := storedRecordBytes(t, ctx, ss, md, "T4", 1)
		if want := goldenBytes(t, golden); !reflect.DeepEqual(got, want) {
			t.Fatalf("stored bytes diverge from the array-in-struct golden:\n stored=%x\n golden=%x", got, want)
		}
	})
}

// requireErrContains asserts the statement failed with a message carrying
// want — used where the Java-verbatim WORDING is the contract.
func requireErrContains(t *testing.T, err error, want string) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected an error containing %q, statement succeeded", want)
	}
	if !strings.Contains(err.Error(), want) {
		t.Fatalf("error %q does not contain %q", err.Error(), want)
	}
}

// TestFDB_StructLiteralInsertRejections pins the structural type errors of
// the target-type push-down. Each is Java's, at Java's code class.
func TestFDB_StructLiteralInsertRejections(t *testing.T) {
	t.Parallel()
	db, ctx, _ := structInsertDB(t, "reject")

	t.Run("record_constructor_at_scalar_slot", func(t *testing.T) {
		// The push-down casts the pushed target type to Type.Record
		// (ExpressionVisitor.java:1047); against a BIGINT column that cast
		// fails with Assert's "expected Record but got Primitive"
		// (Assert.java:211-212).
		_, err := db.ExecContext(ctx, "INSERT INTO T1 VALUES (90, (1, 2), ((3, 4), (5, 6)))")
		requireErrContains(t, err, "expected Record but got Primitive")
	})

	t.Run("arity_mismatch_in_struct_literal", func(t *testing.T) {
		// Java: elementFields.size() == providedColumnContexts.size()
		// (ExpressionVisitor.java:1080-1082) → CANNOT_CONVERT_TYPE.
		_, err := db.ExecContext(ctx, "INSERT INTO T1 VALUES (91, 1, ((3, 4, 5), (5, 6)))")
		requireSQLSTATE(t, err, api.ErrCodeCannotConvertType)
	})

	t.Run("scalar_at_struct_slot", func(t *testing.T) {
		_, err := db.ExecContext(ctx, "INSERT INTO T1 VALUES (92, 1, 7)")
		requireSQLSTATE(t, err, api.ErrCodeCannotConvertType)
	})

	t.Run("scalar_element_in_struct_array", func(t *testing.T) {
		// Element-wise coercion decides, not the literal's shape: Java
		// coerces each element to the target element type
		// (ExpressionVisitor.java:1036-1039) and a primitive→record
		// coercion resolves no physical operator, so SemanticException
		// INCOMPATIBLE_TYPE (PromoteValue.java:370-371) → 22000.
		_, err := db.ExecContext(ctx, "INSERT INTO T3 VALUES (93, [7, 8])")
		requireSQLSTATE(t, err, api.ErrCodeCannotConvertType)
	})

	t.Run("mixed_struct_array_fails_on_the_bad_element", func(t *testing.T) {
		_, err := db.ExecContext(ctx, "INSERT INTO T3 VALUES (95, [(1, 'a'), 7])")
		requireSQLSTATE(t, err, api.ErrCodeCannotConvertType)
	})

	t.Run("wrong_element_type_in_struct_literal", func(t *testing.T) {
		_, err := db.ExecContext(ctx, "INSERT INTO T1 VALUES (94, 1, (('x', 4), (5, 6)))")
		requireSQLSTATE(t, err, api.ErrCodeCannotConvertType)
	})
}

// TestFDB_StructReadBackIsAStruct pins the READ direction: a struct column
// must reach the client as an api.Struct, never as the raw protobuf message
// the value layer carries. Java materializes it at the same boundary
// (RowStruct.getObject's Types.STRUCT arm → ImmutableRowStruct over a
// MessageTuple, RowStruct.java:184-197, :293-294), and a client that receives
// a *dynamicpb.Message instead has no attribute accessors at all.
func TestFDB_StructReadBackIsAStruct(t *testing.T) {
	t.Parallel()
	db, ctx, _ := structInsertDB(t, "read")

	if _, err := db.ExecContext(ctx, "INSERT INTO T1 VALUES (1, 2, ((3, 4), (5, 6)))"); err != nil {
		t.Fatalf("insert: %v", err)
	}
	if _, err := db.ExecContext(ctx, "INSERT INTO T3 VALUES (1, [(10, 's11'), (11, 's12')])"); err != nil {
		t.Fatalf("insert array: %v", err)
	}

	t.Run("nested_struct_attributes", func(t *testing.T) {
		var got any
		if err := db.QueryRowContext(ctx, "SELECT h FROM T1 WHERE id = 1").Scan(&got); err != nil {
			t.Fatalf("select: %v", err)
		}
		s, ok := got.(api.Struct)
		if !ok {
			t.Fatalf("struct column surfaced as %T, want api.Struct", got)
		}
		if s.MetaData().TypeName() != "S3" {
			t.Errorf("type name: got %q, want S3", s.MetaData().TypeName())
		}
		if n := s.AttributeCount(); n != 2 {
			t.Fatalf("attribute count: got %d, want 2", n)
		}
		// By NAME (case-insensitively, as Java resolves it) and by POSITION
		// must reach the same attribute.
		byName, err := s.AttributeByName("e")
		if err != nil {
			t.Fatalf("by name: %v", err)
		}
		byPos, err := s.Attribute(1)
		if err != nil {
			t.Fatalf("by position: %v", err)
		}
		for label, v := range map[string]any{"by-name": byName, "by-position": byPos} {
			inner, isStruct := v.(api.Struct)
			if !isStruct {
				t.Fatalf("%s attribute E is %T, want api.Struct", label, v)
			}
			a, aerr := inner.AttributeByName("A")
			if aerr != nil || a != int64(3) {
				t.Errorf("%s E.A: got %v (%v), want 3", label, a, aerr)
			}
		}
	})

	t.Run("null_struct_field_reads_as_null", func(t *testing.T) {
		if _, err := db.ExecContext(ctx, "INSERT INTO T1 VALUES (2, 2, ((3, 4), null))"); err != nil {
			t.Fatalf("insert: %v", err)
		}
		var got any
		if err := db.QueryRowContext(ctx, "SELECT h FROM T1 WHERE id = 2").Scan(&got); err != nil {
			t.Fatalf("select: %v", err)
		}
		s := got.(api.Struct)
		f, err := s.AttributeByName("F")
		if err != nil {
			t.Fatalf("attribute: %v", err)
		}
		if f != nil {
			t.Errorf("absent struct field read back as %#v, want nil (SQL NULL)", f)
		}
	})

	t.Run("array_of_struct_elements_are_structs", func(t *testing.T) {
		var got any
		if err := db.QueryRowContext(ctx, "SELECT arr FROM T3 WHERE id = 1").Scan(&got); err != nil {
			t.Fatalf("select: %v", err)
		}
		arr, ok := got.([]any)
		if !ok || len(arr) != 2 {
			t.Fatalf("array column: got %#v, want 2 elements", got)
		}
		el, ok := arr[0].(api.Struct)
		if !ok {
			t.Fatalf("array element is %T, want api.Struct", arr[0])
		}
		b, err := el.AttributeByName("B")
		if err != nil || b != "s11" {
			t.Errorf("element[0].B: got %v (%v), want s11", b, err)
		}
	})
}

// TestFDB_StructUpdateAndInsertSelect covers the two DML paths that do NOT
// go through the plan-time VALUES fold — UPDATE SET and INSERT … SELECT —
// and asserts the stored bytes, because both write through the executor's
// own converter rather than the plan-time one.
func TestFDB_StructUpdateAndInsertSelect(t *testing.T) {
	t.Parallel()
	db, ctx, ss := structInsertDB(t, "upd")
	md := structInsertMetaData(t)
	t1 := md.GetRecordType("T1").Descriptor
	hFD := t1.Fields().ByName("H")
	s3 := hFD.Message()

	putS := func(parent *dynamicpb.Message, fd protoreflect.FieldDescriptor, x, y int64) {
		sub := dynamicpb.NewMessage(fd.Message())
		sub.Set(fd.Message().Fields().Get(0), protoreflect.ValueOfInt64(x))
		sub.Set(fd.Message().Fields().Get(1), protoreflect.ValueOfInt64(y))
		parent.Set(fd, protoreflect.ValueOfMessage(sub))
	}
	golden := func(id, g int64, e, f *[2]int64) []byte {
		m := dynamicpb.NewMessage(t1)
		m.Set(t1.Fields().ByName("ID"), protoreflect.ValueOfInt64(id))
		m.Set(t1.Fields().ByName("G"), protoreflect.ValueOfInt64(g))
		h := dynamicpb.NewMessage(s3)
		if e != nil {
			putS(h, s3.Fields().ByName("E"), e[0], e[1])
		}
		if f != nil {
			putS(h, s3.Fields().ByName("F"), f[0], f[1])
		}
		m.Set(hFD, protoreflect.ValueOfMessage(h))
		return goldenBytes(t, m)
	}

	if _, err := db.ExecContext(ctx, "INSERT INTO T1 VALUES (1, 2, ((3, 4), (5, 6)))"); err != nil {
		t.Fatalf("seed: %v", err)
	}

	t.Run("update_set_whole_struct", func(t *testing.T) {
		// inserts-updates-deletes.yamsql's `update B set b3 = (100, 100)`
		// shape. Java reaches the same typed record from the other side (an
		// anonymous constructor coerced into the target descriptor at
		// transform time, MessageHelpers.deepCopyMessageIfNeeded).
		if _, err := db.ExecContext(ctx, "UPDATE T1 SET h = ((30, 40), (50, 60)) WHERE id = 1"); err != nil {
			t.Fatalf("update: %v", err)
		}
		got := storedRecordBytes(t, ctx, ss, md, "T1", 1)
		if want := golden(1, 2, &[2]int64{30, 40}, &[2]int64{50, 60}); !reflect.DeepEqual(got, want) {
			t.Fatalf("stored bytes after UPDATE diverge:\n stored=%x\n golden=%x", got, want)
		}
	})

	t.Run("insert_select_carries_the_struct", func(t *testing.T) {
		// The struct value flows from the scan as a proto message and is
		// copied into the insert target's descriptor by field NUMBER
		// (Java: RecordConstructorValue.deepCopyIfNeeded's record arm).
		if _, err := db.ExecContext(ctx, "INSERT INTO T1 SELECT id + 100, g, h FROM T1 WHERE id = 1"); err != nil {
			t.Fatalf("insert select: %v", err)
		}
		got := storedRecordBytes(t, ctx, ss, md, "T1", 101)
		if want := golden(101, 2, &[2]int64{30, 40}, &[2]int64{50, 60}); !reflect.DeepEqual(got, want) {
			t.Fatalf("stored bytes after INSERT … SELECT diverge:\n stored=%x\n golden=%x", got, want)
		}
	})

	t.Run("update_set_struct_with_null_field", func(t *testing.T) {
		if _, err := db.ExecContext(ctx, "UPDATE T1 SET h = ((7, 8), null) WHERE id = 1"); err != nil {
			t.Fatalf("update: %v", err)
		}
		got := storedRecordBytes(t, ctx, ss, md, "T1", 1)
		if want := golden(1, 2, &[2]int64{7, 8}, nil); !reflect.DeepEqual(got, want) {
			t.Fatalf("stored bytes diverge:\n stored=%x\n golden=%x", got, want)
		}
	})

	t.Run("update_set_scalar_at_struct_column_rejected", func(t *testing.T) {
		_, err := db.ExecContext(ctx, "UPDATE T1 SET h = 5 WHERE id = 1")
		requireSQLSTATE(t, err, api.ErrCodeCannotConvertType)
	})
}

// TestFDB_StructNotNullArrayFieldRejectsNull pins the nullability gate at the
// level Java places it — the message build (RecordConstructorValue.java:135's
// verify), not the visitor — which is why it holds for a struct FIELD and not
// only for a top-level column.
func TestFDB_StructNotNullArrayFieldRejectsNull(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dbPath := "/structnn"
	setup := openTestDB(t, dbPath)
	if _, err := setup.ExecContext(ctx, "CREATE DATABASE "+dbPath); err != nil {
		t.Fatalf("db: %v", err)
	}
	const ddl = `CREATE TYPE AS STRUCT snn (vals BIGINT ARRAY NOT NULL, label STRING)
		CREATE TABLE t (id BIGINT, s snn, PRIMARY KEY (id))`
	if _, err := setup.ExecContext(ctx, "CREATE SCHEMA TEMPLATE structnn_tmpl "+ddl); err != nil {
		t.Fatalf("tmpl: %v", err)
	}
	if _, err := setup.ExecContext(ctx, "CREATE SCHEMA "+dbPath+"/main WITH TEMPLATE structnn_tmpl"); err != nil {
		t.Fatalf("schema: %v", err)
	}
	db, err := sql.Open("fdbsql", "fdbsql://"+dbPath+"?cluster_file="+clusterFilePath+"&schema=main")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	_, err = db.ExecContext(ctx, "INSERT INTO t VALUES (1, (null, 'x'))")
	requireSQLSTATE(t, err, api.ErrCodeNotNullViolation)

	// The same NULL through UPDATE, which does not go through the plan-time
	// fold at all — the gate must hold on both writers.
	if _, err := db.ExecContext(ctx, "INSERT INTO t VALUES (2, ([1, 2], 'y'))"); err != nil {
		t.Fatalf("seed: %v", err)
	}
	_, err = db.ExecContext(ctx, "UPDATE t SET s = (null, 'z') WHERE id = 2")
	requireSQLSTATE(t, err, api.ErrCodeNotNullViolation)
}
