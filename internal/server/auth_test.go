package server

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/A-L-L-A-H/mw-lab/internal/githttp"
	"github.com/A-L-L-A-H/mw-lab/internal/lfs"
	"github.com/A-L-L-A-H/mw-lab/internal/store"
)

func setupAuthServer(t *testing.T) (*Server, *httptest.Server, *store.Store, *githttp.Manager) {
	t.Helper()
	tempRoot := t.TempDir()
	labStore, err := store.Open(filepath.Join(tempRoot, "auth.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}

	gitMgr, err := githttp.NewManager(filepath.Join(tempRoot, "repos"), labStore, true)
	if err != nil {
		t.Fatalf("new git manager: %v", err)
	}
	localBackend, err := lfs.NewLocalDiskBackend(filepath.Join(tempRoot, "lfs"))
	if err != nil {
		t.Fatalf("new lfs backend: %v", err)
	}
	lfsSvc, err := lfs.NewService(labStore, localBackend, "", 0)
	if err != nil {
		t.Fatalf("new lfs service: %v", err)
	}

	labServer := New(Config{Version: "test-auth"}, labStore, gitMgr, nil, lfsSvc)
	httpSrv := httptest.NewServer(labServer.Handler())
	t.Cleanup(func() {
		httpSrv.Close()
		_ = labStore.Close()
	})
	return labServer, httpSrv, labStore, gitMgr
}

func authJSON(method, url string, body any, authToken string) (*http.Request, error) {
	var bodyReader io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		bodyReader = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, url, bodyReader)
	if err != nil {
		return nil, err
	}
	if authToken != "" {
		req.Header.Set("Authorization", "Bearer "+authToken)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	return req, nil
}

func extractSessionCookie(resp *http.Response) string {
	for _, c := range resp.Cookies() {
		if c.Name == "mw_lab_session" {
			return c.Value
		}
	}
	return ""
}

func TestAuthSignupLoginMeLogout(t *testing.T) {
	_, _, labStore, _ := setupAuthServer(t)

	// Test store-level auth operations
	ctx := context.Background()

	// Create user
	user, err := labStore.CreateUser(ctx, store.UserRecord{
		Username:     "alice",
		Email:        "alice@example.com",
		PasswordHash: "hashedpassword",
		IsAdmin:      true,
	})
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	if user.ID == 0 {
		t.Fatal("expected user ID")
	}

	// Get user
	got, err := labStore.GetUserByUsername(ctx, "alice")
	if err != nil {
		t.Fatalf("get user: %v", err)
	}
	if got.Username != "alice" || !got.IsAdmin {
		t.Fatalf("unexpected user: %+v", got)
	}

	// Create session
	tokenPair := struct {
		Token     string
		ExpiresAt time.Time
	}{Token: "test-session-token", ExpiresAt: time.Now().Add(24 * time.Hour)}
	err = labStore.CreateSession(ctx, store.SessionRecord{
		Token:     tokenPair.Token,
		UserID:    user.ID,
		ExpiresAt: tokenPair.ExpiresAt.UTC().Format(time.RFC3339Nano),
		CreatedAt: time.Now().UTC().Format(time.RFC3339Nano),
	})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}

	// Get session
	session, err := labStore.GetSession(ctx, tokenPair.Token)
	if err != nil {
		t.Fatalf("get session: %v", err)
	}
	if session.UserID != user.ID {
		t.Fatalf("session user ID mismatch: %d != %d", session.UserID, user.ID)
	}

	// Delete session
	err = labStore.DeleteSession(ctx, tokenPair.Token)
	if err != nil {
		t.Fatalf("delete session: %v", err)
	}
	_, err = labStore.GetSession(ctx, tokenPair.Token)
	if err != store.ErrNotFound {
		t.Fatalf("expected ErrNotFound after delete")
	}

	// Count users
	count, err := labStore.CountUsers(ctx)
	if err != nil {
		t.Fatalf("count users: %v", err)
	}
	if count != 1 {
		t.Fatalf("expected 1 user, got %d", count)
	}
}

func TestAuthCreateToken(t *testing.T) {
	_, _, labStore, _ := setupAuthServer(t)
	ctx := context.Background()

	// Create user
	user, err := labStore.CreateUser(ctx, store.UserRecord{
		Username:     "bob",
		Email:        "bob@example.com",
		PasswordHash: "hashedpassword",
		IsAdmin:      false,
	})
	if err != nil {
		t.Fatalf("create user: %v", err)
	}

	// Create access token
	tokenPair := struct {
		Token     string
		ExpiresAt time.Time
	}{Token: "test-access-token-xyz", ExpiresAt: time.Now().Add(24 * time.Hour)}

	_, err = labStore.CreateAccessToken(ctx, store.AccessTokenRecord{
		TokenHash:   tokenPair.Token, // using raw token as hash for simplicity in test
		TokenPrefix: "test-a",
		Name:        "my-pat",
		UserID:      user.ID,
		Scope:       "full",
		ExpiresAt:   tokenPair.ExpiresAt.UTC().Format(time.RFC3339Nano),
		CreatedAt:   time.Now().UTC().Format(time.RFC3339Nano),
	})
	if err != nil {
		t.Fatalf("create access token: %v", err)
	}

	// Get access token by hash
	got, err := labStore.GetAccessTokenByHash(ctx, tokenPair.Token)
	if err != nil {
		t.Fatalf("get access token by hash: %v", err)
	}
	if got.UserID != user.ID || got.Name != "my-pat" {
		t.Fatalf("unexpected token: %+v", got)
	}

	// List tokens
	tokens, err := labStore.ListAccessTokens(ctx, user.ID)
	if err != nil {
		t.Fatalf("list tokens: %v", err)
	}
	if len(tokens) != 1 {
		t.Fatalf("expected 1 token, got %d", len(tokens))
	}

	// Delete token
	err = labStore.DeleteAccessToken(ctx, tokens[0].ID, user.ID)
	if err != nil {
		t.Fatalf("delete token: %v", err)
	}
	tokens, err = labStore.ListAccessTokens(ctx, user.ID)
	if err != nil || len(tokens) != 0 {
		t.Fatalf("expected 0 tokens after delete, got %d", len(tokens))
	}
}

func TestAuthWithoutUserStore(t *testing.T) {
	labServer := New(Config{Version: "test"}, nil, nil, nil, nil)
	httpSrv := httptest.NewServer(labServer.Handler())
	defer httpSrv.Close()

	signupReq, _ := authJSON(http.MethodPost, httpSrv.URL+"/api/auth/signup", map[string]any{
		"username": "x", "email": "x@y.com", "password": "p",
	}, "")
	signupResp, err := httpSrv.Client().Do(signupReq)
	if err != nil {
		t.Fatalf("signup: %v", err)
	}
	defer signupResp.Body.Close()
	if signupResp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 without store, got %d", signupResp.StatusCode)
	}
}

