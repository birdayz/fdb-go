package keyspace

import (
	"testing"

	"fdb.dev/pkg/fdbgo/fdb/tuple"
)

func benchKeySpace(b *testing.B) *KeySpace {
	root := NewDirectory("root", KeyTypeNull)
	app := NewDirectory("app", KeyTypeString)
	tenant := NewDirectory("tenant", KeyTypeLong)
	recordType := NewDirectory("type", KeyTypeString)
	mustAdd(b, root, app)
	mustAdd(b, app, tenant)
	mustAdd(b, tenant, recordType)
	return NewKeySpace(root)
}

func BenchmarkPath_Construction(b *testing.B) {
	ks := benchKeySpace(b)
	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		p1, _ := ks.Path(ctx, "app", "myapp")
		p2, _ := p1.Add(ctx, "tenant", int64(42))
		_, _ = p2.Add(ctx, "type", "Order")
	}
}

func BenchmarkPath_ToTuple(b *testing.B) {
	ks := benchKeySpace(b)
	p1, _ := ks.Path(ctx, "app", "myapp")
	p2, _ := p1.Add(ctx, "tenant", int64(42))
	p3, _ := p2.Add(ctx, "type", "Order")
	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_ = p3.ToTuple()
	}
}

func BenchmarkPath_ToSubspace(b *testing.B) {
	ks := benchKeySpace(b)
	p1, _ := ks.Path(ctx, "app", "myapp")
	p2, _ := p1.Add(ctx, "tenant", int64(42))
	p3, _ := p2.Add(ctx, "type", "Order")
	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_ = p3.ToSubspace()
	}
}

func BenchmarkPathFromTuple(b *testing.B) {
	ks := benchKeySpace(b)
	tup := tuple.Tuple{"myapp", int64(42), "Order"}
	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_, _, _ = ks.PathFromTuple(tup)
	}
}

func BenchmarkMemoryResolver_Hit(b *testing.B) {
	r := NewMemoryResolver(0)
	ctx := b.Context()
	if _, err := r.Resolve(ctx, "preloaded"); err != nil {
		b.Fatalf("warm-up Resolve failed: %v", err)
	}
	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_, _ = r.Resolve(ctx, "preloaded")
	}
}
