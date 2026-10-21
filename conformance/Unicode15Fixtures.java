package com.birdayz.conformance;

import com.apple.foundationdb.record.RecordLayerDemo;
import com.apple.foundationdb.record.RecordMetaData;
import com.apple.foundationdb.record.RecordMetaDataBuilder;
import com.apple.foundationdb.record.metadata.Key;
import com.apple.foundationdb.record.provider.common.DynamicMessageRecordSerializer;
import com.apple.foundationdb.record.provider.common.text.DefaultTextTokenizer;
import com.apple.foundationdb.record.provider.common.text.TextTokenizer;
import com.apple.foundationdb.relational.generated.RelationalLexer;
import com.apple.foundationdb.relational.recordlayer.query.CaseInsensitiveCharStream;
import com.apple.foundationdb.relational.recordlayer.query.QueryParser;
import com.apple.foundationdb.relational.recordlayer.query.SemanticAnalyzer;
import com.apple.foundationdb.tuple.Tuple;
import com.google.gson.GsonBuilder;
import com.google.protobuf.ByteString;
import com.google.protobuf.CodedOutputStream;
import com.google.protobuf.DescriptorProtos;
import com.google.protobuf.Descriptors;
import com.google.protobuf.DynamicMessage;
import com.google.protobuf.InvalidProtocolBufferException;
import com.google.protobuf.Message;
import org.antlr.v4.runtime.Token;

import javax.crypto.spec.SecretKeySpec;
import java.io.ByteArrayOutputStream;
import java.nio.charset.StandardCharsets;
import java.nio.file.Files;
import java.nio.file.Path;
import java.security.KeyStore;
import java.security.MessageDigest;
import java.util.ArrayList;
import java.util.HexFormat;
import java.util.LinkedHashMap;
import java.util.List;
import java.util.Locale;
import java.util.Map;
import java.util.TreeMap;
import java.util.jar.JarFile;

/** JDK-21 / Record Layer 4.14.2.0 oracle for RFC-265; no FoundationDB is needed. */
public final class Unicode15Fixtures {
    private static Map<String, Object> object(Object... fields) {
        Map<String, Object> result = new LinkedHashMap<>();
        for (int i = 0; i < fields.length; i += 2) {
            result.put((String) fields[i], fields[i + 1]);
        }
        return result;
    }

    private static String cp(int value) {
        return new String(Character.toChars(value));
    }

    private static Map<String, Object> artifact(Class<?> type, String coordinates) throws Exception {
        Path jar = Path.of(type.getProtectionDomain().getCodeSource().getLocation().toURI());
        return object("coordinates", coordinates, "sha256",
                HexFormat.of().formatHex(MessageDigest.getInstance("SHA-256").digest(Files.readAllBytes(jar))));
    }

    private static Map<String, Object> protobufRuntime() throws Exception {
        // Read the loaded jar rather than compiler-inlined version constants.
        Path path = Path.of(DynamicMessage.class.getProtectionDomain().getCodeSource().getLocation().toURI());
        String runtimeVersion;
        try (JarFile jar = new JarFile(path.toFile())) {
            runtimeVersion = jar.getManifest().getMainAttributes().getValue("Bundle-Version");
        }
        if (runtimeVersion == null || runtimeVersion.isBlank()) {
            throw new IllegalStateException("protobuf runtime jar has no Bundle-Version");
        }
        String coordinates = "com.google.protobuf:protobuf-java:" + runtimeVersion;
        return object("module", coordinates, "runtimeVersion", runtimeVersion,
                "lite", artifact(ByteString.class, coordinates),
                "core", artifact(DynamicMessage.class, coordinates));
    }

    private static Map<String, List<Integer>> positions(List<String> tokens) {
        Map<String, List<Integer>> result = new TreeMap<>();
        for (int i = 0; i < tokens.size(); i++) {
            result.computeIfAbsent(tokens.get(i), key -> new ArrayList<>()).add(i);
        }
        return result;
    }

