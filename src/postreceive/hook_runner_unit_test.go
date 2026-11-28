package postreceive

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func runGitUnit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(
		os.Environ(),
		"GIT_TERMINAL_PROMPT=0",
		"GIT_AUTHOR_NAME=Unit Tester",
		"GIT_AUTHOR_EMAIL=unit@moonwitness.local",
		"GIT_COMMITTER_NAME=Unit Tester",
		"GIT_COMMITTER_EMAIL=unit@moonwitness.local",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v failed: %v\n%s", args, err, string(out))
	}
	return strings.TrimSpace(string(out))
}

func seedBareRepoForUnitTest(t *testing.T, withCommit bool) (string, string) {
	t.Helper()
	root := t.TempDir()
	bareDir := filepath.Join(root, "repo.git")
	runGitUnit(t, root, "init", "--bare", bareDir)
	if !withCommit {
		return bareDir, ""
	}
	workDir := filepath.Join(root, "work")
	if err := os.MkdirAll(workDir, 0o755); err != nil {
		t.Fatalf("mkdir work: %v", err)
	}
	runGitUnit(t, workDir, "init", "-b", "main")
	if err := os.WriteFile(filepath.Join(workDir, "README.md"), []byte("hello\n"), 0o644); err != nil {
		t.Fatalf("write README: %v", err)
	}
	runGitUnit(t, workDir, "add", ".")
	runGitUnit(t, workDir, "commit", "-m", "init commit")
	sha := runGitUnit(t, workDir, "rev-parse", "HEAD")
	runGitUnit(t, workDir, "remote", "add", "origin", bareDir)
	runGitUnit(t, workDir, "push", "-u", "origin", "main")
	return bareDir, sha
}

func TestNewContractHookRunnerDefaults(t *testing.T) {
	runner := NewContractHookRunner(RunnerConfig{})
	if runner.cfg.Timeout != DefaultContractTimeout {
		t.Fatalf("expected default timeout %v, got %v", DefaultContractTimeout, runner.cfg.Timeout)
	}
	if runner.cfg.MaxRefsPerPush != DefaultMaxRefsPerPush {
		t.Fatalf("expected default max refs %d, got %d", DefaultMaxRefsPerPush, runner.cfg.MaxRefsPerPush)
	}
	if runner.cfg.MaxOutputBytes != DefaultMaxOutputBytes {
		t.Fatalf("expected default max output %d, got %d", DefaultMaxOutputBytes, runner.cfg.MaxOutputBytes)
	}
}

func TestRenderAndInstallPostReceiveHookUnit(t *testing.T) {
	rendered := RenderPostReceiveHookScript(`C:\scripts\runner.sh`, 0, 0)
	if !strings.Contains(rendered, "MW_LAB_CONTRACT_TIMEOUT_SECS:-30") {
		t.Fatalf("expected default timeout in script:\n%s", rendered)
	}
	if !strings.Contains(rendered, "MW_LAB_CONTRACT_MAX_COMMITS:-3") {
		t.Fatalf("expected default max commits in script:\n%s", rendered)
	}
	if !strings.Contains(rendered, "C:/scripts/runner.sh") {
		t.Fatalf("expected normalized runner path in script:\n%s", rendered)
	}

	if _, err := InstallPostReceiveHook("  ", "", ""); !errors.Is(err, ErrBareRepoNotFound) {
		t.Fatalf("expected ErrBareRepoNotFound on empty path, got %v", err)
	}
	if _, err := InstallPostReceiveHook(filepath.Join(t.TempDir(), "missing.git"), "", ""); !errors.Is(
		err, ErrBareRepoNotFound,
	) {
		t.Fatalf("expected ErrBareRepoNotFound on missing repo, got %v", err)
	}

	bareDir, _ := seedBareRepoForUnitTest(t, false)
	if _, err := InstallPostReceiveHook(bareDir, filepath.Join(t.TempDir(), "missing-hook"), ""); err == nil {
		t.Fatal("expected error for missing hook template")
	}

	defaultHookPath, err := InstallPostReceiveHook(bareDir, "", `C:\mw\run.sh`)
	if err != nil {
		t.Fatalf("InstallPostReceiveHook default render failed: %v", err)
	}
	rawDefault, _ := os.ReadFile(defaultHookPath)
	if !strings.Contains(string(rawDefault), "C:/mw/run.sh") {
		t.Fatalf("expected rendered runner path in installed hook")
	}

	crlfTemplate := filepath.Join(t.TempDir(), "hook-template")
	if err := os.WriteFile(crlfTemplate, []byte("#!/usr/bin/env bash\r\necho ok\r\n"), 0o644); err != nil {
		t.Fatalf("write crlf template: %v", err)
	}
	customHookPath, err := InstallPostReceiveHook(bareDir, crlfTemplate, "")
	if err != nil {
		t.Fatalf("InstallPostReceiveHook template failed: %v", err)
	}
	rawCustom, _ := os.ReadFile(customHookPath)
	if strings.Contains(string(rawCustom), "\r\n") {
		t.Fatal("expected CRLF normalized to LF in installed hook")
	}
}

