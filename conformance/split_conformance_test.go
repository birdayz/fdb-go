//go:build bazelrunfiles

package conformance_test

import (
	"context"
	"fmt"
	"strings"

	"fdb.dev/gen"
	"fdb.dev/pkg/fdbgo/fdb/subspace"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	"fdb.dev/pkg/recordlayer"
	"github.com/google/uuid"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"google.golang.org/protobuf/proto"
)

var _ = Describe("Split Record Conformance", func() {
	var (
		ctx   context.Context
		env   *TenantEnvironment
		store *SplitConformanceStore
	)

	BeforeEach(func() {
		ctx = context.Background()

		tenantName := fmt.Sprintf("split_%s", uuid.New().String())

		var err error
		env, err = SetupTenantEnvironment(ctx, sharedContainer, tenantName)
		Expect(err).NotTo(HaveOccurred())

		store, err = NewSplitConformanceStore(env.RecordDB, env.Keyspace, env.ClusterFile, env.TenantName)
		Expect(err).NotTo(HaveOccurred())
	})

	AfterEach(func() {
		if env != nil {
			_ = env.Cleanup(ctx)
		}
	})

	Describe("Go writes, Java reads", func() {
		It("should handle 250KB split record (3 chunks)", func() {
			// 250KB will be split into 3 chunks at 100KB boundaries
			padding := strings.Repeat("X", 250_000)
			order := &gen.Order{
				OrderId: proto.Int64(1),
				Price:   proto.Int32(42),
				Flower:  &gen.Flower{Type: proto.String(padding), Color: gen.Color_RED.Enum()},
			}
			err := store.SaveRecord(ctx, order)
			Expect(err).NotTo(HaveOccurred())
		})

		It("should handle small record with split enabled (unsplit path)", func() {
			// A record under 100KB should still work correctly when split is enabled.
			// It goes through the unsplit path (suffix 0) instead of being chunked.
			order := StandardOrder(2)
			err := store.SaveRecord(ctx, order)
			Expect(err).NotTo(HaveOccurred())
		})

		It("should handle 150KB split record (2 chunks)", func() {
			padding := strings.Repeat("A", 150_000)
			order := &gen.Order{
				OrderId: proto.Int64(3),
				Price:   proto.Int32(77),
				Flower:  &gen.Flower{Type: proto.String(padding), Color: gen.Color_BLUE.Enum()},
			}
			err := store.SaveRecord(ctx, order)
			Expect(err).NotTo(HaveOccurred())
		})
	})

	Describe("Java writes, Go reads", func() {
		It("should handle 250KB split record from Java", func() {
			// Java saves a large record split across multiple KV pairs,
			// Go must reassemble the chunks correctly.
			padding := strings.Repeat("Y", 250_000)
			order := &gen.Order{
				OrderId: proto.Int64(10),
				Price:   proto.Int32(99),
				Flower:  &gen.Flower{Type: proto.String(padding), Color: gen.Color_BLUE.Enum()},
			}
			loaded, err := store.JavaSaveThenGoLoad(ctx, order)
			Expect(err).NotTo(HaveOccurred())
			Expect(proto.Equal(order, loaded)).To(BeTrue())
		})

		It("should handle small record from Java with split enabled", func() {
			order := NewOrder(11).
				WithPrice(33).
				WithFlower("Daisy", gen.Color_YELLOW).
				Build()
			loaded, err := store.JavaSaveThenGoLoad(ctx, order)
			Expect(err).NotTo(HaveOccurred())
			Expect(proto.Equal(order, loaded)).To(BeTrue())
		})

		It("should handle 150KB split record from Java", func() {
			padding := strings.Repeat("B", 150_000)
			order := &gen.Order{
				OrderId: proto.Int64(12),
				Price:   proto.Int32(55),
				Flower:  &gen.Flower{Type: proto.String(padding), Color: gen.Color_PINK.Enum()},
			}
			loaded, err := store.JavaSaveThenGoLoad(ctx, order)
			Expect(err).NotTo(HaveOccurred())
			Expect(proto.Equal(order, loaded)).To(BeTrue())
		})
	})

	Describe("Boundary sizes", func() {
		It("should handle record at approximately 100KB", func() {
			// Right around the split boundary. The serialized protobuf size
			// determines whether splitting occurs, not just the string length.
			padding := strings.Repeat("Z", 100_000)
			order := &gen.Order{
				OrderId: proto.Int64(20),
				Price:   proto.Int32(1),
				Flower:  &gen.Flower{Type: proto.String(padding), Color: gen.Color_YELLOW.Enum()},
			}
			err := store.SaveRecord(ctx, order)
			Expect(err).NotTo(HaveOccurred())
		})

		It("should handle minimal order with split enabled", func() {
			order := MinimalOrder(21)
			err := store.SaveRecord(ctx, order)
			Expect(err).NotTo(HaveOccurred())
		})
	})

	Describe("Overwrite with split", func() {
		It("should handle overwriting a split record with a small record", func() {
			// First write a large split record
			padding := strings.Repeat("L", 200_000)
			large := &gen.Order{
				OrderId: proto.Int64(30),
				Price:   proto.Int32(100),
				Flower:  &gen.Flower{Type: proto.String(padding), Color: gen.Color_RED.Enum()},
			}
			err := store.SaveRecord(ctx, large)
			Expect(err).NotTo(HaveOccurred())

			// Overwrite with a small record — old split chunks must be cleared
			small := NewOrder(30).
				WithPrice(1).
				WithFlower("Tiny", gen.Color_BLUE).
				Build()
			err = store.SaveRecord(ctx, small)
			Expect(err).NotTo(HaveOccurred())
		})

		It("should handle overwriting a small record with a split record", func() {
			// First write a small record
			small := NewOrder(31).
				WithPrice(5).
				WithFlower("Small", gen.Color_YELLOW).
				Build()
			err := store.SaveRecord(ctx, small)
			Expect(err).NotTo(HaveOccurred())

			// Overwrite with a large split record
			padding := strings.Repeat("G", 200_000)
			large := &gen.Order{
				OrderId: proto.Int64(31),
				Price:   proto.Int32(200),
				Flower:  &gen.Flower{Type: proto.String(padding), Color: gen.Color_PINK.Enum()},
			}
			err = store.SaveRecord(ctx, large)
			Expect(err).NotTo(HaveOccurred())
		})
	})
})

