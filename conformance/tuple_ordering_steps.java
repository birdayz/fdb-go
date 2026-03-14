package com.birdayz.conformance;

import com.apple.foundationdb.tuple.Tuple;
import com.apple.foundationdb.tuple.Versionstamp;
import com.google.gson.Gson;
import com.google.gson.GsonBuilder;
import com.google.gson.ToNumberPolicy;
import com.google.gson.reflect.TypeToken;

import java.lang.reflect.Type;
import java.util.*;

/**
 * Conformance steps for verifying tuple ordering between Go and Java.
 * Tests that Go's bytes.Compare(tuple.Pack(a), tuple.Pack(b)) produces
 * the same ordering as Java's Tuple.pack() byte comparison.
 */
class TupleOrderingSteps {

    private static final Gson gson = new GsonBuilder()
        .setObjectToNumberStrategy(ToNumberPolicy.LONG_OR_DOUBLE)
        .create();

    /**
     * Compare pairs of tuple values and return the comparison results.
     * Input: JSON array of [{a: {type, value}, b: {type, value}}, ...]
     * Output: array of {cmp: -1|0|1} results.
     */
    @ConformanceStep("compareTupleOrdering")
    public List<Map<String, Object>> compareTupleOrdering(String pairsJson) {
        Type listType = new TypeToken<List<Map<String, Object>>>(){}.getType();
        List<Map<String, Object>> pairs = gson.fromJson(pairsJson, listType);

        List<Map<String, Object>> results = new ArrayList<>();
        for (Map<String, Object> pair : pairs) {
            @SuppressWarnings("unchecked")
            Map<String, Object> aTyped = (Map<String, Object>) pair.get("a");
            @SuppressWarnings("unchecked")
            Map<String, Object> bTyped = (Map<String, Object>) pair.get("b");

            Object aValue = toTupleValue(aTyped);
            Object bValue = toTupleValue(bTyped);

            Tuple tA = singletonTuple(aValue);
            Tuple tB = singletonTuple(bValue);

            byte[] packedA = tA.pack();
            byte[] packedB = tB.pack();

            int cmp = compareUnsigned(packedA, packedB);

            Map<String, Object> result = new HashMap<>();
            result.put("cmp", Integer.signum(cmp));
            results.add(result);
        }
        return results;
    }

    /** Create a single-element tuple, safely handling null. */
    private Tuple singletonTuple(Object value) {
        List<Object> items = new ArrayList<>();
        items.add(value);
        return Tuple.fromList(items);
    }

    /** Convert a typed JSON value to the appropriate Java tuple type. */
    @SuppressWarnings("unchecked")
    private Object toTupleValue(Map<String, Object> typed) {
        String type = (String) typed.get("type");
        switch (type) {
            case "NULL":
                return null;
            case "INT64": {
                Number n = (Number) typed.get("value");
                return n.longValue();
            }
            case "FLOAT64": {
                Number n = (Number) typed.get("value");
                return n.doubleValue();
            }
            case "FLOAT64_SPECIAL": {
                String special = (String) typed.get("value");
                switch (special) {
                    case "NaN": return Double.NaN;
                    case "Infinity": return Double.POSITIVE_INFINITY;
                    case "-Infinity": return Double.NEGATIVE_INFINITY;
                    case "-0.0": return -0.0;
                    default: throw new IllegalArgumentException("Unknown float64 special: " + special);
                }
            }
            case "FLOAT32": {
                Number n = (Number) typed.get("value");
                return n.floatValue();
            }
            case "FLOAT32_SPECIAL": {
                String special = (String) typed.get("value");
                switch (special) {
                    case "NaN": return Float.NaN;
                    case "Infinity": return Float.POSITIVE_INFINITY;
                    case "-Infinity": return Float.NEGATIVE_INFINITY;
                    case "-0.0": return -0.0f;
                    default: throw new IllegalArgumentException("Unknown float32 special: " + special);
                }
            }
            case "STRING":
                return (String) typed.get("value");
            case "BYTES": {
                List<Number> arr = (List<Number>) typed.get("value");
                byte[] bytes = new byte[arr.size()];
                for (int i = 0; i < arr.size(); i++) {
                    bytes[i] = arr.get(i).byteValue();
                }
                return bytes;
            }
            case "BOOL":
                return (Boolean) typed.get("value");
            case "UUID": {
                String uuidStr = (String) typed.get("value");
                return UUID.fromString(uuidStr);
            }
            case "VERSIONSTAMP": {
                List<Number> arr = (List<Number>) typed.get("value");
                byte[] bytes = new byte[arr.size()];
                for (int i = 0; i < arr.size(); i++) {
                    bytes[i] = arr.get(i).byteValue();
                }
                if (bytes.length == 10) {
                    return Versionstamp.complete(bytes);
                } else if (bytes.length == 12) {
                    byte[] global = new byte[10];
                    System.arraycopy(bytes, 0, global, 0, 10);
                    int local = ((bytes[10] & 0xFF) << 8) | (bytes[11] & 0xFF);
                    return Versionstamp.complete(global, local);
                }
                throw new IllegalArgumentException("Versionstamp must be 10 or 12 bytes, got " + bytes.length);
            }
            default:
                throw new IllegalArgumentException("Unknown tuple type: " + type);
        }
    }

    /** Unsigned byte comparison, matching FDB's ByteArrayUtil.compareTo. */
    private static int compareUnsigned(byte[] a, byte[] b) {
        int len = Math.min(a.length, b.length);
        for (int i = 0; i < len; i++) {
            int ai = a[i] & 0xFF;
            int bi = b[i] & 0xFF;
            if (ai != bi) return ai < bi ? -1 : 1;
        }
        if (a.length != b.length) return a.length < b.length ? -1 : 1;
        return 0;
    }
}
