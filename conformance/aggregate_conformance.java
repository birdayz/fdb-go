package com.birdayz.conformance;

import com.apple.foundationdb.record.FunctionNames;
import com.apple.foundationdb.record.IsolationLevel;
import com.apple.foundationdb.record.RecordMetaData;
import com.apple.foundationdb.record.RecordMetaDataBuilder;
import com.apple.foundationdb.record.metadata.Index;
import com.apple.foundationdb.record.metadata.IndexAggregateFunction;
import com.apple.foundationdb.record.metadata.IndexTypes;
import com.apple.foundationdb.record.metadata.Key;
import com.apple.foundationdb.record.metadata.expressions.GroupingKeyExpression;
import com.apple.foundationdb.record.provider.foundationdb.FDBRecordContext;
import com.apple.foundationdb.record.provider.foundationdb.FDBRecordStore;
import com.apple.foundationdb.record.RecordLayerDemo;
import com.apple.foundationdb.record.RecordLayerDemo.Order;
import com.apple.foundationdb.subspace.Subspace;
import com.apple.foundationdb.tuple.Tuple;

import java.util.Collections;

/**
 * Conformance steps for EvaluateAggregateFunction cross-language validation.
 * Each step creates a store with the appropriate index, and evaluates an aggregate
 * function on pre-existing data (written by Go or Java).
 */
class AggregateSteps extends ConformanceBase {

    // ========== COUNT via COUNT index ==========

    private static RecordMetaData createCountAggregateMetaData() {
        RecordMetaDataBuilder b = RecordMetaData.newBuilder()
            .setRecords(RecordLayerDemo.getDescriptor());
        b.getRecordType("Order").setPrimaryKey(Key.Expressions.field("order_id"));
        b.getRecordType("Customer").setPrimaryKey(Key.Expressions.field("customer_id"));
        b.getRecordType("TypedRecord").setPrimaryKey(Key.Expressions.field("id"));
        b.addIndex("Order", new Index("agg_count_price",
            new GroupingKeyExpression(Key.Expressions.field("price"), 0),
            IndexTypes.COUNT));
        return b.build();
    }

    private static FDBRecordStore openCountAggregateStore(FDBRecordContext context, byte[] subspace) {
        return FDBRecordStore.newBuilder()
            .setMetaDataProvider(createCountAggregateMetaData())
            .setContext(context)
            .setSubspace(new Subspace(subspace))
            .setUserVersionChecker(ALWAYS_READABLE_CHECKER)
            .createOrOpen();
    }

    @ConformanceStep("saveOrderWithCountAggregate")
    public void saveOrderWithCountAggregate(String clusterFile, byte[] subspace, Order order, String tenantName) {
        runInContext(clusterFile, tenantName, context -> {
            FDBRecordStore store = openCountAggregateStore(context, subspace);
            store.saveRecord(order);
            return null;
        });
    }

    @ConformanceStep("evaluateCountAggregate")
    public long evaluateCountAggregate(String clusterFile, byte[] subspace, String tenantName) {
        RecordMetaData metaData = createCountAggregateMetaData();
        return runInContext(clusterFile, tenantName, context -> {
            FDBRecordStore store = FDBRecordStore.newBuilder()
                .setMetaDataProvider(metaData)
                .setContext(context)
                .setSubspace(new Subspace(subspace))
                .setUserVersionChecker(ALWAYS_READABLE_CHECKER)
                .createOrOpen();
            IndexAggregateFunction fn = new IndexAggregateFunction(
                FunctionNames.COUNT,
                Key.Expressions.field("price").groupBy(Key.Expressions.empty()),
                "agg_count_price");
            return store.evaluateAggregateFunction(
                Collections.singletonList("Order"), fn, com.apple.foundationdb.record.TupleRange.ALL,
                IsolationLevel.SERIALIZABLE)
                .join()
                .getLong(0);
        });
    }

    // ========== SUM via SUM index ==========

    private static RecordMetaData createSumAggregateMetaData() {
        RecordMetaDataBuilder b = RecordMetaData.newBuilder()
            .setRecords(RecordLayerDemo.getDescriptor());
        b.getRecordType("Order").setPrimaryKey(Key.Expressions.field("order_id"));
        b.getRecordType("Customer").setPrimaryKey(Key.Expressions.field("customer_id"));
        b.getRecordType("TypedRecord").setPrimaryKey(Key.Expressions.field("id"));
        b.addIndex("Order", new Index("agg_sum_price",
            new GroupingKeyExpression(Key.Expressions.field("price"), 1),
            IndexTypes.SUM));
        return b.build();
    }

