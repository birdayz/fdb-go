use base64::{Engine, engine::general_purpose::STANDARD};
use fdb_otel_rust::{ErrorKind, row::Schema};
use serde::Deserialize;
use std::collections::BTreeMap;

#[derive(Deserialize)]
struct Vector {
    name: String,
    otlp: String,
    row: String,
    pk: String,
    keys: BTreeMap<String, Vec<String>>,
}

#[test]
fn go_compiled_rows_and_metadata_expression_keys() {
    let vectors: Vec<Vector> =
        serde_json::from_slice(include_bytes!("../testdata/vectors.json")).unwrap();
    assert_eq!(vectors.len(), 9, "empty or changed fixture population");
    let schema = Schema::pinned().unwrap();
    for v in vectors {
        let otlp = STANDARD.decode(v.otlp).unwrap();
        let prepared = schema.prepare(std::slice::from_ref(&otlp)).unwrap();
        assert_eq!(prepared.len(), 1, "{}", v.name);
        let item = &prepared[0];
        assert_eq!(
            hex::encode(&item.primary_key),
            v.pk,
            "{} primary key",
            v.name
        );
        assert_eq!(item.row, STANDARD.decode(v.row).unwrap(), "{} row", v.name);
        assert_eq!(
            item.otlp, otlp,
            "{} OTLP field/unknown/attribute fidelity",
            v.name
        );
        let keys: BTreeMap<_, _> = item
            .indexes
            .iter()
            .map(|(n, entries)| {
                (
                    n.clone(),
                    entries.iter().map(hex::encode).collect::<Vec<_>>(),
                )
            })
            .collect();
        assert_eq!(keys, v.keys, "{} full index keys", v.name);
        if v.name.starts_with("v2_") {
            assert_eq!(item.indexes.values().map(Vec::len).sum::<usize>(), 10);
        }
        assert!(item.estimate <= 300_000);
    }
}

#[test]
fn malformed_identity_start_and_call_deduplication() {
    let schema = Schema::pinned().unwrap();
    let vectors: Vec<Vector> =
        serde_json::from_slice(include_bytes!("../testdata/vectors.json")).unwrap();
    let otlp = STANDARD.decode(&vectors[1].otlp).unwrap();
    assert_eq!(
        schema.prepare(&[otlp.clone(), otlp.clone()]).unwrap().len(),
        1
    );
    for invalid in [vec![0xff], vec![0x12, 0x02, 0x12, 0x00]] {
        assert_eq!(
            schema.prepare(&[otlp.clone(), invalid]).unwrap_err().kind,
            ErrorKind::Admission
        );
    }
    assert!(schema.prepare(&[]).unwrap().is_empty());
    use prost::Message;
    use prost_reflect::{DescriptorPool, DynamicMessage, Value};
    let pool =
        DescriptorPool::decode(include_bytes!("../testdata/descriptors.pb").as_slice()).unwrap();
    let mut rs = DynamicMessage::decode(
        pool.get_message_by_name("opentelemetry.proto.trace.v1.ResourceSpans")
            .unwrap(),
        otlp.as_slice(),
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
    sp.set_field_by_name("start_time_unix_nano", Value::U64(i64::MAX as u64 + 1));
    ss.set_field_by_name("spans", Value::List(vec![Value::Message(sp)]));
    rs.set_field_by_name("scope_spans", Value::List(vec![Value::Message(ss)]));
    assert_eq!(
        schema.prepare(&[rs.encode_to_vec()]).unwrap_err().kind,
        ErrorKind::Admission
    );
    assert_eq!(
        schema.admit(&[rs.encode_to_vec()]).unwrap_err().kind,
        ErrorKind::Admission
    );
    assert_eq!(
        schema.validate(&[rs.encode_to_vec()]).unwrap_err().kind,
        ErrorKind::Admission
    );
}
