// Stub — satisfies #include in FDB's flat_buffers.h.
// We don't use boost::container::flat_map in any protocol messages.
#pragma once
namespace boost { namespace container {
template <class K, class V, class C = void, class A = void>
class flat_map {};
}} // namespace boost::container
