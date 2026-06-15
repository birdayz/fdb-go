package bench

import (
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"os"
	"strings"
	"testing"

	cgofdb "github.com/apple/foundationdb/bindings/go/src/fdb"
	gofdb "github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/fdb"
)

// Tenant versionstamp-offset differential vs libfdb_c — closes the TODO.md follow-up
// left open by RFC-063 (the versionstamp differential covered the NON-tenant path only).
//
// When a transaction is tenant-scoped, the commit path prepends the tenant's 8-byte
// big-endian prefix to every mutation key. For SetVersionstampedKey the 4-byte LE offset
// suffix (where the server writes the 10-byte stamp) must be bumped by the prefix length,
// or the stamp lands in the wrong place — corrupting the prefix (key escapes the tenant)
// or shifting the surrounding user bytes. C++ applyTenantPrefix does exactly this:
//
//	} else if (m.type == MutationRef::SetVersionstampedKey) {
//	    uint8_t* key = mutateString(param1);
//	    int* offset = reinterpret_cast<int*>(&key[param1.size() - 4]);
//	    *offset += tenantPrefix.size();          // NativeAPI.actor.cpp:6533-6536
//	}
//
// Go mirrors it in buildCommitTransactionRequest (commitpath.go: `off += 8`). For
// SetVersionstampedValue the VALUE is NOT prefixed (only the key is), so its offset must
// be left UNTOUCHED — both clients must agree on that asymmetry too.
//
// Strategy (mirrors RFC-063 runVSKeyCase, rerouted through tenant transactions): both
// clients open the SAME shared tenant (one prefix), write the same template under their
// own isolation sub-prefix, read it back THROUGH the tenant (prefix stripped → the stamp
// sits at the user offset), mask the 10-byte stamp, and byte-compare the structure
// go-vs-cgo. A mis-adjusted offset shifts the stamp, breaking the masked compare or losing
// the key entirely. We additionally RAW-read the full stored key (tenant prefix included)
// and assert the stamp sits at exactly prefix.size()+userOffset — a direct check of the
// +8 mechanism, not just go-vs-cgo parity.

// setupSharedTenant creates a uniquely-named tenant (via cgoClient, so it is visible to
// both clients on the shared cluster) and opens it on both. Returns the go and cgo tenant
// handles plus the 8-byte big-endian tenant prefix (derived from the go handle's ID — the
// same prefix libfdb_c computes, since the tenant ID is cluster state). Registers cleanup
// that empties the tenant keyspace and deletes the tenant.
func setupSharedTenant(t *testing.T, label string) (gofdb.Tenant, cgofdb.Tenant, []byte) {
	t.Helper()
	name := fmt.Sprintf("difftenant_%d_%s", os.Getpid(), label)
	nameB := []byte(name)

	if err := cgoClient.CreateTenant(cgofdb.Key(nameB)); err != nil {
		t.Fatalf("CreateTenant(%q): %v", name, err)
	}
	t.Cleanup(func() { clearAndDeleteTenant(nameB) })

	goT, err := goClient.OpenTenant(gofdb.Key(nameB))
	if err != nil {
		t.Fatalf("go OpenTenant(%q): %v", name, err)
	}
	cgoT, err := cgoClient.OpenTenant(cgofdb.Key(nameB))
	if err != nil {
		t.Fatalf("cgo OpenTenant(%q): %v", name, err)
	}

	prefix := make([]byte, 8)
	binary.BigEndian.PutUint64(prefix, uint64(goT.ID()))
	return goT, cgoT, prefix
}

