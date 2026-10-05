package executor

import (
	"context"
	"fmt"

	"fdb.dev/pkg/recordlayer"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/dynamicpb"
)

// entryBinder binds the scanned index entry under the current correlation,
// over the statement's own bindings
// (RecordQueryPlanWithIndexEntryToQueriedRecord binds it under
// Quantifier.current()). One per cursor, rebound per entry.
type entryBinder struct {
	outer values.CorrelationBinder
	entry *recordlayer.IndexEntry
}

func (b *entryBinder) GetCorrelationBinding(id values.CorrelationIdentifier) (any, bool) {
	if id == values.CurrentCorrelation() {
		return b.entry, b.entry != nil
	}
	if b.outer == nil {
		return nil, false
	}
	return b.outer.GetCorrelationBinding(id)
}

// coveringEntryReader builds the queried record's logical row from an index
// entry with the plan's entry reader: each top-level field of the reader is
// evaluated into its slot; a nested record is a message of the stored nested
// descriptor holding the covered fields, as a base scan keeps nested messages.
type coveringEntryReader struct {
	reader      *values.RecordConstructorValue
	desc        protoreflect.MessageDescriptor
	logicalType *values.RecordType
	layout      values.OrdinalLayout
}

func newCoveringEntryReader(
	reader *values.RecordConstructorValue,
	desc protoreflect.MessageDescriptor,
	logicalType *values.RecordType,
	layout values.OrdinalLayout,
) (*coveringEntryReader, error) {
	if reader == nil || desc == nil || logicalType == nil {
		return nil, fmt.Errorf("executor: covering scan has no entry reader for its record type")
	}
	if len(reader.Fields) != len(logicalType.Fields) {
		return nil, fmt.Errorf("executor: covering entry reader has %d fields, the record's row %d",
			len(reader.Fields), len(logicalType.Fields))
	}
	return &coveringEntryReader{reader: reader, desc: desc, logicalType: logicalType, layout: layout}, nil
}

func (r *coveringEntryReader) row(binder *entryBinder) (*PositionalRow, error) {
	slots := make([]any, len(r.reader.Fields))
	for i, field := range r.reader.Fields {
		if nested, ok := field.Value.(*values.RecordConstructorValue); ok {
			if i >= r.desc.Fields().Len() || r.desc.Fields().Get(i).Message() == nil {
				return nil, fmt.Errorf("executor: covering field %s is no message field", field.Name)
			}
			msg, err := fillCoveredMessage(r.desc.Fields().Get(i).Message(), nested, binder)
			if err != nil {
				return nil, err
			}
			slots[i] = msg.Interface()
			continue
		}
		value, err := field.Value.Evaluate(binder)
		if err != nil {
			return nil, fmt.Errorf("executor: covering field %s: %w", field.Name, err)
		}
		slots[i] = value
	}
	return &PositionalRow{Type: r.logicalType, Slots: slots, Layout: r.layout}, nil
}

// fillCoveredMessage is Java's MessageCopier: the covered fields of a nested
// record set by position on a message of its stored descriptor; an uncovered
// field stays unset.
func fillCoveredMessage(
	md protoreflect.MessageDescriptor,
	record *values.RecordConstructorValue,
	binder *entryBinder,
) (protoreflect.Message, error) {
	if md.Fields().Len() != len(record.Fields) {
		return nil, fmt.Errorf("executor: covered record %s has %d fields, its descriptor %d",
			md.FullName(), len(record.Fields), md.Fields().Len())
	}
	msg := dynamicpb.NewMessage(md)
	for i, field := range record.Fields {
		fd := md.Fields().Get(i)
		if nested, ok := field.Value.(*values.RecordConstructorValue); ok {
			if fd.Message() == nil {
				return nil, fmt.Errorf("executor: covered field %s is no message field", fd.FullName())
			}
			child, err := fillCoveredMessage(fd.Message(), nested, binder)
			if err != nil {
				return nil, err
			}
			msg.Set(fd, protoreflect.ValueOfMessage(child))
			continue
		}
		value, err := field.Value.Evaluate(binder)
		if err != nil {
			return nil, fmt.Errorf("executor: covered field %s: %w", fd.FullName(), err)
		}
		if value == nil {
			continue
		}
		if fd.IsList() {
			if elems, ok := value.([]any); ok && len(elems) == 0 {
				continue
			}
		}
		pv, err := goToProtoScalarValue(fd, value)
		if err != nil {
			return nil, fmt.Errorf("executor: covered field %s: %w", fd.FullName(), err)
		}
		msg.Set(fd, pv)
	}
	return msg, nil
}

// coveringIndexCursor maps each scanned entry through the covering plan's
// entry reader.
type coveringIndexCursor struct {
	inner  recordlayer.RecordCursor[*recordlayer.IndexEntry]
	reader *coveringEntryReader
	binder entryBinder
	closed bool
	// lastNoNext replays the terminal result on a contract-violating re-call.
	lastNoNext *recordlayer.RecordCursorResult[QueryResult]
}

func (c *coveringIndexCursor) OnNext(ctx context.Context) (recordlayer.RecordCursorResult[QueryResult], error) {
	if c.lastNoNext != nil {
		return *c.lastNoNext, nil
	}
	result, err := c.inner.OnNext(ctx)
	if err != nil {
		return recordlayer.NewResultNoNext[QueryResult](recordlayer.SourceExhausted, &recordlayer.EndContinuation{}), err
	}
	if !result.HasNext() {
		res := recordlayer.NewResultNoNext[QueryResult](result.GetNoNextReason(), result.GetContinuation())
		c.lastNoNext = &res
		return res, nil
	}
	entry := result.GetValue()
	c.binder.entry = entry
	pos, err := c.reader.row(&c.binder)
	c.binder.entry = nil
	if err != nil {
		return recordlayer.NewResultNoNext[QueryResult](recordlayer.SourceExhausted, &recordlayer.EndContinuation{}), err
	}
	return recordlayer.NewResultWithValue(
		QueryResult{Positional: pos, PrimaryKey: entry.PrimaryKey()},
		result.GetContinuation(),
	), nil
}

func (c *coveringIndexCursor) Close() error {
	c.closed = true
	return c.inner.Close()
}

func (c *coveringIndexCursor) IsClosed() bool { return c.closed }

var _ recordlayer.RecordCursor[QueryResult] = (*coveringIndexCursor)(nil)
