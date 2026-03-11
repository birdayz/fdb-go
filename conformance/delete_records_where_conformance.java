package com.birdayz.conformance;

import com.apple.foundationdb.record.IndexScanType;
import com.apple.foundationdb.record.RecordMetaData;
import com.apple.foundationdb.record.RecordMetaDataBuilder;
import com.apple.foundationdb.record.ScanProperties;
import com.apple.foundationdb.record.TupleRange;
import com.apple.foundationdb.record.IndexEntry;
import com.apple.foundationdb.record.RecordLayerDemo;
import com.apple.foundationdb.record.RecordLayerDemo.Order;
import com.apple.foundationdb.record.RecordLayerDemo.Customer;
import com.apple.foundationdb.record.metadata.Index;
import com.apple.foundationdb.record.metadata.IndexTypes;
import com.apple.foundationdb.record.metadata.Key;
import com.apple.foundationdb.record.provider.foundationdb.FDBRecordStore;
import com.apple.foundationdb.record.provider.foundationdb.FDBStoredRecord;
import com.apple.foundationdb.subspace.Subspace;
import com.apple.foundationdb.tuple.Tuple;
import com.google.protobuf.Message;

import java.util.ArrayList;
import java.util.HashMap;
import java.util.List;
import java.util.Map;

/**
 * Conformance steps for DeleteRecordsWhere — bulk partition deletion by record type prefix.
 * Uses type-prefixed PKs: concat(recordType(), field("order_id")) / concat(recordType(), field("customer_id")).
 */
class DeleteRecordsWhereSteps extends ConformanceBase {

    /**
     * Metadata with type-prefixed PKs and a type-specific VALUE index on Order.price.
     */
    static RecordMetaData createTypePrefixedMetaData() {
        RecordMetaDataBuilder b = RecordMetaData.newBuilder()
            .setRecords(RecordLayerDemo.getDescriptor());
        b.getRecordType("Order")
            .setPrimaryKey(Key.Expressions.concat(
                Key.Expressions.recordType(),
                Key.Expressions.field("order_id")));
        b.getRecordType("Customer")
            .setPrimaryKey(Key.Expressions.concat(
                Key.Expressions.recordType(),
                Key.Expressions.field("customer_id")));
        b.addIndex("Order", new Index("Order$price_tp",
            Key.Expressions.field("price"), IndexTypes.VALUE));
        return b.build();
    }

    static FDBRecordStore openTypePrefixedStore(com.apple.foundationdb.record.provider.foundationdb.FDBRecordContext context, byte[] subspace) {
        return FDBRecordStore.newBuilder()
            .setMetaDataProvider(createTypePrefixedMetaData())
            .setContext(context)
            .setSubspace(new Subspace(subspace))
            .setUserVersionChecker(ALWAYS_READABLE_CHECKER)
            .createOrOpen();
    }

    @ConformanceStep("saveOrderTypePrefixed")
    public void saveOrderTypePrefixed(String clusterFile, byte[] subspace, Order order, String tenantName) {
        runInContext(clusterFile, tenantName, context -> {
            FDBRecordStore store = openTypePrefixedStore(context, subspace);
            store.saveRecord(order);
            return null;
        });
    }

    @ConformanceStep("saveCustomerTypePrefixed")
    public void saveCustomerTypePrefixed(String clusterFile, byte[] subspace, Customer customer, String tenantName) {
        runInContext(clusterFile, tenantName, context -> {
            FDBRecordStore store = openTypePrefixedStore(context, subspace);
            store.saveRecord(customer);
            return null;
        });
    }

    @ConformanceStep("deleteRecordsWhereType")
    public void deleteRecordsWhereType(String clusterFile, byte[] subspace, String recordType, String tenantName) {
        runInContext(clusterFile, tenantName, context -> {
            FDBRecordStore store = openTypePrefixedStore(context, subspace);
            store.deleteRecordsWhere(recordType, null);
            return null;
        });
    }

    @ConformanceStep("countRecordsTypePrefixed")
    public long countRecordsTypePrefixed(String clusterFile, byte[] subspace, String tenantName) {
        return runInContext(clusterFile, tenantName, context -> {
            FDBRecordStore store = openTypePrefixedStore(context, subspace);
            return store.scanRecords(null, ScanProperties.FORWARD_SCAN)
                .getCount().join();
        });
    }

    @ConformanceStep("loadOrderTypePrefixed")
    public Order loadOrderTypePrefixed(String clusterFile, byte[] subspace, long orderId, String tenantName) {
        return runInContext(clusterFile, tenantName, context -> {
            FDBRecordStore store = openTypePrefixedStore(context, subspace);
            Object typeKey = store.getRecordMetaData().getRecordType("Order").getRecordTypeKey();
            FDBStoredRecord<Message> rec = store.loadRecord(Tuple.from(typeKey, orderId));
            if (rec == null) return null;
            return Order.newBuilder().mergeFrom(rec.getRecord()).build();
        });
    }

    @ConformanceStep("loadCustomerTypePrefixed")
    public Customer loadCustomerTypePrefixed(String clusterFile, byte[] subspace, long customerId, String tenantName) {
        return runInContext(clusterFile, tenantName, context -> {
            FDBRecordStore store = openTypePrefixedStore(context, subspace);
            Object typeKey = store.getRecordMetaData().getRecordType("Customer").getRecordTypeKey();
            FDBStoredRecord<Message> rec = store.loadRecord(Tuple.from(typeKey, customerId));
            if (rec == null) return null;
            return Customer.newBuilder().mergeFrom(rec.getRecord()).build();
        });
    }

    @ConformanceStep("scanIndexTypePrefixed")
    public List<Map<String, Object>> scanIndexTypePrefixed(String clusterFile, byte[] subspace, String indexName, String tenantName) {
        return runInContext(clusterFile, tenantName, context -> {
            RecordMetaData metadata = createTypePrefixedMetaData();
            FDBRecordStore store = FDBRecordStore.newBuilder()
                .setMetaDataProvider(metadata)
                .setContext(context)
                .setSubspace(new Subspace(subspace))
                .setUserVersionChecker(ALWAYS_READABLE_CHECKER)
                .createOrOpen();

            Index index = metadata.getIndex(indexName);
            List<IndexEntry> entries = store.scanIndex(
                index, IndexScanType.BY_VALUE, TupleRange.ALL, null, ScanProperties.FORWARD_SCAN)
                .asList()
                .join();

            List<Map<String, Object>> result = new ArrayList<>();
            for (IndexEntry entry : entries) {
                Map<String, Object> map = new HashMap<>();
                List<Object> keyValues = new ArrayList<>();
                for (Object item : entry.getKey()) {
                    keyValues.add(item);
                }
                map.put("key", keyValues);
                List<Object> pkValues = new ArrayList<>();
                for (Object item : entry.getPrimaryKey()) {
                    pkValues.add(item);
                }
                map.put("primaryKey", pkValues);
                result.add(map);
            }
            return result;
        });
    }
}