// tenantRawScan reads the full (prefix-included) keyspace of a tenant via a NON-tenant,
// RAW_ACCESS cgo transaction — the only way to observe the materialized tenant prefix and
// the absolute stamp position. Returns the raw key/value pairs sorted by key.
func tenantRawScan(t *testing.T, prefix []byte) []cgofdb.KeyValue {
	t.Helper()
	end, err := gofdb.Strinc(prefix) // [prefix, strinc(prefix)) covers EVERY key under the prefix, incl. 0xff first bytes
	if err != nil {
		t.Fatalf("strinc tenant prefix: %v", err)
	}
	r, err := cgoClient.Transact(func(tx cgofdb.Transaction) (any, error) {
		if err := tx.Options().SetRawAccess(); err != nil {
			return nil, err
		}
		return tx.GetRange(cgofdb.KeyRange{Begin: cgofdb.Key(prefix), End: cgofdb.Key(end)}, cgofdb.RangeOptions{}).GetSliceWithError()
	})
	if err != nil {
		t.Fatalf("raw tenant scan: %v", err)
	}
	return r.([]cgofdb.KeyValue)
}

// rawKeyWithSub finds the single raw key under prefix+sub in scan and returns it.
func rawKeyWithSub(t *testing.T, scan []cgofdb.KeyValue, prefix, sub []byte) []byte {
	t.Helper()
	want := append(append([]byte{}, prefix...), sub...)
	for _, kv := range scan {
		if bytes.HasPrefix(kv.Key, want) {
			return kv.Key
		}
	}
	t.Fatalf("raw scan: no key under prefix+%q (got %d keys)", sub, len(scan))
	return nil
}

func runTenantVSKeyCase(t *testing.T, c vsKeyCase) {
	t.Helper()
	ns := strings.ReplaceAll(t.Name(), "/", "_")
	goT, cgoT, prefix := setupSharedTenant(t, ns)

	// masked writes the template under sub-prefix `iso` inside the tenant, reads it back
	// THROUGH the tenant (prefix stripped), and returns the masked hex of (logical+stamp+
	// suffix). Same shape as RFC-063 runVSKeyCase but tenant-scoped.
	masked := func(iso string, write func(template []byte), scan func(begin, end []byte) ([]byte, bool)) string {
		isoB := []byte(iso)
		stampPos := len(isoB) + len(c.logical)
		data := make([]byte, 0, stampPos+vsStampLen+len(c.suffix))
		data = append(data, isoB...)
		data = append(data, c.logical...)
		data = append(data, make([]byte, vsStampLen)...) // placeholder
		data = append(data, c.suffix...)
		write(vsOperand(data, stampPos))
		matKey, ok := scan(isoB, append(append([]byte{}, isoB...), 0xff))
		if !ok {
			t.Fatalf("%s: materialized key not found under tenant sub-prefix %q", c.name, iso)
		}
		if !bytes.HasPrefix(matKey, isoB) {
			t.Fatalf("%s: read-back key %x lost its sub-prefix %q — tenant offset adjustment shifted it", c.name, matKey, iso)
		}
		rest := matKey[len(isoB):] // logical + stamp + suffix (sub-prefix stripped; tenant prefix already stripped on read-back)
		m, nonZero := maskStamp(rest, len(c.logical))
		if !nonZero {
			t.Fatalf("%s: stamp region all-zero at offset %d — tenant +8 adjustment placed it elsewhere", c.name, len(c.logical))
		}
		return hex.EncodeToString(m)
	}

	goMasked := masked("g_", func(template []byte) {
		if _, err := goT.Transact(func(txw gofdb.WritableTransaction) (any, error) {
			tx := txw.(gofdb.Transaction)
			tx.SetVersionstampedKey(gofdb.Key(template), []byte("v"))
			return nil, nil
		}); err != nil {
			t.Fatalf("go %s tenant write: %v", c.name, err)
		}
	}, func(begin, end []byte) ([]byte, bool) {
		r, err := goT.Transact(func(txw gofdb.WritableTransaction) (any, error) {
			tx := txw.(gofdb.Transaction)
			return tx.GetRange(gofdb.KeyRange{Begin: gofdb.Key(begin), End: gofdb.Key(end)}, gofdb.RangeOptions{Limit: 1}).GetSliceWithError()
		})
		if err != nil {
			t.Fatalf("go %s tenant scan: %v", c.name, err)
		}
		kvs := r.([]gofdb.KeyValue)
		if len(kvs) == 0 {
			return nil, false
		}
		return kvs[0].Key, true
	})

	cgoMasked := masked("c_", func(template []byte) {
		if _, err := cgoT.Transact(func(tx cgofdb.Transaction) (any, error) {
			tx.SetVersionstampedKey(cgofdb.Key(template), []byte("v"))
			return nil, nil
		}); err != nil {
			t.Fatalf("cgo %s tenant write: %v", c.name, err)
		}
	}, func(begin, end []byte) ([]byte, bool) {
		r, err := cgoT.Transact(func(tx cgofdb.Transaction) (any, error) {
			return tx.GetRange(cgofdb.KeyRange{Begin: cgofdb.Key(begin), End: cgofdb.Key(end)}, cgofdb.RangeOptions{Limit: 1}).GetSliceWithError()
		})
		if err != nil {
			t.Fatalf("cgo %s tenant scan: %v", c.name, err)
		}
		kvs := r.([]cgofdb.KeyValue)
		if len(kvs) == 0 {
			return nil, false
		}
		return kvs[0].Key, true
	})

	if goMasked != cgoMasked {
		t.Fatalf("%s: masked tenant read-back key differs: go=%s cgo=%s", c.name, goMasked, cgoMasked)
	}

	// Direct +8 mechanism check: raw-read the FULL stored keys (tenant prefix included) and
	// assert the stamp sits at prefix.size()+sub+logical on BOTH writers. A wrong adjustment
	// (e.g. +7, or none) would put the stamp elsewhere in the full key — the masked-at-
	// expected-position check fails even though the read-back parity above could be fooled
	// by a symmetric bug (it cannot be — cgo is the oracle — but this pins the absolute
	// mechanism the TODO calls for).
	scan := tenantRawScan(t, prefix)
	for _, sub := range []string{"g_", "c_"} {
		subB := []byte(sub)
		fk := rawKeyWithSub(t, scan, prefix, subB)
		if !bytes.HasPrefix(fk, prefix) {
			t.Fatalf("%s: raw key %x missing tenant prefix %x", c.name, fk, prefix)
		}
		wantPos := len(prefix) + len(subB) + len(c.logical)
		if _, nonZero := maskStamp(fk, wantPos); !nonZero {
			t.Fatalf("%s: raw key %x has no stamp at absolute offset %d (= prefixLen %d + sub %d + logical %d) — tenant +%d offset adjustment wrong",
				c.name, fk, wantPos, len(prefix), len(subB), len(c.logical), len(prefix))
		}
	}
}

