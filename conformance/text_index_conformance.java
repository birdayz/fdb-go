package com.birdayz.conformance;

import com.apple.foundationdb.record.IndexScanType;
import com.apple.foundationdb.record.RecordMetaData;
import com.apple.foundationdb.record.RecordMetaDataBuilder;
import com.apple.foundationdb.record.ScanProperties;
import com.apple.foundationdb.record.TupleRange;
import com.apple.foundationdb.record.IndexEntry;
import com.apple.foundationdb.record.metadata.Index;
import com.apple.foundationdb.record.metadata.IndexTypes;
import com.apple.foundationdb.record.metadata.Key;
import com.apple.foundationdb.record.provider.common.text.DefaultTextTokenizer;
import com.apple.foundationdb.record.provider.common.text.TextTokenizer;
import com.apple.foundationdb.record.provider.foundationdb.FDBRecordContext;
import com.apple.foundationdb.record.provider.foundationdb.FDBRecordStore;
import com.apple.foundationdb.record.provider.foundationdb.IndexOrphanBehavior;
import com.apple.foundationdb.record.query.RecordQuery;
import com.apple.foundationdb.record.query.expressions.Query;
import com.apple.foundationdb.record.RecordLayerDemo;
import com.apple.foundationdb.record.RecordLayerDemo.Customer;
import com.apple.foundationdb.subspace.Subspace;
import com.apple.foundationdb.tuple.Tuple;
import com.google.gson.Gson;
import com.google.gson.reflect.TypeToken;

import java.util.ArrayList;
import java.util.HashMap;
import java.util.List;
import java.util.Map;

class TextIndexSteps extends ConformanceBase {
    // TEXT index on Customer.name field
    // Customer has: customer_id (PK), name (string), email (string)

    private static RecordMetaData createTextIndexedMetaData() {
        RecordMetaDataBuilder metaDataBuilder = RecordMetaData.newBuilder()
            .setRecords(RecordLayerDemo.getDescriptor());
        metaDataBuilder.getRecordType("Order")
            .setPrimaryKey(Key.Expressions.field("order_id"));
        metaDataBuilder.getRecordType("Customer")
            .setPrimaryKey(Key.Expressions.field("customer_id"));
        metaDataBuilder.getRecordType("TypedRecord")
            .setPrimaryKey(Key.Expressions.field("id"));
        metaDataBuilder.addIndex("Customer", new Index("customer_name_text",
            Key.Expressions.field("name"),
            IndexTypes.TEXT));
        return metaDataBuilder.build();
    }

    private static FDBRecordStore openTextIndexedStore(FDBRecordContext context, byte[] subspace) {
        return FDBRecordStore.newBuilder()
            .setMetaDataProvider(createTextIndexedMetaData())
            .setContext(context)
            .setSubspace(new Subspace(subspace))
            .setUserVersionChecker(ALWAYS_READABLE_CHECKER)
            .createOrOpen();
    }

    @ConformanceStep("saveCustomerWithTextIndex")
    public void saveCustomerWithTextIndex(String clusterFile, byte[] subspace, Customer customer, String tenantName) {
        runInContext(clusterFile, tenantName, context -> {
            FDBRecordStore store = openTextIndexedStore(context, subspace);
            store.saveRecord(customer);
            return null;
        });
    }

    @ConformanceStep("deleteCustomerWithTextIndex")
    public boolean deleteCustomerWithTextIndex(String clusterFile, byte[] subspace, long customerID, String tenantName) {
        return runInContext(clusterFile, tenantName, context -> {
            FDBRecordStore store = openTextIndexedStore(context, subspace);
            return store.deleteRecord(Tuple.from(customerID));
        });
    }

    @ConformanceStep("scanTextIndex")
    public List<Map<String, Object>> scanTextIndex(String clusterFile, byte[] subspace, String tenantName) {
        return runInContext(clusterFile, tenantName, context -> {
            RecordMetaData metadata = createTextIndexedMetaData();
            FDBRecordStore store = FDBRecordStore.newBuilder()
                .setMetaDataProvider(metadata)
                .setContext(context)
                .setSubspace(new Subspace(subspace))
                .setUserVersionChecker(ALWAYS_READABLE_CHECKER)
                .createOrOpen();

            Index index = metadata.getIndex("customer_name_text");
            List<IndexEntry> entries = store.scanIndex(
                index, IndexScanType.BY_TEXT_TOKEN, TupleRange.ALL, null, ScanProperties.FORWARD_SCAN)
                .asList()
                .join();

            return entriesToResult(entries);
        });
    }

    @ConformanceStep("scanTextIndexByToken")
    public List<Map<String, Object>> scanTextIndexByToken(String clusterFile, byte[] subspace, String token, String tenantName) {
        return runInContext(clusterFile, tenantName, context -> {
            RecordMetaData metadata = createTextIndexedMetaData();
            FDBRecordStore store = FDBRecordStore.newBuilder()
                .setMetaDataProvider(metadata)
                .setContext(context)
                .setSubspace(new Subspace(subspace))
                .setUserVersionChecker(ALWAYS_READABLE_CHECKER)
                .createOrOpen();

            Index index = metadata.getIndex("customer_name_text");
            List<IndexEntry> entries = store.scanIndex(
                index, IndexScanType.BY_TEXT_TOKEN, TupleRange.allOf(Tuple.from(token)),
                null, ScanProperties.FORWARD_SCAN)
                .asList()
                .join();

            return entriesToResult(entries);
        });
    }

