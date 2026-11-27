package ci

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

func openTestSQLiteDB(t *testing.T) *sql.DB {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "ci-runner-test.db")
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	db.SetMaxOpenConns(1)
	if _, err := db.Exec("PRAGMA journal_mode = WAL; PRAGMA busy_timeout = 5000;"); err != nil {
		_ = db.Close()
		t.Fatalf("apply pragmas: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func runGitTest(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_TERMINAL_PROMPT=0",
		"GIT_AUTHOR_NAME=CI Test",
		"GIT_AUTHOR_EMAIL=ci@moonwitness.local",
		"GIT_COMMITTER_NAME=CI Test",
		"GIT_COMMITTER_EMAIL=ci@moonwitness.local",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v failed: %v\n%s", args, err, string(out))
	}
	return strings.TrimSpace(string(out))
}

func TestLocalRunnerMultiStageSuccessAndSQLitePersistence(t *testing.T) {
	db := openTestSQLiteDB(t)
	runStore, err := NewRunStore(db)
	if err != nil {
		t.Fatalf("new run store: %v", err)
	}

	workDir := t.TempDir()
	// Write a .mw-ci.json with real OS commands and a DAG dependency.
	ciConfig := `{
		"stages": [
			{"id": "build", "name": "Build", "depends_on": ["lint"], "command": "echo stage-build-ok"},
			{"id": "lint", "name": "Lint", "depends_on": [], "command": "echo stage-lint-ok"}
		]
	}`
	if err := os.WriteFile(filepath.Join(workDir, ".mw-ci.json"), []byte(ciConfig), 0o644); err != nil {
		t.Fatalf("write .mw-ci.json: %v", err)
	}

	runner := NewLocalRunner(LocalRunnerConfig{
		Store:         runStore,
		MaxConcurrent: 2,
	})

	rec, err := runner.Run(context.Background(), RunRequest{
		Owner:   "moonwitness",
		Repo:    "mw-lab",
		Ref:     "refs/heads/main",
		SHA:     "1111111111111111111111111111111111111111",
		Pusher:  "builder",
		Source:  "manual",
		WorkDir: workDir,
	})
	if err != nil {
		t.Fatalf("runner.Run failed: %v", err)
	}
	if rec.Status != StatusPassed {
		t.Fatalf("expected status %q, got %q (summary=%s, logs=%s)", StatusPassed, rec.Status, rec.Summary, rec.Logs)
	}
	if len(rec.Stages) != 2 {
		t.Fatalf("expected 2 stages, got %d", len(rec.Stages))
	}
	// lint must execute before build because build depends_on lint.
	if rec.Stages[0].ID != "lint" || rec.Stages[1].ID != "build" {
		t.Fatalf("expected topological order [lint, build], got [%s, %s]", rec.Stages[0].ID, rec.Stages[1].ID)
	}
	if !strings.Contains(rec.Stages[0].Output, "stage-lint-ok") || !strings.Contains(rec.Stages[1].Output, "stage-build-ok") {
		t.Fatalf("expected real command output in stages, got %+v", rec.Stages)
	}

	latest, err := runStore.LatestByRepo(context.Background(), "moonwitness", "mw-lab")
	if err != nil {
		t.Fatalf("LatestByRepo failed: %v", err)
	}
	if latest.ID != rec.ID || latest.Status != StatusPassed {
		t.Fatalf("persisted latest mismatch: %+v", latest)
	}

	listed, err := runStore.List(context.Background(), RunFilter{Owner: "moonwitness", Repo: "mw-lab"})
	if err != nil {
		t.Fatalf("List failed: %v", err)
	}
	if len(listed) != 1 || listed[0].ID != rec.ID {
		t.Fatalf("unexpected listed runs: %+v", listed)
	}
}

