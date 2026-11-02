package com.birdayz.conformance;

import com.apple.foundationdb.KeySelector;
import com.apple.foundationdb.KeyValue;
import com.apple.foundationdb.MutationType;
import com.apple.foundationdb.ReadTransaction;
import com.apple.foundationdb.Transaction;
import com.apple.foundationdb.map.BunchedMap;
import com.apple.foundationdb.record.provider.foundationdb.indexes.TextIndexBunchedSerializer;
import com.apple.foundationdb.subspace.Subspace;
import com.apple.foundationdb.tuple.Tuple;
import com.google.gson.Gson;
import com.google.gson.GsonBuilder;
import com.google.gson.ToNumberPolicy;
import com.google.gson.reflect.TypeToken;

import java.lang.reflect.InvocationTargetException;
import java.lang.reflect.Proxy;
import java.util.ArrayList;
import java.util.Comparator;
import java.util.HashMap;
import java.util.List;
import java.util.Map;
import java.util.Optional;
import java.util.TreeMap;
import java.util.concurrent.CompletableFuture;
import java.util.function.Function;

/**
 * Java's BunchedMap put and remove, each in its own transaction, with every
 * conflict range, write and snapshot read they issue recorded in order, and
 * the counts TextIndexMaintainer's InstrumentedBunchedMap keeps
 * (TextIndexMaintainer.java:556-610). Go runs the same operations and must
 * record the same.
 */
class BunchedMapSteps extends ConformanceBase {
    private static final Gson GSON = new GsonBuilder().setObjectToNumberStrategy(ToNumberPolicy.LONG_OR_DOUBLE).create();

    /** InstrumentedBunchedMap's counters over a plain BunchedMap of the TEXT serializer. */
    private static final class CountingBunchedMap extends BunchedMap<Tuple, List<Integer>> {
        final Map<String, Long> counts = new TreeMap<>();

        CountingBunchedMap(int bunchSize) {
            super(TextIndexBunchedSerializer.instance(), Comparator.naturalOrder(), bunchSize);
        }

        private void add(String name, long n) {
            counts.merge(name, n, Long::sum);
        }

        @Override
        protected void instrumentDelete(byte[] key, byte[] oldValue) {
            add("delete_index_key", 1);
            add("delete_index_key_bytes", key.length);
            if (oldValue != null) {
                add("delete_index_value_bytes", oldValue.length);
            }
        }

        @Override
        protected void instrumentWrite(byte[] key, byte[] value, byte[] oldValue) {
            add("save_index_key", 1);
            add("save_index_key_bytes", key.length);
            add("save_index_value_bytes", value.length);
            if (oldValue != null) {
                add("delete_index_value_bytes", oldValue.length);
            }
        }

        @Override
        protected CompletableFuture<List<KeyValue>> instrumentRangeRead(CompletableFuture<List<KeyValue>> readFuture) {
            return readFuture.whenComplete((list, err) -> {
                if (list != null && !list.isEmpty()) {
                    long keyBytes = 0;
                    long valueBytes = 0;
                    for (KeyValue kv : list) {
                        keyBytes += kv.getKey().length;
                        valueBytes += kv.getValue().length;
                    }
                    add("load_index_key", list.size());
                    add("load_index_key_bytes", keyBytes);
                    add("load_index_value_bytes", valueBytes);
                }
            });
        }
    }

    private static String hex(byte[] b) {
        StringBuilder sb = new StringBuilder(2 * b.length);
        for (byte x : b) {
            sb.append(Character.forDigit((x >> 4) & 0xf, 16)).append(Character.forDigit(x & 0xf, 16));
        }
        return sb.toString();
    }

    private static String selector(KeySelector s) {
        return hex(s.getKey()) + " " + s.orEqual() + " " + s.getOffset();
    }

    private static Object call(Object target, java.lang.reflect.Method method, Object[] args) throws Throwable {
        try {
            return method.invoke(target, args);
        } catch (InvocationTargetException e) {
            throw e.getCause();
        }
    }

