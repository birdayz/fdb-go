// Package harness exercises persisted Unicode-sensitive keys in real record stores.
package harness

import (
	"bytes"
	"context"
	"encoding/hex"
	"fmt"

	"google.golang.org/protobuf/proto"

	"fdb.dev/gen/demopb"
	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/fdbgo/fdb/subspace"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	"fdb.dev/pkg/recordlayer"
)

type KV struct {
	Key   string `json:"key"`
	Value string `json:"value"`
}

type Posting struct {
	Key       string  `json:"key"`
	Value     string  `json:"value"`
	Token     string  `json:"token"`
	Customer  int64   `json:"customer"`
	Positions []int64 `json:"positions"`
}

type Snapshot struct {
	Name     string    `json:"name"`
	Records  []KV      `json:"records"`
	Index    []KV      `json:"index"`
	Counts   []KV      `json:"counts"`
	Postings []Posting `json:"postings"`
	Order    []string  `json:"order"`
	Lookups  []int     `json:"lookups"`
}

type Report struct {
	Schema int        `json:"schema"`
	Stores []Snapshot `json:"stores"`
}

type spec struct {
	name                    string
	grouped, omit, collated bool
	locale                  string
	strength                int
}

func specs() []spec {
	out := []spec{{name: "text"}, {name: "text_omit", omit: true}, {name: "text_grouped", grouped: true}, {name: "text_grouped_omit", grouped: true, omit: true}}
	for _, locale := range []string{"", "sv", "tr", "zh", "en-u-kn-true"} {
		for strength := 0; strength < 3; strength++ {
			out = append(out, spec{name: fmt.Sprintf("collate_%s_%d", locale, strength), collated: true, locale: locale, strength: strength})
		}
	}
	return out
}

// Texts retains version-delta witnesses, shared Java cases, and the existing
// Go-v0 sigma/segmentation differences. Persisted records require valid UTF-8.
var Texts = []string{
	"Hello café hello WORLD", "İstanbul ıi I", "ﬁnance ﬂ ǅ", "ＡＢＣ１２３ 𝐀𝐁Ｃ 𝟙𝟚",
	"\u1c89 \ua7cb \ua7ce \ua7f1 \U0001ccd6", "a\u1acfb a\u0897b", "\u0264 AΣʕ", "ﬅ ﬆ ΐ ΐ ΰ ΰ",
	"\U00018cff \U000323b0 \U0002ebf0", "file2 file10 file\U00010d50 file\U00011de0", "x:y ĿΣ ΑΣ\u037a \U00010780Σ", "a\ufffdb",
}

var CollationTexts = []string{
	"a", "A", "á", "a\u0301", "ä", "z", "å", "İ", "ı", "Σ", "ς", "\u0264", "\ua7cb", "\U00018cff", "\U000323b0", "\U0002ebf0", "file2", "file10", "file\U00010d50", "file\U00011de0", "\U0001ccd6",
}

func customers(s spec, updated bool) []*demopb.Customer {
	texts := Texts
	if s.collated {
		texts = CollationTexts
	}
	out := make([]*demopb.Customer, 0, len(texts)+25)
	for i, text := range texts {
		if updated {
			text = "replacement " + text + " updated"
		}
		group := int64(i % 2)
		if updated {
			group += 10
		}
		out = append(out, &demopb.Customer{CustomerId: proto.Int64(int64(i + 1)), Name: proto.String(text), Email: proto.String(fmt.Sprint(group))})
	}
	if !s.collated {
		for i := 0; i < 25; i++ {
			text := "common oldterm"
			if updated {
				text = "replacement newterm"
			}
			out = append(out, &demopb.Customer{CustomerId: proto.Int64(int64(100 + i)), Name: proto.String(text), Email: proto.String("bunch")})
		}
	}
	return out
}

func metadata(s spec) (*recordlayer.RecordMetaData, *recordlayer.Index, error) {
	b := recordlayer.NewRecordMetaDataBuilder().SetRecords(demopb.File_record_layer_demo_proto)
	b.GetRecordType("Order").SetPrimaryKey(recordlayer.Field("order_id"))
	b.GetRecordType("Customer").SetPrimaryKey(recordlayer.Field("customer_id"))
	b.GetRecordType("TypedRecord").SetPrimaryKey(recordlayer.Field("id"))
	var index *recordlayer.Index
	if s.collated {
		key := recordlayer.FunctionExpr(recordlayer.CollateFuncJRE, recordlayer.Concat(recordlayer.Field("name"), recordlayer.Literal(s.locale), recordlayer.Literal(int64(s.strength))))
		b.GetRecordType("Customer").SetPrimaryKey(recordlayer.Concat(key, recordlayer.Field("customer_id")))
		b.SetRecordCountKey(key)
		index = recordlayer.NewIndex("collated", key)
	} else {
		var key recordlayer.KeyExpression = recordlayer.Field("name")
		if s.grouped {
			key = recordlayer.GroupBy(recordlayer.Field("name"), recordlayer.Field("email"))
		}
		index = recordlayer.NewTextIndex("text", key)
		if s.omit {
			index.Options[recordlayer.IndexOptionTextOmitPositions] = "true"
		}
	}
	b.AddIndex("Customer", index)
	md, err := b.Build()
	return md, index, err
}

