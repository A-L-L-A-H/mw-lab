package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/A-L-L-A-H/mw-lab/internal/githttp"
	"github.com/A-L-L-A-H/mw-lab/internal/store"
)

func doJSONRequest(t *testing.T, handler http.Handler, method, path string, body any, cookie *http.Cookie,
	bearer string) *httptest.ResponseRecorder {
	t.Helper()
	req, err := authJSON(method, path, body, bearer)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	if cookie != nil {
		req.AddCookie(cookie)
	}
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	return rec
}

func findCookie(rec *httptest.ResponseRecorder, name string) *http.Cookie {
	for _, c := range rec.Result().Cookies() {
		if c.Name == name {
			return c
		}
	}
	return nil
}

func requireStatus(t *testing.T, rec *httptest.ResponseRecorder, want int) {
	t.Helper()
	if rec.Code != want {
		t.Fatalf("status = %d, want %d (%s)", rec.Code, want, rec.Body.String())
	}
}

func requireBodyContains(t *testing.T, rec *httptest.ResponseRecorder, want string) {
	t.Helper()
	if !strings.Contains(rec.Body.String(), want) {
		t.Fatalf("body missing %q: %s", want, rec.Body.String())
	}
}

func TestServerSignupHTTP(t *testing.T) {
	srv, _, _, _ := setupAuthServer(t)
	h := srv.Handler()

	badRec := doJSONRequest(t, h, http.MethodPost, "/api/auth/signup", map[string]string{"username": ""}, nil, "")
	requireStatus(t, badRec, http.StatusBadRequest)

	signupRec := doJSONRequest(t, h, http.MethodPost, "/api/auth/signup", map[string]string{
		"username": "alice", "email": "alice@example.com", "password": "pw1",
	}, nil, "")
	requireStatus(t, signupRec, http.StatusCreated)
	sessCookie := findCookie(signupRec, "mw_lab_session")
	if sessCookie == nil || sessCookie.Value == "" {
		t.Fatal("expected mw_lab_session cookie after signup")
	}

	dupRec := doJSONRequest(t, h, http.MethodPost, "/api/auth/signup", map[string]string{
		"username": "alice", "email": "alice2@example.com", "password": "pw1",
	}, nil, "")
	requireStatus(t, dupRec, http.StatusConflict)
}

func TestServerLoginLogoutHTTP(t *testing.T) {
	srv, _, _, _ := setupAuthServer(t)
	h := srv.Handler()

	signupRec := doJSONRequest(t, h, http.MethodPost, "/api/auth/signup", map[string]string{
		"username": "bob", "email": "bob@example.com", "password": "pw2",
	}, nil, "")
	requireStatus(t, signupRec, http.StatusCreated)

	badLogin := doJSONRequest(t, h, http.MethodPost, "/api/auth/login", map[string]string{
		"username": "bob", "password": "wrong",
	}, nil, "")
	requireStatus(t, badLogin, http.StatusUnauthorized)

	goodLogin := doJSONRequest(t, h, http.MethodPost, "/api/auth/login", map[string]string{
		"username": "bob", "password": "pw2",
	}, nil, "")
	requireStatus(t, goodLogin, http.StatusOK)
	loginCookie := findCookie(goodLogin, "mw_lab_session")
	if loginCookie == nil {
		t.Fatal("expected session cookie on login")
	}

	unauthMe := doJSONRequest(t, h, http.MethodGet, "/api/auth/me", nil, nil, "")
	requireStatus(t, unauthMe, http.StatusUnauthorized)

	logoutRec := doJSONRequest(t, h, http.MethodPost, "/api/auth/logout", nil, loginCookie, "")
	requireStatus(t, logoutRec, http.StatusOK)
	cleared := findCookie(logoutRec, "mw_lab_session")
	if cleared == nil || cleared.MaxAge != -1 {
		t.Fatalf("expected expired cookie on logout, got %+v", cleared)
	}
}

