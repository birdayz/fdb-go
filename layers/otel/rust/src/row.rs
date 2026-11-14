use crate::{
    Error, ErrorKind, Result,
    tuple::{self, Element},
    wire,
};
use base64::{Engine, engine::general_purpose::STANDARD};
use prost::Message;
use prost_reflect::{
    DescriptorPool, DynamicMessage, Kind, MessageDescriptor, ReflectMessage, Value,
};
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
pub struct Schema {
    pool: DescriptorPool,
}

/// A span's position: input wrapper, scope within it and span within that scope.
#[derive(Clone, Copy)]
struct At {
    input: usize,
    scope: usize,
    span: usize,
}

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
        let pool = DescriptorPool::decode(include_bytes!("../testdata/descriptors.pb").as_slice())
            .map_err(|_| Error::new(ErrorKind::Config, "invalid pinned descriptor set"))?;
        let schema = Self { pool };
        wire::check_graph(&schema.resource_spans())?;
        Ok(schema)
    }

    /// All spans are prepared before the caller starts any transactions. A later
    /// duplicate identity replaces the earlier item at its original position.
    pub fn prepare(&self, inputs: &[Vec<u8>]) -> Result<Vec<PreparedSpan>> {
        let mut out = Vec::new();
        let mut positions = HashMap::new();
        self.each_single(inputs, |single, ss, sp, otlp| {
            let item = self.build(single, ss, sp, otlp)?;
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

    /// `prepare`'s decode and identity/start checks without building rows or re-encoding;
    /// returns the span count. Row-size admission happens only in `prepare`.
    pub fn validate(&self, inputs: &[Vec<u8>]) -> Result<usize> {
        let mut spans = 0;
        self.each_span(inputs, |_, _, _, _| {
            spans += 1;
            Ok(())
        })?;
        Ok(spans)
    }

    /// Split at span boundaries without losing wrapper fields or duplicates, under
    /// `prepare`'s decode and identity/start checks; row-size admission is the sink's.
    pub fn admit(&self, inputs: &[Vec<u8>]) -> Result<Vec<Vec<u8>>> {
        let mut out = Vec::new();
        self.each_single(inputs, |_, _, _, otlp| {
            out.push(otlp);
            Ok(())
        })?;
        Ok(out)
    }

    fn resource_spans(&self) -> MessageDescriptor {
        self.pool
            .get_message_by_name("opentelemetry.proto.trace.v1.ResourceSpans")
            .expect("pinned OTLP descriptor")
    }

    /// The one decode and identity/start-time admission path that `prepare`, `validate`
    /// and `admit` share; `visit` sees each checked span, its wrappers and their positions.
    fn each_span(
        &self,
        inputs: &[Vec<u8>],
        mut visit: impl FnMut(At, &DynamicMessage, &DynamicMessage, &DynamicMessage) -> Result<()>,
    ) -> Result<()> {
        let descriptor = self.resource_spans();
        for (input, wire) in inputs.iter().enumerate() {
            let rs = DynamicMessage::decode(descriptor.clone(), wire.as_slice())
                .map_err(|_| Error::new(ErrorKind::Admission, "invalid ResourceSpans protobuf"))?;
            for (scope, ss) in messages(&rs, "scope_spans").iter().enumerate() {
                for (span, sp) in messages(ss, "spans").iter().enumerate() {
                    check_span(sp)?;
                    visit(At { input, scope, span }, &rs, ss, sp)?;
                }
            }
        }
        Ok(())
    }

    /// `each_span` with each span's unknown-free single-span wrapper, scope and span, and
    /// the OTLP Go stores for it: canonical known fields plus each node's raw unknown bytes.
    fn each_single(
        &self,
        inputs: &[Vec<u8>],
        mut visit: impl FnMut(&DynamicMessage, &DynamicMessage, &DynamicMessage, Vec<u8>) -> Result<()>,
    ) -> Result<()> {
        let descriptor = self.resource_spans();
        let scopes_field = descriptor
            .get_field_by_name("scope_spans")
            .expect("scope_spans");
        let Kind::Message(scope_desc) = scopes_field.kind() else {
            unreachable!()
        };
        let spans_field = scope_desc
            .get_field_by_name("spans")
            .expect("spans")
            .number();
        let mut current: Option<(
            usize,
            wire::Node,
            DynamicMessage,
            Vec<(DynamicMessage, Vec<DynamicMessage>)>,
        )> = None;
        self.each_span(inputs, |at, rs, _, _| {
            if current.as_ref().is_none_or(|c| c.0 != at.input) {
                let tree = wire::scan(&descriptor, &inputs[at.input])?;
                let mut clean = rs.clone();
                wire::strip(&mut clean);
                let scopes = messages(&clean, "scope_spans")
                    .into_iter()
                    .map(|ss| {
                        let spans = messages(&ss, "spans");
                        (ss, spans)
                    })
                    .collect();
                current = Some((at.input, tree, clean, scopes));
            }
            let (_, tree, clean, scopes) = current.as_ref().expect("set above");
            let (ss, spans) = &scopes[at.scope];
            let sp = &spans[at.span];
            let mut single_scope = ss.clone();
            single_scope.set_field_by_name("spans", Value::List(vec![Value::Message(sp.clone())]));
            let mut single = clean.clone();
            single.set_field_by_name(
                "scope_spans",
                Value::List(vec![Value::Message(single_scope)]),
            );
            let node = wire::select(tree, scopes_field.number(), at.scope, spans_field, at.span)?;
            let otlp = wire::patch(&descriptor, &single.encode_to_vec(), &node)?;
            visit(&single, ss, sp, otlp)
        })
    }

    fn build(
        &self,
        single: &DynamicMessage,
        ss: &DynamicMessage,
        sp: &DynamicMessage,
        otlp: Vec<u8>,
    ) -> Result<PreparedSpan> {
        let trace = bytes(sp, "trace_id");
        let span = bytes(sp, "span_id");
        let mut row = DynamicMessage::new(
            self.pool
                .get_message_by_name("SPANS")
                .expect("pinned SPANS descriptor"),
        );
        let resource = message(single, "resource");
        let scope = message(ss, "scope");
        for (target, source) in [
            ("TRACE_ID", "trace_id"),
            ("SPAN_ID", "span_id"),
            ("PARENT_SPAN_ID", "parent_span_id"),
        ] {
            set_bytes(&mut row, target, bytes(sp, source));
        }
        for (target, source) in [("TRACE_STATE", "trace_state"), ("NAME", "name")] {
            set_string(&mut row, target, string(sp, source));
        }
        row.set_field_by_name("KIND", Value::I32(enumeration(sp, "kind")));
        for (target, source) in [
            ("FLAGS", "flags"),
            ("START_NS", "start_time_unix_nano"),
            ("END_NS", "end_time_unix_nano"),
            ("DROPPED_ATTRIBUTES", "dropped_attributes_count"),
            ("DROPPED_EVENTS", "dropped_events_count"),
            ("DROPPED_LINKS", "dropped_links_count"),
        ] {
            row.set_field_by_name(target, Value::I64(unsigned(sp, source) as i64));
        }
        if sp.has_field_by_name("status") {
            let status = message(sp, "status");
            row.set_field_by_name("STATUS_CODE", Value::I32(enumeration(&status, "code")));
            set_string(&mut row, "STATUS_MESSAGE", string(&status, "message"));
        }
        for (target, source) in [("SCOPE_NAME", "name"), ("SCOPE_VERSION", "version")] {
            set_string(&mut row, target, string(&scope, source));
        }
        set_string(
            &mut row,
            "RESOURCE_SCHEMA_URL",
            string(single, "schema_url"),
        );
        set_string(&mut row, "SCOPE_SCHEMA_URL", string(ss, "schema_url"));
        let resource_attrs = canonical(&resource);
        if let Some(kv) = resource_attrs
            .iter()
            .find(|kv| string(kv, "key") == "service.name")
        {
            set_string(
                &mut row,
                "SERVICE_NAME",
                string(&message(kv, "value"), "string_value"),
            );
        }
        add_attributes(&mut row, "resource", resource_attrs, true);
        add_attributes(&mut row, "scope", canonical(&scope), true);
        add_attributes(&mut row, "span", canonical(sp), true);
        for (i, event) in messages(sp, "events").iter().enumerate() {
            let mut e = array_element(&row, "EVENTS");
            e.set_field_by_name("ORDINAL", Value::I64(i as i64));
            set_string(&mut e, "NAME", string(event, "name"));
            e.set_field_by_name(
                "TIME_NS",
                Value::I64(unsigned(event, "time_unix_nano") as i64),
            );
            e.set_field_by_name(
                "DROPPED_ATTRIBUTES",
                Value::I64(unsigned(event, "dropped_attributes_count") as i64),
            );
            add_attributes(&mut e, "event", canonical(event), false);
            push_array(&mut row, "EVENTS", e);
        }
        for (i, link) in messages(sp, "links").iter().enumerate() {
            let mut l = array_element(&row, "LINKS");
            l.set_field_by_name("ORDINAL", Value::I64(i as i64));
            for (target, source) in [("TRACE_ID", "trace_id"), ("SPAN_ID", "span_id")] {
                set_bytes(&mut l, target, bytes(link, source));
            }
            set_string(&mut l, "TRACE_STATE", string(link, "trace_state"));
            l.set_field_by_name("FLAGS", Value::I64(unsigned(link, "flags") as i64));
            l.set_field_by_name(
                "DROPPED_ATTRIBUTES",
                Value::I64(unsigned(link, "dropped_attributes_count") as i64),
            );
            add_attributes(&mut l, "link", canonical(link), false);
            push_array(&mut row, "LINKS", l);
        }
        set_bytes(&mut row, "OTLP", otlp.clone());
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
                Element::Bytes(&trace),
                Element::Bytes(&span),
            ]),
            indexes: self.index_entries(&row),
            row: row.encode_to_vec(),
            otlp,
            estimate,
        })
    }

    pub fn decode_row(&self, wire: &[u8]) -> Result<DynamicMessage> {
        DynamicMessage::decode(self.pool.get_message_by_name("SPANS").expect("SPANS"), wire)
            .map_err(|_| Error::new(ErrorKind::Corruption, "invalid stored SPANS row"))
    }

    pub fn index_entries(&self, row: &DynamicMessage) -> BTreeMap<String, Vec<Vec<u8>>> {
        let trace = bytes(row, "TRACE_ID");
        let span = bytes(row, "SPAN_ID");
        let start = if row.has_field_by_name("START_NS") {
            Element::Int(signed(row, "START_NS"))
        } else {
            Element::Null
        };
        let mut out: BTreeMap<String, Vec<Vec<u8>>> =
            INDEXES.iter().map(|n| ((*n).into(), Vec::new())).collect();
        out.get_mut("SPANS_TIME").unwrap().push(tuple::pack(&[
            start.clone(),
            Element::Bytes(&trace),
            Element::Bytes(&span),
            Element::Int(0),
        ]));
        out.get_mut("SPANS_SPAN_ID").unwrap().push(tuple::pack(&[
            Element::Bytes(&span),
            start.clone(),
            Element::Bytes(&trace),
            Element::Int(0),
        ]));
        for (column, index, value) in [
            ("STR_ATTRS", "SPANS_STR_ATTR", "STR_VALUE"),
            ("LONG_STR_ATTRS", "SPANS_LONG_STR_ATTR", "DIGEST"),
            ("INT_ATTRS", "SPANS_INT_ATTR", "INT_VALUE"),
            ("DOUBLE_ATTRS", "SPANS_DOUBLE_ATTR", "DOUBLE_VALUE"),
            ("BOOL_ATTRS", "SPANS_BOOL_ATTR", "BOOL_VALUE"),
        ] {
            for a in array_values(row, column) {
                let scope = string(&a, "SCOPE");
                let key = string(&a, "ATTR_KEY");
                let v = a.get_field_by_name(value).unwrap().into_owned();
                let element = if !a.has_field_by_name(value) {
                    Element::Null
                } else {
                    match &v {
                        Value::String(s) => Element::String(s),
                        Value::Bytes(b) => Element::Bytes(b),
                        Value::I64(i) => Element::Int(*i),
                        Value::F64(f) => Element::Double(*f),
                        Value::Bool(b) => Element::Bool(*b),
                        _ => unreachable!("pinned typed attribute"),
                    }
                };
                out.get_mut(index).unwrap().push(tuple::pack(&[
                    if a.has_field_by_name("SCOPE") {
                        Element::String(&scope)
                    } else {
                        Element::Null
                    },
                    if a.has_field_by_name("ATTR_KEY") {
                        Element::String(&key)
                    } else {
                        Element::Null
                    },
                    element,
                    start.clone(),
                    Element::Bytes(&trace),
                    Element::Bytes(&span),
                    Element::Int(0),
                ]));
            }
        }
        for c in array_values(row, "REF_CHUNKS") {
            let refs = bytes(&c, "REFS");
            out.get_mut("SPANS_REF_CHUNKS").unwrap().push(tuple::pack(&[
                Element::Bytes(&trace),
                start.clone(),
                Element::Bytes(&span),
                if c.has_field_by_name("CHUNK") {
                    Element::Int(signed(&c, "CHUNK"))
                } else {
                    Element::Null
                },
                if c.has_field_by_name("REFS") {
                    Element::Bytes(&refs)
                } else {
                    Element::Null
                },
                Element::Int(0),
            ]));
        }
        for entries in out.values_mut() {
            entries.sort();
            entries.dedup();
        }
        out
    }
}

