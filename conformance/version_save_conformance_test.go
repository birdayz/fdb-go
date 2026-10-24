//go:build bazelrunfiles

package conformance_test

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"google.golang.org/protobuf/proto"

	"fdb.dev/gen/demopb"
	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/fdbgo/fdb/subspace"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	"fdb.dev/pkg/recordlayer"
)

// Both engines save the same records with the same versions and behaviors and must agree on
// every verdict and on the record (1), index (2) and legacy version (8) key-value pairs.
var _ = Describe("Explicit-version saves match Java", func() {
	type versionedSave struct {
		id       int64
		version  []byte // 12 complete bytes, or nil
		behavior recordlayer.VersionstampSaveBehavior
		want     string
	}
	complete := func(b byte) []byte { return []byte{0, 0, 0, 0, 0, 0, 0, 2, 0, b, 0, 1} }
	saves := []versionedSave{
		{1, complete(1), recordlayer.VersionstampSaveBehaviorDefault, "ok"},
		{2, complete(2), recordlayer.VersionstampSaveBehaviorIfPresent, "ok"},
		{3, nil, recordlayer.VersionstampSaveBehaviorIfPresent, "ok"},
		{4, nil, recordlayer.VersionstampSaveBehaviorNoVersion, "ok"},
		{5, complete(5), recordlayer.VersionstampSaveBehaviorWithVersion, "ok"},
		{6, complete(6), recordlayer.VersionstampSaveBehaviorNoVersion, "com.apple.foundationdb.record.RecordCoreException"},
		{1, nil, recordlayer.VersionstampSaveBehaviorNoVersion, "ok"}, // clears record 1's version
	}

	for _, storeVersions := range []bool{false, true} {
		for _, format := range []int32{0, 5} {
			It(fmt.Sprintf("storeRecordVersions=%v, format %d", storeVersions, format), func() {
				ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
				defer cancel()
				clusterFile, err := sharedContainer.ClusterFile(ctx)
				Expect(err).NotTo(HaveOccurred())

				builder := recordlayer.NewRecordMetaDataBuilder().SetRecords(demopb.File_record_layer_demo_proto)
				builder.GetRecordType("Order").SetPrimaryKey(recordlayer.Field("order_id"))
				builder.GetRecordType("Customer").SetPrimaryKey(recordlayer.Field("customer_id"))
				builder.GetRecordType("TypedRecord").SetPrimaryKey(recordlayer.Field("id"))
				builder.SetStoreRecordVersions(storeVersions)
				if storeVersions {
					builder.AddIndex("Order", recordlayer.NewVersionIndex("order$version", recordlayer.VersionKey()))
				}
				md, err := builder.Build()
				Expect(err).NotTo(HaveOccurred())
				mdProto, err := md.ToProto()
				Expect(err).NotTo(HaveOccurred())
				mdBytes, err := proto.Marshal(mdProto)
				Expect(err).NotTo(HaveOccurred())

				records := make([][]int, len(saves))
				versions := make([][]int, len(saves))
				behaviors := make([]string, len(saves))
				var want []string
				for i, s := range saves {
					b, err := proto.MarshalOptions{Deterministic: true}.Marshal(&demopb.Order{OrderId: proto.Int64(s.id), Price: proto.Int32(int32(s.id))})
					Expect(err).NotTo(HaveOccurred())
					records[i] = BytesToIntArray(b)
					versions[i] = BytesToIntArray(s.version)
					behaviors[i] = s.behavior.String()
					want = append(want, s.want)
				}

				javaSS := subspace.Sub(tuple.Tuple{"version_save_java", uuid.NewString()}...)
				var java struct {
					Verdicts []string   `json:"verdicts"`
					KVs      [][]string `json:"kvs"`
				}
				Expect(NewJavaInvoker().InvokeAs(ctx, "saveWithVersionsJava", map[string]any{
					"clusterFile": clusterFile, "subspace": BytesToIntArray(javaSS.Bytes()), "metaData": BytesToIntArray(mdBytes),
					"recordTypeName": "Order", "records": records, "versions": versions, "behaviors": behaviors, "formatVersion": format,
				}, &java)).To(Succeed())
				Expect(java.Verdicts).To(Equal(want), "Java")

				goSS := subspace.Sub(tuple.Tuple{"version_save_go", uuid.NewString()}...)
				db := recordlayer.NewFDBDatabase(sharedDB)
				var verdicts []string
				for _, s := range saves {
					var version *recordlayer.FDBRecordVersion
					if s.version != nil {
						version, err = recordlayer.CompleteVersionFromBytes(s.version)
						Expect(err).NotTo(HaveOccurred())
					}
					_, err := db.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
						b := recordlayer.NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(md).SetSubspace(goSS)
						if format != 0 {
							b.SetFormatVersion(format)
						}
						store, err := b.CreateOrOpen()
						if err != nil {
							return nil, err
						}
						return store.SaveRecordWithVersion(&demopb.Order{OrderId: proto.Int64(s.id), Price: proto.Int32(int32(s.id))},
							recordlayer.RecordExistenceCheckNone, version, s.behavior)
					})
					var coreErr *recordlayer.RecordCoreError
					switch {
					case err == nil:
						verdicts = append(verdicts, "ok")
					case errors.As(err, &coreErr):
						verdicts = append(verdicts, "com.apple.foundationdb.record.RecordCoreException")
					default:
						verdicts = append(verdicts, err.Error())
					}
				}
				Expect(verdicts).To(Equal(want), "Go")

				var goKVs [][]string
				_, err = db.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
					goKVs = [][]string{}
					for _, space := range []int64{1, 2, 8} {
						kvs, err := rtx.Transaction().GetRange(goSS.Sub(space), fdb.RangeOptions{}).GetSliceWithError()
						if err != nil {
							return nil, err
						}
						for _, kv := range kvs {
							goKVs = append(goKVs, []string{hex.EncodeToString(kv.Key[len(goSS.Bytes()):]), hex.EncodeToString(kv.Value)})
						}
					}
					return nil, nil
				})
				Expect(err).NotTo(HaveOccurred())
				Expect(java.KVs).NotTo(BeEmpty())
				Expect(goKVs).To(Equal(java.KVs))
			})
		}
	}
})