    private static List<Object> tokenizers() throws Exception {
        String[][] corpus = {
            {"ascii", "Alpha beta ALPHA 123"},
            {"u1c89", "\u1c89"}, {"ua7cb", "\ua7cb"},
            {"ua7ce", "\ua7ce"}, {"ua7f1", "\ua7f1"},
            {"u1ccd6", cp(0x1ccd6)}, {"u1acf", "\u1acf"},
            {"u0897", "\u0897"}, {"ufb05", "\ufb05"},
            {"u18cff", cp(0x18cff)}, {"u323b0", cp(0x323b0)},
            {"u2ebf0", cp(0x2ebf0)}, {"u0264", "\u0264"},
            {"u0295_sigma", "A\u03a3\u0295"},
            {"delta_neighbors", "a\u1c89b a\ua7cbb a\ua7ceb a\ua7f1b"},
            {"new_marks", "a\u1acfb a\u0897b"},
            {"outlined_neighbors", "a" + cp(0x1ccd6) + "b"},
            {"supplementary_ideographs", cp(0x18cff) + " " + cp(0x323b0) + " " + cp(0x2ebf0)},
            {"old_fold_pairs", "\ufb05 \ufb06 \u0390 \u1fd3 \u03b0 \u1fe3"},
            {"supplementary_nd", cp(0x1e4f0) + " " + cp(0x10d40) + " a" + cp(0x1e4f0)},
            {"supplementary_nfkd", "\ud835\udc00\ud835\udc01\ud835\udc02 \ud835\udfd9\ud835\udfda"},
            {"supplementary_letters", cp(0x10414) + " a" + cp(0x10414) + " " + cp(0x1e900)},
            {"hangul", "\uac00 \u1100\u1161 \ud7a3 \ud55c\uad6d\uc5b4"},
            {"dotted_i", "\u0130stanbul \u0131i I i\u0307"},
            {"ligatures", "\ufb01nance \ufb02 \ufb00 \u01c5 \u01c6 \u00df \u1e9e"},
            {"combining", "caf\u00e9 e\u0301 \u01f0 \u0390 \u0345\u03a3"},
            {"sigma", "\u03911\u03a3 \u0391\u03a31 1\u03a3 \u03a31 \u0391\u03a3\u0391 \u0391\u03a3\u0301 \u0391\u0301\u03a3"},
            {"sigma_ignorable", "\u0391\u03a3\u00ad\u03a3 \u0391\u00ad\u03a3 \u0391\u03a3\u200d\u03a3"},
            {"stream_safe_31", "a" + "\u0301".repeat(31) + "b"},
            {"stream_safe_61", "a" + "\u0301".repeat(61) + "b"},
            {"canonical_reorder", "a\u0315\u0300\u05ae\u0301b"},
            {"nul_replacement", "a\u0000b a\ufffdb"},
            {"empty", ""}, {"marks_only", "\u0301\u0345\u20dd"},
            {"x_colon_y", "x:y"},
            {"nfkd_sigma_boundary", "\u013f\u03a3"},
            {"nfkd_sigma_space", "\u0391\u03a3\u037a"},
            {"nfkd_sigma_arabic_space", "\u0391\u03a3\ufe70\u0392"},
            {"jdk_cased_subset", cp(0x10780) + "\u03a3"},
        };
        List<Object> result = new ArrayList<>();
        for (String[] item : corpus) {
            List<String> tokens = new ArrayList<>();
            // This is exactly the existing tokenizeDefault conformance step's path.
            for (CharSequence token : DefaultTextTokenizer.instance().tokenizeToList(
                    item[1], 0, TextTokenizer.TokenizerMode.INDEX)) {
                tokens.add(token.toString());
            }
            result.add(object("id", item[0], "input", item[1], "tokens", tokens, "positions", positions(tokens)));
        }
        return result;
    }

    private static byte[] bytesField(int number, byte[] value) throws Exception {
        ByteArrayOutputStream bytes = new ByteArrayOutputStream();
        CodedOutputStream output = CodedOutputStream.newInstance(bytes);
        output.writeByteArray(number, value);
        output.flush();
        return bytes.toByteArray();
    }

