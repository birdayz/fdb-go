package com.birdayz.conformance;

import com.apple.foundationdb.record.provider.common.KeyStoreSerializationKeyManager;
import com.apple.foundationdb.record.provider.common.TransformedRecordSerializerJCE;
import com.apple.foundationdb.record.provider.foundationdb.FDBRecordStore;
import com.apple.foundationdb.record.provider.foundationdb.FDBStoredRecord;
import com.apple.foundationdb.record.RecordLayerDemo.Order;
import com.apple.foundationdb.subspace.Subspace;
import com.apple.foundationdb.tuple.Tuple;
import com.google.protobuf.Message;

import javax.crypto.spec.SecretKeySpec;
import java.io.FileOutputStream;
import java.security.KeyStore;
import java.util.HexFormat;
import java.util.List;
import java.util.zip.Deflater;

/**
 * Orders saved and loaded through a TransformedRecordSerializerJCE: compressed, encrypted with an
 * AES key given in hex (CipherPool's default cipher), both, or neither; and through a
 * KeyStoreSerializationKeyManager over a PKCS12 key store the JDK writes. The Go record layer's
 * reader and writer, and its key store reader, are pinned against these (RFC-257
 * serializer-design.md).
 */
class TransformedSerializerSteps extends ConformanceBase {
    private static TransformedRecordSerializerJCE<Message> serializer(boolean compress, boolean encrypt, String keyHex) {
        TransformedRecordSerializerJCE.Builder<Message> b = TransformedRecordSerializerJCE.newDefaultBuilder()
            .setCompressWhenSerializing(compress)
            .setCompressionLevel(Deflater.BEST_COMPRESSION)
            .setEncryptWhenSerializing(encrypt)
            .setWriteValidationRatio(1.0);
        if (keyHex != null && !keyHex.isEmpty()) {
            b.setEncryptionKey(new SecretKeySpec(HexFormat.of().parseHex(keyHex), "AES"));
        }
        return b.build();
    }

    private static TransformedRecordSerializerJCE<Message> keyStoreSerializer(boolean compress, boolean encrypt,
                                                                        String keyStore, String password,
                                                                        List<String> aliases, String defaultAlias) {
        KeyStoreSerializationKeyManager.Builder km = KeyStoreSerializationKeyManager.newBuilder();
        km.setKeyStoreFileName(keyStore);
        km.setKeyStorePassword(password);
        km.setKeyEntryAliases(aliases);
        km.setDefaultKeyEntryAlias(defaultAlias);
        return TransformedRecordSerializerJCE.newDefaultBuilder()
            .setCompressWhenSerializing(compress)
            .setEncryptWhenSerializing(encrypt)
            .setWriteValidationRatio(1.0)
            .setKeyManager(km.build())
            .build();
    }

    private static final Object KEY_STORE_PROPERTIES = new Object();

    /**
     * Writes a key store of the given type ("PKCS12" or "JCEKS") holding one AES SecretKeyEntry per
     * alias, the store under storePassword and each key under entryPassword. With legacy, a PKCS12
     * store is written as JDKs before 12 wrote it: keys under PBEWithSHA1AndDESede and the MAC
     * HmacPBESHA1 (the keystore.pkcs12.* properties PKCS12KeyStore reads when it protects a key and
     * when it stores, set for this write only).
     */
    @ConformanceStep("writeKeyStoreJava")
    public void writeKeyStoreJava(String path, String storeType, String storePassword, String entryPassword,
                                  boolean legacy, List<String> aliases, List<String> keysHex, String keyAlgorithm) throws Exception {
        String algorithm = keyAlgorithm == null || keyAlgorithm.isEmpty() ? "AES" : keyAlgorithm;
        synchronized (KEY_STORE_PROPERTIES) {
            String oldKeyAlg = System.getProperty("keystore.pkcs12.keyProtectionAlgorithm");
            String oldMacAlg = System.getProperty("keystore.pkcs12.macAlgorithm");
            try {
                if (legacy) {
                    System.setProperty("keystore.pkcs12.keyProtectionAlgorithm", "PBEWithSHA1AndDESede");
                    System.setProperty("keystore.pkcs12.macAlgorithm", "HmacPBESHA1");
                }
                KeyStore ks = KeyStore.getInstance(storeType);
                ks.load(null, null);
                KeyStore.ProtectionParameter protection = new KeyStore.PasswordProtection(entryPassword.toCharArray());
                for (int i = 0; i < aliases.size(); i++) {
                    ks.setEntry(aliases.get(i),
                        new KeyStore.SecretKeyEntry(new SecretKeySpec(HexFormat.of().parseHex(keysHex.get(i)), algorithm)),
                        protection);
                }
                try (FileOutputStream out = new FileOutputStream(path)) {
                    ks.store(out, storePassword.toCharArray());
                }
            } finally {
                restoreProperty("keystore.pkcs12.keyProtectionAlgorithm", oldKeyAlg);
                restoreProperty("keystore.pkcs12.macAlgorithm", oldMacAlg);
            }
        }
    }