// TestDifferential_TenantCrossClientCRUD pins the cross-client tenant-metadata interop the
// versionstamp differential first exposed. The tenant nameIndex and lastId are stored as
// TupleCodec<int64_t> (minimal-width); a Go client sharing a cluster with libfdb_c/Java MUST
// read and write them interoperably. Before the minimal-width tuple-int fix:
//   - go.OpenTenant on a libfdb_c-created tenant failed ("decode tenant nameIndex: expected
//     9 bytes, got 2" — libfdb_c writes 2 bytes for a small ID),
//   - go.CreateTenant failed reading the lastId libfdb_c had just written.
//
// This is the project's hard line: Go and C/Java apps share a cluster and must see the same
// tenants. Each leg writes through one client and reads through the OTHER — identical bytes
// prove the tenant prefix (hence the decoded ID) agrees across clients.
func TestDifferential_TenantCrossClientCRUD(t *testing.T) {
	t.Parallel()
	ns := strings.ReplaceAll(t.Name(), "/", "_")
	cName := []byte(fmt.Sprintf("xclient_c_%d_%s", os.Getpid(), ns))
	gName := []byte(fmt.Sprintf("xclient_g_%d_%s", os.Getpid(), ns))

	// cgo creates one tenant; go creates another. go.CreateTenant must read the lastId that
	// cgo just wrote (minimal-width) to allocate the next ID — a pre-fix failure point.
	if err := cgoClient.CreateTenant(cgofdb.Key(cName)); err != nil {
		t.Fatalf("cgo CreateTenant: %v", err)
	}
	t.Cleanup(func() { clearAndDeleteTenant(cName) })
	if err := goClient.CreateTenant(gofdb.Key(gName)); err != nil {
		t.Fatalf("go CreateTenant (must decode cgo-written lastId): %v", err)
	}
	t.Cleanup(func() { clearAndDeleteTenant(gName) })

	key := gofdb.Key("k")

	// Leg 1: go opens the cgo-created tenant (the original OpenTenant failure), writes; cgo
	// reads it back through the same tenant name.
	goOnC, err := goClient.OpenTenant(gofdb.Key(cName))
	if err != nil {
		t.Fatalf("go OpenTenant(cgo-created tenant): %v", err)
	}
	if _, err := goOnC.Transact(func(txw gofdb.WritableTransaction) (any, error) {
		txw.Set(key, []byte("from-go"))
		return nil, nil
	}); err != nil {
		t.Fatalf("go write into cgo-created tenant: %v", err)
	}
	cgoOnC, err := cgoClient.OpenTenant(cgofdb.Key(cName))
	if err != nil {
		t.Fatalf("cgo OpenTenant(cgo-created tenant): %v", err)
	}
	if got := cgoTenantGet(t, cgoOnC, "k"); string(got) != "from-go" {
		t.Fatalf("cgo read of go's write in shared tenant = %q, want %q (prefix/ID disagree across clients)", got, "from-go")
	}

	// Leg 2: cgo opens the go-created tenant, writes; go reads it back.
	cgoOnG, err := cgoClient.OpenTenant(cgofdb.Key(gName))
	if err != nil {
		t.Fatalf("cgo OpenTenant(go-created tenant): %v", err)
	}
	if _, err := cgoOnG.Transact(func(tx cgofdb.Transaction) (any, error) {
		tx.Set(cgofdb.Key("k"), []byte("from-cgo"))
		return nil, nil
	}); err != nil {
		t.Fatalf("cgo write into go-created tenant: %v", err)
	}
	goOnG, err := goClient.OpenTenant(gofdb.Key(gName))
	if err != nil {
		t.Fatalf("go OpenTenant(go-created tenant): %v", err)
	}
	got, err := goOnG.ReadTransact(func(rt gofdb.ReadTransaction) (any, error) {
		return rt.Get(key).Get()
	})
	if err != nil {
		t.Fatalf("go read of cgo's write: %v", err)
	}
	if gb, _ := got.([]byte); string(gb) != "from-cgo" {
		t.Fatalf("go read of cgo's write in shared tenant = %q, want %q", gb, "from-cgo")
	}

	// ListTenants on BOTH clients must include BOTH names (go.ListTenants decodes the nameIndex
	// values; the suffix scan is name-only, but a robust list still requires the value decode in
	// related paths — and this proves go sees the cgo-created tenant and vice versa).
	assertListed := func(label string, names [][]byte) {
		seen := map[string]bool{}
		for _, n := range names {
			seen[string(n)] = true
		}
		if !seen[string(cName)] {
			t.Fatalf("%s ListTenants missing cgo-created tenant %q", label, cName)
		}
		if !seen[string(gName)] {
			t.Fatalf("%s ListTenants missing go-created tenant %q", label, gName)
		}
	}
	goNames, err := goClient.ListTenants()
	if err != nil {
		t.Fatalf("go ListTenants: %v", err)
	}
	goNamesB := make([][]byte, len(goNames))
	for i, n := range goNames {
		goNamesB[i] = []byte(n)
	}
	assertListed("go", goNamesB)

	cgoNames, err := cgoClient.ListTenants()
	if err != nil {
		t.Fatalf("cgo ListTenants: %v", err)
	}
	cgoNamesB := make([][]byte, len(cgoNames))
	for i, n := range cgoNames {
		cgoNamesB[i] = []byte(n)
	}
	assertListed("cgo", cgoNamesB)
}

