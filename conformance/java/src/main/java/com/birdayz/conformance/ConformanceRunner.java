package com.birdayz.conformance;

import com.google.gson.*;
import com.google.protobuf.Message;
import com.google.protobuf.util.JsonFormat;
import java.lang.reflect.*;
import java.io.*;

/**
 * Generic conformance test runner that uses reflection to dispatch to @ConformanceStep annotated methods.
 *
 * Reads JSON request from stdin:
 * {
 *   "step": "saveOrder",
 *   "args": {"subspace": [...], "order": {...}}
 * }
 *
 * Executes the annotated method and writes JSON response to stdout:
 * {
 *   "success": true,
 *   "result": {...},
 *   "error": null
 * }
 */
public class ConformanceRunner {
    private static final Gson gson = new GsonBuilder()
        .setFieldNamingPolicy(FieldNamingPolicy.LOWER_CASE_WITH_UNDERSCORES)
        .create();

    public static void main(String[] args) {
        try {
            // Read JSON request from stdin
            JsonObject request = gson.fromJson(
                new InputStreamReader(System.in),
                JsonObject.class
            );

            String stepName = request.get("step").getAsString();
            JsonElement argsJson = request.get("args");

            // Find and invoke annotated method
            Object result = invokeStep(stepName, argsJson);

            // Send success response
            JsonObject response = new JsonObject();
            response.addProperty("success", true);

            // Handle protobuf messages specially
            if (result instanceof Message) {
                String jsonString = JsonFormat.printer()
                    .preservingProtoFieldNames()    // Use proto field names (order_id) not camelCase (orderId)
                    .print((Message) result);
                response.add("result", JsonParser.parseString(jsonString));
            } else {
                response.add("result", gson.toJsonTree(result));
            }
            response.add("error", JsonNull.INSTANCE);

            System.out.println(gson.toJson(response));
            System.out.flush();

        } catch (Exception e) {
            e.printStackTrace(System.err);

            // Send error response
            JsonObject response = new JsonObject();
            response.addProperty("success", false);
            response.add("result", JsonNull.INSTANCE);

            JsonObject error = new JsonObject();
            error.addProperty("message", e.getMessage());
            error.addProperty("stackTrace", getStackTrace(e));
            response.add("error", error);

            System.out.println(gson.toJson(response));
            System.out.flush();
            System.exit(1);
        }
    }

    private static Object invokeStep(String stepName, JsonElement argsJson) throws Exception {
        // Find all methods with @ConformanceStep annotation
        for (Method method : ConformanceSteps.class.getDeclaredMethods()) {
            ConformanceStep annotation = method.getAnnotation(ConformanceStep.class);
            if (annotation != null && annotation.value().equals(stepName)) {
                // Deserialize args to method parameters
                Object[] params = deserializeArgs(method, argsJson);

                // Invoke method on new instance
                ConformanceSteps instance = new ConformanceSteps();
                return method.invoke(instance, params);
            }
        }

        throw new IllegalArgumentException("Unknown step: " + stepName);
    }

    private static Object[] deserializeArgs(Method method, JsonElement argsJson) {
        Parameter[] params = method.getParameters();
        Object[] result = new Object[params.length];

        if (argsJson == null || argsJson.isJsonNull()) {
            // No args provided
            return result;
        }

        if (argsJson.isJsonObject()) {
            JsonObject argsObj = argsJson.getAsJsonObject();

            // Match args by parameter name
            for (int i = 0; i < params.length; i++) {
                String paramName = params[i].getName();
                Class<?> paramType = params[i].getType();

                // Try both camelCase and snake_case
                JsonElement value = argsObj.get(paramName);
                if (value == null) {
                    // Try snake_case to camelCase conversion
                    String camelCase = toCamelCase(paramName);
                    value = argsObj.get(camelCase);
                }
                if (value == null) {
                    // Try camelCase to snake_case conversion
                    String snakeCase = toSnakeCase(paramName);
                    value = argsObj.get(snakeCase);
                }

                if (value != null && !value.isJsonNull()) {
                    // Handle protobuf messages specially
                    if (Message.class.isAssignableFrom(paramType)) {
                        try {
                            // Get the newBuilder() method from the protobuf class
                            Method newBuilder = paramType.getMethod("newBuilder");
                            Message.Builder builder = (Message.Builder) newBuilder.invoke(null);
                            // Merge from JSON string - ignore unknown fields for flexibility
                            JsonFormat.parser()
                                .ignoringUnknownFields()
                                .merge(value.toString(), builder);
                            result[i] = builder.build();
                        } catch (Exception e) {
                            throw new RuntimeException("Failed to deserialize protobuf message: " + e.getMessage(), e);
                        }
                    } else {
                        result[i] = gson.fromJson(value, paramType);
                    }
                }
            }
        } else if (params.length == 1) {
            // Single parameter - deserialize directly
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

    private static String getStackTrace(Exception e) {
        StringWriter sw = new StringWriter();
        e.printStackTrace(new PrintWriter(sw));
        return sw.toString();
    }
}
