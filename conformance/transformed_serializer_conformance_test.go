//go:build bazelrunfiles

package conformance_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/google/uuid"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"google.golang.org/protobuf/proto"

	"fdb.dev/gen"
	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	"fdb.dev/pkg/recordlayer"
)

// Records written through Java's TransformedRecordSerializerJCE, clear,
// compressed, encrypted with an AES key (CipherPool's default cipher) and
// compressed then encrypted, read by Go; and the same four written by Go's
// TransformedRecordSerializer, read by Java. The stored prefix is asserted on
// both sides; the bodies are compared by the records they hold (deflate output
// is not byte-identical across implementations, and the IV is random).
// RFC-257 serializer-design.md.
var _ = Describe("RFC-257 TransformedRecordSerializer records are read and written as Java's", func() {
	It("reads what Java writes and writes what Java reads, in each transformation", func() {
		ctx := context.Background()
		env, err := SetupTenantEnvironment(ctx, sharedContainer, "tser_"+uuid.New().String())
		Expect(err).NotTo(HaveOccurred())
		defer func() { _ = env.Cleanup(ctx) }()
		java := NewJavaInvoker()
		clusterFile, err := sharedContainer.ClusterFile(ctx)
		Expect(err).NotTo(HaveOccurred())

		key := make([]byte, 16)
		_, err = rand.Read(key)
		Expect(err).NotTo(HaveOccurred())
		keyHex := hex.EncodeToString(key)
		sonnet := strings.Repeat("What's in the brain that ink may character which hath not figured to thee my true spirit? ", 4)
		order := func(id int64) *gen.Order {
			return &gen.Order{OrderId: proto.Int64(id), Price: proto.Int32(int32(id)), Tags: []string{sonnet, "b"}} //nolint:gosec
		}
		goSerializer := func(compress, encrypt bool) *recordlayer.TransformedRecordSerializer {
			s, err := recordlayer.NewTransformedRecordSerializerJCEBuilder().
				SetCompressWhenSerializing(compress).SetEncryptWhenSerializing(encrypt).
				SetEncryptionKey(key).SetWriteValidationRatio(1).Build()
			Expect(err).NotTo(HaveOccurred())
			return s
		}
		rawValue := func(id int64) []byte {
			var raw []byte
			_, err := env.RecordDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
				rng := env.Keyspace.Sub(recordlayer.RecordKey, id)
				kvs, err := rtx.Transaction().GetRange(rng, fdb.RangeOptions{}).GetSliceWithError()
				if err != nil {
					return nil, err
				}
				if len(kvs) == 0 {
					// An unsplit record may be stored at the bare key.
					v, err := rtx.Transaction().Get(fdb.Key(env.Keyspace.Sub(recordlayer.RecordKey).Pack(tuple.Tuple{id}))).Get()
					raw = v
					return nil, err
				}
				Expect(kvs).To(HaveLen(1), "record %d is split", id)
				raw = kvs[0].Value
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
			return raw
		}
		goLoad := func(id int64, s *recordlayer.TransformedRecordSerializer) (proto.Message, error) {
			var got proto.Message
			_, err := env.RecordDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
				b := recordlayer.NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(env.MetaData).SetSubspace(env.Keyspace)
				if s != nil {
					b = b.SetSerializer(s)
				}
				store, err := b.CreateOrOpen()
				if err != nil {
					return nil, err
				}
				rec, err := store.LoadRecord(tuple.Tuple{id})
				if err != nil {
					return nil, err
				}
				if rec == nil {
					return nil, fmt.Errorf("record %d not found", id)
				}
				got = rec.Record
				return nil, nil
			})
			return got, err
		}

		for i, c := range []struct {
			name              string
			compress, encrypt bool
			prefix            byte
		}{
			{"clear", false, false, 0x02},
			{"compressed", true, false, 0x04},
			{"encrypted", false, true, 0x01},
			{"compressed then encrypted", true, true, 0x05},
		} {
			By(c.name)
			// Java writes, Go reads.
			javaID := int64(100 + i)
			Expect(java.InvokeAs(ctx, "saveOrderTransformedJava", map[string]any{
				"clusterFile": clusterFile, "subspace": BytesToIntArray(env.Keyspace.Bytes()), "order": order(javaID),
				"tenantName": env.TenantName, "compress": c.compress, "encrypt": c.encrypt, "keyHex": keyHex,
			}, nil)).To(Succeed())
			raw := rawValue(javaID)
			GinkgoWriter.Printf("TSER %s java-written %d bytes, prefix %02x\n", c.name, len(raw), raw[0])
			Expect(raw[0]).To(Equal(c.prefix), "%s: Java's stored prefix", c.name)
			got, err := goLoad(javaID, goSerializer(false, false))
			Expect(err).NotTo(HaveOccurred(), "%s: Go reads what Java wrote", c.name)
			Expect(proto.Equal(got, order(javaID))).To(BeTrue(), "%s: Go read %v", c.name, got)
			// A store writing with no serializer reads every prefix but cannot
			// decrypt.
			got, err = goLoad(javaID, nil)
			if c.encrypt {
				Expect(err).To(MatchError(ContainSubstring("this serializer cannot decrypt")), c.name)
			} else {
				Expect(err).NotTo(HaveOccurred(), c.name)
				Expect(proto.Equal(got, order(javaID))).To(BeTrue(), c.name)
			}

			// Go writes, Java reads.
			goID := int64(200 + i)
			_, err = env.RecordDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
				store, err := recordlayer.NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(env.MetaData).
					SetSubspace(env.Keyspace).SetSerializer(goSerializer(c.compress, c.encrypt)).CreateOrOpen()
				if err != nil {
					return nil, err
				}
				_, err = store.SaveRecord(order(goID))
				return nil, err
			})
			Expect(err).NotTo(HaveOccurred(), c.name)
			raw = rawValue(goID)
			GinkgoWriter.Printf("TSER %s go-written %d bytes, prefix %02x\n", c.name, len(raw), raw[0])
			Expect(raw[0]).To(Equal(c.prefix), "%s: Go's stored prefix", c.name)
			var javaOrder gen.Order
			Expect(java.InvokeAs(ctx, "loadOrderTransformedJava", map[string]any{
				"clusterFile": clusterFile, "subspace": BytesToIntArray(env.Keyspace.Bytes()), "orderID": goID,
				"tenantName": env.TenantName, "keyHex": keyHex,
			}, &javaOrder)).To(Succeed(), "%s: Java reads what Go wrote", c.name)
			Expect(proto.Equal(&javaOrder, order(goID))).To(BeTrue(), "%s: Java read %v", c.name, &javaOrder)
		}
	})

	// A PKCS12 key store the JDK writes (KeyStore.getInstance("PKCS12"), the
	// JDK 12+ default protection) holding two AES keys, one of them under a
	// mixed-case alias the JDK stores folded, and a password of spaces and
	// symbols, which reaches both the PKCS #12 MAC KDF (UTF-16) and PBKDF2.
	// Java encrypts through KeyStoreSerializationKeyManager under the second
	// key and Go decrypts through its reader of the same file; Go encrypts
	// under the second key and Java decrypts. The prefix carries the key
	// number (1). A password outside printable ASCII is refused by the JDK's
	// PBEKey, which Go's GetKey matches (checkPBEPassword); the first step
	// measures the JDK's refusal. Before the round trip, Go reads every kind
	// of key store the JDK writes (no key store file may be committed, so the
	// JDK writes each one here): the JDK 12+ default, the empty password, a
	// key password apart from the store's, the pre-12 protection, and a JCEKS
	// store, which Go refuses (declared). The algorithms each file holds are
	// checked in its bytes, so a JDK that ignored the legacy properties could
	// not pass as legacy.
	It("reads and writes records encrypted through a key store the JDK writes", func() {
		ctx := context.Background()
		env, err := SetupTenantEnvironment(ctx, sharedContainer, "tserks_"+uuid.New().String())
		Expect(err).NotTo(HaveOccurred())
		defer func() { _ = env.Cleanup(ctx) }()
		java := NewJavaInvoker()
		clusterFile, err := sharedContainer.ClusterFile(ctx)
		Expect(err).NotTo(HaveOccurred())

		dir, err := os.MkdirTemp("", "tserks")
		Expect(err).NotTo(HaveOccurred())
		defer os.RemoveAll(dir)
		keyStore := filepath.Join(dir, "keys.p12")
		const password = " p@ss~w0rd {\\|} "
		aliases := []string{"Key-A", "key-b"}
		keysHex := make([]string, 2)
		for i, n := range []int{16, 32} {
			k := make([]byte, n)
			_, err = rand.Read(k)
			Expect(err).NotTo(HaveOccurred())
			keysHex[i] = hex.EncodeToString(k)
		}
		writeKeyStoreOf := func(path, storeType, storePassword, entryPassword string, legacy bool, keyAlgorithm string) error {
			return java.InvokeAs(ctx, "writeKeyStoreJava", map[string]any{
				"path": path, "storeType": storeType, "storePassword": storePassword, "entryPassword": entryPassword,
				"legacy": legacy, "aliases": aliases, "keysHex": keysHex, "keyAlgorithm": keyAlgorithm,
			}, nil)
		}
		writeKeyStore := func(path, storeType, storePassword, entryPassword string, legacy bool) error {
			return writeKeyStoreOf(path, storeType, storePassword, entryPassword, legacy, "AES")
		}
		err = writeKeyStore(filepath.Join(dir, "refused.p12"), "PKCS12", "pässwörd", "pässwörd", false)
		Expect(err).To(MatchError(ContainSubstring("Password is not ASCII")), "the JDK's PBEKey refuses a non-ASCII key password")
		// The store password alone outside printable ASCII: the JDK's MAC is
		// derived through the same PBEKey, so it refuses the store too (Go's
		// loader refuses such a store, checkPBEPassword on the MAC).
		storeErr := writeKeyStore(filepath.Join(dir, "refused-store.p12"), "PKCS12", "pässwörd", "storepass", false)
		Expect(storeErr).To(MatchError(ContainSubstring("Password is not ASCII")), "the JDK refuses a non-ASCII store password")

		// DER-encoded OIDs: PBES2, pbeWithSHAAnd3-KeyTripleDES-CBC, SHA-1, SHA-256.
		oidPBES2 := []byte{0x06, 0x09, 0x2a, 0x86, 0x48, 0x86, 0xf7, 0x0d, 0x01, 0x05, 0x0d}
		oid3DES := []byte{0x06, 0x0a, 0x2a, 0x86, 0x48, 0x86, 0xf7, 0x0d, 0x01, 0x0c, 0x01, 0x03}
		oidSHA1 := []byte{0x06, 0x05, 0x2b, 0x0e, 0x03, 0x02, 0x1a}
		oidSHA256 := []byte{0x06, 0x09, 0x60, 0x86, 0x48, 0x01, 0x65, 0x03, 0x04, 0x02, 0x01}
		for i, v := range []struct {
			name, storeType, storePassword, entryPassword string
			legacy                                        bool
			holds, lacks                                  [][]byte
			refused                                       string
		}{
			{"PBES2, the JDK 12+ default", "PKCS12", password, password, false, [][]byte{oidPBES2, oidSHA256}, [][]byte{oid3DES}, ""},
			{"the empty password", "PKCS12", "", "", false, [][]byte{oidPBES2}, nil, ""},
			{"a key password apart from the store's", "PKCS12", "storepass", "entrypass", false, [][]byte{oidPBES2}, nil, ""},
			{"the pre-12 protection: 3DES keys, an HmacPBESHA1 MAC", "PKCS12", "storepass", "storepass", true, [][]byte{oid3DES, oidSHA1}, [][]byte{oidPBES2, oidSHA256}, ""},
			{"JCEKS", "JCEKS", "storepass", "storepass", false, nil, nil, "JCEKS"},
		} {
			path := filepath.Join(dir, fmt.Sprintf("variant-%d", i))
			Expect(writeKeyStore(path, v.storeType, v.storePassword, v.entryPassword, v.legacy)).To(Succeed(), v.name)
			raw, err := os.ReadFile(path)
			Expect(err).NotTo(HaveOccurred())
			for _, oid := range v.holds {
				Expect(bytes.Contains(raw, oid)).To(BeTrue(), "%s: the file lacks OID %x", v.name, oid)
			}
			for _, oid := range v.lacks {
				Expect(bytes.Contains(raw, oid)).To(BeFalse(), "%s: the file holds OID %x", v.name, oid)
			}
			b := recordlayer.NewKeyStoreSerializationKeyManagerBuilder().SetKeyStoreFileName(path).SetKeyEntryAliases(aliases)
			if v.storePassword != "" {
				b.SetKeyStorePassword(v.storePassword) // unset, Java's builder and Go's default to ""
			}
			if v.entryPassword != v.storePassword {
				b.SetKeyEntryPassword(v.entryPassword)
			}
			km, err := b.Build()
			if v.refused != "" {
				var argErr *recordlayer.RecordCoreArgumentError
				Expect(errors.As(err, &argErr)).To(BeTrue(), "%s: %v", v.name, err)
				Expect(argErr.Message).To(Equal("Key store loading failed"), v.name)
				Expect(argErr.Cause).To(MatchError(ContainSubstring(v.refused)), v.name)
				continue
			}
			Expect(err).NotTo(HaveOccurred(), "%s: Go loads the key store the JDK wrote", v.name)
			for k := range aliases {
				key, err := km.GetKey(int32(k)) //nolint:gosec
				Expect(err).NotTo(HaveOccurred(), "%s: key %q", v.name, aliases[k])
				Expect(hex.EncodeToString(key)).To(Equal(keysHex[k]), "%s: Go reads key %q as the JDK stored it", v.name, aliases[k])
			}
			GinkgoWriter.Printf("TSERKS variant %q: %d bytes, both keys read\n", v.name, len(raw))
		}

		// The JDK writes a store under "\x00", which its PBEKey admits as the
		// empty password with no NUL terminator. Go reads it under "\x00", and
		// under the empty password by the JDK's RetryWithZero (around the MAC
		// check and each key's decryption).
		nulStore := filepath.Join(dir, "nul.p12")
		Expect(writeKeyStore(nulStore, "PKCS12", "\x00", "\x00", false)).To(Succeed(), "the JDK writes a store under \\0")
		for _, pw := range []string{"\x00", ""} {
			km, err := recordlayer.NewKeyStoreSerializationKeyManagerBuilder().SetKeyStoreFileName(nulStore).
				SetKeyStorePassword(pw).SetKeyEntryAliases(aliases).Build()
			Expect(err).NotTo(HaveOccurred(), "Go loads the JDK's \\0 store under %q", pw)
			for k := range aliases {
				key, err := km.GetKey(int32(k)) //nolint:gosec
				Expect(err).NotTo(HaveOccurred(), "the \\0 store under %q, key %q", pw, aliases[k])
				Expect(hex.EncodeToString(key)).To(Equal(keysHex[k]), "the \\0 store under %q, key %q", pw, aliases[k])
			}
			GinkgoWriter.Printf("TSERKS the JDK's \\0 store read under %q: both keys\n", pw)
		}

		Expect(writeKeyStore(keyStore, "PKCS12", password, password, false)).To(Succeed())

		goSerializer := func(compress bool, defaultAlias string) *recordlayer.TransformedRecordSerializer {
			km, err := recordlayer.NewKeyStoreSerializationKeyManagerBuilder().SetKeyStoreFileName(keyStore).
				SetKeyStorePassword(password).SetKeyEntryAliases(aliases).SetDefaultKeyEntryAlias(defaultAlias).Build()
			Expect(err).NotTo(HaveOccurred())
			for i := range aliases {
				key, err := km.GetKey(int32(i)) //nolint:gosec
				Expect(err).NotTo(HaveOccurred())
				Expect(hex.EncodeToString(key)).To(Equal(keysHex[i]), "Go reads key %q as the JDK stored it", aliases[i])
			}
			s, err := recordlayer.NewTransformedRecordSerializerJCEBuilder().SetCompressWhenSerializing(compress).
				SetEncryptWhenSerializing(true).SetKeyManager(km).SetWriteValidationRatio(1).Build()
			Expect(err).NotTo(HaveOccurred())
			return s
		}
		order := func(id int64) *gen.Order {
			return &gen.Order{OrderId: proto.Int64(id), Price: proto.Int32(int32(id)), Tags: []string{strings.Repeat("sonnet ", 40)}} //nolint:gosec
		}
		rawPrefix := func(id int64) byte {
			var raw []byte
			_, err := env.RecordDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
				kvs, err := rtx.Transaction().GetRange(env.Keyspace.Sub(recordlayer.RecordKey, id), fdb.RangeOptions{}).GetSliceWithError()
				if err != nil {
					return nil, err
				}
				if len(kvs) == 0 {
					v, err := rtx.Transaction().Get(fdb.Key(env.Keyspace.Sub(recordlayer.RecordKey).Pack(tuple.Tuple{id}))).Get()
					raw = v
					return nil, err
				}
				Expect(kvs).To(HaveLen(1), "record %d is split", id)
				raw = kvs[0].Value
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(raw).NotTo(BeEmpty(), "record %d stored", id)
			return raw[0]
		}

		// Java writes under key-b (key number 1), compressed then encrypted: 1<<3|5.
		Expect(java.InvokeAs(ctx, "saveOrderKeyStoreJava", map[string]any{
			"clusterFile": clusterFile, "subspace": BytesToIntArray(env.Keyspace.Bytes()), "order": order(300),
			"tenantName": env.TenantName, "compress": true, "keyStore": keyStore, "password": password,
			"aliases": aliases, "defaultAlias": "key-b",
		}, nil)).To(Succeed())
		Expect(rawPrefix(300)).To(Equal(byte(0x0d)), "Java's prefix: compressed then encrypted, key 1")
		var got proto.Message
		_, err = env.RecordDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
			// A Go manager whose default is the OTHER key still reads key 1: the prefix names it.
			store, err := recordlayer.NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(env.MetaData).
				SetSubspace(env.Keyspace).SetSerializer(goSerializer(false, "Key-A")).CreateOrOpen()
			if err != nil {
				return nil, err
			}
			rec, err := store.LoadRecord(tuple.Tuple{int64(300)})
			if err != nil || rec == nil {
				return nil, fmt.Errorf("load 300: %v, %v", rec, err)
			}
			got = rec.Record
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred(), "Go decrypts what Java encrypted through the key store")
		Expect(proto.Equal(got, order(300))).To(BeTrue(), "Go read %v", got)

		// Go writes under key-b, encrypted only: 1<<3|1.
		_, err = env.RecordDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
			store, err := recordlayer.NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(env.MetaData).
				SetSubspace(env.Keyspace).SetSerializer(goSerializer(false, "key-b")).CreateOrOpen()
			if err != nil {
				return nil, err
			}
			_, err = store.SaveRecord(order(301))
			return nil, err
		})
		Expect(err).NotTo(HaveOccurred())
		Expect(rawPrefix(301)).To(Equal(byte(0x09)), "Go's prefix: encrypted, key 1")
		var javaOrder gen.Order
		Expect(java.InvokeAs(ctx, "loadOrderKeyStoreJava", map[string]any{
			"clusterFile": clusterFile, "subspace": BytesToIntArray(env.Keyspace.Bytes()), "orderID": int64(301),
			"tenantName": env.TenantName, "keyStore": keyStore, "password": password,
			"aliases": aliases, "defaultAlias": "Key-A",
		}, &javaOrder)).To(Succeed(), "Java decrypts what Go encrypted through the key store")
		Expect(proto.Equal(&javaOrder, order(301))).To(BeTrue(), "Java read %v", &javaOrder)
		GinkgoWriter.Printf("TSERKS java-written prefix %02x, go-written prefix %02x, both read across\n", rawPrefix(300), rawPrefix(301))

		// A secret key of another algorithm (HmacSHA256, 32 bytes): Java's AES
		// cipher refuses it when initialized, so neither engine encrypts with it.
		hmacStore := filepath.Join(dir, "hmac.p12")
		Expect(writeKeyStoreOf(hmacStore, "PKCS12", "storepass", "storepass", false, "HmacSHA256")).To(Succeed())
		javaErr := java.InvokeAs(ctx, "saveOrderKeyStoreJava", map[string]any{
			"clusterFile": clusterFile, "subspace": BytesToIntArray(env.Keyspace.Bytes()), "order": order(302),
			"tenantName": env.TenantName, "compress": false, "keyStore": hmacStore, "password": "storepass",
			"aliases": aliases, "defaultAlias": "key-b",
		}, nil)
		Expect(javaErr).To(MatchError(ContainSubstring("Wrong algorithm: AES or Rijndael required")), "Java refuses a non-AES key")
		hmacKM, err := recordlayer.NewKeyStoreSerializationKeyManagerBuilder().SetKeyStoreFileName(hmacStore).
			SetKeyStorePassword("storepass").SetKeyEntryAliases(aliases).SetDefaultKeyEntryAlias("key-b").Build()
		Expect(err).NotTo(HaveOccurred())
		hmacSer, err := recordlayer.NewTransformedRecordSerializerJCEBuilder().SetEncryptWhenSerializing(true).SetKeyManager(hmacKM).Build()
		Expect(err).NotTo(HaveOccurred())
		_, goErr := env.RecordDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
			store, err := recordlayer.NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(env.MetaData).
				SetSubspace(env.Keyspace).SetSerializer(hmacSer).CreateOrOpen()
			if err != nil {
				return nil, err
			}
			_, err = store.SaveRecord(order(302))
			return nil, err
		})
		Expect(goErr).To(MatchError(ContainSubstring("encryption error: Wrong algorithm: AES or Rijndael required")), "Go refuses a non-AES key")
	})
})
