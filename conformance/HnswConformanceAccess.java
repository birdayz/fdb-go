package com.apple.foundationdb.record.provider.foundationdb.indexes;

import com.apple.foundationdb.async.hnsw.Config;
import com.apple.foundationdb.record.metadata.Index;

import java.util.HashMap;
import java.util.Map;

/**
 * Test-only accessor for the package-private HNSW option parse, used by the
 * conformance oracle. It calls the production HnswVectorIndexEngine.parseConfig;
 * it does not reimplement it.
 */
public final class HnswConformanceAccess {
    private HnswConformanceAccess() {
    }

    /** The HNSW configuration Java reads from a VECTOR index's options. */
    public static Map<String, Object> config(final Index index) {
        final Config config = HnswVectorIndexEngine.parseConfig(index);
        final Map<String, Object> out = new HashMap<>();
        out.put("numDimensions", config.numDimensions());
        out.put("m", config.m());
        out.put("mMax", config.mMax());
        out.put("mMax0", config.mMax0());
        out.put("efConstruction", config.efConstruction());
        out.put("efRepair", config.efRepair());
        out.put("statsThreshold", config.statsThreshold());
        out.put("sampleVectorStatsProbability", config.sampleVectorStatsProbability());
        out.put("maintainStatsProbability", config.maintainStatsProbability());
        return out;
    }
}
