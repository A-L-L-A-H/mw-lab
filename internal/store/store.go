package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

var (
	ErrNotFound      = errors.New("record not found")
	ErrAlreadyExists = errors.New("record already exists")
	ErrInvalidInput  = errors.New("invalid input")
)

type RepoRecord struct {
	ID            int64  `json:"id"`
	Owner         string `json:"owner"`
	Name          string `json:"name"`
	Description   string `json:"description"`
	DefaultBranch string `json:"defaultBranch"`
	Private       bool   `json:"private"`
	MirrorURL     string `json:"mirrorUrl,omitempty"`
	LastSyncAt    string `json:"lastSyncAt,omitempty"`
	CreatedAt     string `json:"createdAt"`
	UpdatedAt     string `json:"updatedAt"`
}

type LFSObjectRecord struct {
	OID        string `json:"oid"`
	RepoOwner  string `json:"repoOwner"`
	RepoName   string `json:"repoName"`
	Size       int64  `json:"size"`
	Backend    string `json:"backend"`
	StorageRef string `json:"storageRef"`
	Uploaded   bool   `json:"uploaded"`
	CreatedAt  string `json:"createdAt"`
	UpdatedAt  string `json:"updatedAt"`
}

type WebhookRecord struct {
	ID        int64  `json:"id"`
	RepoOwner string `json:"repoOwner"`
	RepoName  string `json:"repoName"`
	TargetURL string `json:"targetUrl"`
	Secret    string `json:"secret,omitempty"`
	Events    string `json:"events"`
	Active    bool   `json:"active"`
	CreatedAt string `json:"createdAt"`
	UpdatedAt string `json:"updatedAt"`
}

type MirrorRecord struct {
	ID          int64  `json:"id"`
	RepoOwner   string `json:"repoOwner"`
	RepoName    string `json:"repoName"`
	UpstreamURL string `json:"upstreamUrl"`
	Direction   string `json:"direction"`
	IntervalSec int    `json:"intervalSec"`
	LastStatus  string `json:"lastStatus"`
	LastError   string `json:"lastError,omitempty"`
	LastSyncAt  string `json:"lastSyncAt,omitempty"`
	CreatedAt   string `json:"createdAt"`
	UpdatedAt   string `json:"updatedAt"`
}

type PushEventRecord struct {
	ID             int64  `json:"id"`
	RepoOwner      string `json:"repoOwner"`
	RepoName       string `json:"repoName"`
	RefName        string `json:"refName"`
	BeforeSHA      string `json:"beforeSha"`
	AfterSHA       string `json:"afterSha"`
	Pusher         string `json:"pusher"`
	DeliveryStatus string `json:"deliveryStatus"`
	CreatedAt      string `json:"createdAt"`
}

type HealthMetrics struct {
	RepoCount      int64 `json:"repoCount"`
	WebhookCount   int64 `json:"webhookCount"`
	LFSObjectCount int64 `json:"lfsObjectCount"`
	PushEventCount int64 `json:"pushEventCount"`
}

type Store struct {
	db     *sql.DB
	dbPath string
}

func Open(dbPath string) (*Store, error) {
	trimmedPath := strings.TrimSpace(dbPath)
	if trimmedPath == "" {
		return nil, fmt.Errorf("%w: dbPath is required", ErrInvalidInput)
	}
	if trimmedPath != ":memory:" {
		parentDir := filepath.Dir(trimmedPath)
		if err := os.MkdirAll(parentDir, 0o755); err != nil {
			return nil, fmt.Errorf("create sqlite parent directory: %w", err)
		}
	}

	db, err := sql.Open("sqlite", trimmedPath)
	if err != nil {
		return nil, fmt.Errorf("open sqlite database: %w", err)
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)

	pragmas := []string{
		"PRAGMA journal_mode = WAL;",
		"PRAGMA busy_timeout = 5000;",
		"PRAGMA foreign_keys = ON;",
		"PRAGMA synchronous = NORMAL;",
	}
	for _, pragmaSQL := range pragmas {
		if _, err := db.Exec(pragmaSQL); err != nil {
			_ = db.Close()
			return nil, fmt.Errorf("apply sqlite pragma %q: %w", pragmaSQL, err)
		}
	}

	labStore := &Store{
		db:     db,
		dbPath: trimmedPath,
	}
	if err := labStore.migrate(); err != nil {
		_ = db.Close()
		return nil, err
	}
	return labStore, nil
}

