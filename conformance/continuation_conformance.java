package com.birdayz.conformance;

import com.apple.foundationdb.record.ExecuteProperties;
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
