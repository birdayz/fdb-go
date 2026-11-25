package difforacle

// --- Reply and transport serialize methods ---

// SerializeGetReadVersionReply sends a GetReadVersionReply to the oracle.
func (o *Oracle) SerializeGetReadVersionReply(
	processBusyTime int32, version int64, locked bool,
	hasMetadataVersion bool, metadataVersion []byte,
	tagThrottleInfo []byte, midShardSize int64,
	rkDefaultThrottled, rkBatchThrottled bool,
	ssVersionVectorDelta []byte,
	proxyId [16]byte, proxyTagThrottledDuration float64,
) ([]byte, error) {
	o.mu.Lock()
	defer o.mu.Unlock()

	if err := o.writeU8(typeGetReadVersionReply); err != nil {
		return nil, err
	}
	if err := o.writeI32(processBusyTime); err != nil {
		return nil, err
	}
	if err := o.writeI64(version); err != nil {
		return nil, err
	}
	if err := o.writeBool(locked); err != nil {
		return nil, err
	}
	if err := o.writeBool(hasMetadataVersion); err != nil {
		return nil, err
	}
	if hasMetadataVersion {
		if err := o.writeBytes(metadataVersion); err != nil {
			return nil, err
		}
	}
	if err := o.writeBytes(tagThrottleInfo); err != nil {
		return nil, err
	}
	if err := o.writeI64(midShardSize); err != nil {
		return nil, err
	}
	if err := o.writeBool(rkDefaultThrottled); err != nil {
		return nil, err
	}
	if err := o.writeBool(rkBatchThrottled); err != nil {
		return nil, err
	}
	if err := o.writeBytes(ssVersionVectorDelta); err != nil {
		return nil, err
	}
	if err := o.writeUID(proxyId); err != nil {
		return nil, err
	}
	if err := o.writeF64(proxyTagThrottledDuration); err != nil {
		return nil, err
	}
	return o.readResponse()
}

// SerializeGetValueReply sends a GetValueReply to the oracle.
func (o *Oracle) SerializeGetValueReply(
	penalty float64,
	hasError bool, errorData []byte,
	hasValue bool, value []byte,
	cached bool,
) ([]byte, error) {
	o.mu.Lock()
	defer o.mu.Unlock()

	if err := o.writeU8(typeGetValueReply); err != nil {
		return nil, err
	}
	if err := o.writeF64(penalty); err != nil {
		return nil, err
	}
	if err := o.writeBool(hasError); err != nil {
		return nil, err
	}
	if hasError {
		if err := o.writeBytes(errorData); err != nil {
			return nil, err
		}
	}
	if err := o.writeBool(hasValue); err != nil {
		return nil, err
	}
	if hasValue {
		if err := o.writeBytes(value); err != nil {
			return nil, err
		}
	}
	if err := o.writeBool(cached); err != nil {
		return nil, err
	}
	return o.readResponse()
}

// SerializeGetKeyReply sends a GetKeyReply to the oracle.
func (o *Oracle) SerializeGetKeyReply(
	penalty float64,
	hasError bool, errorData []byte,
	cached bool,
) ([]byte, error) {
	o.mu.Lock()
	defer o.mu.Unlock()

	if err := o.writeU8(typeGetKeyReply); err != nil {
		return nil, err
	}
	if err := o.writeF64(penalty); err != nil {
		return nil, err
	}
	if err := o.writeBool(hasError); err != nil {
		return nil, err
	}
	if hasError {
		if err := o.writeBytes(errorData); err != nil {
			return nil, err
		}
	}
	if err := o.writeBool(cached); err != nil {
		return nil, err
	}
	return o.readResponse()
}

// SerializeGetKeyValuesReply sends a GetKeyValuesReply to the oracle.
func (o *Oracle) SerializeGetKeyValuesReply(
	penalty float64,
	hasError bool, errorData []byte,
	data []byte, version int64,
	more, cached bool,
) ([]byte, error) {
	o.mu.Lock()
	defer o.mu.Unlock()

	if err := o.writeU8(typeGetKeyValuesReply); err != nil {
		return nil, err
	}
	if err := o.writeF64(penalty); err != nil {
		return nil, err
	}
	if err := o.writeBool(hasError); err != nil {
		return nil, err
	}
	if hasError {
		if err := o.writeBytes(errorData); err != nil {
			return nil, err
		}
	}
	if err := o.writeBytes(data); err != nil {
		return nil, err
	}
	if err := o.writeI64(version); err != nil {
		return nil, err
	}
	if err := o.writeBool(more); err != nil {
		return nil, err
	}
	if err := o.writeBool(cached); err != nil {
		return nil, err
	}
	return o.readResponse()
}

// SerializeGetKeyServerLocationsReply sends a GetKeyServerLocationsReply to the oracle.
func (o *Oracle) SerializeGetKeyServerLocationsReply(
	results, resultsTssMapping, resultsTagMapping []byte,
) ([]byte, error) {
	o.mu.Lock()
	defer o.mu.Unlock()

	if err := o.writeU8(typeGetKeyServerLocationsReply); err != nil {
		return nil, err
	}
	if err := o.writeBytes(results); err != nil {
		return nil, err
	}
	if err := o.writeBytes(resultsTssMapping); err != nil {
		return nil, err
	}
	if err := o.writeBytes(resultsTagMapping); err != nil {
		return nil, err
	}
	return o.readResponse()
}

