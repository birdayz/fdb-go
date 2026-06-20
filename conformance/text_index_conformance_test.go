//go:build bazelrunfiles

package conformance_test

import (
	"context"
	"fmt"
	"sort"

	"github.com/google/uuid"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"google.golang.org/protobuf/proto"

	"github.com/birdayz/fdb-record-layer-go/gen"
	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/fdb/subspace"
	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/fdb/tuple"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer"
)

// TextIndexEntryResult represents a single TEXT index entry for comparison.
type TextIndexEntryResult struct {
	Token      string
	PrimaryKey int64
	Positions  []int64
}

// TextIndexConformanceStore wraps record operations with a TEXT index on Customer.name.
type TextIndexConformanceStore struct {
	RecordDB    *recordlayer.FDBDatabase
	MetaData    *recordlayer.RecordMetaData
	TextIndex   *recordlayer.Index
	Keyspace    subspace.Subspace
	java        *JavaInvoker
	clusterFile string
	tenantName  string
}

// NewTextIndexConformanceStore creates a conformance store with a TEXT index on Customer.name.
func NewTextIndexConformanceStore(recordDB *recordlayer.FDBDatabase, keyspace subspace.Subspace, clusterFile string, tenantName string) (*TextIndexConformanceStore, error) {
	textIdx := recordlayer.NewTextIndex("customer_name_text", recordlayer.Field("name"))

	builder := recordlayer.NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
	builder.GetRecordType("Order").SetPrimaryKey(recordlayer.Field("order_id"))
	builder.GetRecordType("Customer").SetPrimaryKey(recordlayer.Field("customer_id"))
	builder.GetRecordType("TypedRecord").SetPrimaryKey(recordlayer.Field("id"))
	builder.AddIndex("Customer", textIdx)
	md, err := builder.Build()
	if err != nil {
		return nil, err
	}

	ks := keyspace
	if tenantName != "" {
		ks = subspace.Sub(tuple.Tuple{})
	}

	return &TextIndexConformanceStore{
		RecordDB:    recordDB,
		MetaData:    md,
		TextIndex:   textIdx,
		Keyspace:    ks,
		java:        NewJavaInvoker(),
		clusterFile: clusterFile,
		tenantName:  tenantName,
	}, nil
}

func (s *TextIndexConformanceStore) buildJavaParams() map[string]any {
	params := map[string]any{
		"clusterFile": s.clusterFile,
		"subspace":    BytesToIntArray(s.Keyspace.Bytes()),
	}
	if s.tenantName != "" {
		params["tenantName"] = s.tenantName
	}
	return params
}

// SaveCustomerGo saves a customer with Go (with TEXT index maintenance).
func (s *TextIndexConformanceStore) SaveCustomerGo(ctx context.Context, customer *gen.Customer) error {
	_, err := s.RecordDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
		store, err := recordlayer.NewStoreBuilder().
			SetContext(rtx).SetMetaDataProvider(s.MetaData).SetSubspace(s.Keyspace).CreateOrOpen()
		if err != nil {
			return nil, err
		}
		_, err = store.SaveRecord(customer)
		return nil, err
	})
	return err
}

// SaveCustomerJava saves a customer via Java (with TEXT index maintenance).
func (s *TextIndexConformanceStore) SaveCustomerJava(ctx context.Context, customer *gen.Customer) error {
	params := s.buildJavaParams()
	params["customer"] = customer
	return s.java.InvokeAs(ctx, "saveCustomerWithTextIndex", params, nil)
}

// DeleteCustomerGo deletes a customer with Go (with TEXT index maintenance).
func (s *TextIndexConformanceStore) DeleteCustomerGo(ctx context.Context, customerID int64) (bool, error) {
	var deleted bool
	_, err := s.RecordDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
		store, err := recordlayer.NewStoreBuilder().
			SetContext(rtx).SetMetaDataProvider(s.MetaData).SetSubspace(s.Keyspace).CreateOrOpen()
		if err != nil {
			return nil, err
		}
		deleted, err = store.DeleteRecord(tuple.Tuple{customerID})
		return nil, err
	})
	return deleted, err
}