func (s *Store) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	return s.db.Close()
}

func (s *Store) DB() *sql.DB {
	return s.db
}

func (s *Store) migrate() error {
	schema := `
CREATE TABLE IF NOT EXISTS repos (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	owner TEXT NOT NULL,
	name TEXT NOT NULL,
	description TEXT NOT NULL DEFAULT '',
	default_branch TEXT NOT NULL DEFAULT 'main',
	private INTEGER NOT NULL DEFAULT 0,
	mirror_url TEXT NOT NULL DEFAULT '',
	last_sync_at TEXT NOT NULL DEFAULT '',
	created_at TEXT NOT NULL,
	updated_at TEXT NOT NULL,
	UNIQUE(owner, name)
);

CREATE TABLE IF NOT EXISTS lfs_objects (
	oid TEXT PRIMARY KEY,
	repo_owner TEXT NOT NULL,
	repo_name TEXT NOT NULL,
	size INTEGER NOT NULL,
	backend TEXT NOT NULL,
	storage_ref TEXT NOT NULL DEFAULT '',
	uploaded INTEGER NOT NULL DEFAULT 0,
	created_at TEXT NOT NULL,
	updated_at TEXT NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_lfs_objects_repo ON lfs_objects(repo_owner, repo_name);

CREATE TABLE IF NOT EXISTS webhooks (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	repo_owner TEXT NOT NULL DEFAULT '*',
	repo_name TEXT NOT NULL DEFAULT '*',
	target_url TEXT NOT NULL,
	secret TEXT NOT NULL DEFAULT '',
	events TEXT NOT NULL DEFAULT 'push',
	active INTEGER NOT NULL DEFAULT 1,
	created_at TEXT NOT NULL,
	updated_at TEXT NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_webhooks_repo ON webhooks(repo_owner, repo_name);

CREATE TABLE IF NOT EXISTS mirrors (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	repo_owner TEXT NOT NULL,
	repo_name TEXT NOT NULL,
	upstream_url TEXT NOT NULL,
	direction TEXT NOT NULL DEFAULT 'pull',
	interval_sec INTEGER NOT NULL DEFAULT 0,
	last_status TEXT NOT NULL DEFAULT 'idle',
	last_error TEXT NOT NULL DEFAULT '',
	last_sync_at TEXT NOT NULL DEFAULT '',
	created_at TEXT NOT NULL,
	updated_at TEXT NOT NULL,
	UNIQUE(repo_owner, repo_name)
);

CREATE TABLE IF NOT EXISTS push_events (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	repo_owner TEXT NOT NULL,
	repo_name TEXT NOT NULL,
	ref_name TEXT NOT NULL,
	before_sha TEXT NOT NULL,
	after_sha TEXT NOT NULL,
	pusher TEXT NOT NULL DEFAULT '',
	delivery_status TEXT NOT NULL DEFAULT 'recorded',
	created_at TEXT NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_push_events_repo ON push_events(repo_owner, repo_name, id DESC);
`
	if _, err := s.db.Exec(schema); err != nil {
		return fmt.Errorf("migrate sqlite schema: %w", err)
	}
	return nil
}

func nowUTC() string {
	return time.Now().UTC().Format(time.RFC3339Nano)
}

func boolToInt(flag bool) int {
	if flag {
		return 1
	}
	return 0
}

func isUniqueConstraintError(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "unique constraint") || strings.Contains(msg, "constraint failed")
}

func (s *Store) Metrics(ctx context.Context) (HealthMetrics, error) {
	var metrics HealthMetrics
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM repos`).Scan(&metrics.RepoCount); err != nil {
		return metrics, err
	}
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM webhooks WHERE active = 1`).Scan(&metrics.WebhookCount); err != nil {
		return metrics, err
	}
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM lfs_objects WHERE uploaded = 1`).Scan(&metrics.LFSObjectCount); err != nil {
		return metrics, err
	}
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM push_events`).Scan(&metrics.PushEventCount); err != nil {
		return metrics, err
	}
	return metrics, nil
}

