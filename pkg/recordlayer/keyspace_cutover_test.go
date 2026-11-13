package recordlayer

import (
	"context"
	"errors"
	"fmt"

	"fdb.dev/gen/demopb"
	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/fdbgo/fdb/subspace"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	"fdb.dev/pkg/recordlayer/keyspace"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"google.golang.org/protobuf/proto"
)

var _ = Describe("KeySpace legacy prefix cutover", func() {
	ctx := context.Background()
	path := func() *keyspace.Path {
		root := keyspace.NewDirectory("root", keyspace.KeyTypeNull)
		Expect(root.AddSubdirectory(keyspace.NewDirectory("app", keyspace.KeyTypeString))).To(Succeed())
		p, err := keyspace.NewKeySpace(root).Path(ctx, "app", fmt.Sprintf("cutover/%x", specSubspace().Bytes()))
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func() {
			_, err := sharedDB.Run(ctx, func(rc *FDBRecordContext) (any, error) {
				for _, ss := range []subspace.Subspace{p.ToSubspace(), p.LegacyNestedSubspace()} {
					r, err := fdb.PrefixRange(ss.Bytes())
					if err != nil {
						return nil, err
					}
					rc.Transaction().ClearRange(r)
				}
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})
		return p
	}
	It("retains explicit access to a populated old store and refuses silent flat creation", func() {
		p := path()
		md, err := baseBuilder().Build()
		Expect(err).NotTo(HaveOccurred())
		legacy, flat := p.LegacyNestedSubspace().Sub("store"), p.ToSubspace().Sub("store")
		Expect(flat.Bytes()).To(Equal(append(p.ToTuple().Pack(), tuple.Tuple{"store"}.Pack()...)))
		_, err = sharedDB.Run(ctx, func(rc *FDBRecordContext) (any, error) {
			s, err := NewStoreBuilder().SetContext(rc).SetMetaDataProvider(md).SetSubspace(legacy).CreateOrOpen()
			if err != nil {
				return nil, err
			}
			_, err = s.SaveRecord(&demopb.Customer{CustomerId: proto.Int64(7), Name: proto.String("legacy")})
			return nil, err
		})
		Expect(err).NotTo(HaveOccurred())
		for _, mode := range []string{"create", "open", "create-or-open"} {
			_, err = sharedDB.Run(ctx, func(rc *FDBRecordContext) (any, error) {
				b := NewStoreBuilder().SetContext(rc).SetMetaDataProvider(md).SetSubspace(flat)
				switch mode {
				case "create":
					return b.Create()
				case "open":
					return b.Open()
				default:
					return b.CreateOrOpen()
				}
			})
			var legacyErr *LegacyKeySpacePrefixError
			Expect(errors.As(err, &legacyErr)).To(BeTrue(), mode)
			Expect(legacyErr.LegacyPrefix).To(Equal(legacy.Bytes()))
		}
		_, err = sharedDB.Run(ctx, func(rc *FDBRecordContext) (any, error) {
			s, err := NewStoreBuilder().SetContext(rc).SetMetaDataProvider(md).SetSubspace(legacy).Open()
			if err != nil {
				return nil, err
			}
			r, err := s.LoadRecord(tuple.Tuple{int64(7)})
			if err == nil {
				Expect(r).NotTo(BeNil())
				Expect(r.Record.ProtoReflect().Get(r.Record.ProtoReflect().Descriptor().Fields().ByName("name")).String()).To(Equal("legacy"))
			}
			return nil, err
		})
		Expect(err).NotTo(HaveOccurred())
		_, err = sharedDB.Run(ctx, func(rc *FDBRecordContext) (any, error) {
			b := NewStoreBuilder().SetContext(rc).SetMetaDataProvider(md).SetSubspace(flat).SetAllowLegacyKeySpaceCutover(true)
			s, err := b.CreateOrOpen()
			if err != nil {
				return nil, err
			}
			r, err := s.LoadRecord(tuple.Tuple{int64(7)})
			Expect(r).To(BeNil(), "cutover is explicit, not an automatic data copy")
			return nil, err
		})
		Expect(err).NotTo(HaveOccurred())
	})
	It("orders fresh flat creation against a concurrent legacy writer", func() {
		p := path()
		md, err := baseBuilder().Build()
		Expect(err).NotTo(HaveOccurred())
		tx, err := sharedDB.CreateWritableTransaction()
		Expect(err).NotTo(HaveOccurred())
		defer tx.Cancel()
		rc := sharedDB.NewRecordContextIn(ctx, tx)
		_, err = NewStoreBuilder().SetContext(rc).SetMetaDataProvider(md).SetSubspace(p.ToSubspace()).CreateOrOpen()
		Expect(err).NotTo(HaveOccurred())
		_, err = sharedDB.Run(ctx, func(other *FDBRecordContext) (any, error) {
			other.Transaction().Set(p.LegacyNestedSubspace().Pack(tuple.Tuple{"concurrent"}), []byte("old writer"))
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
		err = tx.Commit().Get()
		var conflict fdb.Error
		Expect(errors.As(err, &conflict)).To(BeTrue())
		Expect(conflict.Code).To(Equal(1020))
	})
})
