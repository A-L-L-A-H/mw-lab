package postreceive

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

const (
	ZeroSHA                   = "0000000000000000000000000000000000000000"
	DefaultContractTimeout    = 30 * time.Second
	DefaultMaxRefsPerPush     = 3
	DefaultMaxOutputBytes     = 32 * 1024
	DefaultHookRelativeTarget = "hooks/post-receive"
)

var (
	ErrBareRepoNotFound = errors.New("bare repository not found")
	ErrInvalidHookInput = errors.New("invalid post-receive ref update line")
)

type RefUpdate struct {
	OldRev string `json:"oldRev"`
	NewRev string `json:"newRev"`
	Ref    string `json:"ref"`
}

type ContractResult struct {
	Ref        string `json:"ref"`
	CommitSHA  string `json:"commitSha"`
	Command    string `json:"command"`
	Passed     bool   `json:"passed"`
	ExitCode   int    `json:"exitCode"`
	DurationMs int64  `json:"durationMs"`
	Output     string `json:"output"`
	Skipped    bool   `json:"skipped"`
}

type RunnerConfig struct {
	Timeout        time.Duration
	MaxRefsPerPush int
	MaxOutputBytes int
	ContractCmd    string
	RunnerScript   string
}

type ContractHookRunner struct {
	cfg RunnerConfig
}

func NewContractHookRunner(cfg RunnerConfig) *ContractHookRunner {
	if cfg.Timeout <= 0 {
		cfg.Timeout = DefaultContractTimeout
	}
	if cfg.MaxRefsPerPush <= 0 {
		cfg.MaxRefsPerPush = DefaultMaxRefsPerPush
	}
	if cfg.MaxOutputBytes <= 0 {
		cfg.MaxOutputBytes = DefaultMaxOutputBytes
	}
	return &ContractHookRunner{cfg: cfg}
}