func TestParsePostReceiveStdinUnit(t *testing.T) {
	payload := "\n0000 1111 refs/heads/main\n\n2222 3333 refs/tags/v1\n"
	updates, err := ParsePostReceiveStdin(strings.NewReader(payload))
	if err != nil {
		t.Fatalf("ParsePostReceiveStdin failed: %v", err)
	}
	if len(updates) != 2 || updates[0].Ref != "refs/heads/main" || updates[1].NewRev != "3333" {
		t.Fatalf("unexpected updates: %+v", updates)
	}

	if _, err := ParsePostReceiveStdin(strings.NewReader("only-two fields\n")); !errors.Is(err, ErrInvalidHookInput) {
		t.Fatalf("expected ErrInvalidHookInput, got %v", err)
	}
}

func TestExecuteUpdatesRepoValidation(t *testing.T) {
	runner := NewContractHookRunner(RunnerConfig{Timeout: 5 * time.Second, MaxRefsPerPush: 1, MaxOutputBytes: 16})

	if _, err := runner.ExecuteUpdates(context.Background(), "", nil); !errors.Is(err, ErrBareRepoNotFound) {
		t.Fatalf("expected ErrBareRepoNotFound for empty path, got %v", err)
	}
	missingDir := filepath.Join(t.TempDir(), "missing")
	if _, err := runner.ExecuteUpdates(context.Background(), missingDir, nil); !errors.Is(
		err, ErrBareRepoNotFound,
	) {
		t.Fatalf("expected ErrBareRepoNotFound for missing dir, got %v", err)
	}
}

func TestExecuteUpdatesSkipsRefsWithoutContractSuite(t *testing.T) {
	runner := NewContractHookRunner(RunnerConfig{Timeout: 5 * time.Second, MaxRefsPerPush: 1, MaxOutputBytes: 16})

	bareDir, sha := seedBareRepoForUnitTest(t, true)
	results, err := runner.ExecuteUpdates(context.Background(), bareDir, []RefUpdate{
		{OldRev: ZeroSHA, NewRev: sha, Ref: "refs/heads/main"},
		{OldRev: sha, NewRev: ZeroSHA, Ref: "refs/heads/deleted"},
		{OldRev: ZeroSHA, NewRev: sha, Ref: "refs/tags/v1.0.0"},
		{OldRev: ZeroSHA, NewRev: sha, Ref: "refs/heads/second"},
	})
	if err != nil {
		t.Fatalf("ExecuteUpdates failed: %v", err)
	}
	if len(results) != 4 || !results[0].Skipped || results[0].Output != "no contract suite detected" {
		t.Fatalf("expected skipped no-contract result, got: %+v", results)
	}
	if !results[1].Skipped || !results[2].Skipped {
		t.Fatalf("expected deleted/tag refs skipped: %+v", results)
	}
	if !results[3].Passed || !results[3].Skipped {
		t.Fatalf("expected second ref skipped when no suite or ref cap hit: %+v", results[3])
	}
}

