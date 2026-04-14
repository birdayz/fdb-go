package meter_test

import (
	"context"
	"os"
	"testing"
	"time"

	. "github.com/onsi/gomega"
	"google.golang.org/protobuf/proto"

	storev1 "github.com/birdayz/fdb-record-layer-go/examples/metrognome/gen/metrognome/store/v1"
	"github.com/birdayz/fdb-record-layer-go/examples/metrognome/internal/meter"
	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/fdb"
	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/fdb/subspace"
	rl "github.com/birdayz/fdb-record-layer-go/pkg/recordlayer"
	foundationdbtc "github.com/birdayz/fdb-record-layer-go/pkg/testcontainers/foundationdb"
)

var testRecordDB *rl.FDBDatabase

func TestMain(m *testing.M) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	container, err := foundationdbtc.Run(ctx, "",
		foundationdbtc.WithAPIVersion(720),
	)
	if err != nil {
		panic("failed to start FDB container: " + err.Error())
	}

	clusterFile, err := container.ClusterFile(ctx)
	if err != nil {
		panic("failed to get cluster file: " + err.Error())
	}

	tmpFile, err := os.CreateTemp("", "meter_test_*.txt")
	if err != nil {
		panic(err.Error())
	}
	if _, err := tmpFile.WriteString(clusterFile); err != nil {
		panic(err.Error())
	}
	tmpFile.Close()

	fdb.MustAPIVersion(720)
	fdbDB, err := fdb.OpenDatabase(tmpFile.Name())
	if err != nil {
		panic("failed to open FDB: " + err.Error())
	}
	testRecordDB = rl.NewFDBDatabase(fdbDB)

	code := m.Run()

	_ = container.Terminate(context.Background())
	_ = os.Remove(tmpFile.Name())
	os.Exit(code)
}

func TestDynamicMeterNoGroupBy(t *testing.T) {
	t.Parallel()
	g := NewWithT(t)
	ctx := context.Background()

	engine := meter.NewEngine(testRecordDB, subspace.Sub("test_meter_no_group"))

	// Register a simple meter with no group-by (just customer + bucket)
	m := &storev1.Meter{
		Id:              proto.String("m1"),
		Slug:            proto.String("simple_counter"),
		Name:            proto.String("Simple Counter"),
		AggregationType: storev1.AggregationType_AGGREGATION_TYPE_SUM.Enum(),
	}
	g.Expect(engine.Register(m)).To(Succeed())

	// Ingest some events
	bucket := int64(1718400000000) // 2024-06-15 00:00:00 UTC
	g.Expect(engine.IngestEvent(ctx, "simple_counter", "cust-1", bucket, 100, nil)).To(Succeed())
	g.Expect(engine.IngestEvent(ctx, "simple_counter", "cust-1", bucket, 200, nil)).To(Succeed())
	g.Expect(engine.IngestEvent(ctx, "simple_counter", "cust-1", bucket, 50, nil)).To(Succeed())

	// Query: should get 350
	total, err := engine.GetUsage(ctx, "simple_counter", "cust-1", bucket, bucket, nil)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(total).To(Equal(int64(350)))
}

