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

// readRawTextIndex returns only the final transaction attempt's index rows.
// afterRead can force a retry after a completed subspace has produced rows.
func readRawTextIndex(ctx context.Context, db *recordlayer.FDBDatabase, s *TextIndexConformanceStore, afterRead func(int) error) ([]string, error) {
	result, err := db.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
		var attempt []string
		for _, ss := range []subspace.Subspace{
			s.Keyspace.Sub(recordlayer.IndexKey, s.TextIndex.SubspaceTupleKey()),
			s.Keyspace.Sub(recordlayer.IndexSecondarySpaceKey, s.TextIndex.SubspaceTupleKey()),
		} {
			kvs, err := rtx.Transaction().GetRange(ss, fdb.RangeOptions{}).GetSliceWithError()
			if err != nil {
				return nil, err
			}
			for _, kv := range kvs {
				attempt = append(attempt, hex.EncodeToString(kv.Key[len(s.Keyspace.Bytes()):])+"="+hex.EncodeToString(kv.Value))
			}
			if afterRead != nil {
				if err := afterRead(len(attempt)); err != nil {
					return nil, err
				}
			}
		}
		return attempt, nil
	})
	if err != nil {
		return nil, err
	}
	return result.([]string), nil
}

// Concurrent TEXT_INDEX_UPDATE reads must preserve Java's index bytes and bunch layout.
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
		out, err := readRawTextIndex(ctx, env.RecordDB, s, nil)
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

	It("discards a partial raw index dump when a subspace read is retried", func() {
		Expect(goStore.SaveCustomerGo(ctx, &demopb.Customer{CustomerId: proto.Int64(1), Name: proto.String("alpha beta gamma")})).To(Succeed())
		baseline := rawTextIndex(goStore)
		Expect(baseline).NotTo(BeEmpty())
		faults, partialRows, reads := 0, 0, 0
		got, err := readRawTextIndex(ctx, env.RecordDB, goStore, func(rows int) error {
			reads++
			if faults == 0 {
				faults++
				partialRows = rows
				return fdb.Error{Code: 1020}
			}
			return nil
		})
		Expect(err).NotTo(HaveOccurred())
		Expect(faults).To(Equal(1))
		Expect(partialRows).To(BeNumerically(">", 0))
		Expect(reads).To(BeNumerically(">=", 3))
		Expect(got).To(Equal(baseline))
	})

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

	// Both engines must write tokenizer versions on insert, update differing versions,
	// and clear them on delete (TextIndexMaintainer.java:438-484).
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
