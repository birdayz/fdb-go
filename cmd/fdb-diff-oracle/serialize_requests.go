package difforacle

// --- Request serialize methods ---

// SerializeGetReadVersionRequest sends a GetReadVersionRequest to the oracle.
func (o *Oracle) SerializeGetReadVersionRequest(
	transactionCount, flags uint32,
	hasTags bool, tags []byte,
	hasDebugID bool, debugID []byte,
	maxVersion int64,
) ([]byte, error) {
	o.mu.Lock()
	defer o.mu.Unlock()

	if err := o.writeU8(typeGetReadVersionRequest); err != nil {
		return nil, err
	}
	if err := o.writeU32(transactionCount); err != nil {
		return nil, err
	}
	if err := o.writeU32(flags); err != nil {
		return nil, err
	}
	if err := o.writeBool(hasTags); err != nil {
		return nil, err
	}
	if hasTags {
		if err := o.writeBytes(tags); err != nil {
			return nil, err
		}
	}
	if err := o.writeBool(hasDebugID); err != nil {
		return nil, err
	}
	if hasDebugID {
		if err := o.writeBytes(debugID); err != nil {
			return nil, err
		}
	}
	if err := o.writeI64(maxVersion); err != nil {
		return nil, err
	}
	return o.readResponse()
}

// SerializeGetValueRequest sends a GetValueRequest to the oracle.
func (o *Oracle) SerializeGetValueRequest(
	key []byte, version int64,
	hasTags bool, tags []byte,
	tenantId int64,
	hasOptions bool,
	ssLatestCommitVersions []byte,
) ([]byte, error) {
	o.mu.Lock()
	defer o.mu.Unlock()

	if err := o.writeU8(typeGetValueRequest); err != nil {
		return nil, err
	}
	if err := o.writeBytes(key); err != nil {
		return nil, err
	}
	if err := o.writeI64(version); err != nil {
		return nil, err
	}
	if err := o.writeBool(hasTags); err != nil {
		return nil, err
	}
	if hasTags {
		if err := o.writeBytes(tags); err != nil {
			return nil, err
		}
	}
	if err := o.writeI64(tenantId); err != nil {
		return nil, err
	}
	if err := o.writeBool(hasOptions); err != nil {
		return nil, err
	}
	if err := o.writeBytes(ssLatestCommitVersions); err != nil {
		return nil, err
	}
	return o.readResponse()
}

// SerializeGetKeyRequest sends a GetKeyRequest to the oracle.
func (o *Oracle) SerializeGetKeyRequest(
	selKey []byte, selOrEqual bool, selOffset int32,
	version int64,
	hasTags bool, tags []byte,
	tenantId int64,
	hasOptions bool,
	ssLatestCommitVersions []byte,
	field10 []byte,
) ([]byte, error) {
	o.mu.Lock()
	defer o.mu.Unlock()

	if err := o.writeU8(typeGetKeyRequest); err != nil {
		return nil, err
	}
	if err := o.writeBytes(selKey); err != nil {
		return nil, err
	}
	if err := o.writeBool(selOrEqual); err != nil {
		return nil, err
	}
	if err := o.writeI32(selOffset); err != nil {
		return nil, err
	}
	if err := o.writeI64(version); err != nil {
		return nil, err
	}
	if err := o.writeBool(hasTags); err != nil {
		return nil, err
	}
	if hasTags {
		if err := o.writeBytes(tags); err != nil {
			return nil, err
		}
	}
	if err := o.writeI64(tenantId); err != nil {
		return nil, err
	}
	if err := o.writeBool(hasOptions); err != nil {
		return nil, err
	}
	if err := o.writeBytes(ssLatestCommitVersions); err != nil {
		return nil, err
	}
	if err := o.writeBytes(field10); err != nil {
		return nil, err
	}
	return o.readResponse()
}

