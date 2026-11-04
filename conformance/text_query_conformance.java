package com.birdayz.conformance;

import com.apple.foundationdb.record.EvaluationContext;
import com.apple.foundationdb.record.ExecuteProperties;
import com.apple.foundationdb.record.IndexEntry;
import com.apple.foundationdb.record.RecordCursor;
import com.apple.foundationdb.record.RecordCursorResult;
import com.apple.foundationdb.record.RecordLayerDemo;
import com.apple.foundationdb.record.RecordMetaData;
import com.apple.foundationdb.record.RecordMetaDataBuilder;
import com.apple.foundationdb.record.metadata.Index;
import com.apple.foundationdb.record.metadata.IndexTypes;
import com.apple.foundationdb.record.metadata.Key;
import com.apple.foundationdb.record.provider.foundationdb.FDBQueriedRecord;
import com.apple.foundationdb.record.provider.foundationdb.FDBRecordStore;
import com.apple.foundationdb.record.provider.foundationdb.cursors.IntersectionCursor;
import com.apple.foundationdb.record.provider.foundationdb.cursors.ProbableIntersectionCursor;
import com.apple.foundationdb.record.provider.foundationdb.cursors.UnionCursor;
import com.apple.foundationdb.record.provider.foundationdb.cursors.UnorderedUnionCursor;
import com.apple.foundationdb.record.query.RecordQuery;
import com.apple.foundationdb.record.query.expressions.Comparisons;
import com.apple.foundationdb.record.query.expressions.Query;
import com.apple.foundationdb.record.query.expressions.QueryComponent;
import com.apple.foundationdb.record.query.expressions.Text;
import com.apple.foundationdb.record.query.plan.RecordQueryPlanner;
import com.apple.foundationdb.record.query.plan.ScanComparisons;
import com.apple.foundationdb.record.query.plan.TextScan;
import com.apple.foundationdb.record.query.plan.plans.RecordQueryPlan;
import com.apple.foundationdb.subspace.Subspace;
import com.google.gson.Gson;
import com.google.gson.reflect.TypeToken;
import com.google.protobuf.Message;
import com.google.protobuf.ByteString;

import java.util.ArrayList;
import java.util.Collections;
import java.util.HashMap;
import java.util.HexFormat;
import java.util.List;
import java.util.Map;
import java.util.Set;
import java.util.function.Function;

/**
 * Java's execution of TEXT queries, for comparison with the Go port: TextScan.scan over three
 * TEXT indexes on Customer.name (plain, grouped by email, with price after the text), the old
 * RecordQueryPlanner's text index plans, and the merge cursors TextScan combines token scans with.
 * Keys, values and continuations are returned as hex so they compare byte for byte.
 */
class TextQuerySteps extends ConformanceBase {
    static final HexFormat HEX = HexFormat.of();

    /** Mirrored by textQueryMetaData in text_query_conformance_test.go. */
    static RecordMetaData createTextQueryMetaData() {
        RecordMetaDataBuilder builder = RecordMetaData.newBuilder().setRecords(RecordLayerDemo.getDescriptor());
        builder.getRecordType("Order").setPrimaryKey(Key.Expressions.field("order_id"));
        builder.getRecordType("Customer").setPrimaryKey(Key.Expressions.field("customer_id"));
        builder.getRecordType("TypedRecord").setPrimaryKey(Key.Expressions.field("id"));
        builder.addIndex("Customer", new Index("name_text", Key.Expressions.field("name"), IndexTypes.TEXT));
        builder.addIndex("Customer", new Index("grouped_text",
                Key.Expressions.field("name").groupBy(Key.Expressions.field("email")), IndexTypes.TEXT));
        builder.addIndex("Customer", new Index("suffix_text",
                Key.Expressions.concat(Key.Expressions.field("name"), Key.Expressions.field("price")), IndexTypes.TEXT));
        return builder.build();
    }

    static FDBRecordStore openTextQueryStore(com.apple.foundationdb.record.provider.foundationdb.FDBRecordContext context,
                                             RecordMetaData metaData, byte[] subspace) {
        return FDBRecordStore.newBuilder()
                .setMetaDataProvider(metaData)
                .setContext(context)
                .setSubspace(new Subspace(subspace))
                .setUserVersionChecker(ALWAYS_READABLE_CHECKER)
                .createOrOpen();
    }

    /** One text query; see TextQuerySpec in text_query_conformance_test.go. */
    static class Spec {
        String index;
        String type;
        String text;
        List<String> tokens;
        int maxDistance;
        boolean strict;
        String grouping;
        Long suffixLow;
        Long suffixHigh;
        boolean reverse;
        int limit;
        int skip;
        int scannedRecordsLimit;
        String continuation;
    }

