package keyspace_test

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/fdb"
	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/fdb/subspace"
	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/fdb/tuple"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/keyspace"
	tcfdb "github.com/birdayz/fdb-record-layer-go/pkg/testcontainers/foundationdb"
	. "github.com/onsi/gomega"
)

var sharedDB fdb.Database

func TestMain(m *testing.M) {
	fdb.MustAPIVersion(730)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	container, err := tcfdb.Run(ctx, "", tcfdb.WithStorageEngine("ssd"))
	if err != nil {
		fmt.Fprintf(os.Stderr, "start FDB container: %v\n", err)
		os.Exit(1)
	}
	defer container.Terminate(ctx)

	// Wait for cluster to be ready
	for i := 0; i < 30; i++ {
		time.Sleep(1 * time.Second)
		code, reader, execErr := container.Exec(ctx, []string{"fdbcli", "--exec", "status minimal"})
		if execErr == nil && reader != nil && code == 0 {
			out, _ := io.ReadAll(reader)
			if strings.Contains(string(out), "available") {
				break
			}
		}
	}

	path, err := container.ClusterFilePath(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "cluster file: %v\n", err)
		os.Exit(1)
	}
	sharedDB, err = fdb.OpenDatabase(path)
	if err != nil {
		fmt.Fprintf(os.Stderr, "open db: %v\n", err)
		os.Exit(1)
	}
	defer sharedDB.Close()

	os.Exit(m.Run())
}

func TestFDBResolver_ResolveAllocatesNew(t *testing.T) {
	t.Parallel()
	g := NewWithT(t)
	ctx := context.Background()

	ss := subspace.Sub(tuple.Tuple{t.Name()})
	// Clean up any prior test data
	_, err := sharedDB.Transact(func(tx fdb.Transaction) (any, error) {
		begin, end := ss.FDBRangeKeys()
		tx.ClearRange(fdb.KeyRange{Begin: begin.FDBKey(), End: end.FDBKey()})
		return nil, nil
	})
	g.Expect(err).NotTo(HaveOccurred())

	r := keyspace.NewFDBResolver(sharedDB, ss)

	v1, err := r.Resolve(ctx, "foo")
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(v1).To(Equal(int64(0)))

	v2, err := r.Resolve(ctx, "bar")
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(v2).To(Equal(int64(1)))

	// Idempotent — same name returns same value
	v1b, err := r.Resolve(ctx, "foo")
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(v1b).To(Equal(int64(0)))
}

func TestFDBResolver_Persistence(t *testing.T) {
	t.Parallel()
	g := NewWithT(t)
	ctx := context.Background()

	ss := subspace.Sub(tuple.Tuple{t.Name()})
	_, err := sharedDB.Transact(func(tx fdb.Transaction) (any, error) {
		begin, end := ss.FDBRangeKeys()
		tx.ClearRange(fdb.KeyRange{Begin: begin.FDBKey(), End: end.FDBKey()})
		return nil, nil
	})
	g.Expect(err).NotTo(HaveOccurred())

	// First resolver writes
	r1 := keyspace.NewFDBResolver(sharedDB, ss)
	v, err := r1.Resolve(ctx, "persisted")
	g.Expect(err).NotTo(HaveOccurred())

	// Invalidate GRV cache so r2's transaction sees r1's commit.
	sharedDB.InvalidateGRVCache()

	// Second resolver with same subspace (simulating restart) reads the same value
	r2 := keyspace.NewFDBResolver(sharedDB, ss)
	v2, err := r2.Resolve(ctx, "persisted")
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(v2).To(Equal(v), "second resolver should see persisted mapping")
}

func TestFDBResolver_ReverseLookup(t *testing.T) {
	t.Parallel()
	g := NewWithT(t)
	ctx := context.Background()

	ss := subspace.Sub(tuple.Tuple{t.Name()})
	_, err := sharedDB.Transact(func(tx fdb.Transaction) (any, error) {
		begin, end := ss.FDBRangeKeys()
		tx.ClearRange(fdb.KeyRange{Begin: begin.FDBKey(), End: end.FDBKey()})
		return nil, nil
	})
	g.Expect(err).NotTo(HaveOccurred())

	r := keyspace.NewFDBResolver(sharedDB, ss)

	v, _ := r.Resolve(ctx, "myapp")
	name, ok, err := r.ReverseLookup(ctx, v)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(ok).To(BeTrue())
	g.Expect(name).To(Equal("myapp"))

	// Non-existent value
	_, ok, err = r.ReverseLookup(ctx, 99999)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(ok).To(BeFalse())
}
