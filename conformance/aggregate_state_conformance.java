package com.birdayz.conformance;

import com.apple.foundationdb.record.RecordCursorProto;
import com.apple.foundationdb.record.query.plan.cascades.typing.Type;
import com.apple.foundationdb.record.query.plan.cascades.typing.TypeRepository;
import com.apple.foundationdb.record.query.plan.cascades.values.Accumulator;
import com.apple.foundationdb.record.query.plan.cascades.values.ArrayAggValue;
import com.apple.foundationdb.record.query.plan.cascades.values.CountValue;
import com.apple.foundationdb.record.query.plan.cascades.values.LiteralValue;
import com.apple.foundationdb.record.query.plan.cascades.values.NumericAggregationValue;
import com.google.gson.JsonArray;
import com.google.gson.JsonElement;
import com.google.gson.JsonObject;
import com.google.gson.JsonParser;
import com.google.protobuf.ByteString;
import com.google.protobuf.Descriptors;
import com.google.protobuf.DynamicMessage;
import com.google.protobuf.Message;

import java.util.ArrayList;
import java.util.Base64;
import java.util.HashMap;
import java.util.List;
import java.util.Map;
import java.util.Optional;

/**
 * The partial state of Java's streaming aggregation: the bytes its accumulators
 * write into a continuation (AccumulatorState) and the grouping key message, and
 * what its accumulators finish with when resumed from given state bytes.
 *
 * <p>The spec is JSON: {"groupKey": [{"type": "LONG", "value": "5"}, ...],
 * "aggregates": [{"op": "SUM", "type": "LONG", "inputs": ["1", null], "state": base64?,
 * "ignoreNulls": bool, "limit": int}]}. Values are strings; BYTES is base64.
 */
class AggregateStateSteps extends ConformanceBase {

    private static Type.TypeCode typeCode(String name) {
        return Type.TypeCode.valueOf(name);
    }

    private static Object javaValue(String type, JsonElement value) {
        if (value == null || value.isJsonNull()) {
            return null;
        }
        String s = value.getAsString();
        switch (type) {
            case "INT": return Integer.parseInt(s);
            case "LONG": return Long.parseLong(s);
            case "FLOAT": return Float.parseFloat(s);
            case "DOUBLE": return Double.parseDouble(s);
            case "BOOLEAN": return Boolean.parseBoolean(s);
            case "STRING": return s;
            case "BYTES": return ByteString.copyFrom(Base64.getDecoder().decode(s));
            default: throw new IllegalArgumentException("unsupported type " + type);
        }
    }

    private static String render(Object value) {
        if (value == null) {
            return null;
        }
        if (value instanceof ByteString) {
            return Base64.getEncoder().encodeToString(((ByteString)value).toByteArray());
        }
        if (value instanceof byte[]) {
            return Base64.getEncoder().encodeToString((byte[])value);
        }
        if (value instanceof List) {
            List<String> out = new ArrayList<>();
            for (Object o : (List<?>)value) {
                out.add(render(o));
            }
            return out.toString();
        }
        return value.toString();
    }

    private static Accumulator accumulator(JsonObject agg, RecordCursorProto.AccumulatorState initial) {
        String op = agg.get("op").getAsString();
        String type = agg.has("type") ? agg.get("type").getAsString() : "LONG";
        switch (op) {
            case "COUNT":
            case "COUNT_STAR": {
                var physical = CountValue.PhysicalOperator.valueOf(op);
                return initial == null ? new CountValue.SumAccumulator(physical) : new CountValue.SumAccumulator(physical, initial);
            }
            case "ARRAY_AGG": {
                Type element = Type.primitiveType(typeCode(type), true);
                var value = new ArrayAggValue(new LiteralValue<>(element, null),
                        agg.get("ignoreNulls").getAsBoolean(), agg.get("limit").getAsInt());
                var repository = TypeRepository.newBuilder().addTypeIfNeeded(value.getResultType()).build();
                return value.createAccumulatorWithInitialState(repository, initial == null ? null : List.of(initial));
            }
            default: {
                String lane = type.substring(0, 1);
                var physical = NumericAggregationValue.PhysicalOperator.valueOf(op + "_" + lane);
                return initial == null ? new NumericAggregationValue.NumericAccumulator(physical)
                                       : new NumericAggregationValue.NumericAccumulator(physical, initial);
            }
        }
    }

    private static Object partial(JsonObject agg, Object input) {
        String op = agg.get("op").getAsString();
        String type = agg.has("type") ? agg.get("type").getAsString() : "LONG";
        switch (op) {
            case "COUNT":
            case "COUNT_STAR":
                return CountValue.PhysicalOperator.valueOf(op).evalInitialToPartial(input);
            case "ARRAY_AGG":
                return input;
            default:
                return NumericAggregationValue.PhysicalOperator.valueOf(op + "_" + type.substring(0, 1)).evalInitialToPartial(input);
        }
    }

    @ConformanceStep("aggregatePartialState")
    public Map<String, Object> aggregatePartialState(String spec) throws Exception {
        JsonObject root = JsonParser.parseString(spec).getAsJsonObject();
        Map<String, Object> response = new HashMap<>();

        JsonArray groupKey = root.getAsJsonArray("groupKey");
        if (groupKey != null) {
            List<Type.Record.Field> fields = new ArrayList<>();
            for (int i = 0; i < groupKey.size(); i++) {
                String type = groupKey.get(i).getAsJsonObject().get("type").getAsString();
                fields.add(Type.Record.Field.of(Type.primitiveType(typeCode(type), true), Optional.of("_" + i)));
            }
            Type.Record recordType = Type.Record.fromFields(false, fields);
            TypeRepository repository = TypeRepository.newBuilder().addTypeIfNeeded(recordType).build();
            DynamicMessage.Builder builder = repository.newMessageBuilder(recordType);
            Descriptors.Descriptor descriptor = builder.getDescriptorForType();
            for (int i = 0; i < groupKey.size(); i++) {
                JsonObject field = groupKey.get(i).getAsJsonObject();
                Object value = javaValue(field.get("type").getAsString(), field.get("value"));
                if (value != null) {
                    builder.setField(descriptor.getFields().get(i), value);
                }
            }
            Message key = builder.build();
            response.put("groupKey", Base64.getEncoder().encodeToString(key.toByteArray()));
        }

        List<String> states = new ArrayList<>();
        List<String> finished = new ArrayList<>();
        for (JsonElement element : root.getAsJsonArray("aggregates")) {
            JsonObject agg = element.getAsJsonObject();
            String type = agg.has("type") ? agg.get("type").getAsString() : "LONG";
            RecordCursorProto.AccumulatorState initial = null;
            if (agg.has("state") && !agg.get("state").isJsonNull()) {
                initial = RecordCursorProto.AccumulatorState.parseFrom(Base64.getDecoder().decode(agg.get("state").getAsString()));
            }
            Accumulator accumulator = accumulator(agg, initial);
            if (agg.has("inputs")) {
                for (JsonElement input : agg.getAsJsonArray("inputs")) {
                    accumulator.accumulate(partial(agg, javaValue(type, input)));
                }
            }
            List<RecordCursorProto.AccumulatorState> written = accumulator.getAccumulatorStates();
            states.add(written.isEmpty() ? "" : Base64.getEncoder().encodeToString(written.get(0).toByteArray()));
            finished.add(render(accumulator.finish()));
        }
        response.put("states", states);
        response.put("finished", finished);
        return response;
    }
}
