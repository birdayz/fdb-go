//go:build bazelrunfiles

package conformance_test

import (
	"context"
	"errors"
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

type splitReadConflictResult struct {
	OperationResult bool `json:"operationResult"`
	CommitCode      int  `json:"commitCode"`
}

var _ = Describe("Single-record read conflict conformance", func() {
	It("matches Java range and GETS loads, including existence's limited range", func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		env, err := SetupTenantEnvironment(ctx, sharedContainer, "split_conflicts_"+uuid.NewString())
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func() {
			cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cleanupCancel()
			Expect(env.Cleanup(cleanupCtx)).To(Succeed())
		})
		metadata, err := createSplitOrderMetaData()
		Expect(err).NotTo(HaveOccurred())
		java := NewJavaInvoker()
		type population struct{ Cases, Conflicts, Commits int }
		counts := map[string]population{}
		for _, operation := range []string{"load", "save", "delete", "exists"} {
			for _, existing := range []bool{false, true} {
				for _, write := range []struct {
					name       string
					primaryKey int64
					suffix     int64
				}{
					{name: "inline-version", primaryKey: 1, suffix: -1},
					{name: "actual-unsplit", primaryKey: 1, suffix: 0},
					{name: "first-chunk", primaryKey: 1, suffix: 1},
					{name: "distant-chunk", primaryKey: 1, suffix: 9},
					{name: "unrelated-pk99", primaryKey: 99, suffix: 0},
				} {
					// Java keyExists stops after suffix 0 when the record exists;
					// full loads protect the entire record, absent suffixes included.
					conflicts := write.primaryKey == 1 && (operation != "exists" || !existing || write.suffix <= 0)
					want := splitReadConflictResult{OperationResult: existing || operation == "save"}
					if conflicts {
						want.CommitCode = 1020
					}
					goResult, err := runGoSplitReadConflict(env, metadata,
						env.Keyspace.Sub("go", operation, existing, write.name), operation, existing, write.primaryKey, write.suffix)
					Expect(err).NotTo(HaveOccurred(), "Go %s existing=%t write=%s", operation, existing, write.name)
					Expect(goResult).To(Equal(want), "Go %s existing=%t write=%s", operation, existing, write.name)
					for _, mode := range []string{"go-default", "java-default", "java-gets"} {
						result := goResult
						if mode != "go-default" {
							result = splitReadConflictResult{}
							Expect(java.InvokeAs(ctx, "splitReadConflictProbe", map[string]any{
								"clusterFile":      env.ClusterFile,
								"subspace":         BytesToIntArray(env.Keyspace.Sub(mode, operation, existing, write.name).Bytes()),
								"tenantName":       env.TenantName,
								"operation":        operation,
								"existing":         existing,
								"viaGets":          mode == "java-gets",
								"writerPrimaryKey": write.primaryKey,
								"suffix":           write.suffix,
							}, &result)).To(Succeed(), "%s %s existing=%t write=%s", mode, operation, existing, write.name)
							Expect(result).To(Equal(want), "%s %s existing=%t write=%s", mode, operation, existing, write.name)
							Expect(result).To(Equal(goResult), "%s versus Go %s existing=%t write=%s", mode, operation, existing, write.name)
						}
						p := counts[mode]
						p.Cases++
						if result.CommitCode == 1020 {
							p.Conflicts++
						} else {
							p.Commits++
						}
						counts[mode] = p
					}
				}
			}
		}
		// Four operations × existing/missing × five writes for each execution mode.
		for _, mode := range []string{"go-default", "java-default", "java-gets"} {
			Expect(counts[mode]).To(Equal(population{Cases: 40, Conflicts: 30, Commits: 10}), mode)
			fmt.Fprintf(GinkgoWriter, "split read conflicts %s: cases=%d conflicts=%d successful-commit-controls=%d\n",
				mode, counts[mode].Cases, counts[mode].Conflicts, counts[mode].Commits)
		}
		AddReportEntry("split-read-conflict-population", counts)
	})
})

// Both transactions are explicit and committed once: a runner retry could turn
// the intended 1020 into a successful reload and make this test meaningless.
func runGoSplitReadConflict(env *TenantEnvironment, metadata *recordlayer.RecordMetaData, root subspace.Subspace,
	operation string, existing bool, writerPrimaryKey, suffix int64,
) (splitReadConflictResult, error) {
	var result splitReadConflictResult
	setupTx, err := env.RecordDB.CreateTransaction()
	if err != nil {
		return result, err
	}
	setup := env.RecordDB.NewRecordContext(setupTx)
	defer setup.Cancel()
	store, err := recordlayer.NewStoreBuilder().SetContext(setup).SetMetaDataProvider(metadata).SetSubspace(root).Create()
	if err != nil {
		return result, fmt.Errorf("setup store: %w", err)
	}
	order := &demopb.Order{OrderId: proto.Int64(1), Price: proto.Int32(1)}
	if existing {
		if _, err := store.SaveRecord(order); err != nil {
			return result, fmt.Errorf("seed record: %w", err)
		}
	}
	if err := setup.Commit(); err != nil {
		return result, fmt.Errorf("commit setup: %w", err)
	}
	readerTx, err := env.RecordDB.CreateTransaction()
	if err != nil {
		return result, err
	}
	reader := env.RecordDB.NewRecordContext(readerTx)
	defer reader.Cancel()
	store, err = recordlayer.NewStoreBuilder().SetContext(reader).SetMetaDataProvider(metadata).SetSubspace(root).Open()
	if err != nil {
		return result, fmt.Errorf("open reader: %w", err)
	}
	primaryKey := tuple.Tuple{int64(1)}
	switch operation {
	case "load":
		var stored *recordlayer.FDBStoredRecord[proto.Message]
		stored, err = store.LoadRecord(primaryKey)
		result.OperationResult = stored != nil
	case "save":
		order.Price = proto.Int32(2)
		var stored *recordlayer.FDBStoredRecord[proto.Message]
		stored, err = store.SaveRecord(order)
		result.OperationResult = stored != nil
	case "delete":
		result.OperationResult, err = store.DeleteRecord(primaryKey)
	case "exists":
		result.OperationResult, err = store.RecordExists(primaryKey, recordlayer.IsolationLevelSerializable)
	default:
		return result, fmt.Errorf("unknown split conflict operation: %s", operation)
	}
	if err != nil {
		return result, fmt.Errorf("reader %s: %w", operation, err)
	}
	readerTx.Set(root.Pack(tuple.Tuple{"reader-marker"}), []byte{1})
	writer, err := env.RecordDB.CreateTransaction()
	if err != nil {
		return result, err
	}
	defer writer.Cancel()
	writer.Set(root.Sub(recordlayer.RecordKey).Pack(tuple.Tuple{writerPrimaryKey, suffix}), []byte{42})
	if err := writer.Commit().Get(); err != nil {
		return result, fmt.Errorf("commit concurrent writer: %w", err)
	}
	if err := reader.Commit(); err != nil {
		var fdbError fdb.Error
		if !errors.As(err, &fdbError) {
			return result, fmt.Errorf("commit reader: %w", err)
		}
		result.CommitCode = fdbError.Code
	}
	return result, nil
}
