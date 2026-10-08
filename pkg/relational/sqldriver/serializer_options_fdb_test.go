package sqldriver_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"fdb.dev/pkg/relational/sqltest/testkit"

	"fdb.dev/pkg/recordlayer"
	"fdb.dev/pkg/recordlayer/keystoretest"
	"fdb.dev/pkg/relational/api"
	"fdb.dev/pkg/relational/core/ddl"
	"fdb.dev/pkg/relational/core/embedded"
	"fdb.dev/pkg/relational/core/fleet"
)

// TestFDB_WritesOverARowTheConnectionCannotRead: a row written encrypted, then
// written, updated or deleted by a connection without the key. Java's
// saveTypedRecord and deleteTypedRecord load the existing record through the
// serializer before anything else (FDBRecordStore.loadExistingRecord), so each
// statement fails DESERIALIZATION_FAILURE (XXF01, ExceptionUtil) and the row is
// untouched; an INSERT of the same key is XXF01, not the 23505 its existence
// check would give. Java's serialization-options.yamsql measures the reads.
func TestFDB_WritesOverARowTheConnectionCannotRead(t *testing.T) {
	t.Parallel()
	db := testkit.SetupErrorDB(t, "/TEST/SER_NO_KEY", "SER_NO_KEY", `create table t(id bigint, s string, primary key(id))`)
	ctx := context.Background()

	keyStore := filepath.Join(t.TempDir(), "keys.p12")
	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(i + 1)
	}
	if err := keystoretest.WritePKCS12(keyStore, "YAML+SQL", "YAML+SQL", []keystoretest.Entry{{Alias: "key-1", Key: key}}); err != nil {
		t.Fatal(err)
	}
	withOptions := func(opts *api.Options) *sql.Conn {
		t.Helper()
		conn, err := db.Conn(ctx)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { conn.Close() })
		if err := conn.Raw(func(dc any) error {
			dc.(*embedded.EmbeddedConnection).SetOptions(opts)
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		return conn
	}
	encrypted := withOptions(api.NewOptionsBuilder().
		Set(api.OptEncryptWhenSerializing, true).
		Set(api.OptEncryptionKeyStore, keyStore).
		Set(api.OptEncryptionKeyEntry, "key-1").
		Set(api.OptEncryptionKeyPassword, "YAML+SQL").Build())
	if _, err := encrypted.ExecContext(ctx, `INSERT INTO t VALUES (1, 'secret')`); err != nil {
		t.Fatalf("encrypted insert: %v", err)
	}

	plain := withOptions(api.NoOptions())
	code := func(err error) string {
		var ae *api.Error
		if errors.As(err, &ae) {
			return string(ae.Code)
		}
		return "no api.Error: " + err.Error()
	}
	for _, q := range []string{
		`INSERT INTO t VALUES (1, 'overwrite')`,
		`UPDATE t SET s = 'overwrite' WHERE id = 1`,
		`DELETE FROM t WHERE id = 1`,
	} {
		_, err := plain.ExecContext(ctx, q)
		if err == nil || code(err) != string(api.ErrCodeDeserializationFailure) {
			t.Errorf("%s without the key: %v, want XXF01", q, err)
		}
	}
	if err := plain.QueryRowContext(ctx, `SELECT s FROM t WHERE id = 1`).Scan(new(string)); err == nil ||
		code(err) != string(api.ErrCodeDeserializationFailure) {
		t.Errorf("read without the key: %v, want XXF01", err)
	}
	var s string
	if err := encrypted.QueryRowContext(ctx, `SELECT s FROM t WHERE id = 1`).Scan(&s); err != nil || s != "secret" {
		t.Fatalf("the row after the refused writes: %q, %v; want it untouched", s, err)
	}
}

