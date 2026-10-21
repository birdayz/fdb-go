package com.birdayz.conformance;

import com.apple.foundationdb.record.RecordLayerDemo;
import com.apple.foundationdb.record.RecordMetaData;
import com.apple.foundationdb.record.RecordMetaDataBuilder;
import com.apple.foundationdb.record.RecordMetaDataProto;
import com.apple.foundationdb.record.metadata.Key;
import com.apple.foundationdb.record.provider.common.DynamicMessageRecordSerializer;
import com.apple.foundationdb.tuple.Tuple;
import com.google.protobuf.Any;
import com.google.protobuf.ByteString;
import com.google.protobuf.DynamicMessage;
import com.google.protobuf.GeneratedMessageV3;
import com.google.protobuf.Message;
import com.google.protobuf.util.JsonFormat;

import java.net.URL;
import java.nio.file.Files;
import java.nio.file.Path;
import java.security.MessageDigest;
import java.util.Enumeration;
import java.util.HexFormat;
import java.util.Properties;
import java.util.jar.JarEntry;
import java.util.jar.JarFile;

/** Proves the Java oracle loads the runtime declared by Record Layer 4.14.2.0. */
public final class ProtobufRuntimeContract {
    private static final String VERSION = "3.25.9";
    // rules_jvm_external changes only MANIFEST.MF to stamp Target-Label. Pin
    // the actual runtime artifacts, not the unmodified Maven download hashes.
    private static final String CORE_SHA256 = "c7121034708f0b9524531a25dce7350deec9591c3c768aa36105c4fd33332ac7";
    private static final String UTIL_SHA256 = "a7614edc48cb984f811a4d6b417d46ab6b1923e9d02484b1d6ed48759125ea1a";

    private static void require(boolean condition, String message) {
        if (!condition) {
            throw new AssertionError(message);
        }
    }

    private static void checkArtifact(Class<?> type, String artifact, String expectedHash) throws Exception {
        URL source = type.getProtectionDomain().getCodeSource().getLocation();
        Path path = Path.of(source.toURI());
        String hash = HexFormat.of().formatHex(MessageDigest.getInstance("SHA-256").digest(Files.readAllBytes(path)));
        System.out.println(type.getName() + " CodeSource=" + source + " sha256=" + hash);
        require(expectedHash.equals(hash), "wrong " + artifact + " artifact loaded for " + type.getName());
        try (JarFile jar = new JarFile(path.toFile())) {
            String manifestVersion = jar.getManifest().getMainAttributes().getValue("Bundle-Version");
            System.out.println(artifact + " Bundle-Version=" + manifestVersion);
            require(VERSION.equals(manifestVersion), "wrong " + artifact + " manifest version");
            require(("@maven//:com_google_protobuf_" + artifact.replace('-', '_')).equals(
                    jar.getManifest().getMainAttributes().getValue("Target-Label")), "unexpected Maven jar stamp");
            JarEntry entry = jar.getJarEntry("META-INF/maven/com.google.protobuf/" + artifact + "/pom.properties");
            // The published 3.25.9 artifacts omit Maven's pom.properties. Do not
            // fabricate it: the manifest and exact official jar hash identify them.
            if (entry == null) {
                System.out.println(artifact + " pom.properties=<absent in published artifact>");
            } else {
                Properties properties = new Properties();
                try (var stream = jar.getInputStream(entry)) {
                    properties.load(stream);
                }
                System.out.println(artifact + " pom.properties=" + properties);
                require(VERSION.equals(properties.getProperty("version")), "wrong pom.properties version");
            }
            require(entry == null, "published 3.25.9 artifact unexpectedly acquired pom.properties");
        }
    }

    private static void checkClasspath() throws Exception {
        String classpath = System.getProperty("java.class.path");
        System.out.println("java.runtime.version=" + System.getProperty("java.runtime.version"));
        System.out.println("java.class.path=" + classpath);
        require(!classpath.contains("/protobuf+/java/core/") && !classpath.contains("\\protobuf+\\java\\core\\"),
                "native protobuf module's Java jars are on the oracle classpath");
        int count = 0;
        Enumeration<URL> definitions = ByteString.class.getClassLoader().getResources("com/google/protobuf/ByteString.class");
        while (definitions.hasMoreElements()) {
            System.out.println("ByteString definition=" + definitions.nextElement());
            count++;
        }
        require(count == 1, "oracle has " + count + " competing protobuf runtimes");
    }