fn canonical(parent: &DynamicMessage) -> Vec<DynamicMessage> {
    let mut out = Vec::new();
    let mut positions = HashMap::new();
    for kv in messages(parent, "attributes") {
        let key = string(&kv, "key");
        if let Some(&i) = positions.get(&key) {
            out[i] = kv;
        } else {
            positions.insert(key, out.len());
            out.push(kv);
        }
    }
    out.retain(|kv| {
        if string(kv, "key").len() > 1024 {
            return false;
        }
        let value = message(kv, "value");
        if value.has_field_by_name("string_value") {
            return string(&value, "string_value").len() <= 64_000;
        }
        if value.has_field_by_name("bytes_value") {
            return bytes(&value, "bytes_value").len() <= 64_000;
        }
        if value.has_field_by_name("array_value") || value.has_field_by_name("kvlist_value") {
            return canonical_json(&value).len() <= 64_000;
        }
        true
    });
    out
}

fn add_attributes(row: &mut DynamicMessage, scope: &str, attrs: Vec<DynamicMessage>, long: bool) {
    for kv in attrs {
        let value = message(&kv, "value");
        let (column, field, v) = if value.has_field_by_name("string_value") {
            let s = string(&value, "string_value");
            if s.len() > 1024 {
                if !long {
                    continue;
                }
                ("LONG_STR_ATTRS", "STR_VALUE", Value::String(s))
            } else {
                ("STR_ATTRS", "STR_VALUE", Value::String(s))
            }
        } else if value.has_field_by_name("int_value") {
            (
                "INT_ATTRS",
                "INT_VALUE",
                Value::I64(signed(&value, "int_value")),
            )
        } else if value.has_field_by_name("double_value") {
            let f = match value.get_field_by_name("double_value").unwrap().as_ref() {
                Value::F64(f) => *f,
                _ => unreachable!(),
            };
            (
                "DOUBLE_ATTRS",
                "DOUBLE_VALUE",
                Value::F64(if f.is_nan() {
                    f64::from_bits(0x7ff8000000000000)
                } else {
                    f
                }),
            )
        } else if value.has_field_by_name("bool_value") {
            (
                "BOOL_ATTRS",
                "BOOL_VALUE",
                value.get_field_by_name("bool_value").unwrap().into_owned(),
            )
        } else if value.has_field_by_name("bytes_value") {
            if !long {
                continue;
            }
            (
                "BYTES_ATTRS",
                "BYTES_VALUE",
                Value::Bytes(bytes(&value, "bytes_value").into()),
            )
        } else {
            (
                "ANY_ATTRS",
                "JSON_VALUE",
                Value::String(canonical_json(&value)),
            )
        };
        let mut a = array_element(row, column);
        a.set_field_by_name("SCOPE", Value::String(scope.into()));
        a.set_field_by_name("ATTR_KEY", Value::String(string(&kv, "key")));
        if column == "LONG_STR_ATTRS" {
            let digest = Sha256::digest(v.as_str().unwrap().as_bytes());
            a.set_field_by_name("DIGEST", Value::Bytes(digest[..16].to_vec().into()));
        }
        a.set_field_by_name(field, v);
        push_array(row, column, a);
    }
}