// TestFDB_FleetBuildsAnEncryptedTenantThroughItsSerializer: a tenant whose rows
// are encrypted has its index built and its statistics collected by the fleet
// only through fleet.Options.Serializer, the serializer the SQL driver opens
// its stores with. Without it the build fails the target ("this serializer
// cannot decrypt") and the index stays pending; with it the index is built and
// the statistics collected. (PendingIndexes opens only the store's header, so
// no record read pins the serializer it is given; the migration opens no user
// store.)
func TestFDB_FleetBuildsAnEncryptedTenantThroughItsSerializer(t *testing.T) {
	t.Parallel()
	if testkit.ClusterFile() == "" {
		t.Skip("FDB not available (no Docker)")
	}
	h := testkit.NewFleetHarness(t)
	const dbPath = "/FRL/testdb_fleet_encrypted"
	const tmplName = "FLEETENC"
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	keyStore := filepath.Join(t.TempDir(), "keys.p12")
	key := make([]byte, 16)
	for i := range key {
		key[i] = byte(0x40 + i)
	}
	if err := keystoretest.WritePKCS12(keyStore, "fleetpass", "fleetpass", []keystoretest.Entry{{Alias: "k", Key: key}}); err != nil {
		t.Fatal(err)
	}
	h.MustRun(t, "bootstrap", func(txn api.Transaction) error {
		if err := h.Cat.Initialize(txn); err != nil {
			return err
		}
		if err := ddl.NewCreateDatabaseConstantAction(dbPath, h.Cat).Execute(txn); err != nil {
			return err
		}
		if err := ddl.NewSaveSchemaTemplateConstantAction(testkit.FleetTemplate(t, tmplName, 1, false), h.Cat.SchemaTemplateCatalog()).Execute(txn); err != nil {
			return err
		}
		return ddl.NewCreateSchemaConstantAction(dbPath, "S1", tmplName, h.Cat, h.Ks).Execute(txn)
	})
	// A connection encrypting as the tenant's does; a fresh one reads the
	// template the schema is bound to when it opens.
	encrypting := func() *sql.Conn {
		t.Helper()
		conn, err := testkit.FleetOpen(t, dbPath, "S1").Conn(ctx)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { conn.Close() })
		if err := conn.Raw(func(dc any) error {
			dc.(*embedded.EmbeddedConnection).SetOptions(api.NewOptionsBuilder().
				Set(api.OptEncryptWhenSerializing, true).
				Set(api.OptEncryptionKeyStore, keyStore).
				Set(api.OptEncryptionKeyEntry, "k").
				Set(api.OptEncryptionKeyPassword, "fleetpass").Build())
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		return conn
	}
	conn := encrypting()
	var vals []string
	for i := 1; i <= 20; i++ {
		vals = append(vals, fmt.Sprintf("(%d,%d,%d)", i, i%10, i*10))
	}
	if _, err := conn.ExecContext(ctx, "INSERT INTO T (PK,C,V) VALUES "+strings.Join(vals, ",")); err != nil {
		t.Fatalf("encrypted insert: %v", err)
	}

	km, err := recordlayer.NewKeyStoreSerializationKeyManagerBuilder().SetKeyStoreFileName(keyStore).
		SetKeyStorePassword("fleetpass").SetDefaultKeyEntryAlias("k").Build()
	if err != nil {
		t.Fatal(err)
	}
	ser, err := recordlayer.NewTransformedRecordSerializerJCEBuilder().SetEncryptWhenSerializing(true).SetKeyManager(km).Build()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := testkit.MigrateFleet(t, h, dbPath, tmplName, fleet.Options{}); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	// The fan-out returns its tally and the targets' errors joined.
	bare, err := fleet.BuildAll(ctx, h.DB, h.Cat, h.Ks, dbPath, fleet.BuildOptions{Limit: 5})
	if bare.Failed != 1 || bare.Built != 0 || err == nil || !strings.Contains(err.Error(), "this serializer cannot decrypt") {
		t.Fatalf("without the serializer: %+v, %v; want the one target failed \"this serializer cannot decrypt\"", bare, err)
	}
	targets := testkit.FleetTargets(t, h, dbPath)
	if len(targets) != 1 {
		t.Fatalf("targets: %v", targets)
	}
	md, err := fleet.PinnedMetadata(ctx, h.DB, h.Cat, targets[0])
	if err != nil {
		t.Fatal(err)
	}
	ss, err := h.Ks.SchemaSubspaceIn(ctx, h.DB, targets[0].DatabaseID, targets[0].SchemaName)
	if err != nil {
		t.Fatal(err)
	}
	if pending, err := fleet.PendingIndexes(ctx, h.DB, md, ss, ser); err != nil || len(pending) != 1 || pending[0].Name != "T_BY_C" {
		t.Fatalf("after the failed build: pending %v, %v; want [T_BY_C] still pending", testkit.FleetIndexNames(pending), err)
	}
	built, err := fleet.BuildAll(ctx, h.DB, h.Cat, h.Ks, dbPath, fleet.BuildOptions{Options: fleet.Options{Serializer: ser}, Limit: 5})
	if err != nil || built.Built != 1 || built.Failed != 0 {
		t.Fatalf("with the serializer: %+v, %v; want the target built", built, err)
	}
	stats, err := fleet.CollectAllStatistics(ctx, h.DB, h.Cat, h.Ks, dbPath, fleet.StatisticsOptions{Options: fleet.Options{Serializer: ser}})
	if err != nil || stats.Collected != 1 || stats.Failed != 0 {
		t.Fatalf("statistics with the serializer: %+v, %v; want the target collected", stats, err)
	}
	var n int
	if err := encrypting().QueryRowContext(ctx, "SELECT COUNT(*) FROM T WHERE C = 3").Scan(&n); err != nil || n != 2 {
		t.Fatalf("after the build: %d, %v; want 2", n, err)
	}
}
