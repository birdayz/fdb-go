package com.birdayz.conformance;

import com.apple.foundationdb.Database;
import com.apple.foundationdb.FDBException;
import com.apple.foundationdb.KeySelector;
import com.apple.foundationdb.KeyValue;
import com.apple.foundationdb.MutationType;
import com.apple.foundationdb.ReadTransaction;
import com.apple.foundationdb.StreamingMode;
import com.apple.foundationdb.Tenant;
import com.apple.foundationdb.Transaction;
import com.google.gson.JsonArray;
import com.google.gson.JsonElement;
import com.google.gson.JsonObject;
import com.google.gson.JsonParser;

import java.lang.reflect.Method;
import java.nio.charset.StandardCharsets;
import java.security.MessageDigest;
import java.security.NoSuchAlgorithmException;
import java.util.ArrayList;
import java.util.HashMap;
import java.util.List;
import java.util.Map;
import java.util.concurrent.CompletableFuture;
import java.util.concurrent.CompletionException;

/** Runs ryw_script_conformance_test.go's transaction scripts on libfdb_c; every format has its Go twin there. */
class RywScriptSteps extends ConformanceBase {
    private static final byte[] CONFLICTING_BEGIN =
        "\u00ff\u00ff/transaction/conflicting_keys/".getBytes(StandardCharsets.ISO_8859_1);
    private static final byte[] CONFLICTING_END =
        "\u00ff\u00ff/transaction/conflicting_keys/\u00ff\u00ff".getBytes(StandardCharsets.ISO_8859_1);

    @ConformanceStep("rywScript")
    public List<String> rywScript(String clusterFile, String tenantName, String script) {
        Database db = createDatabase(clusterFile).database();
        Map<String, Transaction> txs = new HashMap<>();
        Map<String, CompletableFuture<String>> pending = new HashMap<>();
        List<String> out = new ArrayList<>();
        try (Tenant tenant = db.openTenant(tenantName.getBytes(StandardCharsets.UTF_8))) {
            JsonArray steps = JsonParser.parseString(script).getAsJsonArray();
            for (int i = 0; i < steps.size(); i++) {
                JsonObject s = steps.get(i).getAsJsonObject();
                String op = s.get("op").getAsString();
                String name = s.get("tx").getAsString();
                Transaction tr = txs.computeIfAbsent(name, n -> tenant.createTransaction());
                String result = step(tenant, txs, name, tr, s, op, pending);
                if (result != null) {
                    out.add(i + " " + name + " " + op + " " + result);
                }
            }
        } finally {
            for (Transaction tr : txs.values()) {
                tr.close();
            }
        }
        return out;
    }

    private static String step(Tenant tenant, Map<String, Transaction> txs, String name, Transaction tr, JsonObject s,
                               String op, Map<String, CompletableFuture<String>> pending) {
        try {
            switch (op) {
                case "option":
                    switch (s.get("opt").getAsString()) {
                        case "reportConflictingKeys": tr.options().setReportConflictingKeys(); break;
                        case "nextWriteNoWriteConflictRange": tr.options().setNextWriteNoWriteConflictRange(); break;
                        case "readYourWritesDisable": tr.options().setReadYourWritesDisable(); break;
                        default: throw new IllegalArgumentException("unknown option " + s.get("opt"));
                    }
                    return null;
                case "grv":
                    tr.getReadVersion().join();
                    return null;
                case "setReadVersion": {
                    // A negative version is that far past a fresh read version: reads at it wait for storage.
                    long rv = s.get("rv").getAsLong();
                    if (rv < 0) {
                        try (Transaction fresh = tenant.createTransaction()) {
                            rv = fresh.getReadVersion().join() - rv;
                        }
                    }
                    tr.setReadVersion(rv);
                    return null;
                }
                case "reset":
                    reset(tr);
                    return null;
                case "cancel":
                    tr.cancel();
                    return null;
                case "sleep":
                    try {
                        Thread.sleep(s.get("code").getAsInt());
                    } catch (InterruptedException e) {
                        throw new RuntimeException(e);
                    }
                    return null;
                case "onError":
                    // fdb-java hands the reset native transaction to a new object.
                    txs.put(name, tr.onError(new FDBException("injected", s.get("code").getAsInt())).join());
                    return "ok";
                case "set": tr.set(bytes(s, "k"), bytes(s, "v")); return null;
                case "clear": tr.clear(bytes(s, "k")); return null;
                case "clearRange": tr.clear(bytes(s, "k"), bytes(s, "e")); return null;
                case "atomic": tr.mutate(mutation(s.get("code").getAsInt()), bytes(s, "k"), bytes(s, "v")); return null;
                case "addReadConflictRange": tr.addReadConflictRange(bytes(s, "k"), bytes(s, "e")); return null;
                case "addReadConflictKey": tr.addReadConflictKey(bytes(s, "k")); return null;
                case "addWriteConflictRange": tr.addWriteConflictRange(bytes(s, "k"), bytes(s, "e")); return null;
                case "addWriteConflictKey": tr.addWriteConflictKey(bytes(s, "k")); return null;
                case "get":
                case "getKey":
                case "getRange": {
                    CompletableFuture<String> f = read(tr, s, op);
                    String id = s.has("id") ? s.get("id").getAsString() : "";
                    if (!id.isEmpty()) {
                        pending.put(id, f);
                        return null;
                    }
                    return awaitRead(f);
                }
                case "await":
                    return awaitRead(pending.remove(s.get("id").getAsString()));
                case "commit":
                    tr.commit().join();
                    return "ok";
                case "conflicting":
                    return kvs(tr.getRange(CONFLICTING_BEGIN, CONFLICTING_END).asList().join(), CONFLICTING_BEGIN.length);
                default:
                    throw new IllegalArgumentException("unknown op " + op);
            }
        } catch (RuntimeException e) {
            return "err=" + code(e);
        }
    }