// --- Repositories ---

func (s *Store) CreateRepo(ctx context.Context, draft RepoRecord) (RepoRecord, error) {
	if strings.TrimSpace(draft.Owner) == "" || strings.TrimSpace(draft.Name) == "" {
		return RepoRecord{}, fmt.Errorf("%w: owner and name are required", ErrInvalidInput)
	}
	if strings.TrimSpace(draft.DefaultBranch) == "" {
		draft.DefaultBranch = "main"
	}
	stamp := nowUTC()
	execOut, err := s.db.ExecContext(ctx, `
INSERT INTO repos (owner, name, description, default_branch, private, mirror_url, last_sync_at, created_at, updated_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		draft.Owner,
		draft.Name,
		draft.Description,
		draft.DefaultBranch,
		boolToInt(draft.Private),
		strings.TrimSpace(draft.MirrorURL),
		draft.LastSyncAt,
		stamp,
		stamp,
	)
	if err != nil {
		if isUniqueConstraintError(err) {
			return RepoRecord{}, fmt.Errorf("%w: repository %s/%s already exists", ErrAlreadyExists, draft.Owner, draft.Name)
		}
		return RepoRecord{}, fmt.Errorf("insert repository: %w", err)
	}
	insertedID, _ := execOut.LastInsertId()
	draft.ID = insertedID
	draft.MirrorURL = strings.TrimSpace(draft.MirrorURL)
	draft.CreatedAt = stamp
	draft.UpdatedAt = stamp
	return draft, nil
}

func (s *Store) GetRepo(ctx context.Context, owner, name string) (RepoRecord, error) {
	var record RepoRecord
	var privateFlag int
	err := s.db.QueryRowContext(ctx, `
SELECT id, owner, name, description, default_branch, private, mirror_url, last_sync_at, created_at, updated_at
FROM repos
WHERE owner = ? AND name = ?`, owner, name).Scan(
		&record.ID,
		&record.Owner,
		&record.Name,
		&record.Description,
		&record.DefaultBranch,
		&privateFlag,
		&record.MirrorURL,
		&record.LastSyncAt,
		&record.CreatedAt,
		&record.UpdatedAt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return RepoRecord{}, ErrNotFound
	}
	if err != nil {
		return RepoRecord{}, fmt.Errorf("query repository: %w", err)
	}
	record.Private = privateFlag != 0
	return record, nil
}

func (s *Store) ListRepos(ctx context.Context, ownerFilter string) ([]RepoRecord, error) {
	query := `
SELECT id, owner, name, description, default_branch, private, mirror_url, last_sync_at, created_at, updated_at
FROM repos`
	var args []any
	if strings.TrimSpace(ownerFilter) != "" {
		query += ` WHERE owner = ?`
		args = append(args, strings.TrimSpace(ownerFilter))
	}
	query += ` ORDER BY updated_at DESC, owner ASC, name ASC`

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list repositories: %w", err)
	}
	defer rows.Close()

	repos := make([]RepoRecord, 0)
	for rows.Next() {
		var record RepoRecord
		var privateFlag int
		if err := rows.Scan(
			&record.ID,
			&record.Owner,
			&record.Name,
			&record.Description,
			&record.DefaultBranch,
			&privateFlag,
			&record.MirrorURL,
			&record.LastSyncAt,
			&record.CreatedAt,
			&record.UpdatedAt,
		); err != nil {
			return nil, fmt.Errorf("scan repository row: %w", err)
		}
		record.Private = privateFlag != 0
		repos = append(repos, record)
	}
	return repos, rows.Err()
}

func (s *Store) UpdateRepo(ctx context.Context, owner, name string, patch RepoRecord) (RepoRecord, error) {
	existing, err := s.GetRepo(ctx, owner, name)
	if err != nil {
		return RepoRecord{}, err
	}
	existing.Description = patch.Description
	if strings.TrimSpace(patch.DefaultBranch) != "" {
		existing.DefaultBranch = strings.TrimSpace(patch.DefaultBranch)
	}
	existing.Private = patch.Private
	existing.MirrorURL = strings.TrimSpace(patch.MirrorURL)
	existing.UpdatedAt = nowUTC()

	_, err = s.db.ExecContext(ctx, `
UPDATE repos
SET description = ?, default_branch = ?, private = ?, mirror_url = ?, updated_at = ?
WHERE owner = ? AND name = ?`,
		existing.Description,
		existing.DefaultBranch,
		boolToInt(existing.Private),
		existing.MirrorURL,
		existing.UpdatedAt,
		owner,
		name,
	)
	if err != nil {
		return RepoRecord{}, fmt.Errorf("update repository: %w", err)
	}
	return existing, nil
}

func (s *Store) TouchRepoSync(ctx context.Context, owner, name, mirrorURL string) error {
	stamp := nowUTC()
	execOut, err := s.db.ExecContext(ctx, `
UPDATE repos
SET mirror_url = CASE WHEN ? != '' THEN ? ELSE mirror_url END,
    last_sync_at = ?,
    updated_at = ?
WHERE owner = ? AND name = ?`,
		mirrorURL, mirrorURL, stamp, stamp, owner, name,
	)
	if err != nil {
		return fmt.Errorf("update repo sync timestamp: %w", err)
	}
	affected, _ := execOut.RowsAffected()
	if affected == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *Store) DeleteRepo(ctx context.Context, owner, name string) error {
	execOut, err := s.db.ExecContext(ctx, `DELETE FROM repos WHERE owner = ? AND name = ?`, owner, name)
	if err != nil {
		return fmt.Errorf("delete repository: %w", err)
	}
	affected, _ := execOut.RowsAffected()
	if affected == 0 {
		return ErrNotFound
	}
	_, _ = s.db.ExecContext(ctx, `DELETE FROM mirrors WHERE repo_owner = ? AND repo_name = ?`, owner, name)
	return nil
}

// --- Mirrors ---

func (s *Store) UpsertMirror(ctx context.Context, draft MirrorRecord) (MirrorRecord, error) {
	if strings.TrimSpace(draft.RepoOwner) == "" || strings.TrimSpace(draft.RepoName) == "" || strings.TrimSpace(draft.UpstreamURL) == "" {
		return MirrorRecord{}, fmt.Errorf("%w: repoOwner, repoName, and upstreamUrl are required", ErrInvalidInput)
	}
	if strings.TrimSpace(draft.Direction) == "" {
		draft.Direction = "pull"
	}
	if strings.TrimSpace(draft.LastStatus) == "" {
		draft.LastStatus = "idle"
	}
	stamp := nowUTC()
	_, err := s.db.ExecContext(ctx, `
INSERT INTO mirrors (repo_owner, repo_name, upstream_url, direction, interval_sec, last_status, last_error, last_sync_at, created_at, updated_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(repo_owner, repo_name) DO UPDATE SET
	upstream_url = excluded.upstream_url,
	direction = excluded.direction,
	interval_sec = excluded.interval_sec,
	last_status = excluded.last_status,
	last_error = excluded.last_error,
	last_sync_at = CASE WHEN excluded.last_sync_at != '' THEN excluded.last_sync_at ELSE mirrors.last_sync_at END,
	updated_at = excluded.updated_at`,
		draft.RepoOwner,
		draft.RepoName,
		strings.TrimSpace(draft.UpstreamURL),
		draft.Direction,
		draft.IntervalSec,
		draft.LastStatus,
		draft.LastError,
		draft.LastSyncAt,
		stamp,
		stamp,
	)
	if err != nil {
		return MirrorRecord{}, fmt.Errorf("upsert mirror: %w", err)
	}
	return s.GetMirror(ctx, draft.RepoOwner, draft.RepoName)
}

func (s *Store) GetMirror(ctx context.Context, owner, name string) (MirrorRecord, error) {
	var record MirrorRecord
	err := s.db.QueryRowContext(ctx, `
SELECT id, repo_owner, repo_name, upstream_url, direction, interval_sec, last_status, last_error, last_sync_at, created_at, updated_at
FROM mirrors
WHERE repo_owner = ? AND repo_name = ?`, owner, name).Scan(
		&record.ID,
		&record.RepoOwner,
		&record.RepoName,
		&record.UpstreamURL,
		&record.Direction,
		&record.IntervalSec,
		&record.LastStatus,
		&record.LastError,
		&record.LastSyncAt,
		&record.CreatedAt,
		&record.UpdatedAt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return MirrorRecord{}, ErrNotFound
	}
	if err != nil {
		return MirrorRecord{}, fmt.Errorf("query mirror: %w", err)
	}
	return record, nil
}

func (s *Store) ListMirrors(ctx context.Context) ([]MirrorRecord, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT id, repo_owner, repo_name, upstream_url, direction, interval_sec, last_status, last_error, last_sync_at, created_at, updated_at
FROM mirrors
ORDER BY updated_at DESC`)
	if err != nil {
		return nil, fmt.Errorf("list mirrors: %w", err)
	}
	defer rows.Close()

	mirrors := make([]MirrorRecord, 0)
	for rows.Next() {
		var record MirrorRecord
		if err := rows.Scan(
			&record.ID,
			&record.RepoOwner,
			&record.RepoName,
			&record.UpstreamURL,
			&record.Direction,
			&record.IntervalSec,
			&record.LastStatus,
			&record.LastError,
			&record.LastSyncAt,
			&record.CreatedAt,
			&record.UpdatedAt,
		); err != nil {
			return nil, fmt.Errorf("scan mirror row: %w", err)
		}
		mirrors = append(mirrors, record)
	}
	return mirrors, rows.Err()
}

func (s *Store) UpdateMirrorStatus(ctx context.Context, owner, name, upstreamURL, status, lastError string) (MirrorRecord, error) {
	stamp := nowUTC()
	lastSync := ""
	if status == "synced" {
		lastSync = stamp
	}
	return s.UpsertMirror(ctx, MirrorRecord{
		RepoOwner:   owner,
		RepoName:    name,
		UpstreamURL: upstreamURL,
		Direction:   "pull",
		LastStatus:  status,
		LastError:   lastError,
		LastSyncAt:  lastSync,
	})
}

// --- LFS Objects ---

func (s *Store) UpsertLFSObject(ctx context.Context, draft LFSObjectRecord) (LFSObjectRecord, error) {
	if strings.TrimSpace(draft.OID) == "" {
		return LFSObjectRecord{}, fmt.Errorf("%w: oid is required", ErrInvalidInput)
	}
	stamp := nowUTC()
	_, err := s.db.ExecContext(ctx, `
INSERT INTO lfs_objects (oid, repo_owner, repo_name, size, backend, storage_ref, uploaded, created_at, updated_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(oid) DO UPDATE SET
	repo_owner = CASE WHEN excluded.repo_owner != '' THEN excluded.repo_owner ELSE lfs_objects.repo_owner END,
	repo_name = CASE WHEN excluded.repo_name != '' THEN excluded.repo_name ELSE lfs_objects.repo_name END,
	size = CASE WHEN excluded.size > 0 THEN excluded.size ELSE lfs_objects.size END,
	backend = CASE WHEN excluded.backend != '' THEN excluded.backend ELSE lfs_objects.backend END,
	storage_ref = CASE WHEN excluded.storage_ref != '' THEN excluded.storage_ref ELSE lfs_objects.storage_ref END,
	uploaded = CASE WHEN excluded.uploaded = 1 THEN 1 ELSE lfs_objects.uploaded END,
	updated_at = excluded.updated_at`,
		draft.OID,
		draft.RepoOwner,
		draft.RepoName,
		draft.Size,
		draft.Backend,
		draft.StorageRef,
		boolToInt(draft.Uploaded),
		stamp,
		stamp,
	)
	if err != nil {
		return LFSObjectRecord{}, fmt.Errorf("upsert lfs object: %w", err)
	}
	return s.GetLFSObject(ctx, draft.OID)
}

func (s *Store) GetLFSObject(ctx context.Context, oid string) (LFSObjectRecord, error) {
	var record LFSObjectRecord
	var uploadedFlag int
	err := s.db.QueryRowContext(ctx, `
SELECT oid, repo_owner, repo_name, size, backend, storage_ref, uploaded, created_at, updated_at
FROM lfs_objects
WHERE oid = ?`, oid).Scan(
		&record.OID,
		&record.RepoOwner,
		&record.RepoName,
		&record.Size,
		&record.Backend,
		&record.StorageRef,
		&uploadedFlag,
		&record.CreatedAt,
		&record.UpdatedAt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return LFSObjectRecord{}, ErrNotFound
	}
	if err != nil {
		return LFSObjectRecord{}, fmt.Errorf("query lfs object: %w", err)
	}
	record.Uploaded = uploadedFlag != 0
	return record, nil
}

func (s *Store) ListLFSObjects(ctx context.Context, ownerFilter, repoFilter string, limit int) ([]LFSObjectRecord, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	query := `
SELECT oid, repo_owner, repo_name, size, backend, storage_ref, uploaded, created_at, updated_at
FROM lfs_objects WHERE 1=1`
	var args []any
	if strings.TrimSpace(ownerFilter) != "" {
		query += ` AND repo_owner = ?`
		args = append(args, strings.TrimSpace(ownerFilter))
	}
	if strings.TrimSpace(repoFilter) != "" {
		query += ` AND repo_name = ?`
		args = append(args, strings.TrimSpace(repoFilter))
	}
	query += ` ORDER BY updated_at DESC LIMIT ?`
	args = append(args, limit)

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list lfs objects: %w", err)
	}
	defer rows.Close()

	objects := make([]LFSObjectRecord, 0)
	for rows.Next() {
		var record LFSObjectRecord
		var uploadedFlag int
		if err := rows.Scan(
			&record.OID,
			&record.RepoOwner,
			&record.RepoName,
			&record.Size,
			&record.Backend,
			&record.StorageRef,
			&uploadedFlag,
			&record.CreatedAt,
			&record.UpdatedAt,
		); err != nil {
			return nil, fmt.Errorf("scan lfs object row: %w", err)
		}
		record.Uploaded = uploadedFlag != 0
		objects = append(objects, record)
	}
	return objects, rows.Err()
}

