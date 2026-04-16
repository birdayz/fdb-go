// Package auth provides GitHub OAuth login and ConnectRPC auth middleware.
package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"connectrpc.com/connect"
	"golang.org/x/oauth2"
	"golang.org/x/oauth2/github"
	"google.golang.org/protobuf/proto"

	storev1 "github.com/birdayz/fdb-record-layer-go/examples/metrognome/gen/metrognome/store/v1"
	metrognomev1 "github.com/birdayz/fdb-record-layer-go/examples/metrognome/gen/metrognome/v1"
	"github.com/birdayz/fdb-record-layer-go/examples/metrognome/internal/seed"
	"github.com/birdayz/fdb-record-layer-go/examples/metrognome/internal/storage"
	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/fdb"
)

const sessionMaxAge = 86400 * 30 // 30 days

type contextKey struct{}

// Handler manages GitHub OAuth login, sessions, and provides auth middleware.
// Uses SystemDB (__system tenant) for OAuth state and tenant resolution.
// Opens per-org tenant DBs for sessions and billing data.
type Handler struct {
	oauth       *oauth2.Config
	sysDB       *storage.SystemDB
	rawDB       fdb.Database // for opening tenant DBs
	frontendURL string
}

// NewHandler creates a new auth handler.
func NewHandler(cfg *metrognomev1.GitHubOAuth, frontendURL string, sysDB *storage.SystemDB) *Handler {
	return &Handler{
		oauth: &oauth2.Config{
			ClientID:     cfg.GetClientId(),
			ClientSecret: cfg.GetClientSecret(),
			RedirectURL:  cfg.GetRedirectUrl(),
			Scopes:       []string{"read:user", "user:email"},
			Endpoint:     github.Endpoint,
		},
		sysDB:       sysDB,
		rawDB:       sysDB.RawDB(),
		frontendURL: frontendURL,
	}
}

// RegisterRoutes adds the OAuth HTTP routes to the mux.
func (h *Handler) RegisterRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /auth/login", h.handleLogin)
	mux.HandleFunc("GET /auth/callback", h.handleCallback)
	mux.HandleFunc("POST /auth/logout", h.handleLogout)
	mux.HandleFunc("GET /auth/me", h.handleMe)
}

// Interceptor returns a ConnectRPC interceptor that authenticates requests.
func (h *Handler) Interceptor() connect.Interceptor {
	return &authInterceptor{handler: h}
}