    private static Map<String, Object> byteDecoding() throws Exception {
        RecordMetaDataBuilder builder = RecordMetaData.newBuilder().setRecords(RecordLayerDemo.getDescriptor());
        builder.getRecordType("Order").setPrimaryKey(Key.Expressions.field("order_id"));
        builder.getRecordType("Customer").setPrimaryKey(Key.Expressions.field("customer_id"));
        builder.getRecordType("TypedRecord").setPrimaryKey(Key.Expressions.field("id"));
        RecordMetaData metadata = builder.build();
        Descriptors.FieldDescriptor nameField = RecordLayerDemo.Customer.getDescriptor().findFieldByName("name");
        Descriptors.Descriptor proto3 = Descriptors.FileDescriptor.buildFrom(
                DescriptorProtos.FileDescriptorProto.newBuilder().setName("unicode15_utf8.proto").setSyntax("proto3")
                        .addMessageType(DescriptorProtos.DescriptorProto.newBuilder().setName("Utf8Probe")
                                .addField(DescriptorProtos.FieldDescriptorProto.newBuilder().setName("name").setNumber(2)
                                        .setLabel(DescriptorProtos.FieldDescriptorProto.Label.LABEL_OPTIONAL)
                                        .setType(DescriptorProtos.FieldDescriptorProto.Type.TYPE_STRING)))
                        .build(), new Descriptors.FileDescriptor[0]).findMessageTypeByName("Utf8Probe");
        String[][] corpus = {
            {"ascii", "416c7068612062657461"}, {"dotted_i", "c4b0"},
            {"replacement", "61efbfbd62"}, {"nul", "610062"},
            {"isolated_continuation", "618062"}, {"overlong", "61c0af62"},
            {"truncated", "61e282"}, {"surrogate", "61eda08062"},
            {"above_max", "61f490808062"}, {"invalid_ff", "61ff62"},
        };
        List<Object> cases = new ArrayList<>();
        for (String[] item : corpus) {
            byte[] input = HexFormat.of().parseHex(item[1]);
            byte[] customerWire = bytesField(2, input);
            byte[] unionWire = bytesField(2, customerWire);
            // Exercise the actual default record-layer deserializer and the
            // reflected string field used by the TEXT key expression.
            Message record = DynamicMessageRecordSerializer.instance().deserialize(metadata, Tuple.from(1L), unionWire, null);
            String decoded = (String) record.getField(nameField);
            RecordLayerDemo.Customer generated = RecordLayerDemo.Customer.parseFrom(customerWire);
            List<String> tokens = new ArrayList<>();
            for (CharSequence token : DefaultTextTokenizer.instance().tokenizeToList(decoded, 0, TextTokenizer.TokenizerMode.INDEX)) {
                tokens.add(token.toString());
            }
            Map<String, Object> proto3Result;
            try {
                Message parsed = DynamicMessage.parseFrom(proto3, customerWire);
                String value = (String) parsed.getField(proto3.findFieldByName("name"));
                proto3Result = object("accepted", true, "decoded", value);
            } catch (InvalidProtocolBufferException ex) {
                proto3Result = object("accepted", false, "exceptionClass", ex.getClass().getName(), "message", ex.getMessage());
            }
            cases.add(object("id", item[0], "utf8Hex", item[1], "validUtf8", ByteString.copyFrom(input).isValidUtf8(),
                    "proto2", object("accepted", true, "decoded", decoded,
                            "decodedUtf8Hex", HexFormat.of().formatHex(decoded.getBytes(StandardCharsets.UTF_8)),
                            "generatedDecoded", generated.getName(),
                            "generatedNameBytesHex", HexFormat.of().formatHex(generated.getNameBytes().toByteArray()),
                            "tokens", tokens, "positions", positions(tokens)), "proto3", proto3Result));
        }
        return object("proto2Path", "DynamicMessageRecordSerializer.deserialize -> DynamicMessage.parseFrom -> Customer.name getField",
                "proto2Schema", "conformance/record_layer_demo.proto: proto2 Customer.name, java_string_check_utf8=false (default)",
                "generatedPath", "RecordLayerDemo.Customer.parseFrom -> getName (ByteString.toStringUtf8)",
                "proto3Path", "DynamicMessage.parseFrom: proto3 Utf8Probe.name string field (strict UTF-8)",
                "cases", cases);
    }

    private static List<Object> labels() {
        String[][] pairs = {
            {"ascii", "LABEL", "label"}, {"dotted_i", "\u0130", "i"},
            {"dotless_i", "\u0131", "i"}, {"long_s", "\u017f", "s"},
            {"kelvin", "\u212a", "k"}, {"sigma", "\u03a3", "\u03c2"},
            {"sharp_s", "\u00df", "SS"}, {"capital_sharp_s", "\u1e9e", "\u00df"},
            {"u1c89", "\u1c89", "\u1c8a"}, {"ua7cb", "\ua7cb", "\u0264"},
            {"ua7ce", "\ua7ce", "\ua7cf"}, {"ua7f1", "\ua7f1", "S"},
            {"u0295", "\u0295", "\u0295"},
            {"fb05_fb06", "\ufb05", "\ufb06"},
            {"0390_1fd3", "\u0390", "\u1fd3"}, {"03b0_1fe3", "\u03b0", "\u1fe3"},
            {"supplementary_deseret", cp(0x10400), cp(0x10428)},
        };
        List<Object> result = new ArrayList<>();
        for (String[] pair : pairs) {
            // RowStruct.findColumn / RelationalStruct.getOneBasedPosition use this predicate.
            result.add(object("id", pair[0], "left", pair[1], "right", pair[2],
                    "equalsIgnoreCase", pair[1].equalsIgnoreCase(pair[2])));
        }
        return result;
    }

