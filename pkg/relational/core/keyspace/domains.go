// Portions derived from FoundationDB Record Layer (
// RelationalKeyspaceProvider.java, KeySpaceDirectory.java, KeySpace.java,
// DirectoryLayerDirectory.java, and others),
// Copyright 2015-2018 Apple Inc. and the FoundationDB project authors
// Copyright 2021-2025 Apple Inc. and the FoundationDB project authors
// Licensed under the Apache License, Version 2.0; translated to Go and modified.

package keyspace

import (
	"strconv"
	"strings"
	"sync"

	"fdb.dev/pkg/relational/api"
)

// Java's RelationalKeyspaceProvider: one process-wide KeySpace whose root holds
// the system directory (__SYS) and every registered domain. A relational
// database path is /DOMAIN/DATABASE, and its domain must have been registered
// (registerDomainIfNotExists) by whoever runs the engine: Java's server, CLI
// and yaml-test runner register FRL, its embedded tests TEST, and the engine
// itself registers none. Registration is additive and idempotent; there is no
// way to unregister, as in Java.

// Names of the keyspace directories (RelationalKeyspaceProvider).
const (
	DBNameDir        = "dbName"
	SchemaDir        = "schema"
	DefaultSchemaDir = "defaultSchema"
	InterningLayer   = "__internedStrings"
	// InterningLayerValue is the interning directory's constant value.
	InterningLayerValue = "IL"
)

var domains struct {
	mu    sync.RWMutex
	names []string
}

// RegisterDomainIfNotExists adds a domain to the process's keyspace, Java's
// RelationalKeyspaceProvider.instance().registerDomainIfNotExists. Afterwards
// /name/DB is a valid database path for every connection of the process.
func RegisterDomainIfNotExists(name string) {
	domains.mu.Lock()
	defer domains.mu.Unlock()
	for _, n := range domains.names {
		if n == name {
			return
		}
	}
	domains.names = append(domains.names, name)
}

// RegisteredDomains returns the registered domains in registration order.
func RegisteredDomains() []string {
	domains.mu.RLock()
	defer domains.mu.RUnlock()
	return append([]string(nil), domains.names...)
}

type keyType int

const (
	keyNull keyType = iota
	keyString
	keyLong
)

// ksDirectory is the part of Java's KeySpaceDirectory the path matcher reads.
// A DirectoryLayerDirectory is LONG-typed but matched by its string name.
type ksDirectory struct {
	name           string
	keyType        keyType
	directoryLayer bool
	constant       any // nil: any value
	children       []*ksDirectory
}

// keySpaceRoot builds the root's directories: __SYS, then each domain in
// registration order (RelationalKeyspaceProvider's constructor and
// registerDomainIfNotExists).
func keySpaceRoot() []*ksDirectory {
	interned := func() *ksDirectory {
		return &ksDirectory{name: InterningLayer, keyType: keyString, constant: InterningLayerValue}
	}
	root := []*ksDirectory{{
		name: SysName, keyType: keyNull,
		children: []*ksDirectory{
			{name: SysName, keyType: keyNull, children: []*ksDirectory{
				{name: CatalogName, keyType: keyLong, constant: int64(0)},
			}},
			interned(),
		},
	}}
	for _, d := range RegisteredDomains() {
		root = append(root, &ksDirectory{
			name: d, keyType: keyLong, directoryLayer: true, constant: d,
			children: []*ksDirectory{
				interned(),
				{name: DBNameDir, keyType: keyLong, directoryLayer: true, children: []*ksDirectory{
					{name: DefaultSchemaDir, keyType: keyNull},
					{name: SchemaDir, keyType: keyLong, directoryLayer: true},
				}},
			},
		})
	}
	return root
}

// DatabasePath is a resolved database path: the system database, or a
// domain's database.
type DatabasePath struct {
	System   bool
	Domain   string
	Database string
}

// ToDatabasePath resolves a database URI path against the keyspace, Java's
// RelationalKeyspaceProvider.toDatabasePath over KeySpaceUtils.toKeySpacePath:
// the path must be /__SYS or /DOMAIN/DATABASE under a registered domain, and
// anything else is INVALID_PATH "<path> is an invalid database path".
func ToDatabasePath(uri string) (DatabasePath, error) {
	invalid := api.NewErrorf(api.ErrCodeInvalidPath, "<%s> is an invalid database path", uri)
	path := uri
	if len(path) < 1 {
		return DatabasePath{}, invalid
	}
	path = strings.TrimPrefix(path, "/")
	// Just __SYS names the system database, not its domain.
	if path == SysName {
		return DatabasePath{System: true}, nil
	}
	elems := javaSplit(path)
	if strings.HasSuffix(path, "/") {
		elems = append(elems, "")
	}
	matched, values, err := matchSubdirectories(keySpaceRoot(), uri, elems, 0)
	if err != nil {
		return DatabasePath{}, err
	}
	switch {
	case matched == nil:
		return DatabasePath{}, invalid
	case len(matched) == 2 && matched[0] == SysName && matched[1] == SysName:
		return DatabasePath{System: true}, nil
	case matched[len(matched)-1] == DBNameDir:
		return DatabasePath{Domain: values[0], Database: values[1]}, nil
	}
	return DatabasePath{}, invalid
}

// javaSplit is String.split("/"): trailing empty strings are removed, and a
// string without a separator is returned whole.
func javaSplit(s string) []string {
	parts := strings.Split(s, "/")
	if len(parts) == 1 {
		return parts
	}
	for len(parts) > 0 && parts[len(parts)-1] == "" {
		parts = parts[:len(parts)-1]
	}
	return parts
}

// matchSubdirectories is KeySpaceUtils.matchPathToSubdirectories: the one
// subdirectory the element at position matches, or nil; two is ambiguous.
func matchSubdirectories(dirs []*ksDirectory, uri string, elems []string, position int) ([]string, []string, error) {
	var names, values []string
	for _, d := range dirs {
		n, v, err := matchDirectory(d, uri, elems, position)
		if err != nil {
			return nil, nil, err
		}
		if n == nil {
			continue
		}
		if names != nil {
			return nil, nil, api.NewErrorf(api.ErrCodeInvalidPath, "<%s> is ambigous", uri)
		}
		names, values = n, v
	}
	return names, values, nil
}

// matchDirectory is KeySpaceUtils.matchPathToDirectory.
func matchDirectory(d *ksDirectory, uri string, elems []string, position int) ([]string, []string, error) {
	elem := elems[position]
	var value string
	switch {
	case d.keyType == keyNull:
		if elem != "" {
			return nil, nil, nil
		}
	case d.keyType == keyString || d.directoryLayer:
		if c, ok := d.constant.(string); ok && c != elem {
			return nil, nil, nil
		}
		if d.constant == nil && elem == "" {
			return nil, nil, nil
		}
		value = elem
	default:
		parsed, err := strconv.ParseInt(elem, 10, 64)
		if err != nil {
			return nil, nil, nil
		}
		if c, ok := d.constant.(int64); ok && c != parsed {
			return nil, nil, nil
		}
		value = elem
	}
	names, values := []string{d.name}, []string{value}
	if len(d.children) == 0 {
		if position == len(elems)-1 {
			return names, values, nil
		}
		return nil, nil, nil
	}
	if position+1 == len(elems) {
		return names, values, nil
	}
	n, v, err := matchSubdirectories(d.children, uri, elems, position+1)
	if err != nil || n == nil {
		return nil, nil, err
	}
	return append(names, n...), append(values, v...), nil
}
