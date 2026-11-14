//! Whole-call admission and split/replacement planning without a live database.
use base64::{Engine, engine::general_purpose::STANDARD};
use fdb_otel_rust::{
    ErrorKind,
    row::Schema,
    storage::{self, RecordCompression},
};
use prost::Message;
use prost_reflect::{DescriptorPool, DynamicMessage, Value};
use serde::Deserialize;
#[derive(Deserialize)]
struct Vector {
    name: String,
    otlp: String,
}
fn large_span(name_bytes: usize) -> Vec<u8> {
    let vectors: Vec<Vector> =
        serde_json::from_slice(include_bytes!("../testdata/vectors.json")).unwrap();
    let wire = STANDARD
        .decode(&vectors.iter().find(|v| v.name == "v2_0").unwrap().otlp)
        .unwrap();
    let pool =
        DescriptorPool::decode(include_bytes!("../testdata/descriptors.pb").as_slice()).unwrap();
    let mut rs = DynamicMessage::decode(
        pool.get_message_by_name("opentelemetry.proto.trace.v1.ResourceSpans")
            .unwrap(),
        wire.as_slice(),
    )
    .unwrap();
    let mut ss = rs
        .get_field_by_name("scope_spans")
        .unwrap()
        .as_list()
        .unwrap()[0]
        .as_message()
        .unwrap()
        .clone();
    let mut sp = ss.get_field_by_name("spans").unwrap().as_list().unwrap()[0]
        .as_message()
        .unwrap()
        .clone();
    sp.set_field_by_name("name", Value::String("x".repeat(name_bytes)));
    ss.set_field_by_name("spans", Value::List(vec![Value::Message(sp)]));
    rs.set_field_by_name("scope_spans", Value::List(vec![Value::Message(ss)]));
    rs.encode_to_vec()
}
#[test]
fn admitted_record_splits_without_omitting_any_index() {
    let source = large_span(110_000);
    let schema = Schema::pinned().unwrap();
    let prepared = schema.prepare(&[source]).unwrap();
    assert_eq!(prepared.len(), 1);
    assert_eq!(
        prepared[0].indexes.values().map(Vec::len).sum::<usize>(),
        10
    );
    let stored = storage::encode(&prepared[0].row, RecordCompression::None).unwrap();
    let chunks = storage::split(&stored);
    assert_eq!(chunks.len(), 3);
    assert_eq!(chunks[0].0, 1);
    assert_eq!(chunks[1].0, 2);
    assert_eq!(chunks[2].0, 3);
    assert_eq!(chunks[0].1.len(), 100_000);
    assert_eq!(chunks[1].1.len(), 100_000);
    assert_eq!(
        storage::decode(
            &chunks
                .into_iter()
                .flat_map(|(_, bytes)| bytes)
                .collect::<Vec<_>>()
        )
        .unwrap(),
        prepared[0].row
    );
}
#[test]
fn oversized_single_span_fails_the_entire_preparation() {
    let source = large_span(160_000);
    let schema = Schema::pinned().unwrap();
    assert_eq!(
        schema.prepare(&[large_span(10), source]).unwrap_err().kind,
        ErrorKind::Admission
    );
}
