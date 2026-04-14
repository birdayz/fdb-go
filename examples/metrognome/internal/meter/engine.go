// Package meter provides dynamic meter management — runtime proto generation
// from user-defined meter configurations.
//
// When a user creates a meter with group_by properties, we dynamically:
// 1. Build a FileDescriptorProto with a message type matching the meter schema
// 2. Register it in the global proto registry
// 3. Create a Record Layer metadata with SUM/COUNT indexes on the grouped fields
// 4. Manage a per-meter FDB store in its own subspace
//
// This lets users define arbitrary group-by dimensions without static proto schemas.
package meter

import (
	"context"
	"fmt"
	"strconv"
	"sync"
	"sync/atomic"

	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/fdb/subspace"
	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/fdb/tuple"
	rl "github.com/birdayz/fdb-record-layer-go/pkg/recordlayer"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"
	"google.golang.org/protobuf/types/descriptorpb"
	"google.golang.org/protobuf/types/dynamicpb"

	storev1 "github.com/birdayz/fdb-record-layer-go/examples/metrognome/gen/metrognome/store/v1"
)

// Engine manages dynamic per-meter stores backed by runtime-generated protos.
type Engine struct {
	fdb *rl.FDBDatabase
	ss  subspace.Subspace // parent subspace; each meter gets ss.Sub(slug)

	mu     sync.RWMutex
	meters map[string]*meterRuntime // slug → runtime
}

// meterRuntime holds the compiled state for one meter.
type meterRuntime struct {
	config   *storev1.Meter
	metadata *rl.RecordMetaData
	msgDesc  protoreflect.MessageDescriptor
	ss       subspace.Subspace
}

// NewEngine creates a meter engine.
func NewEngine(fdb *rl.FDBDatabase, ss subspace.Subspace) *Engine {
	return &Engine{
		fdb:    fdb,
		ss:     ss,
		meters: make(map[string]*meterRuntime),
	}
}

// Register compiles a meter config into a dynamic proto + Record Layer store.
// Safe to call multiple times for the same slug (idempotent).
func (e *Engine) Register(m *storev1.Meter) error {
	e.mu.Lock()
	defer e.mu.Unlock()

	slug := m.GetSlug()
	if _, ok := e.meters[slug]; ok {
		return nil // already registered
	}

	rt, err := compileMeter(m, e.fdb, e.ss)
	if err != nil {
		return fmt.Errorf("compile meter %s: %w", slug, err)
	}

	// Pre-create the store so IngestBatch can use Open() instead of CreateOrOpen()
	_, err = e.fdb.Run(context.Background(), func(rtx *rl.FDBRecordContext) (any, error) {
		_, err := rl.NewStoreBuilder().
			SetContext(rtx).
			SetMetaDataProvider(rt.metadata).
			SetSubspace(rt.ss).
			CreateOrOpen()
		return nil, err
	})
	if err != nil {
		return fmt.Errorf("create store for meter %s: %w", slug, err)
	}

	e.meters[slug] = rt
	return nil
}

// IngestEvent saves a usage event into the meter's dynamic store.
// The event properties are mapped to the dynamic proto fields.
func (e *Engine) IngestEvent(ctx context.Context, slug string, customerID string, timestampBucket int64, value int64, groupValues map[string]string) error {
	e.mu.RLock()
	rt, ok := e.meters[slug]
	e.mu.RUnlock()
	if !ok {
		return fmt.Errorf("meter %q not registered", slug)
	}

	msg := dynamicpb.NewMessage(rt.msgDesc)

	// Set fixed fields
	setField(msg, rt.msgDesc, "event_id", protoreflect.ValueOfString(fastID()))
	setField(msg, rt.msgDesc, "customer_id", protoreflect.ValueOfString(customerID))
	setField(msg, rt.msgDesc, "timestamp_bucket", protoreflect.ValueOfInt64(timestampBucket))
	setField(msg, rt.msgDesc, "value", protoreflect.ValueOfInt64(value))

	// Set dynamic group-by fields
	for k, v := range groupValues {
		fd := rt.msgDesc.Fields().ByName(protoreflect.Name(k))
		if fd == nil {
			continue // ignore unknown fields
		}
		msg.Set(fd, protoreflect.ValueOfString(v))
	}

	_, err := e.fdb.Run(ctx, func(rtx *rl.FDBRecordContext) (any, error) {
		store, err := rl.NewStoreBuilder().
			SetContext(rtx).
			SetMetaDataProvider(rt.metadata).
			SetSubspace(rt.ss).
			CreateOrOpen()
		if err != nil {
			return nil, err
		}
		_, err = store.SaveRecord(msg)
		return nil, err
	})
	return err
}