// DeleteCustomerJava deletes a customer via Java (with TEXT index maintenance).
func (s *TextIndexConformanceStore) DeleteCustomerJava(ctx context.Context, customerID int64) error {
	params := s.buildJavaParams()
	params["customerID"] = customerID
	return s.java.InvokeAs(ctx, "deleteCustomerWithTextIndex", params, nil)
}

// ScanTextIndexGo scans all TEXT index entries using Go.
func (s *TextIndexConformanceStore) ScanTextIndexGo(ctx context.Context) ([]TextIndexEntryResult, error) {
	var results []TextIndexEntryResult
	_, err := s.RecordDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
		store, err := recordlayer.NewStoreBuilder().
			SetContext(rtx).SetMetaDataProvider(s.MetaData).SetSubspace(s.Keyspace).Open()
		if err != nil {
			return nil, err
		}
		entries, err := recordlayer.AsList(ctx, store.ScanIndexByType(
			s.TextIndex, recordlayer.IndexScanByTextToken, recordlayer.TupleRangeAll, nil, recordlayer.ForwardScan()))
		if err != nil {
			return nil, err
		}
		for _, e := range entries {
			results = append(results, goTextEntryToResult(e))
		}
		return nil, nil
	})
	return results, err
}

// ScanTextIndexJava scans all TEXT index entries using Java.
func (s *TextIndexConformanceStore) ScanTextIndexJava(ctx context.Context) ([]TextIndexEntryResult, error) {
	params := s.buildJavaParams()
	return s.parseJavaTextResults(ctx, "scanTextIndex", params)
}

// ScanTextIndexByTokenGo scans TEXT index entries for a specific token using Go.
func (s *TextIndexConformanceStore) ScanTextIndexByTokenGo(ctx context.Context, token string) ([]TextIndexEntryResult, error) {
	var results []TextIndexEntryResult
	_, err := s.RecordDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
		store, err := recordlayer.NewStoreBuilder().
			SetContext(rtx).SetMetaDataProvider(s.MetaData).SetSubspace(s.Keyspace).Open()
		if err != nil {
			return nil, err
		}
		entries, err := recordlayer.AsList(ctx, store.ScanIndexByType(
			s.TextIndex, recordlayer.IndexScanByTextToken,
			recordlayer.TupleRangeAllOf(tuple.Tuple{token}), nil, recordlayer.ForwardScan()))
		if err != nil {
			return nil, err
		}
		for _, e := range entries {
			results = append(results, goTextEntryToResult(e))
		}
		return nil, nil
	})
	return results, err
}

// ScanTextIndexByTokenJava scans TEXT index entries for a specific token using Java.
func (s *TextIndexConformanceStore) ScanTextIndexByTokenJava(ctx context.Context, token string) ([]TextIndexEntryResult, error) {
	params := s.buildJavaParams()
	params["token"] = token
	return s.parseJavaTextResults(ctx, "scanTextIndexByToken", params)
}

// goTextEntryToResult converts a Go IndexEntry from a TEXT index scan to a result struct.
func goTextEntryToResult(e *recordlayer.IndexEntry) TextIndexEntryResult {
	result := TextIndexEntryResult{
		Token:      e.Key[0].(string),
		PrimaryKey: e.Key[1].(int64),
	}
	// Value is Tuple(Tuple(pos0, pos1, ...))
	if len(e.Value) > 0 {
		if inner, ok := e.Value[0].(tuple.Tuple); ok {
			result.Positions = make([]int64, len(inner))
			for i, v := range inner {
				result.Positions[i] = v.(int64)
			}
		}
	}
	return result
}

