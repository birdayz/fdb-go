package keyspace_test

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/fdbgo/fdb/subspace"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	"fdb.dev/pkg/recordlayer/keyspace"
	tcfdb "fdb.dev/pkg/testcontainers/foundationdb"
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
	_, err := sharedDB.Transact(func(tx fdb.WritableTransaction) (any, error) {
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
	_, err := sharedDB.Transact(func(tx fdb.WritableTransaction) (any, error) {
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

func TestFDBResolver_EmptyStringName(t *testing.T) {
	t.Parallel()
	g := NewWithT(t)
	ctx := context.Background()

	ss := subspace.Sub(tuple.Tuple{t.Name()})
	_, err := sharedDB.Transact(func(tx fdb.WritableTransaction) (any, error) {
		begin, end := ss.FDBRangeKeys()
		tx.ClearRange(fdb.KeyRange{Begin: begin.FDBKey(), End: end.FDBKey()})
		return nil, nil
	})
	g.Expect(err).NotTo(HaveOccurred())

	r := keyspace.NewFDBResolver(sharedDB, ss)

	// Resolve empty string — should work
	v, err := r.Resolve(ctx, "")
	g.Expect(err).NotTo(HaveOccurred())

	// Invalidate cache to force FDB read
	r.InvalidateCache()

	// ReverseLookup should return ("", true, nil) — not ("", false, nil)!
	// Before the fix, the empty-string sentinel made this a false negative.
	name, ok, err := r.ReverseLookup(ctx, v)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(ok).To(BeTrue(), "empty-string name should be found, not treated as missing")
	g.Expect(name).To(Equal(""))
}

func TestFDBResolver_CacheManagement(t *testing.T) {
	t.Parallel()
	g := NewWithT(t)
	ctx := context.Background()

	ss := subspace.Sub(tuple.Tuple{t.Name()})
	_, err := sharedDB.Transact(func(tx fdb.WritableTransaction) (any, error) {
		begin, end := ss.FDBRangeKeys()
		tx.ClearRange(fdb.KeyRange{Begin: begin.FDBKey(), End: end.FDBKey()})
		return nil, nil
	})
	g.Expect(err).NotTo(HaveOccurred())

	r := keyspace.NewFDBResolver(sharedDB, ss)

	// Initial cache is empty
	g.Expect(r.CacheSize()).To(Equal(0))

	// Resolve populates cache
	_, _ = r.Resolve(ctx, "a")
	_, _ = r.Resolve(ctx, "b")
	_, _ = r.Resolve(ctx, "c")
	g.Expect(r.CacheSize()).To(Equal(3))

	// InvalidateCache clears it
	r.InvalidateCache()
	g.Expect(r.CacheSize()).To(Equal(0))

	// After invalidation, Resolve still returns correct value from FDB
	v, err := r.Resolve(ctx, "a")
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(v).To(Equal(int64(0)), "should read persisted value from FDB")
}

// TestFDBResolver_ResolverDirectory_EndToEnd exercises the full
// Phase 1+2+3 pipeline: FDBResolver plugs into ResolverDirectory,
// which lets KeySpace paths use persistent FDB-backed string→int64 mapping.
func TestFDBResolver_ResolverDirectory_EndToEnd(t *testing.T) {
	t.Parallel()
	g := NewWithT(t)

	ss := subspace.Sub(tuple.Tuple{t.Name()})
	_, err := sharedDB.Transact(func(tx fdb.WritableTransaction) (any, error) {
		begin, end := ss.FDBRangeKeys()
		tx.ClearRange(fdb.KeyRange{Begin: begin.FDBKey(), End: end.FDBKey()})
		return nil, nil
	})
	g.Expect(err).NotTo(HaveOccurred())

	resolver := keyspace.NewFDBResolver(sharedDB, ss)

	// Schema: root → app (resolved via FDB) → table (open string)
	root := keyspace.NewDirectory("root", keyspace.KeyTypeNull)
	root.AddSubdirectory(keyspace.ResolverDirectory("app", resolver))
	root.GetSubdirectory("app").AddSubdirectory(keyspace.NewDirectory("table", keyspace.KeyTypeString))

	ks := keyspace.NewKeySpace(root)

	// First app resolution allocates value 0
	path1, err := ks.Path("app", "myapp")
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(path1.GetValue()).To(Equal(int64(0)))

	path1t, err := path1.Add("table", "orders")
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(path1t.ToTuple()).To(Equal(tuple.Tuple{int64(0), "orders"}))

	// Different app gets value 1
	path2, err := ks.Path("app", "otherapp")
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(path2.GetValue()).To(Equal(int64(1)))

	// Same app reuses value 0 (from cache)
	path3, err := ks.Path("app", "myapp")
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(path3.GetValue()).To(Equal(int64(0)))

	// Fresh resolver with invalidated cache still sees persisted mapping
	resolver.InvalidateCache()
	sharedDB.InvalidateGRVCache()
	path4, err := ks.Path("app", "myapp")
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(path4.GetValue()).To(Equal(int64(0)), "persisted mapping survives cache flush")
}

// TestFDBResolver_ConcurrentCrossInstance simulates two separate resolver
// instances racing on allocation for the same name. FDB's transactional
// write conflict on the counter key ensures exactly one allocation wins.
func TestFDBResolver_ConcurrentCrossInstance(t *testing.T) {
	t.Parallel()
	g := NewWithT(t)
	ctx := context.Background()

	ss := subspace.Sub(tuple.Tuple{t.Name()})
	_, err := sharedDB.Transact(func(tx fdb.WritableTransaction) (any, error) {
		begin, end := ss.FDBRangeKeys()
		tx.ClearRange(fdb.KeyRange{Begin: begin.FDBKey(), End: end.FDBKey()})
		return nil, nil
	})
	g.Expect(err).NotTo(HaveOccurred())

	// Create N independent resolver instances (simulating cross-process).
	const instances = 8
	resolvers := make([]*keyspace.FDBResolver, instances)
	for i := range resolvers {
		resolvers[i] = keyspace.NewFDBResolver(sharedDB, ss)
	}

	// Each instance resolves the same name concurrently.
	results := make(chan int64, instances)
	errs := make(chan error, instances)
	for _, r := range resolvers {
		r := r
		go func() {
			v, err := r.Resolve(ctx, "contested")
			if err != nil {
				errs <- err
				return
			}
			results <- v
		}()
	}

	// All should return the same value (FDB conflict resolution).
	seen := make(map[int64]int)
	for i := 0; i < instances; i++ {
		select {
		case v := <-results:
			seen[v]++
		case err := <-errs:
			t.Fatalf("unexpected error: %v", err)
		}
	}
	g.Expect(seen).To(HaveLen(1), "all instances must converge on the same value, got %v", seen)
}

func TestFDBResolver_ReverseLookup(t *testing.T) {
	t.Parallel()
	g := NewWithT(t)
	ctx := context.Background()

	ss := subspace.Sub(tuple.Tuple{t.Name()})
	_, err := sharedDB.Transact(func(tx fdb.WritableTransaction) (any, error) {
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
