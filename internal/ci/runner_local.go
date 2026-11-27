package ci

import (
	"bytes"
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"time"
)

const (
	StatusQueued    = "queued"
	StatusRunning   = "running"
	StatusPassed    = "passed"
	StatusFailed    = "failed"
	StatusCancelled = "cancelled"

	defaultMaxConcurrent = 2
	defaultRunTimeout    = 10 * time.Minute
	maxStageOutputBytes  = 64 * 1024
	maxRunLogsBytes      = 256 * 1024
)

var (
	ErrRunNotFound   = errors.New("ci run not found")
	ErrInvalidSlug   = errors.New("invalid repository slug")
	slugRegex        = regexp.MustCompile(`^[a-zA-Z0-9._-]+$`)
	commitSHAPattern = regexp.MustCompile(`^[0-9a-fA-F]{7,64}$`)
)

type StageResult struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	Command    string `json:"command"`
	Status     string `json:"status"`
	ExitCode   int    `json:"exitCode"`
	DurationMs int64  `json:"durationMs"`
	Output     string `json:"output,omitempty"`
}

type RunRecord struct {
	ID         string        `json:"id"`
	RepoOwner  string        `json:"repoOwner"`
	RepoName   string        `json:"repoName"`
	FullName   string        `json:"fullName"`
	Ref        string        `json:"ref"`
	SHA        string        `json:"sha"`
	Pusher     string        `json:"pusher"`
	Source     string        `json:"source"`
	Status     string        `json:"status"`
	Summary    string        `json:"summary"`
	Logs       string        `json:"logs,omitempty"`
	Stages     []StageResult `json:"stages"`
	DurationMs int64         `json:"durationMs"`
	StartedAt  string        `json:"startedAt,omitempty"`
	FinishedAt string        `json:"finishedAt,omitempty"`
	CreatedAt  string        `json:"createdAt"`
}

type RunFilter struct {
	Owner  string
	Repo   string
	Status string
	Limit  int
}

func ValidateSlug(slug string) error {
	trimmed := strings.TrimSpace(slug)
	if trimmed == "" || len(trimmed) > 128 {
		return fmt.Errorf("%w: must be 1..128 characters", ErrInvalidSlug)
	}
	if !slugRegex.MatchString(trimmed) {
		return fmt.Errorf("%w: %q does not match ^[a-zA-Z0-9._-]+$", ErrInvalidSlug, slug)
	}
	if trimmed == "." || trimmed == ".." || strings.Contains(trimmed, "..") {
		return fmt.Errorf("%w: dot traversal forbidden", ErrInvalidSlug)
	}
	if strings.HasPrefix(trimmed, "-") {
		return fmt.Errorf("%w: leading hyphen forbidden", ErrInvalidSlug)
	}
	return nil
}

type RunStore struct {
	db      *sql.DB
	mu      sync.RWMutex
	memRuns []RunRecord
}

func NewRunStore(db *sql.DB) (*RunStore, error) {
	store := &RunStore{
		db:      db,
		memRuns: make([]RunRecord, 0),
	}
	if db != nil {
		if err := store.migrate(); err != nil {
			return nil, err
		}
	}
	return store, nil
}

func (s *RunStore) migrate() error {
	const schema = `
CREATE TABLE IF NOT EXISTS ci_runs (
	id TEXT PRIMARY KEY,
	repo_owner TEXT NOT NULL,
	repo_name TEXT NOT NULL,
	full_name TEXT NOT NULL,
	ref_name TEXT NOT NULL DEFAULT '',
	commit_sha TEXT NOT NULL DEFAULT '',
	pusher TEXT NOT NULL DEFAULT '',
	source TEXT NOT NULL DEFAULT 'local',
	status TEXT NOT NULL,
	summary TEXT NOT NULL DEFAULT '',
	logs TEXT NOT NULL DEFAULT '',
	stages_json TEXT NOT NULL DEFAULT '[]',
	duration_ms INTEGER NOT NULL DEFAULT 0,
	started_at TEXT NOT NULL DEFAULT '',
	finished_at TEXT NOT NULL DEFAULT '',
	created_at TEXT NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_ci_runs_repo_created ON ci_runs(repo_owner, repo_name, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_ci_runs_full_created ON ci_runs(full_name, created_at DESC);
`
	if _, err := s.db.Exec(schema); err != nil {
		return fmt.Errorf("migrate ci_runs schema: %w", err)
	}
	return nil
}

