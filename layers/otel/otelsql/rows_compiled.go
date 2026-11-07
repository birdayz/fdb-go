package otelsql

import (
	"crypto/sha256"
	"fmt"

	"fdb.dev/layers/otel/otelsql/otelv1pb"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

// BuildCompiled is SpanRows.Build as the compiled otelv1pb.SPANS, for a store
// opened with OpenTenantStoreCompiled: the same columns, the same presence
// (a non-empty string or bytes column, every integer column, an array's
// wrapper once it has an element) and the same arrays, without dynamic
// messages.
func BuildCompiled(s Span) (*otelv1pb.SPANS, error) {
	sp := s.Span
	if sp == nil {
		return nil, fmt.Errorf("otelsql: nil span")
	}
	r := &otelv1pb.SPANS{
		TRACE_ID:            nonEmptyBytes(sp.TraceId),
		SPAN_ID:             nonEmptyBytes(sp.SpanId),
		PARENT_SPAN_ID:      nonEmptyBytes(sp.ParentSpanId),
		TRACE_STATE:         nonEmptyString(sp.TraceState),
		FLAGS:               proto.Int64(int64(sp.Flags)),
		NAME:                nonEmptyString(sp.Name),
		KIND:                proto.Int32(int32(sp.Kind)),
		START_NS:            proto.Int64(int64(sp.StartTimeUnixNano)), //nolint:gosec // as Build
		END_NS:              proto.Int64(int64(sp.EndTimeUnixNano)),   //nolint:gosec // as Build
		RESOURCE_SCHEMA_URL: nonEmptyString(s.ResourceSchemaURL),
		SCOPE_SCHEMA_URL:    nonEmptyString(s.ScopeSchemaURL),
		DROPPED_ATTRIBUTES:  proto.Int64(int64(sp.DroppedAttributesCount)),
		DROPPED_EVENTS:      proto.Int64(int64(sp.DroppedEventsCount)),
		DROPPED_LINKS:       proto.Int64(int64(sp.DroppedLinksCount)),
		OTLP:                nonEmptyBytes(s.OTLP),
	}
	if st := sp.Status; st != nil {
		r.STATUS_CODE = proto.Int32(int32(st.Code))
		r.STATUS_MESSAGE = nonEmptyString(st.Message)
	}
	if sc := s.Scope; sc != nil {
		r.SCOPE_NAME = nonEmptyString(sc.Name)
		r.SCOPE_VERSION = nonEmptyString(sc.Version)
	}
	attrs := compiledAttrs{
		str: &r.STR_ATTRS, ints: &r.INT_ATTRS, doubles: &r.DOUBLE_ATTRS, bools: &r.BOOL_ATTRS, anys: &r.ANY_ATTRS,
		longStr: &r.LONG_STR_ATTRS, bytes: &r.BYTES_ATTRS,
	}
	if res := s.Resource; res != nil {
		for _, kv := range res.Attributes {
			if kv.GetKey() == "service.name" {
				if v, ok := kv.GetValue().GetValue().(*commonpb.AnyValue_StringValue); ok && v.StringValue != "" {
					r.SERVICE_NAME = proto.String(v.StringValue)
				}
			}
		}
		if err := attrs.add(ScopeResource, res.Attributes); err != nil {
			return nil, err
		}
	}
	if sc := s.Scope; sc != nil {
		if err := attrs.add(ScopeScope, sc.Attributes); err != nil {
			return nil, err
		}
	}
	if err := attrs.add(ScopeSpan, sp.Attributes); err != nil {
		return nil, err
	}
	for i, ev := range sp.Events {
		e := &otelv1pb.SPAN_EVENT{
			ORDINAL:            proto.Int64(int64(i)),
			NAME:               nonEmptyString(ev.Name),
			TIME_NS:            proto.Int64(int64(ev.TimeUnixNano)), //nolint:gosec // as Build
			DROPPED_ATTRIBUTES: proto.Int64(int64(ev.DroppedAttributesCount)),
		}
		ea := compiledAttrs{str: &e.STR_ATTRS, ints: &e.INT_ATTRS, doubles: &e.DOUBLE_ATTRS, bools: &e.BOOL_ATTRS, anys: &e.ANY_ATTRS}
		if err := ea.add(ScopeEvent, ev.Attributes); err != nil {
			return nil, err
		}
		if r.EVENTS == nil {
			r.EVENTS = &otelv1pb.SPANS_EVENTS{}
		}
		r.EVENTS.Values = append(r.EVENTS.Values, e)
	}
	for i, ln := range sp.Links {
		l := &otelv1pb.SPAN_LINK{
			ORDINAL:            proto.Int64(int64(i)),
			TRACE_ID:           nonEmptyBytes(ln.TraceId),
			SPAN_ID:            nonEmptyBytes(ln.SpanId),
			TRACE_STATE:        nonEmptyString(ln.TraceState),
			FLAGS:              proto.Int64(int64(ln.Flags)),
			DROPPED_ATTRIBUTES: proto.Int64(int64(ln.DroppedAttributesCount)),
		}
		la := compiledAttrs{str: &l.STR_ATTRS, ints: &l.INT_ATTRS, doubles: &l.DOUBLE_ATTRS, bools: &l.BOOL_ATTRS, anys: &l.ANY_ATTRS}
		if err := la.add(ScopeLink, ln.Attributes); err != nil {
			return nil, err
		}
		if r.LINKS == nil {
			r.LINKS = &otelv1pb.SPANS_LINKS{}
		}
		r.LINKS.Values = append(r.LINKS.Values, l)
	}
	return r, nil
}

// compiledAttrs are a table's or struct's typed attribute arrays; longStr and
// bytes are nil for event and link structs, which project neither.
type compiledAttrs struct {
	str     **otelv1pb.SPANS_STR_ATTRS
	ints    **otelv1pb.SPANS_INT_ATTRS
	doubles **otelv1pb.SPANS_DOUBLE_ATTRS
	bools   **otelv1pb.SPANS_BOOL_ATTRS
	anys    **otelv1pb.SPANS_ANY_ATTRS
	longStr **otelv1pb.SPANS_LONG_STR_ATTRS
	bytes   **otelv1pb.SPANS_BYTES_ATTRS
}

// add is attrArrays.add.
func (a compiledAttrs) add(scope string, kvs []*commonpb.KeyValue) error {
	for _, kv := range kvs {
		key := kv.GetKey()
		if len(key) > MaxIndexedStringBytes {
			continue
		}
		switch v := kv.GetValue().GetValue().(type) {
		case *commonpb.AnyValue_StringValue:
			if len(v.StringValue) <= MaxIndexedStringBytes {
				if *a.str == nil {
					*a.str = &otelv1pb.SPANS_STR_ATTRS{}
				}
				(*a.str).Values = append((*a.str).Values, &otelv1pb.STR_ATTR{SCOPE: proto.String(scope), ATTR_KEY: proto.String(key), STR_VALUE: proto.String(v.StringValue)})
			} else if a.longStr != nil {
				sum := sha256.Sum256([]byte(v.StringValue))
				if *a.longStr == nil {
					*a.longStr = &otelv1pb.SPANS_LONG_STR_ATTRS{}
				}
				(*a.longStr).Values = append((*a.longStr).Values, &otelv1pb.LONG_STR_ATTR{
					SCOPE: proto.String(scope), ATTR_KEY: proto.String(key), DIGEST: sum[:LongStringDigestBytes], STR_VALUE: proto.String(v.StringValue),
				})
			}
		case *commonpb.AnyValue_IntValue:
			if *a.ints == nil {
				*a.ints = &otelv1pb.SPANS_INT_ATTRS{}
			}
			(*a.ints).Values = append((*a.ints).Values, &otelv1pb.INT_ATTR{SCOPE: proto.String(scope), ATTR_KEY: proto.String(key), INT_VALUE: proto.Int64(v.IntValue)})
		case *commonpb.AnyValue_DoubleValue:
			if *a.doubles == nil {
				*a.doubles = &otelv1pb.SPANS_DOUBLE_ATTRS{}
			}
			(*a.doubles).Values = append((*a.doubles).Values, &otelv1pb.DOUBLE_ATTR{SCOPE: proto.String(scope), ATTR_KEY: proto.String(key), DOUBLE_VALUE: proto.Float64(v.DoubleValue)})
		case *commonpb.AnyValue_BoolValue:
			if *a.bools == nil {
				*a.bools = &otelv1pb.SPANS_BOOL_ATTRS{}
			}
			(*a.bools).Values = append((*a.bools).Values, &otelv1pb.BOOL_ATTR{SCOPE: proto.String(scope), ATTR_KEY: proto.String(key), BOOL_VALUE: proto.Bool(v.BoolValue)})
		case *commonpb.AnyValue_BytesValue:
			if a.bytes != nil {
				if *a.bytes == nil {
					*a.bytes = &otelv1pb.SPANS_BYTES_ATTRS{}
				}
				(*a.bytes).Values = append((*a.bytes).Values, &otelv1pb.BYTES_ATTR{SCOPE: proto.String(scope), ATTR_KEY: proto.String(key), BYTES_VALUE: presentBytes(v.BytesValue)})
			}
		case *commonpb.AnyValue_ArrayValue, *commonpb.AnyValue_KvlistValue:
			j, err := protojson.Marshal(kv.GetValue())
			if err != nil {
				return fmt.Errorf("otelsql: attribute %q JSON: %w", key, err)
			}
			if *a.anys == nil {
				*a.anys = &otelv1pb.SPANS_ANY_ATTRS{}
			}
			(*a.anys).Values = append((*a.anys).Values, &otelv1pb.ANY_ATTR{SCOPE: proto.String(scope), ATTR_KEY: proto.String(key), JSON_VALUE: proto.String(string(j))})
		}
	}
	return nil
}

func nonEmptyString(s string) *string {
	if s == "" {
		return nil
	}
	return proto.String(s)
}

// presentBytes is b as a present proto2 bytes value: an attribute's empty
// value is stored (as Build's Set stores it), and a nil slice would be absent.
func presentBytes(b []byte) []byte {
	if b == nil {
		return []byte{}
	}
	return b
}

func nonEmptyBytes(b []byte) []byte {
	if len(b) == 0 {
		return nil
	}
	return b
}
