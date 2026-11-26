package githttp

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/A-L-L-A-H/mw-lab/internal/store"
)

const (
	ZeroSHA          = "0000000000000000000000000000000000000000"
	defaultBlobLimit = 2 * 1024 * 1024
)

var (
	slugPattern = regexp.MustCompile(`^[a-zA-Z0-9._-]+$`)
	refBadChars = regexp.MustCompile(`[\x00-\x1f\x7f ~^:?*\[\\]`)

	ErrInvalidSlug     = errors.New("invalid repository owner or name slug")
	ErrInvalidRef      = errors.New("invalid git reference")
	ErrInvalidPath     = errors.New("invalid repository path")
	ErrInvalidUpstream = errors.New("invalid mirror upstream url")
	ErrRepoNotFound    = errors.New("bare repository not found")
	ErrPathEscape      = errors.New("path escapes repository root")
	ErrBlobTooLarge    = errors.New("blob exceeds maximum readable size")
)

type BranchInfo struct {
	Name        string `json:"name"`
	CommitSHA   string `json:"commitSha"`
	CommittedAt string `json:"committedAt"`
	Subject     string `json:"subject"`
	IsDefault   bool   `json:"isDefault"`
}

type CommitInfo struct {
	SHA         string   `json:"sha"`
	ShortSHA    string   `json:"shortSha"`
	AuthorName  string   `json:"authorName"`
	AuthorEmail string   `json:"authorEmail"`
	AuthoredAt  string   `json:"authoredAt"`
	Parents     []string `json:"parents"`
	Subject     string   `json:"subject"`
}

type TreeEntry struct {
	Mode string `json:"mode"`
	Type string `json:"type"`
	SHA  string `json:"sha"`
	Size int64  `json:"size"`
	Name string `json:"name"`
	Path string `json:"path"`
}

type BlobContent struct {
	Path     string `json:"path"`
	Revision string `json:"revision"`
	Size     int64  `json:"size"`
	Binary   bool   `json:"binary"`
	Content  string `json:"content,omitempty"`
}

type MirrorSyncResult struct {
	Owner       string   `json:"owner"`
	Name        string   `json:"name"`
	UpstreamURL string   `json:"upstreamUrl"`
	BranchCount int      `json:"branchCount"`
	Branches    []string `json:"branches"`
	Status      string   `json:"status"`
}

type Manager struct {
	reposRoot         string
	dbStore           *store.Store
	allowLocalMirrors bool
}

func NewManager(reposRoot string, dbStore *store.Store, allowLocalMirrors bool) (*Manager, error) {
	trimmedRoot := strings.TrimSpace(reposRoot)
	if trimmedRoot == "" {
		return nil, fmt.Errorf("reposRoot is required")
	}
	absRoot, err := filepath.Abs(trimmedRoot)
	if err != nil {
		return nil, fmt.Errorf("resolve repos root: %w", err)
	}
	if err := os.MkdirAll(absRoot, 0o755); err != nil {
		return nil, fmt.Errorf("create repos root: %w", err)
	}
	return &Manager{
		reposRoot:         absRoot,
		dbStore:           dbStore,
		allowLocalMirrors: allowLocalMirrors,
	}, nil
}

func (m *Manager) ReposRoot() string {
	return m.reposRoot
}

func ValidateSlug(slug string) error {
	if slug == "" || len(slug) > 128 {
		return fmt.Errorf("%w: must be 1..128 characters", ErrInvalidSlug)
	}
	if !slugPattern.MatchString(slug) {
		return fmt.Errorf("%w: %q does not match ^[a-zA-Z0-9._-]+$", ErrInvalidSlug, slug)
	}
	if slug == "." || slug == ".." || strings.Contains(slug, "..") {
		return fmt.Errorf("%w: dot traversal forbidden", ErrInvalidSlug)
	}
	if strings.HasPrefix(slug, "-") {
		return fmt.Errorf("%w: leading hyphen forbidden", ErrInvalidSlug)
	}
	return nil
}

