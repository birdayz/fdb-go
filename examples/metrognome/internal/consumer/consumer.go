// Package consumer implements an exactly-once Kafka consumer for usage events.
//
// The key insight: Kafka offsets are NOT committed to Kafka's __consumer_offsets.
// Instead, the offset is written into FDB in the same transaction as the event
// records. On restart, the consumer reads its last committed offset from FDB
// and seeks to it. This gives exactly-once: if the FDB transaction fails, both
// the events AND the offset are rolled back.
package consumer

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/twmb/franz-go/pkg/kgo"
	"google.golang.org/protobuf/proto"

	storev1 "github.com/birdayz/fdb-record-layer-go/examples/metrognome/gen/metrognome/store/v1"
	"github.com/birdayz/fdb-record-layer-go/examples/metrognome/internal/billing"
	"github.com/birdayz/fdb-record-layer-go/examples/metrognome/internal/meter"
	"github.com/birdayz/fdb-record-layer-go/examples/metrognome/internal/storage"
	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/fdb/tuple"
	rl "github.com/birdayz/fdb-record-layer-go/pkg/recordlayer"
)

// Consumer reads usage events from Kafka and writes them to FDB with
// exactly-once semantics via FDB-transactional offset storage.
type Consumer struct {
	client      *kgo.Client
	db          *storage.DB
	meterEngine *meter.Engine
	log         *slog.Logger

	topic     string
	batchSize int

	// Lag tracking
	mu             sync.Mutex
	committedAt    map[int32]int64 // partition → last committed offset
	highWaterMark  map[int32]int64 // partition → latest seen offset from Kafka
	lastCommitTime map[int32]time.Time
}

// Config holds consumer configuration.
type Config struct {
	Brokers   []string
	Topic     string
	GroupID   string
	BatchSize int // max events per FDB transaction (default: 100)
}

// New creates a Kafka consumer. Does NOT start consuming — call Run() for that.
func New(cfg Config, db *storage.DB, meterEngine *meter.Engine, log *slog.Logger) (*Consumer, error) {
	if cfg.BatchSize <= 0 {
		cfg.BatchSize = 100
	}

	// Create Kafka client with manual offset management (no auto-commit)
	client, err := kgo.NewClient(
		kgo.SeedBrokers(cfg.Brokers...),
		kgo.ConsumerGroup(cfg.GroupID),
		kgo.ConsumeTopics(cfg.Topic),
		kgo.DisableAutoCommit(), // we manage offsets in FDB
	)
	if err != nil {
		return nil, fmt.Errorf("create kafka client: %w", err)
	}

	return &Consumer{
		client:         client,
		db:             db,
		meterEngine:    meterEngine,
		log:            log,
		topic:          cfg.Topic,
		batchSize:      cfg.BatchSize,
		committedAt:    make(map[int32]int64),
		highWaterMark:  make(map[int32]int64),
		lastCommitTime: make(map[int32]time.Time),
	}, nil
}

// Run starts the consumer loop. Blocks until ctx is cancelled.
func (c *Consumer) Run(ctx context.Context) error {
	c.log.Info("kafka consumer starting", "topic", c.topic)
	defer c.client.Close()

	for {
		select {
		case <-ctx.Done():
			c.log.Info("kafka consumer stopping")
			return ctx.Err()
		default:
		}

		fetches := c.client.PollRecords(ctx, c.batchSize)
		if fetches.IsClientClosed() {
			return nil
		}

		errs := fetches.Errors()
		for _, err := range errs {
			c.log.Error("kafka fetch error", "topic", err.Topic, "partition", err.Partition, "error", err.Err)
		}

		// Group records by partition for per-partition transactional processing
		fetches.EachPartition(func(p kgo.FetchTopicPartition) {
			if err := c.processPartition(ctx, p); err != nil {
				c.log.Error("process partition failed",
					"topic", p.Topic, "partition", p.Partition, "error", err)
			}
		})
	}
}

