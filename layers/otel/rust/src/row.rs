use crate::{
    Error, ErrorKind, Result,
    otlp::{Frame, Id, KeyValue, Selection, Span, Value},
    records::*,
    tuple::{self, Element},
};
use base64::{Engine, engine::general_purpose::STANDARD};
use prost::Message;
use sha2::{Digest, Sha256};
use std::collections::{BTreeMap, HashMap};

pub const INDEXES: [&str; 8] = [
    "SPANS_TIME",
    "SPANS_SPAN_ID",
    "SPANS_STR_ATTR",
    "SPANS_LONG_STR_ATTR",
    "SPANS_INT_ATTR",
    "SPANS_DOUBLE_ATTR",
    "SPANS_BOOL_ATTR",
    "SPANS_REF_CHUNKS",
];
pub const ADMISSION_BYTES: usize = 300_000;
pub const ACTUAL_TXN_BYTES: i64 = 500_000;

#[derive(Clone)]
pub struct Schema;

#[derive(Clone, Debug)]
pub struct PreparedSpan {
    pub primary_key: Vec<u8>,
    pub row: Vec<u8>,
    pub otlp: Vec<u8>,
    pub indexes: BTreeMap<String, Vec<Vec<u8>>>,
    pub estimate: usize,
}

impl Schema {
    pub fn pinned() -> Result<Self> {
        Ok(Self)
    }

    /// A later duplicate identity replaces the earlier item at its original position.
    pub fn prepare(&self, inputs: &[Vec<u8>]) -> Result<Vec<PreparedSpan>> {
        let mut out = Vec::new();
        let mut positions = HashMap::new();
        self.each_span(inputs, |frame, at| {
            let item = build(frame, at, frame.single(at))?;
            if let Some(&i) = positions.get(&item.primary_key) {
                out[i] = item;
            } else {
                positions.insert(item.primary_key.clone(), out.len());
                out.push(item);
            }
            Ok(())
        })?;
        Ok(out)
    }

    /// Decode and check identity/start times without building rows or re-encoding.
    pub fn validate(&self, inputs: &[Vec<u8>]) -> Result<usize> {
        let mut count = 0;
        self.each_span(inputs, |_, _| {
            count += 1;
            Ok(())
        })?;
        Ok(count)
    }

    /// Split at span boundaries while keeping wrapper fields and duplicates.
    pub fn admit(&self, inputs: &[Vec<u8>]) -> Result<Vec<Vec<u8>>> {
        let mut out = Vec::new();
        self.each_span(inputs, |frame, at| {
            out.push(frame.single(at));
            Ok(())
        })?;
        Ok(out)
    }

    fn each_span(
        &self,
        inputs: &[Vec<u8>],
        mut visit: impl FnMut(&Frame, Selection) -> Result<()>,
    ) -> Result<()> {
        for input in inputs {
            let frame = Frame::decode(input)?;
            for &scope in &frame.root().scope_spans {
                for &span in &frame.scope_spans(scope).spans {
                    check_span(frame.span(span))?;
                    visit(&frame, Selection { scope, span })?;
                }
            }
        }
        Ok(())
    }

    pub fn decode_row(&self, wire: &[u8]) -> Result<Spans> {
        Spans::decode(wire)
            .map_err(|_| Error::new(ErrorKind::Corruption, "invalid stored SPANS row"))
    }

    pub fn index_entries(&self, row: &Spans) -> BTreeMap<String, Vec<Vec<u8>>> {
        index_entries(row)
    }
}

