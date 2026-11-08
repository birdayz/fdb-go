package otelsql

import (
	"fdb.dev/layers/otel/otelsql/otelv1pb"
	"google.golang.org/protobuf/proto"
)

// BuildCompiled is SpanRows.Build as the compiled otelv1pb.SPANS, for a store
// opened with OpenTenantStoreCompiled or EnsureTenant: the same columns, the
// same presence (a non-empty string or bytes column, every integer column, an
// array's wrapper once it has an element) and the same arrays, without
// dynamic messages.
func BuildCompiled(s Span) (*otelv1pb.SPANS, error) {
	sp := s.Span
	if sp == nil {
		return nil, &InvalidRowError{Table: "SPANS", Reason: "nil span"}
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
	sets := spanSets(s)
	r.SERVICE_NAME = nonEmptyString(serviceName(sets[0]))
	attrs := attrLists{long: true}
	attrs.add(ScopeResource, sets[0])
	attrs.add(ScopeScope, sets[1])
	attrs.add(ScopeSpan, sets[2])
	if len(attrs.str) > 0 {
		r.STR_ATTRS = &otelv1pb.SPANS_STR_ATTRS{Values: attrs.str}
	}
	if len(attrs.longStr) > 0 {
		r.LONG_STR_ATTRS = &otelv1pb.SPANS_LONG_STR_ATTRS{Values: attrs.longStr}
	}
	if len(attrs.ints) > 0 {
		r.INT_ATTRS = &otelv1pb.SPANS_INT_ATTRS{Values: attrs.ints}
	}
	if len(attrs.doubles) > 0 {
		r.DOUBLE_ATTRS = &otelv1pb.SPANS_DOUBLE_ATTRS{Values: attrs.doubles}
	}
	if len(attrs.bools) > 0 {
		r.BOOL_ATTRS = &otelv1pb.SPANS_BOOL_ATTRS{Values: attrs.bools}
	}
	if len(attrs.bytes) > 0 {
		r.BYTES_ATTRS = &otelv1pb.SPANS_BYTES_ATTRS{Values: attrs.bytes}
	}
	if len(attrs.anys) > 0 {
		r.ANY_ATTRS = &otelv1pb.SPANS_ANY_ATTRS{Values: attrs.anys}
	}
	for i, ev := range sp.Events {
		e := &otelv1pb.SPAN_EVENT{
			ORDINAL:            proto.Int64(int64(i)),
			NAME:               nonEmptyString(ev.Name),
			TIME_NS:            proto.Int64(int64(ev.TimeUnixNano)), //nolint:gosec // as Build
			DROPPED_ATTRIBUTES: proto.Int64(int64(ev.DroppedAttributesCount)),
		}
		ea := attrLists{}
		ea.add(ScopeEvent, canonicalSet(ev.Attributes, s.ExcludeMessageAttributes))
		e.STR_ATTRS, e.INT_ATTRS, e.DOUBLE_ATTRS, e.BOOL_ATTRS, e.ANY_ATTRS = ea.structArrays()
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
		la := attrLists{}
		la.add(ScopeLink, canonicalSet(ln.Attributes, false))
		l.STR_ATTRS, l.INT_ATTRS, l.DOUBLE_ATTRS, l.BOOL_ATTRS, l.ANY_ATTRS = la.structArrays()
		if r.LINKS == nil {
			r.LINKS = &otelv1pb.SPANS_LINKS{}
		}
		r.LINKS.Values = append(r.LINKS.Values, l)
	}
	chunks, err := PackRefChunks(s.Refs)
	if err != nil {
		return nil, err
	}
	for i, c := range chunks {
		if r.REF_CHUNKS == nil {
			r.REF_CHUNKS = &otelv1pb.SPANS_REF_CHUNKS{}
		}
		r.REF_CHUNKS.Values = append(r.REF_CHUNKS.Values, &otelv1pb.MESSAGE_REF_CHUNK{CHUNK: proto.Int64(int64(i)), REFS: c})
	}
	return r, nil
}

// SpanProjection is what s's SPANS row leaves out of the equality indexes.
func SpanProjection(s Span) Projection {
	sets := spanSets(s)
	return projectionOf(sets[:]...)
}

// spanSets are the canonical resource, scope and span attribute sets of s.
func spanSets(s Span) [3]attrSet {
	return [3]attrSet{
		canonicalSet(s.Resource.GetAttributes(), false),
		canonicalSet(s.Scope.GetAttributes(), false),
		canonicalSet(s.Span.GetAttributes(), s.ExcludeMessageAttributes),
	}
}

// serviceName is the resource set's service.name string, or "".
func serviceName(resource attrSet) string {
	for _, kv := range resource.kvs {
		if kv.Key == "service.name" {
			return kv.GetValue().GetStringValue()
		}
	}
	return ""
}

// structArrays are the arrays of an event or link struct, whose wrappers the
// generator shares with SPANS's.
func (a *attrLists) structArrays() (*otelv1pb.SPAN_EVENT_STR_ATTRS, *otelv1pb.SPAN_EVENT_INT_ATTRS, *otelv1pb.SPAN_EVENT_DOUBLE_ATTRS, *otelv1pb.SPAN_EVENT_BOOL_ATTRS, *otelv1pb.SPAN_EVENT_ANY_ATTRS) {
	var (
		str     *otelv1pb.SPAN_EVENT_STR_ATTRS
		ints    *otelv1pb.SPAN_EVENT_INT_ATTRS
		doubles *otelv1pb.SPAN_EVENT_DOUBLE_ATTRS
		bools   *otelv1pb.SPAN_EVENT_BOOL_ATTRS
		anys    *otelv1pb.SPAN_EVENT_ANY_ATTRS
	)
	if len(a.str) > 0 {
		str = &otelv1pb.SPAN_EVENT_STR_ATTRS{Values: a.str}
	}
	if len(a.ints) > 0 {
		ints = &otelv1pb.SPAN_EVENT_INT_ATTRS{Values: a.ints}
	}
	if len(a.doubles) > 0 {
		doubles = &otelv1pb.SPAN_EVENT_DOUBLE_ATTRS{Values: a.doubles}
	}
	if len(a.bools) > 0 {
		bools = &otelv1pb.SPAN_EVENT_BOOL_ATTRS{Values: a.bools}
	}
	if len(a.anys) > 0 {
		anys = &otelv1pb.SPAN_EVENT_ANY_ATTRS{Values: a.anys}
	}
	return str, ints, doubles, bools, anys
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
