package com.birdayz.conformance;

import com.apple.foundationdb.record.provider.foundationdb.FDBDatabase;
import com.apple.foundationdb.record.provider.foundationdb.keyspace.DirectoryLayerDirectory;
import com.apple.foundationdb.record.provider.foundationdb.keyspace.ExtendedDirectoryLayer;
import com.apple.foundationdb.record.provider.foundationdb.keyspace.KeySpace;
import com.apple.foundationdb.record.provider.foundationdb.keyspace.KeySpaceDirectory;
import com.apple.foundationdb.record.provider.foundationdb.keyspace.KeySpacePath;
import com.google.gson.Gson;
import com.google.gson.GsonBuilder;
import com.google.gson.ToNumberPolicy;
import com.google.gson.reflect.TypeToken;

import java.util.ArrayList;
import java.util.HashMap;
import java.util.List;
import java.util.Map;
import java.util.NoSuchElementException;

/**
 * Java's KeySpacePath.toTuple / toSubspace over a key space with
 * DirectoryLayerDirectory nodes (resolved through ExtendedDirectoryLayer.global,
 * the default scope) and plain nodes, and the global directory layer's resolve
 * and reverseLookup. Go's keyspace and recordlayer.DirectoryLayerDirectory must
 * give the same bytes and the same mappings.
 */
class KeySpaceSteps extends ConformanceBase {
    private static final Gson GSON = new GsonBuilder().setObjectToNumberStrategy(ToNumberPolicy.LONG_OR_DOUBLE).create();

    /** keyspace_conformance_test.go builds the same key space. */
    private static KeySpace keySpace() {
        return new KeySpace(
            new DirectoryLayerDirectory("application")
                .addSubdirectory(new DirectoryLayerDirectory("run")
                    .addSubdirectory(new KeySpaceDirectory("store", KeySpaceDirectory.KeyType.LONG, 1L))
                    .addSubdirectory(new KeySpaceDirectory("name", KeySpaceDirectory.KeyType.STRING)
                        .addSubdirectory(new KeySpaceDirectory("part", KeySpaceDirectory.KeyType.LONG)))),
            new KeySpaceDirectory("plain", KeySpaceDirectory.KeyType.STRING)
                .addSubdirectory(new KeySpaceDirectory("id", KeySpaceDirectory.KeyType.LONG)
                    .addSubdirectory(new KeySpaceDirectory("nothing", KeySpaceDirectory.KeyType.NULL))));
    }

    private static String hex(byte[] b) {
        StringBuilder sb = new StringBuilder(2 * b.length);
        for (byte x : b) {
            sb.append(Character.forDigit((x >> 4) & 0xf, 16)).append(Character.forDigit(x & 0xf, 16));
        }
        return sb.toString();
    }

    /**
     * pathsJson is a list of paths, each a list of [directory, value] steps (a
     * null value for a constant or NULL directory). Returns per path its packed
     * toTuple and its toSubspace key, in hex.
     */
    @ConformanceStep("keySpacePaths")
    public List<Map<String, String>> keySpacePaths(String clusterFile, String pathsJson) {
        List<List<List<Object>>> paths = GSON.fromJson(pathsJson, new TypeToken<List<List<List<Object>>>>(){}.getType());
        KeySpace ks = keySpace();
        return runInContext(clusterFile, null, context -> {
            List<Map<String, String>> out = new ArrayList<>();
            for (List<List<Object>> steps : paths) {
                KeySpacePath path = null;
                for (List<Object> step : steps) {
                    String dir = (String) step.get(0);
                    Object value = step.size() > 1 ? step.get(1) : null;
                    if (path == null) {
                        path = value == null ? ks.path(dir) : ks.path(dir, value);
                    } else {
                        path = value == null ? path.add(dir) : path.add(dir, value);
                    }
                }
                Map<String, String> m = new HashMap<>();
                m.put("tuple", hex(path.toTuple(context).pack()));
                m.put("subspace", hex(path.toSubspace(context).getKey()));
                out.add(m);
            }
            return out;
        });
    }

    /** ExtendedDirectoryLayer.global's resolve of each name, each in its own transaction. */
    @ConformanceStep("directoryLayerResolve")
    public List<Long> directoryLayerResolve(String clusterFile, String namesJson) {
        List<String> names = GSON.fromJson(namesJson, new TypeToken<List<String>>(){}.getType());
        FDBDatabase db = createDatabase(clusterFile);
        List<Long> out = new ArrayList<>();
        for (String name : names) {
            out.add(ExtendedDirectoryLayer.global(db).resolve(name).join());
        }
        return out;
    }

    /** ExtendedDirectoryLayer.global's reverseLookup of each value; null where Java finds none. */
    @ConformanceStep("directoryLayerReverseLookup")
    public List<String> directoryLayerReverseLookup(String clusterFile, String valuesJson) {
        List<Long> values = GSON.fromJson(valuesJson, new TypeToken<List<Long>>(){}.getType());
        FDBDatabase db = createDatabase(clusterFile);
        List<String> out = new ArrayList<>();
        for (Long value : values) {
            try {
                out.add(ExtendedDirectoryLayer.global(db).reverseLookup((com.apple.foundationdb.record.provider.foundationdb.FDBStoreTimer) null, value).join());
            } catch (java.util.concurrent.CompletionException e) {
                if (!(e.getCause() instanceof NoSuchElementException)) {
                    throw e;
                }
                out.add(null);
            }
        }
        return out;
    }
}