    private static RecordMetaData metadata() {
        RecordMetaDataBuilder builder = RecordMetaData.newBuilder().setRecords(RecordLayerDemo.getDescriptor());
        builder.getRecordType("Order").setPrimaryKey(Key.Expressions.field("order_id"));
        builder.getRecordType("Customer").setPrimaryKey(Key.Expressions.field("customer_id"));
        builder.getRecordType("TypedRecord").setPrimaryKey(Key.Expressions.field("id"));
        return builder.build();
    }

    private static void checkParseAndSerializer() throws Exception {
        RecordLayerDemo.Customer customer = RecordLayerDemo.Customer.newBuilder().setCustomerId(1L)
                .setName("\u0130 \u0391\u03a3 \ud835\udc00").setEmail("").build();
        RecordLayerDemo.Customer parsed = RecordLayerDemo.Customer.parseFrom(customer.toByteArray());
        require(customer.equals(parsed) && parsed.hasEmail(), "demo message parse or proto2 presence drift");
        RecordMetaDataProto.MetaData apple = RecordMetaDataProto.MetaData.newBuilder().setVersion(7).build();
        require(apple.equals(RecordMetaDataProto.MetaData.parseFrom(apple.toByteArray())), "Apple message parse failed");
        Any any = Any.newBuilder().setTypeUrl("type.example/fixture").setValue(ByteString.copyFromUtf8("payload")).build();
        require(any.equals(Any.parseFrom(any.toByteArray())), "pinned well-known message parse failed");
        require(JsonFormat.printer().print(customer).contains("customerId"), "protobuf-java-util smoke failed");

        RecordMetaData metadata = metadata();
        var serializer = DynamicMessageRecordSerializer.instance();
        byte[] encoded = serializer.serialize(metadata, metadata.getRecordType("Customer"), customer, null);
        Message decoded = serializer.deserialize(metadata, Tuple.from(1L), encoded, null);
        require(customer.equals(decoded), "record serializer round-trip failed");

        // Customer.name is proto2 without java_string_check_utf8. Pin both the
        // generated raw-byte getter and the default record serializer's String view.
        byte[] invalidName = HexFormat.of().parseHex("61eda08062");
        RecordLayerDemo.Customer invalid = RecordLayerDemo.Customer.newBuilder().setCustomerId(2L)
                .setNameBytes(ByteString.copyFrom(invalidName)).build();
        RecordLayerDemo.Customer invalidParsed = RecordLayerDemo.Customer.parseFrom(invalid.toByteArray());
        require(invalidParsed.getName().equals("a\ufffdb") && invalidParsed.getNameBytes().equals(ByteString.copyFrom(invalidName)),
                "proto2 malformed UTF-8 getter behavior changed");
        byte[] invalidEncoded = serializer.serialize(metadata, metadata.getRecordType("Customer"), invalidParsed, null);
        Message invalidDecoded = serializer.deserialize(metadata, Tuple.from(2L), invalidEncoded, null);
        require(invalidDecoded.getField(RecordLayerDemo.Customer.getDescriptor().findFieldByName("name")).equals("a\ufffdb"),
                "record serializer malformed UTF-8 decode changed");
        System.out.println("demo, Apple, well-known, util, record serializer, and malformed proto2 smoke checks passed");
    }

    public static void main(String[] args) throws Exception {
        checkClasspath();
        checkArtifact(ByteString.class, "protobuf-java", CORE_SHA256);
        checkArtifact(DynamicMessage.class, "protobuf-java", CORE_SHA256);
        checkArtifact(GeneratedMessageV3.class, "protobuf-java", CORE_SHA256);
        checkArtifact(JsonFormat.class, "protobuf-java-util", UTIL_SHA256);
        checkParseAndSerializer();
    }
}
