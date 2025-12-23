package com.birdayz.conformance;

import java.lang.annotation.*;

/**
 * Marks a method as a conformance test step that can be invoked from Go.
 *
 * The annotated method can be called by name using the generic invokeJava() function.
 * Arguments are passed as JSON and automatically deserialized to method parameters.
 * The return value is serialized to JSON and returned to the caller.
 *
 * Example:
 * <pre>
 * {@literal @}ConformanceStep("saveOrder")
 * public void saveOrder(byte[] subspace, Order order) {
 *     // Implementation
 * }
 * </pre>
 */
@Retention(RetentionPolicy.RUNTIME)
@Target(ElementType.METHOD)
public @interface ConformanceStep {
    /**
     * The step name used to invoke this method from Go.
     * Should be descriptive like "saveOrder", "loadOrder", "deleteOrder".
     */
    String value();
}
