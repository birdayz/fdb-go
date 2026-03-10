package com.birdayz.conformance;

import com.apple.foundationdb.Database;
import com.apple.foundationdb.Range;
import com.apple.foundationdb.Tenant;
import com.apple.foundationdb.record.provider.foundationdb.FDBDatabase;
import com.apple.foundationdb.subspace.Subspace;

import java.nio.charset.StandardCharsets;
import java.util.ArrayList;
import java.util.HashMap;
import java.util.List;
import java.util.Map;

class RangeSetSteps extends ConformanceBase {
    @ConformanceStep("rangeSetInsert")
    public boolean rangeSetInsert(String clusterFile, byte[] rsSubspace, byte[] begin, byte[] end, String tenantName) {
        FDBDatabase db = createDatabase(clusterFile);
        Database nativeDb = db.database();
        com.apple.foundationdb.async.RangeSet rs = new com.apple.foundationdb.async.RangeSet(new Subspace(rsSubspace));
        if (tenantName != null && !tenantName.isEmpty()) {
            Tenant tenant = nativeDb.openTenant(tenantName.getBytes(StandardCharsets.UTF_8));
            return rs.insertRange(tenant, begin, end).join();
        } else {
            return rs.insertRange(nativeDb, begin, end).join();
        }
    }

    @ConformanceStep("rangeSetContains")
    public boolean rangeSetContains(String clusterFile, byte[] rsSubspace, byte[] key, String tenantName) {
        FDBDatabase db = createDatabase(clusterFile);
        Database nativeDb = db.database();
        com.apple.foundationdb.async.RangeSet rs = new com.apple.foundationdb.async.RangeSet(new Subspace(rsSubspace));
        if (tenantName != null && !tenantName.isEmpty()) {
            Tenant tenant = nativeDb.openTenant(tenantName.getBytes(StandardCharsets.UTF_8));
            return rs.contains(tenant, key).join();
        } else {
            return rs.contains(nativeDb, key).join();
        }
    }

    @ConformanceStep("rangeSetMissingRanges")
    public List<Map<String, Object>> rangeSetMissingRanges(String clusterFile, byte[] rsSubspace, String tenantName) {
        FDBDatabase db = createDatabase(clusterFile);
        Database nativeDb = db.database();
        com.apple.foundationdb.async.RangeSet rs = new com.apple.foundationdb.async.RangeSet(new Subspace(rsSubspace));
        List<Range> missing;
        if (tenantName != null && !tenantName.isEmpty()) {
            Tenant tenant = nativeDb.openTenant(tenantName.getBytes(StandardCharsets.UTF_8));
            missing = rs.missingRanges(tenant).join();
        } else {
            missing = rs.missingRanges(nativeDb).join();
        }

        List<Map<String, Object>> result = new ArrayList<>();
        for (Range range : missing) {
            Map<String, Object> map = new HashMap<>();
            List<Integer> beginInts = new ArrayList<>();
            for (byte b : range.begin) {
                beginInts.add(b & 0xFF);
            }
            List<Integer> endInts = new ArrayList<>();
            for (byte b : range.end) {
                endInts.add(b & 0xFF);
            }
            map.put("begin", beginInts);
            map.put("end", endInts);
            result.add(map);
        }
        return result;
    }
}
