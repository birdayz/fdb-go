use crate::{otlp::*, records as r, row::Schema};
use base64::{Engine, engine::general_purpose::STANDARD};
use prost_reflect::{DescriptorPool, Kind};

fn kind_matches(name: &str, kind: Kind) -> bool {
    match name {
        "bytes" => matches!(kind, Kind::Bytes),
        "string" => matches!(kind, Kind::String),
        "uint32" => matches!(kind, Kind::Uint32),
        "int32" => matches!(kind, Kind::Int32 | Kind::Enum(_)),
        "int64" => matches!(kind, Kind::Int64),
        "fixed64" => matches!(kind, Kind::Fixed64),
        "fixed32" => matches!(kind, Kind::Fixed32),
        "double" => matches!(kind, Kind::Double),
        "bool" => matches!(kind, Kind::Bool),
        _ => matches!(kind, Kind::Message(m) if m.name() == name),
    }
}
#[test]
fn static_codec_covers_every_pinned_otlp_and_row_field() {
    let pool =
        DescriptorPool::decode(include_bytes!("../testdata/descriptors.pb").as_slice()).unwrap();
    let graphs = [
        ("trace.v1.ResourceSpans", ResourceSpans::FIELDS),
        ("trace.v1.ScopeSpans", ScopeSpans::FIELDS),
        ("trace.v1.Span", Span::FIELDS),
        ("trace.v1.Span.Event", Event::FIELDS),
        ("trace.v1.Span.Link", Link::FIELDS),
        ("trace.v1.Status", Status::FIELDS),
        ("resource.v1.Resource", Resource::FIELDS),
        ("common.v1.EntityRef", EntityRef::FIELDS),
        (
            "common.v1.InstrumentationScope",
            InstrumentationScope::FIELDS,
        ),
        ("common.v1.KeyValue", KeyValue::FIELDS),
        ("common.v1.ArrayValue", ArrayValue::FIELDS),
        ("common.v1.KeyValueList", KeyValueList::FIELDS),
    ];
    assert_eq!(graphs.len(), 12);
    for (name, fields) in graphs {
        let desc = pool
            .get_message_by_name(&format!("opentelemetry.proto.{name}"))
            .unwrap();
        assert_eq!(desc.fields().count(), fields.len(), "{name}");
        assert_eq!(desc.oneofs().count(), 0, "{name}");
        for &(tag, name, (kind, repeated)) in fields {
            let field = desc.get_field(tag).unwrap();
            assert_eq!(field.name(), name.strip_prefix("r#").unwrap_or(name));
            assert!(
                kind_matches(kind, field.kind()),
                "{}:{tag}",
                desc.full_name()
            );
            assert_eq!(field.is_list(), repeated, "{}:{tag}", desc.full_name());
            assert_eq!(
                field.supports_presence(),
                !repeated && matches!(field.kind(), Kind::Message(_)),
                "{}:{tag}",
                desc.full_name()
            );
            assert!(!field.is_map() && !field.is_group());
        }
    }
    let any = pool
        .get_message_by_name("opentelemetry.proto.common.v1.AnyValue")
        .unwrap();
    assert_eq!(any.fields().count(), 8);
    assert_eq!(any.oneofs().count(), 1);
    for (i, kind) in [
        "string",
        "bool",
        "int64",
        "double",
        "ArrayValue",
        "KeyValueList",
        "bytes",
        "int32",
    ]
    .iter()
    .enumerate()
    {
        let field = any.get_field(i as u32 + 1).unwrap();
        assert!(field.containing_oneof().is_some() && kind_matches(kind, field.kind()));
    }
    let records = [
        ("SPANS", r::Spans::FIELDS),
        ("SPAN_EVENT", r::SpanEvent::FIELDS),
        ("SPAN_LINK", r::SpanLink::FIELDS),
        ("STR_ATTR", r::StrAttr::FIELDS),
        ("LONG_STR_ATTR", r::LongStrAttr::FIELDS),
        ("INT_ATTR", r::IntAttr::FIELDS),
        ("DOUBLE_ATTR", r::DoubleAttr::FIELDS),
        ("BOOL_ATTR", r::BoolAttr::FIELDS),
        ("BYTES_ATTR", r::BytesAttr::FIELDS),
        ("ANY_ATTR", r::AnyAttr::FIELDS),
        ("MESSAGE_REF_CHUNK", r::RefChunk::FIELDS),
    ];
    assert_eq!(records.len(), 11);
    let child_names = [
        ("StrAttr", "STR_ATTR"),
        ("LongStrAttr", "LONG_STR_ATTR"),
        ("IntAttr", "INT_ATTR"),
        ("DoubleAttr", "DOUBLE_ATTR"),
        ("BoolAttr", "BOOL_ATTR"),
        ("BytesAttr", "BYTES_ATTR"),
        ("AnyAttr", "ANY_ATTR"),
        ("SpanEvent", "SPAN_EVENT"),
        ("SpanLink", "SPAN_LINK"),
        ("RefChunk", "MESSAGE_REF_CHUNK"),
    ];
    for (name, fields) in records {
        let desc = pool.get_message_by_name(name).unwrap();
        assert_eq!(desc.fields().count(), fields.len(), "{name}");
        for &(tag, kind, ty) in fields {
            let field = desc.get_field(tag.parse().unwrap()).unwrap();
            assert!(
                field.supports_presence() && !field.is_list(),
                "{name}:{tag}"
            );
            if kind != "message" {
                assert!(kind_matches(kind, field.kind()), "{name}:{tag}");
                continue;
            }
            let Kind::Message(wrapper) = field.kind() else {
                panic!("{name}:{tag} not message")
            };
            assert_eq!(wrapper.fields().count(), 1);
            let value = wrapper.get_field(1).unwrap();
            assert!(value.is_list());
            let ty = ty
                .strip_prefix("Array<")
                .unwrap()
                .strip_suffix('>')
                .unwrap();
            let expected = child_names.iter().find(|(t, _)| *t == ty).unwrap().1;
            assert!(kind_matches(expected, value.kind()), "{name}:{tag}");
        }
    }
}