    static Comparisons.TextComparison comparison(Spec spec) {
        Comparisons.Type type = Comparisons.Type.valueOf(spec.type);
        String fallback = "default";
        if (type == Comparisons.Type.TEXT_CONTAINS_ALL_WITHIN) {
            return spec.tokens != null
                    ? new Comparisons.TextWithMaxDistanceComparison(spec.tokens, spec.maxDistance, null, fallback)
                    : new Comparisons.TextWithMaxDistanceComparison(spec.text, spec.maxDistance, null, fallback);
        }
        if (type == Comparisons.Type.TEXT_CONTAINS_ALL_PREFIXES) {
            return spec.tokens != null
                    ? new Comparisons.TextContainsAllPrefixesComparison(spec.tokens, spec.strict, null, fallback)
                    : new Comparisons.TextContainsAllPrefixesComparison(spec.text, spec.strict, null, fallback);
        }
        return spec.tokens != null
                ? new Comparisons.TextComparison(type, spec.tokens, null, fallback)
                : new Comparisons.TextComparison(type, spec.text, null, fallback);
    }

    static ExecuteProperties executeProperties(Spec spec) {
        ExecuteProperties.Builder builder = ExecuteProperties.newBuilder()
                .setReturnedRowLimit(spec.limit)
                .setSkip(spec.skip);
        if (spec.scannedRecordsLimit > 0) {
            builder.setScannedRecordsLimit(spec.scannedRecordsLimit);
        }
        return builder.build();
    }

    static byte[] continuation(Spec spec) {
        return spec.continuation == null ? null : HEX.parseHex(spec.continuation);
    }

    static <T> Map<String, Object> drainPage(RecordCursor<T> cursor, Function<T, Object> item) {
        List<Object> items = new ArrayList<>();
        RecordCursorResult<T> result;
        while ((result = cursor.getNext()).hasNext()) {
            items.add(item.apply(result.get()));
        }
        Map<String, Object> page = new HashMap<>();
        page.put("items", items);
        byte[] cont = result.getContinuation().toBytes();
        page.put("continuation", cont == null ? null : HEX.formatHex(cont));
        page.put("reason", result.getNoNextReason().name());
        return page;
    }

    static Map<String, Object> entryItem(IndexEntry entry) {
        Map<String, Object> m = new HashMap<>();
        m.put("key", HEX.formatHex(entry.getKey().pack()));
        m.put("value", HEX.formatHex(entry.getValue().pack()));
        return m;
    }

    /** TextScan.scan over the spec's index, one page from the spec's continuation. */
    @ConformanceStep("textQueryScan")
    public Map<String, Object> textQueryScan(String clusterFile, byte[] subspace, String specJson, String tenantName) {
        Spec spec = new Gson().fromJson(specJson, Spec.class);
        return runInContext(clusterFile, tenantName, context -> {
            RecordMetaData metaData = createTextQueryMetaData();
            FDBRecordStore store = openTextQueryStore(context, metaData, subspace);
            Index index = metaData.getIndex(spec.index);
            ScanComparisons grouping = spec.grouping == null ? null
                    : new ScanComparisons(List.of(new Comparisons.SimpleComparison(Comparisons.Type.EQUALS, spec.grouping)), Set.of());
            ScanComparisons suffix = null;
            if (spec.suffixLow != null) {
                suffix = new ScanComparisons(List.of(), Set.of(
                        new Comparisons.SimpleComparison(Comparisons.Type.GREATER_THAN_OR_EQUALS, spec.suffixLow),
                        new Comparisons.SimpleComparison(Comparisons.Type.LESS_THAN, spec.suffixHigh)));
            }
            TextScan scan = new TextScan(index, grouping, comparison(spec), suffix);
            try (RecordCursor<IndexEntry> cursor = scan.scan(store, EvaluationContext.EMPTY, continuation(spec),
                    executeProperties(spec).asScanProperties(spec.reverse))) {
                return drainPage(cursor, TextQuerySteps::entryItem);
            }
        });
    }

    static QueryComponent textFilter(Spec spec) {
        Text text = Query.field("name").text();
        switch (Comparisons.Type.valueOf(spec.type)) {
            case TEXT_CONTAINS_ALL:
                return spec.tokens != null ? text.containsAll(spec.tokens) : text.containsAll(spec.text);
            case TEXT_CONTAINS_ALL_WITHIN:
                return spec.tokens != null ? text.containsAll(spec.tokens, spec.maxDistance) : text.containsAll(spec.text, spec.maxDistance);
            case TEXT_CONTAINS_ANY:
                return spec.tokens != null ? text.containsAny(spec.tokens) : text.containsAny(spec.text);
            case TEXT_CONTAINS_PHRASE:
                return spec.tokens != null ? text.containsPhrase(spec.tokens) : text.containsPhrase(spec.text);
            case TEXT_CONTAINS_PREFIX:
                return text.containsPrefix(spec.text);
            case TEXT_CONTAINS_ALL_PREFIXES:
                return spec.tokens != null ? text.containsAllPrefixes(spec.tokens, spec.strict) : text.containsAllPrefixes(spec.text, spec.strict);
            case TEXT_CONTAINS_ANY_PREFIX:
                return spec.tokens != null ? text.containsAnyPrefix(spec.tokens) : text.containsAnyPrefix(spec.text);
            default:
                throw new IllegalArgumentException(spec.type);
        }
    }