// clearAndDeleteTenant empties a tenant's keyspace (DeleteTenant rejects non-empty) and
// deletes it, via cgoClient. Best-effort cleanup.
func clearAndDeleteTenant(name []byte) {
	if ct, err := cgoClient.OpenTenant(cgofdb.Key(name)); err == nil {
		ct.Transact(func(tx cgofdb.Transaction) (any, error) {
			tx.ClearRange(cgofdb.KeyRange{Begin: cgofdb.Key(""), End: cgofdb.Key("\xff")})
			return nil, nil
		})
	}
	cgoClient.DeleteTenant(cgofdb.Key(name))
}

// cgoTenantGet reads a key through a cgo tenant handle.
func cgoTenantGet(t *testing.T, tn cgofdb.Tenant, key string) []byte {
	t.Helper()
	r, err := tn.ReadTransact(func(rt cgofdb.ReadTransaction) (any, error) {
		return rt.Get(cgofdb.Key(key)).Get()
	})
	if err != nil {
		t.Fatalf("cgo tenant get %q: %v", key, err)
	}
	b, _ := r.([]byte)
	return b
}

// tenantName builds a unique tenant name for a (sub)test: pid + test path → parallel-safe.
func tenantName(t *testing.T) []byte {
	t.Helper()
	return []byte(fmt.Sprintf("tcrud_%d_%s", os.Getpid(), strings.ReplaceAll(t.Name(), "/", "_")))
}

