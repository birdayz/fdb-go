package otelsql

import (
	"strings"

	"fdb.dev/pkg/recordlayer"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	resourcepb "go.opentelemetry.io/proto/otlp/resource/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/dynamicpb"
)

// LongStringDigestBytes is the SHA-256 prefix long_str_attrs carries; the
// digest index narrows a lookup and the stored value is compared exactly.
const LongStringDigestBytes = 16

// Attribute scopes of the typed attribute arrays.
const (
	ScopeResource = "resource"
	ScopeScope    = "scope"
	ScopeSpan     = "span"
	ScopeEvent    = "event"
	ScopeLink     = "link"
)

// Span is one OTLP span with its resource and scope, and the envelope bytes
// stored in the otlp column (the exporter's single-span ResourceSpans).
//
// The attribute arrays project each scope's canonical set: unique keys at
// their first position with their last value; keys over MaxAttributeKeyBytes
// and values over MaxAttributeValueBytes left out. Strings up to
// MaxIndexedStringBytes go to str_attrs, longer ones to long_str_attrs; ints,
// doubles and bools to their arrays; bytes to bytes_attrs and arrays, kvlists
// and unset values to any_attrs as CanonicalJSON, neither of which is indexed.
type Span struct {
	Resource          *resourcepb.Resource
	ResourceSchemaURL string
	Scope             *commonpb.InstrumentationScope
	ScopeSchemaURL    string
	Span              *tracepb.Span
	OTLP              []byte
	// Refs are the span's message back references, packed into ref_chunks.
	Refs []MessageRef
	// ExcludeMessageAttributes leaves the GenAI content attributes
	// (MessageAttributeKeys) of the span and its events out of the arrays:
	// their values are decomposed into MESSAGES rows.
	ExcludeMessageAttributes bool
}

// InvalidRowError is input a row builder refuses (BuildCompiled, Build,
// BuildLogCompiled, BuildMessageCompiled, BuildStoreMetaCompiled).
type InvalidRowError struct {
	Table, Reason string
}

func (e *InvalidRowError) Error() string {
	return "otelsql: invalid " + e.Table + " row: " + e.Reason
}

// SpanRows builds SPANS records of one template version. Field descriptors
// are resolved once; Build only fills a dynamic message.
type SpanRows struct {
	d                                                                protoreflect.MessageDescriptor
	traceID, spanID, parentSpanID, traceState, flags, name, kind     protoreflect.FieldDescriptor
	startNs, endNs, statusCode, statusMessage, serviceName           protoreflect.FieldDescriptor
	scopeName, scopeVersion, resourceSchemaURL, scopeSchemaURL       protoreflect.FieldDescriptor
	droppedAttributes, droppedEvents, droppedLinks, otlp             protoreflect.FieldDescriptor
	attrs                                                            attrArrays
	events, links                                                    arrayField
	evOrdinal, evName, evTime, evDropped                             protoreflect.FieldDescriptor
	lnOrdinal, lnTraceID, lnSpanID, lnTraceState, lnFlags, lnDropped protoreflect.FieldDescriptor
	eventAttrs, linkAttrs                                            attrArrays
	refChunks                                                        arrayField
	chunk, refs                                                      protoreflect.FieldDescriptor
}

