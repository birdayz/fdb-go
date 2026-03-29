// Minimal extract of generate_vtable from foundationdb/flow/flat_buffers.cpp.
// Only the non-test code, avoiding UnitTest.h and other FDB deps.

#include "flow/flat_buffers.h"

namespace detail {

void swapWithThreadLocalGlobal(std::vector<int>& writeToOffsets) {
    static thread_local std::vector<int> global;
    global.swap(writeToOffsets);
}

VTable generate_vtable(size_t numMembers, const std::vector<unsigned>& sizesAlignments) {
    if (numMembers == 0) {
        return VTable{ 4, 4 };
    }
    std::vector<std::pair<unsigned, unsigned>> indexed;
    indexed.reserve(numMembers);
    for (unsigned i = 0; i < numMembers; ++i) {
        if (sizesAlignments[i] > 0) {
            indexed.emplace_back(i, sizesAlignments[i]);
        }
    }
    std::stable_sort(indexed.begin(),
                     indexed.end(),
                     [](const std::pair<unsigned, unsigned>& lhs, const std::pair<unsigned, unsigned>& rhs) {
                         return lhs.second > rhs.second;
                     });
    VTable result;
    result.resize(numMembers + 2);
    result[0] = 2 * numMembers + 4;
    int offset = 0;
    for (auto p : indexed) {
        auto align = sizesAlignments[numMembers + p.first];
        auto& res = result[p.first + 2];
        res = offset % align == 0 ? offset : ((offset / align) + 1) * align;
        offset = res + p.second;
        res += 4;
    }
    result[1] = offset + 4;
    return result;
}

} // namespace detail
