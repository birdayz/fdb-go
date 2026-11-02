package com.birdayz.conformance;

import com.apple.foundationdb.tuple.Tuple;
import com.apple.foundationdb.tuple.Versionstamp;
import com.google.gson.Gson;
import com.google.gson.reflect.TypeToken;

import java.math.BigInteger;
import java.util.ArrayList;
import java.util.List;
import java.util.Map;
import java.util.UUID;

/**
 * Java's Tuple.compareTo over tuples built from their elements, so with no
 * packed bytes cached: TupleUtil.compareItems element by element
 * (IterableComparator). Go's compareTuples must order every pair the same.
 */
class TupleCompareSteps {
    /** An element: {t: type, v: value}; a tuple is {t: "tuple", v: [elements]}. */
    @SuppressWarnings("unchecked")
    private static Object element(Map<String, Object> e) {
        String t = (String) e.get("t");
        Object v = e.get("v");
        switch (t) {
            case "null":
                return null;
            case "long":
                return Long.parseLong((String) v);
            case "bigint":
                return new BigInteger((String) v);
            case "string":
                return v;
            case "bytes": {
                List<Number> arr = (List<Number>) v;
                byte[] b = new byte[arr.size()];
                for (int i = 0; i < b.length; i++) {
                    b[i] = arr.get(i).byteValue();
                }
                return b;
            }
            case "float":
                return Float.intBitsToFloat((int) Long.parseLong((String) v));
            case "double":
                return Double.longBitsToDouble(Long.parseUnsignedLong((String) v));
            case "bool":
                return v;
            case "uuid":
                return UUID.fromString((String) v);
            case "versionstamp": {
                List<Number> arr = (List<Number>) v;
                byte[] b = new byte[12];
                for (int i = 0; i < 12; i++) {
                    b[i] = arr.get(i).byteValue();
                }
                return Versionstamp.fromBytes(b);
            }
            case "incomplete_versionstamp":
                return Versionstamp.incomplete(Integer.parseInt((String) v));
            case "tuple":
                return tuple((List<Map<String, Object>>) v);
            default:
                throw new IllegalArgumentException("unknown element type " + t);
        }
    }

    private static Tuple tuple(List<Map<String, Object>> elements) {
        List<Object> items = new ArrayList<>();
        for (Map<String, Object> e : elements) {
            items.add(element(e));
        }
        return Tuple.fromList(items);
    }

    /** tuplesJson is a list of tuples; returns signum(a.compareTo(b)) for every ordered pair, row-major. */
    @ConformanceStep("compareTuplesNatural")
    public List<Integer> compareTuplesNatural(String tuplesJson) {
        List<List<Map<String, Object>>> parsed = new Gson().fromJson(tuplesJson, new TypeToken<List<List<Map<String, Object>>>>(){}.getType());
        List<Tuple> tuples = new ArrayList<>();
        for (List<Map<String, Object>> t : parsed) {
            tuples.add(tuple(t));
        }
        List<Integer> out = new ArrayList<>();
        for (Tuple a : tuples) {
            for (Tuple b : tuples) {
                out.add(Integer.signum(a.compareTo(b)));
            }
        }
        return out;
    }
}
