package lfs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

var (
	ErrObjectNotFound = errors.New("lfs object not found")
	ErrBackendFailure = errors.New("lfs storage backend error")
)

type BlobBackend interface {
	Name() string
	Put(ctx context.Context, oid string, content io.Reader, size int64) (string, error)
	Open(ctx context.Context, oid string, storageRef string) (io.ReadCloser, int64, error)
	Exists(ctx context.Context, oid string, storageRef string) (bool, error)
	Delete(ctx context.Context, oid string, storageRef string) error
}

// LocalDiskBackend stores LFS blobs content-addressed on local disk.
type LocalDiskBackend struct {
	rootDir string
}

func NewLocalDiskBackend(rootDir string) (*LocalDiskBackend, error) {
	trimmed := strings.TrimSpace(rootDir)
	if trimmed == "" {
		return nil, fmt.Errorf("local lfs root directory is required")
	}
	absDir, err := filepath.Abs(trimmed)
	if err != nil {
		return nil, fmt.Errorf("resolve lfs root directory: %w", err)
	}
	if err := os.MkdirAll(absDir, 0o755); err != nil {
		return nil, fmt.Errorf("create lfs root directory: %w", err)
	}
	return &LocalDiskBackend{rootDir: absDir}, nil
}

func (b *LocalDiskBackend) Name() string {
	return "local"
}

func (b *LocalDiskBackend) objectRelPath(oid string) string {
	return filepath.Join(oid[0:2], oid[2:4], oid)
}

func (b *LocalDiskBackend) resolvePath(oid, storageRef string) (string, error) {
	rel := storageRef
	if rel == "" {
		rel = b.objectRelPath(oid)
	}
	fullPath := filepath.Join(b.rootDir, rel)
	absFull, err := filepath.Abs(fullPath)
	if err != nil {
		return "", err
	}
	checkRel, err := filepath.Rel(b.rootDir, absFull)
	if err != nil || checkRel == ".." || strings.HasPrefix(checkRel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("invalid lfs storage path")
	}
	return absFull, nil
}

func (b *LocalDiskBackend) Put(_ context.Context, oid string, content io.Reader, _ int64) (string, error) {
	relPath := b.objectRelPath(oid)
	targetPath, err := b.resolvePath(oid, relPath)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(targetPath), 0o755); err != nil {
		return "", fmt.Errorf("create lfs shard dir: %w", err)
	}

	tempFile, err := os.CreateTemp(filepath.Dir(targetPath), ".lfs-upload-*")
	if err != nil {
		return "", fmt.Errorf("create temp lfs file: %w", err)
	}
	tempPath := tempFile.Name()
	cleanupTemp := true
	defer func() {
		if cleanupTemp {
			_ = tempFile.Close()
			_ = os.Remove(tempPath)
		}
	}()

	if _, err := io.Copy(tempFile, content); err != nil {
		return "", fmt.Errorf("write lfs blob: %w", err)
	}
	if err := tempFile.Close(); err != nil {
		return "", fmt.Errorf("close temp lfs blob: %w", err)
	}
	if err := os.Rename(tempPath, targetPath); err != nil {
		return "", fmt.Errorf("commit lfs blob: %w", err)
	}
	cleanupTemp = false
	return relPath, nil
}

func (b *LocalDiskBackend) Open(_ context.Context, oid string, storageRef string) (io.ReadCloser, int64, error) {
	targetPath, err := b.resolvePath(oid, storageRef)
	if err != nil {
		return nil, 0, err
	}
	fileHandle, err := os.Open(targetPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, 0, ErrObjectNotFound
		}
		return nil, 0, err
	}
	statInfo, err := fileHandle.Stat()
	if err != nil {
		_ = fileHandle.Close()
		return nil, 0, err
	}
	return fileHandle, statInfo.Size(), nil
}

func (b *LocalDiskBackend) Exists(_ context.Context, oid string, storageRef string) (bool, error) {
	targetPath, err := b.resolvePath(oid, storageRef)
	if err != nil {
		return false, err
	}
	statInfo, err := os.Stat(targetPath)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, err
	}
	return !statInfo.IsDir(), nil
}

func (b *LocalDiskBackend) Delete(_ context.Context, oid string, storageRef string) error {
	targetPath, err := b.resolvePath(oid, storageRef)
	if err != nil {
		return err
	}
	err = os.Remove(targetPath)
	if os.IsNotExist(err) {
		return nil
	}
	return err
}

// Echo9DriveBackend proxies LFS blobs to mw-echo 9Drive (/api/storage/upload + /api/storage/:id/download).
type Echo9DriveBackend struct {
	baseURL     string
	bearerToken string
	project     string
	httpClient  *http.Client
}

func NewEcho9DriveBackend(baseURL, bearerToken, project string, httpClient *http.Client) (*Echo9DriveBackend, error) {
	trimmedURL := strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if trimmedURL == "" {
		return nil, fmt.Errorf("MW_ECHO_URL is required for mw-echo 9Drive backend")
	}
	parsed, err := url.Parse(trimmedURL)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
		return nil, fmt.Errorf("invalid MW_ECHO_URL %q", baseURL)
	}
	client := httpClient
	if client == nil {
		client = &http.Client{Timeout: 5 * time.Minute}
	}
	projectSlug := strings.TrimSpace(project)
	if projectSlug == "" {
		projectSlug = "mw-lab"
	}
	return &Echo9DriveBackend{
		baseURL:     trimmedURL,
		bearerToken: strings.TrimSpace(bearerToken),
		project:     projectSlug,
		httpClient:  client,
	}, nil
}

func (b *Echo9DriveBackend) Name() string {
	return "mw-echo-9drive"
}