// processPartition handles all records from one partition in a single FDB transaction.
// Events + the partition's new offset are committed atomically.
func (c *Consumer) processPartition(ctx context.Context, p kgo.FetchTopicPartition) error {
	if len(p.Records) == 0 {
		return nil
	}

	// Parse all events first (outside the transaction)
	type parsedEvent struct {
		record *storev1.UsageEvent
		raw    *kgo.Record
		group  map[string]string
	}
	var events []parsedEvent
	var deadLetters []*storev1.DeadLetter

	now := time.Now().UnixMilli()
	for _, record := range p.Records {
		evt, groupVals, err := parseKafkaRecord(record, now)
		if err != nil {
			c.log.Warn("dead letter: malformed event", "offset", record.Offset, "error", err)
			deadLetters = append(deadLetters, &storev1.DeadLetter{
				Id:           proto.String(fmt.Sprintf("dl_%s_%d_%d", p.Topic, p.Partition, record.Offset)),
				Topic:        proto.String(p.Topic),
				Partition:    proto.Int32(p.Partition),
				Offset:       proto.Int64(record.Offset),
				RawValue:     record.Value,
				ErrorMessage: proto.String(err.Error()),
				CreatedAt:    proto.Int64(now),
			})
			continue
		}
		events = append(events, parsedEvent{record: evt, raw: record, group: groupVals})
	}

	if len(events) == 0 && len(deadLetters) == 0 {
		return nil
	}

	// Determine the final offset for this batch
	lastRecord := p.Records[len(p.Records)-1]
	newOffset := lastRecord.Offset + 1

	// Single FDB transaction: write events + update offset
	_, err := c.db.FDB().Run(ctx, func(rtx *rl.FDBRecordContext) (any, error) {
		store, err := rl.NewStoreBuilder().
			SetContext(rtx).
			SetMetaDataProvider(c.db.MetaData()).
			SetSubspace(c.db.Subspace()).
			CreateOrOpen()
		if err != nil {
			return nil, err
		}

		// Write events (with idempotency pre-check)
		for _, evt := range events {
			// Check idempotency
			idx := store.GetRecordMetaData().GetIndex("event_by_idempotency_key")
			cursor := store.ScanIndex(idx,
				rl.TupleRangeAllOf(tuple.Tuple{evt.record.GetIdempotencyKey()}), nil, rl.ForwardScan())
			existing, err := rl.AsList(ctx, cursor)
			if err != nil {
				return nil, err
			}
			if len(existing) > 0 {
				continue // already ingested (from a previous partial commit)
			}

			if _, err := store.SaveRecord(evt.record); err != nil {
				return nil, fmt.Errorf("save event: %w", err)
			}

			// Also send to dynamic meter engine
			if c.meterEngine != nil {
				_ = c.meterEngine.IngestEvent(ctx,
					evt.record.GetMeterSlug(),
					evt.record.GetCustomerId(),
					evt.record.GetTimestampBucket(),
					evt.record.GetValue(),
					evt.group)
			}
		}

		// Write dead letters
		for _, dl := range deadLetters {
			if _, err := store.SaveRecord(dl); err != nil {
				return nil, fmt.Errorf("save dead letter: %w", err)
			}
		}

		// Write the new offset — this is the key to exactly-once
		offsetRecord := &storev1.KafkaOffset{
			Topic:     proto.String(p.Topic),
			Partition: proto.Int32(p.Partition),
			Offset:    proto.Int64(newOffset),
			UpdatedAt: proto.Int64(now),
		}
		if _, err := store.SaveRecord(offsetRecord); err != nil {
			return nil, fmt.Errorf("save offset: %w", err)
		}

		return nil, nil
	})
	if err != nil {
		return fmt.Errorf("fdb transaction: %w", err)
	}

	// Update lag tracking
	c.mu.Lock()
	c.committedAt[p.Partition] = newOffset
	if hwm := p.Records[len(p.Records)-1].Offset + 1; hwm > c.highWaterMark[p.Partition] {
		c.highWaterMark[p.Partition] = hwm
	}
	c.lastCommitTime[p.Partition] = time.Now()
	c.mu.Unlock()

	c.log.Debug("committed batch",
		"topic", p.Topic, "partition", p.Partition,
		"events", len(events), "new_offset", newOffset)

	return nil
}

// PartitionLag holds lag info for one partition.
type PartitionLag struct {
	Partition      int32     `json:"partition"`
	CommittedAt    int64     `json:"committed_offset"`
	HighWaterMark  int64     `json:"high_water_mark"`
	Lag            int64     `json:"lag"`
	LastCommitTime time.Time `json:"last_commit_time"`
}

// GetLag returns consumer lag per partition.
func (c *Consumer) GetLag() []PartitionLag {
	c.mu.Lock()
	defer c.mu.Unlock()

	var lags []PartitionLag
	for p, committed := range c.committedAt {
		hwm := c.highWaterMark[p]
		lags = append(lags, PartitionLag{
			Partition:      p,
			CommittedAt:    committed,
			HighWaterMark:  hwm,
			Lag:            hwm - committed,
			LastCommitTime: c.lastCommitTime[p],
		})
	}
	return lags
}

// KafkaEvent is the JSON schema for events on the Kafka topic.
type KafkaEvent struct {
	CustomerID     string            `json:"customer_id"`
	EventType      string            `json:"event_type"`
	TimestampMs    int64             `json:"timestamp_ms"`
	Value          int64             `json:"value"`
	IdempotencyKey string            `json:"idempotency_key"`
	Properties     map[string]string `json:"properties,omitempty"`
}

func parseKafkaRecord(record *kgo.Record, now int64) (*storev1.UsageEvent, map[string]string, error) {
	var evt KafkaEvent
	if err := json.Unmarshal(record.Value, &evt); err != nil {
		return nil, nil, fmt.Errorf("unmarshal: %w", err)
	}

	if evt.IdempotencyKey == "" {
		return nil, nil, fmt.Errorf("missing idempotency_key")
	}
	if evt.CustomerID == "" {
		return nil, nil, fmt.Errorf("missing customer_id")
	}

	ts := evt.TimestampMs
	if ts == 0 {
		ts = now
	}

	// Generate a random ID — not the idempotency key (that's for dedup)
	id := fmt.Sprintf("kafka_%d_%d", record.Partition, record.Offset)

	meterSlug := evt.EventType

	return &storev1.UsageEvent{
		Id:              proto.String(id),
		CustomerId:      proto.String(evt.CustomerID),
		EventType:       proto.String(evt.EventType),
		MeterSlug:       proto.String(meterSlug),
		TimestampMs:     proto.Int64(ts),
		Value:           proto.Int64(evt.Value),
		IdempotencyKey:  proto.String(evt.IdempotencyKey),
		PropertiesJson:  proto.String(""), // raw properties stored separately
		IngestedAt:      proto.Int64(now),
		TimestampBucket: proto.Int64(billing.BucketHour(ts)),
	}, evt.Properties, nil
}
