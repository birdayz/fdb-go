package com.birdayz.conformance;

import com.apple.foundationdb.FDBException;
import com.apple.foundationdb.Tenant;
import com.apple.foundationdb.Transaction;
import com.apple.foundationdb.record.ExecuteProperties;
import com.apple.foundationdb.record.IndexFetchMethod;
import com.apple.foundationdb.record.IsolationLevel;
import com.apple.foundationdb.record.RecordCursor;
import com.apple.foundationdb.record.RecordCursorResult;
import com.apple.foundationdb.record.provider.foundationdb.FDBDatabase;
import com.apple.foundationdb.record.provider.foundationdb.FDBQueriedRecord;
import com.apple.foundationdb.record.provider.foundationdb.FDBRecordContext;
import com.apple.foundationdb.record.provider.foundationdb.FDBRecordContextConfig;
import com.apple.foundationdb.record.provider.foundationdb.FDBRecordStore;
import com.apple.foundationdb.record.provider.foundationdb.FDBStoreTimer;
import com.apple.foundationdb.record.provider.foundationdb.IndexScanComparisons;
import com.apple.foundationdb.record.provider.foundationdb.InstrumentedTransaction;
import com.apple.foundationdb.record.query.plan.plans.RecordQueryFetchFromPartialRecordPlan;
import com.apple.foundationdb.record.query.plan.plans.RecordQueryIndexPlan;
import com.google.protobuf.Message;

import java.nio.charset.StandardCharsets;
import java.util.ArrayList;
import java.util.HashMap;
import java.util.List;
import java.util.Map;

/**
 * RFC-258: RecordQueryIndexPlan.executePlan (RecordQueryIndexPlan.java:259-306) with each IndexFetchMethod over
 * IndexRemoteFetchSteps' stores: the plan-level dispatch and fallBackContinueFrom (:707-720), which resumes with the
 * same ExecuteProperties, row limit and skip included.
 */
class IndexFetchPlanSteps extends ConformanceBase {
    public static class Request {
        public String shape = "many";
        public String method = "USE_REMOTE_FETCH_WITH_FALLBACK";
        public boolean reverse;
        public int rowLimit;
        public int skip;
        public int localWriteID = -1;
    }

    @ConformanceStep("indexFetchPlanExecute")
    public Map<String, Object> execute(String clusterFile, String tenantName, byte[] subspace, Request request) {
        FDBDatabase database = createDatabase(clusterFile);
        FDBStoreTimer timer = new FDBStoreTimer();
        try (Tenant tenant = database.database().openTenant(tenantName.getBytes(StandardCharsets.UTF_8));
             Transaction transaction = tenant.createTransaction();
             FDBRecordContext context = createContextFromTransaction(database,
                     new InstrumentedTransaction(timer, null, database, null, transaction, false),
                     FDBRecordContextConfig.newBuilder().setTimer(timer).build())) {
            FDBRecordStore store = IndexRemoteFetchSteps.open(context, subspace, request.shape);
            if (request.localWriteID >= 0) {
                IndexRemoteFetchSteps.localWrite(store, request.shape, request.localWriteID);
            }
            RecordQueryIndexPlan plan = new RecordQueryIndexPlan("price", null, IndexScanComparisons.byValue(),
                    IndexFetchMethod.valueOf(request.method), RecordQueryFetchFromPartialRecordPlan.FetchIndexRecords.PRIMARY_KEY,
                    request.reverse, false);
            ExecuteProperties properties = ExecuteProperties.newBuilder().setReturnedRowLimit(request.rowLimit)
                    .setSkip(request.skip).setIsolationLevel(IsolationLevel.SERIALIZABLE).build();
            context.getTimer().reset();
            Map<String, Object> result = new HashMap<>();
            List<Long> ids = new ArrayList<>();
            result.put("ids", ids);
            result.put("exceptionClass", "");
            result.put("errorCode", 0);
            try (RecordCursor<FDBQueriedRecord<Message>> cursor = store.executeQuery(plan, null, properties)) {
                while (true) {
                    RecordCursorResult<FDBQueriedRecord<Message>> next = cursor.onNext().join();
                    if (!next.hasNext()) {
                        break;
                    }
                    ids.add(((Number) next.get().getPrimaryKey().get(next.get().getPrimaryKey().size() - 1)).longValue());
                }
            } catch (RuntimeException error) {
                Throwable root = error;
                while (root.getCause() != null) {
                    root = root.getCause();
                }
                result.put("exceptionClass", root.getClass().getSimpleName());
                for (Throwable cause = error; cause != null; cause = cause.getCause()) {
                    if (cause instanceof FDBException) {
                        result.put("errorCode", ((FDBException) cause).getCode());
                    }
                }
            }
            result.put("remoteFetches", context.getTimer().getCount(FDBStoreTimer.Counts.REMOTE_FETCH));
            result.put("remoteEntries", context.getTimer().getCount(FDBStoreTimer.Events.SCAN_REMOTE_FETCH_ENTRY));
            return result;
        }
    }
}