    /** The old planner's plan for the spec's text filter on its index, and one page of its records. */
    @ConformanceStep("textQueryPlan")
    public Map<String, Object> textQueryPlan(String clusterFile, byte[] subspace, String specJson, String tenantName) {
        Spec spec = new Gson().fromJson(specJson, Spec.class);
        return runInContext(clusterFile, tenantName, context -> {
            RecordMetaData metaData = createTextQueryMetaData();
            FDBRecordStore store = openTextQueryStore(context, metaData, subspace);
            QueryComponent filter = textFilter(spec);
            if (spec.grouping != null) {
                filter = Query.and(Query.field("email").equalsValue(spec.grouping), filter);
            }
            RecordQuery query = RecordQuery.newBuilder()
                    .setRecordType("Customer")
                    .setFilter(filter)
                    .setAllowedIndex(spec.index)
                    .build();
            RecordQueryPlan plan = new RecordQueryPlanner(metaData, store.getRecordStoreState()).plan(query);
            try (RecordCursor<FDBQueriedRecord<Message>> cursor = store.executeQuery(plan, continuation(spec), executeProperties(spec))) {
                Map<String, Object> page = drainPage(cursor, r -> r.getPrimaryKey().getLong(0));
                page.put("plan", plan.toString());
                return page;
            }
        });
    }

    @ConformanceStep("probableIntersectionTypedKeys")
    public Map<String, Object> probableIntersectionTypedKeys(String shape, String listsJson, int limit, String continuation) {
        List<List<String>> lists = new Gson().fromJson(listsJson, new TypeToken<List<List<String>>>(){}.getType());
        List<Function<byte[], RecordCursor<String>>> children = new ArrayList<>();
        for (List<String> list : lists) {
            children.add(cont -> RecordCursor.fromList(list, cont));
        }
        Function<String, List<Object>> key = label -> {
            int n = Integer.parseInt(label);
            Object value;
            switch (shape) {
                case "long": value = 7L + n; break;
                case "bytes": value = ByteString.copyFromUtf8(label); break;
                case "raw_bytes": value = label.getBytes(java.nio.charset.StandardCharsets.UTF_8); break;
                case "nested": value = Collections.singletonList(label); break;
                case "float_nan": value = Float.intBitsToFloat(0x7fc00001 + n); break;
                case "double_nan": value = Double.longBitsToDouble(0x7ff8000000000001L + n); break;
                case "nested_float_nan": value = Collections.singletonList(Float.intBitsToFloat(0x7fc00001 + n)); break;
                case "nested_double_nan": value = Collections.singletonList(Double.longBitsToDouble(0x7ff8000000000001L + n)); break;
                default: throw new IllegalArgumentException(shape);
            }
            return Collections.singletonList(value);
        };
        byte[] cont = continuation == null ? null : HEX.parseHex(continuation);
        try (RecordCursor<String> cursor = ProbableIntersectionCursor.create(key, children, cont, null).limitRowsTo(limit)) {
            return drainPage(cursor, s -> s);
        }
    }

    /**
     * A merge cursor over in-memory list children, whose next results are always complete, so
     * the unordered merges run in their deterministic order.
     */
    @ConformanceStep("mergeCursorOverLists")
    public Map<String, Object> mergeCursorOverLists(String kind, String listsJson, int limit, String continuation) {
        List<List<String>> lists = new Gson().fromJson(listsJson, new TypeToken<List<List<String>>>(){}.getType());
        List<Function<byte[], RecordCursor<String>>> children = new ArrayList<>();
        for (List<String> list : lists) {
            children.add(cont -> RecordCursor.fromList(list, cont));
        }
        Function<String, List<Object>> key = Collections::singletonList;
        byte[] cont = continuation == null ? null : HEX.parseHex(continuation);
        RecordCursor<String> cursor;
        switch (kind) {
            case "intersection":
                cursor = IntersectionCursor.create(key, false, children, cont, null);
                break;
            case "union":
                cursor = UnionCursor.create(key, false, children, cont, null);
                break;
            case "unordered_union":
                cursor = UnorderedUnionCursor.create(children, cont, null);
                break;
            case "probable_intersection":
                cursor = ProbableIntersectionCursor.create(key, children, cont, null);
                break;
            default:
                throw new IllegalArgumentException(kind);
        }
        try (RecordCursor<String> limited = cursor.limitRowsTo(limit)) {
            return drainPage(limited, s -> s);
        }
    }
}
