package gdrive

import (
	"context"
	"errors"
	"testing"

	"github.com/A-L-L-A-H/mw-gdrive"
	"github.com/A-L-L-A-H/mw-lab/internal/lfs"
)

type failingReader struct{}

func (failingReader) Read(_ []byte) (int, error) {
	return 0, errors.New("read failed")
}

func TestNewGDriveBackendValidationAndName(t *testing.T) {
	if _, err := NewGDriveBackend(nil, "", nil); err == nil {
		t.Fatal("expected error for nil config")
	}
	if _, err := NewGDriveBackend(&gdrive.GDriveConfig{
		ClientSecret: "sec",
		RefreshToken: "ref",
	}, "", nil); err == nil {
		t.Fatal("expected error when ClientID is empty")
	}

	backend, err := NewGDriveBackend(&gdrive.GDriveConfig{
		ClientID:     "cid",
		ClientSecret: "csec",
		RefreshToken: "cref",
	}, "", nil)
	if err != nil {
		t.Fatalf("NewGDriveBackend failed: %v", err)
	}
	if backend.Name() != "google-drive" {
		t.Fatalf("expected Name() == 'google-drive', got %q", backend.Name())
	}
	if backend.rootFolder != "mw-lab" {
		t.Fatalf("expected default rootFolder 'mw-lab', got %q", backend.rootFolder)
	}
}

func TestGDriveBackendPutReadFailure(t *testing.T) {
	backend, err := NewGDriveBackend(&gdrive.GDriveConfig{
		ClientID:     "cid",
		ClientSecret: "csec",
		RefreshToken: "cref",
	}, "custom-folder", nil)
	if err != nil {
		t.Fatalf("NewGDriveBackend failed: %v", err)
	}
	if _, err := backend.Put(context.Background(), "oid-1234", failingReader{}, 4); !errors.Is(
		err, lfs.ErrBackendFailure,
	) {
		t.Fatalf("expected ErrBackendFailure on reader error, got %v", err)
	}
}