// Run performs one committed operation over the same 19 isolated stores, then
// reads committed bytes in separate transactions. Prefix is a tuple string, not
// a directory-layer allocation, so another compiler-built process can reopen it.
func Run(ctx context.Context, raw fdb.Database, prefix, operation string) (Report, error) {
	if prefix == "" {
		return Report{}, fmt.Errorf("prefix must not be empty")
	}
	if operation != "write" && operation != "update" && operation != "delete" && operation != "rebuild" && operation != "inspect" {
		return Report{}, fmt.Errorf("unknown operation %q", operation)
	}
	db := recordlayer.NewFDBDatabase(raw)
	root := subspace.Sub(tuple.Tuple{"unicode15store", prefix})
	report := Report{Schema: 1, Stores: []Snapshot{}}
	for _, s := range specs() {
		md, index, err := metadata(s)
		if err != nil {
			return Report{}, err
		}
		ss := root.Sub(s.name)
		_, err = db.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
			store, err := recordlayer.NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ss).SetGoOnlyCollation(s.collated).CreateOrOpen()
			if err != nil {
				return nil, err
			}
			if operation == "delete" || (operation == "update" && s.collated) {
				records, err := recordlayer.AsList(ctx, store.ScanRecords(nil, recordlayer.ForwardScan()))
				if err != nil {
					return nil, err
				}
				for _, rec := range records {
					deleted, err := store.DeleteRecord(rec.PrimaryKey)
					if err != nil {
						return nil, err
					}
					if !deleted {
						return nil, fmt.Errorf("record %v not deleted", rec.PrimaryKey)
					}
				}
			}
			if operation == "write" || operation == "update" {
				for _, customer := range customers(s, operation == "update") {
					if _, err := store.SaveRecord(customer); err != nil {
						return nil, err
					}
				}
			}
			if operation == "rebuild" {
				return nil, store.RebuildIndex(index)
			}
			return nil, nil
		})
		if err != nil {
			return Report{}, fmt.Errorf("%s/%s: %w", operation, s.name, err)
		}
		var snapshot Snapshot
		_, err = db.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
			store, err := recordlayer.NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ss).SetGoOnlyCollation(s.collated).Open()
			if err != nil {
				return nil, err
			}
			snapshot = Snapshot{Name: s.name, Records: []KV{}, Index: []KV{}, Counts: []KV{}, Postings: []Posting{}, Order: []string{}, Lookups: []int{}}
			read := func(space subspace.Subspace) ([]KV, error) {
				kvs, err := rtx.Transaction().GetRange(space, fdb.RangeOptions{}).GetSliceWithError()
				if err != nil {
					return nil, err
				}
				out := make([]KV, 0, len(kvs))
				for _, kv := range kvs {
					out = append(out, KV{Key: hex.EncodeToString(kv.Key[len(ss.Bytes()):]), Value: hex.EncodeToString(kv.Value)})
				}
				return out, nil
			}
			if snapshot.Index, err = read(store.IndexSubspace(index)); err != nil {
				return nil, err
			}
			if s.collated {
				if snapshot.Records, err = read(store.RecordsSubspace()); err != nil {
					return nil, err
				}
				if snapshot.Counts, err = read(ss.Sub(recordlayer.RecordCountKey)); err != nil {
					return nil, err
				}
				records, err := recordlayer.AsList(ctx, store.ScanRecords(nil, recordlayer.ForwardScan()))
				if err != nil {
					return nil, err
				}
				for _, rec := range records {
					snapshot.Order = append(snapshot.Order, rec.Record.(*demopb.Customer).GetName())
					rows, err := md.GetRecordType("Customer").PrimaryKey.Evaluate(rec, rec.Record)
					if err != nil {
						return nil, err
					}
					if len(rows) != 1 || len(rows[0]) != 2 {
						return nil, fmt.Errorf("unexpected collated primary-key shape: %v", rows)
					}
					primaryKey := tuple.Tuple{rows[0][0], rows[0][1]}
					loaded, err := store.LoadRecord(primaryKey)
					if err != nil {
						return nil, err
					}
					if loaded == nil || !proto.Equal(loaded.Record, rec.Record) {
						return nil, fmt.Errorf("re-evaluated collated primary-key lookup failed")
					}
					entries, err := recordlayer.AsList(ctx, store.ScanIndex(index, recordlayer.TupleRangeAllOf(tuple.Tuple{primaryKey[0]}), nil, recordlayer.ForwardScan()))
					if err != nil {
						return nil, err
					}
					found := false
					for _, entry := range entries {
						if bytes.Equal(entry.PrimaryKey().Pack(), primaryKey.Pack()) {
							found = true
						}
					}
					if !found {
						return nil, fmt.Errorf("re-evaluated collated index range omitted primary key %x", primaryKey.Pack())
					}
					snapshot.Lookups = append(snapshot.Lookups, len(entries))
				}
			} else {
				entries, err := recordlayer.AsList(ctx, store.ScanIndexByType(index, recordlayer.IndexScanByTextToken, recordlayer.TupleRangeAll, nil, recordlayer.ForwardScan()))
				if err != nil {
					return nil, err
				}
				for _, entry := range entries {
					tokenOffset := 0
					if s.grouped {
						tokenOffset = 1
					}
					p := Posting{Key: hex.EncodeToString(entry.Key.Pack()), Value: hex.EncodeToString(entry.Value.Pack()), Token: entry.Key[tokenOffset].(string), Customer: entry.Key[tokenOffset+1].(int64), Positions: []int64{}}
					if len(entry.Value) > 0 {
						for _, pos := range entry.Value[0].(tuple.Tuple) {
							p.Positions = append(p.Positions, pos.(int64))
						}
					}
					snapshot.Postings = append(snapshot.Postings, p)
				}
			}
			return nil, nil
		})
		if err != nil {
			return Report{}, fmt.Errorf("inspect %s: %w", s.name, err)
		}
		report.Stores = append(report.Stores, snapshot)
	}
	return report, nil
}