// IngestBatch saves multiple events in a single FDB transaction for throughput.
func (e *Engine) IngestBatch(ctx context.Context, slug string, events []BatchEvent) error {
	e.mu.RLock()
	rt, ok := e.meters[slug]
	e.mu.RUnlock()
	if !ok {
		return fmt.Errorf("meter %q not registered", slug)
	}

	// Pre-resolve field descriptors (avoid per-event map lookup)
	fdEventID := rt.msgDesc.Fields().ByName("event_id")
	fdCustomerID := rt.msgDesc.Fields().ByName("customer_id")
	fdBucket := rt.msgDesc.Fields().ByName("timestamp_bucket")
	fdValue := rt.msgDesc.Fields().ByName("value")

	type groupFD struct {
		name string
		fd   protoreflect.FieldDescriptor
	}
	var groupFDs []groupFD
	for _, prop := range rt.config.GetGroupByProperties() {
		fd := rt.msgDesc.Fields().ByName(protoreflect.Name(prop))
		if fd != nil {
			groupFDs = append(groupFDs, groupFD{prop, fd})
		}
	}

	_, err := e.fdb.Run(ctx, func(rtx *rl.FDBRecordContext) (any, error) {
		store, err := rl.NewStoreBuilder().
			SetContext(rtx).
			SetMetaDataProvider(rt.metadata).
			SetSubspace(rt.ss).
			Open()
		if err != nil {
			return nil, err
		}
		msgs := make([]proto.Message, len(events))
		for i := range events {
			msg := dynamicpb.NewMessage(rt.msgDesc)
			msg.Set(fdEventID, protoreflect.ValueOfString(fastID()))
			msg.Set(fdCustomerID, protoreflect.ValueOfString(events[i].CustomerID))
			msg.Set(fdBucket, protoreflect.ValueOfInt64(events[i].TimestampBucket))
			msg.Set(fdValue, protoreflect.ValueOfInt64(events[i].Value))
			for _, gfd := range groupFDs {
				if v, ok := events[i].GroupValues[gfd.name]; ok {
					msg.Set(gfd.fd, protoreflect.ValueOfString(v))
				}
			}
			msgs[i] = msg
		}
		_, err = store.SaveRecordBatch(msgs)
		return nil, err
	})
	return err
}

// BatchEvent is a single event for IngestBatch.
type BatchEvent struct {
	CustomerID      string
	TimestampBucket int64
	Value           int64
	GroupValues     map[string]string
}