func TestLocalRunnerFailureStopsPipelineAndInvokesOnFailureOnce(t *testing.T) {
	runStore, err := NewRunStore(nil) // nil-DB in-memory fallback
	if err != nil {
		t.Fatalf("new in-memory run store: %v", err)
	}

	var failureCalls atomic.Int32
	var failedRun RunRecord
	var mu sync.Mutex

	workDir := t.TempDir()
	// Create go.mod to exercise language auto-detection (lint, test, build).
	if err := os.WriteFile(filepath.Join(workDir, "go.mod"), []byte("module example.com/fail\ngo 1.24\n"), 0o644); err != nil {
		t.Fatalf("write go.mod: %v", err)
	}

	var executed []string
	runner := NewLocalRunner(LocalRunnerConfig{
		Store: runStore,
		Executor: func(_ context.Context, _ string, command string) (string, int, error) {
			executed = append(executed, command)
			if strings.Contains(command, "go test") {
				return "FAIL example.com/fail\nboom", 1, errors.New("exit status 1")
			}
			return "ok", 0, nil
		},
		OnFailure: func(_ context.Context, run RunRecord) {
			failureCalls.Add(1)
			mu.Lock()
			failedRun = run
			mu.Unlock()
		},
	})

	rec, err := runner.Run(context.Background(), RunRequest{
		Owner:   "moonwitness",
		Repo:    "broken-svc",
		SHA:     "2222222222222222222222222222222222222222",
		WorkDir: workDir,
	})
	if err != nil {
		t.Fatalf("runner.Run returned unexpected error: %v", err)
	}
	if rec.Status != StatusFailed {
		t.Fatalf("expected status failed, got %q", rec.Status)
	}
	if len(executed) != 2 {
		t.Fatalf("expected pipeline to stop at failing stage 2 (executed=%v)", executed)
	}
	if len(rec.Stages) != 3 || rec.Stages[0].Status != StatusPassed || rec.Stages[1].Status != StatusFailed || rec.Stages[2].Status != "skipped" {
		t.Fatalf("unexpected stage statuses: %+v", rec.Stages)
	}
	if !strings.Contains(rec.Stages[1].Output, "boom") {
		t.Fatalf("expected failure output captured, got %q", rec.Stages[1].Output)
	}
	if failureCalls.Load() != 1 {
		t.Fatalf("expected OnFailure called once, got %d", failureCalls.Load())
	}
	mu.Lock()
	defer mu.Unlock()
	if failedRun.ID != rec.ID || failedRun.Status != StatusFailed {
		t.Fatalf("unexpected failedRun passed to OnFailure: %+v", failedRun)
	}
}

func TestLocalRunnerCloneBareRepoAndCheckoutSHA(t *testing.T) {
	tempRoot := t.TempDir()
	bareDir := filepath.Join(tempRoot, "bare.git")
	seedDir := filepath.Join(tempRoot, "seed")

	runGitTest(t, tempRoot, "init", "--bare", bareDir)
	if err := os.MkdirAll(seedDir, 0o755); err != nil {
		t.Fatalf("mkdir seedDir: %v", err)
	}
	runGitTest(t, seedDir, "init", "-b", "main")
	if err := os.WriteFile(filepath.Join(seedDir, "go.mod"), []byte("module example.com/ciclone\n\ngo 1.24\n"), 0o644); err != nil {
		t.Fatalf("write go.mod: %v", err)
	}
	if err := os.WriteFile(filepath.Join(seedDir, "main.go"), []byte("package main\nfunc Add(a, b int) int { return a + b }\nfunc main() {}\n"), 0o644); err != nil {
		t.Fatalf("write main.go: %v", err)
	}
	if err := os.WriteFile(filepath.Join(seedDir, "main_test.go"), []byte("package main\nimport \"testing\"\nfunc TestAdd(t *testing.T) { if Add(2, 3) != 5 { t.Fatal(\"math\") } }\n"), 0o644); err != nil {
		t.Fatalf("write main_test.go: %v", err)
	}
	runGitTest(t, seedDir, "add", ".")
	runGitTest(t, seedDir, "commit", "-m", "commit v1")
	firstSHA := runGitTest(t, seedDir, "rev-parse", "HEAD")

	// Second commit breaks main_test.go; we explicitly test checking out firstSHA.
	if err := os.WriteFile(filepath.Join(seedDir, "main_test.go"), []byte("package main\nimport \"testing\"\nfunc TestAdd(t *testing.T) { t.Fatal(\"broken on HEAD\") }\n"), 0o644); err != nil {
		t.Fatalf("overwrite main_test.go: %v", err)
	}
	runGitTest(t, seedDir, "add", ".")
	runGitTest(t, seedDir, "commit", "-m", "commit v2 broken")
	runGitTest(t, seedDir, "remote", "add", "origin", bareDir)
	runGitTest(t, seedDir, "push", "-u", "origin", "main")

	db := openTestSQLiteDB(t)
	runStore, err := NewRunStore(db)
	if err != nil {
		t.Fatalf("new run store: %v", err)
	}

	runner := NewLocalRunner(LocalRunnerConfig{
		Store:           runStore,
		AllowLocalClone: true,
		DefaultTimeout:  2 * time.Minute,
	})

	rec, err := runner.Run(context.Background(), RunRequest{
		Owner:    "moonwitness",
		Repo:     "ciclone",
		Ref:      "refs/heads/main",
		SHA:      firstSHA,
		Pusher:   "tester",
		Source:   "webhook",
		CloneURL: bareDir,
	})
	if err != nil {
		t.Fatalf("runner.Run clone failed: %v", err)
	}
	if rec.Status != StatusPassed {
		t.Fatalf("expected passed run at firstSHA=%s, got %s (summary=%s, logs=%s)", firstSHA, rec.Status, rec.Summary, rec.Logs)
	}
	if len(rec.Stages) != 3 {
		t.Fatalf("expected auto-detected Go stages (lint, test, build), got %+v", rec.Stages)
	}
}