// --- Webhooks ---

func (s *Store) CreateWebhook(ctx context.Context, draft WebhookRecord) (WebhookRecord, error) {
	if strings.TrimSpace(draft.TargetURL) == "" {
		return WebhookRecord{}, fmt.Errorf("%w: targetUrl is required", ErrInvalidInput)
	}
	if strings.TrimSpace(draft.RepoOwner) == "" {
		draft.RepoOwner = "*"
	}
	if strings.TrimSpace(draft.RepoName) == "" {
		draft.RepoName = "*"
	}
	if strings.TrimSpace(draft.Events) == "" {
		draft.Events = "push"
	}
	stamp := nowUTC()
	execOut, err := s.db.ExecContext(ctx, `
INSERT INTO webhooks (repo_owner, repo_name, target_url, secret, events, active, created_at, updated_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		strings.TrimSpace(draft.RepoOwner),
		strings.TrimSpace(draft.RepoName),
		strings.TrimSpace(draft.TargetURL),
		draft.Secret,
		strings.TrimSpace(draft.Events),
		boolToInt(draft.Active),
		stamp,
		stamp,
	)
	if err != nil {
		return WebhookRecord{}, fmt.Errorf("insert webhook: %w", err)
	}
	insertedID, _ := execOut.LastInsertId()
	draft.ID = insertedID
	draft.TargetURL = strings.TrimSpace(draft.TargetURL)
	draft.CreatedAt = stamp
	draft.UpdatedAt = stamp
	return draft, nil
}

func (s *Store) GetWebhook(ctx context.Context, webhookID int64) (WebhookRecord, error) {
	var record WebhookRecord
	var activeFlag int
	err := s.db.QueryRowContext(ctx, `
SELECT id, repo_owner, repo_name, target_url, secret, events, active, created_at, updated_at
FROM webhooks
WHERE id = ?`, webhookID).Scan(
		&record.ID,
		&record.RepoOwner,
		&record.RepoName,
		&record.TargetURL,
		&record.Secret,
		&record.Events,
		&activeFlag,
		&record.CreatedAt,
		&record.UpdatedAt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return WebhookRecord{}, ErrNotFound
	}
	if err != nil {
		return WebhookRecord{}, fmt.Errorf("query webhook: %w", err)
	}
	record.Active = activeFlag != 0
	return record, nil
}

func (s *Store) ListWebhooks(ctx context.Context, ownerFilter, repoFilter string) ([]WebhookRecord, error) {
	query := `
SELECT id, repo_owner, repo_name, target_url, secret, events, active, created_at, updated_at
FROM webhooks WHERE 1=1`
	var args []any
	if strings.TrimSpace(ownerFilter) != "" {
		query += ` AND (repo_owner = ? OR repo_owner = '*')`
		args = append(args, strings.TrimSpace(ownerFilter))
	}
	if strings.TrimSpace(repoFilter) != "" {
		query += ` AND (repo_name = ? OR repo_name = '*')`
		args = append(args, strings.TrimSpace(repoFilter))
	}
	query += ` ORDER BY id DESC`

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list webhooks: %w", err)
	}
	defer rows.Close()

	hooks := make([]WebhookRecord, 0)
	for rows.Next() {
		var record WebhookRecord
		var activeFlag int
		if err := rows.Scan(
			&record.ID,
			&record.RepoOwner,
			&record.RepoName,
			&record.TargetURL,
			&record.Secret,
			&record.Events,
			&activeFlag,
			&record.CreatedAt,
			&record.UpdatedAt,
		); err != nil {
			return nil, fmt.Errorf("scan webhook row: %w", err)
		}
		record.Active = activeFlag != 0
		hooks = append(hooks, record)
	}
	return hooks, rows.Err()
}

func (s *Store) MatchWebhooks(ctx context.Context, owner, name, eventName string) ([]WebhookRecord, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT id, repo_owner, repo_name, target_url, secret, events, active, created_at, updated_at
FROM webhooks
WHERE active = 1
  AND (repo_owner = ? OR repo_owner = '*')
  AND (repo_name = ? OR repo_name = '*')
ORDER BY id ASC`, owner, name)
	if err != nil {
		return nil, fmt.Errorf("match webhooks: %w", err)
	}
	defer rows.Close()

	matched := make([]WebhookRecord, 0)
	wantedEvent := strings.ToLower(strings.TrimSpace(eventName))
	for rows.Next() {
		var record WebhookRecord
		var activeFlag int
		if err := rows.Scan(
			&record.ID,
			&record.RepoOwner,
			&record.RepoName,
			&record.TargetURL,
			&record.Secret,
			&record.Events,
			&activeFlag,
			&record.CreatedAt,
			&record.UpdatedAt,
		); err != nil {
			return nil, fmt.Errorf("scan matched webhook: %w", err)
		}
		record.Active = activeFlag != 0
		if eventMatches(record.Events, wantedEvent) {
			matched = append(matched, record)
		}
	}
	return matched, rows.Err()
}

