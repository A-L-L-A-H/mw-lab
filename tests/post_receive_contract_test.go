package postreceive_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/A-L-L-A-H/mw-lab/src/postreceive"
)

func runGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(
		os.Environ(),
		"GIT_TERMINAL_PROMPT=0",
		"GIT_AUTHOR_NAME=V29 Tester",
		"GIT_AUTHOR_EMAIL=v29@moonwitness.local",
		"GIT_COMMITTER_NAME=V29 Tester",
		"GIT_COMMITTER_EMAIL=v29@moonwitness.local",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v failed in %s: %v\n%s", args, dir, err, string(out))
	}
	return strings.TrimSpace(string(out))
}

func findBashExecutable(t *testing.T) string {
	t.Helper()
	candidates := []string{
		`C:\Program Files\Git\bin\bash.exe`,
		`C:\Program Files\Git\usr\bin\bash.exe`,
	}
	for _, c := range candidates {
		if _, err := os.Stat(c); err == nil {
			return c
		}
	}
	if p, err := exec.LookPath("bash"); err == nil {
		return p
	}
	t.Skip("bash not available on PATH")
	return ""
}

func TestBashSyntaxCheckOnHookAndRunnerScripts(t *testing.T) {
	bashPath := findBashExecutable(t)

	repoRoot, err := filepath.Abs(filepath.Join(".."))
	if err != nil {
		t.Fatalf("resolve repo root: %v", err)
	}

	scripts := []string{
		filepath.Join(repoRoot, "hooks", "post-receive"),
		filepath.Join(repoRoot, "scripts", "run-contract-tests.sh"),
	}

	for _, script := range scripts {
		if _, statErr := os.Stat(script); statErr != nil {
			t.Fatalf("expected script %s to exist: %v", script, statErr)
		}
		cmd := exec.Command(bashPath, "-n", script)
		out, runErr := cmd.CombinedOutput()
		if runErr != nil {
			t.Fatalf("bash -n %s failed: %v\n%s", script, runErr, string(out))
		}
	}
}

func TestInstallPostReceiveHookAndParseStdin(t *testing.T) {
	tempRoot := t.TempDir()
	bareDir := filepath.Join(tempRoot, "service.git")
	runGit(t, tempRoot, "init", "--bare", bareDir)

	repoRoot, err := filepath.Abs(filepath.Join(".."))
	if err != nil {
		t.Fatalf("resolve repo root: %v", err)
	}
	hookTemplate := filepath.Join(repoRoot, "hooks", "post-receive")
	runnerScript := filepath.Join(repoRoot, "scripts", "run-contract-tests.sh")

	installedPath, err := postreceive.InstallPostReceiveHook(bareDir, hookTemplate, runnerScript)
	if err != nil {
		t.Fatalf("InstallPostReceiveHook failed: %v", err)
	}
	rawHook, err := os.ReadFile(installedPath)
	if err != nil {
		t.Fatalf("read installed hook: %v", err)
	}
	if !strings.Contains(string(rawHook), "post-receive contract check") {
		t.Fatalf("unexpected installed hook content:\n%s", string(rawHook))
	}

	stdinPayload := strings.Join([]string{
		"0000000000000000000000000000000000000000 1111111111111111111111111111111111111111 refs/heads/main",
		"1111111111111111111111111111111111111111 0000000000000000000000000000000000000000 refs/heads/deleted",
		"2222222222222222222222222222222222222222 3333333333333333333333333333333333333333 refs/tags/v1.0.0",
	}, "\n")
	updates, err := postreceive.ParsePostReceiveStdin(strings.NewReader(stdinPayload))
	if err != nil {
		t.Fatalf("ParsePostReceiveStdin failed: %v", err)
	}
	if len(updates) != 3 {
		t.Fatalf("expected 3 parsed updates, got %d", len(updates))
	}
	if updates[0].Ref != "refs/heads/main" || updates[1].NewRev != postreceive.ZeroSHA {
		t.Fatalf("unexpected parsed updates: %+v", updates)
	}
}

