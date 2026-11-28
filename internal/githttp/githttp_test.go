package githttp

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/A-L-L-A-H/mw-lab/internal/store"
	"github.com/labstack/echo/v4"
)

func runGitHelper(t *testing.T, workDir string, args ...string) string {
	t.Helper()
	var gitArgs []string
	if token := os.Getenv("MW_TEST_AUTH_TOKEN"); token != "" {
		gitArgs = append(gitArgs, "-c", "http.extraheader=Authorization: Bearer "+token)
	}
	gitArgs = append(gitArgs, args...)
	cmd := exec.Command("git", gitArgs...)
	cmd.Dir = workDir
	cmd.Env = append(os.Environ(),
		"GIT_TERMINAL_PROMPT=0",
		"GIT_ASKPASS=",
		"GIT_AUTHOR_NAME=MW Tester",
		"GIT_AUTHOR_EMAIL=tester@moonwitness.local",
		"GIT_COMMITTER_NAME=MW Tester",
		"GIT_COMMITTER_EMAIL=tester@moonwitness.local",
	)
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v failed in %s: %v\n%s", args, workDir, err, string(output))
	}
	return strings.TrimSpace(string(output))
}

func TestSlugAndPathValidation(t *testing.T) {
	validSlugs := []string{"moonwitness", "mw-lab", "alpha_1.0", "A-L-L-A-H"}
	for _, candidate := range validSlugs {
		if err := ValidateSlug(candidate); err != nil {
			t.Fatalf("expected slug %q to be valid, got %v", candidate, err)
		}
	}

	invalidSlugs := []string{"", ".", "..", "../escape", "a..b", "-flag", "has space", "owner/sub", "semi;colon"}
	for _, candidate := range invalidSlugs {
		if err := ValidateSlug(candidate); err == nil {
			t.Fatalf("expected slug %q to be rejected", candidate)
		}
	}

	if _, err := ValidateRelativePath("../secret.txt"); err == nil {
		t.Fatalf("expected parent traversal relative path to fail")
	}
	if _, err := ValidateRelativePath(`a\b.txt`); err == nil {
		t.Fatalf("expected backslash relative path to fail")
	}
	cleanPath, err := ValidateRelativePath("src/cmd/main.go")
	if err != nil || cleanPath != "src/cmd/main.go" {
		t.Fatalf("unexpected clean relative path: %q (%v)", cleanPath, err)
	}

	strictMgr, err := NewManager(t.TempDir(), nil, false)
	if err != nil {
		t.Fatalf("new manager: %v", err)
	}
	if err := strictMgr.ValidateUpstreamURL("https://github.com/A-L-L-A-H/mw-lab.git"); err != nil {
		t.Fatalf("expected https upstream valid: %v", err)
	}
	if err := strictMgr.ValidateUpstreamURL("file:///tmp/repo.git"); err == nil {
		t.Fatalf("expected file:// upstream rejected when allowLocalMirrors=false")
	}
	if err := strictMgr.ValidateUpstreamURL("--upload-pack=evil"); err == nil {
		t.Fatalf("expected hyphen-prefixed upstream rejected")
	}
}

