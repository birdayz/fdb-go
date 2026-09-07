package com.birdayz.conformance;

import com.apple.foundationdb.record.ExecuteProperties;
import com.apple.foundationdb.record.EvaluationContext;
import com.apple.foundationdb.record.query.plan.cascades.Quantifier;
import com.apple.foundationdb.record.query.plan.cascades.Reference;
import com.apple.foundationdb.record.query.plan.cascades.typing.Type;
import com.apple.foundationdb.record.query.plan.cascades.values.LiteralValue;
import com.apple.foundationdb.record.query.plan.plans.RecordQueryPlan;
import com.apple.foundationdb.record.query.plan.plans.RecordQueryExplodePlan;
import com.apple.foundationdb.record.query.plan.plans.RecordQueryFirstOrDefaultPlan;
import com.apple.foundationdb.record.query.plan.plans.RecordQueryMapPlan;
import com.apple.foundationdb.record.ScanProperties;
import com.apple.foundationdb.record.provider.foundationdb.FDBRecordStore;
import com.apple.foundationdb.record.provider.foundationdb.FDBStoredRecord;
import com.apple.foundationdb.record.RecordLayerDemo.Order;
import com.apple.foundationdb.subspace.Subspace;
import com.google.protobuf.Message;

import java.util.ArrayList;
import java.util.Base64;
import java.util.HashMap;
import java.util.List;
import java.util.Map;

class ContinuationSteps extends ConformanceBase {
    @ConformanceStep("firstOrDefaultRequest")
    public Map<String, Object> firstOrDefaultRequest(String clusterFile, byte[] subspace, int count, int skip, boolean mapped, String tenantName) {
        return runInContext(clusterFile, tenantName, context -> {
            FDBRecordStore store = FDBRecordStore.newBuilder()
                .setMetaDataProvider(createMetaData()).setContext(context)
                .setSubspace(new Subspace(subspace)).createOrOpen();
            List<Long> items = new ArrayList<>();
            for (int i = 0; i < count; i++) {
                items.add(11L * (i + 1));
            }
            var collection = new LiteralValue<>(new Type.Array(Type.primitiveType(Type.TypeCode.LONG)), items);
            var inner = new RecordQueryExplodePlan(collection);
            RecordQueryPlan plan = new RecordQueryFirstOrDefaultPlan(
                Quantifier.physical(Reference.plannedOf(inner)), LiteralValue.ofScalar(99L));
            if (mapped) {
                plan = new RecordQueryMapPlan(Quantifier.physical(Reference.plannedOf(plan)), LiteralValue.ofScalar(42L));
            }
            var props = ExecuteProperties.newBuilder().setSkip(skip).setReturnedRowLimit(1).build();
            try (var cursor = plan.executePlan(store, EvaluationContext.empty(), null, props)) {
                var first = cursor.getNext();
                if (!first.hasNext()) {
                    throw new IllegalStateException("first-or-default produced no row");
                }
                Map<String, Object> response = new HashMap<>();
                response.put("value", first.get().getDatum());
                response.put("rowResumable", !first.getContinuation().isEnd());
                var end = cursor.getNext();
                response.put("sourceExhausted", !end.hasNext() && end.getNoNextReason().isSourceExhausted());
                try (var resumed = plan.executePlan(store, EvaluationContext.empty(), first.getContinuation().toBytes(), props)) {
                    var resumedEnd = resumed.getNext();
                    response.put("resumeExhausted", !resumedEnd.hasNext() && resumedEnd.getNoNextReason().isSourceExhausted());
                }
                return response;
            }
        });
    }

    @ConformanceStep("scanOrdersWithContinuation")
    public Map<String, Object> scanOrdersWithContinuation(String clusterFile, byte[] subspace, int limit, String continuation, String tenantName) {
        return runInContext(clusterFile, tenantName, context -> {
            FDBRecordStore store = FDBRecordStore.newBuilder()
                .setMetaDataProvider(createMetaData())
                .setContext(context)
                .setSubspace(new Subspace(subspace))
                .createOrOpen();

            byte[] contBytes = null;
            if (continuation != null && !continuation.isEmpty()) {
                contBytes = Base64.getDecoder().decode(continuation);
            }

            ScanProperties scanProps = new ScanProperties(ExecuteProperties.newBuilder()
                .setReturnedRowLimit(limit)
                .build());

            com.apple.foundationdb.record.RecordCursor<FDBStoredRecord<Message>> cursor =
                store.scanRecords(contBytes, scanProps);

            List<Map<String, Object>> orders = new ArrayList<>();
            byte[] nextContinuation = null;

            com.apple.foundationdb.record.RecordCursorResult<FDBStoredRecord<Message>> result;
            while ((result = cursor.getNext()) != null && result.hasNext()) {
                FDBStoredRecord<Message> record = result.get();
                Order order = Order.newBuilder().mergeFrom(record.getRecord()).build();
                Map<String, Object> orderMap = new HashMap<>();
                orderMap.put("orderId", order.getOrderId());
                if (order.hasPrice()) {
                    orderMap.put("price", order.getPrice());
                }
                orders.add(orderMap);
            }
            if (result != null) {
                nextContinuation = result.getContinuation().toBytes();
            }

            Map<String, Object> response = new HashMap<>();
            response.put("orders", orders);
            if (nextContinuation != null) {
                response.put("continuation", Base64.getEncoder().encodeToString(nextContinuation));
            }
            if (result != null) {
                response.put("sourceExhausted", result.getNoNextReason().isSourceExhausted());
            }
            return response;
        });
    }
}
