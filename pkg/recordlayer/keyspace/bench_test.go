package keyspace

import (
	"testing"

	"fdb.dev/pkg/fdbgo/fdb/tuple"
)

func benchKeySpace() *KeySpace {
	root := NewDirectory("root", KeyTypeNull)
	app := NewDirectory("app", KeyTypeString)
	tenant := NewDirectory("tenant", KeyTypeLong)
	recordType := NewDirectory("type", KeyTypeString)
	root.AddSubdirectory(app)
	app.AddSubdirectory(tenant)
	tenant.AddSubdirectory(recordType)
	return NewKeySpace(root)
}

func BenchmarkPath_Construction(b *testing.B) {
	ks := benchKeySpace()
	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		p1, _ := ks.Path("app", "myapp")
		p2, _ := p1.Add("tenant", int64(42))
		_, _ = p2.Add("type", "Order")
	}
}

func BenchmarkPath_ToTuple(b *testing.B) {
	ks := benchKeySpace()
	p1, _ := ks.Path("app", "myapp")
	p2, _ := p1.Add("tenant", int64(42))
	p3, _ := p2.Add("type", "Order")
	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_ = p3.ToTuple()
	}
}

func BenchmarkPath_ToSubspace(b *testing.B) {
	ks := benchKeySpace()
	p1, _ := ks.Path("app", "myapp")
	p2, _ := p1.Add("tenant", int64(42))
	p3, _ := p2.Add("type", "Order")
	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_ = p3.ToSubspace()
	}
}

func BenchmarkPathFromTuple(b *testing.B) {
	ks := benchKeySpace()
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
