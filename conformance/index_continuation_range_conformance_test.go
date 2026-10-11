//go:build bazelrunfiles

package conformance_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"

	"fdb.dev/gen/demopb"
	"fdb.dev/pkg/fdbgo/fdb/subspace"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	"fdb.dev/pkg/recordlayer"
	"github.com/google/uuid"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"google.golang.org/protobuf/proto"
)

// Narrowed-range continuations: Java strips the common prefix of the packed bounds
// (KeyValueCursorBase.calculatePrefixLength). At every page both engines resume the
// same token; their rows and next tokens must be identical, and the token carried
// forward alternates between Java's and Go's.
var _ = Describe("Narrowed-range continuation conformance", func() {
	var (
		ctx      context.Context
		env      *TenantEnvironment
		java     *JavaInvoker
		metaData *recordlayer.RecordMetaData
		indexes  map[string]*recordlayer.Index
		ks       subspace.Subspace
	)

	BeforeEach(func() {
		ctx = context.Background()
		var err error
		env, err = SetupTenantEnvironment(ctx, sharedContainer, fmt.Sprintf("rangecont_%s", uuid.New().String()))
		Expect(err).NotTo(HaveOccurred())
		java = NewJavaInvoker()

		// Same indexes, in the same order, as createRangeContinuationMetaData.
		bitmap := recordlayer.NewBitmapValueIndex("Order$bm_qty",
			recordlayer.GroupBy(recordlayer.Field("order_id"), recordlayer.Field("quantity")))
		bitmap.Options[recordlayer.IndexOptionBitmapValueEntrySize] = "4"
		indexes = map[string]*recordlayer.Index{
			"Order$qty_price":     recordlayer.NewIndex("Order$qty_price", recordlayer.Concat(recordlayer.Field("quantity"), recordlayer.Field("price"))),
			"Order$cnt_qty_price": recordlayer.NewCountIndex("Order$cnt_qty_price", recordlayer.GroupAll(recordlayer.Concat(recordlayer.Field("quantity"), recordlayer.Field("price")))),
			"Order$bm_qty":        bitmap,
			"Customer$name":       recordlayer.NewIndex("Customer$name", recordlayer.Field("name")),
		}
		builder := recordlayer.NewRecordMetaDataBuilder().SetRecords(demopb.File_record_layer_demo_proto)
		builder.GetRecordType("Order").SetPrimaryKey(recordlayer.Field("order_id"))
		builder.GetRecordType("Customer").SetPrimaryKey(recordlayer.Field("name"))
		builder.GetRecordType("TypedRecord").SetPrimaryKey(recordlayer.Field("id"))
		builder.AddIndex("Order", indexes["Order$qty_price"])
		builder.AddIndex("Order", indexes["Order$cnt_qty_price"])
		builder.AddIndex("Order", indexes["Order$bm_qty"])
		builder.AddIndex("Customer", indexes["Customer$name"])
		metaData, err = builder.Build()
		Expect(err).NotTo(HaveOccurred())

		ks = env.Keyspace
		if env.TenantName != "" {
			ks = subspace.Sub(tuple.Tuple{})
		}
		// Quantity 11 holds prices 2,5,8,11 and 200,201, so remaining columns sort
		// below, inside and above the narrowed prefix.
		_, err = env.RecordDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
			store, err := recordlayer.NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(metaData).SetSubspace(ks).CreateOrOpen()
			if err != nil {
				return nil, err
			}
			save := func(id int64, qty, price int32) error {
				_, err := store.SaveRecord(&demopb.Order{OrderId: proto.Int64(id), Quantity: proto.Int32(qty), Price: proto.Int32(price)})
				return err
			}
			for i := int64(1); i <= 12; i++ {
				if err := save(i, int32(10+(i-1)%3), int32(i)); err != nil {
					return nil, err
				}
			}
			for i := int64(14); i <= 15; i++ {
				if err := save(i, 11, int32(186+i)); err != nil {
					return nil, err
				}
			}
			for i, name := range []string{"aa", "ab", "aba", "abc", "abd", "abz", "ac", "b"} {
				if _, err := store.SaveRecord(&demopb.Customer{CustomerId: proto.Int64(int64(101 + i)), Name: proto.String(name)}); err != nil {
					return nil, err
				}
			}
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	AfterEach(func() {
		if env != nil {
			_ = env.Cleanup(ctx)
		}
	})

	ranges := map[string]recordlayer.TupleRange{
		"allOf11":    recordlayer.TupleRangeAllOf(tuple.Tuple{int64(11)}),
		"between11":  recordlayer.TupleRangeBetweenInclusive(tuple.Tuple{int64(11), int64(2)}, tuple.Tuple{int64(11), int64(8)}),
		"prefixAb":   recordlayer.TupleRangePrefixString("ab"),
		"pkPrefixAb": recordlayer.TupleRangePrefixString("ab"),
		"pk5to9":     recordlayer.TupleRangeBetweenInclusive(tuple.Tuple{int64(5)}, tuple.Tuple{int64(9)}),
	}

	type page struct {
		keys      []any
		cont      []byte
		exhausted bool
	}

	// normalize renders a key the way Java's JSON reply does (numbers as float64).
	normalize := func(t tuple.Tuple) []any {
		out := make([]any, len(t))
		for i, e := range t {
			if n, ok := e.(int64); ok {
				out[i] = float64(n)
			} else {
				out[i] = e
			}
		}
		return out
	}

	scanJava := func(indexName, scanType, rng string, reverse bool, limit int, cont []byte) page {
		params := map[string]any{
			"clusterFile": env.ClusterFile,
			"subspace":    BytesToIntArray(ks.Bytes()),
			"indexName":   indexName,
			"scanType":    scanType,
			"range":       rng,
			"reverse":     reverse,
			"limit":       limit,
		}
		if env.TenantName != "" {
			params["tenantName"] = env.TenantName
		}
		if cont != nil {
			params["continuation"] = base64.StdEncoding.EncodeToString(cont)
		}
		raw, err := java.Invoke(ctx, "scanRangeWithContinuation", params)
		Expect(err).NotTo(HaveOccurred())
		var res struct {
			Keys            [][]any `json:"keys"`
			Continuation    string  `json:"continuation"`
			SourceExhausted bool    `json:"sourceExhausted"`
		}
		Expect(json.Unmarshal(raw, &res)).To(Succeed())
		p := page{exhausted: res.SourceExhausted}
		for _, k := range res.Keys {
			p.keys = append(p.keys, k)
		}
		if res.Continuation != "" {
			p.cont, err = base64.StdEncoding.DecodeString(res.Continuation)
			Expect(err).NotTo(HaveOccurred())
		}
		return p
	}

	scanGo := func(indexName, scanType, rng string, reverse bool, limit int, cont []byte) page {
		var p page
		_, err := env.RecordDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
			p = page{}
			store, err := recordlayer.NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(metaData).SetSubspace(ks).Open()
			if err != nil {
				return nil, err
			}
			props := recordlayer.NewScanProperties(recordlayer.DefaultExecuteProperties().WithReturnedRowLimit(limit))
			props.Reverse = reverse
			if indexName == "" {
				r := ranges[rng]
				cur := store.ScanRecordsInRange(r.Low, r.High, r.LowEndpoint, r.HighEndpoint, cont, props)
				defer cur.Close()
				for {
					res, err := cur.OnNext(ctx)
					if err != nil {
						return nil, err
					}
					if !res.HasNext() {
						p.exhausted = res.GetNoNextReason().IsSourceExhausted()
						p.cont, err = res.GetContinuation().ToBytes()
						return nil, err
					}
					p.keys = append(p.keys, normalize(res.GetValue().PrimaryKey))
				}
			}
			cur := store.ScanIndexByType(indexes[indexName], recordlayer.IndexScanType(scanType), ranges[rng], cont, props)
			defer cur.Close()
			for {
				res, err := cur.OnNext(ctx)
				if err != nil {
					return nil, err
				}
				if !res.HasNext() {
					p.exhausted = res.GetNoNextReason().IsSourceExhausted()
					p.cont, err = res.GetContinuation().ToBytes()
					return nil, err
				}
				p.keys = append(p.keys, normalize(res.GetValue().Key))
			}
		})
		Expect(err).NotTo(HaveOccurred())
		return p
	}

	It("preserves an empty non-nil continuation in a reverse record scan", func() {
		for _, scan := range []func(string, string, string, bool, int, []byte) page{scanJava, scanGo} {
			Expect(scan("", "", "pkPrefixAb", true, 0, nil).keys).To(HaveLen(5))
			p := scan("", "", "pkPrefixAb", true, 0, []byte{})
			Expect(p.keys).To(BeEmpty())
			Expect(p.cont).To(BeNil())
			Expect(p.exhausted).To(BeTrue())
		}
	})

	DescribeTable("both engines agree page by page, each resuming the other's token",
		func(indexName, scanType, rng string, wantRows, pageSize int) {
			for _, reverse := range []bool{false, true} {
				full := scanJava(indexName, scanType, rng, reverse, 0, nil)
				Expect(full.keys).To(HaveLen(wantRows))
				Expect(scanGo(indexName, scanType, rng, reverse, 0, nil).keys).To(Equal(full.keys))

				var cont []byte
				var seen []any
				for step := 0; ; step++ {
					j := scanJava(indexName, scanType, rng, reverse, pageSize, cont)
					g := scanGo(indexName, scanType, rng, reverse, pageSize, cont)
					Expect(g.keys).To(Equal(j.keys), "step %d reverse=%v", step, reverse)
					Expect(g.cont).To(Equal(j.cont), "step %d reverse=%v: token bytes", step, reverse)
					Expect(g.exhausted).To(Equal(j.exhausted))
					seen = append(seen, j.keys...)
					if j.exhausted {
						break
					}
					Expect(step).To(BeNumerically("<", wantRows))
					// Carry Java's token on even steps and Go's on odd ones.
					cont = j.cont
					if step%2 == 1 {
						cont = g.cont
					}
				}
				Expect(seen).To(Equal(full.keys), "reverse=%v", reverse)
			}
		},
		// Page sizes never divide the row count: at an exact boundary Java reports
		// RETURN_LIMIT_REACHED with a token where Go's lookahead reports exhaustion
		// (TODO.md, "row-limit lookahead").
		Entry("VALUE index, equality on the leading column", "Order$qty_price", "BY_VALUE", "allOf11", 6, 4),
		Entry("VALUE index, betweenInclusive under one leading value", "Order$qty_price", "BY_VALUE", "between11", 3, 2),
		Entry("VALUE index, PREFIX_STRING", "Customer$name", "BY_VALUE", "prefixAb", 5, 3),
		Entry("COUNT index, grouped lookup", "Order$cnt_qty_price", "BY_GROUP", "allOf11", 6, 4),
		Entry("BITMAP_VALUE index, grouped lookup", "Order$bm_qty", "BY_GROUP", "allOf11", 4, 3),
		Entry("record scan over a primary-key range", "", "", "pk5to9", 5, 2),
		Entry("record scan, PREFIX_STRING over string primary keys", "", "", "pkPrefixAb", 5, 2),
	)
})
