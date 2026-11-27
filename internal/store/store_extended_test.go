package store

import (
	"context"
	"errors"
	"testing"
	"time"
)

func openMemoryStore(t *testing.T) *Store {
	t.Helper()
	st, err := Open(":memory:")
	if err != nil {
		t.Fatalf("open memory store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

func TestStoreOpenAndLifecycle(t *testing.T) {
	if _, err := Open("   "); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("expected ErrInvalidInput for blank path, got %v", err)
	}
	var nilStore *Store
	if err := nilStore.Close(); err != nil {
		t.Fatalf("expected nil.Close() == nil, got %v", err)
	}
	st := openMemoryStore(t)
	if st.DB() == nil {
		t.Fatal("expected non-nil *sql.DB from DB()")
	}
}

func TestStoreRepoOperationsExtended(t *testing.T) {
	st := openMemoryStore(t)
	ctx := context.Background()

	if _, err := st.CreateRepo(ctx, RepoRecord{Owner: "", Name: "x"}); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("expected ErrInvalidInput on missing owner, got %v", err)
	}
	_, err := st.CreateRepo(ctx, RepoRecord{Owner: "mw", Name: "alpha"})
	if err != nil {
		t.Fatalf("create alpha: %v", err)
	}
	_, err = st.CreateRepo(ctx, RepoRecord{Owner: "other", Name: "beta"})
	if err != nil {
		t.Fatalf("create beta: %v", err)
	}

	all, err := st.ListRepos(ctx, "")
	if err != nil || len(all) != 2 {
		t.Fatalf("expected 2 repos, got len=%d err=%v", len(all), err)
	}
	filtered, err := st.ListRepos(ctx, "mw")
	if err != nil || len(filtered) != 1 || filtered[0].Name != "alpha" {
		t.Fatalf("unexpected filtered repos: %+v err=%v", filtered, err)
	}

	if err := st.TouchRepoSync(ctx, "mw", "alpha", "https://example.com/alpha.git"); err != nil {
		t.Fatalf("TouchRepoSync failed: %v", err)
	}
	got, _ := st.GetRepo(ctx, "mw", "alpha")
	if got.MirrorURL != "https://example.com/alpha.git" || got.LastSyncAt == "" {
		t.Fatalf("expected mirror URL and sync timestamp set: %+v", got)
	}
	if err := st.TouchRepoSync(ctx, "mw", "missing", ""); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound on missing TouchRepoSync, got %v", err)
	}
	if err := st.DeleteRepo(ctx, "mw", "missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound on missing DeleteRepo, got %v", err)
	}
}

func TestStoreMirrorsExtended(t *testing.T) {
	st := openMemoryStore(t)
	ctx := context.Background()

	if _, err := st.UpsertMirror(ctx, MirrorRecord{RepoOwner: "mw", RepoName: "alpha"}); !errors.Is(
		err, ErrInvalidInput,
	) {
		t.Fatalf("expected ErrInvalidInput on missing upstreamUrl, got %v", err)
	}
	if _, err := st.GetMirror(ctx, "mw", "missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound on missing mirror, got %v", err)
	}

	saved, err := st.UpsertMirror(ctx, MirrorRecord{
		RepoOwner:   "mw",
		RepoName:    "alpha",
		UpstreamURL: "https://example.com/up.git",
	})
	if err != nil || saved.Direction != "pull" || saved.LastStatus != "idle" {
		t.Fatalf("unexpected mirror record: %+v err=%v", saved, err)
	}
	mirrors, err := st.ListMirrors(ctx)
	if err != nil || len(mirrors) != 1 {
		t.Fatalf("expected 1 mirror, got len=%d err=%v", len(mirrors), err)
	}
}

func TestStoreLFSObjectsExtended(t *testing.T) {
	st := openMemoryStore(t)
	ctx := context.Background()

	if _, err := st.UpsertLFSObject(ctx, LFSObjectRecord{}); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("expected ErrInvalidInput for empty OID, got %v", err)
	}
	if _, err := st.GetLFSObject(ctx, "missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}

	_, err := st.UpsertLFSObject(ctx, LFSObjectRecord{
		OID:       "sha-1",
		RepoOwner: "mw",
		RepoName:  "alpha",
		Size:      10,
		Backend:   "local",
	})
	if err != nil {
		t.Fatalf("upsert lfs: %v", err)
	}
	updated, err := st.UpsertLFSObject(ctx, LFSObjectRecord{
		OID:        "sha-1",
		StorageRef: "s/h/sha-1",
		Uploaded:   true,
	})
	if err != nil || !updated.Uploaded || updated.StorageRef != "s/h/sha-1" || updated.Size != 10 {
		t.Fatalf("unexpected updated lfs object: %+v err=%v", updated, err)
	}

	items, err := st.ListLFSObjects(ctx, "mw", "alpha", 0)
	if err != nil || len(items) != 1 {
		t.Fatalf("expected 1 lfs item, got len=%d err=%v", len(items), err)
	}
}