// SerializeCommitID sends a CommitID to the oracle.
func (o *Oracle) SerializeCommitID(
	version int64, txnBatchId uint16,
	hasMetadataVersion bool, metadataVersion []byte,
	hasConflictingKRIndices bool, conflictingKRIndices []byte,
) ([]byte, error) {
	o.mu.Lock()
	defer o.mu.Unlock()

	if err := o.writeU8(typeCommitID); err != nil {
		return nil, err
	}
	if err := o.writeI64(version); err != nil {
		return nil, err
	}
	if err := o.writeU16(txnBatchId); err != nil {
		return nil, err
	}
	if err := o.writeBool(hasMetadataVersion); err != nil {
		return nil, err
	}
	if hasMetadataVersion {
		if err := o.writeBytes(metadataVersion); err != nil {
			return nil, err
		}
	}
	if err := o.writeBool(hasConflictingKRIndices); err != nil {
		return nil, err
	}
	if hasConflictingKRIndices {
		if err := o.writeBytes(conflictingKRIndices); err != nil {
			return nil, err
		}
	}
	return o.readResponse()
}

// SerializeError sends an Error to the oracle.
func (o *Oracle) SerializeError(errorCode uint16) ([]byte, error) {
	o.mu.Lock()
	defer o.mu.Unlock()

	if err := o.writeU8(typeError); err != nil {
		return nil, err
	}
	if err := o.writeU16(errorCode); err != nil {
		return nil, err
	}
	return o.readResponse()
}

// SerializeClientDBInfo sends a ClientDBInfo to the oracle.
func (o *Oracle) SerializeClientDBInfo(
	grvProxies, commitProxies []byte,
	id [16]byte,
	hasForward bool, forward []byte,
	history []byte,
	hasEncryptKeyProxy bool, encryptKeyProxy []byte,
	clusterId [16]byte, clusterType int32,
	hasMetaclusterName bool, metaclusterName []byte,
) ([]byte, error) {
	o.mu.Lock()
	defer o.mu.Unlock()

	if err := o.writeU8(typeClientDBInfo); err != nil {
		return nil, err
	}
	if err := o.writeBytes(grvProxies); err != nil {
		return nil, err
	}
	if err := o.writeBytes(commitProxies); err != nil {
		return nil, err
	}
	if err := o.writeUID(id); err != nil {
		return nil, err
	}
	if err := o.writeBool(hasForward); err != nil {
		return nil, err
	}
	if hasForward {
		if err := o.writeBytes(forward); err != nil {
			return nil, err
		}
	}
	if err := o.writeBytes(history); err != nil {
		return nil, err
	}
	if err := o.writeBool(hasEncryptKeyProxy); err != nil {
		return nil, err
	}
	if hasEncryptKeyProxy {
		if err := o.writeBytes(encryptKeyProxy); err != nil {
			return nil, err
		}
	}
	if err := o.writeUID(clusterId); err != nil {
		return nil, err
	}
	if err := o.writeI32(clusterType); err != nil {
		return nil, err
	}
	if err := o.writeBool(hasMetaclusterName); err != nil {
		return nil, err
	}
	if hasMetaclusterName {
		if err := o.writeBytes(metaclusterName); err != nil {
			return nil, err
		}
	}
	return o.readResponse()
}

// SerializeNetworkAddress sends a NetworkAddress to the oracle.
func (o *Oracle) SerializeNetworkAddress(
	ipAddr uint32, port, flags uint16, fromHostname bool,
) ([]byte, error) {
	o.mu.Lock()
	defer o.mu.Unlock()

	if err := o.writeU8(typeNetworkAddress); err != nil {
		return nil, err
	}
	if err := o.writeU32(ipAddr); err != nil {
		return nil, err
	}
	if err := o.writeU16(port); err != nil {
		return nil, err
	}
	if err := o.writeU16(flags); err != nil {
		return nil, err
	}
	if err := o.writeBool(fromHostname); err != nil {
		return nil, err
	}
	return o.readResponse()
}

// SerializeNetworkAddressV6 sends an IPv6 NetworkAddress to the oracle (16-byte address),
// exercising the IPAddress variant tag=2 (count-prefixed vector) marshal path.
func (o *Oracle) SerializeNetworkAddressV6(
	ip16 [16]byte, port, flags uint16, fromHostname bool,
) ([]byte, error) {
	o.mu.Lock()
	defer o.mu.Unlock()

	if err := o.writeU8(typeNetworkAddressV6); err != nil {
		return nil, err
	}
	if err := o.writeBytes(ip16[:]); err != nil {
		return nil, err
	}
	if err := o.writeU16(port); err != nil {
		return nil, err
	}
	if err := o.writeU16(flags); err != nil {
		return nil, err
	}
	if err := o.writeBool(fromHostname); err != nil {
		return nil, err
	}
	return o.readResponse()
}

// SerializeEndpoint sends an Endpoint to the oracle.
func (o *Oracle) SerializeEndpoint(
	ipAddr uint32, port, flags uint16, fromHostname bool,
	token [16]byte,
) ([]byte, error) {
	o.mu.Lock()
	defer o.mu.Unlock()

	if err := o.writeU8(typeEndpoint); err != nil {
		return nil, err
	}
	if err := o.writeU32(ipAddr); err != nil {
		return nil, err
	}
	if err := o.writeU16(port); err != nil {
		return nil, err
	}
	if err := o.writeU16(flags); err != nil {
		return nil, err
	}
	if err := o.writeBool(fromHostname); err != nil {
		return nil, err
	}
	if err := o.writeUID(token); err != nil {
		return nil, err
	}
	return o.readResponse()
}

// SerializeReplyPromise sends a ReplyPromise to the oracle.
func (o *Oracle) SerializeReplyPromise(token [16]byte) ([]byte, error) {
	o.mu.Lock()
	defer o.mu.Unlock()

	if err := o.writeU8(typeReplyPromise); err != nil {
		return nil, err
	}
	if err := o.writeUID(token); err != nil {
		return nil, err
	}
	return o.readResponse()
}