// SerializeGetKeyValuesRequest sends a GetKeyValuesRequest to the oracle.
func (o *Oracle) SerializeGetKeyValuesRequest(
	beginKey []byte, beginOrEqual bool, beginOffset int32,
	endKey []byte, endOrEqual bool, endOffset int32,
	version int64, limit, limitBytes int32,
	hasTags bool, tags []byte,
	tenantId int64,
	hasOptions bool,
	ssLatestCommitVersions []byte,
) ([]byte, error) {
	o.mu.Lock()
	defer o.mu.Unlock()

	if err := o.writeU8(typeGetKeyValuesRequest); err != nil {
		return nil, err
	}
	if err := o.writeBytes(beginKey); err != nil {
		return nil, err
	}
	if err := o.writeBool(beginOrEqual); err != nil {
		return nil, err
	}
	if err := o.writeI32(beginOffset); err != nil {
		return nil, err
	}
	if err := o.writeBytes(endKey); err != nil {
		return nil, err
	}
	if err := o.writeBool(endOrEqual); err != nil {
		return nil, err
	}
	if err := o.writeI32(endOffset); err != nil {
		return nil, err
	}
	if err := o.writeI64(version); err != nil {
		return nil, err
	}
	if err := o.writeI32(limit); err != nil {
		return nil, err
	}
	if err := o.writeI32(limitBytes); err != nil {
		return nil, err
	}
	if err := o.writeBool(hasTags); err != nil {
		return nil, err
	}
	if hasTags {
		if err := o.writeBytes(tags); err != nil {
			return nil, err
		}
	}
	if err := o.writeI64(tenantId); err != nil {
		return nil, err
	}
	if err := o.writeBool(hasOptions); err != nil {
		return nil, err
	}
	if err := o.writeBytes(ssLatestCommitVersions); err != nil {
		return nil, err
	}
	return o.readResponse()
}

// SerializeGetKeyServerLocationsRequest sends a GetKeyServerLocationsRequest to the oracle.
func (o *Oracle) SerializeGetKeyServerLocationsRequest(
	begin []byte, hasEnd bool, end []byte,
	limit int32, reverse bool,
	tenantId, minTenantVersion int64,
) ([]byte, error) {
	o.mu.Lock()
	defer o.mu.Unlock()

	if err := o.writeU8(typeGetKeyServerLocationsRequest); err != nil {
		return nil, err
	}
	if err := o.writeBytes(begin); err != nil {
		return nil, err
	}
	if err := o.writeBool(hasEnd); err != nil {
		return nil, err
	}
	if hasEnd {
		if err := o.writeBytes(end); err != nil {
			return nil, err
		}
	}
	if err := o.writeI32(limit); err != nil {
		return nil, err
	}
	if err := o.writeBool(reverse); err != nil {
		return nil, err
	}
	if err := o.writeI64(tenantId); err != nil {
		return nil, err
	}
	if err := o.writeI64(minTenantVersion); err != nil {
		return nil, err
	}
	return o.readResponse()
}