// parseJavaTextResults parses Java scan results into TextIndexEntryResult.
func (s *TextIndexConformanceStore) parseJavaTextResults(ctx context.Context, step string, params map[string]any) ([]TextIndexEntryResult, error) {
	var javaResults []map[string]any
	if err := s.java.InvokeAs(ctx, step, params, &javaResults); err != nil {
		return nil, fmt.Errorf("java %s failed: %w", step, err)
	}

	var results []TextIndexEntryResult
	for _, m := range javaResults {
		entry := TextIndexEntryResult{}
		if tokenRaw, ok := m["token"]; ok {
			entry.Token = tokenRaw.(string)
		}
		if pkRaw, ok := m["primaryKey"]; ok {
			entry.PrimaryKey = int64(pkRaw.(float64))
		}
		if posRaw, ok := m["positions"]; ok {
			if posSlice, ok := posRaw.([]any); ok {
				entry.Positions = make([]int64, len(posSlice))
				for i, v := range posSlice {
					entry.Positions[i] = int64(v.(float64))
				}
			}
		}
		results = append(results, entry)
	}
	return results, nil
}

// CompareTextIndexEntries compares Go and Java TEXT index scan results.
// Sorts by (token, primaryKey) before comparison.
func CompareTextIndexEntries(goEntries, javaEntries []TextIndexEntryResult) error {
	sortTextEntries(goEntries)
	sortTextEntries(javaEntries)

	if len(goEntries) != len(javaEntries) {
		return fmt.Errorf("entry count mismatch: go=%d java=%d\ngo=%+v\njava=%+v",
			len(goEntries), len(javaEntries), goEntries, javaEntries)
	}
	for i := range goEntries {
		if goEntries[i].Token != javaEntries[i].Token {
			return fmt.Errorf("entry %d token mismatch: go=%q java=%q",
				i, goEntries[i].Token, javaEntries[i].Token)
		}
		if goEntries[i].PrimaryKey != javaEntries[i].PrimaryKey {
			return fmt.Errorf("entry %d primaryKey mismatch: go=%d java=%d",
				i, goEntries[i].PrimaryKey, javaEntries[i].PrimaryKey)
		}
		if len(goEntries[i].Positions) != len(javaEntries[i].Positions) {
			return fmt.Errorf("entry %d positions length mismatch: go=%v java=%v",
				i, goEntries[i].Positions, javaEntries[i].Positions)
		}
		for j := range goEntries[i].Positions {
			if goEntries[i].Positions[j] != javaEntries[i].Positions[j] {
				return fmt.Errorf("entry %d position %d mismatch: go=%d java=%d",
					i, j, goEntries[i].Positions[j], javaEntries[i].Positions[j])
			}
		}
	}
	return nil
}

func sortTextEntries(entries []TextIndexEntryResult) {
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].Token != entries[j].Token {
			return entries[i].Token < entries[j].Token
		}
		return entries[i].PrimaryKey < entries[j].PrimaryKey
	})
}