func NormalizeRepoName(rawRepoName string) (string, error) {
	normalized := strings.TrimSuffix(rawRepoName, ".git")
	if err := ValidateSlug(normalized); err != nil {
		return "", err
	}
	return normalized, nil
}

func ValidateRefName(refName string) error {
	trimmed := strings.TrimSpace(refName)
	if trimmed == "" {
		return nil
	}
	if len(trimmed) > 256 || strings.HasPrefix(trimmed, "-") || strings.Contains(trimmed, "..") || strings.Contains(trimmed, "@{") {
		return fmt.Errorf("%w: %q", ErrInvalidRef, refName)
	}
	if refBadChars.MatchString(trimmed) {
		return fmt.Errorf("%w: %q", ErrInvalidRef, refName)
	}
	return nil
}

func ValidateRelativePath(rawPath string) (string, error) {
	trimmed := strings.TrimSpace(rawPath)
	if trimmed == "" || trimmed == "." || trimmed == "/" {
		return "", nil
	}
	if strings.ContainsRune(trimmed, '\x00') || strings.Contains(trimmed, "\\") || strings.HasPrefix(trimmed, "/") {
		return "", fmt.Errorf("%w: %q", ErrInvalidPath, rawPath)
	}
	segments := strings.Split(trimmed, "/")
	cleaned := make([]string, 0, len(segments))
	for _, segment := range segments {
		if segment == "" || segment == "." {
			continue
		}
		if segment == ".." {
			return "", fmt.Errorf("%w: traversal segment rejected", ErrInvalidPath)
		}
		cleaned = append(cleaned, segment)
	}
	return strings.Join(cleaned, "/"), nil
}

func (m *Manager) ValidateUpstreamURL(upstreamURL string) error {
	trimmed := strings.TrimSpace(upstreamURL)
	if trimmed == "" || strings.HasPrefix(trimmed, "-") || strings.ContainsAny(trimmed, "\r\n\x00") {
		return fmt.Errorf("%w: malformed upstream url", ErrInvalidUpstream)
	}
	parsed, err := url.Parse(trimmed)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidUpstream, err)
	}
	switch strings.ToLower(parsed.Scheme) {
	case "https", "http", "ssh", "git":
		if parsed.Host == "" {
			return fmt.Errorf("%w: missing host", ErrInvalidUpstream)
		}
		return nil
	case "file":
		if !m.allowLocalMirrors {
			return fmt.Errorf("%w: file:// mirrors disabled", ErrInvalidUpstream)
		}
		return nil
	default:
		if m.allowLocalMirrors && filepath.IsAbs(trimmed) {
			return nil
		}
		return fmt.Errorf("%w: unsupported scheme %q", ErrInvalidUpstream, parsed.Scheme)
	}
}

func (m *Manager) RepoPath(owner, repoName string) (string, error) {
	if err := ValidateSlug(owner); err != nil {
		return "", err
	}
	cleanName, err := NormalizeRepoName(repoName)
	if err != nil {
		return "", err
	}
	candidate := filepath.Join(m.reposRoot, owner, cleanName+".git")
	absCandidate, err := filepath.Abs(candidate)
	if err != nil {
		return "", fmt.Errorf("resolve repo path: %w", err)
	}
	relPath, err := filepath.Rel(m.reposRoot, absCandidate)
	if err != nil || relPath == ".." || strings.HasPrefix(relPath, ".."+string(filepath.Separator)) {
		return "", ErrPathEscape
	}
	return absCandidate, nil
}

func (m *Manager) RepoExists(owner, repoName string) (bool, error) {
	repoPath, err := m.RepoPath(owner, repoName)
	if err != nil {
		return false, err
	}
	info, statErr := os.Stat(filepath.Join(repoPath, "HEAD"))
	if statErr != nil {
		if os.IsNotExist(statErr) {
			return false, nil
		}
		return false, statErr
	}
	return !info.IsDir(), nil
}