func TestServerPATAuthenticationHTTP(t *testing.T) {
	srv, _, _, _ := setupAuthServer(t)
	h := srv.Handler()

	signupRec := doJSONRequest(t, h, http.MethodPost, "/api/auth/signup", map[string]string{
		"username": "alice", "email": "alice@example.com", "password": "pw1",
	}, nil, "")
	requireStatus(t, signupRec, http.StatusCreated)
	sessCookie := findCookie(signupRec, "mw_lab_session")
	if sessCookie == nil {
		t.Fatal("expected session cookie after signup")
	}

	unauthTok := doJSONRequest(t, h, http.MethodGet, "/api/auth/tokens", nil, nil, "")
	requireStatus(t, unauthTok, http.StatusUnauthorized)

	createTok := doJSONRequest(t, h, http.MethodPost, "/api/auth/tokens", map[string]string{
		"name": "", "scope": "",
	}, sessCookie, "")
	requireStatus(t, createTok, http.StatusCreated)
	var tokReply struct {
		Token string `json:"token"`
	}
	_ = json.Unmarshal(createTok.Body.Bytes(), &tokReply)
	if tokReply.Token == "" {
		t.Fatal("expected generated PAT token")
	}

	listRec := doJSONRequest(t, h, http.MethodGet, "/api/auth/tokens", nil, nil, tokReply.Token)
	requireStatus(t, listRec, http.StatusOK)
	requireBodyContains(t, listRec, `"name":"PAT"`)

	var listBody struct {
		Tokens []struct {
			ID int64 `json:"id"`
		} `json:"tokens"`
	}
	_ = json.Unmarshal(listRec.Body.Bytes(), &listBody)
	if len(listBody.Tokens) != 1 {
		t.Fatalf("expected 1 token, got %d", len(listBody.Tokens))
	}
	tokenPath := fmt.Sprintf("/api/auth/tokens/%d", listBody.Tokens[0].ID)
	requireStatus(t, doJSONRequest(t, h, http.MethodDelete, tokenPath, nil, sessCookie, ""), http.StatusOK)
}

func TestServerExpiredSessionCookieHTTP(t *testing.T) {
	srv, _, labStore, _ := setupAuthServer(t)
	h := srv.Handler()
	ctx := context.Background()

	signupRec := doJSONRequest(t, h, http.MethodPost, "/api/auth/signup", map[string]string{
		"username": "alice", "email": "alice@example.com", "password": "pw1",
	}, nil, "")
	requireStatus(t, signupRec, http.StatusCreated)
	user, err := labStore.GetUserByUsername(ctx, "alice")
	if err != nil {
		t.Fatalf("lookup alice: %v", err)
	}
	_ = labStore.CreateSession(ctx, store.SessionRecord{
		Token:     "expired-tok",
		UserID:    user.ID,
		ExpiresAt: time.Now().Add(-time.Hour).UTC().Format(time.RFC3339Nano),
		CreatedAt: time.Now().UTC().Format(time.RFC3339Nano),
	})
	expiredRec := doJSONRequest(t, h, http.MethodGet, "/api/auth/tokens", nil, &http.Cookie{
		Name: "mw_lab_session", Value: "expired-tok",
	}, "")
	requireStatus(t, expiredRec, http.StatusUnauthorized)
}

