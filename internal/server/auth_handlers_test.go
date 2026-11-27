package server

import (
	"context"
	"encoding/json"
	"errors"
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

func TestServerSignupLoginLogoutAndTokensHTTP(t *testing.T) {
	srv, _, labStore, _ := setupAuthServer(t)
	h := srv.Handler()

	badRec := doJSONRequest(t, h, http.MethodPost, "/api/auth/signup", map[string]string{"username": ""}, nil, "")
	if badRec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for incomplete signup, got %d", badRec.Code)
	}

	signupRec := doJSONRequest(t, h, http.MethodPost, "/api/auth/signup", map[string]string{
		"username": "alice", "email": "alice@example.com", "password": "pw1",
	}, nil, "")
	if signupRec.Code != http.StatusCreated {
		t.Fatalf("expected 201 signup, got %d (%s)", signupRec.Code, signupRec.Body.String())
	}
	sessCookie := findCookie(signupRec, "mw_lab_session")
	if sessCookie == nil || sessCookie.Value == "" {
		t.Fatal("expected mw_lab_session cookie after signup")
	}

	dupRec := doJSONRequest(t, h, http.MethodPost, "/api/auth/signup", map[string]string{
		"username": "alice", "email": "alice2@example.com", "password": "pw1",
	}, nil, "")
	if dupRec.Code != http.StatusConflict {
		t.Fatalf("expected 409 duplicate signup, got %d", dupRec.Code)
	}

	badLogin := doJSONRequest(t, h, http.MethodPost, "/api/auth/login", map[string]string{
		"username": "alice", "password": "wrong",
	}, nil, "")
	if badLogin.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 bad password, got %d", badLogin.Code)
	}

	goodLogin := doJSONRequest(t, h, http.MethodPost, "/api/auth/login", map[string]string{
		"username": "alice", "password": "pw1",
	}, nil, "")
	if goodLogin.Code != http.StatusOK {
		t.Fatalf("expected 200 login, got %d", goodLogin.Code)
	}
	loginCookie := findCookie(goodLogin, "mw_lab_session")
	if loginCookie == nil {
		t.Fatal("expected session cookie on login")
	}

	unauthMe := doJSONRequest(t, h, http.MethodGet, "/api/auth/me", nil, nil, "")
	if unauthMe.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 on /api/auth/me without context user, got %d", unauthMe.Code)
	}

	unauthTok := doJSONRequest(t, h, http.MethodGet, "/api/auth/tokens", nil, nil, "")
	if unauthTok.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 on /api/auth/tokens, got %d", unauthTok.Code)
	}

	createTok := doJSONRequest(t, h, http.MethodPost, "/api/auth/tokens", map[string]string{
		"name": "", "scope": "",
	}, loginCookie, "")
	if createTok.Code != http.StatusCreated {
		t.Fatalf("expected 201 create token, got %d (%s)", createTok.Code, createTok.Body.String())
	}
	var tokReply struct {
		Token string `json:"token"`
	}
	_ = json.Unmarshal(createTok.Body.Bytes(), &tokReply)
	if tokReply.Token == "" {
		t.Fatal("expected generated PAT token")
	}

	listRec := doJSONRequest(t, h, http.MethodGet, "/api/auth/tokens", nil, nil, tokReply.Token)
	if listRec.Code != http.StatusOK || !strings.Contains(listRec.Body.String(), `"name":"PAT"`) {
		t.Fatalf("expected token list via PAT bearer auth, got %d (%s)", listRec.Code, listRec.Body.String())
	}

	var listBody struct {
		Tokens []struct {
			ID int64 `json:"id"`
		} `json:"tokens"`
	}
	_ = json.Unmarshal(listRec.Body.Bytes(), &listBody)
	if len(listBody.Tokens) != 1 {
		t.Fatalf("expected 1 token, got %d", len(listBody.Tokens))
	}

	delTok := doJSONRequest(t, h, http.MethodDelete, "/api/auth/tokens/1", nil, loginCookie, "")
	if delTok.Code != http.StatusOK {
		t.Fatalf("expected 200 delete token, got %d", delTok.Code)
	}

	_ = labStore.CreateSession(context.Background(), store.SessionRecord{
		Token: "expired-tok", UserID: 1,
		ExpiresAt: time.Now().Add(-time.Hour).UTC().Format(time.RFC3339Nano),
		CreatedAt: time.Now().UTC().Format(time.RFC3339Nano),
	})
	expiredRec := doJSONRequest(t, h, http.MethodGet, "/api/auth/tokens", nil, &http.Cookie{
		Name: "mw_lab_session", Value: "expired-tok",
	}, "")
	if expiredRec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 on expired session cookie, got %d", expiredRec.Code)
	}

	logoutRec := doJSONRequest(t, h, http.MethodPost, "/api/auth/logout", nil, loginCookie, "")
	if logoutRec.Code != http.StatusOK {
		t.Fatalf("expected 200 logout, got %d", logoutRec.Code)
	}
	cleared := findCookie(logoutRec, "mw_lab_session")
	if cleared == nil || cleared.MaxAge != -1 {
		t.Fatalf("expected expired cookie on logout, got %+v", cleared)
	}
}