    private static void restoreProperty(String name, String value) {
        if (value == null) {
            System.clearProperty(name);
        } else {
            System.setProperty(name, value);
        }
    }

    @ConformanceStep("saveOrderKeyStoreJava")
    public void saveOrderKeyStoreJava(String clusterFile, byte[] subspace, Order order, String tenantName, boolean compress,
                                      String keyStore, String password, List<String> aliases, String defaultAlias) {
        runInContext(clusterFile, tenantName, context -> {
            FDBRecordStore store = FDBRecordStore.newBuilder()
                .setMetaDataProvider(createMetaData())
                .setContext(context)
                .setSubspace(new Subspace(subspace))
                .setSerializer(keyStoreSerializer(compress, true, keyStore, password, aliases, defaultAlias))
                .createOrOpen();
            store.saveRecord(order);
            return null;
        });
    }

    @ConformanceStep("loadOrderKeyStoreJava")
    public Order loadOrderKeyStoreJava(String clusterFile, byte[] subspace, long orderID, String tenantName,
                                       String keyStore, String password, List<String> aliases, String defaultAlias) {
        return runInContext(clusterFile, tenantName, context -> {
            FDBRecordStore store = FDBRecordStore.newBuilder()
                .setMetaDataProvider(createMetaData())
                .setContext(context)
                .setSubspace(new Subspace(subspace))
                .setSerializer(keyStoreSerializer(false, false, keyStore, password, aliases, defaultAlias))
                .open();
            FDBStoredRecord<Message> record = store.loadRecord(Tuple.from(orderID));
            if (record == null) {
                throw new RuntimeException("Record not found: " + orderID);
            }
            return Order.newBuilder().mergeFrom(record.getRecord()).build();
        });
    }

    @ConformanceStep("saveOrderTransformedJava")
    public void saveOrderTransformedJava(String clusterFile, byte[] subspace, Order order, String tenantName,
                                         boolean compress, boolean encrypt, String keyHex) {
        runInContext(clusterFile, tenantName, context -> {
            FDBRecordStore store = FDBRecordStore.newBuilder()
                .setMetaDataProvider(createMetaData())
                .setContext(context)
                .setSubspace(new Subspace(subspace))
                .setSerializer(serializer(compress, encrypt, keyHex))
                .createOrOpen();
            store.saveRecord(order);
            return null;
        });
    }

    @ConformanceStep("loadOrderTransformedJava")
    public Order loadOrderTransformedJava(String clusterFile, byte[] subspace, long orderID, String tenantName,
                                          String keyHex) {
        return runInContext(clusterFile, tenantName, context -> {
            FDBRecordStore store = FDBRecordStore.newBuilder()
                .setMetaDataProvider(createMetaData())
                .setContext(context)
                .setSubspace(new Subspace(subspace))
                .setSerializer(serializer(false, false, keyHex))
                .open();
            FDBStoredRecord<Message> record = store.loadRecord(Tuple.from(orderID));
            if (record == null) {
                throw new RuntimeException("Record not found: " + orderID);
            }
            return Order.newBuilder().mergeFrom(record.getRecord()).build();
        });
    }
}