var _ = Describe("TEXT Index Conformance", func() {
	var (
		ctx   context.Context
		env   *TenantEnvironment
		store *TextIndexConformanceStore
	)

	BeforeEach(func() {
		ctx = context.Background()

		tenantName := fmt.Sprintf("txt_%s", uuid.New().String())

		var err error
		env, err = SetupTenantEnvironment(ctx, sharedContainer, tenantName)
		Expect(err).NotTo(HaveOccurred())

		store, err = NewTextIndexConformanceStore(env.RecordDB, env.Keyspace, env.ClusterFile, env.TenantName)
		Expect(err).NotTo(HaveOccurred())
	})

	AfterEach(func() {
		if env != nil {
			_ = env.Cleanup(ctx)
		}
	})

	Describe("Go writes, both scan TEXT index", func() {
		It("should produce identical text entries visible to both Go and Java", func() {
			// Save a customer with "hello world" — tokenizes to "hello" and "world"
			err := store.SaveCustomerGo(ctx, &gen.Customer{
				CustomerId: proto.Int64(1),
				Name:       proto.String("hello world"),
			})
			Expect(err).NotTo(HaveOccurred())

			// Scan all entries with Go
			goEntries, err := store.ScanTextIndexGo(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(goEntries).To(HaveLen(2)) // "hello" and "world"

			// Scan all entries with Java
			javaEntries, err := store.ScanTextIndexJava(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(javaEntries).To(HaveLen(2))

			// Compare
			err = CompareTextIndexEntries(goEntries, javaEntries)
			Expect(err).NotTo(HaveOccurred())

			// Verify specific tokens
			sortTextEntries(goEntries)
			Expect(goEntries[0].Token).To(Equal("hello"))
			Expect(goEntries[0].PrimaryKey).To(Equal(int64(1)))
			Expect(goEntries[0].Positions).To(Equal([]int64{0}))
			Expect(goEntries[1].Token).To(Equal("world"))
			Expect(goEntries[1].PrimaryKey).To(Equal(int64(1)))
			Expect(goEntries[1].Positions).To(Equal([]int64{1}))
		})
	})

	Describe("Java writes, both scan TEXT index", func() {
		It("should produce identical text entries visible to both Go and Java", func() {
			// Java saves a customer with "quick brown fox"
			err := store.SaveCustomerJava(ctx, &gen.Customer{
				CustomerId: proto.Int64(10),
				Name:       proto.String("quick brown fox"),
			})
			Expect(err).NotTo(HaveOccurred())

			// Scan all entries with Go
			goEntries, err := store.ScanTextIndexGo(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(goEntries).To(HaveLen(3)) // "quick", "brown", "fox"

			// Scan all entries with Java
			javaEntries, err := store.ScanTextIndexJava(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(javaEntries).To(HaveLen(3))

			// Compare
			err = CompareTextIndexEntries(goEntries, javaEntries)
			Expect(err).NotTo(HaveOccurred())

			// Verify specific tokens
			sortTextEntries(goEntries)
			Expect(goEntries[0].Token).To(Equal("brown"))
			Expect(goEntries[0].Positions).To(Equal([]int64{1}))
			Expect(goEntries[1].Token).To(Equal("fox"))
			Expect(goEntries[1].Positions).To(Equal([]int64{2}))
			Expect(goEntries[2].Token).To(Equal("quick"))
			Expect(goEntries[2].Positions).To(Equal([]int64{0}))
		})
	})

	Describe("Mixed writes: Go and Java both insert", func() {
		It("should produce correct combined text entries", func() {
			// Go saves customer 1
			err := store.SaveCustomerGo(ctx, &gen.Customer{
				CustomerId: proto.Int64(1),
				Name:       proto.String("alpha beta"),
			})
			Expect(err).NotTo(HaveOccurred())

			// Java saves customer 2 with overlapping token "beta"
			err = store.SaveCustomerJava(ctx, &gen.Customer{
				CustomerId: proto.Int64(2),
				Name:       proto.String("beta gamma"),
			})
			Expect(err).NotTo(HaveOccurred())

			// Scan for "beta" — should find both records
			goByToken, err := store.ScanTextIndexByTokenGo(ctx, "beta")
			Expect(err).NotTo(HaveOccurred())
			Expect(goByToken).To(HaveLen(2))

			javaByToken, err := store.ScanTextIndexByTokenJava(ctx, "beta")
			Expect(err).NotTo(HaveOccurred())
			Expect(javaByToken).To(HaveLen(2))

			err = CompareTextIndexEntries(goByToken, javaByToken)
			Expect(err).NotTo(HaveOccurred())

			// Scan all — "alpha"(1), "beta"(1), "beta"(2), "gamma"(2) = 4 entries
			goAll, err := store.ScanTextIndexGo(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(goAll).To(HaveLen(4))

			javaAll, err := store.ScanTextIndexJava(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(javaAll).To(HaveLen(4))

			err = CompareTextIndexEntries(goAll, javaAll)
			Expect(err).NotTo(HaveOccurred())
		})
	})

	Describe("Go deletes Java-written record", func() {
		It("should remove text entries visible to both Go and Java", func() {
			// Java saves a customer
			err := store.SaveCustomerJava(ctx, &gen.Customer{
				CustomerId: proto.Int64(5),
				Name:       proto.String("deleteme please"),
			})
			Expect(err).NotTo(HaveOccurred())

			// Verify entries exist
			goEntries, err := store.ScanTextIndexGo(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(goEntries).To(HaveLen(2)) // "deleteme", "please"

			// Go deletes the record
			deleted, err := store.DeleteCustomerGo(ctx, 5)
			Expect(err).NotTo(HaveOccurred())
			Expect(deleted).To(BeTrue())

			// Both should see empty index
			goEntries, err = store.ScanTextIndexGo(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(goEntries).To(BeEmpty())

			javaEntries, err := store.ScanTextIndexJava(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(javaEntries).To(BeEmpty())
		})
	})

	Describe("Java deletes Go-written record", func() {
		It("should remove text entries visible to both Go and Java", func() {
			// Go saves a customer
			err := store.SaveCustomerGo(ctx, &gen.Customer{
				CustomerId: proto.Int64(7),
				Name:       proto.String("removable entry"),
			})
			Expect(err).NotTo(HaveOccurred())

			// Verify entries exist
			javaEntries, err := store.ScanTextIndexJava(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(javaEntries).To(HaveLen(2)) // "removable", "entry"

			// Java deletes the record
			err = store.DeleteCustomerJava(ctx, 7)
			Expect(err).NotTo(HaveOccurred())

			// Both should see empty index
			goEntries, err := store.ScanTextIndexGo(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(goEntries).To(BeEmpty())

			javaEntries, err = store.ScanTextIndexJava(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(javaEntries).To(BeEmpty())
		})
	})

	Describe("Update via Go re-indexes correctly", func() {
		It("should update text entries when record name changes", func() {
			// Save initial record
			err := store.SaveCustomerGo(ctx, &gen.Customer{
				CustomerId: proto.Int64(20),
				Name:       proto.String("original text"),
			})
			Expect(err).NotTo(HaveOccurred())

			// Verify initial entries
			goEntries, err := store.ScanTextIndexGo(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(goEntries).To(HaveLen(2)) // "original", "text"

			// Update name
			err = store.SaveCustomerGo(ctx, &gen.Customer{
				CustomerId: proto.Int64(20),
				Name:       proto.String("updated content"),
			})
			Expect(err).NotTo(HaveOccurred())

			// Verify new entries: "updated", "content" (old "original", "text" removed)
			goEntries, err = store.ScanTextIndexGo(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(goEntries).To(HaveLen(2))

			javaEntries, err := store.ScanTextIndexJava(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(javaEntries).To(HaveLen(2))

			err = CompareTextIndexEntries(goEntries, javaEntries)
			Expect(err).NotTo(HaveOccurred())

			sortTextEntries(goEntries)
			Expect(goEntries[0].Token).To(Equal("content"))
			Expect(goEntries[1].Token).To(Equal("updated"))

			// Old tokens should be gone
			oldTokenEntries, err := store.ScanTextIndexByTokenGo(ctx, "original")
			Expect(err).NotTo(HaveOccurred())
			Expect(oldTokenEntries).To(BeEmpty())

			oldTokenEntries, err = store.ScanTextIndexByTokenGo(ctx, "text")
			Expect(err).NotTo(HaveOccurred())
			Expect(oldTokenEntries).To(BeEmpty())
		})
	})

	Describe("Cross-language token extraction consistency", func() {
		It("should tokenize identically in Go and Java", func() {
			// Text with mixed case, punctuation, and repeated words.
			// Default tokenizer: lowercases, splits on whitespace/punct.
			text := "Hello World hello WORLD"

			// Go writes
			err := store.SaveCustomerGo(ctx, &gen.Customer{
				CustomerId: proto.Int64(30),
				Name:       proto.String(text),
			})
			Expect(err).NotTo(HaveOccurred())

			// Verify Go-written entries exist before Java writes.
			goEntriesInitial, err := store.ScanTextIndexGo(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(goEntriesInitial).To(HaveLen(2)) // "hello", "world"

			// Java writes same text in a fresh store — but same tenant, different PK.
			err = store.SaveCustomerJava(ctx, &gen.Customer{
				CustomerId: proto.Int64(31),
				Name:       proto.String(text),
			})
			Expect(err).NotTo(HaveOccurred())

			// Scan "hello" — should find both records (Go-written pk=30, Java-written pk=31)
			helloGo, err := store.ScanTextIndexByTokenGo(ctx, "hello")
			Expect(err).NotTo(HaveOccurred())
			Expect(helloGo).To(HaveLen(2))

			helloJava, err := store.ScanTextIndexByTokenJava(ctx, "hello")
			Expect(err).NotTo(HaveOccurred())
			Expect(helloJava).To(HaveLen(2))

			err = CompareTextIndexEntries(helloGo, helloJava)
			Expect(err).NotTo(HaveOccurred())

			// Scan "world" — should also find both records
			worldGo, err := store.ScanTextIndexByTokenGo(ctx, "world")
			Expect(err).NotTo(HaveOccurred())
			Expect(worldGo).To(HaveLen(2))

			worldJava, err := store.ScanTextIndexByTokenJava(ctx, "world")
			Expect(err).NotTo(HaveOccurred())
			Expect(worldJava).To(HaveLen(2))

			err = CompareTextIndexEntries(worldGo, worldJava)
			Expect(err).NotTo(HaveOccurred())

			// Verify positions: "Hello World hello WORLD" → tokens: hello(0), world(1), hello(2), world(3)
			// Positions for "hello" should be [0, 2], for "world" should be [1, 3]
			sortTextEntries(helloGo)
			Expect(helloGo[0].Positions).To(Equal([]int64{0, 2}))
			Expect(helloGo[1].Positions).To(Equal([]int64{0, 2}))

			sortTextEntries(worldGo)
			Expect(worldGo[0].Positions).To(Equal([]int64{1, 3}))
			Expect(worldGo[1].Positions).To(Equal([]int64{1, 3}))
		})
	})

	Describe("Unicode diacritical conformance", func() {
		It("should produce identical normalized tokens from diacritical text", func() {
			// "Après-midi café résumé" should normalize to:
			// UAX #29 splits on hyphens → "Après", "midi", "café", "résumé"
			// NFKD + strip marks + lowercase → "apres", "midi", "cafe", "resume"
			err := store.SaveCustomerGo(ctx, &gen.Customer{
				CustomerId: proto.Int64(100),
				Name:       proto.String("Après-midi café résumé"),
			})
			Expect(err).NotTo(HaveOccurred())

			goEntries, err := store.ScanTextIndexGo(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(goEntries).To(HaveLen(4))

			javaEntries, err := store.ScanTextIndexJava(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(javaEntries).To(HaveLen(4))

			err = CompareTextIndexEntries(goEntries, javaEntries)
			Expect(err).NotTo(HaveOccurred())

			// Verify the exact normalized tokens.
			sortTextEntries(goEntries)
			Expect(goEntries[0].Token).To(Equal("apres"))
			Expect(goEntries[1].Token).To(Equal("cafe"))
			Expect(goEntries[2].Token).To(Equal("midi"))
			Expect(goEntries[3].Token).To(Equal("resume"))

			// Verify positions: "Après"(0), "-"(skip), "midi"(1), "café"(2), "résumé"(3)
			Expect(goEntries[0].Positions).To(Equal([]int64{0}))
			Expect(goEntries[1].Positions).To(Equal([]int64{2}))
			Expect(goEntries[2].Positions).To(Equal([]int64{1}))
			Expect(goEntries[3].Positions).To(Equal([]int64{3}))
		})
	})

	Describe("Multiple records per token — bunch splitting", func() {
		It("should return all 25 entries for a shared token from both Go and Java", func() {
			// Save 25 customers each containing "common" — forces BunchedMap
			// bunch splitting (bunchSize=20, so at least 2 bunches).
			for i := int64(200); i < 225; i++ {
				err := store.SaveCustomerGo(ctx, &gen.Customer{
					CustomerId: proto.Int64(i),
					Name:       proto.String(fmt.Sprintf("common extra%d", i)),
				})
				Expect(err).NotTo(HaveOccurred())
			}

			// Scan for "common" — should find 25 entries.
			goByToken, err := store.ScanTextIndexByTokenGo(ctx, "common")
			Expect(err).NotTo(HaveOccurred())
			Expect(goByToken).To(HaveLen(25))

			javaByToken, err := store.ScanTextIndexByTokenJava(ctx, "common")
			Expect(err).NotTo(HaveOccurred())
			Expect(javaByToken).To(HaveLen(25))

			err = CompareTextIndexEntries(goByToken, javaByToken)
			Expect(err).NotTo(HaveOccurred())

			// Verify all PKs present (200..224).
			sortTextEntries(goByToken)
			for i, entry := range goByToken {
				Expect(entry.PrimaryKey).To(Equal(int64(200 + i)))
				Expect(entry.Token).To(Equal("common"))
				Expect(entry.Positions).To(Equal([]int64{0}))
			}
		})
	})

	Describe("Position list with many positions", func() {
		It("should record correct positions for repeated tokens", func() {
			// "the the the the the" — token "the" at positions [0, 1, 2, 3, 4].
			err := store.SaveCustomerGo(ctx, &gen.Customer{
				CustomerId: proto.Int64(300),
				Name:       proto.String("the the the the the"),
			})
			Expect(err).NotTo(HaveOccurred())

			goEntries, err := store.ScanTextIndexGo(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(goEntries).To(HaveLen(1))

			javaEntries, err := store.ScanTextIndexJava(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(javaEntries).To(HaveLen(1))

			err = CompareTextIndexEntries(goEntries, javaEntries)
			Expect(err).NotTo(HaveOccurred())

			Expect(goEntries[0].Token).To(Equal("the"))
			Expect(goEntries[0].PrimaryKey).To(Equal(int64(300)))
			Expect(goEntries[0].Positions).To(Equal([]int64{0, 1, 2, 3, 4}))
		})
	})

	Describe("Cross-language update", func() {
		It("should re-index when Java updates a Go-written record", func() {
			// Go saves customer with "alpha beta".
			err := store.SaveCustomerGo(ctx, &gen.Customer{
				CustomerId: proto.Int64(400),
				Name:       proto.String("alpha beta"),
			})
			Expect(err).NotTo(HaveOccurred())

			// Verify initial tokens exist.
			goEntries, err := store.ScanTextIndexGo(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(goEntries).To(HaveLen(2))

			// Java updates same PK with different text.
			err = store.SaveCustomerJava(ctx, &gen.Customer{
				CustomerId: proto.Int64(400),
				Name:       proto.String("gamma delta"),
			})
			Expect(err).NotTo(HaveOccurred())

			// Old tokens must be gone, new tokens present.
			goEntries, err = store.ScanTextIndexGo(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(goEntries).To(HaveLen(2))

			javaEntries, err := store.ScanTextIndexJava(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(javaEntries).To(HaveLen(2))

			err = CompareTextIndexEntries(goEntries, javaEntries)
			Expect(err).NotTo(HaveOccurred())

			sortTextEntries(goEntries)
			Expect(goEntries[0].Token).To(Equal("delta"))
			Expect(goEntries[1].Token).To(Equal("gamma"))

			// Confirm old tokens are truly gone.
			alphaEntries, err := store.ScanTextIndexByTokenGo(ctx, "alpha")
			Expect(err).NotTo(HaveOccurred())
			Expect(alphaEntries).To(BeEmpty())

			betaEntries, err := store.ScanTextIndexByTokenJava(ctx, "beta")
			Expect(err).NotTo(HaveOccurred())
			Expect(betaEntries).To(BeEmpty())
		})
	})

	Describe("Empty text cross-language", func() {
		It("should produce zero index entries for empty name", func() {
			// Save customer with empty name — no tokens to index.
			err := store.SaveCustomerGo(ctx, &gen.Customer{
				CustomerId: proto.Int64(500),
				Name:       proto.String(""),
			})
			Expect(err).NotTo(HaveOccurred())

			goEntries, err := store.ScanTextIndexGo(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(goEntries).To(BeEmpty())

			javaEntries, err := store.ScanTextIndexJava(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(javaEntries).To(BeEmpty())
		})
	})
})