    /** fdb_transaction_reset: fdb-java 7.1 does not expose it, so call its native binding. */
    private static void reset(Transaction tr) {
        try {
            Method getPtr = Class.forName("com.apple.foundationdb.NativeObjectWrapper").getDeclaredMethod("getPtr");
            getPtr.setAccessible(true);
            Method reset = Class.forName("com.apple.foundationdb.FDBTransaction")
                .getDeclaredMethod("Transaction_reset", long.class);
            reset.setAccessible(true);
            reset.invoke(tr, (long) getPtr.invoke(tr));
        } catch (ReflectiveOperationException e) {
            throw new RuntimeException(e);
        }
    }

    private static CompletableFuture<String> read(Transaction tr, JsonObject s, String op) {
        ReadTransaction rt = s.has("snap") && s.get("snap").getAsBoolean() ? tr.snapshot() : tr;
        switch (op) {
            case "get":
                return rt.get(bytes(s, "k")).thenApply(RywScriptSteps::value);
            case "getKey":
                return rt.getKey(selector(s, "b")).thenApply(RywScriptSteps::hex);
            default:
                int limit = s.has("limit") ? s.get("limit").getAsInt() : 0;
                boolean reverse = s.has("reverse") && s.get("reverse").getAsBoolean();
                return rt.getRange(selector(s, "b"), selector(s, "es"), limit, reverse, StreamingMode.WANT_ALL)
                    .asList().thenApply(rows -> kvs(rows, 0));
        }
    }

    private static String awaitRead(CompletableFuture<String> f) {
        try {
            return f.join();
        } catch (RuntimeException e) {
            return "err=" + code(e);
        }
    }

    private static int code(Throwable e) {
        Throwable cause = e;
        while (cause != null && !(cause instanceof FDBException)) {
            cause = cause.getCause();
        }
        if (cause == null) {
            throw e instanceof RuntimeException ? (RuntimeException) e : new CompletionException(e);
        }
        return ((FDBException) cause).getCode();
    }

    private static MutationType mutation(int code) {
        for (MutationType m : MutationType.values()) {
            if (m.code() == code) {
                return m;
            }
        }
        throw new IllegalArgumentException("unknown mutation type " + code);
    }

    private static KeySelector selector(JsonObject s, String field) {
        JsonObject sel = s.getAsJsonObject(field);
        return new KeySelector(unhex(sel.get("k").getAsString()), sel.get("orEqual").getAsBoolean(), sel.get("offset").getAsInt());
    }

    private static byte[] bytes(JsonObject s, String field) {
        JsonElement e = s.get(field);
        return e == null ? new byte[0] : unhex(e.getAsString());
    }

    private static byte[] unhex(String h) {
        byte[] b = new byte[h.length() / 2];
        for (int i = 0; i < b.length; i++) {
            b[i] = (byte) Integer.parseInt(h.substring(2 * i, 2 * i + 2), 16);
        }
        return b;
    }

    private static String hex(byte[] b) {
        StringBuilder sb = new StringBuilder();
        for (byte x : b) {
            sb.append(String.format("%02x", x & 0xff));
        }
        return sb.toString();
    }

    /** A value up to 64 bytes in hex, a longer one as its length and SHA-256. */
    private static String value(byte[] v) {
        if (v == null) {
            return "null";
        }
        if (v.length <= 64) {
            return hex(v);
        }
        try {
            return "len=" + v.length + ":sha256=" + hex(MessageDigest.getInstance("SHA-256").digest(v));
        } catch (NoSuchAlgorithmException e) {
            throw new RuntimeException(e);
        }
    }

    private static String kvs(List<KeyValue> rows, int strip) {
        StringBuilder sb = new StringBuilder("[");
        for (KeyValue kv : rows) {
            if (sb.length() > 1) {
                sb.append(' ');
            }
            byte[] k = kv.getKey();
            byte[] stripped = new byte[k.length - strip];
            System.arraycopy(k, strip, stripped, 0, stripped.length);
            sb.append(hex(stripped)).append('=').append(value(kv.getValue()));
        }
        return sb.append(']').toString();
    }
}