func (h *Handler) handleLogin(w http.ResponseWriter, r *http.Request) {
	state := generateToken()
	oauthState := &storev1.OAuthState{
		State:     proto.String(state),
		ExpiresAt: proto.Int64(time.Now().Add(10 * time.Minute).UnixMilli()),
	}
	if err := h.sysDB.CreateOAuthState(r.Context(), oauthState); err != nil {
		slog.Error("failed to store oauth state", "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	url := h.oauth.AuthCodeURL(state)
	http.Redirect(w, r, url, http.StatusTemporaryRedirect)
}

func (h *Handler) handleCallback(w http.ResponseWriter, r *http.Request) {
	stateToken := r.URL.Query().Get("state")
	oauthState, err := h.sysDB.ConsumeOAuthState(r.Context(), stateToken)
	if err != nil {
		http.Error(w, "invalid or expired state", http.StatusBadRequest)
		return
	}
	if oauthState.GetExpiresAt() < time.Now().UnixMilli() {
		http.Error(w, "state expired", http.StatusBadRequest)
		return
	}

	code := r.URL.Query().Get("code")
	if code == "" {
		http.Error(w, "missing code", http.StatusBadRequest)
		return
	}

	tok, err := h.oauth.Exchange(r.Context(), code)
	if err != nil {
		slog.Error("oauth exchange failed", "error", err)
		http.Error(w, "oauth error", http.StatusInternalServerError)
		return
	}

	ghUser, err := fetchGitHubUser(r.Context(), tok.AccessToken)
	if err != nil {
		slog.Error("github user fetch failed", "error", err)
		http.Error(w, "github error", http.StatusInternalServerError)
		return
	}

	now := time.Now().UnixMilli()
	githubID := fmt.Sprintf("%d", ghUser.ID)
	userID := fmt.Sprintf("gh_%d", ghUser.ID)

	// Check if user already has a tenant membership.
	var tenantName string
	member, err := h.sysDB.GetMember(r.Context(), githubID)
	if err == nil {
		tenantName = member.GetTenantName()
	} else {
		// First login: create tenant + membership.
		tenantName = fmt.Sprintf("org_%d", ghUser.ID)
		tenant := &storev1.Tenant{
			Name:          proto.String(tenantName),
			DisplayName:   proto.String(ghUser.Login),
			OwnerGithubId: proto.String(githubID),
			CreatedAt:     proto.Int64(now),
		}
		if err := h.sysDB.CreateTenant(r.Context(), tenant); err != nil {
			slog.Error("failed to create tenant", "error", err, "tenant", tenantName)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		if err := h.sysDB.CreateMember(r.Context(), &storev1.TenantMember{
			GithubId:   proto.String(githubID),
			TenantName: proto.String(tenantName),
			Role:       proto.String("owner"),
			JoinedAt:   proto.Int64(now),
		}); err != nil {
			slog.Error("failed to create tenant member", "error", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		slog.Info("created tenant for new user", "tenant", tenantName, "user", ghUser.Login)

		// Seed the tenant with demo data so the UI isn't blank on first login.
		seedDB, err := storage.NewTenantDB(h.rawDB, tenantName)
		if err != nil {
			slog.Error("failed to open tenant for seeding", "error", err)
		} else {
			if err := seed.Tenant(r.Context(), seedDB, ghUser.Login); err != nil {
				slog.Warn("tenant seeding failed", "error", err)
			}
		}
	}

	// Open the org tenant and upsert user + session.
	tenantDB, err := storage.NewTenantDB(h.rawDB, tenantName)
	if err != nil {
		slog.Error("failed to open tenant DB", "error", err, "tenant", tenantName)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	user := &storev1.User{
		Id:        proto.String(userID),
		GithubId:  proto.String(githubID),
		Login:     proto.String(ghUser.Login),
		Name:      proto.String(ghUser.Name),
		AvatarUrl: proto.String(ghUser.AvatarURL),
		Email:     proto.String(ghUser.Email),
		CreatedAt: proto.Int64(now),
	}
	if err := tenantDB.Users().Create(r.Context(), user); err != nil {
		_ = tenantDB.Users().Save(r.Context(), user)
	}

	sessionID := generateToken()
	session := &storev1.Session{
		Id:        proto.String(sessionID),
		UserId:    proto.String(userID),
		CreatedAt: proto.Int64(now),
		ExpiresAt: proto.Int64(now + int64(sessionMaxAge)*1000),
	}
	if err := tenantDB.Sessions().Create(r.Context(), session); err != nil {
		slog.Error("session create failed", "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	// Cookie encodes tenant: "org_12345678:session_token"
	http.SetCookie(w, &http.Cookie{
		Name:     "session",
		Value:    tenantName + ":" + sessionID,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   sessionMaxAge,
	})

	slog.Info("github login", "user", ghUser.Login, "id", ghUser.ID, "tenant", tenantName)
	http.Redirect(w, r, h.frontendURL, http.StatusTemporaryRedirect)
}

func (h *Handler) handleLogout(w http.ResponseWriter, _ *http.Request) {
	http.SetCookie(w, &http.Cookie{
		Name:     "session",
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		MaxAge:   -1,
	})
	w.WriteHeader(http.StatusOK)
}

func (h *Handler) handleMe(w http.ResponseWriter, r *http.Request) {
	user, _, err := h.resolveFromCookie(r)
	if err != nil {
		slog.Warn("auth/me failed", "error", err)
		http.Error(w, "not authenticated", http.StatusUnauthorized)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{
		"id":         user.GetId(),
		"login":      user.GetLogin(),
		"name":       user.GetName(),
		"avatar_url": user.GetAvatarUrl(),
		"email":      user.GetEmail(),
	})
}

// resolveFromCookie parses the "tenant:session" cookie, opens the tenant DB,
// and returns the user + tenant DB.
func (h *Handler) resolveFromCookie(r *http.Request) (*storev1.User, *storage.DB, error) {
	cookie, err := r.Cookie("session")
	if err != nil {
		return nil, nil, errors.New("no session cookie")
	}
	tenantName, sessionID, ok := strings.Cut(cookie.Value, ":")
	if !ok || tenantName == "" || sessionID == "" {
		return nil, nil, errors.New("malformed session cookie")
	}

	tenantDB, err := storage.NewTenantDB(h.rawDB, tenantName)
	if err != nil {
		return nil, nil, fmt.Errorf("open tenant: %w", err)
	}

	session, err := tenantDB.Sessions().Get(r.Context(), sessionID)
	if err != nil {
		return nil, nil, err
	}
	if session.GetExpiresAt() > 0 && session.GetExpiresAt() < time.Now().UnixMilli() {
		return nil, nil, errors.New("session expired")
	}

	user, err := tenantDB.Users().Get(r.Context(), session.GetUserId())
	if err != nil {
		return nil, nil, err
	}
	return user, tenantDB, nil
}

// UserFromContext returns the authenticated user from the context, or nil.
func UserFromContext(ctx context.Context) *storev1.User {
	user, _ := ctx.Value(contextKey{}).(*storev1.User)
	return user
}

// --- ConnectRPC interceptor ---

type authInterceptor struct {
	handler *Handler
}

func (a *authInterceptor) WrapUnary(next connect.UnaryFunc) connect.UnaryFunc {
	return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
		if req.Spec().IsClient {
			return next(ctx, req)
		}
		user, tenantDB, err := a.resolveFromHeaders(ctx, req.Header())
		if err != nil {
			return nil, connect.NewError(connect.CodeUnauthenticated, errors.New("not authenticated"))
		}
		ctx = context.WithValue(ctx, contextKey{}, user)
		ctx = storage.WithDB(ctx, tenantDB)
		return next(ctx, req)
	}
}

func (a *authInterceptor) WrapStreamingClient(next connect.StreamingClientFunc) connect.StreamingClientFunc {
	return next
}

func (a *authInterceptor) WrapStreamingHandler(next connect.StreamingHandlerFunc) connect.StreamingHandlerFunc {
	return func(ctx context.Context, conn connect.StreamingHandlerConn) error {
		user, tenantDB, err := a.resolveFromHeaders(ctx, conn.RequestHeader())
		if err != nil {
			return connect.NewError(connect.CodeUnauthenticated, errors.New("not authenticated"))
		}
		ctx = context.WithValue(ctx, contextKey{}, user)
		ctx = storage.WithDB(ctx, tenantDB)
		return next(ctx, conn)
	}
}

func (a *authInterceptor) resolveFromHeaders(ctx context.Context, headers http.Header) (*storev1.User, *storage.DB, error) {
	// Try API key auth first (Authorization: Bearer mgn_...)
	if authHeader := headers.Get("Authorization"); strings.HasPrefix(authHeader, "Bearer mgn_") {
		return a.resolveFromAPIKey(ctx, headers, strings.TrimPrefix(authHeader, "Bearer "))
	}

	// Fall back to session cookie
	raw := headers.Get("Cookie")
	if raw == "" {
		return nil, nil, errors.New("no cookie")
	}
	fakeReq := &http.Request{Header: http.Header{"Cookie": {raw}}}
	return a.handler.resolveFromCookie(fakeReq)
}

// apiKeyCache caches verified API key → (user, tenantDB) mappings.
var (
	apiKeyCacheMu sync.RWMutex
	apiKeyCacheM  = make(map[string]*apiKeyCacheEntry)
)

type apiKeyCacheEntry struct {
	user      *storev1.User
	tenantDB  *storage.DB
	expiresAt time.Time
}

func (a *authInterceptor) resolveFromAPIKey(ctx context.Context, headers http.Header, rawKey string) (*storev1.User, *storage.DB, error) {
	h := sha256.Sum256([]byte(rawKey))
	keyHash := hex.EncodeToString(h[:])

	apiKeyCacheMu.RLock()
	if entry, ok := apiKeyCacheM[keyHash]; ok && time.Now().Before(entry.expiresAt) {
		apiKeyCacheMu.RUnlock()
		return entry.user, entry.tenantDB, nil
	}
	apiKeyCacheMu.RUnlock()

	// API keys need a tenant context. The tenant name must be in the request.
	// For now, require X-Tenant-Name header with API key auth.
	tenantName := headers.Get("X-Tenant-Name")
	if tenantName == "" {
		return nil, nil, errors.New("api key auth requires X-Tenant-Name header")
	}

	tenantDB, err := storage.NewTenantDB(a.handler.rawDB, tenantName)
	if err != nil {
		return nil, nil, fmt.Errorf("open tenant: %w", err)
	}

	apiKey, err := tenantDB.ApiKeys().GetByKeyHash(ctx, keyHash)
	if err != nil {
		return nil, nil, errors.New("invalid api key")
	}
	if apiKey.GetRevoked() {
		return nil, nil, errors.New("api key revoked")
	}
	user := &storev1.User{
		Id:    proto.String("apikey:" + apiKey.GetId()),
		Login: proto.String("api:" + apiKey.GetName()),
		Name:  proto.String(apiKey.GetName()),
	}

	apiKeyCacheMu.Lock()
	apiKeyCacheM[keyHash] = &apiKeyCacheEntry{user: user, tenantDB: tenantDB, expiresAt: time.Now().Add(10 * time.Minute)}
	apiKeyCacheMu.Unlock()

	return user, tenantDB, nil
}

// --- GitHub API ---

type gitHubUser struct {
	ID        int64  `json:"id"`
	Login     string `json:"login"`
	Name      string `json:"name"`
	Email     string `json:"email"`
	AvatarURL string `json:"avatar_url"`
}

func fetchGitHubUser(ctx context.Context, accessToken string) (*gitHubUser, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", "https://api.github.com/user", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Accept", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("github API returned %d", resp.StatusCode)
	}

	var user gitHubUser
	if err := json.NewDecoder(resp.Body).Decode(&user); err != nil {
		return nil, err
	}
	return &user, nil
}

func generateToken() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}