func TestServerHelpersAccessAndErrorMapping(t *testing.T) {
	srv, _, labStore, _ := setupAuthServer(t)
	ctx := context.Background()

	adminUser, _ := labStore.CreateUser(ctx, store.UserRecord{
		Username: "root", Email: "root@example.com", PasswordHash: "x", IsAdmin: true,
	})
	devUser, _ := labStore.CreateUser(ctx, store.UserRecord{
		Username: "dev", Email: "dev@example.com", PasswordHash: "x", IsAdmin: false,
	})
	_ = labStore.UpsertRepoMember(ctx, store.RepoMemberRecord{
		RepoOwner: "mw", RepoName: "demo", UserID: devUser.ID, Role: "write",
	})

	req := httptest.NewRequest(http.MethodGet, "/api/auth/me", nil)
	rec := httptest.NewRecorder()
	c := srv.Echo().NewContext(req, rec)
	c.Set("auth_user_id", adminUser.ID)
	if err := srv.handleMe(c); err != nil || rec.Code != http.StatusOK {
		t.Fatalf("expected 200 from handleMe, code=%d err=%v", rec.Code, err)
	}

	recMissing := httptest.NewRecorder()
	cMissing := srv.Echo().NewContext(req, recMissing)
	cMissing.Set("auth_user_id", int64(9999))
	if err := srv.handleMe(cMissing); err != nil || recMissing.Code != http.StatusNotFound {
		t.Fatalf("expected 404 from handleMe on unknown user, code=%d err=%v", recMissing.Code, err)
	}

	if ok, _ := srv.checkRepoAccess(c, "mw", "demo", "admin"); !ok {
		t.Fatal("expected admin to have repo access")
	}
	cDev := srv.Echo().NewContext(req, httptest.NewRecorder())
	cDev.Set("auth_user_id", devUser.ID)
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
		errRec := httptest.NewRecorder()
		errCtx := srv.Echo().NewContext(req, errRec)
		_ = mapGitInspectError(errCtx, tc.err)
		if errRec.Code != tc.code {
			t.Fatalf("mapGitInspectError(%v) = %d, want %d", tc.err, errRec.Code, tc.code)
		}
	}

	if srv.CIRunner() == nil || srv.CIRunStore() == nil {
		t.Fatal("expected non-nil CIRunner and CIRunStore")
	}
	if got := resolvePlanRoot("  /custom/plans "); got != "/custom/plans" {
		t.Fatalf("unexpected resolvePlanRoot: %q", got)
	}
}