    private static FDBRecordStore openSumAggregateStore(FDBRecordContext context, byte[] subspace) {
        return FDBRecordStore.newBuilder()
            .setMetaDataProvider(createSumAggregateMetaData())
            .setContext(context)
            .setSubspace(new Subspace(subspace))
            .setUserVersionChecker(ALWAYS_READABLE_CHECKER)
            .createOrOpen();
    }

    @ConformanceStep("saveOrderWithSumAggregate")
    public void saveOrderWithSumAggregate(String clusterFile, byte[] subspace, Order order, String tenantName) {
        runInContext(clusterFile, tenantName, context -> {
            FDBRecordStore store = openSumAggregateStore(context, subspace);
            store.saveRecord(order);
            return null;
        });
    }

    @ConformanceStep("evaluateSumAggregate")
    public long evaluateSumAggregate(String clusterFile, byte[] subspace, String tenantName) {
        return runInContext(clusterFile, tenantName, context -> {
            FDBRecordStore store = openSumAggregateStore(context, subspace);
            IndexAggregateFunction fn = new IndexAggregateFunction(
                FunctionNames.SUM,
                new GroupingKeyExpression(Key.Expressions.field("price"), 1),
                "agg_sum_price");
            return store.evaluateAggregateFunction(
                Collections.singletonList("Order"), fn, com.apple.foundationdb.record.TupleRange.ALL,
                IsolationLevel.SERIALIZABLE)
                .join()
                .getLong(0);
        });
    }

    // ========== MIN_EVER via MIN_EVER_LONG index ==========

    private static RecordMetaData createMinEverAggregateMetaData() {
        RecordMetaDataBuilder b = RecordMetaData.newBuilder()
            .setRecords(RecordLayerDemo.getDescriptor());
        b.getRecordType("Order").setPrimaryKey(Key.Expressions.field("order_id"));
        b.getRecordType("Customer").setPrimaryKey(Key.Expressions.field("customer_id"));
        b.getRecordType("TypedRecord").setPrimaryKey(Key.Expressions.field("id"));
        b.addIndex("Order", new Index("agg_min_ever_price",
            new GroupingKeyExpression(Key.Expressions.field("price"), 1),
            IndexTypes.MIN_EVER_LONG));
        return b.build();
    }

    private static FDBRecordStore openMinEverAggregateStore(FDBRecordContext context, byte[] subspace) {
        return FDBRecordStore.newBuilder()
            .setMetaDataProvider(createMinEverAggregateMetaData())
            .setContext(context)
            .setSubspace(new Subspace(subspace))
            .setUserVersionChecker(ALWAYS_READABLE_CHECKER)
            .createOrOpen();
    }

    @ConformanceStep("saveOrderWithMinEverAggregate")
    public void saveOrderWithMinEverAggregate(String clusterFile, byte[] subspace, Order order, String tenantName) {
        runInContext(clusterFile, tenantName, context -> {
            FDBRecordStore store = openMinEverAggregateStore(context, subspace);
            store.saveRecord(order);
            return null;
        });
    }

    @ConformanceStep("evaluateMinEverAggregate")
    public long evaluateMinEverAggregate(String clusterFile, byte[] subspace, String tenantName) {
        return runInContext(clusterFile, tenantName, context -> {
            FDBRecordStore store = openMinEverAggregateStore(context, subspace);
            IndexAggregateFunction fn = new IndexAggregateFunction(
                FunctionNames.MIN_EVER,
                new GroupingKeyExpression(Key.Expressions.field("price"), 1),
                "agg_min_ever_price");
            return store.evaluateAggregateFunction(
                Collections.singletonList("Order"), fn, com.apple.foundationdb.record.TupleRange.ALL,
                IsolationLevel.SERIALIZABLE)
                .join()
                .getLong(0);
        });
    }

    // ========== MAX_EVER via MAX_EVER_LONG index ==========

    private static RecordMetaData createMaxEverAggregateMetaData() {
        RecordMetaDataBuilder b = RecordMetaData.newBuilder()
            .setRecords(RecordLayerDemo.getDescriptor());
        b.getRecordType("Order").setPrimaryKey(Key.Expressions.field("order_id"));
        b.getRecordType("Customer").setPrimaryKey(Key.Expressions.field("customer_id"));
        b.getRecordType("TypedRecord").setPrimaryKey(Key.Expressions.field("id"));
        b.addIndex("Order", new Index("agg_max_ever_price",
            new GroupingKeyExpression(Key.Expressions.field("price"), 1),
            IndexTypes.MAX_EVER_LONG));
        return b.build();
    }

    private static FDBRecordStore openMaxEverAggregateStore(FDBRecordContext context, byte[] subspace) {
        return FDBRecordStore.newBuilder()
            .setMetaDataProvider(createMaxEverAggregateMetaData())
            .setContext(context)
            .setSubspace(new Subspace(subspace))
            .setUserVersionChecker(ALWAYS_READABLE_CHECKER)
            .createOrOpen();
    }