func (b *Echo9DriveBackend) BaseURL() string {
	return b.baseURL
}

func (b *Echo9DriveBackend) Put(ctx context.Context, oid string, content io.Reader, _ int64) (string, error) {
	pipeReader, pipeWriter := io.Pipe()
	formWriter := multipart.NewWriter(pipeWriter)

	go func() {
		defer pipeWriter.Close()
		_ = formWriter.WriteField("project", b.project)
		_ = formWriter.WriteField("logicalPath", fmt.Sprintf("lfs/%s/%s/%s", oid[0:2], oid[2:4], oid))
		filePart, partErr := formWriter.CreateFormFile("file", oid+".lfs")
		if partErr != nil {
			_ = pipeWriter.CloseWithError(partErr)
			return
		}
		if _, copyErr := io.Copy(filePart, content); copyErr != nil {
			_ = pipeWriter.CloseWithError(copyErr)
			return
		}
		if closeErr := formWriter.Close(); closeErr != nil {
			_ = pipeWriter.CloseWithError(closeErr)
		}
	}()

	uploadEndpoint := b.baseURL + "/api/storage/upload"
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, uploadEndpoint, pipeReader)
	if err != nil {
		_ = pipeReader.Close()
		return "", fmt.Errorf("%w: build upload request: %v", ErrBackendFailure, err)
	}
	httpReq.Header.Set("Content-Type", formWriter.FormDataContentType())
	if b.bearerToken != "" {
		httpReq.Header.Set("Authorization", "Bearer "+b.bearerToken)
	}

	httpResp, err := b.httpClient.Do(httpReq)
	if err != nil {
		return "", fmt.Errorf("%w: upload to mw-echo: %v", ErrBackendFailure, err)
	}
	defer httpResp.Body.Close()

	respBody, err := io.ReadAll(io.LimitReader(httpResp.Body, 1<<20))
	if err != nil {
		return "", fmt.Errorf("%w: read mw-echo upload reply: %v", ErrBackendFailure, err)
	}
	if httpResp.StatusCode < 200 || httpResp.StatusCode >= 300 {
		return "", fmt.Errorf("%w: mw-echo upload returned HTTP %d: %s", ErrBackendFailure, httpResp.StatusCode, strings.TrimSpace(string(respBody)))
	}

	var parsedReply struct {
		ID     string `json:"id"`
		Object *struct {
			ID string `json:"id"`
		} `json:"object"`
	}
	if err := json.Unmarshal(respBody, &parsedReply); err != nil {
		return "", fmt.Errorf("%w: parse mw-echo upload reply: %v", ErrBackendFailure, err)
	}
	remoteID := strings.TrimSpace(parsedReply.ID)
	if remoteID == "" && parsedReply.Object != nil {
		remoteID = strings.TrimSpace(parsedReply.Object.ID)
	}
	if remoteID == "" {
		return "", fmt.Errorf("%w: mw-echo upload response missing storage object id", ErrBackendFailure)
	}
	return remoteID, nil
}

func (b *Echo9DriveBackend) Open(ctx context.Context, _ string, storageRef string) (io.ReadCloser, int64, error) {
	trimmedRef := strings.TrimSpace(storageRef)
	if trimmedRef == "" {
		return nil, 0, ErrObjectNotFound
	}
	downloadEndpoint := fmt.Sprintf("%s/api/storage/%s/download", b.baseURL, url.PathEscape(trimmedRef))
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, downloadEndpoint, nil)
	if err != nil {
		return nil, 0, fmt.Errorf("%w: build download request: %v", ErrBackendFailure, err)
	}
	if b.bearerToken != "" {
		httpReq.Header.Set("Authorization", "Bearer "+b.bearerToken)
	}

	httpResp, err := b.httpClient.Do(httpReq)
	if err != nil {
		return nil, 0, fmt.Errorf("%w: download from mw-echo: %v", ErrBackendFailure, err)
	}
	if httpResp.StatusCode == http.StatusNotFound {
		_ = httpResp.Body.Close()
		return nil, 0, ErrObjectNotFound
	}
	if httpResp.StatusCode < 200 || httpResp.StatusCode >= 300 {
		errSnippet, _ := io.ReadAll(io.LimitReader(httpResp.Body, 4096))
		_ = httpResp.Body.Close()
		return nil, 0, fmt.Errorf("%w: mw-echo download returned HTTP %d: %s", ErrBackendFailure, httpResp.StatusCode, strings.TrimSpace(string(errSnippet)))
	}
	return httpResp.Body, httpResp.ContentLength, nil
}

func (b *Echo9DriveBackend) Exists(_ context.Context, _ string, storageRef string) (bool, error) {
	return strings.TrimSpace(storageRef) != "", nil
}

func (b *Echo9DriveBackend) Delete(ctx context.Context, _ string, storageRef string) error {
	trimmedRef := strings.TrimSpace(storageRef)
	if trimmedRef == "" {
		return nil
	}
	deleteEndpoint := fmt.Sprintf("%s/api/storage/%s", b.baseURL, url.PathEscape(trimmedRef))
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodDelete, deleteEndpoint, nil)
	if err != nil {
		return err
	}
	if b.bearerToken != "" {
		httpReq.Header.Set("Authorization", "Bearer "+b.bearerToken)
	}
	httpResp, err := b.httpClient.Do(httpReq)
	if err != nil {
		return err
	}
	defer httpResp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(httpResp.Body, 4096))
	if httpResp.StatusCode == http.StatusNotFound {
		return nil
	}
	if httpResp.StatusCode < 200 || httpResp.StatusCode >= 300 {
		return fmt.Errorf("%w: mw-echo delete returned HTTP %d", ErrBackendFailure, httpResp.StatusCode)
	}
	return nil
}