func TestServerHandleMeHelper(t *testing.T) {
	srv, _, labStore, _ := setupAuthServer(t)

	admin, err := labStore.CreateUser(context.Background(), store.UserRecord{
		Username: "root", Email: "root@example.com", PasswordHash: "x", IsAdmin: true,
	})
	if err != nil {
		t.Fatalf("create admin user: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/auth/me", nil)
	rec := httptest.NewRecorder()
	c := srv.Echo().NewContext(req, rec)
	c.Set("auth_user_id", admin.ID)
	if err := srv.handleMe(c); err != nil {
		t.Fatalf("handleMe returned error: %v", err)
	}
	requireStatus(t, rec, http.StatusOK)

	recMissing := httptest.NewRecorder()
	cMissing := srv.Echo().NewContext(req, recMissing)
	cMissing.Set("auth_user_id", int64(9999))
	if err := srv.handleMe(cMissing); err != nil {
		t.Fatalf("handleMe returned error for unknown user: %v", err)
	}
	requireStatus(t, recMissing, http.StatusNotFound)
}

func TestServerCheckRepoAccessHelper(t *testing.T) {
	srv, _, labStore, _ := setupAuthServer(t)
	ctx := context.Background()

	admin, _ := labStore.CreateUser(ctx, store.UserRecord{
		Username: "root", Email: "root@example.com", PasswordHash: "x", IsAdmin: true,
	})
	dev, _ := labStore.CreateUser(ctx, store.UserRecord{
		Username: "dev", Email: "dev@example.com", PasswordHash: "x",
	})
	_ = labStore.UpsertRepoMember(ctx, store.RepoMemberRecord{
		RepoOwner: "mw", RepoName: "demo", UserID: dev.ID, Role: "write",
	})

	req := httptest.NewRequest(http.MethodGet, "/api/repos/mw/demo", nil)
	cAdmin := srv.Echo().NewContext(req, httptest.NewRecorder())
	cAdmin.Set("auth_user_id", admin.ID)
	if ok, _ := srv.checkRepoAccess(cAdmin, "mw", "demo", "admin"); !ok {
		t.Fatal("expected admin to have repo access")
	}
	cDev := srv.Echo().NewContext(req, httptest.NewRecorder())
	cDev.Set("auth_user_id", dev.ID)
	if ok, _ := srv.checkRepoAccess(cDev, "mw", "demo", "write"); !ok {
		t.Fatal("expected write member to have write access")
	}
	if ok, _ := srv.checkRepoAccess(cDev, "mw", "demo", "admin"); ok {
		t.Fatal("expected write member NOT to have admin access")
	}
	cAnon := srv.Echo().NewContext(req, httptest.NewRecorder())
	if ok, _ := srv.checkRepoAccess(cAnon, "mw", "demo", "read"); ok {
		t.Fatal("expected unauthenticated caller NOT to have access")
	}
}

func TestServerErrorMappingHelper(t *testing.T) {
	srv, _, _, _ := setupAuthServer(t)

	cases := []struct {
		err  error
		code int
	}{
		{githttp.ErrRepoNotFound, http.StatusNotFound},
		{githttp.ErrInvalidPath, http.StatusBadRequest},
		{githttp.ErrBlobTooLarge, http.StatusRequestEntityTooLarge},
		{errors.New("boom"), http.StatusInternalServerError},
	}
	for _, tc := range cases {
		req := httptest.NewRequest(http.MethodGet, "/api/tree", nil)
		rec := httptest.NewRecorder()
		errCtx := srv.Echo().NewContext(req, rec)
		_ = mapGitInspectError(errCtx, tc.err)
		requireStatus(t, rec, tc.code)
	}
}

func TestServerAccessorHelpers(t *testing.T) {
	srv, _, _, _ := setupAuthServer(t)

	if srv.CIRunner() == nil || srv.CIRunStore() == nil {
		t.Fatal("expected non-nil CIRunner and CIRunStore")
	}
	if got := resolvePlanRoot("  /custom/plans "); got != "/custom/plans" {
		t.Fatalf("unexpected resolvePlanRoot: %q", got)
	}
}

func TestServerRepoHTTP(t *testing.T) {
	srv, _, labStore, _ := setupAuthServer(t)
	h := srv.Handler()

	requireStatus(t, doJSONRequest(t, h, http.MethodGet, "/api/repos/mw/missing", nil, nil, ""), 404)
	requireStatus(t, doJSONRequest(t, h, http.MethodPatch, "/api/repos/mw/missing", map[string]any{}, nil, ""), 404)

	createRec := doJSONRequest(t, h, http.MethodPost, "/api/repos", map[string]any{
		"owner": "mw", "name": "portal", "description": "initial",
	}, nil, "")
	requireStatus(t, createRec, http.StatusCreated)
	_, _ = labStore.UpsertMirror(context.Background(), store.MirrorRecord{
		RepoOwner: "mw", RepoName: "portal", UpstreamURL: "https://example.com/portal.git",
	})

	getRec := doJSONRequest(t, h, http.MethodGet, "/api/repos/mw/portal", nil, nil, "")
	requireStatus(t, getRec, http.StatusOK)
	requireBodyContains(t, getRec, `"mirror"`)

	badBranch := doJSONRequest(t, h, http.MethodPatch, "/api/repos/mw/portal", map[string]any{
		"defaultBranch": "bad branch name",
	}, nil, "")
	requireStatus(t, badBranch, http.StatusBadRequest)

	patchRec := doJSONRequest(t, h, http.MethodPatch, "/api/repos/mw/portal", map[string]any{
		"description": "updated", "defaultBranch": "trunk", "private": true,
	}, nil, "")
	requireStatus(t, patchRec, http.StatusOK)
	requireBodyContains(t, patchRec, `"trunk"`)
}

func TestServerListingEndpointsHTTP(t *testing.T) {
	srv, _, _, _ := setupAuthServer(t)
	h := srv.Handler()

	createRec := doJSONRequest(t, h, http.MethodPost, "/api/repos", map[string]any{
		"owner": "mw", "name": "portal",
	}, nil, "")
	requireStatus(t, createRec, http.StatusCreated)

	paths := []string{
		"/api/repos/mw/portal/branches",
		"/api/repos/mw/portal/events",
		"/api/events",
		"/api/lfs/objects",
		"/api/webhooks",
	}
	for _, path := range paths {
		requireStatus(t, doJSONRequest(t, h, http.MethodGet, path, nil, nil, ""), http.StatusOK)
	}

	noUpstream := doJSONRequest(t, h, http.MethodPost, "/api/repos/mw/portal/mirror", map[string]any{}, nil, "")
	if noUpstream.Code != http.StatusBadRequest && noUpstream.Code != http.StatusBadGateway {
		t.Fatalf("expected 400 or 502 for unconfigured mirror, got %d", noUpstream.Code)
	}
}

func TestServerWebhookHTTP(t *testing.T) {
	srv, _, _, _ := setupAuthServer(t)
	h := srv.Handler()

	badHook := doJSONRequest(t, h, http.MethodPatch, "/api/webhooks/abc", map[string]any{}, nil, "")
	requireStatus(t, badHook, http.StatusBadRequest)
	missHook := doJSONRequest(t, h, http.MethodDelete, "/api/webhooks/999", nil, nil, "")
	requireStatus(t, missHook, http.StatusNotFound)

	hookCreate := doJSONRequest(t, h, http.MethodPost, "/api/webhooks", map[string]any{
		"repoOwner": "mw", "repoName": "portal", "targetUrl": "http://127.0.0.1:9999/hook",
	}, nil, "")
	requireStatus(t, hookCreate, http.StatusCreated)

	hookUpdate := doJSONRequest(t, h, http.MethodPatch, "/api/webhooks/1", map[string]any{
		"targetUrl": "http://127.0.0.1:9999/hook-v2", "events": "push,tag", "active": false,
	}, nil, "")
	requireStatus(t, hookUpdate, http.StatusOK)
	requireBodyContains(t, hookUpdate, `"active":false`)

	hookDelete := doJSONRequest(t, h, http.MethodDelete, "/api/webhooks/1", nil, nil, "")
	requireStatus(t, hookDelete, http.StatusOK)
}