func TestServerRepoWebhooksAndLFSEndpoints(t *testing.T) {
	srv, _, labStore, _ := setupAuthServer(t)
	h := srv.Handler()

	if rec := doJSONRequest(t, h, http.MethodGet, "/api/repos/mw/missing", nil, nil, ""); rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404 missing repo, got %d", rec.Code)
	}
	if rec := doJSONRequest(t, h, http.MethodPatch, "/api/repos/mw/missing", map[string]any{}, nil, ""); rec.Code !=
		http.StatusNotFound {
		t.Fatalf("expected 404 patch missing repo, got %d", rec.Code)
	}

	createRec := doJSONRequest(t, h, http.MethodPost, "/api/repos", map[string]any{
		"owner": "mw", "name": "portal", "description": "initial",
	}, nil, "")
	if createRec.Code != http.StatusCreated {
		t.Fatalf("create repo failed: %d", createRec.Code)
	}
	_, _ = labStore.UpsertMirror(context.Background(), store.MirrorRecord{
		RepoOwner: "mw", RepoName: "portal", UpstreamURL: "https://example.com/portal.git",
	})

	getRec := doJSONRequest(t, h, http.MethodGet, "/api/repos/mw/portal", nil, nil, "")
	if getRec.Code != http.StatusOK || !strings.Contains(getRec.Body.String(), `"mirror"`) {
		t.Fatalf("expected 200 with mirror info, got %d (%s)", getRec.Code, getRec.Body.String())
	}

	badBranch := doJSONRequest(t, h, http.MethodPatch, "/api/repos/mw/portal", map[string]any{
		"defaultBranch": "bad branch name",
	}, nil, "")
	if badBranch.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 on invalid defaultBranch, got %d", badBranch.Code)
	}

	patchRec := doJSONRequest(t, h, http.MethodPatch, "/api/repos/mw/portal", map[string]any{
		"description": "updated", "defaultBranch": "trunk", "private": true,
	}, nil, "")
	if patchRec.Code != http.StatusOK || !strings.Contains(patchRec.Body.String(), `"trunk"`) {
		t.Fatalf("expected 200 updated repo, got %d (%s)", patchRec.Code, patchRec.Body.String())
	}

	for _, path := range []string{
		"/api/repos/mw/portal/branches",
		"/api/repos/mw/portal/events",
		"/api/events",
		"/api/lfs/objects",
		"/api/webhooks",
	} {
		rec := doJSONRequest(t, h, http.MethodGet, path, nil, nil, "")
		if rec.Code != http.StatusOK {
			t.Fatalf("GET %s returned %d (%s)", path, rec.Code, rec.Body.String())
		}
	}

	noUpstream := doJSONRequest(t, h, http.MethodPost, "/api/repos/mw/portal/mirror", map[string]any{}, nil, "")
	if noUpstream.Code != http.StatusBadGateway && noUpstream.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 or 502 for unconfigured/unreachable mirror, got %d", noUpstream.Code)
	}

	badHook := doJSONRequest(t, h, http.MethodPatch, "/api/webhooks/abc", map[string]any{}, nil, "")
	if badHook.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 bad webhook id, got %d", badHook.Code)
	}
	missHook := doJSONRequest(t, h, http.MethodDelete, "/api/webhooks/999", nil, nil, "")
	if missHook.Code != http.StatusNotFound {
		t.Fatalf("expected 404 delete missing webhook, got %d", missHook.Code)
	}

	hookCreate := doJSONRequest(t, h, http.MethodPost, "/api/webhooks", map[string]any{
		"repoOwner": "mw", "repoName": "portal", "targetUrl": "http://127.0.0.1:9999/hook",
	}, nil, "")
	if hookCreate.Code != http.StatusCreated {
		t.Fatalf("create webhook failed: %d", hookCreate.Code)
	}
	hookUpdate := doJSONRequest(t, h, http.MethodPatch, "/api/webhooks/1", map[string]any{
		"targetUrl": "http://127.0.0.1:9999/hook-v2", "events": "push,tag", "active": false,
	}, nil, "")
	if hookUpdate.Code != http.StatusOK || !strings.Contains(hookUpdate.Body.String(), `"active":false`) {
		t.Fatalf("update webhook failed: %d (%s)", hookUpdate.Code, hookUpdate.Body.String())
	}
	hookDelete := doJSONRequest(t, h, http.MethodDelete, "/api/webhooks/1", nil, nil, "")
	if hookDelete.Code != http.StatusOK {
		t.Fatalf("delete webhook failed: %d", hookDelete.Code)
	}
}
