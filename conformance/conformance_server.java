package com.birdayz.conformance;

import com.google.gson.Gson;
import com.sun.net.httpserver.HttpExchange;
import com.sun.net.httpserver.HttpServer;

import java.io.IOException;
import java.io.InputStream;
import java.io.OutputStream;
import java.lang.reflect.Method;
import java.net.InetSocketAddress;
import java.nio.charset.StandardCharsets;
import java.util.HashMap;
import java.util.Map;
import java.util.concurrent.Executors;

/**
 * HTTP server that keeps Java process alive and handles conformance step invocations.
 * Eliminates Gradle startup overhead by running as a persistent daemon.
 */
class ConformanceServer {
    private static final Gson gson = new com.google.gson.GsonBuilder()
        .setObjectToNumberStrategy(com.google.gson.ToNumberPolicy.LONG_OR_DOUBLE)
        .create();
    private static final Object[] STEP_INSTANCES = {
        new CrudSteps(),
        new SplitSteps(),
        new IndexSteps(),
        new CompositeIndexSteps(),
        new ScanSteps(),
        new ContinuationSteps(),
        new CountSteps(),
        new VersionSteps(),
        new CustomerSteps(),
        new FanoutIndexSteps(),
        new CountIndexSteps(),
        new RebuildIndexSteps(),
        new DeleteAllSteps(),
        new RangeSetSteps(),
        new StoreHeaderSteps(),
        new StoreHeaderV2Steps(),
        new IndexStateSteps(),
        new SumIndexSteps(),
        new MinMaxEverIndexSteps(),
        new MinMaxEverTupleIndexSteps(),
        new ClearWhenZeroSteps(),
        new CountNotNullIndexSteps(),
        new CountUpdatesIndexSteps(),
        new RankIndexSteps(),
        new CoveringIndexSteps(),
        new DeleteRecordsWhereSteps(),
        new VersionIndexSteps(),
        new OnlineIndexerSteps(),
        new MaxEverVersionIndexSteps(),
    };

    private static class StepEntry {
        final Object instance;
        final Method method;
        StepEntry(Object instance, Method method) {
            this.instance = instance;
            this.method = method;
        }
    }

    private static final Map<String, StepEntry> stepMap = new HashMap<>();

    static {
        for (Object instance : STEP_INSTANCES) {
            for (Method method : instance.getClass().getDeclaredMethods()) {
                ConformanceStep annotation = method.getAnnotation(ConformanceStep.class);
                if (annotation != null) {
                    String name = annotation.value();
                    StepEntry existing = stepMap.get(name);
                    if (existing != null) {
                        throw new RuntimeException(
                            "Duplicate conformance step name '" + name + "': " +
                            existing.instance.getClass().getName() + "." + existing.method.getName() +
                            " and " + instance.getClass().getName() + "." + method.getName());
                    }
                    stepMap.put(name, new StepEntry(instance, method));
                }
            }
        }
    }

    public static void main(String[] args) throws IOException {
        // Bind to random available port
        HttpServer server = HttpServer.create(new InetSocketAddress("127.0.0.1", 0), 0);
        server.setExecutor(Executors.newCachedThreadPool());

        server.createContext("/invoke", ConformanceServer::handleInvoke);
        server.createContext("/health", ConformanceServer::handleHealth);
        server.createContext("/shutdown", ConformanceServer::handleShutdown);

        server.start();

        int port = server.getAddress().getPort();

        // Print port to stdout so Go can capture it
        System.out.println("CONFORMANCE_SERVER_PORT=" + port);
        System.out.flush();

        System.err.println("Conformance server listening on port " + port);
    }

    private static void handleHealth(HttpExchange exchange) throws IOException {
        String response = "{\"status\":\"ok\"}";
        byte[] responseBytes = response.getBytes(StandardCharsets.UTF_8);
        exchange.getResponseHeaders().set("Content-Type", "application/json");
        exchange.sendResponseHeaders(200, responseBytes.length);
        try (OutputStream os = exchange.getResponseBody()) {
            os.write(responseBytes);
        }
    }

    private static void handleShutdown(HttpExchange exchange) throws IOException {
        String response = "{\"status\":\"shutting down\"}";
        byte[] responseBytes = response.getBytes(StandardCharsets.UTF_8);
        exchange.getResponseHeaders().set("Content-Type", "application/json");
        exchange.sendResponseHeaders(200, responseBytes.length);
        try (OutputStream os = exchange.getResponseBody()) {
            os.write(responseBytes);
        }
        System.exit(0);
    }

