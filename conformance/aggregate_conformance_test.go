//go:build bazelrunfiles

package conformance_test

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"google.golang.org/protobuf/proto"

	"fdb.dev/gen"
	"fdb.dev/pkg/fdbgo/fdb/subspace"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	"fdb.dev/pkg/recordlayer"
)

var _ = Describe("EvaluateAggregateFunction Conformance", func() {
	// ========== COUNT aggregate ==========
	Describe("COUNT aggregate via COUNT index", func() {
		var (
			ctx context.Context
			env *TenantEnvironment
			s   *AggregateConformanceStore
		)

		BeforeEach(func() {
			ctx = context.Background()
			tenantName := fmt.Sprintf("agg_count_%s", uuid.New().String())
			var err error
			env, err = SetupTenantEnvironment(ctx, sharedContainer, tenantName)
			Expect(err).NotTo(HaveOccurred())
			s, err = NewAggregateConformanceStore(env, AggTypeCount)
			Expect(err).NotTo(HaveOccurred())
		})

		AfterEach(func() {
			if env != nil {
				_ = env.Cleanup(ctx)
			}
		})

		It("Go writes, Go evaluates COUNT = 3", func() {
			s.SaveOrdersGo(ctx, 100, 200, 300)
			result := s.EvaluateCountGo(ctx)
			Expect(result).To(Equal(int64(3)))
		})

		It("Go writes, Java evaluates COUNT = 3", func() {
			s.SaveOrdersGo(ctx, 100, 200, 300)
			result := s.EvaluateCountJava(ctx)
			Expect(result).To(Equal(int64(3)))
		})
	})

	// ========== SUM aggregate ==========
	Describe("SUM aggregate via SUM index", func() {
		var (
			ctx context.Context
			env *TenantEnvironment
			s   *AggregateConformanceStore
		)

		BeforeEach(func() {
			ctx = context.Background()
			tenantName := fmt.Sprintf("agg_sum_%s", uuid.New().String())
			var err error
			env, err = SetupTenantEnvironment(ctx, sharedContainer, tenantName)
			Expect(err).NotTo(HaveOccurred())
			s, err = NewAggregateConformanceStore(env, AggTypeSum)
			Expect(err).NotTo(HaveOccurred())
		})

		AfterEach(func() {
			if env != nil {
				_ = env.Cleanup(ctx)
			}
		})

		It("Go writes, Go evaluates SUM = 600", func() {
			s.SaveOrdersGo(ctx, 100, 200, 300)
			result := s.EvaluateSumGo(ctx)
			Expect(result).To(Equal(int64(600)))
		})

		It("Go writes, Java evaluates SUM = 600", func() {
			s.SaveOrdersGo(ctx, 100, 200, 300)
			result := s.EvaluateSumJava(ctx)
			Expect(result).To(Equal(int64(600)))
		})
	})

	// ========== MIN via VALUE index ==========
	Describe("MIN aggregate via VALUE index", func() {
		var (
			ctx context.Context
			env *TenantEnvironment
			s   *AggregateConformanceStore
		)

		BeforeEach(func() {
			ctx = context.Background()
			tenantName := fmt.Sprintf("agg_min_%s", uuid.New().String())
			var err error
			env, err = SetupTenantEnvironment(ctx, sharedContainer, tenantName)
			Expect(err).NotTo(HaveOccurred())
			s, err = NewAggregateConformanceStore(env, AggTypeMinValue)
			Expect(err).NotTo(HaveOccurred())
		})

		AfterEach(func() {
			if env != nil {
				_ = env.Cleanup(ctx)
			}
		})

		It("Go writes, Go evaluates MIN = 100", func() {
			s.SaveOrdersGo(ctx, 100, 200, 300)
			result := s.EvaluateMinGo(ctx)
			Expect(result).To(Equal(int64(100)))
		})

		It("Go writes, Java evaluates MIN = 100", func() {
			s.SaveOrdersGo(ctx, 100, 200, 300)
			result := s.EvaluateMinJava(ctx)
			Expect(result).To(Equal(int64(100)))
		})
	})

	// ========== MAX via VALUE index ==========
	Describe("MAX aggregate via VALUE index", func() {
		var (
			ctx context.Context
			env *TenantEnvironment
			s   *AggregateConformanceStore
		)

		BeforeEach(func() {
			ctx = context.Background()
			tenantName := fmt.Sprintf("agg_max_%s", uuid.New().String())
			var err error
			env, err = SetupTenantEnvironment(ctx, sharedContainer, tenantName)
			Expect(err).NotTo(HaveOccurred())
			s, err = NewAggregateConformanceStore(env, AggTypeMaxValue)
			Expect(err).NotTo(HaveOccurred())
		})

		AfterEach(func() {
			if env != nil {
				_ = env.Cleanup(ctx)
			}
		})

		It("Go writes, Go evaluates MAX = 300", func() {
			s.SaveOrdersGo(ctx, 100, 200, 300)
			result := s.EvaluateMaxGo(ctx)
			Expect(result).To(Equal(int64(300)))
		})

		It("Go writes, Java evaluates MAX = 300", func() {
			s.SaveOrdersGo(ctx, 100, 200, 300)
			result := s.EvaluateMaxJava(ctx)
			Expect(result).To(Equal(int64(300)))
		})
	})

	// ========== MIN_EVER via MIN_EVER_LONG index ==========
	Describe("MIN_EVER aggregate via MIN_EVER_LONG index", func() {
		var (
			ctx context.Context
			env *TenantEnvironment
			s   *AggregateConformanceStore
		)

		BeforeEach(func() {
			ctx = context.Background()
			tenantName := fmt.Sprintf("agg_minever_%s", uuid.New().String())
			var err error
			env, err = SetupTenantEnvironment(ctx, sharedContainer, tenantName)
			Expect(err).NotTo(HaveOccurred())
			s, err = NewAggregateConformanceStore(env, AggTypeMinEver)
			Expect(err).NotTo(HaveOccurred())
		})

		AfterEach(func() {
			if env != nil {
				_ = env.Cleanup(ctx)
			}
		})

		It("Go writes, Go evaluates MIN_EVER = 100", func() {
			s.SaveOrdersGo(ctx, 100, 200, 300)
			result := s.EvaluateMinEverGo(ctx)
			Expect(result).To(Equal(int64(100)))
		})

		It("Go writes, Java evaluates MIN_EVER = 100", func() {
			s.SaveOrdersGo(ctx, 100, 200, 300)
			result := s.EvaluateMinEverJava(ctx)
			Expect(result).To(Equal(int64(100)))
		})
	})

	// ========== MAX_EVER via MAX_EVER_LONG index ==========
	Describe("MAX_EVER aggregate via MAX_EVER_LONG index", func() {
		var (
			ctx context.Context
			env *TenantEnvironment
			s   *AggregateConformanceStore
		)

		BeforeEach(func() {
			ctx = context.Background()
			tenantName := fmt.Sprintf("agg_maxever_%s", uuid.New().String())
			var err error
			env, err = SetupTenantEnvironment(ctx, sharedContainer, tenantName)
			Expect(err).NotTo(HaveOccurred())
			s, err = NewAggregateConformanceStore(env, AggTypeMaxEver)
			Expect(err).NotTo(HaveOccurred())
		})

		AfterEach(func() {
			if env != nil {
				_ = env.Cleanup(ctx)
			}
		})

		It("Go writes, Go evaluates MAX_EVER = 300", func() {
			s.SaveOrdersGo(ctx, 100, 200, 300)
			result := s.EvaluateMaxEverGo(ctx)
			Expect(result).To(Equal(int64(300)))
		})

		It("Go writes, Java evaluates MAX_EVER = 300", func() {
			s.SaveOrdersGo(ctx, 100, 200, 300)
			result := s.EvaluateMaxEverJava(ctx)
			Expect(result).To(Equal(int64(300)))
		})

		It("Java writes, Go evaluates MAX_EVER = 300", func() {
			s.SaveOrdersJava(ctx, 100, 200, 300)
			result := s.EvaluateMaxEverGo(ctx)
			Expect(result).To(Equal(int64(300)))
		})
	})

	// ========== Cross-language: Java writes, Go evaluates ==========
	Describe("Java writes, Go evaluates COUNT", func() {
		var (
			ctx context.Context
			env *TenantEnvironment
			s   *AggregateConformanceStore
		)

		BeforeEach(func() {
			ctx = context.Background()
			tenantName := fmt.Sprintf("agg_jcount_%s", uuid.New().String())
			var err error
			env, err = SetupTenantEnvironment(ctx, sharedContainer, tenantName)
			Expect(err).NotTo(HaveOccurred())
			s, err = NewAggregateConformanceStore(env, AggTypeCount)
			Expect(err).NotTo(HaveOccurred())
		})

		AfterEach(func() {
			if env != nil {
				_ = env.Cleanup(ctx)
			}
		})

		It("Java writes 3, Go evaluates COUNT = 3", func() {
			s.SaveOrdersJava(ctx, 100, 200, 300)
			result := s.EvaluateCountGo(ctx)
			Expect(result).To(Equal(int64(3)))
		})
	})

	Describe("Java writes, Go evaluates SUM", func() {
		var (
			ctx context.Context
			env *TenantEnvironment
			s   *AggregateConformanceStore
		)

		BeforeEach(func() {
			ctx = context.Background()
			tenantName := fmt.Sprintf("agg_jsum_%s", uuid.New().String())
			var err error
			env, err = SetupTenantEnvironment(ctx, sharedContainer, tenantName)
			Expect(err).NotTo(HaveOccurred())
			s, err = NewAggregateConformanceStore(env, AggTypeSum)
			Expect(err).NotTo(HaveOccurred())
		})

		AfterEach(func() {
			if env != nil {
				_ = env.Cleanup(ctx)
			}
		})

		It("Java writes 3, Go evaluates SUM = 600", func() {
			s.SaveOrdersJava(ctx, 100, 200, 300)
			result := s.EvaluateSumGo(ctx)
			Expect(result).To(Equal(int64(600)))
		})
	})

	Describe("Java writes, Go evaluates MIN_EVER", func() {
		var (
			ctx context.Context
			env *TenantEnvironment
			s   *AggregateConformanceStore
		)

		BeforeEach(func() {
			ctx = context.Background()
			tenantName := fmt.Sprintf("agg_jminever_%s", uuid.New().String())
			var err error
			env, err = SetupTenantEnvironment(ctx, sharedContainer, tenantName)
			Expect(err).NotTo(HaveOccurred())
			s, err = NewAggregateConformanceStore(env, AggTypeMinEver)
			Expect(err).NotTo(HaveOccurred())
		})

		AfterEach(func() {
			if env != nil {
				_ = env.Cleanup(ctx)
			}
		})

		It("Java writes 3, Go evaluates MIN_EVER = 100", func() {
			s.SaveOrdersJava(ctx, 100, 200, 300)
			result := s.EvaluateMinEverGo(ctx)
			Expect(result).To(Equal(int64(100)))
		})
	})
})