func TestStoreWebhooksAndPushEventsExtended(t *testing.T) {
	st := openMemoryStore(t)
	ctx := context.Background()

	if _, err := st.CreateWebhook(ctx, WebhookRecord{}); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("expected ErrInvalidInput for empty targetUrl, got %v", err)
	}
	if _, err := st.GetWebhook(ctx, 999); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}

	hook, err := st.CreateWebhook(ctx, WebhookRecord{
		TargetURL: "https://hook.local/a",
		Events:    "push,tag",
		Active:    true,
	})
	if err != nil || hook.RepoOwner != "*" || hook.RepoName != "*" {
		t.Fatalf("unexpected default wildcard webhook: %+v err=%v", hook, err)
	}

	listed, err := st.ListWebhooks(ctx, "mw", "alpha")
	if err != nil || len(listed) != 1 {
		t.Fatalf("expected 1 wildcard webhook, got len=%d err=%v", len(listed), err)
	}

	matchedAll, err := st.MatchWebhooks(ctx, "mw", "alpha", "")
	if err != nil || len(matchedAll) != 1 {
		t.Fatalf("expected match on empty target event, got len=%d err=%v", len(matchedAll), err)
	}
	matchedNone, err := st.MatchWebhooks(ctx, "mw", "alpha", "release")
	if err != nil || len(matchedNone) != 0 {
		t.Fatalf("expected no match on release event, got len=%d err=%v", len(matchedNone), err)
	}

	upd, err := st.UpdateWebhook(ctx, hook.ID, WebhookRecord{
		RepoOwner: "mw",
		RepoName:  "alpha",
		TargetURL: "https://hook.local/b",
		Secret:    "sec",
		Events:    "*",
		Active:    true,
	})
	if err != nil || upd.TargetURL != "https://hook.local/b" {
		t.Fatalf("unexpected updated webhook: %+v err=%v", upd, err)
	}
	if _, err := st.UpdateWebhook(ctx, 999, WebhookRecord{}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound on update missing hook, got %v", err)
	}
	if err := st.DeleteWebhook(ctx, hook.ID); err != nil {
		t.Fatalf("delete webhook failed: %v", err)
	}
	if err := st.DeleteWebhook(ctx, hook.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound on second delete, got %v", err)
	}

	if _, err := st.RecordPushEvent(ctx, PushEventRecord{RepoOwner: "mw", RepoName: "alpha", RefName: "refs/heads/main"}); err != nil {
		t.Fatalf("record push event: %v", err)
	}
	events, err := st.ListPushEvents(ctx, "mw", "alpha", 0)
	if err != nil || len(events) != 1 {
		t.Fatalf("expected 1 push event, got len=%d err=%v", len(events), err)
	}
}

func TestStoreUsersSessionsTokensAndMembers(t *testing.T) {
	st := openMemoryStore(t)
	ctx := context.Background()

	u, err := st.CreateUser(ctx, UserRecord{
		Username:     "carol",
		Email:        "carol@example.com",
		PasswordHash: "h1",
		IsAdmin:      true,
	})
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	if _, err := st.CreateUser(ctx, UserRecord{
		Username: "carol", Email: "carol2@example.com", PasswordHash: "h2",
	}); !errors.Is(err, ErrAlreadyExists) {
		t.Fatalf("expected ErrAlreadyExists, got %v", err)
	}
	if _, err := st.GetUserByUsername(ctx, "nobody"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound by username, got %v", err)
	}
	if gotByID, err := st.GetUserByID(ctx, u.ID); err != nil || !gotByID.IsAdmin {
		t.Fatalf("unexpected GetUserByID result: %+v err=%v", gotByID, err)
	}
	if _, err := st.GetUserByID(ctx, 9999); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound by id, got %v", err)
	}
	if n, err := st.CountUsers(ctx); err != nil || n != 1 {
		t.Fatalf("expected 1 user, got n=%d err=%v", n, err)
	}

	exp := time.Now().Add(time.Hour).UTC().Format(time.RFC3339Nano)
	if err := st.CreateSession(ctx, SessionRecord{Token: "s1", UserID: u.ID, ExpiresAt: exp, CreatedAt: exp}); err != nil {
		t.Fatalf("create session: %v", err)
	}
	if sess, err := st.GetSession(ctx, "s1"); err != nil || sess.UserID != u.ID {
		t.Fatalf("get session failed: %+v err=%v", sess, err)
	}
	_ = st.DeleteSession(ctx, "s1")
	if _, err := st.GetSession(ctx, "s1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound after delete session, got %v", err)
	}

	tok, err := st.CreateAccessToken(ctx, AccessTokenRecord{
		TokenHash: "h1", TokenPrefix: "pre", Name: "pat", UserID: u.ID, Scope: "full", ExpiresAt: exp,
	})
	if err != nil {
		t.Fatalf("create access token: %v", err)
	}
	if gotTok, err := st.GetAccessTokenByHash(ctx, "h1"); err != nil || gotTok.ID != tok.ID {
		t.Fatalf("get token failed: %+v err=%v", gotTok, err)
	}
	if _, err := st.GetAccessTokenByHash(ctx, "missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound for missing hash, got %v", err)
	}
	if toks, err := st.ListAccessTokens(ctx, u.ID); err != nil || len(toks) != 1 {
		t.Fatalf("expected 1 token, got len=%d err=%v", len(toks), err)
	}
	if err := st.DeleteAccessToken(ctx, tok.ID, u.ID); err != nil {
		t.Fatalf("delete access token: %v", err)
	}

	if err := st.UpsertRepoMember(ctx, RepoMemberRecord{
		RepoOwner: "mw", RepoName: "alpha", UserID: u.ID, Role: "write",
	}); err != nil {
		t.Fatalf("upsert repo member: %v", err)
	}
	if role, err := st.GetRepoMemberRole(ctx, "mw", "alpha", u.ID); err != nil || role != "write" {
		t.Fatalf("expected write role, got %q err=%v", role, err)
	}
	if _, err := st.GetRepoMemberRole(ctx, "mw", "alpha", 999); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound for missing member, got %v", err)
	}
}