fn check_span(sp: &Span) -> Result<()> {
    if sp.trace_id.len() != 16
        || !sp.trace_id.iter().any(|b| *b != 0)
        || sp.span_id.len() != 8
        || !sp.span_id.iter().any(|b| *b != 0)
    {
        return Err(Error::new(
            ErrorKind::Admission,
            "expected nonzero 16-byte trace and 8-byte span identifiers",
        ));
    }
    if sp.start_time_unix_nano > i64::MAX as u64 {
        return Err(Error::new(
            ErrorKind::Admission,
            "start_time_unix_nano exceeds int64",
        ));
    }
    Ok(())
}
fn nonempty(v: &[u8]) -> Option<Vec<u8>> {
    (!v.is_empty()).then(|| v.to_vec())
}
fn text(v: &str) -> Option<String> {
    (!v.is_empty()).then(|| v.to_owned())
}
fn array<T: Message + Default + std::fmt::Debug>(values: Vec<T>) -> Option<Array<T>> {
    (!values.is_empty()).then_some(Array { values })
}
fn value(frame: &Frame, id: Option<Id>) -> Option<&Value> {
    id.and_then(|id| frame.any_value(id).value.as_ref())
}
fn build(frame: &Frame, at: Selection, otlp: Vec<u8>) -> Result<PreparedSpan> {
    let rs = frame.root();
    let ss = frame.scope_spans(at.scope);
    let sp = frame.span(at.span);
    let resource_attrs = canonical(
        frame,
        rs.resource
            .map_or(&[], |id| frame.resource(id).attributes.as_slice()),
    );
    let scope = ss.scope.map(|id| frame.scope(id));
    let mut attrs = Attrs::default();
    attrs.add(frame, "resource", &resource_attrs, true);
    attrs.add(
        frame,
        "scope",
        &canonical(frame, scope.map_or(&[], |s| s.attributes.as_slice())),
        true,
    );
    attrs.add(frame, "span", &canonical(frame, &sp.attributes), true);
    let service = resource_attrs
        .iter()
        .find(|kv| kv.key == "service.name")
        .and_then(|kv| match value(frame, kv.value) {
            Some(Value::String(s)) => text(s),
            _ => None,
        });
    let status = sp.status.map(|id| frame.status(id));
    let mut row = Spans {
        trace_id: nonempty(&sp.trace_id),
        span_id: nonempty(&sp.span_id),
        parent_span_id: nonempty(&sp.parent_span_id),
        trace_state: text(&sp.trace_state),
        flags: Some(sp.flags as i64),
        name: text(&sp.name),
        kind: Some(sp.kind),
        start_ns: Some(sp.start_time_unix_nano as i64),
        end_ns: Some(sp.end_time_unix_nano as i64),
        status_code: status.map(|s| s.code),
        status_message: status.and_then(|s| text(&s.message)),
        service_name: service,
        scope_name: scope.and_then(|s| text(&s.name)),
        scope_version: scope.and_then(|s| text(&s.version)),
        resource_schema_url: text(&rs.schema_url),
        scope_schema_url: text(&ss.schema_url),
        dropped_attributes: Some(sp.dropped_attributes_count as i64),
        dropped_events: Some(sp.dropped_events_count as i64),
        dropped_links: Some(sp.dropped_links_count as i64),
        str_attrs: array(attrs.strs),
        long_str_attrs: array(attrs.long_strs),
        int_attrs: array(attrs.ints),
        double_attrs: array(attrs.doubles),
        bool_attrs: array(attrs.bools),
        bytes_attrs: array(attrs.bytes),
        any_attrs: array(attrs.anys),
        otlp: nonempty(&otlp),
        ..Spans::default()
    };
    row.events = array(
        sp.events
            .iter()
            .enumerate()
            .map(|(i, &id)| {
                let ev = frame.event(id);
                let mut a = Attrs::default();
                a.add(frame, "event", &canonical(frame, &ev.attributes), false);
                SpanEvent {
                    ordinal: Some(i as i64),
                    name: text(&ev.name),
                    time_ns: Some(ev.time_unix_nano as i64),
                    dropped_attributes: Some(ev.dropped_attributes_count as i64),
                    str_attrs: array(a.strs),
                    int_attrs: array(a.ints),
                    double_attrs: array(a.doubles),
                    bool_attrs: array(a.bools),
                    any_attrs: array(a.anys),
                }
            })
            .collect(),
    );
    row.links = array(
        sp.links
            .iter()
            .enumerate()
            .map(|(i, &id)| {
                let ln = frame.link(id);
                let mut a = Attrs::default();
                a.add(frame, "link", &canonical(frame, &ln.attributes), false);
                SpanLink {
                    ordinal: Some(i as i64),
                    trace_id: nonempty(&ln.trace_id),
                    span_id: nonempty(&ln.span_id),
                    trace_state: text(&ln.trace_state),
                    flags: Some(ln.flags as i64),
                    dropped_attributes: Some(ln.dropped_attributes_count as i64),
                    str_attrs: array(a.strs),
                    int_attrs: array(a.ints),
                    double_attrs: array(a.doubles),
                    bool_attrs: array(a.bools),
                    any_attrs: array(a.anys),
                }
            })
            .collect(),
    );
    let estimate = span_cost(&row);
    if estimate > ADMISSION_BYTES {
        return Err(Error::new(
            ErrorKind::Admission,
            "single span exceeds conservative 300KB transaction estimate",
        ));
    }
    Ok(PreparedSpan {
        primary_key: tuple::pack(&[
            Element::Int(0),
            Element::Bytes(&sp.trace_id),
            Element::Bytes(&sp.span_id),
        ]),
        indexes: index_entries(&row),
        row: row.encode_to_vec(),
        otlp,
        estimate,
    })
}