// SerializeCommitTransactionRequest sends a CommitTransactionRequest to the oracle.
func (o *Oracle) SerializeCommitTransactionRequest(
	readSnapshot int64,
	mutations []Mutation,
	readConflictRanges, writeConflictRanges []ConflictRange,
	flags uint32,
	hasDebugID bool, debugID []byte,
	hasCommitCostEstimation bool, commitCostEstimation []byte,
	hasTagSet bool, tagSet []byte,
	tenantId int64,
	idempotencyId []byte,
) ([]byte, error) {
	o.mu.Lock()
	defer o.mu.Unlock()

	if err := o.writeU8(typeCommitTransactionRequest); err != nil {
		return nil, err
	}
	if err := o.writeI64(readSnapshot); err != nil {
		return nil, err
	}

	// Mutations
	if err := o.writeU32(uint32(len(mutations))); err != nil {
		return nil, err
	}
	for _, m := range mutations {
		if err := o.writeU8(m.Type); err != nil {
			return nil, err
		}
		if err := o.writeBytes(m.Param1); err != nil {
			return nil, err
		}
		if err := o.writeBytes(m.Param2); err != nil {
			return nil, err
		}
	}

	// Read conflict ranges
	if err := o.writeU32(uint32(len(readConflictRanges))); err != nil {
		return nil, err
	}
	for _, cr := range readConflictRanges {
		if err := o.writeBytes(cr.Begin); err != nil {
			return nil, err
		}
		if err := o.writeBytes(cr.End); err != nil {
			return nil, err
		}
	}

	// Write conflict ranges
	if err := o.writeU32(uint32(len(writeConflictRanges))); err != nil {
		return nil, err
	}
	for _, cr := range writeConflictRanges {
		if err := o.writeBytes(cr.Begin); err != nil {
			return nil, err
		}
		if err := o.writeBytes(cr.End); err != nil {
			return nil, err
		}
	}

	if err := o.writeU32(flags); err != nil {
		return nil, err
	}
	if err := o.writeBool(hasDebugID); err != nil {
		return nil, err
	}
	if hasDebugID {
		if err := o.writeBytes(debugID); err != nil {
			return nil, err
		}
	}
	if err := o.writeBool(hasCommitCostEstimation); err != nil {
		return nil, err
	}
	if hasCommitCostEstimation {
		if err := o.writeBytes(commitCostEstimation); err != nil {
			return nil, err
		}
	}
	if err := o.writeBool(hasTagSet); err != nil {
		return nil, err
	}
	if hasTagSet {
		if err := o.writeBytes(tagSet); err != nil {
			return nil, err
		}
	}
	if err := o.writeI64(tenantId); err != nil {
		return nil, err
	}
	if err := o.writeBytes(idempotencyId); err != nil {
		return nil, err
	}

	return o.readResponse()
}

// SerializeOpenDatabaseCoordRequest sends an OpenDatabaseCoordRequest to the oracle.
func (o *Oracle) SerializeOpenDatabaseCoordRequest(
	issues, supportedVersions, traceLogGroup []byte,
	knownClientInfoID [16]byte,
	clusterKey, coordinators, hostnames []byte,
	internal bool,
) ([]byte, error) {
	o.mu.Lock()
	defer o.mu.Unlock()

	if err := o.writeU8(typeOpenDatabaseCoordRequest); err != nil {
		return nil, err
	}
	if err := o.writeBytes(issues); err != nil {
		return nil, err
	}
	if err := o.writeBytes(supportedVersions); err != nil {
		return nil, err
	}
	if err := o.writeBytes(traceLogGroup); err != nil {
		return nil, err
	}
	if err := o.writeUID(knownClientInfoID); err != nil {
		return nil, err
	}
	if err := o.writeBytes(clusterKey); err != nil {
		return nil, err
	}
	if err := o.writeBytes(coordinators); err != nil {
		return nil, err
	}
	if err := o.writeBytes(hostnames); err != nil {
		return nil, err
	}
	if err := o.writeBool(internal); err != nil {
		return nil, err
	}
	return o.readResponse()
}

// SerializeReadOptions sends a ReadOptions (carried inside a GetValueRequest with key="ro")
// to the oracle. Exercises RFC-117's Optional<Version> consistencyCheckStartVersion + the
// sibling Optional<UID> debugID + lockAware. The field order matches the C++ handleReadOptions.
func (o *Oracle) SerializeReadOptions(
	roType int32, cacheResult bool,
	hasDebugID bool, debugID [16]byte,
	hasCCSV bool, ccsv int64,
	lockAware bool,
) ([]byte, error) {
	o.mu.Lock()
	defer o.mu.Unlock()

	if err := o.writeU8(typeReadOptions); err != nil {
		return nil, err
	}
	if err := o.writeI32(roType); err != nil {
		return nil, err
	}
	if err := o.writeBool(cacheResult); err != nil {
		return nil, err
	}
	if err := o.writeBool(hasDebugID); err != nil {
		return nil, err
	}
	if hasDebugID {
		if err := o.writeUID(debugID); err != nil {
			return nil, err
		}
	}
	if err := o.writeBool(hasCCSV); err != nil {
		return nil, err
	}
	if hasCCSV {
		if err := o.writeI64(ccsv); err != nil {
			return nil, err
		}
	}
	if err := o.writeBool(lockAware); err != nil {
		return nil, err
	}
	return o.readResponse()
}