// assertSameTenantCode requires go and cgo to return the same error code, and that code to
// equal want (libfdb_c is the oracle; if cgo itself differs from want, investigate).
func assertSameTenantCode(t *testing.T, label string, goCode, cgoCode, want int) {
	t.Helper()
	if goCode != cgoCode {
		t.Fatalf("%s: error code differs: go=%d cgo=%d (want %d)", label, goCode, cgoCode, want)
	}
	if goCode != want {
		t.Fatalf("%s: both returned %d, want %d", label, goCode, want)
	}
}

// TestDifferential_TenantCRUDErrors pins go==cgo error CODES across the tenant CRUD error
// surface. The Go facade returns coded fdb.Error (2131–2136); libfdb_c returns the same FDB
// codes. Each case drives BOTH clients at identical cluster state and asserts equal codes — a
// wrong code, or success-where-the-other-errored, is a real interop divergence.
func TestDifferential_TenantCRUDErrors(t *testing.T) {
	t.Parallel()

	t.Run("duplicate_create", func(t *testing.T) {
		t.Parallel()
		name := tenantName(t)
		if err := cgoClient.CreateTenant(cgofdb.Key(name)); err != nil {
			t.Fatalf("seed create: %v", err)
		}
		t.Cleanup(func() { clearAndDeleteTenant(name) })
		goCode := fdbErrorCode(goClient.CreateTenant(gofdb.Key(name)))
		cgoCode := fdbErrorCode(cgoClient.CreateTenant(cgofdb.Key(name)))
		assertSameTenantCode(t, "duplicate_create", goCode, cgoCode, 2132) // tenant_already_exists
	})

	t.Run("delete_nonexistent", func(t *testing.T) {
		t.Parallel()
		name := tenantName(t) // never created
		goCode := fdbErrorCode(goClient.DeleteTenant(gofdb.Key(name)))
		cgoCode := fdbErrorCode(cgoClient.DeleteTenant(cgofdb.Key(name)))
		assertSameTenantCode(t, "delete_nonexistent", goCode, cgoCode, 2131) // tenant_not_found
	})

	t.Run("delete_nonempty", func(t *testing.T) {
		t.Parallel()
		name := tenantName(t)
		if err := cgoClient.CreateTenant(cgofdb.Key(name)); err != nil {
			t.Fatalf("seed create: %v", err)
		}
		t.Cleanup(func() { clearAndDeleteTenant(name) })
		tn, err := cgoClient.OpenTenant(cgofdb.Key(name))
		if err != nil {
			t.Fatalf("seed open: %v", err)
		}
		if _, err := tn.Transact(func(tx cgofdb.Transaction) (any, error) {
			tx.Set(cgofdb.Key("k"), []byte("v"))
			return nil, nil
		}); err != nil {
			t.Fatalf("seed write: %v", err)
		}
		goCode := fdbErrorCode(goClient.DeleteTenant(gofdb.Key(name)))
		cgoCode := fdbErrorCode(cgoClient.DeleteTenant(cgofdb.Key(name)))
		assertSameTenantCode(t, "delete_nonempty", goCode, cgoCode, 2133) // tenant_not_empty
	})

	t.Run("invalid_name_ff_prefix", func(t *testing.T) {
		t.Parallel()
		// FDB forbids tenant names starting with \xff. Both clients must reject with the same
		// code (don't hard-code it — let the libfdb_c oracle define it, just require agreement
		// and a non-zero/non-success result).
		name := append([]byte{0xFF}, tenantName(t)...)
		goCode := fdbErrorCode(goClient.CreateTenant(gofdb.Key(name)))
		cgoCode := fdbErrorCode(cgoClient.CreateTenant(cgofdb.Key(name)))
		if goCode != cgoCode {
			t.Fatalf("invalid_name: error code differs: go=%d cgo=%d", goCode, cgoCode)
		}
		if goCode == 0 {
			t.Fatalf("invalid_name: both accepted a \\xff-prefixed tenant name (must reject)")
		}
	})
}

