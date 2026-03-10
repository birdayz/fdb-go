package com.birdayz.conformance;

import com.apple.foundationdb.record.provider.foundationdb.FDBRecordStore;
import com.apple.foundationdb.record.provider.foundationdb.FDBStoredRecord;
import com.apple.foundationdb.record.RecordLayerDemo.Customer;
import com.apple.foundationdb.subspace.Subspace;
import com.apple.foundationdb.tuple.Tuple;
import com.google.protobuf.Message;

class CustomerSteps extends ConformanceBase {
    @ConformanceStep("saveCustomer")
    public void saveCustomer(String clusterFile, byte[] subspace, Customer customer, String tenantName) {
        runInContext(clusterFile, tenantName, context -> {
            FDBRecordStore store = FDBRecordStore.newBuilder()
                .setMetaDataProvider(createMetaData())
                .setContext(context)
                .setSubspace(new Subspace(subspace))
                .createOrOpen();
            store.saveRecord(customer);
            return null;
        });
    }

    @ConformanceStep("loadCustomer")
    public Customer loadCustomer(String clusterFile, byte[] subspace, long customerID, String tenantName) {
        return runInContext(clusterFile, tenantName, context -> {
            FDBRecordStore store = FDBRecordStore.newBuilder()
                .setMetaDataProvider(createMetaData())
                .setContext(context)
                .setSubspace(new Subspace(subspace))
                .open();
            FDBStoredRecord<Message> record = store.loadRecord(Tuple.from(customerID));
            if (record == null) {
                throw new RuntimeException("Customer not found: " + customerID);
            }
            return Customer.newBuilder()
                .mergeFrom(record.getRecord())
                .build();
        });
    }

    @ConformanceStep("deleteCustomer")
    public boolean deleteCustomer(String clusterFile, byte[] subspace, long customerID, String tenantName) {
        return runInContext(clusterFile, tenantName, context -> {
            FDBRecordStore store = FDBRecordStore.newBuilder()
                .setMetaDataProvider(createMetaData())
                .setContext(context)
                .setSubspace(new Subspace(subspace))
                .open();
            return store.deleteRecord(Tuple.from(customerID));
        });
    }

    @ConformanceStep("customerExists")
    public boolean customerExists(String clusterFile, byte[] subspace, long customerID, String tenantName) {
        return runInContext(clusterFile, tenantName, context -> {
            FDBRecordStore store = FDBRecordStore.newBuilder()
                .setMetaDataProvider(createMetaData())
                .setContext(context)
                .setSubspace(new Subspace(subspace))
                .open();
            return store.loadRecord(Tuple.from(customerID)) != null;
        });
    }
}
