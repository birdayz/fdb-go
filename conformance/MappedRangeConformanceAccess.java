package com.apple.foundationdb;

import java.util.List;

/** Test-only single-batch access; public asList() changes ITERATOR to EXACT/WANT_ALL. */
public final class MappedRangeConformanceAccess {
    private MappedRangeConformanceAccess() {
    }

    public static final class Batch {
        public final List<MappedKeyValue> rows;
        public final boolean more;

        private Batch(MappedRangeResult result) {
            rows = result.values;
            more = result.more;
        }
    }

    public static Batch fetch(Transaction transaction, KeySelector begin, KeySelector end,
                              byte[] mapper, int limit, StreamingMode mode, int iteration,
                              boolean reverse) {
        try (FutureMappedResults future = ((FDBTransaction) transaction).getMappedRange_internal(
                begin, end, mapper, limit, 0, mode.code(), iteration, false, reverse)) {
            return new Batch(future.join().get());
        }
    }
}