    /** tr, recording the BunchedMap's operations on it into trace. */
    @SuppressWarnings("unchecked")
    private static Transaction recording(Transaction tr, List<String> trace) {
        return (Transaction) Proxy.newProxyInstance(Transaction.class.getClassLoader(), new Class<?>[] {Transaction.class},
            (proxy, method, args) -> {
                switch (method.getName()) {
                    case "runAsync":
                        // BunchedMap runs its body on the context it is given.
                        return ((Function<Transaction, CompletableFuture<?>>) args[0]).apply((Transaction) proxy);
                    case "snapshot": {
                        ReadTransaction snapshot = tr.snapshot();
                        return Proxy.newProxyInstance(ReadTransaction.class.getClassLoader(), new Class<?>[] {ReadTransaction.class},
                            (sp, sm, sargs) -> {
                                if (sm.getName().equals("getRange") && sargs != null && sargs.length >= 2 && sargs[0] instanceof KeySelector) {
                                    trace.add("snapshot_range " + selector((KeySelector) sargs[0]) + " " + selector((KeySelector) sargs[1]));
                                }
                                return call(snapshot, sm, sargs);
                            });
                    }
                    case "addReadConflictKey":
                        trace.add("read_conflict_key " + hex((byte[]) args[0]));
                        break;
                    case "addReadConflictRange":
                        trace.add("read_conflict_range " + hex((byte[]) args[0]) + " " + hex((byte[]) args[1]));
                        break;
                    case "addWriteConflictKey":
                        trace.add("write_conflict_key " + hex((byte[]) args[0]));
                        break;
                    case "addWriteConflictRange":
                        trace.add("write_conflict_range " + hex((byte[]) args[0]) + " " + hex((byte[]) args[1]));
                        break;
                    case "set":
                        trace.add("set " + hex((byte[]) args[0]) + " " + hex((byte[]) args[1]));
                        break;
                    case "clear":
                        if (args.length == 1 && args[0] instanceof byte[]) {
                            trace.add("clear " + hex((byte[]) args[0]));
                        } else {
                            trace.add("clear " + method.toGenericString());
                        }
                        break;
                    case "mutate":
                        trace.add("mutate " + args[0] + " " + hex((byte[]) args[1]) + " " + hex((byte[]) args[2]));
                        break;
                    default:
                        break;
                }
                return call(tr, method, args);
            });
    }

    /**
     * Runs ops ([{op: put|remove, map, key: [string, long], value: [int...]}])
     * on a BunchedMap of the given bunch size, the maps at subspace + (map).
     * Returns per op its trace, counts and result (the old value, or null).
     */
    @ConformanceStep("bunchedMapTrace")
    public List<Map<String, Object>> bunchedMapTrace(String clusterFile, byte[] subspace, String tenantName, int bunchSize, String opsJson) {
        List<Map<String, Object>> ops = GSON.fromJson(opsJson, new TypeToken<List<Map<String, Object>>>(){}.getType());
        List<Map<String, Object>> results = new ArrayList<>();
        for (Map<String, Object> op : ops) {
            Map<String, Object> result = runInContext(clusterFile, tenantName, context -> {
                CountingBunchedMap map = new CountingBunchedMap(bunchSize);
                List<String> trace = new ArrayList<>();
                Transaction tr = recording(context.ensureActive(), trace);
                Subspace mapSubspace = new Subspace(subspace).subspace(Tuple.from(op.get("map")));
                List<?> keyItems = (List<?>) op.get("key");
                Tuple key = Tuple.fromList(new ArrayList<>(keyItems));
                Optional<List<Integer>> old;
                if ("put".equals(op.get("op"))) {
                    // No list is TextIndexMaintainer's omitted position list:
                    // Collections.emptyList().
                    List<Integer> value = new ArrayList<>();
                    if (op.get("value") != null) {
                        for (Object v : (List<?>) op.get("value")) {
                            value.add(((Number) v).intValue());
                        }
                    }
                    old = map.put(tr, mapSubspace, key, value).join();
                } else {
                    old = map.remove(tr, mapSubspace, key).join();
                }
                Map<String, Object> out = new HashMap<>();
                out.put("trace", trace);
                out.put("counts", map.counts);
                out.put("old", old.orElse(null));
                return out;
            });
            results.add(result);
        }
        return results;
    }
}
