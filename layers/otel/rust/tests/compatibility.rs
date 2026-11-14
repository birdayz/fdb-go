use base64::{Engine, engine::general_purpose::STANDARD};
use fdb_otel_rust::{ErrorKind, row::Schema};
use serde::Deserialize;
use std::collections::BTreeMap;

#[derive(Clone, Deserialize)]
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

#[derive(Deserialize)]
struct WireCase {
    name: String,
    input_otlp: String,
    outputs: Vec<Vector>,
}

#[derive(Deserialize)]
struct Malformed {
    name: String,
    input_otlp: String,
}

#[derive(Deserialize)]
struct FuzzCase {
    name: String,
    input_otlp: String,
    outputs: Vec<Vector>,
    decode_error: Option<String>,
}

#[derive(Deserialize)]
struct WireFixture {
    cases: Vec<WireCase>,
    malformed: Vec<Malformed>,
    fuzz_cases: Vec<FuzzCase>,
    fuzz_candidate_count: usize,
    fuzz_filtered_count: usize,
}

fn wire_fixture() -> WireFixture {
    serde_json::from_slice(include_bytes!("../testdata/wire_vectors.json")).unwrap()
}

fn assert_outputs(name: &str, prepared: &[fdb_otel_rust::row::PreparedSpan], outputs: &[Vector]) {
    assert_eq!(prepared.len(), outputs.len(), "{name} output count");
    for (item, v) in prepared.iter().zip(outputs) {
        assert_eq!(
            item.otlp,
            STANDARD.decode(&v.otlp).unwrap(),
            "{} OTLP",
            v.name
        );
        assert_eq!(item.row, STANDARD.decode(&v.row).unwrap(), "{} row", v.name);
        assert_eq!(
            hex::encode(&item.primary_key),
            v.pk,
            "{} primary key",
            v.name
        );
        let keys: BTreeMap<_, _> = item
            .indexes
            .iter()
            .map(|(n, e)| (n.clone(), e.iter().map(hex::encode).collect::<Vec<_>>()))
            .collect();
        assert_eq!(keys, v.keys, "{} index keys", v.name);
        let schema = Schema::pinned().unwrap();
        let stored = schema.decode_row(&item.row).unwrap();
        assert_eq!(
            schema.index_entries(&stored),
            item.indexes,
            "{} stored row indexes",
            v.name
        );
    }
}

#[test]
fn go_unknown_fields_survive_prepare_admit_and_reprepare() {
    use prost::Message;
    use prost_reflect::{DescriptorPool, DynamicMessage};
    let fixture = wire_fixture();
    assert_eq!(fixture.cases.len(), 15, "empty or changed wire fixture");
    let schema = Schema::pinned().unwrap();
    let pool =
        DescriptorPool::decode(include_bytes!("../testdata/descriptors.pb").as_slice()).unwrap();
    let rs_desc = pool
        .get_message_by_name("opentelemetry.proto.trace.v1.ResourceSpans")
        .unwrap();
    let (mut all_inputs, mut all_outputs) = (Vec::new(), Vec::new());
    for case in &fixture.cases {
        let input = STANDARD.decode(&case.input_otlp).unwrap();
        let expected: Vec<Vec<u8>> = case
            .outputs
            .iter()
            .map(|v| STANDARD.decode(&v.otlp).unwrap())
            .collect();
        for otlp in expected
            .iter()
            .filter(|_| case.name == "object_unknowns_at_every_node")
        {
            // The original raw-unknown case needs more than prost's unknown representation.
            let reencoded = DynamicMessage::decode(rs_desc.clone(), otlp.as_slice())
                .unwrap()
                .encode_to_vec();
            assert_ne!(&reencoded, otlp, "{} needs raw unknown bytes", case.name);
        }
        let input = std::slice::from_ref(&input);
        assert_outputs(&case.name, &schema.prepare(input).unwrap(), &case.outputs);
        let admitted = schema.admit(input).unwrap();
        assert_eq!(admitted, expected, "{} queue admit", case.name);
        assert_eq!(
            schema.validate(input).unwrap(),
            expected.len(),
            "{}",
            case.name
        );
        assert_outputs(
            &case.name,
            &schema.prepare(&admitted).unwrap(),
            &case.outputs,
        );
        assert_eq!(
            schema.admit(&admitted).unwrap(),
            expected,
            "{} re-admit",
            case.name
        );
        all_inputs.push(input[0].clone());
        all_outputs.extend(case.outputs.iter().cloned());
    }
    assert_eq!(all_outputs.len(), 22);
    assert_outputs(
        "all cases in one call",
        &schema.prepare(&all_inputs).unwrap(),
        &all_outputs,
    );
}