// requireTenantAbsent asserts a tenant name is absent on BOTH clients (not listed, OpenTenant
// fails with tenant_not_found 2131).
func requireTenantAbsent(t *testing.T, name []byte) {
	t.Helper()
	goNames, err := goClient.ListTenants()
	if err != nil {
		t.Fatalf("go ListTenants: %v", err)
	}
	for _, n := range goNames {
		if string(n) == string(name) {
			t.Fatalf("tenant %q still listed by go after delete", name)
		}
	}
	cgoNames, err := cgoClient.ListTenants()
	if err != nil {
		t.Fatalf("cgo ListTenants: %v", err)
	}
	for _, n := range cgoNames {
		if string(n) == string(name) {
			t.Fatalf("tenant %q still listed by cgo after delete", name)
		}
	}
	if code := fdbErrorCode(func() error { _, e := goClient.OpenTenant(gofdb.Key(name)); return e }()); code != 2131 {
		t.Fatalf("go OpenTenant(deleted) code=%d, want 2131 tenant_not_found", code)
	}
}

// TestDifferential_TenantCrossClientDelete pins that each client can DELETE a tenant the OTHER
// created. go.DeleteTenant must decode the libfdb_c-written nameIndex (the codec path the fix
// corrected) to find the ID; before the fix this failed. After delete, neither client lists it.
func TestDifferential_TenantCrossClientDelete(t *testing.T) {
	t.Parallel()
	ns := strings.ReplaceAll(t.Name(), "/", "_")

	// cgo creates → go deletes.
	cName := []byte(fmt.Sprintf("xdel_c_%d_%s", os.Getpid(), ns))
	if err := cgoClient.CreateTenant(cgofdb.Key(cName)); err != nil {
		t.Fatalf("cgo create: %v", err)
	}
	if err := goClient.DeleteTenant(gofdb.Key(cName)); err != nil {
		t.Fatalf("go DeleteTenant(cgo-created tenant): %v", err)
	}
	requireTenantAbsent(t, cName)

	// go creates → cgo deletes.
	gName := []byte(fmt.Sprintf("xdel_g_%d_%s", os.Getpid(), ns))
	if err := goClient.CreateTenant(gofdb.Key(gName)); err != nil {
		t.Fatalf("go create: %v", err)
	}
	if err := cgoClient.DeleteTenant(cgofdb.Key(gName)); err != nil {
		t.Fatalf("cgo DeleteTenant(go-created tenant): %v", err)
	}
	requireTenantAbsent(t, gName)
}