func TestBoundedLocalContractRunnerExecutionAndRefCap(t *testing.T) {
	tempRoot := t.TempDir()
	bareDir := filepath.Join(tempRoot, "contracts.git")
	workDir := filepath.Join(tempRoot, "work")

	runGit(t, tempRoot, "init", "--bare", bareDir)
	if err := os.MkdirAll(workDir, 0o755); err != nil {
		t.Fatalf("mkdir work: %v", err)
	}
	runGit(t, workDir, "init", "-b", "main")
	if err := os.WriteFile(filepath.Join(workDir, "contract.txt"), []byte("schema-v1"), 0o644); err != nil {
		t.Fatalf("write contract.txt: %v", err)
	}
	runGit(t, workDir, "add", ".")
	runGit(t, workDir, "commit", "-m", "initial contract commit")
	commitSHA := runGit(t, workDir, "rev-parse", "HEAD")
	runGit(t, workDir, "remote", "add", "origin", bareDir)
	runGit(t, workDir, "push", "-u", "origin", "main")

	runner := postreceive.NewContractHookRunner(postreceive.RunnerConfig{
		Timeout:        10 * time.Second,
		MaxRefsPerPush: 2,
		MaxOutputBytes: 32,
		ContractCmd:    "echo contract-verified-ok-1234567890-abcdefghijklmnopqrstuvwxyz",
	})

	updates := []postreceive.RefUpdate{
		{OldRev: postreceive.ZeroSHA, NewRev: commitSHA, Ref: "refs/heads/main"},
		{OldRev: commitSHA, NewRev: postreceive.ZeroSHA, Ref: "refs/heads/deleted"},
		{OldRev: postreceive.ZeroSHA, NewRev: commitSHA, Ref: "refs/tags/v1.0.0"},
		{OldRev: postreceive.ZeroSHA, NewRev: commitSHA, Ref: "refs/heads/feat-a"},
		{OldRev: postreceive.ZeroSHA, NewRev: commitSHA, Ref: "refs/heads/feat-b"},
	}

	results, err := runner.ExecuteUpdates(context.Background(), bareDir, updates)
	if err != nil {
		t.Fatalf("ExecuteUpdates failed: %v", err)
	}
	if len(results) != 5 {
		t.Fatalf("expected 5 results, got %d", len(results))
	}

	// 1. refs/heads/main executes and passes with bounded output
	if !results[0].Passed || results[0].Skipped {
		t.Fatalf("expected main to execute and pass: %+v", results[0])
	}
	if !strings.Contains(results[0].Output, "contract-verified-ok") || !strings.Contains(results[0].Output, "truncated") {
		t.Fatalf("expected bounded output truncation on main, got: %q", results[0].Output)
	}

	// 2. deleted branch and tag are skipped
	if !results[1].Skipped || !results[2].Skipped {
		t.Fatalf("expected deleted ref and tag ref to be skipped: %+v %+v", results[1], results[2])
	}

	// 3. feat-a executes (2nd active branch <= MaxRefsPerPush=2)
	if !results[3].Passed || results[3].Skipped {
		t.Fatalf("expected feat-a to execute and pass: %+v", results[3])
	}

	// 4. feat-b is skipped because MaxRefsPerPush=2 was reached
	if !results[4].Skipped || !strings.Contains(results[4].Output, "max bounded refs per push") {
		t.Fatalf("expected feat-b skipped due to MaxRefsPerPush cap, got: %+v", results[4])
	}
}

func TestBoundedLocalContractRunnerFailingCommand(t *testing.T) {
	tempRoot := t.TempDir()
	bareDir := filepath.Join(tempRoot, "fail-contracts.git")
	workDir := filepath.Join(tempRoot, "work")

	runGit(t, tempRoot, "init", "--bare", bareDir)
	if err := os.MkdirAll(workDir, 0o755); err != nil {
		t.Fatalf("mkdir work: %v", err)
	}
	runGit(t, workDir, "init", "-b", "main")
	if err := os.WriteFile(filepath.Join(workDir, "contract.txt"), []byte("broken"), 0o644); err != nil {
		t.Fatalf("write contract.txt: %v", err)
	}
	runGit(t, workDir, "add", ".")
	runGit(t, workDir, "commit", "-m", "failing contract commit")
	commitSHA := runGit(t, workDir, "rev-parse", "HEAD")
	runGit(t, workDir, "remote", "add", "origin", bareDir)
	runGit(t, workDir, "push", "-u", "origin", "main")

	runner := postreceive.NewContractHookRunner(postreceive.RunnerConfig{
		Timeout:     5 * time.Second,
		ContractCmd: "echo contract-violation && exit 7",
	})

	results, err := runner.ExecuteUpdates(context.Background(), bareDir, []postreceive.RefUpdate{
		{OldRev: postreceive.ZeroSHA, NewRev: commitSHA, Ref: "refs/heads/main"},
	})
	if err != nil {
		t.Fatalf("ExecuteUpdates returned unexpected error: %v", err)
	}
	if len(results) != 1 || results[0].Passed || results[0].ExitCode != 7 {
		t.Fatalf("expected failed contract result with exit 7, got: %+v", results)
	}
	if !strings.Contains(results[0].Output, "contract-violation") {
		t.Fatalf("expected failure output captured, got: %q", results[0].Output)
	}
}