func (m *Manager) InitBareRepo(ctx context.Context, owner, repoName, defaultBranch string) (string, error) {
	repoPath, err := m.RepoPath(owner, repoName)
	if err != nil {
		return "", err
	}
	branchName := strings.TrimSpace(defaultBranch)
	if branchName == "" {
		branchName = "main"
	}
	if err := ValidateRefName(branchName); err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(repoPath), 0o755); err != nil {
		return "", fmt.Errorf("create owner repo directory: %w", err)
	}
	if _, err := os.Stat(filepath.Join(repoPath, "HEAD")); err == nil {
		return repoPath, nil
	}

	if _, err := runGit(ctx, "", "init", "--bare", "--initial-branch="+branchName, repoPath); err != nil {
		if _, fallbackErr := runGit(ctx, "", "init", "--bare", repoPath); fallbackErr != nil {
			return "", fmt.Errorf("git init --bare failed: %w", err)
		}
		_, _ = runGit(ctx, repoPath, "symbolic-ref", "HEAD", "refs/heads/"+branchName)
	}
	if _, err := runGit(ctx, repoPath, "config", "http.receivepack", "true"); err != nil {
		return "", fmt.Errorf("enable http.receivepack: %w", err)
	}
	if _, err := runGit(ctx, repoPath, "config", "http.uploadpack", "true"); err != nil {
		return "", fmt.Errorf("enable http.uploadpack: %w", err)
	}
	return repoPath, nil
}

func (m *Manager) DeleteBareRepo(owner, repoName string) error {
	repoPath, err := m.RepoPath(owner, repoName)
	if err != nil {
		return err
	}
	if err := os.RemoveAll(repoPath); err != nil {
		return fmt.Errorf("remove bare repository: %w", err)
	}
	return nil
}

func (m *Manager) ListBranches(ctx context.Context, owner, repoName string) ([]BranchInfo, error) {
	repoPath, err := m.requireRepoPath(owner, repoName)
	if err != nil {
		return nil, err
	}
	defaultBranch := m.detectHeadBranch(ctx, repoPath)
	rawOut, err := runGit(ctx, repoPath,
		"for-each-ref",
		"--sort=-committerdate",
		"--format=%(refname:short)%1f%(objectname)%1f%(committerdate:iso-strict)%1f%(subject)",
		"refs/heads",
	)
	if err != nil {
		return nil, fmt.Errorf("list branches: %w", err)
	}
	branches := make([]BranchInfo, 0)
	for _, rawLine := range strings.Split(strings.TrimRight(string(rawOut), "\r\n"), "\n") {
		line := strings.TrimRight(rawLine, "\r")
		if line == "" {
			continue
		}
		fields := strings.SplitN(line, "\x1f", 4)
		if len(fields) < 4 {
			continue
		}
		branches = append(branches, BranchInfo{
			Name:        fields[0],
			CommitSHA:   fields[1],
			CommittedAt: fields[2],
			Subject:     fields[3],
			IsDefault:   fields[0] == defaultBranch,
		})
	}
	return branches, nil
}

func (m *Manager) ListCommits(ctx context.Context, owner, repoName, revision string, limit int) ([]CommitInfo, error) {
	repoPath, err := m.requireRepoPath(owner, repoName)
	if err != nil {
		return nil, err
	}
	if limit <= 0 || limit > 200 {
		limit = 30
	}
	targetRev, err := m.resolveRevision(ctx, repoPath, revision)
	if err != nil {
		return nil, err
	}
	if targetRev == "" {
		return []CommitInfo{}, nil
	}

	rawOut, err := runGit(ctx, repoPath,
		"log",
		fmt.Sprintf("-n%d", limit),
		"--format=%H%x1f%h%x1f%an%x1f%ae%x1f%aI%x1f%P%x1f%s%x1e",
		targetRev,
		"--",
	)
	if err != nil {
		return nil, fmt.Errorf("git log failed: %w", err)
	}

	commits := make([]CommitInfo, 0)
	for _, rawRecord := range strings.Split(string(rawOut), "\x1e") {
		record := strings.TrimSpace(rawRecord)
		if record == "" {
			continue
		}
		fields := strings.SplitN(record, "\x1f", 7)
		if len(fields) < 7 {
			continue
		}
		parentList := []string{}
		if strings.TrimSpace(fields[5]) != "" {
			parentList = strings.Fields(fields[5])
		}
		commits = append(commits, CommitInfo{
			SHA:         fields[0],
			ShortSHA:    fields[1],
			AuthorName:  fields[2],
			AuthorEmail: fields[3],
			AuthoredAt:  fields[4],
			Parents:     parentList,
			Subject:     fields[6],
		})
	}
	return commits, nil
}