func TestDifferential_TenantVersionstampedKey(t *testing.T) {
	t.Parallel()
	cases := []vsKeyCase{
		{"offset0_no_logical", nil, nil},           // sub-prefix + stamp (stamp right after the 8-byte tenant prefix + sub)
		{"after_logical", []byte("abc"), nil},      // sub + "abc" + stamp
		{"mid_key", []byte("pre"), []byte("post")}, // sub + "pre" + stamp + "post"
		{"binary_surround", []byte{0x00, 0xff}, []byte{0x01, 0x00, 0xff}},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			runTenantVSKeyCase(t, c)
		})
	}
}

func runTenantVSValueCase(t *testing.T, c vsValueCase) {
	t.Helper()
	ns := strings.ReplaceAll(t.Name(), "/", "_")
	goT, cgoT, prefix := setupSharedTenant(t, ns)

	// SetVersionstampedValue in a tenant: the KEY is prefixed, the VALUE (with its stamp
	// offset) is NOT — so the value offset must be left unchanged. We write at a fixed
	// per-client key, read the value back through the tenant, mask, and compare.
	masked := func(key string, write func(k, template []byte), read func(k []byte) ([]byte, bool)) string {
		stampPos := len(c.logical)
		data := make([]byte, 0, stampPos+vsStampLen+len(c.suffix))
		data = append(data, c.logical...)
		data = append(data, make([]byte, vsStampLen)...)
		data = append(data, c.suffix...)
		kb := []byte(key)
		write(kb, vsOperand(data, stampPos))
		val, ok := read(kb)
		if !ok {
			t.Fatalf("%s: tenant value not found at %q", c.name, key)
		}
		wantLen := len(c.logical) + vsStampLen + len(c.suffix)
		if len(val) != wantLen {
			t.Fatalf("%s: tenant value len=%d want=%d", c.name, len(val), wantLen)
		}
		m, nonZero := maskStamp(val, len(c.logical))
		if !nonZero {
			t.Fatalf("%s: value stamp region all-zero — value offset must NOT be tenant-adjusted", c.name)
		}
		return hex.EncodeToString(m)
	}

	goMasked := masked("g", func(k, template []byte) {
		if _, err := goT.Transact(func(txw gofdb.WritableTransaction) (any, error) {
			tx := txw.(gofdb.Transaction)
			tx.SetVersionstampedValue(gofdb.Key(k), template)
			return nil, nil
		}); err != nil {
			t.Fatalf("go %s tenant vsvalue write: %v", c.name, err)
		}
	}, func(k []byte) ([]byte, bool) {
		r, err := goT.Transact(func(txw gofdb.WritableTransaction) (any, error) {
			tx := txw.(gofdb.Transaction)
			return tx.Get(gofdb.Key(k)).Get()
		})
		if err != nil {
			t.Fatalf("go %s tenant vsvalue read: %v", c.name, err)
		}
		b, _ := r.([]byte)
		return b, b != nil
	})

	cgoMasked := masked("c", func(k, template []byte) {
		if _, err := cgoT.Transact(func(tx cgofdb.Transaction) (any, error) {
			tx.SetVersionstampedValue(cgofdb.Key(k), template)
			return nil, nil
		}); err != nil {
			t.Fatalf("cgo %s tenant vsvalue write: %v", c.name, err)
		}
	}, func(k []byte) ([]byte, bool) {
		r, err := cgoT.Transact(func(tx cgofdb.Transaction) (any, error) { return tx.Get(cgofdb.Key(k)).Get() })
		if err != nil {
			t.Fatalf("cgo %s tenant vsvalue read: %v", c.name, err)
		}
		b, _ := r.([]byte)
		return b, b != nil
	})

	if goMasked != cgoMasked {
		t.Fatalf("%s: masked tenant value differs: go=%s cgo=%s", c.name, goMasked, cgoMasked)
	}

	// Raw-read both values and assert the stamp stayed at the UNADJUSTED offset len(logical)
	// (the value is not prefixed). A bug that erroneously +8'd the value offset would move it.
	scan := tenantRawScan(t, prefix)
	for _, sub := range []string{"g", "c"} {
		fk := rawKeyWithSub(t, scan, prefix, []byte(sub))
		// locate the value for this key in the scan
		var val []byte
		for _, kv := range scan {
			if bytes.Equal(kv.Key, fk) {
				val = kv.Value
			}
		}
		if _, nonZero := maskStamp(val, len(c.logical)); !nonZero {
			t.Fatalf("%s: raw value %x has no stamp at offset %d — value offset must stay UNADJUSTED in a tenant", c.name, val, len(c.logical))
		}
	}
}