func TestBareRepoInspectionAndMirror(t *testing.T) {
	ctx := context.Background()
	tempRoot := t.TempDir()
	labStore, err := store.Open(filepath.Join(tempRoot, "lab.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer func() { _ = labStore.Close() }()

	mgr, err := NewManager(filepath.Join(tempRoot, "repos"), labStore, true)
	if err != nil {
		t.Fatalf("new manager: %v", err)
	}

	// Build a real local upstream Git repository with two commits and nested directories.
	upstreamDir := filepath.Join(tempRoot, "upstream-src")
	if err := os.MkdirAll(filepath.Join(upstreamDir, "pkg", "forge"), 0o755); err != nil {
		t.Fatalf("mkdir upstream: %v", err)
	}
	runGitHelper(t, upstreamDir, "init", "-b", "main")
	if err := os.WriteFile(filepath.Join(upstreamDir, "README.md"), []byte("# Sovereign Forge\n"), 0o644); err != nil {
		t.Fatalf("write README: %v", err)
	}
	runGitHelper(t, upstreamDir, "add", "README.md")
	runGitHelper(t, upstreamDir, "commit", "-m", "initial readme commit")

	if err := os.WriteFile(filepath.Join(upstreamDir, "pkg", "forge", "engine.go"), []byte("package forge\n\nfunc Ready() bool { return true }\n"), 0o644); err != nil {
		t.Fatalf("write engine.go: %v", err)
	}
	runGitHelper(t, upstreamDir, "add", ".")
	runGitHelper(t, upstreamDir, "commit", "-m", "add forge engine package")

	if _, err := labStore.CreateRepo(ctx, store.RepoRecord{Owner: "moonwitness", Name: "mirrored"}); err != nil {
		t.Fatalf("create repo metadata: %v", err)
	}
	syncOutcome, err := mgr.SyncMirror(ctx, "moonwitness", "mirrored", upstreamDir)
	if err != nil {
		t.Fatalf("sync mirror: %v", err)
	}
	if syncOutcome.Status != "synced" || syncOutcome.BranchCount != 1 || syncOutcome.Branches[0] != "main" {
		t.Fatalf("unexpected mirror sync outcome: %+v", syncOutcome)
	}

	branches, err := mgr.ListBranches(ctx, "moonwitness", "mirrored")
	if err != nil || len(branches) != 1 || branches[0].Name != "main" || !branches[0].IsDefault {
		t.Fatalf("unexpected branches: %+v (%v)", branches, err)
	}

	commits, err := mgr.ListCommits(ctx, "moonwitness", "mirrored", "main", 10)
	if err != nil {
		t.Fatalf("list commits: %v", err)
	}
	if len(commits) != 2 || commits[0].Subject != "add forge engine package" || commits[1].Subject != "initial readme commit" {
		t.Fatalf("unexpected commits: %+v", commits)
	}

	rootTree, err := mgr.ListTree(ctx, "moonwitness", "mirrored", "main", "")
	if err != nil {
		t.Fatalf("list root tree: %v", err)
	}
	if len(rootTree) != 2 || rootTree[0].Type != "tree" || rootTree[0].Name != "pkg" || rootTree[1].Name != "README.md" {
		t.Fatalf("unexpected root tree: %+v", rootTree)
	}

	subTree, err := mgr.ListTree(ctx, "moonwitness", "mirrored", "main", "pkg/forge")
	if err != nil || len(subTree) != 1 || subTree[0].Path != "pkg/forge/engine.go" {
		t.Fatalf("unexpected sub tree: %+v (%v)", subTree, err)
	}

	blobDoc, err := mgr.ReadBlob(ctx, "moonwitness", "mirrored", "main", "pkg/forge/engine.go", 0)
	if err != nil {
		t.Fatalf("read blob: %v", err)
	}
	if blobDoc.Binary || !strings.Contains(blobDoc.Content, "func Ready() bool") {
		t.Fatalf("unexpected blob content: %+v", blobDoc)
	}
}

func TestSmartHTTPClonePushAndWebhookDelivery(t *testing.T) {
	ctx := context.Background()
	tempRoot := t.TempDir()
	labStore, err := store.Open(filepath.Join(tempRoot, "lab.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer func() { _ = labStore.Close() }()

	mgr, err := NewManager(filepath.Join(tempRoot, "repos"), labStore, true)
	if err != nil {
		t.Fatalf("new manager: %v", err)
	}

	if _, err := labStore.CreateRepo(ctx, store.RepoRecord{Owner: "alloy", Name: "signal"}); err != nil {
		t.Fatalf("create repo row: %v", err)
	}
	if _, err := mgr.InitBareRepo(ctx, "alloy", "signal", "main"); err != nil {
		t.Fatalf("init bare repo: %v", err)
	}

	webhookSecret := "mw-echo-hmac-secret"
	var mu sync.Mutex
	var receivedPayloads []PushPayload
	var receivedEventHeader string
	var receivedSignature string
	var signatureValid bool

	webhookServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rawBody, readErr := io.ReadAll(r.Body)
		if readErr != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		var payload PushPayload
		if err := json.Unmarshal(rawBody, &payload); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		mac := hmac.New(sha256.New, []byte(webhookSecret))
		_, _ = mac.Write(rawBody)
		expectedSig := "sha256=" + hex.EncodeToString(mac.Sum(nil))

		mu.Lock()
		receivedPayloads = append(receivedPayloads, payload)
		receivedEventHeader = r.Header.Get("X-MW-Lab-Event")
		receivedSignature = r.Header.Get("X-MW-Lab-Signature-256")
		signatureValid = hmac.Equal([]byte(receivedSignature), []byte(expectedSig))
		mu.Unlock()

		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(`{"accepted":true}`))
	}))
	defer webhookServer.Close()

	if _, err := labStore.CreateWebhook(ctx, store.WebhookRecord{
		RepoOwner: "alloy",
		RepoName:  "signal",
		TargetURL: webhookServer.URL,
		Secret:    webhookSecret,
		Events:    "push",
		Active:    true,
	}); err != nil {
		t.Fatalf("register webhook: %v", err)
	}

	dispatcher := NewWebhookDispatcher(labStore, webhookServer.Client())
	gitHandler := NewHandler(mgr, labStore, dispatcher)
	gitHandler.SetAuth("test-token-123", nil)
	echoApp := echo.New()
	gitHandler.RegisterRoutes(echoApp)
	gitServer := httptest.NewServer(echoApp)
	defer gitServer.Close()

	repoURL := gitServer.URL + "/alloy/signal.git"

	// Create local working tree, commit, and push via Git Smart HTTP (receive-pack).
	workDir := filepath.Join(tempRoot, "work-push")
	if err := os.MkdirAll(workDir, 0o755); err != nil {
		t.Fatalf("mkdir workDir: %v", err)
	}
	t.Setenv("MW_TEST_AUTH_TOKEN", "test-token-123")
	runGitHelper(t, workDir, "init", "-b", "main")
	if err := os.WriteFile(filepath.Join(workDir, "hello.txt"), []byte("sovereign push payload\n"), 0o644); err != nil {
		t.Fatalf("write hello.txt: %v", err)
	}
	runGitHelper(t, workDir, "add", "hello.txt")
	runGitHelper(t, workDir, "commit", "-m", "first smart http push")
	headSHA := runGitHelper(t, workDir, "rev-parse", "HEAD")
	runGitHelper(t, workDir, "remote", "add", "origin", repoURL)
	runGitHelper(t, workDir, "push", "-u", "origin", "main")

	dispatcher.Wait()

	mu.Lock()
	deliveredCount := len(receivedPayloads)
	var firstPayload PushPayload
	if deliveredCount > 0 {
		firstPayload = receivedPayloads[0]
	}
	validSig := signatureValid
	eventHeader := receivedEventHeader
	mu.Unlock()

	if deliveredCount != 1 {
		t.Fatalf("expected 1 webhook push delivery, got %d", deliveredCount)
	}
	if eventHeader != "push" || !validSig {
		t.Fatalf("unexpected webhook headers: event=%q validSig=%v", eventHeader, validSig)
	}
	if firstPayload.Repository.FullName != "alloy/signal" || firstPayload.Ref != "refs/heads/main" || firstPayload.After != headSHA {
		t.Fatalf("unexpected push webhook payload: %+v", firstPayload)
	}
	if len(firstPayload.Commits) != 1 || firstPayload.Commits[0].Subject != "first smart http push" {
		t.Fatalf("expected push commits in webhook payload, got %+v", firstPayload.Commits)
	}

	events, err := labStore.ListPushEvents(ctx, "alloy", "signal", 10)
	if err != nil || len(events) != 1 || events[0].DeliveryStatus != "delivered:1/1" {
		t.Fatalf("expected delivered push_events record, got %+v (%v)", events, err)
	}

	// Clone back via Git Smart HTTP (upload-pack) and verify file contents match.
	cloneDir := filepath.Join(tempRoot, "work-clone")
	runGitHelper(t, tempRoot, "-c", "core.autocrlf=false", "clone", repoURL, cloneDir)
	clonedContent, err := os.ReadFile(filepath.Join(cloneDir, "hello.txt"))
	if err != nil {
		t.Fatalf("read cloned file: %v", err)
	}
	if strings.TrimSpace(string(clonedContent)) != "sovereign push payload" {
		t.Fatalf("unexpected cloned content: %q", string(clonedContent))
	}
}
