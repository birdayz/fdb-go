//! Go stores each message's unknown fields as their original bytes, in order, after its
//! canonical known fields; this module reproduces that for the pinned OTLP graph.
use crate::{Error, ErrorKind, Result};
use prost::encoding::{
    DecodeContext, WireType, decode_key, decode_varint, encode_key, encode_varint, skip_field,
};
use prost_reflect::{DynamicMessage, Kind, MessageDescriptor, Value};
use std::collections::{BTreeMap, HashSet};

/// One message's raw unknown field records and the nodes of its known message fields.
#[derive(Clone, Debug, Default)]
pub(crate) struct Node {
    unknown: Vec<u8>,
    children: BTreeMap<u32, Child>,
}

#[derive(Clone, Debug)]
enum Child {
    One(Node),
    Many(Vec<Node>),
}

fn invalid() -> Error {
    Error::new(ErrorKind::Admission, "invalid ResourceSpans protobuf")
}

fn misaligned() -> Error {
    Error::new(
        ErrorKind::Admission,
        "OTLP unknown fields do not align with the decoded message",
    )
}

/// Rejects a reachable map, group or extension field, which `scan` and `patch` do not model.
pub(crate) fn check_graph(root: &MessageDescriptor) -> Result<()> {
    let mut seen = HashSet::new();
    let mut stack = vec![root.clone()];
    while let Some(desc) = stack.pop() {
        if !seen.insert(desc.full_name().to_owned()) {
            continue;
        }
        if desc.extensions().next().is_some() {
            return Err(Error::new(ErrorKind::Config, "OTLP extension field"));
        }
        for field in desc.fields() {
            if field.is_map() || field.is_group() {
                return Err(Error::new(ErrorKind::Config, "OTLP map or group field"));
            }
            if let Kind::Message(child) = field.kind() {
                stack.push(child);
            }
        }
    }
    Ok(())
}

/// Records `wire`'s unknown field bytes per message node, merging singular message
/// occurrences and dropping replaced oneof arms as protobuf decoding does.
pub(crate) fn scan(desc: &MessageDescriptor, wire: &[u8]) -> Result<Node> {
    let mut node = Node::default();
    scan_into(desc, wire, &mut node, 0)?;
    Ok(node)
}

fn scan_into(desc: &MessageDescriptor, mut buf: &[u8], node: &mut Node, depth: u32) -> Result<()> {
    if depth > 100 {
        return Err(invalid());
    }
    while !buf.is_empty() {
        let start = buf;
        let (tag, wire_type) = decode_key(&mut buf).map_err(|_| invalid())?;
        let Some(field) = desc.get_field(tag) else {
            skip_field(wire_type, tag, &mut buf, DecodeContext::default())
                .map_err(|_| invalid())?;
            node.unknown
                .extend_from_slice(&start[..start.len() - buf.len()]);
            continue;
        };
        if let Some(oneof) = field.containing_oneof() {
            for other in oneof.fields().filter(|f| f.number() != tag) {
                node.children.remove(&other.number());
            }
        }
        match field.kind() {
            Kind::Message(child_desc) if wire_type == WireType::LengthDelimited => {
                let len = usize::try_from(decode_varint(&mut buf).map_err(|_| invalid())?)
                    .map_err(|_| invalid())?;
                if len > buf.len() {
                    return Err(invalid());
                }
                let (payload, rest) = buf.split_at(len);
                buf = rest;
                let child = if field.is_list() {
                    let Child::Many(list) = node
                        .children
                        .entry(tag)
                        .or_insert_with(|| Child::Many(Vec::new()))
                    else {
                        return Err(misaligned());
                    };
                    list.push(Node::default());
                    list.last_mut().expect("just pushed")
                } else {
                    let Child::One(child) = node
                        .children
                        .entry(tag)
                        .or_insert_with(|| Child::One(Node::default()))
                    else {
                        return Err(misaligned());
                    };
                    child
                };
                scan_into(&child_desc, payload, child, depth + 1)?;
            }
            _ => skip_field(wire_type, tag, &mut buf, DecodeContext::default())
                .map_err(|_| invalid())?,
        }
    }
    Ok(())
}

