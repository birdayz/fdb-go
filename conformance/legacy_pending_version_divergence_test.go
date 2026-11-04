//go:build bazelrunfiles

package conformance_test

import (
	"context"
	"encoding/hex"
	"fmt"
	"time"

	"fdb.dev/gen/demopb"
	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/fdbgo/fdb/subspace"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	"fdb.dev/pkg/recordlayer"
	"github.com/google/uuid"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"google.golang.org/protobuf/proto"
)

var _ = Describe("Declared legacy pending-version deletion boundary", func() {
	for _, kind := range []string{"point", "where"} {
		for _, seed := range []bool{false, true} {
			It(fmt.Sprintf("contrasts actual Java pending orphan with Go cleanup delete=%s seed=%t", kind, seed), func() {
				ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
				defer cancel()
				builder := recordlayer.NewRecordMetaDataBuilder().SetRecords(demopb.File_record_layer_demo_proto)
				builder.GetRecordType("Order").SetRecordTypeKey(int64(42)).SetPrimaryKey(recordlayer.Concat(recordlayer.RecordTypeKey(), recordlayer.Field("order_id"), recordlayer.Field("price")))
				builder.GetRecordType("Customer").SetRecordTypeKey(int64(43)).SetPrimaryKey(recordlayer.Concat(recordlayer.RecordTypeKey(), recordlayer.Field("customer_id")))
				builder.GetRecordType("TypedRecord").SetRecordTypeKey(int64(44)).SetPrimaryKey(recordlayer.Concat(recordlayer.RecordTypeKey(), recordlayer.Field("id")))
				builder.SetStoreRecordVersions(false).SetSplitLongRecords(true)
				md, err := builder.Build()
				Expect(err).NotTo(HaveOccurred())
				metadata, err := md.ToProto()
				Expect(err).NotTo(HaveOccurred())
				metadataBytes, err := proto.Marshal(metadata)
				Expect(err).NotTo(HaveOccurred())
				record := &demopb.Order{OrderId: proto.Int64(1), Price: proto.Int32(7), Quantity: proto.Int32(2)}
				recordBytes, err := proto.Marshal(record)
				Expect(err).NotTo(HaveOccurred())
				cluster, err := sharedContainer.ClusterFile(ctx)
				Expect(err).NotTo(HaveOccurred())
				root := subspace.Sub("legacy_pending_boundary", uuid.NewString())
				var java struct {
					RecordRows          int      `json:"recordRows"`
					Versions            []string `json:"versions"`
					VersionKeys         []string `json:"versionKeys"`
					FormatVersion       int      `json:"formatVersion"`
					StoreRecordVersions bool     `json:"storeRecordVersions"`
				}
				Expect(NewJavaInvoker().InvokeAs(ctx, "legacyExplicitVersionDelete", map[string]any{
					"clusterFile": cluster, "subspace": BytesToIntArray(root.Sub("java").Bytes()), "metaData": BytesToIntArray(metadataBytes),
					"record": BytesToIntArray(recordBytes), "deleteKind": kind, "seed": seed,
				}, &java)).To(Succeed())
				Expect(java.RecordRows).To(BeZero())
				Expect(java.FormatVersion).To(Equal(5))
				Expect(java.StoreRecordVersions).To(BeFalse())
				Expect(java.VersionKeys).To(Equal([]string{hex.EncodeToString(root.Sub("java").Sub(recordlayer.RecordVersionKey).Pack(tuple.Tuple{int64(42), int64(1), int64(7)}))}))
				Expect(java.Versions).To(HaveLen(1), "Java retains the pending explicit legacy version despite deleting the record")
				javaVersion, err := hex.DecodeString(java.Versions[0])
				Expect(err).NotTo(HaveOccurred())
				Expect(javaVersion).To(HaveLen(12))
				Expect(javaVersion[10:]).To(Equal([]byte{0, 1}))
				old := make([]byte, 12)
				old[0], old[11] = 1, 9
				Expect(javaVersion).NotTo(Equal(old))
				goRoot := root.Sub("go")
				db := recordlayer.NewFDBDatabase(sharedDB)
				open := func(rc *recordlayer.FDBRecordContext) (*recordlayer.FDBRecordStore, error) {
					return recordlayer.NewStoreBuilder().SetContext(rc).SetMetaDataProvider(md).SetSubspace(goRoot).SetFormatVersion(5).CreateOrOpen()
				}
				if seed {
					_, err = db.Run(ctx, func(rc *recordlayer.FDBRecordContext) (any, error) {
						store, err := open(rc)
						if err != nil {
							return nil, err
						}
						version, err := recordlayer.CompleteVersionFromBytes(old)
						if err != nil {
							return nil, err
						}
						return store.SaveRecordWithVersion(record, recordlayer.RecordExistenceCheckNone, version, recordlayer.VersionstampSaveBehaviorIfPresent)
					})
					Expect(err).NotTo(HaveOccurred())
				}
				_, err = db.Run(ctx, func(rc *recordlayer.FDBRecordContext) (any, error) {
					store, err := open(rc)
					if err != nil {
						return nil, err
					}
					version, err := recordlayer.IncompleteVersion(1)
					if err != nil {
						return nil, err
					}
					if _, err := store.SaveRecordWithVersion(record, recordlayer.RecordExistenceCheckNone, version, recordlayer.VersionstampSaveBehaviorIfPresent); err != nil {
						return nil, err
					}
					if kind == "where" {
						return nil, store.DeleteRecordsWhere(tuple.Tuple{int64(42), int64(1)})
					}
					found, err := store.DeleteRecord(tuple.Tuple{int64(42), int64(1), int64(7)})
					Expect(found).To(BeTrue())
					return nil, err
				})
				Expect(err).NotTo(HaveOccurred())
				_, err = db.Run(ctx, func(rc *recordlayer.FDBRecordContext) (any, error) {
					rows, err := rc.Transaction().GetRange(goRoot.Sub(recordlayer.RecordKey), fdb.RangeOptions{}).GetSliceWithError()
					if err != nil {
						return nil, err
					}
					Expect(rows).To(BeEmpty())
					versions, err := rc.Transaction().GetRange(goRoot.Sub(recordlayer.RecordVersionKey), fdb.RangeOptions{}).GetSliceWithError()
					if err != nil {
						return nil, err
					}
					if seed {
						Expect(versions).To(HaveLen(1))
						Expect(versions[0].Key).To(Equal(goRoot.Sub(recordlayer.RecordVersionKey).Pack(tuple.Tuple{int64(42), int64(1), int64(7)})))
						Expect(versions[0].Value).To(Equal(old))
					} else {
						Expect(versions).To(BeEmpty())
					}
					return nil, nil
				})
				Expect(err).NotTo(HaveOccurred())
			})
		}
	}
})
