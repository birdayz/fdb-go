package com.birdayz.conformance;

import com.apple.foundationdb.record.RecordMetaData;
import com.apple.foundationdb.record.RecordMetaDataBuilder;
import com.apple.foundationdb.record.RecordLayerDemo;
import com.apple.foundationdb.record.metadata.Index;
import com.apple.foundationdb.record.metadata.IndexTypes;
import com.apple.foundationdb.record.metadata.Key;
import com.apple.foundationdb.record.provider.foundationdb.FDBQueriedRecord;
import com.apple.foundationdb.record.provider.foundationdb.FDBRecordContext;
import com.apple.foundationdb.record.provider.foundationdb.FDBRecordStore;
import com.apple.foundationdb.record.query.ParameterRelationshipGraph;
import com.apple.foundationdb.record.query.RecordQuery;
import com.apple.foundationdb.record.query.expressions.Query;
import com.apple.foundationdb.record.query.plan.QueryPlanner;
import com.apple.foundationdb.record.query.plan.RecordQueryPlannerConfiguration;
import com.apple.foundationdb.record.query.plan.cascades.CascadesPlanner;
import com.apple.foundationdb.record.query.plan.cascades.explain.ExplainPlanVisitor;
import com.apple.foundationdb.record.query.plan.plans.RecordQueryPlan;
import com.apple.foundationdb.subspace.Subspace;
import com.google.gson.JsonArray;
import com.google.gson.JsonObject;

import java.util.ArrayList;
import java.util.List;

/**
 * RFC-257 WS-F: the target's RECORD layer (not its relational layer) planning an IN
 * predicate with the Cascades planner under a caller-chosen planner configuration, so the
 * record layer's default (PREFER_SCAN, in-union size 0) can be measured beside the
 * relational configuration (PREFER_INDEX, size 24). Orders are stored in a fresh
 * subspace: order_id 1..5 with prices 10, 10, 20, (absent), 10.
 */
class WsfRecordLayerSteps extends ConformanceBase {

    private static RecordMetaData metaData() {
        RecordMetaDataBuilder builder = RecordMetaData.newBuilder().setRecords(RecordLayerDemo.getDescriptor());
        builder.getRecordType("Order").setPrimaryKey(Key.Expressions.field("order_id"));
        builder.getRecordType("Customer").setPrimaryKey(Key.Expressions.field("customer_id"));
        builder.getRecordType("TypedRecord").setPrimaryKey(Key.Expressions.field("id"));
        builder.addIndex("Order", new Index("wsf_price", Key.Expressions.field("price"), IndexTypes.VALUE));
        return builder.build();
    }

    private static FDBRecordStore open(FDBRecordContext context, byte[] subspace) {
        return FDBRecordStore.newBuilder()
                .setMetaDataProvider(metaData())
                .setContext(context)
                .setSubspace(new Subspace(subspace))
                .setUserVersionChecker(ALWAYS_READABLE_CHECKER)
                .createOrOpen();
    }

    /**
     * Saves the five orders, then plans {@code price IN prices} (sorted by {@code sort}:
     * "price", "order_id" or "" for none) with a CascadesPlanner whose configuration is the
     * record layer's default when {@code relationalConfiguration} is false and the relational
     * layer's (PREFER_INDEX, in-union size 24) when true, and executes it. Returns the plan's
     * explain and either the order ids in cursor order or the execution error.
     */
    @ConformanceStep("wsfRecordLayerIn")
    public JsonObject wsfRecordLayerIn(String clusterFile, byte[] subspace, String tenantName,
                                       List<Object> prices, String sort, boolean relationalConfiguration) {
        runInContext(clusterFile, tenantName, context -> {
            FDBRecordStore store = open(context, subspace);
            int[][] rows = {{1, 10}, {2, 10}, {3, 20}, {4, -1}, {5, 10}};
            for (int[] r : rows) {
                RecordLayerDemo.Order.Builder order = RecordLayerDemo.Order.newBuilder().setOrderId(r[0]);
                if (r[1] >= 0) {
                    order.setPrice(r[1]);
                }
                store.saveRecord(order.build());
            }
            return null;
        });
        return runInContext(clusterFile, tenantName, context -> {
            FDBRecordStore store = open(context, subspace);
            CascadesPlanner planner = new CascadesPlanner(store.getRecordMetaData(), store.getRecordStoreState(),
                    store.getIndexMaintainerRegistry());
            RecordQueryPlannerConfiguration.Builder config = RecordQueryPlannerConfiguration.builder();
            if (relationalConfiguration) {
                config.setIndexScanPreference(QueryPlanner.IndexScanPreference.PREFER_INDEX)
                        .setAttemptFailedInJoinAsUnionMaxSize(24);
            }
            planner.setConfiguration(config.build());
            List<Object> values = new ArrayList<>();
            for (Object p : prices) {
                values.add(((Number) p).intValue());
            }
            RecordQuery.Builder query = RecordQuery.newBuilder().setRecordType("Order")
                    .setFilter(Query.field("price").in(values));
            if (!sort.isEmpty()) {
                query.setSort(Key.Expressions.field(sort));
            }
            JsonObject out = new JsonObject();
            RecordQueryPlan plan;
            try {
                plan = planner.plan(query.build(), ParameterRelationshipGraph.empty());
            } catch (RuntimeException e) {
                out.addProperty("planError", e.getClass().getSimpleName() + ": " + e.getMessage());
                return out;
            }
            out.addProperty("explain", ExplainPlanVisitor.toStringForDebugging(plan));
            out.addProperty("inUnionMaxSize", planner.getConfiguration().getAttemptFailedInJoinAsUnionMaxSize());
            try {
                List<FDBQueriedRecord<com.google.protobuf.Message>> records = store.executeQuery(plan).asList().join();
                JsonArray ids = new JsonArray();
                for (FDBQueriedRecord<com.google.protobuf.Message> r : records) {
                    ids.add(((Number) r.getRecord().getField(
                            r.getRecord().getDescriptorForType().findFieldByName("order_id"))).longValue());
                }
                out.add("ids", ids);
            } catch (RuntimeException e) {
                Throwable cause = e;
                while (cause.getCause() != null && !(cause instanceof com.apple.foundationdb.record.RecordCoreException)) {
                    cause = cause.getCause();
                }
                out.addProperty("executeError", cause.getClass().getSimpleName() + ": " + cause.getMessage());
            }
            return out;
        });
    }
}