func (m *Manager) ListTree(ctx context.Context, owner, repoName, revision, subPath string) ([]TreeEntry, error) {
	repoPath, err := m.requireRepoPath(owner, repoName)
	if err != nil {
		return nil, err
	}
	cleanSubPath, err := ValidateRelativePath(subPath)
	if err != nil {
		return nil, err
	}
	targetRev, err := m.resolveRevision(ctx, repoPath, revision)
	if err != nil {
		return nil, err
	}
	if targetRev == "" {
		return []TreeEntry{}, nil
	}

	treeSpec := targetRev
	if cleanSubPath != "" {
		treeSpec = targetRev + ":" + cleanSubPath
		objTypeBytes, typeErr := runGit(ctx, repoPath, "cat-file", "-t", treeSpec)
		if typeErr != nil {
			return nil, fmt.Errorf("%w: path %q not found", ErrInvalidPath, cleanSubPath)
		}
		if strings.TrimSpace(string(objTypeBytes)) != "tree" {
			return nil, fmt.Errorf("%w: %q is not a directory", ErrInvalidPath, cleanSubPath)
		}
	}

	rawOut, err := runGit(ctx, repoPath, "ls-tree", "-l", treeSpec)
	if err != nil {
		return nil, fmt.Errorf("git ls-tree failed: %w", err)
	}

	entries := make([]TreeEntry, 0)
	for _, rawLine := range strings.Split(strings.TrimRight(string(rawOut), "\r\n"), "\n") {
		line := strings.TrimRight(rawLine, "\r")
		if line == "" {
			continue
		}
		tabIdx := strings.IndexByte(line, '\t')
		if tabIdx <= 0 {
			continue
		}
		metaFields := strings.Fields(line[:tabIdx])
		if len(metaFields) < 4 {
			continue
		}
		entryName := line[tabIdx+1:]
		var entrySize int64
		if metaFields[3] != "-" {
			parsedSize, parseErr := strconv.ParseInt(metaFields[3], 10, 64)
			if parseErr == nil {
				entrySize = parsedSize
			}
		}
		fullPath := entryName
		if cleanSubPath != "" {
			fullPath = cleanSubPath + "/" + entryName
		}
		entries = append(entries, TreeEntry{
			Mode: metaFields[0],
			Type: metaFields[1],
			SHA:  metaFields[2],
			Size: entrySize,
			Name: entryName,
			Path: fullPath,
		})
	}
	sort.Slice(entries, func(leftIdx, rightIdx int) bool {
		if entries[leftIdx].Type != entries[rightIdx].Type {
			return entries[leftIdx].Type == "tree"
		}
		return entries[leftIdx].Name < entries[rightIdx].Name
	})
	return entries, nil
}

