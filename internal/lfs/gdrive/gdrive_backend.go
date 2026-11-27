// Package gdrive provides a Google Drive LFS backend for mw-lab.
// It uses the shared github.com/A-L-L-A-H/mw-gdrive module so Google Drive
// credentials and OAuth tokens are shared across mw-echo, mw-lab, etc.
package gdrive

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/A-L-L-A-H/mw-gdrive"
	"github.com/A-L-L-A-H/mw-lab/internal/lfs"
)

var (
	ErrObjectNotFound = errors.New("lfs object not found")
	ErrBackendFailure = errors.New("lfs storage backend error")
)

type GDriveBackend struct {
	cfg        *gdrive.GDriveConfig
	client     *gdrive.GDriveClient
	httpClient *http.Client
	rootFolder string
}

func NewGDriveBackend(cfg *gdrive.GDriveConfig, rootFolder string, httpClient *http.Client) (*GDriveBackend, error) {
	if cfg == nil || strings.TrimSpace(cfg.ClientID) == "" || strings.TrimSpace(cfg.ClientSecret) == "" || strings.TrimSpace(cfg.RefreshToken) == "" {
		return nil, errors.New("GDriveBackend requires clientId, clientSecret, and refreshToken")
	}

	client := gdrive.NewGDriveClient(*cfg)

	if strings.TrimSpace(rootFolder) == "" {
		rootFolder = "mw-lab"
	}

	return &GDriveBackend{
		cfg:        cfg,
		client:     client,
		httpClient: httpClient,
		rootFolder: rootFolder,
	}, nil
}

func (b *GDriveBackend) Name() string {
	return "google-drive"
}

func (b *GDriveBackend) Put(ctx context.Context, oid string, content io.Reader, _ int64) (string, error) {
	payload, err := io.ReadAll(io.LimitReader(content, 256*1024*1024)) // 256 MiB max
	if err != nil {
		return "", fmt.Errorf("%w: read content: %v", lfs.ErrBackendFailure, err)
	}

	fileID, err := b.client.Upload(ctx, oid, "application/octet-stream", payload)
	if err != nil {
		return "", fmt.Errorf("%w: %v", lfs.ErrBackendFailure, err)
	}
	return fileID, nil
}

func (b *GDriveBackend) Open(ctx context.Context, oid string, storageRef string) (io.ReadCloser, int64, error) {
	trimmedRef := strings.TrimSpace(storageRef)
	if trimmedRef == "" {
		trimmedRef = oid
	}
	return b.client.Download(ctx, trimmedRef)
}

func (b *GDriveBackend) Exists(ctx context.Context, oid string, storageRef string) (bool, error) {
	trimmedRef := strings.TrimSpace(storageRef)
	if trimmedRef == "" {
		trimmedRef = oid
	}
	return b.client.Exists(ctx, trimmedRef)
}

func (b *GDriveBackend) Delete(ctx context.Context, oid string, storageRef string) error {
	trimmedRef := strings.TrimSpace(storageRef)
	if trimmedRef == "" {
		trimmedRef = oid
	}
	return b.client.Delete(ctx, trimmedRef)
}