    private static void handleInvoke(HttpExchange exchange) throws IOException {
        if (!"POST".equals(exchange.getRequestMethod())) {
            sendError(exchange, 405, "Method not allowed");
            return;
        }

        try {
            // Read request body
            String requestBody;
            try (InputStream is = exchange.getRequestBody()) {
                requestBody = new String(is.readAllBytes(), StandardCharsets.UTF_8);
            }

            // Parse JSON request
            @SuppressWarnings("unchecked")
            Map<String, Object> request = gson.fromJson(requestBody, Map.class);

            String stepName = (String) request.get("step");
            @SuppressWarnings("unchecked")
            Map<String, Object> params = (Map<String, Object>) request.get("params");

            if (stepName == null || params == null) {
                sendError(exchange, 400, "Missing 'step' or 'params' in request");
                return;
            }

            // Find and invoke the method
            Object result = invokeStep(stepName, params);

            // Send response
            // For protobuf messages, use JsonFormat instead of Gson
            String resultJson;
            if (result != null && result instanceof com.google.protobuf.Message) {
                resultJson = com.google.protobuf.util.JsonFormat.printer()
                    .print((com.google.protobuf.Message) result);
            } else if (result != null) {
                resultJson = gson.toJson(result);
            } else {
                resultJson = "null";
            }

            // Build response manually to avoid double-encoding JSON
            String responseBody = String.format("{\"success\":true,\"result\":%s}", resultJson);
            byte[] responseBytes = responseBody.getBytes(StandardCharsets.UTF_8);
            exchange.getResponseHeaders().set("Content-Type", "application/json");
            exchange.sendResponseHeaders(200, responseBytes.length);
            try (OutputStream os = exchange.getResponseBody()) {
                os.write(responseBytes);
            }

        } catch (Exception e) {
            e.printStackTrace();
            String errorMsg = e.getMessage() != null ? e.getMessage() : e.getClass().getName();
            if (e.getCause() != null) {
                errorMsg += " (caused by: " + e.getCause() + ")";
            }
            sendError(exchange, 500, "Error invoking step: " + errorMsg);
        }
    }

    private static void sendError(HttpExchange exchange, int code, String message) throws IOException {
        Map<String, Object> response = Map.of(
            "success", false,
            "error", message
        );
        String responseBody = gson.toJson(response);
        byte[] responseBytes = responseBody.getBytes(StandardCharsets.UTF_8);
        exchange.getResponseHeaders().set("Content-Type", "application/json");
        exchange.sendResponseHeaders(code, responseBytes.length);
        try (OutputStream os = exchange.getResponseBody()) {
            os.write(responseBytes);
        }
    }

    private static Object invokeStep(String stepName, Map<String, Object> params) throws Exception {
        StepEntry entry = stepMap.get(stepName);
        if (entry == null) {
            throw new IllegalArgumentException("No conformance step found with name: " + stepName);
        }
        Object[] args = deserializeArgs(entry.method, gson.toJsonTree(params));
        return entry.method.invoke(entry.instance, args);
    }

    private static Object[] deserializeArgs(Method method, com.google.gson.JsonElement argsJson) {
        java.lang.reflect.Parameter[] params = method.getParameters();
        Object[] result = new Object[params.length];

        if (argsJson == null || argsJson.isJsonNull()) {
            return result;
        }

        if (argsJson.isJsonObject()) {
            com.google.gson.JsonObject argsObj = argsJson.getAsJsonObject();

            for (int i = 0; i < params.length; i++) {
                String paramName = params[i].getName();
                Class<?> paramType = params[i].getType();

                com.google.gson.JsonElement value = argsObj.get(paramName);
                if (value == null) {
                    String camelCase = toCamelCase(paramName);
                    value = argsObj.get(camelCase);
                }
                if (value == null) {
                    String snakeCase = toSnakeCase(paramName);
                    value = argsObj.get(snakeCase);
                }

                if (value != null && !value.isJsonNull()) {
                    if (com.google.protobuf.Message.class.isAssignableFrom(paramType)) {
                        try {
                            Method newBuilder = paramType.getMethod("newBuilder");
                            com.google.protobuf.Message.Builder builder = (com.google.protobuf.Message.Builder) newBuilder.invoke(null);
                            com.google.protobuf.util.JsonFormat.parser()
                                .ignoringUnknownFields()
                                .merge(value.toString(), builder);
                            result[i] = builder.build();
                        } catch (Exception e) {
                            throw new RuntimeException("Failed to deserialize protobuf message: " + e.getMessage(), e);
                        }
                    } else if (paramType == long.class || paramType == Long.class) {
                        // Handle long specially to avoid precision loss from double conversion
                        result[i] = value.getAsLong();
                    } else if (paramType == int.class || paramType == Integer.class) {
                        result[i] = value.getAsInt();
                    } else if (paramType == boolean.class || paramType == Boolean.class) {
                        result[i] = value.getAsBoolean();
                    } else if (paramType == String.class) {
                        result[i] = value.getAsString();
                    } else {
                        result[i] = gson.fromJson(value, paramType);
                    }
                }
            }
        } else if (params.length == 1) {
            result[0] = gson.fromJson(argsJson, params[0].getType());
        }

        return result;
    }

    private static String toCamelCase(String snake) {
        StringBuilder result = new StringBuilder();
        boolean capitalizeNext = false;
        for (char c : snake.toCharArray()) {
            if (c == '_') {
                capitalizeNext = true;
            } else {
                result.append(capitalizeNext ? Character.toUpperCase(c) : c);
                capitalizeNext = false;
            }
        }
        return result.toString();
    }

    private static String toSnakeCase(String camel) {
        return camel.replaceAll("([a-z])([A-Z])", "$1_$2").toLowerCase();
    }
}