func eventMatches(configuredEvents, targetEvent string) bool {
	if targetEvent == "" {
		return true
	}
	for _, token := range strings.Split(configuredEvents, ",") {
		normalized := strings.ToLower(strings.TrimSpace(token))
		if normalized == "*" || normalized == targetEvent {
			return true
		}
	}
	return false
}

func (s *Store) UpdateWebhook(ctx context.Context, webhookID int64, patch WebhookRecord) (WebhookRecord, error) {
	existing, err := s.GetWebhook(ctx, webhookID)
	if err != nil {
		return WebhookRecord{}, err
	}
	if strings.TrimSpace(patch.RepoOwner) != "" {
		existing.RepoOwner = strings.TrimSpace(patch.RepoOwner)
	}
	if strings.TrimSpace(patch.RepoName) != "" {
		existing.RepoName = strings.TrimSpace(patch.RepoName)
	}
	if strings.TrimSpace(patch.TargetURL) != "" {
		existing.TargetURL = strings.TrimSpace(patch.TargetURL)
	}
	if patch.Secret != "" {
		existing.Secret = patch.Secret
	}
	if strings.TrimSpace(patch.Events) != "" {
		existing.Events = strings.TrimSpace(patch.Events)
	}
	existing.Active = patch.Active
	existing.UpdatedAt = nowUTC()

	_, err = s.db.ExecContext(ctx, `
UPDATE webhooks
SET repo_owner = ?, repo_name = ?, target_url = ?, secret = ?, events = ?, active = ?, updated_at = ?
WHERE id = ?`,
		existing.RepoOwner,
		existing.RepoName,
		existing.TargetURL,
		existing.Secret,
		existing.Events,
		boolToInt(existing.Active),
		existing.UpdatedAt,
		webhookID,
	)
	if err != nil {
		return WebhookRecord{}, fmt.Errorf("update webhook: %w", err)
	}
	return existing, nil
}

