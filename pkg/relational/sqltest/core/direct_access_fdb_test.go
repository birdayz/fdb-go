package sqltest

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"testing"

	"fdb.dev/pkg/relational/sqltest/testkit"

	"github.com/google/uuid"

	"fdb.dev/pkg/relational/api"
	"fdb.dev/pkg/relational/core/embedded"
	"fdb.dev/pkg/relational/core/rowstruct"
)

// openDirectAccessDB creates dbPath with schema S over Java's UniqueIndexTests
// T4 (#4243's UUID attribute and its nested unique index mv5b) and a table
// with a scalar UUID column.
func openDirectAccessDB(t *testing.T, dbPath, template string) *sql.DB {
	t.Helper()
	ctx := context.Background()
	setup := testkit.OpenDB(t, dbPath)
	testkit.MustExecCtx(t, setup, ctx, "CREATE DATABASE "+dbPath)
	testkit.MustExecCtx(t, setup, ctx, "CREATE SCHEMA TEMPLATE "+template+" "+
		"CREATE TYPE AS STRUCT ST1(st1_a bigint, st1_b uuid) "+
		"CREATE TABLE T4(t4_p bigint, t4_st1 st1 array, primary key(t4_p)) "+
		"CREATE UNIQUE INDEX mv5b AS SELECT v.st1_b from t4 t, (SELECT u.st1_b from t.t4_st1 u) v "+
		"CREATE TABLE TU(tu_p bigint, tu_k bigint, tu_u uuid, primary key(tu_p, tu_k))")
	testkit.MustExecCtx(t, setup, ctx, "CREATE SCHEMA "+dbPath+"/s WITH TEMPLATE "+template)
	db, err := sql.Open("fdbsql",
		fmt.Sprintf("fdbsql://%s?cluster_file=%s&schema=S", strings.ToUpper(dbPath), testkit.ClusterFile()))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

// directAccess runs fn with the connection's direct-access statement.
func directAccess(t *testing.T, db *sql.DB, fn func(api.DirectAccessStatement) error) error {
	t.Helper()
	conn, err := db.Conn(context.Background())
	if err != nil {
		t.Fatalf("db.Conn: %v", err)
	}
	defer conn.Close()
	return conn.Raw(func(driverConn any) error {
		ec, ok := driverConn.(*embedded.EmbeddedConnection)
		if !ok {
			t.Fatalf("driver connection is %T, want *embedded.EmbeddedConnection", driverConn)
		}
		return fn(ec.DirectAccess())
	})
}

// TestFDB_DirectAccessNestedUUIDUniqueIndex is Java's UniqueIndexTests
// insertToArrayNestedUuidFieldMarkedUnique (#4243): UUIDs inside structs of an
// array are inserted through the direct-access API, a unique index over them
// accepts distinct UUIDs and refuses a repeated one, and SQL reads the UUIDs
// back.
func TestFDB_DirectAccessNestedUUIDUniqueIndex(t *testing.T) {
	t.Parallel()
	db := openDirectAccessDB(t, "/FRL/testdb_direct_uuid_unique", "direct_uuid_unique")
	ctx := context.Background()

	nonUnique := uuid.New()
	var records []api.Struct
	for i := 0; i < 5; i++ {
		arr := rowstruct.NewArrayBuilder()
		for j := 0; j < 5; j++ {
			u := uuid.New()
			if i == 2 && j == 3 {
				u = nonUnique
			}
			arr.AddStruct(rowstruct.NewStructBuilder().AddUUID("ST1_B", u).Build())
		}
		records = append(records, rowstruct.NewStructBuilder().AddLong("T4_P", int64(i)).AddArray("T4_ST1", arr.Build()).Build())
	}
	if err := directAccess(t, db, func(s api.DirectAccessStatement) error {
		n, err := s.ExecuteInsert(ctx, "T4", records, nil)
		if err == nil && n != 5 {
			t.Errorf("inserted %d records, want 5", n)
		}
		return err
	}); err != nil {
		t.Fatalf("unique inserts: %v", err)
	}

	duplicate := []api.Struct{rowstruct.NewStructBuilder().AddLong("T4_P", 5).
		AddArray("T4_ST1", rowstruct.NewArrayBuilder().
			AddStruct(rowstruct.NewStructBuilder().AddUUID("ST1_B", nonUnique).Build()).Build()).
		Build()}
	err := directAccess(t, db, func(s api.DirectAccessStatement) error {
		_, err := s.ExecuteInsert(ctx, "T4", duplicate, nil)
		return err
	})
	var apiErr *api.Error
	if !errors.As(err, &apiErr) || apiErr.Code != api.ErrCodeUniqueConstraintViolation {
		t.Fatalf("repeated nested UUID: err = %v, want SQLSTATE %s", err, api.ErrCodeUniqueConstraintViolation)
	}

	var got string
	if err := db.QueryRowContext(ctx,
		"SELECT u.st1_b FROM t4 t, t.t4_st1 u WHERE t.t4_p = 2 AND u.st1_b = ?", nonUnique.String()).Scan(&got); err != nil {
		t.Fatalf("reading the UUID back: %v", err)
	}
	if got != nonUnique.String() {
		t.Errorf("read back %s, want %s", got, nonUnique)
	}
	var count int64
	if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM t4").Scan(&count); err != nil || count != 5 {
		t.Errorf("COUNT(*) = %d (%v), want 5: the refused insert wrote nothing", count, err)
	}
}