func TestDynamicMeterWithGroupBy(t *testing.T) {
	t.Parallel()
	g := NewWithT(t)
	ctx := context.Background()

	engine := meter.NewEngine(testRecordDB, subspace.Sub("test_meter_group"))

	// Register a meter with group-by on "region" and "model"
	m := &storev1.Meter{
		Id:                proto.String("m2"),
		Slug:              proto.String("llm_tokens"),
		Name:              proto.String("LLM Tokens"),
		AggregationType:   storev1.AggregationType_AGGREGATION_TYPE_SUM.Enum(),
		GroupByProperties: []string{"region", "model"},
	}
	g.Expect(engine.Register(m)).To(Succeed())

	bucket := int64(1718400000000)

	// Ingest events with different groups
	g.Expect(engine.IngestEvent(ctx, "llm_tokens", "cust-1", bucket, 500,
		map[string]string{"region": "us-east-1", "model": "gpt-4"})).To(Succeed())
	g.Expect(engine.IngestEvent(ctx, "llm_tokens", "cust-1", bucket, 300,
		map[string]string{"region": "us-east-1", "model": "gpt-4"})).To(Succeed())
	g.Expect(engine.IngestEvent(ctx, "llm_tokens", "cust-1", bucket, 1000,
		map[string]string{"region": "eu-west-1", "model": "claude-4"})).To(Succeed())
	g.Expect(engine.IngestEvent(ctx, "llm_tokens", "cust-2", bucket, 200,
		map[string]string{"region": "us-east-1", "model": "gpt-4"})).To(Succeed())

	// Query total for cust-1, us-east-1, gpt-4: should be 500 + 300 = 800
	total, err := engine.GetUsage(ctx, "llm_tokens", "cust-1", bucket, bucket,
		map[string]string{"region": "us-east-1", "model": "gpt-4"})
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(total).To(Equal(int64(800)))

	// Query total for cust-1, eu-west-1, claude-4: should be 1000
	total, err = engine.GetUsage(ctx, "llm_tokens", "cust-1", bucket, bucket,
		map[string]string{"region": "eu-west-1", "model": "claude-4"})
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(total).To(Equal(int64(1000)))

	// Query total for cust-2: should be 200
	total, err = engine.GetUsage(ctx, "llm_tokens", "cust-2", bucket, bucket,
		map[string]string{"region": "us-east-1", "model": "gpt-4"})
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(total).To(Equal(int64(200)))
}

func TestDynamicMeterMultipleBuckets(t *testing.T) {
	t.Parallel()
	g := NewWithT(t)
	ctx := context.Background()

	engine := meter.NewEngine(testRecordDB, subspace.Sub("test_meter_buckets"))

	m := &storev1.Meter{
		Id:              proto.String("m3"),
		Slug:            proto.String("api_reqs"),
		Name:            proto.String("API Requests"),
		AggregationType: storev1.AggregationType_AGGREGATION_TYPE_SUM.Enum(),
	}
	g.Expect(engine.Register(m)).To(Succeed())

	hour := int64(3600 * 1000)
	bucket1 := int64(1718400000000)
	bucket2 := bucket1 + hour
	bucket3 := bucket1 + 2*hour

	g.Expect(engine.IngestEvent(ctx, "api_reqs", "cust-1", bucket1, 10, nil)).To(Succeed())
	g.Expect(engine.IngestEvent(ctx, "api_reqs", "cust-1", bucket2, 20, nil)).To(Succeed())
	g.Expect(engine.IngestEvent(ctx, "api_reqs", "cust-1", bucket3, 30, nil)).To(Succeed())

	// Query across all 3 buckets: 10 + 20 + 30 = 60
	total, err := engine.GetUsage(ctx, "api_reqs", "cust-1", bucket1, bucket3, nil)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(total).To(Equal(int64(60)))

	// Query just bucket 2: 20
	total, err = engine.GetUsage(ctx, "api_reqs", "cust-1", bucket2, bucket2, nil)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(total).To(Equal(int64(20)))
}

func TestDynamicMeterIdempotentRegister(t *testing.T) {
	t.Parallel()
	g := NewWithT(t)

	engine := meter.NewEngine(testRecordDB, subspace.Sub("test_meter_idempotent"))

	m := &storev1.Meter{
		Id:   proto.String("m4"),
		Slug: proto.String("idem_meter"),
		Name: proto.String("Idempotent"),
	}

	g.Expect(engine.Register(m)).To(Succeed())
	g.Expect(engine.Register(m)).To(Succeed()) // second call should be no-op
}

func TestDynamicMeterUnregistered(t *testing.T) {
	t.Parallel()
	g := NewWithT(t)
	ctx := context.Background()

	engine := meter.NewEngine(testRecordDB, subspace.Sub("test_meter_unreg"))

	err := engine.IngestEvent(ctx, "nonexistent", "cust-1", 0, 100, nil)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("not registered"))
}