// AggType selects which aggregate index + function to test.
type AggType int

const (
	AggTypeCount AggType = iota
	AggTypeSum
	AggTypeMinValue // MIN via VALUE index
	AggTypeMaxValue // MAX via VALUE index
	AggTypeMinEver  // MIN_EVER via MIN_EVER_LONG index
	AggTypeMaxEver  // MAX_EVER via MAX_EVER_LONG index
)

// AggregateConformanceStore wraps Go store operations and Java invocations
// for testing EvaluateAggregateFunction cross-language compatibility.
type AggregateConformanceStore struct {
	recordDB    *recordlayer.FDBDatabase
	metaData    *recordlayer.RecordMetaData
	keyspace    subspace.Subspace
	java        *JavaInvoker
	clusterFile string
	tenantName  string
	aggType     AggType
}

func NewAggregateConformanceStore(env *TenantEnvironment, aggType AggType) (*AggregateConformanceStore, error) {
	builder := recordlayer.NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
	builder.GetRecordType("Order").SetPrimaryKey(recordlayer.Field("order_id"))
	builder.GetRecordType("Customer").SetPrimaryKey(recordlayer.Field("customer_id"))
	builder.GetRecordType("TypedRecord").SetPrimaryKey(recordlayer.Field("id"))

	switch aggType {
	case AggTypeCount:
		builder.AddIndex("Order",
			recordlayer.NewCountIndex("agg_count_price", recordlayer.GroupAll(recordlayer.Field("price"))))
	case AggTypeSum:
		builder.AddIndex("Order",
			recordlayer.NewSumIndex("agg_sum_price", recordlayer.Ungrouped(recordlayer.Field("price"))))
	case AggTypeMinEver:
		builder.AddIndex("Order",
			recordlayer.NewMinEverLongIndex("agg_min_ever_price", recordlayer.Ungrouped(recordlayer.Field("price"))))
	case AggTypeMaxEver:
		builder.AddIndex("Order",
			recordlayer.NewMaxEverLongIndex("agg_max_ever_price", recordlayer.Ungrouped(recordlayer.Field("price"))))
	case AggTypeMinValue, AggTypeMaxValue:
		builder.AddIndex("Order",
			recordlayer.NewIndex("agg_price_value", recordlayer.Field("price")))
	}

	md, err := builder.Build()
	if err != nil {
		return nil, err
	}

	ks := env.Keyspace
	if env.TenantName != "" {
		ks = subspace.Sub(tuple.Tuple{})
	}

	return &AggregateConformanceStore{
		recordDB:    env.RecordDB,
		metaData:    md,
		keyspace:    ks,
		java:        NewJavaInvoker(),
		clusterFile: env.ClusterFile,
		tenantName:  env.TenantName,
		aggType:     aggType,
	}, nil
}

