package storage

import (
	"context"
	"fmt"

	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/fdb"
	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/fdb/subspace"
	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/fdb/tuple"
	rl "github.com/birdayz/fdb-record-layer-go/pkg/recordlayer"

	storev1 "github.com/birdayz/fdb-record-layer-go/examples/metrognome/gen/metrognome/store/v1"
)

const systemTenantName = "__system"

// SystemDB is the control-plane store in the __system FDB tenant.
// Holds tenants, tenant memberships, invites, and pre-login OAuth state.
// NOT billing data — that lives in per-org tenant stores (DB).
type SystemDB struct {
	fdb      *rl.FDBDatabase
	rawDB    fdb.Database // for creating/opening tenants
	metadata *rl.RecordMetaData
	ss       subspace.Subspace
}

// NewSystemDB creates or opens the __system tenant and its record store.
func NewSystemDB(rawDB fdb.Database) (*SystemDB, error) {
	// Ensure __system tenant exists. Already-exists is fine (idempotent).
	_ = rawDB.CreateTenant(fdb.Key(systemTenantName))

	tenant, err := rawDB.OpenTenant(fdb.Key(systemTenantName))
	if err != nil {
		return nil, fmt.Errorf("open __system tenant: %w", err)
	}

	fdbDB := rl.NewFDBDatabaseFromTenant(tenant)

	builder := rl.NewRecordMetaDataBuilder().
		SetRecords(storev1.File_metrognome_store_v1_store_proto)

	// Only the system record types — Tenant, TenantMember, Invite, OAuthState.
	// Other types exist in the union but are unused here.
	builder.GetRecordType("Tenant").SetPrimaryKey(
		rl.Concat(rl.RecordTypeKey(), rl.Field("name")))

	builder.GetRecordType("TenantMember").SetPrimaryKey(
		rl.Concat(rl.RecordTypeKey(), rl.Field("github_id")))

	builder.GetRecordType("Invite").SetPrimaryKey(
		rl.Concat(rl.RecordTypeKey(), rl.Field("code")))

	builder.GetRecordType("OAuthState").SetPrimaryKey(
		rl.Concat(rl.RecordTypeKey(), rl.Field("state")))

	// Unused types still need PKs for the union to build.
	builder.GetRecordType("Customer").SetPrimaryKey(rl.Concat(rl.RecordTypeKey(), rl.Field("id")))
	builder.GetRecordType("Meter").SetPrimaryKey(rl.Concat(rl.RecordTypeKey(), rl.Field("id")))
	builder.GetRecordType("Plan").SetPrimaryKey(rl.Concat(rl.RecordTypeKey(), rl.Field("id")))
	builder.GetRecordType("Charge").SetPrimaryKey(rl.Concat(rl.RecordTypeKey(), rl.Field("id")))
	builder.GetRecordType("Contract").SetPrimaryKey(rl.Concat(rl.RecordTypeKey(), rl.Field("id")))
	builder.GetRecordType("UsageEvent").SetPrimaryKey(rl.Concat(rl.RecordTypeKey(), rl.Field("idempotency_key")))
	builder.GetRecordType("Invoice").SetPrimaryKey(rl.Concat(rl.RecordTypeKey(), rl.Field("id")))
	builder.GetRecordType("Credit").SetPrimaryKey(rl.Concat(rl.RecordTypeKey(), rl.Field("id")))
	builder.GetRecordType("Alert").SetPrimaryKey(rl.Concat(rl.RecordTypeKey(), rl.Field("id")))
	builder.GetRecordType("KafkaOffset").SetPrimaryKey(rl.Concat(rl.RecordTypeKey(), rl.Field("topic")))
	builder.GetRecordType("DeadLetter").SetPrimaryKey(rl.Concat(rl.RecordTypeKey(), rl.Field("id")))
	builder.GetRecordType("User").SetPrimaryKey(rl.Concat(rl.RecordTypeKey(), rl.Field("id")))
	builder.GetRecordType("Session").SetPrimaryKey(rl.Concat(rl.RecordTypeKey(), rl.Field("id")))
	builder.GetRecordType("Product").SetPrimaryKey(rl.Concat(rl.RecordTypeKey(), rl.Field("id")))
	builder.GetRecordType("RateCard").SetPrimaryKey(rl.Concat(rl.RecordTypeKey(), rl.Field("id")))
	builder.GetRecordType("Rate").SetPrimaryKey(rl.Concat(rl.RecordTypeKey(), rl.Field("id")))
	builder.GetRecordType("ApiKey").SetPrimaryKey(rl.Concat(rl.RecordTypeKey(), rl.Field("id")))

	// Index: look up TenantMember by tenant_name (list members of a tenant)
	builder.AddIndex("TenantMember", rl.NewIndex("member_by_tenant", rl.Field("tenant_name")))

	// Index: look up invites by tenant_name
	builder.AddIndex("Invite", rl.NewIndex("invite_by_tenant", rl.Field("tenant_name")))

	md, err := builder.Build()
	if err != nil {
		return nil, fmt.Errorf("build system metadata: %w", err)
	}

	ss := subspace.Sub("sys")

	// CreateOrOpen the store.
	_, err = fdbDB.Run(context.Background(), func(rtx *rl.FDBRecordContext) (any, error) {
		_, err := rl.NewStoreBuilder().
			SetContext(rtx).
			SetMetaDataProvider(md).
			SetSubspace(ss).
			CreateOrOpen()
		return nil, err
	})
	if err != nil {
		return nil, fmt.Errorf("create/open system store: %w", err)
	}

	return &SystemDB{
		fdb:      fdbDB,
		rawDB:    rawDB,
		metadata: md,
		ss:       ss,
	}, nil
}

