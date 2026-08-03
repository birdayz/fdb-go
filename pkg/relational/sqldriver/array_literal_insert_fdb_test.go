package sqldriver_test

import (
	"context"
	"database/sql"
	"fmt"
	"reflect"
	"testing"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/dynamicpb"

	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/fdbgo/fdb/subspace"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	"fdb.dev/pkg/recordlayer"
	"fdb.dev/pkg/relational/api"
	"fdb.dev/pkg/relational/core/metadata"
)

// arrayInsertDB provisions one database with one ARRAY-typed table per
// element type, for the array-literal INSERT … VALUES tests (CQ-72).
func arrayInsertDB(t *testing.T, tag string) (*sql.DB, context.Context) {
	t.Helper()
	ctx := context.Background()
	dbPath := "/arrins_" + tag
	setup := openTestDB(t, dbPath)
	if _, err := setup.ExecContext(ctx, "CREATE DATABASE "+dbPath); err != nil {
		t.Fatalf("db: %v", err)
	}
	tmpl := "arrins_tmpl_" + tag
	if _, err := setup.ExecContext(ctx, "CREATE SCHEMA TEMPLATE "+tmpl+
		" CREATE TABLE t_int (pk BIGINT, x INTEGER ARRAY, PRIMARY KEY (pk))"+
		" CREATE TABLE t_big (pk BIGINT, x BIGINT ARRAY, PRIMARY KEY (pk))"+
		" CREATE TABLE t_dbl (pk BIGINT, x DOUBLE ARRAY, PRIMARY KEY (pk))"+
		" CREATE TABLE t_str (pk BIGINT, x STRING ARRAY, PRIMARY KEY (pk))"); err != nil {
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
	return db, ctx
}

// scanArrayCell reads back the single row's array column as whatever the
// driver hands database/sql, for element-wise comparison.
func scanArrayCell(t *testing.T, db *sql.DB, ctx context.Context, table string, pk int64) any {
	t.Helper()
	var got any
	if err := db.QueryRowContext(ctx, fmt.Sprintf("SELECT x FROM %s WHERE pk = %d", table, pk)).Scan(&got); err != nil {
		t.Fatalf("read back %s: %v", table, err)
	}
	return got
}

// TestFDB_ArrayLiteralInsertValues is the CQ-72 regression net: an array
// literal in INSERT … VALUES must convert to the column's repeated proto
// field instead of falling through ConvertToProtoValue's scalar lanes to
// the verbatim 22000. Covers Java's element-promotion rules
// (ExpressionVisitor.coerceValueIfNecessary → PromoteValue per element):
// INT literals promote into BIGINT and DOUBLE arrays; STRING does not
// promote into INTEGER (22000, same class as Java's
// SemanticException INCOMPATIBLE_TYPE → CANNOT_CONVERT_TYPE).
func TestFDB_ArrayLiteralInsertValues(t *testing.T) {
	t.Parallel()
	db, ctx := arrayInsertDB(t, "basic")

	t.Run("integer_array", func(t *testing.T) {
		if _, err := db.ExecContext(ctx, "INSERT INTO t_int VALUES (1, [10, 20, 30])"); err != nil {
			t.Fatalf("insert: %v", err)
		}
		got := scanArrayCell(t, db, ctx, "t_int", 1)
		want := []any{int64(10), int64(20), int64(30)}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("read back: got %#v, want %#v", got, want)
		}
	})

	t.Run("bigint_array_int_literal_promotes", func(t *testing.T) {
		// Java: PromoteValue INT_TO_LONG per element (in-predicate.yamsql
		// inserts [10, 20, 30] into a `bigint array` column).
		if _, err := db.ExecContext(ctx, "INSERT INTO t_big VALUES (1, [10, 20, 30])"); err != nil {
			t.Fatalf("insert: %v", err)
		}
		got := scanArrayCell(t, db, ctx, "t_big", 1)
		want := []any{int64(10), int64(20), int64(30)}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("read back: got %#v, want %#v", got, want)
		}
	})

	t.Run("double_array_mixed_literals_promote", func(t *testing.T) {
		// [1.5, 2, 3]: the literal's own element type folds to DOUBLE
		// (MaximumType), the INT elements are promoted (INT_TO_DOUBLE).
		if _, err := db.ExecContext(ctx, "INSERT INTO t_dbl VALUES (1, [1.5, 2, 3])"); err != nil {
			t.Fatalf("insert: %v", err)
		}
		got := scanArrayCell(t, db, ctx, "t_dbl", 1)
		want := []any{1.5, 2.0, 3.0}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("read back: got %#v, want %#v", got, want)
		}
	})

	t.Run("string_array", func(t *testing.T) {
		if _, err := db.ExecContext(ctx, "INSERT INTO t_str VALUES (1, ['a', 'b', 'c'])"); err != nil {
			t.Fatalf("insert: %v", err)
		}
		got := scanArrayCell(t, db, ctx, "t_str", 1)
		want := []any{"a", "b", "c"}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("read back: got %#v, want %#v", got, want)
		}
	})

	t.Run("wrong_element_type_rejected_22000", func(t *testing.T) {
		// Java: no STRING→INT promotion edge → SemanticException
		// INCOMPATIBLE_TYPE → CANNOT_CONVERT_TYPE (SQLSTATE 22000).
		_, err := db.ExecContext(ctx, "INSERT INTO t_int VALUES (90, ['a', 'b'])")
		requireSQLSTATE(t, err, api.ErrCodeCannotConvertType)
	})

	t.Run("scalar_into_array_column_rejected_22000", func(t *testing.T) {
		_, err := db.ExecContext(ctx, "INSERT INTO t_int VALUES (91, 5)")
		requireSQLSTATE(t, err, api.ErrCodeCannotConvertType)
	})

	t.Run("null_element_rejected", func(t *testing.T) {
		// Java forbids NULL elements in collections
		// (MessageHelpers.coerceArray, SemanticException UNSUPPORTED —
		// surfaces as the unmapped internal-error class; upstream
		// fdb-record-layer#3646 tracks lifting this).
		_, err := db.ExecContext(ctx, "INSERT INTO t_int VALUES (92, [10, NULL, 30])")
		requireSQLSTATE(t, err, api.ErrCodeInternalError)
	})

	t.Run("empty_array", func(t *testing.T) {
		// `[]` is Java's untyped empty array (emptyArrayOfNone), promoted
		// by the INSERT target to a typed empty array. empty array is
		// distinct from NULL through the NullableArrayWrapper (present
		// wrapper, empty list) — Java's semantics: it reads back as an
		// empty array, NOT SQL NULL (contrast the null_array subtest).
		if _, err := db.ExecContext(ctx, "INSERT INTO t_int VALUES (2, [])"); err != nil {
			t.Fatalf("insert: %v", err)
		}
		got := scanArrayCell(t, db, ctx, "t_int", 2)
		if arr, ok := got.([]any); !ok || len(arr) != 0 {
			t.Fatalf("read back: got %#v, want empty []any ([] ≠ NULL through the wrapper)", got)
		}
	})

	t.Run("update_set_array_literal", func(t *testing.T) {
		// The UPDATE path converts through the executor's goToProtoValue,
		// not the plan-time ConvertToProtoValue — its repeated arm is a
		// separate fix direction (prepared.yamsql upstream does
		// `update ta set e = [10, 100, 1000]`).
		if _, err := db.ExecContext(ctx, "INSERT INTO t_big VALUES (5, [1, 2])"); err != nil {
			t.Fatalf("seed: %v", err)
		}
		if _, err := db.ExecContext(ctx, "UPDATE t_big SET x = [10, 100, 1000] WHERE pk = 5"); err != nil {
			t.Fatalf("update: %v", err)
		}
		got := scanArrayCell(t, db, ctx, "t_big", 5)
		want := []any{int64(10), int64(100), int64(1000)}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("read back after update: got %#v, want %#v", got, want)
		}
	})

	t.Run("cast_array_literal_to_string_array", func(t *testing.T) {
		// Java CastValue ARRAY_TO_ARRAY: element-wise cast; the walker must
		// keep the `ARRAY` suffix of `CAST(x AS STRING ARRAY)` (dropping it
		// used to die "No cast defined from ARRAY to STRING"). Pins the
		// closure of the cast-array gap (upstream
		// documentation-queries/cast-documentation-queries.yamsql).
		var got any
		if err := db.QueryRowContext(ctx, "SELECT CAST([1, 2, 3] AS STRING ARRAY) FROM t_int WHERE pk = 1").Scan(&got); err != nil {
			t.Fatalf("cast: %v", err)
		}
		want := []any{"1", "2", "3"}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("cast result: got %#v, want %#v", got, want)
		}
		// The untyped empty array casts to a typed empty array (Java
		// emptyArrayOfNone promoted via NONE→ARRAY).
		if err := db.QueryRowContext(ctx, "SELECT CAST([] AS STRING ARRAY) FROM t_int WHERE pk = 1").Scan(&got); err != nil {
			t.Fatalf("cast empty: %v", err)
		}
		if arr, ok := got.([]any); !ok || len(arr) != 0 {
			t.Fatalf("cast empty result: got %#v, want empty []any", got)
		}
	})

	t.Run("null_array", func(t *testing.T) {
		if _, err := db.ExecContext(ctx, "INSERT INTO t_int VALUES (3, NULL)"); err != nil {
			t.Fatalf("insert: %v", err)
		}
		if got := scanArrayCell(t, db, ctx, "t_int", 3); got != nil {
			t.Fatalf("read back: got %#v, want nil", got)
		}
	})
}