func TestLocalRunnerContextTimeoutMarksFailedAndDoesNotHangWait(t *testing.T) {
	runStore, _ := NewRunStore(nil)
	workDir := t.TempDir()

	var onFailCount atomic.Int32
	runner := NewLocalRunner(LocalRunnerConfig{
		Store:          runStore,
		DefaultTimeout: 80 * time.Millisecond,
		Executor: func(ctx context.Context, _ string, _ string) (string, int, error) {
			<-ctx.Done()
			return "timed out", -1, ctx.Err()
		},
		OnFailure: func(_ context.Context, _ RunRecord) {
			onFailCount.Add(1)
		},
	})

	queued, err := runner.Enqueue(context.Background(), RunRequest{
		Owner:   "moonwitness",
		Repo:    "slow-repo",
		SHA:     "3333333333333333333333333333333333333333",
		WorkDir: workDir,
		Stages:  []StageTask{{ID: "slow", Name: "Slow", Command: "sleep 10"}},
	})
	if err != nil {
		t.Fatalf("Enqueue failed: %v", err)
	}
	if queued.Status != StatusQueued {
		t.Fatalf("expected initial queued status, got %q", queued.Status)
	}

	done := make(chan struct{})
	go func() {
		runner.Wait()
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("runner.Wait() hung after timeout")
	}

	finalRec, err := runStore.Get(context.Background(), queued.ID)
	if err != nil {
		t.Fatalf("Get failed: %v", err)
	}
	if finalRec.Status != StatusFailed {
		t.Fatalf("expected failed status after timeout, got %q", finalRec.Status)
	}
	if onFailCount.Load() != 1 {
		t.Fatalf("expected OnFailure invoked on timeout, got %d", onFailCount.Load())
	}
}

func TestLocalRunnerEnqueueConcurrencyBound(t *testing.T) {
	runStore, _ := NewRunStore(nil)
	workDir := t.TempDir()

	var active atomic.Int32
	var maxObserved atomic.Int32

	runner := NewLocalRunner(LocalRunnerConfig{
		Store:         runStore,
		MaxConcurrent: 1,
		Executor: func(_ context.Context, _ string, _ string) (string, int, error) {
			cur := active.Add(1)
			for {
				prev := maxObserved.Load()
				if cur <= prev || maxObserved.CompareAndSwap(prev, cur) {
					break
				}
			}
			time.Sleep(40 * time.Millisecond)
			active.Add(-1)
			return "ok", 0, nil
		},
	})

	for i := 0; i < 3; i++ {
		_, err := runner.Enqueue(context.Background(), RunRequest{
			Owner:   "moonwitness",
			Repo:    "bounded",
			WorkDir: workDir,
			Stages:  []StageTask{{ID: "test", Command: "echo ok"}},
		})
		if err != nil {
			t.Fatalf("Enqueue %d failed: %v", i, err)
		}
	}

	runner.Wait()
	if maxObserved.Load() != 1 {
		t.Fatalf("expected max concurrent executions = 1, observed %d", maxObserved.Load())
	}
	runs, err := runStore.List(context.Background(), RunFilter{Repo: "bounded"})
	if err != nil {
		t.Fatalf("List failed: %v", err)
	}
	if len(runs) != 3 {
		t.Fatalf("expected 3 runs, got %d", len(runs))
	}
}