// NewSpanRows resolves the SPANS record type of md.
func NewSpanRows(md *recordlayer.RecordMetaData) (*SpanRows, error) {
	rt := md.GetRecordType("SPANS")
	if rt == nil {
		return nil, &TemplateMismatchError{Reason: "metadata has no SPANS table"}
	}
	r := &SpanRows{d: rt.Descriptor}
	f := fieldsOf(rt.Descriptor)
	var err error
	for _, x := range []struct {
		dst  *protoreflect.FieldDescriptor
		name string
	}{
		{&r.traceID, "trace_id"},
		{&r.spanID, "span_id"},
		{&r.parentSpanID, "parent_span_id"},
		{&r.traceState, "trace_state"},
		{&r.flags, "flags"},
		{&r.name, "name"},
		{&r.kind, "kind"},
		{&r.startNs, "start_ns"},
		{&r.endNs, "end_ns"},
		{&r.statusCode, "status_code"},
		{&r.statusMessage, "status_message"},
		{&r.serviceName, "service_name"},
		{&r.scopeName, "scope_name"},
		{&r.scopeVersion, "scope_version"},
		{&r.resourceSchemaURL, "resource_schema_url"},
		{&r.scopeSchemaURL, "scope_schema_url"},
		{&r.droppedAttributes, "dropped_attributes"},
		{&r.droppedEvents, "dropped_events"},
		{&r.droppedLinks, "dropped_links"},
		{&r.otlp, "otlp"},
	} {
		if *x.dst, err = f.get(x.name); err != nil {
			return nil, err
		}
	}
	if r.attrs, err = newAttrArrays(f, true); err != nil {
		return nil, err
	}
	if r.events, err = newArrayField(f, "events"); err != nil {
		return nil, err
	}
	if r.links, err = newArrayField(f, "links"); err != nil {
		return nil, err
	}
	ev, ln := fieldsOf(r.events.elem), fieldsOf(r.links.elem)
	for _, x := range []struct {
		f    fieldMap
		dst  *protoreflect.FieldDescriptor
		name string
	}{
		{ev, &r.evOrdinal, "ordinal"},
		{ev, &r.evName, "name"},
		{ev, &r.evTime, "time_ns"},
		{ev, &r.evDropped, "dropped_attributes"},
		{ln, &r.lnOrdinal, "ordinal"},
		{ln, &r.lnTraceID, "trace_id"},
		{ln, &r.lnSpanID, "span_id"},
		{ln, &r.lnTraceState, "trace_state"},
		{ln, &r.lnFlags, "flags"},
		{ln, &r.lnDropped, "dropped_attributes"},
	} {
		if *x.dst, err = x.f.get(x.name); err != nil {
			return nil, err
		}
	}
	if r.eventAttrs, err = newAttrArrays(ev, false); err != nil {
		return nil, err
	}
	if r.linkAttrs, err = newAttrArrays(ln, false); err != nil {
		return nil, err
	}
	if r.refChunks, err = newArrayField(f, "ref_chunks"); err != nil {
		return nil, err
	}
	rc := fieldsOf(r.refChunks.elem)
	if r.chunk, err = rc.get("chunk"); err != nil {
		return nil, err
	}
	if r.refs, err = rc.get("refs"); err != nil {
		return nil, err
	}
	return r, nil
}