func TestExecuteUpdatesRunsContractCommand(t *testing.T) {
	bareDir, sha := seedBareRepoForUnitTest(t, true)
	cmdRunner := NewContractHookRunner(RunnerConfig{
		Timeout:        5 * time.Second,
		MaxRefsPerPush: 1,
		MaxOutputBytes: 24,
		ContractCmd:    "echo ok-contract-output-1234567890",
	})
	cmdResults, err := cmdRunner.ExecuteUpdates(context.Background(), bareDir, []RefUpdate{
		{OldRev: ZeroSHA, NewRev: sha, Ref: "refs/heads/main"},
		{OldRev: ZeroSHA, NewRev: sha, Ref: "refs/heads/extra"},
	})
	if err != nil {
		t.Fatalf("ExecuteUpdates with command failed: %v", err)
	}
	if !cmdResults[0].Passed || cmdResults[0].Skipped || !strings.Contains(cmdResults[0].Output, "truncated") {
		t.Fatalf("unexpected executed result: %+v", cmdResults[0])
	}
	if !cmdResults[1].Skipped || !strings.Contains(cmdResults[1].Output, "max bounded refs") {
		t.Fatalf("expected cap skipped result: %+v", cmdResults[1])
	}
}

func TestExecuteUpdatesReportsContractFailure(t *testing.T) {
	bareDir, sha := seedBareRepoForUnitTest(t, true)
	failRunner := NewContractHookRunner(RunnerConfig{Timeout: 5 * time.Second, ContractCmd: "echo fail && exit 5"})

	failResults, err := failRunner.ExecuteUpdates(context.Background(), bareDir, []RefUpdate{
		{OldRev: ZeroSHA, NewRev: sha, Ref: "refs/heads/main"},
	})
	if err != nil || len(failResults) != 1 || failResults[0].Passed || failResults[0].ExitCode != 5 {
		t.Fatalf("expected exit code 5 failure, got err=%v results=%+v", err, failResults)
	}
}

func TestDetectWorkspaceContractCommandAndBoundOutput(t *testing.T) {
	emptyDir := t.TempDir()
	if cmd := detectWorkspaceContractCommand(emptyDir); cmd != "" {
		t.Fatalf("expected empty command, got %q", cmd)
	}

	pkgDir := t.TempDir()
	_ = os.WriteFile(filepath.Join(pkgDir, "package.json"), []byte("{}"), 0o644)
	if cmd := detectWorkspaceContractCommand(pkgDir); cmd != "npm run test:contract --if-present" {
		t.Fatalf("unexpected package.json command: %q", cmd)
	}

	goDir := t.TempDir()
	_ = os.WriteFile(filepath.Join(goDir, "go.mod"), []byte("module test\n"), 0o644)
	if cmd := detectWorkspaceContractCommand(goDir); cmd != "go test -run TestContract ./..." {
		t.Fatalf("unexpected go.mod command: %q", cmd)
	}

	scriptsDir := t.TempDir()
	_ = os.MkdirAll(filepath.Join(scriptsDir, "scripts"), 0o755)
	_ = os.WriteFile(filepath.Join(scriptsDir, "scripts", "contract-test.sh"), []byte("#!/bin/sh\n"), 0o644)
	if cmd := detectWorkspaceContractCommand(scriptsDir); cmd != "bash ./scripts/contract-test.sh" {
		t.Fatalf("unexpected scripts/contract-test.sh command: %q", cmd)
	}

	mwDir := t.TempDir()
	_ = os.WriteFile(filepath.Join(mwDir, ".mw-contract-test.sh"), []byte("#!/bin/sh\n"), 0o644)
	if cmd := detectWorkspaceContractCommand(mwDir); cmd != "bash ./.mw-contract-test.sh" {
		t.Fatalf("unexpected .mw-contract-test.sh command: %q", cmd)
	}

	if got := boundOutput("short", 0); got != "short" {
		t.Fatalf("expected unchanged string on maxBytes=0, got %q", got)
	}
	if got := boundOutput("abcdefghij", 4); !strings.HasPrefix(got, "abcd") || !strings.Contains(got, "truncated") {
		t.Fatalf("unexpected truncated string: %q", got)
	}
}