func TestRepoMemberRoleStore(t *testing.T) {
	tempRoot := t.TempDir()
	labStore, err := store.Open(filepath.Join(tempRoot, "members.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer func() { _ = labStore.Close() }()

	ctx := context.Background()

	_, err = labStore.CreateUser(ctx, store.UserRecord{
		Username:     "devuser",
		Email:        "dev@example.com",
		PasswordHash: "hash",
		IsAdmin:      false,
	})
	if err != nil {
		t.Fatalf("create user: %v", err)
	}

	u, err := labStore.GetUserByUsername(ctx, "devuser")
	if err != nil {
		t.Fatalf("get user: %v", err)
	}

	err = labStore.UpsertRepoMember(ctx, store.RepoMemberRecord{
		RepoOwner: "alloy", RepoName: "signal", UserID: u.ID, Role: "write",
	})
	if err != nil {
		t.Fatalf("upsert member: %v", err)
	}

	role, err := labStore.GetRepoMemberRole(ctx, "alloy", "signal", u.ID)
	if err != nil {
		t.Fatalf("get member role: %v", err)
	}
	if role != "write" {
		t.Fatalf("expected write role, got %s", role)
	}

	err = labStore.UpsertRepoMember(ctx, store.RepoMemberRecord{
		RepoOwner: "alloy", RepoName: "signal", UserID: u.ID, Role: "admin",
	})
	if err != nil {
		t.Fatalf("upsert member admin: %v", err)
	}
	role, err = labStore.GetRepoMemberRole(ctx, "alloy", "signal", u.ID)
	if err != nil {
		t.Fatalf("get member role after update: %v", err)
	}
	if role != "admin" {
		t.Fatalf("expected admin role, got %s", role)
	}

	_, err = labStore.GetRepoMemberRole(ctx, "alloy", "signal", 999)
	if err != store.ErrNotFound {
		t.Fatalf("expected ErrNotFound for missing member, got %v", err)
	}
}