// RawDB returns the underlying fdb.Database (for creating/opening tenants).
func (s *SystemDB) RawDB() fdb.Database { return s.rawDB }

// run executes fn within a transaction on the system store.
func (s *SystemDB) run(ctx context.Context, fn func(*rl.FDBRecordStore) (any, error)) (any, error) {
	return s.fdb.Run(ctx, func(rtx *rl.FDBRecordContext) (any, error) {
		store, err := rl.NewStoreBuilder().
			SetContext(rtx).
			SetMetaDataProvider(s.metadata).
			SetSubspace(s.ss).
			SetAssumeAllIndexesReadable(true).
			Build()
		if err != nil {
			return nil, err
		}
		return fn(store)
	})
}

// runInStore provides both context and store.
func (s *SystemDB) runInStore(ctx context.Context, fn func(*rl.FDBRecordContext, *rl.FDBRecordStore) (any, error)) (any, error) {
	return s.fdb.Run(ctx, func(rtx *rl.FDBRecordContext) (any, error) {
		store, err := rl.NewStoreBuilder().
			SetContext(rtx).
			SetMetaDataProvider(s.metadata).
			SetSubspace(s.ss).
			SetAssumeAllIndexesReadable(true).
			Build()
		if err != nil {
			return nil, err
		}
		return fn(rtx, store)
	})
}

// pk builds a primary key tuple for the system store.
func (s *SystemDB) pk(typeName string, fields ...any) tuple.Tuple {
	t := make(tuple.Tuple, 0, 1+len(fields))
	t = append(t, int64(s.metadata.GetRecordType(typeName).RecordTypeIndex))
	for _, f := range fields {
		t = append(t, f)
	}
	return t
}

// --- Tenant CRUD ---

func (s *SystemDB) CreateTenant(ctx context.Context, t *storev1.Tenant) error {
	// Create FDB tenant first. Already-exists is fine (idempotent).
	_ = s.rawDB.CreateTenant(fdb.Key(t.GetName()))
	// Then record it in the system store.
	_, err := s.run(ctx, func(rs *rl.FDBRecordStore) (any, error) {
		_, err := rs.SaveRecord(t)
		return nil, err
	})
	return err
}

func (s *SystemDB) GetTenant(ctx context.Context, name string) (*storev1.Tenant, error) {
	result, err := s.run(ctx, func(rs *rl.FDBRecordStore) (any, error) {
		rec, err := rs.LoadRecord(s.pk("Tenant", name))
		if err != nil {
			return nil, err
		}
		if rec == nil {
			return nil, ErrNotFound
		}
		return rec.Record, nil
	})
	if err != nil {
		return nil, err
	}
	return result.(*storev1.Tenant), nil
}

// --- TenantMember CRUD ---

func (s *SystemDB) CreateMember(ctx context.Context, m *storev1.TenantMember) error {
	_, err := s.run(ctx, func(rs *rl.FDBRecordStore) (any, error) {
		_, err := rs.SaveRecord(m)
		return nil, err
	})
	return err
}

func (s *SystemDB) GetMember(ctx context.Context, githubID string) (*storev1.TenantMember, error) {
	result, err := s.run(ctx, func(rs *rl.FDBRecordStore) (any, error) {
		rec, err := rs.LoadRecord(s.pk("TenantMember", githubID))
		if err != nil {
			return nil, err
		}
		if rec == nil {
			return nil, ErrNotFound
		}
		return rec.Record, nil
	})
	if err != nil {
		return nil, err
	}
	return result.(*storev1.TenantMember), nil
}

// --- OAuthState ---

func (s *SystemDB) CreateOAuthState(ctx context.Context, state *storev1.OAuthState) error {
	_, err := s.run(ctx, func(rs *rl.FDBRecordStore) (any, error) {
		_, err := rs.SaveRecord(state)
		return nil, err
	})
	return err
}

func (s *SystemDB) ConsumeOAuthState(ctx context.Context, stateToken string) (*storev1.OAuthState, error) {
	result, err := s.runInStore(ctx, func(_ *rl.FDBRecordContext, rs *rl.FDBRecordStore) (any, error) {
		pk := s.pk("OAuthState", stateToken)
		rec, err := rs.LoadRecord(pk)
		if err != nil {
			return nil, err
		}
		if rec == nil {
			return nil, ErrNotFound
		}
		if _, err := rs.DeleteRecord(pk); err != nil {
			return nil, err
		}
		return rec.Record, nil
	})
	if err != nil {
		return nil, err
	}
	return result.(*storev1.OAuthState), nil
}

// --- Invite ---

func (s *SystemDB) CreateInvite(ctx context.Context, inv *storev1.Invite) error {
	_, err := s.run(ctx, func(rs *rl.FDBRecordStore) (any, error) {
		_, err := rs.SaveRecord(inv)
		return nil, err
	})
	return err
}

func (s *SystemDB) ConsumeInvite(ctx context.Context, code string) (*storev1.Invite, error) {
	result, err := s.runInStore(ctx, func(_ *rl.FDBRecordContext, rs *rl.FDBRecordStore) (any, error) {
		pk := s.pk("Invite", code)
		rec, err := rs.LoadRecord(pk)
		if err != nil {
			return nil, err
		}
		if rec == nil {
			return nil, ErrNotFound
		}
		if _, err := rs.DeleteRecord(pk); err != nil {
			return nil, err
		}
		return rec.Record, nil
	})
	if err != nil {
		return nil, err
	}
	return result.(*storev1.Invite), nil
}
