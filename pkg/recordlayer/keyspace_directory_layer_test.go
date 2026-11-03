package recordlayer

import (
	"context"
	"fmt"

	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	"fdb.dev/pkg/recordlayer/keyspace"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("KeySpace directory layer resolution", func() {
	ctx := context.Background()
	db := func() *FDBDatabase { return sharedDB }
	// Names unique to the spec: the global directory layer is shared.
	name := func(s string) string { return fmt.Sprintf("%x/%s", specSubspace().Bytes(), s) }
	get := func(key []byte) []byte {
		v, err := sharedDB.RunRead(ctx, func(tx fdb.ReadTransaction) (any, error) { return tx.Get(fdb.Key(key)).Get() })
		Expect(err).NotTo(HaveOccurred())
		return v.([]byte)
	}

	It("keeps ExtendedDirectoryLayer.global's mappings where the directory layer keeps its root's", func() {
		n := name("edl")
		v, err := NewKeySpaceResolver(db(), ExtendedDirectoryLayerGlobal{}).Resolve(ctx, n)
		Expect(err).NotTo(HaveOccurred())
		// (0xFE, (0xFE, 0, name)) -> (value), and the allocation (0xFE, (0xFE, "hca", 1, value)) -> (name).
		mapping := append([]byte{0xFE}, tuple.Tuple{[]byte{0xFE}, int64(0), n}.Pack()...)
		Expect(get(mapping)).To(Equal(tuple.Tuple{v}.Pack()))
		allocation := append([]byte{0xFE}, tuple.Tuple{[]byte{0xFE}, []byte("hca"), int64(1), v}.Pack()...)
		Expect(get(allocation)).To(Equal(tuple.Tuple{n}.Pack()))
		// The directory layer's node metadata (its node, 0xFE + (prefix), and
		// that node's "layer") is not written.
		layer := append([]byte{0xFE}, tuple.Tuple{tuple.Tuple{v}.Pack(), []byte("layer")}.Pack()...)
		Expect(get(layer)).To(BeNil())

		again, err := NewKeySpaceResolver(db(), ExtendedDirectoryLayerGlobal{}).Resolve(ctx, n)
		Expect(err).NotTo(HaveOccurred())
		Expect(again).To(Equal(v))
	})

	It("reads the mappings ScopedDirectoryLayer.global creates and the reverse", func() {
		scoped, extended := NewKeySpaceResolver(db(), GlobalDirectoryLayer{}), NewKeySpaceResolver(db(), ExtendedDirectoryLayerGlobal{})
		for _, c := range []struct {
			creator, reader *KeySpaceResolver
			name            string
		}{{scoped, extended, name("by-scoped")}, {extended, scoped, name("by-extended")}} {
			v, err := c.creator.Resolve(ctx, c.name)
			Expect(err).NotTo(HaveOccurred())
			got, err := c.reader.Resolve(ctx, c.name)
			Expect(err).NotTo(HaveOccurred())
			Expect(got).To(Equal(v), c.name)
			for _, r := range []*KeySpaceResolver{scoped, extended, NewKeySpaceResolver(db(), ExtendedDirectoryLayerGlobal{})} {
				back, ok, err := r.ReverseLookup(ctx, v)
				Expect(err).NotTo(HaveOccurred())
				Expect(ok).To(BeTrue())
				Expect(back).To(Equal(c.name))
			}
		}
		_, ok, err := extended.ReverseLookup(ctx, int64(1)<<60)
		Expect(err).NotTo(HaveOccurred())
		Expect(ok).To(BeFalse())
	})

	It("resolves DirectoryLayerDirectory path elements, each subspace inside its parent's", func() {
		root := keyspace.NewDirectory("/", keyspace.KeyTypeNull)
		app := DirectoryLayerDirectory("app", db())
		Expect(app.AddSubdirectory(keyspace.NewConstantDirectory("data", keyspace.KeyTypeLong, int64(3)))).To(Succeed())
		Expect(root.AddSubdirectory(app)).To(Succeed())
		ks := keyspace.NewKeySpace(root)
		p, err := ks.Path(ctx, "app", name("path"))
		Expect(err).NotTo(HaveOccurred())
		child, err := p.Add(ctx, "data", nil)
		Expect(err).NotTo(HaveOccurred())
		v, err := NewKeySpaceResolver(db(), ExtendedDirectoryLayerGlobal{}).Resolve(ctx, name("path"))
		Expect(err).NotTo(HaveOccurred())
		Expect(p.ToSubspace().Bytes()).To(Equal(tuple.Tuple{v}.Pack()))
		Expect(child.ToSubspace().Bytes()).To(Equal(tuple.Tuple{v, int64(3)}.Pack()))
		Expect(p.ToSubspace().Contains(child.ToSubspace())).To(BeTrue())
		_, err = ks.Path(ctx, "app", int64(5))
		Expect(err).To(HaveOccurred(), "a DirectoryLayerDirectory takes names, as Java's isValueValid")
	})
})