func (m *Manager) ReadBlob(ctx context.Context, owner, repoName, revision, filePath string, maxBytes int64) (*BlobContent, error) {
	repoPath, err := m.requireRepoPath(owner, repoName)
	if err != nil {
		return nil, err
	}
	cleanFilePath, err := ValidateRelativePath(filePath)
	if err != nil || cleanFilePath == "" {
		return nil, fmt.Errorf("%w: file path is required", ErrInvalidPath)
	}
	targetRev, err := m.resolveRevision(ctx, repoPath, revision)
	if err != nil {
		return nil, err
	}
	if targetRev == "" {
		return nil, fmt.Errorf("%w: repository has no commits", ErrInvalidPath)
	}
	if maxBytes <= 0 {
		maxBytes = defaultBlobLimit
	}

	blobSpec := targetRev + ":" + cleanFilePath
	objTypeBytes, err := runGit(ctx, repoPath, "cat-file", "-t", blobSpec)
	if err != nil {
		return nil, fmt.Errorf("%w: blob %q not found", ErrInvalidPath, cleanFilePath)
	}
	if strings.TrimSpace(string(objTypeBytes)) != "blob" {
		return nil, fmt.Errorf("%w: %q is not a file", ErrInvalidPath, cleanFilePath)
	}

	sizeBytes, err := runGit(ctx, repoPath, "cat-file", "-s", blobSpec)
	if err != nil {
		return nil, fmt.Errorf("read blob size: %w", err)
	}
	blobSize, err := strconv.ParseInt(strings.TrimSpace(string(sizeBytes)), 10, 64)
	if err != nil {
		return nil, fmt.Errorf("parse blob size: %w", err)
	}
	if blobSize > maxBytes {
		return nil, fmt.Errorf("%w: %d > %d bytes", ErrBlobTooLarge, blobSize, maxBytes)
	}

	rawBlob, err := runGit(ctx, repoPath, "cat-file", "-p", blobSpec)
	if err != nil {
		return nil, fmt.Errorf("read blob content: %w", err)
	}

	isBinary := bytes.IndexByte(rawBlob, 0) >= 0 || !utf8.Valid(rawBlob)
	blobResp := &BlobContent{
		Path:     cleanFilePath,
		Revision: targetRev,
		Size:     blobSize,
		Binary:   isBinary,
	}
	if !isBinary {
		blobResp.Content = string(rawBlob)
	}
	return blobResp, nil
}

func (m *Manager) SyncMirror(ctx context.Context, owner, repoName, upstreamURL string) (*MirrorSyncResult, error) {
	if err := ValidateSlug(owner); err != nil {
		return nil, err
	}
	cleanName, err := NormalizeRepoName(repoName)
	if err != nil {
		return nil, err
	}
	if err := m.ValidateUpstreamURL(upstreamURL); err != nil {
		return nil, err
	}
	repoPath, err := m.InitBareRepo(ctx, owner, cleanName, "main")
	if err != nil {
		return nil, err
	}

	_, fetchErr := runGit(ctx, repoPath,
		"fetch",
		"--prune",
		upstreamURL,
		"+refs/heads/*:refs/heads/*",
		"+refs/tags/*:refs/tags/*",
	)
	if fetchErr != nil {
		if m.dbStore != nil {
			_, _ = m.dbStore.UpdateMirrorStatus(ctx, owner, cleanName, upstreamURL, "failed", fetchErr.Error())
		}
		return nil, fmt.Errorf("mirror fetch failed: %w", fetchErr)
	}

	branches, err := m.ListBranches(ctx, owner, cleanName)
	if err != nil {
		return nil, err
	}
	m.alignHeadToExistingBranch(ctx, repoPath, branches)

	branchNames := make([]string, 0, len(branches))
	for _, branch := range branches {
		branchNames = append(branchNames, branch.Name)
	}

	if m.dbStore != nil {
		_, _ = m.dbStore.UpdateMirrorStatus(ctx, owner, cleanName, upstreamURL, "synced", "")
		_ = m.dbStore.TouchRepoSync(ctx, owner, cleanName, upstreamURL)
	}
	return &MirrorSyncResult{
		Owner:       owner,
		Name:        cleanName,
		UpstreamURL: upstreamURL,
		BranchCount: len(branchNames),
		Branches:    branchNames,
		Status:      "synced",
	}, nil
}

