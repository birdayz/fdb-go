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
	"github.com/birdayz/fdb-record-layer-go/examples/metrognome/internal/storage"
)

const sessionMaxAge = 86400 * 30 // 30 days

type contextKey struct{}

// Handler manages GitHub OAuth login, sessions, and provides auth middleware.
type Handler struct {
	oauth       *oauth2.Config
	db          *storage.DB
	frontendURL string
	states      map[string]time.Time
	mu          sync.Mutex
}

// NewHandler creates a new auth handler.
func NewHandler(cfg *metrognomev1.GitHubOAuth, frontendURL string, db *storage.DB) *Handler {
	h := &Handler{
		oauth: &oauth2.Config{
			ClientID:     cfg.GetClientId(),
			ClientSecret: cfg.GetClientSecret(),
			RedirectURL:  cfg.GetRedirectUrl(),
			Scopes:       []string{"read:user", "user:email"},
			Endpoint:     github.Endpoint,
		},
		db:          db,
		frontendURL: frontendURL,
		states:      make(map[string]time.Time),
	}
	go h.cleanupStates()
	return h
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
	h.mu.Lock()
	h.states[state] = time.Now().Add(10 * time.Minute)
	h.mu.Unlock()

	url := h.oauth.AuthCodeURL(state)
	http.Redirect(w, r, url, http.StatusTemporaryRedirect)
}

func (h *Handler) handleCallback(w http.ResponseWriter, r *http.Request) {
	state := r.URL.Query().Get("state")
	h.mu.Lock()
	expiry, ok := h.states[state]
	if ok {
		delete(h.states, state)
	}
	h.mu.Unlock()

	if !ok || time.Now().After(expiry) {
		http.Error(w, "invalid or expired state", http.StatusBadRequest)
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

	// Fetch GitHub user info
	ghUser, err := fetchGitHubUser(r.Context(), tok.AccessToken)
	if err != nil {
		slog.Error("github user fetch failed", "error", err)
		http.Error(w, "github error", http.StatusInternalServerError)
		return
	}

	now := time.Now().UnixMilli()
	userID := fmt.Sprintf("gh_%d", ghUser.ID)

	// Upsert user
	user := &storev1.User{
		Id:        proto.String(userID),
		GithubId:  proto.String(fmt.Sprintf("%d", ghUser.ID)),
		Login:     proto.String(ghUser.Login),
		Name:      proto.String(ghUser.Name),
		AvatarUrl: proto.String(ghUser.AvatarURL),
		Email:     proto.String(ghUser.Email),
		CreatedAt: proto.Int64(now),
	}
	if err := h.db.Users().Create(r.Context(), user); err != nil {
		// Already exists — that's fine, just update
		_ = h.db.Users().Save(r.Context(), user)
	}

	// Create session
	sessionID := generateToken()
	session := &storev1.Session{
		Id:        proto.String(sessionID),
		UserId:    proto.String(userID),
		CreatedAt: proto.Int64(now),
		ExpiresAt: proto.Int64(now + int64(sessionMaxAge)*1000),
	}
	if err := h.db.Sessions().Create(r.Context(), session); err != nil {
		slog.Error("session create failed", "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	http.SetCookie(w, &http.Cookie{
		Name:     "session",
		Value:    sessionID,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   sessionMaxAge,
	})

	slog.Info("github login", "user", ghUser.Login, "id", ghUser.ID)

	// Redirect to frontend
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
	user, err := h.resolveUser(r)
	if err != nil {
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

func (h *Handler) resolveUser(r *http.Request) (*storev1.User, error) {
	cookie, err := r.Cookie("session")
	if err != nil {
		return nil, errors.New("no session cookie")
	}
	return h.ResolveSession(r.Context(), cookie.Value)
}

// ResolveSession looks up a session and returns the user.
func (h *Handler) ResolveSession(ctx context.Context, sessionID string) (*storev1.User, error) {
	session, err := h.db.Sessions().Get(ctx, sessionID)
	if err != nil {
		return nil, err
	}
	if session.GetExpiresAt() > 0 && session.GetExpiresAt() < time.Now().UnixMilli() {
		return nil, errors.New("session expired")
	}
	return h.db.Users().Get(ctx, session.GetUserId())
}

func (h *Handler) cleanupStates() {
	ticker := time.NewTicker(5 * time.Minute)
	defer ticker.Stop()
	for range ticker.C {
		h.mu.Lock()
		now := time.Now()
		for k, exp := range h.states {
			if now.After(exp) {
				delete(h.states, k)
			}
		}
		h.mu.Unlock()
	}
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
		user, err := a.resolveFromHeaders(ctx, req.Header())
		if err != nil {
			return nil, connect.NewError(connect.CodeUnauthenticated, errors.New("not authenticated"))
		}
		ctx = context.WithValue(ctx, contextKey{}, user)
		return next(ctx, req)
	}
}

func (a *authInterceptor) WrapStreamingClient(next connect.StreamingClientFunc) connect.StreamingClientFunc {
	return next
}

func (a *authInterceptor) WrapStreamingHandler(next connect.StreamingHandlerFunc) connect.StreamingHandlerFunc {
	return func(ctx context.Context, conn connect.StreamingHandlerConn) error {
		user, err := a.resolveFromHeaders(ctx, conn.RequestHeader())
		if err != nil {
			return connect.NewError(connect.CodeUnauthenticated, errors.New("not authenticated"))
		}
		ctx = context.WithValue(ctx, contextKey{}, user)
		return next(ctx, conn)
	}
}

func (a *authInterceptor) resolveFromHeaders(ctx context.Context, headers http.Header) (*storev1.User, error) {
	// Try API key auth first (Authorization: Bearer mgn_...)
	if authHeader := headers.Get("Authorization"); strings.HasPrefix(authHeader, "Bearer mgn_") {
		return a.resolveFromAPIKey(ctx, strings.TrimPrefix(authHeader, "Bearer "))
	}

	// Fall back to session cookie
	raw := headers.Get("Cookie")
	if raw == "" {
		return nil, errors.New("no cookie")
	}
	fakeReq := &http.Request{Header: http.Header{"Cookie": {raw}}}
	cookie, err := fakeReq.Cookie("session")
	if err != nil {
		return nil, err
	}
	return a.handler.ResolveSession(ctx, cookie.Value)
}

func (a *authInterceptor) resolveFromAPIKey(ctx context.Context, rawKey string) (*storev1.User, error) {
	h := sha256.Sum256([]byte(rawKey))
	keyHash := hex.EncodeToString(h[:])
	apiKey, err := a.handler.db.ApiKeys().GetByKeyHash(ctx, keyHash)
	if err != nil {
		return nil, errors.New("invalid api key")
	}
	if apiKey.GetRevoked() {
		return nil, errors.New("api key revoked")
	}
	// Return a synthetic user for API key access
	return &storev1.User{
		Id:    proto.String("apikey:" + apiKey.GetId()),
		Login: proto.String("api:" + apiKey.GetName()),
		Name:  proto.String(apiKey.GetName()),
	}, nil
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
