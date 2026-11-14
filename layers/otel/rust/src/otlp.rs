//! Static OTLP v1.11 codec. Child IDs keep decoding, encoding and dropping deep
//! protobuf messages off the call stack; unknown fields retain Go's raw values.
use crate::{Error, ErrorKind, Result};

pub(crate) type Id = usize;
const MESSAGE_DEPTH: usize = 10_000;

#[derive(Clone, Debug)]
pub(crate) struct Node {
    pub(crate) data: Data,
    unknown: Vec<u8>,
}
#[derive(Clone, Debug)]
pub(crate) struct Frame {
    nodes: Vec<Node>,
    sizes: Vec<usize>,
    /// Single-span encodes of this request (shared by clones), so unit tests can show
    /// the write path encodes each span once.
    #[cfg(test)]
    encodes: std::sync::Arc<std::sync::atomic::AtomicUsize>,
}
#[derive(Clone, Copy)]
pub(crate) struct Selection {
    pub(crate) scope: Id,
    pub(crate) span: Id,
}
#[derive(Clone, Copy)]
enum Step {
    Done,
    Unknown,
    Child(Kind, Option<Id>),
}
#[derive(Clone, Copy)]
enum Field<'a> {
    Varint(u64),
    Fixed32(u32),
    Fixed64(u64),
    Bytes(&'a [u8]),
    Child(Id),
}
macro_rules! field_type {
    (bytes) => { Vec<u8> };
    (string) => { String };
    (uint32) => { u32 };
    (int32) => { i32 };
    (fixed32) => { u32 };
    (fixed64) => { u64 };
    (one($kind:ident)) => { Option<Id> };
    (many($kind:ident)) => { Vec<Id> };
    (strings) => { Vec<String> };
}
macro_rules! wire_type {
    (bytes) => {
        2
    };
    (string) => {
        2
    };
    (strings) => {
        2
    };
    (uint32) => {
        0
    };
    (int32) => {
        0
    };
    (fixed32) => {
        5
    };
    (fixed64) => {
        1
    };
    (one($kind:ident)) => {
        2
    };
    (many($kind:ident)) => {
        2
    };
}
macro_rules! read_field {
    ($field:expr, bytes, $buf:expr) => {{
        $field = take_bytes($buf)?.to_vec();
        Ok(Step::Done)
    }};
    ($field:expr, string, $buf:expr) => {{
        $field = take_string($buf)?;
        Ok(Step::Done)
    }};
    ($field:expr, strings, $buf:expr) => {{
        $field.push(take_string($buf)?);
        Ok(Step::Done)
    }};
    ($field:expr, uint32, $buf:expr) => {{
        $field = varint($buf)? as u32;
        Ok(Step::Done)
    }};
    ($field:expr, int32, $buf:expr) => {{
        $field = varint($buf)? as i32;
        Ok(Step::Done)
    }};
    ($field:expr, fixed32, $buf:expr) => {{
        $field = u32::from_le_bytes(take($buf, 4)?.try_into().unwrap());
        Ok(Step::Done)
    }};
    ($field:expr, fixed64, $buf:expr) => {{
        $field = u64::from_le_bytes(take($buf, 8)?.try_into().unwrap());
        Ok(Step::Done)
    }};
    ($field:expr, one($kind:ident), $buf:expr) => {
        Ok(Step::Child(Kind::$kind, $field))
    };
    ($field:expr, many($kind:ident), $buf:expr) => {
        Ok(Step::Child(Kind::$kind, None))
    };
}
macro_rules! attach_field {
    ($field:expr, one($kind:ident), $id:expr) => {
        $field = Some($id)
    };
    ($field:expr, many($kind:ident), $id:expr) => {
        $field.push($id)
    };
    ($field:expr, $scalar:ident, $id:expr) => {
        unreachable!("scalar child attachment")
    };
}
macro_rules! visit_field {
    ($visit:expr, $tag:expr, $field:expr, bytes) => {
        if !$field.is_empty() {
            $visit($tag, Field::Bytes(&$field));
        }
    };
    ($visit:expr, $tag:expr, $field:expr, string) => {
        if !$field.is_empty() {
            $visit($tag, Field::Bytes($field.as_bytes()));
        }
    };
    ($visit:expr, $tag:expr, $field:expr, strings) => {
        for s in &$field {
            $visit($tag, Field::Bytes(s.as_bytes()));
        }
    };
    ($visit:expr, $tag:expr, $field:expr, uint32) => {
        if $field != 0 {
            $visit($tag, Field::Varint($field as u64));
        }
    };
    ($visit:expr, $tag:expr, $field:expr, int32) => {
        if $field != 0 {
            $visit($tag, Field::Varint($field as i64 as u64));
        }
    };
    ($visit:expr, $tag:expr, $field:expr, fixed32) => {
        if $field != 0 {
            $visit($tag, Field::Fixed32($field));
        }
    };
    ($visit:expr, $tag:expr, $field:expr, fixed64) => {
        if $field != 0 {
            $visit($tag, Field::Fixed64($field));
        }
    };
    ($visit:expr, $tag:expr, $field:expr, one($kind:ident)) => {
        if let Some(id) = $field {
            $visit($tag, Field::Child(id));
        }
    };
    ($visit:expr, $tag:expr, $field:expr, many($kind:ident)) => {
        for &id in &$field {
            $visit($tag, Field::Child(id));
        }
    };
}
#[cfg(test)]
macro_rules! schema_kind {
    (one($kind:ident)) => {
        (stringify!($kind), false)
    };
    (many($kind:ident)) => {
        (stringify!($kind), true)
    };
    (strings) => {
        ("string", true)
    };
    ($scalar:ident) => {
        (stringify!($scalar), false)
    };
}
macro_rules! message {
    ($name:ident { $($tag:literal => $field:ident: $mode:ident $(($arg:ident))?;)* }) => {
        #[derive(Clone, Debug, Default)]
        pub(crate) struct $name { $(pub(crate) $field: field_type!($mode $(($arg))?),)* }
        impl $name {
            #[cfg(test)]
            pub(crate) const FIELDS: &'static [(u32, &'static str, (&'static str, bool))] = &[
                $(($tag, stringify!($field), schema_kind!($mode $(($arg))?)),)*
            ];
            fn read(&mut self, tag: u32, wt: u8, buf: &mut &[u8]) -> Result<Step> {
                let _ = &buf;
                match tag {
                    $($tag if wt == wire_type!($mode $(($arg))?) => read_field!(self.$field, $mode $(($arg))?, buf),)*
                    _ => Ok(Step::Unknown),
                }
            }
            fn attach(&mut self, tag: u32, id: Id) {
                let _ = id;
                match tag {
                    $($tag => attach_field!(self.$field, $mode $(($arg))?, id),)*
                    _ => unreachable!("unknown child attachment"),
                }
            }
            fn visit<'a>(&'a self, visit: &mut impl FnMut(u32, Field<'a>)) {
                $(visit_field!(visit, $tag, self.$field, $mode $(($arg))?);)*
            }
        }
    };
}
message!(ResourceSpans {
    1 => resource: one(Resource);
    2 => scope_spans: many(ScopeSpans);
    3 => schema_url: string;
});
message!(ScopeSpans {
    1 => scope: one(InstrumentationScope);
    2 => spans: many(Span);
    3 => schema_url: string;
});
message!(Span {
    1 => trace_id: bytes;
    2 => span_id: bytes;
    3 => trace_state: string;
    4 => parent_span_id: bytes;
    5 => name: string;
    6 => kind: int32;
    7 => start_time_unix_nano: fixed64;
    8 => end_time_unix_nano: fixed64;
    9 => attributes: many(KeyValue);
    10 => dropped_attributes_count: uint32;
    11 => events: many(Event);
    12 => dropped_events_count: uint32;
    13 => links: many(Link);
    14 => dropped_links_count: uint32;
    15 => status: one(Status);
    16 => flags: fixed32;
});
message!(Event {
    1 => time_unix_nano: fixed64;
    2 => name: string;
    3 => attributes: many(KeyValue);
    4 => dropped_attributes_count: uint32;
});
message!(Link {
    1 => trace_id: bytes;
    2 => span_id: bytes;
    3 => trace_state: string;
    4 => attributes: many(KeyValue);
    5 => dropped_attributes_count: uint32;
    6 => flags: fixed32;
});
message!(Status { 2 => message: string; 3 => code: int32; });
message!(Resource {
    1 => attributes: many(KeyValue);
    2 => dropped_attributes_count: uint32;
    3 => entity_refs: many(EntityRef);
});
message!(EntityRef {
    1 => schema_url: string;
    2 => r#type: string;
    3 => id_keys: strings;
    4 => description_keys: strings;
});
message!(InstrumentationScope {
    1 => name: string;
    2 => version: string;
    3 => attributes: many(KeyValue);
    4 => dropped_attributes_count: uint32;
});
message!(KeyValue { 1 => key: string; 2 => value: one(AnyValue); 3 => key_strindex: int32; });
message!(ArrayValue { 1 => values: many(AnyValue); });
message!(KeyValueList { 1 => values: many(KeyValue); });

