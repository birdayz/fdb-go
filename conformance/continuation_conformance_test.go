//go:build bazelrunfiles

package conformance_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"fdb.dev/gen"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	"fdb.dev/pkg/recordlayer"
	"fdb.dev/pkg/recordlayer/query/executor"
	"fdb.dev/pkg/recordlayer/query/plan/cascades"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
	"github.com/google/uuid"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/anypb"
)

var _ = Describe("Continuation Token Conformance", func() {
	var (
		ctx  context.Context
		env  *TenantEnvironment
		java *JavaInvoker
	)

	BeforeEach(func() {
		ctx = context.Background()
		var err error

		tenantName := fmt.Sprintf("cont_%s", uuid.New().String())
		env, err = SetupTenantEnvironment(ctx, sharedContainer, tenantName)
		Expect(err).NotTo(HaveOccurred())

		java = NewJavaInvoker()

		// Seed 10 orders with Go
		_, err = env.RecordDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
			store, err := recordlayer.NewStoreBuilder().
				SetContext(rtx).
				SetMetaDataProvider(env.MetaData).
				SetSubspace(env.Keyspace).
				CreateOrOpen()
			if err != nil {
				return nil, err
			}
			for i := int64(1); i <= 10; i++ {
				_, err = store.SaveRecord(StandardOrder(i))
				if err != nil {
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

	buildJavaParams := func() map[string]any {
		params := map[string]any{
			"clusterFile": env.ClusterFile,
			"subspace":    BytesToIntArray(env.Keyspace.Bytes()),
		}
		if env.TenantName != "" {
			params["tenantName"] = env.TenantName
		}
		return params
	}

	It("pins Java pending queue split resource limits", func() {
		for _, tc := range []struct {
			name      string
			scanLimit int
			byteLimit int
			rowLimit  int
			reverse   bool
			fail      bool
			ids       []int64
			resumed   []int64
			reason    string
			scans     int
			byteParts []int
		}{
			{name: "forward unlimited", ids: []int64{1, 2, 3}, reason: "SOURCE_EXHAUSTED", scans: 5, byteParts: []int{0, 1, 2, 2, 3, 4, 4}},
			{name: "reverse unlimited", reverse: true, ids: []int64{3, 2, 1}, reason: "SOURCE_EXHAUSTED", scans: 5, byteParts: []int{4, 3, 3, 2, 1, 1, 0}},
			{name: "forward scan limit", scanLimit: 1, ids: []int64{1}, resumed: []int64{2, 3}, reason: "SCAN_LIMIT_REACHED", scans: 3, byteParts: []int{0, 1, 2}},
			{name: "reverse scan limit", scanLimit: 1, reverse: true, ids: []int64{3}, resumed: []int64{2, 1}, reason: "SCAN_LIMIT_REACHED", scans: 2, byteParts: []int{4, 3}},
			{name: "forward byte limit", byteLimit: 1, ids: []int64{1}, resumed: []int64{2, 3}, reason: "BYTE_LIMIT_REACHED", scans: 3, byteParts: []int{0, 1, 2}},
			{name: "scan precedes bytes", scanLimit: 1, byteLimit: 1, ids: []int64{1}, resumed: []int64{2, 3}, reason: "SCAN_LIMIT_REACHED", scans: 3, byteParts: []int{0, 1, 2}},
			{name: "fail mid split", scanLimit: 1, fail: true, scans: 2, byteParts: []int{0}},
			{name: "exact row boundary", rowLimit: 3, ids: []int64{1, 2, 3}, reason: "RETURN_LIMIT_REACHED", scans: 5, byteParts: []int{0, 1, 2, 2, 3, 4, 4}},
		} {
			params := buildJavaParams()
			params["scanLimit"], params["byteLimit"], params["rowLimit"] = tc.scanLimit, tc.byteLimit, tc.rowLimit
			params["reverse"], params["fail"] = tc.reverse, tc.fail
			raw, err := java.Invoke(ctx, "pendingQueueLimits", params)
			Expect(err).NotTo(HaveOccurred(), tc.name)
			var result struct {
				Continuation   string  `json:"continuation"`
				PhysicalBytes  []int64 `json:"physicalBytes"`
				IDs            []int64 `json:"ids"`
				Resumed        []int64 `json:"resumed"`
				Reason         string  `json:"reason"`
				End            bool    `json:"end"`
				TerminalCached bool    `json:"terminalCached"`
				ErrorClass     string  `json:"errorClass"`
				Scans          int     `json:"scans"`
				Bytes          int64   `json:"bytes"`
			}
			Expect(json.Unmarshal(raw, &result)).To(Succeed())
			Expect(result.PhysicalBytes).To(HaveLen(5), tc.name)
			Expect(result.IDs).To(Equal(append([]int64{}, tc.ids...)), tc.name)
			if tc.fail {
				Expect(result.ErrorClass).To(Equal("ScanLimitReachedException"), tc.name)
			} else {
				Expect(result.ErrorClass).To(BeEmpty(), tc.name)
				Expect(result.Reason).To(Equal(tc.reason), tc.name)
				Expect(result.Resumed).To(Equal(append([]int64{}, tc.resumed...)), tc.name)
				Expect(result.End).To(Equal(tc.reason == "SOURCE_EXHAUSTED"), tc.name)
				Expect(result.TerminalCached).To(BeTrue(), tc.name)
			}
			Expect(result.Scans).To(Equal(tc.scans), tc.name)
			var wantBytes int64
			for _, part := range tc.byteParts {
				wantBytes += result.PhysicalBytes[part]
			}
			Expect(result.Bytes).To(Equal(wantBytes), tc.name)
			_, err = env.RecordDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
				root := env.Keyspace.Sub("pendingQueueLimits")
				queue := recordlayer.NewPendingWritesQueue(root.Sub("entries"), root.Sub("size"), 0, &gen.Order{})
				props := recordlayer.DefaultExecuteProperties()
				props.ScannedRecordsLimit, props.ScannedBytesLimit, props.ReturnedRowLimit = tc.scanLimit, int64(tc.byteLimit), tc.rowLimit
				props.FailOnScanLimitReached = tc.fail
				cursor := queue.GetQueueCursor(rtx, recordlayer.NewScanProperties(props).WithReverse(tc.reverse), nil)
				defer cursor.Close()
				ids := []int64{}
				for {
					item, scanErr := cursor.OnNext(ctx)
					if scanErr != nil {
						var limitErr *recordlayer.ScanLimitReachedError
						Expect(tc.fail && errors.As(scanErr, &limitErr)).To(BeTrue(), "%s: %v", tc.name, scanErr)
						break
					}
					if item.HasNext() {
						ids = append(ids, item.GetValue().Payload.GetOrderId())
						continue
					}
					Expect(tc.fail).To(BeFalse(), tc.name)
					reasons := map[string]recordlayer.NoNextReason{"SOURCE_EXHAUSTED": recordlayer.SourceExhausted, "RETURN_LIMIT_REACHED": recordlayer.ReturnLimitReached, "SCAN_LIMIT_REACHED": recordlayer.ScanLimitReached, "BYTE_LIMIT_REACHED": recordlayer.ByteLimitReached}
					Expect(item.GetNoNextReason()).To(Equal(reasons[tc.reason]), tc.name)
					continuation, err := item.GetContinuation().ToBytes()
					Expect(err).NotTo(HaveOccurred())
					Expect(base64.StdEncoding.EncodeToString(continuation)).To(Equal(result.Continuation), tc.name)
					again, err := cursor.OnNext(ctx)
					Expect(err).NotTo(HaveOccurred())
					Expect(again).To(Equal(item))
					if !item.GetContinuation().IsEnd() {
						next := queue.GetQueueCursor(rtx, recordlayer.ForwardScan().WithReverse(tc.reverse), continuation)
						defer next.Close()
						resumed := []int64{}
						for {
							row, err := next.OnNext(ctx)
							Expect(err).NotTo(HaveOccurred())
							if !row.HasNext() {
								break
							}
							resumed = append(resumed, row.GetValue().Payload.GetOrderId())
						}
						Expect(resumed).To(Equal(result.Resumed), tc.name)
					}
					break
				}
				Expect(ids).To(Equal(result.IDs), tc.name)
				Expect(props.ScanState.RecordsScanned()).To(Equal(result.Scans), tc.name)
				Expect(props.ScanState.BytesScanned()).To(Equal(result.Bytes), tc.name)
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
			fmt.Fprintf(GinkgoWriter, "PENDING_QUEUE_LIMIT case=%s ids=%v scans=%d bytes=%d reason=%s error=%s\n", tc.name, result.IDs, result.Scans, result.Bytes, result.Reason, result.ErrorClass)
		}
	})

	It("pins Java nested pending queue Any URL validation", func() {
		for _, tc := range []struct {
			kind    string
			message proto.Message
		}{
			{"vector", &gen.OldAndNewIndexEntries{}},
			{"sliding", &gen.SlidingWindowQueueEntry{}},
			{"delete-where", &gen.DeleteWhere{Prefix: tuple.Tuple{int64(7)}.Pack()}},
		} {
			for _, prefix := range []string{"", "type.googleapis.com/", "custom.example/v1/", "/"} {
				data, err := anypb.New(tc.message)
				Expect(err).NotTo(HaveOccurred())
				data.TypeUrl = prefix + string(data.MessageName())
				encoded, err := proto.Marshal(data)
				Expect(err).NotTo(HaveOccurred())
				var accepted bool
				Expect(java.InvokeAs(ctx, "pendingQueueNestedAny", map[string]any{"kind": tc.kind, "payload": BytesToIntArray(encoded)}, &accepted)).To(Succeed())
				Expect(accepted).To(Equal(prefix != ""), "kind=%s prefix=%q", tc.kind, prefix)
				fmt.Fprintf(GinkgoWriter, "NESTED-ANY kind=%s prefix=%q accepted=%t\n", tc.kind, prefix, accepted)
			}
		}
	})

	It("pins Java pending queue envelope and Any validation", func() {
		order, err := anypb.New(&gen.Order{OrderId: proto.Int64(42)})
		Expect(err).NotTo(HaveOccurred())
		operation, err := anypb.New(&gen.PendingWritesQueueEntry{Operation: gen.PendingWritesQueueEntry_UPDATE.Enum()})
		Expect(err).NotTo(HaveOccurred())
		for _, tc := range []struct {
			name         string
			version      *int32
			payload      *anypb.Any
			indexPayload bool
			errorText    string
			value        int64
			rawEnvelope  []byte
		}{
			{name: "default URL", version: proto.Int32(1), payload: order, value: 42},
			{name: "absent version", payload: order, value: 42},
			{name: "negative version", version: proto.Int32(-1), payload: order, value: 42},
			{name: "custom URL", version: proto.Int32(1), payload: &anypb.Any{TypeUrl: "example.test/" + string(gen.File_record_layer_demo_proto.Messages().ByName("Order").FullName()), Value: order.Value}, value: 42},
			{name: "slashless URL", version: proto.Int32(1), payload: &anypb.Any{TypeUrl: string(gen.File_record_layer_demo_proto.Messages().ByName("Order").FullName()), Value: order.Value}, errorText: "Pending writes queue entry payload type does not match the queue's bound type"},
			{name: "malformed envelope", rawEnvelope: []byte{0xff}, errorText: "Failed to parse pending writes queue entry"},
			{name: "future version", version: proto.Int32(2), payload: order, errorText: "Pending writes queue entry version is newer than this reader supports"},
			{name: "missing payload", version: proto.Int32(1), errorText: "Pending writes queue entry payload type does not match the queue's bound type"},
			{name: "wrong type", version: proto.Int32(1), payload: operation, errorText: "Pending writes queue entry payload type does not match the queue's bound type"},
			{name: "malformed payload", version: proto.Int32(1), payload: &anypb.Any{TypeUrl: order.TypeUrl, Value: []byte{0xff}}, errorText: "Failed to unpack pending writes queue entry payload"},
			{name: "update operation", version: proto.Int32(1), payload: operation, indexPayload: true, value: 1},
			{name: "overwide update operation", version: proto.Int32(1), payload: &anypb.Any{TypeUrl: operation.TypeUrl, Value: []byte{8, 0x81, 0x80, 0x80, 0x80, 0x10}}, indexPayload: true, value: 1},
			{name: "overwide delete operation", version: proto.Int32(1), payload: &anypb.Any{TypeUrl: operation.TypeUrl, Value: []byte{8, 0x82, 0x80, 0x80, 0x80, 0x10}}, indexPayload: true, value: 2},
			{name: "unknown required operation", version: proto.Int32(1), payload: &anypb.Any{TypeUrl: operation.TypeUrl, Value: []byte{8, 99}}, indexPayload: true, errorText: "Failed to unpack pending writes queue entry payload"},
			{name: "known then unknown operation", version: proto.Int32(1), payload: &anypb.Any{TypeUrl: operation.TypeUrl, Value: []byte{8, 1, 8, 99}}, indexPayload: true, value: 1},
			{name: "negative unknown then known operation", version: proto.Int32(1), payload: &anypb.Any{TypeUrl: operation.TypeUrl, Value: []byte{8, 0xff, 0xff, 0xff, 0xff, 0x0f, 8, 1}}, indexPayload: true, value: 1},
			{name: "overwide unknown then known operation", version: proto.Int32(1), payload: &anypb.Any{TypeUrl: operation.TypeUrl, Value: []byte{8, 0xe3, 0x80, 0x80, 0x80, 0x10, 8, 1}}, indexPayload: true, value: 1},
			{name: "unknown then known operation", version: proto.Int32(1), payload: &anypb.Any{TypeUrl: operation.TypeUrl, Value: []byte{8, 99, 8, 1}}, indexPayload: true, value: 1},
			{name: "absent required operation", version: proto.Int32(1), payload: &anypb.Any{TypeUrl: operation.TypeUrl}, indexPayload: true, errorText: "Failed to unpack pending writes queue entry payload"},
		} {
			envelope, err := proto.Marshal(&gen.PendingWriteItem{Version: tc.version, Payload: tc.payload, EnqueueTimestamp: proto.Int64(123)})
			Expect(err).NotTo(HaveOccurred())
			params := buildJavaParams()
			if tc.rawEnvelope != nil {
				envelope = tc.rawEnvelope
			}
			params["envelope"] = BytesToIntArray(envelope)
			params["indexPayload"] = tc.indexPayload
			raw, err := java.Invoke(ctx, "pendingQueueEnvelope", params)
			Expect(err).NotTo(HaveOccurred(), tc.name)
			var result struct {
				Value        int64  `json:"value"`
				TypeURL      string `json:"typeUrl"`
				Timestamp    int64  `json:"timestamp"`
				Incarnation  int64  `json:"incarnation"`
				Serialized   string `json:"serialized"`
				ErrorClass   string `json:"errorClass"`
				ErrorMessage string `json:"errorMessage"`
				Info         struct {
					Version       *int32 `json:"version"`
					StoredVersion *int32 `json:"stored_version"`
					ExpectedType  string `json:"expected_type"`
					ActualType    string `json:"actual_type"`
				} `json:"info"`
			}
			Expect(json.Unmarshal(raw, &result)).To(Succeed())
			if tc.errorText != "" {
				Expect(result.ErrorClass).To(Equal("RecordCoreStorageException"), tc.name)
				Expect(result.ErrorMessage).To(Equal(tc.errorText), tc.name)
			} else {
				Expect(result.ErrorClass).To(BeEmpty(), tc.name)
				Expect(result.ErrorMessage).To(BeEmpty(), tc.name)
				Expect(result.Value).To(Equal(tc.value), tc.name)
				Expect(result.TypeURL).To(Equal(tc.payload.TypeUrl), tc.name)
				Expect(result.Timestamp).To(Equal(int64(123)), tc.name)
				Expect(result.Incarnation).To(Equal(int64(7)), tc.name)
			}
			_, err = env.RecordDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
				root := env.Keyspace.Sub("pendingQueueEnvelope")
				var value, timestamp int64
				var url string
				var decodeErr error
				if tc.indexPayload {
					queue := recordlayer.NewPendingWritesQueue(root.Sub("entries"), root.Sub("size"), 0, &gen.PendingWritesQueueEntry{})
					cursor := queue.GetQueueCursor(rtx, recordlayer.ForwardScan(), nil)
					defer cursor.Close()
					item, err := cursor.OnNext(ctx)
					decodeErr = err
					if err == nil {
						Expect(item.HasNext()).To(BeTrue())
						entry := item.GetValue()
						value, timestamp, url = int64(entry.Payload.GetOperation()), entry.EnqueueTimestamp, entry.PayloadTypeURL
						serialized, err := proto.Marshal(entry.Payload)
						Expect(err).NotTo(HaveOccurred())
						Expect(base64.StdEncoding.EncodeToString(serialized)).To(Equal(result.Serialized), tc.name)
					}
				} else {
					queue := recordlayer.NewPendingWritesQueue(root.Sub("entries"), root.Sub("size"), 0, &gen.Order{})
					cursor := queue.GetQueueCursor(rtx, recordlayer.ForwardScan(), nil)
					defer cursor.Close()
					item, err := cursor.OnNext(ctx)
					decodeErr = err
					if err == nil {
						Expect(item.HasNext()).To(BeTrue())
						entry := item.GetValue()
						value, timestamp, url = entry.Payload.GetOrderId(), entry.EnqueueTimestamp, entry.PayloadTypeURL
					}
				}
				if tc.errorText != "" {
					var storageErr *recordlayer.RecordCoreStorageError
					Expect(errors.As(decodeErr, &storageErr)).To(BeTrue(), "%s: %v", tc.name, decodeErr)
					Expect(storageErr.Message).To(Equal(tc.errorText), tc.name)
					Expect(storageErr.KeyTuple).To(HaveLen(2), tc.name)
					Expect(storageErr.KeyTuple[0]).To(Equal(int64(7)), tc.name)
					Expect(storageErr.Version).To(Equal(result.Info.Version), tc.name)
					Expect(storageErr.StoredVersion).To(Equal(result.Info.StoredVersion), tc.name)
					Expect(storageErr.ActualType).To(Equal(result.Info.ActualType), tc.name)
					if result.Info.ExpectedType != "" {
						expected := order
						javaClass := "com.apple.foundationdb.record.RecordLayerDemo$Order"
						if tc.indexPayload {
							expected = operation
							javaClass = "com.apple.foundationdb.record.IndexBuildProto$PendingWritesQueueEntry"
						}
						Expect(result.Info.ExpectedType).To(Equal(javaClass), tc.name)
						Expect(storageErr.ExpectedType).To(Equal(strings.TrimPrefix(expected.TypeUrl, "type.googleapis.com/")), tc.name)
					} else {
						Expect(storageErr.ExpectedType).To(BeEmpty(), tc.name)
					}
					if tc.name == "future version" {
						Expect(storageErr.Version).NotTo(BeNil())
						Expect(*storageErr.Version).To(Equal(int32(1)))
						Expect(*storageErr.StoredVersion).To(Equal(int32(2)))
					}
				} else {
					Expect(decodeErr).NotTo(HaveOccurred(), tc.name)
					Expect(value).To(Equal(result.Value), tc.name)
					Expect(timestamp).To(Equal(result.Timestamp), tc.name)
					Expect(url).To(Equal(result.TypeURL), tc.name)
				}
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
			fmt.Fprintf(GinkgoWriter, "PENDING_QUEUE_ENVELOPE case=%s expectedError=%q value=%d\n", tc.name, tc.errorText, tc.value)
		}
	})

	It("pins bidirectional pending queue skip and row continuation semantics", func() {
		for _, javaWriter := range []bool{true, false} {
			for _, skip := range []int{0, 1, 5} {
				root := env.Keyspace.Sub("pendingQueueSkip")
				queue := recordlayer.NewPendingWritesQueue(root.Sub("entries"), root.Sub("size"), 0, &gen.Order{})
				if !javaWriter {
					_, err := env.RecordDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
						rtx.ClearRange(root)
						for id := int64(1); id <= 3; id++ {
							if err := queue.Enqueue(rtx, &gen.Order{OrderId: proto.Int64(id), Tags: []string{strings.Repeat("x", 120000)}}, 0); err != nil {
								return nil, err
							}
						}
						return nil, nil
					})
					Expect(err).NotTo(HaveOccurred())
				}
				params := buildJavaParams()
				params["skip"], params["seed"] = skip, javaWriter
				raw, err := java.Invoke(ctx, "pendingQueueSkip", params)
				Expect(err).NotTo(HaveOccurred())
				var result struct {
					IDs       []int64 `json:"ids"`
					Reason    string  `json:"reason"`
					Resumed   []int64 `json:"resumed"`
					Exhausted bool    `json:"exhausted"`
					Size      int64   `json:"size"`
				}
				Expect(json.Unmarshal(raw, &result)).To(Succeed())
				// Java clears the inner skip without applying an outer skip wrapper.
				// Preserve this observed queue behavior independently of record scans.
				Expect(result.IDs).To(Equal([]int64{1, 2}))
				Expect(result.Reason).To(Equal("RETURN_LIMIT_REACHED"))
				Expect(result.Resumed).To(Equal([]int64{3}))
				Expect(result.Exhausted).To(BeTrue())
				Expect(result.Size).To(Equal(int64(3)))
				_, err = env.RecordDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
					props := recordlayer.DefaultExecuteProperties().WithSkip(skip).WithReturnedRowLimit(2)
					cursor := queue.GetQueueCursor(rtx, recordlayer.NewScanProperties(props), nil)
					defer cursor.Close()
					for _, id := range result.IDs {
						row, err := cursor.OnNext(ctx)
						Expect(err).NotTo(HaveOccurred())
						Expect(row.HasNext()).To(BeTrue())
						Expect(row.GetValue().Payload.GetOrderId()).To(Equal(id))
					}
					stop, err := cursor.OnNext(ctx)
					Expect(err).NotTo(HaveOccurred())
					Expect(stop.HasNext()).To(BeFalse())
					Expect(stop.GetNoNextReason()).To(Equal(recordlayer.ReturnLimitReached))
					continuation, err := stop.GetContinuation().ToBytes()
					Expect(err).NotTo(HaveOccurred())
					next := queue.GetQueueCursor(rtx, recordlayer.ForwardScan(), continuation)
					defer next.Close()
					row, err := next.OnNext(ctx)
					Expect(err).NotTo(HaveOccurred())
					Expect(row.HasNext()).To(BeTrue())
					Expect(row.GetValue().Payload.GetOrderId()).To(Equal(result.Resumed[0]))
					end, err := next.OnNext(ctx)
					Expect(err).NotTo(HaveOccurred())
					Expect(end.HasNext()).To(BeFalse())
					Expect(end.GetNoNextReason()).To(Equal(recordlayer.SourceExhausted))
					return nil, nil
				})
				Expect(err).NotTo(HaveOccurred())
				fmt.Fprintf(GinkgoWriter, "PENDING_QUEUE_SKIP javaWriter=%t skip=%d ids=%v reason=%s resumed=%v size=%d\n", javaWriter, skip, result.IDs, result.Reason, result.Resumed, result.Size)
			}
		}
	})

	It("executes an inline record explode across independent Java type repositories", func() {
		for _, nullable := range []bool{true, false} {
			params := buildJavaParams()
			params["nullable"] = nullable
			raw, err := java.Invoke(ctx, "explodeIndependentRepositories", params)
			Expect(err).NotTo(HaveOccurred())
			var result struct {
				Rows [][]int64 `json:"rows"`
			}
			Expect(json.Unmarshal(raw, &result)).To(Succeed())
			Expect(result.Rows).To(Equal([][]int64{{7, 9}, {7, 9}}))
			constructor := values.NewRawRecordConstructorValue(
				values.RecordConstructorField{Name: "Z", Value: &values.ConstantValue{Typ: values.NotNullInt, Value: int32(7)}},
				values.RecordConstructorField{Name: "A", Value: &values.ConstantValue{Typ: values.NotNullLong, Value: int64(9)}},
			)
			plan, err := plans.NewRecordQueryExplodePlan(values.NewArrayConstructorValue(values.WithNullability(constructor.Type(), nullable), []values.Value{constructor}))
			Expect(err).NotTo(HaveOccurred())
			Expect(cascades.FinalizePlan(plan)).To(Succeed())
			original := constructor.MessageDescriptor()
			foreign, err := values.NewTypeProtoRepository().MessageDescriptorFor(constructor.Type())
			Expect(err).NotTo(HaveOccurred())
			Expect(foreign == original).To(BeFalse())
			for i := range 2 {
				if i == 1 {
					constructor.SetMessageDescriptor(foreign)
				}
				cur, err := executor.ExecutePlan(ctx, plan, nil, executor.EmptyEvaluationContext(), nil, recordlayer.DefaultExecuteProperties())
				Expect(err).NotTo(HaveOccurred())
				rows, err := executor.CollectAll(ctx, cur)
				_ = cur.Close()
				Expect(err).NotTo(HaveOccurred())
				Expect(rows).To(HaveLen(1))
				Expect(rows[0].Positional.Slots).To(Equal([]any{int64(7), int64(9)}))
			}
			fmt.Fprintf(GinkgoWriter, "EXPLODE_REPOSITORIES nullable=%v Java rows=%v Go rows=[[7 9] [7 9]] distinct descriptors verified\n", nullable, result.Rows)
		}
	})

	It("pins nullable protobuf explode carriers against Java", func() {
		for _, nullable := range []bool{false, true} {
			for _, nestedArray := range []bool{false, true} {
				for _, duplicateNames := range []bool{false, true} {
					params := buildJavaParams()
					params["nullable"], params["nestedArray"], params["duplicateNames"] = nullable, nestedArray, duplicateNames
					raw, err := java.Invoke(ctx, "explodeProtoShape", params)
					if duplicateNames {
						var javaErr *JavaError
						Expect(errors.As(err, &javaErr)).To(BeTrue())
						Expect(javaErr.ExceptionClass).To(Equal("IllegalArgumentException"))
					} else {
						Expect(err).NotTo(HaveOccurred())
					}
					var result struct {
						Value    int64    `json:"value"`
						Nullable bool     `json:"nullable"`
						Tags     []string `json:"tags"`
					}
					if !duplicateNames {
						Expect(json.Unmarshal(raw, &result)).To(Succeed())
						Expect(result.Value).To(Equal(int64(99)))
						Expect(result.Nullable).To(Equal(nullable))
						Expect(result.Tags).To(Equal([]string{"a"}))
					}
					message := &gen.Order{OrderId: proto.Int64(99), Tags: []string{"a"}}
					original := executor.PositionalTypeForDescriptor(message.ProtoReflect().Descriptor())
					fields := append([]values.Field(nil), original.Fields...)
					if duplicateNames {
						fields[1].Name = fields[0].Name
					}
					if nestedArray {
						fields[3].FieldType = values.WithNullability(fields[3].FieldType, true)
					}
					declared := &values.RecordType{Nullable: nullable, Fields: fields}
					plan, err := plans.NewRecordQueryExplodePlan(&values.ConstantValue{Value: []any{message}, Typ: values.NewArrayType(false, declared)})
					Expect(err).NotTo(HaveOccurred())
					cur, err := executor.ExecutePlan(ctx, plan, nil, executor.EmptyEvaluationContext(), nil, recordlayer.DefaultExecuteProperties())
					Expect(err).NotTo(HaveOccurred())
					rows, err := executor.CollectAll(ctx, cur)
					_ = cur.Close()
					if duplicateNames {
						var resolution *values.ResolutionError
						Expect(errors.As(err, &resolution)).To(BeTrue())
						Expect(resolution.ErrorCode).To(Equal(values.LayoutCarrierMismatch))
						fmt.Fprintf(GinkgoWriter, "EXPLODE_PROTO duplicateNames=true nullable=%v nestedArray=%v Java=IllegalArgumentException Go=LayoutCarrierMismatch\n", nullable, nestedArray)
						continue
					}
					Expect(err).NotTo(HaveOccurred())
					Expect(rows).To(HaveLen(1))
					Expect(rows[0].Positional.Slots[0]).To(Equal(int64(99)))
					Expect(rows[0].Positional.Type.IsNullable()).To(Equal(nullable))
					Expect(rows[0].Positional.Slots[3]).To(Equal([]any{"a"}))
					fmt.Fprintf(GinkgoWriter, "EXPLODE_PROTO nullable=%v nestedArray=%v duplicateNames=%v Java=%d Go=%v\n", nullable, nestedArray, duplicateNames, result.Value, rows[0].Positional.Slots[0])
				}
			}
		}
	})

	It("evaluates bound defaults against the live Java core API", func() {
		for _, all := range []bool{false, true} {
			for _, empty := range []bool{false, true} {
				for _, sameAlias := range []bool{false, true} {
					params := buildJavaParams()
					params["all"], params["empty"], params["sameAlias"] = all, empty, sameAlias
					raw, err := java.Invoke(ctx, "defaultBinding", params)
					Expect(err).NotTo(HaveOccurred())
					var result struct {
						Value     int64 `json:"value"`
						Exhausted bool  `json:"exhausted"`
					}
					Expect(json.Unmarshal(raw, &result)).To(Succeed())
					want := int64(11)
					if empty {
						want = 99
					}
					Expect(result.Value).To(Equal(want))
					Expect(result.Exhausted).To(BeTrue())
					input := []any{int64(11)}
					if empty {
						input = nil
					}
					inner, err := plans.NewRecordQueryExplodePlan(&values.ConstantValue{Value: input, Typ: values.NewArrayType(false, values.NullableLong)})
					Expect(err).NotTo(HaveOccurred())
					var fallback values.Value = &values.ParameterValue{Ordinal: 1, Typ: values.NullableLong}
					quantifier := expressions.NewPhysicalQuantifier(expressions.FinalOf(inner))
					ec := executor.EmptyEvaluationContext().WithParams([]any{int64(99)})
					if sameAlias {
						fallback, err = values.NewQuantifiedObjectValue(quantifier.GetAlias(), inner.GetResultType())
						Expect(err).NotTo(HaveOccurred())
						ec = ec.WithBinding(quantifier.GetAlias(), int64(99))
					}
					var plan plans.RecordQueryPlan
					if all {
						plan, err = plans.NewRecordQueryDefaultOnEmptyPlanFromQuantifier(quantifier, fallback)
					} else {
						plan, err = plans.NewRecordQueryFirstOrDefaultPlanFromQuantifier(quantifier, fallback)
					}
					Expect(err).NotTo(HaveOccurred())
					cur, err := executor.ExecutePlan(ctx, plan, nil, ec, nil, recordlayer.DefaultExecuteProperties())
					Expect(err).NotTo(HaveOccurred())
					rows, err := executor.CollectAll(ctx, cur)
					_ = cur.Close()
					Expect(err).NotTo(HaveOccurred())
					Expect(rows).To(HaveLen(1))
					Expect(rows[0].Positional.Slots).To(Equal([]any{want}))
					fmt.Fprintf(GinkgoWriter, "DEFAULT_BINDING all=%v empty=%v sameAlias=%v Java=%d Go=%v\n", all, empty, sameAlias, result.Value, rows[0].Positional.Slots)
				}
			}
		}
	})

	It("pins first-or-default request properties against the live Java core API", func() {
		// This is a direct core-plan probe, not a SQL reach claim. Java has no
		// separate projection plan; both Go mapping forms correspond to MapPlan.
		for _, kind := range []string{"direct", "map", "projection"} {
			for count := 0; count <= 2; count++ {
				for skip := 0; skip <= 2; skip++ {
					params := buildJavaParams()
					params["count"], params["skip"], params["mapped"] = count, skip, kind != "direct"
					raw, err := java.Invoke(ctx, "firstOrDefaultRequest", params)
					Expect(err).NotTo(HaveOccurred())
					var result struct {
						Value           int64 `json:"value"`
						RowResumable    bool  `json:"rowResumable"`
						SourceExhausted bool  `json:"sourceExhausted"`
						ResumeExhausted bool  `json:"resumeExhausted"`
					}
					Expect(json.Unmarshal(raw, &result)).To(Succeed())
					want := int64(99)
					if skip < count {
						want = int64(11 * (skip + 1))
					}
					if kind != "direct" {
						want = 42
					}
					Expect(result.Value).To(Equal(want))
					Expect(result.RowResumable).To(BeTrue())
					Expect(result.SourceExhausted).To(BeTrue())
					Expect(result.ResumeExhausted).To(BeTrue())
					input := []any{}
					for i := 0; i < count; i++ {
						input = append(input, int64(11*(i+1)))
					}
					inner, err := plans.NewRecordQueryExplodePlan(&values.ConstantValue{
						Value: input, Typ: values.NewArrayType(false, values.NotNullLong),
					})
					Expect(err).NotTo(HaveOccurred())
					first, err := plans.NewRecordQueryFirstOrDefaultPlan(inner, &values.ConstantValue{Value: int64(99), Typ: values.NotNullLong})
					Expect(err).NotTo(HaveOccurred())
					var plan plans.RecordQueryPlan = first
					constant := &values.ConstantValue{Value: int64(42), Typ: values.NotNullLong}
					if kind == "map" {
						plan, err = plans.NewRecordQueryMapPlan(plan, constant)
					} else if kind == "projection" {
						plan, err = plans.NewRecordQueryProjectionPlan([]values.Value{constant}, plan)
					}
					Expect(err).NotTo(HaveOccurred())
					props := recordlayer.DefaultExecuteProperties().WithSkip(skip).WithReturnedRowLimit(1)
					cursor, err := executor.ExecutePlan(ctx, plan, nil, executor.EmptyEvaluationContext(), nil, props)
					Expect(err).NotTo(HaveOccurred())
					row, err := cursor.OnNext(ctx)
					Expect(err).NotTo(HaveOccurred())
					Expect(row.HasNext()).To(BeTrue())
					got, ok := row.GetValue().Positional.Get(0)
					Expect(ok).To(BeTrue())
					Expect(got).To(Equal(want))
					token, err := row.GetContinuation().ToBytes()
					Expect(err).NotTo(HaveOccurred())
					Expect(token).NotTo(BeEmpty())
					end, err := cursor.OnNext(ctx)
					Expect(err).NotTo(HaveOccurred())
					Expect(end.HasNext()).To(BeFalse())
					Expect(end.GetNoNextReason()).To(Equal(recordlayer.SourceExhausted))
					Expect(cursor.Close()).To(Succeed())
					resumed, err := executor.ExecutePlan(ctx, plan, nil, executor.EmptyEvaluationContext(), token, props)
					Expect(err).NotTo(HaveOccurred())
					end, err = resumed.OnNext(ctx)
					Expect(err).NotTo(HaveOccurred())
					Expect(end.HasNext()).To(BeFalse())
					Expect(end.GetNoNextReason()).To(Equal(recordlayer.SourceExhausted))
					Expect(resumed.Close()).To(Succeed())
					GinkgoWriter.Printf("FOD_REQUEST kind=%s count=%d skip=%d java=%d go=%v\n", kind, count, skip, result.Value, got)
				}
			}
		}
	})

	// javaScanResult represents the response from scanOrdersWithContinuation
	type javaScanResult struct {
		Orders          []map[string]any `json:"orders"`
		Continuation    string           `json:"continuation"` // base64
		SourceExhausted bool             `json:"sourceExhausted"`
	}

	scanWithJava := func(limit int, continuation string) javaScanResult {
		params := buildJavaParams()
		params["limit"] = limit
		if continuation != "" {
			params["continuation"] = continuation
		}
		raw, err := java.Invoke(ctx, "scanOrdersWithContinuation", params)
		Expect(err).NotTo(HaveOccurred())

		var result javaScanResult
		err = json.Unmarshal(raw, &result)
		Expect(err).NotTo(HaveOccurred())
		return result
	}

	scanWithGo := func(limit int, continuation []byte) ([]*gen.Order, []byte, bool) {
		var orders []*gen.Order
		var nextCont []byte
		var sourceExhausted bool

		_, err := env.RecordDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
			store, err := recordlayer.NewStoreBuilder().
				SetContext(rtx).
				SetMetaDataProvider(env.MetaData).
				SetSubspace(env.Keyspace).
				CreateOrOpen()
			if err != nil {
				return nil, err
			}

			scanProps := recordlayer.NewScanProperties(
				recordlayer.DefaultExecuteProperties().WithReturnedRowLimit(limit),
			)

			cursor := store.ScanRecords(continuation, scanProps)
			for {
				result, err := cursor.OnNext(ctx)
				if err != nil {
					return nil, err
				}
				if !result.HasNext() {
					var contErr error
					nextCont, contErr = result.GetContinuation().ToBytes()
					if contErr != nil {
						return nil, contErr
					}
					sourceExhausted = result.GetNoNextReason().IsSourceExhausted()
					break
				}
				order := result.GetValue().Record.(*gen.Order)
				orders = append(orders, order)
			}
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
		return orders, nextCont, sourceExhausted
	}

	Describe("Go generates continuation, Java resumes", func() {
		It("should allow Java to resume from Go's continuation token", func() {
			// Go scans first 3 records
			goOrders, goCont, goExhausted := scanWithGo(3, nil)
			Expect(goOrders).To(HaveLen(3))
			Expect(goExhausted).To(BeFalse())
			Expect(goCont).NotTo(BeNil())
			Expect(*goOrders[0].OrderId).To(Equal(int64(1)))
			Expect(*goOrders[2].OrderId).To(Equal(int64(3)))

			// Java resumes from Go's continuation token
			goContB64 := base64.StdEncoding.EncodeToString(goCont)
			javaResult := scanWithJava(3, goContB64)
			Expect(javaResult.Orders).To(HaveLen(3))
			Expect(javaResult.SourceExhausted).To(BeFalse())

			// Java should get records 4, 5, 6
			for i, order := range javaResult.Orders {
				orderID, ok := order["orderId"].(float64)
				Expect(ok).To(BeTrue())
				Expect(int64(orderID)).To(Equal(int64(4 + i)))
			}

			// Java resumes again from its own continuation to get 7, 8, 9
			javaResult2 := scanWithJava(3, javaResult.Continuation)
			Expect(javaResult2.Orders).To(HaveLen(3))
			for i, order := range javaResult2.Orders {
				orderID, ok := order["orderId"].(float64)
				Expect(ok).To(BeTrue())
				Expect(int64(orderID)).To(Equal(int64(7 + i)))
			}

			// Final batch: record 10, then exhausted
			javaResult3 := scanWithJava(3, javaResult2.Continuation)
			Expect(javaResult3.Orders).To(HaveLen(1))
			Expect(javaResult3.SourceExhausted).To(BeTrue())
			orderID, ok := javaResult3.Orders[0]["orderId"].(float64)
			Expect(ok).To(BeTrue())
			Expect(int64(orderID)).To(Equal(int64(10)))
		})
	})

	Describe("Java generates continuation, Go resumes", func() {
		It("should allow Go to resume from Java's continuation token", func() {
			// Java scans first 3 records
			javaResult := scanWithJava(3, "")
			Expect(javaResult.Orders).To(HaveLen(3))
			Expect(javaResult.SourceExhausted).To(BeFalse())
			Expect(javaResult.Continuation).NotTo(BeEmpty())

			// Verify Java got records 1, 2, 3
			for i, order := range javaResult.Orders {
				orderID, ok := order["orderId"].(float64)
				Expect(ok).To(BeTrue())
				Expect(int64(orderID)).To(Equal(int64(1 + i)))
			}

			// Go resumes from Java's continuation token
			javaCont, err := base64.StdEncoding.DecodeString(javaResult.Continuation)
			Expect(err).NotTo(HaveOccurred())

			goOrders, goCont, goExhausted := scanWithGo(3, javaCont)
			Expect(goOrders).To(HaveLen(3))
			Expect(goExhausted).To(BeFalse())

			// Go should get records 4, 5, 6
			for i, order := range goOrders {
				Expect(*order.OrderId).To(Equal(int64(4 + i)))
			}

			// Go resumes again to get 7, 8, 9
			goOrders2, goCont2, goExhausted2 := scanWithGo(3, goCont)
			Expect(goOrders2).To(HaveLen(3))
			Expect(goExhausted2).To(BeFalse())
			for i, order := range goOrders2 {
				Expect(*order.OrderId).To(Equal(int64(7 + i)))
			}

			// Final batch: record 10, then exhausted
			goOrders3, _, goExhausted3 := scanWithGo(3, goCont2)
			Expect(goOrders3).To(HaveLen(1))
			Expect(goExhausted3).To(BeTrue())
			Expect(*goOrders3[0].OrderId).To(Equal(int64(10)))
		})
	})

	Describe("Go and Java emit byte-identical continuation tokens", func() {
		It("Go's continuation matches Java's for the same scan position (TO_NEW wire parity)", func() {
			// Both engines scan the first 3 records from the start with the same limit.
			_, goCont, goExhausted := scanWithGo(3, nil)
			Expect(goExhausted).To(BeFalse())
			Expect(goCont).NotTo(BeNil())

			javaResult := scanWithJava(3, "")
			Expect(javaResult.Continuation).NotTo(BeEmpty())
			javaCont, err := base64.StdEncoding.DecodeString(javaResult.Continuation)
			Expect(err).NotTo(HaveOccurred())

			// Pre-fix Go emitted a raw key suffix while Java emits a proto-wrapped
			// KeyValueCursorContinuation{inner_continuation, magic_number}
			// (KeyValueCursorBase defaults SerializationMode to TO_NEW). The engines
			// were merely read-tolerant of each other; the EMITTED bytes diverged.
			// They must now be byte-identical — a token written by one engine must be
			// indistinguishable from the other's, the actual wire-compat contract.
			Expect(goCont).To(Equal(javaCont))

			// And the bytes are the TO_NEW proto form (magic present), not raw.
			msg := &gen.KeyValueCursorContinuation{}
			Expect(msg.UnmarshalVT(goCont)).To(Succeed())
			Expect(msg.GetMagicNumber()).To(Equal(int64(6_773_487_359_078_157_740)))
		})
	})

	Describe("Alternating Go and Java with continuations", func() {
		It("should maintain correct position when alternating implementations", func() {
			// Go scans 2
			goOrders, goCont, _ := scanWithGo(2, nil)
			Expect(goOrders).To(HaveLen(2))
			Expect(*goOrders[0].OrderId).To(Equal(int64(1)))
			Expect(*goOrders[1].OrderId).To(Equal(int64(2)))

			// Java resumes with Go's continuation, scans 2
			goContB64 := base64.StdEncoding.EncodeToString(goCont)
			javaResult := scanWithJava(2, goContB64)
			Expect(javaResult.Orders).To(HaveLen(2))
			Expect(int64(javaResult.Orders[0]["orderId"].(float64))).To(Equal(int64(3)))
			Expect(int64(javaResult.Orders[1]["orderId"].(float64))).To(Equal(int64(4)))

			// Go resumes with Java's continuation, scans 2
			javaCont, err := base64.StdEncoding.DecodeString(javaResult.Continuation)
			Expect(err).NotTo(HaveOccurred())
			goOrders2, goCont2, _ := scanWithGo(2, javaCont)
			Expect(goOrders2).To(HaveLen(2))
			Expect(*goOrders2[0].OrderId).To(Equal(int64(5)))
			Expect(*goOrders2[1].OrderId).To(Equal(int64(6)))

			// Java resumes with Go's continuation, scans remaining
			goContB64_2 := base64.StdEncoding.EncodeToString(goCont2)
			javaResult2 := scanWithJava(10, goContB64_2)
			Expect(javaResult2.Orders).To(HaveLen(4))
			Expect(javaResult2.SourceExhausted).To(BeTrue())
			Expect(int64(javaResult2.Orders[0]["orderId"].(float64))).To(Equal(int64(7)))
			Expect(int64(javaResult2.Orders[3]["orderId"].(float64))).To(Equal(int64(10)))
		})
	})
})
