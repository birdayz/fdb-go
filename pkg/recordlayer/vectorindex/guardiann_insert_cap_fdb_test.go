package vectorindex

import (
	"context"
	"errors"

	"fdb.dev/pkg/recordlayer"

	. "github.com/onsi/ginkgo/v2"

	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	"fdb.dev/pkg/recordlayer/vectorcodec"
	. "github.com/onsi/gomega"
)

// Java writes the vector's identity row before the deferred-mode hard-cap
// check (Insert.java:293, :330-338), so a caller that catches the refusal and
// commits keeps an identity with no reference, and a later insert of that key
// is a no-op. Go checks the cap before the identity write: a refused insert
// leaves no GuardiANN state (declared in DIVERGENCES.md).
var _ = Describe("GuardiANN deferred hard cap", func() {
	It("leaves no identity behind a refused insert, so the key can be inserted later", func() {
		ctx := context.Background()
		cfg := defaultGuardiannConfig(2)
		cfg.primaryClusterMin, cfg.primaryClusterMax, cfg.primaryClusterHardMax = 3, 12, 13
		cfg.collapseMinDuplicates = 6
		cfg.deterministicRandomness = true
		Expect(cfg.validate()).To(Succeed())
		ss := specSubspace().Sub("guardiann-cap")
		vector := func(i int64) gVector { return gVector{data: []float64{float64(i), 1}, typ: vectorcodec.TypeDouble} }
		run := func(f func(g *guardiann, tx fdb.WritableTransaction) error) {
			_, err := sharedDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
				return nil, f(newGuardiann(ss, cfg, nil, nil), rtx.Transaction())
			})
			Expect(err).NotTo(HaveOccurred())
		}
		// Deferred mode: thirteen primaries reach the hard cap, no split runs.
		run(func(g *guardiann, tx fdb.WritableTransaction) error {
			for i := int64(0); i < 13; i++ {
				if err := g.insert(tx, tuple.Tuple{i}, vector(i), nil, false); err != nil {
					return err
				}
			}
			return nil
		})
		refused := tuple.Tuple{int64(13)}
		run(func(g *guardiann, tx fdb.WritableTransaction) error {
			err := g.insert(tx, refused, vector(13), nil, false)
			var capacity *guardiannClusterCapacityError
			Expect(errors.As(err, &capacity)).To(BeTrue(), "error: %v", err)
			Expect(capacity.Error()).To(Equal("primary cluster reached its hard cap while the deferred split backlog is not being drained"))
			Expect(capacity.size).To(Equal(14))
			Expect(capacity.hardMax).To(Equal(13))
			return nil // the caller catches the refusal and commits
		})
		run(func(g *guardiann, tx fdb.WritableTransaction) error {
			md, err := g.fetchVectorMetadata(tx, refused)
			Expect(err).NotTo(HaveOccurred())
			Expect(md).To(BeNil(), "the refused insert left an identity row")
			return nil
		})
		// Inline mode relieves the backlog; the refused key then indexes normally.
		run(func(g *guardiann, tx fdb.WritableTransaction) error {
			return g.insert(tx, refused, vector(13), nil, true)
		})
		run(func(g *guardiann, tx fdb.WritableTransaction) error {
			md, err := g.fetchVectorMetadata(tx, refused)
			Expect(err).NotTo(HaveOccurred())
			Expect(md).NotTo(BeNil())
			res, err := g.search(tx, 1, defaultGuardiannSearchConfig(), vector(13))
			Expect(err).NotTo(HaveOccurred())
			Expect(res).To(HaveLen(1))
			Expect(res[0].primaryKey).To(Equal(refused))
			return nil
		})
	})
})