// GetUsage queries the SUM aggregate for a meter, optionally filtered by group values.
func (e *Engine) GetUsage(ctx context.Context, slug string, customerID string, startBucket, endBucket int64, groupFilter map[string]string) (int64, error) {
	e.mu.RLock()
	rt, ok := e.meters[slug]
	e.mu.RUnlock()
	if !ok {
		return 0, fmt.Errorf("meter %q not registered", slug)
	}

	// Build the scan range for the SUM index.
	// SUM index key: [customer_id, group1, group2, ..., timestamp_bucket]
	// If all group values are provided, we can do an exact prefix + bucket range.
	// If group values are missing, we scan all groups for the customer and sum.
	allGroupsProvided := true
	prefix := tuple.Tuple{customerID}
	for _, prop := range rt.config.GetGroupByProperties() {
		if v, ok := groupFilter[prop]; ok {
			prefix = append(prefix, v)
		} else {
			allGroupsProvided = false
			break
		}
	}

	var scanRange rl.TupleRange
	if allGroupsProvided {
		// Exact group prefix + bucket range
		rangeStart := append(append(tuple.Tuple{}, prefix...), startBucket)
		rangeEnd := append(append(tuple.Tuple{}, prefix...), endBucket)
		scanRange = rl.TupleRangeBetweenInclusive(rangeStart, rangeEnd)
	} else {
		// Scan all groups for this customer — EvaluateAggregateFunction
		// will sum across all matching entries
		scanRange = rl.TupleRangeAllOf(prefix)
	}

	result, err := e.fdb.Run(ctx, func(rtx *rl.FDBRecordContext) (any, error) {
		store, err := rl.NewStoreBuilder().
			SetContext(rtx).
			SetMetaDataProvider(rt.metadata).
			SetSubspace(rt.ss).
			CreateOrOpen()
		if err != nil {
			return nil, err
		}

		aggResult, err := store.EvaluateAggregateFunction(ctx,
			[]string{"Event"},
			rl.NewSumAggregateFunction(buildGroupBy(rt.config)),
			scanRange,
			rl.IsolationLevelSnapshot)
		if err != nil {
			return nil, err
		}
		if len(aggResult) == 0 {
			return int64(0), nil
		}
		return aggResult[0], nil
	})
	if err != nil {
		return 0, err
	}
	return result.(int64), nil
}

// GetUsageBuckets returns per-bucket usage values by scanning the SUM index.
func (e *Engine) GetUsageBuckets(ctx context.Context, slug string, customerID string, startBucket, endBucket int64, groupFilter map[string]string) (map[int64]int64, error) {
	e.mu.RLock()
	rt, ok := e.meters[slug]
	e.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("meter %q not registered", slug)
	}

	// Build scan range (same logic as GetUsage)
	allGroupsProvided := true
	prefix := tuple.Tuple{customerID}
	for _, prop := range rt.config.GetGroupByProperties() {
		if v, ok := groupFilter[prop]; ok {
			prefix = append(prefix, v)
		} else {
			allGroupsProvided = false
			break
		}
	}

	var scanRange rl.TupleRange
	if allGroupsProvided {
		rangeStart := append(append(tuple.Tuple{}, prefix...), startBucket)
		rangeEnd := append(append(tuple.Tuple{}, prefix...), endBucket)
		scanRange = rl.TupleRangeBetweenInclusive(rangeStart, rangeEnd)
	} else {
		scanRange = rl.TupleRangeAllOf(prefix)
	}

	result, err := e.fdb.Run(ctx, func(rtx *rl.FDBRecordContext) (any, error) {
		store, err := rl.NewStoreBuilder().
			SetContext(rtx).
			SetMetaDataProvider(rt.metadata).
			SetSubspace(rt.ss).
			CreateOrOpen()
		if err != nil {
			return nil, err
		}

		idx := store.GetRecordMetaData().GetIndex("usage_sum")
		cursor := store.ScanIndex(idx, scanRange, nil, rl.ForwardScan())
		entries, err := rl.AsList(ctx, cursor)
		if err != nil {
			return nil, err
		}

		// Index key: [customer_id, group_by..., timestamp_bucket]
		// The bucket is the last element in the key
		buckets := make(map[int64]int64, len(entries))
		for _, e := range entries {
			bucketIdx := len(e.Key) - 1
			bucket := e.Key[bucketIdx].(int64)
			val := e.Value[0].(int64)
			buckets[bucket] += val
		}
		return buckets, nil
	})
	if err != nil {
		return nil, err
	}
	return result.(map[int64]int64), nil
}

// UsageGroupEntry holds usage for one combination of group-by values.
type UsageGroupEntry struct {
	GroupValues map[string]string
	Value       int64
}