#[derive(Clone, Debug, Default)]
pub(crate) struct AnyValue {
    pub(crate) value: Option<Value>,
}
#[derive(Clone, Debug)]
pub(crate) enum Value {
    String(String),
    Bool(bool),
    Int(i64),
    Double(f64),
    Array(Id),
    Kvlist(Id),
    Bytes(Vec<u8>),
    StringIndex(i32),
}
impl AnyValue {
    fn read(&mut self, tag: u32, wt: u8, buf: &mut &[u8]) -> Result<Step> {
        self.value = Some(match (tag, wt) {
            (1, 2) => Value::String(take_string(buf)?),
            (2, 0) => Value::Bool(varint(buf)? != 0),
            (3, 0) => Value::Int(varint(buf)? as i64),
            (4, 1) => Value::Double(f64::from_bits(u64::from_le_bytes(
                take(buf, 8)?.try_into().unwrap(),
            ))),
            (5, 2) => {
                return Ok(Step::Child(
                    Kind::ArrayValue,
                    match self.value {
                        Some(Value::Array(id)) => Some(id),
                        _ => None,
                    },
                ));
            }
            (6, 2) => {
                return Ok(Step::Child(
                    Kind::KeyValueList,
                    match self.value {
                        Some(Value::Kvlist(id)) => Some(id),
                        _ => None,
                    },
                ));
            }
            (7, 2) => Value::Bytes(take_bytes(buf)?.to_vec()),
            (8, 0) => Value::StringIndex(varint(buf)? as i32),
            _ => return Ok(Step::Unknown),
        });
        Ok(Step::Done)
    }
    fn attach(&mut self, tag: u32, id: Id) {
        self.value = Some(match tag {
            5 => Value::Array(id),
            6 => Value::Kvlist(id),
            _ => unreachable!("oneof child attachment"),
        });
    }
    fn visit<'a>(&'a self, visit: &mut impl FnMut(u32, Field<'a>)) {
        match &self.value {
            Some(Value::String(s)) => visit(1, Field::Bytes(s.as_bytes())),
            Some(Value::Bool(v)) => visit(2, Field::Varint(u64::from(*v))),
            Some(Value::Int(v)) => visit(3, Field::Varint(*v as u64)),
            Some(Value::Double(v)) => visit(4, Field::Fixed64(v.to_bits())),
            Some(Value::Array(id)) => visit(5, Field::Child(*id)),
            Some(Value::Kvlist(id)) => visit(6, Field::Child(*id)),
            Some(Value::Bytes(v)) => visit(7, Field::Bytes(v)),
            Some(Value::StringIndex(v)) => visit(8, Field::Varint(*v as i64 as u64)),
            None => (),
        }
    }
}
/// A node's stored type: `boxed` keeps a large message behind a pointer so the arena's
/// uniform node stays small for the many small messages (attributes and values).
macro_rules! stored {
    ($kind:ident boxed) => { Box<$kind> };
    ($kind:ident) => { $kind };
}
macro_rules! graph {
    ($($kind:ident $(=> $get:ident)? $([$boxed:ident])?),* $(,)?) => {
        #[derive(Clone, Copy)]
        enum Kind { $($kind),* }
        #[derive(Clone, Debug)]
        pub(crate) enum Data { $($kind(stored!($kind $($boxed)?))),* }
        impl Kind {
            fn node(self) -> Node { Node { data: match self { $(Self::$kind => Data::$kind($kind::default().into())),* }, unknown: Vec::new() } }
        }
        impl Data {
            fn read(&mut self, tag: u32, wt: u8, buf: &mut &[u8]) -> Result<Step> {
                match self { $(Self::$kind(m) => m.read(tag, wt, buf)),* }
            }
            fn attach(&mut self, tag: u32, id: Id) { match self { $(Self::$kind(m) => m.attach(tag, id)),* } }
            fn visit<'a>(&'a self, visit: &mut impl FnMut(u32, Field<'a>)) { match self { $(Self::$kind(m) => m.visit(visit)),* } }
        }
        impl Frame { $($(pub(crate) fn $get(&self, id: Id) -> &$kind {
            match &self.nodes[id].data { Data::$kind(m) => m, _ => unreachable!("typed arena edge") }
        })?)* }
    };
}
graph!(ResourceSpans => resource_spans, ScopeSpans => scope_spans, Span => span [boxed],
    Event => event, Link => link [boxed], Status => status, Resource => resource,
    EntityRef [boxed], InstrumentationScope => scope, KeyValue => key_value,
    AnyValue => any_value, ArrayValue => array, KeyValueList => kvlist);

impl Frame {
    pub(crate) fn decode(input: &[u8]) -> Result<Self> {
        let mut frame = Self {
            nodes: vec![Kind::ResourceSpans.node()],
            sizes: Vec::new(),
            #[cfg(test)]
            encodes: Default::default(),
        };
        let mut pending = vec![(0, input, 1)];
        while let Some((id, mut buf, depth)) = pending.pop() {
            while !buf.is_empty() {
                let (tag, wt) = key(&mut buf, (1 << 29) - 1)?;
                let start = buf;
                match frame.nodes[id].data.read(tag, wt, &mut buf)? {
                    Step::Done => (),
                    Step::Unknown => {
                        skip(tag, wt, &mut buf)?;
                        put_varint(
                            ((tag as u64) << 3) | wt as u64,
                            &mut frame.nodes[id].unknown,
                        );
                        frame.nodes[id]
                            .unknown
                            .extend_from_slice(&start[..start.len() - buf.len()]);
                    }
                    Step::Child(kind, existing) => {
                        let payload = take_bytes(&mut buf)?;
                        if depth >= MESSAGE_DEPTH {
                            return Err(invalid());
                        }
                        let child = existing.unwrap_or_else(|| {
                            frame.nodes.push(kind.node());
                            frame.nodes.len() - 1
                        });
                        frame.nodes[id].data.attach(tag, child);
                        pending.push((id, buf, depth));
                        pending.push((child, payload, depth + 1));
                        break;
                    }
                }
            }
        }
        // A request is kept (queued) until its consumer encodes it: keep no spare capacity.
        frame.nodes.shrink_to_fit();
        frame.sizes.resize(frame.nodes.len(), 0);
        for id in (0..frame.nodes.len()).rev() {
            frame.sizes[id] = frame.size(id, None, &frame.sizes);
        }
        Ok(frame)
    }
    pub(crate) fn root(&self) -> &ResourceSpans {
        self.resource_spans(0)
    }
    pub(crate) fn span_count(&self) -> usize {
        self.root()
            .scope_spans
            .iter()
            .map(|&scope| self.scope_spans(scope).spans.len())
            .sum()
    }
    fn fields<'a>(
        &'a self,
        id: Id,
        select: Option<Selection>,
        visit: &mut impl FnMut(u32, Field<'a>),
    ) {
        if let Some(s) = select {
            if id == 0 {
                let rs = self.root();
                if let Some(resource) = rs.resource {
                    visit(1, Field::Child(resource));
                }
                visit(2, Field::Child(s.scope));
                if !rs.schema_url.is_empty() {
                    visit(3, Field::Bytes(rs.schema_url.as_bytes()));
                }
                return;
            }
            if id == s.scope {
                let ss = self.scope_spans(id);
                if let Some(scope) = ss.scope {
                    visit(1, Field::Child(scope));
                }
                visit(2, Field::Child(s.span));
                if !ss.schema_url.is_empty() {
                    visit(3, Field::Bytes(ss.schema_url.as_bytes()));
                }
                return;
            }
        }
        self.nodes[id].data.visit(visit);
    }
    fn size(&self, id: Id, select: Option<Selection>, sizes: &[usize]) -> usize {
        let mut size = self.nodes[id].unknown.len();
        self.fields(id, select, &mut |tag, field| {
            size += varint_len((tag as u64) << 3)
                + match field {
                    Field::Varint(v) => varint_len(v),
                    Field::Fixed32(_) => 4,
                    Field::Fixed64(_) => 8,
                    Field::Bytes(v) => varint_len(v.len() as u64) + v.len(),
                    Field::Child(child) => varint_len(sizes[child] as u64) + sizes[child],
                };
        });
        size
    }
    #[cfg(test)]
    pub(crate) fn encodes(&self) -> usize {
        self.encodes.load(std::sync::atomic::Ordering::SeqCst)
    }
    pub(crate) fn single(&self, select: Selection) -> Vec<u8> {
        #[cfg(test)]
        self.encodes
            .fetch_add(1, std::sync::atomic::Ordering::SeqCst);
        let scope_size = self.size(select.scope, Some(select), &self.sizes);
        // Only the root's selected-scope length differs from the base arena sizes.
        let mut root_size = self.nodes[0].unknown.len();
        self.fields(0, Some(select), &mut |tag, field| {
            let n = match field {
                Field::Child(id) if id == select.scope => scope_size,
                Field::Child(id) => self.sizes[id],
                Field::Bytes(b) => b.len(),
                _ => unreachable!(),
            };
            root_size += varint_len((tag as u64) << 3) + varint_len(n as u64) + n;
        });
        enum Work<'a> {
            Node(Id),
            Field(u32, Field<'a>),
            Raw(&'a [u8]),
        }
        let mut out = Vec::with_capacity(root_size);
        let mut pending = vec![Work::Node(0)];
        while let Some(work) = pending.pop() {
            match work {
                Work::Raw(bytes) => out.extend_from_slice(bytes),
                Work::Node(id) => {
                    pending.push(Work::Raw(&self.nodes[id].unknown));
                    let start = pending.len();
                    self.fields(id, Some(select), &mut |tag, f| {
                        pending.push(Work::Field(tag, f))
                    });
                    pending[start..].reverse();
                }
                Work::Field(tag, field) => {
                    let wt = match field {
                        Field::Varint(_) => 0,
                        Field::Fixed64(_) => 1,
                        Field::Bytes(_) | Field::Child(_) => 2,
                        Field::Fixed32(_) => 5,
                    };
                    put_varint(((tag as u64) << 3) | wt, &mut out);
                    match field {
                        Field::Varint(v) => put_varint(v, &mut out),
                        Field::Fixed32(v) => out.extend_from_slice(&v.to_le_bytes()),
                        Field::Fixed64(v) => out.extend_from_slice(&v.to_le_bytes()),
                        Field::Bytes(bytes) => {
                            put_varint(bytes.len() as u64, &mut out);
                            out.extend_from_slice(bytes);
                        }
                        Field::Child(id) => {
                            put_varint(
                                if id == select.scope {
                                    scope_size
                                } else {
                                    self.sizes[id]
                                } as u64,
                                &mut out,
                            );
                            pending.push(Work::Node(id));
                        }
                    }
                }
            }
        }
        debug_assert_eq!(out.len(), root_size);
        out
    }
}
fn invalid() -> Error {
    Error::new(ErrorKind::Admission, "invalid ResourceSpans protobuf")
}
fn varint(buf: &mut &[u8]) -> Result<u64> {
    let mut out = 0;
    for i in 0..10 {
        let b = *buf.first().ok_or_else(invalid)?;
        *buf = &buf[1..];
        if i == 9 && b > 1 {
            return Err(invalid());
        }
        out |= ((b & 0x7f) as u64) << (i * 7);
        if b < 0x80 {
            return Ok(out);
        }
    }
    Err(invalid())
}
fn key(buf: &mut &[u8], max: u32) -> Result<(u32, u8)> {
    let v = varint(buf)?;
    let tag = v >> 3;
    if tag == 0 || tag > max as u64 || v & 7 > 5 {
        return Err(invalid());
    }
    Ok((tag as u32, (v & 7) as u8))
}
fn take<'a>(buf: &mut &'a [u8], n: usize) -> Result<&'a [u8]> {
    if n > buf.len() {
        return Err(invalid());
    }
    let (value, rest) = buf.split_at(n);
    *buf = rest;
    Ok(value)
}
fn take_bytes<'a>(buf: &mut &'a [u8]) -> Result<&'a [u8]> {
    let n = usize::try_from(varint(buf)?).map_err(|_| invalid())?;
    take(buf, n)
}
fn take_string(buf: &mut &[u8]) -> Result<String> {
    std::str::from_utf8(take_bytes(buf)?)
        .map(str::to_owned)
        .map_err(|_| invalid())
}
fn skip_value(wt: u8, buf: &mut &[u8]) -> Result<()> {
    match wt {
        0 => {
            varint(buf)?;
        }
        1 => {
            take(buf, 8)?;
        }
        2 => {
            take_bytes(buf)?;
        }
        5 => {
            take(buf, 4)?;
        }
        _ => return Err(invalid()),
    }
    Ok(())
}
fn skip(tag: u32, wt: u8, buf: &mut &[u8]) -> Result<()> {
    if wt != 3 {
        return skip_value(wt, buf);
    }
    let mut groups = vec![tag];
    while !groups.is_empty() {
        let (tag, wt) = key(buf, i32::MAX as u32)?;
        match wt {
            3 => {
                if groups.len() > 10_000 {
                    return Err(invalid());
                }
                groups.push(tag);
            }
            4 => {
                if groups.pop() != Some(tag) {
                    return Err(invalid());
                }
            }
            _ => skip_value(wt, buf)?,
        }
    }
    Ok(())
}
fn varint_len(v: u64) -> usize {
    ((64 - v.leading_zeros()).max(1) as usize).div_ceil(7)
}
fn put_varint(mut v: u64, out: &mut Vec<u8>) {
    while v >= 0x80 {
        out.push(v as u8 | 0x80);
        v >>= 7;
    }
    out.push(v as u8);
}
