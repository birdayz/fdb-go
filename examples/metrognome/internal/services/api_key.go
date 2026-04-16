package services

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"

	storev1 "github.com/birdayz/fdb-record-layer-go/examples/metrognome/gen/metrognome/store/v1"
	metrognomev1 "github.com/birdayz/fdb-record-layer-go/examples/metrognome/gen/metrognome/v1"
	"github.com/birdayz/fdb-record-layer-go/examples/metrognome/gen/metrognome/v1/metrognomev1connect"
	"github.com/birdayz/fdb-record-layer-go/examples/metrognome/internal/storage"
)

type ApiKeyService struct {
	metrognomev1connect.UnimplementedApiKeyServiceHandler
	store *storage.ApiKeyStore
}

func NewApiKeyService(store *storage.ApiKeyStore) *ApiKeyService {
	return &ApiKeyService{store: store}
}

func generateAPIKey() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return "mgn_" + hex.EncodeToString(b), nil
}

func hashKey(key string) string {
	h := sha256.Sum256([]byte(key))
	return hex.EncodeToString(h[:])
}

func (s *ApiKeyService) CreateApiKey(ctx context.Context, req *connect.Request[metrognomev1.CreateApiKeyRequest]) (*connect.Response[metrognomev1.CreateApiKeyResponse], error) {
	if req.Msg.GetName() == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("name is required"))
	}

	rawKey, err := generateAPIKey()
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("generate key: %w", err))
	}

	id := newID("ak")
	now := time.Now().UnixMilli()
	record := &storev1.ApiKey{
		Id:        proto.String(id),
		Name:      proto.String(req.Msg.GetName()),
		KeyHash:   proto.String(hashKey(rawKey)),
		KeyPrefix: proto.String(rawKey[:12] + "..."),
		CreatedAt: proto.Int64(now),
		Revoked:   proto.Bool(false),
	}

	if err := s.store.Create(ctx, record); err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("create api key: %w", err))
	}

	return connect.NewResponse(&metrognomev1.CreateApiKeyResponse{
		ApiKey: apiKeyToAPI(record),
		RawKey: rawKey,
	}), nil
}

func (s *ApiKeyService) ListApiKeys(ctx context.Context, req *connect.Request[metrognomev1.ListApiKeysRequest]) (*connect.Response[metrognomev1.ListApiKeysResponse], error) {
	items, err := s.store.List(ctx)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	keys := make([]*metrognomev1.ApiKey, len(items))
	for i, item := range items {
		keys[i] = apiKeyToAPI(item)
	}
	return connect.NewResponse(&metrognomev1.ListApiKeysResponse{
		ApiKeys: keys,
	}), nil
}

func (s *ApiKeyService) RevokeApiKey(ctx context.Context, req *connect.Request[metrognomev1.RevokeApiKeyRequest]) (*connect.Response[metrognomev1.RevokeApiKeyResponse], error) {
	if req.Msg.GetId() == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("id is required"))
	}

	items, err := s.store.List(ctx)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	for _, item := range items {
		if item.GetId() == req.Msg.GetId() {
			item.Revoked = proto.Bool(true)
			if err := s.store.Save(ctx, item); err != nil {
				return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("revoke: %w", err))
			}
			return connect.NewResponse(&metrognomev1.RevokeApiKeyResponse{}), nil
		}
	}
	return nil, connect.NewError(connect.CodeNotFound, fmt.Errorf("api key not found"))
}

func apiKeyToAPI(s *storev1.ApiKey) *metrognomev1.ApiKey {
	return &metrognomev1.ApiKey{
		Id:         s.GetId(),
		Name:       s.GetName(),
		KeyPrefix:  s.GetKeyPrefix(),
		CreatedBy:  s.GetCreatedBy(),
		CreatedAt:  s.GetCreatedAt(),
		LastUsedAt: s.GetLastUsedAt(),
		Revoked:    s.GetRevoked(),
	}
}
