package keyspace

import (
	"context"
	"sync"
	"testing"

	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/fdb/tuple"
	. "github.com/onsi/gomega"
)

func TestMemoryResolver_Resolve(t *testing.T) {
	t.Parallel()
	g := NewWithT(t)

	r := NewMemoryResolver(100)
	ctx := context.Background()

	// First resolution allocates a value
	v1, err := r.Resolve(ctx, "foo")
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(v1).To(Equal(int64(100)))

	// Same name returns same value (idempotent)
	v2, err := r.Resolve(ctx, "foo")
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(v2).To(Equal(int64(100)))

	// Different name gets next value
	v3, err := r.Resolve(ctx, "bar")
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(v3).To(Equal(int64(101)))

	g.Expect(r.Size()).To(Equal(2))
}

func TestMemoryResolver_ReverseLookup(t *testing.T) {
	t.Parallel()
	g := NewWithT(t)

	r := NewMemoryResolver(0)
	ctx := context.Background()

	v, _ := r.Resolve(ctx, "app1")
	name, ok, err := r.ReverseLookup(ctx, v)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(ok).To(BeTrue())
	g.Expect(name).To(Equal("app1"))

	// Non-existent value
	_, ok, err = r.ReverseLookup(ctx, 9999)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(ok).To(BeFalse())
}

func TestMemoryResolver_Concurrent(t *testing.T) {
	t.Parallel()
	g := NewWithT(t)

	r := NewMemoryResolver(0)
	ctx := context.Background()

	const goroutines = 20
	const perGoroutine = 50
	var wg sync.WaitGroup
	seen := sync.Map{}

	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for j := 0; j < perGoroutine; j++ {
				v, err := r.Resolve(ctx, "app1") // same name — should get same value
				g.Expect(err).NotTo(HaveOccurred())
				seen.Store(v, true)
			}
		}(i)
	}
	wg.Wait()

	// All goroutines resolving the same name should see the same value
	count := 0
	seen.Range(func(_, _ any) bool {
		count++
		return true
	})
	g.Expect(count).To(Equal(1), "concurrent Resolve of same name should return same value")
}

func TestResolverDirectory(t *testing.T) {
	t.Parallel()
	g := NewWithT(t)

	resolver := NewMemoryResolver(1000)

	root := NewDirectory("root", KeyTypeNull)
	root.AddSubdirectory(ResolverDirectory("app", resolver))

	ks := NewKeySpace(root)

	// Path with string name — should resolve to int64
	path, err := ks.Path("app", "myapp")
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(path.GetValue()).To(Equal(int64(1000)))

	// Tuple contains resolved value
	g.Expect(path.ToTuple()).To(Equal(tuple.Tuple{int64(1000)}))

	// Second time returns same value
	path2, err := ks.Path("app", "myapp")
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(path2.GetValue()).To(Equal(int64(1000)))

	// Different name gets different value
	path3, err := ks.Path("app", "otherapp")
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(path3.GetValue()).To(Equal(int64(1001)))

	// Reverse lookup works
	name, ok, err := resolver.ReverseLookup(context.Background(), 1000)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(ok).To(BeTrue())
	g.Expect(name).To(Equal("myapp"))
}

func TestResolverDirectoryTypeError(t *testing.T) {
	t.Parallel()
	g := NewWithT(t)

	resolver := NewMemoryResolver(0)

	root := NewDirectory("root", KeyTypeNull)
	root.AddSubdirectory(ResolverDirectory("app", resolver))
	ks := NewKeySpace(root)

	// Matches Java's DirectoryLayerDirectory.isValueValid: strings OK, ints rejected.
	// Passing an int is rejected (would be a cluster-specific value, likely a bug).
	_, err := ks.Path("app", int64(42))
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("expected string"))

	// Passing a string succeeds (resolved to int64).
	path, err := ks.Path("app", "myapp")
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(path.GetValue()).To(BeAssignableToTypeOf(int64(0)))
}