func (s *AggregateConformanceStore) buildJavaParams() map[string]any {
	params := map[string]any{
		"clusterFile": s.clusterFile,
		"subspace":    BytesToIntArray(s.keyspace.Bytes()),
	}
	if s.tenantName != "" {
		params["tenantName"] = s.tenantName
	}
	return params
}

// SaveOrdersGo saves orders with sequential IDs and the given prices via Go.
func (s *AggregateConformanceStore) SaveOrdersGo(ctx context.Context, prices ...int32) {
	for i, price := range prices {
		_, err := s.recordDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
			store, err := recordlayer.NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(s.metaData).SetSubspace(s.keyspace).CreateOrOpen()
			if err != nil {
				return nil, err
			}
			_, err = store.SaveRecord(&gen.Order{
				OrderId: proto.Int64(int64(i + 1)),
				Price:   proto.Int32(price),
			})
			return nil, err
		})
		Expect(err).NotTo(HaveOccurred())
	}
}

// javaStepForSave returns the Java step name for saving an order.
func (s *AggregateConformanceStore) javaStepForSave() string {
	switch s.aggType {
	case AggTypeCount:
		return "saveOrderWithCountAggregate"
	case AggTypeSum:
		return "saveOrderWithSumAggregate"
	case AggTypeMinEver:
		return "saveOrderWithMinEverAggregate"
	case AggTypeMaxEver:
		return "saveOrderWithMaxEverAggregate"
	case AggTypeMinValue:
		return "saveOrderWithMinValueAggregate"
	case AggTypeMaxValue:
		return "saveOrderWithMaxValueAggregate"
	}
	return ""
}