fn canonical<'a>(frame: &'a Frame, attrs: &[Id]) -> Vec<&'a KeyValue> {
    let mut out = Vec::new();
    let mut positions = HashMap::new();
    for &id in attrs {
        let kv = frame.key_value(id);
        if let Some(&i) = positions.get(kv.key.as_str()) {
            out[i] = kv;
        } else {
            positions.insert(kv.key.as_str(), out.len());
            out.push(kv);
        }
    }
    out.retain(|kv| {
        kv.key.len() <= 1024
            && match value(frame, kv.value) {
                Some(Value::String(v)) => v.len() <= 64_000,
                Some(Value::Bytes(v)) => v.len() <= 64_000,
                Some(Value::Array(_) | Value::Kvlist(_)) => {
                    canonical_json(frame, kv.value).len() <= 64_000
                }
                _ => true,
            }
    });
    out
}
#[derive(Default)]
struct Attrs {
    strs: Vec<StrAttr>,
    long_strs: Vec<LongStrAttr>,
    ints: Vec<IntAttr>,
    doubles: Vec<DoubleAttr>,
    bools: Vec<BoolAttr>,
    bytes: Vec<BytesAttr>,
    anys: Vec<AnyAttr>,
}
impl Attrs {
    fn add(&mut self, frame: &Frame, scope: &str, attrs: &[&KeyValue], long: bool) {
        for kv in attrs {
            let (scope, attr_key) = (Some(scope.to_owned()), Some(kv.key.clone()));
            match value(frame, kv.value) {
                Some(Value::String(v)) if v.len() <= 1024 => self.strs.push(StrAttr {
                    scope,
                    attr_key,
                    value: Some(v.clone()),
                }),
                Some(Value::String(v)) if long => self.long_strs.push(LongStrAttr {
                    scope,
                    attr_key,
                    digest: Some(Sha256::digest(v.as_bytes())[..16].to_vec()),
                    value: Some(v.clone()),
                }),
                Some(Value::String(_)) => (),
                Some(Value::Int(v)) => self.ints.push(IntAttr {
                    scope,
                    attr_key,
                    value: Some(*v),
                }),
                Some(Value::Double(v)) => self.doubles.push(DoubleAttr {
                    scope,
                    attr_key,
                    value: Some(if v.is_nan() {
                        f64::from_bits(0x7ff8000000000000)
                    } else {
                        *v
                    }),
                }),
                Some(Value::Bool(v)) => self.bools.push(BoolAttr {
                    scope,
                    attr_key,
                    value: Some(*v),
                }),
                Some(Value::Bytes(v)) if long => self.bytes.push(BytesAttr {
                    scope,
                    attr_key,
                    value: Some(v.clone()),
                }),
                Some(Value::Bytes(_)) => (),
                _ => self.anys.push(AnyAttr {
                    scope,
                    attr_key,
                    value: Some(canonical_json(frame, kv.value)),
                }),
            }
        }
    }
}
fn json_string(s: &str) -> String {
    serde_json::to_string(s)
        .expect("string serialization")
        .replace('\u{2028}', "\\u2028")
        .replace('\u{2029}', "\\u2029")
}
fn canonical_json(frame: &Frame, id: Option<Id>) -> String {
    enum Work<'a> {
        Value(Option<Id>),
        Raw(&'static str),
        String(&'a str),
    }
    let mut pending = vec![Work::Value(id)];
    let mut out = String::new();
    while let Some(work) = pending.pop() {
        match work {
            Work::Raw(s) => out.push_str(s),
            Work::String(s) => out.push_str(&json_string(s)),
            Work::Value(id) => match value(frame, id) {
                Some(Value::String(s)) => out.push_str(&json_string(s)),
                Some(Value::Int(v)) => out.push_str(&v.to_string()),
                Some(Value::Bool(v)) => out.push_str(if *v { "true" } else { "false" }),
                Some(Value::Double(v)) => {
                    let s = format_double(*v);
                    out.push_str(&if v.is_finite() { s } else { json_string(&s) });
                }
                Some(Value::Bytes(v)) => out.push_str(&json_string(&STANDARD.encode(v))),
                Some(Value::Array(id)) => {
                    out.push('[');
                    pending.push(Work::Raw("]"));
                    for (i, &id) in frame.array(*id).values.iter().enumerate().rev() {
                        pending.push(Work::Value(Some(id)));
                        if i != 0 {
                            pending.push(Work::Raw(","));
                        }
                    }
                }
                Some(Value::Kvlist(id)) => {
                    out.push('{');
                    pending.push(Work::Raw("}"));
                    for (i, &id) in frame.kvlist(*id).values.iter().enumerate().rev() {
                        let kv = frame.key_value(id);
                        pending.push(Work::Value(kv.value));
                        pending.push(Work::Raw(":"));
                        pending.push(Work::String(&kv.key));
                        if i != 0 {
                            pending.push(Work::Raw(","));
                        }
                    }
                }
                _ => out.push_str("null"),
            },
        }
    }
    out
}
pub fn format_double(f: f64) -> String {
    if f.is_nan() {
        return "NaN".into();
    }
    if f == f64::INFINITY {
        return "+Inf".into();
    }
    if f == f64::NEG_INFINITY {
        return "-Inf".into();
    }
    // Go's 'g', -1 switches notation at exponents -4 and 6.
    let scientific = format!("{f:e}");
    let (mantissa, exponent) = scientific.split_once('e').unwrap();
    let exponent: i32 = exponent.parse().unwrap();
    if !(-4..6).contains(&exponent) {
        return format!(
            "{mantissa}e{}{:02}",
            if exponent < 0 { "-" } else { "+" },
            exponent.unsigned_abs()
        );
    }
    f.to_string()
}
fn span_cost(row: &Spans) -> usize {
    let mut cost = 2048 + row.encoded_len();
    macro_rules! add {
        ($field:ident, $length:expr) => {
            if let Some(a) = &row.$field {
                for v in &a.values {
                    cost += v.attr_key.as_ref().map_or(0, String::len)
                        + v.value.as_ref().map_or(0, $length)
                        + 512;
                }
            }
        };
    }
    add!(str_attrs, String::len);
    add!(long_str_attrs, String::len);
    add!(int_attrs, |v: &i64| v.to_string().len());
    add!(double_attrs, |v: &f64| format_double(*v).len());
    add!(bool_attrs, |v: &bool| if *v { 4 } else { 5 });
    add!(bytes_attrs, Vec::len);
    add!(any_attrs, String::len);
    cost
}
fn index_entries(row: &Spans) -> BTreeMap<String, Vec<Vec<u8>>> {
    let trace = row.trace_id.as_deref().unwrap_or_default();
    let span = row.span_id.as_deref().unwrap_or_default();
    let start = row.start_ns.map_or(Element::Null, Element::Int);
    let mut out: BTreeMap<String, Vec<Vec<u8>>> =
        INDEXES.iter().map(|n| ((*n).into(), Vec::new())).collect();
    out.get_mut("SPANS_TIME").unwrap().push(tuple::pack(&[
        start.clone(),
        Element::Bytes(trace),
        Element::Bytes(span),
        Element::Int(0),
    ]));
    out.get_mut("SPANS_SPAN_ID").unwrap().push(tuple::pack(&[
        Element::Bytes(span),
        start.clone(),
        Element::Bytes(trace),
        Element::Int(0),
    ]));
    macro_rules! add {
        ($field:ident, $index:literal, $value:ident, $variant:ident) => {
            if let Some(a) = &row.$field {
                for v in &a.values {
                    out.get_mut($index).unwrap().push(tuple::pack(&[
                        v.scope.as_deref().map_or(Element::Null, Element::String),
                        v.attr_key.as_deref().map_or(Element::Null, Element::String),
                        v.$value
                            .as_ref()
                            .map_or(Element::Null, |x| Element::$variant(x.as_ref())),
                        start.clone(),
                        Element::Bytes(trace),
                        Element::Bytes(span),
                        Element::Int(0),
                    ]));
                }
            }
        };
    }
    add!(str_attrs, "SPANS_STR_ATTR", value, String);
    add!(long_str_attrs, "SPANS_LONG_STR_ATTR", digest, Bytes);
    macro_rules! scalar {
        ($field:ident, $index:literal, $variant:ident) => {
            if let Some(a) = &row.$field {
                for v in &a.values {
                    out.get_mut($index).unwrap().push(tuple::pack(&[
                        v.scope.as_deref().map_or(Element::Null, Element::String),
                        v.attr_key.as_deref().map_or(Element::Null, Element::String),
                        v.value.map_or(Element::Null, Element::$variant),
                        start.clone(),
                        Element::Bytes(trace),
                        Element::Bytes(span),
                        Element::Int(0),
                    ]));
                }
            }
        };
    }
    scalar!(int_attrs, "SPANS_INT_ATTR", Int);
    scalar!(double_attrs, "SPANS_DOUBLE_ATTR", Double);
    scalar!(bool_attrs, "SPANS_BOOL_ATTR", Bool);
    if let Some(a) = &row.ref_chunks {
        for v in &a.values {
            out.get_mut("SPANS_REF_CHUNKS").unwrap().push(tuple::pack(&[
                Element::Bytes(trace),
                start.clone(),
                Element::Bytes(span),
                v.chunk.map_or(Element::Null, Element::Int),
                v.refs.as_deref().map_or(Element::Null, Element::Bytes),
                Element::Int(0),
            ]));
        }
    }
    for entries in out.values_mut() {
        entries.sort();
        entries.dedup();
    }
    out
}