// Build returns the SPANS record of s. Typed columns and attribute arrays are
// query projections; the otlp column is the lossless envelope.
func (r *SpanRows) Build(s Span) (proto.Message, error) {
	sp := s.Span
	if sp == nil {
		return nil, &InvalidRowError{Table: "SPANS", Reason: "nil span"}
	}
	m := dynamicpb.NewMessage(r.d)
	setBytes(m, r.traceID, sp.TraceId)
	setBytes(m, r.spanID, sp.SpanId)
	setBytes(m, r.parentSpanID, sp.ParentSpanId)
	setString(m, r.traceState, sp.TraceState)
	m.Set(r.flags, intValue(r.flags, int64(sp.Flags)))
	setString(m, r.name, sp.Name)
	m.Set(r.kind, intValue(r.kind, int64(sp.Kind)))
	m.Set(r.startNs, intValue(r.startNs, int64(sp.StartTimeUnixNano)))
	m.Set(r.endNs, intValue(r.endNs, int64(sp.EndTimeUnixNano)))
	if st := sp.Status; st != nil {
		m.Set(r.statusCode, intValue(r.statusCode, int64(st.Code)))
		setString(m, r.statusMessage, st.Message)
	}
	setString(m, r.resourceSchemaURL, s.ResourceSchemaURL)
	setString(m, r.scopeSchemaURL, s.ScopeSchemaURL)
	if sc := s.Scope; sc != nil {
		setString(m, r.scopeName, sc.Name)
		setString(m, r.scopeVersion, sc.Version)
	}
	m.Set(r.droppedAttributes, intValue(r.droppedAttributes, int64(sp.DroppedAttributesCount)))
	m.Set(r.droppedEvents, intValue(r.droppedEvents, int64(sp.DroppedEventsCount)))
	m.Set(r.droppedLinks, intValue(r.droppedLinks, int64(sp.DroppedLinksCount)))
	sets := spanSets(s)
	setString(m, r.serviceName, serviceName(sets[0]))
	r.attrs.add(m, ScopeResource, sets[0])
	r.attrs.add(m, ScopeScope, sets[1])
	r.attrs.add(m, ScopeSpan, sets[2])
	for i, ev := range sp.Events {
		e := r.events.appendMessage(m)
		e.Set(r.evOrdinal, intValue(r.evOrdinal, int64(i)))
		setString(e, r.evName, ev.Name)
		e.Set(r.evTime, intValue(r.evTime, int64(ev.TimeUnixNano)))
		e.Set(r.evDropped, intValue(r.evDropped, int64(ev.DroppedAttributesCount)))
		r.eventAttrs.add(e, ScopeEvent, canonicalSet(ev.Attributes, s.ExcludeMessageAttributes))
	}
	for i, ln := range sp.Links {
		l := r.links.appendMessage(m)
		l.Set(r.lnOrdinal, intValue(r.lnOrdinal, int64(i)))
		setBytes(l, r.lnTraceID, ln.TraceId)
		setBytes(l, r.lnSpanID, ln.SpanId)
		setString(l, r.lnTraceState, ln.TraceState)
		l.Set(r.lnFlags, intValue(r.lnFlags, int64(ln.Flags)))
		l.Set(r.lnDropped, intValue(r.lnDropped, int64(ln.DroppedAttributesCount)))
		r.linkAttrs.add(l, ScopeLink, canonicalSet(ln.Attributes, false))
	}
	chunks, err := PackRefChunks(s.Refs)
	if err != nil {
		return nil, err
	}
	for i, c := range chunks {
		e := r.refChunks.appendMessage(m)
		e.Set(r.chunk, protoreflect.ValueOfInt64(int64(i)))
		e.Set(r.refs, protoreflect.ValueOfBytes(c))
	}
	setBytes(m, r.otlp, s.OTLP)
	return m, nil
}

// attrArrays are the typed attribute arrays of a table or struct; long is
// false for event and link structs, which project neither long strings nor bytes.
type attrArrays struct {
	str, longStr, ints, doubles, bools, bytesA, anys arrayField
	long                                             bool
}

func newAttrArrays(f fieldMap, long bool) (attrArrays, error) {
	a := attrArrays{long: long}
	var err error
	for _, x := range []struct {
		dst  *arrayField
		name string
		skip bool
	}{
		{&a.str, "str_attrs", false},
		{&a.longStr, "long_str_attrs", !long},
		{&a.ints, "int_attrs", false},
		{&a.doubles, "double_attrs", false},
		{&a.bools, "bool_attrs", false},
		{&a.bytesA, "bytes_attrs", !long},
		{&a.anys, "any_attrs", false},
	} {
		if x.skip {
			continue
		}
		if *x.dst, err = newArrayField(f, x.name); err != nil {
			return a, err
		}
	}
	return a, nil
}

// add projects a canonical set under scope, as attrLists.add does.
func (a *attrArrays) add(m protoreflect.Message, scope string, set attrSet) {
	for _, kv := range set.kvs {
		key := kv.Key
		switch v := kv.GetValue().GetValue().(type) {
		case *commonpb.AnyValue_StringValue:
			if len(v.StringValue) <= MaxIndexedStringBytes {
				a.str.appendAttr(m, scope, key, protoreflect.ValueOfString(v.StringValue))
			} else if a.long {
				e := a.longStr.appendAttr(m, scope, key, protoreflect.ValueOfString(v.StringValue))
				e.Set(a.longStr.digest, protoreflect.ValueOfBytes(LongStringDigest(v.StringValue)))
			}
		case *commonpb.AnyValue_IntValue:
			a.ints.appendAttr(m, scope, key, protoreflect.ValueOfInt64(v.IntValue))
		case *commonpb.AnyValue_DoubleValue:
			a.doubles.appendAttr(m, scope, key, protoreflect.ValueOfFloat64(ProjectedDouble(v.DoubleValue)))
		case *commonpb.AnyValue_BoolValue:
			a.bools.appendAttr(m, scope, key, protoreflect.ValueOfBool(v.BoolValue))
		case *commonpb.AnyValue_BytesValue:
			if a.long {
				a.bytesA.appendAttr(m, scope, key, protoreflect.ValueOfBytes(v.BytesValue))
			}
		default:
			a.anys.appendAttr(m, scope, key, protoreflect.ValueOfString(string(CanonicalJSON(kv.GetValue()))))
		}
	}
}