#[test]
fn deep_go_fixtures_decode_clone_project_encode_and_drop_on_two_mib_stack() {
    std::thread::Builder::new()
        .stack_size(2 * 1024 * 1024)
        .spawn(|| {
            let fixtures: serde_json::Value =
                serde_json::from_slice(include_bytes!("../testdata/wire_vectors.json")).unwrap();
            let cases = fixtures["cases"].as_array().unwrap();
            let mut checked = 0;
            for name in ["raw_message_depth_10000", "raw_unknown_group_depth_10001"] {
                let case = cases.iter().find(|c| c["name"] == name).unwrap();
                let wire = STANDARD
                    .decode(case["input_otlp"].as_str().unwrap())
                    .unwrap();
                let frame = Frame::decode(&wire).unwrap();
                let cloned = frame.clone();
                drop(frame);
                let scope = cloned.root().scope_spans[0];
                let span = cloned.scope_spans(scope).spans[0];
                let encoded = cloned.single(Selection { scope, span });
                drop(cloned);
                let schema = Schema::pinned().unwrap();
                let rows = schema.prepare(std::slice::from_ref(&wire)).unwrap();
                assert_eq!(rows.len(), 1);
                assert_eq!(
                    encoded,
                    STANDARD
                        .decode(case["outputs"][0]["otlp"].as_str().unwrap())
                        .unwrap()
                );
                assert_eq!(
                    rows[0].row,
                    STANDARD
                        .decode(case["outputs"][0]["row"].as_str().unwrap())
                        .unwrap()
                );
                assert_eq!(
                    schema.admit(std::slice::from_ref(&wire)).unwrap(),
                    [encoded]
                );
                assert_eq!(schema.validate(&[wire]).unwrap(), 1);
                checked += 1;
            }
            assert_eq!(checked, 2);
        })
        .unwrap()
        .join()
        .unwrap();
}

mod footprint {
    //! Bytes a queued span holds between acceptance and its consumer's single encode.
    use super::*;
    use std::{
        alloc::{GlobalAlloc, Layout, System},
        cell::Cell,
    };

    thread_local! {
        /// Live heap bytes requested by the current thread (frees by other threads subtract
        /// from theirs), so concurrently running tests do not disturb a measurement.
        static LIVE: Cell<isize> = const { Cell::new(0) };
    }
    struct Counting;
    fn add(delta: isize) {
        let _ = LIVE.try_with(|live| live.set(live.get() + delta));
    }
    // SAFETY: forwards every call to the system allocator unchanged and only counts sizes.
    unsafe impl GlobalAlloc for Counting {
        unsafe fn alloc(&self, layout: Layout) -> *mut u8 {
            add(layout.size() as isize);
            unsafe { System.alloc(layout) }
        }
        unsafe fn dealloc(&self, ptr: *mut u8, layout: Layout) {
            add(-(layout.size() as isize));
            unsafe { System.dealloc(ptr, layout) }
        }
        unsafe fn realloc(&self, ptr: *mut u8, layout: Layout, new_size: usize) -> *mut u8 {
            add(new_size as isize - layout.size() as isize);
            unsafe { System.realloc(ptr, layout, new_size) }
        }
    }
    #[global_allocator]
    static COUNTING: Counting = Counting;

    fn live() -> isize {
        LIVE.with(Cell::get)
    }

    /// Eight fixture frames of about 440 bytes each held 9.6 KB per span (21.7x) before
    /// nodes were right-sized and the large messages boxed, and 3.3 KB (7.5x) after.
    #[test]
    fn a_queued_span_costs_a_small_multiple_of_its_encoded_size() {
        let vectors: serde_json::Value =
            serde_json::from_slice(include_bytes!("../testdata/vectors.json")).unwrap();
        let schema = Schema::pinned().unwrap();
        let mut checked = 0;
        for v in vectors.as_array().unwrap() {
            let name = v["name"].as_str().unwrap();
            if !name.starts_with("v2_") {
                continue;
            }
            let wire = STANDARD.decode(v["otlp"].as_str().unwrap()).unwrap();
            let before = live();
            let accepted = schema.accept(std::slice::from_ref(&wire)).unwrap();
            let held = (live() - before) as usize;
            assert_eq!(accepted.len(), 1);
            assert!(
                held <= 10 * wire.len(),
                "{name}: {held} bytes held for {} encoded",
                wire.len()
            );
            checked += 1;
            drop(accepted);
        }
        assert_eq!(checked, 8);
    }
}