fn span_cost(row: &DynamicMessage) -> usize {
    let mut cost = 2048 + row.encoded_len();
    for (column, field) in [
        ("STR_ATTRS", "STR_VALUE"),
        ("LONG_STR_ATTRS", "STR_VALUE"),
        ("INT_ATTRS", "INT_VALUE"),
        ("DOUBLE_ATTRS", "DOUBLE_VALUE"),
        ("BOOL_ATTRS", "BOOL_VALUE"),
        ("BYTES_ATTRS", "BYTES_VALUE"),
        ("ANY_ATTRS", "JSON_VALUE"),
    ] {
        for a in array_values(row, column) {
            let v = a.get_field_by_name(field).unwrap().into_owned();
            let len = match v {
                Value::String(s) => s.len(),
                Value::Bytes(b) => b.len(),
                Value::I64(i) => i.to_string().len(),
                Value::F64(f) => format_double(f).len(),
                Value::Bool(b) => {
                    if b {
                        4
                    } else {
                        5
                    }
                }
                _ => unreachable!(),
            };
            cost += string(&a, "ATTR_KEY").len() + len + 512;
        }
    }
    cost
}

pub fn canonical_json(v: &DynamicMessage) -> String {
    if v.has_field_by_name("string_value") {
        return json_string(&string(v, "string_value"));
    }
    if v.has_field_by_name("int_value") {
        return signed(v, "int_value").to_string();
    }
    if v.has_field_by_name("bool_value") {
        return match v.get_field_by_name("bool_value").unwrap().as_ref() {
            Value::Bool(b) => b.to_string(),
            _ => unreachable!(),
        };
    }
    if v.has_field_by_name("double_value") {
        let f = match v.get_field_by_name("double_value").unwrap().as_ref() {
            Value::F64(f) => *f,
            _ => unreachable!(),
        };
        let text = format_double(f);
        return if f.is_finite() {
            text
        } else {
            json_string(&text)
        };
    }
    if v.has_field_by_name("bytes_value") {
        return json_string(&STANDARD.encode(bytes(v, "bytes_value")));
    }
    if v.has_field_by_name("array_value") {
        return format!(
            "[{}]",
            messages(&message(v, "array_value"), "values")
                .iter()
                .map(canonical_json)
                .collect::<Vec<_>>()
                .join(",")
        );
    }
    if v.has_field_by_name("kvlist_value") {
        return format!(
            "{{{}}}",
            messages(&message(v, "kvlist_value"), "values")
                .iter()
                .map(|kv| format!(
                    "{}:{}",
                    json_string(&string(kv, "key")),
                    canonical_json(&message(kv, "value"))
                ))
                .collect::<Vec<_>>()
                .join(",")
        );
    }
    "null".into()
}