    private static List<Map<String, Object>> entriesToResult(List<IndexEntry> entries) {
        List<Map<String, Object>> result = new ArrayList<>();
        for (IndexEntry entry : entries) {
            Map<String, Object> map = new HashMap<>();
            // Key: [token, primaryKeyColumns...]
            map.put("token", entry.getKey().getString(0));
            map.put("primaryKey", entry.getKey().getLong(1));
            // Value: Tuple(positionList) — nested tuple of positions
            Tuple positionsTuple = entry.getValue().getNestedTuple(0);
            List<Long> positions = new ArrayList<>();
            for (int j = 0; j < positionsTuple.size(); j++) {
                positions.add(positionsTuple.getLong(j));
            }
            map.put("positions", positions);
            result.add(map);
        }
        return result;
    }

    /** Pin the upstream tokenized-primary-key overlap behavior before changing wire keys. */
    @ConformanceStep("probeTextPrimaryKeyOverlap")
    public Map<String, Object> probeTextPrimaryKeyOverlap(String clusterFile, byte[] subspace, String tenantName) {
        return runInContext(clusterFile, tenantName, context -> {
            RecordMetaDataBuilder builder = RecordMetaData.newBuilder().setRecords(RecordLayerDemo.getDescriptor());
            builder.getRecordType("Order").setPrimaryKey(Key.Expressions.field("order_id"));
            builder.getRecordType("Customer").setPrimaryKey(Key.Expressions.concat(
                    Key.Expressions.field("name"), Key.Expressions.field("customer_id")));
            builder.getRecordType("TypedRecord").setPrimaryKey(Key.Expressions.field("id"));
            builder.addIndex("Customer", new Index("overlapping_text", Key.Expressions.field("name"), IndexTypes.TEXT));
            RecordMetaData metadata = builder.build();
            FDBRecordStore store = FDBRecordStore.newBuilder().setMetaDataProvider(metadata)
                    .setContext(context).setSubspace(new Subspace(subspace))
                    .setUserVersionChecker(ALWAYS_READABLE_CHECKER).createOrOpen();
            var saved = store.saveRecord(Customer.newBuilder().setCustomerId(1L).setName("red blue").build());
            Index index = metadata.getIndex("overlapping_text");
            List<IndexEntry> entries = store.scanIndex(index, IndexScanType.BY_TEXT_TOKEN,
                    TupleRange.allOf(Tuple.from("red")), null, ScanProperties.FORWARD_SCAN).asList().join();
            Map<String, Object> result = new HashMap<>();
            result.put("positions", index.getPrimaryKeyComponentPositions());
            result.put("savedText", saved.getPrimaryKey().getString(0));
            result.put("savedId", saved.getPrimaryKey().getLong(1));
            result.put("entryCount", entries.size());
            if (entries.size() != 1) {
                return result;
            }
            IndexEntry entry = entries.get(0);
            result.put("extractedText", entry.getPrimaryKey().getString(0));
            result.put("extractedId", entry.getPrimaryKey().getLong(1));
            result.put("originalRecordExists", store.loadRecord(saved.getPrimaryKey()) != null);
            try {
                store.loadIndexEntryRecord(entry, IndexOrphanBehavior.ERROR).join();
                result.put("fetchError", "");
            } catch (RuntimeException error) {
                Throwable cause = error;
                while (cause.getCause() != null) { cause = cause.getCause(); }
                result.put("fetchError", cause.getMessage());
                result.put("fetchClass", cause.getClass().getSimpleName());
            }
            var query = RecordQuery.newBuilder().setRecordType("Customer")
                    .setFilter(Query.field("name").text().contains("red")).build();
            var plan = store.planQuery(query);
            result.put("queryPlan", plan.toString());
            try {
                result.put("queryRows", store.executeQuery(plan).asList().join().size());
                result.put("queryError", "");
            } catch (RuntimeException error) {
                Throwable cause = error;
                while (cause.getCause() != null) { cause = cause.getCause(); }
                result.put("queryError", cause.getMessage());
                result.put("queryClass", cause.getClass().getSimpleName());
            }
            return result;
        });
    }

    /** Java's default TEXT tokenizer, one token list per input text. */
    @ConformanceStep("tokenizeDefault")
    public List<List<String>> tokenizeDefault(String textsJson) {
        List<String> texts = new Gson().fromJson(textsJson, new TypeToken<List<String>>(){}.getType());
        List<List<String>> result = new ArrayList<>();
        for (String text : texts) {
            List<String> tokens = new ArrayList<>();
            for (CharSequence token : DefaultTextTokenizer.instance().tokenizeToList(text, 0, TextTokenizer.TokenizerMode.INDEX)) {
                tokens.add(token.toString());
            }
            result.add(tokens);
        }
        return result;
    }
}