// SplitConformanceStore wraps split record operations and cross-validates with Java.
// It uses split-enabled metadata on both Go and Java sides so that records >100KB
// are stored as multiple FDB key-value pairs and can be read by either implementation.
type SplitConformanceStore struct {
	recordDB    *recordlayer.FDBDatabase
	metaData    *recordlayer.RecordMetaData
	keyspace    subspace.Subspace
	java        *JavaInvoker
	clusterFile string
	tenantName  string
}

// NewSplitConformanceStore creates a split-enabled conformance store for cross-validation.
func NewSplitConformanceStore(recordDB *recordlayer.FDBDatabase, keyspace subspace.Subspace, clusterFile string, tenantName string) (*SplitConformanceStore, error) {
	md, err := createSplitOrderMetaData()
	if err != nil {
		return nil, fmt.Errorf("failed to create split metadata: %w", err)
	}

	return &SplitConformanceStore{
		recordDB:    recordDB,
		metaData:    md,
		keyspace:    keyspace,
		java:        NewJavaInvoker(),
		clusterFile: clusterFile,
		tenantName:  tenantName,
	}, nil
}

// buildJavaParams builds base parameters for Java invocations.
func (s *SplitConformanceStore) buildJavaParams() map[string]any {
	params := map[string]any{
		"clusterFile": s.clusterFile,
		"subspace":    BytesToIntArray(s.keyspace.Bytes()),
	}
	if s.tenantName != "" {
		params["tenantName"] = s.tenantName
	}
	return params
}

