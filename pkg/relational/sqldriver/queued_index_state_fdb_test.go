package sqldriver_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/fdbgo/fdb/subspace"
	"fdb.dev/pkg/recordlayer"
	"fdb.dev/pkg/recordlayer/vectorcodec"
	"fdb.dev/pkg/relational/api"
	apiddl "fdb.dev/pkg/relational/api/ddl"
	rlddl "fdb.dev/pkg/relational/core/ddl"
	"fdb.dev/pkg/relational/core/embedded"
	relkeyspace "fdb.dev/pkg/relational/core/keyspace"
)

// TestFDB_QueuedVectorIndexIsWriteOnlyToSQL pins the relational treatment of
// WRITE_ONLY_WITH_QUEUE (Java 4.14's queued index state, which its relational
// layer reaches through RecordLayerSetStoreStateConstantAction): a store moved
// to format 15 with its GuardiANN index queued, out of band, keeps both when
// the SQL driver opens it; INSERT succeeds and leaves the index queued (its
// writes go to the pending queue, not the index); and the planner does not
// read the queued index, which is not READABLE.
func TestFDB_QueuedVectorIndexIsWriteOnlyToSQL(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	const ddl = "CREATE TABLE docs (id BIGINT, embedding VECTOR(2, DOUBLE), PRIMARY KEY (id)) " +
		"CREATE VECTOR INDEX docsIdx USING GUARDIANN ON docs (embedding) " +
		"OPTIONS (metric = euclidean_metric, primary_cluster_min = 2, primary_cluster_max = 10, collapse_min_duplicates = 5)"
	const dbPath, schemaName = "/FRL/testdb_queued_state", "queued_state"
	db := setupErrorTestDB(t, dbPath, schemaName, ddl)

	tmpl, err := embedded.BuildSchemaTemplateFromDDLNamed(ddl, strings.ToUpper(schemaName)+"_TMPL")
	if err != nil {
		t.Fatalf("DDL: %v", err)
	}
	md := tmpl.Underlying()
	fdb.MustAPIVersion(730)
	rawDB, err := fdb.OpenDatabase(clusterFilePath)
	if err != nil {
		t.Fatalf("open raw FDB: %v", err)
	}
	t.Cleanup(rawDB.Close)
	rdb := recordlayer.NewFDBDatabase(rawDB)
	ss, err := relkeyspace.New(subspace.Sub()).LookupSchemaSubspace(context.Background(), rdb, strings.ToUpper(dbPath), strings.ToUpper(schemaName))
	if err != nil {
		t.Fatalf("schema subspace: %v", err)
	}
	inStore := func(format int32, f func(*recordlayer.FDBRecordStore) error) {
		t.Helper()
		if _, err := rdb.Run(ctx, func(rctx *recordlayer.FDBRecordContext) (any, error) {
			b := recordlayer.NewStoreBuilder().SetContext(rctx).SetMetaDataProvider(md).SetSubspace(ss).
				SetStoreStateCache(recordlayer.PassThroughStoreStateCache())
			if format != 0 {
				b = b.SetFormatVersion(format)
			}
			store, openErr := b.Open()
			if openErr != nil {
				return nil, openErr
			}
			return nil, f(store)
		}); err != nil {
			t.Fatal(err)
		}
	}
	// Queue the index as Java's `set schema state` does: the store-state action
	// in one metadata transaction. At the store's format 14 Go refuses it
	// (DIVERGENCES "Queued index states require format 15 ..."); at 15 it lands.
	setState := func(format int32) error {
		conn, err := db.Conn(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close()
		return conn.Raw(func(dc any) error {
			ec, ok := dc.(*embedded.EmbeddedConnection)
			if !ok {
				return fmt.Errorf("need an embedded connection, got %T", dc)
			}
			return ec.ApplyMetadataOperation(ctx, func(f apiddl.MetadataOperationsFactory, txn api.Transaction) error {
				return f.(*rlddl.RecordLayerMetadataOperationsFactory).SetStoreState(strings.ToUpper(dbPath), strings.ToUpper(schemaName),
					rlddl.RecordLayerConfig{IndexStates: map[string]recordlayer.IndexState{"DOCSIDX": recordlayer.IndexStateWriteOnlyWithQueue}, FormatVersion: format}).Execute(txn)
			})
		})
	}
	var unsupported *recordlayer.UnsupportedFeatureForFormatVersionError
	if err := setState(0); !errors.As(err, &unsupported) {
		t.Fatalf("set schema state WRITE_ONLY_WITH_QUEUE at format 14: %v; want UnsupportedFeatureForFormatVersionError", err)
	}
	if err := setState(15); err != nil {
		t.Fatalf("set schema state WRITE_ONLY_WITH_QUEUE at format 15: %v", err)
	}

	for i, v := range [][]float64{{1, 0}, {0.9, 0.1}, {0, 1}} {
		if _, err := db.ExecContext(ctx, "INSERT INTO docs VALUES (?, ?)", int64(i+1), vectorcodec.Serialize(v)); err != nil {
			t.Fatalf("insert %d into a queued index's table: %v", i+1, err)
		}
	}

	inStore(0, func(store *recordlayer.FDBRecordStore) error {
		if got := store.GetFormatVersion(); got != 15 {
			return fmt.Errorf("store format = %d after SQL writes, want 15 (never downgraded)", got)
		}
		if got := store.GetIndexState("DOCSIDX"); got != recordlayer.IndexStateWriteOnlyWithQueue {
			return fmt.Errorf("DOCSIDX state = %s after SQL writes, want WRITE_ONLY_WITH_QUEUE", got)
		}
		return nil
	})

	knn := "SELECT id FROM docs QUALIFY row_number() OVER (ORDER BY euclidean_distance(embedding, [1.0, 0.0]) ASC) <= 2"
	// The distance rank is index-only: with its one index queued, nothing
	// serves it, and the query is unplannable (0AF00), not answered from it.
	var plan string
	err = db.QueryRowContext(ctx, "EXPLAIN "+knn).Scan(&plan)
	if apiErr := asAPIError(err); apiErr == nil || apiErr.Code != api.ErrCodeUnsupportedQuery {
		t.Errorf("KNN over the queued index: plan %q, err %v; want 0AF00 (the queued index is not READABLE)", plan, err)
	}
	var n int64
	if err := db.QueryRowContext(ctx, "SELECT count(*) FROM docs").Scan(&n); err != nil || n != 3 {
		t.Fatalf("count = %d, %v; want 3", n, err)
	}
}
