//! SPANS records have bounded depth and explicit proto2-style field presence.
use prost::Message;

#[derive(Clone, PartialEq, Message)]
pub struct Array<T: Message + Default + std::fmt::Debug> {
    #[prost(message, repeated, tag = "1")]
    pub(crate) values: Vec<T>,
}
macro_rules! record {
    ($name:ident { $($tag:literal => $field:ident: $ty:ty, $kind:ident;)* }) => {
        #[derive(Clone, PartialEq, Message)]
        pub struct $name {
            $(#[prost($kind, optional, tag = $tag)] pub(crate) $field: Option<$ty>,)*
        }
        #[cfg(test)]
        impl $name {
            pub(crate) const FIELDS: &'static [(&'static str, &'static str, &'static str)] = &[
                $(($tag, stringify!($kind), stringify!($ty)),)*
            ];
        }
    };
}
record!(StrAttr { "1" => scope: String, string; "2" => attr_key: String, string; "3" => value: String, string; });
record!(LongStrAttr { "1" => scope: String, string; "2" => attr_key: String, string; "3" => digest: Vec<u8>, bytes; "4" => value: String, string; });
record!(IntAttr { "1" => scope: String, string; "2" => attr_key: String, string; "3" => value: i64, int64; });
record!(DoubleAttr { "1" => scope: String, string; "2" => attr_key: String, string; "3" => value: f64, double; });
record!(BoolAttr { "1" => scope: String, string; "2" => attr_key: String, string; "3" => value: bool, bool; });
record!(BytesAttr { "1" => scope: String, string; "2" => attr_key: String, string; "3" => value: Vec<u8>, bytes; });
record!(AnyAttr { "1" => scope: String, string; "2" => attr_key: String, string; "3" => value: String, string; });
record!(RefChunk { "1" => chunk: i64, int64; "2" => refs: Vec<u8>, bytes; });
record!(Spans {
    "1" => trace_id: Vec<u8>, bytes;
    "2" => span_id: Vec<u8>, bytes;
    "3" => parent_span_id: Vec<u8>, bytes;
    "4" => trace_state: String, string;
    "5" => flags: i64, int64;
    "6" => name: String, string;
    "7" => kind: i32, int32;
    "8" => start_ns: i64, int64;
    "9" => end_ns: i64, int64;
    "10" => status_code: i32, int32;
    "11" => status_message: String, string;
    "12" => service_name: String, string;
    "13" => scope_name: String, string;
    "14" => scope_version: String, string;
    "15" => resource_schema_url: String, string;
    "16" => scope_schema_url: String, string;
    "17" => dropped_attributes: i64, int64;
    "18" => dropped_events: i64, int64;
    "19" => dropped_links: i64, int64;
    "20" => str_attrs: Array<StrAttr>, message;
    "21" => long_str_attrs: Array<LongStrAttr>, message;
    "22" => int_attrs: Array<IntAttr>, message;
    "23" => double_attrs: Array<DoubleAttr>, message;
    "24" => bool_attrs: Array<BoolAttr>, message;
    "25" => bytes_attrs: Array<BytesAttr>, message;
    "26" => any_attrs: Array<AnyAttr>, message;
    "27" => events: Array<SpanEvent>, message;
    "28" => links: Array<SpanLink>, message;
    "29" => ref_chunks: Array<RefChunk>, message;
    "30" => otlp: Vec<u8>, bytes;
});
record!(SpanEvent {
    "1" => ordinal: i64, int64;
    "2" => name: String, string;
    "3" => time_ns: i64, int64;
    "4" => dropped_attributes: i64, int64;
    "5" => str_attrs: Array<StrAttr>, message;
    "6" => int_attrs: Array<IntAttr>, message;
    "7" => double_attrs: Array<DoubleAttr>, message;
    "8" => bool_attrs: Array<BoolAttr>, message;
    "9" => any_attrs: Array<AnyAttr>, message;
});
record!(SpanLink {
    "1" => ordinal: i64, int64;
    "2" => trace_id: Vec<u8>, bytes;
    "3" => span_id: Vec<u8>, bytes;
    "4" => trace_state: String, string;
    "5" => flags: i64, int64;
    "6" => dropped_attributes: i64, int64;
    "7" => str_attrs: Array<StrAttr>, message;
    "8" => int_attrs: Array<IntAttr>, message;
    "9" => double_attrs: Array<DoubleAttr>, message;
    "10" => bool_attrs: Array<BoolAttr>, message;
    "11" => any_attrs: Array<AnyAttr>, message;
});
