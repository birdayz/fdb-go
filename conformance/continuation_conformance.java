package com.birdayz.conformance;

import com.apple.foundationdb.record.ExecuteProperties;
import com.apple.foundationdb.record.EvaluationContext;
import com.apple.foundationdb.record.Bindings;
import com.apple.foundationdb.record.query.plan.cascades.values.Value;
import com.apple.foundationdb.record.query.plan.cascades.values.FieldValue;
import com.apple.foundationdb.record.query.plan.cascades.Quantifier;
import com.apple.foundationdb.record.query.plan.cascades.Column;
import com.apple.foundationdb.record.query.plan.cascades.typing.TypeRepository;
import com.apple.foundationdb.record.query.plan.cascades.values.RecordConstructorValue;
import com.apple.foundationdb.record.query.plan.cascades.values.AbstractArrayConstructorValue;
import com.apple.foundationdb.record.query.plan.cascades.Reference;
import com.apple.foundationdb.record.query.plan.cascades.typing.Type;
import com.apple.foundationdb.record.query.plan.cascades.values.LiteralValue;
import com.apple.foundationdb.record.query.plan.cascades.values.ParameterObjectValue;
import com.apple.foundationdb.record.query.plan.plans.RecordQueryDefaultOnEmptyPlan;
import com.apple.foundationdb.record.query.plan.plans.RecordQueryPlan;
import com.apple.foundationdb.record.query.plan.plans.QueryResult;
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
import java.util.Optional;

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

    @ConformanceStep("defaultBinding")
    public Map<String, Object> defaultBinding(String clusterFile, byte[] subspace, boolean all, boolean empty, boolean sameAlias, String tenantName) {
        return runInContext(clusterFile, tenantName, context -> {
            FDBRecordStore store = FDBRecordStore.newBuilder()
                .setMetaDataProvider(createMetaData()).setContext(context)
                .setSubspace(new Subspace(subspace)).createOrOpen();
            var type = Type.primitiveType(Type.TypeCode.LONG);
            var inner = new RecordQueryExplodePlan(new LiteralValue<>(new Type.Array(type), empty ? List.<Long>of() : List.of(11L)));
            var quantifier = Quantifier.physical(Reference.plannedOf(inner));
            Value fallback = sameAlias ? quantifier.getFlowedObjectValue() : ParameterObjectValue.of("fallback", type);
            var binding = sameAlias ? Bindings.Internal.CORRELATION.bindingName(quantifier.getAlias().getId()) : "fallback";
            RecordQueryPlan plan = all ? new RecordQueryDefaultOnEmptyPlan(quantifier, fallback)
                : new RecordQueryFirstOrDefaultPlan(quantifier, fallback);
            try (var cursor = plan.executePlan(store, EvaluationContext.forBinding(binding, sameAlias ? QueryResult.ofComputed(99L) : 99L), null, ExecuteProperties.SERIAL_EXECUTE)) {
                var first = cursor.getNext();
                if (!first.hasNext()) {
                    throw new IllegalStateException("default plan produced no row");
                }
                return Map.of("value", first.get().getDatum(), "exhausted", !cursor.getNext().hasNext());
            }
        });
    }

    @ConformanceStep("explodeProtoShape")
    public Map<String, Object> explodeProtoShape(String clusterFile, byte[] subspace, boolean nullable, boolean nestedArray, boolean duplicateNames, String tenantName) {
        return runInContext(clusterFile, tenantName, context -> {
            FDBRecordStore store = FDBRecordStore.newBuilder()
                .setMetaDataProvider(createMetaData()).setContext(context)
                .setSubspace(new Subspace(subspace)).createOrOpen();
            var type = Type.Record.fromDescriptor(Order.getDescriptor()).withNullability(nullable);
            if (duplicateNames) {
                var fields = new ArrayList<>(type.getFields());
                var second = fields.get(1);
                fields.set(1, Type.Record.Field.of(second.getFieldType(), fields.get(0).getFieldNameOptional(), second.getFieldIndexOptional()));
                type = Type.Record.fromFields(nullable, fields);
            }
            if (nestedArray) {
                var fields = new ArrayList<>(type.getFields());
                var tags = fields.get(3);
                fields.set(3, Type.Record.Field.of(tags.getFieldType().withNullability(true), tags.getFieldNameOptional(), tags.getFieldIndexOptional()));
                type = Type.Record.fromFields(nullable, fields);
            }
            var item = Order.newBuilder().setOrderId(99L).addTags("a").build();
            var plan = new RecordQueryExplodePlan(new LiteralValue<>(new Type.Array(type), List.of(item)));
            try (var cursor = plan.executePlan(store, EvaluationContext.empty(), null, ExecuteProperties.SERIAL_EXECUTE)) {
                var row = cursor.getNext();
                if (!row.hasNext()) {
                    throw new IllegalStateException("explode produced no row");
                }
                var edge = Quantifier.physical(Reference.plannedOf(plan));
                var projected = new RecordQueryMapPlan(edge, FieldValue.ofOrdinalNumber(edge.getFlowedObjectValue(), 3));
                try (var tags = projected.executePlan(store, EvaluationContext.empty(), null, ExecuteProperties.SERIAL_EXECUTE)) {
                    return Map.of("value", Order.newBuilder().mergeFrom((Message)row.get().getMessage()).build().getOrderId(),
                        "nullable", plan.getResultValue().getResultType().isNullable(), "tags", tags.getNext().get().getDatum());
                }
            }
        });
    }

    @ConformanceStep("explodeIndependentRepositories")
    public Map<String, Object> explodeIndependentRepositories(String clusterFile, byte[] subspace, boolean nullable, String tenantName) {
        return runInContext(clusterFile, tenantName, context -> {
            FDBRecordStore store = FDBRecordStore.newBuilder().setMetaDataProvider(createMetaData()).setContext(context)
                .setSubspace(new Subspace(subspace)).createOrOpen();
            var record = RecordConstructorValue.ofColumns(List.of(
                Column.of(Optional.of("Z"), LiteralValue.ofScalar(7)), Column.of(Optional.of("A"), LiteralValue.ofScalar(9L))));
            var plan = new RecordQueryExplodePlan(AbstractArrayConstructorValue.LightArrayConstructorValue.of(
                List.of(record), record.getResultType().withNullability(nullable)));
            var firstRepo = TypeRepository.newBuilder().addTypeIfNeeded(record.getResultType()).build();
            var secondRepo = TypeRepository.newBuilder().addTypeIfNeeded(record.getResultType()).build();
            if (firstRepo.getMessageDescriptor(record.getResultType()) == secondRepo.getMessageDescriptor(record.getResultType())) {
                throw new IllegalStateException("independent repositories shared a descriptor");
            }
            List<Object> results = new ArrayList<>();
            for (var repository : List.of(firstRepo, secondRepo)) {
                try (var cursor = plan.executePlan(store, EvaluationContext.forTypeRepository(repository), null, ExecuteProperties.SERIAL_EXECUTE)) {
                    Message message = cursor.getNext().get().getMessage();
                    if (message.getDescriptorForType() != repository.getMessageDescriptor(record.getResultType())) {
                        throw new IllegalStateException("constructor did not use the supplied repository");
                    }
                    results.add(List.of(message.getField(message.getDescriptorForType().getFields().get(0)),
                        message.getField(message.getDescriptorForType().getFields().get(1))));
                }
            }
            return Map.of("rows", results);
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