func (s *RunStore) Create(ctx context.Context, record RunRecord) (RunRecord, error) {
	if s == nil {
		return RunRecord{}, errors.New("run store is nil")
	}
	record.RepoOwner = strings.TrimSpace(record.RepoOwner)
	record.RepoName = strings.TrimSpace(record.RepoName)
	if record.FullName == "" {
		if record.RepoOwner != "" && record.RepoName != "" {
			record.FullName = record.RepoOwner + "/" + record.RepoName
		} else {
			record.FullName = record.RepoName
		}
	}
	if strings.TrimSpace(record.ID) == "" {
		record.ID = generateRunID()
	}
	if strings.TrimSpace(record.Status) == "" {
		record.Status = StatusQueued
	}
	if strings.TrimSpace(record.Source) == "" {
		record.Source = "local"
	}
	if strings.TrimSpace(record.CreatedAt) == "" {
		record.CreatedAt = time.Now().UTC().Format(time.RFC3339Nano)
	}
	if record.Stages == nil {
		record.Stages = []StageResult{}
	}

	if s.db == nil {
		s.mu.Lock()
		defer s.mu.Unlock()
		copied := cloneRunRecord(record)
		s.memRuns = append(s.memRuns, copied)
		return cloneRunRecord(copied), nil
	}

	stagesBytes, err := json.Marshal(record.Stages)
	if err != nil {
		return RunRecord{}, fmt.Errorf("marshal stages: %w", err)
	}

	_, err = s.db.ExecContext(ctx, `
		INSERT INTO ci_runs (
			id, repo_owner, repo_name, full_name, ref_name, commit_sha, pusher,
			source, status, summary, logs, stages_json, duration_ms, started_at, finished_at, created_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		record.ID,
		record.RepoOwner,
		record.RepoName,
		record.FullName,
		record.Ref,
		record.SHA,
		record.Pusher,
		record.Source,
		record.Status,
		record.Summary,
		record.Logs,
		string(stagesBytes),
		record.DurationMs,
		record.StartedAt,
		record.FinishedAt,
		record.CreatedAt,
	)
	if err != nil {
		return RunRecord{}, fmt.Errorf("insert ci run: %w", err)
	}
	return cloneRunRecord(record), nil
}

func (s *RunStore) Update(ctx context.Context, record RunRecord) error {
	if s == nil {
		return errors.New("run store is nil")
	}
	if strings.TrimSpace(record.ID) == "" {
		return fmt.Errorf("%w: run id is required", ErrInvalidInput)
	}
	if record.Stages == nil {
		record.Stages = []StageResult{}
	}

	if s.db == nil {
		s.mu.Lock()
		defer s.mu.Unlock()
		for idx := range s.memRuns {
			if s.memRuns[idx].ID == record.ID {
				s.memRuns[idx] = cloneRunRecord(record)
				return nil
			}
		}
		return ErrRunNotFound
	}

	stagesBytes, err := json.Marshal(record.Stages)
	if err != nil {
		return fmt.Errorf("marshal stages: %w", err)
	}

	result, err := s.db.ExecContext(ctx, `
		UPDATE ci_runs
		SET status = ?, summary = ?, logs = ?, stages_json = ?, duration_ms = ?, started_at = ?, finished_at = ?
		WHERE id = ?`,
		record.Status,
		record.Summary,
		record.Logs,
		string(stagesBytes),
		record.DurationMs,
		record.StartedAt,
		record.FinishedAt,
		record.ID,
	)
	if err != nil {
		return fmt.Errorf("update ci run: %w", err)
	}
	affected, err := result.RowsAffected()
	if err == nil && affected == 0 {
		return ErrRunNotFound
	}
	return nil
}

func (s *RunStore) Get(ctx context.Context, id string) (RunRecord, error) {
	if s == nil {
		return RunRecord{}, errors.New("run store is nil")
	}
	trimmedID := strings.TrimSpace(id)
	if trimmedID == "" {
		return RunRecord{}, ErrRunNotFound
	}

	if s.db == nil {
		s.mu.RLock()
		defer s.mu.RUnlock()
		for _, item := range s.memRuns {
			if item.ID == trimmedID {
				return cloneRunRecord(item), nil
			}
		}
		return RunRecord{}, ErrRunNotFound
	}

	row := s.db.QueryRowContext(ctx, `
		SELECT id, repo_owner, repo_name, full_name, ref_name, commit_sha, pusher,
		       source, status, summary, logs, stages_json, duration_ms, started_at, finished_at, created_at
		FROM ci_runs
		WHERE id = ?`, trimmedID)
	record, err := scanRunRecord(row)
	if errors.Is(err, sql.ErrNoRows) {
		return RunRecord{}, ErrRunNotFound
	}
	if err != nil {
		return RunRecord{}, fmt.Errorf("get ci run: %w", err)
	}
	return record, nil
}

func (s *RunStore) LatestByRepo(ctx context.Context, owner, repo string) (RunRecord, error) {
	if s == nil {
		return RunRecord{}, errors.New("run store is nil")
	}
	cleanOwner := strings.TrimSpace(owner)
	cleanRepo := strings.TrimSpace(repo)
	if cleanRepo == "" {
		return RunRecord{}, ErrRunNotFound
	}

	if s.db == nil {
		s.mu.RLock()
		defer s.mu.RUnlock()
		for i := len(s.memRuns) - 1; i >= 0; i-- {
			item := s.memRuns[i]
			if item.RepoName != cleanRepo {
				continue
			}
			if cleanOwner != "" && item.RepoOwner != cleanOwner {
				continue
			}
			return cloneRunRecord(item), nil
		}
		return RunRecord{}, ErrRunNotFound
	}

	var row *sql.Row
	if cleanOwner != "" {
		row = s.db.QueryRowContext(ctx, `
			SELECT id, repo_owner, repo_name, full_name, ref_name, commit_sha, pusher,
			       source, status, summary, logs, stages_json, duration_ms, started_at, finished_at, created_at
			FROM ci_runs
			WHERE repo_owner = ? AND repo_name = ?
			ORDER BY created_at DESC, rowid DESC
			LIMIT 1`, cleanOwner, cleanRepo)
	} else {
		row = s.db.QueryRowContext(ctx, `
			SELECT id, repo_owner, repo_name, full_name, ref_name, commit_sha, pusher,
			       source, status, summary, logs, stages_json, duration_ms, started_at, finished_at, created_at
			FROM ci_runs
			WHERE repo_name = ?
			ORDER BY created_at DESC, rowid DESC
			LIMIT 1`, cleanRepo)
	}

	record, err := scanRunRecord(row)
	if errors.Is(err, sql.ErrNoRows) {
		return RunRecord{}, ErrRunNotFound
	}
	if err != nil {
		return RunRecord{}, fmt.Errorf("latest ci run: %w", err)
	}
	return record, nil
}

func (s *RunStore) List(ctx context.Context, filter RunFilter) ([]RunRecord, error) {
	if s == nil {
		return []RunRecord{}, errors.New("run store is nil")
	}
	limit := filter.Limit
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	cleanOwner := strings.TrimSpace(filter.Owner)
	cleanRepo := strings.TrimSpace(filter.Repo)
	cleanStatus := strings.TrimSpace(filter.Status)

	if s.db == nil {
		s.mu.RLock()
		defer s.mu.RUnlock()
		out := make([]RunRecord, 0, minInt(limit, len(s.memRuns)))
		for i := len(s.memRuns) - 1; i >= 0 && len(out) < limit; i-- {
			item := s.memRuns[i]
			if cleanOwner != "" && item.RepoOwner != cleanOwner {
				continue
			}
			if cleanRepo != "" && item.RepoName != cleanRepo {
				continue
			}
			if cleanStatus != "" && item.Status != cleanStatus {
				continue
			}
			out = append(out, cloneRunRecord(item))
		}
		return out, nil
	}

	clauses := make([]string, 0, 3)
	args := make([]any, 0, 4)
	if cleanOwner != "" {
		clauses = append(clauses, "repo_owner = ?")
		args = append(args, cleanOwner)
	}
	if cleanRepo != "" {
		clauses = append(clauses, "repo_name = ?")
		args = append(args, cleanRepo)
	}
	if cleanStatus != "" {
		clauses = append(clauses, "status = ?")
		args = append(args, cleanStatus)
	}

	query := `
		SELECT id, repo_owner, repo_name, full_name, ref_name, commit_sha, pusher,
		       source, status, summary, logs, stages_json, duration_ms, started_at, finished_at, created_at
		FROM ci_runs`
	if len(clauses) > 0 {
		query += " WHERE " + strings.Join(clauses, " AND ")
	}
	query += " ORDER BY created_at DESC, rowid DESC LIMIT ?"
	args = append(args, limit)

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list ci runs: %w", err)
	}
	defer rows.Close()

	results := make([]RunRecord, 0)
	for rows.Next() {
		record, scanErr := scanRunRecord(rows)
		if scanErr != nil {
			return nil, fmt.Errorf("scan ci run: %w", scanErr)
		}
		results = append(results, record)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate ci runs: %w", err)
	}
	return results, nil
}

type rowScanner interface {
	Scan(dest ...any) error
}

func scanRunRecord(scanner rowScanner) (RunRecord, error) {
	var record RunRecord
	var stagesJSON string
	err := scanner.Scan(
		&record.ID,
		&record.RepoOwner,
		&record.RepoName,
		&record.FullName,
		&record.Ref,
		&record.SHA,
		&record.Pusher,
		&record.Source,
		&record.Status,
		&record.Summary,
		&record.Logs,
		&stagesJSON,
		&record.DurationMs,
		&record.StartedAt,
		&record.FinishedAt,
		&record.CreatedAt,
	)
	if err != nil {
		return RunRecord{}, err
	}
	if strings.TrimSpace(stagesJSON) != "" {
		if unmarshalErr := json.Unmarshal([]byte(stagesJSON), &record.Stages); unmarshalErr != nil {
			record.Stages = []StageResult{}
		}
	}
	if record.Stages == nil {
		record.Stages = []StageResult{}
	}
	return record, nil
}

func cloneRunRecord(in RunRecord) RunRecord {
	out := in
	if in.Stages != nil {
		out.Stages = make([]StageResult, len(in.Stages))
		copy(out.Stages, in.Stages)
	} else {
		out.Stages = []StageResult{}
	}
	return out
}

type CommandExecutor func(ctx context.Context, dir, command string) (output string, exitCode int, err error)

type LocalRunnerConfig struct {
	Store           *RunStore
	MaxConcurrent   int
	DefaultTimeout  time.Duration
	Executor        CommandExecutor
	OnFailure       func(ctx context.Context, run RunRecord)
	AllowLocalClone bool
}

type RunRequest struct {
	Owner    string      `json:"owner"`
	Repo     string      `json:"repo"`
	Ref      string      `json:"ref"`
	SHA      string      `json:"sha"`
	Pusher   string      `json:"pusher"`
	Source   string      `json:"source"`
	WorkDir  string      `json:"workDir,omitempty"`
	CloneURL string      `json:"cloneUrl,omitempty"`
	Stages   []StageTask `json:"stages,omitempty"`
}

type LocalRunner struct {
	store           *RunStore
	sem             chan struct{}
	defaultTimeout  time.Duration
	executor        CommandExecutor
	onFailure       func(ctx context.Context, run RunRecord)
	allowLocalClone bool
	wg              sync.WaitGroup
}

func NewLocalRunner(cfg LocalRunnerConfig) *LocalRunner {
	store := cfg.Store
	if store == nil {
		store, _ = NewRunStore(nil)
	}
	maxConc := cfg.MaxConcurrent
	if maxConc <= 0 {
		maxConc = defaultMaxConcurrent
	}
	timeout := cfg.DefaultTimeout
	if timeout <= 0 {
		timeout = defaultRunTimeout
	}
	executor := cfg.Executor
	if executor == nil {
		executor = defaultCommandExecutor
	}
	return &LocalRunner{
		store:           store,
		sem:             make(chan struct{}, maxConc),
		defaultTimeout:  timeout,
		executor:        executor,
		onFailure:       cfg.OnFailure,
		allowLocalClone: cfg.AllowLocalClone,
	}
}

func (r *LocalRunner) Store() *RunStore {
	if r == nil {
		return nil
	}
	return r.store
}

func (r *LocalRunner) Wait() {
	if r == nil {
		return
	}
	r.wg.Wait()
}

func (r *LocalRunner) Run(ctx context.Context, req RunRequest) (RunRecord, error) {
	if r == nil {
		return RunRecord{}, errors.New("local runner is nil")
	}
	record, err := r.createInitialRecord(ctx, req)
	if err != nil {
		return RunRecord{}, err
	}

	select {
	case r.sem <- struct{}{}:
		defer func() { <-r.sem }()
	case <-ctx.Done():
		r.finalizeFailed(context.Background(), &record, time.Now().UTC(), fmt.Sprintf("cancelled while waiting for runner slot: %v", ctx.Err()))
		return record, ctx.Err()
	}

	r.executeRun(ctx, &record, req)
	return record, nil
}

func (r *LocalRunner) Enqueue(ctx context.Context, req RunRequest) (RunRecord, error) {
	if r == nil {
		return RunRecord{}, errors.New("local runner is nil")
	}
	record, err := r.createInitialRecord(ctx, req)
	if err != nil {
		return RunRecord{}, err
	}

	r.wg.Add(1)
	go func(initial RunRecord, runReq RunRequest) {
		defer r.wg.Done()
		runCtx, cancel := context.WithTimeout(context.Background(), r.defaultTimeout)
		defer cancel()

		r.sem <- struct{}{}
		defer func() { <-r.sem }()

		r.executeRun(runCtx, &initial, runReq)
	}(record, req)

	return record, nil
}

func (r *LocalRunner) createInitialRecord(ctx context.Context, req RunRequest) (RunRecord, error) {
	repoName := strings.TrimSpace(req.Repo)
	if repoName == "" {
		return RunRecord{}, fmt.Errorf("%w: repo is required", ErrInvalidInput)
	}
	owner := strings.TrimSpace(req.Owner)
	refName := strings.TrimSpace(req.Ref)
	if refName == "" {
		refName = "refs/heads/main"
	}
	pusher := strings.TrimSpace(req.Pusher)
	if pusher == "" {
		pusher = "unknown"
	}
	source := strings.TrimSpace(req.Source)
	if source == "" {
		source = "local"
	}

	return r.store.Create(ctx, RunRecord{
		RepoOwner: owner,
		RepoName:  repoName,
		Ref:       refName,
		SHA:       strings.TrimSpace(req.SHA),
		Pusher:    pusher,
		Source:    source,
		Status:    StatusQueued,
		Summary:   "queued",
		Stages:    []StageResult{},
	})
}

func (r *LocalRunner) executeRun(ctx context.Context, record *RunRecord, req RunRequest) {
	started := time.Now().UTC()
	record.Status = StatusRunning
	record.Summary = "running"
	record.StartedAt = started.Format(time.RFC3339Nano)
	_ = r.store.Update(context.Background(), *record)

	if _, hasDeadline := ctx.Deadline(); !hasDeadline && r.defaultTimeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, r.defaultTimeout)
		defer cancel()
	}

	workspaceDir, cleanup, err := r.prepareWorkspace(ctx, req)
	if cleanup != nil {
		defer cleanup()
	}
	if err != nil {
		r.finalizeFailed(ctx, record, started, fmt.Sprintf("workspace preparation failed: %v", err))
		return
	}

	stages, err := resolveStages(workspaceDir, req.Stages)
	if err != nil {
		r.finalizeFailed(ctx, record, started, err.Error())
		return
	}

	orderedStages, err := orderStages(stages)
	if err != nil {
		r.finalizeFailed(ctx, record, started, err.Error())
		return
	}

	var logBuilder strings.Builder
	stageResults := make([]StageResult, 0, len(orderedStages))
	failedIndex := -1
	failureReason := ""

	for idx, stage := range orderedStages {
		if ctxErr := ctx.Err(); ctxErr != nil {
			failedIndex = idx
			failureReason = fmt.Sprintf("run timed out or cancelled before stage %q: %v", stage.ID, ctxErr)
			stageResults = append(stageResults, StageResult{
				ID:       stage.ID,
				Name:     stage.Name,
				Command:  stage.Command,
				Status:   StatusFailed,
				ExitCode: -1,
				Output:   truncateOutput(failureReason, maxStageOutputBytes),
			})
			break
		}

		stageStart := time.Now()
		output, exitCode, execErr := r.executor(ctx, workspaceDir, stage.Command)
		durationMs := time.Since(stageStart).Milliseconds()
		clippedOut := truncateOutput(output, maxStageOutputBytes)

		if logBuilder.Len() < maxRunLogsBytes {
			fmt.Fprintf(&logBuilder, "=== [%s] %s ===\n%s\n", stage.ID, stage.Command, clippedOut)
		}

		stageStatus := StatusPassed
		if execErr != nil || exitCode != 0 {
			stageStatus = StatusFailed
			if exitCode == 0 {
				exitCode = 1
			}
			if ctx.Err() != nil {
				failureReason = fmt.Sprintf("stage %q timed out or cancelled: %v", stage.ID, ctx.Err())
			} else if execErr != nil {
				failureReason = fmt.Sprintf("stage %q failed (exit %d): %v", stage.ID, exitCode, execErr)
			} else {
				failureReason = fmt.Sprintf("stage %q exited with code %d", stage.ID, exitCode)
			}
			failedIndex = idx
		}

		stageResults = append(stageResults, StageResult{
			ID:         stage.ID,
			Name:       stage.Name,
			Command:    stage.Command,
			Status:     stageStatus,
			ExitCode:   exitCode,
			DurationMs: durationMs,
			Output:     clippedOut,
		})
		if failedIndex >= 0 {
			break
		}
	}

	if failedIndex >= 0 {
		for rem := failedIndex + 1; rem < len(orderedStages); rem++ {
			skipped := orderedStages[rem]
			stageResults = append(stageResults, StageResult{
				ID:      skipped.ID,
				Name:    skipped.Name,
				Command: skipped.Command,
				Status:  "skipped",
			})
		}
		record.Stages = stageResults
		record.Logs = truncateOutput(logBuilder.String(), maxRunLogsBytes)
		r.finalizeFailed(ctx, record, started, failureReason)
		return
	}

	finished := time.Now().UTC()
	record.Stages = stageResults
	record.Logs = truncateOutput(logBuilder.String(), maxRunLogsBytes)
	record.Status = StatusPassed
	record.Summary = fmt.Sprintf("passed %d stage(s)", len(stageResults))
	record.FinishedAt = finished.Format(time.RFC3339Nano)
	record.DurationMs = finished.Sub(started).Milliseconds()
	_ = r.store.Update(context.Background(), *record)
}

func (r *LocalRunner) finalizeFailed(_ context.Context, record *RunRecord, started time.Time, summary string) {
	finished := time.Now().UTC()
	record.Status = StatusFailed
	record.Summary = strings.TrimSpace(summary)
	record.FinishedAt = finished.Format(time.RFC3339Nano)
	record.DurationMs = finished.Sub(started).Milliseconds()
	_ = r.store.Update(context.Background(), *record)

	if r.onFailure != nil {
		notifyCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		r.onFailure(notifyCtx, cloneRunRecord(*record))
	}
}

func (r *LocalRunner) prepareWorkspace(ctx context.Context, req RunRequest) (string, func(), error) {
	if workDir := strings.TrimSpace(req.WorkDir); workDir != "" {
		absDir, err := filepath.Abs(workDir)
		if err != nil {
			return "", nil, fmt.Errorf("resolve workDir: %w", err)
		}
		info, err := os.Stat(absDir)
		if err != nil || !info.IsDir() {
			return "", nil, fmt.Errorf("workDir %q is not an accessible directory", absDir)
		}
		return absDir, nil, nil
	}

	cloneURL := strings.TrimSpace(req.CloneURL)
	if cloneURL == "" {
		return "", nil, errors.New("neither workDir nor cloneUrl was provided")
	}
	if err := validateCloneSource(cloneURL, r.allowLocalClone); err != nil {
		return "", nil, err
	}

	tmpRoot, err := os.MkdirTemp("", "mw-lab-ci-*")
	if err != nil {
		return "", nil, fmt.Errorf("create temp workspace: %w", err)
	}
	cleanup := func() {
		_ = os.RemoveAll(tmpRoot)
	}

	repoDir := filepath.Join(tmpRoot, "repo")
	cloneOut, err := runGitCmd(ctx, tmpRoot, "clone", "--", cloneURL, repoDir)
	if err != nil {
		cleanup()
		return "", nil, fmt.Errorf("git clone failed: %w (%s)", err, strings.TrimSpace(cloneOut))
	}

	commitSHA := strings.TrimSpace(req.SHA)
	if commitSHA != "" && commitSHA != zeroCommitSHA {
		if !commitSHAPattern.MatchString(commitSHA) {
			cleanup()
			return "", nil, fmt.Errorf("invalid commit sha %q", commitSHA)
		}
		checkoutOut, checkoutErr := runGitCmd(ctx, repoDir, "checkout", "--detach", commitSHA)
		if checkoutErr != nil {
			_, _ = runGitCmd(ctx, repoDir, "fetch", "origin", commitSHA)
			checkoutOut, checkoutErr = runGitCmd(ctx, repoDir, "checkout", "--detach", commitSHA)
			if checkoutErr != nil {
				cleanup()
				return "", nil, fmt.Errorf("git checkout %s failed: %w (%s)", commitSHA, checkoutErr, strings.TrimSpace(checkoutOut))
			}
		}
	} else if refName := strings.TrimSpace(req.Ref); refName != "" {
		branch := strings.TrimPrefix(refName, "refs/heads/")
		if branch != "" && !strings.HasPrefix(branch, "-") {
			_, _ = runGitCmd(ctx, repoDir, "checkout", branch)
		}
	}

	return repoDir, cleanup, nil
}

func validateCloneSource(cloneURL string, allowLocal bool) error {
	if cloneURL == "" || strings.HasPrefix(cloneURL, "-") || strings.ContainsAny(cloneURL, "\r\n\x00") {
		return errors.New("malformed cloneUrl")
	}
	parsed, err := url.Parse(cloneURL)
	if err == nil {
		switch strings.ToLower(parsed.Scheme) {
		case "http", "https", "git", "ssh":
			if parsed.Host == "" {
				return errors.New("cloneUrl missing host")
			}
			return nil
		case "file":
			if !allowLocal {
				return errors.New("file:// cloneUrl is disabled")
			}
			return nil
		}
	}
	if allowLocal {
		if info, statErr := os.Stat(cloneURL); statErr == nil && info.IsDir() {
			return nil
		}
	}
	return fmt.Errorf("unsupported or inaccessible cloneUrl %q", cloneURL)
}

func runGitCmd(ctx context.Context, dir string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "CI=true")
	var buf limitedBuffer
	buf.limit = maxStageOutputBytes
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	err := cmd.Run()
	return buf.String(), err
}

func resolveStages(workspaceDir string, explicit []StageTask) ([]StageTask, error) {
	if len(explicit) > 0 {
		out := make([]StageTask, 0, len(explicit))
		for i, stage := range explicit {
			if strings.TrimSpace(stage.Command) == "" {
				return nil, fmt.Errorf("stage %d has empty command", i)
			}
			if strings.TrimSpace(stage.ID) == "" {
				stage.ID = fmt.Sprintf("stage-%d", i+1)
			}
			if strings.TrimSpace(stage.Name) == "" {
				stage.Name = stage.ID
			}
			out = append(out, stage)
		}
		return out, nil
	}

	ciFilePath := filepath.Join(workspaceDir, ".mw-ci.json")
	if rawBytes, err := os.ReadFile(ciFilePath); err == nil {
		var parsed struct {
			Stages []StageTask `json:"stages"`
			Tasks  []StageTask `json:"tasks"`
		}
		if jsonErr := json.Unmarshal(rawBytes, &parsed); jsonErr != nil {
			return nil, fmt.Errorf("parse .mw-ci.json: %w", jsonErr)
		}
		configured := parsed.Stages
		if len(configured) == 0 {
			configured = parsed.Tasks
		}
		if len(configured) > 0 {
			return resolveStages(workspaceDir, configured)
		}
	}

	if _, err := os.Stat(filepath.Join(workspaceDir, "go.mod")); err == nil {
		return []StageTask{
			{ID: "lint", Name: "Lint", Stage: "lint", DependsOn: []string{}, Command: "go vet ./..."},
			{ID: "test", Name: "Test", Stage: "test", DependsOn: []string{"lint"}, Command: "go test -count=1 ./..."},
			{ID: "build", Name: "Build", Stage: "build", DependsOn: []string{"test"}, Command: "go build ./..."},
		}, nil
	}
	if _, err := os.Stat(filepath.Join(workspaceDir, "Cargo.toml")); err == nil {
		return []StageTask{
			{ID: "check", Name: "Check", Stage: "lint", DependsOn: []string{}, Command: "cargo check"},
			{ID: "test", Name: "Test", Stage: "test", DependsOn: []string{"check"}, Command: "cargo test"},
		}, nil
	}
	if _, err := os.Stat(filepath.Join(workspaceDir, "package.json")); err == nil {
		return []StageTask{
			{ID: "test", Name: "Test", Stage: "test", DependsOn: []string{}, Command: "npm test"},
		}, nil
	}

	return nil, errors.New("no CI stages configured or detected")
}

func orderStages(stages []StageTask) ([]StageTask, error) {
	byID := make(map[string]StageTask, len(stages))
	order := make([]string, 0, len(stages))
	for _, st := range stages {
		if _, dup := byID[st.ID]; dup {
			return nil, fmt.Errorf("duplicate stage id %q", st.ID)
		}
		byID[st.ID] = st
		order = append(order, st.ID)
	}

	state := make(map[string]int, len(stages)) // 0=unvisited, 1=visiting, 2=visited
	result := make([]StageTask, 0, len(stages))
	var visit func(id string) error
	visit = func(id string) error {
		switch state[id] {
		case 1:
			return fmt.Errorf("dependency cycle detected at stage %q", id)
		case 2:
			return nil
		}
		state[id] = 1
		st, ok := byID[id]
		if !ok {
			return fmt.Errorf("unknown stage dependency %q", id)
		}
		for _, dep := range st.DependsOn {
			if err := visit(dep); err != nil {
				return err
			}
		}
		state[id] = 2
		result = append(result, st)
		return nil
	}

	for _, id := range order {
		if err := visit(id); err != nil {
			return nil, err
		}
	}
	return result, nil
}

func defaultCommandExecutor(ctx context.Context, dir, command string) (string, int, error) {
	var cmd *exec.Cmd
	if runtime.GOOS == "windows" {
		cmd = exec.CommandContext(ctx, "cmd", "/C", command)
	} else {
		cmd = exec.CommandContext(ctx, "sh", "-c", command)
	}
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "CI=true", "GIT_TERMINAL_PROMPT=0")

	var buf limitedBuffer
	buf.limit = maxStageOutputBytes
	cmd.Stdout = &buf
	cmd.Stderr = &buf

	err := cmd.Run()
	if err == nil {
		return buf.String(), 0, nil
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return buf.String(), exitErr.ExitCode(), err
	}
	return buf.String(), -1, err
}

type limitedBuffer struct {
	buf       bytes.Buffer
	limit     int
	truncated bool
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	total := len(p)
	if b.limit <= 0 {
		return total, nil
	}
	remaining := b.limit - b.buf.Len()
	if remaining > 0 {
		if len(p) > remaining {
			_, _ = b.buf.Write(p[:remaining])
			b.truncated = true
		} else {
			_, _ = b.buf.Write(p)
		}
	} else {
		b.truncated = true
	}
	return total, nil
}

func (b *limitedBuffer) String() string {
	if !b.truncated {
		return b.buf.String()
	}
	return b.buf.String() + "\n...[output truncated]"
}

func truncateOutput(s string, maxBytes int) string {
	if maxBytes <= 0 || len(s) <= maxBytes {
		return s
	}
	return s[:maxBytes] + "\n...[output truncated]"
}

func generateRunID() string {
	var raw [8]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return fmt.Sprintf("run-%d", time.Now().UTC().UnixNano())
	}
	return "run-" + hex.EncodeToString(raw[:])
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}