fn json_string(s: &str) -> String {
    serde_json::to_string(s)
        .expect("string serialization")
        .replace('\u{2028}', "\\u2028")
        .replace('\u{2029}', "\\u2029")
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
    // Rust's shortest scientific rendering and Go's 'g', -1 share the
    // round-trip digits; Go switches notation at exponents -4 and 6.
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

fn message(m: &DynamicMessage, name: &str) -> DynamicMessage {
    m.get_field_by_name(name)
        .unwrap()
        .as_message()
        .unwrap()
        .clone()
}
fn messages(m: &DynamicMessage, name: &str) -> Vec<DynamicMessage> {
    m.get_field_by_name(name)
        .unwrap()
        .as_list()
        .unwrap()
        .iter()
        .map(|v| v.as_message().unwrap().clone())
        .collect()
}
fn string(m: &DynamicMessage, name: &str) -> String {
    m.get_field_by_name(name).unwrap().as_str().unwrap().into()
}
fn check_span(sp: &DynamicMessage) -> Result<()> {
    let trace = bytes(sp, "trace_id");
    let span = bytes(sp, "span_id");
    if trace.len() != 16
        || !trace.iter().any(|b| *b != 0)
        || span.len() != 8
        || !span.iter().any(|b| *b != 0)
    {
        return Err(Error::new(
            ErrorKind::Admission,
            "expected nonzero 16-byte trace and 8-byte span identifiers",
        ));
    }
    if unsigned(sp, "start_time_unix_nano") > i64::MAX as u64 {
        return Err(Error::new(
            ErrorKind::Admission,
            "start_time_unix_nano exceeds int64",
        ));
    }
    Ok(())
}
fn bytes(m: &DynamicMessage, name: &str) -> Vec<u8> {
    m.get_field_by_name(name)
        .unwrap()
        .as_bytes()
        .unwrap()
        .to_vec()
}
fn signed(m: &DynamicMessage, name: &str) -> i64 {
    m.get_field_by_name(name).unwrap().as_i64().unwrap()
}
fn unsigned(m: &DynamicMessage, name: &str) -> u64 {
    let v = m.get_field_by_name(name).unwrap();
    match v.as_ref() {
        Value::U64(i) => *i,
        Value::U32(i) => *i as u64,
        _ => unreachable!(),
    }
}
fn enumeration(m: &DynamicMessage, name: &str) -> i32 {
    m.get_field_by_name(name).unwrap().as_enum_number().unwrap()
}
fn set_string(m: &mut DynamicMessage, name: &str, value: String) {
    if !value.is_empty() {
        m.set_field_by_name(name, Value::String(value));
    }
}
fn set_bytes(m: &mut DynamicMessage, name: &str, value: Vec<u8>) {
    if !value.is_empty() {
        m.set_field_by_name(name, Value::Bytes(value.into()));
    }
}
fn array_element(m: &DynamicMessage, name: &str) -> DynamicMessage {
    let field = m.descriptor().get_field_by_name(name).unwrap();
    let Kind::Message(wrapper) = field.kind() else {
        unreachable!()
    };
    let Kind::Message(element) = wrapper.get_field(1).unwrap().kind() else {
        unreachable!()
    };
    DynamicMessage::new(element)
}
fn push_array(m: &mut DynamicMessage, name: &str, element: DynamicMessage) {
    let mut wrapper = message(m, name);
    let field = wrapper.descriptor().get_field(1).unwrap();
    let mut list = wrapper.get_field(&field).as_list().unwrap().to_vec();
    list.push(Value::Message(element));
    wrapper.set_field(&field, Value::List(list));
    m.set_field_by_name(name, Value::Message(wrapper));
}
fn array_values(m: &DynamicMessage, name: &str) -> Vec<DynamicMessage> {
    let wrapper = message(m, name);
    let field = wrapper.descriptor().get_field(1).unwrap();
    wrapper
        .get_field(&field)
        .as_list()
        .unwrap()
        .iter()
        .map(|v| v.as_message().unwrap().clone())
        .collect()
}