// arrayField is an ARRAY column: repeated directly, or a nullable array's
// wrapper message { repeated values = 1; }.
type arrayField struct {
	fd, values                protoreflect.FieldDescriptor
	elem                      protoreflect.MessageDescriptor
	scope, key, value, digest protoreflect.FieldDescriptor // attribute structs only
}

func newArrayField(f fieldMap, name string) (arrayField, error) {
	fd, err := f.get(name)
	if err != nil {
		return arrayField{}, err
	}
	a := arrayField{fd: fd}
	elemField := fd
	if !fd.IsList() {
		if fd.Kind() != protoreflect.MessageKind || fd.Message().Fields().Len() != 1 || !fd.Message().Fields().Get(0).IsList() {
			return a, &TemplateMismatchError{Reason: "column " + name + " is not an array"}
		}
		a.values = fd.Message().Fields().Get(0)
		elemField = a.values
	}
	if elemField.Kind() == protoreflect.MessageKind {
		a.elem = elemField.Message()
		ef := fieldsOf(a.elem)
		a.scope, a.key = ef.m["SCOPE"], ef.m["ATTR_KEY"]
		a.digest = ef.m["DIGEST"]
		for _, v := range []string{"STR_VALUE", "INT_VALUE", "DOUBLE_VALUE", "BOOL_VALUE", "BYTES_VALUE", "JSON_VALUE"} {
			if x, ok := ef.m[v]; ok {
				a.value = x
			}
		}
	}
	return a, nil
}

func (a *arrayField) list(m protoreflect.Message) protoreflect.List {
	if a.values == nil {
		return m.Mutable(a.fd).List()
	}
	return m.Mutable(a.fd).Message().Mutable(a.values).List()
}

func (a *arrayField) appendMessage(m protoreflect.Message) protoreflect.Message {
	return a.list(m).AppendMutable().Message()
}

func (a *arrayField) appendAttr(m protoreflect.Message, scope, key string, v protoreflect.Value) protoreflect.Message {
	e := a.appendMessage(m)
	e.Set(a.scope, protoreflect.ValueOfString(scope))
	e.Set(a.key, protoreflect.ValueOfString(key))
	e.Set(a.value, v)
	return e
}

// fieldMap resolves columns by name; storage names are upper case.
type fieldMap struct {
	m map[string]protoreflect.FieldDescriptor
}

func fieldsOf(d protoreflect.MessageDescriptor) fieldMap {
	fs := d.Fields()
	m := make(map[string]protoreflect.FieldDescriptor, fs.Len())
	for i := 0; i < fs.Len(); i++ {
		m[strings.ToUpper(string(fs.Get(i).Name()))] = fs.Get(i)
	}
	return fieldMap{m}
}

func (f fieldMap) get(name string) (protoreflect.FieldDescriptor, error) {
	if fd, ok := f.m[strings.ToUpper(name)]; ok {
		return fd, nil
	}
	return nil, &TemplateMismatchError{Reason: "missing column " + name}
}

func intValue(fd protoreflect.FieldDescriptor, v int64) protoreflect.Value {
	if fd.Kind() == protoreflect.Int32Kind || fd.Kind() == protoreflect.Sint32Kind || fd.Kind() == protoreflect.Sfixed32Kind {
		return protoreflect.ValueOfInt32(int32(v)) //nolint:gosec // OTLP enums and counts fit
	}
	return protoreflect.ValueOfInt64(v)
}

func setString(m protoreflect.Message, fd protoreflect.FieldDescriptor, s string) {
	if s != "" {
		m.Set(fd, protoreflect.ValueOfString(s))
	}
}

func setBytes(m protoreflect.Message, fd protoreflect.FieldDescriptor, b []byte) {
	if len(b) != 0 {
		m.Set(fd, protoreflect.ValueOfBytes(b))
	}
}