    private static List<Object> identifiers() throws Exception {
        String[][] cases = {
            {"dotless_i", "SELECT \u0131d FROM t", "\u0131d"},
            {"long_s_keyword", "\u017felect id FROM t", "\u017felect"},
            {"quoted", "SELECT \"\u0131d\u017f\" FROM t", "\"\u0131d\u017f\""},
        };
        List<Object> result = new ArrayList<>();
        for (String[] item : cases) {
            QueryParser.parse(item[1]);
            RelationalLexer lexer = new RelationalLexer(new CaseInsensitiveCharStream(item[1]));
            List<Object> tokens = new ArrayList<>();
            for (Token token : lexer.getAllTokens()) {
                if (token.getChannel() == Token.DEFAULT_CHANNEL) {
                    tokens.add(object("kind", lexer.getVocabulary().getSymbolicName(token.getType()), "text", token.getText()));
                }
            }
            result.add(object("id", item[0], "sql", item[1], "input", item[2], "parseAccepted", true,
                    "normalized", SemanticAnalyzer.normalizeString(item[2], false), "tokens", tokens));
        }
        return result;
    }

    private static List<Object> aliases() throws Exception {
        String[][] cases = {
            {"ascii", "ALIAS"}, {"dotted_i", "\u0130"}, {"dotless_i", "\u0131"},
            {"sigma", "\u0391\u03a3"}, {"kelvin", "\u212a"}, {"sharp_s", "\u1e9e"},
            {"u1c89", "\u1c89"}, {"ua7cb", "\ua7cb"}, {"ua7ce", "\ua7ce"},
            {"ua7f1", "\ua7f1"}, {"u0264", "\u0264"}, {"u0295", "\u0295"},
        };
        List<Object> result = new ArrayList<>();
        for (String[] item : cases) {
            KeyStore store = KeyStore.getInstance("PKCS12");
            store.load(null, null);
            KeyStore.PasswordProtection password = new KeyStore.PasswordProtection("fixture".toCharArray());
            store.setEntry(item[1], new KeyStore.SecretKeyEntry(new SecretKeySpec(new byte[16], "AES")), password);
            String storedAlias = store.aliases().nextElement();
            String lowerEnglish = item[1].toLowerCase(Locale.ENGLISH);
            if (!storedAlias.equals(lowerEnglish)) {
                throw new AssertionError("PKCS12 alias did not use English full lowercase: " + item[0]);
            }
            result.add(object("id", item[0], "input", item[1], "storedAlias", storedAlias,
                    "lowerEnglish", lowerEnglish, "lookupOriginal", store.containsAlias(item[1]),
                    "lookupNormalized", store.containsAlias(storedAlias)));
        }
        return result;
    }

    private static String emit() throws Exception {
        if (Runtime.version().feature() != 21) {
            throw new IllegalStateException("RFC-265 oracle requires JDK 21, got " + Runtime.version());
        }
        Map<String, Object> fixture = object(
                "schema", 1,
                "java", object("runtimeVersion", System.getProperty("java.runtime.version"),
                        "vmVersion", System.getProperty("java.vm.version"), "vendor", System.getProperty("java.vendor")),
                "recordLayer", artifact(DefaultTextTokenizer.class, "org.foundationdb:fdb-record-layer-core:4.14.2.0"),
                "relational", artifact(SemanticAnalyzer.class, "org.foundationdb:fdb-relational-core:4.14.2.0"),
                "protobufRuntime", protobufRuntime(),
                "tokenizers", tokenizers(), "labels", labels(), "identifiers", identifiers(), "aliases", aliases(),
                "byteDecoding", byteDecoding());
        return new GsonBuilder().setPrettyPrinting().disableHtmlEscaping().create().toJson(fixture) + "\n";
    }

    public static void main(String[] args) throws Exception {
        String json = emit();
        if (args.length >= 2 && args[0].equals("--check")) {
            if (!Files.readString(Path.of(args[1]), StandardCharsets.UTF_8).equals(json)) {
                throw new AssertionError("Java Unicode-15 fixture drift; regenerate with bazel run //conformance:unicode15_fixtures -- --write");
            }
        } else if (args.length == 1 && args[0].equals("--write")) {
            Path output = Path.of(System.getenv("BUILD_WORKSPACE_DIRECTORY"), "conformance/testdata/unicode15_java.json");
            Files.writeString(output, json, StandardCharsets.UTF_8);
        } else if (args.length == 0) {
            System.out.print(json);
        } else {
            throw new IllegalArgumentException("expected no arguments, --write, or --check FILE");
        }
    }
}