func (m *Manager) SnapshotRefs(ctx context.Context, repoPath string) (map[string]string, error) {
	rawOut, err := runGit(ctx, repoPath, "for-each-ref", "--format=%(refname)%1f%(objectname)", "refs/heads", "refs/tags")
	if err != nil {
		return nil, fmt.Errorf("snapshot git refs: %w", err)
	}
	snapshot := make(map[string]string)
	for _, rawLine := range strings.Split(strings.TrimRight(string(rawOut), "\r\n"), "\n") {
		line := strings.TrimRight(rawLine, "\r")
		if line == "" {
			continue
		}
		parts := strings.SplitN(line, "\x1f", 2)
		if len(parts) == 2 {
			snapshot[parts[0]] = parts[1]
		}
	}
	return snapshot, nil
}

func (m *Manager) requireRepoPath(owner, repoName string) (string, error) {
	repoPath, err := m.RepoPath(owner, repoName)
	if err != nil {
		return "", err
	}
	if _, statErr := os.Stat(filepath.Join(repoPath, "HEAD")); statErr != nil {
		if os.IsNotExist(statErr) {
			return "", fmt.Errorf("%w: %s/%s", ErrRepoNotFound, owner, repoName)
		}
		return "", statErr
	}
	return repoPath, nil
}

func (m *Manager) detectHeadBranch(ctx context.Context, repoPath string) string {
	rawOut, err := runGit(ctx, repoPath, "symbolic-ref", "--short", "HEAD")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(rawOut))
}

func (m *Manager) alignHeadToExistingBranch(ctx context.Context, repoPath string, branches []BranchInfo) {
	if len(branches) == 0 {
		return
	}
	currentHead := m.detectHeadBranch(ctx, repoPath)
	for _, branch := range branches {
		if branch.Name == currentHead {
			return
		}
	}
	preferred := branches[0].Name
	for _, candidate := range []string{"main", "master"} {
		for _, branch := range branches {
			if branch.Name == candidate {
				preferred = candidate
				break
			}
		}
	}
	_, _ = runGit(ctx, repoPath, "symbolic-ref", "HEAD", "refs/heads/"+preferred)
}

func (m *Manager) resolveRevision(ctx context.Context, repoPath, revision string) (string, error) {
	trimmed := strings.TrimSpace(revision)
	if err := ValidateRefName(trimmed); err != nil {
		return "", err
	}
	if trimmed != "" {
		resolved, err := runGit(ctx, repoPath, "rev-parse", "--verify", trimmed+"^{commit}")
		if err != nil {
			return "", fmt.Errorf("%w: %s", ErrInvalidRef, trimmed)
		}
		return strings.TrimSpace(string(resolved)), nil
	}
	if resolved, err := runGit(ctx, repoPath, "rev-parse", "--verify", "HEAD^{commit}"); err == nil {
		return strings.TrimSpace(string(resolved)), nil
	}
	rawRefs, err := runGit(ctx, repoPath, "for-each-ref", "--count=1", "--format=%(objectname)", "refs/heads")
	if err != nil {
		return "", nil
	}
	return strings.TrimSpace(string(rawRefs)), nil
}

func runGit(ctx context.Context, gitDir string, args ...string) ([]byte, error) {
	cmdArgs := make([]string, 0, len(args)+2)
	if gitDir != "" {
		cmdArgs = append(cmdArgs, "--git-dir="+gitDir)
	}
	cmdArgs = append(cmdArgs, args...)
	cmd := exec.CommandContext(ctx, "git", cmdArgs...)
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "LC_ALL=C")
	var stdoutBuf bytes.Buffer
	var stderrBuf bytes.Buffer
	cmd.Stdout = &stdoutBuf
	cmd.Stderr = &stderrBuf
	if err := cmd.Run(); err != nil {
		errDetail := strings.TrimSpace(stderrBuf.String())
		if errDetail != "" {
			return nil, fmt.Errorf("%w: %s", err, errDetail)
		}
		return nil, err
	}
	return stdoutBuf.Bytes(), nil
}
