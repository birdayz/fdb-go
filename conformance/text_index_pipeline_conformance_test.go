//go:build bazelrunfiles

package conformance_test

import (
	"context"
	"encoding/hex"
	"fmt"
	"strings"

	"github.com/google/uuid"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"google.golang.org/protobuf/proto"

	"fdb.dev/gen/demopb"
	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/fdbgo/fdb/subspace"
	"fdb.dev/pkg/recordlayer"
)

// Go's TEXT maintainer reads the token maps of one record concurrently
// (TEXT_INDEX_UPDATE pipeline, as Java's TextIndexMaintainer). The index it
// writes must be Java's byte for byte, bunch layout included.
var _ = Describe("TEXT index pipelined update conformance", func() {
	var (
		ctx                context.Context
		env                *TenantEnvironment
		goStore, javaStore *TextIndexConformanceStore
	)

	BeforeEach(func() {
		ctx = context.Background()
		var err error
		env, err = SetupTenantEnvironment(ctx, sharedContainer, fmt.Sprintf("txp_%s", uuid.New().String()))
		Expect(err).NotTo(HaveOccurred())
		goStore, err = NewTextIndexConformanceStore(env.RecordDB, env.Keyspace, env.ClusterFile, env.TenantName)
		Expect(err).NotTo(HaveOccurred())
		javaStore, err = NewTextIndexConformanceStore(env.RecordDB, env.Keyspace, env.ClusterFile, env.TenantName)
		Expect(err).NotTo(HaveOccurred())
		goStore.Keyspace = subspace.Sub("go")
		javaStore.Keyspace = subspace.Sub("java")
	})

	AfterEach(func() {
		if env != nil {
			_ = env.Cleanup(ctx)
		}
	})

	// name has n distinct tokens from a 31-word vocabulary, so each token map
	// holds most records and spans more than one bunch; the first word repeats.
	name := func(seed, n int) string {
		words := make([]string, n)
		for j := range words {
			words[j] = fmt.Sprintf("v%02d", (seed*5+j*2)%31)
		}
		return strings.Join(words, " ") + " " + words[0]
	}

	// rawTextIndex is every key-value of the index's primary and secondary
	// (tokenizer version) subspaces, keys relative to the store.
	rawTextIndex := func(s *TextIndexConformanceStore) []string {
		var out []string
		_, err := env.RecordDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
			for _, ss := range []subspace.Subspace{
				s.Keyspace.Sub(recordlayer.IndexKey, s.TextIndex.SubspaceTupleKey()),
				s.Keyspace.Sub(recordlayer.IndexSecondarySpaceKey, s.TextIndex.SubspaceTupleKey()),
			} {
				kvs, err := rtx.Transaction().GetRange(ss, fdb.RangeOptions{}).GetSliceWithError()
				if err != nil {
					return nil, err
				}
				for _, kv := range kvs {
					out = append(out, hex.EncodeToString(kv.Key[len(s.Keyspace.Bytes()):])+"="+hex.EncodeToString(kv.Value))
				}
			}
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
		return out
	}

	expectSameIndex := func(step string) {
		goRaw, javaRaw := rawTextIndex(goStore), rawTextIndex(javaStore)
		Expect(goRaw).NotTo(BeEmpty(), step)
		Expect(goRaw).To(Equal(javaRaw), step)
		// Java reads the Go-written index as Go does.
		goEntries, err := goStore.ScanTextIndexGo(ctx)
		Expect(err).NotTo(HaveOccurred())
		javaEntries, err := goStore.ScanTextIndexJava(ctx)
		Expect(err).NotTo(HaveOccurred())
		Expect(CompareTextIndexEntries(goEntries, javaEntries)).To(Succeed(), step)
	}

	It("writes Java's index for records with many distinct tokens, through inserts, a token-set change and a delete", func() {
		const records = 45
		for i := int64(0); i < records; i++ {
			c := &demopb.Customer{CustomerId: proto.Int64(i), Name: proto.String(name(int(i), 25))}
			Expect(goStore.SaveCustomerGo(ctx, c)).To(Succeed())
			Expect(javaStore.SaveCustomerJava(ctx, c)).To(Succeed())
		}
		expectSameIndex("insert")
		// More key-values than tokens plus tokenizer versions: maps span bunches.
		Expect(len(rawTextIndex(goStore))).To(BeNumerically(">", 31+records))

		// An update whose token set changes: some tokens leave, some arrive,
		// shared ones change positions.
		updated := &demopb.Customer{CustomerId: proto.Int64(5), Name: proto.String(name(11, 18) + " fresh tokens here")}
		Expect(goStore.SaveCustomerGo(ctx, updated)).To(Succeed())
		Expect(javaStore.SaveCustomerJava(ctx, updated)).To(Succeed())
		expectSameIndex("update")

		deleted, err := goStore.DeleteCustomerGo(ctx, 9)
		Expect(err).NotTo(HaveOccurred())
		Expect(deleted).To(BeTrue())
		Expect(javaStore.DeleteCustomerJava(ctx, 9)).To(Succeed())
		expectSameIndex("delete")

		// Each engine updates a record the other wrote; the indexes stay equal.
		again := &demopb.Customer{CustomerId: proto.Int64(20), Name: proto.String(name(3, 30))}
		Expect(javaStore.SaveCustomerGo(ctx, again)).To(Succeed())
		Expect(goStore.SaveCustomerJava(ctx, again)).To(Succeed())
		expectSameIndex("cross-engine update")
	})

	// Java's TextIndexMaintainer.update writes the record's tokenizer version
	// on every insert, whether or not the record has text, rewrites it on an
	// update only when it differs from the index's, and clears it on delete
	// (TextIndexMaintainer.java:438-484). Both engines leave the same keys.
	It("writes and clears each record's tokenizer version key as Java does", func() {
		versionKeys := func(s *TextIndexConformanceStore) int {
			var n int
			_, err := env.RecordDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
				secondary := s.Keyspace.Sub(recordlayer.IndexSecondarySpaceKey, s.TextIndex.SubspaceTupleKey())
				kvs, err := rtx.Transaction().GetRange(secondary, fdb.RangeOptions{}).GetSliceWithError()
				n = len(kvs)
				return nil, err
			})
			Expect(err).NotTo(HaveOccurred())
			return n
		}
		customer := func(id int64, name *string, email string) *demopb.Customer {
			return &demopb.Customer{CustomerId: proto.Int64(id), Name: name, Email: proto.String(email)}
		}
		for _, step := range []struct {
			name   string
			save   *demopb.Customer
			delete int64
			live   int
		}{
			{name: "insert without text", save: customer(1, nil, "a"), live: 1},
			{name: "insert with empty text", save: customer(2, proto.String(""), "b"), live: 2},
			{name: "insert with text", save: customer(3, proto.String("alpha beta"), "c"), live: 3},
			{name: "update without a text change", save: customer(3, proto.String("alpha beta"), "c2"), live: 3},
			{name: "update adding text", save: customer(1, proto.String("gamma"), "a"), live: 3},
			{name: "update changing text", save: customer(3, proto.String("beta delta"), "c2"), live: 3},
			{name: "delete without text", delete: 2, live: 2},
			{name: "delete with text", delete: 3, live: 1},
		} {
			if step.save != nil {
				Expect(goStore.SaveCustomerGo(ctx, step.save)).To(Succeed(), step.name)
				Expect(javaStore.SaveCustomerJava(ctx, step.save)).To(Succeed(), step.name)
			} else {
				deleted, err := goStore.DeleteCustomerGo(ctx, step.delete)
				Expect(err).NotTo(HaveOccurred(), step.name)
				Expect(deleted).To(BeTrue(), step.name)
				Expect(javaStore.DeleteCustomerJava(ctx, step.delete)).To(Succeed(), step.name)
			}
			expectSameIndex(step.name)
			Expect(versionKeys(goStore)).To(Equal(step.live), step.name)
		}
	})
})
