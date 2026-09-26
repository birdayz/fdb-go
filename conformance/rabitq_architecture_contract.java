package com.birdayz.conformance;

import com.apple.foundationdb.linear.DoubleRealVector;
import com.apple.foundationdb.linear.Metric;
import com.apple.foundationdb.rabitq.RaBitQuantizer;

import java.util.HexFormat;

/** Standalone architecture oracle: no FoundationDB process or native client is required. */
class RaBitQArchitectureContract {
    public static void main(String[] args) {
        final var quantizer = new RaBitQuantizer(Metric.EUCLIDEAN_SQUARE_METRIC, 1);
        final var boundary = new double[]{Math.scalb(9.0, -29), 1.0 + Math.scalb(1.0, -27)};
        final var encoded = HexFormat.of().formatHex(quantizer.encode(new DoubleRealVector(boundary)).getRawData());
        final var expected = "033ff0000004000001bff55555560000003ff4444433ccccd0b0";
        if (!encoded.equals(expected)) {
            throw new AssertionError("separately rounded encoding: " + encoded + " != " + expected);
        }
        final var zero = HexFormat.of().formatHex(quantizer.encode(new DoubleRealVector(new double[4])).getRawData());
        System.out.println("RABITQ-ARCH arch=" + System.getProperty("os.arch") + " boundary=" + encoded + " zero=" + zero);
        // Retain the complete bytes rather than treating all NaN payloads as
        // equivalent: they are persisted and participate in content signatures.
        final var architecture = System.getProperty("os.arch");
        final String expectedZero;
        switch (architecture) {
            case "aarch64":
                expectedZero = "03000000000000000080000000000000007ff8000000000000aa";
                break;
            case "amd64":
                expectedZero = "0300000000000000008000000000000000fff8000000000000aa";
                break;
            default:
                throw new AssertionError("unmeasured Java NaN architecture: " + architecture);
        }
        // Math.sqrt of a negative argument preserves the host's native NaN sign.
        // Java 4.14.2.0 writes that sign verbatim, despite using scalar reductions.
        if (!zero.equals(expectedZero)) {
            throw new AssertionError("zero encoding: " + zero + " != " + expectedZero);
        }
        final var vectors = new double[][]{
            new double[4], new double[]{-0.0, 0.0, -0.0}, new double[]{1.0, 1.0, 1.0}
        };
        final var names = new String[]{"zero_four", "signed_zero", "equal_three"};
        final var nanBits = architecture.equals("aarch64") ? "7ff8000000000000" : "fff8000000000000";
        final var expectedEncodings = new String[][]{
            {"0300000000000000008000000000000000" + nanBits + "aa",
             "0300000000000000008000000000000000" + nanBits + "842100",
             "0300000000000000008000000000000000" + nanBits + "8040201000"},
            {"0300000000000000008000000000000000" + nanBits + "a8",
             "0300000000000000008000000000000000" + nanBits + "8420",
             "0300000000000000008000000000000000" + nanBits + "80402000"},
            {"034008000000000000bff55555555555530000000000000000fc",
             "034008000000000000bfc08421084210840000000000000000fffe",
             "034008000000000000bf80182436517a37" + nanBits + "ff7fbfc0"}
        };
        for (int i = 0; i < vectors.length; i++) {
            for (int bitIndex = 0; bitIndex < 3; bitIndex++) {
                final int bits = new int[]{1, 4, 8}[bitIndex];
                final var bytes = new RaBitQuantizer(Metric.EUCLIDEAN_SQUARE_METRIC, bits)
                        .encode(new DoubleRealVector(vectors[i])).getRawData();
                final var actual = HexFormat.of().formatHex(bytes);
                System.out.println("RABITQ-ARCH-GOLDEN name=" + names[i] + "_" + bits + " hex=" + actual);
                if (!actual.equals(expectedEncodings[i][bitIndex])) {
                    throw new AssertionError(names[i] + "_" + bits + ": " + actual + " != " + expectedEncodings[i][bitIndex]);
                }
            }
        }
    }
}