func (s *Store) DeleteWebhook(ctx context.Context, webhookID int64) error {
	execOut, err := s.db.ExecContext(ctx, `DELETE FROM webhooks WHERE id = ?`, webhookID)
	if err != nil {
		return fmt.Errorf("delete webhook: %w", err)
	}
	affected, _ := execOut.RowsAffected()
	if affected == 0 {
		return ErrNotFound
	}
	return nil
}

// --- Push Events ---

func (s *Store) RecordPushEvent(ctx context.Context, draft PushEventRecord) (PushEventRecord, error) {
	if strings.TrimSpace(draft.DeliveryStatus) == "" {
		draft.DeliveryStatus = "recorded"
	}
	stamp := nowUTC()
	execOut, err := s.db.ExecContext(ctx, `
INSERT INTO push_events (repo_owner, repo_name, ref_name, before_sha, after_sha, pusher, delivery_status, created_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		draft.RepoOwner,
		draft.RepoName,
		draft.RefName,
		draft.BeforeSHA,
		draft.AfterSHA,
		draft.Pusher,
		draft.DeliveryStatus,
		stamp,
	)
	if err != nil {
		return PushEventRecord{}, fmt.Errorf("insert push event: %w", err)
	}
	insertedID, _ := execOut.LastInsertId()
	draft.ID = insertedID
	draft.CreatedAt = stamp
	return draft, nil
}

func (s *Store) UpdatePushEventStatus(ctx context.Context, eventID int64, deliveryStatus string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE push_events SET delivery_status = ? WHERE id = ?`, deliveryStatus, eventID)
	if err != nil {
		return fmt.Errorf("update push event status: %w", err)
	}
	return nil
}