// TestFDB_ArrayLiteralInsertWireBytes pins the stored wire bytes of an
// array-literal INSERT: the record must serialize as the table
// descriptor's array shape — for the NULLABLE column X that is the
// NullableArrayWrapper message field (Java's nullable-array `values`
// wrapper wire shape) — byte-equal to the same message built directly
// through protobuf. A representation change (e.g. dropping the wrapper
// on one side only) breaks this before it corrupts a shared cluster.
func TestFDB_ArrayLiteralInsertWireBytes(t *testing.T) {
	t.Parallel()
	db, ctx := arrayInsertDB(t, "wire")

	if _, err := db.ExecContext(ctx, "INSERT INTO t_int VALUES (7, [10, 20, 30])"); err != nil {
		t.Fatalf("insert: %v", err)
	}

	// Open the record store the driver wrote — empty keyspace root,
	// schema subspace [dbPath, schemaName] (keyspace.SchemaSubspace) —
	// with an equivalent metadata build, and load the raw record.
	b := metadata.NewSchemaTemplateBuilder().SetName("arrins_wire_check")
	b.AddTable("T_INT", []metadata.ColumnSpec{
		metadata.NewColumnSpec("PK", api.NewLongType(true), 1),
		metadata.NewColumnSpec("X", api.NewArrayType(api.NewIntegerType(false), true), 2),
	}, []string{"PK"})
	tmpl, err := b.Build()
	if err != nil {
		t.Fatalf("metadata build: %v", err)
	}
	md := tmpl.Underlying()
	desc := md.GetRecordType("T_INT").Descriptor

	fdb.MustAPIVersion(730)
	rawDB, err := fdb.OpenDatabase(clusterFilePath)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	rlDB := recordlayer.NewFDBDatabase(rawDB)
	ss := subspace.Sub().Sub(tuple.Tuple{"/arrins_wire", "MAIN"})

	var storedBytes []byte
	_, err = rlDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
		store, sErr := recordlayer.NewStoreBuilder().
			SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ss).Open()
		if sErr != nil {
			return nil, sErr
		}
		// Non-intermingled layout (the SQL DDL default): the primary key
		// is prefixed with the record type key.
		rec, lErr := store.LoadRecord(tuple.Tuple{md.GetRecordType("T_INT").GetRecordTypeKey(), int64(7)})
		if lErr != nil {
			return nil, lErr
		}
		if rec == nil {
			return nil, fmt.Errorf("record pk=7 not found under %v", ss)
		}
		bs, mErr := proto.MarshalOptions{Deterministic: true}.Marshal(rec.Record)
		if mErr != nil {
			return nil, mErr
		}
		storedBytes = bs
		return nil, nil
	})
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	// Golden: the identical message built straight through protobuf —
	// pk=7, x = {10, 20, 30} (the wrapper shape for the nullable column,
	// via setArrayField).
	golden := dynamicpb.NewMessage(desc)
	golden.Set(desc.Fields().ByName("PK"), protoreflect.ValueOfInt64(7))
	setArrayField(golden, desc.Fields().ByName("X"),
		protoreflect.ValueOfInt32(10), protoreflect.ValueOfInt32(20), protoreflect.ValueOfInt32(30))
	goldenBytes, err := proto.MarshalOptions{Deterministic: true}.Marshal(golden)
	if err != nil {
		t.Fatalf("golden marshal: %v", err)
	}

	if !reflect.DeepEqual(storedBytes, goldenBytes) {
		t.Fatalf("stored record bytes diverge from the plain-repeated golden:\n stored=%x\n golden=%x", storedBytes, goldenBytes)
	}
}