// TestFDB_DirectAccessRoundTrip pins the rest of Java's direct-access
// statement over a scalar UUID column and a two-column primary key: get by the
// complete key, scan by a key prefix, the duplicate-key refusal and its
// REPLACE_ON_DUPLICATE_PK override, delete, and delete over a prefix.
func TestFDB_DirectAccessRoundTrip(t *testing.T) {
	t.Parallel()
	db := openDirectAccessDB(t, "/FRL/testdb_direct_roundtrip", "direct_roundtrip")
	ctx := context.Background()

	u := []uuid.UUID{uuid.New(), uuid.New(), uuid.New(), uuid.New()}
	row := func(p, k int64, v uuid.UUID) api.Struct {
		return rowstruct.NewStructBuilder().AddLong("TU_P", p).AddLong("TU_K", k).AddUUID("TU_U", v).Build()
	}
	key := func(cols ...any) *api.KeySet {
		ks := api.NewKeySet()
		for i := 0; i < len(cols); i += 2 {
			_, _ = ks.SetKeyColumn(cols[i].(string), cols[i+1])
		}
		return ks
	}
	check := func(what string, err error) {
		t.Helper()
		if err != nil {
			t.Fatalf("%s: %v", what, err)
		}
	}

	check("insert", directAccess(t, db, func(s api.DirectAccessStatement) error {
		_, err := s.ExecuteInsert(ctx, "TU", []api.Struct{row(1, 1, u[0]), row(1, 2, u[1]), row(2, 1, u[2])}, nil)
		return err
	}))

	check("get", directAccess(t, db, func(s api.DirectAccessStatement) error {
		rs, err := s.ExecuteGet(ctx, "TU", key("TU_P", int64(1), "TU_K", int64(2)), nil)
		if err != nil {
			return err
		}
		if !rs.Next() {
			t.Fatal("get found no row")
		}
		if got, _ := rs.ObjectByName("TU_U"); got != u[1].String() {
			t.Errorf("get TU_U = %v, want %s", got, u[1])
		}
		if rs.Next() {
			t.Error("get returned a second row")
		}
		return nil
	}))

	check("scan", directAccess(t, db, func(s api.DirectAccessStatement) error {
		rs, err := s.ExecuteScan(ctx, "TU", key("TU_P", int64(1)), nil)
		if err != nil {
			return err
		}
		var ks []int64
		for rs.Next() {
			k, _ := rs.LongByName("TU_K")
			ks = append(ks, k)
		}
		if fmt.Sprint(ks) != "[1 2]" {
			t.Errorf("scan of TU_P = 1 read TU_K %v, want [1 2]", ks)
		}
		return nil
	}))

	err := directAccess(t, db, func(s api.DirectAccessStatement) error {
		_, err := s.ExecuteInsert(ctx, "TU", []api.Struct{row(1, 1, u[3])}, nil)
		return err
	})
	var apiErr *api.Error
	if !errors.As(err, &apiErr) || apiErr.Code != api.ErrCodeUniqueConstraintViolation {
		t.Fatalf("duplicate key: err = %v, want SQLSTATE %s", err, api.ErrCodeUniqueConstraintViolation)
	}
	check("replace", directAccess(t, db, func(s api.DirectAccessStatement) error {
		_, err := s.ExecuteInsert(ctx, "TU", []api.Struct{row(1, 1, u[3])},
			api.NewOptionsBuilder().Set(api.OptReplaceOnDuplicatePK, true).Build())
		return err
	}))
	var replaced string
	check("read replaced", db.QueryRowContext(ctx, "SELECT tu_u FROM tu WHERE tu_p = 1 AND tu_k = 1").Scan(&replaced))
	if replaced != u[3].String() {
		t.Errorf("replaced TU_U = %s, want %s", replaced, u[3])
	}

	check("delete", directAccess(t, db, func(s api.DirectAccessStatement) error {
		n, err := s.ExecuteDelete(ctx, "TU", key("TU_P", int64(2), "TU_K", int64(1)), nil)
		if err == nil && n != 1 {
			t.Errorf("deleted %d records, want 1", n)
		}
		return err
	}))
	check("delete range", directAccess(t, db, func(s api.DirectAccessStatement) error {
		n, err := s.ExecuteDeleteRange(ctx, "TU", key("TU_P", int64(1)), nil)
		if err == nil && n != 2 {
			t.Errorf("deleted %d records over TU_P = 1, want 2", n)
		}
		return err
	}))
	var left int64
	check("count", db.QueryRowContext(ctx, "SELECT COUNT(*) FROM tu").Scan(&left))
	if left != 0 {
		t.Errorf("%d records left, want 0", left)
	}
}