func (s *Store) ListPushEvents(ctx context.Context, ownerFilter, repoFilter string, limit int) ([]PushEventRecord, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	query := `
SELECT id, repo_owner, repo_name, ref_name, before_sha, after_sha, pusher, delivery_status, created_at
FROM push_events WHERE 1=1`
	var args []any
	if strings.TrimSpace(ownerFilter) != "" {
		query += ` AND repo_owner = ?`
		args = append(args, strings.TrimSpace(ownerFilter))
	}
	if strings.TrimSpace(repoFilter) != "" {
		query += ` AND repo_name = ?`
		args = append(args, strings.TrimSpace(repoFilter))
	}
	query += ` ORDER BY id DESC LIMIT ?`
	args = append(args, limit)

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list push events: %w", err)
	}
	defer rows.Close()

	events := make([]PushEventRecord, 0)
	for rows.Next() {
		var record PushEventRecord
		if err := rows.Scan(
			&record.ID,
			&record.RepoOwner,
			&record.RepoName,
			&record.RefName,
			&record.BeforeSHA,
			&record.AfterSHA,
			&record.Pusher,
			&record.DeliveryStatus,
			&record.CreatedAt,
		); err != nil {
			return nil, fmt.Errorf("scan push event row: %w", err)
		}
		events = append(events, record)
	}
	return events, rows.Err()
}
