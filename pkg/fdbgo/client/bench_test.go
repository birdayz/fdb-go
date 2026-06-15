package client

import (
	"context"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	fdb "github.com/apple/foundationdb/bindings/go/src/fdb"
	tcfdb "github.com/birdayz/fdb-record-layer-go/pkg/testcontainers/foundationdb"
)

// Benchmarks comparing pure Go FDB client vs CGo (libfdb_c) client.
//
// Both clients connect to the same FDB testcontainer and read the same key.
// Measures the full Get path: GRV + locate + read + parse.
//
// Run:
//
//	bazelisk run //pkg/fdbgo/client:client_test -- \
//	  -test.run='^$' \
//	  -test.bench='BenchmarkGetValue' \
//	  -test.benchtime=10s \
//	  -test.benchmem \
//	  -test.count=3

func BenchmarkGetValue_PureGo(b *testing.B) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	db := openBenchDB(b, ctx)
	defer db.Close()

	_, err := db.Transact(ctx, func(tx *Transaction) (any, error) {
		tx.Set([]byte("bench_key"), make([]byte, 100))
		return nil, nil
	})
	if err != nil {
		b.Fatalf("seed: %v", err)
	}

	db.Transact(ctx, func(tx *Transaction) (any, error) {
		return tx.Get(ctx, []byte("bench_key"))
	})

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, err := db.Transact(ctx, func(tx *Transaction) (any, error) {
			return tx.Get(ctx, []byte("bench_key"))
		})
		if err != nil {
			b.Fatalf("get: %v", err)
		}
	}
}

func BenchmarkGetValue_CGo(b *testing.B) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	container, err := tcfdb.Run(ctx, "", tcfdb.WithStorageEngine("ssd"), tcfdb.WithDirectIP())
	if err != nil {
		b.Fatalf("start container: %v", err)
	}
	b.Cleanup(func() { container.Terminate(ctx) })

	connStr, err := container.ClusterFile(ctx)
	if err != nil {
		b.Fatalf("cluster file: %v", err)
	}

	clusterFile := b.TempDir() + "/fdb.cluster"
	if err := os.WriteFile(clusterFile, []byte(connStr), 0o644); err != nil {
		b.Fatalf("write cluster file: %v", err)
	}

	fdb.MustAPIVersion(730)
	cgoDB := fdb.MustOpenDatabase(clusterFile)

	_, err = cgoDB.Transact(func(tx fdb.Transaction) (any, error) {
		tx.Set(fdb.Key("bench_key"), make([]byte, 100))
		return nil, nil
	})
	if err != nil {
		b.Fatalf("seed: %v", err)
	}

	cgoDB.Transact(func(tx fdb.Transaction) (any, error) {
		return tx.Get(fdb.Key("bench_key")).Get()
	})

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, err := cgoDB.Transact(func(tx fdb.Transaction) (any, error) {
			return tx.Get(fdb.Key("bench_key")).Get()
		})
		if err != nil {
			b.Fatalf("get: %v", err)
		}
	}
}

func openBenchDB(b *testing.B, ctx context.Context) *Database {
	b.Helper()

	container, err := tcfdb.Run(ctx, "", tcfdb.WithStorageEngine("ssd"), tcfdb.WithDirectIP())
	if err != nil {
		b.Fatalf("start FDB container: %v", err)
	}
	b.Cleanup(func() { container.Terminate(ctx) })

	connStr, err := container.ClusterFile(ctx)
	if err != nil {
		b.Fatalf("get cluster file: %v", err)
	}

	cf, err := ParseClusterString(connStr)
	if err != nil {
		b.Fatalf("parse cluster string: %v", err)
	}

	_, internalReader, err := container.Exec(ctx, []string{"cat", "/var/fdb/fdb.cluster"})
	if err != nil {
		b.Fatalf("read internal cluster file: %v", err)
	}
	internalBytes, _ := io.ReadAll(internalReader)
	internalStr := string(internalBytes)
	if idx := strings.Index(internalStr, cf.Description); idx >= 0 {
		internalStr = internalStr[idx:]
	}
	internalCF, err := ParseClusterString(strings.TrimSpace(internalStr))
	if err != nil {
		b.Fatalf("parse internal cluster: %v", err)
	}

	connectCF := &ClusterFile{
		Description:  internalCF.Description,
		ID:           internalCF.ID,
		Coordinators: cf.Coordinators,
	}

	db, err := OpenDatabaseFromConfig(ctx, connectCF)
	if err != nil {
		b.Fatalf("OpenDatabaseFromConfig: %v", err)
	}
	b.Cleanup(func() { db.Close() })

	return db
}