/// Drains every parsed unknown field, so encoding yields canonical known fields only.
pub(crate) fn strip(message: &mut DynamicMessage) {
    message.take_unknown_fields().for_each(drop);
    for (_, value) in message.fields_mut() {
        match value {
            Value::Message(child) => strip(child),
            Value::List(items) => {
                for item in items {
                    if let Value::Message(child) = item {
                        strip(child);
                    }
                }
            }
            _ => {}
        }
    }
}

/// The node of the single-span wrapper keeping only scope `scope` of field `scopes` and
/// its span `span` of field `spans`; every other node keeps its unknown bytes.
pub(crate) fn select(
    root: &Node,
    scopes: u32,
    scope: usize,
    spans: u32,
    span: usize,
) -> Result<Node> {
    fn pick(node: &Node, field: u32, i: usize) -> Result<&Node> {
        match node.children.get(&field) {
            Some(Child::Many(list)) => list.get(i).ok_or_else(misaligned),
            _ => Err(misaligned()),
        }
    }
    let scope_node = pick(root, scopes, scope)?;
    let mut single_scope = Node {
        unknown: scope_node.unknown.clone(),
        children: scope_node
            .children
            .iter()
            .filter(|(field, _)| **field != spans)
            .map(|(field, child)| (*field, child.clone()))
            .collect(),
    };
    single_scope.children.insert(
        spans,
        Child::Many(vec![pick(scope_node, spans, span)?.clone()]),
    );
    let mut single = Node {
        unknown: root.unknown.clone(),
        children: root
            .children
            .iter()
            .filter(|(field, _)| **field != scopes)
            .map(|(field, child)| (*field, child.clone()))
            .collect(),
    };
    single
        .children
        .insert(scopes, Child::Many(vec![single_scope]));
    Ok(single)
}

/// Re-emits `canonical` (known fields only) with every message's recorded unknown bytes
/// appended after its known fields, rebuilding each enclosing length.
pub(crate) fn patch(desc: &MessageDescriptor, canonical: &[u8], node: &Node) -> Result<Vec<u8>> {
    let mut buf = canonical;
    let mut out = Vec::with_capacity(canonical.len() + node.unknown.len());
    let mut used: BTreeMap<u32, usize> = BTreeMap::new();
    while !buf.is_empty() {
        let start = buf;
        let (tag, wire_type) = decode_key(&mut buf).map_err(|_| misaligned())?;
        let field = desc.get_field(tag).ok_or_else(misaligned)?;
        match field.kind() {
            Kind::Message(child_desc) if wire_type == WireType::LengthDelimited => {
                let len = usize::try_from(decode_varint(&mut buf).map_err(|_| misaligned())?)
                    .map_err(|_| misaligned())?;
                if len > buf.len() {
                    return Err(misaligned());
                }
                let (payload, rest) = buf.split_at(len);
                buf = rest;
                let index = used.entry(tag).or_default();
                let child = match node.children.get(&tag) {
                    Some(Child::One(child)) if !field.is_list() && *index == 0 => child,
                    Some(Child::Many(list)) if field.is_list() => {
                        list.get(*index).ok_or_else(misaligned)?
                    }
                    _ => return Err(misaligned()),
                };
                *index += 1;
                let patched = patch(&child_desc, payload, child)?;
                encode_key(tag, WireType::LengthDelimited, &mut out);
                encode_varint(patched.len() as u64, &mut out);
                out.extend_from_slice(&patched);
            }
            _ => {
                skip_field(wire_type, tag, &mut buf, DecodeContext::default())
                    .map_err(|_| misaligned())?;
                out.extend_from_slice(&start[..start.len() - buf.len()]);
            }
        }
    }
    for (tag, child) in &node.children {
        let expected = match child {
            Child::One(_) => 1,
            Child::Many(list) => list.len(),
        };
        if used.get(tag).copied().unwrap_or(0) != expected {
            return Err(misaligned());
        }
    }
    out.extend_from_slice(&node.unknown);
    Ok(out)
}