// SaveRecord saves a record with Go (split enabled), then has Java load it to verify
// Java can read Go's split chunks. Also reads back with Go for sanity.
func (s *SplitConformanceStore) SaveRecord(ctx context.Context, msg proto.Message) error {
	order, ok := msg.(*gen.Order)
	if !ok {
		return fmt.Errorf("only Order records are supported in split conformance tests")
	}

	// 1. Save with Go (split-enabled metadata)
	_, err := s.recordDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
		store, err := recordlayer.NewStoreBuilder().
			SetContext(rtx).
			SetMetaDataProvider(s.metaData).
			SetSubspace(s.keyspace).
			CreateOrOpen()
		if err != nil {
			return nil, err
		}
		_, err = store.SaveRecord(msg)
		return nil, err
	})
	if err != nil {
		return fmt.Errorf("go save failed: %w", err)
	}

	// 2. Java loads what Go wrote (validates Java can read Go's split chunks)
	var javaOrder gen.Order
	params := s.buildJavaParams()
	params["orderID"] = *order.OrderId
	err = s.java.InvokeAs(ctx, "loadSplitOrder", params, &javaOrder)
	if err != nil {
		return fmt.Errorf("java cross-check read of Go-written split record failed: %w", err)
	}

	// 3. Go loads back its own data
	goOrder, err := s.loadRecordWithGo(ctx, *order.OrderId)
	if err != nil {
		return fmt.Errorf("go cross-check read failed: %w", err)
	}

	// 4. Compare
	if !proto.Equal(goOrder, &javaOrder) {
		return fmt.Errorf("split conformance mismatch: Java read differs from Go read\nGo:   %+v\nJava: %+v", goOrder, &javaOrder)
	}

	return nil
}

// JavaSaveThenGoLoad has Java save a record (with split enabled), then Go loads it.
// Validates Go can reassemble Java's split chunks.
func (s *SplitConformanceStore) JavaSaveThenGoLoad(ctx context.Context, order *gen.Order) (*gen.Order, error) {
	// 1. Java saves the record with split-enabled metadata
	params := s.buildJavaParams()
	params["order"] = order
	err := s.java.InvokeAs(ctx, "saveSplitOrder", params, nil)
	if err != nil {
		return nil, fmt.Errorf("java save of split record failed: %w", err)
	}

	// 2. Go loads what Java wrote
	goOrder, err := s.loadRecordWithGo(ctx, *order.OrderId)
	if err != nil {
		return nil, fmt.Errorf("go load of Java-written split record failed: %w", err)
	}

	// 3. Also verify Java can read its own data
	var javaOrder gen.Order
	params = s.buildJavaParams()
	params["orderID"] = *order.OrderId
	err = s.java.InvokeAs(ctx, "loadSplitOrder", params, &javaOrder)
	if err != nil {
		return nil, fmt.Errorf("java cross-check read of its own split record failed: %w", err)
	}

	// 4. Compare Go and Java reads
	if !proto.Equal(goOrder, &javaOrder) {
		return nil, fmt.Errorf("split conformance mismatch: Go read differs from Java read\nGo:   %+v\nJava: %+v", goOrder, &javaOrder)
	}

	return goOrder, nil
}

// loadRecordWithGo loads a record using only Go with split-enabled metadata.
func (s *SplitConformanceStore) loadRecordWithGo(ctx context.Context, orderID int64) (*gen.Order, error) {
	var order *gen.Order
	_, err := s.recordDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
		store, err := recordlayer.NewStoreBuilder().
			SetContext(rtx).
			SetMetaDataProvider(s.metaData).
			SetSubspace(s.keyspace).
			CreateOrOpen()
		if err != nil {
			return nil, err
		}

		storedRecord, err := store.LoadRecord(tuple.Tuple{orderID})
		if err != nil {
			return nil, err
		}

		if storedRecord == nil {
			return nil, fmt.Errorf("record not found: %d", orderID)
		}

		order = storedRecord.Record.(*gen.Order)
		return nil, nil
	})
	return order, err
}

// createSplitOrderMetaData creates RecordMetaData with split long records enabled.
// Must match the Java createSplitMetaData() configuration exactly.
func createSplitOrderMetaData() (*recordlayer.RecordMetaData, error) {
	builder := recordlayer.NewRecordMetaDataBuilder().
		SetRecords(gen.File_record_layer_demo_proto).
		SetSplitLongRecords(true)
	builder.GetRecordType("Order").SetPrimaryKey(recordlayer.Field("order_id"))
	builder.GetRecordType("Customer").SetPrimaryKey(recordlayer.Field("customer_id"))
	builder.GetRecordType("TypedRecord").SetPrimaryKey(recordlayer.Field("id"))
	return builder.Build()
}
