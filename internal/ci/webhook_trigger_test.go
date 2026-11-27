package ci

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/labstack/echo/v4"
)

func signWebhookPayload(secret string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

func setupSeededBareRepo(t *testing.T, repoName string) (bareDir string, headSHA string) {
	t.Helper()
	tempRoot := t.TempDir()
	bareDir = filepath.Join(tempRoot, repoName+".git")
	seedDir := filepath.Join(tempRoot, "seed")
	runGitTest(t, tempRoot, "init", "--bare", bareDir)
	if err := os.MkdirAll(seedDir, 0o755); err != nil {
		t.Fatalf("mkdir seedDir: %v", err)
	}
	runGitTest(t, seedDir, "init", "-b", "main")
	if err := os.WriteFile(filepath.Join(seedDir, ".mw-ci.json"), []byte(`{"stages":[{"id":"check","command":"echo webhook-ci-ok"}]}`), 0o644); err != nil {
		t.Fatalf("write .mw-ci.json: %v", err)
	}
	runGitTest(t, seedDir, "add", ".")
	runGitTest(t, seedDir, "commit", "-m", "seed repo")
	headSHA = runGitTest(t, seedDir, "rev-parse", "HEAD")
	runGitTest(t, seedDir, "remote", "add", "origin", bareDir)
	runGitTest(t, seedDir, "push", "-u", "origin", "main")
	return bareDir, headSHA
}

func TestWebhookTriggerGitHubPayloadAcceptedAndRuns(t *testing.T) {
	bareDir, headSHA := setupSeededBareRepo(t, "alpha-app")
	runStore, _ := NewRunStore(nil)
	runner := NewLocalRunner(LocalRunnerConfig{
		Store:           runStore,
		AllowLocalClone: true,
		Executor: func(_ context.Context, _ string, command string) (string, int, error) {
			return "ran: " + command, 0, nil
		},
	})

	handler := NewWebhookTriggerHandler(WebhookTriggerConfig{
		Secret: "super-secret-key",
		Runner: runner,
		RepoResolver: func(owner, repo string) (string, bool) {
			if owner == "moonwitness" && repo == "alpha-app" {
				return bareDir, true
			}
			return "", false
		},
	})

	e := echo.New()
	e.POST("/api/ci/webhook", handler.HandleWebhook)

	payload := []byte(`{
		"ref": "refs/heads/main",
		"after": "` + headSHA + `",
		"pusher": {"name": "octocat", "login": "octocat"},
		"repository": {
			"name": "alpha-app",
			"full_name": "moonwitness/alpha-app",
			"owner": {"login": "moonwitness", "name": "MoonWitness"}
		}
	}`)
	req := httptest.NewRequest(http.MethodPost, "/api/ci/webhook", bytes.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-GitHub-Event", "push")
	req.Header.Set("X-Hub-Signature-256", signWebhookPayload("super-secret-key", payload))
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)

	if rec.Code != http.StatusAccepted {
		t.Fatalf("expected 202 Accepted, got %d (%s)", rec.Code, rec.Body.String())
	}

	var respBody struct {
		Status string    `json:"status"`
		Run    RunRecord `json:"run"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &respBody); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if respBody.Status != "queued" || respBody.Run.ID == "" {
		t.Fatalf("unexpected response body: %+v", respBody)
	}

	runner.Wait()
	finalRun, err := runStore.Get(context.Background(), respBody.Run.ID)
	if err != nil {
		t.Fatalf("get run: %v", err)
	}
	if finalRun.Status != StatusPassed || finalRun.SHA != headSHA || finalRun.Pusher != "octocat" {
		t.Fatalf("unexpected completed run: %+v", finalRun)
	}
}

func TestWebhookTriggerMWLabSignatureAndPayload(t *testing.T) {
	bareDir, headSHA := setupSeededBareRepo(t, "beta-app")
	runStore, _ := NewRunStore(nil)
	runner := NewLocalRunner(LocalRunnerConfig{
		Store:           runStore,
		AllowLocalClone: true,
		Executor: func(_ context.Context, _ string, _ string) (string, int, error) {
			return "ok", 0, nil
		},
	})

	handler := NewWebhookTriggerHandler(WebhookTriggerConfig{
		Secret: "mw-secret",
		Runner: runner,
		RepoResolver: func(owner, repo string) (string, bool) {
			if owner == "moonwitness" && repo == "beta-app" {
				return bareDir, true
			}
			return "", false
		},
	})

	e := echo.New()
	e.POST("/api/ci/webhook", handler.HandleWebhook)

	payload := []byte(`{
		"event": "push",
		"ref": "refs/heads/main",
		"after": "` + headSHA + `",
		"pusher": "local-dev",
		"repository": {
			"owner": "moonwitness",
			"name": "beta-app",
			"fullName": "moonwitness/beta-app"
		}
	}`)
	req := httptest.NewRequest(http.MethodPost, "/api/ci/webhook", bytes.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-MW-Lab-Event", "push")
	req.Header.Set("X-MW-Lab-Signature-256", signWebhookPayload("mw-secret", payload))
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)

	if rec.Code != http.StatusAccepted {
		t.Fatalf("expected 202 Accepted, got %d (%s)", rec.Code, rec.Body.String())
	}
	runner.Wait()

	runs, _ := runStore.List(context.Background(), RunFilter{Repo: "beta-app"})
	if len(runs) != 1 || runs[0].Status != StatusPassed || runs[0].Pusher != "local-dev" {
		t.Fatalf("unexpected run state: %+v", runs)
	}
}

func TestWebhookTriggerSecurityAndIgnoredEvents(t *testing.T) {
	runStore, _ := NewRunStore(nil)
	runner := NewLocalRunner(LocalRunnerConfig{Store: runStore})
	e := echo.New()

	// 1. Fail-closed when secret is unconfigured.
	unconfigured := NewWebhookTriggerHandler(WebhookTriggerConfig{Secret: "", Runner: runner})
	e.POST("/unconfigured", unconfigured.HandleWebhook)
	req := httptest.NewRequest(http.MethodPost, "/unconfigured", strings.NewReader(`{}`))
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 when secret unconfigured, got %d", rec.Code)
	}

	// 2. Configured handler rejects missing/invalid/tampered signatures.
	handler := NewWebhookTriggerHandler(WebhookTriggerConfig{
		Secret: "strict-secret",
		Runner: runner,
		RepoResolver: func(owner, repo string) (string, bool) {
			return "/tmp/unused", true
		},
	})
	e.POST("/api/ci/webhook", handler.HandleWebhook)

	validBody := []byte(`{"ref":"refs/heads/main","after":"1234567890abcdef1234567890abcdef12345678","repository":{"owner":"mw","name":"app"}}`)
	validSig := signWebhookPayload("strict-secret", validBody)

	// Missing signature -> 401
	reqNoSig := httptest.NewRequest(http.MethodPost, "/api/ci/webhook", bytes.NewReader(validBody))
	recNoSig := httptest.NewRecorder()
	e.ServeHTTP(recNoSig, reqNoSig)
	if recNoSig.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 on missing signature, got %d", recNoSig.Code)
	}

	// Tampered body -> 401
	tamperedBody := []byte(`{"ref":"refs/heads/main","after":"1234567890abcdef1234567890abcdef12345679","repository":{"owner":"mw","name":"app"}}`)
	reqTampered := httptest.NewRequest(http.MethodPost, "/api/ci/webhook", bytes.NewReader(tamperedBody))
	reqTampered.Header.Set("X-Hub-Signature-256", validSig)
	recTampered := httptest.NewRecorder()
	e.ServeHTTP(recTampered, reqTampered)
	if recTampered.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 on tampered payload, got %d", recTampered.Code)
	}

	// 3. Ping event -> 200 OK without triggering run.
	pingBody := []byte(`{"zen":"keep it simple"}`)
	reqPing := httptest.NewRequest(http.MethodPost, "/api/ci/webhook", bytes.NewReader(pingBody))
	reqPing.Header.Set("X-GitHub-Event", "ping")
	reqPing.Header.Set("X-Hub-Signature-256", signWebhookPayload("strict-secret", pingBody))
	recPing := httptest.NewRecorder()
	e.ServeHTTP(recPing, reqPing)
	if recPing.Code != http.StatusOK {
		t.Fatalf("expected 200 on ping, got %d", recPing.Code)
	}

	// 4. Zero-SHA deletion push -> 200 OK ignored.
	zeroBody := []byte(`{"ref":"refs/heads/old","after":"` + zeroCommitSHA + `","repository":{"owner":"mw","name":"app"}}`)
	reqZero := httptest.NewRequest(http.MethodPost, "/api/ci/webhook", bytes.NewReader(zeroBody))
	reqZero.Header.Set("X-GitHub-Event", "push")
	reqZero.Header.Set("X-Hub-Signature-256", signWebhookPayload("strict-secret", zeroBody))
	recZero := httptest.NewRecorder()
	e.ServeHTTP(recZero, reqZero)
	if recZero.Code != http.StatusOK || !strings.Contains(recZero.Body.String(), "ref deleted") {
		t.Fatalf("expected 200 ref deleted on zero SHA, got %d (%s)", recZero.Code, recZero.Body.String())
	}

	// 5. Path traversal repo slug -> 400 Bad Request.
	badSlugBody := []byte(`{"ref":"refs/heads/main","after":"1234567890abcdef","repository":{"owner":"mw","name":"../etc"}}`)
	reqBadSlug := httptest.NewRequest(http.MethodPost, "/api/ci/webhook", bytes.NewReader(badSlugBody))
	reqBadSlug.Header.Set("X-GitHub-Event", "push")
	reqBadSlug.Header.Set("X-Hub-Signature-256", signWebhookPayload("strict-secret", badSlugBody))
	recBadSlug := httptest.NewRecorder()
	e.ServeHTTP(recBadSlug, reqBadSlug)
	if recBadSlug.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 on path traversal repo slug, got %d (%s)", recBadSlug.Code, recBadSlug.Body.String())
	}

	// 6. Malformed JSON -> 400 Bad Request.
	badJSON := []byte(`{not-json`)
	reqBadJSON := httptest.NewRequest(http.MethodPost, "/api/ci/webhook", bytes.NewReader(badJSON))
	reqBadJSON.Header.Set("X-Hub-Signature-256", signWebhookPayload("strict-secret", badJSON))
	recBadJSON := httptest.NewRecorder()
	e.ServeHTTP(recBadJSON, reqBadJSON)
	if recBadJSON.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 on malformed json, got %d", recBadJSON.Code)
	}

	runs, _ := runStore.List(context.Background(), RunFilter{})
	if len(runs) != 0 {
		t.Fatalf("expected 0 runs created across security/ignored checks, got %d", len(runs))
	}
}