// SaveOrdersJava saves orders with sequential IDs and the given prices via Java.
func (s *AggregateConformanceStore) SaveOrdersJava(ctx context.Context, prices ...int32) {
	stepName := s.javaStepForSave()
	for i, price := range prices {
		params := s.buildJavaParams()
		params["order"] = &gen.Order{
			OrderId: proto.Int64(int64(i + 1)),
			Price:   proto.Int32(price),
		}
		err := s.java.InvokeAs(ctx, stepName, params, nil)
		Expect(err).NotTo(HaveOccurred())
	}
}

// --- COUNT ---

func (s *AggregateConformanceStore) EvaluateCountGo(ctx context.Context) int64 {
	var result int64
	_, err := s.recordDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
		store, err := recordlayer.NewStoreBuilder().
			SetContext(rtx).SetMetaDataProvider(s.metaData).SetSubspace(s.keyspace).CreateOrOpen()
		if err != nil {
			return nil, err
		}
		r, err := store.EvaluateAggregateFunction(ctx, []string{"Order"},
			&recordlayer.IndexAggregateFunction{
				Name:    recordlayer.FunctionNameCount,
				Operand: recordlayer.GroupAll(recordlayer.Field("price")),
			},
			recordlayer.TupleRangeAll, recordlayer.IsolationLevelSerializable)
		if err != nil {
			return nil, err
		}
		result = r[0].(int64)
		return nil, nil
	})
	Expect(err).NotTo(HaveOccurred())
	return result
}

