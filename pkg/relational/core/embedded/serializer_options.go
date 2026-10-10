// Portions derived from FoundationDB Record Layer (StoreConfig.java,
// TypeContract.java, RecordLayerDatabase.java, TransformedRecordSerializer.java),
// Copyright 2015-2018 Apple Inc. and the FoundationDB project authors
// Copyright 2021-2024 Apple Inc. and the FoundationDB project authors
// Copyright 2021-2025 Apple Inc. and the FoundationDB project authors
// Licensed under the Apache License, Version 2.0; translated to Go and modified.

package embedded

import (
	"errors"
	"fmt"

	"fdb.dev/pkg/recordlayer"
	"fdb.dev/pkg/relational/api"
)

// defaultRelationalSerializer is Java's StoreConfig.DEFAULT_RELATIONAL_SERIALIZER
// (StoreConfig.java:54-59): the plain TransformedRecordSerializer, compressing
// at Deflater.DEFAULT_COMPRESSION, never encrypting, no write validation. A
// record too small to shrink is written as the clear prefix 02 and the union
// message, byte-equal to Java's.
var defaultRelationalSerializer = mustBuildSerializer(recordlayer.NewTransformedRecordSerializerBuilder().
	SetEncryptWhenSerializing(false).
	SetCompressWhenSerializing(true).
	SetCompressionLevel(recordlayer.DeflaterDefaultCompression).
	SetWriteValidationRatio(0))

func mustBuildSerializer(b *recordlayer.TransformedRecordSerializerBuilder) *recordlayer.TransformedRecordSerializer {
	s, err := b.Build()
	if err != nil {
		panic(fmt.Sprintf("the default relational serializer does not build: %v", err))
	}
	return s
}

// serializerFromOptions is Java's StoreConfig.serializerFromOptions
// (StoreConfig.java:130-148): the serializer a user store is opened with, read
// from the connection's options at every store open, as Java's
// RecordLayerDatabase.loadStore builds its StoreConfig from the options of the
// moment.
func serializerFromOptions(o *api.Options) (*recordlayer.TransformedRecordSerializer, error) {
	encrypted := optBool(o, api.OptEncryptWhenSerializing, false)
	compressed := optBool(o, api.OptCompressWhenSerializing, true)
	keyManager, err := keyManagerFromOptions(o)
	if err != nil {
		return nil, err
	}
	if !encrypted && compressed && keyManager == nil {
		return defaultRelationalSerializer, nil
	}
	b := recordlayer.NewTransformedRecordSerializerJCEBuilder().
		SetEncryptWhenSerializing(encrypted).
		SetCompressWhenSerializing(compressed).
		SetCompressionLevel(recordlayer.DeflaterDefaultCompression).
		SetWriteValidationRatio(0)
	if keyManager != nil {
		b.SetKeyManager(keyManager)
	} else if encrypted {
		return nil, api.NewError(api.ErrCodeUnsupportedOperation, "Key store not specified")
	}
	return b.Build()
}

// keyManagerFromOptions is Java's StoreConfig.keyManagerFromOptions
// (StoreConfig.java:150-173): no ENCRYPTION_KEY_STORE is no key manager; a key
// store that does not build is "problem with encryption options",
// UNSUPPORTED_OPERATION, caused by the builder's refusal.
func keyManagerFromOptions(o *api.Options) (recordlayer.SerializationKeyManager, error) {
	keyStore, err := optNullableString(o, api.OptEncryptionKeyStore)
	if err != nil || keyStore == nil {
		return nil, err
	}
	b := recordlayer.NewKeyStoreSerializationKeyManagerBuilder().SetKeyStoreFileName(*keyStore)
	entry, err := optNullableString(o, api.OptEncryptionKeyEntry)
	if err != nil {
		return nil, err
	}
	if entry != nil {
		b.SetDefaultKeyEntryAlias(*entry)
	}
	switch v := o.Get(api.OptEncryptionKeyEntryList).(type) {
	case nil:
	case []string:
		b.SetKeyEntryAliases(v)
	default:
		return nil, api.NewErrorf(api.ErrCodeInvalidParameter, "Option %s should be of type %s but is %T", api.OptEncryptionKeyEntryList, "[]string", v)
	}
	password, err := optNullableString(o, api.OptEncryptionKeyPassword)
	if err != nil {
		return nil, err
	}
	if password != nil {
		b.SetKeyStorePassword(*password)
	}
	km, err := b.Build()
	if err != nil {
		var argErr *recordlayer.RecordCoreArgumentError
		if errors.As(err, &argErr) {
			return nil, api.WrapError(api.ErrCodeUnsupportedOperation, "problem with encryption options", err)
		}
		return nil, err
	}
	return km, nil
}

// optNullableString reads one of Java's TypeContract.nullableStringType
// options: unset or set to null is nil.
func optNullableString(o *api.Options, name api.OptionName) (*string, error) {
	switch v := o.Get(name).(type) {
	case nil:
		return nil, nil
	case string:
		return &v, nil
	default:
		return nil, invalidOptionType(name, v)
	}
}

// invalidOptionType is the refusal Java's TypeContract.validate gives a value
// of the wrong type (TypeContract.java:72, INVALID_PARAMETER). Java refuses it
// when the option is set; Go's Options do not check a value's type then, so
// the reader refuses it, in Java's words with Go's type names.
func invalidOptionType(name api.OptionName, v any) error {
	return api.NewErrorf(api.ErrCodeInvalidParameter, "Option %s should be of type %s but is %T", name, "string", v)
}
