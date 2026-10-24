package com.birdayz.conformance;

import com.apple.foundationdb.KeyValue;
import com.apple.foundationdb.record.RecordMetaData;
import com.apple.foundationdb.record.RecordMetaDataOptionsProto;
import com.apple.foundationdb.record.RecordMetaDataProto;
import com.apple.foundationdb.record.provider.foundationdb.FDBRecordStore;
import com.apple.foundationdb.record.provider.foundationdb.leaderboard.TimeWindowLeaderboardWindowUpdate;
import com.apple.foundationdb.record.query.expressions.Query;
import com.apple.foundationdb.subspace.Subspace;
import com.apple.foundationdb.tuple.Tuple;
import com.google.protobuf.Descriptors;
import com.google.protobuf.DynamicMessage;
import com.google.protobuf.ExtensionRegistry;
import com.google.protobuf.InvalidProtocolBufferException;

import java.util.ArrayList;
import java.util.Arrays;
import java.util.Collections;
import java.util.HashMap;
import java.util.HexFormat;
import java.util.List;
import java.util.Map;

class TupleRangeProbeSteps extends ConformanceBase {
    private static final ExtensionRegistry EXTENSION_REGISTRY;
    static {
        EXTENSION_REGISTRY = ExtensionRegistry.newInstance();
        RecordMetaDataOptionsProto.registerAllExtensions(EXTENSION_REGISTRY);
    }

    /**
     * Runs ops on the store at subspace under metaData, each in its own transaction, and returns
     * each op's verdict ("ok", or the root exception's full class name), the record and legacy
     * version keys (spaces 1 and 8, whose values carry commit versions), and the key-value pairs
     * of the index, secondary, record count and uniqueness violation spaces (2, 3, 4, 7), all
     * relative to the subspace. An op is "save" (arg: a serialized recordTypeName), "delete"
     * (arg: a packed primary key), "deleteWhere:TYPE:FIELD" (arg: a packed one-element tuple,
     * deleteRecordsWhere(TYPE, FIELD == value)), "writeOnly:INDEX", "resolve:INDEX" (arg: a packed
     * (valueKey, remainPrimaryKey) of nested tuples) or "allTimeWindow:INDEX" (an all-time
     * leaderboard window at update timestamp 500). formatVersion 0 is the default.
     */
    @ConformanceStep("tupleRangeOpsJava")
    public Map<String, Object> tupleRangeOpsJava(String clusterFile, byte[] subspace, byte[] metaData,
                                                 String recordTypeName, String[] ops, byte[][] args,
                                                 int formatVersion) throws InvalidProtocolBufferException {
        final RecordMetaData md = RecordMetaData.build(RecordMetaDataProto.MetaData.parseFrom(metaData, EXTENSION_REGISTRY));
        final Descriptors.Descriptor descriptor = md.getRecordType(recordTypeName).getDescriptor();
        final Subspace ss = new Subspace(subspace);
        final List<String> verdicts = new ArrayList<>();
        for (int i = 0; i < ops.length; i++) {
            final String[] op = ops[i].split(":");
            final byte[] arg = args[i];
            final DynamicMessage record = "save".equals(op[0]) ? DynamicMessage.parseFrom(descriptor, arg) : null;
            try {
                runInContext(clusterFile, null, context -> {
                    final FDBRecordStore.Builder builder = FDBRecordStore.newBuilder().setMetaDataProvider(md)
                            .setContext(context).setSubspace(ss).setUserVersionChecker(ALWAYS_READABLE_CHECKER);
                    if (formatVersion > 0) {
                        builder.setFormatVersion(formatVersion);
                    }
                    final FDBRecordStore store = builder.createOrOpen();
                    switch (op[0]) {
                        case "save":
                            store.saveRecord(record);
                            break;
                        case "delete":
                            store.deleteRecord(Tuple.fromBytes(arg));
                            break;
                        case "deleteWhere":
                            store.deleteRecordsWhere(op[1], Query.field(op[2]).equalsValue(Tuple.fromBytes(arg).get(0)));
                            break;
                        case "writeOnly":
                            store.markIndexWriteOnly(op[1]).join();
                            break;
                        case "resolve":
                            final Tuple t = Tuple.fromBytes(arg);
                            store.resolveUniquenessViolation(md.getIndex(op[1]), t.getNestedTuple(0), t.getNestedTuple(1)).join();
                            break;
                        case "allTimeWindow":
                            store.performIndexOperation(op[1], new TimeWindowLeaderboardWindowUpdate(
                                    500, false, 0, true, Collections.emptyList(),
                                    TimeWindowLeaderboardWindowUpdate.Rebuild.IF_OVERLAPPING_CHANGED));
                            break;
                        default:
                            throw new IllegalArgumentException("unknown op " + String.join(":", op));
                    }
                    return null;
                });
                verdicts.add("ok");
            } catch (RuntimeException ex) {
                Throwable cause = ex;
                while (cause.getCause() != null && cause.getCause() != cause) {
                    cause = cause.getCause();
                }
                verdicts.add(cause.getClass().getName());
            }
        }
        final Map<String, Object> result = new HashMap<>();
        result.put("verdicts", verdicts);
        result.put("keys", dump(clusterFile, ss, false, 1L, 8L));
        result.put("kvs", dump(clusterFile, ss, true, 2L, 3L, 4L, 7L));
        return result;
    }

    private List<List<String>> dump(String clusterFile, Subspace ss, boolean withValues, long... spaces) {
        return runInContext(clusterFile, null, context -> {
            final byte[] prefix = ss.getKey();
            final List<List<String>> out = new ArrayList<>();
            for (long space : spaces) {
                for (KeyValue kv : context.ensureActive().getRange(ss.range(Tuple.from(space))).asList().join()) {
                    final String key = HexFormat.of().formatHex(Arrays.copyOfRange(kv.getKey(), prefix.length, kv.getKey().length));
                    out.add(withValues ? List.of(key, HexFormat.of().formatHex(kv.getValue())) : List.of(key));
                }
            }
            return out;
        });
    }
}