    @ConformanceStep("saveOrderWithMaxEverAggregate")
    public void saveOrderWithMaxEverAggregate(String clusterFile, byte[] subspace, Order order, String tenantName) {
        runInContext(clusterFile, tenantName, context -> {
            FDBRecordStore store = openMaxEverAggregateStore(context, subspace);
            store.saveRecord(order);
            return null;
        });
    }

    @ConformanceStep("evaluateMaxEverAggregate")
    public long evaluateMaxEverAggregate(String clusterFile, byte[] subspace, String tenantName) {
        return runInContext(clusterFile, tenantName, context -> {
            FDBRecordStore store = openMaxEverAggregateStore(context, subspace);
            IndexAggregateFunction fn = new IndexAggregateFunction(
                FunctionNames.MAX_EVER,
                new GroupingKeyExpression(Key.Expressions.field("price"), 1),
                "agg_max_ever_price");
            return store.evaluateAggregateFunction(
                Collections.singletonList("Order"), fn, com.apple.foundationdb.record.TupleRange.ALL,
                IsolationLevel.SERIALIZABLE)
                .join()
                .getLong(0);
        });
    }

    // ========== MIN via VALUE index ==========

    private static RecordMetaData createMinValueAggregateMetaData() {
        RecordMetaDataBuilder b = RecordMetaData.newBuilder()
            .setRecords(RecordLayerDemo.getDescriptor());
        b.getRecordType("Order").setPrimaryKey(Key.Expressions.field("order_id"));
        b.getRecordType("Customer").setPrimaryKey(Key.Expressions.field("customer_id"));
        b.getRecordType("TypedRecord").setPrimaryKey(Key.Expressions.field("id"));
        b.addIndex("Order", new Index("agg_price_value",
            Key.Expressions.field("price"), IndexTypes.VALUE));
        return b.build();
    }

    private static FDBRecordStore openMinValueAggregateStore(FDBRecordContext context, byte[] subspace) {
        return FDBRecordStore.newBuilder()
            .setMetaDataProvider(createMinValueAggregateMetaData())
            .setContext(context)
            .setSubspace(new Subspace(subspace))
            .setUserVersionChecker(ALWAYS_READABLE_CHECKER)
            .createOrOpen();
    }

    @ConformanceStep("saveOrderWithMinValueAggregate")
    public void saveOrderWithMinValueAggregate(String clusterFile, byte[] subspace, Order order, String tenantName) {
        runInContext(clusterFile, tenantName, context -> {
            FDBRecordStore store = openMinValueAggregateStore(context, subspace);
            store.saveRecord(order);
            return null;
        });
    }

    @ConformanceStep("evaluateMinAggregate")
    public long evaluateMinAggregate(String clusterFile, byte[] subspace, String tenantName) {
        return runInContext(clusterFile, tenantName, context -> {
            FDBRecordStore store = openMinValueAggregateStore(context, subspace);
            IndexAggregateFunction fn = new IndexAggregateFunction(
                FunctionNames.MIN,
                Key.Expressions.field("price"),
                "agg_price_value");
            return store.evaluateAggregateFunction(
                Collections.singletonList("Order"), fn, com.apple.foundationdb.record.TupleRange.ALL,
                IsolationLevel.SERIALIZABLE)
                .join()
                .getLong(0);
        });
    }

    // ========== MAX via VALUE index ==========
    // Reuses same metadata as MIN (same VALUE index on price)

    @ConformanceStep("saveOrderWithMaxValueAggregate")
    public void saveOrderWithMaxValueAggregate(String clusterFile, byte[] subspace, Order order, String tenantName) {
        runInContext(clusterFile, tenantName, context -> {
            FDBRecordStore store = openMinValueAggregateStore(context, subspace);
            store.saveRecord(order);
            return null;
        });
    }

    @ConformanceStep("evaluateMaxAggregate")
    public long evaluateMaxAggregate(String clusterFile, byte[] subspace, String tenantName) {
        return runInContext(clusterFile, tenantName, context -> {
            FDBRecordStore store = openMinValueAggregateStore(context, subspace);
            IndexAggregateFunction fn = new IndexAggregateFunction(
                FunctionNames.MAX,
                Key.Expressions.field("price"),
                "agg_price_value");
            return store.evaluateAggregateFunction(
                Collections.singletonList("Order"), fn, com.apple.foundationdb.record.TupleRange.ALL,
                IsolationLevel.SERIALIZABLE)
                .join()
                .getLong(0);
        });
    }
}