// TestDifferential_TenantVersionstampedValue pins that SetVersionstampedValue's offset is
// left UNCHANGED in a tenant (only the key is prefixed) — go==cgo.
func TestDifferential_TenantVersionstampedValue(t *testing.T) {
	t.Parallel()
	cases := []vsValueCase{
		{"offset0", nil, nil},
		{"after_logical", []byte("hdr"), nil},
		{"mid_value", []byte("pre"), []byte("post")},
		{"binary_surround", []byte{0x00, 0xff}, []byte{0x01, 0x00, 0xff}},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			runTenantVSValueCase(t, c)
		})
	}
}

// TestDifferential_TenantVersionstampErrors pins that the offset-VALIDATION boundary is
// identical in a tenant. Both clients validate the USER operand (pre-prefix) — C++ at the
// atomicOp() call (before applyTenantPrefix), Go in the per-mutation commit loop on m.Key
// (before buildCommitTransactionRequest applies the prefix). So the tenant prefix must NOT
// shift the boundary: offset+10==body still commits; +1 still rejects (2000). A bug that
// validated the POST-+8 offset would move the boundary by 8 in a tenant and diverge here.
func TestDifferential_TenantVersionstampErrors(t *testing.T) {
	t.Parallel()
	goT, cgoT, _ := setupSharedTenant(t, strings.ReplaceAll(t.Name(), "/", "_"))

	cases := []struct {
		name    string
		operand func(iso []byte) []byte
		wantOK  bool
	}{
		{"valid_tight", func(iso []byte) []byte {
			return vsOperand(append(append([]byte{}, iso...), make([]byte, vsStampLen)...), len(iso))
		}, true},
		{"offbyone_reject", func(iso []byte) []byte {
			return vsOperand(append(append([]byte{}, iso...), make([]byte, vsStampLen)...), len(iso)+1)
		}, false},
		{"offset_past_body", func(iso []byte) []byte { return vsOperand(make([]byte, vsStampLen), 99) }, false},
		{"operand_too_small", func(iso []byte) []byte { return []byte{0x00, 0x00, 0x00} }, false},
		{"empty_body", func(iso []byte) []byte { return vsOperand(nil, 0) }, false},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			goCode := func() int {
				_, err := goT.Transact(func(txw gofdb.WritableTransaction) (any, error) {
					tx := txw.(gofdb.Transaction)
					tx.SetVersionstampedKey(gofdb.Key(c.operand([]byte("g_"+c.name))), []byte("v"))
					return nil, nil
				})
				return fdbErrorCode(err)
			}()
			cgoCode := func() int {
				_, err := cgoT.Transact(func(tx cgofdb.Transaction) (any, error) {
					tx.SetVersionstampedKey(cgofdb.Key(c.operand([]byte("c_"+c.name))), []byte("v"))
					return nil, nil
				})
				return fdbErrorCode(err)
			}()
			if goCode != cgoCode {
				t.Fatalf("%s: tenant commit error code differs: go=%d cgo=%d", c.name, goCode, cgoCode)
			}
			if c.wantOK && goCode != 0 {
				t.Fatalf("%s: expected clean tenant commit, got code %d", c.name, goCode)
			}
			if !c.wantOK && goCode == 0 {
				t.Fatalf("%s: expected rejection in tenant, both committed cleanly", c.name)
			}
		})
	}
}