// GetUsageGroups returns usage broken down by group-by property values.
// Scans all SUM index entries for the customer, aggregates by unique group combinations.
func (e *Engine) GetUsageGroups(ctx context.Context, slug string, customerID string, startBucket, endBucket int64) ([]UsageGroupEntry, error) {
	e.mu.RLock()
	rt, ok := e.meters[slug]
	e.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("meter %q not registered", slug)
	}

	if len(rt.config.GetGroupByProperties()) == 0 {
		return nil, fmt.Errorf("meter %q has no group-by properties", slug)
	}

	result, err := e.fdb.Run(ctx, func(rtx *rl.FDBRecordContext) (any, error) {
		store, err := rl.NewStoreBuilder().
			SetContext(rtx).
			SetMetaDataProvider(rt.metadata).
			SetSubspace(rt.ss).
			CreateOrOpen()
		if err != nil {
			return nil, err
		}

		idx := store.GetRecordMetaData().GetIndex("usage_sum")
		cursor := store.ScanIndex(idx,
			rl.TupleRangeAllOf(tuple.Tuple{customerID}),
			nil, rl.ForwardScan())
		entries, err := rl.AsList(ctx, cursor)
		if err != nil {
			return nil, err
		}

		// Index key: [customer_id, group1, group2, ..., timestamp_bucket]
		// Group by the group values (everything between customer_id and bucket)
		groupProps := rt.config.GetGroupByProperties()
		type groupKey string // serialized group values
		groups := make(map[groupKey]*UsageGroupEntry)

		for _, e := range entries {
			// Extract group values from the key
			gv := make(map[string]string, len(groupProps))
			for i, prop := range groupProps {
				if i+1 < len(e.Key)-1 { // skip customer_id (0) and bucket (last)
					gv[prop] = fmt.Sprintf("%v", e.Key[i+1])
				}
			}

			// Build a stable key for grouping
			var gk string
			for _, prop := range groupProps {
				gk += prop + "=" + gv[prop] + ";"
			}

			val := e.Value[0].(int64)
			if existing, ok := groups[groupKey(gk)]; ok {
				existing.Value += val
			} else {
				groups[groupKey(gk)] = &UsageGroupEntry{
					GroupValues: gv,
					Value:       val,
				}
			}
		}

		result := make([]UsageGroupEntry, 0, len(groups))
		for _, g := range groups {
			result = append(result, *g)
		}
		return result, nil
	})
	if err != nil {
		return nil, err
	}
	return result.([]UsageGroupEntry), nil
}