#[test]
fn go_rejected_wire_is_admission_for_prepare_admit_and_validate() {
    let fixture = wire_fixture();
    assert_eq!(
        fixture.malformed.len(),
        13,
        "empty or changed malformed fixture"
    );
    let schema = Schema::pinned().unwrap();
    let valid = STANDARD.decode(&fixture.cases[0].input_otlp).unwrap();
    for m in fixture.malformed {
        let bad = STANDARD.decode(&m.input_otlp).unwrap();
        for inputs in [vec![bad.clone()], vec![valid.clone(), bad]] {
            let name = &m.name;
            assert_eq!(
                schema.prepare(&inputs).unwrap_err().kind,
                ErrorKind::Admission,
                "{name}"
            );
            assert_eq!(
                schema.admit(&inputs).unwrap_err().kind,
                ErrorKind::Admission,
                "{name}"
            );
            assert_eq!(
                schema.validate(&inputs).unwrap_err().kind,
                ErrorKind::Admission,
                "{name}"
            );
        }
    }
}

#[test]
fn seeded_mutations_and_shapes_match_go_outputs_and_decode_errors() {
    let fixture = wire_fixture();
    assert_eq!(fixture.fuzz_candidate_count, 82);
    assert!(fixture.fuzz_filtered_count >= 4);
    assert_eq!(fixture.fuzz_cases.len() + fixture.fuzz_filtered_count, 82);
    let schema = Schema::pinned().unwrap();
    let (mut accepted, mut rejected) = (0, 0);
    for case in fixture.fuzz_cases {
        let wire = STANDARD.decode(&case.input_otlp).unwrap();
        let input = std::slice::from_ref(&wire);
        if let Some(error) = case.decode_error {
            assert!(!error.is_empty());
            assert!(case.outputs.is_empty());
            assert_eq!(
                schema.prepare(input).unwrap_err().kind,
                ErrorKind::Admission,
                "{}",
                case.name
            );
            assert_eq!(
                schema.admit(input).unwrap_err().kind,
                ErrorKind::Admission,
                "{}",
                case.name
            );
            assert_eq!(
                schema.validate(input).unwrap_err().kind,
                ErrorKind::Admission,
                "{}",
                case.name
            );
            rejected += 1;
        } else {
            assert!(!case.outputs.is_empty());
            assert_outputs(&case.name, &schema.prepare(input).unwrap(), &case.outputs);
            let expected: Vec<_> = case
                .outputs
                .iter()
                .map(|v| STANDARD.decode(&v.otlp).unwrap())
                .collect();
            let admitted = schema.admit(input).unwrap();
            assert_eq!(admitted, expected, "{} admit", case.name);
            assert_eq!(
                schema.validate(input).unwrap(),
                expected.len(),
                "{} validate",
                case.name
            );
            assert_outputs(
                &case.name,
                &schema.prepare(&admitted).unwrap(),
                &case.outputs,
            );
            accepted += 1;
        }
    }
    assert!(
        accepted >= 13 && rejected > 0,
        "accepted={accepted} rejected={rejected}"
    );
}
