package server

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/A-L-L-A-H/mw-lab/internal/ci"
	"github.com/A-L-L-A-H/mw-lab/internal/githttp"
	"github.com/A-L-L-A-H/mw-lab/internal/lfs"
	"github.com/A-L-L-A-H/mw-lab/internal/store"
)

func execGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_TERMINAL_PROMPT=0",
		"GIT_AUTHOR_NAME=Server Test",
		"GIT_AUTHOR_EMAIL=server@moonwitness.local",
		"GIT_COMMITTER_NAME=Server Test",
		"GIT_COMMITTER_EMAIL=server@moonwitness.local",
	)
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v failed: %v\n%s", args, err, string(output))
	}
}

func TestServerEndToEndAPIAndDashboard(t *testing.T) {
	tempRoot := t.TempDir()
	labStore, err := store.Open(filepath.Join(tempRoot, "mw-lab.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer func() { _ = labStore.Close() }()

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
	dispatcher := githttp.NewWebhookDispatcher(labStore, nil)

	labServer := New(Config{Version: "test-1.0"}, labStore, gitMgr, dispatcher, lfsSvc)
	httpSrv := httptest.NewServer(labServer.Handler())
	defer httpSrv.Close()

	// 1. Dashboard at "/" renders RIOT HTML.
	dashResp, err := httpSrv.Client().Get(httpSrv.URL + "/")
	if err != nil {
		t.Fatalf("get dashboard: %v", err)
	}
	dashBody, _ := io.ReadAll(dashResp.Body)
	_ = dashResp.Body.Close()
	if dashResp.StatusCode != http.StatusOK || !strings.Contains(string(dashBody), "MW-LAB GIT") {
		t.Fatalf("unexpected dashboard response: status=%d", dashResp.StatusCode)
	}

	// 2. Health check endpoint.
	healthResp, err := httpSrv.Client().Get(httpSrv.URL + "/health")
	if err != nil {
		t.Fatalf("get health: %v", err)
	}
	var healthPayload map[string]any
	_ = json.NewDecoder(healthResp.Body).Decode(&healthPayload)
	_ = healthResp.Body.Close()
	if healthResp.StatusCode != http.StatusOK || healthPayload["status"] != "ok" || healthPayload["lfsBackend"] != "local" {
		t.Fatalf("unexpected health payload: %+v", healthPayload)
	}

	// 3. Create repository via POST /api/repos.
	createResp, err := httpSrv.Client().Post(httpSrv.URL+"/api/repos", "application/json", bytes.NewBufferString(`{
		"owner": "moonwitness",
		"name": "sovereign-demo",
		"description": "E2E API repo"
	}`))
	if err != nil {
		t.Fatalf("post /api/repos: %v", err)
	}
	_ = createResp.Body.Close()
	if createResp.StatusCode != http.StatusCreated {
		t.Fatalf("expected 201 on create repo, got %d", createResp.StatusCode)
	}

	// 4. Push real commit via Git Smart HTTP, then inspect commits, tree, and blob via REST API.
	workDir := filepath.Join(tempRoot, "client-work")
	if err := os.MkdirAll(filepath.Join(workDir, "docs"), 0o755); err != nil {
		t.Fatalf("mkdir workDir: %v", err)
	}
	execGit(t, workDir, "init", "-b", "main")
	if err := os.WriteFile(filepath.Join(workDir, "docs", "manifest.txt"), []byte("sovereign-manifest-v1\n"), 0o644); err != nil {
		t.Fatalf("write manifest: %v", err)
	}
	execGit(t, workDir, "add", ".")
	execGit(t, workDir, "commit", "-m", "seed manifest")
	execGit(t, workDir, "remote", "add", "origin", httpSrv.URL+"/moonwitness/sovereign-demo.git")
	execGit(t, workDir, "push", "-u", "origin", "main")
	dispatcher.Wait()

	commitsResp, err := httpSrv.Client().Get(httpSrv.URL + "/api/repos/moonwitness/sovereign-demo/commits")
	if err != nil {
		t.Fatalf("get commits: %v", err)
	}
	var commitsBody struct {
		Commits []githttp.CommitInfo `json:"commits"`
		Count   int                  `json:"count"`
	}
	_ = json.NewDecoder(commitsResp.Body).Decode(&commitsBody)
	_ = commitsResp.Body.Close()
	if commitsResp.StatusCode != http.StatusOK || commitsBody.Count != 1 || commitsBody.Commits[0].Subject != "seed manifest" {
		t.Fatalf("unexpected commits reply: status=%d body=%+v", commitsResp.StatusCode, commitsBody)
	}

	treeResp, err := httpSrv.Client().Get(httpSrv.URL + "/api/repos/moonwitness/sovereign-demo/tree?path=docs")
	if err != nil {
		t.Fatalf("get tree: %v", err)
	}
	var treeBody struct {
		Entries []githttp.TreeEntry `json:"entries"`
		Count   int                 `json:"count"`
	}
	_ = json.NewDecoder(treeResp.Body).Decode(&treeBody)
	_ = treeResp.Body.Close()
	if treeResp.StatusCode != http.StatusOK || treeBody.Count != 1 || treeBody.Entries[0].Path != "docs/manifest.txt" {
		t.Fatalf("unexpected tree reply: status=%d body=%+v", treeResp.StatusCode, treeBody)
	}

	blobResp, err := httpSrv.Client().Get(httpSrv.URL + "/api/repos/moonwitness/sovereign-demo/blob?path=docs/manifest.txt")
	if err != nil {
		t.Fatalf("get blob: %v", err)
	}
	var blobBody githttp.BlobContent
	_ = json.NewDecoder(blobResp.Body).Decode(&blobBody)
	_ = blobResp.Body.Close()
	if blobResp.StatusCode != http.StatusOK || blobBody.Content != "sovereign-manifest-v1\n" {
		t.Fatalf("unexpected blob reply: status=%d body=%+v", blobResp.StatusCode, blobBody)
	}

	// 5. Webhook CRUD via /api/webhooks.
	hookCreateResp, err := httpSrv.Client().Post(httpSrv.URL+"/api/webhooks", "application/json", bytes.NewBufferString(`{
		"repoOwner": "moonwitness",
		"repoName": "sovereign-demo",
		"targetUrl": "http://127.0.0.1:9999/webhook",
		"secret": "hook-secret",
		"events": "push"
	}`))
	if err != nil {
		t.Fatalf("create webhook: %v", err)
	}
	var hookReply struct {
		Webhook webhookView `json:"webhook"`
	}
	_ = json.NewDecoder(hookCreateResp.Body).Decode(&hookReply)
	_ = hookCreateResp.Body.Close()
	if hookCreateResp.StatusCode != http.StatusCreated || !hookReply.Webhook.SecretConfigured || hookReply.Webhook.ID <= 0 {
		t.Fatalf("unexpected webhook create reply: status=%d body=%+v", hookCreateResp.StatusCode, hookReply)
	}

	// 6. Trigger mirror sync into a second repo from local bare repo path.
	mirrorCreateResp, err := httpSrv.Client().Post(httpSrv.URL+"/api/repos", "application/json", bytes.NewBufferString(`{
		"owner": "moonwitness",
		"name": "sovereign-mirror"
	}`))
	if err != nil {
		t.Fatalf("create mirror target repo: %v", err)
	}
	_ = mirrorCreateResp.Body.Close()

	sourceBarePath, err := gitMgr.RepoPath("moonwitness", "sovereign-demo")
	if err != nil {
		t.Fatalf("repo path: %v", err)
	}
	mirrorReqBody, _ := json.Marshal(map[string]string{"upstreamUrl": sourceBarePath})
	mirrorSyncResp, err := httpSrv.Client().Post(httpSrv.URL+"/api/repos/moonwitness/sovereign-mirror/mirror", "application/json", bytes.NewReader(mirrorReqBody))
	if err != nil {
		t.Fatalf("post mirror sync: %v", err)
	}
	var mirrorOutcome githttp.MirrorSyncResult
	_ = json.NewDecoder(mirrorSyncResp.Body).Decode(&mirrorOutcome)
	_ = mirrorSyncResp.Body.Close()
	if mirrorSyncResp.StatusCode != http.StatusOK || mirrorOutcome.Status != "synced" || mirrorOutcome.BranchCount != 1 {
		t.Fatalf("unexpected mirror sync reply: status=%d body=%+v", mirrorSyncResp.StatusCode, mirrorOutcome)
	}

	// 7. Delete repo via DELETE /api/repos/:owner/:name.
	delReq, _ := http.NewRequest(http.MethodDelete, httpSrv.URL+"/api/repos/moonwitness/sovereign-mirror", nil)
	delResp, err := httpSrv.Client().Do(delReq)
	if err != nil {
		t.Fatalf("delete repo: %v", err)
	}
	_ = delResp.Body.Close()
	if delResp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 on delete repo, got %d", delResp.StatusCode)
	}
}

func TestServerBearerAuthGuard(t *testing.T) {
	tempRoot := t.TempDir()
	labStore, err := store.Open(filepath.Join(tempRoot, "auth.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer func() { _ = labStore.Close() }()

	gitMgr, _ := githttp.NewManager(filepath.Join(tempRoot, "repos"), labStore, false)
	localBackend, _ := lfs.NewLocalDiskBackend(filepath.Join(tempRoot, "lfs"))
	lfsSvc, _ := lfs.NewService(labStore, localBackend, "", 0)
	labServer := New(Config{AuthToken: "sovereign-secret-token"}, labStore, gitMgr, nil, lfsSvc)

	unauthReq := httptest.NewRequest(http.MethodGet, "/api/repos", nil)
	unauthRec := httptest.NewRecorder()
	labServer.Handler().ServeHTTP(unauthRec, unauthReq)
	if unauthRec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 without bearer token, got %d", unauthRec.Code)
	}

	authReq := httptest.NewRequest(http.MethodGet, "/api/repos", nil)
	authReq.Header.Set("Authorization", "Bearer sovereign-secret-token")
	authRec := httptest.NewRecorder()
	labServer.Handler().ServeHTTP(authRec, authReq)
	if authRec.Code != http.StatusOK {
		t.Fatalf("expected 200 with valid bearer token, got %d", authRec.Code)
	}
}

func TestTriggerCIAndPushHookDispatch(t *testing.T) {
	t.Setenv("MW_MISSION_URL", "")

	var mu sync.Mutex
	var dispatchedMissions []ci.MissionRequest
	var dispatchedKeys []string

	mockMissionSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/missions" || r.Method != http.MethodPost {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		rawBody, err := io.ReadAll(r.Body)
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		var parsed ci.MissionRequest
		if err := json.Unmarshal(rawBody, &parsed); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		mu.Lock()
		dispatchedMissions = append(dispatchedMissions, parsed)
		dispatchedKeys = append(dispatchedKeys, r.Header.Get("Idempotency-Key"))
		mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"id":"mission-99","status":"queued"}`))
	}))
	defer mockMissionSrv.Close()

	tempRoot := t.TempDir()
	labStore, err := store.Open(filepath.Join(tempRoot, "ci.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer func() { _ = labStore.Close() }()

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

	// 1. Unconfigured server returns 400 on trigger-ci.
	unconfiguredSrv := New(Config{Version: "test-ci"}, labStore, gitMgr, nil, lfsSvc)
	createReq := httptest.NewRequest(http.MethodPost, "/api/repos", bytes.NewBufferString(`{"owner":"moonwitness","name":"ci-repo"}`))
	createReq.Header.Set("Content-Type", "application/json")
	createRec := httptest.NewRecorder()
	unconfiguredSrv.Handler().ServeHTTP(createRec, createReq)
	if createRec.Code != http.StatusCreated {
		t.Fatalf("expected 201 create repo, got %d", createRec.Code)
	}

	noURLReq := httptest.NewRequest(http.MethodPost, "/api/repos/moonwitness/ci-repo/trigger-ci", bytes.NewBufferString(`{"sha":"abcdef123456"}`))
	noURLReq.Header.Set("Content-Type", "application/json")
	noURLRec := httptest.NewRecorder()
	unconfiguredSrv.Handler().ServeHTTP(noURLRec, noURLReq)
	if noURLRec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 when no missionUrl / MW_MISSION_URL configured, got %d", noURLRec.Code)
	}

	// 2. Configured server dispatches on git-receive-pack push AND on POST /api/repos/:owner/:name/trigger-ci.
	configuredSrv := New(Config{
		Version:      "test-ci",
		MissionURL:   mockMissionSrv.URL,
		CIHTTPClient: mockMissionSrv.Client(),
	}, labStore, gitMgr, nil, lfsSvc)
	httpSrv := httptest.NewServer(configuredSrv.Handler())
	defer httpSrv.Close()

	workDir := filepath.Join(tempRoot, "ci-work")
	if err := os.MkdirAll(workDir, 0o755); err != nil {
		t.Fatalf("mkdir workDir: %v", err)
	}
	execGit(t, workDir, "init", "-b", "main")
	if err := os.WriteFile(filepath.Join(workDir, "main.go"), []byte("package main\nfunc main() {}\n"), 0o644); err != nil {
		t.Fatalf("write main.go: %v", err)
	}
	execGit(t, workDir, "add", ".")
	execGit(t, workDir, "commit", "-m", "initial ci commit")
	headCmd := exec.Command("git", "rev-parse", "HEAD")
	headCmd.Dir = workDir
	headBytes, err := headCmd.Output()
	if err != nil {
		t.Fatalf("rev-parse HEAD: %v", err)
	}
	headSHA := strings.TrimSpace(string(headBytes))

	execGit(t, workDir, "remote", "add", "origin", httpSrv.URL+"/moonwitness/ci-repo.git")
	execGit(t, workDir, "push", "-u", "origin", "main")
	configuredSrv.WaitCI()

	// Manual trigger-ci endpoint returns 202 Accepted with dispatched mission JSON.
	triggerResp, err := httpSrv.Client().Post(
		httpSrv.URL+"/api/repos/moonwitness/ci-repo/trigger-ci",
		"application/json",
		bytes.NewBufferString(`{"pusher":"release-bot"}`),
	)
	if err != nil {
		t.Fatalf("post trigger-ci: %v", err)
	}
	var triggerOutcome ci.DispatchResult
	_ = json.NewDecoder(triggerResp.Body).Decode(&triggerOutcome)
	_ = triggerResp.Body.Close()

	if triggerResp.StatusCode != http.StatusAccepted {
		t.Fatalf("expected 202 Accepted from trigger-ci, got %d", triggerResp.StatusCode)
	}
	expectedKey := "ci:moonwitness/ci-repo:" + headSHA
	if triggerOutcome.Mission.IdempotencyKey != expectedKey {
		t.Fatalf("expected idempotency_key %q, got %q", expectedKey, triggerOutcome.Mission.IdempotencyKey)
	}
	if len(triggerOutcome.Mission.Tasks) != 3 {
		t.Fatalf("expected 3 DAG stages, got %d", len(triggerOutcome.Mission.Tasks))
	}

	mu.Lock()
	missionCount := len(dispatchedMissions)
	firstMission := dispatchedMissions[0]
	firstKey := dispatchedKeys[0]
	mu.Unlock()

	if missionCount != 2 {
		t.Fatalf("expected 2 mission dispatches (1 push hook + 1 manual trigger), got %d", missionCount)
	}
	if firstKey != expectedKey || firstMission.IdempotencyKey != expectedKey {
		t.Fatalf("unexpected push-hook idempotency key: header=%q body=%q", firstKey, firstMission.IdempotencyKey)
	}
	expectedTags := []string{"git", "ci"}
	if !reflect.DeepEqual(firstMission.CapabilityTags, expectedTags) {
		t.Fatalf("unexpected capability tags: %v", firstMission.CapabilityTags)
	}
	if firstMission.Tasks[0].ID != "lint" || len(firstMission.Tasks[0].DependsOn) != 0 ||
		firstMission.Tasks[1].ID != "test" || !reflect.DeepEqual(firstMission.Tasks[1].DependsOn, []string{"lint"}) ||
		firstMission.Tasks[2].ID != "build" || !reflect.DeepEqual(firstMission.Tasks[2].DependsOn, []string{"test"}) {
		t.Fatalf("unexpected 3-stage DAG: %+v", firstMission.Tasks)
	}
}