// compileMeter builds the dynamic proto, registers it, and creates the metadata.
func compileMeter(m *storev1.Meter, fdb *rl.FDBDatabase, parentSS subspace.Subspace) (*meterRuntime, error) {
	slug := m.GetSlug()
	packageName := fmt.Sprintf("metrognome.dynamic.%s", slug)
	fileName := fmt.Sprintf("metrognome/dynamic/%s.proto", slug)

	// Build the event message fields
	fieldNum := int32(1)
	fields := []*descriptorpb.FieldDescriptorProto{
		stringField("event_id", fieldNum),
	}
	fieldNum++
	fields = append(fields, stringField("customer_id", fieldNum))
	fieldNum++

	// Group-by properties become string fields
	for _, prop := range m.GetGroupByProperties() {
		fields = append(fields, stringField(prop, fieldNum))
		fieldNum++
	}

	// Timestamp bucket
	fields = append(fields, int64Field("timestamp_bucket", fieldNum))
	fieldNum++

	// Value (the aggregated field)
	fields = append(fields, int64Field("value", fieldNum))

	// Build UnionDescriptor (required by Record Layer)
	unionField := &descriptorpb.FieldDescriptorProto{
		Name:     proto.String("_Event"),
		Number:   proto.Int32(1),
		Label:    descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(),
		Type:     descriptorpb.FieldDescriptorProto_TYPE_MESSAGE.Enum(),
		TypeName: proto.String("Event"),
	}

	fdp := &descriptorpb.FileDescriptorProto{
		Name:    proto.String(fileName),
		Package: proto.String(packageName),
		Syntax:  proto.String("proto2"),
		MessageType: []*descriptorpb.DescriptorProto{
			{
				Name:  proto.String("Event"),
				Field: fields,
			},
			{
				Name:  proto.String("UnionDescriptor"),
				Field: []*descriptorpb.FieldDescriptorProto{unionField},
			},
		},
	}

	// Build the file descriptor
	fd, err := protodesc.NewFile(fdp, nil)
	if err != nil {
		return nil, fmt.Errorf("build file descriptor: %w", err)
	}

	// Register the dynamic message type in the global registry.
	// Use recover to handle the panic from duplicate registration
	// (proto v2 panics on conflict instead of returning an error).
	eventDesc := fd.Messages().ByName("Event")
	msgType := dynamicpb.NewMessageType(eventDesc)
	safeRegister(msgType)

	unionDesc := fd.Messages().ByName("UnionDescriptor")
	unionMsgType := dynamicpb.NewMessageType(unionDesc)
	safeRegister(unionMsgType)

	// Build Record Layer metadata
	builder := rl.NewRecordMetaDataBuilder().SetRecords(fd)

	// Primary key: RecordTypeKey + event_id (unique per event, prevents overwrites)
	builder.GetRecordType("Event").SetPrimaryKey(
		rl.Concat(rl.RecordTypeKey(), rl.Field("event_id")))

	// SUM index: GroupBy(value, customer_id, group_by..., timestamp_bucket)
	builder.AddIndex("Event", rl.NewSumIndex("usage_sum", buildGroupBy(m)))

	// COUNT index
	countGroupParts := []rl.KeyExpression{rl.Field("customer_id")}
	for _, prop := range m.GetGroupByProperties() {
		countGroupParts = append(countGroupParts, rl.Field(prop))
	}
	countGroupParts = append(countGroupParts, rl.Field("timestamp_bucket"))
	builder.AddIndex("Event", rl.NewCountIndex("usage_count",
		rl.GroupBy(rl.EmptyKey(), countGroupParts...)))

	md, err := builder.Build()
	if err != nil {
		return nil, fmt.Errorf("build metadata: %w", err)
	}

	return &meterRuntime{
		config:   m,
		metadata: md,
		msgDesc:  eventDesc,
		ss:       parentSS.Sub("meter_" + slug),
	}, nil
}

// buildGroupBy creates a GroupBy expression: GroupBy(Field("value"), customer_id, group_by..., timestamp_bucket)
func buildGroupBy(m *storev1.Meter) rl.KeyExpression {
	groupParts := []rl.KeyExpression{rl.Field("customer_id")}
	for _, prop := range m.GetGroupByProperties() {
		groupParts = append(groupParts, rl.Field(prop))
	}
	groupParts = append(groupParts, rl.Field("timestamp_bucket"))
	return rl.GroupBy(rl.Field("value"), groupParts...)
}

func stringField(name string, number int32) *descriptorpb.FieldDescriptorProto {
	return &descriptorpb.FieldDescriptorProto{
		Name:   proto.String(name),
		Number: proto.Int32(number),
		Label:  descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(),
		Type:   descriptorpb.FieldDescriptorProto_TYPE_STRING.Enum(),
	}
}

func int64Field(name string, number int32) *descriptorpb.FieldDescriptorProto {
	return &descriptorpb.FieldDescriptorProto{
		Name:   proto.String(name),
		Number: proto.Int32(number),
		Label:  descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(),
		Type:   descriptorpb.FieldDescriptorProto_TYPE_INT64.Enum(),
	}
}

var idCounter atomic.Uint64

func fastID() string {
	return strconv.FormatUint(idCounter.Add(1), 36)
}

func safeRegister(mt protoreflect.MessageType) {
	defer func() { recover() }() // ignore panic from duplicate registration
	protoregistry.GlobalTypes.RegisterMessage(mt)
}

func setField(msg *dynamicpb.Message, desc protoreflect.MessageDescriptor, name string, val protoreflect.Value) {
	fd := desc.Fields().ByName(protoreflect.Name(name))
	if fd != nil {
		msg.Set(fd, val)
	}
}