func (s *AggregateConformanceStore) EvaluateCountJava(ctx context.Context) int64 {
	params := s.buildJavaParams()
	var result float64
	err := s.java.InvokeAs(ctx, "evaluateCountAggregate", params, &result)
	Expect(err).NotTo(HaveOccurred())
	return int64(result)
}

// --- SUM ---

func (s *AggregateConformanceStore) EvaluateSumGo(ctx context.Context) int64 {
	var result int64
	_, err := s.recordDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
		store, err := recordlayer.NewStoreBuilder().
			SetContext(rtx).SetMetaDataProvider(s.metaData).SetSubspace(s.keyspace).CreateOrOpen()
		if err != nil {
			return nil, err
		}
		r, err := store.EvaluateAggregateFunction(ctx, []string{"Order"},
			&recordlayer.IndexAggregateFunction{
				Name:    recordlayer.FunctionNameSum,
				Operand: recordlayer.Ungrouped(recordlayer.Field("price")),
			},
			recordlayer.TupleRangeAll, recordlayer.IsolationLevelSerializable)
		if err != nil {
			return nil, err
		}
		result = r[0].(int64)
		return nil, nil
	})
	Expect(err).NotTo(HaveOccurred())
	return result
}

func (s *AggregateConformanceStore) EvaluateSumJava(ctx context.Context) int64 {
	params := s.buildJavaParams()
	var result float64
	err := s.java.InvokeAs(ctx, "evaluateSumAggregate", params, &result)
	Expect(err).NotTo(HaveOccurred())
	return int64(result)
}

// --- MIN via VALUE index ---

func (s *AggregateConformanceStore) EvaluateMinGo(ctx context.Context) int64 {
	var result int64
	_, err := s.recordDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
		store, err := recordlayer.NewStoreBuilder().
			SetContext(rtx).SetMetaDataProvider(s.metaData).SetSubspace(s.keyspace).CreateOrOpen()
		if err != nil {
			return nil, err
		}
		r, err := store.EvaluateAggregateFunction(ctx, []string{"Order"},
			&recordlayer.IndexAggregateFunction{
				Name:    recordlayer.FunctionNameMin,
				Operand: recordlayer.Field("price"),
			},
			recordlayer.TupleRangeAll, recordlayer.IsolationLevelSerializable)
		if err != nil {
			return nil, err
		}
		result = r[0].(int64)
		return nil, nil
	})
	Expect(err).NotTo(HaveOccurred())
	return result
}

func (s *AggregateConformanceStore) EvaluateMinJava(ctx context.Context) int64 {
	params := s.buildJavaParams()
	var result float64
	err := s.java.InvokeAs(ctx, "evaluateMinAggregate", params, &result)
	Expect(err).NotTo(HaveOccurred())
	return int64(result)
}