func RenderPostReceiveHookScript(runnerScriptPath string, timeoutSecs int, maxCommits int) string {
	if timeoutSecs <= 0 {
		timeoutSecs = int(DefaultContractTimeout.Seconds())
	}
	if maxCommits <= 0 {
		maxCommits = DefaultMaxRefsPerPush
	}
	escapedRunner := strings.ReplaceAll(strings.TrimSpace(runnerScriptPath), `\`, `/`)
	return fmt.Sprintf(`#!/usr/bin/env bash
# V29: mw-lab bare-repository post-receive hook
# Triggers bounded local contract tests on pushed branch commits.

set -euo pipefail

export MW_LAB_CONTRACT_TIMEOUT_SECS="${MW_LAB_CONTRACT_TIMEOUT_SECS:-%d}"
export MW_LAB_CONTRACT_MAX_COMMITS="${MW_LAB_CONTRACT_MAX_COMMITS:-%d}"
export MW_LAB_CONTRACT_RUNNER="${MW_LAB_CONTRACT_RUNNER:-%s}"

ZERO_SHA="0000000000000000000000000000000000000000"
HOOK_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
BARE_REPO_DIR="${GIT_DIR:-$(cd "${HOOK_DIR}/.." && pwd)}"

processed=0
status=0

while read -r old_rev new_rev ref_name; do
  [[ -z "${ref_name:-}" ]] && continue
  [[ "${new_rev}" == "${ZERO_SHA}" ]] && continue
  [[ "${ref_name}" != refs/heads/* ]] && continue

  processed=$((processed + 1))
  if [[ "${processed}" -gt "${MW_LAB_CONTRACT_MAX_COMMITS}" ]]; then
    echo "[mw-lab:v29] bounded ref limit (${MW_LAB_CONTRACT_MAX_COMMITS}) reached"
    break
  fi

  if [[ -n "${MW_LAB_CONTRACT_RUNNER}" && -f "${MW_LAB_CONTRACT_RUNNER}" ]]; then
    if ! bash "${MW_LAB_CONTRACT_RUNNER}" \
      --bare-repo "${BARE_REPO_DIR}" \
      --ref "${ref_name}" \
      --old-rev "${old_rev}" \
      --new-rev "${new_rev}" \
      --timeout "${MW_LAB_CONTRACT_TIMEOUT_SECS}"; then
      status=1
    fi
  fi
done

exit "${status}"
`, timeoutSecs, maxCommits, escapedRunner)
}

func InstallPostReceiveHook(bareRepoPath, hookSourcePath, runnerScriptPath string) (string, error) {
	trimmedRepo := strings.TrimSpace(bareRepoPath)
	if trimmedRepo == "" {
		return "", fmt.Errorf("%w: empty path", ErrBareRepoNotFound)
	}
	absRepo, err := filepath.Abs(trimmedRepo)
	if err != nil {
		return "", err
	}
	headInfo, statErr := os.Stat(filepath.Join(absRepo, "HEAD"))
	if statErr != nil || headInfo.IsDir() {
		return "", fmt.Errorf("%w: %s", ErrBareRepoNotFound, absRepo)
	}

	hooksDir := filepath.Join(absRepo, "hooks")
	if err := os.MkdirAll(hooksDir, 0o755); err != nil {
		return "", fmt.Errorf("create hooks directory: %w", err)
	}

	var scriptBytes []byte
	if trimmedSrc := strings.TrimSpace(hookSourcePath); trimmedSrc != "" {
		raw, readErr := os.ReadFile(trimmedSrc)
		if readErr != nil {
			return "", fmt.Errorf("read hook template: %w", readErr)
		}
		scriptBytes = bytes.ReplaceAll(raw, []byte("\r\n"), []byte("\n"))
	} else {
		scriptBytes = []byte(RenderPostReceiveHookScript(runnerScriptPath, 30, DefaultMaxRefsPerPush))
	}

	hookPath := filepath.Join(hooksDir, "post-receive")
	if err := os.WriteFile(hookPath, scriptBytes, 0o755); err != nil {
		return "", fmt.Errorf("write post-receive hook: %w", err)
	}
	_ = os.Chmod(hookPath, 0o755)
	return hookPath, nil
}

func ParsePostReceiveStdin(r io.Reader) ([]RefUpdate, error) {
	scanner := bufio.NewScanner(r)
	updates := make([]RefUpdate, 0)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) != 3 {
			return nil, fmt.Errorf("%w: %q", ErrInvalidHookInput, line)
		}
		updates = append(updates, RefUpdate{
			OldRev: fields[0],
			NewRev: fields[1],
			Ref:    fields[2],
		})
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return updates, nil
}

func (r *ContractHookRunner) ExecuteUpdates(ctx context.Context, bareRepoPath string, updates []RefUpdate) ([]ContractResult, error) {
	trimmedRepo := strings.TrimSpace(bareRepoPath)
	if trimmedRepo == "" {
		return nil, fmt.Errorf("%w: empty bareRepoPath", ErrBareRepoNotFound)
	}
	absRepo, err := filepath.Abs(trimmedRepo)
	if err != nil {
		return nil, err
	}
	if _, statErr := os.Stat(filepath.Join(absRepo, "HEAD")); statErr != nil {
		return nil, fmt.Errorf("%w: %s", ErrBareRepoNotFound, absRepo)
	}

	results := make([]ContractResult, 0, len(updates))
	executed := 0

	for _, upd := range updates {
		if upd.NewRev == "" || upd.NewRev == ZeroSHA || !strings.HasPrefix(upd.Ref, "refs/heads/") {
			results = append(results, ContractResult{
				Ref:       upd.Ref,
				CommitSHA: upd.NewRev,
				Passed:    true,
				Skipped:   true,
			})
			continue
		}

		if executed >= r.cfg.MaxRefsPerPush {
			results = append(results, ContractResult{
				Ref:       upd.Ref,
				CommitSHA: upd.NewRev,
				Passed:    true,
				Skipped:   true,
				Output:    fmt.Sprintf("skipped: max bounded refs per push (%d) reached", r.cfg.MaxRefsPerPush),
			})
			continue
		}
		executed++

		res := r.runSingleRefContract(ctx, absRepo, upd)
		results = append(results, res)
	}
	return results, nil
}

func (r *ContractHookRunner) runSingleRefContract(ctx context.Context, bareRepoPath string, upd RefUpdate) ContractResult {
	start := time.Now()
	runCtx, cancel := context.WithTimeout(ctx, r.cfg.Timeout)
	defer cancel()

	tmpWork, err := os.MkdirTemp("", "mw-lab-contract-*")
	if err != nil {
		return ContractResult{
			Ref:       upd.Ref,
			CommitSHA: upd.NewRev,
			Passed:    false,
			ExitCode:  -1,
			Output:    fmt.Sprintf("create temp dir failed: %v", err),
		}
	}
	defer func() { _ = os.RemoveAll(tmpWork) }()

	cloneCmd := exec.CommandContext(runCtx, "git", "clone", "--no-checkout", "--", bareRepoPath, tmpWork)
	cloneCmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	if out, cloneErr := cloneCmd.CombinedOutput(); cloneErr != nil {
		return ContractResult{
			Ref:        upd.Ref,
			CommitSHA:  upd.NewRev,
			Passed:     false,
			ExitCode:   -1,
			DurationMs: time.Since(start).Milliseconds(),
			Output:     boundOutput(fmt.Sprintf("git clone failed: %v (%s)", cloneErr, strings.TrimSpace(string(out))), r.cfg.MaxOutputBytes),
		}
	}

	checkoutCmd := exec.CommandContext(runCtx, "git", "checkout", "--detach", upd.NewRev)
	checkoutCmd.Dir = tmpWork
	checkoutCmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	if out, coErr := checkoutCmd.CombinedOutput(); coErr != nil {
		return ContractResult{
			Ref:        upd.Ref,
			CommitSHA:  upd.NewRev,
			Passed:     false,
			ExitCode:   -1,
			DurationMs: time.Since(start).Milliseconds(),
			Output:     boundOutput(fmt.Sprintf("git checkout %s failed: %v (%s)", upd.NewRev, coErr, strings.TrimSpace(string(out))), r.cfg.MaxOutputBytes),
		}
	}

	contractCmd := strings.TrimSpace(r.cfg.ContractCmd)
	if contractCmd == "" {
		contractCmd = detectWorkspaceContractCommand(tmpWork)
	}
	if contractCmd == "" {
		return ContractResult{
			Ref:        upd.Ref,
			CommitSHA:  upd.NewRev,
			Passed:     true,
			Skipped:    true,
			DurationMs: time.Since(start).Milliseconds(),
			Output:     "no contract suite detected",
		}
	}

	var shellCmd *exec.Cmd
	if runtime.GOOS == "windows" {
		shellCmd = exec.CommandContext(runCtx, "cmd", "/C", contractCmd)
	} else {
		shellCmd = exec.CommandContext(runCtx, "sh", "-c", contractCmd)
	}
	shellCmd.Dir = tmpWork
	shellCmd.Env = append(
		os.Environ(),
		"CI=true",
		"GIT_TERMINAL_PROMPT=0",
		"MW_CONTRACT_REF="+upd.Ref,
		"MW_CONTRACT_OLD_REV="+upd.OldRev,
		"MW_CONTRACT_NEW_REV="+upd.NewRev,
	)

	rawOut, execErr := shellCmd.CombinedOutput()
	durationMs := time.Since(start).Milliseconds()
	clipped := boundOutput(string(rawOut), r.cfg.MaxOutputBytes)

	if execErr == nil {
		return ContractResult{
			Ref:        upd.Ref,
			CommitSHA:  upd.NewRev,
			Command:    contractCmd,
			Passed:     true,
			ExitCode:   0,
			DurationMs: durationMs,
			Output:     clipped,
		}
	}

	exitCode := 1
	var exitErr *exec.ExitError
	if errors.As(execErr, &exitErr) {
		exitCode = exitErr.ExitCode()
	} else if errors.Is(runCtx.Err(), context.DeadlineExceeded) {
		exitCode = 124
		clipped = boundOutput(clipped+"\n[mw-lab:v29] contract test timed out", r.cfg.MaxOutputBytes)
	}

	return ContractResult{
		Ref:        upd.Ref,
		CommitSHA:  upd.NewRev,
		Command:    contractCmd,
		Passed:     false,
		ExitCode:   exitCode,
		DurationMs: durationMs,
		Output:     clipped,
	}
}

func detectWorkspaceContractCommand(workDir string) string {
	if _, err := os.Stat(filepath.Join(workDir, ".mw-contract-test.sh")); err == nil {
		return "bash ./.mw-contract-test.sh"
	}
	if _, err := os.Stat(filepath.Join(workDir, "scripts", "contract-test.sh")); err == nil {
		return "bash ./scripts/contract-test.sh"
	}
	if _, err := os.Stat(filepath.Join(workDir, "go.mod")); err == nil {
		return "go test -run TestContract ./..."
	}
	if _, err := os.Stat(filepath.Join(workDir, "package.json")); err == nil {
		return "npm run test:contract --if-present"
	}
	return ""
}

func boundOutput(text string, maxBytes int) string {
	if maxBytes <= 0 || len(text) <= maxBytes {
		return text
	}
	return text[:maxBytes] + "\n...[contract output truncated]"
}
