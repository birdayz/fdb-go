package com.apple.foundationdb.relational.recordlayer;

import com.apple.foundationdb.record.provider.foundationdb.FDBRecordStoreBase;
import com.apple.foundationdb.record.query.plan.plans.RecordQueryPlan;
import com.apple.foundationdb.relational.api.RelationalConnection;
import com.apple.foundationdb.relational.recordlayer.query.Plan;
import com.apple.foundationdb.relational.recordlayer.query.PlanContext;
import com.apple.foundationdb.relational.recordlayer.query.PlanGenerator;
import com.apple.foundationdb.relational.recordlayer.query.QueryPlan;
import com.apple.foundationdb.relational.util.Assert;

import java.util.Optional;

/**
 * Test-only: the physical plan the SQL layer runs for a statement, planned as
 * EmbeddedRelationalStatement.executeInternal plans it (AbstractEmbeddedStatement:85-90)
 * with the connection's options, in a transaction this rolls back if it started it.
 */
public final class RelationalPlanConformanceAccess {
    private RelationalPlanConformanceAccess() {
    }

    public static RecordQueryPlan plan(RelationalConnection connection, String sql) throws Exception {
        final EmbeddedRelationalConnection conn = connection.unwrap(EmbeddedRelationalConnection.class);
        final boolean started = conn.ensureTransactionActive();
        try (var schema = conn.getRecordLayerDatabase().loadSchema(conn.getSchema())) {
            final FDBRecordStoreBase<?> store = schema.loadStore().unwrap(FDBRecordStoreBase.class);
            final PlanContext planContext = PlanContext.builder()
                    .fromRecordStore(store, conn.getOptions())
                    .fromDatabase(conn.getRecordLayerDatabase())
                    .withMetricsCollector(Assert.notNullUnchecked(conn.getMetricCollector()))
                    .withSchemaTemplate(conn.getTransaction().getBoundSchemaTemplateMaybe().orElse(conn.getSchemaTemplate()))
                    .build();
            final Plan<?> plan = PlanGenerator.create(Optional.empty(), planContext, store, conn.getOptions()).getPlan(sql);
            return ((QueryPlan.PhysicalQueryPlan) plan).getRecordQueryPlan();
        } finally {
            if (started) {
                conn.rollbackInternal();
            }
        }
    }
}
