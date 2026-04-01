package types

// emptyVersionVector is the serialized form of an empty VersionVector.
// C++ VersionVector::getEncodedSize() = sizeof(size_t) + sizeof(Version) = 16.
var emptyVersionVector = make([]byte, 16)