// --- MAX via VALUE index ---

func (s *AggregateConformanceStore) EvaluateMaxGo(ctx context.Context) int64 {
	var result int64
	_, err := s.recordDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
		store, err := recordlayer.NewStoreBuilder().
			SetContext(rtx).SetMetaDataProvider(s.metaData).SetSubspace(s.keyspace).CreateOrOpen()
		if err != nil {
			return nil, err
		}
		r, err := store.EvaluateAggregateFunction(ctx, []string{"Order"},
			&recordlayer.IndexAggregateFunction{
				Name:    recordlayer.FunctionNameMax,
				Operand: recordlayer.Field("price"),
			},
			recordlayer.TupleRangeAll, recordlayer.IsolationLevelSerializable)
		if err != nil {
			return nil, err
		}
		result = r[0].(int64)
		return nil, nil
	})
	Expect(err).NotTo(HaveOccurred())
	return result
}

func (s *AggregateConformanceStore) EvaluateMaxJava(ctx context.Context) int64 {
	params := s.buildJavaParams()
	var result float64
	err := s.java.InvokeAs(ctx, "evaluateMaxAggregate", params, &result)
	Expect(err).NotTo(HaveOccurred())
	return int64(result)
}

// --- MIN_EVER via MIN_EVER_LONG index ---

func (s *AggregateConformanceStore) EvaluateMinEverGo(ctx context.Context) int64 {
	var result int64
	_, err := s.recordDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
		store, err := recordlayer.NewStoreBuilder().
			SetContext(rtx).SetMetaDataProvider(s.metaData).SetSubspace(s.keyspace).CreateOrOpen()
		if err != nil {
			return nil, err
		}
		r, err := store.EvaluateAggregateFunction(ctx, []string{"Order"},
			&recordlayer.IndexAggregateFunction{
				Name:    recordlayer.FunctionNameMinEver,
				Operand: recordlayer.Ungrouped(recordlayer.Field("price")),
			},
			recordlayer.TupleRangeAll, recordlayer.IsolationLevelSerializable)
		if err != nil {
			return nil, err
		}
		result = r[0].(int64)
		return nil, nil
	})
	Expect(err).NotTo(HaveOccurred())
	return result
}

func (s *AggregateConformanceStore) EvaluateMinEverJava(ctx context.Context) int64 {
	params := s.buildJavaParams()
	var result float64
	err := s.java.InvokeAs(ctx, "evaluateMinEverAggregate", params, &result)
	Expect(err).NotTo(HaveOccurred())
	return int64(result)
}

// --- MAX_EVER via MAX_EVER_LONG index ---

func (s *AggregateConformanceStore) EvaluateMaxEverGo(ctx context.Context) int64 {
	var result int64
	_, err := s.recordDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
		store, err := recordlayer.NewStoreBuilder().
			SetContext(rtx).SetMetaDataProvider(s.metaData).SetSubspace(s.keyspace).CreateOrOpen()
		if err != nil {
			return nil, err
		}
		r, err := store.EvaluateAggregateFunction(ctx, []string{"Order"},
			&recordlayer.IndexAggregateFunction{
				Name:    recordlayer.FunctionNameMaxEver,
				Operand: recordlayer.Ungrouped(recordlayer.Field("price")),
			},
			recordlayer.TupleRangeAll, recordlayer.IsolationLevelSerializable)
		if err != nil {
			return nil, err
		}
		result = r[0].(int64)
		return nil, nil
	})
	Expect(err).NotTo(HaveOccurred())
	return result
}

func (s *AggregateConformanceStore) EvaluateMaxEverJava(ctx context.Context) int64 {
	params := s.buildJavaParams()
	var result float64
	err := s.java.InvokeAs(ctx, "evaluateMaxEverAggregate", params, &result)
	Expect(err).NotTo(HaveOccurred())
	return int64(result)
}
